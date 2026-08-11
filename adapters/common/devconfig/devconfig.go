// Package devconfig is the provider-neutral developer-runtime
// configuration and credential resolution shared by every tool adapter
// (Claude Code, Codex, Cursor). It owns the `~/.config/openbox/dev.json`
// contract the installers write and the hook binaries read, plus the
// OS/file secret-store readers (module home recorded in ADR-0007).
//
// It was extracted, behavior-preserving, from
// adapters/claude-code/creds.go — that adapter keeps thin aliases so its
// public API and tests are unchanged. Like adapters/common/git, this
// module is dependency-free: it never imports the client, an adapter, or
// the CLI.
//
// INV-1 is load-bearing throughout: dev.json and the env carry only
// non-secret coordinates (where the secrets live, never the secret
// values); the obx_ key and Ed25519 seed are read from the secret store
// only at flush time and are never logged, printed, or placed on an argv.
package devconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Env vars: non-secret coordinates + optional direct overrides for CI/tests.
// The names are the cross-adapter contract (the same dev.json serves every
// provider), so they are exported here and aliased by the adapters.
const (
	EnvBaseURL         = "OPENBOX_BASE_URL"
	EnvDID             = "OPENBOX_AGENT_DID"
	EnvSecretService   = "OPENBOX_SECRET_SERVICE"
	EnvAPIKeyAccount   = "OPENBOX_API_KEY_ACCOUNT"
	EnvPrivKeyAccount  = "OPENBOX_PRIVATE_KEY_ACCOUNT"
	EnvContentCapture  = "OPENBOX_CONTENT_CAPTURE"
	EnvFinops          = "OPENBOX_FINOPS"
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
	// EnvPendingApprovalDir relocates the filed-approval markers the gate and
	// the rewake watcher coordinate through (tests point it at a temp dir).
	EnvPendingApprovalDir = "OPENBOX_PENDING_APPROVAL_DIR"
	EnvAPIKeyDirect       = "OPENBOX_API_KEY"
	EnvSeedDirect         = "OPENBOX_ED25519_SEED"
	EnvConfigPath         = "OPENBOX_CONFIG"
	// Policy-bundle signing key pins (E8-S6). Non-secret; env overrides config.
	EnvOrgSigningPubKey = "OPENBOX_ORG_SIGNING_PUBKEY"
	EnvOrgSigningKeyID  = "OPENBOX_ORG_SIGNING_KEY_ID"
	EnvSecretFile       = "OPENBOX_SECRET_FILE"
	EnvAgentID          = "OPENBOX_AGENT_ID"
	EnvBackendURL       = "OPENBOX_BACKEND_URL"
	EnvControlToken     = "OPENBOX_CONTROL_TOKEN"
	EnvSpoolDir         = "OPENBOX_SPOOL_DIR"

	// DefaultBaseURL is the core data-plane base used when nothing configures one.
	DefaultBaseURL = "https://core.openbox.ai"
)

