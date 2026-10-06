package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// ── configChangeOutputContract: mirrors TestPromptContractRender,
// but pins the config gate's OWN literal and shape (never aliased to the
// prompt gate's).
func TestConfigChangeOutputContractRender(t *testing.T) {
	if ccConfigDecisionBlock != "block" {
		t.Fatalf("ccConfigDecisionBlock = %q, want the verified vendor literal %q", ccConfigDecisionBlock, "block")
	}

	t.Run("halt blocks and stops the session", func(t *testing.T) {
		line, applied := configContract.Render(hookflow.DecisionHalt, "OpenBox governance: kill switch (policy: p-1)", nil)
		if applied != hookflow.DecisionHalt {
			t.Fatalf("applied = %q, want %q", applied, hookflow.DecisionHalt)
		}
		var got userPromptSubmitOutput
		if err := json.Unmarshal(line, &got); err != nil {
			t.Fatalf("not valid JSON: %v (%q)", err, line)
		}
		if got.Decision != ccConfigDecisionBlock || got.Continue == nil || *got.Continue || got.StopReason == "" {
			t.Errorf("halt render = %+v, want decision:block + continue:false + stopReason", got)
		}
	})

	t.Run("deny renders the VERIFIED block literal exactly, nothing more", func(t *testing.T) {
		line, applied := configContract.Render(hookflow.DecisionDeny, "OpenBox governance: blocked (policy: p-2)", nil)
		if applied != ccConfigDecisionBlock {
			t.Fatalf("applied = %q, want %q", applied, ccConfigDecisionBlock)
		}
		var got map[string]any
		if err := json.Unmarshal(line, &got); err != nil {
			t.Fatalf("not valid JSON: %v (%q)", err, line)
		}
		if len(got) != 2 {
			t.Fatalf("block output = %v, want exactly {decision, reason} (VERIFIED literal, no continue/stopReason)", got)
		}
		if got["decision"] != "block" {
			t.Errorf("decision = %v, want %q", got["decision"], "block")
		}
	})

	t.Run("proceed writes nothing (tighten-only)", func(t *testing.T) {
		if line, applied := configContract.Render("", "", nil); line != nil || applied != "" {
			t.Errorf("proceed rendered (%q, %q), want nothing", line, applied)
		}
	})

	t.Run("no path ever emits an allow", func(t *testing.T) {
		for _, dec := range []string{hookflow.DecisionDeny, hookflow.DecisionHalt} {
			line, _ := configContract.Render(dec, "r", nil)
			if bytes.Contains(line, []byte(`"decision":"allow"`)) {
				t.Errorf("decision %q rendered an allow: %q", dec, line)
			}
		}
	})

	var _ hookflow.OutputContract = configContract
}

// ── configSubject: the hookflow.PromptTarget template, not enforceTarget --
// no tool_input, no HighRisk, identity + source/file_path axes only.
func TestConfigSubject_Fields(t *testing.T) {
	id := Identity{DeveloperDID: testDID}
	m := testMapper()
	ev := &HookEvent{SessionID: "sess-cc-1", Source: "user_settings", FilePath: "/home/dev/.claude/settings.json"}
	target := configSubject{id: id, mapper: m, ev: ev}

	if target.SessionID() != "sess-cc-1" {
		t.Errorf("SessionID = %q", target.SessionID())
	}
	if target.ToolName() != configToolKind || configToolKind != "config" {
		t.Errorf("ToolName/configToolKind = %q, want %q", target.ToolName(), "config")
	}
	if target.ToolInput() != nil {
		t.Errorf("ToolInput = %q, want nil: a config change has no tool_input to rewrite", target.ToolInput())
	}
	if target.HighRisk() {
		t.Error("HighRisk must be false: this is not a shell/MCP call")
	}

	req := target.DecisionRequest(false)
	if req.SessionID != "sess-cc-1" || req.DeveloperDID != testDID {
		t.Fatalf("identity not carried: %+v", req)
	}
	if req.EventType != client.EventConfigChange {
		t.Errorf("EventType = %q, want ConfigChange", req.EventType)
	}
	if req.Content != nil {
		t.Errorf("Content = %+v, want nil: a config change carries no body", req.Content)
	}
	if got := req.Attributes["source"]; got != "user_settings" {
		t.Errorf("source attribute = %v, want the raw source", got)
	}
	if got := req.Attributes["file_path"]; got != "/home/dev/.claude/settings.json" {
		t.Errorf("file_path attribute = %v", got)
	}

	// CompactAny drops an empty axis rather than sending an empty string a
	// policy could spuriously not-match on.
	bare := configSubject{id: id, mapper: m, ev: &HookEvent{SessionID: "sess-cc-2"}}
	if bareReq := bare.DecisionRequest(false); bareReq.Attributes != nil {
		t.Errorf("Attributes = %v, want nil when source/file_path are both empty", bareReq.Attributes)
	}

	devEv, ok := target.DevEvent(nil)
	if !ok {
		t.Fatal("DevEvent must map a well-formed ConfigChange")
	}
	if devEv.EventType != client.EventConfigChange {
		t.Errorf("mapped EventType = %q, want ConfigChange", devEv.EventType)
	}

	var _ hookflow.EnforceTarget = configSubject{}
}

