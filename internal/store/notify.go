package store

import "context"

// Post-commit change-notification hook (#515). The store rings a recipient's
// "doorbell" after a write commits so a cross-process waiter (the recipient's
// mailman idle loop, an inbox --watch, a wait_for_reply) can wake without
// polling. SQLite's own update_hook is useless here — it fires only for the
// connection that did the write, and the writer and waiter are separate
// processes.
//
// Layering: internal/store stays a pure leaf. The hook is a plain func value,
// NOT an import of internal/notify — the CLI wires store.SetNotifier(notify.Notify)
// once at startup (cli.Run, beside tmuxio.SetActivePaneProfile). This is the
// injected-func-value form of #515 D2: it keeps leaf-purity (store imports
// nothing new) AND makes the notify can't-forget (every recipient-bearing write
// method fires it from ONE place, not ~20 scattered caller sites). The four
// publicID-only delivery transitions (markDelivered / MarkFailed) carry no
// recipient without an extra fetch, so those stay caller-side in the mailman,
// which already holds its serving agent — D2's no-extra-query property intact.
//
// Best-effort contract: a notify is an optimization over a slow poll, never a
// replacement (the payload always still comes from SQLite). A dropped ring costs
// one fallback-poll of latency, never a lost message — so fireNotify swallows
// everything, including panics.

// notifier is the process-wide post-commit notification hook. Set ONCE at
// startup via SetNotifier, before any store is opened or any mailman goroutine
// runs, so the field needs no synchronization (mirrors tmuxio's
// SetActivePaneProfile process-global). nil by default: a store with no notifier
// installed simply rings no doorbells, and every consumer falls back to its slow
// poll — the correct no-op for tests and read-only commands.
var notifier func(toAgent string)

// SetNotifier installs the process-wide post-commit recipient-notification hook.
// Construction-time only — call once before opening stores or starting the
// mailman. Passing nil disables notification (useful to reset between tests).
func SetNotifier(fn func(toAgent string)) { notifier = fn }

// watcher is the process-wide consumer-side wake hook (#515) — the read
// counterpart to notifier. A store-resident blocking poll (WaitForReply) calls
// it to obtain a low-latency wake channel for a recipient's doorbell, so it can
// return the moment a matching write rings instead of waiting out its poll. Set
// ONCE at startup via SetWatcher, same construction-time discipline as notifier.
// nil by default → store-resident waits stay pure poll-only (correct for tests
// and any process that never wired it). Keeping it a func value preserves
// store's leaf-purity: the CLI wires it to internal/notify, store imports
// nothing new.
var watcher func(ctx context.Context, key string) <-chan struct{}

// SetWatcher installs the process-wide consumer-side wake hook. Construction-
// time only. The installed func MUST return a channel that (a) delivers a signal
// when Notify(key) fires and (b) is torn down when ctx is done, so a per-call
// watch cannot leak. Passing nil disables wakes (poll-only). This is the read
// half of the #515 store hooks; SetNotifier is the write half.
func SetWatcher(fn func(ctx context.Context, key string) <-chan struct{}) { watcher = fn }

// watchKey returns a wake channel for key via the installed watcher, or nil when
// none is installed. A nil channel never fires in a select, so callers degrade
// cleanly to their poll.
func watchKey(ctx context.Context, key string) <-chan struct{} {
	if watcher == nil {
		return nil
	}
	return watcher(ctx, key)
}

// fireNotify rings toAgent's doorbell via the installed notifier, if any.
//
// Two invariants the callers must respect:
//   - Call it strictly AFTER the originating write has COMMITTED (#515 D4). A
//     wake delivered before the row is visible would have the woken consumer
//     re-poll, find nothing, and sleep again — defeating the optimization.
//   - Call it only when the write actually made a row DELIVERABLE (queued). A
//     deferred/staged insert is not yet in the live queue, so it must not ring;
//     PromoteDeferred rings when that row is finally promoted.
//
// Panic-safe: a misbehaving notifier can never propagate into a just-committed
// write's return path (Surveyor #515 note 1).
func fireNotify(toAgent string) {
	if notifier == nil || toAgent == "" {
		return
	}
	defer func() { _ = recover() }()
	notifier(toAgent)
}
