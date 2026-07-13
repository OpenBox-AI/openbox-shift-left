package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Credential resolution for the hook binary. Identity is minted by
// `openbox dev init` (STORY-SL-2) and stored in the OS secret store; the hook
// reads it here. INV-1: the obx_ key and Ed25519 seed are read straight into the
// client and NEVER logged, printed, or placed on an argv. The secret store is
// queried by (service, account) coordinates passed via env (the plugin's hook
// wiring sets them); the secret VALUES never travel through env or argv on the
// read path — they come back on the child process's stdout only.
//
// Env used (non-secret coordinates + optional direct override for CI/tests):
//
//	OPENBOX_BASE_URL           core base (default https://core.openbox.ai)
//	OPENBOX_AGENT_DID          did:aip:… (non-secret)
//	OPENBOX_SECRET_SERVICE     secret-store service namespace
//	OPENBOX_API_KEY_ACCOUNT    account holding the obx_ key
//	OPENBOX_PRIVATE_KEY_ACCOUNT account holding the base64 Ed25519 seed
//	OPENBOX_CONTENT_CAPTURE    "1"/"true" to opt into content (default: metadata-only)
//	OPENBOX_FINOPS             "1"/"true" to opt into transcript usage extraction (default: off)
//	OPENBOX_API_KEY            direct obx_ key override (CI/tests; discouraged in prod)
//	OPENBOX_ED25519_SEED       direct base64 seed override (CI/tests)
const (
	envBaseURL        = "OPENBOX_BASE_URL"
	envDID            = "OPENBOX_AGENT_DID"
	envSecretService  = "OPENBOX_SECRET_SERVICE"
	envAPIKeyAccount  = "OPENBOX_API_KEY_ACCOUNT"
	envPrivKeyAccount = "OPENBOX_PRIVATE_KEY_ACCOUNT"
	envContentCapture = "OPENBOX_CONTENT_CAPTURE"
	envFinops         = "OPENBOX_FINOPS"
	envInstallGitHook = "OPENBOX_INSTALL_GIT_HOOK"
	envEnforce        = "OPENBOX_ENFORCE"
	envSidecarSocket  = "OPENBOX_SIDECAR_SOCKET"
	envAPIKeyDirect   = "OPENBOX_API_KEY"
	envSeedDirect     = "OPENBOX_ED25519_SEED"
	envConfigPath     = "OPENBOX_CONFIG"
	envSecretFile     = "OPENBOX_SECRET_FILE"

	defaultBaseURL = "https://core.openbox.ai"
)

