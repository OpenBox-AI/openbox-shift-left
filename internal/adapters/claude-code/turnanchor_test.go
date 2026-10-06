package claudecode

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// anchorTurnLine is one assistant turn's usage row, the only shape readTurnUsage sums.
func anchorTurnLine(in, out int) string {
	return `{"isSidechain":false,"timestamp":"2026-09-07T02:00:00Z","message":{"model":"m1","usage":{"input_tokens":` +
		strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `}}}` + "\n"
}

// TestResumeAnchorsTheTurnCursorInsteadOfRebillingTheSitting pins the resume
// anchor, and pins it in both directions in one test because the anchored assertion alone would
// also pass if readTurnUsage were simply broken.
//
// The bug: SessionEnd clears the cursor, so a resumed sitting reopens the SAME
// transcript at offset 0 and readTurnUsage -- one window, cursor to EOF --
// charges every earlier turn to the first Stop after the resume. It stayed
// invisible only because that pair reused activity id `<session>:turn:0` and the
// control plane dropped it as a duplicate. Run identity puts a distinct run_id
// in that dedupe key, so the duplicate would be stored and the prior sitting
// billed twice.
func TestResumeAnchorsTheTurnCursorInsteadOfRebillingTheSitting(t *testing.T) {
	// Three turns from the previous sitting: 60 in, 6 out.
	prior := anchorTurnLine(10, 1) + anchorTurnLine(20, 2) + anchorTurnLine(30, 3)
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	newTurn := func() {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(anchorTurnLine(7, 5)); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	ad := &Adapter{Engine: hookflow.NewEngine(t.TempDir())}
	logger := log.New(io.Discard, "", 0)
	ev := &HookEvent{SessionID: "s-resume", Source: "resume", TranscriptPath: path}

	// The cursor is absent, exactly as SessionEnd leaves it.
	if got := ad.Turns.Read(ev.SessionID, ""); got != (hookflow.TurnPos{}) {
		t.Fatalf("precondition: cursor = %+v, want zero", got)
	}

	// Unanchored is the regression this guards: the whole prior sitting is
	// re-read and charged to the resumed run's first turn.
	newTurn()
	unanchored, _, err := readTurnUsage(path, ad.Turns.Read(ev.SessionID, ""), false)
	if err != nil {
		t.Fatalf("unanchored read: %v", err)
	}
	if unanchored.Input != 67 || unanchored.Output != 11 {
		t.Fatalf("unanchored window = %d/%d, want 67/11 -- if this changed, the "+
			"re-billing bug this guards has moved and the anchored assertion below proves less",
			unanchored.Input, unanchored.Output)
	}

	// Anchored: rebuild the same situation, but pin the cursor at SessionStart
	// before the new turn lands.
	if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	anchorTurnCursor(ad, logger, ev)
	newTurn()

	anchored, _, err := readTurnUsage(path, ad.Turns.Read(ev.SessionID, ""), false)
	if err != nil {
		t.Fatalf("anchored read: %v", err)
	}
	if anchored.Input != 7 || anchored.Output != 5 {
		t.Errorf("anchored window = %d/%d, want 7/5 (only the new turn); the "+
			"resumed sitting is being charged for the previous one",
			anchored.Input, anchored.Output)
	}
}

// TestForkAnchorsToo: a fork opens a fresh session id over a COPY of the
// parent's history, so it never had the duplicate-activity-id protection that
// hid the resume case -- it has been over-counting outright. Same one-line fix.
func TestForkAnchorsToo(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"resume", true},
		{"fork", true},
		{"startup", false},
		{"clear", false},
		{"compact", false},
		{"", false},
		{"not-a-source", false},
	} {
		if got := anchorsTurnCursor(tc.source); got != tc.want {
			t.Errorf("anchorsTurnCursor(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestAnchorFailureIsFailOpen: a transcript that cannot be stat'ed must cost one
// log line, never a blocked session start (INV-3).
func TestAnchorFailureIsFailOpen(t *testing.T) {
	ad := &Adapter{Engine: hookflow.NewEngine(t.TempDir())}
	logger := log.New(io.Discard, "", 0)

	anchorTurnCursor(ad, logger, &HookEvent{SessionID: "s1", Source: "resume",
		TranscriptPath: filepath.Join(t.TempDir(), "absent.jsonl")})
	if got := ad.Turns.Read("s1", ""); got != (hookflow.TurnPos{}) {
		t.Errorf("cursor written from a missing transcript: %+v", got)
	}

	anchorTurnCursor(ad, logger, &HookEvent{SessionID: "s2", Source: "resume"})
	if got := ad.Turns.Read("s2", ""); got != (hookflow.TurnPos{}) {
		t.Errorf("cursor written with no transcript path: %+v", got)
	}
}
