package hookflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func TestTurnCursor_RoundTrip(t *testing.T) {
	c := TurnCursor{Dir: t.TempDir()}

	if got := c.Read("s1", ""); got != (TurnPos{}) {
		t.Errorf("fresh cursor = %+v, want zero", got)
	}
	if err := c.Write("s1", "", TurnPos{Offset: 4096, Index: 3}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := c.Read("s1", ""); got != (TurnPos{Offset: 4096, Index: 3}) {
		t.Errorf("read back = %+v, want {4096 3}", got)
	}

	if err := c.Write("s1", "", TurnPos{Offset: 8192, Index: 4}); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got := c.Read("s1", ""); got != (TurnPos{Offset: 8192, Index: 4}) {
		t.Errorf("after replace = %+v, want {8192 4}", got)
	}
}

// TestTurnCursor_AgentWindowsAreIsolated the main thread's window and a
// subagent's must never share a cursor: a subagent's Stop fires against its
// own transcript window, and a shared cursor would hand each the other's
// tokens.
func TestTurnCursor_AgentWindowsAreIsolated(t *testing.T) {
	c := TurnCursor{Dir: t.TempDir()}

	if err := c.Write("s1", "", TurnPos{Offset: 100, Index: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Write("s1", "agt-a", TurnPos{Offset: 900, Index: 7}); err != nil {
		t.Fatal(err)
	}
	if err := c.Write("s1", "agt-b", TurnPos{Offset: 20, Index: 0}); err != nil {
		t.Fatal(err)
	}

	if got := c.Read("s1", ""); got != (TurnPos{Offset: 100, Index: 1}) {
		t.Errorf("main cursor = %+v, want {100 1}", got)
	}
	if got := c.Read("s1", "agt-a"); got != (TurnPos{Offset: 900, Index: 7}) {
		t.Errorf("agt-a cursor = %+v, want {900 7}", got)
	}
	if got := c.Read("s1", "agt-b"); got != (TurnPos{Offset: 20, Index: 0}) {
		t.Errorf("agt-b cursor = %+v, want {20 0}", got)
	}

	if err := c.Write("s1", "main", TurnPos{Offset: 5, Index: 5}); err != nil {
		t.Fatal(err)
	}
	if got := c.Read("s1", ""); got != (TurnPos{Offset: 100, Index: 1}) {
		t.Errorf("an agent named \"main\" overwrote the main-thread cursor: %+v", got)
	}
}

func TestTurnCursor_SessionsAreIsolated(t *testing.T) {
	c := TurnCursor{Dir: t.TempDir()}
	if err := c.Write("s1", "", TurnPos{Offset: 10, Index: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Write("s2", "", TurnPos{Offset: 20, Index: 2}); err != nil {
		t.Fatal(err)
	}
	if got := c.Read("s1", ""); got.Offset != 10 {
		t.Errorf("s1 cursor = %+v", got)
	}
	if got := c.Read("s2", ""); got.Offset != 20 {
		t.Errorf("s2 cursor = %+v", got)
	}
}

// TestTurnCursor_CorruptRecordReadsAsZero a corrupt or negative record must
// read as zero rather than propagating a bad offset: re-reading the window
// over-reports into a server that deduplicates on the deterministic turn id,
// whereas trusting garbage skips real turns silently.
func TestTurnCursor_CorruptRecordReadsAsZero(t *testing.T) {
	c := TurnCursor{Dir: t.TempDir()}
	if err := c.Write("s1", "", TurnPos{Offset: 500, Index: 5}); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{
		"not json at all",
		`{"offset":-1,"index":2}`,
		`{"offset":10,"index":-3}`,
		`{"offset":`, // truncated mid-write
		"",
	} {
		if err := os.WriteFile(c.RecordPath("s1", ""), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := c.Read("s1", ""); got != (TurnPos{}) {
			t.Errorf("corrupt record %q read as %+v, want zero", body, got)
		}
	}
}

func TestTurnCursor_MissingDirIsInert(t *testing.T) {
	var c TurnCursor // zero value: Dir == ""
	if got := c.Read("s1", ""); got != (TurnPos{}) {
		t.Errorf("read with no Dir = %+v", got)
	}
	if err := c.Write("s1", "", TurnPos{Offset: 1, Index: 1}); err != nil {
		t.Errorf("write with no Dir should be a no-op, got %v", err)
	}
	c.ClearSession("s1") // must not panic
}

// TestTurnCursor_ClearSessionSweepsEveryAgent clearSession sweeps the main
// cursor and every subagent's in one call; the SessionEnd contract.
func TestTurnCursor_ClearSessionSweepsEveryAgent(t *testing.T) {
	c := TurnCursor{Dir: t.TempDir()}
	for _, agent := range []string{"", "agt-a", "agt-b"} {
		if err := c.Write("s1", agent, TurnPos{Offset: 1, Index: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Write("s2", "", TurnPos{Offset: 9, Index: 9}); err != nil {
		t.Fatal(err)
	}

	c.ClearSession("s1")

	for _, agent := range []string{"", "agt-a", "agt-b"} {
		if got := c.Read("s1", agent); got != (TurnPos{}) {
			t.Errorf("cursor for agent %q survived ClearSession: %+v", agent, got)
		}
	}
	if _, err := os.Stat(c.SessionDir("s1")); !os.IsNotExist(err) {
		t.Errorf("session cursor dir survived ClearSession (err=%v)", err)
	}
	if got := c.Read("s2", ""); got.Offset != 9 {
		t.Errorf("ClearSession(s1) disturbed s2: %+v", got)
	}
}

// TestTurnCursor_RecordHoldsOnlyNumbers the record must hold integers and
// nothing else (INV-2).
func TestTurnCursor_RecordHoldsOnlyNumbers(t *testing.T) {
	c := TurnCursor{Dir: t.TempDir()}
	if err := c.Write("s1", "agt-a", TurnPos{Offset: 4096, Index: 2}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(c.RecordPath("s1", "agt-a"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(raw))
	if got != `{"offset":4096,"index":2}` {
		t.Errorf("cursor record = %s; want exactly the two integers", got)
	}
}

// TestTurnCursor_IDsCannotTraverse a crafted session or agent id must not
// escape the cursor root.
func TestTurnCursor_IDsCannotTraverse(t *testing.T) {
	root := t.TempDir()
	c := TurnCursor{Dir: root}
	for _, tc := range []struct{ session, agent string }{
		{"../../etc", ""},
		{"s1", "../../../tmp/pwn"},
		{"a/b/c", "d/e"},
	} {
		p := c.RecordPath(tc.session, tc.agent)
		rel, err := filepath.Rel(root, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("RecordPath(%q,%q) = %q escapes the cursor root", tc.session, tc.agent, p)
		}
		if err := c.Write(tc.session, tc.agent, TurnPos{Offset: 1, Index: 1}); err != nil {
			t.Errorf("Write(%q,%q): %v", tc.session, tc.agent, err)
		}
	}
}

// TestEngineSweepsTurnCursorsOnSessionEnd the SessionEnded sweep must reach
// the turn cursors, not only the duration stash.
func TestEngineSweepsTurnCursorsOnSessionEnd(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)

	if err := e.Turns.Write("s1", "", TurnPos{Offset: 128, Index: 2}); err != nil {
		t.Fatal(err)
	}
	if err := e.Turns.Write("s1", "agt-a", TurnPos{Offset: 64, Index: 1}); err != nil {
		t.Fatal(err)
	}

	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       "ev-end",
		EventType:     client.EventSessionEnded,
		SessionID:     "s1",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:     "2026-08-11T10:00:00Z",
	}
	e.ThreadDuration(&ev)

	if got := e.Turns.Read("s1", ""); got != (TurnPos{}) {
		t.Errorf("main cursor survived SessionEnded: %+v", got)
	}
	if got := e.Turns.Read("s1", "agt-a"); got != (TurnPos{}) {
		t.Errorf("subagent cursor survived SessionEnded: %+v", got)
	}
}

// TestEngineTurnCursorLivesUnderSpoolRoot the cursor dir lives under the spool
// root but must never be mistaken for a spool file by the drain (the same
// property the duration stash relies on).
func TestEngineTurnCursorLivesUnderSpoolRoot(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	want := filepath.Join(dir, "turns")
	if e.Turns.Dir != want {
		t.Errorf("Turns.Dir = %q, want %q", e.Turns.Dir, want)
	}
	if e.Turns.Dir == e.Durations.Dir {
		t.Error("turn cursors and the duration stash share a directory")
	}
}
