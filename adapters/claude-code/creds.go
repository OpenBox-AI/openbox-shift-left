package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/sidecar"
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
	envBaseURL         = "OPENBOX_BASE_URL"
	envDID             = "OPENBOX_AGENT_DID"
	envSecretService   = "OPENBOX_SECRET_SERVICE"
	envAPIKeyAccount   = "OPENBOX_API_KEY_ACCOUNT"
	envPrivKeyAccount  = "OPENBOX_PRIVATE_KEY_ACCOUNT"
	envContentCapture  = "OPENBOX_CONTENT_CAPTURE"
	envFinops          = "OPENBOX_FINOPS"
	envInstallGitHook  = "OPENBOX_INSTALL_GIT_HOOK"
	envEnforce         = "OPENBOX_ENFORCE"
	envFailClosed      = "OPENBOX_FAIL_CLOSED"
	envEnforceTimeout  = "OPENBOX_ENFORCE_TIMEOUT_MS"
	envTier2           = "OPENBOX_TIER2"
	envTier2Timeout    = "OPENBOX_TIER2_TIMEOUT_MS"
	envSidecarSocket   = "OPENBOX_SIDECAR_SOCKET"
	envSecretDetection = "OPENBOX_SECRET_DETECTION"
	envFindings        = "OPENBOX_FINDINGS"
	envFindingsCursor  = "OPENBOX_FINDINGS_CURSOR"
	envEnforcementFile = "OPENBOX_ENFORCEMENT_FILE"
	envAPIKeyDirect    = "OPENBOX_API_KEY"
	envSeedDirect      = "OPENBOX_ED25519_SEED"
	envConfigPath      = "OPENBOX_CONFIG"
	envSecretFile      = "OPENBOX_SECRET_FILE"
	envAgentID         = "OPENBOX_AGENT_ID"
	envBackendURL      = "OPENBOX_BACKEND_URL"
	envControlToken    = "OPENBOX_CONTROL_TOKEN"
	envSidecarBundle   = "OPENBOX_SIDECAR_BUNDLE"
	envStaleDir        = "OPENBOX_STALE_DIR"

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
	// ContentCapture is the org content posture (OD4). It is a *bool so an ABSENT
	// field means the DEFAULT, which is now ON (brian 2026-07-15 — reverses the
	// original metadata-only-by-default INV-2/OD4/NFR-1 posture): prompts/file
	// bodies/outputs are captured onto emitted events and egressed unless an org
	// opts OUT. Set false (`content_capture:false` or OPENBOX_CONTENT_CAPTURE=0) to
	// restore metadata-only. Guardrail redaction at source ([EXT-guardrail-redaction])
	// is still inert, and Tier-1 local secret detection (E6-S9, enforce mode) only
	// covers Write/Edit bodies — so with capture ON, other content egresses UNREDACTED.
	ContentCapture    *bool  `json:"content_capture,omitempty"`
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
	// FailClosed selects the per-org FAILURE POLICY for enforce mode (STORY-E6-S3,
	// OD9). Default false = FAIL-OPEN: when the local sidecar cannot deliver a real
	// verdict (absent, timeout, malformed) the tool PROCEEDS (degrade to observe) —
	// an OpenBox outage never blocks a developer. Set true to opt into FAIL-CLOSED:
	// the same outage DENIES the tool call. This mirrors the reference SDK's
	// governance_policy (fail_open|fail_closed, on_api_error). It ONLY changes the
	// evaluation-UNAVAILABLE case — a real ALLOW/CONSTRAIN verdict from a reachable
	// sidecar still proceeds under either policy. OPENBOX_FAIL_CLOSED overrides it.
	FailClosed bool `json:"fail_closed,omitempty"`
	// EnforceTimeoutMS overrides the hard per-call decision budget (milliseconds)
	// the enforce hook allows the sidecar before it gives up (STORY-E6-S3; from
	// spike S2). 0/absent ⇒ sidecar.DefaultDecisionTimeout (~50 ms, ADR-0002). It is
	// CLAMPED to maxEnforceTimeout (2 s) so the whole PreToolUse hook stays well
	// under Claude Code's 5 s hook kill — past that, CC kills the hook and the tool
	// proceeds (a CC-layer fail-OPEN), which would silently defeat a fail-CLOSED org.
	// A fail-closed org may raise it to ride out transient sidecar slowness without
	// spuriously blocking. OPENBOX_ENFORCE_TIMEOUT_MS overrides it.
	EnforceTimeoutMS int `json:"enforce_timeout_ms,omitempty"`
	// Tier2 enables the Tier-2 synchronous /evaluate escalation for high-risk
	// classes (Bash / MCP execution) in enforce mode (STORY-E6-S10, design §7). It
	// is a *bool so an ABSENT field means the DEFAULT (OFF): T2 is opt-in because it
	// adds hot-path secret I/O + a ~1.6 s network round-trip on high-risk calls, and
	// because T1-only enforce ("v1 minus T2", design §7 Option C) is a valid honest
	// posture. With it off the enforce path is byte-identical to E6-S9 (T1-only). Set
	// true (config `tier2:true` or OPENBOX_TIER2) to close the §2a policy-only floor
	// for arbitrary execution. Only meaningful in enforce mode (ResolveEnforce).
	Tier2 *bool `json:"tier2,omitempty"`
	// Tier2TimeoutMS overrides the in-binary budget (milliseconds) one T2 /evaluate
	// escalation may take (STORY-E6-S10). 0/absent ⇒ defaultTier2Timeout (3.5 s). It
	// is CLAMPED to maxTier2Timeout (4 s) so the whole PreToolUse hook stays under
	// Claude Code's 5 s hook timeout (CC fails OPEN on a hook timeout — the same
	// correctness bound as EnforceTimeoutMS, scaled for the network round-trip).
	// OPENBOX_TIER2_TIMEOUT_MS overrides it.
	Tier2TimeoutMS int `json:"tier2_timeout_ms,omitempty"`
	// SidecarSocket overrides the Unix socket the enforce hook dials (default:
	// sidecar.DefaultSocketPath()). The OPENBOX_SIDECAR_SOCKET env overrides it —
	// the SAME env `openbox sidecar serve` reads, so the daemon and the hook agree.
	SidecarSocket string `json:"sidecar_socket,omitempty"`
	// SecretDetection enables Tier-1 local secret/entropy detection + redact-and-
	// continue (STORY-E6-S9, OD-SYNC-10). It is a *bool so an ABSENT field means the
	// DEFAULT (ON): the protection is opt-OUT, not opt-in, because the detected
	// secret/redaction stays strictly LOCAL (the file body reaches only the Unix
	// socket; the redaction rides sidecar.Decision, never client.Evaluation) — so it
	// honors INV-2 (egress-only) WITHOUT the OD4 content-capture opt-in, which
	// governs EGRESS. Set false to disable. OPENBOX_SECRET_DETECTION overrides it.
	// Only meaningful in enforce mode.
	SecretDetection *bool `json:"secret_detection,omitempty"`
	// Findings enables the Tier-3 FINDINGS LOOP (STORY-E6-S11, design §7 T3): surface
	// async governance findings (guardrail categories, goal-drift, risk, would-block)
	// recorded on the flush path (the SL-9 advisories.jsonl sink) back INTO the session
	// as a content-free summary — on UserPromptSubmit + PostToolUse via
	// hookSpecificOutput.additionalContext (→ model) + systemMessage (→ user). It is a
	// *bool so an ABSENT field means the DEFAULT (OFF): findings is opt-in because it is
	// the FIRST time the observe path writes to a hook's stdout, so it must be chosen
	// explicitly; with it off, UserPromptSubmit/PostToolUse write NOTHING (byte-identical
	// to Phase-1). It NEVER blocks (only additionalContext/systemMessage, no decision —
	// INV-3) and surfaces only CATEGORIES/COUNTS, never content (INV-2). Orthogonal to
	// enforce — the findings loop is advisory feedback in BOTH observe and enforce
	// sessions. Set true (config `findings:true` or OPENBOX_FINDINGS). Env overrides.
	Findings *bool `json:"findings,omitempty"`
	// AgentID is the backend agent id used to fetch this agent's current policy
	// (STORY-E6-S8, ADR-0005): `openbox dev sync` and the session-start staleness
	// check read it to call GET /agent/<id>/policies/current. Non-secret (INV-1),
	// persisted by `dev init`. OPENBOX_AGENT_ID overrides (ResolveAgentID).
	AgentID string `json:"agent_id,omitempty"`
	// BackendURL is the openbox-backend CONTROL-PLANE base (distinct from BaseURL,
	// the core data-plane base) used for the policy read. Persisted by `dev init`
	// so `dev sync`/staleness need not re-supply it. OPENBOX_BACKEND_URL overrides
	// (ResolveBackendURL). Non-secret.
	BackendURL string `json:"backend_url,omitempty"`
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

