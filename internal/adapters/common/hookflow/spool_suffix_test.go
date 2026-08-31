package hookflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpoolSuffixesStayUnique. The spool's rotate, reclaim and recovery names
// are only ever distinguished by their random suffix, so two of them colliding
// is two drains writing the same path -- one event stream silently overwriting
// the other. The suffix used to come from a helper that returned the literal
// "sfx-fallback" when the entropy read failed, which turns every name in the
// directory into the same name at exactly the moment the machine is already in
// trouble. crypto/rand.Text has no error return to mishandle: Read panics
// rather than reporting failure, so the constant branch is unrepresentable.
//
// This is a regression net, not a reproduction. The old branch was reachable
// only on an entropy failure that a test cannot induce; what is assertable is
// that a thousand names really are a thousand names.
func TestSpoolSuffixesStayUnique(t *testing.T) {
	dir := t.TempDir()
	s := Spool{Dir: dir}
	line, err := jsonLine(ev("sess", "e1"))
	if err != nil {
		t.Fatalf("jsonLine: %v", err)
	}

	const writes = 1000
	for range writes {
		s.writeRecovery(filepath.Join(dir, "sess.jsonl"), [][]byte{line}, 0)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != writes {
		t.Errorf("%d recovery files after %d writes: names collided, so a drain overwrote "+
			"another drain's carry-over instead of adding to it", len(entries), writes)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "sfx-fallback") {
			t.Errorf("recovery file %q carries the constant fallback suffix; on entropy failure "+
				"every spool name collapses to one", e.Name())
		}
		if !IsRecoveryFile(e.Name()) {
			t.Errorf("recovery file %q is not recognised by IsRecoveryFile, so the sweep "+
				"would never pick it up", e.Name())
		}
	}
}