// DevConfig is the non-secret coordinate file the installers write and the
// hooks read (INV-1: it holds where the secrets live, never the secret values).
// Env vars of the same meaning override any field. One file serves every
// provider — the coordinates, content posture, and ADR-0006 enforce posture are
// org/developer-scoped, not tool-scoped.
//
// Field semantics (defaults, opt-in/out rationale) are documented on the
// resolver functions below; the *bool fields distinguish "absent = adapter
// default" from an explicit false.
type DevConfig struct {
	BaseURL           string `json:"base_url,omitempty"`
	DID               string `json:"developer_did,omitempty"`
	SecretService     string `json:"secret_service,omitempty"`
	APIKeyAccount     string `json:"api_key_account,omitempty"`
	PrivateKeyAccount string `json:"private_key_account,omitempty"`
	// ContentCapture is the org content posture. Absent means the default,
	// which is on (reverses metadata-only-by-default).
	ContentCapture *bool `json:"content_capture,omitempty"`
	// Finops gates per-turn usage capture: token counts AND the model id that
	// spent them (ADR-0014 — the name predates the model binding and is kept
	// because renaming a config key is a user-visible break; read it as "usage
	// and model capture", not "token counts only").
	//
	// A POINTER, and that is load-bearing rather than stylistic. It was a plain
	// bool whose resolver returned `&b` unconditionally, so `resolveBool` never
	// reached its default and an absent `finops` field was indistinguishable from
	// an explicit `false`. Flipping the default with a plain bool would have
	// produced a flip that silently did nothing for every existing config file —
	// and worse, pinned every absent field to false forever. Tri-state is what
	// makes "absent means the default" true. Compare ContentCapture above.
	//
	// Default ON, as of the same reasoning that flipped content capture: absent
	// means on; `finops:false` or OPENBOX_FINOPS=0 opts out.
	Finops *bool `json:"finops,omitempty"`
	// InstallGitHook enables ambient install of the prepare-commit-msg
	// hook on SessionStart. Default false — it modifies a repo's
	// .git/hooks.
	InstallGitHook bool `json:"install_git_hook,omitempty"`
	// Enforce flips the developer runtime from observe/advisory to
	// enforce (ADR-0006, persisted by `init --enforce`). Default
	// false.
	Enforce bool `json:"enforce,omitempty"`
	// FailClosed selects the enforce failure policy. Default false =
	// fail-open: an OpenBox outage never blocks a developer.
	FailClosed bool `json:"fail_closed,omitempty"`
	// EnforceTimeoutMS is inert under the in-process decider (ADR-0006);
	// retained for back-compat parsing. Clamping is adapter-owned.
	EnforceTimeoutMS int `json:"enforce_timeout_ms,omitempty"`
	// Tier2 enables the Tier-2 synchronous /evaluate escalation for
	// high-risk classes in enforce mode. Absent = default off (opt-in).
	Tier2 *bool `json:"tier2,omitempty"`
	// Tier2TimeoutMS bounds one Tier-2 escalation (ms). Clamping is adapter-owned.
	Tier2TimeoutMS int `json:"tier2_timeout_ms,omitempty"`
	// ApprovalHoldMS bounds how long the gate holds a tool call while a filed
	// approval is decided (ms, OD-E9-1). Absent = the engine default. Clamping
	// is adapter-owned: the hold can never outlive the provider's hook timeout.
	ApprovalHoldMS int `json:"approval_hold_ms,omitempty"`
	// SecretDetection enables Tier-1 local secret/entropy detection.
	// Absent = default on (opt-out): the detection stays strictly local.
	SecretDetection *bool `json:"secret_detection,omitempty"`
	// RequireVerifiedBundle refuses to load a policy bundle whose signature did
	// not verify (OD-RF-3), turning the signature from detection into
	// prevention. Absent = false, because the backend does not sign yet and
	// requiring it today would leave every install bundle-less. Lockable, so an
	// org that has deployed signing can mandate it.
	RequireVerifiedBundle *bool `json:"require_verified_bundle,omitempty"`
	// Findings enables the Tier-3 findings loop. Absent = default off
	// (opt-in: it is the first observe-path stdout writer).
	Findings *bool `json:"findings,omitempty"`
	// RealtimeFlush enables the debounced background flush that delivers
	// spooled events to core mid-session instead of only at SessionEnd.
	// Absent = default on (opt-out): it changes only delivery timing, never
	// what egresses, and the hook hot path stays free of network I/O.
	RealtimeFlush *bool `json:"realtime_flush,omitempty"`
	// AgentID is the backend agent id for the policy read. Non-secret.
	AgentID string `json:"agent_id,omitempty"`
	// BackendURL is the openbox-backend control-plane base (distinct from
	// BaseURL, the core data-plane base). Non-secret.
	BackendURL string `json:"backend_url,omitempty"`
	// SecretFile, when set, points at the CLI's opt-in file secret backend
	// (0600 JSON) read instead of the OS keychain. EnvSecretFile overrides.
	SecretFile string `json:"secret_file,omitempty"`
	// OrgSigningKeyID and OrgSigningPubKey pin the org's policy-bundle signing
	// key (E8-S6 / ADR-0008). Both are non-secret — a public key and its id —
	// so they live in plain config alongside the other coordinates rather than
	// in the secret store (INV-1 concerns private material only).
	//
	// Absent means unverifiable rather than untrusted: a signed bundle with no
	// pinned key reports integrity "no_key", which reads as an incomplete
	// deployment. Its policy is still loaded and enforced — the same treatment an
	// unsigned bundle gets, since both mean "this client cannot check the
	// content" — but Trusted() is false and the session posture says so, so the
	// control plane can tell an unpinned fleet from a verified one.
	// `openbox init` pins these once the backend serves them.
	OrgSigningKeyID  string `json:"org_signing_key_id,omitempty"`
	OrgSigningPubKey string `json:"org_signing_pubkey,omitempty"` // base64 raw Ed25519
}

// DefaultConfigPath is where the installer writes the dev config and the hook
// looks for it when OPENBOX_CONFIG is unset. Under XDG on Linux / the standard
// config dir elsewhere.
func DefaultConfigPath() string {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "dev.json")
}

// Load reads the dev config at path if present. A missing file is not an error
// (env may supply everything); a malformed file is an error surfaced fail-open.
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

