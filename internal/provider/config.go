package provider

import "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"

// ConfigUpdate maps an install-time credential reference onto the dev-config
// update an installer writes.
func ConfigUpdate(ref CredentialRef) devconfig.Update {
	installGitHook := ref.InstallGitHook
	return devconfig.Update{
		BaseURL:        ref.BaseURL,
		DID:            ref.DID,
		AgentID:        ref.AgentID,    // for `dev sync` / staleness
		BackendURL:     ref.BackendURL, // control-plane base for the policy read
		ContentCapture: ref.ContentCapture,
		InstallGitHook: &installGitHook,
		Enforce:        ref.Enforce,
		Tier2:          ref.Tier2,
		Findings:       ref.Findings,
	}
}
