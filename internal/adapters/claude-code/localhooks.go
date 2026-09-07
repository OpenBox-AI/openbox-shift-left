package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// That asymmetry is why the default inverted: a default the command cannot
// finish reports success while governing nothing.

// fileChangedMatcher is the watch list, not a pattern: Claude Code splits this
// on '|' into literal basenames in cwd, and "*" would watch a file named "*".
// Scope: the four files whose modification most changes what an agent can do
// or reach. .claude/settings*.json is deliberately NOT here -- ConfigChange
// covers it better, because ConfigChange can block and this cannot.
const fileChangedMatcher = ".env|.envrc|.mcp.json|CLAUDE.md"

var localHookEvents = []struct {
	Event         string
	Matcher       string
	Timeout       int
	StatusMessage string
	Async         bool
}{
	{Event: "SessionStart", Timeout: otherHookTimeoutSec},
	{Event: "UserPromptSubmit", Timeout: preToolUseHookTimeoutSec, StatusMessage: "OpenBox governance…"},
	{Event: "PreToolUse", Matcher: "*", Timeout: preToolUseHookTimeoutSec, StatusMessage: "OpenBox governance…"},
	{Event: "PostToolUse", Matcher: "*", Timeout: otherHookTimeoutSec},
	{Event: "PostToolUseFailure", Matcher: "*", Timeout: otherHookTimeoutSec},
	{Event: "Stop", Timeout: otherHookTimeoutSec},
	{Event: "SubagentStop", Timeout: otherHookTimeoutSec},
	{Event: "SubagentStart", Timeout: otherHookTimeoutSec},
	{Event: "PermissionDenied", Matcher: "*", Timeout: otherHookTimeoutSec},
	{Event: "StopFailure", Timeout: otherHookTimeoutSec},
	{Event: "SessionEnd", Timeout: 15},

	// The 21 new hook classes below, phase-04 table-B order (D5's sync/async
	// split). ConfigChange is the one armed hook (D4): it stays sync so a
	// headless run cannot silently lose a settings-edit decision, and it is
	// the only new row that carries a StatusMessage.
	{Event: "Setup", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "InstructionsLoaded", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "UserPromptExpansion", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "MessageDisplay", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "PermissionRequest", Timeout: otherHookTimeoutSec},
	{Event: "PostToolBatch", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "Notification", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "TaskCreated", Timeout: otherHookTimeoutSec},
	{Event: "TaskCompleted", Timeout: otherHookTimeoutSec},
	{Event: "TeammateIdle", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "ConfigChange", Timeout: preToolUseHookTimeoutSec, StatusMessage: "OpenBox governance…"},
	{Event: "CwdChanged", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "DirectoryAdded", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "FileChanged", Matcher: fileChangedMatcher, Timeout: otherHookTimeoutSec, Async: true},
	{Event: "WorktreeRemove", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "PreCompact", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "PostCompact", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "PreModelSwitch", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "PostModelSwitch", Timeout: otherHookTimeoutSec, Async: true},
	{Event: "Elicitation", Timeout: otherHookTimeoutSec},
	{Event: "ElicitationResult", Timeout: otherHookTimeoutSec},
}

// localHookPath addresses one event's matcher-group array. The event names are
// closed Go constants in localHookEvents and none carries gjson path syntax,
// which TestLocalHookEventNamesCarryNoPathSyntax holds rather than an escaper
// nothing would ever exercise.
func localHookPath(event string) string { return "hooks." + event }

// localHookEntries decodes one event's array into the []any shape sweepStale and
// reconcileLocalHook work on. Only the touched event; the rest stays as bytes.
func localHookEntries(raw []byte, event string) ([]any, error) {
	r := gjson.GetBytes(raw, localHookPath(event))
	if !r.Exists() || r.Type == gjson.Null {
		return nil, nil
	}
	if !r.IsArray() {
		// The old code read a non-array as absent and then overwrote it. Refusing
		// is the same posture as refusing an unparsable file: a shape somebody
		// chose is not ours to replace.
		return nil, fmt.Errorf("hooks.%s is not a JSON array; refusing to rewrite it", event)
	}
	var entries []any
	if err := json.Unmarshal([]byte(r.Raw), &entries); err != nil {
		return nil, fmt.Errorf("parse hooks.%s: %w", event, err)
	}
	return entries, nil
}

