package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

func TestResolveEnforce(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	write := func(json string) { _ = os.WriteFile(cfgPath, []byte(json), 0o600) }
	t.Setenv(envConfigPath, cfgPath)
	os.Unsetenv(envEnforce) // env genuinely absent → config decides

	// Default: no config field, no env → TRUE (ADR-0016 reversed the observe
	// default). The adapter resolves through devconfig, so this pins that the
	// facade did not keep a stale default of its own.
	write(`{"developer_did":"` + testDID + `"}`)
	if !ResolveEnforce() {
		t.Error("an absent enforce field must resolve to ON (ADR-0016)")
	}
	// An explicit false still opts out.
	write(`{"developer_did":"` + testDID + `","enforce":false}`)
	if ResolveEnforce() {
		t.Error("enforce:false in config must opt out")
	}

	// Config enables enforce mode.
	write(`{"developer_did":"` + testDID + `","enforce":true}`)
	if !ResolveEnforce() {
		t.Error("enforce:true in config should enable enforce mode")
	}

	// Env overrides config either way.
	t.Setenv(envEnforce, "false")
	if ResolveEnforce() {
		t.Error("env false must override config true")
	}
	write(`{"developer_did":"` + testDID + `"}`)
	t.Setenv(envEnforce, "1")
	if !ResolveEnforce() {
		t.Error("env 1 must override config absent/false")
	}
}

func TestBuildDecisionRequest(t *testing.T) {
	id := Identity{DeveloperDID: testDID}

	t.Run("bash carries the local-only command", func(t *testing.T) {
		ev := &HookEvent{
			SessionID:      "sess-1",
			PermissionMode: "default",
			ToolName:       "Bash",
			ToolInput:      []byte(`{"command":"rm -rf /tmp/x"}`),
		}
		req := buildDecisionRequest(id, ev, false)
		if req.SessionID != "sess-1" || req.DeveloperDID != testDID {
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
		if got := req.Attributes["permission_mode"]; got != "default" {
			t.Errorf("permission_mode = %q", got)
		}
	})

	t.Run("file tool carries path + operation, not a command", func(t *testing.T) {
		ev := &HookEvent{
			SessionID: "sess-2",
			ToolName:  "Write",
			ToolInput: []byte(`{"file_path":"/etc/passwd","content":"secret"}`),
		}
		req := buildDecisionRequest(id, ev, false)
		if req.Tool.Kind != client.ToolFile {
			t.Errorf("kind = %q, want file", req.Tool.Kind)
		}
		if got := req.Attributes["file_path"]; got != "/etc/passwd" {
			t.Errorf("file_path = %q", got)
		}
		if got := req.Attributes["file_operation"]; got != "write" {
			t.Errorf("file_operation = %q, want write", got)
		}
		if _, ok := req.Attributes["command"]; ok {
			t.Error("a file tool must not carry a command attribute")
		}
	})

	t.Run("mcp tool carries server + function", func(t *testing.T) {
		ev := &HookEvent{
			SessionID: "sess-3",
			ToolName:  "mcp__github__create_issue",
		}
		req := buildDecisionRequest(id, ev, false)
		if req.Tool.Kind != client.ToolMCP || req.Tool.MCPServer != "github" {
			t.Errorf("tool = %+v, want mcp/github", req.Tool)
		}
		if got := req.Attributes["mcp_function"]; got != "create_issue" {
			t.Errorf("mcp_function = %q", got)
		}
	})

	t.Run("content is nil when content-capture is off (INV-2 default)", func(t *testing.T) {
		// The OD4 default: even a file-write body is never carried when capture is off.
		ev := &HookEvent{SessionID: "s", ToolName: "Write", ToolInput: []byte(`{"file_path":"/x","content":"secret"}`)}
		if req := buildDecisionRequest(id, ev, false); req.Content != nil {
			t.Errorf("Content must stay nil with content-capture off, got %+v", req.Content)
		}
	})
}

// TestBuildDecisionRequest_ContentGating covers E6-S4 AC-5: the file BODY is carried
// on the LOCAL DecisionRequest ONLY when content-capture is on, and ONLY for a file
// tool; it is never carried for a non-file tool and never with capture off.
func TestBuildDecisionRequest_ContentGating(t *testing.T) {
	id := Identity{DeveloperDID: testDID}

	t.Run("file write body carried when capture on", func(t *testing.T) {
		ev := &HookEvent{SessionID: "s", ToolName: "Write", ToolInput: []byte(`{"file_path":"/x","content":"api_key=SECRET"}`)}
		req := buildDecisionRequest(id, ev, true)
		if req.Content == nil || req.Content.FileText != "api_key=SECRET" {
			t.Fatalf("Content = %+v, want the file body carried locally", req.Content)
		}
		if req.Content.Prompt != "" || req.Content.Output != "" {
			t.Errorf("only the file body should be set, got %+v", req.Content)
		}
	})

	t.Run("Edit new_string carried when capture on", func(t *testing.T) {
		ev := &HookEvent{SessionID: "s", ToolName: "Edit", ToolInput: []byte(`{"file_path":"/x","old_string":"a","new_string":"b-with-token"}`)}
		req := buildDecisionRequest(id, ev, true)
		if req.Content == nil || req.Content.FileText != "b-with-token" {
			t.Fatalf("Content = %+v, want the new_string carried", req.Content)
		}
	})

	t.Run("non-file tool carries no content even with capture on", func(t *testing.T) {
		ev := &HookEvent{SessionID: "s", ToolName: "Bash", ToolInput: []byte(`{"command":"echo hi"}`)}
		if req := buildDecisionRequest(id, ev, true); req.Content != nil {
			t.Errorf("a Bash tool must carry no Content, got %+v", req.Content)
		}
	})

	t.Run("empty body carries no content", func(t *testing.T) {
		ev := &HookEvent{SessionID: "s", ToolName: "Write", ToolInput: []byte(`{"file_path":"/x"}`)}
		if req := buildDecisionRequest(id, ev, true); req.Content != nil {
			t.Errorf("an absent body must carry no Content, got %+v", req.Content)
		}
	})
}

func TestCapCommand_ByteBoundedRuneSafe(t *testing.T) {
	// A short command passes through unchanged.
	if got := hookflow.CapCommand("rm -rf /"); got != "rm -rf /" {
		t.Errorf("short command changed: %q", got)
	}
	// A long multibyte command must be bounded by BYTES (not runes) so the
	// marshaled request cannot overrun the server's byte read-limit (G_SEC LOW-1),
	// and must never split a rune (valid UTF-8 preserved).
	long := strings.Repeat("é", hookflow.MaxCommandLen) // 2 bytes/rune → ~2× the byte budget
	got := hookflow.CapCommand(long)
	if len(got) > hookflow.MaxCommandLen {
		t.Errorf("capCommand returned %d bytes, want <= %d (byte bound)", len(got), hookflow.MaxCommandLen)
	}
	if !utf8.ValidString(got) {
		t.Error("capCommand split a multibyte rune (invalid UTF-8)")
	}
}

// TestEnforceDecision_FailOpenWhenSidecarAbsent and
// TestEnforceDecision_LiveBlock are deleted with the local evaluator
// (ADR-0017).
//
// The first asserted that an absent bundle degrades to a prompt fail-open allow
// rather than blocking; the second, that a loaded BLOCK rule denies the matching
// call with the policy reason and id. Both were about the decider that no longer
// exists. The surviving local step decides nothing and cannot block, so the
// fail-open property holds by construction; the block property moved to the
// server and is asserted end to end by C1 and C12 against a real /evaluate.

func TestRunHook_EnforceGate(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())

	payload := `{"hook_event_name":"PreToolUse","session_id":"sess-1","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`

	run := func() string {
		var stderr, stdout bytes.Buffer
		logger := log.New(&stderr, "", 0)
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, logger)
		// A cold-start (bundle-less) sidecar fails open → VerdictUnknown → the apply
		// emits NOTHING (tighten-only): stdout must stay empty in every case here.
		if stdout.Len() != 0 {
			t.Errorf("fail-open/allow decision must not write to stdout; stdout=%q", stdout.String())
		}
		return stderr.String()
	}

	// Enforce OFF → the gate does not run; no enforce diagnostic emitted (the
	// in-process behavioral analog of "the sidecar was never dialed").
	t.Setenv(envEnforce, "0")
	if out := run(); strings.Contains(out, "enforce decision:") {
		t.Errorf("enforce-off must not run the gate; stderr=%q", out)
	}

	// Enforce ON → the PreToolUse hook synchronously obtains + logs a decision.
	t.Setenv(envEnforce, "1")
	if out := run(); !strings.Contains(out, "enforce decision:") {
		t.Errorf("enforce-on must obtain + log a decision; stderr=%q", out)
	}
}

