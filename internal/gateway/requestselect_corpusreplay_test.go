package gateway

// Manual corpus-replay gate. Not part of the suite's normal work: it is skipped
// unless OPENBOX_REPLAY_CORPUS names a file of JSON-encoded stored request
// documents, one per line. It exists because a unit test proving a fix on one
// hand-made body is not evidence about the field -- that gap is exactly how an
// earlier pass called a defect fixed while it persisted on 15% of real traffic.
//
// It runs the SHIPPED predicate rather than a re-implementation of it, which is
// the whole point: a proxy that agrees on a fixture can still disagree in the
// field.

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

func TestReplayTheJudgedTailOverAStoredCorpus(t *testing.T) {
	path := os.Getenv("OPENBOX_REPLAY_CORPUS")
	if path == "" {
		t.Skip("set OPENBOX_REPLAY_CORPUS to a file of stored request documents")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()

	var docs, before, after, allNonTurn int
	depth := map[int]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var body string
		if json.Unmarshal(sc.Bytes(), &body) != nil {
			continue
		}
		var doc struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if json.Unmarshal([]byte(body), &doc) != nil || len(doc.Messages) == 0 {
			continue
		}
		docs++
		if isTurn(doc.Messages[len(doc.Messages)-1]) {
			before++
		}
		kept, skipped := trimTrailingNonTurns(doc.Messages)
		depth[skipped]++
		if isTurn(kept[len(kept)-1]) {
			after++
		} else if skipped == 0 {
			allNonTurn++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan corpus: %v", err)
	}
	if docs == 0 {
		t.Fatal("corpus produced no parseable documents")
	}
	pct := func(n int) float64 { return 100 * float64(n) / float64(docs) }
	t.Logf("docs=%d  newest element is a real turn: BEFORE %d (%.1f%%) -> AFTER %d (%.1f%%)",
		docs, before, pct(before), after, pct(after))
	t.Logf("walk-back depth distribution: %v; entirely non-turn: %d", depth, allNonTurn)
	if pct(after) < 95 {
		t.Errorf("after the shipped predicate the judged tail is a real turn on %.1f%% of the "+
			"corpus, under the 95%% bar", pct(after))
	}
	if pct(after) <= pct(before) {
		t.Errorf("the predicate did not improve the judged tail: %.1f%% -> %.1f%%", pct(before), pct(after))
	}
}
