package muse

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

const provider = "muse"

const agentToolName = provider

// Identity is the developer-agent identity the adapter emits under. Only the
// DID is needed to build events; the key material lives in the client, never
// here (INV-1).
type Identity struct {
	DeveloperDID string // did:aip:<uuid>
}

// RunIdentity is one hook invocation's resolved continue-as-new identity:
// generation 0 (the zero value) means no continuation has happened for this
// session, and the wire run_id falls back to the session id. A nil Run on the
// Mapper means the same thing.
type RunIdentity struct {
	Generation int
	// RunID is the minted run id; empty at generation 0.
	RunID string
	// ContinuedFrom is the run this one continues from; set only when
	// Generation >= 1, and only read by the SessionStart case.
	ContinuedFrom string
}

// EvidenceState is a session's telemetry completeness. An alias, not a copy.
type EvidenceState = hookflow.EvidenceState

// Mapper translates Muse hook payloads into normalized DevEvents.
type Mapper struct {
	Identity Identity
	Now      func() time.Time // injectable clock; defaults to time.Now
	// NewID, when non-nil, overrides the idempotency-id source; used by tests to
	// pin ids.
	NewID func() string
	// CaptureContent authorizes copying content (the prompt, tool input and
	// output, model-call message previews) onto an emitted event: on by default,
	// opt-out honoured.
	CaptureContent bool
	// RedactContent redacts a content body for secrets before it is attached.
	// Nil is the identity, which is the honest secret_detection:false case.
	RedactContent func(string) string
	// Posture, when non-nil, is attached to the SessionStarted metadata only.
	Posture *devconfig.Posture
	// Evidence, when non-nil, records how much of the session's telemetry is
	// known to be undelivered at session end.
	Evidence *EvidenceState
	// Run is the session's resolved run identity.
	Run *RunIdentity
}

// NewMapper returns a Mapper with production defaults.
func NewMapper(id Identity) Mapper {
	return Mapper{Identity: id, Now: time.Now}
}

// Map converts one hook payload into a normalized DevEvent. The bool reports
// whether an event should be emitted at all: false when the payload is unusable
// (no session id, no valid developer DID) or the hook maps to nothing. The
// caller drops it; a gated caller then denies, never proceeds.
func (m Mapper) Map(hook HookName, e *HookEvent) (client.DevEvent, bool) {
	if e == nil || e.SessionID == "" {
		return client.DevEvent{}, false
	}
	if !strings.HasPrefix(m.Identity.DeveloperDID, "did:aip:") {
		return client.DevEvent{}, false
	}

	ts := m.clock().UTC().Format(time.RFC3339Nano)
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		SessionID:     e.SessionID,
		DeveloperDID:  m.Identity.DeveloperDID,
		Timestamp:     ts,
	}
	if m.Run != nil {
		ev.RunID = m.Run.RunID
		ev.RunGeneration = m.Run.Generation
	}
	agent := client.Tool{Name: agentToolName, Kind: client.ToolShell}

	switch hook {
	case HookSessionStart:
		ev.EventType = client.EventSessionStarted
		ev.Tool = agent
		ev.Metadata = sessionStartMetadata(e)
		if m.Posture != nil {
			ev.Metadata["posture"] = m.Posture.Metadata()
		}
		if m.Run != nil && m.Run.ContinuedFrom != "" {
			ev.ContinuedFromRunID = m.Run.ContinuedFrom
		}

	case HookUserPromptSubmit:
		ev.EventType = client.EventPromptSubmitted
		ev.Tool = agent
		ev.Metadata = hookflow.Compact(map[string]any{
			"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes),
			"turn_id":         capStr(e.TurnID),
		})
		if m.CaptureContent && e.Prompt != "" {
			if p := m.redact(e.Prompt); p != "" {
				ev.Content = &client.Content{Prompt: p}
			}
		}

	case HookPreToolUse:
		ev.EventType = client.EventToolCall
		ev.StartedAt = ts
		ev.Tool, ev.Span = mapTool(e, "started")
		ev.Metadata = toolMetadata(e)
		if m.CaptureContent {
			if in := m.redact(toolInputExtract(e)); in != "" {
				ev.Content = &client.Content{ToolInput: in}
			}
		}

	case HookPermissionRequest:
		// A signal, not an activity: the escalation is not a second execution of
		// the tool, and Muse gives it no tool_use_id to pair with.
		ev.EventType = client.EventPermissionRequest
		kind, _, _, mcpServer, _ := classifyTool(e.ToolName)
		ev.Tool = client.Tool{Name: capStr(e.ToolName), Kind: kind}
		if kind == client.ToolMCP {
			ev.Tool.MCPServer = capStr(mcpServer)
		}
		ev.Metadata = hookflow.Compact(map[string]any{
			"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes),
			"tool_name":       capStr(e.ToolName),
		})

	case HookPostToolUse:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Status = client.StatusCompleted
		ev.Content = m.gatedToolOutput(outputText(e.ToolResponse))

	case HookPostToolUseFailure:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Status = client.StatusFailed
		ev.Content = m.gatedToolOutput(outputText(e.Error))

	case HookSubagentStart:
		ev.EventType = client.EventSubagentStarted
		ev.Tool = agent
		ev.Metadata = hookflow.Compact(map[string]any{"agent_id": capStr(e.SubagentID)})

	case HookStopFailure:
		// The provider's error text is undocumented and never bound, so the row
		// says only that a turn ended in a provider-side error.
		ev.EventType = client.EventAPIError
		ev.Tool = agent
		ev.Metadata = hookflow.Compact(map[string]any{"turn_id": capStr(e.TurnID)})

	case HookSessionEnd:
		ev.EventType = client.EventSessionEnded
		ev.EndedAt = ts
		ev.Tool = agent
		ev.Metadata = hookflow.Compact(map[string]any{"reason": hookflow.EnumOr(e.Reason, reasonValues)})
		if m.Evidence != nil {
			ev.Metadata = mergeMetadata(ev.Metadata, m.Evidence.Metadata())
		}

	case HookPreLLMCall:
		return m.mapModelCallRequested(ev, agent, e)

	case HookPostLLMCall:
		return m.mapModelCallFinished(ev, agent, e)

	default:
		return client.DevEvent{}, false
	}

	ev.EventID = m.eventID(ev)
	return ev, true
}

