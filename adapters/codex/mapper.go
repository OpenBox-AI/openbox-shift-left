package codex

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// provider is the constant provider tag carried in metadata (MAPPING.md §2).
const provider = "codex"

// agentToolName is the tool.name used for session-lifecycle events, which are
// produced by the coding agent itself rather than a discrete tool — matching
// the SL-1 conformance testdata convention and the CC adapter's precedent.
const agentToolName = provider

// Identity is the developer-agent identity the adapter emits under. It is
// minted by `openbox dev init` (STORY-SL-2) and resolved from the shared
// dev.json contract (creds.go / devconfig). Only the DID is needed to build
// events; the obx_ key and Ed25519 seed live in the client, never here (INV-1).
type Identity struct {
	DeveloperDID string // did:aip:<uuid>
}

// Mapper translates Codex hook payloads into normalized SL-1 DevEvents. It is a
// pure function of (hook, payload, identity, clock, id source) — no I/O — so
// the whole acceptance surface (SL3-SEC-3 no-content, tool classification,
// tool_use_id pairing) is unit-testable. Structure mirrors the Claude Code
// mapper 1:1; deviations are Codex-surface-driven and commented inline.
type Mapper struct {
	Identity Identity
	Now      func() time.Time // injectable clock; defaults to time.Now
	// NewID, when non-nil, OVERRIDES the idempotency-id source (INV-5) — used by
	// tests to pin ids. When nil (the production default), the id is DERIVED
	// deterministically from the event's own structural fields (deriveID).
	NewID func() string
	// CaptureContent authorizes copying the (content) prompt text onto the
	// emitted PromptSubmitted event (E7-S7 / OD4; ON by default as of
	// 2026-07-15, opt-out honored). Set from ResolveContentCapture() in RunHook —
	// the SAME flag the flush client's Emit uses to strip content, so capture
	// and egress agree. Only the prompt is gated here — command strings, patch
	// bodies, and tool output are never decoded at all (SL3-SEC-3).
	CaptureContent bool
	// Finops, when non-nil, carries the usage NUMBERS ONLY the finops reader
	// extracted from the SessionEnd rollout JSONL (STORY-SL7-C / SL-16 parity).
	// Map copies them onto the SessionEnded event only. nil (the default) ⇒ events
	// carry no tokens/cost — byte-identical to the pre-SL7-C output. The Mapper
	// itself does NO file I/O: the content-bearing rollout read + fail-open logging
	// happen in RunHook (which owns the logger), so this stays a pure mapping of
	// its inputs (like Now / NewID), preserving the INV-2 guarantee that Map never
	// touches content.
	Finops *FinopsUsage
}

// FinopsUsage is the numbers-only usage rollup the finops reader produces from a
// rollout (STORY-SL7-C). It carries only the SL-1 Tokens/Cost value structs — no
// content, by construction (see usage.go). Cost is always nil for Codex (its
// token path carries no cost field).
type FinopsUsage struct {
	Tokens *client.Tokens
	Cost   *client.Cost
}

// NewMapper returns a Mapper with production defaults (deterministic deriveID,
// time.Now clock).
func NewMapper(id Identity) Mapper {
	return Mapper{Identity: id, Now: time.Now}
}

// Map converts one hook payload into a normalized DevEvent. The bool reports
// whether an event should be emitted at all: false when the payload is unusable
// (no session id, or no valid developer DID) — the caller drops it fail-open
// (INV-3), never blocking the tool call.
//
// Map NEVER copies content (prompt text under the default gate, command
// strings, patch bodies, tool output) into the event (INV-2 / SL3-SEC-3). It
// carries only structural metadata: tool identity, pairing ids, and lifecycle
// enums.
func (m Mapper) Map(hook HookName, e *HookEvent) (client.DevEvent, bool) {
	if e == nil || e.SessionID == "" {
		return client.DevEvent{}, false
	}
	if !strings.HasPrefix(m.Identity.DeveloperDID, "did:aip:") {
		return client.DevEvent{}, false
	}

	now := m.clock()
	// RFC3339Nano: the sub-second precision is the per-event distinguisher
	// deriveID folds into the id (CC-adapter precedent; core parses it).
	ts := now.UTC().Format(time.RFC3339Nano)

	// WorkspaceID is left empty so the client uses the developer DID as core's
	// workflow_id — stable per session regardless of which hook fires (the CC
	// adapter's F4 rationale applies unchanged).
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		SessionID:     e.SessionID, // Codex session ≡ thread (addendum #2)
		DeveloperDID:  m.Identity.DeveloperDID,
		Timestamp:     ts,
	}

	switch hook {
	case HookSessionStart:
		ev.EventType = client.EventSessionStarted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = sessionStartMetadata(e)

	case HookUserPromptSubmit:
		ev.EventType = client.EventPromptSubmitted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = compact(map[string]any{"permission_mode": enumOr(e.PermissionMode, permissionModes)})
		// E7-S7 / OD4: the prompt IS the signal's input and it is CONTENT, so it is
		// carried on ev.Content.Prompt ONLY under content-capture (→ SignalReceived
		// signal_args downstream, capped). Off ⇒ Content stays nil and the prompt
		// never egresses (Emit would strip it anyway).
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
		// Codex's SessionEnd payload carries reason (pinned to "other" by the
		// 0.145.0 schema) and neither model nor permission_mode — leaner than CC's.
		ev.Metadata = compact(map[string]any{"reason": enumOr(e.Reason, reasonValues)})
		// STORY-SL7-C (SL-16 parity): attach the opt-in rollout usage rollup, if the
		// finops reader extracted any. Numbers only — Tokens carry no content
		// (usage.go); Cost is always nil for Codex. nil ⇒ nothing attached (finops
		// off or a session with no recorded token counts).
		if m.Finops != nil {
			ev.Tokens = m.Finops.Tokens
			ev.Cost = m.Finops.Cost
		}

	default:
		return client.DevEvent{}, false
	}

	// Derive the idempotency id LAST, from the fully-populated structural fields
	// (INV-5) — the switch's distinguishers (event_type, tool, pairing id) all
	// feed the derivation.
	ev.EventID = m.eventID(ev)
	return ev, true
}

