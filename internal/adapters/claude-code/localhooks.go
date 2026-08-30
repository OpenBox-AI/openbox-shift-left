package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// That asymmetry is why the default inverted: a default the command cannot
// finish reports success while governing nothing.

var localHookEvents = []struct {
	Event         string
	Matcher       string
	Timeout       int
	StatusMessage string
}{
	{Event: "SessionStart", Timeout: 5},
	{Event: "UserPromptSubmit", Timeout: preToolUseHookTimeoutSec, StatusMessage: "OpenBox governance…"},
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
	stale := map[string][]string{}
	var redundant []string
	for _, ev := range localHookEvents {
		command := localHookCommand(engine, "hook claude-code "+ev.Event)
		entries, _ := hooks[ev.Event].([]any)
		entries, dropped, deduped := sweepStale(entries, ev.Event, engine)
		for _, path := range dropped {
			stale[path] = append(stale[path], ev.Event)
		}
		if deduped {
			redundant = append(redundant, ev.Event)
		}
		if hasLocalHookCommand(entries, command) {
			entries = reconcileLocalHook(entries, command, ev.Timeout, ev.StatusMessage)
			if ev.Event == "PreToolUse" {
				entries = reconcileLocalHook(entries, localHookCommand(engine, "rewake claude-code"), rewakeHookTimeoutSec, "")
			}
			hooks[ev.Event] = entries
			continue
		}
		hook := map[string]any{"type": "command", "command": command, "timeout": ev.Timeout}
		if ev.StatusMessage != "" {
			hook["statusMessage"] = ev.StatusMessage
		}
		handlers := []any{hook}
		if ev.Event == "PreToolUse" {
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
	if err := writeFileAtomic(settingsPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("local-hooks: write %s: %w", settingsPath, err)
	}

	if len(stale) > 0 {
		paths := make([]string, 0, len(stale))
		for p := range stale {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(os.Stderr, "openbox: replaced OpenBox hook registrations at a different engine path in %s\n"+
				"  old engine: %s\n  new engine: %s\n  events: %s\n"+
				"  The old engine no longer runs for this project. While both were registered, every hook fired "+
				"once per engine and every governed tool call was stored twice.\n",
				settingsPath, p, engine, strings.Join(stale[p], ", "))
		}
	}
	if len(redundant) > 0 {
		fmt.Fprintf(os.Stderr, "openbox: removed duplicate OpenBox hook registrations in %s\n"+
			"  events: %s\n"+
			"  The same hook was registered more than once at this engine, so it fired once per "+
			"registration and every matching event was stored that many times.\n",
			settingsPath, strings.Join(redundant, ", "))
	}
	return nil
}

// writeFileAtomic claude Code then cannot parse the settings for that project
// at all: every hook in the file stops applying, which is a governance failure
// that reports itself as nothing.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// LocalHookAudit is the read-only view of one project's OpenBox hook
// registration: which engine(s) it points at, and whether any invocation is
// registered more than once.
type LocalHookAudit struct {
	// SettingsPath is the file inspected, reported whether or not it exists so a
	// reader can see which directory was audited.
	SettingsPath string
	// Present reports whether that file exists. Absent is the normal state for a
	// global-scope install, or a directory that was never initialized.
	Present bool
	// Engines are the distinct engine paths classified as OpenBox-owned, sorted.
	Engines []string
	// DuplicateEvents are the events where one invocation is registered more than
	// once; the same defect within a single engine path.
	DuplicateEvents []string
}

// AuditLocalHooks reports what OpenBox registrations a project's
// settings.local.json holds, so `openbox doctor` can surface a second engine.
// It exists so doctor and the installer cannot hold two opinions about what
// "ours" means: both classify through ownedLocalHook.
func AuditLocalHooks(projectDir string) (LocalHookAudit, error) {
	dir, err := filepath.Abs(projectDir)
	if err != nil {
		return LocalHookAudit{}, fmt.Errorf("local-hooks: resolve %q: %w", projectDir, err)
	}
	audit := LocalHookAudit{SettingsPath: filepath.Join(dir, ".claude", "settings.local.json")}

	raw, err := os.ReadFile(audit.SettingsPath)
	if os.IsNotExist(err) {
		return audit, nil
	}
	if err != nil {
		return audit, fmt.Errorf("local-hooks: read %s: %w", audit.SettingsPath, err)
	}
	audit.Present = true
	settings := map[string]any{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return audit, fmt.Errorf("local-hooks: %s is not valid JSON: %w", audit.SettingsPath, err)
	}
	hooks, _ := settings["hooks"].(map[string]any)

	engines := map[string]bool{}
	for _, ev := range localHookEvents {
		entries, _ := hooks[ev.Event].([]any)
		counts := map[string]int{}
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			inner, _ := entry["hooks"].([]any)
			for _, h := range inner {
				hook, _ := h.(map[string]any)
				hookType, _ := hook["type"].(string)
				command, _ := hook["command"].(string)
				path, ok := ownedLocalHook(hookType, command, ev.Event)
				if !ok {
					continue
				}
				engines[path] = true
				_, invocation, _ := splitEngineToken(strings.TrimSpace(command))
				counts[invocation]++
			}
		}
		for _, n := range counts {
			if n > 1 {
				audit.DuplicateEvents = append(audit.DuplicateEvents, ev.Event)
				break
			}
		}
	}
	for e := range engines {
		audit.Engines = append(audit.Engines, e)
	}
	sort.Strings(audit.Engines)
	return audit, nil
}

// sweepStale foreign handlers, the first handler of each of our invocations at
// the engine being installed, and anything it cannot parse are all kept.
func sweepStale(entries []any, event, engine string) (kept []any, dropped []string, deduped bool) {
	want := unquoteHookCommand(engine)
	seen := map[string]bool{}
	registered := map[string]bool{}
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			kept = append(kept, e)
			continue
		}
		inner, ok := entry["hooks"].([]any)
		if !ok {
			kept = append(kept, e)
			continue
		}
		survivors := make([]any, 0, len(inner))
		for _, h := range inner {
			hook, _ := h.(map[string]any)
			hookType, _ := hook["type"].(string)
			command, _ := hook["command"].(string)
			if path, owned := ownedLocalHook(hookType, command, event); owned {
				if path != want {
					if !seen[path] {
						seen[path] = true
						dropped = append(dropped, path)
					}
					continue
				}
				_, invocation, _ := splitEngineToken(strings.TrimSpace(command))
				if registered[invocation] {
					deduped = true
					continue // already registered at this engine; a second copy just fires again
				}
				registered[invocation] = true
			}
			survivors = append(survivors, h)
		}
		if len(survivors) == 0 && len(inner) > 0 {
			continue // the entry carried nothing but our redundant handlers
		}
		entry["hooks"] = survivors
		kept = append(kept, entry)
	}
	return kept, dropped, deduped
}