func mergeMetadata(dst, src map[string]any) map[string]any {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]any, len(src))
	}
	for k, v := range src {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
	return dst
}

// toolMetadata is identifiers only, never content (INV-2).
func toolMetadata(e *HookEvent) map[string]any {
	return hookflow.Compact(map[string]any{
		"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes),
		"tool_use_id":     capStr(e.ToolUseID),
		"turn_id":         capStr(e.TurnID),
	})
}

// mapTool builds a tool call's identity, and its two ids kept deliberately
// separate: InvocationID is Muse's tool_use_id, OperationID is what is being
// done, identical across a retry.
func mapTool(e *HookEvent, stage string) (client.Tool, *client.Span) {
	kind, sem, fileOp, mcpServer, function := classifyTool(e.ToolName)

	tool := client.Tool{Name: capStr(e.ToolName), Kind: kind}
	if kind == client.ToolMCP {
		tool.MCPServer = capStr(mcpServer)
	}

	span := &client.Span{SemanticType: sem, Stage: stage}
	switch {
	case hookflow.IsFileSemantic(sem):
		span.FilePath = capStr(e.filePath()) // structural locator only (INV-2)
		span.FileOp = fileOp
	case kind == client.ToolMCP:
		span.MCPServer = capStr(mcpServer)
		span.Function = capStr(function)
	}
	span.InvocationID = capStr(e.ToolUseID)
	span.OperationID = operationID(kind, e)
	return tool, span
}

func operationID(kind client.ToolKind, e *HookEvent) string {
	switch kind {
	case client.ToolShell:
		if op := client.OperationForCommand(e.command()); op != "" {
			return op
		}
	case client.ToolMCP:
		return client.OperationForArgs(e.ToolInput)
	}
	return capStr(e.ToolUseID)
}

// classifyTool sorts a Muse tool name into the contract's tool classes. A name
// this table does not know is shell-kinded and semantically opaque, which is
// the case a file write must never fall into: TestFixturesToolsAreClassified
// fails on any fixture tool that lands here by default.
func classifyTool(name string) (kind client.ToolKind, sem, fileOp, mcpServer, function string) {
	if strings.HasPrefix(name, "mcp__") {
		server, fn := hookflow.SplitMCPName(name)
		if server == "" {
			return client.ToolShell, "internal", "", "", ""
		}
		return client.ToolMCP, "mcp_tool_call", "", server, fn
	}
	if c, ok := builtinTools[name]; ok {
		return c.kind, c.sem, c.fileOp, "", ""
	}
	return client.ToolShell, "internal", "", "", ""
}

type toolClass struct {
	kind   client.ToolKind
	sem    string
	fileOp string
}

