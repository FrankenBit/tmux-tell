package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os/signal"
	"syscall"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/render"
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

	logger.Printf("starting pane=%s", a.PaneID)
	defer logger.Printf("stopped")

	for {
		if stopCtx.Err() != nil {
			return exitOK
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

		logger.Printf("delivering id=%s from=%s body_bytes=%d",
			msg.PublicID, msg.FromAgent, len(msg.Body))

		rendered := render.Message(*msg)

		deliverCtx, cancel := context.WithTimeout(opCtx, opts.DeliverTimeout)
		derr := tmuxio.Deliver(deliverCtx, tmuxio.DeliverParams{
			Pane:        a.PaneID,
			Body:        rendered,
			VerifyToken: "id " + msg.PublicID,
		})
		cancel()

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
