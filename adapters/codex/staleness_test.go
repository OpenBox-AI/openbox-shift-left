package codex

import (
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

func TestStaleMarkerLifecycle_Codex(t *testing.T) {
	t.Setenv(envStaleDir, filepath.Join(t.TempDir(), "stale"))

	if sessionIsStale("sess-a") {
		t.Fatal("no marker yet → not stale")
	}
	if err := writeStaleMarker("sess-a"); err != nil {
		t.Fatal(err)
	}
	if !sessionIsStale("sess-a") {
		t.Fatal("after writeStaleMarker → stale")
	}
	if err := clearStaleMarker("sess-a"); err != nil {
		t.Fatal(err)
	}
	if sessionIsStale("sess-a") {
		t.Fatal("after clearStaleMarker → not stale")
	}
}

func TestStaleMarkerPath_TraversalGuard(t *testing.T) {
	t.Setenv(envStaleDir, "/base")
	for _, bad := range []string{"", ".", "..", "/", "  "} {
		if p := staleMarkerPath(bad); p != "" {
			t.Errorf("degenerate session id %q must yield no marker path, got %q", bad, p)
		}
	}
	// A crafted traversal id is reduced to its base name (cannot escape the dir).
	if p := staleMarkerPath("../../etc/passwd"); filepath.Dir(p) != "/base" {
		t.Errorf("marker path must stay under the stale dir, got %q", p)
	}
}

func TestStaleGateDecision_OnlyFailClosed(t *testing.T) {
	t.Setenv(envStaleDir, filepath.Join(t.TempDir(), "stale"))
	if err := writeStaleMarker("sess-x"); err != nil {
		t.Fatal(err)
	}

	// fail-open: staleness never denies.
	t.Setenv(devconfig.EnvFailClosed, "0")
	if _, blocked := staleGateDecision("sess-x"); blocked {
		t.Error("fail-open must never deny on staleness")
	}

	// fail-closed + stale: deny with a content-free reason.
	t.Setenv(devconfig.EnvFailClosed, "1")
	dec, blocked := staleGateDecision("sess-x")
	if !blocked || !dec.Evaluation.WouldBlock() {
		t.Fatalf("fail-closed + stale must deny, got blocked=%v dec=%+v", blocked, dec)
	}
	if dec.FailOpen {
		t.Error("a stale deny is an intentional real deny, not an outage fallback (FailOpen must be false)")
	}
}

func TestClearAllStaleMarkers_Codex(t *testing.T) {
	t.Setenv(envStaleDir, filepath.Join(t.TempDir(), "stale"))
	_ = writeStaleMarker("a")
	_ = writeStaleMarker("b")
	if err := ClearAllStaleMarkers(); err != nil {
		t.Fatal(err)
	}
	if sessionIsStale("a") || sessionIsStale("b") {
		t.Error("ClearAllStaleMarkers must remove every marker")
	}
	// Absent dir → no-op nil.
	t.Setenv(envStaleDir, filepath.Join(t.TempDir(), "does-not-exist"))
	if err := ClearAllStaleMarkers(); err != nil {
		t.Errorf("absent stale dir must be a no-op, got %v", err)
	}
}
