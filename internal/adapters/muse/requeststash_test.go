package muse

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fakeStashClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	now := start
	prev := stashNow
	stashNow = func() time.Time { return now }
	t.Cleanup(func() { stashNow = prev })
	return &now
}

func TestRequestStashRoundTripAndTakeDeletes(t *testing.T) {
	spool := t.TempDir()
	fakeStashClock(t, time.Unix(1790726400, 0))
	in := RequestEntry{SessionID: "s1", ChildSessionID: "c1", TurnID: "turn-0001", Body: `{"messages":[]}`}
	if err := PutRequest(spool, "resp_1", in, true); err != nil {
		t.Fatal(err)
	}
	got, ok := TakeRequest(spool, "resp_1")
	if !ok || got.SessionID != "s1" || got.ChildSessionID != "c1" || got.TurnID != "turn-0001" || got.Body != in.Body || got.WrittenAt.IsZero() {
		t.Fatalf("take = %+v ok=%v", got, ok)
	}
	if _, ok := TakeRequest(spool, "resp_1"); ok {
		t.Error("a taken entry came back")
	}
	if _, ok := TakeRequest(spool, "never"); ok {
		t.Error("a missing entry reported found")
	}
	if _, ok := TakeRequest("", "resp_1"); ok {
		t.Error("empty spool reported found")
	}
}

func TestRequestStashFileIs0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	spool := t.TempDir()
	if err := PutRequest(spool, "resp_1", RequestEntry{SessionID: "s", Body: "b"}, true); err != nil {
		t.Fatal(err)
	}
	var seen int
	filepath.WalkDir(filepath.Join(spool, ".openbox", "requests"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		seen++
		if info, _ := d.Info(); info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v", p, info.Mode().Perm())
		}
		return nil
	})
	if seen != 1 {
		t.Errorf("stash files = %d", seen)
	}
}

func TestRequestStashCaptureOffWritesNothing(t *testing.T) {
	spool := t.TempDir()
	if err := PutRequest(spool, "resp_1", RequestEntry{SessionID: "s", Body: "b"}, false); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Errorf("capture off wrote %d entries", len(entries))
	}
}

func TestRequestStashRefusesEchoAndEmptyIDs(t *testing.T) {
	spool := t.TempDir()
	for _, id := range []string{EchoResponseID, ""} {
		err := PutRequest(spool, id, RequestEntry{SessionID: "s", Body: "b"}, true)
		if !errors.Is(err, ErrUnjoinableResponseID) {
			t.Errorf("id %q: err = %v", id, err)
		}
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Errorf("a refused id wrote %d entries", len(entries))
	}
}

func TestRequestStashEmptyBodyWritesNothing(t *testing.T) {
	spool := t.TempDir()
	if err := PutRequest(spool, "resp_1", RequestEntry{SessionID: "s"}, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := TakeRequest(spool, "resp_1"); ok {
		t.Error("an empty body was stashed")
	}
}

func TestRequestStashExpiredEntryIsNotReturnedAndIsSweptOnPut(t *testing.T) {
	spool := t.TempDir()
	now := fakeStashClock(t, time.Unix(1790726400, 0))
	if err := PutRequest(spool, "old", RequestEntry{SessionID: "s", Body: "b"}, true); err != nil {
		t.Fatal(err)
	}
	if err := PutRequest(spool, "old2", RequestEntry{SessionID: "s", Body: "b"}, true); err != nil {
		t.Fatal(err)
	}
	// Age both files on disk, then move the clock past the TTL.
	dir := filepath.Join(spool, ".openbox", "requests")
	aged := now.Add(-2 * requestTTL)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.Chtimes(filepath.Join(dir, e.Name()), aged, aged)
	}
	*now = now.Add(requestTTL + time.Minute)
	if _, ok := TakeRequest(spool, "old"); ok {
		t.Error("an expired entry was returned")
	}
	if err := PutRequest(spool, "new", RequestEntry{SessionID: "s", Body: "b"}, true); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 1 {
		t.Errorf("after the sweep %d entries remain, want only the new one", len(left))
	}
	if _, ok := TakeRequest(spool, "new"); !ok {
		t.Error("the fresh entry was lost to the sweep")
	}
}

