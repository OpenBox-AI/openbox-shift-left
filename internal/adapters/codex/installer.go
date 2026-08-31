package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CredentialRef is the install-time seam's credential coordinate type (shared
// provider SPI; INV-1: it carries coordinates + the non-secret DID only, never
// the obx_ key or Ed25519 seed value).
type CredentialRef = providerspi.CredentialRef

// `my-audit-log && "/usr/bin/openbox" hook codex PreToolUse`) is foreign; its
// added functionality must survive re-install; so a substring test is not
// enough; isOpenBoxHandler parses the command instead of scanning it.
var ownedInvocation = regexp.MustCompile(`^hook (?:codex|claude-code) [A-Za-z]+$`)

// The four hot hooks get a small explicit bound so a wedged hook can never
// stall a tool call for long; the SessionEnd flush hook gets more headroom
// (the engine's own flushBudget of 12 s stays under it).
const (
	hotHookTimeoutSec        = 5
	preToolUseHookTimeoutSec = 30
	sessionEndHookTimeoutSec = 15
)

const hooksDescription = "OpenBox observe hooks (STORY-SL7-A); managed by `openbox init --provider codex`; re-running updates the openbox entries in place and never touches foreign hooks."

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
	fmt.Fprintf(&b, "OpenBox Codex hooks (observe-only, STORY-SL7-A; requires codex-cli >= 0.145.0; hooks are stable and ON by default):\n")
	fmt.Fprintf(&b, "  - Write OpenBox hook entries → %s (merged in place, idempotent; foreign entries untouched)\n", i.hooksPath())
	fmt.Fprintf(&b, "      SessionStart, UserPromptSubmit, SessionEnd (matcher omitted)   timeout %ds/%ds\n", hotHookTimeoutSec, sessionEndHookTimeoutSec)
	fmt.Fprintf(&b, "      PostToolUse (matcher \"*\"; Bash, apply_patch, mcp__*)          timeout %ds\n", hotHookTimeoutSec)
	fmt.Fprintf(&b, "      PreToolUse  (matcher \"*\"; the gating hook, may hold for approval) timeout %ds\n", preToolUseHookTimeoutSec)
	fmt.Fprintf(&b, "      each: { \"type\": \"command\", \"command\": %q }\n", i.hookCommand("<Event>"))
	fmt.Fprintf(&b, "  - Write dev config (non-secret coordinates) → %s\n", i.configPath())
	fmt.Fprintf(&b, "      developer_did=%s\n", ref.DID)
	fmt.Fprintf(&b, "      base_url=%s\n", devconfig.BaseURLLabel(ref.BaseURL))
	fmt.Fprintf(&b, "      content_capture=%s (default ON as of 2026-07-15; set false to restore metadata-only)\n", contentCaptureLabel(ref.ContentCapture))
	fmt.Fprintf(&b, "  - Credentials are NOT touched here: `openbox auth` wrote them to ~/.openbox/.env and\n")
	fmt.Fprintf(&b, " the hook reads them at runtime; hooks.json carries the engine path +\n")
	fmt.Fprintf(&b, "    event names ONLY (no key, DID, or URL).\n")
	fmt.Fprintf(&b, "\nTrust step (Codex hash-trusts non-managed hooks):\n")
	fmt.Fprintf(&b, "  After install, run /hooks inside Codex to review and TRUST the new OpenBox hooks -\n")
	fmt.Fprintf(&b, "  until trusted they do not run. (`--dangerously-bypass-hook-trust` and `--disable hooks`\n")
	fmt.Fprintf(&b, "  remain user-side bypass vectors; requirements.toml-managed hooks are the future\n")
	fmt.Fprintf(&b, "  non-disablable option; OD-SL7-DIST.)\n")
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

// hookedEvents are the five Codex events this installer maintains entries for.
// Every other key in the file, at any depth, is somebody else's.
var hookedEvents = []HookName{
	HookSessionStart, HookUserPromptSubmit, HookPreToolUse, HookPostToolUse, HookSessionEnd,
}

// hooksEventPath addresses one event inside the hooks object. The names are
// closed Go constants and none carries gjson path syntax, which
// TestHookedEventNamesCarryNoPathSyntax holds rather than an escaper nothing
// would ever exercise.
func hooksEventPath(ev HookName) string { return "hooks." + string(ev) }

// canonicalJSONEqual compares two JSON values by content, not bytes: both go
// through the same key-sorting encoder, so formatting is not a difference.
func canonicalJSONEqual(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	ac, aErr := json.Marshal(av)
	bc, bErr := json.Marshal(bv)
	return aErr == nil && bErr == nil && bytes.Equal(ac, bc)
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

// writeHooks a pre-existing file that is not valid JSON is a hard error; never
// clobber a file we cannot understand.
func (i Installer) writeHooks() error {
	path := i.hooksPath()

	// Path edits, not a decode/re-encode round trip. This is Codex's file:
	// binding it to a struct dropped every top-level key the struct does not
	// name, and re-marshalling alphabetised the event order Codex wrote.
	before, err := os.ReadFile(path)
	switch {
	case err == nil:
		if !gjson.ValidBytes(before) {
			detail := json.Unmarshal(before, new(any))
			return fmt.Errorf("codex install: refusing to modify unparsable %s: %w", path, detail)
		}
	case os.IsNotExist(err):
		before = nil
	default:
		return fmt.Errorf("codex install: read %s: %w", path, err)
	}

	out := before
	if len(out) == 0 {
		out = []byte("{}")
	}
	if !gjson.GetBytes(out, "description").Exists() {
		if out, err = sjson.SetBytes(out, "description", hooksDescription); err != nil {
			return fmt.Errorf("codex install: hooks.json description: %w", err)
		}
	}

	for _, ev := range hookedEvents {
		existing := gjson.GetBytes(out, hooksEventPath(ev))
		merged, err := i.mergeEvent(json.RawMessage(existing.Raw), ev)
		if err != nil {
			return fmt.Errorf("codex install: hooks.json event %s: %w", ev, err)
		}
		// Skip the splice when the event already says what it should. Without
		// this, a re-run of `openbox init` reformats every event it owns and puts
		// a diff in Codex's file for no change at all.
		if canonicalJSONEqual([]byte(existing.Raw), merged) {
			continue
		}
		if out, err = sjson.SetRawBytes(out, hooksEventPath(ev), merged); err != nil {
			return fmt.Errorf("codex install: hooks.json event %s: %w", ev, err)
		}
	}
	if len(before) == 0 {
		// Nothing of Codex's to preserve in a file this install created, and sjson
		// splices compactly, so give it the indentation a person can read.
		var doc any
		if json.Unmarshal(out, &doc) == nil {
			if pretty, mErr := json.MarshalIndent(doc, "", "  "); mErr == nil {
				out = pretty
			}
		}
	}
	out = bytes.TrimRight(out, "\n")

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
				continue // ours (possibly stale/mangled-import); superseded below
			}
			foreign = append(foreign, h)
		}
		if len(foreign) == 0 {
			continue // the group only carried our handlers; drop it
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
			return "", false // unterminated quote; not our shape
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