// ResolveContentCapture reports the org content posture (OD4): config
// `content_capture` first, then the OPENBOX_CONTENT_CAPTURE env override (env wins
// either way, so env can disable what config enabled), same precedence as every
// other coordinate. DEFAULT ON as of brian 2026-07-15 (reverses the original
// metadata-only-by-default INV-2/OD4/NFR-1 posture) — an ABSENT config field (the
// normal case, since `dev init` writes no content_capture) yields ON: tool content
// reaches the local sidecar AND egresses on emitted events, and the enforce hook
// applies `updatedInput` redaction. Set `content_capture:false` or
// OPENBOX_CONTENT_CAPTURE=0 to opt back to metadata-only. Modeled as *bool (like
// SecretDetection) so absent (default ON) is distinguishable from an explicit
// false (opt-out). A missing/unreadable config leaves the default ON. Cheap
// config+env read, no secret I/O; safe on the PreToolUse hot path. It mirrors the
// ContentCaptureEnabled that ResolveCredentials derives, without the secret-store work.
func ResolveContentCapture() bool {
	enabled := true
	if cfg, err := loadDevConfig(DefaultConfigPath()); err == nil && cfg.ContentCapture != nil {
		enabled = *cfg.ContentCapture
	}
	if v, ok := os.LookupEnv(envContentCapture); ok {
		enabled = isTruthy(v)
	}
	return enabled
}

