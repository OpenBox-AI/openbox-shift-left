package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

// TestMain is the asserted-hermeticity control for this package, matching the
// ones in adapters/claude-code and cli/cmd/openbox.
//
// Every test here builds the real hook binary and runs it as a CHILD with
// os.Environ() plus a few pins. The pins cover the spool and dev config; they
// never covered the enforcement, advisory and pending-approval sinks, which
// resolve from os.UserConfigDir() rather than OPENBOX_HOME (a deliberate split,
// see devconfig/paths.go). So every run appended real enforcement records to
// the developer's own audit trail — thousands of them, from a session id that
// only exists in a fixture.
//
// Containment is done at the process level rather than at the five child-env
// construction sites, because children inherit it automatically and a sixth
// site added later is covered without anyone remembering to.
//
//   - HOME moves to a sentinel, so anything deriving from it lands there;
//   - the three ambient sinks are pinned to their own throwaway dir, OUTSIDE
//     the sentinel, so writing them is contained without weakening the
//     assertion below;
//   - after the run, any file under the sentinel fails the suite, naming what
//     escaped.
//
// GOCACHE/GOPATH are resolved before HOME moves: they derive from it, and the
// `go build` each test runs would otherwise face an empty cache in a temp dir.
func TestMain(m *testing.M) {
	if os.Getenv("GOCACHE") == "" {
		if dir, err := os.UserCacheDir(); err == nil && dir != "" {
			os.Setenv("GOCACHE", filepath.Join(dir, "go-build"))
		}
	}
	if os.Getenv("GOPATH") == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			os.Setenv("GOPATH", filepath.Join(home, "go"))
		}
	}

	sentinel, err := os.MkdirTemp("", "openbox-hermetic-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermeticity guard: cannot create sentinel dir: %v\n", err)
		os.Exit(1)
	}
	sinks, err := os.MkdirTemp("", "openbox-test-sinks-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermeticity guard: cannot create sink dir: %v\n", err)
		os.Exit(1)
	}
	xdgConfig := filepath.Join(sentinel, "xdg-config")
	os.Setenv("HOME", sentinel)
	os.Setenv("XDG_CONFIG_HOME", xdgConfig)
	os.Setenv(devconfig.EnvEnforcementFile, filepath.Join(sinks, "enforcements.jsonl"))
	os.Setenv(devconfig.EnvPendingApprovalDir, filepath.Join(sinks, "pending-approvals"))
	os.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(sinks, "advisories.jsonl"))
	// The session registry is the fourth of these, and it was found by this
	// guard rather than by enumeration — which is the argument for asserting
	// instead of only containing.
	os.Setenv("OPENBOX_SESSION_DIR", filepath.Join(sinks, "sessions"))

	code := m.Run()

	if leaks := filesUnder(sentinel, goConfigDirs(sentinel, xdgConfig)); len(leaks) > 0 {
		fmt.Fprintf(os.Stderr, "HERMETICITY VIOLATION: %d file(s) written under the sentinel HOME — "+
			"a test (or a child it spawned) escaped its path pinning and would have written into the developer's real home:\n", len(leaks))
		for _, l := range leaks {
			fmt.Fprintf(os.Stderr, "  %s\n", l)
		}
		if code == 0 {
			code = 1
		}
	}
	os.RemoveAll(sentinel)
	os.RemoveAll(sinks)
	os.Exit(code)
}

// filesUnder returns every regular file below root (relative paths), skipping
// anything inside skipDirs — each test shells out to the go command, and
// flagging its own bookkeeping would fail the guard for a reason nobody can act
// on.
func filesUnder(root string, skipDirs []string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			out = append(out, fmt.Sprintf("(walk error at %s: %v)", path, err))
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		for _, prefix := range skipDirs {
			if strings.HasPrefix(rel, prefix) {
				return nil
			}
		}
		out = append(out, rel)
		return nil
	})
	return out
}

// goConfigDirs returns the sentinel-relative directories where the Go toolchain
// keeps its own bookkeeping. Skipping only a `go/` directory cannot mask us:
// our own writes land under `openbox/`.
//
// The XDG entry is DERIVED from the config root this guard sets, never from
// XDG's default. The guard overrides XDG_CONFIG_HOME, so a hardcoded
// ".config/go" matches nothing on linux — while macOS keeps passing because
// os.UserConfigDir ignores XDG there. That asymmetry is exactly how this guard
// came to be green on darwin and red on linux CI.
func goConfigDirs(sentinel, xdgConfig string) []string {
	sep := string(filepath.Separator)
	dirs := []string{
		filepath.Join("Library", "Application Support", "go") + sep, // macOS
		filepath.Join(".config", "go") + sep,                        // a child that clears XDG_CONFIG_HOME
	}
	if rel, err := filepath.Rel(sentinel, xdgConfig); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		dirs = append(dirs, filepath.Join(rel, "go")+sep)
	}
	return dirs
}
