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

var _ hookflow.OutputContract = outputContract{}

var contract = outputContract{}

func recordEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	hookflow.RecordEnforcement(logger, e.SessionID, string(kind), dec, res)
}

func applyDecision(stdout io.Writer, dec decision.Decision, localRedaction bool, origInput json.RawMessage) (applied string, emitted bool) {
	res := hookflow.ApplyDecision(stdout, dec, localRedaction, origInput, contract)
	return res.Decision, res.Emitted
}
