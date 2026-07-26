package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	providerspi "github.com/openbox-ai/openbox-shift-left/provider"
)

// CredentialRef is the SL-2 install-time seam's credential coordinate type
// (shared provider SPI — INV-1: it carries coordinates + the non-secret DID
// only, never the obx_ key or Ed25519 seed value).
type CredentialRef = providerspi.CredentialRef

// ownedInvocation identifies a hooks.json command handler as OpenBox-owned by
// matching the EXACT command shape this installer generates: one engine token
// (optionally double-quoted) followed by `hook codex <event>` — and nothing
// else. This is the idempotency key (the Codex HooksFile schema is
// deny_unknown_fields at the top level, so a custom marker field is not an
// option): re-install replaces exactly these entries in place and NEVER
// touches a foreign handler. The match is ANCHORED (G_SEC SL7-A F1): a user's
// compound/wrapper handler that merely EMBEDS an openbox invocation (e.g.
// `my-audit-log && "/usr/bin/openbox" hook codex PreToolUse`) is foreign — its
// added functionality must survive re-install — so a substring test is not
// enough; isOpenBoxHandler parses the command instead of scanning it.
// `hook claude-code <event>` is deliberately also owned: Codex's external-agent
// migration (addendum #12) can import the OpenBox Claude Code hooks in mangled
// form — those are OUR entries pointed at the wrong provider parser, so they
// are superseded, not duplicated alongside.
var ownedInvocation = regexp.MustCompile(`^hook (?:codex|claude-code) [A-Za-z]+$`)

// Hook timeouts in SECONDS (the Codex `timeout` unit — addendum #8; contrast
// CC's 5 s hard kill, Codex's default is 600 s). The four hot hooks get a small
// explicit bound so a wedged hook can never stall a tool call for long; the
// SessionEnd flush hook gets more headroom (the engine's own flushBudget of
// 12 s stays under it).
const (
	hotHookTimeoutSec        = 5
	sessionEndHookTimeoutSec = 15
)

// hooksDescription is the top-level marker comment written into a fresh
// hooks.json (the HooksFile schema's only free-text field).
const hooksDescription = "OpenBox observe hooks (STORY-SL7-A) — managed by `openbox dev init --provider codex`; re-running updates the openbox entries in place and never touches foreign hooks."

// Installer writes the OpenBox hook entries into Codex's hooks.json and the
// non-secret dev config, delegated from `openbox dev init` (the SL-2 provider
// seam). It implements provider.Installer and replaces the SL-2 stub for codex.
// Zero-value fields default to the standard install locations; tests set them
// to temp paths.
//
// Unlike the Claude Code installer there is no plugin bundle and no engine
// copy: Codex hooks invoke the engine by ABSOLUTE PATH (EngineBinary — the CC
// EngineBinary precedent, resolved from os.Executable() by the CLI registry),
// so `dev init` is the whole install. The Codex plugin channel is a recorded
// future distribution option (OD-SL7-DIST).
type Installer struct {
	HooksPath    string // where hooks.json lives (default: defaultHooksPath())
	ConfigPath   string // where the dev config is written (default: DefaultConfigPath())
	EngineBinary string // absolute engine path baked into hook commands; "" ⇒ "openbox" on PATH
}

// Name is the provider this installer serves.
func (Installer) Name() providerspi.Name { return providerspi.Codex }

// Available reports that the Codex adapter is built (not the SL-2 stub).
func (Installer) Available() bool { return true }

