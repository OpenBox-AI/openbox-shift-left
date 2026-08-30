package hookflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func TestSessionHaltLatchRoundTrip(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	if _, halted := SessionHalted("s-1"); halted {
		t.Fatal("an unlatched session reads halted")
	}
	WriteSessionHalt(nopLogger(), "s-1", client.Evaluation{Reason: "kill switch", PolicyID: "p-1"})
	info, halted := SessionHalted("s-1")
	if !halted {
		t.Fatal("latched session not read back as halted")
	}
	if info.Reason != "kill switch" || info.PolicyID != "p-1" || info.TS == "" {
		t.Errorf("latch info = %+v, want the preserved reason, policy id and a timestamp", info)
	}
	// The latch is per-session: a sibling session is untouched.
	if _, halted := SessionHalted("s-2"); halted {
		t.Error("a different session reads halted")
	}
}

// A latch that exists but will not parse still halts: presence is the decided
// state, and a corrupt file must not quietly un-halt a session the control
// plane terminated. The replayed decision then carries the generic reason.
func TestSessionHaltCorruptLatchStillHalts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(devconfig.EnvHaltDir, dir)
	WriteSessionHalt(nopLogger(), "s-corrupt", client.Evaluation{Reason: "orig"})
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly the latch file, got %v (%v)", entries, err)
	}
	if err := os.WriteFile(filepath.Join(dir, entries[0].Name()), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, halted := SessionHalted("s-corrupt")
	if !halted {
		t.Fatal("a present-but-corrupt latch must still halt")
	}
	dec := SessionHaltDecision(info)
	if dec.Evaluation.Verdict != client.VerdictHalt || !dec.SessionHalt || dec.Source != SourceSessionHalt {
		t.Errorf("replayed decision = %+v, want a session-halting HALT sourced %q", dec, SourceSessionHalt)
	}
	if dec.Evaluation.Reason == "" {
		t.Error("a corrupt latch must replay with the generic reason, not an empty one")
	}
}

// Two session ids that sanitize to the same filename component must not share
// a latch — a collision would halt an innocent session. The raw-id hash suffix
// is what keeps them apart.
func TestSessionHaltNoCollisionAcrossSanitizedIDs(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	WriteSessionHalt(nopLogger(), "s/../a", client.Evaluation{Reason: "x"})
	if _, halted := SessionHalted("s:..:a"); halted {
		t.Error("a session sharing only the SANITIZED name reads halted (collision)")
	}
	if _, halted := SessionHalted("s/../a"); !halted {
		t.Error("the latched session itself must read halted")
	}
}

// The latch must stay inside its directory whatever the session id contains.
func TestSessionHaltPathIsConfinedToHaltDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(devconfig.EnvHaltDir, dir)
	WriteSessionHalt(nopLogger(), "../../escape", client.Evaluation{})
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("latch not written inside the halt dir: %v (%v)", entries, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.json")); err == nil {
		t.Error("latch escaped the halt directory")
	}
}

func TestSessionHaltEmptySessionIDNeverLatches(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	WriteSessionHalt(nopLogger(), "", client.Evaluation{Reason: "x"})
	if _, halted := SessionHalted(""); halted {
		t.Error("an empty session id must never read halted")
	}
}
