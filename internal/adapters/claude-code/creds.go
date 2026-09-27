package claudecode

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// INV-1: the obx_ key and signing key are read straight into the client and
// never logged, printed, or placed on an argv.
const (
	envBaseURL         = devconfig.EnvBaseURL
	envDID             = devconfig.EnvDID
	envContentCapture  = devconfig.EnvContentCapture
	envFinops          = devconfig.EnvFinops
	envRealtime        = devconfig.EnvRealtime
	envInstallGitHook  = devconfig.EnvInstallGitHook
	envEnforce         = devconfig.EnvEnforce
	envFailClosed      = devconfig.EnvFailClosed
	envTier2           = devconfig.EnvTier2
	envSecretDetection = devconfig.EnvSecretDetection
	envFindings        = devconfig.EnvFindings
	envFindingsCursor  = devconfig.EnvFindingsCursor
	envEnforcementFile = devconfig.EnvEnforcementFile
	envAPIKeyDirect    = devconfig.EnvAPIKeyDirect
	// envAgentPrivateKey is the legacy (v1) Ed25519 seed name; kept only as a
	// legacy-store fixture value in tests (devconfig.LegacyStoreFor's own
	// signal), never read by this adapter's resolver anymore.
	envAgentPrivateKey    = devconfig.EnvAgentPrivateKey
	envWorkloadPrivateKey = devconfig.EnvWorkloadPrivateKey
	envAgentID            = devconfig.EnvAgentID
	envConfigPath         = devconfig.EnvConfigPath

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
// secret-store access (INV-1, and zero secret I/O on the hot path).
func ResolveIdentity() (Identity, error) {
	did, err := devconfig.ResolveDID()
	if err != nil {
		return Identity{}, err
	}
	return Identity{DeveloperDID: did}, nil
}

// ResolveCoordinates resolves the NON-secret target coordinates (base URL +
// DID) with zero secret-store access.
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
func ResolveFindingsCursor() string { return devconfig.ResolveFindingsCursor(provider) }

// ResolveEnforce reports whether the developer runtime is in enforce mode.
func ResolveEnforce() bool { return devconfig.ResolveEnforce() }

// ResolveFailClosed reports the enforce failure policy (default false = fail-
// open).
func ResolveFailClosed() bool { return devconfig.ResolveFailClosed() }

// ResolveTier2 reads the deprecated, inert `tier2` key. See
// devconfig.ResolveTier2 for why an explicit false is deliberately not
// honoured.
func ResolveTier2() bool { return devconfig.ResolveTier2() }

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
// (base64 raw Ed25519) and its id, from the shared dev config.
func ResolveOrgSigningKey() (pubKeyB64, keyID string) { return devconfig.ResolveOrgSigningKey() }

// ResolveCredentials assembles Credentials through the shared resolver:
// secrets from the environment then this tool's ~/.openbox/<tool>/.env, coordinates from the
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
		AgentID:               dc.AgentID,
		WorkloadPrivateKey:    dc.WorkloadPrivateKey,
		TokenCachePath:        dc.TokenCachePath,
		ContentCaptureEnabled: dc.ContentCaptureEnabled,
	}, nil
}
