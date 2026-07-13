package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// runRegisterCLI parses register-subcommand flags and dispatches to the
// shared register pipeline. Mirrors the `tmux-tell.register` MCP tool so
// operators-at-a-bare-shell can register their own pane without needing
// an MCP client (load-bearing for the operator-as-recipient use
// case per #116).
//
// Usage: tmux-tell-claude register --name <name> [--pane <pane>]
//
//	[--delivery-mode paste-and-enter|mailbox-only]
//	[--start-mailman=true|false] [--force]
//	[--alias <alias>]
//
// Mailman lifecycle default: start_mailman defaults true UNLESS
// delivery_mode is `mailbox-only` (then default false — no daemon
// needed since mailbox-only agents never receive a paste). Explicit
// --start-mailman=true overrides.
func runRegisterCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	name := fs.String("name", "", "agent name (the new identity); required")
	sessionIDFlag := fs.String("session-id", "", "explicit session id for exact session-as-addressee routing (#626 Phase 1b); default: self-discovered from the registering pane's process environment")
	pane := fs.String("pane", "", "pane id like %5 (default: $TMUX_PANE)")
	deliveryMode := fs.String("delivery-mode", store.DeliveryModePasteAndEnter,
		"how the mailman delivers to this agent: 'paste-and-enter' (default), 'mailbox-only' (operator-as-recipient per #116; messages stay queued, operator polls via inbox), or 'hook-context' (#249; the recipient agent's session pulls pending messages as additionalContext via a SessionStart/UserPromptSubmit hook — no pane paste)")
	startMailmanFlag := fs.String("start-mailman", "",
		"true|false — start the mailman daemon for this agent. Default: true (mailbox-only defaults to false; explicit true overrides). Note: --start-mailman=true combined with --delivery-mode=mailbox-only is allowed but vestigial — the daemon will start, observe the mailbox-only mode at startup, log the no-work condition, and exit cleanly with Result=success. The 'mailman: active' field in the response is momentary in this case. Note (#293): --start-mailman=true is REJECTED when TMUX_TELL_DB / --db points at a non-default DB path — the systemd-managed mailman launches from the unit-file Environment=, not the caller's env, so it would silently poll the default DB instead. For sandbox-DB callers, use --start-mailman=false and run `<binary> serve --agent <name>` as a foreground subprocess.")
	force := fs.Bool("force", false,
		"overwrite an existing registration with the same name")
	alias := fs.String("alias", "",
		"optional alternative name the discover walker should accept for this canonical agent")
	purgeStale := fs.Bool("purge-stale-queue", false,
		"on a delivery_mode flip, ack the messages queued under the prior mode — they were emitted under the old delivery semantics and would not auto-deliver under the new one (#390)")
	keepStale := fs.Bool("keep-stale-queue", false,
		"on a delivery_mode flip, leave the prior-mode queued messages in place — they stay backlog-fenced (visible in `inbox`, not auto-delivered; clear later with `inbox --ack-all`) (#390)")
	keepPane := fs.Bool("keep-pane", false,
		"update non-pane fields (delivery_mode, alias, etc.) without touching the stored pane_id. "+
			"Intended for acting-on-behalf scenarios (e.g. Bosun flipping another chamber's delivery_mode) "+
			"where the caller is NOT the registered pane. Mutually exclusive with --pane. (#403)")
	relaunchCmd := fs.String("relaunch-cmd", "",
		"the command the mailman send-keys into a post-exit bare shell to restart this chamber "+
			"(#285/#730), e.g. 'chamber-claude.sh Bosun' or 'claude --resume Bosun'. Required for the "+
			"#285 shrink-threshold respawn and the #730 auto-restart co-trigger to actually relaunch — "+
			"the substrate cannot infer it (under tmux-resurrect pane_start_command is the resurrect restore). "+
			"Only applied when the flag is explicitly passed, so a bare re-register never wipes a stored value.")
	autoRestart := fs.Bool("auto-restart", false,
		"arm the #730 co-trigger: a tmux-tell-triggered /compact that exits this chamber is auto-relaunched "+
			"via --relaunch-cmd. Only applied when explicitly passed (default off; operator/wrapper opt-in).")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	// Track which of the opt-in relaunch flags were explicitly passed so a bare
	// re-register (the wrapper auto-registers every launch) never resets a stored
	// relaunch_cmd / auto_restart to the flag zero-value.
	relaunchCmdSet, autoRestartSet := false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "relaunch-cmd":
			relaunchCmdSet = true
		case "auto-restart":
			autoRestartSet = true
		}
	})

	if *name == "" {
		return writeJSONError(stdout, stderr, "--name required", exitUsage)
	}

	// Track whether --pane was explicit BEFORE env resolution (#403 Fix B).
	paneExplicit := *pane != ""
	if *keepPane && paneExplicit {
		return writeJSONError(stdout, stderr,
			"--keep-pane and --pane are mutually exclusive: --keep-pane means 'don't touch the pane'; pass only one",
			exitUsage)
	}

	resolvedPane := *pane
	if !*keepPane {
		if resolvedPane == "" {
			resolvedPane = os.Getenv("TMUX_PANE")
		}
		if resolvedPane == "" {
			return writeJSONError(stdout, stderr,
				"pane required: pass --pane or run inside tmux with $TMUX_PANE set",
				exitUsage)
		}
	}
	if !store.ValidDeliveryMode(*deliveryMode) {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("invalid --delivery-mode %q (want %q or %q)",
				*deliveryMode, store.DeliveryModePasteAndEnter, store.DeliveryModeMailboxOnly),
			exitUsage)
	}

	// Mailman-start default depends on delivery_mode. Explicit flag
	// override beats the implicit default. Both mailbox-only (#116) and
	// hook-context (#249) have a no-paste mailman that short-circuits at
	// startup, so neither auto-starts one.
	start := *deliveryMode != store.DeliveryModeMailboxOnly && *deliveryMode != store.DeliveryModeHookContext
	if *startMailmanFlag != "" {
		switch *startMailmanFlag {
		case "true":
			start = true
		case "false":
			start = false
		default:
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("invalid --start-mailman %q (want true|false)", *startMailmanFlag),
				exitUsage)
		}
	}

	// #293: refuse start_mailman with a non-default DB path BEFORE any DB
	// writes. The systemd-managed mailman launches from the unit-file
	// Environment= directive, not the caller's env, so a sandbox-DB caller
	// requesting a systemd-managed mailman would silently misroute: agent
	// row in sandbox DB, mailman polling production DB. Detect-and-refuse
	// at the call site before the upsert happens — the caller's intent is
	// already incoherent, and partial writes ("registered but the daemon
	// will silently mismatch") read worse than a clean refusal.
	resolvedDB := resolveDBPath(*dbPath)
	if start {
		if mismatched, callerDB := startMailmanWouldMismatchSystemd(resolvedDB); mismatched {
			return writeJSONError(stdout, stderr,
				startMailmanMismatchError(*name, callerDB),
				exitDataErr)
		}
		// #356: refuse start_mailman when the D-Bus / XDG session vars required
		// by `systemctl --user` are absent from this process's environment.
		if missing := startMailmanMissingEnv(); len(missing) > 0 {
			return writeJSONError(stdout, stderr,
				startMailmanEnvError(*name, missing),
				exitDataErr)
		}
	}

	s, err := store.Open(resolvedDB)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	ctx := context.Background()
	existing, err := s.GetAgent(ctx, *name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("lookup: %v", err), exitInternal)
	}
	if existing != nil && !*force {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("agent %q already registered with pane %s; pass --force to overwrite",
				*name, existing.PaneID), exitDataErr)
	}

	// #390: a delivery_mode flip orphans the agent's pre-flip queued messages —
	// they were emitted under the old delivery semantics and sit below the new
	// mailman's backlog floor, so ClaimNext silently skips them (reads as a bug).
	// Force an explicit operator disposition rather than leaving them silently
	// fenced. Fires only on an actual mode change with orphan rows present, so a
	// same-mode --force re-register (a chamber restart) never trips it. `--force`
	// is orthogonal — it authorizes overwriting the registration, not a queue
	// disposition (ratified: no silent semantic coupling).
	if existing != nil && existing.DeliveryMode != *deliveryMode {
		if *purgeStale && *keepStale {
			return writeJSONError(stdout, stderr,
				"pass at most one of --purge-stale-queue / --keep-stale-queue", exitUsage)
		}
		stale, cerr := s.CountStaleQueued(ctx, *name)
		if cerr != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("count stale queue: %v", cerr), exitInternal)
		}
		switch {
		case stale == 0:
			// Nothing orphaned by the flip — no disposition needed.
		case *purgeStale:
			n, aerr := s.AckStaleQueued(ctx, *name)
			if aerr != nil {
				return writeJSONError(stdout, stderr,
					fmt.Sprintf("purge stale queue: %v", aerr), exitInternal)
			}
			fmt.Fprintf(stderr, "register: purged %d message(s) queued under the prior delivery_mode (%s → %s)\n",
				n, existing.DeliveryMode, *deliveryMode)
		case *keepStale:
			fmt.Fprintf(stderr, "register: kept %d message(s) queued under the prior delivery_mode (%s → %s); they will not auto-deliver — see them via `inbox` (backlog-fenced) or clear with `inbox --ack-all`\n",
				stale, existing.DeliveryMode, *deliveryMode)
		default:
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("agent %q has %d message(s) queued under the prior delivery_mode (%s) that will NOT auto-deliver after the flip to %s — pass --purge-stale-queue to ack them, or --keep-stale-queue to leave them queued (visible as backlog-fenced in inbox)",
					*name, stale, existing.DeliveryMode, *deliveryMode),
				exitDataErr)
		}
	}

	// #403 Fix B: refuse to silently rewrite pane_id when $TMUX_PANE differs
	// from the stored pane and the caller did not explicitly pass --pane or
	// --keep-pane. This catches the acting-on-behalf case (QM running register
	// for Lookout while $TMUX_PANE=%7 would clobber Lookout's stored %8) and
	// the post-crash self-reregister case (where the new pane must be named
	// explicitly). The refuse path fires only on an existing registration with a
	// stored pane_id — a first-time registration has nothing to protect.
	//
	// COUPLING(alcatraz-infra:scripts/chamber-claude.sh,chamber-codex.sh): Fix B
	// is bypassed on the #532 auto-register self-heal path because both wrappers
	// pass --pane "$TMUX_PANE" explicitly (paneExplicit=true). Dropping --pane
	// from either wrapper would make Fix B refuse the auto-register → silent
	// self-heal break. (#403)
	if existing != nil && existing.PaneID != "" && !paneExplicit && !*keepPane {
		if resolvedPane != existing.PaneID {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf(
					"register: refusing to silently rewrite pane_id %s → %s for agent %q.\n"+
						"Pass --pane %s to keep the existing pane, --pane %s to actually change it,\n"+
						"or --keep-pane to update other fields without touching pane_id.",
					existing.PaneID, resolvedPane, *name,
					existing.PaneID, resolvedPane),
				exitDataErr)
		}
	}

	if err := s.UpsertAgent(ctx, *name, resolvedPane); err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("upsert: %v", err), exitInternal)
	}
	if err := s.SetDeliveryMode(ctx, *name, *deliveryMode); err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("set delivery_mode: %v", err), exitInternal)
	}
	// #285/#730 relaunch config — only when the flag was explicitly passed, so a
	// bare re-register preserves a previously-registered value (see fs.Visit above).
	// Non-fatal: registration already succeeded; a failed tunable write is awkward
	// but doesn't break the bus.
	if relaunchCmdSet {
		if err := s.SetRelaunchCmd(ctx, *name, *relaunchCmd); err != nil {
			fmt.Fprintf(stderr, "WARN register: set relaunch_cmd: %v\n", err)
		}
	}
	if autoRestartSet {
		if err := s.SetAutoRestart(ctx, *name, *autoRestart); err != nil {
			fmt.Fprintf(stderr, "WARN register: set auto_restart: %v\n", err)
		}
	}
	// #224: auto-clear any prior attention_state on (re)register. The chamber
	// is back and ready; whatever it was awaiting is presumed resolved (or
	// has been answered out-of-band). Substrate-honest reset so the operator's
	// attention queue doesn't carry stale "awaiting_operator" signals across
	// chamber restarts / /compact / spawn-die cycles.
	if err := s.SetAttentionState(ctx, *name, store.AttentionStateIdle); err != nil {
		// Non-fatal: registration already succeeded above. A failed
		// attention-state clear is operationally awkward but doesn't break
		// the bus. Surface as a soft signal rather than aborting.
		fmt.Fprintf(stderr, "WARN register: clear attention_state: %v\n", err)
	}
	// #291: clear any mailman stuck-state on (re)register. A stuck mailman
	// parked itself because the pane registration was stale / wrong-server;
	// re-registering IS the operator fixing that registration, so the park
	// signal is presumed resolved. The serving mailman re-reads the agent row
	// each loop, so clearing stuck_reason here resumes delivery on its next
	// iteration (with a fresh consecutive-failure counter). This is the AC4
	// `register --force clears the stuck state` path — `--force` is required
	// to overwrite an existing (stuck) agent, so the clear naturally rides it.
	if err := s.ClearStuck(ctx, *name); err != nil {
		// Non-fatal, same rationale as the attention-state clear above.
		fmt.Fprintf(stderr, "WARN register: clear stuck_reason: %v\n", err)
	}
	// #626 Phase 1b: self-discover the intrinsic session identity. An explicit
	// --session-id wins (the launch wrapper passes the UUID it minted); otherwise
	// walk the registering pane's process tree for the wrapper-injected
	// TMUX_TELL_SESSION_ID (#643 — same UUID the wrapper --setenv's tree-wide).
	// When found, store it as the primary exact match key for delivery
	// resolution. When NOT found (a raw non-wrapper launch, or a bare pane),
	// leave session_id untouched — a prior value is preserved, and a never-set
	// one stays empty so delivery uses the name-based fallback (#626 AC6).
	// Non-fatal: registration already succeeded.
	sessionID := *sessionIDFlag
	if sessionID == "" && resolvedPane != "" {
		if sid, ok := discover.New().SessionIDForPane(ctx, resolvedPane); ok {
			sessionID = sid
		}
	}
	if sessionID != "" {
		if err := s.SetSessionID(ctx, *name, sessionID); err != nil {
			fmt.Fprintf(stderr, "WARN register: set session_id: %v\n", err)
		}
	}
	if *alias != "" {
		if err := s.AddAlias(ctx, *name, *alias); err != nil {
			if errors.Is(err, store.ErrAliasCollision) {
				return writeJSONError(stdout, stderr,
					fmt.Sprintf("alias %q rejected: %v", *alias, err),
					exitDataErr)
			}
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("add alias: %v", err), exitInternal)
		}
	}

	// Surface the recipient's queued-message backlog at register time
	// (#151) so a fresh or re-registering session learns it has mail
	// waiting without needing a separate inbox poll — closes the
	// inbox-poll-not-push gap for the spawn-per-task / post-restart
	// chamber pattern. Non-fatal: registration already succeeded above,
	// so a count hiccup degrades to a soft `queued_error` field rather
	// than failing the register (mirrors the mailman-start soft-fail
	// precedent below; an honest 0 must not be confused with "unknown").
	queued, qErr := s.RecipientQueueDepth(ctx, *name)

	out := map[string]any{
		"ok":            true,
		"name":          *name,
		"pane":          resolvedPane,
		"delivery_mode": *deliveryMode,
		"registered":    true,
	}
	if qErr != nil {
		out["queued_error"] = qErr.Error()
	} else {
		out["queued"] = queued
		// #204 don't-flood policy: when this (re)register found a queued
		// backlog, stamp the claim-floor + insert the 📬 nudge per the
		// resolved on-register-backlog policy. Config load degrades to
		// defaults on error (Resolve* treat a nil/empty file as "use
		// hardcoded"). Gated on qErr == nil so a count hiccup doesn't get
		// mistaken for an empty backlog.
		cfg, _ := config.Load()
		addBacklogPolicyFields(out, applyBacklogPolicy(ctx, s, cfg, *name, *deliveryMode, queued))
	}

	// #258(a): promote this agent's register-deferred messages
	// (deliver_after="register") — the spawn-die session-bridge ("remember
	// this for my next dispatch", e.g. Pilot's dispatch-across-sessions). The
	// register IS the trigger fire; no explicit flush_deferred is needed.
	//
	// Deliberately AFTER the #204 backlog count + floor above. The AC sketched
	// "promote before the floor so the rows count as live" — but re-evaluating
	// per the AC's own note: #227 already exempts deliver_after-marked rows
	// from the floor in ClaimNext, so a promoted register row delivers on the
	// mailman's next loop regardless of floor position. Promoting AFTER the
	// count keeps that delivery guarantee while NOT folding these rows into the
	// ordinary-backlog `queued` count or its don't-flood 📬 nudge — a
	// register-deferred message is meant to be DELIVERED on register, not
	// announced as backlog to go poll. So the announce policy sees only genuine
	// backlog; the register rows ride the exemption straight to delivery, and
	// the response reports them separately as `deferred_promoted` (non-zero
	// only, to keep the common no-deferred register quiet). Best-effort:
	// registration already succeeded, so a promote hiccup degrades to a soft
	// field — a still-deferred row promotes on the next register.
	if deferredPromoted, dpErr := s.PromoteDeferred(ctx, *name, deferTriggerRegister); dpErr != nil {
		out["deferred_promoted_error"] = dpErr.Error()
	} else if deferredPromoted > 0 {
		out["deferred_promoted"] = deferredPromoted
	}
	if start {
		if err := startMailman(ctx, *name); err != nil {
			out["mailman"] = "failed"
			out["mailman_error"] = err.Error()
		} else {
			out["mailman"] = "active"
		}
	} else {
		out["mailman"] = "skipped"
	}
	_ = writeJSONResult(stdout, out)
	return exitOK
}
