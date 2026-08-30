package claudecode

import (
	"encoding/json"
	"io"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

const (
	ccDecisionDeny = "deny"
	ccDecisionAsk  = "ask"
)

var contentFieldKeys = []string{"content", "new_string"}

type outputContract struct{}

// ApprovalDecision: Claude Code has a native permission prompt, so a
// REQUIRE_APPROVAL verdict becomes `ask` and the developer decides.
func (outputContract) ApprovalDecision() string { return ccDecisionAsk }

func (outputContract) ContentFieldKeys() []string { return contentFieldKeys }

// Render builds the PreToolUse stdout contract. PermissionDecisionReason is
// shown locally (stdout → Claude Code on the same machine, no egress) and
// carries the policy-authored reason, never the tool command/file/output
// content (INV-2).
func (outputContract) Render(decision, reason string, updatedInput json.RawMessage) ([]byte, string) {
	out := preToolUseOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName:            string(HookPreToolUse),
		PermissionDecision:       decision,
		PermissionDecisionReason: reason,
	}}
	switch decision {
	case hookflow.DecisionHalt:
		out.Continue = new(bool)
		out.StopReason = reason
		out.HookSpecificOutput.PermissionDecision = ccDecisionDeny
	case "":
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

var _ hookflow.OutputContract = outputContract{}

var contract = outputContract{}

const ccPromptDecisionBlock = "block"

type userPromptSubmitOutput struct {
	Continue   *bool  `json:"continue,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
	Decision   string `json:"decision,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type promptOutputContract struct{}

// ApprovalDecision: prompts have no native permission prompt, so anything that
// would `ask` blocks instead; strictly tighter, never a silent proceed.
func (promptOutputContract) ApprovalDecision() string { return ccPromptDecisionBlock }

// ContentFieldKeys: a prompt has no redactable tool_input field, so the
// proceed-path rewrite can never engage (updatedInput is a PreToolUse-only
// lever).
func (promptOutputContract) ContentFieldKeys() []string { return nil }

// Render builds the UserPromptSubmit stdout contract: any refusal becomes
// `decision:"block"`; a session-halting HALT additionally stops the session
// via `continue:false`.
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

var promptContract = promptOutputContract{}

func recordEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	hookflow.RecordEnforcement(logger, e.SessionID, string(kind), dec, res)
}

func applyDecision(stdout io.Writer, dec decision.Decision, localRedaction bool, origInput json.RawMessage) (applied string, emitted bool) {
	res := hookflow.ApplyDecision(stdout, dec, localRedaction, origInput, contract)
	return res.Decision, res.Emitted
}