// load reads the default-path config, swallowing the not-exist case.
func load() (DevConfig, error) { return Load(DefaultConfigPath()) }

// Credentials is the resolved runtime identity for a hook binary. It carries
// the secret VALUES (read straight from the secret store) — it exists only in
// process memory on the flush path and must never be logged or persisted.
type Credentials struct {
	BaseURL               string
	APIKey                string
	DID                   string
	SeedB64               string
	ContentCaptureEnabled bool
}

// SecretLookup reads one secret by (service, account). OSSecretLookup is the
// production implementation; adapters keep an injectable var for tests.
type SecretLookup func(service, account string) (string, error)

// ResolveDID resolves only the developer DID (env, then config file) — no
// secret-store access. This is the hot path: observe/spool needs the DID
// to attribute events but never the obx_ key or seed, so a tool-use hook
// does zero secret I/O (INV-1).
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

// ResolveCoordinates resolves the non-secret target coordinates — the
// core base URL and the developer DID — from env then the dev config,
// with zero secret-store access (INV-1). Backs read-only previews (`dev
// verify --dry-run`). A missing/unreadable config degrades to env +
// defaults; did is "" when nothing configures it.
func ResolveCoordinates() (baseURL, did string) {
	cfg, _ := load()
	baseURL = FirstNonEmpty(os.Getenv(EnvBaseURL), cfg.BaseURL, DefaultBaseURL)
	did = FirstNonEmpty(os.Getenv(EnvDID), cfg.DID)
	return baseURL, did
}

// SpoolDir is where hot-path events are spooled before flush: the
// OPENBOX_SPOOL_DIR override when set, else `<user-config>/openbox/<subdir>`.
// Each adapter passes its own subdir (cc-spool, codex-spool, …) so concurrent
// tools never share a spool by accident, while the env override still lets
// tests (and operators) pin one location.
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
// Default false (it modifies a repo's .git/hooks); config enables; env
// overrides either way. A missing/unreadable config is false (fail-safe).
func ResolveInstallGitHook() bool {
	return resolveBool("install_git_hook", func(c DevConfig) *bool { b := c.InstallGitHook; return &b }, false, EnvInstallGitHook)
}

// ResolveFinops reports whether usage capture is enabled — per-turn token counts
// and the model id that spent them (ADR-0014).
//
// **Default ON.** An absent `finops` field resolves to on, mirroring the
// 2026-07-15 content-capture reversal; `finops:false` in managed config or
// OPENBOX_FINOPS=0 opts out, and the env wins either way. With it off,
// transcript_path is never opened, no turn event is emitted, and no model id
// reaches the wire beyond the one SessionStarted already carried.
//
// The default is an EGRESS-POSTURE decision, not a convenience: four integers and
// a model id per turn now leave developer machines for orgs that never asked. It
// gets what content capture got — a documented default, two opt-outs, and the
// effective state recorded on SessionStarted as posture evidence, so an auditor
// can tell after the fact which sessions captured. See docs/data-and-privacy.md.
//
// Before this was a *bool the flip was impossible to make correctly — see the
// Finops field comment.
func ResolveFinops() bool {
	return resolveBool("finops", func(c DevConfig) *bool { return c.Finops }, true, EnvFinops)
}

// ResolveContentCapture reports the org content posture: config
// `content_capture` first, then the env override (env wins either way).
// Default on — an absent config field yields on; set
// `content_capture:false` or OPENBOX_CONTENT_CAPTURE=0 to opt back to
// metadata-only. A missing/unreadable config leaves the default ON. Cheap
// config+env read, no secret I/O; safe on the hot path.
func ResolveContentCapture() bool {
	return resolveBool("content_capture", func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture)
}

// ResolveSecretDetection reports whether Tier-1 local secret/entropy
// detection is on. Default true — opt-out: the body it acts on reaches
// only the local decider and the redaction stays local (INV-2 is
// egress-only).
func ResolveSecretDetection() bool {
	return resolveBool("secret_detection", func(c DevConfig) *bool { return c.SecretDetection }, true, EnvSecretDetection)
}

// ResolveRequireVerifiedBundle reports whether an unverified policy bundle must
// be refused rather than enforced (OD-RF-3). Default false.
func ResolveRequireVerifiedBundle() bool {
	return resolveBool("require_verified_bundle", func(c DevConfig) *bool { return c.RequireVerifiedBundle }, false, EnvRequireVerified)
}

// ResolveFindings reports whether the Tier-3 findings loop is on. Default
// false — opt-in, because it is the first observe-path stdout writer.
func ResolveFindings() bool {
	return resolveBool("findings", func(c DevConfig) *bool { return c.Findings }, false, EnvFindings)
}

