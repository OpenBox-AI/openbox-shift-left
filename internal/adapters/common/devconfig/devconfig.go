// Package devconfig is the provider-neutral developer-runtime configuration
// and credential resolution shared by every tool adapter (Claude Code, Codex,
// Cursor). One store per field: the credential file is never read for a
// coordinate and dev.json never holds a secret. Before that decision the DID
// lived in both dev.json and the OS keychain, and a stale keychain entry
// silently reverted a corrected DID on the next install; this split is what
// makes that impossible rather than merely fixed.
package devconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const (
	EnvBaseURL         = "OPENBOX_BASE_URL"
	EnvDID             = "OPENBOX_AGENT_DID"
	EnvContentCapture  = "OPENBOX_CONTENT_CAPTURE"
	EnvFinops          = "OPENBOX_FINOPS"
	EnvTelemetry       = "OPENBOX_TELEMETRY"
	EnvInstallGitHook  = "OPENBOX_INSTALL_GIT_HOOK"
	EnvEnforce         = "OPENBOX_ENFORCE"
	EnvFailClosed      = "OPENBOX_FAIL_CLOSED"
	EnvEnforceTimeout  = "OPENBOX_ENFORCE_TIMEOUT_MS"
	EnvTier2           = "OPENBOX_TIER2"
	EnvTier2Timeout    = "OPENBOX_TIER2_TIMEOUT_MS"
	EnvApprovalHold    = "OPENBOX_APPROVAL_HOLD_MS"
	EnvSecretDetection = "OPENBOX_SECRET_DETECTION"
	EnvRequireVerified = "OPENBOX_REQUIRE_VERIFIED_BUNDLE"
	EnvFindings        = "OPENBOX_FINDINGS"
	EnvRealtime        = "OPENBOX_REALTIME"
	EnvFindingsCursor  = "OPENBOX_FINDINGS_CURSOR"
	EnvEnforcementFile = "OPENBOX_ENFORCEMENT_FILE"
	// EnvPendingApprovalDir relocates the filed-approval markers the gate and the
	// rewake watcher coordinate through (tests point it at a temp dir).
	EnvPendingApprovalDir = "OPENBOX_PENDING_APPROVAL_DIR"
	// EnvHaltDir relocates the session-halt latches a HALT verdict writes (tests
	// point it at a temp dir).
	EnvHaltDir      = "OPENBOX_HALT_DIR"
	EnvAPIKeyDirect = "OPENBOX_API_KEY"
	// EnvAgentPrivateKey is the Ed25519 signing key, under the name the OpenBox
	// platform documents for its own SDK.
	EnvAgentPrivateKey = "OPENBOX_AGENT_PRIVATE_KEY"
	EnvConfigPath      = "OPENBOX_CONFIG"
	// EnvOrgSigningPubKey policy-bundle signing key pins (E8-S6).
	EnvOrgSigningPubKey = "OPENBOX_ORG_SIGNING_PUBKEY"
	EnvOrgSigningKeyID  = "OPENBOX_ORG_SIGNING_KEY_ID"
	EnvAgentID          = "OPENBOX_AGENT_ID"
	EnvBackendURL       = "OPENBOX_BACKEND_URL"
	EnvControlToken     = "OPENBOX_CONTROL_TOKEN"
	EnvSpoolDir         = "OPENBOX_SPOOL_DIR"

	// DefaultBaseURL is the core data-plane base used when nothing configures
	// one.
	DefaultBaseURL = "https://core.openbox.ai"
	// DefaultBackendURL is the control-plane base used when nothing configures
	// one.
	DefaultBackendURL = "https://api.openbox.ai"
)

// deprecatedPrivateKeyEnvNames reading both costs two map lookups and a
// warning; breaking them would strand pipelines this repo cannot see.
var deprecatedPrivateKeyEnvNames = []string{"OPENBOX_ED25519_SEED", "OPENBOX_SEED"}

