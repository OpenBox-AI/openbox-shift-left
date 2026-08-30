package codex

import (
	"context"
	"encoding/json"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// INV-3b (the carve-out to "observe never blocks"): an enforce PreToolUse hook
// may block, but only pre-execution, hard-bounded, and fail-open by default.

// EnforceDecision is the PreToolUse enforce gate: it synchronously obtains a
// governance decision from the in-process decider for the tool about to run.
// It never errors and never blocks; the decider fails open
// (VerdictUnknown/allow) on any fault.
func EnforceDecision(ctx context.Context, cl decision.Decider, id Identity, e *HookEvent, localRedaction bool) decision.Decision {
	return cl.Decide(ctx, buildDecisionRequest(id, e, localRedaction))
}

// buildDecisionRequest assembles the local decision request from a PreToolUse
// payload, reusing the mapper's tool classification (classifyTool) so the
// enforce gate and the observe event classify a tool identically.
func buildDecisionRequest(id Identity, e *HookEvent, localRedaction bool) decision.DecisionRequest {
	kind, sem, fileOp, mcpServer, function := classifyTool(e.ToolName)

	tool := client.Tool{Name: capStr(e.ToolName), Kind: kind}
	if kind == client.ToolMCP {
		tool.MCPServer = capStr(mcpServer)
	}

	attrs := map[string]any{
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
	}
	switch {
	case isFileSemantic(sem):
		attrs["file_operation"] = fileOp
	case kind == client.ToolMCP:
		attrs["mcp_function"] = capStr(function)
	case kind == client.ToolShell:
		// It goes only to the in-process decider and is never egressed/ logged
		// (HookEvent.command).
		attrs["command"] = hookflow.CapCommand(e.command())
	}

	req := decision.DecisionRequest{
		SessionID:    e.SessionID,
		DeveloperDID: id.DeveloperDID,
		EventType:    client.EventToolCall, // the pre-execution gate is a ToolCall decision
		Tool:         tool,
		Attributes:   hookflow.CompactAny(attrs),
	}

	if localRedaction && isFileSemantic(sem) {
		if body := e.fileText(); body != "" && len(body) <= hookflow.MaxRedactBody {
			req.Content = &client.Content{FileText: body}
		}
	}
	return req
}

func isFileSemantic(sem string) bool {
	switch sem {
	case "file_read", "file_write", "file_open", "file_delete":
		return true
	}
	return false
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

// For a fail-closed org that is a silently ungoverned call, so our verdict
// must land first.

const maxEnforceTimeout = 2 * time.Second
