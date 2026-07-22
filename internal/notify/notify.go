// Package notify is a daemonless, best-effort cross-process change-notification
// layer over the SQLite bus (#515). SQLite's update_hook fires only for the
// connection that did the write (same process), but tmux-tell's writer (the
// mailman / a send inserting a reply) and waiter (wait_for_reply, the mailman
// idle loop, inbox --watch, …) are SEPARATE processes — so a cross-process
// notify path is needed alongside the DB.
//
// The load-bearing contract: a notify is an OPTIMIZATION OVER A SLOW POLL, never
// a replacement. The doorbell carries only "something changed for <key>" — the
// payload always still comes from SQLite. So every consumer keeps its poll
// (slowed to a correctness-fallback cadence, e.g. 5s) and merely ADDS a
// `case <-notifyCh:` for low-latency wakes. A missed ring self-heals on the next
// slow poll. That is what lets this layer skip durability, exactly-once, and
// daemon-crash-loses-data handling — a missed signal costs ≤ one fallback
// interval of latency, never a lost message.
//
// Mechanism: doorbell FILES under $XDG_RUNTIME_DIR/tmux-tell/notify/, one per
// recipient key. Notify(key) writes the file (generating an inotify event); a
// Watcher places ONE fsnotify watch on the parent directory and demultiplexes
// events to per-key subscriber channels. Daemonless — no broker process to run,
// supervise, or restart — which preserves the embedded store's zero-ops
// property (resisting the broker daemon was the motivating design constraint).
//
// Layering: this is a leaf package — it imports only fsnotify + the stdlib,
// never internal/store or internal/cli. internal/store stays a pure leaf too:
// it never imports this package. The producer side is wired caller-side (or via
// an injected func value), so the low-level store stays notify-agnostic.
//
// Env-consistency assumption: the producer and consumer processes must resolve
// the same runtime dir, i.e. share $XDG_RUNTIME_DIR (true for one user / systemd
// session, which is how chambers run). If they ever disagree, the doorbell is
// simply never seen and every consumer falls back to its slow poll — degraded,
// not broken, per the best-effort contract.
package notify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// notifyPayload is a fixed 1-byte body written on every Notify. The byte content
// is irrelevant — what matters is that a write(2) of ≥1 byte deterministically
// generates an inotify IN_MODIFY (fsnotify Write) even when the content is
// unchanged, so a repeated Notify to an already-existing doorbell still wakes
// watchers. (A zero-byte truncate of an already-empty file may emit no event.)
var notifyPayload = []byte{'.'}

// runtimeDir resolves the per-user runtime root the doorbell files live under.
// Keyed on $XDG_RUNTIME_DIR — the XDG spec's home for ephemeral, may-vanish
// runtime state, which matches the doorbells' best-effort semantics exactly.
// Falls back to $TMPDIR then /tmp so a stripped environment still functions
// (consistency between producer and consumer is the only requirement; see the
// package doc's env-consistency note).
func runtimeDir() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return rt
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		return tmp
	}
	return "/tmp"
}

// notifyDir is the single directory all recipient doorbell files live in.
func notifyDir() string {
	return filepath.Join(runtimeDir(), "tmux-tell", "notify")
}

// sanitizeKey maps a recipient key to a safe, deterministic filename. Both
// Notify and Watch run keys through it, so the producer's filename and the
// consumer's subscriber bucket always agree. Agent names are already
// constrained at registration; this only defends against a stray separator
// turning a doorbell into a subdirectory. A sanitize collision (two distinct
// keys → one filename) merely cross-wakes — benign under the best-effort
// contract (the spuriously-woken consumer re-polls SQLite and finds nothing).
func sanitizeKey(key string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '_' || r == '-':
			return r
		default:
			return '_'
		}
	}, key)
}

// Notify rings the doorbell for recipient key — best-effort, never errors. It
// writes <notifyDir>/<key> via os.WriteFile, generating fsnotify Create+Write
// (first ring, new file) or Write (subsequent rings) that a Watcher on the
// directory observes — the filter catches either, so the wake survives a single
// dropped Create. Errors are
// swallowed: a missed ring costs one slow-poll cycle of latency on one path,
// never a lost message. The signature is func(string) so it can be injected
// directly as a store notifier callback without the store importing this
// package.
//
// Producers must call Notify strictly AFTER the originating DB write has
// committed — a wake delivered before the row is visible would have the consumer
// re-poll, see nothing, and sleep again, defeating the optimization (#515 D4).
func Notify(key string) {
	if key == "" {
		return
	}
	dir := notifyDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, sanitizeKey(key)), notifyPayload, 0o600)
}

// Watcher is a single fsnotify watch on the doorbell directory that
// demultiplexes events to per-key subscriber channels. Create one per process
// and call Watch any number of times.
//
// O(events), not O(keys): the shared directory watch + one demux goroutine
// processes each filesystem event exactly once and routes it by filename, rather
// than holding one fsnotify handle per watched key (which would be O(keys)
// handles and, across a shared dir, O(keys²) fan-out). If agent-count × notify
// rate ever bottlenecks the single demux goroutine, the escape hatch is
// per-recipient subdirectories watched independently (#515 scale note) — not
// needed at current scale.
type Watcher struct {
	fsw *fsnotify.Watcher
	dir string

	mu   sync.Mutex
	subs map[string][]chan struct{}

	// closed is closed by Close to release every per-Watch cleanup goroutine,
	// even those whose ctx never cancels (e.g. a context.Background whose Done()
	// is a nil channel) — otherwise such a goroutine would block forever and leak.
	closed    chan struct{}
	closeOnce sync.Once
}

