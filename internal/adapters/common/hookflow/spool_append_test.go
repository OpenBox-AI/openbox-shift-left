package hookflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// Append and SpoolObserveHead share one write path and differ only in the
// file they target, the wording of their open/write errors, and that only
// Append traces. These pin exactly those differences.

func appendTestEvent() client.DevEvent {
	return client.DevEvent{EventID: "ev-append-1", SessionID: "sess-append-1", EventType: client.EventToolCall}
}

func TestAppendAndObserveHeadTargetTheirOwnFile(t *testing.T) {
	sp := Spool{Dir: filepath.Join(t.TempDir(), "spool")}
	ev := appendTestEvent()

	if err := sp.Append(ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := sp.SpoolObserveHead(ev); err != nil {
		t.Fatalf("SpoolObserveHead: %v", err)
	}
	for _, path := range []string{sp.SessionPath(ev.SessionID), sp.headPath(ev.SessionID)} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := NonEmptyLines(data)
		if len(lines) != 1 || !strings.Contains(string(lines[0]), ev.EventID) {
			t.Errorf("%s holds %q, want exactly one line for %s", filepath.Base(path), data, ev.EventID)
		}
		if !strings.HasSuffix(string(data), "\n") {
			t.Errorf("%s is not newline-terminated", filepath.Base(path))
		}
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(path), fi.Mode().Perm())
		}
	}
}

func TestAppendAndObserveHeadErrorWording(t *testing.T) {
	ev := appendTestEvent()

	// A regular file where the spool directory should be fails the mkdir.
	blocked := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(Spool, client.DevEvent) error{
		"Append": Spool.Append, "SpoolObserveHead": Spool.SpoolObserveHead,
	} {
		err := call(Spool{Dir: blocked}, ev)
		if err == nil || !strings.HasPrefix(err.Error(), "spool mkdir: ") {
			t.Errorf("%s with a file as Dir: err = %v, want prefix %q", name, err, "spool mkdir: ")
		}
	}

	// An unmarshalable event fails before anything is opened.
	bad := ev
	bad.Metadata = map[string]any{"c": make(chan int)}
	for name, call := range map[string]func(Spool, client.DevEvent) error{
		"Append": Spool.Append, "SpoolObserveHead": Spool.SpoolObserveHead,
	} {
		err := call(Spool{Dir: t.TempDir()}, bad)
		if err == nil || !strings.HasPrefix(err.Error(), "spool marshal: ") {
			t.Errorf("%s with an unmarshalable event: err = %v, want prefix %q", name, err, "spool marshal: ")
		}
	}

	// A directory where the target file should be fails the open.
	sp := Spool{Dir: t.TempDir()}
	if err := os.Mkdir(sp.SessionPath(ev.SessionID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sp.headPath(ev.SessionID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sp.Append(ev); err == nil || !strings.HasPrefix(err.Error(), "spool open: ") {
		t.Errorf("Append onto a directory: err = %v, want prefix %q", err, "spool open: ")
	}
	if err := sp.SpoolObserveHead(ev); err == nil || !strings.HasPrefix(err.Error(), "spool open head: ") {
		t.Errorf("SpoolObserveHead onto a directory: err = %v, want prefix %q", err, "spool open head: ")
	}
}

func TestOnlyAppendTracesTheSpoolAppend(t *testing.T) {
	dir := withTraceDir(t)
	sp := Spool{Dir: t.TempDir()}
	ev := appendTestEvent()

	if err := sp.SpoolObserveHead(ev); err != nil {
		t.Fatalf("SpoolObserveHead: %v", err)
	}
	if n := countSpoolAppendRows(t, dir); n != 0 {
		t.Fatalf("SpoolObserveHead traced %d spool.append row(s), want 0", n)
	}
	if err := sp.Append(ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if n := countSpoolAppendRows(t, dir); n != 1 {
		t.Fatalf("Append traced %d spool.append row(s), want 1", n)
	}
}

func countSpoolAppendRows(t *testing.T, dir string) int {
	t.Helper()
	recs, _, err := trace.Read(dir, nil)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("trace.Read: %v", err)
	}
	n := 0
	for _, r := range recs {
		if r.Stage == trace.StageSpoolAppend {
			n++
		}
	}
	return n
}