// setLocalHookEntries splices one event's array back in and leaves the rest of
// the document alone -- including the array itself when it already says what it
// should, because otherwise a re-run of `openbox init` reformats every event it
// owns for no change at all. When entries do change they are re-encoded, so a
// foreign matcher group keeps its content but not its own key order.
func setLocalHookEntries(raw []byte, event string, entries []any) ([]byte, error) {
	encoded, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("encode hooks.%s: %w", event, err)
	}
	if canonicalJSONEqual([]byte(gjson.GetBytes(raw, localHookPath(event)).Raw), encoded) {
		return raw, nil
	}
	return sjson.SetRawBytes(raw, localHookPath(event), encoded)
}

// deleteLocalHookEvent takes one event's key out of the document, and the hooks
// block with it once that key was the last one. Removal deletes rather than
// splicing an emptied array back in because encoding/json marshals an emptied
// slice as null, and Claude Code refuses a null event at load -- "must be an
// array of matchers; received null" -- warning once per key on every session
// start. An emptied array would load, but it is the same residue reading as a
// cleanup that failed. A developer's own event keeps the block, exactly as an
// org's own variables keep the env block in gatewayservice.
func deleteLocalHookEvent(raw []byte, event string) ([]byte, error) {
	out, err := sjson.DeleteBytes(raw, localHookPath(event))
	if err != nil {
		return nil, fmt.Errorf("remove hooks.%s: %w", event, err)
	}
	// Not an object is not our shape, and keys left are somebody's to keep.
	if block := gjson.GetBytes(out, "hooks"); !block.IsObject() || len(block.Map()) > 0 {
		return out, nil
	}
	if out, err = sjson.DeleteBytes(out, "hooks"); err != nil {
		return nil, fmt.Errorf("remove the emptied hooks block: %w", err)
	}
	return out, nil
}

// localHookEventIsNull reports the residue the removal above used to leave: the
// key present and null. It is checked separately from localHookEntries, which
// reads absent and null alike as "no entries", because the two need opposite
// treatment -- an absent key is nothing to write, and rewriting the file for it
// would reformat a document in the developer's own repository for no change.
// An empty array is left alone: it is valid, it is silent, and it may be theirs.
func localHookEventIsNull(raw []byte, event string) bool {
	r := gjson.GetBytes(raw, localHookPath(event))
	return r.Exists() && r.Type == gjson.Null
}

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

