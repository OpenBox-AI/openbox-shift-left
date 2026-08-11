package codex

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

// provider is the constant provider tag carried in metadata (MAPPING.md §2).
const provider = "codex"

// agentToolName is the tool.name used for session-lifecycle events, which
// are produced by the coding agent itself rather than a discrete tool —
// matching the conformance testdata convention and the CC adapter's
// precedent.
const agentToolName = provider

// Identity is the developer-agent identity the adapter emits under. It is
// minted by `openbox init` and resolved from the shared dev.json
// contract (creds.go / devconfig). Only the DID is needed to build events;
// the obx_ key and Ed25519 seed live in the client, never here (INV-1).
type Identity struct {
	DeveloperDID string // did:aip:<uuid>
}

// Mapper translates Codex hook payloads into normalized DevEvents. It is a
// pure function of (hook, payload, identity, clock, id source) — no I/O —
// so the whole acceptance surface (no-content, tool classification,
// tool_use_id pairing) is unit-testable. Structure mirrors the Claude Code
// mapper 1:1; deviations are Codex-surface-driven and commented inline.
type Mapper struct {
	Identity Identity
	Now      func() time.Time // injectable clock; defaults to time.Now
	// NewID, when non-nil, overrides the idempotency-id source (INV-5) —
	// used by tests to pin ids. When nil (the production default), the id
	// is derived deterministically from the event's own structural fields
	// (deriveID).
	NewID func() string
	// CaptureContent authorizes copying the (content) prompt text onto
	// the emitted PromptSubmitted event (on by default, opt-out honored).
	// Set from ResolveContentCapture() in RunHook — the same flag the
	// flush client's Emit uses to strip content, so capture and egress
	// agree. Only the prompt is gated here — command strings, patch
	// bodies, and tool output are never decoded at all.
	CaptureContent bool
	// Finops, when non-nil, carries the usage numbers only the finops
	// reader extracted from the SessionEnd rollout JSONL. Map copies them
	// onto the SessionEnded event only. nil (the default) ⇒ events carry
	// no tokens/cost. The Mapper itself does no file I/O: the
	// content-bearing rollout read + fail-open logging happen in RunHook
	// (which owns the logger), so this stays a pure mapping of its inputs
	// (like Now / NewID), preserving the INV-2 guarantee that Map never
	// touches content.
	Finops *FinopsUsage
	// ThreadID is the ambient CODEX_THREAD_ID the hook process inherited —
	// the id of the thread this event came from, which the hook payload does
	// not carry (see HookEvent's doc comment). RunHook reads the env and
	// passes it in so Map stays a pure function of its inputs, exactly as
	// Finops does for the rollout read.
	//
	// It is only *recorded* when it differs from the payload's session id,
	// i.e. under a forked thread: then the event stream is keyed by the root
	// session while the git trailer attributes commits by this thread id, and
	// metadata.thread_id/root_session_id is the join between them. For an
	// unforked run the two are equal and nothing is emitted, so today's wire
	// output is unchanged. Structural identifiers, never content (INV-2).
	ThreadID string
	// Posture, when non-nil, is the session's effective posture (E8-S5),
	// attached to the SessionStarted event's metadata only. RunHook resolves
	// it (config reads, a bundle hash, the freshness check) and passes it in,
	// so Map stays I/O-free — the same split as Finops. nil ⇒ no posture key,
	// which is what the enforce/observe tests and the conformance fixtures see.
	Posture *devconfig.Posture
	// Evidence, when non-nil, records how much of this session's telemetry is
	// known to be undelivered at session end (E8-S7). Attached to the
	// SessionEnded event's metadata only. RunHook counts the carry-over files
	// and passes the result in, keeping Map I/O-free.
	Evidence *EvidenceState
}

