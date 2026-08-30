package claudecode

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestMain is the asserted-hermeticity control for this package, the twin of
// internal/adapters/codex's.
func TestMain(m *testing.M) {
	sentinel, err := os.MkdirTemp("", "openbox-hermetic-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermeticity guard: cannot create sentinel dir: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("HOME", sentinel)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(sentinel, "xdg-config"))

	originalWD, wdErr := os.Getwd()
	cwd := filepath.Join(sentinel, "cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "hermeticity guard: cannot create sentinel cwd: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chdir(cwd); err != nil {
		fmt.Fprintf(os.Stderr, "hermeticity guard: cannot enter sentinel cwd: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if leaks := filesUnder(sentinel); len(leaks) > 0 {
		fmt.Fprintf(os.Stderr, "HERMETICITY VIOLATION: %d file(s) written under the sentinel HOME/cwd — "+
			"a test escaped its path pinning and would have written into the developer's real home or source tree:\n", len(leaks))
		for _, l := range leaks {
			fmt.Fprintf(os.Stderr, "  %s\n", l)
		}
		if code == 0 {
			code = 1
		}
	}
	if wdErr == nil {
		_ = os.Chdir(originalWD) // leave cwd removable
	}
	os.RemoveAll(sentinel)
	os.Exit(code)
}

func filesUnder(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			out = append(out, fmt.Sprintf("(walk error at %s: %v)", path, err))
			return nil
		}
		if !d.IsDir() {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			out = append(out, rel)
		}
		return nil
	})
	return out
}