// ResolveSecretDetection reports whether Tier-1 local secret/entropy detection is
// on (STORY-E6-S9, OD-SYNC-10). DEFAULT TRUE — opt-OUT, not opt-in: an absent
// config field keeps it on; config `secret_detection:false` disables it; the
// OPENBOX_SECRET_DETECTION env overrides either way (env wins). Unlike
// ResolveContentCapture (which defaults FALSE and governs EGRESS), this is on by
// default because the file body it acts on reaches ONLY the local sidecar and the
// redaction stays LOCAL — never egressed (INV-2 is egress-only). A
// missing/unreadable config leaves the default ON (the protection never turns
// itself off by accident). Cheap config+env read, no secret I/O; safe on the hot
// path. Only meaningful in enforce mode (ResolveEnforce).
func ResolveSecretDetection() bool {
	enabled := true
	if cfg, err := loadDevConfig(DefaultConfigPath()); err == nil && cfg.SecretDetection != nil {
		enabled = *cfg.SecretDetection
	}
	if v, ok := os.LookupEnv(envSecretDetection); ok {
		enabled = isTruthy(v)
	}
	return enabled
}

// ResolveFindings reports whether the Tier-3 findings loop is on (STORY-E6-S11):
// config `findings` (*bool) first, then the OPENBOX_FINDINGS env override (env wins).
// DEFAULT FALSE — opt-in, because it is the first observe-path stdout writer: with it
// unset, UserPromptSubmit/PostToolUse write NOTHING and the surface path is
// byte-identical to Phase-1. A missing/unreadable config leaves it OFF (fail-safe — a
// surface that injects into the model/user context never turns itself on by accident).
// Cheap config+env read, no secret I/O; safe on the PostToolUse hot path. Independent
// of enforce (advisory feedback works in observe and enforce sessions alike).
func ResolveFindings() bool {
	enabled := false
	if cfg, err := loadDevConfig(DefaultConfigPath()); err == nil && cfg.Findings != nil {
		enabled = *cfg.Findings
	}
	if v, ok := os.LookupEnv(envFindings); ok {
		enabled = isTruthy(v)
	}
	return enabled
}

