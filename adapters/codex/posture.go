package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/decision"
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
func effectivePosture(staleness devconfig.Staleness) devconfig.Posture {
	p := devconfig.EffectivePosture()
	p.Adapter = adapterVersion
	p.AdapterVersion = adapterVersion
	p.ProviderVersion = providerVersion()
	p.Staleness = staleness
	p.BundleVersion, p.BundlePolicyID, p.BundleSHA256 = bundleCoordinates()
	return p
}

// bundleCoordinates reads the local policy bundle's opaque identity: its
// version and policy id (already the staleness pin) plus a hash of the file as
// it sits on disk. The hash is what makes a local edit visible: E8-S6 gives the
// bundle a signature to verify, but even before that, a recorded digest lets
// the control plane see that two sessions claiming the same policy id ran
// different bytes. Never the policy text itself.
func bundleCoordinates() (version, policyID, sha string) {
	path := ResolveBundlePath()
	if b, err := decision.LoadBundleFile(path); err == nil && b != nil {
		version, policyID = b.Version, b.PolicyID
	}
	if raw, err := os.ReadFile(path); err == nil {
		sum := sha256.Sum256(raw)
		sha = hex.EncodeToString(sum[:])
	}
	return version, policyID, sha
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
