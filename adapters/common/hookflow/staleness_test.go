package hookflow

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

// isolateConfig points config resolution at an empty file, so a test never
// reads the developer's real dev.json.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(t.TempDir(), "none.json"))
}

// STORY-E6-S8 AC-4/5/6: session-start policy staleness.

// writeLocalPin writes a local bundle carrying a PIN so the staleness check has
// something to compare against, and points OPENBOX_SIDECAR_BUNDLE at it.
func writeLocalPin(t *testing.T, policyID, updatedAt string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.json")
	raw, _ := json.Marshal(map[string]any{
		"version":    policyID + "@" + updatedAt,
		"policy_id":  policyID,
		"updated_at": updatedAt,
	})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBundle, path)
}

// backendServingPin starts a fake control plane returning the given policy PIN.
func backendServingPin(t *testing.T, policyID, updatedAt string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/policies/current") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"status":200,"data":{"id":"` + policyID + `","updated_at":"` + updatedAt + `"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func stalenessEnv(t *testing.T, backendURL string) {
	t.Helper()
	isolateConfig(t)
	t.Setenv(devconfig.EnvControlToken, "obx_key_orgkey")
	t.Setenv(devconfig.EnvBackendURL, backendURL)
	t.Setenv(devconfig.EnvAgentID, "agent-1")
	t.Setenv(EnvStaleDir, filepath.Join(t.TempDir(), "stale"))
}

func TestStaleness_FailOpen_WarnsAndProceeds(t *testing.T) {
	writeLocalPin(t, "pol-1", "v1")
	backend := backendServingPin(t, "pol-1", "v2") // NEWER → mismatch
	stalenessEnv(t, backend)
	t.Setenv(devconfig.EnvFailClosed, "0") // fail-open

	var stdout bytes.Buffer
	CheckPolicyStaleness(log.New(&bytes.Buffer{}, "", 0), "sess-A", &stdout)

	// Fail-open surfaces a SessionStart additionalContext nudge and proceeds.
	if !strings.Contains(stdout.String(), "additionalContext") || !strings.Contains(stdout.String(), "dev sync") {
		t.Errorf("fail-open should emit an additionalContext nudge; got %q", stdout.String())
	}
	// No stale marker under fail-open → the PreToolUse gate never denies.
	if SessionIsStale("sess-A") {
		t.Errorf("fail-open must NOT write a stale marker")
	}
	if _, blocked := StaleGateDecision("sess-A"); blocked {
		t.Errorf("fail-open must not produce a stale deny")
	}
}

func TestStaleness_FailClosed_MarksAndGateDenies(t *testing.T) {
	writeLocalPin(t, "pol-1", "v1")
	backend := backendServingPin(t, "pol-1", "v2") // mismatch
	stalenessEnv(t, backend)
	t.Setenv(devconfig.EnvFailClosed, "1") // fail-closed

	var stdout bytes.Buffer
	CheckPolicyStaleness(log.New(&bytes.Buffer{}, "", 0), "sess-B", &stdout)

	// Fail-closed writes a content-free marker and must NOT block the session
	// (no non-additionalContext stdout at SessionStart).
	if !SessionIsStale("sess-B") {
		t.Fatalf("fail-closed should mark the session stale")
	}
	// The marker file is content-free.
	if data, _ := os.ReadFile(staleMarkerPath("sess-B")); len(data) != 0 {
		t.Errorf("stale marker must be content-free, got %q", data)
	}
	// The PreToolUse gate now synthesizes a deny.
	dec, blocked := StaleGateDecision("sess-B")
	if !blocked || !dec.Evaluation.WouldBlock() {
		t.Fatalf("stale gate should deny under fail-closed; blocked=%v verdict=%v", blocked, dec.Evaluation.Verdict)
	}
	if strings.Contains(dec.Evaluation.Reason, "rm") { // content-free sanity
		t.Errorf("stale reason must be content-free")
	}
	// dev sync clears markers → the gate proceeds again.
	if err := ClearAllStaleMarkers(); err != nil {
		t.Fatal(err)
	}
	if _, blocked := StaleGateDecision("sess-B"); blocked {
		t.Errorf("after clearing markers the gate must proceed")
	}
}

