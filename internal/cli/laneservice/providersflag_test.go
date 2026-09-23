package laneservice

import (
	"strings"
	"testing"
)

// TestWithProvidersNilLeavesArgsByteIdentical is the flag-omission contract:
// a caller that never sets Providers (today's every existing test in
// lanes_test.go) must get a unit whose args do not change at all -- a
// daemon's own flag parsing must not see a new flag it was never given.
func TestWithProvidersNilLeavesArgsByteIdentical(t *testing.T) {
	base := Transport("127.0.0.1:8790", "", false)
	got := base.WithProviders(nil)
	if len(got.Args) != len(base.Args) {
		t.Fatalf("WithProviders(nil) changed Args length: %d -> %d", len(base.Args), len(got.Args))
	}
	for i := range base.Args {
		if got.Args[i] != base.Args[i] {
			t.Errorf("WithProviders(nil) changed Args[%d]: %+v -> %+v", i, base.Args[i], got.Args[i])
		}
	}
	home := t.TempDir()
	if got.LaunchdPlist(home, "/bin/openbox") != base.LaunchdPlist(home, "/bin/openbox") {
		t.Error("WithProviders(nil) changed the rendered launchd plist")
	}
	if got.SystemdUnit("/bin/openbox") != base.SystemdUnit("/bin/openbox") {
		t.Error("WithProviders(nil) changed the rendered systemd unit")
	}
}

// TestWithProvidersEmptyIsPresentButBlank covers the "every provider was
// uninstalled" case: the flag must still be carried, with an empty value,
// never omitted -- omitting it would fall back to the daemon's own
// claude-code default instead of intercepting nothing.
func TestWithProvidersEmptyIsPresentButBlank(t *testing.T) {
	got := Transport("127.0.0.1:8790", "", false).WithProviders([]string{})
	unit := got.SystemdUnit("/bin/openbox")
	if !strings.Contains(unit, "--providers") {
		t.Fatalf("an explicitly empty Providers dropped the flag entirely:\n%s", unit)
	}
	last := got.Args[len(got.Args)-1]
	if last.Value != "" {
		t.Errorf("an explicitly empty Providers rendered value %q, want empty", last.Value)
	}
}

// TestWithProvidersJoinsAsCommaList is the multi-provider shape a daemon's
// own --providers flag parses back into a slice.
func TestWithProvidersJoinsAsCommaList(t *testing.T) {
	got := Transport("127.0.0.1:8790", "", false).WithProviders([]string{"claude-code", "codex"})
	last := got.Args[len(got.Args)-1]
	if last.Value != "claude-code,codex" {
		t.Errorf("got %q, want %q", last.Value, "claude-code,codex")
	}
	if !last.Quote {
		t.Error("the --providers value must be quoted like every other caller-supplied value")
	}
	flag := got.Args[len(got.Args)-2]
	if flag.Value != ProvidersFlag || flag.Quote {
		t.Errorf("the --providers flag name itself was not rendered as a plain literal: %+v", flag)
	}
}
