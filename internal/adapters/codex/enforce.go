package codex

import (
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// An enforce PreToolUse hook may block, but only pre-execution and within a
// hard latency bound.

// buildDecisionRequest assembles the local decision request from a PreToolUse
// payload, reusing the mapper's tool classification (classifyTool) so the
// enforce gate and the observe event classify a tool identically.
func buildDecisionRequest(id Identity, e *HookEvent, localRedaction bool) decision.DecisionRequest {
	tc := toolCall(e)
	tc.DeveloperDID = id.DeveloperDID
	return tc.Request(localRedaction)
}

type preToolUseOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	// UpdatedInput is the redacted replacement tool_input. Reconstructed from the
	// original tool_input with only the "command" field swapped
	// (redactToolInput); never sourced whole from the decision.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
}
