package codex

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

const provider = "codex"

const agentToolName = provider

// Identity is the developer-agent identity the adapter emits under. Only the
// DID is needed to build events; the obx_ key and Ed25519 seed live in the
// client, never here (INV-1).
type Identity struct {
	DeveloperDID string // did:aip:<uuid>
}

// Mapper translates Codex hook payloads into normalized DevEvents.
type Mapper struct {
	Identity Identity
	Now      func() time.Time // injectable clock; defaults to time.Now
	// NewID, when non-nil, overrides the idempotency-id source (INV-5); used by
	// tests to pin ids.
	NewID func() string
	// CaptureContent authorizes copying the (content) prompt text onto the
	// emitted PromptSubmitted event (on by default, opt-out honored). Only the
	// prompt is gated here; command strings, patch bodies, and tool output are
	// never decoded at all.
	CaptureContent bool
	// Finops, when non-nil, carries the usage numbers only the finops reader
	// extracted from the SessionEnd rollout jsonl.
	Finops *FinopsUsage
	// ThreadID is the ambient CODEX_THREAD_ID the hook process inherited; the id
	// of the thread this event came from, which the hook payload does not carry
	// (see HookEvent's doc comment). Structural identifiers, never content
	// (INV-2).
	ThreadID string
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

// FinopsUsage is the usage rollup the finops reader produces from a rollout:
// the four token counts, plus the model id; the ONE string the projection
// egresses (see usage.go's INV-2 note). No cost field at all: Codex's token
// path carries none, and cost is never derived here.
type FinopsUsage struct {
	Tokens *client.Tokens
	// Model is the last non-empty `turn_context.payload.model` in the rollout;
	// the model in effect when the session ended.
	Model string
}

// NewMapper returns a Mapper with production defaults (deterministic deriveID,
// time.Now clock).
func NewMapper(id Identity) Mapper {
	return Mapper{Identity: id, Now: time.Now}
}

// Map converts one hook payload into a normalized DevEvent. The bool reports
// whether an event should be emitted at all: false when the payload is
// unusable (no session id, or no valid developer DID); the caller drops it
// fail-open (INV-3), never blocking the tool call.
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
		SessionID:     e.SessionID, // the root/continuity id; correct under forks too (E8-S4)
		DeveloperDID:  m.Identity.DeveloperDID,
		Timestamp:     ts,
	}

	switch hook {
	case HookSessionStart:
		ev.EventType = client.EventSessionStarted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = sessionStartMetadata(e)
		if m.Posture != nil {
			ev.Metadata["posture"] = m.Posture.Metadata()
		}

	case HookUserPromptSubmit:
		ev.EventType = client.EventPromptSubmitted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = compact(map[string]any{"permission_mode": enumOr(e.PermissionMode, permissionModes)})
		// Off ⇒ Content stays nil and the prompt never egresses (Emit would strip it
		// anyway).
		if m.CaptureContent && e.Prompt != "" {
			ev.Content = &client.Content{Prompt: e.Prompt}
		}

	case HookPreToolUse:
		ev.EventType = client.EventToolCall
		ev.StartedAt = ts
		ev.Tool, ev.Span = mapTool(e, "started")
		ev.Metadata = toolMetadata(e)

	case HookPostToolUse:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)

	case HookSessionEnd:
		ev.EventType = client.EventSessionEnded
		ev.EndedAt = ts
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = compact(map[string]any{"reason": enumOr(e.Reason, reasonValues)})
		// Nil ⇒ nothing attached (finops off, or a session with no recorded token
		// counts).
		if m.Finops != nil {
			ev.Tokens = m.Finops.Tokens
			ev.Model = capStr(m.Finops.Model)
		}
		if m.Evidence != nil {
			ev.Metadata = mergeMetadata(ev.Metadata, m.Evidence.metadata())
		}

	default:
		return client.DevEvent{}, false
	}

	ev.Metadata = mergeMetadata(ev.Metadata, m.sessionTreeMetadata(e))

	ev.EventID = m.eventID(ev)
	return ev, true
}