// NewWatcher creates the runtime dir (so the directory watch never races a
// not-yet-created doorbell file) and starts watching it. The caller owns
// Close.
func NewWatcher() (*Watcher, error) {
	dir := notifyDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("notify: mkdir runtime dir: %w", err)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("notify: new fsnotify watcher: %w", err)
	}
	// Watch the DIRECTORY, not the per-key files (#515 D6). The dir always
	// exists (MkdirAll above), so Add never errors on a missing path and never
	// races a first-contact doorbell creation: the FIRST Notify(key) creates
	// <dir>/<key> and its Create event is caught. A file-level watch would have
	// to Add a path that does not exist until the first ring, systematically
	// missing every recipient's first message — the exact latency-critical path.
	if err := fsw.Add(dir); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("notify: watch runtime dir %q: %w", dir, err)
	}
	w := &Watcher{
		fsw:    fsw,
		dir:    dir,
		subs:   make(map[string][]chan struct{}),
		closed: make(chan struct{}),
	}
	go w.loop()
	return w, nil
}

// loop is the single demux goroutine. It exits when fsw is closed (the Events
// channel closes).
func (w *Watcher) loop() {
	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			// Notify writes via os.WriteFile (Create-then-Write); filter to the
			// matching ops so the producer's mutation and this filter cannot
			// drift (#515: a CHMOD-only producer paired with a Write-only filter
			// would silently never wake).
			if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			w.fan(filepath.Base(ev.Name))
		case _, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// Best-effort: a dropped fsnotify error self-heals on the
			// consumer's slow poll. Nothing to do but keep demuxing.
		}
	}
}

// fan delivers a coalesced wake to every subscriber on key.
//
// Snapshot semantics (#817): the subscriber slice is DEEP-COPIED under the
// mutex, not merely aliased. `subs := w.subs[key]` copies the slice HEADER
// but shares the BACKING ARRAY with any concurrent writer — and Watch's
// cleanup goroutine at :245 mutates the backing array in place via
// `append(subs[:i], subs[i+1:]...)` (which shifts elements left within the
// same cap, no reallocation). Fan iterating the shared backing array while
// cleanup shifts through it was the race the -race gate reproduces
// (~1-in-3 on the flagged TestMultipleSubscribersSameKey).
//
// The deep-copy allocates once per fire (typically 1-3 subscribers per key)
// and gives fan an IMMUTABLE snapshot that can't be mutated by concurrent
// register/unregister. Keeps the send loop lock-free — a slow consumer
// can't block a Watch or a Close.
func (w *Watcher) fan(key string) {
	w.mu.Lock()
	subs := append([]chan struct{}(nil), w.subs[key]...)
	w.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
			// Coalesce: a wake is already pending on this buffered-1 channel.
			// The consumer re-reads SQLite (always the latest state) on wake,
			// so one wake per burst suffices.
		}
	}
}

// Watch returns a channel that receives a coalesced signal whenever Notify(key)
// fires, until ctx is done. The channel is buffered-1 with non-blocking sends so
// a burst of rings collapses to a single pending wake.
//
// Executable invariant (#515 D6): a Watch started BEFORE any Notify(key) ever
// fired still wakes on the first Notify — the directory watch is live from
// NewWatcher, so the first ring's Create event is observed. This is what the
// dir-watch design buys over a file-level watch; notify_test.go pins it.
func (w *Watcher) Watch(ctx context.Context, key string) <-chan struct{} {
	ch := make(chan struct{}, 1)
	key = sanitizeKey(key)
	w.mu.Lock()
	w.subs[key] = append(w.subs[key], ch)
	w.mu.Unlock()
	go func() {
		// Release on ctx cancel OR Watcher Close. The Close arm is essential: a
		// caller may pass a context.Background (Done() == nil), and a bare
		// <-ctx.Done() on a nil channel would block this goroutine forever.
		select {
		case <-ctx.Done():
		case <-w.closed:
		}
		w.mu.Lock()
		subs := w.subs[key]
		for i, c := range subs {
			if c == ch {
				w.subs[key] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		if len(w.subs[key]) == 0 {
			delete(w.subs, key)
		}
		w.mu.Unlock()
	}()
	return ch
}

// Close stops the directory watch and the demux goroutine, and releases every
// per-Watch cleanup goroutine (via the closed channel) so none can outlive the
// Watcher even when its Watch ctx never cancels. Idempotent.
func (w *Watcher) Close() error {
	w.closeOnce.Do(func() { close(w.closed) })
	return w.fsw.Close()
}

// WatchOrNil is the best-effort consumer entry point for a poll loop that wants
// low-latency wakes for one key. It returns a wake channel and a stop func to
// defer. On ANY setup failure (e.g. inotify watch limit) it returns
// (nil, no-op): a nil channel never fires in a select, so the caller's loop
// silently falls back to poll-only — exactly the best-effort contract. Callers
// add `case <-ch:` to their existing select and otherwise change nothing.
func WatchOrNil(ctx context.Context, key string) (<-chan struct{}, func()) {
	w, err := NewWatcher()
	if err != nil {
		return nil, func() {}
	}
	return w.Watch(ctx, key), func() { _ = w.Close() }
}
