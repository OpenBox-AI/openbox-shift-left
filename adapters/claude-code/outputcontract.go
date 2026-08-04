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
	hso := hookSpecificOutput{
		HookEventName:            string(HookPreToolUse),
		PermissionDecision:       decision,
		PermissionDecisionReason: reason,
	}
	if decision == "" {
		// The proceed path: Claude Code accepts updatedInput on its own, with no
		// accompanying decision — the inverse of Codex, which requires the two
		// to be bundled.
		hso.UpdatedInput = updatedInput
	}
	if hso.PermissionDecision == "" && len(hso.UpdatedInput) == 0 {
		return nil, "" // proceed with nothing to say → write nothing
	}
	line, err := json.Marshal(preToolUseOutput{HookSpecificOutput: hso})
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