// DevConfig is the non-secret coordinate file the installer writes and the hook
// reads (INV-1: it holds where the secrets live, never the secret values). Env
// vars of the same meaning override any field.
type DevConfig struct {
	BaseURL           string `json:"base_url,omitempty"`
	DID               string `json:"developer_did,omitempty"`
	SecretService     string `json:"secret_service,omitempty"`
	APIKeyAccount     string `json:"api_key_account,omitempty"`
	PrivateKeyAccount string `json:"private_key_account,omitempty"`
	ContentCapture    bool   `json:"content_capture,omitempty"`
	// Finops enables opt-in transcript usage extraction (STORY-SL-16, OD-FINOPS):
	// on SessionEnd flush the hook reads the session's transcript_path and pulls
	// usage NUMBERS ONLY (tokens; cost when the transcript carries it) onto the
	// SessionEnded event's Tokens/Cost. Default false — opening transcript_path is
	// a content-bearing read, so it is gated behind an EXPLICIT opt-in that is
	// DELIBERATELY SEPARATE from ContentCapture: content_capture authorizes full
	// prompt/output/file-text egress, a far broader posture than "read the file to
	// extract integers" (OD-FINOPS gate-design ruling). INV-2 is preserved by a
	// projection-only parser (numbers cannot carry content); no content is ever
	// egressed regardless of this flag. Phase-1: set via this config field or the
	// OPENBOX_FINOPS env (ResolveFinops). Threading a `dev init --finops` flag
	// through the shared provider CredentialRef is a follow-up (mirrors how
	// --install-git-hook was wired post-SL-5), out of SL-16's adapter-only scope.
	Finops bool `json:"finops,omitempty"`
	// InstallGitHook enables ambient install of the SL-5 prepare-commit-msg hook
	// into the session's repo on SessionStart. Default false — it modifies a
	// repo's .git/hooks. Set by `openbox dev init --install-git-hook`.
	InstallGitHook bool `json:"install_git_hook,omitempty"`
	// Enforce flips the developer runtime from observe/advisory to ENFORCE
	// (STORY-E6-S1, Phase-2). Default false — the whole of Phase-1 stays observe.
	// When true, the PreToolUse hook SYNCHRONOUSLY obtains a governance decision
	// from the local sidecar (sidecar.Client) BEFORE the tool runs (the INV-3b
	// pre-execution gate, bounded ~50ms, fail-open). E6-S1 only OBTAINS + records
	// the decision; turning a BLOCK/HALT verdict into an actual CC `deny`/`ask` is
	// E6-S2's apply. OPENBOX_ENFORCE overrides this either way (ResolveEnforce).
	Enforce bool `json:"enforce,omitempty"`
	// SidecarSocket overrides the Unix socket the enforce hook dials (default:
	// sidecar.DefaultSocketPath()). The OPENBOX_SIDECAR_SOCKET env overrides it —
	// the SAME env `openbox sidecar serve` reads, so the daemon and the hook agree.
	SidecarSocket string `json:"sidecar_socket,omitempty"`
	// SecretFile, when set, points at the CLI's opt-in file secret backend
	// (`openbox dev init --secret-backend file`) — a 0600 JSON store the hook
	// reads instead of the OS keychain, for machines with no keyring. The
	// OPENBOX_SECRET_FILE env overrides it. Empty ⇒ use the OS secret store.
	SecretFile string `json:"secret_file,omitempty"`
}

// DefaultConfigPath is where the installer writes the dev config and the hook
// looks for it when OPENBOX_CONFIG is unset. Under XDG on Linux / the standard
// config dir elsewhere.
func DefaultConfigPath() string {
	if p := os.Getenv(envConfigPath); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "dev.json")
}

// loadDevConfig reads the dev config if present. A missing file is not an error
// (env may supply everything); a malformed file is an error surfaced fail-open.
func loadDevConfig(path string) (DevConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DevConfig{}, nil
		}
		return DevConfig{}, fmt.Errorf("read dev config: %w", err)
	}
	var c DevConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return DevConfig{}, fmt.Errorf("parse dev config %s: %w", path, err)
	}
	return c, nil
}

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

// secretLookup is the OS secret-store reader; overridable in tests. It mirrors
// the backends `openbox dev init` (STORY-SL-2) wrote with.
var secretLookup = osSecretLookup

// ResolveIdentity resolves ONLY the developer DID (env, then config file) — no
// secret-store access. This is the hot path: Observe/spool needs the DID to
// attribute events but never the obx_ key or seed, so a PreToolUse/PostToolUse
// hook does zero secret I/O (INV-1 + NFR-2 latency).
func ResolveIdentity() (Identity, error) {
	cfg, err := loadDevConfig(DefaultConfigPath())
	if err != nil {
		return Identity{}, err
	}
	did := firstNonEmpty(os.Getenv(envDID), cfg.DID)
	if did == "" {
		return Identity{}, fmt.Errorf("no developer DID configured (run `openbox dev init`)")
	}
	return Identity{DeveloperDID: did}, nil
}

// ResolveCoordinates resolves the NON-SECRET target coordinates — the core base
// URL and the developer DID — from env then the dev config, applying the same
// precedence and default as ResolveCredentials but with ZERO secret-store access
// (INV-1). It backs read-only previews such as `openbox dev verify --dry-run`,
// which must show what WOULD be called without reading the obx_ key or seed. A
// missing/unreadable config degrades to env + defaults (best-effort display);
// did is "" when nothing configures it (the caller shows "not configured").
func ResolveCoordinates() (baseURL, did string) {
	cfg, _ := loadDevConfig(DefaultConfigPath())
	baseURL = firstNonEmpty(os.Getenv(envBaseURL), cfg.BaseURL, defaultBaseURL)
	did = firstNonEmpty(os.Getenv(envDID), cfg.DID)
	return baseURL, did
}

