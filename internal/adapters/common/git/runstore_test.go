package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunStore_ReadAbsentIsGeneration0NoError is the ordinary, overwhelmingly
// common state: a session that has never been continued has no run record at
// all, and that must read as generation 0 WITHOUT an error -- an error here
// would make every hook of every ordinary session log a stderr line.
func TestRunStore_ReadAbsentIsGeneration0NoError(t *testing.T) {
	s := RunStore{Dir: t.TempDir()}
	rec, err := s.Read("sess-1")
	if err != nil {
		t.Fatalf("Read of an absent record returned an error: %v", err)
	}
	if rec.Generation != 0 || rec.RunID != "" {
		t.Errorf("Read of an absent record = %+v, want generation 0 and no run id", rec)
	}
}

// TestRunStore_BumpTwiceIsTheSecondInvocation is the registry-level half of
// the second-invocation check:
// two bumps against one store produce generations 1 then 2, two distinct
// UUIDs neither equal to the session id, and PreviousRunID = the session id
// then = the first UUID. The assertion that matters is on the SECOND call.
func TestRunStore_BumpTwiceIsTheSecondInvocation(t *testing.T) {
	s := RunStore{Dir: t.TempDir()}

	first, err := s.Bump("sess-1")
	if err != nil {
		t.Fatalf("first Bump: %v", err)
	}
	if first.Generation != 1 {
		t.Errorf("first Bump generation = %d, want 1", first.Generation)
	}
	if first.RunID == "" || first.RunID == "sess-1" {
		t.Errorf("first Bump run id = %q, want a fresh UUID distinct from the session id", first.RunID)
	}
	if first.PreviousRunID != "sess-1" {
		t.Errorf("first Bump previous_run_id = %q, want the session id (no prior run)", first.PreviousRunID)
	}

	second, err := s.Bump("sess-1")
	if err != nil {
		t.Fatalf("second Bump: %v", err)
	}
	if second.Generation != 2 {
		t.Errorf("second Bump generation = %d, want 2 (the second invocation must advance, not restart)", second.Generation)
	}
	if second.RunID == "" || second.RunID == first.RunID || second.RunID == "sess-1" {
		t.Errorf("second Bump run id = %q, want a THIRD distinct UUID (first was %q)", second.RunID, first.RunID)
	}
	if second.PreviousRunID != first.RunID {
		t.Errorf("second Bump previous_run_id = %q, want the first bump's run id %q", second.PreviousRunID, first.RunID)
	}

	// Read path agrees with the write path -- a version that passes with the
	// write stubbed out (e.g. Read answering from memory instead of disk) is a
	// defect; force a real re-read from a FRESH RunStore value.
	reread, err := (RunStore{Dir: s.Dir}).Read("sess-1")
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread != second {
		t.Errorf("re-read from disk = %+v, want it to equal the second Bump's return %+v", reread, second)
	}
}

// TestRunStore_BumpAbsentRecordMintsGeneration1: resume (or clear)
// with no record mints a new run at generation 1, PreviousRunID = the session
// id -- not a collision with a sealed generation-0 run, because the minted id
// is a fresh UUID either way.
func TestRunStore_BumpAbsentRecordMintsGeneration1(t *testing.T) {
	s := RunStore{Dir: t.TempDir()}
	rec, err := s.Bump("sess-resume")
	if err != nil {
		t.Fatalf("Bump: %v", err)
	}
	if rec.Generation != 1 {
		t.Errorf("generation = %d, want 1", rec.Generation)
	}
	if rec.PreviousRunID != "sess-resume" {
		t.Errorf("previous_run_id = %q, want the bare session id", rec.PreviousRunID)
	}
}

