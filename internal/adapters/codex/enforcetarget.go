package codex

import (
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
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
// copy of the same call — attaches the content the server needs to judge it. The
// observe path spools its own separately-mapped copy that never carries one, so
// SL3-SEC-3 holds by construction. Content-gated at the client choke point. Same
// rationale as the Claude Code adapter's, which documents it at length.
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

// evaluationContext is what the server needs to decide about this call. Every
// gated class attaches now (ADR-0017), not only shell and MCP, and the bytes are
// the REDACTED ones: rebuilt through the same RedactToolInput the tool-call
// rewrite uses, from the same detection result, so the server judges exactly the
// text the call was rewritten to.
//
// Codex's redactable field IS "command" (apply_patch bodies arrive there), so
// unlike Claude Code the shell branch below really can be rewritten — which is
// why it reads the command back out of the rebuilt object rather than off the
// event.
func evaluationContext(e *HookEvent, redacted *client.Content) string {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	input := e.ToolInput
	if redacted != nil && redacted.FileText != "" {
		if rebuilt := hookflow.RedactToolInput(input, redacted.FileText, contentFieldKeys); len(rebuilt) > 0 {
			input = rebuilt
		}
	}
	if kind == client.ToolShell {
		var obj struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(input, &obj); err == nil && obj.Command != "" {
			return hookflow.CapCommand(obj.Command)
		}
		return hookflow.CapCommand(e.command())
	}
	return hookflow.CapCommand(string(input))
}

var _ hookflow.EnforceTarget = enforceTarget{}
