package atomicfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// The package had no tests at all. What follows is the behaviour both platform
// branches owe their callers, asserted once so the two cannot drift: the
// build-tag split is an implementation detail, and every claim here has to hold
// on either side of it.

func TestWriteCreatesTheFileWithTheRequestedMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	body := []byte(`{"model":"opus"}`)

	if err := Write(path, body, 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("read back %q, want %q", got, body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows mode bits are a fiction; ~/.openbox/.env is unprotected there by
	// design (CLAUDE.md), and this package must not imply otherwise.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600. The activation record can hold a displaced relay URL with an "+
			"embedded credential, and its caller passes 0600 for that reason", info.Mode().Perm())
	}
}

func TestWriteReplacesAnExistingFileWholesale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"old":true,"padding":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"new":true}`)
	if err := Write(path, body, 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("read back %q, want exactly %q: a shorter replacement must not leave the tail of the "+
			"longer original behind, which is what an in-place write would do", got, body)
	}
}

func TestWriteLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := Write(path, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "settings.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want just settings.json: a stray temp file in ~/.claude is "+
			"litter in a directory the developer owns", names)
	}
}

// TestConcurrentWritersNeverPublishAPartialFile is the property the package
// exists for. Every reader of these files refuses to rewrite one it cannot
// parse, so a truncated settings.json blocks its own repair; a reader must
// therefore see either the whole previous document or the whole new one, never
// a prefix of either.
//
// This does not test durability across a crash -- no unit test can -- and the
// package does not claim to close the lost-update race either. It claims the
// file is never observed torn, and that is what is asserted.
func TestConcurrentWritersNeverPublishAPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	docs := make([][]byte, 8)
	for i := range docs {
		pad := make([]byte, 1<<15) // 32 KiB, well past one page
		for j := range pad {
			pad[j] = byte('a' + i)
		}
		b, err := json.Marshal(map[string]any{"writer": i, "pad": string(pad)})
		if err != nil {
			t.Fatal(err)
		}
		docs[i] = b
	}
	if err := Write(path, docs[0], 0o600); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for reads := 0; reads < 2000; reads++ {
			raw, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					t.Errorf("settings.json vanished mid-write; the replacement is not atomic")
					break
				}
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Errorf("read a document that does not parse (%d bytes): %v\n"+
					"a reader that refuses to rewrite unparseable JSON is now stuck", len(raw), err)
				break
			}
		}
		close(stop)
	}()

	for i := range docs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := Write(path, docs[i], 0o600); err != nil {
					t.Errorf("Write from writer %d: %v", i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