// DefaultSpoolDir is where hot-path events are spooled before flush. Override
// with OPENBOX_SPOOL_DIR (tests use a temp dir).
func DefaultSpoolDir() string {
	if p := os.Getenv("OPENBOX_SPOOL_DIR"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "cc-spool")
}

// ResolveInstallGitHook reports whether the adapter should install the SL-5
// prepare-commit-msg hook into the session's repo on SessionStart. Default false
// (it modifies a repo's .git/hooks). The dev config's install_git_hook (written
// by `openbox dev init --install-git-hook`) enables it; OPENBOX_INSTALL_GIT_HOOK
// overrides either way. A missing/unreadable config is treated as false
// (fail-safe) — this never blocks or fails a session.
func ResolveInstallGitHook() bool {
	enabled := false
	if cfg, err := loadDevConfig(DefaultConfigPath()); err == nil {
		enabled = cfg.InstallGitHook
	}
	if v, ok := os.LookupEnv(envInstallGitHook); ok {
		enabled = isTruthy(v)
	}
	return enabled
}

// ResolveFinops reports whether opt-in transcript usage extraction is enabled
// (STORY-SL-16, OD-FINOPS): config field first, then the OPENBOX_FINOPS env
// override (env wins), same precedence as every other coordinate. Default false:
// with it unset, transcript_path is NEVER opened and events carry no tokens/cost
// (byte-identical to pre-SL-16 output). No secret I/O — a cheap config+env read
// safe on the flush path. It is deliberately independent of ContentCapture (see
// DevConfig.Finops).
func ResolveFinops() bool {
	enabled := false
	if cfg, err := loadDevConfig(DefaultConfigPath()); err == nil {
		enabled = cfg.Finops
	}
	if v, ok := os.LookupEnv(envFinops); ok {
		enabled = isTruthy(v)
	}
	return enabled
}

// ResolveEnforce reports whether the developer runtime is in ENFORCE mode
// (STORY-E6-S1 / Phase-2): config field first, then the OPENBOX_ENFORCE env
// override (env wins), same precedence as every other coordinate. Default false
// — with it unset the runtime stays observe/advisory and the PreToolUse hot path
// NEVER dials the sidecar (byte-identical to Phase-1). No secret I/O: a cheap
// config+env read safe to call on the PreToolUse hot path. A missing/unreadable
// config is treated as false (fail-safe) — enforcement never turns itself on by
// accident, and a config read error never blocks a tool call (INV-3).
func ResolveEnforce() bool {
	enabled := false
	if cfg, err := loadDevConfig(DefaultConfigPath()); err == nil {
		enabled = cfg.Enforce
	}
	if v, ok := os.LookupEnv(envEnforce); ok {
		enabled = isTruthy(v)
	}
	return enabled
}

// ResolveSidecarSocket resolves the Unix socket path the enforce hook dials: the
// OPENBOX_SIDECAR_SOCKET env first, then the dev config's sidecar_socket, else ""
// (the caller lets sidecar.DefaultSocketPath() decide, so the daemon and the hook
// agree without configuration). No secret I/O; a missing/unreadable config
// degrades to env-or-empty. Empty is the normal case (use the default path).
func ResolveSidecarSocket() string {
	cfg, _ := loadDevConfig(DefaultConfigPath())
	return firstNonEmpty(os.Getenv(envSidecarSocket), cfg.SidecarSocket)
}

