package claudecode

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestMain is the asserted-hermeticity control for this package, the twin of
// adapters/codex's. Codex grew one after its stub-era tests drove a real
// installer at DEFAULT paths and wrote the developer's actual home; this
// package never got the same guard, and the same class of escape recurred here
// at a far larger scale — a full test run wrote hook registrations into the
// checked-out source tree, ~10MB engine copies into the real
// ~/.claude/plugins/openbox-observe/bin, and thousands of enforcement records
// into the developer's real audit sink. Hundreds of concurrent writers in one
// directory then convoyed on an APFS lock and took the machine down.
//
// Two halves, because containment alone hides the bug:
//
//  1. CONTAIN — before any test runs, every ambient path `init` and the hook
//     engine resolve from is pointed at a throwaway sentinel: HOME (which
//     os.UserConfigDir derives the spool, advisories.jsonl and
//     enforcements.jsonl from, and which the installer derives
//     ~/.claude/plugins from), XDG_CONFIG_HOME, and the WORKING DIRECTORY —
//     `init` defaults to project scope and takes the project from cwd, which
//     under `go test` is this package's own source directory.
//  2. ASSERT — after the run, any file under the sentinel fails the suite,
//     naming what escaped. A test that slips its own pinning is then fixed
//     rather than silently contained, which is the only reason the escape
//     stayed invisible for as long as it did.
//
// Per-test pinning (t.Setenv, explicit PluginDir/ConfigPath, t.TempDir project
// dirs) stays the first line of defence. This catches what gets past it.
func TestMain(m *testing.M) {
	sentinel, err := os.MkdirTemp("", "openbox-hermetic-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermeticity guard: cannot create sentinel dir: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("HOME", sentinel)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(sentinel, "xdg-config"))

	// Contain cwd too, inside the sentinel so a stray project-scoped write is
	// caught by the same assertion rather than needing its own check.
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
