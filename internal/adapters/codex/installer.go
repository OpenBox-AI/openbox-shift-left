package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// CredentialRef is the install-time seam's credential coordinate type (shared
// provider SPI; INV-1: it carries coordinates + the non-secret DID only, never
// the obx_ key or Ed25519 seed value).
type CredentialRef = providerspi.CredentialRef

// ownedInvocation identifies a hooks.json command handler as OpenBox-owned by
// matching the exact command shape this installer generates: one engine token
// (optionally double-quoted) followed by `hook codex <event>`; and nothing
// else.
var ownedInvocation = regexp.MustCompile(`^hook (?:codex|claude-code) [A-Za-z]+$`)

// Hook timeouts in seconds (the Codex `timeout` unit; addendum #8; contrast
// CC's 5 s hard kill, Codex's default is 600 s).
const (
	hotHookTimeoutSec        = 5
	preToolUseHookTimeoutSec = 30
	sessionEndHookTimeoutSec = 15
)

const hooksDescription = "OpenBox observe hooks (STORY-SL7-A) — managed by `openbox init --provider codex`; re-running updates the openbox entries in place and never touches foreign hooks."

// Installer writes the OpenBox hook entries into Codex's hooks.json and the
// non-secret dev config, delegated from `openbox init` (the provider seam).
type Installer struct {
	HooksPath    string // where hooks.json lives (default: defaultHooksPath())
	ConfigPath   string // where the dev config is written (default: DefaultConfigPath())
	EngineBinary string // absolute engine path baked into hook commands; "" ⇒ "openbox" on PATH
}

// Name is the provider this installer serves.
func (Installer) Name() providerspi.Name { return providerspi.Codex }

// Available reports that the Codex adapter is built (not the stub).
func (Installer) Available() bool { return true }

// Plan describes what Install would write, without writing anything (--dry-run
// and the onboarding summary). It never prints a secret value (INV-1).
func (i Installer) Plan(ref CredentialRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OpenBox Codex hooks (observe-only, STORY-SL7-A; requires codex-cli >= 0.145.0 — hooks are stable and ON by default):\n")
	fmt.Fprintf(&b, "  - Write OpenBox hook entries → %s (merged in place, idempotent — foreign entries untouched)\n", i.hooksPath())
	fmt.Fprintf(&b, "      SessionStart, UserPromptSubmit, SessionEnd (matcher omitted)   timeout %ds/%ds\n", hotHookTimeoutSec, sessionEndHookTimeoutSec)
	fmt.Fprintf(&b, "      PostToolUse (matcher \"*\" — Bash, apply_patch, mcp__*)          timeout %ds\n", hotHookTimeoutSec)
	fmt.Fprintf(&b, "      PreToolUse  (matcher \"*\"; the gating hook, may hold for approval) timeout %ds\n", preToolUseHookTimeoutSec)
	fmt.Fprintf(&b, "      each: { \"type\": \"command\", \"command\": %q }\n", i.hookCommand("<Event>"))
	fmt.Fprintf(&b, "  - Write dev config (non-secret coordinates) → %s\n", i.configPath())
	fmt.Fprintf(&b, "      developer_did=%s\n", ref.DID)
	fmt.Fprintf(&b, "      base_url=%s\n", devconfig.BaseURLLabel(ref.BaseURL))
	fmt.Fprintf(&b, "      content_capture=%s (default ON as of 2026-07-15; set false to restore metadata-only)\n", contentCaptureLabel(ref.ContentCapture))
	fmt.Fprintf(&b, "  - Credentials are NOT touched here: `openbox auth` wrote them to ~/.openbox/.env and\n")
	fmt.Fprintf(&b, " the hook reads them at runtime — hooks.json carries the engine path +\n")
	fmt.Fprintf(&b, "    event names ONLY (no key, DID, or URL).\n")
	fmt.Fprintf(&b, "\nTrust step (Codex hash-trusts non-managed hooks):\n")
	fmt.Fprintf(&b, "  After install, run /hooks inside Codex to review and TRUST the new OpenBox hooks —\n")
	fmt.Fprintf(&b, "  until trusted they do not run. (`--dangerously-bypass-hook-trust` and `--disable hooks`\n")
	fmt.Fprintf(&b, "  remain user-side bypass vectors; requirements.toml-managed hooks are the future\n")
	fmt.Fprintf(&b, "  non-disablable option — OD-SL7-DIST.)\n")
	fmt.Fprintf(&b, "\nCommit attribution: a Codex-run `git commit` is stamped `OpenBox-Session:` from the\n")
	fmt.Fprintf(&b, "CODEX_THREAD_ID env Codex injects into every exec (no liveness registry). Enable the\n")
	fmt.Fprintf(&b, "ambient prepare-commit-msg hook install with `openbox init --install-git-hook`,\n")
	fmt.Fprintf(&b, "or per repo with `openbox hook git install`.\n")
	return b.String()
}

// Install merges the OpenBox hook entries into hooks.json and writes the dev
// config. Idempotent: re-running updates the OpenBox-owned entries in place
// (recognized by ownershipMarkers), never duplicates them, and never modifies
// or removes a foreign/imported entry.
func (i Installer) Install(ref CredentialRef) error {
	if ref.DID == "" {
		return fmt.Errorf("codex install: CredentialRef.DID is required")
	}
	if err := i.writeHooks(); err != nil {
		return err
	}
	return i.writeConfig(ref)
}

type hooksFile struct {
	Description string                     `json:"description,omitempty"`
	Hooks       map[string]json.RawMessage `json:"hooks"`
}

type matcherGroup struct {
	Matcher *string           `json:"matcher,omitempty"`
	Hooks   []json.RawMessage `json:"hooks"`
}

type commandHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// writeHooks performs the idempotent merge. A pre-existing file that is not
// valid JSON is a hard error; never clobber a file we cannot understand.
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codex install: hooks dir: %w", err)
	}
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

func (i Installer) hookCommand(event string) string {
	engine := i.EngineBinary
	if engine == "" {
		engine = "openbox" // packaging fallback: resolve on PATH
		return engine + " hook codex " + event
	}
	return `"` + engine + `" hook codex ` + event
}

func timeoutFor(ev HookName) int {
	switch ev {
	case HookSessionEnd:
		return sessionEndHookTimeoutSec
	case HookPreToolUse:
		return preToolUseHookTimeoutSec
	}
	return hotHookTimeoutSec
}

func (i Installer) writeConfig(ref CredentialRef) error {
	if err := devconfig.WriteConfig(i.configPath(), providerspi.ConfigUpdate(ref)); err != nil {
		return fmt.Errorf("codex install: %w", err)
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
	if p, err := devconfig.DevConfigWritePath(); err == nil {
		return p
	}
	return DefaultConfigPath()
}

// defaultHooksPath is Codex's user-level hooks file: $CODEX_HOME/hooks.json
// when CODEX_HOME is set (Codex's own home override), else
// ~/.codex/hooks.json.
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
