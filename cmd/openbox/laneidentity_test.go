package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

// TestResolveProviderIdentitiesSkipsAnUnconfiguredProvider is the common
// machine: only claude-code has ever been `init`-ed, so Codex has no store,
// and the daemon must still come up governing the tool it can rather than
// failing outright.
func TestResolveProviderIdentitiesSkipsAnUnconfiguredProvider(t *testing.T) {
	isolateHomeOnly(t)
	seedCredentials(t, "claude-code")

	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	identities := resolveProviderIdentities(warn)

	cc, ok := identities["claude-code"]
	if !ok {
		t.Fatal("claude-code was seeded a full identity and must resolve")
	}
	if cc.DID != testDIDFor(t, "claude-code") {
		t.Errorf("DID = %q, want %q", cc.DID, testDIDFor(t, "claude-code"))
	}
	if cc.Client == nil {
		t.Error("claude-code resolved with no client; nothing could sign its egress")
	}
	if _, ok := identities["codex"]; ok {
		t.Error("codex has no store on this machine and must not resolve an identity")
	}
	if len(warnings) == 0 {
		t.Error("skipping an unconfigured provider must say so, or a silent gap looks identical to a working one")
	}
}

// TestResolveProviderIdentitiesResolvesBothWhenBothAreConfigured pins the
// core claim: a Codex record is attributable to the Codex agent's own DID and
// client, never the CC one, once both tools have been `init`-ed.
func TestResolveProviderIdentitiesResolvesBothWhenBothAreConfigured(t *testing.T) {
	isolateHomeOnly(t)
	seedCredentials(t, "claude-code", "codex")

	identities := resolveProviderIdentities(nil)
	cc, ccOK := identities["claude-code"]
	codex, codexOK := identities["codex"]
	if !ccOK || !codexOK {
		t.Fatalf("both tools were seeded; got claude-code=%v codex=%v", ccOK, codexOK)
	}
	if cc.DID == codex.DID {
		t.Fatal("fixture drift: the two tools must carry distinct DIDs")
	}
	if cc.DID != testDIDFor(t, "claude-code") || codex.DID != testDIDFor(t, "codex") {
		t.Errorf("DIDs = %q, %q; want each tool's own", cc.DID, codex.DID)
	}
	if cc.Client == codex.Client {
		t.Fatal("the two providers must not share one client: the client is what signs the egress")
	}
}

// TestResolveProviderIdentitiesNeverBindsAProvider pins the isolation
// property pre-resolution exists for: it must never flip the process-global
// bound provider, because two concurrent records would then resolve each
// other's identity store. BoundProvider must read exactly what it did before
// the call, on either side of it.
func TestResolveProviderIdentitiesNeverBindsAProvider(t *testing.T) {
	isolateHomeOnly(t)
	seedCredentials(t, "claude-code", "codex")

	before := devconfig.BoundProvider()
	resolveProviderIdentities(nil)
	after := devconfig.BoundProvider()
	if before != after {
		t.Fatalf("BoundProvider changed from %q to %q; pre-resolution must not bind", before, after)
	}
}

// TestResolveProviderIdentitiesHonoursEachToolsOwnContentCapture is the
// privacy-posture fix: a lane daemon governs every configured tool for its
// whole life, and each tool's content_capture posture must come from THAT
// tool's own dev.json, never from whichever tool a startup-only BindProvider
// call happened to bind for unrelated setup work. Proven on the wire, not
// merely on the Credentials struct: codex's client must never carry the
// relayed request body when codex opted out, while claude-code's does, with
// codex opting IN and claude-code opting OUT flipped in the other test below.
func TestResolveProviderIdentitiesHonoursEachToolsOwnContentCapture(t *testing.T) {
	memhttptest.RequireBind(t)
	isolateHomeOnly(t)

	ccFake := fakecore.New(t, fakecore.Script{})
	codexFake := fakecore.New(t, fakecore.Script{})
	on, off := true, false
	seedIdentityWithCapture(t, "claude-code", ccFake, &on)
	seedIdentityWithCapture(t, "codex", codexFake, &off)

	identities := resolveProviderIdentities(nil)
	cc, ccOK := identities["claude-code"]
	codex, codexOK := identities["codex"]
	if !ccOK || !codexOK {
		t.Fatalf("both tools were seeded; got claude-code=%v codex=%v", ccOK, codexOK)
	}

	emitModelCall(t, cc.Client, "claude-code", cc.DID)
	emitModelCall(t, codex.Client, "codex", codex.DID)

	if got := ccFake.Inbox(); len(got) != 1 || !hasActivityInput(got[0]) {
		t.Errorf("claude-code opted IN to content_capture and must carry activity_input on the wire; inbox=%v", got)
	}
	if got := codexFake.Inbox(); len(got) != 1 || hasActivityInput(got[0]) {
		t.Errorf("codex opted OUT of content_capture and must NOT carry activity_input on the wire; inbox=%v", got)
	}
}

