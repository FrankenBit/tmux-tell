package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/discover"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/render"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/sdnotify"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

// serveOpts is the resolved configuration for runServeWithStore.
type serveOpts struct {
	Agent              string
	InterMessageDelay  time.Duration
	IdlePollInterval   time.Duration
	PauseCheckInterval time.Duration
	DeliverTimeout     time.Duration
	// Walker resolves pane-id drift via the shared discover package. When
	// nil, runServeWithStore constructs a discover.New() — tests can inject
	// a fake walker that doesn't touch real tmux/proc.
	Walker *discover.Walker
}

// runServeCLI parses serve-subcommand flags, sets up signal handling, and
// drives the mailman loop.
//
// Usage: claude-msg serve --agent NAME [tuning flags]
func runServeCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	agent := fs.String("agent", "", "agent name to serve (required)")
	interMsg := fs.Duration("inter-message-delay", 200*time.Millisecond,
		"pause between successive deliveries")
	idlePoll := fs.Duration("idle-poll", 250*time.Millisecond,
		"queue-empty sleep before re-checking")
	pausePoll := fs.Duration("pause-poll", time.Second,
		"interval to re-check the paused flag")
	deliverTimeout := fs.Duration("deliver-timeout", 30*time.Second,
		"per-message deadline for the tmux delivery sequence")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *agent == "" {
		fmt.Fprintln(stderr, "--agent required")
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		fmt.Fprintf(stderr, "open store: %v\n", err)
		return exitInternal
	}
	defer s.Close()

	stopCtx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger := log.New(stderr,
		fmt.Sprintf("[mailman/%s] ", *agent),
		log.LstdFlags|log.Lmicroseconds)

	return runServeWithStore(stopCtx, s, serveOpts{
		Agent:              *agent,
		InterMessageDelay:  *interMsg,
		IdlePollInterval:   *idlePoll,
		PauseCheckInterval: *pausePoll,
		DeliverTimeout:     *deliverTimeout,
	}, logger, stdout, stderr)
}

