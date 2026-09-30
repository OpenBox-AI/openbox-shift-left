package muse

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
// the server needs to judge it: a shell call's command verbatim, an MCP call's
// arguments verbatim (a policy deciding whether a call is dangerous has to see
// what will run), a file write's redacted body rebuilt into its tool_input and
// scanned whole, and everything else through the mapper's own redacted content.
//
// Map runs with CaptureContent forced on so the shape of this copy does not
// depend on the target's own flag. Whether the content then leaves the machine
// is decided once, at the client, which strips it when capture is off.
func (t enforceTarget) DevEvent(redacted *client.Content) (client.DevEvent, bool) {
	override := t.overrideContent(redacted)

	m := t.mapper
	m.CaptureContent = override == ""
	ev, ok := m.Map(HookPreToolUse, t.ev)
	if !ok {
		return ev, false
	}
	if override != "" {
		ev.Content = &client.Content{ToolInput: override}
		return ev, true
	}
	if ev.Content != nil {
		ev.Content.ToolInput = bounded(ev.Content.ToolInput)
	}
	return ev, true
}

// overrideContent returns the content for a class with a recorded carve-out, or
// "" for the redacted default. It reads only the hook event, never Map's output.
func (t enforceTarget) overrideContent(redacted *client.Content) string {
	kind, sem, _, _, _ := classifyTool(t.ev.ToolName)
	switch {
	case kind == client.ToolShell && isBuiltin(t.ev.ToolName):
		// Membership in builtinTools is part of the test: classifyTool's default
		// is shell/"internal", so the kind alone would put every unknown name on
		// the verbatim path by fallthrough.
		return bounded(t.ev.command())
	case kind == client.ToolMCP:
		return bounded(string(t.ev.ToolInput))
	case hookflow.IsFileSemantic(sem) && redacted != nil && redacted.FileText != "":
		// The rebuild swaps only the body field, so every other field is still
		// the original: scan the whole rebuilt object, the same pass the observe
		// copy runs over tool_input, so the two agree on what redaction covers.
		rebuilt := hookflow.RedactToolInput(t.ev.ToolInput, redacted.FileText, contentFieldKeys)
		if len(rebuilt) == 0 {
			return ""
		}
		return bounded(t.mapper.redact(string(rebuilt)))
	}
	return ""
}

func isBuiltin(name string) bool {
	_, ok := builtinTools[name]
	return ok
}

// bounded caps content at MaxRedactBody, the first of the two bounds every
// egressing body passes (the client's capBody is the second).
func bounded(s string) string {
	return hookflow.TruncateBytes(s, hookflow.MaxRedactBody)
}

// toolInputExtract is the raw extraction the observe path builds a tool_input
// body from: the shell command alone for a shell call, the whole tool_input
// otherwise. Its result is never used unredacted.
func toolInputExtract(e *HookEvent) string {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	if kind == client.ToolShell {
		return e.command()
	}
	return string(e.ToolInput)
}

var _ hookflow.EnforceTarget = enforceTarget{}
