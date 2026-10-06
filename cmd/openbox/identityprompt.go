package main

import (
	"fmt"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// Validation and display for an agent identity typed in by hand.
//
// These moved out of `auth` when it stopped owning agent credentials. They are
// not dead weight left behind: `init --provider <tool>` offers to adopt an
// existing agent rather than register a new one, and adopting means pasting
// this agent's id and API key, and pointing at the file holding its workload
// signing key. Keeping one copy is what stops the adopt prompt from growing a
// second, subtly different idea of what a valid API key looks like.

// apiKeyProblem returns a human-readable problem with a hand-typed agent
// runtime key, or "" when it is usable. It never echoes a secret body: the
// org/agent key mix-up is reported by prefix only.
func apiKeyProblem(apiKey string) string {
	if strings.HasPrefix(strings.TrimSpace(apiKey), "obx_key_") {
		return fmt.Sprintf("that looks like an ORGANIZATION key (%s…), not this agent's runtime key.\n"+
			"  An obx_key_ key belongs in %s and can create and rotate agents org-wide.\n"+
			"  The agent runtime key starts obx_ (no `key_`) and is shown once on the agent's page\n"+
			"  when it is created. See docs/getting-started.md § Get the right credential.",
			safePrefix(strings.TrimSpace(apiKey)), devconfig.EnvControlToken)
	}
	if strings.TrimSpace(apiKey) == "" {
		return "no API key given. Paste this agent's obx_ runtime key, or decline the adopt\n" +
			"  prompt to register a new agent and have one issued."
	}
	return ""
}

func maskToken(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "(none)"
	}
	if len(v) <= 12 {
		return fmt.Sprintf("(%d chars)", len(v))
	}
	return fmt.Sprintf("%s…%s (%d chars)", v[:8], v[len(v)-4:], len(v))
}