// TestRunHook_EnforceOnlyPreToolUse guards AC-6: even in enforce mode, a
// non-PreToolUse hook never dials the sidecar (the gate is a pre-execution concept).
func TestRunHook_EnforceOnlyPreToolUse(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforce, "1")

	// PostToolUse in enforce mode must still only observe: the gate never runs, so no
	// "enforce decision:" diagnostic is emitted (the in-process analog of "never dialed").
	payload := `{"hook_event_name":"PostToolUse","session_id":"sess-1","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	var stderr bytes.Buffer
	logger := log.New(&stderr, "", 0)
	RunHook("PostToolUse", strings.NewReader(payload), &bytes.Buffer{}, logger)
	if strings.Contains(stderr.String(), "enforce decision:") {
		t.Errorf("PostToolUse must not run the enforce gate even in enforce mode; stderr=%q", stderr.String())
	}
}

// ── E6-S2: apply(verdict) tests ──────────────────────────────────────────────

// parsePermissionDecision decodes a PreToolUse hook stdout blob and returns the
// permissionDecision + reason (empty decision when nothing was written).
func parsePermissionDecision(t *testing.T, out []byte) (decision, reason string) {
	t.Helper()
	if len(bytes.TrimSpace(out)) == 0 {
		return "", ""
	}
	var got preToolUseOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stdout is not valid PreToolUse JSON: %v (%q)", err, out)
	}
	if got.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", got.HookSpecificOutput.HookEventName)
	}
	return got.HookSpecificOutput.PermissionDecision, got.HookSpecificOutput.PermissionDecisionReason
}

// TestMapVerdict exercises the full SDK cascade port (OD-ENF-SCOPE):
// HALT/BLOCK/guardrail-fail → deny; REQUIRE_APPROVAL → ask; CONSTRAIN/ALLOW/
// UNKNOWN → proceed (no decision). Order matters: a guardrail failure denies
// regardless of the (non-HALT/BLOCK) verdict, matching verdict_handler.py:84-90.
func TestMapVerdict(t *testing.T) {
	guardFail := &client.GuardrailResult{Passed: false, Reasons: []client.GuardrailReason{{Type: "pii", Reason: "secret detail"}}}
	guardPass := &client.GuardrailResult{Passed: true}

	cases := []struct {
		name     string
		eval     client.Evaluation
		wantDec  string
		wantEmit bool
		// reasonNoLeak asserts the free text below is NOT present in the reason.
		reasonNoLeak string
		reasonHas    string
	}{
		{"halt denies", client.Evaluation{Verdict: client.VerdictHalt, Reason: "kill switch"}, ccDecisionDeny, true, "", "kill switch"},
		{"block denies with policy id", client.Evaluation{Verdict: client.VerdictBlock, Reason: "no rm -rf", PolicyID: "p-1"}, ccDecisionDeny, true, "", "p-1"},
		{"require_approval asks", client.Evaluation{Verdict: client.VerdictRequireApproval, Reason: "needs review"}, ccDecisionAsk, true, "", "needs review"},
		// E6-S6: the approval id (server correlation id) is surfaced on the ask reason.
		{"require_approval surfaces approval id", client.Evaluation{Verdict: client.VerdictRequireApproval, Reason: "needs review", ApprovalID: "appr-42"}, ccDecisionAsk, true, "", "appr-42"},
		{"constrain proceeds", client.Evaluation{Verdict: client.VerdictConstrain, Reason: "scoped"}, "", false, "", ""},
		{"allow proceeds", client.Evaluation{Verdict: client.VerdictAllow}, "", false, "", ""},
		{"unknown (fail-open) proceeds", client.Evaluation{Verdict: client.VerdictUnknown}, "", false, "", ""},
		// Guardrail failure denies even when the verdict itself would proceed, and
		// the reason carries only the CATEGORY (pii), never the free text (INV-2).
		{"guardrail fail denies (category only)", client.Evaluation{Verdict: client.VerdictAllow, Guardrail: guardFail}, ccDecisionDeny, true, "secret detail", "pii"},
		{"guardrail pass does not deny", client.Evaluation{Verdict: client.VerdictAllow, Guardrail: guardPass}, "", false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec, reason := hookflow.MapVerdict(c.eval, contract)
			if dec != c.wantDec {
				t.Errorf("decision = %q, want %q", dec, c.wantDec)
			}
			if (dec != "") != c.wantEmit {
				t.Errorf("emit = %t, want %t", dec != "", c.wantEmit)
			}
			if c.reasonHas != "" && !strings.Contains(reason, c.reasonHas) {
				t.Errorf("reason %q missing %q", reason, c.reasonHas)
			}
			if c.reasonNoLeak != "" && strings.Contains(reason, c.reasonNoLeak) {
				t.Errorf("reason %q leaked guardrail free text %q (INV-2)", reason, c.reasonNoLeak)
			}
		})
	}
}

func TestApplyDecision(t *testing.T) {
	t.Run("block writes a deny permissionDecision", func(t *testing.T) {
		var out bytes.Buffer
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictBlock, Reason: "destructive"}}
		applied, emitted := applyDecision(&out, dec, false, nil)
		if !emitted || applied != ccDecisionDeny {
			t.Fatalf("applied=%q emitted=%t, want deny/true", applied, emitted)
		}
		d, reason := parsePermissionDecision(t, out.Bytes())
		if d != "deny" {
			t.Errorf("permissionDecision = %q, want deny", d)
		}
		if !strings.Contains(reason, "destructive") {
			t.Errorf("reason = %q, want the policy reason surfaced", reason)
		}
	})

	t.Run("allow writes nothing (tighten-only)", func(t *testing.T) {
		var out bytes.Buffer
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictAllow}}
		if applied, emitted := applyDecision(&out, dec, false, nil); emitted || applied != "" {
			t.Errorf("allow must emit nothing, got applied=%q emitted=%t", applied, emitted)
		}
		if out.Len() != 0 {
			t.Errorf("allow wrote to stdout: %q", out.String())
		}
	})

	t.Run("fail-open (unknown) writes nothing", func(t *testing.T) {
		var out bytes.Buffer
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictUnknown}, FailOpen: true}
		if _, emitted := applyDecision(&out, dec, false, nil); emitted || out.Len() != 0 {
			t.Errorf("fail-open must not write a decision; stdout=%q", out.String())
		}
	})

	t.Run("nil stdout never panics and reports not-emitted", func(t *testing.T) {
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictBlock}}
		if _, emitted := applyDecision(nil, dec, false, nil); emitted {
			t.Error("a nil stdout must degrade to not-emitted (fail-open)")
		}
	})
}

// ── E6-S4/E6-S9: input redaction application (updatedInput) ──────────────────

// contentField extracts a tool_input's content field (content|new_string) for a
// test assertion.
func contentField(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not an object: %s", raw)
	}
	for _, k := range []string{"content", "new_string"} {
		if v, ok := m[k]; ok {
			var s string
			_ = json.Unmarshal(v, &s)
			return s
		}
	}
	return ""
}

// TestApplyInputRedaction covers the E6-S9 content-field reconstruction and its
// gates (AC-1/AC-3/AC-5): it rebuilds the ORIGINAL tool_input with ONLY the content
// field replaced by the redacted body, when local redaction is on and the result
// differs; otherwise nil (no rewrite).
func TestApplyInputRedaction(t *testing.T) {
	writeOrig := json.RawMessage(`{"file_path":"/x","content":"api_key=SECRET"}`)
	editOrig := json.RawMessage(`{"file_path":"/y","old_string":"a","new_string":"tok=SECRET"}`)
	redactedBody := "api_key=${OPENBOX_REDACTED_SECRET_ASSIGNMENT}"

	rc := func(body string) *client.Content { return &client.Content{FileText: body} }

	cases := []struct {
		name           string
		localRedaction bool
		redacted       *client.Content
		orig           json.RawMessage
		wantContent    string // "" ⇒ expect nil (no rewrite); else expected content-field value
	}{
		{"applies to Write.content", true, rc(redactedBody), writeOrig, redactedBody},
		{"applies to Edit.new_string", true, rc("tok=${OPENBOX_REDACTED_SECRET_ASSIGNMENT}"), editOrig, "tok=${OPENBOX_REDACTED_SECRET_ASSIGNMENT}"},
		{"inert when local redaction off", false, rc(redactedBody), writeOrig, ""},
		{"nil when no RedactedContent", true, nil, writeOrig, ""},
		{"nil when empty RedactedContent body", true, rc(""), writeOrig, ""},
		{"nil when redacted body equals original", true, rc("api_key=SECRET"), writeOrig, ""},
		{"nil when original has no content field", true, rc(redactedBody), json.RawMessage(`{"file_path":"/x"}`), ""},
		{"nil when original unparseable", true, rc(redactedBody), json.RawMessage(`{bad`), ""},
		{"nil when original is an empty object", true, rc(redactedBody), json.RawMessage(`{}`), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hookflow.ApplyInputRedaction(decision.Decision{RedactedContent: c.redacted}, c.localRedaction, c.orig, contract.ContentFieldKeys())
			if c.wantContent == "" {
				if got != nil {
					t.Errorf("want nil (no rewrite), got %s", got)
				}
				return
			}
			if cf := contentField(t, got); cf != c.wantContent {
				t.Errorf("content field = %q, want %q (out=%s)", cf, c.wantContent, got)
			}
		})
	}
}

// TestApplyInputRedaction_StructuralFieldsInviolable covers AC-2 (the E6-S7
// carry-forward): the emitted updatedInput differs from the original ONLY in the
// content field; structural locators are carried over verbatim and can never be
// altered by the sidecar's returned body (the body is a plain string; only the
// content field can change).
func TestApplyInputRedaction_StructuralFieldsInviolable(t *testing.T) {
	orig := json.RawMessage(`{"file_path":"/etc/app.conf","content":"password=hunter2secret9999","mode":"0644"}`)
	got := hookflow.ApplyInputRedaction(
		decision.Decision{RedactedContent: &client.Content{FileText: "password=${OPENBOX_REDACTED_SECRET_ASSIGNMENT}"}},
		true, orig, contract.ContentFieldKeys())
	if got == nil {
		t.Fatal("expected a rewrite")
	}
	var o, g map[string]json.RawMessage
	_ = json.Unmarshal(orig, &o)
	_ = json.Unmarshal(got, &g)
	for _, k := range []string{"file_path", "mode"} {
		if string(o[k]) != string(g[k]) {
			t.Errorf("structural field %q changed: %s → %s", k, o[k], g[k])
		}
	}
	if string(o["content"]) == string(g["content"]) {
		t.Error("content field was not redacted")
	}
	if strings.Contains(string(got), "hunter2secret9999") {
		t.Errorf("secret survived: %s", got)
	}
}

// TestApplyInputRedaction_NonEmptyFieldSelection covers G3 Finding 1 / G_SEC LOW-1:
// redactToolInput must write the redacted body into the SAME field fileText() reads
// — the first NON-EMPTY content key. On a degenerate {"content":"","new_string":
// "<secret>"} (or content:null), the redaction must land on new_string (where the
// secret is), never into the empty content (which would leave the secret in place
// AND corrupt the call). This is the field the scanner scanned.
func TestApplyInputRedaction_NonEmptyFieldSelection(t *testing.T) {
	redactedBody := "tok=${OPENBOX_REDACTED_SECRET_ASSIGNMENT}"
	for _, orig := range []json.RawMessage{
		json.RawMessage(`{"file_path":"/x","content":"","new_string":"tok=AKIAIOSFODNN7EXAMPLE"}`),
		json.RawMessage(`{"file_path":"/x","content":null,"new_string":"tok=AKIAIOSFODNN7EXAMPLE"}`),
	} {
		got := hookflow.ApplyInputRedaction(decision.Decision{RedactedContent: &client.Content{FileText: redactedBody}}, true, orig, contract.ContentFieldKeys())
		if got == nil {
			t.Fatalf("expected a rewrite for %s", orig)
		}
		if strings.Contains(string(got), "AKIAIOSFODNN7EXAMPLE") {
			t.Errorf("secret survived (redacted the wrong field): %s → %s", orig, got)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(got, &m)
		var ns string
		_ = json.Unmarshal(m["new_string"], &ns)
		if ns != redactedBody {
			t.Errorf("new_string not redacted: %s", got)
		}
	}
}

// TestApplyDecision_Redaction covers AC-1/AC-4/AC-5: on the proceed path a redaction
// is emitted as updatedInput ALONE (no permissionDecision); a deny/ask carries no
// updatedInput; and with local redaction off the proceed path writes nothing
// (byte-identical to E6-S3).
func TestApplyDecision_Redaction(t *testing.T) {
	orig := json.RawMessage(`{"file_path":"/x","content":"api_key=SECRET"}`)
	rc := &client.Content{FileText: "api_key=${OPENBOX_REDACTED_SECRET_ASSIGNMENT}"}

	t.Run("proceed + redaction on → updatedInput alone", func(t *testing.T) {
		var out bytes.Buffer
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictAllow}, RedactedContent: rc}
		applied, emitted := applyDecision(&out, dec, true, orig)
		if applied != "" || !emitted {
			t.Fatalf("applied=%q emitted=%t, want proceed(\"\")/emitted(true)", applied, emitted)
		}
		var got preToolUseOutput
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("stdout not valid JSON: %v (%q)", err, out.String())
		}
		if got.HookSpecificOutput.PermissionDecision != "" {
			t.Errorf("permissionDecision must be absent on a redaction-only output, got %q", got.HookSpecificOutput.PermissionDecision)
		}
		if cf := contentField(t, got.HookSpecificOutput.UpdatedInput); cf != rc.FileText {
			t.Errorf("updatedInput content = %q, want redacted", cf)
		}
		if strings.Contains(out.String(), `"permissionDecision":"allow"`) {
			t.Errorf("must never emit permissionDecision:allow (tighten-only): %q", out.String())
		}
	})

	t.Run("proceed + redaction OFF → nothing (E6-S3 identical)", func(t *testing.T) {
		var out bytes.Buffer
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictAllow}, RedactedContent: rc}
		if _, emitted := applyDecision(&out, dec, false, orig); emitted || out.Len() != 0 {
			t.Errorf("redaction off must write nothing, got %q", out.String())
		}
	})

	t.Run("deny carries no updatedInput even with a redaction present", func(t *testing.T) {
		var out bytes.Buffer
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictBlock, Reason: "nope"}, RedactedContent: rc}
		applied, _ := applyDecision(&out, dec, true, orig)
		if applied != ccDecisionDeny {
			t.Fatalf("want deny, got %q", applied)
		}
		var got preToolUseOutput
		_ = json.Unmarshal(out.Bytes(), &got)
		if len(got.HookSpecificOutput.UpdatedInput) != 0 {
			t.Errorf("a deny must not rewrite the input, got updatedInput=%s", got.HookSpecificOutput.UpdatedInput)
		}
	})

	t.Run("ask carries no updatedInput (faithful to the SDK)", func(t *testing.T) {
		var out bytes.Buffer
		dec := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictRequireApproval}, RedactedContent: rc}
		applied, _ := applyDecision(&out, dec, true, orig)
		if applied != ccDecisionAsk {
			t.Fatalf("want ask, got %q", applied)
		}
		var got preToolUseOutput
		_ = json.Unmarshal(out.Bytes(), &got)
		if len(got.HookSpecificOutput.UpdatedInput) != 0 {
			t.Errorf("ask must not rewrite the input, got updatedInput=%s", got.HookSpecificOutput.UpdatedInput)
		}
	})
}

// TestRecordEnforcement_NoRedactionLeak covers AC-3: the redacted content lives on
// the sidecar Decision (LOCAL-only) and must NEVER be serialized into the durable
// enforcement audit — the audit stays content-free even for a proceed+redaction,
// carrying only the category NAMES.
func TestRecordEnforcement_NoRedactionLeak(t *testing.T) {
	enfFile := filepath.Join(t.TempDir(), "enforcements.jsonl")
	t.Setenv(envEnforcementFile, enfFile)
	logger := log.New(&bytes.Buffer{}, "", 0)

	dec := decision.Decision{Source: "local-bundle",
		RedactedContent:     &client.Content{FileText: "api_key=REDACTION_SENTINEL"},
		RedactionCategories: []string{"secret_assignment"},
		Evaluation:          client.Evaluation{Verdict: client.VerdictAllow}}
	ev := &HookEvent{SessionID: "s", ToolName: "Write", ToolInput: []byte(`{"file_path":"/x","content":"api_key=ORIGINAL_SENTINEL"}`)}

	var out bytes.Buffer
	res := hookflow.ApplyDecision(&out, dec, true, ev.ToolInput, contract)
	recordEnforcement(logger, ev, dec, res)

	data, err := os.ReadFile(enfFile)
	if err != nil {
		t.Fatalf("enforcement sink not written: %v", err)
	}
	for _, sentinel := range []string{"REDACTION_SENTINEL", "ORIGINAL_SENTINEL"} {
		if strings.Contains(string(data), sentinel) {
			t.Errorf("enforcement audit leaked content %q (INV-2): %s", sentinel, data)
		}
	}
	// The content-free category signal IS recorded.
	if !strings.Contains(string(data), "secret_assignment") || !strings.Contains(string(data), `"redacted":true`) {
		t.Errorf("expected content-free redaction signal in audit, got %s", data)
	}
	// The redacted body IS applied locally via updatedInput (stdout → CC, on-machine).
	if !strings.Contains(out.String(), "REDACTION_SENTINEL") {
		t.Errorf("expected the redacted body to be applied via updatedInput, stdout=%q", out.String())
	}
}

// TestRunHook_EnforceApply_Block is the E6-S2 end-to-end guard: enforce ON + a
// live sidecar whose bundle blocks `rm -rf` → a PreToolUse hook writes a `deny`
// permissionDecision to stdout AND appends a content-free durable enforcement
// record. A benign command in the same session writes nothing (tighten-only) and
// records a proceed. INV-2: neither surface carries the shell command.
func TestRunHook_EnforceApply_Block(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforce, "1")
	enfFile := filepath.Join(t.TempDir(), "enforcements.jsonl")
	t.Setenv(envEnforcementFile, enfFile)
	// The verdict comes from the control plane now (ADR-0017); what this case
	// asserts — the apply cascade and the durable audit line — is unchanged. The
	// stub answers per COMMAND, the way the rule it replaces did, so the benign
	// half is still a genuine proceed rather than an absent server.
	//
	// Content capture is on so the stub can see the command it is judging. That
	// is the real posture for content-aware policy; the INV-2 assertions below
	// are about stdout and the local audit, which stay content-free either way.
	t.Setenv(envContentCapture, "1")
	blockRmRf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(raw), "rm -rf") {
			_, _ = w.Write([]byte(`{"verdict":"block","reason":"destructive recursive delete","policy_id":"test-policy"}`))
			return
		}
		_, _ = w.Write([]byte(`{"verdict":"allow"}`))
	}))
	defer blockRmRf.Close()
	tier2Creds(t, blockRmRf.URL)
	t.Setenv(envContentCapture, "1")

	run := func(payload string) string {
		var stdout bytes.Buffer
		logger := log.New(&bytes.Buffer{}, "", 0)
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, logger)
		return stdout.String()
	}

	// Dangerous command → deny on stdout, with the policy reason (never the command).
	danger := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"rm -rf /tmp/x"}}`
	out := run(danger)
	d, reason := parsePermissionDecision(t, []byte(out))
	if d != "deny" {
		t.Fatalf("dangerous command: permissionDecision = %q, want deny (stdout=%q)", d, out)
	}
	if !strings.Contains(reason, "destructive recursive delete") || !strings.Contains(reason, "test-policy") {
		t.Errorf("reason = %q, want the policy reason + id", reason)
	}
	if strings.Contains(out, "rm -rf") {
		t.Errorf("stdout leaked the shell command (INV-2): %q", out)
	}

	// Benign command → nothing to stdout (governance only tightens).
	benign := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	if out := run(benign); strings.TrimSpace(out) != "" {
		t.Errorf("benign command must not write a decision; stdout=%q", out)
	}

	// The durable enforcement audit records both decisions, content-free.
	data, err := os.ReadFile(enfFile)
	if err != nil {
		t.Fatalf("enforcement sink not written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 enforcement records, got %d: %q", len(lines), data)
	}
	if strings.Contains(string(data), "rm -rf") || strings.Contains(string(data), "echo hi") {
		t.Errorf("enforcement record leaked the shell command (INV-2): %q", data)
	}
	var first hookflow.EnforcementRecord
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("enforcement record is not valid JSON: %v", err)
	}
	if first.Verdict != string(client.VerdictBlock) || first.AppliedDecision != "deny" || !first.WouldBlock {
		t.Errorf("first record = %+v, want BLOCK/deny/would_block", first)
	}
	if first.PolicyID != "test-policy" || first.ToolKind != string(client.ToolShell) {
		t.Errorf("first record missing policy/tool metadata: %+v", first)
	}
}