// TestRunStore_ReadFailOpen: every malformed-record shape yields
// generation 0 (the zero value) and a non-nil error the caller logs once and
// continues past -- never a block.
func TestRunStore_ReadFailOpen(t *testing.T) {
	t.Run("unreadable directory", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := t.TempDir()
		blocked := filepath.Join(dir, "runs")
		if err := os.MkdirAll(blocked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(blocked, 0o700) }) // TempDir cleanup needs it back
		s := RunStore{Dir: blocked}
		rec, err := s.Read("sess-1")
		if err == nil {
			t.Fatal("Read of an unreadable directory returned no error")
		}
		if rec.Generation != 0 || rec.RunID != "" {
			t.Errorf("Read on error = %+v, want the zero value", rec)
		}
	})

	t.Run("truncated garbage JSON", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(runRecordPath(dir, "sess-1"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := RunStore{Dir: dir}
		rec, err := s.Read("sess-1")
		if err == nil {
			t.Fatal("Read of garbage JSON returned no error")
		}
		if rec.Generation != 0 || rec.RunID != "" {
			t.Errorf("Read on error = %+v, want the zero value", rec)
		}
	})

	t.Run("generation >= 1 with an unparseable run id", func(t *testing.T) {
		dir := t.TempDir()
		writeRawRecord(t, dir, RunRecord{SessionID: "sess-1", Generation: 1, RunID: "not-a-uuid"})
		s := RunStore{Dir: dir}
		rec, err := s.Read("sess-1")
		if err == nil {
			t.Fatal("Read of an inconsistent (gen>=1, bad uuid) record returned no error")
		}
		if rec.Generation != 0 {
			t.Errorf("Read on error = %+v, want the zero value", rec)
		}
	})

	t.Run("generation 0 with a non-empty run id", func(t *testing.T) {
		dir := t.TempDir()
		writeRawRecord(t, dir, RunRecord{SessionID: "sess-1", Generation: 0, RunID: "550e8400-e29b-41d4-a716-446655440000"})
		s := RunStore{Dir: dir}
		rec, err := s.Read("sess-1")
		if err == nil {
			t.Fatal("Read of an inconsistent (gen 0, non-empty run id) record returned no error")
		}
		if rec.Generation != 0 || rec.RunID != "" {
			t.Errorf("Read on error = %+v, want the zero value", rec)
		}
	})

	t.Run("Bump recovers from a corrupt old record instead of propagating it", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(runRecordPath(dir, "sess-1"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := RunStore{Dir: dir}
		rec, err := s.Bump("sess-1")
		if err != nil {
			t.Fatalf("Bump must recover from a corrupt OLD record (fail-open), got: %v", err)
		}
		if rec.Generation != 1 || rec.PreviousRunID != "sess-1" {
			t.Errorf("Bump after a corrupt old record = %+v, want a fresh generation 1 rooted at the session id", rec)
		}
	})
}

// TestRunStore_BumpNewIDErrorFailsOpen: a mint failure must not write a
// partial or wrong record, and must be reported so the caller can fall back
// to generation 0.
func TestRunStore_BumpNewIDErrorFailsOpen(t *testing.T) {
	dir := t.TempDir()
	s := RunStore{
		Dir:   dir,
		NewID: func() (string, error) { return "", os.ErrClosed },
	}
	if _, err := s.Bump("sess-1"); err == nil {
		t.Fatal("Bump with a failing NewID returned no error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed mint left files behind: %v", entries)
	}
}

// TestRunDirIsASubdirectoryNeverAFlatSibling pins the run store's
// structural guarantee: SessionResolver.resolveFromRegistry's directory scan
// (session.go:139) skips runs/ only because it IS a directory. If this ever
// stops being a subdirectory of the session dir, that scan would start trying
// to unmarshal run records as SessionRecords.
func TestRunDirIsASubdirectoryNeverAFlatSibling(t *testing.T) {
	sessionDir := t.TempDir()
	runDir := RunDir(sessionDir)
	if !strings.HasPrefix(runDir, sessionDir+string(filepath.Separator)) {
		t.Fatalf("RunDir(%q) = %q, want a subdirectory of the session dir", sessionDir, runDir)
	}
	if _, err := (RunStore{Dir: runDir}).Bump("s"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	sawDir := false
	for _, e := range entries {
		if e.IsDir() {
			sawDir = true
		} else if strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("a run record landed directly in the session dir (not under runs/): %s", e.Name())
		}
	}
	if !sawDir {
		t.Fatal("runs/ was not created as a subdirectory of the session dir")
	}
}

func writeRawRecord(t *testing.T, dir string, rec RunRecord) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runRecordPath(dir, rec.SessionID), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
