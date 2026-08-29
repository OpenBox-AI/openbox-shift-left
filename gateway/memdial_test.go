package gateway

import (
	"os"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/gateway/internal/dialhook"
)

// TestMain points the relay's upstream dial at the in-memory registry.
//
// The relay reaches its upstream through its own hand-tuned Transport, so the
// http.DefaultTransport substitution that serves the rest of the repo's tests
// cannot reach it. Only the dial is replaced: DisableCompression,
// ForceAttemptHTTP2, the redirect rule and the idle pool all stay in the path,
// which is what keeps the byte-identity assertions meaningful.
//
// memhttptest.DialContext falls through to a real dial for any address it did not
// hand out, so a test that means to reach a genuinely unreachable host still
// does.
func TestMain(m *testing.M) {
	dialhook.UpstreamDialContext = memhttptest.DialContext
	os.Exit(m.Run())
}