func configPayload(sid, source, filePath string) string {
	return `{"hook_event_name":"ConfigChange","session_id":"` + sid + `","cwd":"/tmp","source":"` + source + `","file_path":"` + filePath + `"}`
}

func setupConfigChangeEnv(t *testing.T) {
	t.Helper()
	isolateConfig(t)
	t.Setenv(envAgentID, testAgentID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv("OPENBOX_HALT_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))
	t.Setenv(envContentCapture, "0")
	t.Setenv(envEnforce, "1")
	t.Setenv(envFailClosed, "0")
}

func runConfigHook(t *testing.T, payload string) (stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	RunHook("ConfigChange", strings.NewReader(payload), &out, log.New(&errBuf, "", 0))
	return out.String(), errBuf.String()
}

// TestRunHook_ConfigChange_BlockDeniesAndRecords is the config gate's
// core end-to-end guard: DENY -> exactly one JSON line {"decision":"block",
// "reason":...} on stdout, one human line on stderr, one enforcement
// record with tool_kind=="config" carrying ts+reason.
func TestRunHook_ConfigChange_BlockDeniesAndRecords(t *testing.T) {
	setupConfigChangeEnv(t)
	serveVerdict(t, `{"verdict":"block","reason":"unauthorized settings edit","policy_id":"cc-pol"}`)

	out, errOut := runConfigHook(t, configPayload("cc-1", "user_settings", "/home/dev/.claude/settings.json"))

	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("stdout not valid JSON: %v (%q)", err, out)
	}
	if len(got) != 2 || got["decision"] != "block" {
		t.Fatalf("stdout = %v, want exactly {decision:block, reason:...}", got)
	}
	reason, _ := got["reason"].(string)
	if !strings.Contains(reason, "unauthorized settings edit") || !strings.Contains(reason, "cc-pol") {
		t.Errorf("reason = %q, want the policy reason + id", reason)
	}

	if !strings.Contains(errOut, "user_settings") || !strings.Contains(errOut, "settings.json") {
		t.Errorf("stderr denial line missing file/source: %q", errOut)
	}
	if !strings.Contains(errOut, "unauthorized settings edit") {
		t.Errorf("stderr denial line missing the policy reason: %q", errOut)
	}

	data, err := os.ReadFile(os.Getenv(envEnforcementFile))
	if err != nil {
		t.Fatalf("enforcement sink not written: %v", err)
	}
	var rec hookflow.EnforcementRecord
	if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
		t.Fatalf("enforcement record not valid JSON: %v", err)
	}
	if rec.ToolKind != configToolKind {
		t.Errorf("tool_kind = %q, want %q", rec.ToolKind, configToolKind)
	}
	if rec.Timestamp == "" {
		t.Error("ts must be present")
	}
	if rec.Reason != "unauthorized settings edit" {
		t.Errorf("reason = %q, want the verbatim policy reason", rec.Reason)
	}
	if rec.AppliedDecision != ccConfigDecisionBlock {
		t.Errorf("applied_decision = %q, want %q", rec.AppliedDecision, ccConfigDecisionBlock)
	}
}

// TestRunHook_ConfigChange_AllowWritesNothing: tighten-only -- an ALLOW verdict
// is byte-identical to observe (nothing on stdout, no stderr diagnostic line).
func TestRunHook_ConfigChange_AllowWritesNothing(t *testing.T) {
	setupConfigChangeEnv(t)
	serveVerdict(t, `{"verdict":"allow"}`)

	out, errOut := runConfigHook(t, configPayload("cc-2", "project_settings", "/repo/.claude/settings.json"))
	if strings.TrimSpace(out) != "" {
		t.Errorf("allow must write nothing to stdout; got %q", out)
	}
	if strings.Contains(errOut, "OpenBox blocked") {
		t.Errorf("allow must not write the stderr denial line; stderr=%q", errOut)
	}
}

