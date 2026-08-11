package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// localhooks.go — opt-in LOCAL-TESTING hook scope (CredentialRef.LocalHooksDir).
//
// Production activates the plugin's hooks org-wide via managed settings (or
// the user enabling the plugin globally). For local testing you often want
// shift-left to govern ONE project only. When `init --local-hooks <dir>`
// is passed, Install additionally merges the hook entries into
// <dir>/.claude/settings.local.json — the per-developer, git-ignored Claude
// Code settings layer — pointing at the plugin's engine binary. Sessions
// started in any other directory stay ungoverned.
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
	{Event: "Stop", Timeout: 5},
	{Event: "SubagentStop", Timeout: 5},
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
		command := fmt.Sprintf("%s hook claude-code %s", engine, ev.Event)
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
				"command":     engine + " rewake claude-code",
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

// hasLocalHookCommand reports whether any entry already carries this exact
// command (idempotent re-init; never duplicates).
func hasLocalHookCommand(entries []any, command string) bool {
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		inner, _ := entry["hooks"].([]any)
		for _, h := range inner {
			hook, _ := h.(map[string]any)
			if hook["command"] == command {
				return true
			}
		}
	}
	return false
}
