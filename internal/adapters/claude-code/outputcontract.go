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

// ApprovalDecision is the tighten-only fallback for a caller that renders a
// decision WITHOUT the approval hold. The gate is not such a caller: it holds a
// REQUIRE_APPROVAL for a real decision first and renders what came back, so
// `ask` never reaches the coding agent through it. Deliberately -- the
// provider's own prompt would ask the developer to approve their own filed
// request (OD-E9-1, gate.go).
//
// It stays non-empty because DecisionTightens treats an empty approval verb as
// "does not tighten", which would let a REQUIRE_APPROVAL through.
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

const ccConfigDecisionBlock = "block"

// configChangeOutputContract is structurally identical to
// promptOutputContract but carries its OWN literal, NOT
// ccConfigDecisionBlock = ccPromptDecisionBlock: the two events are
// independent provider surfaces that happen to spell refusal the same way
// today, and aliasing would let a divergence in one silently change the
// other. Same reasoning as maxModelCallBodyBytes, client/payload.go:613.
type configChangeOutputContract struct{}

// ApprovalDecision: a config change has no native permission prompt either,
// so an `ask` becomes a block, same as a prompt.
func (configChangeOutputContract) ApprovalDecision() string { return ccConfigDecisionBlock }

// ContentFieldKeys: a config change has no redactable tool_input field.
func (configChangeOutputContract) ContentFieldKeys() []string { return nil }

// Render builds the ConfigChange stdout contract: any refusal becomes
// decision:"block" (the vendor-verified literal); a session-halting HALT
// additionally stops the session via continue:false. Tighten-only: no path
// here ever emits an allow.
func (configChangeOutputContract) Render(decision, reason string, _ json.RawMessage) ([]byte, string) {
	if decision == "" {
		return nil, "" // proceed → write nothing
	}
	out := userPromptSubmitOutput{Decision: ccConfigDecisionBlock, Reason: reason}
	applied := ccConfigDecisionBlock
	if decision == hookflow.DecisionHalt {
		out.Continue = new(bool)
		out.StopReason = reason
		applied = hookflow.DecisionHalt
	}
	line, err := json.Marshal(out)
	if err != nil {
		return nil, "" // fail-open: never wedge on a marshal fault
	}
	return line, applied
}

var _ hookflow.OutputContract = configChangeOutputContract{}

var configContract = configChangeOutputContract{}

func recordEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	hookflow.RecordEnforcement(logger, e.SessionID, string(kind), dec, res)
}

func applyDecision(stdout io.Writer, dec decision.Decision, localRedaction bool, origInput json.RawMessage) (applied string, emitted bool) {
	res := hookflow.ApplyDecision(stdout, dec, localRedaction, origInput, contract)
	return res.Decision, res.Emitted
}
