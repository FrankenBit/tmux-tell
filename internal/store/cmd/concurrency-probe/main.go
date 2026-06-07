// concurrency-probe is a tiny test-only binary used by the cross-
// process cap-as-ceiling pin in internal/store/messages_xprocess_test.go.
//
// It opens the store at --db, attempts a single InsertMessage or a
// two-row InsertMessagePair against the specified --from / --to /
// --cap, and exits with one of three documented codes the parent
// test counts:
//
//	0  the insert (or pair) succeeded — the row(s) landed
//	2  the insert (or pair) was cap-rejected via ErrRecipientQueueFull
//	1  any other failure (unexpected — the parent test fails the run)
//
// The point of running this as a separate process per attempt is to
// exercise SQLite's file-level RESERVED lock + _txlock=immediate +
// busy_timeout across distinct OS-level processes, complementing the
// intra-process pin (TestPin_AtomicCapEnforcement_CeilingUnderConcurrency
// in pin_test.go) which exercises BeginTx atomicity inside a single
// *Store. Surveyor #29 round-3 review flagged the cross-process axis
// as the load-bearing real-world case (mailman daemons + claude-msg
// CLI invocations + MCP server children all hit the same messages.db
// from separate processes).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// Exit codes are part of the contract with messages_xprocess_test.go;
// changing them is a coordinated edit across both files.
const (
	exitAccepted    = 0 // The insert (or pair) succeeded.
	exitOther       = 1 // Unexpected error — parent test fails the run.
	exitCapRejected = 2 // ErrRecipientQueueFull — cap held the ceiling.
)

func main() {
	db := flag.String("db", "", "messages.db path (file-backed)")
	from := flag.String("from", "", "sender agent name (must be registered)")
	to := flag.String("to", "", "recipient agent name (must be registered)")
	capN := flag.Int("cap", 0, "MaxRecipientQueue value to enforce")
	mode := flag.String("mode", "single", "single | pair")
	flag.Parse()

	if *db == "" || *from == "" || *to == "" || *capN <= 0 {
		fmt.Fprintln(os.Stderr, "concurrency-probe: --db, --from, --to, --cap required")
		os.Exit(exitOther)
	}

	s, err := store.Open(*db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "concurrency-probe: open: %v\n", err)
		os.Exit(exitOther)
	}
	defer s.Close()

	ctx := context.Background()
	p := store.InsertParams{
		FromAgent:         *from,
		ToAgent:           *to,
		Body:              "concurrency-probe",
		MaxRecipientQueue: *capN,
		// MaxSenderBacklog intentionally zero; the parent test
		// pre-seeds enough sender headroom that only the recipient
		// cap matters, mirroring the in-process pin.
	}

	switch *mode {
	case "single":
		_, err = s.InsertMessage(ctx, p)
	case "pair":
		p2 := p
		p2.Body = "concurrency-probe-pair-2"
		_, _, err = s.InsertMessagePair(ctx, p, p2, true)
	default:
		fmt.Fprintf(os.Stderr, "concurrency-probe: unknown --mode %q\n", *mode)
		os.Exit(exitOther)
	}

	switch {
	case err == nil:
		os.Exit(exitAccepted)
	case errors.Is(err, store.ErrRecipientQueueFull):
		os.Exit(exitCapRejected)
	default:
		fmt.Fprintf(os.Stderr, "concurrency-probe: unexpected: %v\n", err)
		os.Exit(exitOther)
	}
}
