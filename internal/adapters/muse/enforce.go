package muse

import (
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// buildDecisionRequest assembles the local decision request from a PreToolUse
// payload, reusing the mapper's tool classification so the gate and the observe
// event classify a tool identically.
func buildDecisionRequest(id Identity, e *HookEvent, localRedaction bool) decision.DecisionRequest {
	tc := toolCall(e)
	tc.DeveloperDID = id.DeveloperDID
	return tc.Request(localRedaction)
}

// recordEnforcement files the applied decision of a tool-scoped gate.
func recordEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	hookflow.RecordEnforcement(logger, e.SessionID, string(kind), dec, res)
}
