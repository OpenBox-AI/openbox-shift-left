package claudecode

import (
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

const bashToolName = "Bash"

const preToolUseHookTimeoutSec = 30

const otherHookTimeoutSec = 5

// HookCeilings declares what Claude Code kills a hook at, so hookflow derives
// its own budget (EnforceBudget subtracts the engine's margin).
func (Engine) HookCeilings() providerspi.HookCeiling {
	return providerspi.HookCeiling{
		Gating: time.Duration(preToolUseHookTimeoutSec) * time.Second,
		Other:  otherHookTimeoutSec * time.Second,
	}
}

var evaluator = hookflow.Evaluator{
	Ceiling: Engine{}.HookCeilings(),
	NewClient: func(logger *log.Logger) (hookflow.Governor, error) {
		creds, err := ResolveCredentials()
		if err != nil {
			return nil, err
		}
		return creds.NewClient(logger)
	},
}

func isHighRiskClass(toolName string) bool {
	if toolName == bashToolName {
		return true
	}
	kind, _, _, _, _ := classifyTool(toolName)
	return kind == client.ToolMCP
}

func decisionTightens(dec decision.Decision) bool {
	return hookflow.DecisionTightens(dec, contract)
}
