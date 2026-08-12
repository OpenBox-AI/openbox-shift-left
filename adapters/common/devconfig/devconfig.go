// Package devconfig is the provider-neutral developer-runtime
// configuration and credential resolution shared by every tool adapter
// (Claude Code, Codex, Cursor). It owns two files (module home recorded in
// ADR-0007, layout in ADR-0015):
//
//	~/.openbox/dev.json   posture + the non-secret coordinates
//	~/.openbox/.env       the credentials, in plaintext, 0600
//
// One store per field: the credential file is never read for a coordinate and
// dev.json never holds a secret. Before ADR-0015 the DID lived in both dev.json
// and the OS keychain, and a stale keychain entry silently reverted a corrected
// DID on the next install — this split is what makes that impossible rather than
// merely fixed.
//
// It was extracted, behavior-preserving, from
// adapters/claude-code/creds.go — that adapter keeps thin aliases so its
// public API and tests are unchanged. Like adapters/common/git, this
// module is dependency-free: it never imports the client, an adapter, or
// the CLI.
//
// INV-1 is narrowed by ADR-0015 and still load-bearing: dev.json carries only
// non-secret coordinates, and the credential values are read at flush time and
// never logged, printed, or placed on an argv. What INV-1 no longer claims is
// that a secret is absent from a plaintext file — that is now the only place one
// lives, and ADR-0015 records what it costs.
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

// Env vars: non-secret coordinates + optional direct overrides for CI/tests.
// The names are the cross-adapter contract (the same dev.json serves every
// provider), so they are exported here and aliased by the adapters.
const (
	EnvBaseURL         = "OPENBOX_BASE_URL"
	EnvDID             = "OPENBOX_AGENT_DID"
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
	// EnvAgentPrivateKey is the Ed25519 signing key, under the name the OpenBox
	// platform documents for its own SDK. It replaced OPENBOX_ED25519_SEED,
	// which no published doc ever mentioned — so a developer following the docs
	// set a variable this repo ignored (ADR-0015). Both old names still read;
	// see deprecatedPrivateKeyEnvNames.
	EnvAgentPrivateKey = "OPENBOX_AGENT_PRIVATE_KEY"
	EnvConfigPath      = "OPENBOX_CONFIG"
	// Policy-bundle signing key pins (E8-S6). Non-secret; env overrides config.
	EnvOrgSigningPubKey = "OPENBOX_ORG_SIGNING_PUBKEY"
	EnvOrgSigningKeyID  = "OPENBOX_ORG_SIGNING_KEY_ID"
	EnvAgentID          = "OPENBOX_AGENT_ID"
	EnvBackendURL       = "OPENBOX_BACKEND_URL"
	EnvControlToken     = "OPENBOX_CONTROL_TOKEN"
	EnvSpoolDir         = "OPENBOX_SPOOL_DIR"

	// DefaultBaseURL is the core data-plane base used when nothing configures one.
	DefaultBaseURL = "https://core.openbox.ai"
	// DefaultBackendURL is the control-plane base used when nothing configures
	// one. Until ADR-0015 there was no default at all and `init` simply errored,
	// which made the hosted service the one deployment you had to configure by
	// hand. Self-hosted installs must still set BOTH URLs: the control plane
	// cannot tell the CLI where the operator's core lives, so accepting one
	// default and overriding the other points events at the hosted core and
	// surfaces much later as a 401.
	DefaultBackendURL = "https://api.openbox.ai"
)

