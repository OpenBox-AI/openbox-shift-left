package codex

import (
	"encoding/json"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// PermissionRequest fires when a tool call needs escalated permission, between
// PreToolUse and PostToolUse (ordering verified on codex-cli 0.150.0-alpha.8).
// It is the only lever in the whole Codex surface that can LOOSEN:
// `behavior:"allow"` skips the human approval prompt outright.
//
// OpenBox therefore never emits allow here -- not bundled with a rewrite, not
// ever. The adapter's existing rule for PreToolUse is "allow rides only a
// redacting rewrite, never a grant"; on this surface there is no rewrite to
// ride, because updatedInput is reserved and fails closed, so the rule collapses
// to "never at all". A governance product that emitted it would be an approval
// bypass. That is the reason for permissionOutputContract's shape, and
// TestPermissionGate_NeverLoosens is its enforcement.
type permissionTarget struct {
	id     Identity
	mapper Mapper
	ev     *HookEvent
}

func (t permissionTarget) SessionID() string { return t.ev.SessionID }
func (t permissionTarget) ToolName() string  { return t.ev.ToolName }

// ToolInput: Codex's updatedInput on this surface is reserved and documented to
// fail closed, so there is no rewrite target. Returning nil pairs with
// ContentFieldKeys() nil to make a rewrite structurally impossible.
func (t permissionTarget) ToolInput() json.RawMessage { return nil }

func (t permissionTarget) HighRisk() bool { return isHighRiskClass(t.ev.ToolName) }

// DecisionRequest reuses the PreToolUse request builder so a tool is classified
// identically whether it is seen at the pre-execution gate or at the escalation.
func (t permissionTarget) DecisionRequest(localRedaction bool) decision.DecisionRequest {
	req := buildDecisionRequest(t.id, t.ev, localRedaction)
	req.EventType = client.EventPermissionRequest
	return req
}

func (t permissionTarget) DevEvent(redacted *client.Content) (client.DevEvent, bool) {
	ev, ok := t.mapper.Map(HookPermissionRequest, t.ev)
	if !ok {
		return ev, false
	}
	if in := evaluationContext(t.ev, redacted); in != "" {
		ev.Content = &client.Content{ToolInput: in}
	}
	return ev, true
}

var _ hookflow.EnforceTarget = permissionTarget{}

// permissionRequestOutput is the PermissionRequest stdout shape. Codex rejects
// `continue`, `stopReason`, `suppressOutput`, `updatedInput`, `updatedPermissions`
// and `interrupt:true` on this surface, so none of them has a field here at all:
// a key that cannot be spelled cannot be emitted by a later edit.
type permissionRequestOutput struct {
	HookSpecificOutput permissionHookSpecificOutput `json:"hookSpecificOutput"`
}

type permissionHookSpecificOutput struct {
	HookEventName string              `json:"hookEventName"`
	Decision      permissionDecisionW `json:"decision"`
}

type permissionDecisionW struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message,omitempty"`
}

type permissionOutputContract struct{}

// ApprovalDecision: anything that would ask denies instead. This surface IS the
// approval prompt, so "ask" would mean handing the decision back to the human
// the policy already answered for.
func (permissionOutputContract) ApprovalDecision() string { return codexDecisionDeny }

// ContentFieldKeys is nil: the rewrite lever here is reserved and fails closed.
func (permissionOutputContract) ContentFieldKeys() []string { return nil }

// Render emits a deny, or nothing. There is deliberately no branch that can
// produce any other behavior: HALT folds into the per-call deny (only the prompt
// contract latches), and a proceed writes nothing so the human's own approval
// prompt still happens.
func (permissionOutputContract) Render(dec, reason string, _ json.RawMessage) ([]byte, string) {
	switch dec {
	case codexDecisionDeny, hookflow.DecisionHalt:
	default:
		return nil, "" // proceed → write nothing → the human is still asked
	}
	if reason == "" {
		// Codex rejects a denial with no message, and a rejected output fails
		// open. A generic message that is actually delivered governs; an empty
		// one that is discarded does not.
		reason = "denied by OpenBox policy"
	}
	line, err := json.Marshal(permissionRequestOutput{
		HookSpecificOutput: permissionHookSpecificOutput{
			HookEventName: string(HookPermissionRequest),
			Decision:      permissionDecisionW{Behavior: codexDecisionDeny, Message: reason},
		},
	})
	if err != nil {
		return nil, "" // fail-open: never wedge an approval on a marshal fault
	}
	return line, codexDecisionDeny
}

var _ hookflow.OutputContract = permissionOutputContract{}

var permissionContract = permissionOutputContract{}

func recordPermissionEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	hookflow.RecordEnforcement(logger, e.SessionID, string(kind), dec, res)
}
