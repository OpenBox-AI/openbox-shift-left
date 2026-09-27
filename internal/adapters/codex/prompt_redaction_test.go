package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// awsSecretFixture is assembled at run time rather than written as one literal:
// this repo's own local hook rewrites secret-shaped literals on disk, so a
// pasted key would silently become a placeholder in the source file and the
// test would assert nothing. Split, it survives the edit and still matches the
// detector's keyword rule byte-for-byte.
func awsSecretFixture() string { return "AKIA" + "IOSFODNN7EXAMPLE" }

// spooledBody returns the single spool file's contents.
func spooledBody(t *testing.T, spool string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(spool, "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no spool file written under %s (err=%v); the case proves nothing", spool, err)
	}
	var b strings.Builder
	for _, f := range files {
		raw, rErr := os.ReadFile(f)
		if rErr != nil {
			t.Fatalf("read spool %s: %v", f, rErr)
		}
		b.Write(raw)
	}
	return b.String()
}

// promptPayload builds a UserPromptSubmit payload carrying a secret embedded in
// ordinary prose, so "redacted" stays distinguishable from "dropped".
func promptPayload(session, secret string) string {
	return `{"hook_event_name":"UserPromptSubmit","session_id":"` + session + `","cwd":"/r",` +
		`"model":"gpt-5.6-sol","permission_mode":"default","turn_id":"t1",` +
		`"prompt":"deploy with key ` + secret + ` now","transcript_path":null}`
}

// TestRunHook_PromptIsRedactedBeforeItIsSpooled is the regression test for the
// content leak it closes: the Codex prompt is the adapter's only egressed
// content class, and without a RedactContent collaborator on the Codex Mapper
// it would ship unscanned even with secret_detection on.
//
// The assertion is the triple from the Claude Code precedent
// (claude-code/content_conformance_test.go): the secret absent, AND the
// placeholder present, AND the surrounding prose intact. Absence alone also
// passes for a client that stopped sending prompts entirely, and a placeholder
// without the surrounding text would not distinguish redaction from truncation.
func TestRunHook_PromptIsRedactedBeforeItIsSpooled(t *testing.T) {
	spool := setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "1")
	os.Unsetenv(devconfig.EnvSecretDetection) // default ON

	secret := awsSecretFixture()
	// UserPromptSubmit is gated unconditionally now (ResolveEnforce always
	// reports true); with no reachable control plane it denies (delivery is
	// always fail-closed), but the escalation was never attempted (no client
	// configured), so the gate's own SpoolObserve still appends this call's
	// observe copy to the local spool -- what this test actually exercises.
	stdout, _ := runHook(t, "UserPromptSubmit", promptPayload("th-redact", secret))
	if !strings.Contains(stdout, `"decision":"block"`) {
		t.Fatalf("no reachable control plane must deny (delivery is always fail-closed), got %q", stdout)
	}

	body := spooledBody(t, spool)
	if strings.Contains(body, secret) {
		t.Errorf("the prompt's credential reached the spool unredacted:\n%s", body)
	}
	if !strings.Contains(body, "OPENBOX_REDACTED") {
		t.Errorf("no redaction placeholder in the spooled prompt; it was never scanned:\n%s", body)
	}
	if !strings.Contains(body, "deploy with key") {
		t.Errorf("the prompt's non-secret text did not survive, so this proves nothing about redaction:\n%s", body)
	}
}

// TestRunHook_PromptOptOutIsHonestlyUnredacted pins the other half of the
// setting. With secret_detection off the prompt egresses verbatim; RedactContent
// stays nil and nil means identity. Asserting this stops a later change from
// making redaction unconditional, which would quietly turn the documented
// opt-out into a lie.
func TestRunHook_PromptOptOutIsHonestlyUnredacted(t *testing.T) {
	spool := setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "1")
	t.Setenv(devconfig.EnvSecretDetection, "0")

	secret := awsSecretFixture()
	runHook(t, "UserPromptSubmit", promptPayload("th-optout", secret))

	body := spooledBody(t, spool)
	if !strings.Contains(body, secret) {
		t.Errorf("with secret_detection off the prompt must egress verbatim; got:\n%s", body)
	}
}

