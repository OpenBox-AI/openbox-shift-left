package claudecode

import (
	"os"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Credential resolution for the hook binary. The provider-neutral
// config/credential machinery — the dev.json `DevConfig` contract, the
// Resolve* precedence rules, and the OS/file secret-store readers — lives
// in the shared module adapters/common/devconfig, consumed by every
// adapter (ADR-0007). This file is the Claude Code adapter's thin,
// behavior-preserving facade over it: every symbol below keeps its
// pre-extraction name, signature, and semantics (the full original
// documentation now lives on the devconfig definitions). Only the
// enforce-budget clamps stay here — they encode Claude Code's own
// correctness bound (the 5s hook kill), which is provider-specific.
//
// Identity is minted by `openbox dev init` and stored in the OS secret
// store; the hook reads it here. INV-1: the obx_ key and Ed25519 seed are
// read straight into the client and never logged, printed, or placed on an
// argv. See devconfig for the full env/config contract.
const (
	envBaseURL         = devconfig.EnvBaseURL
	envDID             = devconfig.EnvDID
	envSecretService   = devconfig.EnvSecretService
	envAPIKeyAccount   = devconfig.EnvAPIKeyAccount
	envPrivKeyAccount  = devconfig.EnvPrivKeyAccount
	envContentCapture  = devconfig.EnvContentCapture
	envFinops          = devconfig.EnvFinops
	envInstallGitHook  = devconfig.EnvInstallGitHook
	envEnforce         = devconfig.EnvEnforce
	envFailClosed      = devconfig.EnvFailClosed
	envEnforceTimeout  = devconfig.EnvEnforceTimeout
	envTier2           = devconfig.EnvTier2
	envTier2Timeout    = devconfig.EnvTier2Timeout
	envSecretDetection = devconfig.EnvSecretDetection
	envFindings        = devconfig.EnvFindings
	envFindingsCursor  = devconfig.EnvFindingsCursor
	envEnforcementFile = devconfig.EnvEnforcementFile
	envAPIKeyDirect    = devconfig.EnvAPIKeyDirect
	envSeedDirect      = devconfig.EnvSeedDirect
	envConfigPath      = devconfig.EnvConfigPath
	envSecretFile      = devconfig.EnvSecretFile
	envAgentID         = devconfig.EnvAgentID
	envBackendURL      = devconfig.EnvBackendURL
	envControlToken    = devconfig.EnvControlToken
	envSidecarBundle   = "OPENBOX_SIDECAR_BUNDLE" // decision-bundle override; enforce-only, not part of the shared contract
	envStaleDir        = "OPENBOX_STALE_DIR"

	defaultBaseURL = devconfig.DefaultBaseURL
)

// DevConfig is the shared non-secret coordinate file contract (see
// devconfig.DevConfig). Aliased so the installer and every existing caller keep
// compiling unchanged.
type DevConfig = devconfig.DevConfig

// DefaultConfigPath is where the installer writes the dev config and the hook
// looks for it when OPENBOX_CONFIG is unset.
func DefaultConfigPath() string { return devconfig.DefaultConfigPath() }

// loadDevConfig reads the dev config if present (missing file is not an error).
func loadDevConfig(path string) (DevConfig, error) { return devconfig.Load(path) }

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

// secretLookup is the OS secret-store reader; overridable in tests. It
// mirrors the backends `openbox dev init` wrote with.
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

// ResolveCoordinates resolves the NON-SECRET target coordinates (base URL +
// DID) with zero secret-store access — backs `dev verify --dry-run`.
func ResolveCoordinates() (baseURL, did string) { return devconfig.ResolveCoordinates() }

// DefaultSpoolDir is where hot-path events are spooled before flush. Override
// with OPENBOX_SPOOL_DIR (tests use a temp dir).
func DefaultSpoolDir() string { return devconfig.SpoolDir("cc-spool") }

// ResolveInstallGitHook reports whether to install the prepare-commit-msg
// hook on SessionStart (default false; env overrides config).
func ResolveInstallGitHook() bool { return devconfig.ResolveInstallGitHook() }

// ResolveFinops reports whether opt-in transcript usage extraction is
// enabled (default false).
func ResolveFinops() bool { return devconfig.ResolveFinops() }

// ResolveContentCapture reports the org content posture (default on,
// opt-out via config false / env 0).
func ResolveContentCapture() bool { return devconfig.ResolveContentCapture() }

// ResolveSecretDetection reports whether Tier-1 local secret detection is
// on (default true, opt-out).
func ResolveSecretDetection() bool { return devconfig.ResolveSecretDetection() }

// ResolveFindings reports whether the Tier-3 findings loop is on (default
// false, opt-in).
func ResolveFindings() bool { return devconfig.ResolveFindings() }

// ResolveFindingsCursor resolves the findings-loop cursor state file path.
func ResolveFindingsCursor() string { return devconfig.ResolveFindingsCursor() }

// ResolveEnforce reports whether the developer runtime is in enforce mode
// (ADR-0006; default false = observe).
func ResolveEnforce() bool { return devconfig.ResolveEnforce() }

// maxEnforceTimeout caps the configurable enforce decision budget. It is a
// correctness bound, not a nicety: Claude Code kills the PreToolUse hook at
// 5s, and a hook-kill lets the tool proceed — a CC-layer fail-open that
// would silently defeat a fail-closed org. Clamping the whole enforce wait
// to 2s keeps the full hook (config read + gate + spool) under that kill
// so a fail-closed deny is actually delivered, and keeps INV-3b bounded.
// Provider-specific — deliberately not moved into devconfig.
const maxEnforceTimeout = 2 * time.Second

// ResolveFailClosed reports the enforce failure policy (default false =
// fail-open).
func ResolveFailClosed() bool { return devconfig.ResolveFailClosed() }

// ResolveEnforceTimeout resolves the hard per-call decision budget the
// enforce hook allows the local decider: config first, env-if-parseable
// wins; <=0 yields 0 (⇒ decision.DefaultDecisionTimeout); a positive value
// over maxEnforceTimeout is clamped (INV-3b bounded + under CC's 5s kill).
func ResolveEnforceTimeout() time.Duration {
	ms := devconfig.ResolveTimeoutMS(func(c DevConfig) int { return c.EnforceTimeoutMS }, envEnforceTimeout)
	if ms <= 0 {
		return 0 // ⇒ decision.DefaultDecisionTimeout
	}
	// Clamp in milliseconds before the multiply so a near-max-int64 value
	// can never overflow time.Duration (which would wrap to a
	// negative/huge duration). Bounded by construction, not by accident.
	if maxMS := int64(maxEnforceTimeout / time.Millisecond); int64(ms) > maxMS {
		return maxEnforceTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

// ResolveTier2 reports whether the Tier-2 synchronous /evaluate escalation
// is on (default false, opt-in).
func ResolveTier2() bool { return devconfig.ResolveTier2() }

// ResolveTier2Timeout resolves the in-binary budget for one Tier-2
// /evaluate escalation: config first, env-if-parseable wins; <=0 yields
// defaultTier2Timeout; clamped to maxTier2Timeout (the CC 5s-hook-kill
// bound).
func ResolveTier2Timeout() time.Duration {
	ms := devconfig.ResolveTimeoutMS(func(c DevConfig) int { return c.Tier2TimeoutMS }, envTier2Timeout)
	if ms <= 0 {
		return defaultTier2Timeout
	}
	// Clamp in milliseconds before the multiply so a near-max-int64 value
	// can never overflow time.Duration (mirrors ResolveEnforceTimeout's
	// overflow-proof clamp).
	if maxMS := int64(maxTier2Timeout / time.Millisecond); int64(ms) > maxMS {
		return maxTier2Timeout
	}
	return time.Duration(ms) * time.Millisecond
}

// ResolveAgentID resolves the backend agent id for policy sync/staleness.
// Empty when unconfigured.
func ResolveAgentID() string { return devconfig.ResolveAgentID() }

// ResolveBackendURL resolves the openbox-backend control-plane base URL.
func ResolveBackendURL() string { return devconfig.ResolveBackendURL() }

// ResolveControlToken resolves the org control-plane credential: the
// OPENBOX_CONTROL_TOKEN env only (never config, never the secret store).
func ResolveControlToken() string { return devconfig.ResolveControlToken() }

// ResolveBundlePath resolves the local policy-bundle path the in-process
// decider evaluates and `dev sync`/staleness read: OPENBOX_SIDECAR_BUNDLE env,
// else decision.DefaultBundlePath(). Enforce-specific (imports decision), so it
// stays in this adapter rather than the shared devconfig module.
func ResolveBundlePath() string {
	if p := os.Getenv(envSidecarBundle); p != "" {
		return p
	}
	return decision.DefaultBundlePath()
}

// ResolveCredentials assembles Credentials from env + the secret store via
// the shared resolver, threading this adapter's injectable secretLookup
// (the test seam). It returns an error (never a panic) when identity is
// incomplete; the caller logs it fail-open and exits 0 (INV-3). No secret
// value is ever included in a returned error.
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

// osSecretLookup is the platform secret-store reader (shared implementation;
// kept under its original name for the adapter's tests).
func osSecretLookup(service, account string) (string, error) {
	return devconfig.OSSecretLookup(service, account)
}

func firstNonEmpty(vals ...string) string { return devconfig.FirstNonEmpty(vals...) }

func isTruthy(s string) bool { return devconfig.IsTruthy(s) }
