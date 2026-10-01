package hookflow

import (
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// PromptToolKind labels a prompt in the gate's diagnostics, the pending-approval
// marker and the enforcement audit: a prompt is not a tool, so the label says
// what it is.
const PromptToolKind = "prompt"

// PromptTarget is the gate's view of a submitted prompt, shared by every
// adapter: Hook is the adapter's prompt-submit hook and Event its payload.
type PromptTarget[H ~string, E any, M Mapper[H, E]] struct {
	SessionIDValue string
	DeveloperDID   string
	Mapper         M
	Hook           H
	Event          *E
}

func (t PromptTarget[H, E, M]) SessionID() string { return t.SessionIDValue }

func (t PromptTarget[H, E, M]) ToolName() string { return PromptToolKind }

// ToolInput: a prompt has no tool_input, so there is nothing the proceed-path
// rewrite could reconstruct (the prompt contract declares no content fields
// either; the pair keeps updatedInput structurally impossible here).
func (t PromptTarget[H, E, M]) ToolInput() json.RawMessage { return nil }

func (t PromptTarget[H, E, M]) HighRisk() bool { return false }

// DecisionRequest carries only identity axes: there is no tool to classify and
// no command to bound, so the local decider has nothing to match on and the
// verdict comes from /evaluate.
func (t PromptTarget[H, E, M]) DecisionRequest(bool) decision.DecisionRequest {
	return decision.DecisionRequest{
		SessionID:    t.SessionIDValue,
		DeveloperDID: t.DeveloperDID,
		EventType:    client.EventPromptSubmitted,
	}
}

// DevEvent maps the prompt for the inline evaluation through the same Mapper
// (and pinned clock) the observe copy uses, so the two derive one event_id and
// the gate's own EscalationOutcome tracking holds. It inherits the Mapper's
// redactor, so the evaluated copy is scanned exactly like the spooled one.
func (t PromptTarget[H, E, M]) DevEvent(*client.Content) (client.DevEvent, bool) {
	return t.Mapper.Map(t.Hook, t.Event)
}
