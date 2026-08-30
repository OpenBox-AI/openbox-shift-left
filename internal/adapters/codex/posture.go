package codex

import (
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// adapterVersion identifies this adapter build in the recorded posture. It is
// coarse on purpose: the posture answers "which governance behaviour was in
// effect", and that changes with the adapter's contract, not with every commit.
const adapterVersion = "codex/1"

// effectivePosture assembles the session's posture: the config-resolved fields
// from devconfig plus what only this adapter can determine — the local bundle's
// integrity coordinates, the provider version, and the freshness outcome the
// caller just obtained.
//
// Best-effort by construction: every lookup that can fail degrades to an empty
// field, which reads downstream as "unknown" rather than as a false claim. It
// runs once per session on SessionStart, off the tool hot path.
func effectivePosture() devconfig.Posture {
	p := devconfig.EffectivePosture()
	p.Adapter = adapterVersion
	p.AdapterVersion = adapterVersion
	p.ProviderVersion = providerVersion()
	p.ProviderManaged = providerManaged()
	return p
}

// providerVersion asks the Codex CLI for its version. Bounded and best-effort:
// a missing binary or a slow answer yields "" (unknown) rather than delaying
// the session.
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
// deployed and actually constrains the session (E8-S8). Without it, enforcement is
// a hook in the developer's own config and a local edit removes the gate — so this
// is the field that separates "enforcing" from "enforcing with assurance".
//
// It answers a deliberately narrow question: does a managed file exist at a known
// path and does it carry a mandate Codex will apply. It cannot confirm the file is
// root-owned or that the provider parsed it, so "true" here is evidence, not proof
// — hence the tri-state (an unreadable path is "unknown", never a quiet "false").
//
// The mandate is identified by a TOP-LEVEL requirement key, not by a substring.
// Substring matching is what let the earlier mis-nested template (mandate keys
// written below a `[hooks]` header, hence bound as `hooks.*` and ignored) report
// this machine as managed while nothing was in effect. `hook codex` is
// deliberately NOT the marker: hook definitions are not a requirements key at
// all, so a file naming our hook there proves nothing about the mandate.
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

// codexRequirementKeys are the requirements.toml keys whose presence means this
// machine is genuinely constrained: hook exclusivity, or a pin on the approval /
// sandbox modes a local config may select. Any one of them is a mandate.
var codexRequirementKeys = []string{
	"allow_managed_hooks_only",
	"allowed_approval_policies",
	"allowed_sandbox_modes",
}

// codexMandated reports whether a requirements.toml body defines at least one
// mandate key at the top level (where Codex reads it).
func codexMandated(raw []byte) bool {
	keys := devconfig.TopLevelTOMLKeys(raw)
	for _, k := range codexRequirementKeys {
		if keys[k] {
			return true
		}
	}
	return false
}
