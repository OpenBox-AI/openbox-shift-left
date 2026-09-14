package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/prompt"
)

// adoptExistingAgent offers to take over an agent that already exists instead
// of registering a new one, and writes this tool's store from the four values
// the developer pastes.
//
// It exists because `auth` used to be the interactive route to a rotated key,
// and shrinking it to the org connection removed that route entirely. It is
// also the only recovery from the duplicate-name halt, which fires precisely
// when an agent exists in the org and this machine has lost its credentials.
//
// Declining is the default. Adopting overwrites nothing -- this branch is only
// reached when the tool has no store -- but registering a second agent for a
// tool that already has one is the outcome nobody wants, so the question is
// asked with "no" as the answer a stray Enter gives.
func (a *app) adoptExistingAgent(tool string) (adopted bool, code int) {
	p, err := a.newPrompt()
	if err != nil {
		if errors.Is(err, prompt.ErrNotATerminal) {
			// Registering is the right thing to do without a terminal: this
			// branch already knows the tool has no identity and the org token is
			// present, so refusing for want of a prompt would turn a working
			// unattended install into a failure.
			return false, exitOK
		}
		return false, a.errorf("%v", err)
	}

	yes, err := p.Confirm("Adopt an existing agent instead of registering a new one?", false)
	if err != nil {
		return false, a.errorf("%v", err)
	}
	if !yes {
		return false, exitOK
	}

	agentID, err := p.Line("Agent id", "")
	if err != nil {
		return false, a.errorf("%v", err)
	}
	did, err := p.Line("Agent DID", "")
	if err != nil {
		return false, a.errorf("%v", err)
	}
	apiKey, err := p.Secret("API key (obx_…)", false)
	if err != nil {
		return false, a.errorf("%v", err)
	}
	signingKey, err := p.Secret("Signing key (base64)", false)
	if err != nil {
		return false, a.errorf("%v", err)
	}

	// Validated before anything is written, so a mistyped seed does not leave a
	// half-populated store that the next run reads as "already registered".
	if strings.TrimSpace(agentID) == "" {
		return false, a.errorf("no agent id given. It is on the agent's page in the dashboard,\n" +
			"  beside the DID. Decline the adopt prompt to register a new agent instead.")
	}
	if problem := validateAgentIdentity(did, apiKey, signingKey); problem != "" {
		return false, a.errorf("%s", problem)
	}

	// The config first, then the credentials. Both orders can be interrupted;
	// only this one fails safe. A DID with no credential file is the state the
	// hook warns about by name and the next `init` repairs, because the reuse
	// decision reads the credential file -- credentials with no DID would be
	// reused forever while resolving nothing.
	cfgPath, err := devconfig.DevConfigWritePathFor(tool)
	if err != nil {
		return false, a.errorf("%v", err)
	}
	if err := devconfig.WriteConfig(cfgPath, devconfig.Update{
		DID:     strings.TrimSpace(did),
		AgentID: strings.TrimSpace(agentID),
	}); err != nil {
		return false, a.errorf("write dev config: %v", err)
	}
	envPath, err := devconfig.EnvFilePathFor(tool)
	if err != nil {
		return false, a.errorf("%v", err)
	}
	if err := devconfig.WriteEnvFile(envPath, map[string]string{
		devconfig.EnvAPIKeyDirect:    strings.TrimSpace(apiKey),
		devconfig.EnvAgentPrivateKey: strings.TrimSpace(signingKey),
	}); err != nil {
		return false, a.errorf("write credentials: %v", err)
	}

	// Paths only. The seed reached this process from a terminal and goes to a
	// 0600 file; it is never echoed back, logged or put on an argv (INV-1).
	fmt.Fprintf(a.stdout, "✓ adopted agent %s for %s\n", strings.TrimSpace(agentID), tool)
	fmt.Fprintf(a.stdout, "  %s (0600; plaintext;)\n  %s\n", envPath, cfgPath)
	return true, exitOK
}
