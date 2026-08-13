package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// ── shared enforce test helpers (consumed by enforce_test + enforce_conformance) ──

// isolateConfig points the dev config at a nonexistent temp file so no real
// ~/.config/openbox/dev.json is read (hermeticity; the TestMain sentinel in
// testmain_test.go is the structural backstop).
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(t.TempDir(), "none.json"))
}

// writeBundleFile marshals b to a temp bundle file and returns its path. A nil b
// returns a path to a file that does NOT exist, which the in-process decider treats
// as cold-start fail-open (VerdictUnknown / "fail-open:no-bundle").
func writeBundleFile(t *testing.T, b *decision.Bundle) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy-bundle.json")
	if b == nil {
		return path
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return path
}

// setBundleEnv points the full-hook decider (RunHook → newDecider → ResolveBundlePath)
// at a bundle file for b (nil → cold-start fail-open) and returns the path.
func setBundleEnv(t *testing.T, b *decision.Bundle) string {
	t.Helper()
	path := writeBundleFile(t, b)
	t.Setenv(envSidecarBundle, path)
	return path
}

// newTestDecider builds an in-process decider over a bundle file for b (nil →
// cold-start fail-open).
func newTestDecider(t *testing.T, b *decision.Bundle) decision.Decider {
	t.Helper()
	return decision.NewInProcessDecider(decision.InProcessConfig{BundlePath: writeBundleFile(t, b)})
}

// parsePreToolUse parses a Codex PreToolUse hook stdout line into its decision,
// reason, and raw updatedInput. Empty stdout → ("","",nil).
func parsePreToolUse(t *testing.T, out []byte) (decisionVal, reason string, updatedInput json.RawMessage) {
	t.Helper()
	if len(bytes.TrimSpace(out)) == 0 {
		return "", "", nil
	}
	var o preToolUseOutput
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("stdout not valid PreToolUse JSON: %v (%q)", err, out)
	}
	if o.HookSpecificOutput.HookEventName != string(HookPreToolUse) {
		t.Errorf("hookEventName = %q, want %q", o.HookSpecificOutput.HookEventName, HookPreToolUse)
	}
	return o.HookSpecificOutput.PermissionDecision, o.HookSpecificOutput.PermissionDecisionReason, o.HookSpecificOutput.UpdatedInput
}

// blockRuleBundle blocks a `rm -rf` shell command — the canonical enforce BLOCK.
func blockRuleBundle() *decision.Bundle {
	return &decision.Bundle{
		Version:         "conf-block",
		DefaultDecision: "allow",
		Rules: []decision.Rule{{
			ID:       "no-rm-rf",
			Match:    decision.RuleMatch{ToolKind: "shell", AttributeContains: map[string]string{"command": "rm -rf"}},
			Decision: "block",
			Reason:   "destructive recursive delete",
			PolicyID: "conf-policy",
		}},
	}
}

// ── unit tests ──

func TestResolveEnforce_Codex(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	write := func(j string) { _ = os.WriteFile(cfgPath, []byte(j), 0o600) }
	t.Setenv(devconfig.EnvConfigPath, cfgPath)
	os.Unsetenv(devconfig.EnvEnforce)

	// Default ON (ADR-0016 reversed the observe default). This adapter resolves
	// through devconfig, so the assertion pins that its facade kept no stale
	// default of its own.
	write(`{"developer_did":"` + testDID + `"}`)
	if !ResolveEnforce() {
		t.Error("an absent enforce field must resolve to ON (ADR-0016)")
	}
	// An explicit false still opts out — the property the *bool change bought.
	write(`{"developer_did":"` + testDID + `","enforce":false}`)
	if ResolveEnforce() {
		t.Error("enforce:false in config must opt out")
	}
	write(`{"developer_did":"` + testDID + `","enforce":true}`)
	if !ResolveEnforce() {
		t.Error("enforce:true in config should enable enforce mode")
	}
	t.Setenv(devconfig.EnvEnforce, "false")
	if ResolveEnforce() {
		t.Error("env false must override config true")
	}
}