// deprecatedPrivateKeyEnvNames are the pre-ADR-0015 names for the signing key,
// honoured for reads (never written) so an existing CI job keeps working.
//
// OPENBOX_ED25519_SEED was this repo's name; OPENBOX_SEED was the git action's.
// Reading both costs two map lookups and a warning; breaking them would strand
// pipelines this repo cannot see. Removing them needs an ADR amendment.
var deprecatedPrivateKeyEnvNames = []string{"OPENBOX_ED25519_SEED", "OPENBOX_SEED"}

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
	BaseURL string `json:"base_url,omitempty"`
	DID     string `json:"developer_did,omitempty"`
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
	// Enforce flips the developer runtime from observe/advisory to enforce.
	// Absent means the default, which is now ON (ADR-0016, reversing the
	// observe-by-default posture ADR-0006 shipped with); `enforce:false` or
	// OPENBOX_ENFORCE=0 opts out.
	//
	// It is a *bool, and that is load-bearing rather than stylistic. As a plain
	// `bool` with `omitempty`, an explicit false marshalled to NOTHING — so
	// writing the opt-out erased it from the file and the next read saw an
	// absent field and re-defaulted to on. The opt-out was silently
	// un-appliable. (The other half of the same trap, an accessor that cannot
	// tell absent from false, is already handled upstream by the key-presence
	// map in resolveBoolWithSource — but that only covers reads, not writes.)
	// TestEnforceOptOutRoundTrips is the assertion that keeps this honest.
	Enforce *bool `json:"enforce,omitempty"`
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