func TestRequestStashSweepIsBounded(t *testing.T) {
	spool := t.TempDir()
	now := fakeStashClock(t, time.Unix(1790726400, 0))
	dir := filepath.Join(spool, ".openbox", "requests")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	aged := now.Add(-2 * requestTTL)
	for i := 0; i < maxStashSweep+50; i++ {
		p := filepath.Join(dir, strings.Repeat("a", 8)+string(rune('a'+i%26))+strings.Repeat("b", i/26+1))
		os.WriteFile(p, []byte("{}"), 0o600)
		os.Chtimes(p, aged, aged)
	}
	if err := PutRequest(spool, "r", RequestEntry{SessionID: "s", Body: "b"}, true); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(dir)
	if len(left) < 50 || len(left) > maxStashSweep+50 {
		t.Errorf("remaining = %d: sweep examined more or fewer than its bound", len(left))
	}
}

func TestTurnIndexAppendsInOrderAndTakeDeletes(t *testing.T) {
	spool := t.TempDir()
	for _, id := range []string{"resp_1", "resp_2", "resp_1"} {
		if err := AppendTurnResponse(spool, "s1", "turn-0001", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := AppendTurnResponse(spool, "s1", "turn-0002", "resp_9"); err != nil {
		t.Fatal(err)
	}
	got := TakeTurnResponses(spool, "s1", "turn-0001")
	if strings.Join(got, ",") != "resp_1,resp_2" {
		t.Errorf("turn 1 = %v", got)
	}
	if again := TakeTurnResponses(spool, "s1", "turn-0001"); len(again) != 0 {
		t.Errorf("a taken index came back: %v", again)
	}
	if other := TakeTurnResponses(spool, "s1", "turn-0002"); len(other) != 1 || other[0] != "resp_9" {
		t.Errorf("turn 2 = %v", other)
	}
	if other := TakeTurnResponses(spool, "s2", "turn-0001"); len(other) != 0 {
		t.Errorf("another session saw %v", other)
	}
}

func TestTurnIndexRefusesEchoAndEmptyAndCarriesNoContent(t *testing.T) {
	spool := t.TempDir()
	if err := AppendTurnResponse(spool, "s", "t", EchoResponseID); !errors.Is(err, ErrUnjoinableResponseID) {
		t.Errorf("echo err = %v", err)
	}
	if err := AppendTurnResponse(spool, "", "t", "r"); err == nil {
		t.Error("empty session accepted")
	}
	if err := AppendTurnResponse(spool, "s", "", "r"); err == nil {
		t.Error("empty turn accepted")
	}
	if err := AppendTurnResponse(spool, "s", "t", "resp_x"); err != nil {
		t.Fatal(err)
	}
	// Written regardless of capture: it holds an id and nothing else.
	filepath.WalkDir(filepath.Join(spool, ".openbox", "turnresponses"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			if strings.TrimSpace(string(b)) != "resp_x" {
				t.Errorf("index content = %q", b)
			}
			if runtime.GOOS != "windows" {
				if info, _ := d.Info(); info.Mode().Perm() != 0o600 {
					t.Errorf("index mode = %v", info.Mode().Perm())
				}
			}
		}
		return nil
	})
}

func TestTurnIndexSweepsStaleEntries(t *testing.T) {
	spool := t.TempDir()
	now := fakeStashClock(t, time.Unix(1790726400, 0))
	if err := AppendTurnResponse(spool, "s1", "turn-old", "r1"); err != nil {
		t.Fatal(err)
	}
	var stale string
	filepath.WalkDir(filepath.Join(spool, ".openbox", "turnresponses"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			stale = p
		}
		return nil
	})
	aged := now.Add(-2 * turnIndexTTL)
	os.Chtimes(stale, aged, aged)
	if err := AppendTurnResponse(spool, "s1", "turn-new", "r2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale index survived the sweep: %v", err)
	}
	if got := TakeTurnResponses(spool, "s1", "turn-new"); len(got) != 1 {
		t.Errorf("fresh index lost: %v", got)
	}
}