// MapUsageRollup builds Codex's session-rollup `llm_completion` activity pair
// : the same wire carrier and the same activity_output shape Claude Code's
// per-turn pairs use, at the granularity Codex's wired hook surface offers.
func (m Mapper) MapUsageRollup(e *HookEvent) (started, completed client.DevEvent, ok bool) {
	if e == nil || e.SessionID == "" || m.Finops == nil || m.Finops.Tokens == nil {
		return client.DevEvent{}, client.DevEvent{}, false
	}
	if !strings.HasPrefix(m.Identity.DeveloperDID, "did:aip:") {
		return client.DevEvent{}, client.DevEvent{}, false
	}

	ts := m.clock().UTC().Format(time.RFC3339Nano)
	base := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		SessionID:     e.SessionID,
		DeveloperDID:  m.Identity.DeveloperDID,
		Tool:          client.Tool{Name: agentToolName, Kind: client.ToolShell},
		SessionRollup: true,
	}

	started = base
	started.EventType = client.EventTurnStarted
	started.Timestamp = ts
	started.Metadata = compact(map[string]any{"usage_scope": "session"})
	started.EventID = m.eventID(started)

	completed = base
	completed.EventType = client.EventTurnCompleted
	completed.Timestamp = ts
	completed.EndedAt = ts
	completed.Tokens = m.Finops.Tokens
	completed.Model = capStr(m.Finops.Model)
	completed.Metadata = compact(map[string]any{"usage_scope": "session"})
	completed.EventID = m.eventID(completed)

	return started, completed, true
}

func (m Mapper) sessionTreeMetadata(e *HookEvent) map[string]any {
	if m.ThreadID == "" || m.ThreadID == e.SessionID {
		return nil
	}
	return map[string]any{
		"thread_id":       capStr(m.ThreadID),
		"root_session_id": capStr(e.SessionID),
	}
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

// toolMetadata identifiers only, never content (INV-2);
// tool_input/tool_response are not represented.
func toolMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
		"tool_use_id":     capStr(e.ToolUseID),
		"turn_id":         capStr(e.TurnID),
	})
}

//   - Span.InvocationID = tool_use_id.
//   - Span.OperationID = what is being done, identical across a retry.
//     Activity_id derives from it, and activity_id is the approval key plus
//     the scope of both of core's bypass grants.
func mapTool(e *HookEvent, stage string) (client.Tool, *client.Span) {
	kind, sem, fileOp, mcpServer, function := classifyTool(e.ToolName)

	tool := client.Tool{Name: capStr(e.ToolName), Kind: kind}
	if kind == client.ToolMCP {
		tool.MCPServer = capStr(mcpServer)
	}

	span := &client.Span{SemanticType: sem, Stage: stage}
	if kind == client.ToolMCP {
		span.MCPServer = capStr(mcpServer)
		span.Function = capStr(function)
	}
	span.InvocationID = capStr(e.ToolUseID)
	span.OperationID = operationID(kind, e)
	if kind == client.ToolFile {
		span.FileOp = fileOp
	}
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

func classifyTool(name string) (kind client.ToolKind, sem, fileOp, mcpServer, function string) {
	if strings.HasPrefix(name, "mcp__") {
		server, fn := splitMCPName(name)
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

var builtinTools = map[string]toolClass{
	"Bash":        {client.ToolShell, "internal", ""},
	"apply_patch": {client.ToolFile, "file_write", "edit"},
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
		"cwd":             capStr(e.Cwd),                  // structural (blessed by conformance testdata); not content
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
	})
}

const maxIdentLen = 512

// A value outside its set is dropped (never egressed verbatim), keeping
// metadata clean.
var (
	sourceValues    = map[string]bool{"startup": true, "resume": true, "clear": true, "compact": true}
	reasonValues    = map[string]bool{"other": true}
	permissionModes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "dontAsk": true, "bypassPermissions": true}
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
	if ev.Span != nil {
		b.WriteByte(sep)
		b.WriteString(ev.Span.FilePath)
		b.WriteByte(sep)
		b.WriteString(ev.Span.Function) // the MCP function name
		b.WriteByte(sep)
		b.WriteString(ev.Span.InvocationID)
	}
	if tid, ok := ev.Metadata["thread_id"].(string); ok && tid != "" {
		b.WriteByte(sep)
		b.WriteString(tid)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "cdx-" + hex.EncodeToString(sum[:])
}

// randomID is a source of filesystem-unique suffixes for the spool (rotate /
// recovery / reclaim file names). It is NOT the event idempotency id (that is
// deriveID); it must stay random so concurrent drains never collide on a name.
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "sfx-fallback"
	}
	return hex.EncodeToString(b[:])
}
