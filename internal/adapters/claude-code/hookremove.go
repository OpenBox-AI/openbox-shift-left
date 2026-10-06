package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/tidwall/gjson"
)

// RemoveLocalHooks takes every OpenBox registration out of one Claude Code
// settings file and reports what it removed as "event engine" lines. It is the
// mirror of writeLocalHooks, and takes a settings-file path rather than a
// project directory because the same removal serves the user-scope file, a
// project's settings.local.json and the plugin bundle's own document.
//
// It differs from sweepStale in one way that matters: sweepStale keeps the
// handler registered at the engine currently being installed, because that one
// is the live registration. Removal has no live engine to keep, so ours goes
// at every engine path. A handler left behind keeps firing at once — Claude
// Code's file watcher picks up settings edits immediately — against a machine
// whose credentials this command is about to delete.
//
// An absent file is success: uninstall walks every surface unconditionally
// rather than branching on a detected provider, so "never installed here" is
// the ordinary case.
func RemoveLocalHooks(settingsPath string) (removed []string, err error) {
	before, err := os.ReadFile(settingsPath)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("local-hooks: read %s: %w", settingsPath, err)
	}
	// Explicit, because sjson edits a malformed document without complaint and a
	// truncated settings file stops every hook in it applying -- including the
	// developer's own -- while reporting nothing. Refusing also keeps the file
	// repairable: the install side refuses what it cannot parse too.
	if !gjson.ValidBytes(before) {
		detail := json.Unmarshal(before, new(any))
		return nil, fmt.Errorf("local-hooks: %s exists but is not valid JSON; fix or remove it by hand: %w",
			settingsPath, detail)
	}

	out := before
	for _, ev := range localHookEvents {
		entries, err := localHookEntries(out, ev.Event)
		if err != nil {
			return removed, fmt.Errorf("local-hooks: %s in %s: %w", ev.Event, settingsPath, err)
		}
		kept, engines := dropOwnedHandlers(entries, ev.Event)
		for _, engine := range engines {
			removed = append(removed, ev.Event+" "+engine)
		}
		if len(kept) > 0 {
			if len(engines) == 0 {
				continue // nothing of ours under this event; leave the array as it is
			}
			if out, err = setLocalHookEntries(out, ev.Event, kept); err != nil {
				return removed, fmt.Errorf("local-hooks: %s in %s: %w", ev.Event, settingsPath, err)
			}
			continue
		}
		// The event carries nothing now, either because every group under it was
		// ours or because an older removal left a null behind. Delete the key --
		// see deleteLocalHookEvent for why an emptied array is not written. An
		// absent key is the ordinary case and not a change.
		if len(engines) == 0 && !localHookEventIsNull(out, ev.Event) {
			continue
		}
		if out, err = deleteLocalHookEvent(out, ev.Event); err != nil {
			return removed, fmt.Errorf("local-hooks: %s in %s: %w", ev.Event, settingsPath, err)
		}
	}
	if bytes.Equal(out, before) {
		// Nothing of ours was in it. Rewriting would reformat a document in the
		// developer's own repository for no change at all.
		return removed, nil
	}
	if before[len(before)-1] == '\n' && (len(out) == 0 || out[len(out)-1] != '\n') {
		out = append(out, '\n') // sjson's splice can consume the trailing newline
	}
	if err := writeFileAtomic(settingsPath, out, 0o644); err != nil {
		return removed, fmt.Errorf("local-hooks: write %s: %w", settingsPath, err)
	}
	return removed, nil
}

// dropOwnedHandlers removes our handlers from one event's matcher groups and
// reports the engine path each removal was registered at. A group left with no
// handlers is dropped whole: Claude Code reads an empty group as a group, and
// the residue reads as a cleanup that failed. Anything it cannot parse is
// kept, which is the same direction of error the install side takes -- keep a
// stranger's entry rather than guess.
func dropOwnedHandlers(entries []any, event string) (kept []any, engines []string) {
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
			if engine, owned := ownedLocalHook(hookType, command, event); owned {
				engines = append(engines, engine)
				continue
			}
			survivors = append(survivors, h)
		}
		if len(survivors) == 0 && len(inner) > 0 {
			continue
		}
		entry["hooks"] = survivors
		kept = append(kept, entry)
	}
	return kept, engines
}

// DefaultPluginDir is where every install materializes the engine copy at
// bin/openbox. Exported because a full uninstall has to delete the directory --
// and because an install from an older binary also left a plugin manifest and a
// second copy of the hook config there, which is the one hook path Claude Code
// does not de-duplicate against the settings files.
func DefaultPluginDir() string { return userPluginDir() }

// HookInvocationMarkers are the substrings that identify an OpenBox
// registration in a settings file, derived from the same event list the
// installer writes so a renamed event cannot silently escape detection. The
// engine path is deliberately not part of them: a registration carries
// whichever path installed it.
func HookInvocationMarkers() []string {
	seen := map[string]bool{"rewake claude-code": true}
	markers := []string{"rewake claude-code"}
	for _, ev := range localHookEvents {
		m := "hook claude-code " + ev.Event
		if !seen[m] {
			seen[m] = true
			markers = append(markers, m)
		}
	}
	return markers
}
