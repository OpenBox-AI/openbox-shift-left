package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTailLines_UnderCapReturnsLastNComplete (V6, phase-09's tail-cap
// decision, case a): a file of many lines under the cap yields the last n
// COMPLETE lines, newest last.
func TestTailLines_UnderCapReturnsLastNComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enforcements.jsonl")
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&sb, `{"line":%d}`+"\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	got := tailLines(path, enforcementTailBytes, 3)
	want := []string{`{"line":8}`, `{"line":9}`, `{"line":10}`}
	if len(got) != len(want) {
		t.Fatalf("tailLines returned %d lines, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestTailLines_OverCapDropsPartialFirstLine (case b): a file larger than the
// byte cap must never unmarshal the window's partial first line as a record.
func TestTailLines_OverCapDropsPartialFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enforcements.jsonl")
	var sb strings.Builder
	var wholeLines []string
	// Each line is >100 bytes; a tiny cap forces a mid-line seek.
	for i := 1; i <= 20; i++ {
		line := fmt.Sprintf(`{"line":%d,"pad":"%s"}`, i, strings.Repeat("x", 80))
		wholeLines = append(wholeLines, line)
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	const tinyCap = 250 // a few lines' worth; well under the file size
	got := tailLines(path, tinyCap, 100)
	if len(got) == 0 {
		t.Fatal("expected at least one complete line in the window")
	}
	for _, line := range got {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a returned line failed to unmarshal (a fragment leaked through): %q: %v", line, err)
		}
	}
	// Every returned line must be one of the file's actual, WHOLE lines --
	// never a byte-sliced fragment of one -- and together they must be an
	// exact tail suffix of the file (case b: the window's partial first line
	// is dropped, never repaired or unmarshalled).
	if len(got) >= len(wholeLines) {
		t.Fatalf("got %d lines from a %d-line file with a cap far under its size; the partial-first-line drop did not shrink the window", len(got), len(wholeLines))
	}
	wantSuffix := wholeLines[len(wholeLines)-len(got):]
	for i := range got {
		if got[i] != wantSuffix[i] {
			t.Errorf("line[%d] = %q, want %q (exact tail suffix)", i, got[i], wantSuffix[i])
		}
	}
	if got[len(got)-1] != wholeLines[len(wholeLines)-1] {
		t.Errorf("last line = %q, want the file's actual last record %q", got[len(got)-1], wholeLines[len(wholeLines)-1])
	}
}

// TestTailLines_AbsentAndEmptyYieldNoLines (case c): an absent file and an
// empty file both return no lines, the one outcome both doctor readers treat
// as "(none recorded)".
func TestTailLines_AbsentAndEmptyYieldNoLines(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "does-not-exist.jsonl")
	if got := tailLines(absent, enforcementTailBytes, 5); got != nil {
		t.Errorf("absent file: tailLines = %v, want nil", got)
	}

	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tailLines(empty, enforcementTailBytes, 5); got != nil {
		t.Errorf("empty file: tailLines = %v, want nil", got)
	}
}

// TestTailLines_NoCompleteLineInWindow: a window entirely consumed by one
// partial line (no newline anywhere in it) returns no lines, not a fragment.
func TestTailLines_NoCompleteLineInWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enforcements.jsonl")
	// One giant line, no trailing newline, far larger than the tiny cap below.
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tailLines(path, 50, 5); got != nil {
		t.Errorf("no-complete-line window: tailLines = %v, want nil", got)
	}
}

// TestEnforcementTailBytes_IsNotAnEgressBound (insight 13): the tail cap must
// not be spelled with, or reused as, an egress bound.
//
// What this can and cannot check: it pins the value, which is what stops the
// constant being quietly re-aimed at an egress path by giving it MaxRedactBody's
// or capBody's number. It CANNOT detect a future reuse of the identifier itself
// somewhere else -- no unit test can. That half is enforced by review and by the
// distinct spelling: grep enforcementTailBytes and it should have exactly one
// production reference, in tailread.go. This bound owns a local read of an audit
// file, never bytes on the wire.
func TestEnforcementTailBytes_IsNotAnEgressBound(t *testing.T) {
	if enforcementTailBytes != 64<<10 {
		t.Errorf("enforcementTailBytes = %d, want the decided 64 KiB (see the Tail cap decision)", enforcementTailBytes)
	}
}
