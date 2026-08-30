package claudecode

import (
	"encoding/json"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// The prompt gate (plan 260818-1714).
//
// In enforce mode UserPromptSubmit is a synchronous gate exactly like
// PreToolUse: the PromptSubmitted event is evaluated by /evaluate BEFORE the
// prompt is processed, and a HALT/BLOCK blocks (and erases) the prompt. The
// gate machinery is the shared hookflow.EnforceGate — same escalation, same
// failure policy, same approval hold, same spool-dedupe; what differs is only
// this target (how the prompt event reads) and promptOutputContract (how a
// refusal is spelled on this hook).

// promptToolKind labels prompt-gate decisions in the enforcement audit, where
// tool_kind otherwise carries shell/file/mcp.
const promptToolKind = "prompt"

// promptTarget adapts a UserPromptSubmit hook event to the shared enforce gate.
type promptTarget struct {
	id     Identity
	mapper Mapper
	ev     *HookEvent
}

func (t promptTarget) SessionID() string { return t.ev.SessionID }

// ToolName labels the gate's diagnostics and the pending-approval marker; a
// prompt is not a tool, so the label says what it is.
func (t promptTarget) ToolName() string { return promptToolKind }

// ToolInput: a prompt has no tool_input, so there is nothing the proceed-path
// rewrite could reconstruct (promptOutputContract declares no content fields
// either — the pair keeps updatedInput structurally impossible here).
func (t promptTarget) ToolInput() json.RawMessage { return nil }

func (t promptTarget) HighRisk() bool { return false }

// DecisionRequest carries only identity axes. The local step is a secret
// redactor for tool bodies; prompt text is attached (and capture-gated) by the
// Mapper on the event itself, so there is nothing here for the local step to
// scan — it reports no verdict and the gate escalates, as it does for every
// gated call (ADR-0017).
func (t promptTarget) DecisionRequest(bool) decision.DecisionRequest {
	return decision.DecisionRequest{
		SessionID:    t.ev.SessionID,
		DeveloperDID: t.id.DeveloperDID,
		EventType:    client.EventPromptSubmitted,
	}
}

// DevEvent maps the prompt for the inline evaluation through the SAME Mapper
// (and pinned clock) the observe copy uses, so the two derive one event_id and
// the gate's OnDelivered dedupe holds. Content posture is the mapper's:
// the prompt rides ev.Content.Prompt only under content_capture, and the
// client's stripContent drops it at the choke point when the org opted out —
// identical to the observe path, no second attach here.
func (t promptTarget) DevEvent(*client.Content) (client.DevEvent, bool) {
	return t.mapper.Map(HookUserPromptSubmit, t.ev)
}

var _ hookflow.EnforceTarget = promptTarget{}

// recordPromptEnforcement writes the durable audit line for a prompt-gate
// decision under its own tool kind.
func recordPromptEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	hookflow.RecordEnforcement(logger, e.SessionID, promptToolKind, dec, res)
}
