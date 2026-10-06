package claudecode

import (
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// buildDecisionRequest assembles the local decision request from a PreToolUse
// payload, reusing the Mapper's tool classification (classifyTool / filePath)
// so the enforce gate and the observe event classify a tool identically.
func buildDecisionRequest(id Identity, e *HookEvent, localRedaction bool) decision.DecisionRequest {
	tc := toolCall(e)
	tc.DeveloperDID = id.DeveloperDID
	return tc.Request(localRedaction)
}

// Only `deny`/`ask` are ever emitted; enforcement can add a restriction, never
// remove one of Claude Code's built-in prompts.

type preToolUseOutput struct {
	// Continue/StopReason are Claude Code's session-stop lever, common to every
	// hook and documented to take precedence over any per-event decision.
	Continue           *bool              `json:"continue,omitempty"`
	StopReason         string             `json:"stopReason,omitempty"`
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	// UpdatedInput is the redacted replacement tool_input. Reconstructed from the
	// original tool_input with only the content field swapped (redactToolInput);
	// never sourced whole from the decision.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
}