// ── E6-S3: fail-open / fail-closed failure policy ────────────────────────────

// TestApplyFailurePolicy guards AC-2/AC-3/AC-4: the transform synthesizes a HALT
// ONLY on a fail-open decision under fail-closed; every other case is a no-op
// (fail-open proceeds; a real verdict is never overridden under either policy).
func TestApplyFailurePolicy(t *testing.T) {
	failOpenDec := decision.Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: "sidecar unavailable"},
		FailOpen:   true,
		Source:     "fail-open:no-bundle",
	}

	t.Run("fail-open policy is a no-op even on an outage", func(t *testing.T) {
		got := hookflow.ApplyFailurePolicy(failOpenDec, hookflow.FailOpen)
		if got.Evaluation.Verdict != client.VerdictUnknown || got.Evaluation.WouldBlock() {
			t.Errorf("fail-open must leave the outage as UNKNOWN/proceed, got %+v", got.Evaluation)
		}
	})

	t.Run("fail-closed synthesizes HALT on an outage", func(t *testing.T) {
		got := hookflow.ApplyFailurePolicy(failOpenDec, hookflow.FailClosed)
		if got.Evaluation.Verdict != client.VerdictHalt || !got.Evaluation.WouldBlock() {
			t.Errorf("fail-closed must synthesize a blocking HALT, got %+v", got.Evaluation)
		}
		if !strings.Contains(got.Evaluation.Reason, "fail-closed") {
			t.Errorf("reason should mark fail-closed, got %q", got.Evaluation.Reason)
		}
		if !strings.Contains(got.Evaluation.Reason, "sidecar unavailable") {
			t.Errorf("reason should carry the content-free cause, got %q", got.Evaluation.Reason)
		}
		// The audit signature of a fail-closed deny: a HALT that is ALSO hookflow.FailOpen
		// (a real HALT never carries hookflow.FailOpen==true).
		if !got.FailOpen {
			t.Error("fail-closed synthetic HALT must retain hookflow.FailOpen==true for the audit")
		}
	})

	// A REAL verdict from a reachable sidecar is never overridden — the policy
	// governs the evaluation-unavailable case only.
	for _, v := range []client.Verdict{client.VerdictAllow, client.VerdictConstrain, client.VerdictBlock} {
		t.Run("real "+string(v)+" untouched under fail-closed", func(t *testing.T) {
			real := decision.Decision{Evaluation: client.Evaluation{Verdict: v}, FailOpen: false}
			if got := hookflow.ApplyFailurePolicy(real, hookflow.FailClosed); got.Evaluation.Verdict != v {
				t.Errorf("real %s verdict must pass through fail-closed unchanged, got %q", v, got.Evaluation.Verdict)
			}
		})
	}
}

