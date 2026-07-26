package codex

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestMain is the ASSERTED-hermeticity control from incident INC-SL7A-DEVJSON
// (G_SEC SL7-A F4): stub-era tests once drove a real installer at DEFAULT
// paths and wrote the developer's actual ~/.codex/hooks.json and
// ~/.config/openbox/dev.json. Per-test env pinning (t.Setenv in the helpers)
// remains the first line of defense; this guard makes the property STRUCTURAL
// for the whole suite:
//
//  1. CONTAIN — before any test runs, HOME / XDG_CONFIG_HOME / CODEX_HOME are
//     pointed at a throwaway sentinel dir, so a test that escapes its own
//     pinning writes there, never into the developer's real home; and
//  2. ASSERT — after the run, any file found under the sentinel FAILS the
//     suite loudly, so the escape is fixed rather than silently contained.
//
// Tests that pin their own paths (t.Setenv) override the sentinel per-test as
// before; this only catches what slips through.
func TestMain(m *testing.M) {
	sentinel, err := os.MkdirTemp("", "openbox-hermetic-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermeticity guard: cannot create sentinel dir: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("HOME", sentinel)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(sentinel, "xdg-config"))
	os.Setenv("CODEX_HOME", filepath.Join(sentinel, "codex-home"))

	code := m.Run()

	if leaks := filesUnder(sentinel); len(leaks) > 0 {
		fmt.Fprintf(os.Stderr, "HERMETICITY VIOLATION (INC-SL7A-DEVJSON guard): %d file(s) written under the sentinel HOME — a test escaped its path pinning and would have touched the real home dir:\n", len(leaks))
		for _, l := range leaks {
			fmt.Fprintf(os.Stderr, "  %s\n", l)
		}
		if code == 0 {
			code = 1
		}
	}
	os.RemoveAll(sentinel)
	os.Exit(code)
}

// filesUnder returns every regular file below root (relative paths),
// best-effort — a walk error is reported as a pseudo-leak so it is never
// silently ignored.
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
