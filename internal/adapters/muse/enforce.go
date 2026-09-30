package muse

import (
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// buildDecisionRequest assembles the local decision request from a PreToolUse
// payload, reusing the mapper's tool classification so the gate and the observe
// event classify a tool identically.
func buildDecisionRequest(id Identity, e *HookEvent, localRedaction bool) decision.DecisionRequest {
	kind, sem, fileOp, mcpServer, function := classifyTool(e.ToolName)

	tool := client.Tool{Name: capStr(e.ToolName), Kind: kind}
	if kind == client.ToolMCP {
		tool.MCPServer = capStr(mcpServer)
	}

	attrs := map[string]any{
		"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes),
	}
	switch {
	case hookflow.IsFileSemantic(sem):
		attrs["file_path"] = capStr(e.filePath()) // structural locator (INV-2)
		attrs["file_operation"] = fileOp
	case kind == client.ToolMCP:
		attrs["mcp_function"] = capStr(function)
	case kind == client.ToolShell:
		// Only the in-process decider sees it; it is never egressed or logged.
		attrs["command"] = hookflow.CapCommand(e.shellText())
	}

	req := decision.DecisionRequest{
		SessionID:    e.SessionID,
		DeveloperDID: id.DeveloperDID,
		EventType:    client.EventToolCall, // the pre-execution gate is a ToolCall decision
		Tool:         tool,
		Attributes:   hookflow.CompactAny(attrs),
	}

	if localRedaction && hookflow.IsFileSemantic(sem) {
		if body := e.fileText(); body != "" && len(body) <= hookflow.MaxRedactBody {
			req.Content = &client.Content{FileText: body}
		}
	}
	return req
}

// recordEnforcement files the applied decision of a tool-scoped gate.
func recordEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	hookflow.RecordEnforcement(logger, e.SessionID, string(kind), dec, res)
}
