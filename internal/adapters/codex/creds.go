package codex

import (
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Env-var aliases for the enforce leg. The names are the cross-adapter
// contract (the same dev.json serves every provider), so they alias the
// shared devconfig constants; only the two enforce-only, decision-specific
// overrides are Codex-local (not part of the shared contract).
const (
	envEnforcementFile = devconfig.EnvEnforcementFile
	envEnforceTimeout  = devconfig.EnvEnforceTimeout
	envTier2Timeout    = devconfig.EnvTier2Timeout
	envSidecarBundle   = "OPENBOX_SIDECAR_BUNDLE" // decision-bundle override; enforce-only
	envStaleDir        = "OPENBOX_STALE_DIR"      // per-session stale-marker dir; enforce-only
)

// Credential/config resolution for the Codex hook path — thin bindings over the shared
// internal/adapters/common/devconfig module. Codex reads the same `~/.openbox/dev.json`
// posture `openbox init` writes and the same `~/.openbox/.env` credentials `openbox
// auth` writes, for every provider; nothing here is Codex-specific except the spool
// subdir name.
//
// INV-1: the hook reads the DID only on the hot path (no secret I/O); the obx_ key +
// signing key are read (from ~/.openbox/.env, or the OPENBOX_API_KEY /
// OPENBOX_AGENT_PRIVATE_KEY env overrides) only at flush and go straight into the
// client, never logged/printed/argv'd.

// DevConfig is the shared non-secret coordinate file contract.
type DevConfig = devconfig.DevConfig

// DefaultConfigPath is where the installer writes the dev config and the hook
// looks for it when OPENBOX_CONFIG is unset.
func DefaultConfigPath() string { return devconfig.DefaultConfigPath() }

// Credentials is the resolved runtime identity for the hook binary.
type Credentials struct {
	BaseURL               string
	APIKey                string
	DID                   string
	PrivateKeyB64         string
	ContentCaptureEnabled bool
}

// Identity is the non-secret projection used by the Mapper.
func (c Credentials) Identity() Identity { return Identity{DeveloperDID: c.DID} }

// NewClient builds the AIP-signed transport from the resolved credentials.
func (c Credentials) NewClient(logger client.Logger) (*client.Client, error) {
	return client.New(client.Config{
		BaseURL:               c.BaseURL,
		APIKey:                c.APIKey,
		DID:                   c.DID,
		PrivateKeyB64:         c.PrivateKeyB64,
		ContentCaptureEnabled: c.ContentCaptureEnabled,
		Logger:                logger,
	})
}

// ResolveIdentity resolves only the developer DID (env, then config file) —
// no secret-store access (INV-1: zero secret I/O on the hot path).
func ResolveIdentity() (Identity, error) {
	did, err := devconfig.ResolveDID()
	if err != nil {
		return Identity{}, err
	}
	return Identity{DeveloperDID: did}, nil
}

// ResolveCredentials assembles Credentials via the shared resolver: secrets from
// the environment then ~/.openbox/.env, coordinates from the environment then
// dev.json. Errors are handled fail-open by the caller (INV-3); no secret value
// ever appears in an error.
func ResolveCredentials() (Credentials, error) {
	dc, err := devconfig.ResolveCredentials()
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		BaseURL:               dc.BaseURL,
		APIKey:                dc.APIKey,
		DID:                   dc.DID,
		PrivateKeyB64:         dc.PrivateKeyB64,
		ContentCaptureEnabled: dc.ContentCaptureEnabled,
	}, nil
}

// DefaultSpoolDir is where hot-path events are spooled before flush — a
// codex-specific subdir so a machine running Claude Code AND Codex never
// cross-drains spools. OPENBOX_SPOOL_DIR overrides (tests use a temp dir).
func DefaultSpoolDir() string { return devconfig.SpoolDir("codex-spool") }

// ResolveContentCapture reports the org content posture (default on,
// opt-out via `content_capture:false` / OPENBOX_CONTENT_CAPTURE=0).
func ResolveContentCapture() bool { return devconfig.ResolveContentCapture() }

