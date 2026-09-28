// Package devconfig is the provider-neutral developer-runtime configuration
// and credential resolution shared by every tool adapter (Claude Code, Codex,
// Cursor). One store per field: the credential file is never read for a
// coordinate and dev.json never holds a secret. Without that split the DID
// lived in both dev.json and the OS keychain, and a stale keychain entry
// silently reverted a corrected DID on the next install; this split is what
// makes that impossible rather than merely fixed.
package devconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// platform documents for its own SDK. Retired as a credential source now
	// that v3 is the only identity; kept readable only as a legacy marker
	// (LegacyStoreFor) and a deprecated-alias source.
	EnvAgentPrivateKey = "OPENBOX_AGENT_PRIVATE_KEY"
	// EnvWorkloadPrivateKey is the v3 keycloak_workload RS256 client-assertion
	// signing key: PKCS8 DER, base64 std.
	EnvWorkloadPrivateKey = "OPENBOX_WORKLOAD_PRIVATE_KEY"
	EnvConfigPath         = "OPENBOX_CONFIG"
	// EnvOrgSigningPubKey policy-bundle signing key pins.
	EnvOrgSigningPubKey = "OPENBOX_ORG_SIGNING_PUBKEY"
	EnvOrgSigningKeyID  = "OPENBOX_ORG_SIGNING_KEY_ID"
	EnvAgentID          = "OPENBOX_AGENT_ID"
	EnvBackendURL       = "OPENBOX_BACKEND_URL"
	EnvControlToken     = "OPENBOX_CONTROL_TOKEN"
	EnvSpoolDir         = "OPENBOX_SPOOL_DIR"
	// EnvSpoolRoot relocates the BASE every subdir-scoped spool resolves
	// under (SpoolDir's own fallback join point), one level above
	// EnvSpoolDir: a lane daemon's unit resolves this once, from the
	// installing process's own ConfigDir(), so cc-spool and codex-spool
	// still land in the SAME directory a hook flusher's own SpoolDir call
	// resolves -- os.UserConfigDir() alone cannot be trusted to agree with
	// itself across a daemon that has no $HOME at all. EnvSpoolDir still
	// outranks it (it names the whole path, collapsing every subdir into
	// one directory on purpose; see providers.OwnedSpoolDirs's own doc).
	EnvSpoolRoot = "OPENBOX_SPOOL_ROOT"

	// DefaultBaseURL is the core data-plane base used when nothing configures
	// one.
	DefaultBaseURL = "https://core.openbox.ai"
	// DefaultBackendURL is the control-plane base used when nothing configures
	// one.
	DefaultBackendURL = "https://api.openbox.ai"

	// IdentityMethodKeycloakWorkload marks a dev.json written for a v3
	// keycloak_workload agent, as opposed to a legacy (v1) store. Its presence
	// is never itself the legacy discriminator; see LegacyStoreFor.
	IdentityMethodKeycloakWorkload = "keycloak_workload"
)

// legacySeedEnvNames are the Ed25519 seed env names a v1 store could hold,
// under the documented name or a deprecated alias. Retired as a credential
// source (no v3 resolver reads any of them), and kept only as a legacy-store
// detection signal: LegacyStoreFor and WriteWorkloadIdentity iterate this to
// find and purge a seed sitting beside (or instead of) a v3 workload key.
var legacySeedEnvNames = []string{EnvAgentPrivateKey, "OPENBOX_ED25519_SEED", "OPENBOX_SEED"}

