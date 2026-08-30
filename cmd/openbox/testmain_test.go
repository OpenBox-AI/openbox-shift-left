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
// `init`, matching the one in internal/adapters/claude-code and
// internal/adapters/codex.
//   - It does NOT redirect the working directory.
//   - It pins the Go tool's caches to their real locations before moving HOME.
func TestMain(m *testing.M) {
	scrubAmbientSessionEnv()

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
	xdgConfig := filepath.Join(sentinel, "xdg-config")
	os.Setenv("HOME", sentinel)
	os.Setenv("XDG_CONFIG_HOME", xdgConfig)

	code := m.Run()

	if leaks := filesUnder(sentinel, goConfigDirs(sentinel, xdgConfig)); len(leaks) > 0 {
		fmt.Fprintf(os.Stderr, "HERMETICITY VIOLATION: %d file(s) written under the sentinel HOME; "+
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

func filesUnder(root string, skipDirs []string) []string {
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
			for _, prefix := range skipDirs {
				if strings.HasPrefix(rel, prefix) {
					return nil
				}
			}
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// goConfigDirs returns the sentinel-relative directories where the Go
// toolchain keeps its own bookkeeping. Skipping only a `go/` directory cannot
// mask us: our own writes land under `openbox/`.
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
