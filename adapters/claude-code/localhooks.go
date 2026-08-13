package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// localhooks.go — PROJECT hook scope (CredentialRef.ProjectDir).
//
// This is what `openbox init` does by DEFAULT (ADR-0016), because it is the only
// scope the CLI can complete by itself: Install merges the hook entries into
// <dir>/.claude/settings.local.json — the per-developer, git-ignored Claude Code
// settings layer — pointing at the plugin's engine binary, and they take effect
// on the next session in that directory. Sessions started in any other directory
// stay ungoverned.
//
// Global scope (empty ProjectDir) activates the plugin org-wide through managed
// settings, which is an administrator's deployment; Install prints the snippet
// and applies nothing. That asymmetry is why the default inverted: a default the
// command cannot finish reports success while governing nothing.
//
// The merge is additive and idempotent: existing settings keys and foreign
// hook entries are preserved; our entry is appended only if the exact command
// is not already present for that event.

// localHookEvents maps hook event → (matcher, timeoutSeconds, statusMessage).
// Mirrors the plugin bundle's hooks/hooks.json — including PreToolUse's raised
// ceiling, so a locally-scoped install gates and holds exactly as the plugin
// does rather than killing the hook mid-hold. TestLocalHooksMirrorPluginBundle
// pins the two lists together, so a hook added to one and not the other fails.
var localHookEvents = []struct {
	Event         string
	Matcher       string
	Timeout       int
	StatusMessage string
}{
	{Event: "SessionStart", Timeout: 5},
	{Event: "UserPromptSubmit", Timeout: 5},
	{Event: "PreToolUse", Matcher: "*", Timeout: preToolUseHookTimeoutSec, StatusMessage: "OpenBox governance…"},
	{Event: "PostToolUse", Matcher: "*", Timeout: 5},
	{Event: "PostToolUseFailure", Matcher: "*", Timeout: 5},
	{Event: "Stop", Timeout: 5},
	{Event: "SubagentStop", Timeout: 5},
	{Event: "SubagentStart", Timeout: 5},
	{Event: "PermissionDenied", Matcher: "*", Timeout: 5},
	{Event: "StopFailure", Timeout: 5},
	{Event: "SessionEnd", Timeout: 15},
}

// writeLocalHooks merges the plugin hook block into
// <projectDir>/.claude/settings.local.json. engine is the absolute path of
// the plugin's bin/openbox.
func writeLocalHooks(projectDir, engine string) error {
	dir, err := filepath.Abs(projectDir)
	if err != nil {
		return fmt.Errorf("local-hooks: resolve %q: %w", projectDir, err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("local-hooks: %q is not an existing directory", dir)
	}
	settingsPath := filepath.Join(dir, ".claude", "settings.local.json")

	settings := map[string]any{}
	if raw, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("local-hooks: %s exists but is not valid JSON — fix or remove it first: %w", settingsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("local-hooks: read %s: %w", settingsPath, err)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, ev := range localHookEvents {
		command := localHookCommand(engine, "hook claude-code "+ev.Event)
		entries, _ := hooks[ev.Event].([]any)
		if hasLocalHookCommand(entries, command) {
			continue
		}
		hook := map[string]any{"type": "command", "command": command, "timeout": ev.Timeout}
		if ev.StatusMessage != "" {
			hook["statusMessage"] = ev.StatusMessage
		}
		handlers := []any{hook}
		if ev.Event == "PreToolUse" {
			// The background approval watcher rides alongside the gate, exactly
			// as in the plugin bundle (see plugin/hooks/hooks.json).
			handlers = append(handlers, map[string]any{
				"type":        "command",
				"command":     localHookCommand(engine, "rewake claude-code"),
				"asyncRewake": true,
				"timeout":     rewakeHookTimeoutSec,
			})
		}
		entries = append(entries, map[string]any{
			"matcher": ev.Matcher,
			"hooks":   handlers,
		})
		hooks[ev.Event] = entries
	}
	settings["hooks"] = hooks

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return fmt.Errorf("local-hooks: mkdir %s: %w", filepath.Dir(settingsPath), err)
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("local-hooks: marshal settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("local-hooks: write %s: %w", settingsPath, err)
	}
	return nil
}

// hasLocalHookCommand reports whether any entry already carries this command
// (idempotent re-init; never duplicates).
//
// The comparison IGNORES QUOTING around the engine path, and that is not
// cosmetic. When the path started being quoted, an exact-string check stopped
// recognising the entries earlier versions wrote — so a re-init would have
// appended a second, quoted handler beside the old unquoted one, and every hook
// event would fire TWICE for the rest of that project's life. Duplicate events
// with the same activity_id land in core's dedupe, but the duplicate PreToolUse
// gate would evaluate and hold twice, which is a real latency and approval
// defect. Normalising both sides is what keeps the merge idempotent across the
// format change.
func hasLocalHookCommand(entries []any, command string) bool {
	want := unquoteHookCommand(command)
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		inner, _ := entry["hooks"].([]any)
		for _, h := range inner {
			hook, _ := h.(map[string]any)
			got, _ := hook["command"].(string)
			if unquoteHookCommand(got) == want {
				return true
			}
		}
	}
	return false
}

// unquoteHookCommand strips double quotes so a quoted and an unquoted spelling of
// the same command compare equal. Quotes never appear inside an engine path or an
// event name, so removing them all is sufficient and needs no parsing.
func unquoteHookCommand(command string) string {
	return strings.ReplaceAll(command, `"`, "")
}

// localHookCommand builds a hook command line with the engine path QUOTED.
//
// Unquoted was a latent bug that ADR-0016 promoted to a live one. The engine
// resolves to ~/.claude/plugins/openbox-observe/bin/openbox, so a home directory
// containing a space — an ordinary macOS or Windows account named for a person —
// produced a command a shell splits into two tokens. Every hook in that project
// then fails to start, silently, with no error at install time.
//
// It was survivable while project scope was an opt-in testing flag. It is not now
// that `openbox init` merges these hooks BY DEFAULT for every install. The plugin
// bundle's own hooks.json has always quoted (`"${CLAUDE_PLUGIN_ROOT}/bin/openbox"`)
// and so does the Codex installer's hookCommand; this was the one path that did
// not, and TestLocalHooksMirrorPluginBundle compares hook LISTS, not command
// strings, so nothing caught the divergence.
func localHookCommand(engine, args string) string {
	return `"` + engine + `" ` + args
}
