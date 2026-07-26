package codex

import (
	"os"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Env-var aliases for the enforce leg (STORY-SL7-B). The names are the
// cross-adapter contract (the same dev.json serves every provider — OD-SL7-SHARE),
// so they alias the shared devconfig constants; only the two enforce-only,
// decision-specific overrides are Codex-local (not part of the shared contract).
const (
	envEnforcementFile = devconfig.EnvEnforcementFile
	envEnforceTimeout  = devconfig.EnvEnforceTimeout
	envTier2Timeout    = devconfig.EnvTier2Timeout
	envSidecarBundle   = "OPENBOX_SIDECAR_BUNDLE" // decision-bundle override; enforce-only
	envStaleDir        = "OPENBOX_STALE_DIR"      // per-session stale-marker dir; enforce-only
)

// Credential/config resolution for the Codex hook path — thin bindings over the
// shared adapters/common/devconfig module (OD-SL7-SHARE ruling (a), ADR-0007).
// Codex reads the SAME `~/.config/openbox/dev.json` contract and OS/file secret
// store `openbox dev init` writes for every provider; nothing here is
// Codex-specific except the spool subdir name.
//
// INV-1: the hook reads the DID only on the hot path (no secret I/O); the obx_
// key + Ed25519 seed are read (secret store, or the OPENBOX_API_KEY /
// OPENBOX_ED25519_SEED CI overrides) only at flush and go straight into the
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
	SeedB64               string
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
		SeedB64:               c.SeedB64,
		ContentCaptureEnabled: c.ContentCaptureEnabled,
		Logger:                logger,
	})
}

// secretLookup is the OS secret-store reader; overridable in tests.
var secretLookup devconfig.SecretLookup = devconfig.OSSecretLookup

// ResolveIdentity resolves ONLY the developer DID (env, then config file) — no
// secret-store access (INV-1 + NFR-2: zero secret I/O on the hot path).
func ResolveIdentity() (Identity, error) {
	did, err := devconfig.ResolveDID()
	if err != nil {
		return Identity{}, err
	}
	return Identity{DeveloperDID: did}, nil
}

// ResolveCredentials assembles Credentials from env + the dev config + the OS
// (or opt-in file) secret store, via the shared resolver. Errors are handled
// fail-open by the caller (INV-3); no secret value ever appears in an error.
func ResolveCredentials() (Credentials, error) {
	dc, err := devconfig.ResolveCredentials(secretLookup)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		BaseURL:               dc.BaseURL,
		APIKey:                dc.APIKey,
		DID:                   dc.DID,
		SeedB64:               dc.SeedB64,
		ContentCaptureEnabled: dc.ContentCaptureEnabled,
	}, nil
}

// DefaultSpoolDir is where hot-path events are spooled before flush — a
// codex-specific subdir so a machine running Claude Code AND Codex never
// cross-drains spools. OPENBOX_SPOOL_DIR overrides (tests use a temp dir).
func DefaultSpoolDir() string { return devconfig.SpoolDir("codex-spool") }

// ResolveContentCapture reports the org content posture (OD4; DEFAULT ON as of
// 2026-07-15, opt-out via `content_capture:false` / OPENBOX_CONTENT_CAPTURE=0).
func ResolveContentCapture() bool { return devconfig.ResolveContentCapture() }

// ResolveInstallGitHook reports whether to ambient-install the SL-5
// prepare-commit-msg hook on SessionStart (default false; env overrides).
func ResolveInstallGitHook() bool { return devconfig.ResolveInstallGitHook() }

// ResolveFinops reports whether opt-in rollout usage extraction is enabled
// (STORY-SL7-C / SL-16 parity). Default false: the SessionEnd transcript_path is
// never opened for usage with it unset (byte-identical to the pre-SL7-C path). A
// SEPARATE flag from content_capture — finops on does NOT imply content egress
// (numbers only, INV-2).
func ResolveFinops() bool { return devconfig.ResolveFinops() }

// ResolveCoordinates resolves the NON-SECRET target coordinates (base URL + DID)
// with zero secret-store access — used by read-only previews.
func ResolveCoordinates() (baseURL, did string) { return devconfig.ResolveCoordinates() }

