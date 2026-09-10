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
// the shell-and-MCP carve-out in docs/data-and-privacy.md (§What a gated tool
// call sends; deliberately no line number, the bullet moves):
//   - A shell-KINDED call carries only its `command` field, verbatim: a policy
//     deciding whether a command is dangerous has to see the command that will
//     actually run. Note the arm is WIDER than "Bash": classifyTool's default
//     is shell/"internal", so any name absent from builtinTools lands here and
//     carries content only if its input has a `command` key. BashOutput and
//     KillShell take `bash_id`/`shell_id`, so in practice they carry nothing.
//   - An MCP call carries its whole tool_input verbatim; same rationale, same
//     carve-out.
//   - A file write carries the redacted body, rebuilt through the same
//     RedactToolInput the local decider already redacted with (E8: the
//     enforce copy is the same bytes the rewrite put on disk).
//   - Everything else -- a subagent spawn's prompt (Agent/ToolSearch), a file
//     read's arguments, a glob/grep pattern, and any future builtin with no
//     override below -- keeps Map's own redacted Content untouched.
//
// Map runs here with CaptureContent forced on, so the shape of the enforce
// copy does not depend on the target's own flag: three tests construct this
// target with CaptureContent at its zero value, and the escalation test pins
// that the enforce copy equals Map's output computed as if capture were on.
// Whether that content then LEAVES the machine is decided once, at the client:
// Emit strips Content when capture is off (client.go's stripContent; C19).
//
// That strip is honoured for the user-file and env sources ONLY. A *locked*
// managed `content_capture:false` is honoured by ResolveContentCapture (so the
// observe copy carries nothing) and ignored by ResolveCredentials, which reads
// the user file and env but never the managed layer -- so under an org lock
// this content still egresses. Pre-existing and not this function's to fix
// (devconfig.ResolveCredentials owns it); recorded here so the paragraph above
// is not read as a guarantee it does not give.
func (t enforceTarget) DevEvent(redacted *client.Content) (client.DevEvent, bool) {
	m := t.mapper
	m.CaptureContent = true
	ev, ok := m.Map(HookPreToolUse, t.ev)
	if !ok {
		return ev, false
	}

	kind, sem, _, _, _ := classifyTool(t.ev.ToolName)
	switch {
	case kind == client.ToolShell && sem != "llm_tool_call":
		// Verbatim by decision (data-and-privacy.md:362). A subagent spawn
		// (Agent/ToolSearch) is shell-KINDED but "llm_tool_call"-semantic, so
		// it is excluded here and falls through to the redacted default.
		if in := bounded(commandOf(t.ev.ToolInput, t.ev)); in != "" {
			ev.Content = &client.Content{ToolInput: in}
		}
	case kind == client.ToolMCP:
		// Verbatim by decision (same rationale, same doc).
		if in := bounded(string(t.ev.ToolInput)); in != "" {
			ev.Content = &client.Content{ToolInput: in}
		}
	case isFileSemantic(sem) && redacted != nil && redacted.FileText != "":
		// E8: the enforce copy is the same bytes the rewrite put on disk.
		if rebuilt := hookflow.RedactToolInput(t.ev.ToolInput, redacted.FileText, contentFieldKeys); len(rebuilt) > 0 {
			if in := bounded(string(rebuilt)); in != "" {
				ev.Content = &client.Content{ToolInput: in}
			}
		}
	}
	// No case matched above: keep Map's redacted Content untouched (Agent,
	// ToolSearch, Read, NotebookRead, Glob, Grep, ...).
	return ev, true
}

// bounded caps an override arm's content at MaxRedactBody. The default
// (redacted) arm needs no separate bound here: RedactText already
// bound-then-redacts (hookflow/enforce.go), but a verbatim or rebuilt
// override bypasses that, so it re-applies the same cap on its own content --
// after the rebuild for the file arm, so a placeholder that grew past the
// bound while replacing a shorter secret is still caught.
func bounded(s string) string {
	return hookflow.TruncateBytes(s, hookflow.MaxRedactBody)
}

// toolInputExtract is the raw-extraction step Map's observe path uses
// (mapper.go) to build every tool_input body it attaches: the shell command
// alone for a real shell call, the whole tool_input otherwise. Its result is
// never used unredacted -- both mapper.go call sites run it through m.redact
// before attaching it, so a verbatim default arm here is not a verbatim
// result on the wire. Kept as-is: DevEvent above no longer calls it for the
// enforce copy.
func toolInputExtract(e *HookEvent, redacted *client.Content) string {
	kind, sem, _, _, _ := classifyTool(e.ToolName)
	input := e.ToolInput
	if redacted != nil && redacted.FileText != "" {
		if rebuilt := hookflow.RedactToolInput(input, redacted.FileText, contentFieldKeys); len(rebuilt) > 0 {
			input = rebuilt
		}
	}
	// A subagent spawn (Agent/ToolSearch) is shell-KINDED, so the local enforce
	// gate is unchanged (insight 6), but its semantic is "llm_tool_call", not a
	// real shell command: e.command() would unmarshal it against {command} and
	// yield "". Carry the whole tool_input instead (owner ruling 2) -- a thin
	// {subagent_type} alone would be a judgement about a spawn the judge cannot
	// see (insight 3).
	if kind == client.ToolShell && sem != "llm_tool_call" {
		return commandOf(input, e)
	}
	return string(input)
}

func commandOf(input json.RawMessage, e *HookEvent) string {
	if len(input) == 0 || string(input) == string(e.ToolInput) {
		return e.command()
	}
	var obj struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &obj); err != nil {
		return e.command()
	}
	return obj.Command
}

var _ hookflow.EnforceTarget = enforceTarget{}
