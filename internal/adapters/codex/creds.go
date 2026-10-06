package codex

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

const envEnforcementFile = devconfig.EnvEnforcementFile

// DevConfig is the shared non-secret coordinate file contract.
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

// ResolveEnforce reports whether the developer runtime is in enforce mode. A
// config read error never turns enforcement on (INV-3 fail-safe).
func ResolveEnforce() bool { return devconfig.ResolveEnforce() }

// ResolveSecretDetection reports whether local secret detection is on (default
// true, opt-out; the detection stays strictly local).
func ResolveSecretDetection() bool { return devconfig.ResolveSecretDetection() }

// ResolveFindings reports whether the findings loop is on (default false, opt-
// in; it is the first observe-path stdout writer).
func ResolveFindings() bool { return devconfig.ResolveFindings() }
