package notify

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the package's tests under goleak so a Watcher that fails to
// stop its demux goroutine, or a Watch whose ctx-cleanup goroutine never
// returns, fails loudly rather than silently leaking. Every test must Close its
// Watcher and cancel every Watch ctx.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
