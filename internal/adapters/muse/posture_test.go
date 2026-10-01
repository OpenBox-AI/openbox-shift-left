package muse

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeMuse puts a `muse` script on PATH that appends a line to a counter file
// per run and then runs body.
func fakeMuse(t *testing.T, body string) (counter string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in for muse")
	}
	bin := t.TempDir()
	counter = filepath.Join(bin, "runs")
	script := "#!/bin/sh\necho x >> " + counter + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "muse"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return counter
}

func runs(t *testing.T, counter string) int {
	t.Helper()
	raw, err := os.ReadFile(counter)
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "x")
}

// A version read within the TTL is reused, so a hook that opens a run does not
// start `muse --version` again; one past the TTL reads it afresh.
func TestProviderVersionIsCached(t *testing.T) {
	counter := fakeMuse(t, "echo 1.4.1")
	dir := t.TempDir()
	now := time.Now()

	for i := 0; i < 3; i++ {
		if v := cachedProviderVersion(dir, now.Add(time.Duration(i)*time.Minute)); v != "1.4.1" {
			t.Fatalf("version = %q", v)
		}
	}
	if n := runs(t, counter); n != 1 {
		t.Errorf("muse ran %d times inside the TTL, want 1", n)
	}
	if v := cachedProviderVersion(dir, now.Add(2*providerVersionTTL)); v != "1.4.1" || runs(t, counter) != 2 {
		t.Errorf("an expired cache was not refreshed: version %q after %d runs", v, runs(t, counter))
	}
}

// A muse that hangs is killed at the bound and reads as no version, and a
// failed read is not remembered.
func TestProviderVersionTimesOut(t *testing.T) {
	fakeMuse(t, "exec sleep 30")
	defer func(prev time.Duration) { providerVersionTimeout = prev }(providerVersionTimeout)
	providerVersionTimeout = 100 * time.Millisecond

	dir := t.TempDir()
	start := time.Now()
	if v := cachedProviderVersion(dir, start); v != "" {
		t.Errorf("a hung muse produced version %q", v)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the bound did not hold: %s", took)
	}
	if _, err := os.Stat(filepath.Join(dir, providerVersionCacheFile)); !os.IsNotExist(err) {
		t.Errorf("a failed read was cached (%v)", err)
	}
}
