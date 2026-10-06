package codex

import (
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

const bashToolName = "Bash"

// HookCeilings declares what Codex kills a hook at.
func (Engine) HookCeilings() providerspi.HookCeiling {
	return providerspi.HookCeiling{
		Gating: time.Duration(preToolUseHookTimeoutSec) * time.Second,
		Other:  time.Duration(hotHookTimeoutSec) * time.Second,
	}
}

var evaluator = hookflow.NewEvaluator(Engine{}.HookCeilings())

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