// DevConfig is the non-secret coordinate file the installers write and the
// hooks read (INV-1: it holds where the secrets live, never the secret
// values).
type DevConfig struct {
	BaseURL string `json:"base_url,omitempty"`
	DID     string `json:"developer_did,omitempty"`
	// ContentCapture is the org content posture.
	ContentCapture *bool `json:"content_capture,omitempty"`
	// Finops gates per-turn usage capture: token counts AND the model id that
	// spent them (that decision; the name predates the model binding and is kept
	// because renaming a config key is a user-visible break; read it as "usage
	// and model capture", not "token counts only").
	Finops *bool `json:"finops,omitempty"`

	// Telemetry enables the local OTLP receiver lane (that decision's `:otel:`).
	Telemetry *bool `json:"telemetry,omitempty"`
	// InstallGitHook enables ambient install of the prepare-commit-msg hook on
	// SessionStart.
	InstallGitHook bool `json:"install_git_hook,omitempty"`
	// Enforce flips the developer runtime from observe/advisory to enforce. It is
	// a *bool, and that is load-bearing rather than stylistic.
	Enforce *bool `json:"enforce,omitempty"`
	// FailClosed selects the enforce failure policy. Default false = fail-open:
	// an OpenBox outage never blocks a developer.
	FailClosed bool `json:"fail_closed,omitempty"`
	// EnforceTimeoutMS is inert under the in-process decider; retained for back-
	// compat parsing.
	EnforceTimeoutMS int `json:"enforce_timeout_ms,omitempty"`
	// Tier2 is deprecated and inert. Parsed so an existing dev.json does not
	// become an error, and deliberately NOT honoured: an org that set
	// `tier2:false` under the old design would otherwise stay silently ungoverned
	// after upgrading, which is the failure this whole change exists to close.
	Tier2 *bool `json:"tier2,omitempty"`
	// Tier2TimeoutMS is deprecated and inert.
	Tier2TimeoutMS int `json:"tier2_timeout_ms,omitempty"`
	// ApprovalHoldMS bounds how long the gate holds a tool call while a filed
	// approval is decided (ms, OD-E9-1). Clamping is adapter-owned: the hold can
	// never outlive the provider's hook timeout.
	ApprovalHoldMS int `json:"approval_hold_ms,omitempty"`
	// SecretDetection enables Tier-1 local secret/entropy detection.
	SecretDetection *bool `json:"secret_detection,omitempty"`
	// RequireVerifiedBundle is deprecated and inert. Parsed so an existing
	// dev.json does not become an error, and deliberately absent from the
	// reported posture; a control that cannot engage must not appear as one, or
	// an org reading `true` would believe a signature check was protecting it.
	RequireVerifiedBundle *bool `json:"require_verified_bundle,omitempty"`
	// Findings enables the findings loop.
	Findings *bool `json:"findings,omitempty"`
	// RealtimeFlush enables the debounced background flush that delivers spooled
	// events to core mid-session instead of only at SessionEnd. Absent = default
	// on (opt-out): it changes only delivery timing, never what egresses, and the
	// hook hot path stays free of network I/O.
	RealtimeFlush *bool `json:"realtime_flush,omitempty"`
	// AgentID is the backend agent id for the policy read.
	AgentID string `json:"agent_id,omitempty"`
	// BackendURL is the openbox-backend control-plane base (distinct from
	// BaseURL, the core data-plane base).
	BackendURL string `json:"backend_url,omitempty"`
	// OrgSigningKeyID and OrgSigningPubKey pin the org's policy-bundle signing
	// key (E8-S6 /).
	OrgSigningKeyID  string `json:"org_signing_key_id,omitempty"`
	OrgSigningPubKey string `json:"org_signing_pubkey,omitempty"` // base64 raw Ed25519
}

// DefaultConfigPath is where the hook looks for the dev config when
// OPENBOX_CONFIG is unset: ~/.openbox/dev.json, with a read-side fallback to
// the pre-that decision location while an unmigrated file lives there.
func DefaultConfigPath() string {
	p, err := DevConfigPath()
	if err != nil {
		return filepath.Join(legacyConfigDir(), "dev.json")
	}
	return p
}

