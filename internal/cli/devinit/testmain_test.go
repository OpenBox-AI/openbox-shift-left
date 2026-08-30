package devinit

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestMain is the asserted-hermeticity control from incident INC-sl7a-devjson
// (G_SEC SL7-A F4): this suite's mock-backend integration test once assumed
// "codex is a Stub ⇒ Install() is a no-op" and, when story-SL7-A made codex a
// real installer, wrote the developer's actual ~/.codex/hooks.json and
// ~/.config/openbox/dev.json.
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
		fmt.Fprintf(os.Stderr, "HERMETICITY VIOLATION (INC-SL7A-DEVJSON guard): %d file(s) written under the sentinel HOME; a test escaped its path pinning and would have touched the real home dir:\n", len(leaks))
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
