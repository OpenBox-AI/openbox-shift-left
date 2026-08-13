package claudecode

import (
	"encoding/json"
	"fmt"
	"io"
)

// HookName is the Claude Code hook event this adapter reacts to. It is passed to
// the hook binary as an argv subcommand (deterministic, independent of the
// payload) and cross-checked against the payload's hook_event_name when present.
type HookName string

const (
	HookSessionStart     HookName = "SessionStart"
	HookUserPromptSubmit HookName = "UserPromptSubmit"
	HookPreToolUse       HookName = "PreToolUse"
	HookPostToolUse      HookName = "PostToolUse"
	HookSessionEnd       HookName = "SessionEnd"
	// HookStop / HookSubagentStop are the turn boundaries (ADR-0014). They are
	// the only per-turn signal the hook surface offers, and their value is
	// purely that they FIRE: the token numbers come from the transcript, not
	// from the payload, which carries only content fields this adapter refuses
	// to bind.
	//
	// Both can block a session via `decision: "block"`, so both are treated as
	// stdout-forbidden on every path (INV-3) — see RunHook.
	HookStop         HookName = "Stop"
	HookSubagentStop HookName = "SubagentStop"

	// HookPostToolUseFailure is the other half of PostToolUse. Claude Code
	// documents the pair as mutually exclusive — "Run after successful tool" vs
	// "Run after tool fails" — and a probe of 2.1.229 confirmed it: a failing
	// Bash fired this hook and no PostToolUse at all
	// (plans/reports/probe-260813-2329-claude-code-hook-surface.md).
	//
	// That exclusivity is what makes the outcome structural rather than inferred,
	// and it is the whole basis for status on a tool result (ADR-0018). If a
	// future version fired both, one failed call would produce two completed-side
	// events sharing an activity_id — a success and a failure counted against one
	// total — and SUCCESS% would be corrupt rather than merely wrong. That is a
	// stop-and-replan signal, not something to paper over.
	HookPostToolUseFailure HookName = "PostToolUseFailure"

	// HookSubagentStart is the opening boundary SubagentStop already had a close
	// for. Until now a subagent was visible only indirectly, through the agent_id
	// riding its tool events, so a subagent that spawned and did nothing left no
	// trace at all.
	HookSubagentStart HookName = "SubagentStart"

	// HookPermissionDenied fires "after auto mode classifier denies a tool call"
	// (2.1.229). Note the narrowness: a `permissions.deny` rule denies WITHOUT
	// firing it (verified — the probe's deny-rule run produced PreToolUse only),
	// so absence of this event is not evidence that nothing was denied.
	HookPermissionDenied HookName = "PermissionDenied"

	// HookStopFailure fires when a turn ends in a provider-side error instead of
	// an assistant message — rate limits, billing, auth, overload. Its payload is
	// the only structural surface for "the model could not answer", which is
	// otherwise indistinguishable from a quiet session.
	//
	// Claude Code ignores this hook's output and exit code entirely; the
	// stdout-forbidden discipline still applies uniformly (INV-3).
	HookStopFailure HookName = "StopFailure"
)

// hookNames is the set the plugin wires (capabilities.go / plugin/hooks.json).
var hookNames = map[HookName]bool{
	HookSessionStart:       true,
	HookUserPromptSubmit:   true,
	HookPreToolUse:         true,
	HookPostToolUse:        true,
	HookPostToolUseFailure: true,
	HookSessionEnd:         true,
	HookStop:               true,
	HookSubagentStop:       true,
	HookSubagentStart:      true,
	HookPermissionDenied:   true,
	HookStopFailure:        true,
}

// ParseHookName validates a raw argv value as a known hook name.
func ParseHookName(s string) (HookName, error) {
	h := HookName(s)
	if !hookNames[h] {
		return "", fmt.Errorf("unknown Claude Code hook %q", s)
	}
	return h, nil
}

