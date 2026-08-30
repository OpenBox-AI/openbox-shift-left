package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentWriteLocalHooksNeverPublishesAnUnparsableSettingsFile two
// `openbox init` runs against one project used to be able to splice their
// output together: the write was a truncate-then-write, so both truncated to
// zero and then wrote at their own offsets, and the shorter document landed
// inside the longer one.
func TestConcurrentWriteLocalHooksNeverPublishesAnUnparsableSettingsFile(t *testing.T) {
	project := t.TempDir()
	settings := filepath.Join(project, ".claude", "settings.local.json")

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

// TestWriteLocalHooksPublishesAReadableSettingsFile the atomic write must
// preserve the mode the previous plain write published; settings.local.json is
// not a secret, and other tools read it.
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