// TestLogEnforceDecision_PolicyLegible makes a fail-closed deny legible in the
// stderr diagnostic: a synthesized HALT (would_block=true) that is ALSO fail_open
// would look contradictory without the policy label.
func TestLogEnforceDecision_PolicyLegible(t *testing.T) {
	if hookflow.FailOpen.String() != "fail_open" || hookflow.FailClosed.String() != "fail_closed" {
		t.Fatalf("policy String() = %q/%q", hookflow.FailOpen, hookflow.FailClosed)
	}
	var buf bytes.Buffer
	dec := hookflow.ApplyFailurePolicy(decision.Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: "sidecar unavailable"},
		FailOpen:   true,
	}, hookflow.FailClosed)
	hookflow.LogEnforceDecision(log.New(&buf, "", 0), "Bash", dec, hookflow.FailClosed)
	out := buf.String()
	if !strings.Contains(out, "policy=fail_closed") || !strings.Contains(out, "fail_open=true") {
		t.Errorf("diagnostic should mark the fail-closed policy + fail_open cause; got %q", out)
	}
}

// TestRunHook_EnforceFailClosed is the AC-4 end-to-end guard: with enforce ON and
// fail_closed ON, an UNREACHABLE sidecar → a `deny` on stdout (a fail-closed deny),
// while a reachable sidecar's real ALLOW still PROCEEDS (the policy governs outages
// only). AC-7: enforce-OFF is byte-identical even under fail_closed=1. INV-2: the
// fail-closed reason carries no tool content.
func TestRunHook_EnforceFailClosed(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))

	payload := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	run := func() string {
		var stdout bytes.Buffer
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, log.New(&bytes.Buffer{}, "", 0))
		return stdout.String()
	}

	// enforce ON + fail_closed ON + NO bundle loaded (cold-start fail-open) → deny.
	t.Setenv(envEnforce, "1")
	t.Setenv(envFailClosed, "1")
	out := run()
	d, reason := parsePermissionDecision(t, []byte(out))
	if d != ccDecisionDeny {
		t.Fatalf("fail-closed + outage: permissionDecision = %q, want deny (stdout=%q)", d, out)
	}
	if !strings.Contains(reason, "fail-closed") {
		t.Errorf("fail-closed deny reason = %q, want it to explain the fail-closed outage", reason)
	}
	if strings.Contains(out, "echo hi") {
		t.Errorf("fail-closed reason leaked the shell command (INV-2): %q", out)
	}

	// A reachable /evaluate answering ALLOW → a real verdict → PROCEED even under
	// fail-closed (the policy does not touch a real allow verdict). The verdict
	// has to come from the server since ADR-0017; a local allow with nothing
	// reachable is the outage case asserted just above.
	serveVerdict(t, `{"verdict":"allow"}`)
	allowURL, _ := serveEvaluate(t, `{"verdict":"allow"}`, 200, 0)
	tier2Creds(t, allowURL)
	if out := run(); strings.TrimSpace(out) != "" {
		t.Errorf("fail-closed must NOT block a real allow; stdout=%q", out)
	}

	// enforce OFF (still fail_closed=1) → byte-identical to observe: nothing to stdout.
	t.Setenv(envEnforce, "0")
	if out := run(); strings.TrimSpace(out) != "" {
		t.Errorf("enforce-off must not deny even under fail_closed=1; stdout=%q", out)
	}
}