// ResolveCredentials assembles Credentials from env + the OS secret store. It
// returns an error (never a panic) when identity is incomplete; the caller logs
// it fail-open and exits 0 (INV-3) — a missing identity must never block a tool
// call. No secret value is ever included in a returned error.
func ResolveCredentials() (Credentials, error) {
	// Config file supplies the coordinates; each env var overrides its field.
	cfg, err := loadDevConfig(DefaultConfigPath())
	if err != nil {
		return Credentials{}, err
	}

	c := Credentials{
		BaseURL: firstNonEmpty(os.Getenv(envBaseURL), cfg.BaseURL, defaultBaseURL),
		DID:     firstNonEmpty(os.Getenv(envDID), cfg.DID),
		// Content capture: config default, overridable EITHER way by env (so env
		// can disable what config enabled — consistent with the other coordinates).
		ContentCaptureEnabled: cfg.ContentCapture,
	}
	if v, ok := os.LookupEnv(envContentCapture); ok {
		c.ContentCaptureEnabled = isTruthy(v)
	}
	if c.DID == "" {
		return Credentials{}, fmt.Errorf("no developer DID configured (run `openbox dev init`)")
	}

	service := firstNonEmpty(os.Getenv(envSecretService), cfg.SecretService)
	apiKeyAccount := firstNonEmpty(os.Getenv(envAPIKeyAccount), cfg.APIKeyAccount)
	privKeyAccount := firstNonEmpty(os.Getenv(envPrivKeyAccount), cfg.PrivateKeyAccount)

	// Secret source: the CLI's opt-in file backend (when a path is configured)
	// or the OS secret store. The direct env overrides win over both.
	lookup := secretLookup
	if secretFile := firstNonEmpty(os.Getenv(envSecretFile), cfg.SecretFile); secretFile != "" {
		lookup = func(svc, acct string) (string, error) { return fileSecretLookup(secretFile, svc, acct) }
	}

	// obx_ key: direct override, else secret source.
	if v := os.Getenv(envAPIKeyDirect); v != "" {
		c.APIKey = v
	} else if service != "" && apiKeyAccount != "" {
		v, err := lookup(service, apiKeyAccount)
		if err != nil {
			return Credentials{}, fmt.Errorf("read api key from secret store: %w", err)
		}
		c.APIKey = v
	}
	if c.APIKey == "" {
		return Credentials{}, fmt.Errorf("no obx_ API key available (env %s or secret store)", envAPIKeyDirect)
	}

	// Ed25519 seed: direct override, else secret source.
	if v := os.Getenv(envSeedDirect); v != "" {
		c.SeedB64 = v
	} else if service != "" && privKeyAccount != "" {
		v, err := lookup(service, privKeyAccount)
		if err != nil {
			return Credentials{}, fmt.Errorf("read signing seed from secret store: %w", err)
		}
		c.SeedB64 = v
	}
	if c.SeedB64 == "" {
		return Credentials{}, fmt.Errorf("no Ed25519 seed available (env %s or secret store)", envSeedDirect)
	}

	return c, nil
}

// fileSecretLookup reads one secret by (service, account) from the CLI's opt-in
// file backend — the same 0600 nested-JSON format the CLI writes
// (cli/internal/secret/file.go): {"<service>":{"<account>":"<value>"}}. It is
// used only when a secret-file path is explicitly configured; the default path
// stays the OS keychain (osSecretLookup). A missing file/account is ErrNotFound-
// shaped (an error), handled fail-open by the caller.
func fileSecretLookup(path, service, account string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	var m map[string]map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("parse secret file %s: %w", path, err)
	}
	if v, ok := m[service][account]; ok && v != "" {
		return v, nil
	}
	return "", fmt.Errorf("secret file has no value for account %q", account)
}

// osSecretLookup reads one secret by (service, account) from the platform secret
// store, matching what STORY-SL-2 wrote: libsecret (secret-tool) on Linux, the
// login keychain (security) on macOS. The value returns on the child's stdout;
// it is never logged. (macOS keychain hardening is tracked as SL2-SEC-1.)
func osSecretLookup(service, account string) (string, error) {
	// Reject leading-dash coordinates so a crafted config/env value can't be
	// reparsed as a flag by the backend CLI (G_SEC F3). argv (not a shell) is
	// used, so there is no shell-injection surface; this closes arg-injection.
	if strings.HasPrefix(service, "-") || strings.HasPrefix(account, "-") {
		return "", fmt.Errorf("secret-store coordinate must not start with '-'")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w")
	case "linux":
		cmd = exec.Command("secret-tool", "lookup", "service", service, "account", account)
	default:
		return "", fmt.Errorf("no secret-store backend for %s", runtime.GOOS)
	}
	out, err := cmd.Output()
	if err != nil {
		// Do not wrap stderr (it could echo the account); keep the error opaque.
		return "", fmt.Errorf("secret-store lookup failed for account %q", account)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