// EvidenceState is the completeness of a session's telemetry as the client can
// see it at session end.
//
// It describes the spool BEFORE this session's final flush: a non-zero
// Undelivered means an earlier flush failed and those events are waiting in
// carry-over files. It is not a claim that they are lost — a later flush
// re-sends them, and the server deduplicates — so "degraded" here means
// "incomplete as of now", and only exceeding the retry bound makes loss
// permanent (which the spool logs).
type EvidenceState struct {
	Undelivered int
}

// metadata renders the evidence state. The state string is always present so a
// reader can distinguish "complete" from "this client does not report it".
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

// FinopsUsage is the numbers-only usage rollup the finops reader produces
// from a rollout. It carries only Tokens/Cost value structs — no content,
// by construction (see usage.go). Cost is always nil for Codex (its token
// path carries no cost field).
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
// Map never copies content (prompt text under the default gate, command
// strings, patch bodies, tool output) into the event (INV-2). It carries
// only structural metadata: tool identity, pairing ids, and lifecycle
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

	// WorkspaceID is left empty so the client uses the developer DID as
	// core's workflow_id — stable per session regardless of which hook
	// fires (the CC adapter's rationale applies unchanged).
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		SessionID:     e.SessionID, // the root/continuity id — correct under forks too (E8-S4)
		DeveloperDID:  m.Identity.DeveloperDID,
		Timestamp:     ts,
	}

	switch hook {
	case HookSessionStart:
		ev.EventType = client.EventSessionStarted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = sessionStartMetadata(e)
		// Effective posture as evidence (E8-S5): structural booleans and
		// opaque ids only, so it is INV-1/INV-2 safe to egress.
		if m.Posture != nil {
			ev.Metadata["posture"] = m.Posture.Metadata()
		}

	case HookUserPromptSubmit:
		ev.EventType = client.EventPromptSubmitted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = compact(map[string]any{"permission_mode": enumOr(e.PermissionMode, permissionModes)})
		// The prompt is the signal's input and it is content, so it is
		// carried on ev.Content.Prompt only under content-capture (→
		// SignalReceived signal_args downstream, capped). Off ⇒ Content
		// stays nil and the prompt never egresses (Emit would strip it
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
		// Codex's SessionEnd payload carries reason (pinned to "other" by the
		// 0.145.0 schema) and neither model nor permission_mode — leaner than CC's.
		ev.Metadata = compact(map[string]any{"reason": enumOr(e.Reason, reasonValues)})
		// Attach the opt-in rollout usage rollup, if the finops reader
		// extracted any. Numbers only — Tokens carry no content (usage.go);
		// Cost is always nil for Codex. nil ⇒ nothing attached (finops off
		// or a session with no recorded token counts).
		if m.Finops != nil {
			ev.Tokens = m.Finops.Tokens
			ev.Cost = m.Finops.Cost
		}
		// Telemetry completeness as the client sees it (E8-S7).
		if m.Evidence != nil {
			ev.Metadata = mergeMetadata(ev.Metadata, m.Evidence.metadata())
		}

	default:
		return client.DevEvent{}, false
	}

	// Record the session-tree linkage on every event of a forked thread, before
	// the id derivation so a fork's events cannot collide with the root's.
	ev.Metadata = mergeMetadata(ev.Metadata, m.sessionTreeMetadata(e))

	// Derive the idempotency id LAST, from the fully-populated structural fields
	// (INV-5) — the switch's distinguishers (event_type, tool, pairing id) all
	// feed the derivation.
	ev.EventID = m.eventID(ev)
	return ev, true
}

// sessionTreeMetadata returns the fork linkage, or nil for the common case.
// Empty when the ambient thread id is absent (not a Codex-launched process) or
// equal to the session id (an unforked root thread) — so unforked runs emit
// byte-identical metadata to before this story.
func (m Mapper) sessionTreeMetadata(e *HookEvent) map[string]any {
	if m.ThreadID == "" || m.ThreadID == e.SessionID {
		return nil
	}
	return map[string]any{
		"thread_id":       capStr(m.ThreadID),
		"root_session_id": capStr(e.SessionID),
	}
}

