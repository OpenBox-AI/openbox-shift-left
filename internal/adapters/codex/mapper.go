package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
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
	// RedactContent redacts a content body for secrets before it is attached to
	// an event. Nil ⇒ identity, which is the honest `secret_detection:false`
	// case: the text egresses unredacted (that decision says so rather than
	// hiding it).
	RedactContent func(string) string
	// Finops, when non-nil, carries the usage numbers only the finops reader
	// extracted from the SessionEnd rollout jsonl.
	Finops *FinopsUsage
	// ThreadID is the ambient CODEX_THREAD_ID the hook process inherited; the id
	// of the thread this event came from, which the hook payload does not carry
	// (see HookEvent's doc comment). Structural identifiers, never content
	// (INV-2).
	ThreadID string
	// Posture, when non-nil, is the session's effective posture, attached
	// to the SessionStarted event's metadata only.
	Posture *devconfig.Posture
	// Evidence, when non-nil, records how much of this session's telemetry is
	// known to be undelivered at session end.
	Evidence *EvidenceState
}

// EvidenceState is a session's telemetry completeness. An alias, not a copy.
type EvidenceState = hookflow.EvidenceState

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
		SessionID:     e.SessionID, // the root/continuity id; correct under forks too
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
		ev.Metadata = hookflow.Compact(map[string]any{"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes)})
		// Off ⇒ Content stays nil and the prompt never egresses (Emit would strip it
		// anyway). The capture gate is checked first, so capture-off never builds a
		// redactor; redaction happens before attachment, which is the only
		// in-transit control there is.
		if m.CaptureContent && e.Prompt != "" {
			ev.Content = &client.Content{Prompt: m.redact(e.Prompt)}
		}

	case HookPreToolUse:
		ev.EventType = client.EventToolCall
		ev.StartedAt = ts
		ev.Tool, ev.Span = mapTool(e, "started")
		ev.Metadata = toolMetadata(e)

	case HookPermissionRequest:
		// A signal, not an activity: the escalation is not a second execution of
		// the tool, and Codex gives it no tool_use_id to pair with, so it can
		// never become half of an activity pair.
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

	case HookSubagentStart:
		ev.EventType = client.EventSubagentStarted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = hookflow.Compact(map[string]any{
			"agent_id":   capStr(e.AgentID),
			"agent_type": capStr(e.AgentType),
		})

	case HookPreCompact, HookPostCompact:
		if hook == HookPreCompact {
			ev.EventType = client.EventPreCompact
		} else {
			ev.EventType = client.EventPostCompact
		}
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = hookflow.Compact(map[string]any{"trigger": hookflow.EnumOr(e.Trigger, compactTriggers)})

	case HookStop, HookSubagentStop:
		// Deliberately (zero, false): a turn end is not a signal. It is emitted as
		// an llm_completion activity PAIR by emitTurn/MapTurn instead, which is the
		// same shape Claude Code uses and the reason no EventSubagentStopped exists
		// in the shared vocabulary.
		return client.DevEvent{}, false

	case HookPostToolUse:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)

	case HookSessionEnd:
		ev.EventType = client.EventSessionEnded
		ev.EndedAt = ts
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = hookflow.Compact(map[string]any{"reason": hookflow.EnumOr(e.Reason, reasonValues)})
		// Nil ⇒ nothing attached (finops off, or a session with no recorded token
		// counts).
		if m.Finops != nil {
			ev.Tokens = m.Finops.Tokens
			ev.Model = capStr(m.Finops.Model)
		}
		if m.Evidence != nil {
			ev.Metadata = mergeMetadata(ev.Metadata, m.Evidence.Metadata())
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
	started.Metadata = hookflow.Compact(map[string]any{"usage_scope": "session"})
	started.EventID = m.eventID(started)

	completed = base
	completed.EventType = client.EventTurnCompleted
	completed.Timestamp = ts
	completed.EndedAt = ts
	completed.Tokens = m.Finops.Tokens
	completed.Model = capStr(m.Finops.Model)
	completed.Metadata = hookflow.Compact(map[string]any{"usage_scope": "session"})
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
	return hookflow.Compact(map[string]any{
		"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes),
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

var builtinTools = map[string]toolClass{
	"Bash":        {client.ToolShell, "internal", ""},
	"apply_patch": {client.ToolFile, "file_write", "edit"},
}

func sessionStartMetadata(e *HookEvent) map[string]any {
	return hookflow.Compact(map[string]any{
		"provider":        provider,
		"source":          hookflow.EnumOr(e.Source, sourceValues), // startup|resume|clear|compact
		"model":           capStr(e.Model),                         // free-form model id → bounded
		"cwd":             capStr(e.Cwd),                           // structural (blessed by conformance testdata); not content
		"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes),
	})
}

const maxIdentLen = 512

// A value outside its set is dropped (never egressed verbatim), keeping
// metadata clean.
var (
	sourceValues    = map[string]bool{"startup": true, "resume": true, "clear": true, "compact": true}
	reasonValues    = map[string]bool{"other": true}
	permissionModes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "dontAsk": true, "bypassPermissions": true}
	// compactTriggers is the closed enum the Pre/PostCompact schemas declare.
	compactTriggers = map[string]bool{"manual": true, "auto": true}
)

// capStr delegates so the adapters and the engine cap an identifier the same
// way; maxIdentLen stays declared here because tests read it.
func capStr(s string) string { return hookflow.CapIdent(s) }

func (m Mapper) clock() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// redact runs a content body through the secret detector before it is attached.
// It lives on the Mapper rather than at the call site so every caller of Map
// inherits it: the enforce gate re-maps the same hook event for its own
// DecisionRequest copy, and a call-site redactor would leave that copy raw.
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

// MapTurn builds one turn's llm_completion activity pair.
//
// SessionRollup is deliberately NEVER set here. That single field is what routes
// an id to the `<session>:usage:rollup` namespace; leaving it false lets
// client/payload.go derive `<session>:turn:N` (or `<session>:agent:<id>:turn:N`
// for a sidechain), which is what keeps per-turn rows from colliding with the
// SessionEnd rollup.
func (m Mapper) MapTurn(e *HookEvent, w turnWindow, index int) (started, completed client.DevEvent, ok bool) {
	if e == nil || e.SessionID == "" || !w.HasUsage {
		return client.DevEvent{}, client.DevEvent{}, false
	}
	if !strings.HasPrefix(m.Identity.DeveloperDID, "did:aip:") {
		return client.DevEvent{}, client.DevEvent{}, false
	}

	ts := m.clock().UTC().Format(time.RFC3339Nano)
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
	started.Timestamp = ts
	started.StartedAt = ts
	started.Metadata = mergeMetadata(
		hookflow.Compact(map[string]any{"turn_index": turnIndex}),
		m.sessionTreeMetadata(e))
	started.EventID = m.eventID(started)

	completed = base
	completed.EventType = client.EventTurnCompleted
	completed.Timestamp = ts
	completed.EndedAt = ts
	completed.Tokens = w.tokens()
	completed.Model = capStr(w.Model)
	completed.Metadata = mergeMetadata(
		hookflow.Compact(map[string]any{"turn_index": turnIndex}),
		m.sessionTreeMetadata(e))
	// Content rides the COMPLETED half only: a turn's input is the prompt, which
	// already ships on PromptSubmitted under the same gate. Redaction happens here
	// rather than at the call site so a second caller of MapTurn inherits it.
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
	completed.EventID = m.eventID(completed)

	return started, completed, true
}