// TestResolveProviderIdentitiesHonoursTheOppositeContentCaptureDirection
// flips which tool opts out, so the property proven above is not an artifact
// of always testing codex as the one that opts out.
func TestResolveProviderIdentitiesHonoursTheOppositeContentCaptureDirection(t *testing.T) {
	memhttptest.RequireBind(t)
	isolateHomeOnly(t)

	ccFake := fakecore.New(t, fakecore.Script{})
	codexFake := fakecore.New(t, fakecore.Script{})
	on, off := true, false
	seedIdentityWithCapture(t, "claude-code", ccFake, &off)
	seedIdentityWithCapture(t, "codex", codexFake, &on)

	identities := resolveProviderIdentities(nil)
	cc := identities["claude-code"]
	codex := identities["codex"]

	emitModelCall(t, cc.Client, "claude-code", cc.DID)
	emitModelCall(t, codex.Client, "codex", codex.DID)

	if got := ccFake.Inbox(); len(got) != 1 || hasActivityInput(got[0]) {
		t.Errorf("claude-code opted OUT of content_capture and must NOT carry activity_input on the wire; inbox=%v", got)
	}
	if got := codexFake.Inbox(); len(got) != 1 || !hasActivityInput(got[0]) {
		t.Errorf("codex opted IN to content_capture and must carry activity_input on the wire; inbox=%v", got)
	}
}

// seedIdentityWithCapture is seedToolCredentials plus an explicit
// content_capture posture and a fake core to sign against, for the tests
// above that need each tool's posture to differ.
func seedIdentityWithCapture(t *testing.T, tool string, fake *fakecore.Server, capture *bool) {
	t.Helper()
	envPath, err := devconfig.EnvFilePathFor(tool)
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(envPath, map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_test_k",
		devconfig.EnvAgentPrivateKey: fake.SeedB64(),
	}); err != nil {
		t.Fatal(err)
	}
	cfgPath, err := devconfig.DevConfigWritePathFor(tool)
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteConfig(cfgPath, devconfig.Update{
		DID:            fake.DID(),
		BaseURL:        fake.URL(),
		ContentCapture: capture,
	}); err != nil {
		t.Fatal(err)
	}
}

// emitModelCall sends one relayed-call-shaped DevEvent (the opening half,
// which is what carries activity_input) through c, failing the test on
// delivery error.
func emitModelCall(t *testing.T, c *client.Client, tool, did string) {
	t.Helper()
	ev := client.DevEvent{
		SchemaVersion:  client.SchemaVersion,
		EventID:        "ev-" + tool,
		EventType:      client.EventTurnStarted,
		SessionID:      "sess-" + tool,
		DeveloperDID:   did,
		Timestamp:      "2026-09-23T10:00:00Z",
		StartedAt:      "2026-09-23T10:00:00Z",
		Tool:           client.Tool{Name: tool, Kind: client.ToolShell},
		ActivityType:   client.ActivityTypeLLMCompletion,
		ProxyRequestID: "px-" + tool,
		Span: &client.Span{
			SemanticType: client.ActivityTypeLLMCompletion,
			Stage:        "started",
			HTTPMethod:   "POST",
			HTTPURL:      "https://api.example/v1/messages",
			RequestBody:  `{"prompt":"a prompt that must never egress with content_capture off"}`,
		},
	}
	if _, err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("%s: Emit: %v", tool, err)
	}
}

func hasActivityInput(r fakecore.Received) bool {
	v, ok := r.Body["activity_input"]
	return ok && v != nil
}

// TestResolveProviderIdentitiesPassesWarnAsTheClientsLogger is the Logger-
// parity fix: without it, client.Emit's own diagnostic detail (not just the
// caller's own "delivery failed: %v" line) fell back to a discarding
// nopLogger for both lane daemons' send path. A client built with no base
// URL at all fails inside Emit before any network call, which is enough to
// prove the client's OWN logger -- not the caller's after-the-fact wrapping
// -- received something.
func TestResolveProviderIdentitiesPassesWarnAsTheClientsLogger(t *testing.T) {
	isolateHomeOnly(t)
	seedCredentials(t, "claude-code")
	// An empty BaseURL still resolves DID/APIKey/PrivateKey, but Emit's POST
	// will fail to build/dial, which is where the client's OWN Logger.Printf
	// fires.
	t.Setenv(devconfig.EnvBaseURL, "")

	var warnings []string
	identities := resolveProviderIdentities(func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	cc, ok := identities["claude-code"]
	if !ok {
		t.Fatal("claude-code was seeded and must resolve")
	}

	before := len(warnings)
	_, _ = cc.Client.Emit(context.Background(), client.DevEvent{
		EventID:      "ev-logger-parity",
		EventType:    client.EventTurnStarted,
		SessionID:    "sess-logger-parity",
		DeveloperDID: cc.DID,
		Timestamp:    "2026-09-23T10:00:00Z",
		Tool:         client.Tool{Name: "claude-code", Kind: client.ToolShell},
		ActivityType: client.ActivityTypeLLMCompletion,
	})
	if len(warnings) <= before {
		t.Error("the client built by resolveProviderIdentities never used the warn closure as its Logger")
	}
}