// TestRecordEnforcement_GuardrailCategoryOnly guards the durable-audit half of AC-6
// / INV-2 (G3 LOW-2, G_SEC LOW-1): a guardrail-failure decision records the CATEGORY
// type only — never the guardrail reason free text (which can describe detected
// content) or the field name.
func TestRecordEnforcement_GuardrailCategoryOnly(t *testing.T) {
	enfFile := filepath.Join(t.TempDir(), "enforcements.jsonl")
	t.Setenv(envEnforcementFile, enfFile)
	logger := log.New(&bytes.Buffer{}, "", 0)

	dec := decision.Decision{Source: "local-bundle", Evaluation: client.Evaluation{
		Verdict: client.VerdictAllow, // verdict alone would proceed…
		Guardrail: &client.GuardrailResult{Passed: false, Reasons: []client.GuardrailReason{
			{Type: "pii", Field: "ssn", Reason: "detected 123-45-6789 in the argument"},
		}},
	}}
	// …but a failed guardrail denies (mapVerdict) and the record captures it.
	res := hookflow.ApplyDecision(&bytes.Buffer{}, dec, false, nil, contract)
	applied := res.Decision
	if applied != ccDecisionDeny {
		t.Fatalf("guardrail failure should deny, got %q", applied)
	}
	ev := &HookEvent{SessionID: "s", ToolName: "Write", ToolInput: []byte(`{"file_path":"/x"}`)}
	recordEnforcement(logger, ev, dec, res)

	data, err := os.ReadFile(enfFile)
	if err != nil {
		t.Fatalf("enforcement sink not written: %v", err)
	}
	if !strings.Contains(string(data), "pii") {
		t.Errorf("record should carry the guardrail category; got %q", data)
	}
	for _, leak := range []string{"123-45-6789", "detected", "ssn"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("record leaked guardrail free text/field %q (INV-2): %q", leak, data)
		}
	}
	var rec hookflow.EnforcementRecord
	_ = json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec)
	if len(rec.GuardrailCategories) != 1 || rec.GuardrailCategories[0] != "pii" {
		t.Errorf("guardrail_categories = %v, want [pii]", rec.GuardrailCategories)
	}
}

