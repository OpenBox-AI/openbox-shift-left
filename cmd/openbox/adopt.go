package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/prompt"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
)

// adoptExistingAgent offers to take over an agent that already exists instead
// of registering a new one, and writes this tool's store from three values:
// the agent id, its API key, and the path to a file holding its workload
// signing key.
//
// It exists because `auth` used to be the interactive route to a rotated key,
// and shrinking it to the org connection removed that route entirely. It is
// also the only recovery from a taken-name registration where this machine
// still holds the older agent's own key.
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

	agentIDInput, err := p.Line("Agent id", "")
	if err != nil {
		return false, a.errorf("%v", err)
	}
	agentID := strings.TrimSpace(agentIDInput)
	if agentID == "" {
		return false, a.errorf("no agent id given. It is on the agent's page in the dashboard.\n" +
			"  Decline the adopt prompt to register a new agent instead.")
	}
	if _, err := uuid.Parse(agentID); err != nil {
		return false, a.errorf("%q is not a valid agent id (expected a UUID, as shown on the agent's\n"+
			"  page in the dashboard). Decline the adopt prompt to register a new agent instead.", agentID)
	}

	apiKey, err := p.Secret("API key (obx_…)", false)
	if err != nil {
		return false, a.errorf("%v", err)
	}
	if problem := apiKeyProblem(apiKey); problem != "" {
		return false, a.errorf("%s", problem)
	}

	keyPathInput, err := p.Line("Private key file path", "")
	if err != nil {
		return false, a.errorf("%v", err)
	}
	keyPath := expandHome(strings.TrimSpace(keyPathInput))
	if keyPath == "" {
		return false, a.errorf("no private key file given. It holds either the PKCS#8 PEM the agent's\n" +
			"  page offered to download, or another machine's OPENBOX_WORKLOAD_PRIVATE_KEY value.\n" +
			"  Decline the adopt prompt to register a new agent instead.")
	}

	// Read once and validated before anything is written, so a wrong or
	// unreadable key never leaves a half-populated store that the next run
	// reads as "already registered". The file itself is never deleted or
	// moved: only the normalised copy this produces is written to the
	// credential store.
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return false, a.errorf("read private key file %s: %v", keyPath, err)
	}
	normalizedKey, err := workloadauth.NormalizePrivateKey(string(raw))
	if err != nil {
		return false, a.errorf("%v\n  A wrong key only fails later, at exchange; run `openbox doctor` "+
			"to confirm once adopted.", err)
	}

	// Captured before the write: WriteWorkloadIdentity clears the legacy
	// markers this reads, so reading it after would always answer false.
	wasLegacy := false
	if ls, lerr := devconfig.LegacyStoreFor(tool); lerr == nil {
		wasLegacy = ls.Legacy
	}

	if err := devconfig.WriteWorkloadIdentity(tool, devconfig.WorkloadIdentity{
		AgentID:       agentID,
		APIKey:        strings.TrimSpace(apiKey),
		PrivateKeyB64: normalizedKey,
	}); err != nil {
		return false, a.errorf("write credentials: %v", err)
	}

	if wasLegacy {
		if n, derr := discardLegacySpool(tool); derr == nil && n > 0 {
			fmt.Fprintf(a.stdout, "discarded %d events queued under the previous identity\n", n)
		}
	}

	envPath, _ := devconfig.EnvFilePathFor(tool)
	cfgPath, _ := devconfig.DevConfigPathFor(tool)
	// Paths only. The key reached this process from a file and goes to a 0600
	// file; it is never echoed back, logged or put on an argv (INV-1).
	fmt.Fprintf(a.stdout, "✓ adopted agent %s for %s\n", agentID, tool)
	fmt.Fprintf(a.stdout, "  %s (0600; plaintext; values never printed)\n  %s\n", envPath, cfgPath)
	fmt.Fprintf(a.stdout, "  %s was read, not moved; keep it safe\n", keyPath)
	fmt.Fprintln(a.stdout, "  run `openbox doctor` to confirm")
	return true, exitOK
}

// expandHome resolves a leading ~ the way a shell would, for a path typed
// into a prompt rather than passed through one.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}