// DefaultConfigPath is where the hook looks for the dev config when
// OPENBOX_CONFIG is unset: ~/.openbox/dev.json (ADR-0015), with a read-side
// fallback to the pre-ADR-0015 location while an unmigrated file lives there.
//
// It keeps returning a bare string rather than (string, error) because it is
// called from every hook read path, where there is nothing useful to do with an
// error but carry on and let the missing-config path fail open (INV-3).
// DevConfigPath is the same resolution with the error surfaced, for callers that
// can act on it.
func DefaultConfigPath() string {
	p, err := DevConfigPath()
	if err != nil {
		// Unresolvable home: name the legacy location rather than an empty
		// path, so a read still finds a config written by an older binary.
		return filepath.Join(legacyConfigDir(), "dev.json")
	}
	return p
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
// the secret VALUES (read from the environment or ~/.openbox/.env) — it exists
// only in process memory on the flush path and must never be logged or
// persisted.
type Credentials struct {
	BaseURL               string
	APIKey                string
	DID                   string
	PrivateKeyB64         string
	ContentCaptureEnabled bool
}

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

// ResolveDIDOrEmpty resolves the developer DID and returns "" when nothing
// configures one, for callers where an absent DID is a legitimate state to
// report rather than an error to propagate — the install path, which may be
// about to write one.
//
// ResolveDID stays the hot-path form: a hook with no DID cannot attribute an
// event, so there it is an error.
func ResolveDIDOrEmpty() string {
	cfg, _ := load()
	return FirstNonEmpty(os.Getenv(EnvDID), cfg.DID)
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

// ResolveEnforce reports whether the developer runtime is in enforce mode:
// config field first, then the env override.
//
// DEFAULT ON (ADR-0016) — an absent field means enforce; `enforce:false` or
// OPENBOX_ENFORCE=0 opts out. Two properties keep that default safe: it is inert
// until the org publishes a policy (nothing to deny means nothing denied), and
// fail_closed stays off, so an OpenBox outage never blocks a tool call.
//
// A config read error resolves to the default, which now means an unreadable
// config enforces. That is the right direction: the alternative is that
// corrupting a file becomes a way to switch governance off.
func ResolveEnforce() bool {
	return resolveBool("enforce", func(c DevConfig) *bool { return c.Enforce }, true, EnvEnforce)
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

// ResolveCredentials assembles Credentials from the environment, the credential
// file, and the dev config. It returns an error (never a panic) when identity is
// incomplete; the caller logs it fail-open and exits 0 (INV-3). No secret value
// is ever included in a returned error.
//
// TWO source chains through one funnel, and conflating them is the trap
// (ADR-0015):
//
//	secrets      (api key, private key)  real env var > ~/.openbox/.env
//	coordinates  (DID, base URL)         real env var > dev.json > built-in default
//
// The credential file sits exactly where the deleted secret store sat and nowhere
// else. It is never consulted for a coordinate, so no field has two files that
// can disagree — which is what makes the pre-ADR-0015 two-DID-stores revert loop
// structurally impossible rather than merely fixed. A DID written into `.env` is
// ignored; TestEnvFileIsNotACoordinateSource pins that, and relaxing it reopens
// the bug class.
func ResolveCredentials() (Credentials, error) {
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

	secrets, envPath, err := loadSecretFile()
	if err != nil {
		return Credentials{}, err
	}

	c.APIKey = FirstNonEmpty(os.Getenv(EnvAPIKeyDirect), secrets[EnvAPIKeyDirect])
	if c.APIKey == "" {
		return Credentials{}, missingCredentialError("obx_ API key", EnvAPIKeyDirect, envPath)
	}

	// The private key is read under its platform-documented name first, then the
	// two deprecated aliases (see resolvePrivateKey).
	c.PrivateKeyB64 = resolvePrivateKey(secrets)
	if c.PrivateKeyB64 == "" {
		return Credentials{}, missingCredentialError("Ed25519 signing key", EnvAgentPrivateKey, envPath)
	}

	return c, nil
}

// loadSecretFile reads ~/.openbox/.env, returning the map and the path it tried.
//
// A missing file is an empty map and no error — "no credentials configured" is
// reported by the caller in its own words, naming what to run. An unresolvable
// home or an unparseable file IS an error: silently treating either as "no
// credentials" would send a user hunting for a registration problem they do not
// have.
func loadSecretFile() (map[string]string, string, error) {
	path, err := EnvFilePath()
	if err != nil {
		// No home to resolve. Callers still work if the real env vars are set,
		// so return an empty map rather than failing outright, and let the
		// missing-credential error name OPENBOX_HOME.
		return map[string]string{}, "", nil
	}
	kv, err := ParseEnvFile(path)
	if err != nil {
		return nil, path, err
	}
	return kv, path, nil
}

// resolvePrivateKey reads the signing key under its current name, then the two
// deprecated aliases, warning once per process for an alias.
//
// Three names existed for one value: OPENBOX_ED25519_SEED here,
// OPENBOX_SEED in the git action, and OPENBOX_AGENT_PRIVATE_KEY in the
// platform's own published SDK docs. A developer who followed those docs set a
// variable this repo ignored — a live defect independent of ADR-0015. The
// documented name wins; the other two keep working so nobody's CI breaks on
// upgrade. Retiring them needs its own decision, not a commit.
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

// warnDeprecatedPrivateKeyName warns once per process, on STDERR only.
//
// stderr is not a style choice: a hook writing to stdout injects context into the
// coding agent's conversation (INV-3). A deprecation notice must never become
// model input.
func warnDeprecatedPrivateKeyName(alias string) {
	deprecatedNameWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "openbox: %s is deprecated — use %s (same value, the name OpenBox documents)\n",
			alias, EnvAgentPrivateKey)
	})
}

var deprecatedNameWarnOnce sync.Once

// missingCredentialError names the file, the env var, and how to fix it —
// including, on macOS/Linux, how to read credentials back out of the OS keychain
// this release deleted support for.
//
// The keychain hint is here because this error is where a user upgrading an
// existing install actually meets ADR-0015's no-migration decision. Telling them
// only "run openbox auth" would send someone with working credentials off to
// register a second agent.
//
// It never echoes a value, and never names an account coordinate that no longer
// exists in this config.
func missingCredentialError(what, envName, envPath string) error {
	where := "~/.openbox/.env"
	if envPath != "" {
		where = envPath
	}
	msg := fmt.Sprintf("no %s available: set %s, or run `openbox auth` to write %s", what, envName, where)
	if envPath == "" {
		msg += fmt.Sprintf(" (no home directory could be resolved — set %s to an absolute path)", EnvHome)
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