func TestBuildDecisionRequest_Codex(t *testing.T) {
	id := Identity{DeveloperDID: testDID}

	t.Run("bash carries the local-only command axis", func(t *testing.T) {
		ev := &HookEvent{SessionID: "s1", PermissionMode: "default", ToolName: "Bash",
			ToolInput: []byte(`{"command":"rm -rf /tmp/x"}`)}
		req := buildDecisionRequest(id, ev, false)
		if req.SessionID != "s1" || req.DeveloperDID != testDID {
			t.Fatalf("identity not carried: %+v", req)
		}
		if req.EventType != client.EventToolCall {
			t.Errorf("event_type = %q, want ToolCall", req.EventType)
		}
		if req.Tool.Name != "Bash" || req.Tool.Kind != client.ToolShell {
			t.Errorf("tool = %+v, want Bash/shell", req.Tool)
		}
		if got := req.Attributes["command"]; got != "rm -rf /tmp/x" {
			t.Errorf("command attr = %q, want the shell command (local-only)", got)
		}
		if req.Content != nil {
			t.Errorf("shell tool must not carry Content, got %+v", req.Content)
		}
	})

	t.Run("apply_patch carries file_operation + patch body Content when redaction on", func(t *testing.T) {
		ev := &HookEvent{SessionID: "s2", ToolName: "apply_patch",
			ToolInput: []byte(`{"command":"*** Begin Patch\n+secret\n*** End Patch"}`)}
		req := buildDecisionRequest(id, ev, true)
		if req.Tool.Kind != client.ToolFile {
			t.Errorf("tool kind = %q, want file", req.Tool.Kind)
		}
		if got := req.Attributes["file_operation"]; got != "edit" {
			t.Errorf("file_operation = %v, want edit", got)
		}
		if _, ok := req.Attributes["command"]; ok {
			t.Errorf("apply_patch must NOT carry a command match axis (no file_path either): %+v", req.Attributes)
		}
		if req.Content == nil || !strings.Contains(req.Content.FileText, "+secret") {
			t.Errorf("patch body must ride Content.FileText for redaction, got %+v", req.Content)
		}
	})

	t.Run("redaction off ⇒ no Content (byte-identical to no-redaction)", func(t *testing.T) {
		ev := &HookEvent{SessionID: "s3", ToolName: "apply_patch",
			ToolInput: []byte(`{"command":"*** Begin Patch\n+x\n*** End Patch"}`)}
		if req := buildDecisionRequest(id, ev, false); req.Content != nil {
			t.Errorf("localRedaction off must leave Content nil, got %+v", req.Content)
		}
	})
}

// mapVerdict / applyDecision — the provider-shaped apply edge.
func TestMapVerdict_Codex(t *testing.T) {
	cases := []struct {
		name    string
		eval    client.Evaluation
		want    string
		wantSub string // substring the reason must contain ("" = proceed)
	}{
		{"HALT→deny", client.Evaluation{Verdict: client.VerdictHalt, Reason: "halted"}, codexDecisionDeny, "halted"},
		{"BLOCK→deny", client.Evaluation{Verdict: client.VerdictBlock, Reason: "blocked"}, codexDecisionDeny, "blocked"},
		{"guardrail-fail→deny", client.Evaluation{Verdict: client.VerdictAllow, Guardrail: &client.GuardrailResult{Passed: false, Reasons: []client.GuardrailReason{{Type: "pii"}}}}, codexDecisionDeny, "pii"},
		{"REQUIRE_APPROVAL→deny (OD-SL7-ASK)", client.Evaluation{Verdict: client.VerdictRequireApproval}, codexDecisionDeny, "approval"},
		{"CONSTRAIN→proceed", client.Evaluation{Verdict: client.VerdictConstrain}, "", ""},
		{"ALLOW→proceed", client.Evaluation{Verdict: client.VerdictAllow}, "", ""},
		{"UNKNOWN→proceed", client.Evaluation{Verdict: client.VerdictUnknown}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, reason := hookflow.MapVerdict(c.eval, contract)
			if d != c.want {
				t.Fatalf("decision = %q, want %q", d, c.want)
			}
			if c.want == codexDecisionDeny && c.wantSub != "" && !strings.Contains(reason, c.wantSub) {
				t.Errorf("reason = %q, want it to contain %q", reason, c.wantSub)
			}
			// Tighten-only: mapVerdict NEVER yields allow.
			if d == codexDecisionAllow {
				t.Errorf("mapVerdict must never emit allow (tighten-only)")
			}
		})
	}
}

