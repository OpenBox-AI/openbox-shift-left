package codex

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

// DevEvent maps the call for a Tier-2 escalation, and — unlike the observe copy
// of the same call — attaches the approval context, so an approver can see what
// they are deciding about. The observe path spools its own separately-mapped
// copy that never carries one, so SL3-SEC-3 holds by construction. Content-gated
// at the client choke point. Same rationale as the Claude Code adapter's, which
// documents it at length.
func (t enforceTarget) DevEvent() (client.DevEvent, bool) {
	ev, ok := t.mapper.Map(HookPreToolUse, t.ev)
	if !ok {
		return ev, false
	}
	if in := approvalContext(t.ev); in != "" {
		ev.Content = &client.Content{ToolInput: in}
	}
	return ev, true
}

// approvalContext is what an approver needs to decide about this call: the
// command for a shell tool, the arguments for an MCP one. Empty for every other
// class — they cannot be escalated, so no approval can exist for them.
func approvalContext(e *HookEvent) string {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	switch kind {
	case client.ToolShell:
		return hookflow.CapCommand(e.command())
	case client.ToolMCP:
		return hookflow.CapCommand(string(e.ToolInput))
	}
	return ""
}

var _ hookflow.EnforceTarget = enforceTarget{}
