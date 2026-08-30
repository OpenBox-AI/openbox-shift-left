package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Two `openbox init` runs against one project used to be able to splice their
// output together: the write was a truncate-then-write, so both truncated to
// zero and then wrote at their own offsets, and the shorter document landed
// inside the longer one. The file ended as a complete JSON document followed by
// the tail of another — invalid, which makes Claude Code drop EVERY hook in it
// for that project. Governance that reports itself as nothing is the failure
// this product exists to prevent, so the write commits through a rename now.
//
// The reader goroutine is the assertion that matters: it is not enough that the
// file is valid once the dust settles, because a session starting mid-install
// reads it exactly then.
func TestConcurrentWriteLocalHooksNeverPublishesAnUnparsableSettingsFile(t *testing.T) {
	project := t.TempDir()
	settings := filepath.Join(project, ".claude", "settings.local.json")

	// Seed the file so the reader always has something to parse, and give the
	// writers different engine paths so each produces a DIFFERENT length —
	// equal-length writes could interleave undetectably.
	if err := writeLocalHooks(project, "/opt/openbox/bin/openbox"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	engines := []string{
		"/a/openbox",
		"/a/much/longer/path/to/the/openbox/engine/binary/openbox",
		"/mid/length/openbox",
		"/b/openbox",
	}

	stop := make(chan struct{})
	var readErr error
	var readMu sync.Mutex
	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(settings)
			if os.IsNotExist(err) {
				continue // between create and rename is not a torn read
			}
			if err != nil {
				continue
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				readMu.Lock()
				if readErr == nil {
					readErr = err
				}
				readMu.Unlock()
				return
			}
		}
	}()

	var writers sync.WaitGroup
	for i := 0; i < 8; i++ {
		for _, engine := range engines {
			writers.Add(1)
			go func(engine string) {
				defer writers.Done()
				// Errors are not the subject here (concurrent runs may legitimately
				// fail); publishing an unparsable file is.
				_ = writeLocalHooks(project, engine)
			}(engine)
		}
	}
	writers.Wait()
	close(stop)
	reader.Wait()

	readMu.Lock()
	defer readMu.Unlock()
	if readErr != nil {
		t.Fatalf("a reader saw an unparsable settings file mid-write: %v", readErr)
	}

	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var final map[string]any
	if err := json.Unmarshal(raw, &final); err != nil {
		t.Fatalf("settings file is not valid JSON after concurrent writes: %v\n%s", err, raw)
	}
	if _, ok := final["hooks"]; !ok {
		t.Errorf("last writer left no hooks key:\n%s", raw)
	}

	// The rename's temp file is an implementation detail that must not survive
	// into the project the developer has checked out.
	entries, err := os.ReadDir(filepath.Dir(settings))
	if err != nil {
		t.Fatalf("read .claude dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "settings.local.json" {
			t.Errorf("leftover file in .claude/: %s", e.Name())
		}
	}
}

// The atomic write must preserve the mode the previous plain write published —
// settings.local.json is not a secret, and other tools read it. CreateTemp
// makes 0600, so this fails without the explicit chmod.
func TestWriteLocalHooksPublishesAReadableSettingsFile(t *testing.T) {
	project := t.TempDir()
	if err := writeLocalHooks(project, "/opt/openbox/bin/openbox"); err != nil {
		t.Fatalf("writeLocalHooks: %v", err)
	}
	info, err := os.Stat(filepath.Join(project, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("stat settings: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("settings mode = %v, want 0644 (CreateTemp's 0600 must not leak through)", perm)
	}
}
