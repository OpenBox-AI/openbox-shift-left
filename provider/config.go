package provider

import "github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"

// ConfigUpdate maps an install-time credential reference onto the dev-config
// update an installer writes.
//
// It lives here, once, because every provider needs exactly the same mapping:
// both shipped adapters carried a private copy, and those copies had already
// drifted in which fields a re-init preserved. A third adapter would have
// inherited whichever copy it was ported from.
//
// InstallGitHook is passed as an explicit value rather than left unspecified —
// it is a per-run install choice, not a posture the developer expects to
// persist untouched across a re-init. The enforce posture is the opposite: nil
// means "this run did not say", which preserves what is already on disk.
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
