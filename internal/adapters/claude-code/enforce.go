package claudecode

import (
	"context"
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// EnforceDecision is the PreToolUse enforce gate: it synchronously obtains a
// governance decision from the in-process decider for the tool that is about
// to run.
func EnforceDecision(ctx context.Context, cl decision.Decider, id Identity, e *HookEvent, localRedaction bool) decision.Decision {
	return cl.Decide(ctx, buildDecisionRequest(id, e, localRedaction))
}

// buildDecisionRequest assembles the local decision request from a PreToolUse
// payload, reusing the Mapper's tool classification (classifyTool / filePath)
// so the enforce gate and the observe event classify a tool identically.
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
		attrs["file_path"] = capStr(e.filePath()) // structural locator (INV-2)
		attrs["file_operation"] = fileOp
	case kind == client.ToolMCP:
		attrs["mcp_function"] = capStr(function)
	case kind == client.ToolShell:
		// It goes only to the in-process decider and is never egressed/logged
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
