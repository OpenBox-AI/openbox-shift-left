package codex

import (
	"os"
	"os/exec"
	"strings"
	"time"

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

func providerVersion() string {
	bin := os.Getenv("CODEX_BIN")
	if bin == "" {
		bin = "codex"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	out, err := runWithTimeout(providerVersionTimeout, path, "--version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

const providerVersionTimeout = 2 * time.Second

func runWithTimeout(d time.Duration, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
		return out, err
	case <-time.After(d):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, os.ErrDeadlineExceeded
	}
}

// providerManaged reports whether this provider's own managed configuration is
// deployed and actually constrains the session (E8-S8).
func providerManaged() string {
	paths := []string{"/etc/codex/requirements.toml"}
	sawPath := false
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				sawPath = true // exists but unreadable by this user
			}
			continue
		}
		if codexMandated(raw) {
			return "true"
		}
		return "false" // a managed file, but it constrains nothing we rely on
	}
	if sawPath {
		return "unknown"
	}
	return "false"
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