// mergeMetadata folds src into dst (dst wins on collision), allocating only if
// there is something to add. Returns dst so it can be assigned in place.
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

// mapTool builds the Tool identity and the semantic Span for a
// Pre/PostToolUse event. stage is "started" (ToolCall) or "completed"
// (ToolResult).
//
// Two identities, deliberately separate — the same split the Claude Code
// adapter documents at length on its own mapTool, and for the same reason:
//
//   - Span.InvocationID = tool_use_id. Keys the cross-process duration
//     stash, so the completed hook recovers when the started one fired.
//   - Span.OperationID = what is being done, identical across a retry.
//     activity_id derives from it, and activity_id is the approval key
//     plus the scope of both of core's bypass grants.
//
// Both used to be tool_use_id, carried on span.function, so every retry
// became a different activity and an approval could never be consumed.
// Codex is affected identically to Claude Code — it mints a fresh
// tool_use_id per call too — even though the loop was first seen there.
//
// Per class: shell hashes the command; MCP hashes the argument shape
// beside the real function name (which stays wire data, mcp_tool);
// anything else falls back to the invocation, preserving today's
// granularity for classes that are never escalated and so can never hold
// an approval.
//
// Codex file spans carry no file_path — apply_patch's input is the patch
// body, which is content — so a file operation has no structural
// discriminator here and takes the fallback. That is honest rather than
// lossy: file classes are not gated on Codex either.
//
// Both fields are local: spooled so the enforce path and a later flush
// agree, never emitted as wire fields. Structural or hashed throughout,
// never content (INV-2).
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
	// No file_path: Codex tool_input carries no structural path (apply_patch's
	// input is the patch body — content), so file spans honestly omit it.
	return tool, span
}

// operationID derives the per-class operation discriminator described on
// mapTool. It reads the tool's own input, which stays local — only the hash
// reaches the id.
func operationID(kind client.ToolKind, e *HookEvent) string {
	switch kind {
	case client.ToolShell:
		// The command IS the operation. It falls through to the invocation only
		// for a degenerate call carrying no command, where collapsing every such
		// call onto one activity would over-grant.
		if op := client.OperationForCommand(e.command()); op != "" {
			return op
		}
	case client.ToolMCP:
		// The server and function are already structural fields of the key, so
		// the arguments are all that is left to distinguish. No fallback: an
		// empty argument set is a legitimate, stable identity, and falling back
		// to the invocation would leave a no-argument MCP call unable to survive
		// a retry.
		return client.OperationForArgs(e.ToolInput)
	}
	// No structural discriminator: fall back to the invocation. A gated class
	// must never reach here — see TestHighRiskClassesHaveAStableOperationID.
	return capStr(e.ToolUseID)
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
			// non-conformant (the contract requires it), so fall back to
			// the catch-all.
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
		"cwd":             capStr(e.Cwd),                  // structural (blessed by conformance testdata); not content
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
	})
}

// maxIdentLen bounds every externally-influenced identifier field before
// egress (the CC adapter's posture): a crafted payload or a malicious MCP
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
		b.WriteString(ev.Span.Function) // the MCP function name
		// The invocation id is what actually separates two same-instant calls of
		// the SAME tool. It used to arrive here via Function; splitting the
		// operation and invocation identities took it out, and without it those
		// two calls would derive one event_id — the idempotency key (INV-5) — so
		// the server would dedupe the second away as a replay of the first.
		b.WriteByte(sep)
		b.WriteString(ev.Span.InvocationID)
	}
	// Forked threads share the root's session id (E8-S4), so two threads can
	// otherwise produce identical structural fields. Present only under a fork,
	// so unforked ids are unchanged.
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
		// Non-fatal: a crypto/rand failure is astronomically unlikely; a colliding
		// suffix only risks losing the race for one orphaned spool file.
		return "sfx-fallback"
	}
	return hex.EncodeToString(b[:])
}