// ResolveFindingsCursor resolves the path of the findings-loop cursor state file
// (STORY-E6-S11): the OPENBOX_FINDINGS_CURSOR env override, else a fixed file next to
// the advisory sink under the user config dir. It stores only a byte offset into
// advisories.jsonl (structural, content-free — INV-2). No secret I/O.
func ResolveFindingsCursor() string {
	if p := os.Getenv(envFindingsCursor); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "findings.cursor")
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

// maxEnforceTimeout caps the configurable enforce decision budget. It is a
// CORRECTNESS bound, not a nicety: Claude Code kills the PreToolUse hook at 5 s
// (plugin/hooks/hooks.json), and a hook-kill lets the tool proceed — a CC-layer
// fail-OPEN that would silently defeat a fail-CLOSED org. Clamping the whole
// enforce wait to 2 s keeps the full hook (config read + gate + spool) under that
// kill so a fail-closed deny is actually delivered, and keeps INV-3b bounded.
const maxEnforceTimeout = 2 * time.Second

// ResolveFailClosed reports the enforce FAILURE POLICY (STORY-E6-S3, OD9): config
// field first, then the OPENBOX_FAIL_CLOSED env override (env wins), same
// precedence as every other coordinate. Default FALSE = fail-OPEN (OD9): an
// OpenBox outage degrades to observe and the tool proceeds. True = fail-CLOSED:
// the same outage denies. A missing/unreadable config is false (fail-safe — an org
// never becomes fail-closed by accident, and a config read error never turns
// enforcement's teeth ON). Cheap config+env read, no secret I/O; safe on the hot
// path. Only meaningful in enforce mode (ResolveEnforce).
func ResolveFailClosed() bool {
	enabled := false
	if cfg, err := loadDevConfig(DefaultConfigPath()); err == nil {
		enabled = cfg.FailClosed
	}
	if v, ok := os.LookupEnv(envFailClosed); ok {
		enabled = isTruthy(v)
	}
	return enabled
}

// ResolveEnforceTimeout resolves the hard per-call decision budget the enforce
// hook allows the local sidecar (STORY-E6-S3; the S2 timeout made a knob):
// enforce_timeout_ms config first, then OPENBOX_ENFORCE_TIMEOUT_MS (env wins when
// present AND parseable — a garbage env value is ignored and the config value
// stands, so a fat-fingered env never silently wipes a valid config). A resolved
// value <=0 yields 0; the caller passes 0 to sidecar.NewClient, which substitutes
// sidecar.DefaultDecisionTimeout (~50 ms, ADR-0002), so the default behavior is
// byte-identical to E6-S1. A positive value over maxEnforceTimeout is clamped
// (INV-3b bounded + keeps the hook under CC's 5 s kill). No secret I/O; a
// missing/unreadable config degrades to env-or-default.
func ResolveEnforceTimeout() time.Duration {
	ms := 0
	if cfg, err := loadDevConfig(DefaultConfigPath()); err == nil {
		ms = cfg.EnforceTimeoutMS
	}
	if v, ok := os.LookupEnv(envEnforceTimeout); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			ms = n
		}
	}
	if ms <= 0 {
		return 0 // ⇒ sidecar.DefaultDecisionTimeout
	}
	// Clamp in MILLISECONDS before the multiply so a near-max-int64 value can never
	// overflow time.Duration (which would wrap to a negative/huge duration). Bounded
	// by construction, not by accident (G_SEC INFO-2).
	if maxMS := int64(maxEnforceTimeout / time.Millisecond); int64(ms) > maxMS {
		return maxEnforceTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

// ResolveTier2 reports whether the Tier-2 synchronous /evaluate escalation is on
// (STORY-E6-S10, design §7). DEFAULT FALSE — opt-in, UNLIKE secret detection: T2
// adds hot-path secret I/O + a network round-trip on high-risk calls, so it must be
// chosen explicitly. Config `tier2` first, then the OPENBOX_TIER2 env override (env
// wins either way). A missing/unreadable config leaves it OFF (fail-safe — the
// latency-adding tier never turns itself on by accident). Cheap config+env read, no
// secret I/O; safe on the PreToolUse hot path. Only meaningful in enforce mode.
func ResolveTier2() bool {
	enabled := false
	if cfg, err := loadDevConfig(DefaultConfigPath()); err == nil && cfg.Tier2 != nil {
		enabled = *cfg.Tier2
	}
	if v, ok := os.LookupEnv(envTier2); ok {
		enabled = isTruthy(v)
	}
	return enabled
}

// ResolveTier2Timeout resolves the in-binary budget for one Tier-2 /evaluate
// escalation (STORY-E6-S10): tier2_timeout_ms config first, then
// OPENBOX_TIER2_TIMEOUT_MS (env wins when present AND parseable — a garbage env
// value is ignored so it never silently wipes a valid config). A resolved value
// <=0 yields defaultTier2Timeout; a positive value over maxTier2Timeout is clamped
// (the correctness bound: the hook must return before CC's 5 s hook timeout, which
// fails OPEN). No secret I/O; a missing/unreadable config degrades to env-or-default.
func ResolveTier2Timeout() time.Duration {
	ms := 0
	if cfg, err := loadDevConfig(DefaultConfigPath()); err == nil {
		ms = cfg.Tier2TimeoutMS
	}
	if v, ok := os.LookupEnv(envTier2Timeout); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			ms = n
		}
	}
	if ms <= 0 {
		return defaultTier2Timeout
	}
	// Clamp in MILLISECONDS before the multiply so a near-max-int64 value can never
	// overflow time.Duration (mirrors ResolveEnforceTimeout's overflow-proof clamp).
	if maxMS := int64(maxTier2Timeout / time.Millisecond); int64(ms) > maxMS {
		return maxTier2Timeout
	}
	return time.Duration(ms) * time.Millisecond
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