// ResolveRealtime reports whether the debounced background flush is on —
// spooled events are delivered to core within a debounce window of each hook
// instead of waiting for SessionEnd. Default on; set `realtime_flush:false`
// or OPENBOX_REALTIME=0 to restore batch-at-session-end. Timing-only: the
// content posture (ResolveContentCapture) is untouched, and with it off the
// trigger performs zero I/O.
func ResolveRealtime() bool {
	return resolveBool("realtime_flush", func(c DevConfig) *bool { return c.RealtimeFlush }, true, EnvRealtime)
}

// ResolveFindingsCursor resolves the findings-loop cursor state file: the
// env override, else a fixed file next to the advisory sink. It stores
// only a byte offset (structural, content-free — INV-2).
func ResolveFindingsCursor(provider string) string {
	if p := os.Getenv(EnvFindingsCursor); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	// One cursor per provider (OD-RF-1). The advisory sink is deliberately
	// shared — it is developer-scoped, not tool-scoped — but a shared CURSOR
	// meant whichever tool's hook fired first consumed the delta and advanced
	// it for both. Findings a Codex session caused could then surface only into
	// a Claude Code session, and vice versa: cross-tool bleed one way, silence
	// the other. Per-provider cursors mean each tool sees every finding exactly
	// once. A finding relevant to both surfaces in both, which is the right
	// direction to be wrong in for a nudge.
	name := "findings.cursor"
	if p := sanitizeProvider(provider); p != "" {
		name = "findings-" + p + ".cursor"
	}
	return filepath.Join(dir, "openbox", name)
}

// sanitizeProvider reduces a provider name to a safe filename component, so a
// caller cannot steer the cursor path.
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

// ResolveEnforce reports whether the developer runtime is in enforce mode
// (ADR-0006): config field first, then the env override. Default false —
// observe/advisory; a config read error never turns enforcement on
// (INV-3).
func ResolveEnforce() bool {
	return resolveBool("enforce", func(c DevConfig) *bool { b := c.Enforce; return &b }, false, EnvEnforce)
}

// ResolveFailClosed reports the enforce failure policy. Default false =
// fail-open; an org never becomes fail-closed by accident.
func ResolveFailClosed() bool {
	return resolveBool("fail_closed", func(c DevConfig) *bool { b := c.FailClosed; return &b }, false, EnvFailClosed)
}

// ResolveTier2 reports whether the Tier-2 synchronous escalation is on.
// Default false — opt-in (it adds hot-path secret I/O + latency).
func ResolveTier2() bool {
	return resolveBool("tier2", func(c DevConfig) *bool { return c.Tier2 }, false, EnvTier2)
}

// ResolveTimeoutMS resolves a millisecond budget knob: the config field
// first (via cfgMS), then the env override when present and parseable —
// a garbage env value is ignored and the config value stands, so a
// fat-fingered env never silently wipes a valid config. Clamping is
// deliberately the caller's job: each adapter owns its correctness bound
// (e.g. Claude Code's 5s hook kill).
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

// ResolveAgentID resolves the backend agent id for policy sync/staleness:
// env first, then the dev config. Empty when unconfigured.
// ResolveOrgSigningKey returns the pinned policy-bundle signing key (base64 raw
// Ed25519) and its id. Env overrides config, matching every other coordinate.
// Both empty ⇒ no key pinned, which verification reports as "no_key".
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

// ResolveBackendURL resolves the openbox-backend control-plane base URL:
// env first, then the dev config. Empty when unconfigured.
func ResolveBackendURL() string {
	cfg, _ := load()
	return FirstNonEmpty(os.Getenv(EnvBackendURL), cfg.BackendURL)
}

// ResolveControlToken resolves the org control-plane credential: the
// OPENBOX_CONTROL_TOKEN env only. Deliberately not a config field and
// never read from the runtime secret store — supplied via env only so it
// cannot leak via a config file or argv (INV-1).
func ResolveControlToken() string {
	return os.Getenv(EnvControlToken)
}

