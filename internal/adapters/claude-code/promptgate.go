package claudecode

import (
	"encoding/json"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

const promptToolKind = "prompt"

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
// either; the pair keeps updatedInput structurally impossible here).
func (t promptTarget) ToolInput() json.RawMessage { return nil }

func (t promptTarget) HighRisk() bool { return false }

// DecisionRequest carries only identity axes.
func (t promptTarget) DecisionRequest(bool) decision.DecisionRequest {
	return decision.DecisionRequest{
		SessionID:    t.ev.SessionID,
		DeveloperDID: t.id.DeveloperDID,
		EventType:    client.EventPromptSubmitted,
	}
}

// DevEvent maps the prompt for the inline evaluation through the same Mapper
// (and pinned clock) the observe copy uses, so the two derive one event_id and
// the gate's OnDelivered dedupe holds.
func (t promptTarget) DevEvent(*client.Content) (client.DevEvent, bool) {
	return t.mapper.Map(HookUserPromptSubmit, t.ev)
}

var _ hookflow.EnforceTarget = promptTarget{}

func recordPromptEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	hookflow.RecordEnforcement(logger, e.SessionID, promptToolKind, dec, res)
}