// DevConfig is the non-secret coordinate file the installers write and the
// hooks read (INV-1: it holds where the secrets live, never the secret
// values).
type DevConfig struct {
	BaseURL string `json:"base_url,omitempty"`
	DID     string `json:"developer_did,omitempty"`
	// ContentCapture is the org content posture.
	ContentCapture *bool `json:"content_capture,omitempty"`
	// Finops gates per-turn usage capture: token counts AND the model id that
	// spent them (the name predates the model binding and is kept
	// because renaming a config key is a user-visible break; read it as "usage
	// and model capture", not "token counts only").
	Finops *bool `json:"finops,omitempty"`

	// Telemetry enables the local OTLP receiver lane (the `:otel:` lane).
	Telemetry *bool `json:"telemetry,omitempty"`
	// InstallGitHook enables ambient install of the prepare-commit-msg hook on
	// SessionStart.
	InstallGitHook bool `json:"install_git_hook,omitempty"`
	// Enforce is deprecated and inert: every gated tool call is evaluated by
	// OpenBox unconditionally now (ResolveEnforce always reports true). Parsed
	// so an existing dev.json does not become an error, and so an explicit
	// value can still be named in the deprecated-key warning.
	Enforce *bool `json:"enforce,omitempty"`
	// FailClosed is deprecated and inert: a gated call is always fail-closed now
	// (an undelivered evaluation denies that call). A *bool, not a plain bool, so an explicit
	// `"fail_closed": false` in a file is distinguishable from the key being
	// absent altogether -- the same reason Tier2 is a *bool.
	FailClosed *bool `json:"fail_closed,omitempty"`
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
	// approval is decided (ms); undecided past it, the call is denied. Clamping is adapter-owned: the hold can
	// never outlive the provider's hook timeout.
	ApprovalHoldMS int `json:"approval_hold_ms,omitempty"`
	// SecretDetection enables local secret/entropy detection.
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
	// key.
	OrgSigningKeyID  string `json:"org_signing_key_id,omitempty"`
	OrgSigningPubKey string `json:"org_signing_pubkey,omitempty"` // base64 raw Ed25519
	// IdentityMethod is "keycloak_workload" for a v3 store; empty or any other
	// value reads as legacy alongside the DID/seed markers LegacyStoreFor also
	// checks. dev.json never stores the derived attribution DID: this field
	// is the only identity-shape marker that lives here.
	IdentityMethod string `json:"identity_method,omitempty"`
}

