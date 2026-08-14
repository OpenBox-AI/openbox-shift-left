package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain is the asserted-hermeticity control for the package that owns
// `init`, matching the one in adapters/claude-code and adapters/codex.
//
// isolateHome is the per-test lever and stays the first line of defence, but it
// only protects tests that CALL it. A test that forgets writes to the
// developer's real audit sink and — through the real installer — their real
// ~/.claude/plugins bundle. Containing HOME here makes the property structural
// for the whole package, and asserting on the sentinel afterwards means an
// escape is reported rather than quietly absorbed.
//
// Two deliberate differences from the adapter guard:
//
//   - It does NOT redirect the working directory. Two tests here resolve paths
//     relative to the package dir (the codex fixtures at ../../../adapters, and
//     the end-to-end test that `go build`s this package). cwd containment
//     therefore stays per-test, in isolateHome, where it can apply only to the
//     tests that run `init`.
//   - It pins the Go tool's caches to their real locations BEFORE moving HOME.
//     GOCACHE and GOPATH derive from HOME, so relocating it without this points
//     the `go build` inside the end-to-end test at an empty cache under a temp
//     dir — a slow full rebuild, and a failure if that dir is gone by then.
func TestMain(m *testing.M) {
	// Ambient agent context must not reach these tests; see envscrub_test.go for
	// why, and for the test that asserts it happened.
	scrubAmbientSessionEnv()

	// Resolve the real cache locations first: after HOME moves, these lookups
	// would answer with the sentinel.
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
	os.Setenv("HOME", sentinel)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(sentinel, "xdg-config"))

	code := m.Run()

	if leaks := filesUnder(sentinel); len(leaks) > 0 {
		fmt.Fprintf(os.Stderr, "HERMETICITY VIOLATION: %d file(s) written under the sentinel HOME — "+
			"a test escaped isolateHome and would have written into the developer's real home:\n", len(leaks))
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
			if isToolchainArtifact(rel) {
				return nil
			}
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// isToolchainArtifact reports whether a file under the sentinel belongs to the
// Go toolchain rather than to us. The end-to-end test shells out to `go build`,
// and the go command writes its own counter files under the relocated HOME.
// Those are not an escape by our code, and flagging them would make the guard
// fail for a reason no one can act on — the fastest way to get a useful guard
// deleted.
func isToolchainArtifact(rel string) bool {
	for _, prefix := range []string{
		filepath.Join("Library", "Application Support", "go") + string(filepath.Separator), // macOS
		filepath.Join(".config", "go") + string(filepath.Separator),                        // linux/XDG
	} {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}
