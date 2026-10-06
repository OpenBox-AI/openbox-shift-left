package claudecode

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
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

// Credentials is the resolved runtime identity for the hook binary; the shape
// is shared by every adapter (hookflow.Credentials).
type Credentials = hookflow.Credentials

// ResolveIdentity resolves only the developer DID; no secret-store access
// (INV-1: zero secret I/O on the hot path).
func ResolveIdentity() (Identity, error) { return hookflow.ResolveIdentity() }

// ResolveCredentials assembles Credentials via the shared resolver: secrets
// from the environment then this tool's ~/.openbox/<tool>/.env, coordinates
// from the environment then dev.json.
func ResolveCredentials() (Credentials, error) { return hookflow.ResolveCredentials() }

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

// ResolveEnforce reports whether the developer runtime is in enforce mode.
func ResolveEnforce() bool { return devconfig.ResolveEnforce() }

// ResolveFailClosed reports the enforce failure policy (default false = fail-
// open).
func ResolveFailClosed() bool { return devconfig.ResolveFailClosed() }

// ResolveTier2 reads the deprecated, inert `tier2` key. See
// devconfig.ResolveTier2 for why an explicit false is deliberately not
// honoured.
func ResolveTier2() bool { return devconfig.ResolveTier2() }
