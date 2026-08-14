package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
// The merge preserves everything foreign and replaces what is ours. Existing
// settings keys, and hook entries a developer added themselves, survive
// untouched; our own entry is recognised by its ARGV SHAPE rather than by an
// exact command string, so a registration left behind at a DIFFERENT engine
// path — the residue of an install run with another HOME — is dropped and
// replaced rather than kept beside the current one.
//
// Keeping it is what the exact-string comparison used to do, and the cost was
// invisible: both engines fired for every event, so every governed tool call was
// stored twice, for as long as that project lived. A replacement prints a notice
// naming the engine it retired — silently swapping a governing binary is the
// same class of problem as the silent duplicate.
//
// A repeat of one of our OWN invocations at the CURRENT engine is collapsed for
// the same reason: it fires twice and stores twice exactly as a second engine
// does. That case is why the sweep is a de-duplication and not only a
// replacement — `openbox doctor` reports both conditions and names this command
// as the remedy for both, so a shape it warned about and this could not repair
// would leave a developer re-running a command that never clears the warning.
//
// One shape stays unrecognised: an UNQUOTED engine path CONTAINING A SPACE,
// because the leading token ends at the first space. Only a pre-quoting build
// could have written one, and those hooks never started at all (see
// localHookCommand), so the leftover is dead weight rather than a second live
// engine. `openbox doctor` is what surfaces it.

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
	// stale engine path → the events it was still registered for, and the events
	// that carried one of our invocations more than once at THIS engine.
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
			// Already ours at this engine. The sweep may still have changed the
			// slice, so write it back rather than leaving the original in place.
			hooks[ev.Event] = entries
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
	if err := writeFileAtomic(settingsPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("local-hooks: write %s: %w", settingsPath, err)
	}

	// Name what was retired, and only once it is actually retired. A developer
	// whose events were being stored twice has no other way to learn which build
	// was running, and a governing binary must not be swapped without saying so —
	// but "the old engine no longer runs" is a claim about the file on disk, so
	// announcing it before the write would state it of a write that can still
	// fail.
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

// writeFileAtomic replaces path in one step: a temp file in the SAME directory
// (so the rename cannot cross a filesystem boundary), then a rename over the
// target.
//
// This was an os.WriteFile, and that is a truncate-then-write — two steps a
// concurrent writer can interleave with. Two `openbox init` runs against one
// project both truncated to zero and then wrote at their OWN offsets, so the
// shorter document landed inside the longer one and the file ended as a
// complete JSON document followed by the tail of another. Claude Code then
// cannot parse the settings for that project at all: every hook in the file
// stops applying, which is a governance failure that reports itself as nothing.
// Every other durable write in this repo already commits through a rename; this
// one did not, and it is the only one a developer can trigger twice at once.
//
// The guarantee is that a reader sees either the old file or the new one, never
// a splice of both. It is NOT mutual exclusion: two concurrent runs still race
// to be last, and the loser's merge is discarded. That is harmless here — both
// write the same OpenBox registrations for the same engine — and closing it
// would need a lockfile, which is a bigger contract than the corruption
// warrants.
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
	// CreateTemp makes the file 0600; settings.local.json is not a secret and
	// the previous WriteFile published it at perm, so match that rather than
	// silently tightening a file other tools read.
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
	// reader can see WHICH directory was audited.
	SettingsPath string
	// Present reports whether that file exists. Absent is the normal state for a
	// global-scope install, or a directory that was never initialized.
	Present bool
	// Engines are the distinct engine paths classified as OpenBox-owned, sorted.
	// More than one means every hook fires once per engine.
	Engines []string
	// DuplicateEvents are the events where one invocation is registered more than
	// once — the same defect within a single engine path.
	DuplicateEvents []string
}

// AuditLocalHooks reports what OpenBox registrations a project's
// settings.local.json holds, so `openbox doctor` can surface a second engine.
//
// It exists so doctor and the installer cannot hold two opinions about what
// "ours" means: both classify through ownedLocalHook. Doctor re-deriving its own
// parse is precisely the divergence that let the duplicate registration survive
// unnoticed in the first place.
//
// Read-only, and diagnostic rather than assurance: it proves what is REGISTERED
// in one file, never that a hook ran.
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
		// Count per INVOCATION, not per event: PreToolUse legitimately carries two
		// of ours (the gate and the approval watcher), so counting handlers would
		// warn on every healthy install.
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

// sweepStale reduces one event's entries to at most ONE registration of each of
// our invocations, at the engine being installed. It reports the distinct other
// engine paths it dropped, and whether it also collapsed a repeat of an
// invocation already registered at this engine.
//
// Both removals answer the same defect — a hook registered twice fires twice and
// stores every matching event twice — but they are reported separately because
// they tell a developer different things: one says which build was also running,
// the other says the file had our own entry in it twice.
//
// Foreign handlers, the first handler of each of our invocations at the engine
// being installed, and anything it cannot parse are all kept. The direction of
// error is fixed at over-keep, never over-delete, because the cost of keeping one
// stale entry is duplicate telemetry while the cost of deleting one foreign hook
// is breaking a developer's own tooling — and nothing is dropped that is not
// provably a second copy of a command this file itself generates. An entry whose
// handlers were ALL ours is removed with them; an entry that was already empty is
// left exactly as it was.
//
// Keep/drop discipline mirrors adapters/codex Installer.mergeEvent, which has
// always replaced by shape; see ownedLocalHook for why there are two copies.
func sweepStale(entries []any, event, engine string) (kept []any, dropped []string, deduped bool) {
	want := unquoteHookCommand(engine)
	seen := map[string]bool{}
	// Which of our invocations are already registered at this engine. Keyed by
	// invocation, not by event: PreToolUse legitimately carries two of ours (the
	// gate and the approval watcher), and they must not collapse into each other.
	// Declared out here because the copies can sit in DIFFERENT entries.
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
		// Mutate in place so matcher and any key we do not know about survive.
		entry["hooks"] = survivors
		kept = append(kept, entry)
	}
	return kept, dropped, deduped
}

// ownedLocalHook reports whether one hook handler is OpenBox-owned, and at which
// engine path, regardless of where that engine lives.
//
// Owned means: a `command` handler whose ENTIRE command is one engine token
// followed by exactly one of the two invocations this file generates —
// `hook claude-code <the event it is filed under>` or `rewake claude-code`.
// Anything else is foreign, including a compound command that merely embeds one
// of those invocations, and including an owned invocation filed under the wrong
// event key. Exact-remainder matching after a one-token strip is what keeps a
// developer's own hook safe; a `strings.Contains` here would delete it.
//
// Ported from adapters/codex isOpenBoxHandler + stripEngineToken
// (adapters/codex/installer.go:262-302) rather than shared: the adapters are
// separate go.work modules, this one owns a second invocation (`rewake`) that
// Codex's anchored regex rejects, and the handlers arrive here as
// map[string]any instead of json.RawMessage. A shared home costs a twelfth
// module for ~25 lines. Revisit when a third adapter needs the same parse.
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
	// Compare unquoted forms, for the same reason hasLocalHookCommand does: an
	// install written before localHookCommand quoted must still be recognised as
	// the same engine.
	return unquoteHookCommand(token), true
}

// splitEngineToken splits a command line into its leading engine token and the
// remainder — a double-quoted path (the shape localHookCommand generates) or a
// single unquoted word (a pre-quoting install, or the `openbox`-on-PATH
// fallback). ok=false when there is no well-formed leading token, e.g. an
// unterminated quote.
//
// A bare token with no remainder carries no hook invocation, so it returns an
// empty rest and lets the caller's exact-match reject it.
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
