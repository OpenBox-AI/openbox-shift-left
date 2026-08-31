package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

const provider = "claude-code"

const agentToolName = provider

// Identity is the developer-agent identity the adapter emits under. Only the
// DID is needed to build events; the obx_ key and Ed25519 seed live in the
// client, never here (INV-1).
type Identity struct {
	DeveloperDID string // did:aip:<uuid>
}

// Mapper translates Claude Code hook payloads into normalized DevEvents.
type Mapper struct {
	Identity Identity
	Now      func() time.Time // injectable clock; defaults to time.Now
	// NewID, when non-nil, overrides the idempotency-id source (INV-5); used by
	// tests to pin ids.
	NewID func() string
	// Finops, when non-nil, carries the usage numbers only the finops reader
	// extracted from the SessionEnd transcript.
	Finops *FinopsUsage
	// CaptureContent authorizes copying the (content) prompt text onto the
	// emitted PromptSubmitted event. Default false = metadata-only (INV-2): the
	// prompt is never egressed.
	CaptureContent bool
	// RedactContent redacts a content body for secrets before it is attached to
	// an event. Nil ⇒ identity, which is the honest `secret_detection:false`
	// case: the text egresses unredacted (that decision says so rather than
	// hiding it).
	RedactContent func(string) string
	// Posture, when non-nil, is the session's effective posture (E8-S5), attached
	// to the SessionStarted event's metadata only.
	Posture *devconfig.Posture
	// Evidence, when non-nil, records how much of this session's telemetry is
	// known to be undelivered at session end (E8-S7).
	Evidence *EvidenceState
}

// EvidenceState is the completeness of a session's telemetry as the client can
// see it at session end.
type EvidenceState struct {
	Undelivered int
}

func (e EvidenceState) metadata() map[string]any {
	state := "complete"
	if e.Undelivered > 0 {
		state = "degraded"
	}
	m := map[string]any{"evidence_state": state}
	if e.Undelivered > 0 {
		m["evidence_undelivered"] = e.Undelivered
	}
	return m
}

// FinopsUsage is the numbers-only usage rollup the finops reader produces from
// a transcript.
type FinopsUsage struct {
	Tokens *client.Tokens
	Cost   *client.Cost
}

// NewMapper returns a Mapper with production defaults.
func NewMapper(id Identity) Mapper {
	return Mapper{Identity: id, Now: time.Now}
}

// Map converts one hook payload into a normalized DevEvent. The bool reports
// whether an event should be emitted at all: it is false when the payload is
// unusable (no session id, or no valid developer DID); in which case the
// caller drops it fail-open (INV-3), never blocking the tool call.
func (m Mapper) Map(hook HookName, e *HookEvent) (client.DevEvent, bool) {
	if e == nil || e.SessionID == "" {
		return client.DevEvent{}, false
	}
	if !strings.HasPrefix(m.Identity.DeveloperDID, "did:aip:") {
		return client.DevEvent{}, false
	}

	now := m.clock()
	ts := now.UTC().Format(time.RFC3339Nano)

	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		SessionID:     e.SessionID,
		DeveloperDID:  m.Identity.DeveloperDID,
		Timestamp:     ts,
	}

	switch hook {
	case HookSessionStart:
		ev.EventType = client.EventSessionStarted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = mergeMetadata(
			sessionStartMetadata(e),
			// Silent on any failure: an optional attribution field must never stop a
			// session reporting.
			accountMetadata(localAccount(homeDir())))
		if m.Posture != nil {
			ev.Metadata["posture"] = m.Posture.Metadata()
		}

	case HookUserPromptSubmit:
		ev.EventType = client.EventPromptSubmitted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = mergeMetadata(
			compact(map[string]any{"permission_mode": enumOr(e.PermissionMode, permissionModes)}),
			subagentMetadata(e))
		// Default off ⇒ Content stays nil and the prompt never egresses (Emit would
		// strip it anyway).
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
			// It also coupled two unrelated numbers: changing MaxCommandLen for local-
			// matching reasons silently changed what the server can see.
			if in := m.redact(toolInputExtract(e, nil)); in != "" {
				ev.Content = &client.Content{ToolInput: in}
			}
		}

	case HookPostToolUse:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Status = client.StatusCompleted
		ev.Content = m.gatedToolOutput(e.toolOutputText())

	case HookPostToolUseFailure:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Status = client.StatusFailed
		if e.IsInterrupt != nil {
			ev.Metadata["is_interrupt"] = *e.IsInterrupt
		}
		ev.Content = m.gatedToolOutput(e.ErrorType)

	case HookSubagentStart:
		ev.EventType = client.EventSubagentStarted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = subagentMetadata(e)

	case HookPermissionDenied:
		// It must never reach signal_args; see Content.SignalDetail for what core
		// would do with it.
		ev.EventType = client.EventPermissionDenied
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Content = m.gatedSignalDetail(e.Reason)

	case HookStopFailure:
		ev.EventType = client.EventAPIError
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = mergeMetadata(
			compact(map[string]any{"error_type": enumOr(e.ErrorType, apiErrorTypes)}),
			subagentMetadata(e))
		ev.Content = m.gatedSignalDetail(e.ErrorDetails)

	case HookSessionEnd:
		ev.EventType = client.EventSessionEnded
		ev.EndedAt = ts
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = compact(map[string]any{"reason": enumOr(e.Reason, reasonValues)})
		if m.Finops != nil {
			ev.Tokens = m.Finops.Tokens
			ev.Cost = m.Finops.Cost
		}
		if m.Evidence != nil {
			ev.Metadata = mergeMetadata(ev.Metadata, m.Evidence.metadata())
		}

	case HookStop, HookSubagentStop:
		return client.DevEvent{}, false

	default:
		return client.DevEvent{}, false
	}

	ev.EventID = m.eventID(ev)
	return ev, true
}

