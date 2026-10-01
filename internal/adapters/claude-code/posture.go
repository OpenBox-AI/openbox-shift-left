package claudecode

import (
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// adapterVersion identifies this adapter build in the recorded posture.
const adapterVersion = "claude-code/1"

func effectivePosture() devconfig.Posture {
	p := devconfig.EffectivePosture()
	p.Adapter = adapterVersion
	p.AdapterVersion = adapterVersion
	p.ProviderVersion = providerVersion()
	p.ProviderManaged = providerManaged()
	return p
}

func providerVersion() string { return devconfig.ProviderVersion("CLAUDE_BIN", "claude") }

// providerManaged reports whether this provider's own managed configuration is
// deployed and names the OpenBox hook.
func providerManaged() string {
	return devconfig.ProviderManaged(
		[]string{"/etc/claude-code/managed-settings.json", "/Library/Application Support/ClaudeCode/managed-settings.json"},
		func(raw []byte) bool { return strings.Contains(string(raw), "hook claude-code") })
}