// AC-6: no org key / offline / can't-determine never denies at fetch time.
func TestStaleness_NoKeyProceeds(t *testing.T) {
	writeLocalPin(t, "pol-1", "v1")
	isolateConfig(t)
	t.Setenv(EnvStaleDir, filepath.Join(t.TempDir(), "stale"))
	os.Unsetenv(devconfig.EnvControlToken) // no org key in the hook env
	t.Setenv(devconfig.EnvBackendURL, "https://unused.example")
	t.Setenv(devconfig.EnvAgentID, "agent-1")
	t.Setenv(devconfig.EnvFailClosed, "1") // even fail-closed must not deny without a determination

	var stdout bytes.Buffer
	CheckPolicyStaleness(log.New(&bytes.Buffer{}, "", 0), "sess-C", &stdout)
	if SessionIsStale("sess-C") {
		t.Errorf("no org key must NOT mark stale (can't determine → proceed)")
	}
	if strings.TrimSpace(stdout.String()) != "" {
		t.Errorf("no-key path must be silent; got %q", stdout.String())
	}
}

func TestStaleness_MatchIsSilent(t *testing.T) {
	writeLocalPin(t, "pol-7", "v9")
	backend := backendServingPin(t, "pol-7", "v9") // identical PIN → in sync
	stalenessEnv(t, backend)
	t.Setenv(devconfig.EnvFailClosed, "1")

	var stdout bytes.Buffer
	CheckPolicyStaleness(log.New(&bytes.Buffer{}, "", 0), "sess-D", &stdout)
	if SessionIsStale("sess-D") || strings.TrimSpace(stdout.String()) != "" {
		t.Errorf("a matching PIN must be silent and never mark stale")
	}
}

// Unreachable backend → proceed on last-good bundle (never deny at fetch time).
func TestStaleness_UnreachableProceeds(t *testing.T) {
	writeLocalPin(t, "pol-1", "v1")
	stalenessEnv(t, "http://127.0.0.1:1") // nothing listening
	t.Setenv(devconfig.EnvFailClosed, "1")

	var stdout bytes.Buffer
	CheckPolicyStaleness(log.New(&bytes.Buffer{}, "", 0), "sess-E", &stdout)
	if SessionIsStale("sess-E") {
		t.Errorf("an unreachable backend must NOT mark stale (proceed on last-good)")
	}
}

// The stale-marker path is guarded against traversal from a crafted session id.
func TestStaleMarkerPath_TraversalGuard(t *testing.T) {
	t.Setenv(EnvStaleDir, t.TempDir())
	for _, id := range []string{"../escape", "..", "/", "", "  "} {
		p := staleMarkerPath(id)
		if p != "" && filepath.Dir(p) != staleMarkerDir() {
			t.Errorf("session id %q escaped the marker dir: %q", id, p)
		}
	}
}

// These two came from the Codex adapter's copy of this suite; the Claude Code
// copy had no equivalent, so the shared engine keeps both sides' coverage.
func TestStaleGateDecision_OnlyFailClosed(t *testing.T) {
	t.Setenv(EnvStaleDir, filepath.Join(t.TempDir(), "stale"))
	if err := WriteStaleMarker("sess-x"); err != nil {
		t.Fatal(err)
	}

	// fail-open: staleness never denies.
	t.Setenv(devconfig.EnvFailClosed, "0")
	if _, blocked := StaleGateDecision("sess-x"); blocked {
		t.Error("fail-open must never deny on staleness")
	}

	// fail-closed + stale: deny with a content-free reason.
	t.Setenv(devconfig.EnvFailClosed, "1")
	dec, blocked := StaleGateDecision("sess-x")
	if !blocked || !dec.Evaluation.WouldBlock() {
		t.Fatalf("fail-closed + stale must deny, got blocked=%v dec=%+v", blocked, dec)
	}
	if dec.FailOpen {
		t.Error("a stale deny is an intentional real deny, not an outage fallback (FailOpen must be false)")
	}
}

func TestClearAllStaleMarkers(t *testing.T) {
	t.Setenv(EnvStaleDir, filepath.Join(t.TempDir(), "stale"))
	_ = WriteStaleMarker("a")
	_ = WriteStaleMarker("b")
	if err := ClearAllStaleMarkers(); err != nil {
		t.Fatal(err)
	}
	if SessionIsStale("a") || SessionIsStale("b") {
		t.Error("ClearAllStaleMarkers must remove every marker")
	}
	// Absent dir → no-op nil.
	t.Setenv(EnvStaleDir, filepath.Join(t.TempDir(), "does-not-exist"))
	if err := ClearAllStaleMarkers(); err != nil {
		t.Errorf("absent stale dir must be a no-op, got %v", err)
	}
}
