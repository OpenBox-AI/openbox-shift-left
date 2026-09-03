// Package managed REPORTS the provider-level configuration that turns OpenBox
// governance from a per-developer opt-in into an org mandate. It no longer
// installs it: deploying a root-owned file is an administrator's job, done with
// whatever the fleet already uses, and the artefacts to deploy are static under
// deployments/managed/. What is left is the read half, which `openbox doctor`
// reports from -- including whether the machine's own hooks can run at all,
// which makes this a safety report rather than an informational line.
package managed

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// Provider is a coding tool whose managed configuration we can read.
type Provider string

const (
	ProviderClaudeCode Provider = "claude-code"
	ProviderCodex      Provider = "codex"
)

// ClaudeCodeManagedDir exposes the managed-settings directory so `openbox
// doctor` resolves it through this package rather than re-deriving the path.
func ClaudeCodeManagedDir() string {
	dir, _ := claudeCodeDir()
	return dir
}

func claudeCodeDir() (dir, warning string) {
	switch runtime.GOOS {
	case "linux":
		return "/etc/claude-code", ""
	case "darwin":
		return "/Library/Application Support/ClaudeCode", ""
	case "windows":
		return `C:\Program Files\ClaudeCode`, ""
	default:
		return "", fmt.Sprintf("claude-code: no known managed-settings path for %s; "+
			"deploy the template by hand", runtime.GOOS)
	}
}

func codexDir() (dir, warning string) {
	switch runtime.GOOS {
	case "linux", "darwin":
		return "/etc/codex", ""
	default:
		return "", fmt.Sprintf("codex: no known managed-config path for %s; "+
			"deploy the template by hand", runtime.GOOS)
	}
}

// ProviderState reports whether a provider's managed configuration is deployed
// on this machine, for `openbox doctor` and for posture.provider_managed
// (E8-S8).
func ProviderState(p Provider) string {
	var dir string
	var files []string
	switch p {
	case ProviderClaudeCode:
		dir, _ = claudeCodeDir()
		files = []string{"managed-settings.json"}
	case ProviderCodex:
		dir, _ = codexDir()
		files = []string{"requirements.toml"}
	default:
		return "unknown (no template for this provider)"
	}
	if dir == "" {
		return "unknown (no managed path known for this OS)"
	}
	for _, name := range files {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if mandates(p, raw) {
			return "managed (" + path + ")"
		}
		return "present but imposes no OpenBox mandate (" + path + ")"
	}
	return "not managed (no " + filepath.Join(dir, files[0]) + ")"
}

var codexRequirementKeys = []string{
	"allow_managed_hooks_only",
	"allowed_approval_policies",
	"allowed_sandbox_modes",
}

func mandates(p Provider, raw []byte) bool {
	if p == ProviderCodex {
		keys := devconfig.TopLevelTOMLKeys(raw)
		for _, k := range codexRequirementKeys {
			if keys[k] {
				return true
			}
		}
		return false
	}
	return strings.Contains(string(raw), "hook "+string(p))
}