// Load reads the dev config at path if present.
func Load(path string) (DevConfig, error) {
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

func load() (DevConfig, error) { return Load(DefaultConfigPath()) }

// Credentials is the resolved runtime identity for a hook binary. It carries
// the secret values (read from the environment or ~/.openbox/.env); it exists
// only in process memory on the flush path and must never be logged or
// persisted.
type Credentials struct {
	BaseURL               string
	APIKey                string
	DID                   string
	PrivateKeyB64         string
	ContentCaptureEnabled bool
}

// ResolveDID resolves only the developer DID (env, then config file); no
// secret-store access. This is the hot path: observe/spool needs the DID to
// attribute events but never the obx_ key or seed, so a tool-use hook does
// zero secret I/O (INV-1).
func ResolveDID() (string, error) {
	cfg, err := load()
	if err != nil {
		return "", err
	}
	did := FirstNonEmpty(os.Getenv(EnvDID), cfg.DID)
	if did == "" {
		return "", fmt.Errorf("no developer DID configured (run `openbox init`)")
	}
	return did, nil
}

// ResolveDIDOrEmpty resolves the developer DID and returns "" when nothing
// configures one, for callers where an absent DID is a legitimate state to
// report rather than an error to propagate; the install path, which may be
// about to write one.
func ResolveDIDOrEmpty() string {
	cfg, _ := load()
	return FirstNonEmpty(os.Getenv(EnvDID), cfg.DID)
}

// ResolveCoordinates resolves the non-secret target coordinates; the core base
// URL and the developer DID; from env then the dev config, with zero secret-
// store access (INV-1).
func ResolveCoordinates() (baseURL, did string) {
	cfg, _ := load()
	baseURL = FirstNonEmpty(os.Getenv(EnvBaseURL), cfg.BaseURL, DefaultBaseURL)
	did = FirstNonEmpty(os.Getenv(EnvDID), cfg.DID)
	return baseURL, did
}

// SpoolDir is where hot-path events are spooled before flush: the
// OPENBOX_SPOOL_DIR override when set, else `<user-config>/openbox/<subdir>`.
func SpoolDir(subdir string) string {
	if p := os.Getenv(EnvSpoolDir); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", subdir)
}

// ResolveInstallGitHook reports whether the adapter should install the
// prepare-commit-msg hook into the session's repo on SessionStart.
func ResolveInstallGitHook() bool {
	return resolveBool("install_git_hook", func(c DevConfig) *bool { b := c.InstallGitHook; return &b }, false, EnvInstallGitHook)
}

// ResolveFinops reports whether usage capture is enabled; per-turn token
// counts and the model id that spent them. With it off, transcript_path is
// never opened, no turn event is emitted, and no model id reaches the wire
// beyond the one SessionStarted already carried.
func ResolveFinops() bool {
	return resolveBool("finops", func(c DevConfig) *bool { return c.Finops }, true, EnvFinops)
}

// ResolveTelemetry reports whether the local telemetry lane may record.
// Default ON; see the field comment for why a default-off second switch would
// be a bug rather than a conservative choice. This gates recording, never
// receiving.
func ResolveTelemetry() bool {
	return resolveBool("telemetry", func(c DevConfig) *bool { return c.Telemetry }, true, EnvTelemetry)
}

// ResolveContentCapture reports the org content posture: config
// `content_capture` first, then the env override (env wins either way).
func ResolveContentCapture() bool {
	return resolveBool("content_capture", func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture)
}

// ResolveSecretDetection reports whether Tier-1 local secret/entropy detection
// is on.
func ResolveSecretDetection() bool {
	return resolveBool("secret_detection", func(c DevConfig) *bool { return c.SecretDetection }, true, EnvSecretDetection)
}

// ResolveFindings reports whether the findings loop is on.
func ResolveFindings() bool {
	return resolveBool("findings", func(c DevConfig) *bool { return c.Findings }, false, EnvFindings)
}

// ResolveRealtime reports whether the debounced background flush is on;
// spooled events are delivered to core within a debounce window of each hook
// instead of waiting for SessionEnd.
func ResolveRealtime() bool {
	return resolveBool("realtime_flush", func(c DevConfig) *bool { return c.RealtimeFlush }, true, EnvRealtime)
}

// ResolveFindingsCursor resolves the findings-loop cursor state file: the env
// override, else a fixed file next to the advisory sink.
func ResolveFindingsCursor(provider string) string {
	if p := os.Getenv(EnvFindingsCursor); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	name := "findings.cursor"
	if p := sanitizeProvider(provider); p != "" {
		name = "findings-" + p + ".cursor"
	}
	return filepath.Join(dir, "openbox", name)
}

func sanitizeProvider(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		}
	}
	return b.String()
}