// Plan describes what Install would write, without writing anything (--dry-run
// and the onboarding summary). It never prints a secret value (INV-1).
func (i Installer) Plan(ref CredentialRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OpenBox Codex hooks (observe-only, STORY-SL7-A; requires codex-cli >= 0.145.0 — hooks are stable and ON by default):\n")
	fmt.Fprintf(&b, "  - Write OpenBox hook entries → %s (merged in place, idempotent — foreign entries untouched)\n", i.hooksPath())
	fmt.Fprintf(&b, "      SessionStart, UserPromptSubmit, SessionEnd (matcher omitted)   timeout %ds/%ds\n", hotHookTimeoutSec, sessionEndHookTimeoutSec)
	fmt.Fprintf(&b, "      PreToolUse, PostToolUse (matcher \"*\" — Bash, apply_patch, mcp__*) timeout %ds\n", hotHookTimeoutSec)
	fmt.Fprintf(&b, "      each: { \"type\": \"command\", \"command\": %q }\n", i.hookCommand("<Event>"))
	fmt.Fprintf(&b, "  - Write dev config (non-secret coordinates) → %s\n", i.configPath())
	fmt.Fprintf(&b, "      developer_did=%s\n", ref.DID)
	fmt.Fprintf(&b, "      secret_service=%q api_key_account=%q private_key_account=%q\n",
		ref.SecretService, ref.APIKeyAccount, ref.PrivateKeyAccount)
	fmt.Fprintf(&b, "      content_capture=%s (default ON as of 2026-07-15; set false to restore metadata-only)\n", contentCaptureLabel(ref.ContentCapture))
	fmt.Fprintf(&b, "  - Credentials stay in the OS secret store; the hook reads them at runtime (INV-1) —\n")
	fmt.Fprintf(&b, "    hooks.json carries the engine path + event names ONLY (no key, DID, or URL).\n")
	fmt.Fprintf(&b, "\nTrust step (Codex hash-trusts non-managed hooks):\n")
	fmt.Fprintf(&b, "  After install, run /hooks inside Codex to review and TRUST the new OpenBox hooks —\n")
	fmt.Fprintf(&b, "  until trusted they do not run. (`--dangerously-bypass-hook-trust` and `--disable hooks`\n")
	fmt.Fprintf(&b, "  remain user-side bypass vectors; requirements.toml-managed hooks are the future\n")
	fmt.Fprintf(&b, "  non-disablable option — OD-SL7-DIST.)\n")
	fmt.Fprintf(&b, "\nCommit attribution: a Codex-run `git commit` is stamped `OpenBox-Session:` from the\n")
	fmt.Fprintf(&b, "CODEX_THREAD_ID env Codex injects into every exec (no liveness registry). Enable the\n")
	fmt.Fprintf(&b, "ambient prepare-commit-msg hook install with `openbox dev init --install-git-hook`,\n")
	fmt.Fprintf(&b, "or per repo with `openbox hook git install`.\n")
	return b.String()
}

// Install merges the OpenBox hook entries into hooks.json and writes the dev
// config. Idempotent: re-running updates the OpenBox-owned entries in place
// (recognized by ownershipMarkers), never duplicates them, and never modifies
// or removes a foreign/imported entry. It does NOT write managed/requirements
// config (enterprise mandate is a separate story).
func (i Installer) Install(ref CredentialRef) error {
	if ref.DID == "" {
		return fmt.Errorf("codex install: CredentialRef.DID is required")
	}
	if err := i.writeHooks(); err != nil {
		return err
	}
	return i.writeConfig(ref)
}

// hooksFile mirrors codex-rs HooksFile (config/src/hook_config.rs @
// rust-v0.145.0): top-level `description` + `hooks` keyed by the PascalCase
// event name; deny_unknown_fields, so nothing else may be added. Foreign event
// arrays are preserved as raw bytes.
type hooksFile struct {
	Description string                     `json:"description,omitempty"`
	Hooks       map[string]json.RawMessage `json:"hooks"`
}

// matcherGroup mirrors codex-rs MatcherGroup. Foreign handlers inside a group
// are carried as raw bytes so their unknown fields survive the round-trip.
type matcherGroup struct {
	Matcher *string           `json:"matcher,omitempty"`
	Hooks   []json.RawMessage `json:"hooks"`
}