// ── STORY-SL7-B enforce-leg resolvers (thin bindings over the shared devconfig
//    contract; identical names/semantics to the Claude Code adapter). ──

// ResolveEnforce reports whether the developer runtime is in ENFORCE mode
// (E6-S1/ADR-0006; default false = observe). A config read error never turns
// enforcement on (INV-3 fail-safe).
func ResolveEnforce() bool { return devconfig.ResolveEnforce() }

// ResolveFailClosed reports the enforce FAILURE POLICY (E6-S3, OD9; default FALSE
// = fail-open — an org never becomes fail-closed by accident).
func ResolveFailClosed() bool { return devconfig.ResolveFailClosed() }

// ResolveTier2 reports whether the Tier-2 synchronous /evaluate escalation is on
// (E6-S10; DEFAULT FALSE, opt-in).
func ResolveTier2() bool { return devconfig.ResolveTier2() }

// ResolveSecretDetection reports whether Tier-1 local secret detection is on
// (E6-S9, OD-SYNC-10; DEFAULT TRUE, opt-out — the detection stays strictly local).
func ResolveSecretDetection() bool { return devconfig.ResolveSecretDetection() }

// ResolveFindings reports whether the Tier-3 findings loop is on (E6-S11; DEFAULT
// FALSE, opt-in — it is the first observe-path stdout writer).
func ResolveFindings() bool { return devconfig.ResolveFindings() }

// ResolveFindingsCursor resolves the findings-loop cursor state file path.
func ResolveFindingsCursor() string { return devconfig.ResolveFindingsCursor() }

// ResolveAgentID resolves the backend agent id for policy sync/staleness (E6-S8).
func ResolveAgentID() string { return devconfig.ResolveAgentID() }

// ResolveBackendURL resolves the openbox-backend CONTROL-PLANE base URL.
func ResolveBackendURL() string { return devconfig.ResolveBackendURL() }

// ResolveControlToken resolves the ORG control-plane credential (OD-SYNC-4): the
// OPENBOX_CONTROL_TOKEN env ONLY (never config, never the secret store).
func ResolveControlToken() string { return devconfig.ResolveControlToken() }

// ResolveBundlePath resolves the local policy-bundle path the in-process decider
// evaluates and `dev sync`/staleness read: OPENBOX_SIDECAR_BUNDLE env, else
// decision.DefaultBundlePath(). Enforce-specific (imports decision), so it stays in
// this adapter rather than the shared devconfig module.
func ResolveBundlePath() string {
	if p := os.Getenv(envSidecarBundle); p != "" {
		return p
	}
	return decision.DefaultBundlePath()
}

// ResolveEnforceTimeout resolves the hard per-call T1 decision budget: config
// first, env-if-parseable wins; <=0 yields 0 (⇒ decision.DefaultDecisionTimeout); a
// positive value over maxEnforceTimeout is clamped. Clamping is ADAPTER-OWNED (the
// devconfig resolver deliberately does not clamp) and DERIVED for Codex — see the
// derivation note on the clamp constants in enforce.go.
func ResolveEnforceTimeout() time.Duration {
	ms := devconfig.ResolveTimeoutMS(func(c DevConfig) int { return c.EnforceTimeoutMS }, envEnforceTimeout)
	if ms <= 0 {
		return 0
	}
	// Clamp in MILLISECONDS before the multiply so a near-max-int64 value can never
	// overflow time.Duration (G_SEC overflow-proof clamp, CC parity).
	if maxMS := int64(maxEnforceTimeout / time.Millisecond); int64(ms) > maxMS {
		return maxEnforceTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

// ResolveTier2Timeout resolves the in-binary budget for one Tier-2 escalation:
// config first, env-if-parseable wins; <=0 yields defaultTier2Timeout; clamped to
// maxTier2Timeout (the Codex whole-hook wall-clock bound — see enforce_tier2.go).
func ResolveTier2Timeout() time.Duration {
	ms := devconfig.ResolveTimeoutMS(func(c DevConfig) int { return c.Tier2TimeoutMS }, envTier2Timeout)
	if ms <= 0 {
		return defaultTier2Timeout
	}
	if maxMS := int64(maxTier2Timeout / time.Millisecond); int64(ms) > maxMS {
		return maxTier2Timeout
	}
	return time.Duration(ms) * time.Millisecond
}
