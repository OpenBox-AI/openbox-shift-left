package claudecode

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
