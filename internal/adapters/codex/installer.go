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
// stall a tool call for long.
//
// SessionEnd is not ours to choose: Codex clamps it to 3 s and logs that it did
// ("clamping SessionEnd hook timeout to 3s in <hooks.json>", measured on
// codex-cli 0.150.0-alpha.8). Registering anything larger
// was fiction -- the value in the file was 15, the enforced ceiling was 3, and
// the engine's own drain budget sat above both. Ask for exactly what we get.
//
// Losing the old headroom costs no events. Egress cadence does not depend on
// this hook: every non-SessionEnd hook already fires a RealtimeTrigger (2 s
// debounce), and whatever the SessionEnd flush cannot drain stays spooled for
// the next hook's FlushOrSweep carry-over sweep.
const (
	hotHookTimeoutSec        = 5
	preToolUseHookTimeoutSec = 30
	sessionEndHookTimeoutSec = 3
)

const hooksDescription = "OpenBox observe hooks; managed by `openbox init --provider codex`; re-running updates the openbox entries in place and never touches foreign hooks."

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

// Install merges the OpenBox hook entries into hooks.json and writes the dev
// config. Idempotent: re-running updates the OpenBox-owned entries in place
// (recognized by ownershipMarkers), never duplicates them, and never modifies
// or removes a foreign/imported entry.
func (i Installer) Install(ref CredentialRef) error {
	if ref.AgentID == "" {
		return fmt.Errorf("codex install: CredentialRef.AgentID is required")
	}
	if err := i.writeHooks(); err != nil {
		return err
	}
	return i.writeConfig(ref)
}

// hookedEvents are the Codex events this installer maintains entries for.
// Every other key in the file, at any depth, is somebody else's.
//
// Adding one here is three lockstep edits -- this slice, a HookName const, and a
// RunHook branch -- and uninstall follows for free, because hookremove.go
// iterates this same slice. Adding an event anywhere else leaves an orphan hook
// behind after `openbox uninstall`.
var hookedEvents = []HookName{
	HookSessionStart, HookUserPromptSubmit, HookPreToolUse, HookPermissionRequest,
	HookPostToolUse, HookStop, HookSubagentStart, HookSubagentStop,
	HookPreCompact, HookPostCompact, HookSessionEnd,
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codex install: hooks dir: %w", err)
	}
	return writeHooksFile(path, out, "codex install: commit hooks.json")
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
	_, rest, ok := stripEngineToken(strings.TrimSpace(h.Command))
	return ok && ownedInvocation.MatchString(rest)
}

// stripEngineToken splits `"<engine>" hook codex X` into its two halves, so
// the shape is parsed once: handlerEngine wants the engine, isOpenBoxHandler
// wants the rest.
func stripEngineToken(cmd string) (engine, rest string, ok bool) {
	if cmd == "" {
		return "", "", false
	}
	if cmd[0] == '"' {
		end := strings.IndexByte(cmd[1:], '"')
		if end < 0 {
			return "", "", false // unterminated quote; not our shape
		}
		return cmd[1 : end+1], strings.TrimSpace(cmd[end+2:]), true
	}
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		return cmd[:i], strings.TrimSpace(cmd[i+1:]), true
	}
	return cmd, "", true // a bare single token carries no hook invocation
}

func (i Installer) hookCommand(event string) string {
	engine := i.EngineBinary
	if engine == "" {
		engine = "openbox" // packaging fallback: resolve on PATH
		return engine + " hook codex " + event
	}
	return `"` + engine + `" hook codex ` + event
}

// gatedHooks is the one source of truth for which Codex hook classes run the
// enforcement gate (RunHook's own `gated` check, hookrun.go) and therefore
// need the raised ceiling at install time (timeoutFor, below): PreToolUse
// and PermissionRequest can each hold for a real approval decision (a
// tighter bound would time out mid-decision and fail the call open, which
// is the one outcome this surface must never produce), and UserPromptSubmit
// budgets its own evaluation off this SAME ceiling (evaluator.Ceiling,
// shared across every gated class) -- an install still at the old 5s bound
// would kill the hook mid-evaluation (or mid gate-drain) while the
// evaluator itself still believes it has up to 30s. Defining the gated set
// independently in two places let them drift; this is read by both.
var gatedHooks = map[HookName]bool{
	HookPreToolUse:        true,
	HookPermissionRequest: true,
	HookUserPromptSubmit:  true,
}

// Gated reports whether hook runs the enforcement gate.
func (h HookName) Gated() bool { return gatedHooks[h] }

func timeoutFor(ev HookName) int {
	switch {
	case ev == HookSessionEnd:
		return sessionEndHookTimeoutSec
	case ev.Gated():
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
