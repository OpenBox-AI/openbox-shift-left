package claudecode

import (
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Credential resolution for the hook binary. INV-1: the obx_ key and signing
// key are read straight into the client and never logged, printed, or placed
// on an argv.
const (
	envBaseURL         = devconfig.EnvBaseURL
	envDID             = devconfig.EnvDID
	envContentCapture  = devconfig.EnvContentCapture
	envFinops          = devconfig.EnvFinops
	envRealtime        = devconfig.EnvRealtime
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
	envAgentPrivateKey = devconfig.EnvAgentPrivateKey
	envConfigPath      = devconfig.EnvConfigPath
	envAgentID         = devconfig.EnvAgentID
	envBackendURL      = devconfig.EnvBackendURL
	envControlToken    = devconfig.EnvControlToken
	envSidecarBundle   = "OPENBOX_SIDECAR_BUNDLE" // decision-bundle override; enforce-only, not part of the shared contract
	envStaleDir        = "OPENBOX_STALE_DIR"

	defaultBaseURL = devconfig.DefaultBaseURL
)

// DevConfig is the shared non-secret coordinate file contract (see
// devconfig.DevConfig).
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

// ResolveIdentity resolves only the developer DID (env, then config file); no
// secret-store access (INV-1 + NFR-2: zero secret I/O on the hot path).
func ResolveIdentity() (Identity, error) {
	did, err := devconfig.ResolveDID()
	if err != nil {
		return Identity{}, err
	}
	return Identity{DeveloperDID: did}, nil
}

// ResolveCoordinates resolves the NON-secret target coordinates (base URL +
// DID) with zero secret-store access; backs `dev verify --dry-run`.
func ResolveCoordinates() (baseURL, did string) { return devconfig.ResolveCoordinates() }

// DefaultSpoolDir is where hot-path events are spooled before flush.
func DefaultSpoolDir() string { return devconfig.SpoolDir("cc-spool") }

// ResolveInstallGitHook reports whether to install the prepare-commit-msg hook
// on SessionStart (default false; env overrides config).
func ResolveInstallGitHook() bool { return devconfig.ResolveInstallGitHook() }

// ResolveFinops reports whether transcript usage extraction is enabled.
func ResolveFinops() bool { return devconfig.ResolveFinops() }

// ResolveContentCapture reports the org content posture (default on, opt-out
// via config false / env 0).
func ResolveContentCapture() bool { return devconfig.ResolveContentCapture() }

// ResolveSecretDetection reports whether local secret detection is on (default
// true, opt-out).
func ResolveSecretDetection() bool { return devconfig.ResolveSecretDetection() }

// ResolveFindings reports whether the findings loop is on.
func ResolveFindings() bool { return devconfig.ResolveFindings() }

// ResolveFindingsCursor resolves the findings-loop cursor state file path.
func ResolveFindingsCursor() string { return devconfig.ResolveFindingsCursor("claude-code") }

// ResolveEnforce reports whether the developer runtime is in enforce mode.
func ResolveEnforce() bool { return devconfig.ResolveEnforce() }

// maxEnforceTimeout correctness bound, not a nicety: Claude Code kills the
// PreToolUse hook at 5s, and a hook-kill lets the tool proceed; a CC-layer
// fail-open that would silently defeat a fail-closed org.
const maxEnforceTimeout = 2 * time.Second

// ResolveFailClosed reports the enforce failure policy (default false = fail-
// open).
func ResolveFailClosed() bool { return devconfig.ResolveFailClosed() }

// ResolveTier2 reads the deprecated, inert `tier2` key. See
// devconfig.ResolveTier2 for why an explicit false is deliberately not
// honoured.
func ResolveTier2() bool { return devconfig.ResolveTier2() }

// ResolveEvaluationTimeout resolves the in-binary budget for one /evaluate
// escalation: config first, env-if-parseable wins; <=0 yields
// defaultEvaluationTimeout; clamped to maxEvaluationTimeout (the CC 5s-hook-
// kill bound).
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

// ResolveCredentials assembles Credentials through the shared resolver:
// secrets from the environment then ~/.openbox/.env, coordinates from the
// environment then dev.json. It returns an error (never a panic) when identity
// is incomplete; the caller logs it fail-open and exits 0 (INV-3).
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

func firstNonEmpty(vals ...string) string { return devconfig.FirstNonEmpty(vals...) }

func isTruthy(s string) bool { return devconfig.IsTruthy(s) }