// ResolveAgentID resolves the backend agent id for policy sync/staleness
// (STORY-E6-S8): OPENBOX_AGENT_ID env first, then the dev config's agent_id
// (persisted by `dev init`). Empty when nothing configures it (the caller then
// skips the staleness check — never blocks). No secret I/O.
func ResolveAgentID() string {
	cfg, _ := loadDevConfig(DefaultConfigPath())
	return firstNonEmpty(os.Getenv(envAgentID), cfg.AgentID)
}

// ResolveBackendURL resolves the openbox-backend CONTROL-PLANE base URL for the
// policy read: OPENBOX_BACKEND_URL env first, then the dev config's backend_url.
// Empty when unconfigured (staleness is then skipped — proceed). No secret I/O.
func ResolveBackendURL() string {
	cfg, _ := loadDevConfig(DefaultConfigPath())
	return firstNonEmpty(os.Getenv(envBackendURL), cfg.BackendURL)
}

// ResolveControlToken resolves the ORG control-plane credential for the policy
// read (OD-SYNC-4): the OPENBOX_CONTROL_TOKEN env ONLY. It is deliberately NOT a
// config field and NEVER read from the runtime secret store — it is a
// control-plane credential (an obx_key_ org key or Keycloak JWT), distinct from
// the agent runtime obx_ key, supplied via env only so it cannot leak via a
// config file or argv (INV-1). Empty when absent (staleness/sync then proceed on
// the last-good bundle — never deny at fetch time).
func ResolveControlToken() string {
	return os.Getenv(envControlToken)
}

// ResolveBundlePath resolves the local policy-bundle path the daemon serves and
// `dev sync`/staleness read: OPENBOX_SIDECAR_BUNDLE env, else the sidecar default.
// The daemon reads the SAME env, so hook and daemon agree.
func ResolveBundlePath() string {
	if p := os.Getenv(envSidecarBundle); p != "" {
		return p
	}
	return sidecar.DefaultBundlePath()
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
		// Content capture (OD4): DEFAULT ON (brian 2026-07-15) — an absent config
		// field means ON; an explicit `content_capture:false` opts out. Overridable
		// EITHER way by env (so env can disable what config enabled — consistent with
		// the other coordinates). Mirrors ResolveContentCapture.
		ContentCaptureEnabled: cfg.ContentCapture == nil || *cfg.ContentCapture,
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
