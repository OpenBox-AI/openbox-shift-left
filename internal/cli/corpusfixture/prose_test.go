package corpusfixture

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSyntheticProseIsExactlyTheRequestedLength holds the property the fixture
// scrub depends on: a replacement must occupy the same rune geometry as what it
// replaced, or the soak test's oversized-body precondition and the projected
// per-call cost both move.
func TestSyntheticProseIsExactlyTheRequestedLength(t *testing.T) {
	for _, n := range []int{0, 1, 7, 63, 64, 200, 1024, 23743, 65537} {
		got := SyntheticProse(n)
		if r := utf8.RuneCountInString(got); r != n {
			t.Errorf("SyntheticProse(%d) is %d runes, want %d", n, r, n)
		}
		if len(got) != utf8.RuneCountInString(got) {
			t.Errorf("SyntheticProse(%d) is not pure ASCII; a multi-byte rune makes the byte and rune bounds disagree", n)
		}
	}
}

// TestSyntheticProseIsDeterministic is what lets Scan re-derive the filler for an
// observed length and compare, rather than matching a regexp that drifts.
func TestSyntheticProseIsDeterministic(t *testing.T) {
	for _, n := range []int{100, 5000} {
		if SyntheticProse(n) != SyntheticProse(n) {
			t.Errorf("SyntheticProse(%d) is not stable across calls", n)
		}
	}
	long := SyntheticProse(5000)
	if !strings.HasPrefix(long, SyntheticProse(100)) {
		t.Error("SyntheticProse is not prefix-stable across lengths; a truncated block would stop comparing equal")
	}
}

// TestSyntheticProseTripsNoSentinel keeps the replacement from becoming a finding
// of its own: filler that happened to match a home path or a hex identifier would
// make every regenerated fixture fail the gate that admits it.
func TestSyntheticProseTripsNoSentinel(t *testing.T) {
	body := `{"note":` + quote(SyntheticProse(4096)) + `}`
	if v := Scan([]byte(body)); len(v) != 0 {
		t.Errorf("filler tripped %d sentinel(s), first: %s", len(v), v[0])
	}
}

// TestSyntheticProseSaysWhatItIs keeps a reader of the committed corpus from
// mistaking filler for a recorded prompt.
func TestSyntheticProseSaysWhatItIs(t *testing.T) {
	if !strings.Contains(SyntheticProse(400), "synthetic") {
		t.Error("filler does not announce itself; a reader cannot tell it from recorded text")
	}
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