// ── E6-S6: REQUIRE_APPROVAL → CC ask (interactive local HITL prompt) ──────────

// TestApprovalReason guards AC-1 / INV-2: the ask reason surfaces the content-free
// approval CONTEXT — the policy reason, the policy id, and the server approval id
// (the one approval-specific evaluate field) — with a generic fallback when the
// policy carried no reason, and never any tool-content free text.
func TestApprovalReason(t *testing.T) {
	t.Run("surfaces reason, policy id, and approval id", func(t *testing.T) {
		r := hookflow.ApprovalReason(client.Evaluation{
			Verdict:    client.VerdictRequireApproval,
			Reason:     "production database migration",
			PolicyID:   "pol-db-1",
			ApprovalID: "appr-77",
		})
		for _, want := range []string{"production database migration", "pol-db-1", "appr-77"} {
			if !strings.Contains(r, want) {
				t.Errorf("approval reason %q missing %q", r, want)
			}
		}
	})

	t.Run("falls back to a generic approval message with no policy reason", func(t *testing.T) {
		r := hookflow.ApprovalReason(client.Evaluation{Verdict: client.VerdictRequireApproval})
		if !strings.Contains(strings.ToLower(r), "approval") {
			t.Errorf("empty-reason approval must still say it requires approval, got %q", r)
		}
		// No approval/policy id → no dangling "(approval: )" / "(policy: )" fragments.
		if strings.Contains(r, "(approval:") || strings.Contains(r, "(policy:") {
			t.Errorf("no ids present, but reason has an id fragment: %q", r)
		}
	})

	t.Run("omits the approval fragment when core sends no approval id", func(t *testing.T) {
		r := hookflow.ApprovalReason(client.Evaluation{Verdict: client.VerdictRequireApproval, Reason: "review needed", PolicyID: "p"})
		if strings.Contains(r, "(approval:") {
			t.Errorf("no approval id, but reason carries an approval fragment: %q", r)
		}
	})
}

