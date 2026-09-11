package claudecode

import (
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

type enforceTarget struct {
	id     Identity
	mapper Mapper
	ev     *HookEvent
}

func (t enforceTarget) SessionID() string          { return t.ev.SessionID }
func (t enforceTarget) ToolName() string           { return t.ev.ToolName }
func (t enforceTarget) ToolInput() json.RawMessage { return t.ev.ToolInput }
func (t enforceTarget) HighRisk() bool             { return isHighRiskClass(t.ev.ToolName) }

func (t enforceTarget) DecisionRequest(localRedaction bool) decision.DecisionRequest {
	return buildDecisionRequest(t.id, t.ev, localRedaction)
}

// DevEvent maps the call for the inline evaluation and attaches the content
// the server needs to judge it. Redacted is the default for every class;
// verbatim is the exception, only where a recorded decision says so -- see
// the shell-and-MCP carve-out in docs/data-and-privacy.md (§What an enforced
// call sends; deliberately no line number, the bullet moves):
//   - A shell call named in builtinTools carries only its `command` field,
//     verbatim: a policy deciding whether a command is dangerous has to see
//     the command that will actually run. Membership in the table is part of
//     the test, not decoration -- classifyTool's default is shell/"internal",
//     so testing the kind alone would put every name the provider adds next,
//     and every typo, on the verbatim path by fallthrough.
//   - An MCP call carries its whole tool_input verbatim; same rationale, same
//     carve-out.
//   - A file write carries the redacted body, rebuilt through the same
//     RedactToolInput the local decider already redacted with (E8: the
//     enforce copy is the same bytes the rewrite put on disk), and then the
//     whole rebuilt object is scanned: the rebuild preserves byte-for-byte
//     every field the decider was never handed.
//   - Everything else -- a subagent spawn's prompt (Agent/ToolSearch), a file
//     read's arguments, a glob/grep pattern, an unrecognized tool name, and
//     any future builtin with no override -- keeps Map's own redacted Content.
//
// Map runs here with CaptureContent forced on, so the shape of the enforce
// copy does not depend on the target's own flag: three tests construct this
// target with CaptureContent at its zero value, and the escalation test pins
// that the enforce copy equals Map's output computed as if capture were on.
// It is skipped only when an override already produced the content, because
// Map's redaction pass is the expensive half of this blocking hook and an
// override overwrites its result.
// Whether that content then LEAVES the machine is decided once, at the client:
// Emit strips Content when capture is off (client.go's stripContent; C19).
//
// That strip covers every source of the setting, the managed layer included.
// It did not always: ResolveCredentials resolved the posture by hand from the
// user file plus the environment and never consulted the managed layer, so a
// *locked* `content_capture:false` stopped the observe copy (via
// ResolveContentCapture) and let this one through. Both now resolve through
// ResolveContentCapture, so the two cannot disagree about a lock.
func (t enforceTarget) DevEvent(redacted *client.Content) (client.DevEvent, bool) {
	kind, sem, _, _, _ := classifyTool(t.ev.ToolName)
	override := t.overrideContent(kind, sem, redacted)

	m := t.mapper
	m.CaptureContent = override == ""
	ev, ok := m.Map(HookPreToolUse, t.ev)
	if !ok {
		return ev, false
	}
	if override != "" {
		ev.Content = &client.Content{ToolInput: override}
		return ev, true
	}
	// No override: Map's redacted Content (Agent, ToolSearch, Read, Glob, ...).
	// It is bounded here and not upstream because Mapper.redact is the identity
	// function when no redactor is wired -- with secret_detection off, nothing
	// else on this arm applies MaxRedactBody at all.
	if ev.Content != nil {
		ev.Content.ToolInput = bounded(ev.Content.ToolInput)
	}
	return ev, true
}

// overrideContent returns the content for a class with a recorded carve-out,
// or "" for the redacted default. It reads only the hook event, never Map's
// output, so the caller can decide whether Map needs to build content at all.
func (t enforceTarget) overrideContent(kind client.ToolKind, sem string, redacted *client.Content) string {
	switch {
	case shellCarveOut(t.ev.ToolName, kind, sem):
		// Verbatim by decision (the section named above). A subagent spawn
		// (Agent/ToolSearch) is shell-KINDED but "llm_tool_call"-semantic, so
		// it is excluded here and falls through to the redacted default.
		return bounded(t.ev.command())
	case kind == client.ToolMCP:
		// Verbatim by decision (same rationale, same doc).
		return bounded(string(t.ev.ToolInput))
	case isFileSemantic(sem) && redacted != nil && redacted.FileText != "":
		// E8: the enforce copy is the same bytes the rewrite put on disk.
		// The rebuild swaps only the content field, so every other field is
		// still the original -- an Edit's old_string among them, a full copy
		// of the text being replaced that the decider never saw, because
		// fileText reads content/new_string and buildDecisionRequest puts only
		// that on the request. Scan the whole rebuilt object, the same pass
		// the observe copy runs over tool_input, so the two copies agree on
		// what redaction covers. Re-scanning the body the decider already
		// redacted is a no-op: the assignment rule and the entropy pass both
		// skip a value containing OPENBOX_REDACTED, so the bytes E8 is about
		// are unchanged. This is the egress copy only; the disk rewrite goes
		// through ApplyInputRedaction, which must keep old_string verbatim or
		// the Edit stops matching the file.
		rebuilt := hookflow.RedactToolInput(t.ev.ToolInput, redacted.FileText, contentFieldKeys)
		if len(rebuilt) == 0 {
			return ""
		}
		return bounded(t.mapper.redact(string(rebuilt)))
	}
	return ""
}

// shellCarveOut reports whether a call is the recorded verbatim shell case:
// shell-kinded, not a subagent spawn, and named in builtinTools. The table
// membership is the "recorded" half -- without it classifyTool's shell default
// makes the carve-out a fallthrough that any unknown name lands in.
func shellCarveOut(name string, kind client.ToolKind, sem string) bool {
	if kind != client.ToolShell || sem == "llm_tool_call" {
		return false
	}
	_, recorded := builtinTools[name]
	return recorded
}

// bounded caps content at MaxRedactBody, the first of the two bounds every
// egressing body passes (the client's capBody is the second). Every arm needs
// it: a verbatim override never reaches RedactText, and the rebuilt override
// and the redacted default reach it only when a redactor is wired. For the
// file arm it runs after the rebuild and its scan, so a placeholder that grew
// past the bound while replacing a shorter secret is still caught.
func bounded(s string) string {
	return hookflow.TruncateBytes(s, hookflow.MaxRedactBody)
}

// toolInputExtract is the raw-extraction step Map's observe path uses
// (mapper.go) to build every tool_input body it attaches: the shell command
// alone for a real shell call, the whole tool_input otherwise. Its result is
// never used unredacted -- both mapper.go call sites run it through m.redact
// before attaching it, so a verbatim shell arm here is not a verbatim result
// on the wire. DevEvent no longer calls it: the enforce copy has its own
// carve-out table, which is an allowlist rather than this kind test.
func toolInputExtract(e *HookEvent) string {
	kind, sem, _, _, _ := classifyTool(e.ToolName)
	// A subagent spawn (Agent/ToolSearch) is shell-KINDED, so the local enforce
	// gate is unchanged (insight 6), but its semantic is "llm_tool_call", not a
	// real shell command: e.command() would unmarshal it against {command} and
	// yield "". Carry the whole tool_input instead (owner ruling 2) -- a thin
	// {subagent_type} alone would be a judgement about a spawn the judge cannot
	// see (insight 3).
	if kind == client.ToolShell && sem != "llm_tool_call" {
		return e.command()
	}
	return string(e.ToolInput)
}

var _ hookflow.EnforceTarget = enforceTarget{}
