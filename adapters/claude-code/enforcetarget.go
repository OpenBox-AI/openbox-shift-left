package claudecode

import (
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// enforceTarget adapts a native hook event to the shared enforce gate. Reading
// the provider's own event shape is the only provider-specific part of gating
// a tool call; the order of the gate's steps is shared (hookflow.EnforceGate).
type enforceTarget struct {
	id     Identity
	mapper Mapper
	ev     *HookEvent
}

func (t enforceTarget) SessionID() string          { return t.ev.SessionID }
func (t enforceTarget) ToolName() string           { return t.ev.ToolName }
func (t enforceTarget) ToolInput() json.RawMessage { return t.ev.ToolInput }
func (t enforceTarget) HighRisk() bool             { return isHighRiskClass(t.ev.ToolName) }

func (t enforceTarget) DecisionRequest(localRedaction bool) decision.DecisionRequest {
	return buildDecisionRequest(t.id, t.ev, localRedaction)
}

// DevEvent maps the call for the inline evaluation, and — unlike the observe
// copy of the same call — attaches the content the server needs to judge it.
//
// This is the ONLY place a tool's own input is put on an outbound event, and it
// is why the split matters: the observe path spools its own separately-mapped
// copy that never carries one, so SL3-SEC-3 (commands and file bodies never
// egress on observe events) holds by construction rather than by care.
//
// Evaluation is a different question from telemetry. The org has asked OpenBox
// to decide about this call; it cannot decide on content it cannot see.
// Content-gated all the same — the client's stripContent drops it when the org
// has content capture off (INV-2 at the choke point, not by adapter
// convention).
func (t enforceTarget) DevEvent(redacted *client.Content) (client.DevEvent, bool) {
	ev, ok := t.mapper.Map(HookPreToolUse, t.ev)
	if !ok {
		return ev, false
	}
	if in := evaluationContext(t.ev, redacted); in != "" {
		ev.Content = &client.Content{ToolInput: in}
	}
	return ev, true
}

// evaluationContext is what the server needs to decide about this call: the
// command for a shell tool, the arguments for an MCP one, the file body for a
// write.
//
// It covered only shell and MCP while those were the only escalated classes.
// ADR-0017 evaluates every gated class, so a Write's content is attached now —
// which is a real change in what leaves the machine, disclosed in the ADR and
// gated on content capture.
//
// **Redaction happens first, and this returns the redacted bytes.** The body is
// rebuilt through the same RedactToolInput the tool-call rewrite uses, from the
// same detection result, so the server judges exactly the text the developer's
// tool was rewritten to — not the original, and not a second redaction that
// could differ from it. When nothing was scanned or nothing matched, redacted is
// nil and the original stands.
func evaluationContext(e *HookEvent, redacted *client.Content) string {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	input := e.ToolInput
	if redacted != nil && redacted.FileText != "" {
		if rebuilt := hookflow.RedactToolInput(input, redacted.FileText, contentFieldKeys); len(rebuilt) > 0 {
			input = rebuilt
		}
	}
	if kind == client.ToolShell {
		// The command alone, not the enclosing object: it is the field an
		// approver and every existing dashboard read. Claude Code declares no
		// redactable shell field (contentFieldKeys is content/new_string), so
		// input is unchanged here and reading from the event is equivalent —
		// stated rather than assumed, because adding "command" to those keys
		// would silently make it wrong.
		return hookflow.CapCommand(commandOf(input, e))
	}
	return hookflow.CapCommand(string(input))
}

// commandOf reads the shell command from a possibly-rebuilt tool_input, falling
// back to the event's own accessor when the rebuild did not happen.
func commandOf(input json.RawMessage, e *HookEvent) string {
	if len(input) == 0 || jsonEqualRaw(input, e.ToolInput) {
		return e.command()
	}
	var obj struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &obj); err != nil {
		return e.command()
	}
	return obj.Command
}

func jsonEqualRaw(a, b json.RawMessage) bool { return string(a) == string(b) }

var _ hookflow.EnforceTarget = enforceTarget{}
