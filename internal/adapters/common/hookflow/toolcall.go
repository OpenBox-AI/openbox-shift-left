package hookflow

import (
	"encoding/json"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// ToolClass is one builtin tool's classification in an adapter's table.
type ToolClass struct {
	Kind   client.ToolKind
	Sem    string
	FileOp string
}

// ClassifyTool classifies a tool by name: an `mcp__<server>__<fn>` name is an
// MCP call, a name in builtins is what the table says, and anything else is
// internal shell work.
func ClassifyTool[C ~struct {
	Kind   client.ToolKind
	Sem    string
	FileOp string
}](name string, builtins map[string]C) (kind client.ToolKind, sem, fileOp, mcpServer, function string) {
	if strings.HasPrefix(name, "mcp__") {
		server, fn := SplitMCPName(name)
		if server == "" {
			return client.ToolShell, "internal", "", "", ""
		}
		return client.ToolMCP, "mcp_tool_call", "", server, fn
	}
	if b, ok := builtins[name]; ok {
		c := ToolClass(b)
		return c.Kind, c.Sem, c.FileOp, "", ""
	}
	return client.ToolShell, "internal", "", "", ""
}

// ToolCall is what an adapter knows about one tool invocation: its identity,
// its classification (the same classifyTool for the observe event and the
// gate, so the two classify a tool identically) and lazy accessors for the
// payload fields only some branches read.
type ToolCall struct {
	SessionID       string
	DeveloperDID    string
	ToolName        string
	ToolUseID       string
	ToolInput       json.RawMessage
	PermissionMode  string
	PermissionModes map[string]bool

	Kind      client.ToolKind
	Sem       string
	FileOp    string
	MCPServer string
	Function  string

	// FilePath, when non-nil, is the structural file_path locator (INV-2) of a
	// file-semantic tool.
	FilePath func() string
	// Command is the shell command a shell call's operation id derives from.
	Command func() string
	// DecisionCommand, when non-nil, replaces Command as the command the local
	// decider sees. It goes only to the in-process decider and is never
	// egressed or logged.
	DecisionCommand func() string
	// FileText is the file body, attached for local redaction only.
	FileText func() string
}

// Tool is the call's normalized tool.
func (t ToolCall) Tool() client.Tool {
	tool := client.Tool{Name: CapIdent(t.ToolName), Kind: t.Kind}
	if t.Kind == client.ToolMCP {
		tool.MCPServer = CapIdent(t.MCPServer)
	}
	return tool
}

// Span is the call's span at stage.
func (t ToolCall) Span(stage string) *client.Span {
	span := &client.Span{SemanticType: t.Sem, Stage: stage}
	switch {
	case IsFileSemantic(t.Sem):
		if t.FilePath != nil {
			span.FilePath = CapIdent(t.FilePath()) // structural locator only (INV-2)
		}
		span.FileOp = t.FileOp
	case t.Kind == client.ToolMCP:
		span.MCPServer = CapIdent(t.MCPServer)
		span.Function = CapIdent(t.Function)
	}
	span.InvocationID = CapIdent(t.ToolUseID)
	span.OperationID = t.OperationID()
	return span
}

// OperationID is the call's stable operation id: the shell command's
// operation, an MCP call's arguments, else the invocation id. A gated class
// must never reach the last case; see TestHighRiskClassesHaveAStableOperationID.
func (t ToolCall) OperationID() string {
	switch t.Kind {
	case client.ToolShell:
		if op := client.OperationForCommand(t.Command()); op != "" {
			return op
		}
	case client.ToolMCP:
		return client.OperationForArgs(t.ToolInput)
	}
	return CapIdent(t.ToolUseID)
}

// Request assembles the local decision request for the pre-execution gate, a
// ToolCall decision.
func (t ToolCall) Request(localRedaction bool) decision.DecisionRequest {
	attrs := map[string]any{
		"permission_mode": EnumOr(t.PermissionMode, t.PermissionModes),
	}
	switch {
	case IsFileSemantic(t.Sem):
		if t.FilePath != nil {
			attrs["file_path"] = CapIdent(t.FilePath())
		}
		attrs["file_operation"] = t.FileOp
	case t.Kind == client.ToolMCP:
		attrs["mcp_function"] = CapIdent(t.Function)
	case t.Kind == client.ToolShell:
		command := t.Command
		if t.DecisionCommand != nil {
			command = t.DecisionCommand
		}
		attrs["command"] = CapCommand(command())
	}

	req := decision.DecisionRequest{
		SessionID:    t.SessionID,
		DeveloperDID: t.DeveloperDID,
		EventType:    client.EventToolCall,
		Tool:         t.Tool(),
		Attributes:   CompactAny(attrs),
	}

	if localRedaction && IsFileSemantic(t.Sem) {
		if body := t.FileText(); body != "" && len(body) <= MaxRedactBody {
			req.Content = &client.Content{FileText: body}
		}
	}
	return req
}
