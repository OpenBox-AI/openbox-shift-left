package main

import (
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// The help text's provider placeholder must name every supported tool: it was
// once a literal that stayed "claude-code|codex" after Muse shipped.
func TestUsageNamesEverySupportedProvider(t *testing.T) {
	a, _, errb := testApp(nil)
	a.usage()
	got := errb.String()
	want := "openbox init --provider <" + strings.Join(provider.Supported(), "|") + ">"
	if n := strings.Count(got, want); n != 2 {
		t.Fatalf("usage names %q %d times, want 2 (setup and usage blocks):\n%s", want, n, got)
	}
	// Enforcement is always on (devconfig.ResolveEnforce), so offering
	// OPENBOX_ENFORCE=false as an "observe only" switch would advertise a no-op.
	if strings.Contains(got, devconfig.EnvEnforce+"=") {
		t.Fatalf("usage still offers %s, which is no longer honoured:\n%s", devconfig.EnvEnforce, got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "%!") {
			t.Fatalf("usage has a formatting artifact: %q", line)
		}
	}
}