// TestRecordEnforcement_ApprovalID guards the audit half of AC-1/AC-2: an ask
// decision carrying a server approval id records approval_ref (a correlation id, not
// content) so the ask is tie-able to the governance approval — and the audit stays
// content-free (INV-2). The approval id is injected on the Decision directly,
// because a LOCAL rule bundle does not mint approval ids (they are server-minted by
// core); the adapter surfaces one only when the decision carries it (omitempty).
func TestRecordEnforcement_ApprovalID(t *testing.T) {
	enfFile := filepath.Join(t.TempDir(), "enforcements.jsonl")
	t.Setenv(envEnforcementFile, enfFile)
	logger := log.New(&bytes.Buffer{}, "", 0)

	dec := decision.Decision{Source: "local-bundle", Evaluation: client.Evaluation{
		Verdict:    client.VerdictRequireApproval,
		Reason:     "external repository mutation",
		PolicyID:   "mcp-policy",
		ApprovalID: "appr-77",
	}}
	var out bytes.Buffer
	res := hookflow.ApplyDecision(&out, dec, false, nil, contract)
	applied := res.Decision
	if applied != ccDecisionAsk {
		t.Fatalf("require_approval should ask, got %q", applied)
	}
	// The stdout ask reason surfaces the approval + policy ids.
	if _, reason := parsePermissionDecision(t, out.Bytes()); !strings.Contains(reason, "appr-77") || !strings.Contains(reason, "mcp-policy") {
		t.Errorf("ask reason should carry the approval + policy ids, got %q", reason)
	}

	ev := &HookEvent{SessionID: "s", ToolName: "mcp__github__create_issue", ToolInput: []byte(`{"title":"secret-project-x"}`)}
	recordEnforcement(logger, ev, dec, res)

	data, err := os.ReadFile(enfFile)
	if err != nil {
		t.Fatalf("enforcement sink not written: %v", err)
	}
	if strings.Contains(string(data), "secret-project-x") {
		t.Errorf("enforcement record leaked tool content (INV-2): %q", data)
	}
	var rec hookflow.EnforcementRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("enforcement record is not valid JSON: %v", err)
	}
	if rec.AppliedDecision != ccDecisionAsk || rec.Verdict != string(client.VerdictRequireApproval) {
		t.Errorf("record = %+v, want REQUIRE_APPROVAL/ask", rec)
	}
	if rec.ApprovalRef != "appr-77" {
		t.Errorf("record approval_ref = %q, want appr-77 (correlates the ask to the governance approval)", rec.ApprovalRef)
	}
}

// TestApprovalRefFallsBackToGovernanceEventID guards E9 §1.3 defect 1: core
// declares approval_id but never assigns it, so a reference built from that
// field alone was always empty and the code quoting it was dead. The reference
// must fall back to governance_event_id — a REQUIRED field of core's verdict
// response — so the prompt and the audit carry something an approver can
// actually resolve.
func TestApprovalRefFallsBackToGovernanceEventID(t *testing.T) {
	eval := client.Evaluation{
		Verdict:           client.VerdictRequireApproval,
		GovernanceEventID: "ge-42",
	}
	if got := eval.ApprovalRef(); got != "ge-42" {
		t.Errorf("ApprovalRef with no approval_id = %q, want the governance event id", got)
	}
	if r := hookflow.ApprovalReason(eval); !strings.Contains(r, "ge-42") {
		t.Errorf("approval reason %q must quote the resolvable reference", r)
	}
	// approval_id still wins if core ever starts minting one.
	eval.ApprovalID = "appr-1"
	if got := eval.ApprovalRef(); got != "appr-1" {
		t.Errorf("ApprovalRef = %q, want the approval id to take precedence", got)
	}
}

