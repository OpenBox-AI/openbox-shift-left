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

// DevEvent maps the call for an inline evaluation, and — unlike the observe
// copy of the same call — attaches the approval context.
//
// This is the ONLY place a tool's own input is put on an outbound event, and it
// is why the split matters: the observe path spools its own separately-mapped
// copy that never carries one, so SL3-SEC-3 (commands and file bodies never
// egress on observe events) holds by construction rather than by care.
//
// An escalation is a different question from telemetry. The org has said "stop
// and ask me about this class of call"; it cannot answer without seeing which
// call. Content-gated all the same — the client's stripContent drops it when
// the org has content capture off (INV-2 at the choke point, not by adapter
// convention).
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
// command for a shell tool, the arguments for an MCP one.
//
// It is deliberately empty for every other class. Those cannot be escalated
// (isHighRiskClass), so no approval can exist for them, so attaching their
// input would egress content that nothing is going to read — and for a file
// write that input is the whole file body.
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