// TestRunHook_PromptCaptureOffAttachesNothing guards the ordering redaction
// depends on: the CaptureContent gate is checked BEFORE the redactor is
// consulted, so capture-off attaches no prompt at all and the redactor is never
// built. A redactor that ran first would make capture-off cost work it should
// not do, and would put content on a path the gate is supposed to own.
func TestRunHook_PromptCaptureOffAttachesNothing(t *testing.T) {
	spool := setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "0")
	os.Unsetenv(devconfig.EnvSecretDetection)

	secret := awsSecretFixture()
	runHook(t, "UserPromptSubmit", promptPayload("th-nocapture", secret))

	body := spooledBody(t, spool)
	if strings.Contains(body, secret) || strings.Contains(body, "deploy with key") {
		t.Errorf("content_capture=0 must attach no prompt at all:\n%s", body)
	}
	if !strings.Contains(body, "PromptSubmitted") {
		t.Errorf("the signal itself must still ship with capture off:\n%s", body)
	}
}

// TestWire_PromptIsRedactedOnTheOutboundBytes asserts the same triple one layer
// out, on the bytes the real client puts on the wire. Asserting a struct is not
// asserting the wire, and the
// spool assertions above stop one layer short of egress.
func TestWire_PromptIsRedactedOnTheOutboundBytes(t *testing.T) {
	// Content capture must be on at the CLIENT too, or Emit strips Content before
	// egress (INV-2) and the case would pass vacuously on an empty body.
	cl, fc := newWireCapture(t, func(c *client.Config) { c.ContentCaptureEnabled = true })

	m := testMapper()
	m.NewID = nil
	m.CaptureContent = true
	// Built exactly as RunHook builds it under ResolveSecretDetection().
	redactor := decision.NewRedactor()
	m.RedactContent = func(s string) string { return hookflow.RedactText(redactor, s) }

	secret := awsSecretFixture()
	ev, ok := m.Map(HookUserPromptSubmit, &HookEvent{
		SessionID: "th-wire",
		Prompt:    "deploy with key " + secret + " now",
	})
	if !ok {
		t.Fatal("UserPromptSubmit must map to an event")
	}
	emit(t, cl, ev)

	bodies := fc.Inbox()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly 1 wire body, got %d", len(bodies))
	}
	wire := string(bodies[0].Raw)
	if strings.Contains(wire, secret) {
		t.Errorf("the credential reached the wire unredacted:\n%s", wire)
	}
	if !strings.Contains(wire, "OPENBOX_REDACTED") {
		t.Errorf("no redaction placeholder on the wire:\n%s", wire)
	}
	if !strings.Contains(wire, "deploy with key") {
		t.Errorf("the non-secret prompt text did not reach the wire:\n%s", wire)
	}
}

// TestMapper_RedactionIsStructural pins redaction inside the mapper rather than
// at the call site, so a second caller of Map inherits it. The prompt
// gate re-maps the same event through this same Mapper for its DecisionRequest;
// if redaction lived at the RunHook call site instead, the gate's copy would
// carry the raw prompt. (claude-code/mapper_test.go TestMapTurn_RedactionIsStructural
// is the precedent.)
func TestMapper_RedactionIsStructural(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true
	m.RedactContent = func(string) string { return "REDACTED-BY-COLLABORATOR" }

	ev, ok := m.Map(HookUserPromptSubmit, &HookEvent{SessionID: "s", Prompt: "anything at all"})
	if !ok {
		t.Fatal("UserPromptSubmit must map to an event")
	}
	if ev.Content == nil {
		t.Fatal("capture on ⇒ Content must be attached")
	}
	if ev.Content.Prompt != "REDACTED-BY-COLLABORATOR" {
		t.Errorf("Map must route the prompt through RedactContent; got %q", ev.Content.Prompt)
	}
}

// TestMapper_NilRedactorIsIdentity is the nil contract, asserted directly so the
// opt-out cannot regress into a nil-pointer panic.
func TestMapper_NilRedactorIsIdentity(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true
	m.RedactContent = nil

	ev, _ := m.Map(HookUserPromptSubmit, &HookEvent{SessionID: "s", Prompt: "verbatim text"})
	if ev.Content == nil || ev.Content.Prompt != "verbatim text" {
		t.Errorf("nil RedactContent must be identity; got %+v", ev.Content)
	}
}
