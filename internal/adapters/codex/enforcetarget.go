package codex

import (
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

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

// DevEvent maps the call for the inline evaluation, and; unlike the observe
// copy of the same call; attaches the content the server needs to judge it.
// The observe path spools its own separately-mapped copy that never carries
// one, so the observe path stays content-free by construction.
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

func evaluationContext(e *HookEvent, redacted *client.Content) string {
	input := e.ToolInput
	if redacted != nil && redacted.FileText != "" {
		if rebuilt := hookflow.RedactToolInput(input, redacted.FileText, contentFieldKeys); len(rebuilt) > 0 {
			input = rebuilt
		}
	}
	// Egress, not the local decision request: bounded by MaxRedactBody, never
	// MaxCommandLen (enforce.go caps its own DecisionRequest separately).
	return hookflow.TruncateBytes(toolInputText(e, input), hookflow.MaxRedactBody)
}

// toolInputExtract is the raw extraction the observe path builds a tool_input
// body from. MaxCommandLen bounds a local decision request, never egress, so
// no command cap applies here. Never used unredacted.
func toolInputExtract(e *HookEvent) string { return toolInputText(e, e.ToolInput) }

// toolInputText is the shell command alone for a shell call (falling back to
// the event's own command when input carries none), the whole input otherwise.
func toolInputText(e *HookEvent, input json.RawMessage) string {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	if kind != client.ToolShell {
		return string(input)
	}
	var obj struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &obj); err == nil && obj.Command != "" {
		return obj.Command
	}
	return e.command()
}

var _ hookflow.EnforceTarget = enforceTarget{}
