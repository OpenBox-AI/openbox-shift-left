package codex

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// adapterVersion identifies this adapter build in the recorded posture.
const adapterVersion = "codex/1"

func effectivePosture() devconfig.Posture {
	p := devconfig.EffectivePosture()
	p.Adapter = adapterVersion
	p.AdapterVersion = adapterVersion
	p.ProviderVersion = providerVersion()
	p.ProviderManaged = providerManaged()
	return p
}

func providerVersion() string { return devconfig.ProviderVersion("CODEX_BIN", "codex") }

// providerManaged reports whether this provider's own managed configuration is
// deployed and actually constrains the session.
func providerManaged() string {
	return devconfig.ProviderManaged([]string{"/etc/codex/requirements.toml"}, codexMandated)
}

var codexRequirementKeys = []string{
	"allow_managed_hooks_only",
	"allowed_approval_policies",
	"allowed_sandbox_modes",
}

func codexMandated(raw []byte) bool {
	keys := devconfig.TopLevelTOMLKeys(raw)
	for _, k := range codexRequirementKeys {
		if keys[k] {
			return true
		}
	}
	return false
}
