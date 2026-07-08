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
//	OPENBOX_API_KEY            direct obx_ key override (CI/tests; discouraged in prod)
//	OPENBOX_ED25519_SEED       direct base64 seed override (CI/tests)
const (
	envBaseURL        = "OPENBOX_BASE_URL"
	envDID            = "OPENBOX_AGENT_DID"
	envSecretService  = "OPENBOX_SECRET_SERVICE"
	envAPIKeyAccount  = "OPENBOX_API_KEY_ACCOUNT"
	envPrivKeyAccount = "OPENBOX_PRIVATE_KEY_ACCOUNT"
	envContentCapture = "OPENBOX_CONTENT_CAPTURE"
	envAPIKeyDirect   = "OPENBOX_API_KEY"
	envSeedDirect     = "OPENBOX_ED25519_SEED"
	envConfigPath     = "OPENBOX_CONFIG"

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

	// obx_ key: direct override, else secret store.
	if v := os.Getenv(envAPIKeyDirect); v != "" {
		c.APIKey = v
	} else if service != "" && apiKeyAccount != "" {
		v, err := secretLookup(service, apiKeyAccount)
		if err != nil {
			return Credentials{}, fmt.Errorf("read api key from secret store: %w", err)
		}
		c.APIKey = v
	}
	if c.APIKey == "" {
		return Credentials{}, fmt.Errorf("no obx_ API key available (env %s or secret store)", envAPIKeyDirect)
	}

	// Ed25519 seed: direct override, else secret store.
	if v := os.Getenv(envSeedDirect); v != "" {
		c.SeedB64 = v
	} else if service != "" && privKeyAccount != "" {
		v, err := secretLookup(service, privKeyAccount)
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