// DefaultConfigPath is where the hook looks for the dev config when
// OPENBOX_CONFIG is unset: the bound tool's ~/.openbox/<tool>/dev.json, or
// ~/.openbox/dev.json unbound, with a read-side fallback to the pre-that
// decision location while an unmigrated file lives there.
//
// The fallback is scoped to the bound tool for the same reason the bound
// branch of DevConfigPath skips resolveConfigPath: a per-tool store has never
// existed at the legacy location, so a bound read landing there would be one
// tool answering with the org's identity -- exactly the cross-boundary read
// the per-tool split exists to make impossible. It is reached only when Home()
// itself fails (a relative OPENBOX_HOME, or no home directory at all), where
// the honest answer is a path that does not exist rather than one that does
// and belongs to somebody else.
func DefaultConfigPath() string {
	p, err := DevConfigPath()
	if err != nil {
		if tool := BoundProvider(); tool != "" {
			return filepath.Join(legacyConfigDir(), tool, "dev.json")
		}
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
// the secret values (read from the environment or ~/.openbox/<tool>/.env); it exists
// only in process memory on the flush path and must never be logged or
// persisted.
type Credentials struct {
	BaseURL string
	APIKey  string
	// DID is the in-memory attribution label: derived from AgentID via
	// AttributionDIDFor, never itself stored or read from a store. Never
	// empty on a successful resolve.
	DID                   string
	ContentCaptureEnabled bool
	// AgentID is the v3 workload agent id: env OPENBOX_AGENT_ID, else
	// dev.json's agent_id field. Required; DID is derived from it.
	AgentID string
	// WorkloadPrivateKey is the RS256 client-assertion signing key
	// (OPENBOX_WORKLOAD_PRIVATE_KEY: PKCS8 DER, base64 std). Same store
	// precedence as every other secret here: env beats the credential file.
	WorkloadPrivateKey string
	// TokenCachePath is this tool's workload-token.json cache path. Never a
	// credential source itself (it is a cache, not a store); populated for a
	// caller that needs to know where the bearer cache lives.
	TokenCachePath string
}

// ErrLegacyStore is returned by ResolveDID, ResolveCredentials and
// ResolveCredentialsFor when a tool's store predates the v3 keycloak_workload
// model (a stored developer_did, or an Ed25519 signing seed under the
// documented or a deprecated name -- see LegacyStoreFor). The v3 binary cannot
// speak for a legacy identity at all; the only remedy is re-registering it:
// `openbox init --provider <tool>`.
var ErrLegacyStore = errors.New("legacy (pre-IAMv3) identity store; hooks send nothing")

// legacyStoreError wraps ErrLegacyStore with the reasons LegacyStoreFor (or
// its shared detection, legacyStoreFromValues) found, and the tool-scoped
// re-init remedy.
func legacyStoreError(tool string, ls LegacyStore) error {
	remedy := "`openbox init --provider <tool>`"
	if tool != "" {
		remedy = "`openbox init --provider " + tool + "`"
	}
	reasons := "no reason recorded"
	if len(ls.Reasons) > 0 {
		reasons = strings.Join(ls.Reasons, "; ")
	}
	return fmt.Errorf("%w (%s); run %s to register a v3 identity", ErrLegacyStore, reasons, remedy)
}

// ResolveDID resolves the in-memory attribution DID for a v3 workload
// identity: env OPENBOX_AGENT_ID, else dev.json's agent_id, run through
// AttributionDIDFor. No secret-store access. This is the hot path:
// observe/spool needs the DID to attribute events but never the obx_ key or
// the workload signing key, so a tool-use hook does zero secret I/O (INV-1).
//
// OPENBOX_AGENT_DID is retired and never consulted (it named a v1 DID that
// could disagree with the store, which is exactly the two-store bug this
// derivation exists to make impossible). A stored developer_did means the
// store predates v3 entirely: ErrLegacyStore, never a derived answer that
// would silently disagree with what's on disk.
func ResolveDID() (string, error) {
	cfg, err := load()
	if err != nil {
		return "", err
	}
	if cfg.DID != "" {
		return "", legacyStoreError(BoundProvider(), LegacyStore{Legacy: true, Reasons: []string{"dev.json holds a stored developer_did"}})
	}
	agentID := FirstNonEmpty(os.Getenv(EnvAgentID), cfg.AgentID)
	if agentID == "" {
		return "", missingAgentIDError(BoundProvider())
	}
	return AttributionDIDFor(agentID)
}

// ResolveDIDOrEmpty resolves the developer DID and returns "" when nothing
// configures one OR the store is legacy, for callers where an absent DID is a
// legitimate state to report rather than an error to propagate; the install
// path, which may be about to write one.
func ResolveDIDOrEmpty() string {
	did, err := ResolveDID()
	if err != nil {
		return ""
	}
	return did
}

// ResolveCoordinates resolves the non-secret target coordinates; the core base
// URL and the derived developer DID; from env then the dev config, with zero
// secret-store access (INV-1). A legacy store or a missing agent id resolves
// the DID half to "", same as ResolveDIDOrEmpty.
func ResolveCoordinates() (baseURL, did string) {
	cfg, _ := load()
	baseURL = FirstNonEmpty(os.Getenv(EnvBaseURL), cfg.BaseURL, DefaultBaseURL)
	return baseURL, ResolveDIDOrEmpty()
}

// SpoolDir is where hot-path events are spooled before flush:
// OPENBOX_SPOOL_DIR (the whole path) when set, else OPENBOX_SPOOL_ROOT/subdir
// when THAT is set, else `<user-config>/openbox/<subdir>` -- the same base
// ConfigDir() resolves, just not routed through it (ConfigDir has no subdir
// parameter), so an unset root still joins the identical directory.
func SpoolDir(subdir string) string {
	if p := os.Getenv(EnvSpoolDir); p != "" {
		return p
	}
	if root := os.Getenv(EnvSpoolRoot); root != "" {
		return filepath.Join(root, subdir)
	}
	return filepath.Join(userConfigDir(), "openbox", subdir)
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

// ResolveTelemetryFor is ResolveTelemetry scoped to one named tool's own
// dev.json, for the same reason ResolveContentCaptureFor exists: a lane
// daemon recording turns for more than one tool must gate each tool's OWN
// recording on that tool's OWN posture, never on whichever tool it bound at
// startup for unrelated setup work.
func ResolveTelemetryFor(tool string) (bool, error) {
	path, err := DevConfigPathFor(tool)
	if err != nil {
		return false, err
	}
	return resolveBoolAt("telemetry", func(c DevConfig) *bool { return c.Telemetry }, true, EnvTelemetry, path), nil
}

// ResolveContentCapture reports the org content posture: config
// `content_capture` first, then the env override (env wins either way).
func ResolveContentCapture() bool {
	return resolveBool("content_capture", func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture)
}

// ResolveContentCaptureFor is ResolveContentCapture scoped to one named
// tool's own dev.json instead of the ambiently bound ResolveContentCapture's
// DefaultConfigPath()/BoundProvider(). A process governing more than one tool
// for its whole life (a shared lane daemon) must ask each tool's own store
// this question rather than answering every tool with whichever one it bound
// at startup for something unrelated. The managed (org-mandated) layer still
// applies the same way, since it is not a per-tool store; only the env
// override is process-wide by nature.
func ResolveContentCaptureFor(tool string) (bool, error) {
	path, err := DevConfigPathFor(tool)
	if err != nil {
		return false, err
	}
	return resolveBoolAt("content_capture", func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture, path), nil
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
	name := "findings.cursor"
	if p := sanitizeProvider(provider); p != "" {
		name = "findings-" + p + ".cursor"
	}
	return filepath.Join(userConfigDir(), "openbox", name)
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

// ResolveEnforce always reports true: every gated tool call is evaluated by
// OpenBox unconditionally now, so there is no longer a mode to select. The
// `enforce` key (dev.json, managed) and OPENBOX_ENFORCE still parse -- so the
// key stays readable and deadKeysPresent can still warn on it -- but neither
// selects anything here any more.
func ResolveEnforce() bool {
	return true
}

// ResolveFailClosed reports the enforce failure policy. Deprecated and inert:
// a gated call is always fail-closed now (an undelivered evaluation denies
// that call); the key still
// parses so it can warn.
func ResolveFailClosed() bool {
	return resolveBool("fail_closed", func(c DevConfig) *bool { return c.FailClosed }, false, EnvFailClosed)
}

var deprecationOnce sync.Once

func warnDeprecatedKeys() {
	dead := deadKeysPresent()
	if len(dead) == 0 {
		return
	}
	deprecationOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "openbox: %s set but ignored; every gated tool call is "+
			"evaluated by OpenBox and every event it cannot record halts the run, so "+
			"there are no tiers to switch between, no local bundle to verify, and no "+
			"failure policy left to choose. Remove from dev.json / the environment to "+
			"silence this.\n",
			strings.Join(dead, ", "))
	})
}

// deadKeysPresent looks in every layer a key can arrive from, the managed one
// included. An org is the reader who most needs the warning: it cannot see the
// developer's stderr, but a key it sets is one it believes is governing, and
// the posture row no longer mentions the key at all.
func deadKeysPresent() []string {
	cfgs := make([]DevConfig, 0, 2)
	if cfg, err := load(); err == nil {
		cfgs = append(cfgs, cfg)
	}
	if st := cachedManaged(); st.readable {
		cfgs = append(cfgs, st.cfg.DevConfig)
	}
	set := func(pick func(DevConfig) bool) bool {
		for _, c := range cfgs {
			if pick(c) {
				return true
			}
		}
		return false
	}
	var dead []string
	if _, env := os.LookupEnv(EnvTier2); env || set(func(c DevConfig) bool { return c.Tier2 != nil }) {
		dead = append(dead, "`tier2`")
	}
	if _, env := os.LookupEnv(EnvTier2Timeout); env || set(func(c DevConfig) bool { return c.Tier2TimeoutMS != 0 }) {
		dead = append(dead, "`tier2_timeout_ms`")
	}
	if _, env := os.LookupEnv(EnvRequireVerified); env || set(func(c DevConfig) bool { return c.RequireVerifiedBundle != nil }) {
		dead = append(dead, "`require_verified_bundle`")
	}
	if _, env := os.LookupEnv(EnvFailClosed); env || set(func(c DevConfig) bool { return c.FailClosed != nil }) {
		dead = append(dead, "`fail_closed`")
	}
	if _, env := os.LookupEnv(EnvEnforce); env || set(func(c DevConfig) bool { return c.Enforce != nil }) {
		dead = append(dead, "`enforce`")
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

// ResolveControlToken resolves the org control-plane credential:
// OPENBOX_CONTROL_TOKEN, then the org-level .env.
//
// It was env-only until `auth` stopped registering agents and `init` started.
// Those are two different processes with nothing exported between them, so the
// token has to survive on disk, and ~/.openbox/.env is the one file both
// agree on. State the cost plainly: this credential creates and rotates agents
// across the whole organization, the file is plaintext, 0600 on macOS and
// Linux and unprotected on Windows, and anything running as the developer --
// the governed agent included -- can read it. Persisting it buys the
// auth/init split; it hardens nothing.
//
// It is still never a config field and never the per-tool secret store: this
// reads OrgEnvFilePath, not EnvFilePath, so a fleet credential planted in a
// tool's own .env cannot be served to a bound process (INV-1).
func ResolveControlToken() string {
	tok, _ := ResolveControlTokenWithSource()
	return tok
}

// ResolveControlTokenWithSource is the same resolution, and additionally names
// where the value came from: the org file's path, or the environment variable.
// Empty source means nothing configured one.
//
// This is the one credential the org file outranks the environment for, and the
// exception is deliberate. Every other value here is resolved fresh on the hot
// path, where an exported variable is a legitimate per-run override. The
// control token is not: it is handed between two commands in two processes --
// `auth` writes it, `init` reads it -- and while the environment won, an export
// could silently break that handoff. The export that does it is rarely a
// deliberate one. Measured: a GUI editor launched four weeks earlier carried one
// in its environment, so every terminal it spawned inherited a stale token,
// `auth` wrote the right one, `init` sent the old one, and nothing the developer
// could do to the file changed what was sent. A setup flow whose second command
// ignores what its first command just wrote is not a flow.
//
// The environment is still the fallback, which is what keeps the two routes that
// depend on it working: a CI image that never runs `auth` has no file at all,
// and a developer who declines the token prompt to keep it off disk leaves none
// in the file either. Only the both-present case changed, and that is the case
// that was doing the harm.
func ResolveControlTokenWithSource() (token, source string) {
	if path, err := OrgEnvFilePath(); err == nil {
		if kv, err := ParseEnvFile(path); err == nil {
			if v := kv[EnvControlToken]; v != "" {
				return v, path
			}
		}
	}
	if v := os.Getenv(EnvControlToken); v != "" {
		return v, "the " + EnvControlToken + " environment variable"
	}
	return "", ""
}

// ResolveCredentials assembles Credentials from the environment, the
// credential file, and the dev config. It returns an error (never a panic)
// when identity is incomplete; the caller logs it fail-open and exits 0
// (INV-3).
func ResolveCredentials() (Credentials, error) {
	tool := BoundProvider()
	if tool == "" {
		return Credentials{}, ErrProviderUnbound
	}
	envPath, err := EnvFilePath()
	if err != nil {
		return Credentials{}, err
	}
	// DefaultConfigPath, not DevConfigPathFor: the bound path honours
	// OPENBOX_CONFIG, and an operator who pointed it at a file expects the
	// running hook to read that file.
	return resolveCredentialsFrom(tool, DefaultConfigPath(), envPath)
}

// ResolveCredentialsFor assembles one named tool's identity without binding,
// for a caller that reports on every store in turn. Binding inside such a loop
// is the one thing that could make every row a copy of the first, because a
// held config pin freezes what the first read resolved.
//
// ContentCaptureEnabled is resolved from THIS tool's own dev.json (via
// resolveBoolAt on cfgPath below), never from whichever tool happens to be
// ambiently bound: a lane daemon serving two tools calls this once per tool at
// startup, and a Codex `content_capture:false` must be honoured for Codex's
// own client while Claude Code's stays on, and vice versa. The managed
// (org-mandated) layer still applies to both, since it is not a per-tool
// store.
func ResolveCredentialsFor(tool string) (Credentials, error) {
	cfgPath, err := DevConfigPathFor(tool)
	if err != nil {
		return Credentials{}, err
	}
	envPath, err := EnvFilePathFor(tool)
	if err != nil {
		return Credentials{}, err
	}
	return resolveCredentialsFrom(tool, cfgPath, envPath)
}

// resolveCredentialsFrom is the v3 flip: a legacy store (LegacyStoreFor's
// detection, applied to the values this call already loaded) refuses outright
// with ErrLegacyStore, since the v3 binary cannot speak for it at all. Order
// matters beyond structure: the two secrets are checked BEFORE the agent id
// coordinate, so a store missing only the coordinate fails with the
// coordinate error and a store missing a secret fails with the secret error,
// never the other one (TestEnvFileIsNotACoordinateSource's guard (c) pins
// this).
func resolveCredentialsFrom(tool, cfgPath, envPath string) (Credentials, error) {
	cfg, err := Load(cfgPath)
	if err != nil {
		return Credentials{}, err
	}

	secrets, err := ParseEnvFile(envPath)
	if err != nil {
		return Credentials{}, err
	}

	if ls := legacyStoreFromValues(cfg, secrets); ls.Legacy {
		return Credentials{}, legacyStoreError(tool, ls)
	}

	c := Credentials{
		BaseURL: FirstNonEmpty(os.Getenv(EnvBaseURL), cfg.BaseURL, DefaultBaseURL),
		// Resolved from cfgPath -- THIS tool's own dev.json for
		// ResolveCredentialsFor, the ambient bound tool's for ResolveCredentials
		// -- through the same default -> managed (a locked key wins outright) ->
		// user -> env precedence ResolveContentCapture applies, just scoped to
		// the file this call was actually given rather than re-deriving
		// DefaultConfigPath()/BoundProvider() and silently answering for a
		// different tool. A locked managed `content_capture:false` still wins
		// outright for every tool, since the managed layer is org-wide, not a
		// per-tool store.
		ContentCaptureEnabled: resolveBoolAt("content_capture", func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture, cfgPath),
	}

	c.APIKey = FirstNonEmpty(os.Getenv(EnvAPIKeyDirect), secrets[EnvAPIKeyDirect])
	if c.APIKey == "" {
		return Credentials{}, missingCredentialError(tool, "obx_ API key", EnvAPIKeyDirect, envPath)
	}

	c.WorkloadPrivateKey = FirstNonEmpty(os.Getenv(EnvWorkloadPrivateKey), secrets[EnvWorkloadPrivateKey])
	if c.WorkloadPrivateKey == "" {
		return Credentials{}, missingCredentialError(tool, "workload signing key", EnvWorkloadPrivateKey, envPath)
	}

	// The agent id is a coordinate (INV-1: one store per field), so it is read
	// from env or dev.json ONLY, never from the secrets file even when a
	// coordinate-shaped value sits there (TestEnvFileIsNotACoordinateSource).
	agentID := FirstNonEmpty(os.Getenv(EnvAgentID), cfg.AgentID)
	if agentID == "" {
		return Credentials{}, missingAgentIDError(tool)
	}
	did, err := AttributionDIDFor(agentID)
	if err != nil {
		return Credentials{}, err
	}
	c.AgentID = agentID
	c.DID = did

	if p, err := WorkloadTokenCachePathFor(tool); err == nil {
		c.TokenCachePath = p
	}

	return c, nil
}

// EnvIdentityPresent reports whether the environment alone supplies a
// complete v3 workload identity: an API key and the workload signing key. A
// legacy seed (current or deprecated alias) no longer counts: it identifies a
// store the v3 binary cannot speak for, not an identity it can use.
//
// It exists so the install gate and the registration decision ask the same
// question. They disagreed once: the gate accepted an environment identity and
// registration looked only at the file, so a machine provisioned entirely
// through exported variables was told it had hit a build bug. And an exported
// identity outranks every store at runtime, so minting an agent for such a
// machine would create one nobody ever uses -- one per ephemeral CI runner.
func EnvIdentityPresent() bool {
	return os.Getenv(EnvAPIKeyDirect) != "" && os.Getenv(EnvWorkloadPrivateKey) != ""
}

// ErrMissingAgentID is wrapped into the error resolveCredentialsFrom and
// ResolveDID return when every secret required is present but no agent id
// configures the coordinate they attribute to. Distinguishing it from a
// missing-secret error is the whole point of
// TestEnvFileIsNotACoordinateSource's guard (a): a caller must be able to
// tell "no identity papers" apart from "papers, but nothing says who holds
// them".
var ErrMissingAgentID = errors.New("no agent id configured")

func missingAgentIDError(tool string) error {
	remedy := "`openbox init --provider <tool>`"
	if tool != "" {
		remedy = "`openbox init --provider " + tool + "`"
	}
	return fmt.Errorf("%w: set %s, or run %s to register one", ErrMissingAgentID, EnvAgentID, remedy)
}

func missingCredentialError(tool, what, envName, envPath string) error {
	where := "~/.openbox/<tool>/.env"
	if envPath != "" {
		where = envPath
	}
	remedy := "`openbox init --provider <tool>`"
	if tool != "" {
		remedy = "`openbox init --provider " + tool + "`"
	}
	msg := fmt.Sprintf("no %s available: set %s, or run %s to write %s", what, envName, remedy, where)
	if envPath == "" {
		msg += fmt.Sprintf(" (no home directory could be resolved; set %s to an absolute path)", EnvHome)
	}
	return errors.New(msg)
}

func resolveBool(fieldName string, field func(DevConfig) *bool, def bool, envKey string) bool {
	v, _ := resolveBoolWithSource(fieldName, field, def, envKey)
	return v
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

// BaseURLLabel renders a resolved data-plane base for an install plan. It lived
// in role.go, which went with the approver persona.
func BaseURLLabel(baseURL string) string {
	if baseURL == "" {
		return DefaultBaseURL + "  (default; the SaaS core)"
	}
	return baseURL
}