// toolMetadata builds the Pre/PostToolUse metadata: the permission mode plus
// the structural Codex correlation ids (tool_use_id, turn_id). Identifiers
// only, never content (INV-2) — tool_input/tool_response are not represented.
func toolMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
		"tool_use_id":     capStr(e.ToolUseID),
		"turn_id":         capStr(e.TurnID),
	})
}

// mapTool builds the Tool identity and the semantic Span for a Pre/PostToolUse
// event. stage is "started" (ToolCall) or "completed" (ToolResult).
//
// tool_use_id pairing (AC-5): the client derives the wire span_id/activity_id
// shared by a call's started+completed spans from (session, tool.name,
// span.file_path, span.function) — client/payload.go activityPairKey. Codex
// gives us the per-invocation tool_use_id CC lacks, so for NON-MCP tools we
// carry it on span.function: that field feeds the pair key (making the derived
// ids exact per invocation — two identical sequential Bash calls no longer
// collide), feeds the E7-S8 duration-stash key, and — verified against
// client/payload.go — is NOT emitted on the wire for shell/file/tool hook
// types (hookSpanShape and structuralActivityInput read span.function for MCP
// only). For MCP tools span.function must stay the real MCP function name
// (it IS wire data: mcp_tool), so MCP pairing keeps the CC-parity fallback
// derivation; tool_use_id still rides metadata for audit. A structural
// identifier either way — never content (INV-2).
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
	} else if e.ToolUseID != "" {
		span.Function = capStr(e.ToolUseID) // pairing id channel — see doc comment
	}
	if kind == client.ToolFile {
		span.FileOp = fileOp
	}
	// No file_path: Codex tool_input carries no structural path (apply_patch's
	// input is the patch body — content), so file spans honestly omit it.
	return tool, span
}

