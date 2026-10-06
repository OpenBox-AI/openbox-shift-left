package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

const (
	toolCmdMarker = "echo tool-input-marker"
	toolOutMarker = "tool-output-marker"
)

func toolPre(name, input string) *HookEvent {
	return &HookEvent{SessionID: "th-tc", ToolName: name, ToolUseID: "c1", ToolInput: json.RawMessage(input)}
}

func toolPost(name, response string) *HookEvent {
	return &HookEvent{SessionID: "th-tc", ToolName: name, ToolUseID: "c1", ToolResponse: json.RawMessage(response)}
}

func TestToolContent_CaptureOffAttachesNothing(t *testing.T) {
	m := testMapper()
	pre, _ := m.Map(HookPreToolUse, toolPre("Bash", `{"command":"`+toolCmdMarker+`"}`))
	post, _ := m.Map(HookPostToolUse, toolPost("Bash", `{"output":"`+toolOutMarker+`"}`))
	for _, ev := range []client.DevEvent{pre, post} {
		if ev.Content != nil {
			t.Fatalf("capture off must attach no content, got %+v", ev.Content)
		}
	}
}

func TestToolContent_CaptureOnToolInput(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true

	shell, _ := m.Map(HookPreToolUse, toolPre("Bash", `{"command":"`+toolCmdMarker+`"}`))
	if shell.Content == nil || shell.Content.ToolInput != toolCmdMarker {
		t.Fatalf("shell call must carry its command, got %+v", shell.Content)
	}

	patch, _ := m.Map(HookPreToolUse, toolPre("apply_patch", `{"command":"*** Begin Patch"}`))
	if patch.Content == nil || !strings.Contains(patch.Content.ToolInput, "Begin Patch") {
		t.Fatalf("non-shell call must carry its input, got %+v", patch.Content)
	}

	mcp, _ := m.Map(HookPreToolUse, toolPre("mcp__srv__fn", `{"q":"x"}`))
	if mcp.Content == nil || mcp.Content.ToolInput != `{"q":"x"}` {
		t.Fatalf("mcp call must carry the raw input, got %+v", mcp.Content)
	}

	empty, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-tc", ToolName: "Bash"})
	if empty.Content != nil {
		t.Fatalf("no input must leave Content nil, got %+v", empty.Content)
	}
}

func TestToolContent_CaptureOnToolOutput(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true
	for name, resp := range map[string]string{
		"object output": `{"output":"` + toolOutMarker + `"}`,
		"object stdout": `{"stdout":"` + toolOutMarker + `"}`,
		"bare string":   `"` + toolOutMarker + `"`,
	} {
		ev, _ := m.Map(HookPostToolUse, toolPost("Bash", resp))
		if ev.Content == nil || ev.Content.ToolOutput != toolOutMarker {
			t.Errorf("%s: got %+v", name, ev.Content)
		}
	}

	raw, _ := m.Map(HookPostToolUse, toolPost("Bash", `{"exit":0}`))
	if raw.Content == nil || raw.Content.ToolOutput != `{"exit":0}` {
		t.Errorf("unknown shape must fall back to the raw JSON, got %+v", raw.Content)
	}
	for _, resp := range []string{``, `null`, `""`} {
		ev, _ := m.Map(HookPostToolUse, toolPost("Bash", resp))
		if ev.Content != nil {
			t.Errorf("response %q must leave Content nil, got %+v", resp, ev.Content)
		}
	}
	if ev, _ := m.Map(HookPostToolUse, toolPost("Bash", `{"output":"x"}`)); ev.Status != "" {
		t.Errorf("status must stay unreported, got %q", ev.Status)
	}
}

func TestToolContent_RedactedBeforeAttach(t *testing.T) {
	secret := awsSecretFixture()
	m := testMapper()
	m.CaptureContent = true
	m.RedactContent = func(s string) string { return strings.ReplaceAll(s, secret, "${OPENBOX_REDACTED_X}") }

	pre, _ := m.Map(HookPreToolUse, toolPre("Bash", `{"command":"deploy `+secret+` now"}`))
	post, _ := m.Map(HookPostToolUse, toolPost("Bash", `{"output":"key `+secret+` ok"}`))
	for _, ev := range []client.DevEvent{pre, post} {
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret reached the event: %s", raw)
		}
		if !strings.Contains(string(raw), "OPENBOX_REDACTED") {
			t.Fatalf("no placeholder; content never scanned: %s", raw)
		}
	}
}

