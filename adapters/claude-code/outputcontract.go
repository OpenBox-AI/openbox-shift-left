package claudecode

import (
	"encoding/json"
	"io"
	"log"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Decision literals Claude Code honours on a PreToolUse hook's stdout.
const (
	ccDecisionDeny = "deny"
	ccDecisionAsk  = "ask"
)

// contentFieldKeys are the tool_input fields that may hold a redactable body,
// in the precedence order HookEvent.fileText reads them. Write and Edit are the
// two tools whose input carries a file body.
var contentFieldKeys = []string{"content", "new_string"}

// outputContract is the Claude Code half of the enforce cascade: everything the
// shared engine cannot know about how this tool spells a hook response.
type outputContract struct{}

// ApprovalDecision: Claude Code has a native permission prompt, so a
// REQUIRE_APPROVAL verdict becomes `ask` and the developer decides.
func (outputContract) ApprovalDecision() string { return ccDecisionAsk }

func (outputContract) ContentFieldKeys() []string { return contentFieldKeys }

// Render builds the PreToolUse stdout contract. An exit-0 hook that prints this
// JSON has its permissionDecision honoured: `deny` blocks the tool call (Claude
// sees the reason), `ask` shows the user a permission prompt, and a bare
// updatedInput rewrites the tool input before the call proceeds.
//
// permissionDecisionReason is shown locally (stdout → Claude Code on the same
// machine, no egress) and carries the policy-authored reason, never the tool
// command/file/output content (INV-2).
func (outputContract) Render(decision, reason string, updatedInput json.RawMessage) ([]byte, string) {
	out := preToolUseOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName:            string(HookPreToolUse),
		PermissionDecision:       decision,
		PermissionDecisionReason: reason,
	}}
	switch decision {
	case hookflow.DecisionHalt:
		// A session-halting HALT: deny the pending call AND stop the session's
		// processing now. `continue:false` is documented to override the
		// per-event decision, so the deny rides along for the transcript and
		// for older Claude Code versions that may ignore the top-level field.
		out.Continue = new(bool)
		out.StopReason = reason
		out.HookSpecificOutput.PermissionDecision = ccDecisionDeny
	case "":
		// The proceed path: Claude Code accepts updatedInput on its own, with no
		// accompanying decision — the inverse of Codex, which requires the two
		// to be bundled.
		out.HookSpecificOutput.UpdatedInput = updatedInput
		if len(updatedInput) == 0 {
			return nil, "" // proceed with nothing to say → write nothing
		}
	}
	line, err := json.Marshal(out)
	if err != nil {
		return nil, "" // fail-open: never wedge a tool call on a marshal fault
	}
	return line, decision
}

// compile-time proof this satisfies the shared cascade's seam.
var _ hookflow.OutputContract = outputContract{}

// contract is the adapter's singleton; the enforce path threads it into the
// shared cascade.
var contract = outputContract{}

// ── The prompt gate's output contract ────────────────────────────────────
//
// UserPromptSubmit spells a refusal differently from PreToolUse: a TOP-LEVEL
// `{"decision":"block","reason":…}` blocks the prompt — Claude Code erases it
// and shows the reason to the developer. There is no native ask and no input
// rewrite for prompts, so this contract is deny/halt-only.

// ccPromptDecisionBlock is the one decision literal UserPromptSubmit honours.
const ccPromptDecisionBlock = "block"

// userPromptSubmitOutput is the UserPromptSubmit hook stdout contract.
// Continue/StopReason carry the session stop exactly as on preToolUseOutput
// (same *bool rationale).
type userPromptSubmitOutput struct {
	Continue   *bool  `json:"continue,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
	Decision   string `json:"decision,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// promptOutputContract is the UserPromptSubmit half of the enforce cascade.
type promptOutputContract struct{}

// ApprovalDecision: prompts have no native permission prompt, so anything that
// would `ask` blocks instead — strictly tighter, never a silent proceed. In
// practice the gate's approval hold intercepts REQUIRE_APPROVAL first and an
// unanswered hold arrives here as a synthesized HALT, so this literal is a
// backstop rather than a path.
func (promptOutputContract) ApprovalDecision() string { return ccPromptDecisionBlock }

// ContentFieldKeys: a prompt has no redactable tool_input field, so the
// proceed-path rewrite can never engage (updatedInput is a PreToolUse-only
// lever).
func (promptOutputContract) ContentFieldKeys() []string { return nil }

// Render builds the UserPromptSubmit stdout contract: any refusal becomes
// `decision:"block"`; a session-halting HALT additionally stops the session
// via `continue:false`. The proceed path writes nothing — byte-identical to
// observe, and it leaves stdout free for the findings surface.
func (promptOutputContract) Render(decision, reason string, _ json.RawMessage) ([]byte, string) {
	if decision == "" {
		return nil, "" // proceed → write nothing
	}
	out := userPromptSubmitOutput{Decision: ccPromptDecisionBlock, Reason: reason}
	applied := ccPromptDecisionBlock
	if decision == hookflow.DecisionHalt {
		out.Continue = new(bool)
		out.StopReason = reason
		applied = hookflow.DecisionHalt
	}
	line, err := json.Marshal(out)
	if err != nil {
		return nil, "" // fail-open: never wedge a prompt on a marshal fault
	}
	return line, applied
}

var _ hookflow.OutputContract = promptOutputContract{}

// promptContract is the adapter's singleton for the prompt gate.
var promptContract = promptOutputContract{}

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
