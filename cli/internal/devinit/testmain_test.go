package devinit

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestMain is the ASSERTED-hermeticity control from incident INC-SL7A-DEVJSON
// (G_SEC SL7-A F4): this suite's mock-backend integration test once assumed
// "codex is a Stub ⇒ Install() is a no-op" and, when STORY-SL7-A made codex a
// real installer, wrote the developer's actual ~/.codex/hooks.json and
// ~/.config/openbox/dev.json. The same assumption now rests on the cursor Stub
// and would re-arm at SL-8's registry swap — so the property is made
// STRUCTURAL here:
//
//  1. CONTAIN — HOME / XDG_CONFIG_HOME / CODEX_HOME are pointed at a throwaway
//     sentinel dir before any test runs, so a future Stub-flip escapes into
//     the sentinel, never into the developer's real home; and
//  2. ASSERT — any file found under the sentinel after the run FAILS the suite
//     loudly, so the escape is fixed rather than silently contained.
//
// Tests that pin their own paths (t.Setenv / injected Installer paths)
// override the sentinel per-test as before.
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