// commandHandler is the OpenBox handler shape (codex-rs HookHandlerConfig
// `command` variant; timeout is in SECONDS).
type commandHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// writeHooks performs the idempotent merge. A pre-existing file that is not
// valid JSON is a hard error — never clobber a file we cannot understand.
func (i Installer) writeHooks() error {
	path := i.hooksPath()

	hf := hooksFile{Hooks: map[string]json.RawMessage{}}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &hf); err != nil {
			return fmt.Errorf("codex install: refusing to modify unparsable %s: %w", path, err)
		}
		if hf.Hooks == nil {
			hf.Hooks = map[string]json.RawMessage{}
		}
	case os.IsNotExist(err):
		// fresh file
	default:
		return fmt.Errorf("codex install: read %s: %w", path, err)
	}
	if hf.Description == "" {
		hf.Description = hooksDescription
	}

	for _, ev := range []HookName{HookSessionStart, HookUserPromptSubmit, HookPreToolUse, HookPostToolUse, HookSessionEnd} {
		merged, err := i.mergeEvent(hf.Hooks[string(ev)], ev)
		if err != nil {
			return fmt.Errorf("codex install: hooks.json event %s: %w", ev, err)
		}
		hf.Hooks[string(ev)] = merged
	}

	out, err := json.MarshalIndent(hf, "", "  ")
	if err != nil {
		return err
	}
	// 0700 (G_SEC SL7-A F2): when `dev init` runs before Codex ever has, this
	// creates ~/.codex itself — the directory Codex later drops auth.json
	// (OpenAI credentials) into — so it gets the same owner-only posture as the
	// config dir, spool, and stash.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codex install: hooks dir: %w", err)
	}
	// Atomic write (temp in the same dir + rename): Codex re-reading mid-install
	// sees either the old file or the whole new one, never a truncated merge.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hooks-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("codex install: commit hooks.json: %w", err)
	}
	return nil
}

// mergeEvent strips every OpenBox-owned handler from the event's existing
// matcher groups (dropping groups that become empty), preserves everything
// foreign byte-for-byte, and appends the fresh OpenBox group last.
func (i Installer) mergeEvent(existing json.RawMessage, ev HookName) (json.RawMessage, error) {
	var groups []matcherGroup
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &groups); err != nil {
			return nil, fmt.Errorf("parse matcher groups: %w", err)
		}
	}

	kept := groups[:0]
	for _, g := range groups {
		var foreign []json.RawMessage
		for _, h := range g.Hooks {
			if isOpenBoxHandler(h) {
				continue // ours (possibly stale/mangled-import) — superseded below
			}
			foreign = append(foreign, h)
		}
		if len(foreign) == 0 {
			continue // the group only carried our handlers — drop it
		}
		g.Hooks = foreign
		kept = append(kept, g)
	}

	ours, err := json.Marshal(commandHandler{
		Type:    "command",
		Command: i.hookCommand(string(ev)),
		Timeout: timeoutFor(ev),
	})
	if err != nil {
		return nil, err
	}
	group := matcherGroup{Hooks: []json.RawMessage{ours}}
	if ev == HookPreToolUse || ev == HookPostToolUse {
		star := "*" // match-all over tool_name (Codex treats ""/"*" as match-all)
		group.Matcher = &star
	}
	kept = append(kept, group)

	return json.Marshal(kept)
}

// isOpenBoxHandler reports whether a raw handler entry is OpenBox-owned: a
// `command` handler whose ENTIRE command is one engine token followed by the
// anchored `hook codex|claude-code <event>` invocation (ownedInvocation) —
// i.e. exactly the shape hookCommand generates, regardless of which engine
// path a previous install baked in. Anything else — including a compound
// command that embeds such an invocation — is foreign (kept). Unparsable
// entries are foreign (kept).
func isOpenBoxHandler(raw json.RawMessage) bool {
	var h struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	if json.Unmarshal(raw, &h) != nil || h.Type != "command" {
		return false
	}
	rest, ok := stripEngineToken(strings.TrimSpace(h.Command))
	return ok && ownedInvocation.MatchString(rest)
}