// TestRunHook_ConfigChange_HaltLatchesAndReplays: a HALT adds continue:false +
// stopReason and latches the session; the next ConfigChange in that session
// replays the halt with NO further /evaluate round trip.
func TestRunHook_ConfigChange_HaltLatchesAndReplays(t *testing.T) {
	setupConfigChangeEnv(t)
	url, hits := serveEvaluate(t, `{"verdict":"halt","reason":"config kill switch","policy_id":"cc-halt"}`, 200, 0)
	evalCreds(t, url)

	out, _ := runConfigHook(t, configPayload("cc-halt-1", "local_settings", "/repo/.claude/settings.local.json"))
	var got userPromptSubmitOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("stdout not valid JSON: %v (%q)", err, out)
	}
	if got.Decision != ccConfigDecisionBlock || got.Continue == nil || *got.Continue || got.StopReason == "" {
		t.Fatalf("halt render = %+v, want decision:block + continue:false + stopReason", got)
	}
	if _, halted := hookflow.SessionHalted("cc-halt-1"); !halted {
		t.Fatal("the session must be latched after an emitted session halt")
	}

	before := hits.V3EvaluateAttempts()
	out2, _ := runConfigHook(t, configPayload("cc-halt-1", "local_settings", "/repo/.claude/settings.local.json"))
	var got2 userPromptSubmitOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(out2)), &got2); err != nil {
		t.Fatalf("replay stdout not valid JSON: %v (%q)", err, out2)
	}
	if got2.Continue == nil || *got2.Continue {
		t.Errorf("replayed halt = %+v, want continue:false again", got2)
	}
	if after := hits.V3EvaluateAttempts(); after != before {
		t.Errorf("latched session made %d further /evaluate calls, want 0 (the latch is the decided state)", after-before)
	}
}

// TestRunHook_ConfigChange_PolicySettingsNeverGated is the central
// policy_settings guard: source=="policy_settings" must never enter the gate, even when the
// server would block. Nothing is written (stdout or stderr), no enforcement
// record is filed (a block that never happened must never be claimed), and
// the observe copy still spools.
func TestRunHook_ConfigChange_PolicySettingsNeverGated(t *testing.T) {
	setupConfigChangeEnv(t)
	serveVerdict(t, `{"verdict":"block","reason":"would have blocked","policy_id":"cc-pol-2"}`)

	out, errOut := runConfigHook(t, configPayload("cc-3", "policy_settings", "/etc/claude/policy.json"))
	if strings.TrimSpace(out) != "" {
		t.Errorf("policy_settings must never be gated; stdout=%q", out)
	}
	if strings.Contains(errOut, "OpenBox blocked") {
		t.Errorf("policy_settings must never file a stderr denial line; stderr=%q", errOut)
	}
	if _, err := os.Stat(os.Getenv(envEnforcementFile)); err == nil {
		t.Error("policy_settings must never write an enforcement record (it would claim a block that never happened)")
	}

	eng := hookflow.NewEngine(os.Getenv("OPENBOX_SPOOL_DIR"))
	if eng.Spool.BacklogCount() == 0 {
		t.Error("the observe copy must still spool for policy_settings")
	}
}

// TestRunHook_ConfigChange_RawCompare_UnknownSourceStillGates guards the raw-
// vs-EnumOr distinction: an unrecognized future `source` must
// still be gated, not silently exempted by an allowlist round-trip.
func TestRunHook_ConfigChange_RawCompare_UnknownSourceStillGates(t *testing.T) {
	setupConfigChangeEnv(t)
	serveVerdict(t, `{"verdict":"block","reason":"new source blocked","policy_id":"cc-new"}`)

	out, _ := runConfigHook(t, configPayload("cc-4", "future_source_vendor_added", "/x/settings.json"))
	if strings.TrimSpace(out) == "" {
		t.Fatal("an unrecognized source must still be gated (raw compare), got no decision")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("stdout not valid JSON: %v (%q)", err, out)
	}
	if got["decision"] != "block" {
		t.Errorf("decision = %v, want block", got["decision"])
	}
}

