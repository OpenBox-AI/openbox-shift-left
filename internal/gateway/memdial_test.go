package gateway

import (
	"os"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway/internal/dialhook"
)

// TestMain points the relay's upstream dial at the in-memory registry. The
// relay reaches its upstream through its own hand-tuned Transport, so the
// http.DefaultTransport substitution that serves the rest of the repo's tests
// cannot reach it.
func TestMain(m *testing.M) {
	dialhook.UpstreamDialContext = memhttptest.DialContext
	os.Exit(m.Run())
}
