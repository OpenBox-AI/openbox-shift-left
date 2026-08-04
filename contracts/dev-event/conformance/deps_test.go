package conformance

import (
	"os"
	"strings"
	"testing"
)

// This module is deliberately dependency-free so the adapters can import it in
// their tests without pulling anything in, and so `go test` runs offline with
// no module downloads. That is a property worth asserting rather than trusting:
// a single convenience import would quietly end it.
func TestModuleStaysDependencyFree(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "require") || strings.HasPrefix(line, "replace") {
			t.Errorf("conformance gained a dependency (%q). It must stay importable by any "+
				"adapter's tests without pulling anything in; move whatever needs the "+
				"dependency to a caller.", line)
		}
	}
}
