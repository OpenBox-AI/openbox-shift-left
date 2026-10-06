package muse

import (
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// PermissionRequest fires when a tool call needs approval, and is the only
// lever on the Muse surface that can LOOSEN: `behavior:"allow"` skips the human
// approval prompt outright. OpenBox never emits allow here. It is also a
// record, never the gate: it does not fire when Muse's approval judge or
// --yolo settles the call itself, so PreToolUse is what governs the tool.
//
// permissionTarget is the gate's view of that event, evaluated like any other
// gated call; permissionContract (outputcontract.go) is deny-or-nothing.
type permissionTarget struct {
	id     Identity
	mapper Mapper
	ev     *HookEvent
}

func (t permissionTarget) SessionID() string { return t.ev.SessionID }
func (t permissionTarget) ToolName() string  { return t.ev.ToolName }

// ToolInput is nil: no rewrite lever exists on this surface, and nil paired
// with permissionContract's nil ContentFieldKeys makes one structurally
// impossible.
func (t permissionTarget) ToolInput() json.RawMessage { return nil }

func (t permissionTarget) HighRisk() bool { return isHighRiskClass(t.ev.ToolName) }

// DecisionRequest reuses the PreToolUse builder so a tool is classified
// identically at the pre-execution gate and at the escalation.
func (t permissionTarget) DecisionRequest(localRedaction bool) decision.DecisionRequest {
	req := buildDecisionRequest(t.id, t.ev, localRedaction)
	req.EventType = client.EventPermissionRequest
	return req
}

func (t permissionTarget) DevEvent(*client.Content) (client.DevEvent, bool) {
	return t.mapper.Map(HookPermissionRequest, t.ev)
}

var _ hookflow.EnforceTarget = permissionTarget{}