// MapTurn builds one model turn's ActivityStarted/ActivityCompleted pair from
// a Stop/SubagentStop firing and the transcript window it delimits.
func (m Mapper) MapTurn(e *HookEvent, w turnWindow, index int) (started, completed client.DevEvent, ok bool) {
	if e == nil || e.SessionID == "" || !w.HasUsage {
		return client.DevEvent{}, client.DevEvent{}, false
	}
	if !strings.HasPrefix(m.Identity.DeveloperDID, "did:aip:") {
		return client.DevEvent{}, client.DevEvent{}, false
	}

	now := m.clock()
	closeTS := now.UTC().Format(time.RFC3339Nano)
	openTS := closeTS
	haveRealOpen := false
	if !w.Open.IsZero() {
		openTS = w.Open.UTC().Format(time.RFC3339Nano)
		haveRealOpen = true
	}

	turnIndex := index
	base := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		SessionID:     e.SessionID,
		DeveloperDID:  m.Identity.DeveloperDID,
		Tool:          client.Tool{Name: agentToolName, Kind: client.ToolShell},
		TurnIndex:     &turnIndex,
		AgentID:       capStr(e.AgentID),
	}

	started = base
	started.EventType = client.EventTurnStarted
	started.Timestamp = openTS
	started.StartedAt = openTS
	started.Metadata = turnMetadata(e, turnIndex)
	started.EventID = m.eventID(started)

	completed = base
	completed.EventType = client.EventTurnCompleted
	completed.Timestamp = closeTS
	completed.EndedAt = closeTS
	if haveRealOpen {
		completed.StartedAt = openTS
	}
	completed.Tokens = w.tokens()
	completed.Model = capStr(w.Model)
	// Deliberately NOT on the started half: a turn's input is the prompt, which
	// already rides PromptSubmitted under the same gate.
	if m.CaptureContent {
		var c client.Content
		if e.LastAssistantMessage != "" {
			c.Output = m.redact(e.LastAssistantMessage)
		}
		if w.Thinking != "" {
			c.Thinking = m.redact(w.Thinking)
		}
		if c.Output != "" || c.Thinking != "" {
			completed.Content = &c
		}
	}
	completed.Metadata = turnMetadata(e, turnIndex)
	completed.EventID = m.eventID(completed)

	return started, completed, true
}

// turnMetadata identifiers and one integer, never content (INV-2).
func turnMetadata(e *HookEvent, index int) map[string]any {
	m := compact(map[string]any{
		"agent_id":   capStr(e.AgentID),
		"agent_type": capStr(e.AgentType),
	})
	m["turn_index"] = index
	return m
}

// mapTool two identities, deliberately separate (client.Span.InvocationID /
// OperationID):
//   - InvocationID = tool_use_id, which Claude Code mints per call.
//   - Shell → the command, hashed.
func mapTool(e *HookEvent, stage string) (client.Tool, *client.Span) {
	kind, sem, fileOp, mcpServer, function := classifyTool(e.ToolName)

	tool := client.Tool{Name: capStr(e.ToolName), Kind: kind}
	if kind == client.ToolMCP {
		tool.MCPServer = capStr(mcpServer)
	}

	span := &client.Span{SemanticType: sem, Stage: stage}
	switch {
	case isFileSemantic(sem):
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
	// A gated class must never reach here; see
	// TestHighRiskClassesHaveAStableOperationID.
	return capStr(e.ToolUseID)
}

// toolMetadata identifiers only, never content (INV-2); tool_input and
// tool_response are not represented.
func toolMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
		"tool_use_id":     capStr(e.ToolUseID),
		"agent_id":        capStr(e.AgentID),
		"agent_type":      capStr(e.AgentType),
		"subagent_type":   capStr(e.subagentType()),
	})
}

func subagentMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"agent_id":   capStr(e.AgentID),
		"agent_type": capStr(e.AgentType),
	})
}

func mergeMetadata(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
	return dst
}

func classifyTool(name string) (kind client.ToolKind, sem, fileOp, mcpServer, function string) {
	if strings.HasPrefix(name, "mcp__") {
		server, fn := splitMCPName(name)
		if server == "" {
			// Claude Code never emits this.
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

var builtinTools = map[string]toolClass{
	"Write":        {client.ToolFile, "file_write", "write"},
	"Edit":         {client.ToolFile, "file_write", "edit"},
	"MultiEdit":    {client.ToolFile, "file_write", "edit"},
	"NotebookEdit": {client.ToolFile, "file_write", "edit"},
	"Read":         {client.ToolFile, "file_read", "read"},
	"NotebookRead": {client.ToolFile, "file_read", "read"},
	"Glob":         {client.ToolFile, "internal", ""},
	"Grep":         {client.ToolFile, "internal", ""},
	"Bash":         {client.ToolShell, "internal", ""},
	"BashOutput":   {client.ToolShell, "internal", ""},
	"KillShell":    {client.ToolShell, "internal", ""},
}

func isFileSemantic(sem string) bool {
	switch sem {
	case "file_read", "file_write", "file_open", "file_delete":
		return true
	}
	return false
}

func splitMCPName(name string) (server, function string) {
	rest := strings.TrimPrefix(name, "mcp__")
	parts := strings.SplitN(rest, "__", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

func sessionStartMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"provider":        provider,
		"source":          enumOr(e.Source, sourceValues), // startup|resume|clear|compact
		"model":           capStr(e.Model),                // free-form model id → bounded
		"cwd":             capStr(e.Cwd),                  // structural (blessed by SL-1 testdata); not content
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
	})
}

// maxIdentLen bounds every externally-influenced identifier/path field before
// egress: a crafted payload or a malicious MCP server's tool name can't push
// an unbounded / content-shaped string into tool.name, span.function,
// span.mcp_server, or file_path.
const maxIdentLen = 512

var apiErrorTypes = map[string]bool{
	"authentication_failed": true,
	"oauth_org_not_allowed": true,
	"billing_error":         true,
	"rate_limit":            true,
	"overloaded":            true,
	"invalid_request":       true,
	"model_not_found":       true,
	"server_error":          true,
	"max_output_tokens":     true,
	"unknown":               true,
}

var (
	sourceValues    = map[string]bool{"startup": true, "resume": true, "clear": true, "compact": true}
	reasonValues    = map[string]bool{"clear": true, "resume": true, "logout": true, "prompt_input_exit": true, "bypass_permissions_disabled": true, "other": true}
	permissionModes = map[string]bool{"default": true, "plan": true, "acceptEdits": true, "auto": true, "dontAsk": true, "bypassPermissions": true}
)

func enumOr(v string, allowed map[string]bool) string {
	if allowed[v] {
		return v
	}
	return ""
}

func capStr(s string) string {
	r := []rune(s)
	if len(r) <= maxIdentLen {
		return s
	}
	return string(r[:maxIdentLen])
}

func compact(m map[string]any) map[string]any {
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
	return m
}

func (m Mapper) clock() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
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

func (m Mapper) gatedSignalDetail(text string) *client.Content {
	if !m.CaptureContent || text == "" {
		return nil
	}
	out := m.redact(text)
	if out == "" {
		return nil
	}
	return &client.Content{SignalDetail: out}
}

func (m Mapper) redact(s string) string {
	if m.RedactContent == nil {
		return s
	}
	return m.RedactContent(s)
}

func (m Mapper) eventID(ev client.DevEvent) string {
	if m.NewID != nil {
		return m.NewID()
	}
	return deriveID(ev)
}

//   - The same logical event always yields the same id; robust even if the id
//     is ever recomputed from the spooled/persisted record (the fields it
//     hashes all survive the spool round-trip), and
//   - Two distinct events never collide: the high-resolution timestamp
//     (RFC3339Nano) is the per-event distinguisher, reinforced by the
//     structural separators (session, type, tool name, file/function locator).
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
	if ev.Span != nil {
		b.WriteByte(sep)
		b.WriteString(ev.Span.FilePath)
		b.WriteByte(sep)
		b.WriteString(ev.Span.Function)
		b.WriteByte(sep)
		b.WriteString(ev.Span.InvocationID)
	}
	if ev.TurnIndex != nil {
		b.WriteByte(sep)
		b.WriteString(strconv.Itoa(*ev.TurnIndex))
	}
	if ev.AgentID != "" {
		b.WriteByte(sep)
		b.WriteString(ev.AgentID)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "cc-" + hex.EncodeToString(sum[:])
}