// stripEngineToken removes the leading engine token from a command line — a
// double-quoted path (the shape we generate for a resolved engine) or a single
// unquoted word (the `openbox`-on-PATH fallback; also matches an env-prefixed
// path like $CLAUDE_PLUGIN_ROOT/bin/openbox from a migrated import) — and
// returns the space-trimmed remainder. ok=false when the command has no
// well-formed leading token (e.g. an unterminated quote).
func stripEngineToken(cmd string) (rest string, ok bool) {
	if cmd == "" {
		return "", false
	}
	if cmd[0] == '"' {
		end := strings.IndexByte(cmd[1:], '"')
		if end < 0 {
			return "", false // unterminated quote — not our shape
		}
		return strings.TrimSpace(cmd[end+2:]), true
	}
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		return strings.TrimSpace(cmd[i+1:]), true
	}
	return "", true // a bare single token carries no hook invocation
}

// hookCommand renders the shell command line for one event: the (quoted)
// absolute engine path + `hook codex <event>`. Codex runs command handlers
// through the user's shell, so the path is double-quoted against spaces —
// the same convention as the CC plugin's hooks.json. INV-1: the command
// carries the engine path + event name ONLY.
func (i Installer) hookCommand(event string) string {
	engine := i.EngineBinary
	if engine == "" {
		engine = "openbox" // packaging fallback: resolve on PATH
		return engine + " hook codex " + event
	}
	return `"` + engine + `" hook codex ` + event
}

func timeoutFor(ev HookName) int {
	if ev == HookSessionEnd {
		return sessionEndHookTimeoutSec
	}
	return hotHookTimeoutSec
}

// writeConfig writes the shared non-secret dev config (same dev.json contract
// as every provider, incl. the ADR-0006 enforce/tier2/findings posture from
// ref — so the SL7-B enforce leg needs no new install surface). Ported from
// the CC installer, including the preserve-prior-sync-coordinates behavior.
func (i Installer) writeConfig(ref CredentialRef) error {
	cfg := DevConfig{
		BaseURL:           ref.BaseURL,
		DID:               ref.DID,
		SecretService:     ref.SecretService,
		APIKeyAccount:     ref.APIKeyAccount,
		PrivateKeyAccount: ref.PrivateKeyAccount,
		ContentCapture:    ref.ContentCapture,
		InstallGitHook:    ref.InstallGitHook,
		AgentID:           ref.AgentID,    // E6-S8: for `dev sync` / staleness
		BackendURL:        ref.BackendURL, // control-plane base for the policy read
		// ADR-0006: persist the enforce posture so the runtime hook reads it from
		// dev.json (no OPENBOX_ENFORCE / OPENBOX_TIER2 / OPENBOX_FINDINGS needed).
		Enforce:  ref.Enforce,
		Tier2:    ref.Tier2,
		Findings: ref.Findings,
	}
	// Preserve previously-persisted sync coordinates on a re-init that does not
	// carry them (the idempotent "already initialized, reusing creds" path).
	if cfg.AgentID == "" || cfg.BackendURL == "" {
		if prior, err := devconfig.Load(i.configPath()); err == nil {
			if cfg.AgentID == "" {
				cfg.AgentID = prior.AgentID
			}
			if cfg.BackendURL == "" {
				cfg.BackendURL = prior.BackendURL
			}
		}
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := i.configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codex install: config dir: %w", err)
	}
	// 0600: coordinates are not secret, but keep them owner-only anyway.
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("codex install: write config: %w", err)
	}
	return nil
}

func (i Installer) hooksPath() string {
	if i.HooksPath != "" {
		return i.HooksPath
	}
	return defaultHooksPath()
}

func (i Installer) configPath() string {
	if i.ConfigPath != "" {
		return i.ConfigPath
	}
	return DefaultConfigPath()
}

// defaultHooksPath is Codex's user-level hooks file: $CODEX_HOME/hooks.json
// when CODEX_HOME is set (Codex's own home override), else ~/.codex/hooks.json.
// (Repo-level .codex/hooks.json and config.toml [hooks] are alternative
// locations this installer deliberately does not touch.)
func defaultHooksPath() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "hooks.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".codex", "hooks.json")
}

// contentCaptureLabel renders the *bool org content posture for the install
// preview: nil ⇒ the adapter default (ON as of 2026-07-15), else the pinned value.
func contentCaptureLabel(b *bool) string {
	switch {
	case b == nil:
		return "on (default)"
	case *b:
		return "on"
	default:
		return "off"
	}
}
