package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DefaultHooksPath is where this installer writes hooks.json: under $CODEX_HOME
// when set, else ~/.codex. Exported so an uninstall can look where the install
// wrote without duplicating the resolution.
func DefaultHooksPath() string { return defaultHooksPath() }

// RemoveHooks takes every OpenBox registration out of one Codex hooks.json and
// reports what it removed as "event engine" lines. It is the mirror of
// writeHooks, and holds the same line on ownership: every other key in this
// file, at any depth, is somebody else's.
//
// Unlike mergeEvent it keeps no handler at all — removal has no live engine to
// preserve, and a handler left pointing at a deleted binary keeps firing
// against a machine whose credentials this command is about to delete.
//
// An absent file is success: uninstall walks every surface unconditionally
// rather than branching on a detected provider.
func RemoveHooks(hooksPath string) (removed []string, err error) {
	before, err := os.ReadFile(hooksPath)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("codex uninstall: read %s: %w", hooksPath, err)
	}
	// Explicit, because sjson edits a malformed document without complaint and
	// Codex applies none of the hooks in a file it cannot parse. Refusing keeps
	// the file repairable; the install side refuses it too.
	if !gjson.ValidBytes(before) {
		detail := json.Unmarshal(before, new(any))
		return nil, fmt.Errorf("codex uninstall: %s exists but is not valid JSON; fix or remove it by hand: %w",
			hooksPath, detail)
	}

	out := before
	for _, ev := range hookedEvents {
		existing := gjson.GetBytes(out, hooksEventPath(ev))
		if !existing.Exists() || existing.Raw == "" {
			continue
		}
		kept, engines, err := dropOwnedGroups(json.RawMessage(existing.Raw))
		if err != nil {
			return removed, fmt.Errorf("codex uninstall: %s in %s: %w", ev, hooksPath, err)
		}
		for _, engine := range engines {
			removed = append(removed, string(ev)+" "+engine)
		}
		if len(engines) == 0 {
			continue
		}
		if out, err = sjson.SetRawBytes(out, hooksEventPath(ev), kept); err != nil {
			return removed, fmt.Errorf("codex uninstall: %s in %s: %w", ev, hooksPath, err)
		}
	}
	if bytes.Equal(out, before) {
		// Nothing of ours was in it; rewriting would put a diff in Codex's file
		// for no change at all.
		return removed, nil
	}
	if err := writeHooksFile(hooksPath, out, "codex uninstall: commit "+hooksPath); err != nil {
		return removed, err
	}
	return removed, nil
}

// dropOwnedGroups removes our handlers from one event's matcher groups and
// reports the engine path each removal was registered at. A group left with no
// handlers is dropped whole, the way mergeEvent drops one it emptied.
func dropOwnedGroups(existing json.RawMessage) (json.RawMessage, []string, error) {
	var groups []matcherGroup
	if err := json.Unmarshal(existing, &groups); err != nil {
		return nil, nil, fmt.Errorf("parse matcher groups: %w", err)
	}
	var engines []string
	kept := make([]matcherGroup, 0, len(groups))
	for _, g := range groups {
		foreign := make([]json.RawMessage, 0, len(g.Hooks))
		for _, h := range g.Hooks {
			if isOpenBoxHandler(h) {
				engines = append(engines, handlerEngine(h))
				continue
			}
			foreign = append(foreign, h)
		}
		if len(foreign) == 0 {
			continue
		}
		g.Hooks = foreign
		kept = append(kept, g)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, err
	}
	return encoded, engines, nil
}

// handlerEngine is the engine path a handler was registered at, for the report.
// isOpenBoxHandler has already established the shape.
func handlerEngine(raw json.RawMessage) string {
	var h struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(raw, &h) != nil {
		return "unknown"
	}
	engine, _, ok := stripEngineToken(strings.TrimSpace(h.Command))
	if !ok {
		return "unknown"
	}
	return engine
}

// writeHooksFile commits the document the way writeHooks does: 0600, one
// trailing newline, atomic rename. Codex reads this file on every session, so a
// partially written one is a governed machine that stops being governed.
func writeHooksFile(path string, out []byte, commitErr string) error {
	out = bytes.TrimRight(out, "\n")
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
		return fmt.Errorf("%s: %w", commitErr, err)
	}
	return nil
}

// HookInvocationMarkers are the substrings that identify an OpenBox
// registration in a Codex hooks file, derived from the same event list the
// installer writes so a renamed event cannot silently escape detection.
func HookInvocationMarkers() []string {
	markers := make([]string, 0, len(hookedEvents))
	for _, ev := range hookedEvents {
		markers = append(markers, "hook codex "+string(ev))
	}
	return markers
}