// ResolveCredentials assembles Credentials from env + the dev config + the
// given secret source. It returns an error (never a panic) when identity is
// incomplete; the caller logs it fail-open and exits 0 (INV-3). No secret value
// is ever included in a returned error. When the config/env points at the
// opt-in file secret backend, it is used instead of lookup; the direct env
// overrides win over both.
func ResolveCredentials(lookup SecretLookup) (Credentials, error) {
	cfg, err := load()
	if err != nil {
		return Credentials{}, err
	}

	c := Credentials{
		BaseURL: FirstNonEmpty(os.Getenv(EnvBaseURL), cfg.BaseURL, DefaultBaseURL),
		DID:     FirstNonEmpty(os.Getenv(EnvDID), cfg.DID),
		// Content capture: default on — an absent config field means on; an
		// explicit false opts out; env overrides either way (mirrors ResolveContentCapture).
		ContentCaptureEnabled: cfg.ContentCapture == nil || *cfg.ContentCapture,
	}
	if v, ok := os.LookupEnv(EnvContentCapture); ok {
		c.ContentCaptureEnabled = IsTruthy(v)
	}
	if c.DID == "" {
		return Credentials{}, fmt.Errorf("no developer DID configured (run `openbox init`)")
	}

	service := FirstNonEmpty(os.Getenv(EnvSecretService), cfg.SecretService)
	apiKeyAccount := FirstNonEmpty(os.Getenv(EnvAPIKeyAccount), cfg.APIKeyAccount)
	privKeyAccount := FirstNonEmpty(os.Getenv(EnvPrivKeyAccount), cfg.PrivateKeyAccount)

	// Secret source: the CLI's opt-in file backend (when a path is configured)
	// or the given lookup (normally the OS secret store). Direct env overrides win.
	if lookup == nil {
		lookup = OSSecretLookup
	}
	if secretFile := FirstNonEmpty(os.Getenv(EnvSecretFile), cfg.SecretFile); secretFile != "" {
		lookup = func(svc, acct string) (string, error) { return FileSecretLookup(secretFile, svc, acct) }
	}

	// obx_ key: direct override, else secret source.
	if v := os.Getenv(EnvAPIKeyDirect); v != "" {
		c.APIKey = v
	} else if service != "" && apiKeyAccount != "" {
		v, err := lookup(service, apiKeyAccount)
		if err != nil {
			return Credentials{}, fmt.Errorf("read api key from secret store: %w", err)
		}
		c.APIKey = v
	}
	if c.APIKey == "" {
		return Credentials{}, fmt.Errorf("no obx_ API key available (env %s or secret store)", EnvAPIKeyDirect)
	}

	// Ed25519 seed: direct override, else secret source.
	if v := os.Getenv(EnvSeedDirect); v != "" {
		c.SeedB64 = v
	} else if service != "" && privKeyAccount != "" {
		v, err := lookup(service, privKeyAccount)
		if err != nil {
			return Credentials{}, fmt.Errorf("read signing seed from secret store: %w", err)
		}
		c.SeedB64 = v
	}
	if c.SeedB64 == "" {
		return Credentials{}, fmt.Errorf("no Ed25519 seed available (env %s or secret store)", EnvSeedDirect)
	}

	return c, nil
}

// FileSecretLookup reads one secret by (service, account) from the CLI's opt-in
// file backend — the 0600 nested-JSON format the CLI writes
// (cli/internal/secret/file.go): {"<service>":{"<account>":"<value>"}}.
func FileSecretLookup(path, service, account string) (string, error) {
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

// OSSecretLookup reads one secret by (service, account) from the platform
// secret store, matching what `openbox init` wrote: libsecret
// (secret-tool) on Linux, the login keychain (security) on macOS. The
// value returns on the child's stdout; it is never logged.
func OSSecretLookup(service, account string) (string, error) {
	// Reject leading-dash coordinates so a crafted config/env value can't
	// be reparsed as a flag by the backend CLI. argv (not a shell) is
	// used, so there is no shell-injection surface; this closes
	// arg-injection.
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

// resolveBool applies the shared precedence for boolean posture flags: the
// config field (def when absent/nil or the config is unreadable), then the env
// override (env wins either way, so env can disable what config enabled).
// resolveBool resolves one boolean posture field through the full precedence
// chain. The managed layer (E8-S9) can outrank both the user config and the
// environment for fields an org locks; see managed.go for why that inversion is
// necessary rather than merely convenient.
//
// fieldName is the JSON name the managed file's "locked" list refers to.
func resolveBool(fieldName string, field func(DevConfig) *bool, def bool, envKey string) bool {
	v, _ := resolveBoolWithSource(fieldName, field, def, envKey)
	return v
}

// unmarshalStrict decodes config JSON, rejecting unknown fields so a typo in a
// managed file surfaces as unreadable rather than as a silently-ignored mandate —
// `{"enfoce": true}` must not look like a file that simply governs nothing.
//
// Keys beginning with "//" are allowed through as documentation. JSON has no
// comments, and an ops file that mandates security settings needs to explain
// itself where the operator will actually read it; this is the same convention
// Claude Code's own managed-settings.json uses. Only that prefix is exempt, so a
// misspelled real field is still caught.
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