// TestTouchesSessionRegistry_RestrictedToEleven pins that the registry
// touch (session -> cwd, for the git trailer) fires only for the 11 hooks
// that already perform it; none of the 21 new hooks (including ConfigChange
// and CwdChanged) write it.
func TestTouchesSessionRegistry_RestrictedToEleven(t *testing.T) {
	eleven := []HookName{
		HookSessionStart, HookUserPromptSubmit, HookPreToolUse, HookPostToolUse,
		HookPostToolUseFailure, HookSessionEnd, HookStop, HookSubagentStop,
		HookSubagentStart, HookPermissionDenied, HookStopFailure,
	}
	for _, h := range eleven {
		if !touchesSessionRegistry(h) {
			t.Errorf("touchesSessionRegistry(%s) = false, want true (one of the existing eleven)", h)
		}
	}
	newHooks := []HookName{
		HookConfigChange, HookCwdChanged, HookSetup, HookInstructionsLoaded,
		HookMessageDisplay, HookTaskCreated, HookFileChanged,
	}
	for _, h := range newHooks {
		if touchesSessionRegistry(h) {
			t.Errorf("touchesSessionRegistry(%s) = true, want false (a new hook must not write the registry)", h)
		}
	}
}

// TestRunHook_ConfigChange_RegistryNotTouched is the RunHook-level guard for
// the same restriction: no session record file appears after a ConfigChange
// hook, even though PreToolUse still writes one.
func TestRunHook_ConfigChange_RegistryNotTouched(t *testing.T) {
	setupConfigChangeEnv(t)
	serveVerdict(t, `{"verdict":"allow"}`)
	regDir := os.Getenv("OPENBOX_SESSION_DIR")

	_, _ = runConfigHook(t, configPayload("cc-reg-1", "user_settings", "/x"))
	if _, err := os.Stat(filepath.Join(regDir, "cc-reg-1.json")); err == nil {
		t.Error("ConfigChange must not write a session registry record")
	}

	var out bytes.Buffer
	RunHook("PreToolUse", strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"cc-reg-2","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`),
		&out, log.New(io.Discard, "", 0))
	if _, err := os.Stat(filepath.Join(regDir, "cc-reg-2.json")); err != nil {
		t.Error("PreToolUse (one of the existing eleven) must still write a session registry record")
	}
}

// TestSurfaceFindings_UnreachableForConfigChange is the RunHook-level trace
// that SurfaceFindings is guarded at hookrun.go by hook equality
// checks that ConfigChange (and every other new hook) can never satisfy.
//
// ConfigChange's own source is pinned to policy_settings, which is the one
// ConfigChange source that is never gated at all (raw compare, not through
// EnumOr) -- so it, like the other three hooks below, takes the observe path
// unconditionally and this proves SurfaceFindings is unreachable there, not
// merely that the (now-always-on) gate happened to deny first.
func TestSurfaceFindings_UnreachableForConfigChange(t *testing.T) {
	adv, _ := findingsEnv(t, true)
	isolateConfig(t)
	t.Setenv(envAgentID, testAgentID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	seedAdvisories(t, adv, hookflow.AdvisoryRecord{Verdict: "BLOCK", WouldBlock: true})

	for _, hook := range []string{"ConfigChange", "MessageDisplay", "Setup", "CwdChanged"} {
		var out bytes.Buffer
		payload := `{"hook_event_name":"` + hook + `","session_id":"s","cwd":"/tmp","source":"policy_settings"}`
		RunHook(hook, strings.NewReader(payload), &out, nopLogger())
		if strings.Contains(out.String(), "OpenBox governance") {
			t.Errorf("%s must never surface findings, got %q", hook, out.String())
		}
	}
}

// TestRecordConfigEnforcement_NoContentLeak: the stderr denial line carries only
// file_path, source, and the policy-authored reason -- never tool content,
// consistent with the config subject's Content-free DecisionRequest.
func TestRecordConfigEnforcement_NoContentLeak(t *testing.T) {
	enfFile := filepath.Join(t.TempDir(), "enforcements.jsonl")
	t.Setenv(envEnforcementFile, enfFile)
	var errBuf bytes.Buffer
	logger := log.New(&errBuf, "", 0)

	dec := decision.Decision{Source: "evaluate", Evaluation: client.Evaluation{
		Verdict: client.VerdictBlock, Reason: "no unsupervised MCP servers", PolicyID: "mcp-guard",
	}}
	ev := &HookEvent{SessionID: "s", Source: "project_settings", FilePath: "/repo/.claude/settings.json"}
	res := hookflow.ApplyDecision(&bytes.Buffer{}, dec, false, nil, configContract)
	recordConfigEnforcement(logger, ev, dec, res)

	if !strings.Contains(errBuf.String(), "project_settings") || !strings.Contains(errBuf.String(), "settings.json") {
		t.Errorf("stderr line missing file/source: %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no unsupervised MCP servers") {
		t.Errorf("stderr line missing the policy reason: %q", errBuf.String())
	}
}