// ResolveEnforce reports whether the developer runtime is in enforce mode:
// config field first, then the env override.
func ResolveEnforce() bool {
	return resolveBool("enforce", func(c DevConfig) *bool { return c.Enforce }, true, EnvEnforce)
}

// ResolveFailClosed reports the enforce failure policy. Default false = fail-
// open; an org never becomes fail-closed by accident.
func ResolveFailClosed() bool {
	return resolveBool("fail_closed", func(c DevConfig) *bool { b := c.FailClosed; return &b }, false, EnvFailClosed)
}

var deprecationOnce sync.Once

func warnDeprecatedKeys() {
	dead := deadKeysPresent()
	if len(dead) == 0 {
		return
	}
	deprecationOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "openbox: %s set but ignored; every gated tool call is "+
			"evaluated by OpenBox, so there are no tiers to switch between and no "+
			"local bundle to verify. Remove from dev.json / the environment to silence this.\n",
			strings.Join(dead, ", "))
	})
}

func deadKeysPresent() []string {
	cfg, err := load()
	ok := err == nil
	var dead []string
	if _, env := os.LookupEnv(EnvTier2); env || (ok && cfg.Tier2 != nil) {
		dead = append(dead, "`tier2`")
	}
	if _, env := os.LookupEnv(EnvTier2Timeout); env || (ok && cfg.Tier2TimeoutMS != 0) {
		dead = append(dead, "`tier2_timeout_ms`")
	}
	if _, env := os.LookupEnv(EnvRequireVerified); env || (ok && cfg.RequireVerifiedBundle != nil) {
		dead = append(dead, "`require_verified_bundle`")
	}
	return dead
}

// ResolveTier2 reads the deprecated, inert `tier2` key.
func ResolveTier2() bool {
	return resolveBool("tier2", func(c DevConfig) *bool { return c.Tier2 }, false, EnvTier2)
}

// ResolveTimeoutMS resolves a millisecond budget knob: the config field first
// (via cfgMS), then the env override when present and parseable; a garbage env
// value is ignored and the config value stands, so a fat-fingered env never
// silently wipes a valid config.
func ResolveTimeoutMS(cfgMS func(DevConfig) int, envKey string) int {
	ms := 0
	if cfg, err := load(); err == nil {
		ms = cfgMS(cfg)
	}
	if v, ok := os.LookupEnv(envKey); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			ms = n
		}
	}
	return ms
}

// ResolveOrgSigningKey resolveAgentID resolves the backend agent id for policy
// sync/staleness: env first, then the dev config.
func ResolveOrgSigningKey() (pubKeyB64, keyID string) {
	c, _ := load()
	pubKeyB64, keyID = c.OrgSigningPubKey, c.OrgSigningKeyID
	if v := strings.TrimSpace(os.Getenv(EnvOrgSigningPubKey)); v != "" {
		pubKeyB64 = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvOrgSigningKeyID)); v != "" {
		keyID = v
	}
	return pubKeyB64, keyID
}

func ResolveAgentID() string {
	cfg, _ := load()
	return FirstNonEmpty(os.Getenv(EnvAgentID), cfg.AgentID)
}

// ResolveBackendURL resolves the openbox-backend control-plane base URL: env
// first, then the dev config.
func ResolveBackendURL() string {
	cfg, _ := load()
	return FirstNonEmpty(os.Getenv(EnvBackendURL), cfg.BackendURL)
}

// ResolveControlToken resolves the org control-plane credential: the
// OPENBOX_CONTROL_TOKEN env only. Deliberately not a config field and never
// read from the runtime secret store; supplied via env only so it cannot leak
// via a config file or argv (INV-1).
func ResolveControlToken() string {
	return os.Getenv(EnvControlToken)
}

