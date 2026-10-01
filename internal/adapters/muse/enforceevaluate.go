package muse

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// evaluator is the one bounded /evaluate round trip every gated event makes.
// Its ceiling is the gating ceiling the installer declares for every gated
// handler, PreLLMCall included, so the whole-hook budget is derived from it
// (hookflow.EnforceBudget) rather than restated here.
var evaluator = hookflow.NewEvaluator(Engine{}.HookCeilings())

// isHighRiskClass reports whether a tool is shell execution or an MCP call.
func isHighRiskClass(toolName string) bool {
	kind, _, _, _, _ := classifyTool(toolName)
	if kind == client.ToolMCP {
		return true
	}
	c, known := builtinTools[toolName]
	return known && c.Kind == client.ToolShell
}