// writeHooks merges OpenBox's hook registrations into one settings file,
// addressed by path. The path is a parameter rather than a project directory
// because the same write serves the user-wide file that governs every session
// on this machine and, historically, a single project's local file. Its parent
// is created below, so a directory that does not exist yet is not an error.
func writeHooks(settingsPath, engine string) error {
	// Only the events below are rewritten. A map[string]any round trip kept every
	// key and alphabetised and reindented all of them, in a file inside the
	// developer's own repository. The validity check is explicit because sjson
	// edits a malformed document without complaint, and a broken settings file
	// stops every hook in it applying -- a governance failure reporting nothing.
	before, err := os.ReadFile(settingsPath)
	switch {
	case err == nil:
		if !gjson.ValidBytes(before) {
			detail := json.Unmarshal(before, new(any))
			return fmt.Errorf("local-hooks: %s exists but is not valid JSON; fix or remove it first: %w",
				settingsPath, detail)
		}
	case os.IsNotExist(err):
		before = nil
	default:
		return fmt.Errorf("local-hooks: read %s: %w", settingsPath, err)
	}
	out := before
	if len(out) == 0 {
		out = []byte("{}")
	}

	stale := map[string][]string{}
	var redundant []string
	for _, ev := range localHookEvents {
		command := localHookCommand(engine, "hook claude-code "+ev.Event)
		entries, err := localHookEntries(out, ev.Event)
		if err != nil {
			return fmt.Errorf("local-hooks: %s in %s: %w", ev.Event, settingsPath, err)
		}
		entries, dropped, deduped := sweepStale(entries, ev.Event, engine)
		for _, path := range dropped {
			stale[path] = append(stale[path], ev.Event)
		}
		if deduped {
			redundant = append(redundant, ev.Event)
		}
		if !hasLocalHookCommand(entries, command) {
			hook := map[string]any{"type": "command", "command": command, "timeout": ev.Timeout}
			if ev.StatusMessage != "" {
				hook["statusMessage"] = ev.StatusMessage
			}
			if ev.Async {
				hook["async"] = true // never write "async": false; absent and false must not be two states
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
			entries = append(entries, map[string]any{"matcher": ev.Matcher, "hooks": handlers})
		} else {
			entries = reconcileLocalHook(entries, command, ev.Timeout, ev.StatusMessage, ev.Async)
			if ev.Event == "PreToolUse" {
				entries = reconcileLocalHook(entries, localHookCommand(engine, "rewake claude-code"), rewakeHookTimeoutSec, "", false)
			}
		}
		if out, err = setLocalHookEntries(out, ev.Event, entries); err != nil {
			return fmt.Errorf("local-hooks: %s in %s: %w", ev.Event, settingsPath, err)
		}
	}
	if len(before) == 0 {
		// Nothing of the developer's to preserve in a file this created, and sjson
		// splices compactly, so give it the indentation a person can read.
		var doc any
		if json.Unmarshal(out, &doc) == nil {
			if pretty, mErr := json.MarshalIndent(doc, "", "  "); mErr == nil {
				out = append(pretty, '\n')
			}
		}
	} else if before[len(before)-1] == '\n' && (len(out) == 0 || out[len(out)-1] != '\n') {
		out = append(out, '\n') // sjson's splice can consume the trailing newline
	}

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return fmt.Errorf("local-hooks: mkdir %s: %w", filepath.Dir(settingsPath), err)
	}
	if err := writeFileAtomic(settingsPath, out, 0o644); err != nil {
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

// LocalHookAudit is the read-only view of one settings file's OpenBox hook
// registration: which engine(s) it points at, and whether any invocation is
// registered more than once.
type LocalHookAudit struct {
	// SettingsPath is the file inspected, reported whether or not it exists so a
	// reader can see which directory was audited.
	SettingsPath string
	// Present reports whether that file exists. For the user-wide file, absent
	// means this machine was never initialized. For a project file, absent is
	// the ordinary state: an install governs every session without touching one.
	Present bool
	// Engines are the distinct engine paths classified as OpenBox-owned, sorted.
	Engines []string
	// DuplicateEvents are the events where one invocation is registered more than
	// once; the same defect within a single engine path.
	DuplicateEvents []string
}

// AuditHooks reports what OpenBox registrations one settings file holds, so
// `openbox doctor` can surface a second engine. It exists so doctor and the
// installer cannot hold two opinions about what "ours" means: both classify
// through ownedLocalHook. The path is a parameter because doctor has two
// levels to report: the user-wide file and the cwd's project file.
func AuditHooks(settingsPath string) (LocalHookAudit, error) {
	audit := LocalHookAudit{SettingsPath: settingsPath}

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
			return "", "", false // unterminated quote; not our shape
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
// async is set or deleted exactly as statusMessage already is: that symmetry
// is what keeps a re-run of `openbox init` against an existing install from
// leaving a stale "async" key on an upgraded row (e.g. ConfigChange, whose
// Async is false).
func reconcileLocalHook(entries []any, command string, timeoutSec int, statusMessage string, async bool) []any {
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
			if async {
				hook["async"] = true
			} else {
				delete(hook, "async") // never leave "async": false; absent and false must not be two states
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