func TestApplyDecision_Codex(t *testing.T) {
	t.Run("deny emits permissionDecision:deny + reason, no updatedInput", func(t *testing.T) {
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictBlock, Reason: "nope", PolicyID: "p1"}}
		var out bytes.Buffer
		applied, emitted := applyDecision(&out, dec, false, []byte(`{"command":"x"}`))
		if !emitted || applied != codexDecisionDeny {
			t.Fatalf("applied=%q emitted=%v, want deny/true", applied, emitted)
		}
		d, reason, ui := parsePreToolUse(t, out.Bytes())
		if d != codexDecisionDeny || !strings.Contains(reason, "nope") || len(ui) != 0 {
			t.Errorf("deny output wrong: d=%q reason=%q ui=%s", d, reason, ui)
		}
	})

	t.Run("proceed with no redaction writes NOTHING (observe-identical)", func(t *testing.T) {
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictAllow}}
		var out bytes.Buffer
		applied, emitted := applyDecision(&out, dec, true, []byte(`{"command":"x"}`))
		if emitted || applied != "" || out.Len() != 0 {
			t.Errorf("proceed must write nothing, got applied=%q out=%q", applied, out.String())
		}
	})

	t.Run("redaction emits allow+updatedInput (Codex rewrite contract), never bare allow", func(t *testing.T) {
		dec := decision.Decision{
			Evaluation:      client.Evaluation{Verdict: client.VerdictAllow},
			RedactedContent: &client.Content{FileText: "*** Begin Patch\n+KEY=${OPENBOX_REDACTED_AWS_KEY}\n*** End Patch"},
		}
		orig := []byte(`{"command":"*** Begin Patch\n+KEY=AKIAIOSFODNN7EXAMPLE\n*** End Patch"}`)
		var out bytes.Buffer
		applied, emitted := applyDecision(&out, dec, true, orig)
		if !emitted || applied != codexDecisionAllow {
			t.Fatalf("applied=%q emitted=%v, want allow/true", applied, emitted)
		}
		d, _, ui := parsePreToolUse(t, out.Bytes())
		if d != codexDecisionAllow {
			t.Fatalf("redaction must ride permissionDecision:allow (Codex rejects a bare updatedInput), got %q", d)
		}
		if len(ui) == 0 {
			t.Fatal("allow must carry a non-empty updatedInput")
		}
		if strings.Contains(out.String(), "AKIAIOSFODNN7EXAMPLE") {
			t.Errorf("raw secret must not appear on stdout: %s", out.String())
		}
		if !strings.Contains(out.String(), "OPENBOX_REDACTED") {
			t.Errorf("expected the redaction placeholder in updatedInput: %s", out.String())
		}
		// The rewritten command field is present + structural shape preserved.
		var m map[string]any
		_ = json.Unmarshal(ui, &m)
		if _, ok := m["command"]; !ok {
			t.Errorf("updatedInput must keep the command field, got %v", m)
		}
	})

	t.Run("nil stdout fails open (never wedges)", func(t *testing.T) {
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictBlock}}
		if applied, emitted := applyDecision(nil, dec, false, nil); emitted || applied != "" {
			t.Errorf("nil stdout must fail open, got applied=%q emitted=%v", applied, emitted)
		}
	})
}

