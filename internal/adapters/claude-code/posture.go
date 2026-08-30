package claudecode

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
const adapterVersion = "claude-code/1"

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

// providerVersion asks the Claude Code CLI for its version. Bounded and
// best-effort: a missing binary or a slow answer yields "" (unknown) rather
// than delaying the session.
func providerVersion() string {
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
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
// deployed and names the OpenBox hook (E8-S8). Without it, enforcement is a hook
// in the developer's own config and a local edit removes the gate — so this is
// the field that separates "enforcing" from "enforcing with assurance".
//
// It answers a deliberately narrow question: does a managed file exist at a known
// path and does it invoke us. It cannot confirm the file is root-owned or that the
// provider parsed it, so "true" here is evidence, not proof — hence the tri-state
// (an unreadable path is "unknown", never a quiet "false").
func providerManaged() string {
	paths := []string{"/etc/claude-code/managed-settings.json", "/Library/Application Support/ClaudeCode/managed-settings.json"}
	sawPath := false
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				sawPath = true // exists but unreadable by this user
			}
			continue
		}
		if strings.Contains(string(raw), "hook claude-code") {
			return "true"
		}
		return "false" // managed, but not by us
	}
	if sawPath {
		return "unknown"
	}
	return "false"
}