// builtinTools maps Muse's tool names. Muse accepts the Claude Code names as
// aliases (the documented ones are Bash, Read, Edit, Write, Grep, WebFetch,
// WebSearch), and the native names read_file, write_file and edit_file are
// named in Muse's own documentation. The other native spellings below are
// unverified guesses kept so a rename does not turn a write into an
// unclassified call; a real capture should settle them.
var builtinTools = map[string]toolClass{
	"Bash":       {client.ToolShell, "internal", ""},
	"BashOutput": {client.ToolShell, "internal", ""},
	"KillShell":  {client.ToolShell, "internal", ""},
	"bash":       {client.ToolShell, "internal", ""},
	"shell":      {client.ToolShell, "internal", ""},

	"Write":        {client.ToolFile, "file_write", "write"},
	"write_file":   {client.ToolFile, "file_write", "write"},
	"create_file":  {client.ToolFile, "file_write", "write"},
	"Edit":         {client.ToolFile, "file_write", "edit"},
	"edit_file":    {client.ToolFile, "file_write", "edit"},
	"MultiEdit":    {client.ToolFile, "file_write", "edit"},
	"multi_edit":   {client.ToolFile, "file_write", "edit"},
	"NotebookEdit": {client.ToolFile, "file_write", "edit"},
	"apply_patch":  {client.ToolFile, "file_write", "edit"},
	"patch_file":   {client.ToolFile, "file_write", "edit"},

	"Read":         {client.ToolFile, "file_read", "read"},
	"read_file":    {client.ToolFile, "file_read", "read"},
	"NotebookRead": {client.ToolFile, "file_read", "read"},
	"Glob":         {client.ToolFile, "internal", ""},
	"Grep":         {client.ToolFile, "internal", ""},
	"glob":         {client.ToolFile, "internal", ""},
	"grep":         {client.ToolFile, "internal", ""},

	"WebFetch":  {client.ToolShell, "internal", ""},
	"WebSearch": {client.ToolShell, "internal", ""},
}

func sessionStartMetadata(e *HookEvent) map[string]any {
	return hookflow.Compact(map[string]any{
		"provider":        provider,
		"source":          hookflow.EnumOr(e.Source, sourceValues),
		"model":           capStr(e.Model),
		"cwd":             capStr(e.Cwd),
		"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes),
	})
}

const maxIdentLen = 512

// A value outside its set is dropped, never egressed verbatim.
var (
	sourceValues = map[string]bool{"startup": true, "resume": true, "clear": true, "compact": true}
	// reasonValues is SessionEnd's cause; Muse's own values are undocumented.
	reasonValues = map[string]bool{"clear": true, "resume": true, "logout": true, "prompt_input_exit": true, "other": true}
	// permissionModes is the closed enum the contract declares for
	// metadata.permission_mode; a Muse mode outside it is dropped.
	permissionModes = map[string]bool{"default": true, "plan": true, "acceptEdits": true, "auto": true, "dontAsk": true, "bypassPermissions": true}
)

// isBumpSource reports whether a SessionStart source opens a new run of the
// same session: a resume and a clear both continue-as-new. Whether Muse keeps
// the session id across /clear is unverified; either way the new run is
// unlatched, and a clear under a fresh id simply has no run to continue.
func isBumpSource(source string) bool {
	s := hookflow.EnumOr(source, sourceValues)
	return s == "resume" || s == "clear"
}

// capStr delegates so the adapters and the engine cap an identifier the same
// way.
func capStr(s string) string { return hookflow.CapIdent(s) }

func (m Mapper) clock() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// redact runs a content body through the secret detector before it is attached.
// It lives on the Mapper so every caller of Map inherits it: the gate re-maps
// the same hook event for its own copy, and a call-site redactor would leave
// that copy raw.
func (m Mapper) redact(s string) string {
	if m.RedactContent == nil {
		return s
	}
	return m.RedactContent(s)
}

func (m Mapper) gatedToolOutput(text string) *client.Content {
	if !m.CaptureContent || text == "" {
		return nil
	}
	out := m.redact(text)
	if out == "" {
		return nil
	}
	return &client.Content{ToolOutput: out}
}

func (m Mapper) eventID(ev client.DevEvent) string {
	if m.NewID != nil {
		return m.NewID()
	}
	return deriveID(ev)
}

func deriveID(ev client.DevEvent) string {
	const sep = 0x1f
	var b strings.Builder
	b.WriteString(ev.SessionID)
	b.WriteByte(sep)
	b.WriteString(string(ev.EventType))
	b.WriteByte(sep)
	b.WriteString(ev.Tool.Name)
	b.WriteByte(sep)
	b.WriteString(ev.Timestamp) // RFC3339Nano; the per-event distinguisher
	b.WriteByte(sep)
	b.WriteString(ev.ModelCallRequestID)
	if ev.Span != nil {
		b.WriteByte(sep)
		b.WriteString(ev.Span.FilePath)
		b.WriteByte(sep)
		b.WriteString(ev.Span.Function)
		b.WriteByte(sep)
		b.WriteString(ev.Span.InvocationID)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "mus-" + hex.EncodeToString(sum[:])
}