// HookEvent is the subset of a Claude Code hook's stdin JSON this adapter
// reads.
//
// It captures only non-content, structural fields (INV-2) — session id,
// working directory, tool identity, lifecycle enums — with one deliberate,
// gated exception: Prompt (the UserPromptSubmit text). Command strings,
// file contents, and tool output remain intentionally not decoded, so they
// cannot leak into an emitted event even by accident. Prompt is decoded but
// is copied onto an event only under the content-capture opt-in
// (Mapper.CaptureContent) — with capture off it is inert, exactly like the
// structural-only fields. Unknown/extra fields are ignored
// (forward-compatible with Claude Code drift).
type HookEvent struct {
	// Common (present on every hook payload).
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`

	// TranscriptPath is the filesystem path to this session's JSONL
	// transcript. It is a structural locator (like Cwd), not content —
	// INV-2 permits it. The file it points at is content-bearing, so it is
	// opened only on the turn boundaries (Stop/SubagentStop) and SessionEnd,
	// only when the finops gate is set (ResolveFinops — default ON as of
	// ADR-0014, opt-OUT via finops:false / OPENBOX_FINOPS=0), and even then
	// only an allowlist projection touches it, extracting the four token
	// counts plus the model id. With finops off this field is decoded but
	// never dereferenced.
	TranscriptPath string `json:"transcript_path"`

	// SessionStart.
	Source string `json:"source"` // startup|resume|clear|compact
	Model  string `json:"model"`

	// PreToolUse / PostToolUse.
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"` // decoded only for a structural file_path

	// ToolUseID pairs a PreToolUse with its PostToolUse. Claude Code carries
	// it on both, so it replaces the heuristic (session, tool, locator)
	// pairing for non-MCP tools — two identical sequential Bash calls no
	// longer collide onto one span (see mapper.mapTool). Structural
	// identifier, never content (INV-2). Absent on older Claude Code
	// versions, which fall back to the heuristic derivation.
	ToolUseID string `json:"tool_use_id"`

	// AgentID / AgentType identify the subagent an event occurred inside.
	// They ride *every* hook payload fired within a subagent (not only the
	// Subagent* hooks), so a session's subagent tree is reconstructable from
	// the tool events alone: events sharing an AgentID belong to one subagent
	// and AgentType names its kind. Empty on the main agent's own events.
	// Structural identifiers, never content (INV-2).
	//
	// Verified against the installed claude 2.1.220 binary, which documents
	// agent_id as: "Subagent identifier. Present only when the hook fires
	// from within a subagent (e.g., a tool called by an AgentTool worker).
	// Absent for the main thread, even in --agent sessions. Use this field
	// (not agent_type) to distinguish subagent calls from main-thread calls."
	// Note parent_agent_id is deliberately absent here: it exists only as an
	// OTel span attribute on claude_code.subagent.spawn, not as a hook
	// payload field, so the tree is flat-by-agent_id rather than parented.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`

	// SessionEnd.
	Reason string `json:"reason"`

	// IsInterrupt separates "the user cancelled this" from "the tool failed"
	// on PostToolUseFailure. Both are status:"failed" — the call did not
	// complete either way — but without this an interrupted session reads as a
	// session full of broken tools, which is the wrong story to tell an
	// operator looking at a red Tool Health panel.
	//
	// A *bool, so absent stays absent: older versions and the ordinary hooks
	// omit the key, and false ("a real failure") is a different statement from
	// "not reported". Same lesson as Enforce in ADR-0016.
	//
	// PostToolUseFailure's OTHER field, `error`, is free text a tool wrote and
	// is deliberately unbound here — ADR-0019 P1 owns it.
	IsInterrupt *bool `json:"is_interrupt"`

	// ErrorType is StopFailure's error class — a closed provider enum, verified
	// against the 2.1.229 schema: authentication_failed, oauth_org_not_allowed,
	// billing_error, rate_limit, overloaded, invalid_request, model_not_found,
	// server_error, max_output_tokens, unknown.
	//
	// CAUTION, and the reason this field is named for its type rather than its
	// JSON key: `error` is ALSO the key PostToolUseFailure uses for free-text
	// tool error output, so this binding decodes that string too. It is safe
	// only because of how it is consumed — the sole reader is
	// enumOr(e.ErrorType, apiErrorTypes) on the StopFailure arm, and enumOr
	// returns "" for anything outside the ten values, so a free-text error has
	// no path to an emitted event. That is an allowlist, not an impossibility
	// (the ADR-0014 distinction), so it is pinned by a test rather than left to
	// this comment: TestMap_FreeTextErrorNeverEgresses.
	//
	// StopFailure's `error_details` and `last_assistant_message` are NOT bound.
	ErrorType string `json:"error"`

	// LastAssistantMessage is the text of the assistant message that closed this
	// turn (Stop / SubagentStop). It is CONTENT, and it is the one field of
	// these payloads this adapter binds.
	//
	// This comment used to say the opposite — that Stop and SubagentStop
	// "deliberately bind NOTHING of their own", and that the absence was the
	// safeguard. ADR-0018 changed that for this ONE field, and the honest
	// version of the argument is:
	//
	//   - It is decoded here and copied onto an event only under
	//     Mapper.CaptureContent, then redacted for secrets BEFORE attachment,
	//     then capped at 64KB by the client. With capture off it is inert,
	//     exactly like Prompt.
	//   - The safeguard is no longer structural for this field. It is a gate
	//     plus a redaction plus a cap, each of which can be got wrong, which is
	//     why each is asserted on the outbound bytes rather than trusted.
	//   - With `secret_detection:false` the text egresses UNREDACTED. Stated,
	//     not mitigated.
	//
	// Why this field and not the transcript: the provider itself recommends it
	// ("Avoids the need to read and parse the transcript file" — 2.1.229 schema
	// description), the transcript is written asynchronously and lags, and
	// sourcing it here leaves usage.go's transcript allowlist and its sentinel
	// TestFinops_NoContentOnWire completely untouched.
	//
	// Still unbound, deliberately: `error_details`, `background_tasks`,
	// `session_crons`. And `stop_reason` is not "deferred" — it does not exist
	// on this payload in 2.1.229 (verified: absent empirically and absent from
	// the binary's own input schema).
	//
	// What these hooks also use is above: SessionID, TranscriptPath, and
	// AgentID/AgentType for the subagent attribution.
	LastAssistantMessage string `json:"last_assistant_message"`

	// Prompt is the UserPromptSubmit prompt text — content (INV-2), not
	// structural. It is decoded here but is consumed only by the mapper
	// when content-capture is opted in (Mapper.CaptureContent); with
	// capture off it is never copied onto an emitted event, so it cannot
	// egress by accident. Redaction at source is a separate layer
	// ([EXT-guardrail-redaction]).
	Prompt string `json:"prompt"`
}

