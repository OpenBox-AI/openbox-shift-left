package codex

import (
	"encoding/json"
	"io"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

const (
	codexDecisionDeny  = "deny"
	codexDecisionAllow = "allow"
)

var contentFieldKeys = []string{"command"}

type outputContract struct{}

// ApprovalDecision: Codex rejects `ask`, and a no-decision fallthrough under
// approval_policy=never auto-runs the tool ungoverned.
func (outputContract) ApprovalDecision() string { return codexDecisionDeny }

func (outputContract) ContentFieldKeys() []string { return contentFieldKeys }

// Render builds the PreToolUse stdout contract. Allow is emitted only bundled
// with a non-empty redacting updatedInput, never bare.
func (outputContract) Render(decision, reason string, updatedInput json.RawMessage) ([]byte, string) {
	hso := hookSpecificOutput{HookEventName: string(HookPreToolUse)}
	applied := ""

	switch {
	case decision == codexDecisionDeny, decision == hookflow.DecisionHalt:
		hso.PermissionDecision = codexDecisionDeny
		hso.PermissionDecisionReason = reason
		applied = codexDecisionDeny
	case len(updatedInput) > 0:
		// Refuse to pair `allow` with a rewrite Codex will reject. Codex requires
		// a STRING at the content field; anything else invalidates the whole
		// output, and an invalidated PreToolUse output means the tool runs with
		// the ORIGINAL, unredacted input. Measured on 0.150.0-alpha.8: an array
		// command logged `hook returned updatedInput without string field
		// 'command'` and the original command executed.
		//
		// RedactToolInput already guarantees the string shape, so nothing reaches
		// this branch malformed today. The guard is here because the failure is
		// SILENT in production -- no error, no log, just an ungoverned call -- and
		// the contract is the last place that can still refuse it.
		if !carriesStringContentField(updatedInput) {
			return nil, "" // proceed ungoverned rather than emit a line that fails open
		}
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

// carriesStringContentField reports whether a rebuilt tool_input still carries a
// non-empty STRING at one of the keys this contract rewrites. Keyed off
// contentFieldKeys rather than a literal, so adding a rewritable field cannot
// leave the guard behind.
func carriesStringContentField(updatedInput json.RawMessage) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(updatedInput, &obj); err != nil {
		return false
	}
	for _, k := range contentFieldKeys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return true
		}
	}
	return false
}

var _ hookflow.OutputContract = outputContract{}

var contract = outputContract{}

const codexPromptDecisionBlock = "block"

// userPromptSubmitOutput is the UserPromptSubmit stdout shape. It deliberately
// does NOT reuse hookSpecificOutput: that wrapper is PreToolUse-only, and
// Codex's parser rejects a PreToolUse line carrying any of `continue`,
// `stopReason` or `suppressOutput` -- the very fields this contract needs. The
// two shapes are separate because the vendor keeps them separate.
type userPromptSubmitOutput struct {
	Continue   *bool  `json:"continue,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
	Decision   string `json:"decision,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type promptOutputContract struct{}

// ApprovalDecision: a prompt has no native permission prompt, so anything that
// would `ask` blocks instead; strictly tighter, never a silent proceed.
func (promptOutputContract) ApprovalDecision() string { return codexPromptDecisionBlock }

// ContentFieldKeys: a prompt has no redactable tool_input field, so the
// proceed-path rewrite can never engage. Paired with promptTarget.ToolInput()
// returning nil, this makes updatedInput structurally impossible here.
func (promptOutputContract) ContentFieldKeys() []string { return nil }

// Render builds the UserPromptSubmit stdout contract: any refusal becomes
// `decision:"block"`, and a session-terminating HALT additionally stops the
// thread with `continue:false` + `stopReason`.
//
// This is the adapter's ONLY renderer that returns DecisionHalt, and therefore
// the only writer of the session-halt latch (hookflow/gate.go writes the latch
// on exactly that return). Both tool contracts fold HALT into their per-call
// refusal instead, because UserPromptSubmit is the one Codex surface with a
// session-stop lever. Measured on codex-cli 0.150.0-alpha.8: a plain block logs
// `hook: UserPromptSubmit Blocked`, and the same output plus `continue:false`
// logs `hook: UserPromptSubmit Stopped` -- two distinct states, which is what
// makes the latch meaningful (phase 00 probe P0.5).
func (promptOutputContract) Render(decision, reason string, _ json.RawMessage) ([]byte, string) {
	if decision == "" {
		return nil, "" // proceed → write nothing
	}
	out := userPromptSubmitOutput{Decision: codexPromptDecisionBlock, Reason: reason}
	applied := codexPromptDecisionBlock
	if decision == hookflow.DecisionHalt {
		out.Continue = new(bool) // *bool → false; the key must be present to mean it
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

var promptContract = promptOutputContract{}

func recordEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	hookflow.RecordEnforcement(logger, e.SessionID, string(kind), dec, res)
}

func applyDecision(stdout io.Writer, dec decision.Decision, localRedaction bool, origInput json.RawMessage) (applied string, emitted bool) {
	res := hookflow.ApplyDecision(stdout, dec, localRedaction, origInput, contract)
	return res.Decision, res.Emitted
}