// runServeWithStore is the testable mailman loop. stopCtx is the signal
// context — cancellation requests a graceful exit at the next loop edge.
// SQL and tmux operations use independent contexts so an in-flight message
// completes cleanly even when SIGTERM has already fired.
func runServeWithStore(stopCtx context.Context, s *store.Store,
	opts serveOpts, logger *log.Logger,
	_ io.Writer, stderr io.Writer,
) int {
	// Background context for store + tmux operations. We don't want a
	// SIGTERM mid-Deliver to leave a half-pasted message; instead we let
	// the current iteration finish, then exit at the top of the next.
	opCtx := context.Background()

	// Startup: agent must be registered with a pane_id.
	a, err := s.GetAgent(opCtx, opts.Agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "agent %q not registered — run 'claude-msg discover'\n", opts.Agent)
			return exitUnavailable
		}
		fmt.Fprintf(stderr, "get_agent: %v\n", err)
		return exitInternal
	}
	if a.PaneID == "" {
		fmt.Fprintf(stderr, "agent %q has no pane_id — run 'claude-msg discover'\n", opts.Agent)
		return exitUnavailable
	}

	if n, err := s.RecoverDelivering(opCtx, opts.Agent); err != nil {
		logger.Printf("recover_failed err=%v", err)
	} else if n > 0 {
		logger.Printf("recovered count=%d", n)
	}

	walker := opts.Walker
	logger.Printf("starting pane=%s", a.PaneID)
	defer logger.Printf("stopped")

	// systemd watchdog: tell the manager we're up, log the interval that
	// will keep WatchdogSec= happy. The ping at the bottom of each loop
	// iteration covers the busy path; the idle-poll select includes the
	// watchdog window for empty queues.
	if err := sdnotify.Ready(); err != nil {
		logger.Printf("sdnotify_ready_err err=%v", err)
	}
	watchdogPing, _ := sdnotify.WatchdogInterval()
	if watchdogPing > 0 {
		logger.Printf("watchdog interval=%s", watchdogPing)
	}

	for {
		if stopCtx.Err() != nil {
			return exitOK
		}
		if watchdogPing > 0 {
			_ = sdnotify.Watchdog()
		}

		// Re-read every iteration so pause/resume and discover updates
		// are picked up without restarting the daemon.
		a, err := s.GetAgent(opCtx, opts.Agent)
		if err != nil {
			logger.Printf("get_agent_failed err=%v", err)
			if stopOrSleep(stopCtx, opts.PauseCheckInterval) {
				return exitOK
			}
			continue
		}
		if a.Paused {
			if stopOrSleep(stopCtx, opts.PauseCheckInterval) {
				return exitOK
			}
			continue
		}

		msg, err := s.ClaimNext(opCtx, opts.Agent)
		if err != nil {
			logger.Printf("claim_failed err=%v", err)
			if stopOrSleep(stopCtx, opts.IdlePollInterval) {
				return exitOK
			}
			continue
		}
		if msg == nil {
			if stopOrSleep(stopCtx, opts.IdlePollInterval) {
				return exitOK
			}
			continue
		}

		logger.Printf("delivering id=%s kind=%s from=%s body_bytes=%d",
			msg.PublicID, msg.Kind, msg.FromAgent, len(msg.Body))

		paneForDelivery := a.PaneID
		deliverCtx, cancel := context.WithTimeout(opCtx, opts.DeliverTimeout)
		derr := deliverOne(deliverCtx, paneForDelivery, msg)
		cancel()

		// Auto-heal on pane-id drift: if tmux says the pane is gone, ask
		// the discover walker for the agent's current pane, update the
		// row, retry once. Avoids marking messages 'failed' when the
		// operator just respawned a pane in a new window.
		if derr != nil && isCantFindPaneError(derr) {
			if walker == nil {
				walker = discover.New()
			}
			newPane, lerr := walker.LookupByName(opCtx, opts.Agent)
			if lerr == nil && newPane != "" && newPane != paneForDelivery {
				logger.Printf("auto_heal id=%s agent=%s old_pane=%s new_pane=%s",
					msg.PublicID, opts.Agent, paneForDelivery, newPane)
				if uerr := s.UpsertAgent(opCtx, opts.Agent, newPane); uerr != nil {
					logger.Printf("auto_heal_update_failed err=%v", uerr)
				} else {
					retryCtx, rcancel := context.WithTimeout(opCtx, opts.DeliverTimeout)
					derr = deliverOne(retryCtx, newPane, msg)
					rcancel()
				}
			} else if lerr != nil {
				logger.Printf("auto_heal_lookup_err err=%v", lerr)
			}
		}

		if derr != nil {
			logger.Printf("deliver_failed id=%s err=%v", msg.PublicID, derr)
			if err := s.MarkFailed(opCtx, msg.PublicID, derr.Error()); err != nil {
				logger.Printf("mark_failed_err id=%s err=%v", msg.PublicID, err)
			}
		} else {
			logger.Printf("delivered id=%s", msg.PublicID)
			if err := s.MarkDelivered(opCtx, msg.PublicID); err != nil {
				logger.Printf("mark_delivered_err id=%s err=%v", msg.PublicID, err)
			}
		}

		if stopOrSleep(stopCtx, opts.InterMessageDelay) {
			return exitOK
		}
	}
}

// deliverOne dispatches a single message to a pane based on its Kind:
// regular messages go through the paste-buffer renderer with verification;
// control commands type their body directly via send-keys -l so they hit
// Claude Code's slash-command parser without the chat header.
func deliverOne(ctx context.Context, pane string, msg *store.Message) error {
	if msg.Kind == store.KindControl {
		return tmuxio.SendKeys(ctx, pane, msg.Body)
	}
	return tmuxio.Deliver(ctx, tmuxio.DeliverParams{
		Pane:        pane,
		Body:        render.Message(*msg),
		VerifyToken: "id " + msg.PublicID,
	})
}

// isCantFindPaneError detects the tmux delivery failure mode that means
// the recipient's stored pane_id no longer exists. tmux 3.x phrases this
// as "can't find pane: %N"; we match on the substring so the format can
// drift across versions without breaking the auto-heal path.
func isCantFindPaneError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "can't find pane")
}

// stopOrSleep waits for d or until stopCtx is cancelled. Returns true on
// cancellation so the caller can exit.
func stopOrSleep(stopCtx context.Context, d time.Duration) bool {
	if d <= 0 {
		return stopCtx.Err() != nil
	}
	select {
	case <-stopCtx.Done():
		return true
	case <-time.After(d):
		return false
	}
}