// ResolveInstallGitHook reports whether to ambient-install the
// prepare-commit-msg hook on SessionStart (default false; env overrides).
func ResolveInstallGitHook() bool { return devconfig.ResolveInstallGitHook() }

// ResolveFinops reports whether rollout usage extraction is
// enabled. Default false: the SessionEnd transcript_path is never opened
// for usage with it unset. A separate flag from content_capture — finops
// on does not imply content egress (numbers only, INV-2).
func ResolveFinops() bool { return devconfig.ResolveFinops() }

// ResolveCoordinates resolves the non-secret target coordinates (base URL
// + DID) with zero secret-store access — used by read-only previews.
func ResolveCoordinates() (baseURL, did string) { return devconfig.ResolveCoordinates() }

// ── Enforce-leg resolvers (thin bindings over the shared devconfig
//    contract; identical names/semantics to the Claude Code adapter). ──

// ResolveEnforce reports whether the developer runtime is in enforce mode.
// DEFAULT ON (that decision reversed the observe default; that decision governs
// the in-process mechanism, not the default value). A config read error never
// turns enforcement on (INV-3 fail-safe).
func ResolveEnforce() bool { return devconfig.ResolveEnforce() }

// ResolveFailClosed reports the enforce failure policy (default false =
// fail-open — an org never becomes fail-closed by accident).
func ResolveFailClosed() bool { return devconfig.ResolveFailClosed() }

// ResolveTier2 reads the DEPRECATED, inert `tier2` key. It changes nothing:
// every gated call is evaluated. It is retained so an existing dev.json parses,
// and it warns once to stderr when present. See devconfig.ResolveTier2 for why
// an explicit false is deliberately not honoured. Historic note: `openbox init`
// used to couple tier2 to the enforce posture and enforce now defaults on — so a
// bare install writes tier2: true. There is no standalone flag; it follows
// enforce.
func ResolveTier2() bool { return devconfig.ResolveTier2() }

// ResolveSecretDetection reports whether local secret detection is
// on (default true, opt-out — the detection stays strictly local).
func ResolveSecretDetection() bool { return devconfig.ResolveSecretDetection() }

// ResolveFindings reports whether the findings loop is on (default
// false, opt-in — it is the first observe-path stdout writer).
func ResolveFindings() bool { return devconfig.ResolveFindings() }

// ResolveFindingsCursor resolves the findings-loop cursor state file path.
func ResolveFindingsCursor() string { return devconfig.ResolveFindingsCursor("codex") }

// ResolveAgentID resolves the backend agent id for policy sync/staleness.
func ResolveAgentID() string { return devconfig.ResolveAgentID() }

// ResolveBackendURL resolves the openbox-backend control-plane base URL.
func ResolveBackendURL() string { return devconfig.ResolveBackendURL() }

// ResolveControlToken resolves the org control-plane credential: the
// OPENBOX_CONTROL_TOKEN env only (never config, never the secret store).
func ResolveControlToken() string { return devconfig.ResolveControlToken() }

// ResolveOrgSigningKey returns the org's pinned policy-bundle signing key
// (base64 raw Ed25519) and its id, from the shared dev config (E8-S6).
func ResolveOrgSigningKey() (pubKeyB64, keyID string) { return devconfig.ResolveOrgSigningKey() }

// ResolveEvaluationTimeout resolves the in-binary budget for one
// escalation: config first, env-if-parseable wins; <=0 yields
// defaultEvaluationTimeout; clamped to maxEvaluationTimeout (the Codex whole-hook
// wall-clock bound — see enforce_tier2.go).
func ResolveEvaluationTimeout() time.Duration {
	ms := devconfig.ResolveTimeoutMS(func(c DevConfig) int { return c.Tier2TimeoutMS }, envTier2Timeout)
	if ms <= 0 {
		return hookflow.DefaultEvaluationTimeout
	}
	if maxMS := int64(maxEvaluationTimeout / time.Millisecond); int64(ms) > maxMS {
		return maxEvaluationTimeout
	}
	return time.Duration(ms) * time.Millisecond
}