// The fixtures are the vendor payload shape; mapping them end to end proves
// the field names, not just the helper.
func TestToolContent_FixturePayloadsMap(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true
	pre := mustParseFixture(t, "pretooluse.json")
	post := mustParseFixture(t, "posttooluse-string.json") // the live shape
	tolerated := mustParseFixture(t, "posttooluse.json")   // object form, tolerated
	a, _ := m.Map(HookPreToolUse, pre)
	b, _ := m.Map(HookPostToolUse, post)
	if a.Content == nil || a.Content.ToolInput != "go test ./..." {
		t.Errorf("fixture tool_input: %+v", a.Content)
	}
	if b.Content == nil || b.Content.ToolOutput != "hello-live\n" {
		t.Errorf("fixture tool_response: %+v", b.Content)
	}
	c, _ := m.Map(HookPostToolUse, tolerated)
	if c.Content == nil || !strings.HasPrefix(c.Content.ToolOutput, "ok ") {
		t.Errorf("object-shaped tool_response: %+v", c.Content)
	}
}

// The shape Codex 0.156.1 actually delivers: tool_response is a bare string.
func TestToolContent_LiveShapedStringFixture(t *testing.T) {
	ev := mustParseFixture(t, "posttooluse-string.json")
	if got := outputText(ev.ToolResponse); got != "hello-live\n" {
		t.Fatalf("outputText = %q", got)
	}
	m := testMapper()
	m.CaptureContent = true
	out, _ := m.Map(HookPostToolUse, ev)
	if out.Content == nil || out.Content.ToolOutput != "hello-live\n" {
		t.Fatalf("mapped content = %+v", out.Content)
	}
}

func mustParseFixture(t *testing.T, name string) *HookEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	ev, err := ParseHookEvent(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return ev
}

// The spooled wire rows carry the content under capture on and none under off.
func TestToolContent_SpoolFollowsCaptureGate(t *testing.T) {
	post := `{"hook_event_name":"PostToolUse","session_id":"th-sp","cwd":"/r","model":"m",` +
		`"permission_mode":"default","turn_id":"t1","tool_name":"Bash","tool_use_id":"c1",` +
		`"tool_input":{"command":"ls"},"tool_response":{"output":"` + toolOutMarker + `"},"transcript_path":null}`

	on := setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "1")
	runHook(t, "PostToolUse", post)
	if body := spooledBody(t, on); !strings.Contains(body, toolOutMarker) {
		t.Errorf("capture on: tool output missing from the spool:\n%s", body)
	}

	off := setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "0")
	runHook(t, "PostToolUse", post)
	if body := spooledBody(t, off); strings.Contains(body, toolOutMarker) {
		t.Errorf("capture off: tool output leaked into the spool:\n%s", body)
	}
}

// MaxCommandLen bounds the local decision request, never egress: the gate's
// egress copy of a long command must survive past it, bounded only by the
// redaction body cap.
func TestToolContent_GateEgressCopyIsNotCommandCapped(t *testing.T) {
	long := strings.Repeat("a", hookflow.MaxCommandLen*3)
	ev := toolPre("Bash", `{"command":"`+long+`"}`)
	m := testMapper()
	m.CaptureContent = true
	got, ok := enforceTarget{id: m.Identity, mapper: m, ev: ev}.DevEvent(nil)
	if !ok || got.Content == nil {
		t.Fatalf("no gate content: ok=%v %+v", ok, got.Content)
	}
	if len(got.Content.ToolInput) != len(long) {
		t.Fatalf("gate egress copy = %d bytes, want %d (only MaxRedactBody bounds egress)", len(got.Content.ToolInput), len(long))
	}

	huge := strings.Repeat("a", hookflow.MaxRedactBody+10)
	got, _ = enforceTarget{id: m.Identity, mapper: m, ev: toolPre("Bash", `{"command":"`+huge+`"}`)}.DevEvent(nil)
	if got.Content == nil || len(got.Content.ToolInput) > hookflow.MaxRedactBody {
		t.Fatalf("gate egress copy must stay within MaxRedactBody")
	}
}

// A JSON string is the tool's output verbatim, even when it happens to parse
// as an object.
func TestToolContent_StringResponseIsNeverUnwrapped(t *testing.T) {
	raw := `{"output":"inner"}`
	quoted, _ := json.Marshal(raw)
	if got := outputText(quoted); got != raw {
		t.Fatalf("outputText = %q, want the printed text %q", got, raw)
	}
}
