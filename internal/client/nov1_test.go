package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenV1Strings is every literal the deleted v1 (AIP Ed25519-signed)
// protocol left behind: its routes and its four signature headers. Slice 04d
// deleted signing.go and the dual-mode branch; a match here means the
// deletion was incomplete, or a later change quietly reintroduced it.
var forbiddenV1Strings = []string{
	"/api/v1/",
	"X-OpenBox-Agent-",
	"X-OpenBox-Body-SHA256",
}

// TestNoV1PathSurvives is a grep-style check over every non-test source file
// in this package: no v1 route or signature header literal survives.
func TestNoV1PathSurvives(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	scanned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		scanned++
		src := string(b)
		for _, forbidden := range forbiddenV1Strings {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s contains %q; the v1 protocol was deleted, this must not survive", e.Name(), forbidden)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no non-test Go file was scanned; this check would pass vacuously")
	}
}
