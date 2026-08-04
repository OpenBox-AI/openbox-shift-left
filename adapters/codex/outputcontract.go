package codex

import (
	"encoding/json"
	"io"
	"log"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Decision literals Codex honours on a PreToolUse hook's stdout.
//
// The output enum is allow|deny|ask (schema.rs PreToolUsePermissionDecisionWire),
// but the runtime parser (hooks/src/engine/output_parser.rs) rejects `ask`,
// `allow` without updatedInput, and updatedInput without `allow`. A rejected
// output marks the hook Failed and discards the decision, which fails open. So
// the only usable levers are deny+reason and allow+updatedInput.
const (
	codexDecisionDeny  = "deny"
	codexDecisionAllow = "allow"
)

// contentFieldKeys is the tool_input field that may hold a redactable body.
// apply_patch's PreToolUse tool_input is {"command": <raw patch text>} (core
// registry ApplyPatchHandler.pre_tool_use_payload) and updatedInput is re-parsed
// via updated_hook_command → tool_input["command"], so the patch body rides
// "command" — a delta from Claude Code's content/new_string.
var contentFieldKeys = []string{"command"}

// outputContract is the Codex half of the enforce cascade: everything the
// shared engine cannot know about how this tool spells a hook response.
type outputContract struct{}

// ApprovalDecision: Codex rejects `ask`, and a no-decision fallthrough under
// approval_policy=never auto-runs the tool ungoverned. No approval-policy mode
// could be shown to surface a native prompt (codex exec is non-interactive), so
// every REQUIRE_APPROVAL becomes a content-free deny — strictly tighter than
// Claude Code, never a silent proceed (OD-SL7-ASK).
func (outputContract) ApprovalDecision() string { return codexDecisionDeny }

func (outputContract) ContentFieldKeys() []string { return contentFieldKeys }

// Render builds the PreToolUse stdout contract. The output schema is
// additionalProperties:false at every level (binary-embedded
// pre-tool-use.command.output), so exactly the documented keys are emitted.
//
// allow is emitted only bundled with a non-empty redacting updatedInput, never
// bare. That is not a grant: Codex resolves competing hooks by "any deny wins"
// and takes updated_input only when not blocked, and there is no
// approve/bypass lever (PreToolUseHookResult is Continue{updated_input} |
// Blocked). So allow+updatedInput means "proceed via Codex's own
// approval/sandbox flow, with redacted input" — tighten-only holds.
func (outputContract) Render(decision, reason string, updatedInput json.RawMessage) ([]byte, string) {
	hso := hookSpecificOutput{HookEventName: string(HookPreToolUse)}
	applied := ""

	switch {
	case decision == codexDecisionDeny:
		hso.PermissionDecision = codexDecisionDeny
		hso.PermissionDecisionReason = reason
		applied = codexDecisionDeny
	case len(updatedInput) > 0:
		hso.PermissionDecision = codexDecisionAllow
		hso.UpdatedInput = updatedInput
		applied = codexDecisionAllow
	default:
		return nil, "" // proceed with nothing to say → write nothing
	}

	line, err := json.Marshal(preToolUseOutput{HookSpecificOutput: hso})
	if err != nil {
		return nil, "" // fail-open: never wedge a tool call on a marshal fault
	}
	return line, applied
}

// compile-time proof this satisfies the shared cascade's seam.
var _ hookflow.OutputContract = outputContract{}

// contract is the adapter's singleton; the enforce path threads it into the
// shared cascade.
var contract = outputContract{}

// recordEnforcement adapts the native hook event to the shared audit writer,
// which needs only the session, the tool's kind, and the raw tool input.
func recordEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	hookflow.RecordEnforcement(logger, e.SessionID, string(kind), dec, res)
}

// applyDecision writes the hook response for a decision through the shared
// cascade, bound to this provider's contract.
func applyDecision(stdout io.Writer, dec decision.Decision, localRedaction bool, origInput json.RawMessage) (applied string, emitted bool) {
	res := hookflow.ApplyDecision(stdout, dec, localRedaction, origInput, contract)
	return res.Decision, res.Emitted
}
