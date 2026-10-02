package main

import (
	"strings"
	"testing"

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
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "%!") {
			t.Fatalf("usage has a formatting artifact: %q", line)
		}
	}
}
