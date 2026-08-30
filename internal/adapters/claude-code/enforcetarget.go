package claudecode

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

// DevEvent maps the call for the inline evaluation and attaches the content
// the server needs to judge it. Recorded, not silently fixed; changing it
// changes what policy can match on, which is an owner decision, not a cleanup.
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
func evaluationContext(e *HookEvent, redacted *client.Content) string {
	return hookflow.TruncateBytes(toolInputExtract(e, redacted), hookflow.MaxRedactBody)
}

func toolInputExtract(e *HookEvent, redacted *client.Content) string {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	input := e.ToolInput
	if redacted != nil && redacted.FileText != "" {
		if rebuilt := hookflow.RedactToolInput(input, redacted.FileText, contentFieldKeys); len(rebuilt) > 0 {
			input = rebuilt
		}
	}
	if kind == client.ToolShell {
		return commandOf(input, e)
	}
	return string(input)
}

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