// maxHookPayload bounds the stdin read so a pathological payload can't exhaust
// memory. It is generous (a PreToolUse payload for a large file Write carries
// the whole file_text) so real large-write events are not dropped — only the
// structural fields are kept regardless of size. Beyond this, decode fails and
// the event is dropped fail-open.
const maxHookPayload = 32 << 20 // 32 MiB

// ParseHookEvent decodes a Claude Code hook payload from r over a bounded reader
// (maxHookPayload). Only structural fields are bound to HookEvent; prompt text,
// command strings, file contents, and tool output have no field here, so the
// decoder scans past them without capturing them. (tool_input is kept as raw
// bytes but is read only for a structural file_path and is never egressed.)
// A malformed or empty body is an error the caller treats fail-open (log to
// stderr, emit nothing, exit 0) — never a block (INV-3).
func ParseHookEvent(r io.Reader) (*HookEvent, error) {
	dec := json.NewDecoder(io.LimitReader(r, maxHookPayload))
	var ev HookEvent
	if err := dec.Decode(&ev); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("empty hook payload")
		}
		return nil, fmt.Errorf("parse hook payload: %w", err)
	}
	return &ev, nil
}

// filePath extracts a structural file path from tool_input for a file tool.
// Claude Code's file tools use "file_path" (Read/Write/Edit/MultiEdit) or
// "notebook_path" (notebook tools). It returns "" when neither is present. The
// path is a structural locator (INV-2 permits it — it is not file content).
func (e *HookEvent) filePath() string {
	if len(e.ToolInput) == 0 {
		return ""
	}
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if err := json.Unmarshal(e.ToolInput, &in); err != nil {
		return ""
	}
	if in.FilePath != "" {
		return in.FilePath
	}
	return in.NotebookPath
}

