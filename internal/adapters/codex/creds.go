package codex

import (
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

const (
	envEnforcementFile = devconfig.EnvEnforcementFile
	envTier2Timeout    = devconfig.EnvTier2Timeout
)

// DevConfig is the shared non-secret coordinate file contract.
type DevConfig = devconfig.DevConfig

// DefaultConfigPath is where the installer writes the dev config and the hook
// looks for it when OPENBOX_CONFIG is unset.
func DefaultConfigPath() string { return devconfig.DefaultConfigPath() }

// Credentials is the resolved runtime identity for the hook binary.
type Credentials struct {
	BaseURL string
	APIKey  string
	// DID is the in-memory attribution label, derived from AgentID; never
	// itself a store value.
	DID                   string
	AgentID               string
	WorkloadPrivateKey    string
	TokenCachePath        string
	ContentCaptureEnabled bool
}

// Identity is the non-secret projection used by the Mapper.
func (c Credentials) Identity() Identity { return Identity{DeveloperDID: c.DID} }

// NewClient builds the v3 workload-authenticated transport from the resolved
// credentials.
func (c Credentials) NewClient(logger client.Logger) (*client.Client, error) {
	return client.New(client.Config{
		BaseURL:               c.BaseURL,
		APIKey:                c.APIKey,
		WorkloadPrivateKey:    c.WorkloadPrivateKey,
		TokenCachePath:        c.TokenCachePath,
		ContentCaptureEnabled: c.ContentCaptureEnabled,
		Logger:                logger,
		// Every hook client -- the flusher's, the gate's own escalation --
		// gets exactly one attempt at the wire too: a retry here would race
		// DrainSession's own single-attempt accounting. The git action's
		// client is unrelated and keeps the library default.
		MaxRetries: &zeroRetries,
	})
}

// zeroRetries makes MaxRetries: 0 addressable; client.Config.MaxRetries is a
// *int precisely so an explicit zero is expressible (unset is the library
// default, defaultMaxRetries).
var zeroRetries = 0

// ResolveIdentity resolves only the developer DID (env, then config file); no
// secret-store access (INV-1: zero secret I/O on the hot path).
func ResolveIdentity() (Identity, error) {
	did, err := devconfig.ResolveDID()
	if err != nil {
		return Identity{}, err
	}
	return Identity{DeveloperDID: did}, nil
}

// ResolveCredentials assembles Credentials via the shared resolver: secrets
// from the environment then this tool's ~/.openbox/<tool>/.env, coordinates from the environment
// then dev.json.
func ResolveCredentials() (Credentials, error) {
	dc, err := devconfig.ResolveCredentials()
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		BaseURL:               dc.BaseURL,
		APIKey:                dc.APIKey,
		DID:                   dc.DID,
		AgentID:               dc.AgentID,
		WorkloadPrivateKey:    dc.WorkloadPrivateKey,
		TokenCachePath:        dc.TokenCachePath,
		ContentCaptureEnabled: dc.ContentCaptureEnabled,
	}, nil
}

// DefaultSpoolDir is where hot-path events are spooled before flush; a codex-
// specific subdir so a machine running Claude Code AND Codex never cross-
// drains spools.
func DefaultSpoolDir() string { return devconfig.SpoolDir("codex-spool") }

// ResolveContentCapture reports the org content posture (default on, opt-out
// via `content_capture:false` / OPENBOX_CONTENT_CAPTURE=0).
func ResolveContentCapture() bool { return devconfig.ResolveContentCapture() }

// ResolveInstallGitHook reports whether to ambient-install the prepare-commit-
// msg hook on SessionStart (default false; env overrides).
func ResolveInstallGitHook() bool { return devconfig.ResolveInstallGitHook() }

// ResolveFinops reports whether rollout usage extraction is enabled. Default
// false: the SessionEnd transcript_path is never opened for usage with it
// unset.
func ResolveFinops() bool { return devconfig.ResolveFinops() }

// ResolveCoordinates resolves the non-secret target coordinates (base URL +
// DID) with zero secret-store access; used by read-only previews.
func ResolveCoordinates() (baseURL, did string) { return devconfig.ResolveCoordinates() }

// ResolveEnforce reports whether the developer runtime is in enforce mode. A
// config read error never turns enforcement on (INV-3 fail-safe).
func ResolveEnforce() bool { return devconfig.ResolveEnforce() }

// ResolveFailClosed reports the enforce failure policy (default false = fail-
// open; an org never becomes fail-closed by accident).
func ResolveFailClosed() bool { return devconfig.ResolveFailClosed() }

// ResolveTier2 reads the deprecated, inert `tier2` key. See
// devconfig.ResolveTier2 for why an explicit false is deliberately not
// honoured.
func ResolveTier2() bool { return devconfig.ResolveTier2() }

// ResolveSecretDetection reports whether local secret detection is on (default
// true, opt-out; the detection stays strictly local).
func ResolveSecretDetection() bool { return devconfig.ResolveSecretDetection() }

// ResolveFindings reports whether the findings loop is on (default false, opt-
// in; it is the first observe-path stdout writer).
func ResolveFindings() bool { return devconfig.ResolveFindings() }

// ResolveFindingsCursor resolves the findings-loop cursor state file path.
func ResolveFindingsCursor() string { return devconfig.ResolveFindingsCursor(provider) }

// ResolveAgentID resolves the backend agent id for policy sync/staleness.
func ResolveAgentID() string { return devconfig.ResolveAgentID() }

// ResolveBackendURL resolves the openbox-backend control-plane base URL.
func ResolveBackendURL() string { return devconfig.ResolveBackendURL() }

// ResolveControlToken resolves the org control-plane credential: the
// OPENBOX_CONTROL_TOKEN: the environment first, then the org-level
// ~/.openbox/.env that `openbox auth` writes. Never a config field and never a
// per-tool secret store, so a compromised tool store cannot supply a
// fleet-wide credential.
func ResolveControlToken() string { return devconfig.ResolveControlToken() }

// ResolveOrgSigningKey returns the org's pinned policy-bundle signing key
// (base64 raw Ed25519) and its id, from the shared dev config (E8-S6).
func ResolveOrgSigningKey() (pubKeyB64, keyID string) { return devconfig.ResolveOrgSigningKey() }

// ResolveEvaluationTimeout resolves the in-binary budget for one escalation:
// config first, env-if-parseable wins; <=0 yields defaultEvaluationTimeout;
// clamped to maxEvaluationTimeout (the Codex whole-hook wall-clock bound; see
// enforce_tier2.go).
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
