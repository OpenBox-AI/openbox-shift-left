package muse

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

// ToolInput: a prompt has no tool_input, so nothing could be rewritten.
func (t promptTarget) ToolInput() json.RawMessage { return nil }

func (t promptTarget) HighRisk() bool { return false }

// DecisionRequest carries only identity axes: there is no tool to classify, so
// the local decider has nothing to match on and the verdict is /evaluate's.
func (t promptTarget) DecisionRequest(bool) decision.DecisionRequest {
	return decision.DecisionRequest{
		SessionID:    t.ev.SessionID,
		DeveloperDID: t.id.DeveloperDID,
		EventType:    client.EventPromptSubmitted,
	}
}

// DevEvent maps the prompt for the inline evaluation through the same Mapper
// (and pinned clock) the observe copy uses, so the two derive one event_id.
func (t promptTarget) DevEvent(*client.Content) (client.DevEvent, bool) {
	return t.mapper.Map(HookUserPromptSubmit, t.ev)
}

var _ hookflow.EnforceTarget = promptTarget{}

func recordPromptEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	hookflow.RecordEnforcement(logger, e.SessionID, promptToolKind, dec, res)
}

// haltReplayTriple picks the contract and the labels a halted run replays with,
// per hook. Every gated class reads the latch, not just the one that wrote it:
// a run halted at a prompt would otherwise keep running tools and model calls.
func haltReplayTriple(hook HookName, ev *HookEvent) (hookflow.OutputContract, string, string) {
	switch hook {
	case HookPreToolUse, HookPermissionRequest:
		kind, _, _, _, _ := classifyTool(ev.ToolName)
		return contractFor(hook), ev.ToolName, string(kind)
	case HookPreLLMCall:
		return llmContract, modelCallToolKind, modelCallToolKind
	}
	return promptContract, promptToolKind, promptToolKind
}