func ownedLocalHook(hookType, command, event string) (engine string, ok bool) {
	if hookType != "command" {
		return "", false
	}
	token, rest, ok := splitEngineToken(strings.TrimSpace(command))
	if !ok {
		return "", false
	}
	if rest != "hook claude-code "+event && rest != "rewake claude-code" {
		return "", false
	}
	return unquoteHookCommand(token), true
}

func splitEngineToken(cmd string) (engine, rest string, ok bool) {
	if cmd == "" {
		return "", "", false
	}
	if cmd[0] == '"' {
		end := strings.IndexByte(cmd[1:], '"')
		if end < 0 {
			return "", "", false // unterminated quote — not our shape
		}
		return cmd[1 : end+1], strings.TrimSpace(cmd[end+2:]), true
	}
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		return cmd[:i], strings.TrimSpace(cmd[i+1:]), true
	}
	return cmd, "", true
}

// reconcileLocalHook it never touches a foreign hook, another of our
// invocations (the rewake watcher has its own command), or the group matcher.
func reconcileLocalHook(entries []any, command string, timeoutSec int, statusMessage string) []any {
	want := unquoteHookCommand(command)
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		inner, _ := entry["hooks"].([]any)
		for _, h := range inner {
			hook, _ := h.(map[string]any)
			got, _ := hook["command"].(string)
			if unquoteHookCommand(got) != want {
				continue
			}
			hook["timeout"] = timeoutSec
			if statusMessage != "" {
				hook["statusMessage"] = statusMessage
			} else {
				delete(hook, "statusMessage")
			}
		}
	}
	return entries
}

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

// unquoteHookCommand quotes never appear inside an engine path or an event
// name, so removing them all is sufficient and needs no parsing.
func unquoteHookCommand(command string) string {
	return strings.ReplaceAll(command, `"`, "")
}

// localHookCommand every hook in that project then fails to start, silently,
// with no error at install time.
func localHookCommand(engine, args string) string {
	return `"` + engine + `" ` + args
}