// TestRunHook_EnforceApply_Approval is the E6-S6 end-to-end guard: enforce ON + a
// live sidecar whose bundle requires approval for an MCP tool → a PreToolUse hook
// writes an `ask` permissionDecision to stdout with a content-free reason (policy
// reason + id), the durable audit records the ask, and enforce-OFF is byte-identical
// (no ask even for the approval-required tool). The local bundle mints no approval
// id, so the reason/audit gracefully omit it (see TestRecordEnforcement_ApprovalID
// for the approval-id path).
func TestRunHook_EnforceApply_Approval(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	enfFile := filepath.Join(t.TempDir(), "enforcements.jsonl")
	t.Setenv(envEnforcementFile, enfFile)
	// A short hold: this case is about the verdict and the audit, not about how
	// long the gate waits for an approver.
	t.Setenv(devconfig.EnvApprovalHold, "200")
	serveVerdict(t, `{"verdict":"require_approval","reason":"external repository mutation","policy_id":"mcp-policy"}`)

	payload := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"mcp__github__create_issue","tool_input":{"title":"secret-project-x plans"}}`
	run := func() string {
		var stdout bytes.Buffer
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, log.New(&bytes.Buffer{}, "", 0))
		return stdout.String()
	}

	// enforce ON → the request is FILED, held for, and — with no approver in this
	// test — denied when the hold runs out (OD-E9-1), with the policy reason + id
	// surfaced content-free.
	//
	// It used to render as `ask`, the provider's own prompt. That was the
	// LOCALLY-derived approval: nothing had been filed, so there was nothing to
	// wait on and the only sensible move was to let the developer decide. A
	// server REQUIRE_APPROVAL is a filed record, so the gate holds for a real
	// answer instead — and an unanswered request denies rather than asking the
	// developer to approve their own.
	t.Setenv(envEnforce, "1")
	out := run()
	d, reason := parsePermissionDecision(t, []byte(out))
	if d != ccDecisionDeny {
		t.Fatalf("approval-required tool: permissionDecision = %q, want deny after an "+
			"unanswered hold (stdout=%q)", d, out)
	}
	if !strings.Contains(reason, "mcp-policy") {
		t.Errorf("deny reason = %q, want it to name the deciding policy", reason)
	}
	if strings.Contains(out, "secret-project-x") {
		t.Errorf("stdout leaked the tool input (INV-2): %q", out)
	}

	// The durable audit records the ask, content-free.
	data, err := os.ReadFile(enfFile)
	if err != nil {
		t.Fatalf("enforcement sink not written: %v", err)
	}
	if strings.Contains(string(data), "secret-project-x") {
		t.Errorf("enforcement record leaked tool content (INV-2): %q", data)
	}
	var rec hookflow.EnforcementRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("enforcement record is not valid JSON: %v", err)
	}
	if rec.AppliedDecision != ccDecisionDeny || rec.Verdict != string(client.VerdictHalt) {
		t.Errorf("record = %+v, want an unanswered approval recorded as HALT/deny", rec)
	}

	// enforce OFF → byte-identical to observe: nothing at all for the approval tool.
	t.Setenv(envEnforce, "0")
	if out := run(); strings.TrimSpace(out) != "" {
		t.Errorf("enforce-off must write nothing; stdout=%q", out)
	}
}

// The evaluation context (OD-E9-7): a gated call must carry what it is asking
// to do, or neither the server nor an approver can decide about it — `kind=shell
// tool_name=Bash` tells them exactly nothing.
//
// The matching guarantee is that the OBSERVE copy of the same call never
// carries it (SL3-SEC-3), which is why the two are mapped separately.
func TestEscalationCarriesApprovalContext_ObserveNeverDoes(t *testing.T) {
	isolateConfig(t)
	m := testMapper()

	for _, tc := range []struct {
		name, tool, input, want string
	}{
		{"shell carries the command", "Bash", `{"command":"rm -rf /tmp/x"}`, "rm -rf /tmp/x"},
		{"mcp carries the arguments", "mcp__github__create_issue", `{"title":"ship it"}`, "ship it"},
		{"file carries the body", "Write", `{"file_path":"/tmp/a","content":"hello"}`, "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookEv := &HookEvent{SessionID: "s1", ToolName: tc.tool, ToolInput: []byte(tc.input)}

			escalated, ok := enforceTarget{id: Identity{DeveloperDID: testDID}, mapper: m, ev: hookEv}.DevEvent(nil)
			if !ok {
				t.Fatal("escalation event did not map")
			}
			if escalated.Content == nil || !strings.Contains(escalated.Content.ToolInput, tc.want) {
				t.Errorf("escalation lacks the approval context: %+v", escalated.Content)
			}

			// The observe copy of the very same call must stay metadata-only.
			observed, _ := m.Map(HookPreToolUse, hookEv)
			if observed.Content != nil {
				t.Errorf("observe copy carries content (SL3-SEC-3): %+v", observed.Content)
			}
		})
	}

	// Redaction runs before attachment (E8). Given a detection result, the
	// attached body is the REDACTED one — the same bytes the tool call itself is
	// rewritten to, never the original.
	fileEv := &HookEvent{
		SessionID: "s1", ToolName: "Write",
		ToolInput: []byte(`{"file_path":"/tmp/a","content":"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"}`),
	}
	red := &client.Content{FileText: "AWS_ACCESS_KEY_ID=OPENBOX_REDACTED"}
	ev, _ := enforceTarget{id: Identity{DeveloperDID: testDID}, mapper: m, ev: fileEv}.DevEvent(red)
	if ev.Content == nil {
		t.Fatal("a gated Write must carry its body for evaluation (ADR-0017 E7)")
	}
	if strings.Contains(ev.Content.ToolInput, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("the RAW body was attached — redaction must precede attachment (E8): %q", ev.Content.ToolInput)
	}
	if !strings.Contains(ev.Content.ToolInput, "OPENBOX_REDACTED") {
		t.Errorf("the redacted body was not attached: %q", ev.Content.ToolInput)
	}
	// The structural locator survives the rebuild verbatim.
	if !strings.Contains(ev.Content.ToolInput, "/tmp/a") {
		t.Errorf("file_path lost in the redacted rebuild: %q", ev.Content.ToolInput)
	}
}
