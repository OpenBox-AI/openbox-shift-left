package hookflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if _, halted := SessionHalted("s-2"); halted {
		t.Error("a different session reads halted")
	}
}

// TestSessionHaltCorruptLatchStillHalts a latch that exists but will not parse
// still halts: presence is the decided state, and a corrupt file must not
// quietly un-halt a session the control plane terminated.
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

// TestSessionHaltEmptyLatchStillHaltsWithAGenericReason: WriteSessionHaltIfAbsent's
// create-then-write (O_CREATE|O_EXCL, then a separate Write) leaves a brief
// window where a concurrent reader can observe a present but EMPTY (0-byte)
// file -- unlike WriteSessionHalt's own rename-into-place, which a reader
// never observes mid-write. Presence must still halt, and the replayed
// reason must still be generic and non-empty, exactly as for a corrupt
// latch: an operator (or the next gated call) must never see a blank reason.
func TestSessionHaltEmptyLatchStillHaltsWithAGenericReason(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(devconfig.EnvHaltDir, dir)

	f, err := os.OpenFile(haltPath("s-empty-window"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close() // deliberately never written to: the exact window under test

	info, halted := SessionHalted("s-empty-window")
	if !halted {
		t.Fatal("a present-but-empty latch must still halt (presence is the decided state)")
	}
	dec := SessionHaltDecision(info)
	if dec.Evaluation.Verdict != client.VerdictHalt || !dec.SessionHalt || dec.Source != SourceSessionHalt {
		t.Errorf("replayed decision = %+v, want a session-halting HALT sourced %q", dec, SourceSessionHalt)
	}
	if dec.Evaluation.Reason == "" {
		t.Error("an empty latch must replay with the generic reason, not a blank one")
	}
}

// TestSessionHaltNoCollisionAcrossSanitizedIDs two session ids that sanitize
// to the same filename component must not share a latch; a collision would
// halt an innocent session.
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

// TestSessionHaltPathIsConfinedToHaltDir the latch must stay inside its
// directory whatever the session id contains.
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

// TestSessionHaltWriteUsesAtomicWriteFile pins that the latch file lands
// through atomicWriteFile (rename-into-place), not a direct os.WriteFile:
// mode and content must come out exactly as atomicWriteFile leaves them.
func TestSessionHaltWriteUsesAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(devconfig.EnvHaltDir, dir)
	WriteSessionHalt(nopLogger(), "s-atomic", client.Evaluation{Reason: "kill switch", PolicyID: "p-1"})

	path := haltPath("s-atomic")
	assertMode(t, path, 0o600)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var info SessionHaltInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("latch file did not parse: %v (raw=%q)", err, raw)
	}
	if info.Reason != "kill switch" || info.PolicyID != "p-1" {
		t.Errorf("latch info = %+v, want the written reason and policy id", info)
	}

	// No temp file left behind alongside the committed one (renameio/the
	// Windows fallback both clean up on success).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("halt dir has %d entries, want exactly the committed latch file: %v", len(entries), entries)
	}
}

// TestSessionHaltLatchNeverObservedPartiallyWritten pins the reason
// WriteSessionHalt must go through an atomic (rename-into-place) writer
// rather than os.WriteFile: with a lane daemon and a hook flusher both able
// to latch the same run concurrently, a reader must only ever observe a
// complete, previously-committed file or nothing at all -- never bytes from
// a write still in flight. Each writer's payload is large enough to force
// more than a single underlying write() so a non-atomic writer would be
// likely to expose a torn read under contention.
func TestSessionHaltLatchNeverObservedPartiallyWritten(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	const sessionID = "s-concurrent"

	const writers = 8
	const writesPerWriter = 25
	padding := strings.Repeat("x", 64*1024) // forces a multi-chunk write

	stop := make(chan struct{})
	var readerErr error
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(haltPath(sessionID))
			if err != nil {
				continue // not written yet: acceptable, never a torn read
			}
			var info SessionHaltInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				readerErr = fmt.Errorf("observed a non-JSON (partially written) latch file: %w", err)
				return
			}
		}
	}()

	var writersWG sync.WaitGroup
	for i := 0; i < writers; i++ {
		writersWG.Add(1)
		go func(i int) {
			defer writersWG.Done()
			for j := 0; j < writesPerWriter; j++ {
				WriteSessionHalt(nopLogger(), sessionID, client.Evaluation{
					Reason:   fmt.Sprintf("writer-%d-write-%d-%s", i, j, padding),
					PolicyID: "p-race",
				})
			}
		}(i)
	}
	writersWG.Wait()
	close(stop)
	readerWG.Wait()

	if readerErr != nil {
		t.Fatal(readerErr)
	}
	if _, halted := SessionHalted(sessionID); !halted {
		t.Fatal("session must read halted after concurrent writers finished")
	}
}
