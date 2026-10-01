package muse

import (
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

const promptToolKind = hookflow.PromptToolKind

// newPromptTarget is the gate's view of a UserPromptSubmit payload
// (hookflow.PromptTarget).
func newPromptTarget(id Identity, mapper Mapper, ev *HookEvent) hookflow.EnforceTarget {
	return hookflow.PromptTarget[HookName, HookEvent, Mapper]{
		SessionIDValue: ev.SessionID,
		DeveloperDID:   id.DeveloperDID,
		Mapper:         mapper,
		Hook:           HookUserPromptSubmit,
		Event:          ev,
	}
}

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
