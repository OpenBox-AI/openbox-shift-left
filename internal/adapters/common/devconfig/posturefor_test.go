package devconfig

import (
	"path/filepath"
	"testing"
)

// TestResolveContentCaptureForReadsEachToolsOwnFile is the privacy-posture
// gap this fixes: a process that governs more than one tool for its whole
// life must answer each tool's content_capture question from THAT tool's own
// dev.json, never from whichever tool it happens to have bound at startup.
func TestResolveContentCaptureForReadsEachToolsOwnFile(t *testing.T) {
	isolateConfig(t) // binds claude-code; must not matter to either answer below

	off := false
	codexPath, err := DevConfigPathFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(codexPath, Update{ContentCapture: &off}); err != nil {
		t.Fatal(err)
	}
	// claude-code gets no dev.json at all: it must default ON, not inherit
	// codex's opt-out.

	cc, err := ResolveContentCaptureFor("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !cc {
		t.Error("claude-code has no content_capture override and must default ON")
	}
	codex, err := ResolveContentCaptureFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if codex {
		t.Error("codex's own content_capture:false must be honoured for codex, not overridden by claude-code's default")
	}
}

// TestResolveContentCaptureForHonoursTheOppositeDirectionToo is the same
// property from the other side: claude-code opted out, codex left default.
func TestResolveContentCaptureForHonoursTheOppositeDirectionToo(t *testing.T) {
	isolateConfig(t)

	off := false
	ccPath, err := DevConfigPathFor("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(ccPath, Update{ContentCapture: &off}); err != nil {
		t.Fatal(err)
	}

	cc, err := ResolveContentCaptureFor("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if cc {
		t.Error("claude-code's own content_capture:false must be honoured")
	}
	codex, err := ResolveContentCaptureFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if !codex {
		t.Error("codex has no dev.json yet and must default ON, not inherit claude-code's opt-out")
	}
}

// TestResolveContentCaptureForManagedLockStillAppliesToEveryTool: the managed
// layer is an org-wide mandate, not a per-tool store, so a locked
// content_capture must win for every tool regardless of what its own dev.json
// says.
func TestResolveContentCaptureForManagedLockStillAppliesToEveryTool(t *testing.T) {
	isolateConfig(t)
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	writeJSON(t, managedPath, `{"content_capture":false,"locked":["content_capture"]}`)
	t.Setenv(EnvManagedConfig, managedPath)

	on := true
	codexPath, err := DevConfigPathFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(codexPath, Update{ContentCapture: &on}); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveContentCaptureFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("a locked managed content_capture:false must override codex's own dev.json")
	}
}

// TestResolveContentCaptureForEnvOverrideIsProcessWide documents the one
// exception: OPENBOX_CONTENT_CAPTURE is a process environment variable, so it
// necessarily applies the same way to every tool this process resolves for --
// there is no per-tool environment.
func TestResolveContentCaptureForEnvOverrideIsProcessWide(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvContentCapture, "0")

	for _, tool := range []string{"claude-code", "codex"} {
		got, err := ResolveContentCaptureFor(tool)
		if err != nil {
			t.Fatal(err)
		}
		if got {
			t.Errorf("%s: OPENBOX_CONTENT_CAPTURE=0 must force OFF for every tool", tool)
		}
	}
}

// TestResolveTelemetryForReadsEachToolsOwnFile mirrors
// TestResolveContentCaptureForReadsEachToolsOwnFile for the telemetry-
// recording gate: a shared lane daemon must not let one tool's opt-out (or
// opt-in) decide whether the OTHER tool's turns are recorded.
func TestResolveTelemetryForReadsEachToolsOwnFile(t *testing.T) {
	isolateConfig(t)

	codexPath, err := DevConfigPathFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	// Update has no Telemetry field (only `init` writes ContentCapture/Enforce/
	// etc. through it); write the fixture directly, the same way
	// TestManaged_* does for fields Update does not cover.
	writeJSON(t, codexPath, `{"developer_did":"did:aip:codex","telemetry":false}`)

	cc, err := ResolveTelemetryFor("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !cc {
		t.Error("claude-code has no telemetry override and must default ON")
	}
	codex, err := ResolveTelemetryFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if codex {
		t.Error("codex's own telemetry:false must be honoured for codex")
	}
}

// TestResolveCredentialsForUsesTheNamedToolsOwnContentCapture is the
// end-to-end proof for the field ResolveCredentialsFor callers actually
// consume: Credentials.ContentCaptureEnabled, which cmd/openbox/laneidentity.go
// feeds straight into the client that signs a tool's egress.
func TestResolveCredentialsForUsesTheNamedToolsOwnContentCapture(t *testing.T) {
	isolateConfig(t)

	off := false
	for _, tool := range []string{"claude-code", "codex"} {
		if err := WriteEnvFile(mustEnvFilePathFor(t, tool), map[string]string{
			EnvAPIKeyDirect:       "obx_test_k",
			EnvWorkloadPrivateKey: testWorkloadKeyB64ForPostureTest,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ccPath, err := DevConfigPathFor("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(ccPath, Update{AgentID: "cccccccc-0000-5000-a000-0000000000cc"}); err != nil {
		t.Fatal(err)
	}
	codexPath, err := DevConfigPathFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(codexPath, Update{AgentID: "dddddddd-0000-5000-a000-0000000000dd", ContentCapture: &off}); err != nil {
		t.Fatal(err)
	}

	cc, err := ResolveCredentialsFor("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !cc.ContentCaptureEnabled {
		t.Error("claude-code's Credentials must default content capture ON")
	}
	codex, err := ResolveCredentialsFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if codex.ContentCaptureEnabled {
		t.Error("codex's Credentials must honour codex's own content_capture:false, not claude-code's default")
	}
}

func mustEnvFilePathFor(t *testing.T, tool string) string {
	t.Helper()
	p, err := EnvFilePathFor(tool)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// testWorkloadKeyB64ForPostureTest is an arbitrary placeholder workload-key
// fixture; nothing in this file ever signs with it.
const testWorkloadKeyB64ForPostureTest = "wk_test_not_a_real_key"