// subagentType extracts the agent kind a Task call spawns
// (tool_input.subagent_type: "code-reviewer", "general-purpose", …).
//
// Unlike command() and fileText(), this one DOES egress — which is why it is an
// identifier and not content. It names a configured agent kind, chosen from the
// installed set; it is not written by the model and carries nothing about the
// work. It is bounded by capStr at the call site like every other
// externally-influenced identifier.
//
// What is deliberately NOT read from the same tool_input: `prompt` and
// `description`, which are free text the model composed. Extracting those is
// ADR-0019 P1 territory, not a side effect of naming the agent.
//
// Returns "" for any other tool, or an absent/unparsable field.
func (e *HookEvent) subagentType() string {
	if len(e.ToolInput) == 0 {
		return ""
	}
	var in struct {
		SubagentType string `json:"subagent_type"`
	}
	if err := json.Unmarshal(e.ToolInput, &in); err != nil {
		return ""
	}
	return in.SubagentType
}

// command extracts the shell command string from a Bash tool_input.
//
// Local-only (INV-2): this is used solely to populate the enforce-mode
// decision.DecisionRequest, which is evaluated in-process on this machine —
// it never egresses to core and is never logged. It is the axis a local
// policy matches a dangerous command on (the canonical `rm -rf …` rule),
// analogous to the SDK sending activity_input to its own governance gate.
// The observe/telemetry egress path (Mapper) still never decodes the
// command, so the metadata-only-on-the-wire posture is unchanged; the
// command is read here only for the never-egressed local decision. Returns
// "" for a non-Bash tool or an absent/unparsable command.
func (e *HookEvent) command() string {
	if len(e.ToolInput) == 0 {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(e.ToolInput, &in); err != nil {
		return ""
	}
	return in.Command
}

// fileText extracts the file body a file-write tool carries — Claude
// Code's Write uses "content", Edit uses "new_string". This is content,
// not a structural locator. (MultiEdit nests bodies under
// edits[].new_string; extracting those is deferred — under-capturing is
// the INV-2-safe direction, since a missing body just means nothing local
// to redact.)
//
// Local-only (INV-2), and stricter than command(): it is read solely to
// populate the enforce-mode decision.DecisionRequest.Content when the org
// opted into content capture (ResolveContentCapture), so a
// redaction-capable local evaluator has the body to redact — the analog of
// the reference SDK sending the full activity_input to its gate. It is
// passed in-process to the decider and is never egressed to core and never
// logged; the observe/telemetry egress path (Mapper) still never decodes
// it, so the metadata-only-on-the-wire posture is unchanged. Returns "" for
// a non-file tool, an absent/unparsable body, or (via the caller's gate)
// when content capture is off.
func (e *HookEvent) fileText() string {
	if len(e.ToolInput) == 0 {
		return ""
	}
	var in struct {
		Content   string `json:"content"`    // Write
		NewString string `json:"new_string"` // Edit / MultiEdit
	}
	if err := json.Unmarshal(e.ToolInput, &in); err != nil {
		return ""
	}
	if in.Content != "" {
		return in.Content
	}
	return in.NewString
}