// classifyTool maps a Codex hook tool_name to the provider-agnostic tool class
// and the intended openbox-core span semantic type. The literals are grounded
// in codex-rs @ rust-v0.145.0 `core/src/tools/hook_names.rs` +
// `core/src/tools/registry.rs` (recorded by TestClassifyTool_GroundedLiterals):
//
//   - "Bash" — yes, literally: HookToolName::bash() is "the hook identity
//     historically used for shell-like tools", serialized for the shell_command,
//     unified_exec, and sandboxing exec paths → shell/internal.
//   - "apply_patch" — HookToolName::apply_patch(); `Write`/`Edit` exist ONLY as
//     internal matcher aliases and are never serialized as tool_name →
//     file/file_write.
//   - "mcp__<server>__<tool>" — handlers/mcp.rs ensure_mcp_prefix →
//     mcp/mcp_tool_call.
//   - everything else (web_search, update_plan, view_image, spawn_agent, …) —
//     function_hook_tool_name serializes the flat tool name → the coarse
//     shell/internal catch-all.
//
// The real tool name always rides tool.name + metadata.tool_name, so no
// identity is lost to the 3-value kind enum (a tool name is an identifier, not
// content). semantic_type is an intent/hint core recomputes server-side.
func classifyTool(name string) (kind client.ToolKind, sem, fileOp, mcpServer, function string) {
	if strings.HasPrefix(name, "mcp__") {
		server, fn := splitMCPName(name)
		if server == "" {
			// Malformed MCP name: kind=mcp with an empty mcp_server is
			// non-conformant (SL-1 requires it), so fall back to the catch-all.
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

// builtinTools classifies Codex's built-in hook tool names (see classifyTool
// for the source grounding). Anything not listed falls through to
// shell/internal.
var builtinTools = map[string]toolClass{
	// Shell command execution (core has no shell semantic type → "internal").
	"Bash": {client.ToolShell, "internal", ""},
	// apply_patch is Codex's sole file-write tool; "edit" mirrors the CC
	// adapter's multi-file-edit file_operation value.
	"apply_patch": {client.ToolFile, "file_write", "edit"},
}

// splitMCPName parses Codex's mcp__<server>__<tool> naming into the server and
// tool (function) names. A malformed name yields the remainder as the server
// with an empty function.
func splitMCPName(name string) (server, function string) {
	rest := strings.TrimPrefix(name, "mcp__")
	parts := strings.SplitN(rest, "__", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

// sessionStartMetadata builds the SessionStarted metadata (MAPPING.md §2):
// provider + the structural session facts Codex exposes. No content.
func sessionStartMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"provider":        provider,
		"source":          enumOr(e.Source, sourceValues), // startup|resume|clear|compact
		"model":           capStr(e.Model),                // free-form model id → bounded
		"cwd":             capStr(e.Cwd),                  // structural (blessed by SL-1 testdata); not content
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
	})
}

// maxIdentLen bounds every externally-influenced identifier field before egress
// (the CC adapter's G_SEC F1 posture): a crafted payload or a malicious MCP
// server's tool name can't push an unbounded / content-shaped string into
// tool.name, span.function, span.mcp_server, or metadata ids.
const maxIdentLen = 512

// Known Codex lifecycle enum values, grounded in the 0.145.0 embedded hook
// schemas (session-start.command.input / session-end.command.input). A value
// outside its set is dropped (never egressed verbatim), keeping metadata clean.
var (
	sourceValues = map[string]bool{"startup": true, "resume": true, "clear": true, "compact": true}
	// The SessionEnd schema pins reason to the single const "other" today; kept
	// as a set so a future enum widening is a one-line change.
	reasonValues = map[string]bool{"other": true}
	// Codex reuses the Claude-Code-style permission modes on hook payloads
	// (embedded schema enum) — NOT the approval_policy values (untrusted/…).
	permissionModes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "dontAsk": true, "bypassPermissions": true}
)

// enumOr returns v when it is a recognized enum value, else "" (which compact
// then drops).
func enumOr(v string, allowed map[string]bool) string {
	if allowed[v] {
		return v
	}
	return ""
}

// capStr rune-safely bounds an identifier/path to maxIdentLen.
func capStr(s string) string {
	r := []rune(s)
	if len(r) <= maxIdentLen {
		return s
	}
	return string(r[:maxIdentLen])
}

// compact drops empty-string values so metadata carries only what is known.
// It always returns a non-nil map so lifecycle events keep a stable metadata
// object.
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

// eventID resolves the event's idempotency id (INV-5): the injected NewID when
// a caller/test pins one, otherwise the deterministic derivation.
func (m Mapper) eventID(ev client.DevEvent) string {
	if m.NewID != nil {
		return m.NewID()
	}
	return deriveID(ev)
}

// deriveID computes the deterministic, collision-safe idempotency id (INV-5)
// for an event as "cdx-" + sha256 over its structural fields — the same
// contract as the CC adapter's deriveID (same logical event ⇒ same id through
// spool→rotate→flush→recovery; two distinct events never collide), with the
// "cdx-" prefix namespacing this provider. For non-MCP tool events the pairing
// slot (span.function) carries the tool_use_id, so two same-instant identical
// tool calls are separated by a per-invocation id, not just the timestamp.
// INV-1: only non-secret structural fields feed the hash. INV-2: no
// prompt/command/patch/output text is ever hashed.
func deriveID(ev client.DevEvent) string {
	// 0x1f (unit separator) delimits fields so no concatenation of two events'
	// values can alias a third.
	const sep = 0x1f
	var b strings.Builder
	b.WriteString(ev.SessionID)
	b.WriteByte(sep)
	b.WriteString(string(ev.EventType))
	b.WriteByte(sep)
	b.WriteString(ev.Tool.Name)
	b.WriteByte(sep)
	b.WriteString(ev.Timestamp) // RFC3339Nano — the per-event distinguisher
	if ev.Span != nil {
		b.WriteByte(sep)
		b.WriteString(ev.Span.FilePath)
		b.WriteByte(sep)
		b.WriteString(ev.Span.Function) // tool_use_id (non-MCP) / MCP function
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
		// Non-fatal: a crypto/rand failure is astronomically unlikely; a colliding
		// suffix only risks losing the race for one orphaned spool file.
		return "sfx-fallback"
	}
	return hex.EncodeToString(b[:])
}