// ResolveCredentials assembles Credentials from the environment, the
// credential file, and the dev config. It returns an error (never a panic)
// when identity is incomplete; the caller logs it fail-open and exits 0
// (INV-3).
func ResolveCredentials() (Credentials, error) {
	cfg, err := load()
	if err != nil {
		return Credentials{}, err
	}

	c := Credentials{
		BaseURL:               FirstNonEmpty(os.Getenv(EnvBaseURL), cfg.BaseURL, DefaultBaseURL),
		DID:                   FirstNonEmpty(os.Getenv(EnvDID), cfg.DID),
		ContentCaptureEnabled: cfg.ContentCapture == nil || *cfg.ContentCapture,
	}
	if v, ok := os.LookupEnv(EnvContentCapture); ok {
		c.ContentCaptureEnabled = IsTruthy(v)
	}
	if c.DID == "" {
		return Credentials{}, fmt.Errorf("no developer DID configured (run `openbox init`)")
	}

	secrets, envPath, err := loadSecretFile()
	if err != nil {
		return Credentials{}, err
	}

	c.APIKey = FirstNonEmpty(os.Getenv(EnvAPIKeyDirect), secrets[EnvAPIKeyDirect])
	if c.APIKey == "" {
		return Credentials{}, missingCredentialError("obx_ API key", EnvAPIKeyDirect, envPath)
	}

	c.PrivateKeyB64 = resolvePrivateKey(secrets)
	if c.PrivateKeyB64 == "" {
		return Credentials{}, missingCredentialError("Ed25519 signing key", EnvAgentPrivateKey, envPath)
	}

	return c, nil
}

// loadSecretFile an unresolvable home or an unparseable file IS an error:
// silently treating either as "no credentials" would send a user hunting for a
// registration problem they do not have.
func loadSecretFile() (map[string]string, string, error) {
	path, err := EnvFilePath()
	if err != nil {
		return map[string]string{}, "", nil
	}
	kv, err := ParseEnvFile(path)
	if err != nil {
		return nil, path, err
	}
	return kv, path, nil
}

func resolvePrivateKey(secrets map[string]string) string {
	if v := os.Getenv(EnvAgentPrivateKey); v != "" {
		return v
	}
	for _, alias := range deprecatedPrivateKeyEnvNames {
		if v := os.Getenv(alias); v != "" {
			warnDeprecatedPrivateKeyName(alias)
			return v
		}
	}
	if v := secrets[EnvAgentPrivateKey]; v != "" {
		return v
	}
	for _, alias := range deprecatedPrivateKeyEnvNames {
		if v := secrets[alias]; v != "" {
			warnDeprecatedPrivateKeyName(alias)
			return v
		}
	}
	return ""
}

// warnDeprecatedPrivateKeyName a deprecation notice must never become model
// input.
func warnDeprecatedPrivateKeyName(alias string) {
	deprecatedNameWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "openbox: %s is deprecated; use %s (same value, the name OpenBox documents)\n",
			alias, EnvAgentPrivateKey)
	})
}

var deprecatedNameWarnOnce sync.Once

func missingCredentialError(what, envName, envPath string) error {
	where := "~/.openbox/.env"
	if envPath != "" {
		where = envPath
	}
	msg := fmt.Sprintf("no %s available: set %s, or run `openbox auth` to write %s", what, envName, where)
	if envPath == "" {
		msg += fmt.Sprintf(" (no home directory could be resolved; set %s to an absolute path)", EnvHome)
	}
	switch runtime.GOOS {
	case "darwin":
		msg += "\n  upgrading an existing install? credentials used to live in your keychain and are not migrated:" +
			"\n    security find-generic-password -s ai.openbox.dev -a '<org>/<provider>/api_key' -w" +
			"\n  paste those into `openbox auth`, or re-issue them with `openbox auth --rotate`"
	case "linux":
		msg += "\n  upgrading an existing install? credentials used to live in libsecret and are not migrated:" +
			"\n    secret-tool lookup service ai.openbox.dev account '<org>/<provider>/api_key'" +
			"\n  paste those into `openbox auth`, or re-issue them with `openbox auth --rotate`"
	}
	return errors.New(msg)
}

func resolveBool(fieldName string, field func(DevConfig) *bool, def bool, envKey string) bool {
	v, _ := resolveBoolWithSource(fieldName, field, def, envKey)
	return v
}

func unmarshalStrict(raw []byte, v any) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return err
	}
	for k := range all {
		if strings.HasPrefix(k, "//") {
			delete(all, k)
		}
	}
	stripped, err := json.Marshal(all)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(stripped))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// FirstNonEmpty returns the first non-empty string.
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// IsTruthy interprets an env toggle value: 1/true/yes/on (case-insensitive).
func IsTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