func TestRedactToolInput_Codex(t *testing.T) {
	t.Run("swaps command, preserves other fields verbatim", func(t *testing.T) {
		orig := json.RawMessage(`{"command":"secret-body","note":"keep-me"}`)
		out := hookflow.RedactToolInput(orig, "clean-body", contract.ContentFieldKeys())
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("bad output: %v", err)
		}
		if m["command"] != "clean-body" {
			t.Errorf("command = %v, want clean-body", m["command"])
		}
		if m["note"] != "keep-me" {
			t.Errorf("structural field note must survive verbatim, got %v", m["note"])
		}
	})
	t.Run("no recognized content field ⇒ nil (nothing safe to rewrite)", func(t *testing.T) {
		if out := hookflow.RedactToolInput(json.RawMessage(`{"other":"x"}`), "y", contract.ContentFieldKeys()); out != nil {
			t.Errorf("expected nil for no content field, got %s", out)
		}
	})
	t.Run("non-object ⇒ nil", func(t *testing.T) {
		if out := hookflow.RedactToolInput(json.RawMessage(`"a string"`), "y", contract.ContentFieldKeys()); out != nil {
			t.Errorf("expected nil for non-object, got %s", out)
		}
	})
}

func TestApplyFailurePolicy_Codex(t *testing.T) {
	realAllow := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictAllow}, FailOpen: false, Source: "local-bundle"}
	outage := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictUnknown}, FailOpen: true, Source: "fail-open:no-bundle"}

	if got := hookflow.ApplyFailurePolicy(outage, hookflow.FailOpen); got.Evaluation.WouldBlock() {
		t.Error("fail-open + outage must proceed")
	}
	if got := hookflow.ApplyFailurePolicy(outage, hookflow.FailClosed); !got.Evaluation.WouldBlock() {
		t.Error("fail-closed + outage must synthesize a HALT (deny)")
	}
	if got := hookflow.ApplyFailurePolicy(realAllow, hookflow.FailClosed); got.Evaluation.WouldBlock() {
		t.Error("fail-closed must NOT deny a REAL allow (engages on no-verdict only)")
	}
}

// EnforceDecision obtains a real deny from a bundled decider (no network).
func TestEnforceDecision_ObtainsRealVerdict(t *testing.T) {
	dec := EnforceDecision(context.Background(), newTestDecider(t, blockRuleBundle()),
		Identity{DeveloperDID: testDID},
		&HookEvent{SessionID: "s", ToolName: "Bash", ToolInput: []byte(`{"command":"rm -rf /"}`)}, false)
	if !dec.Evaluation.WouldBlock() {
		t.Fatalf("expected a real BLOCK verdict, got %+v", dec)
	}
	if dec.FailOpen {
		t.Errorf("a real verdict must be hookflow.FailOpen=false, got %+v", dec)
	}
}

// The adapter-owned clamps are DERIVED from the installed gate-hook timeout, not
// copied from Claude Code's constants (OD-SL7-T2-TIMEOUT).
func TestClampsDerivedFromInstalledTimeout(t *testing.T) {
	if (Engine{}).HookCeilings().Gating != time.Duration(preToolUseHookTimeoutSec)*time.Second {
		t.Errorf("(Engine{}).HookCeilings().Gating must derive from the installer's preToolUseHookTimeoutSec")
	}
	if hookflow.EnforceBudget((Engine{}).HookCeilings()) != (Engine{}).HookCeilings().Gating-hookflow.HookBudgetMargin {
		t.Errorf("hookflow.EnforceBudget((Engine{}).HookCeilings()) must be the installed timeout minus the margin")
	}
	if hookflow.EnforceBudget((Engine{}).HookCeilings()) >= (Engine{}).HookCeilings().Gating {
		t.Errorf("the whole-hook budget must land strictly before Codex's hook kill (probe P1 fail-open)")
	}
	if maxTier2Timeout > hookflow.EnforceBudget((Engine{}).HookCeilings()) {
		t.Errorf("the T2 clamp must stay within the whole-hook budget")
	}
}
