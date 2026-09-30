package muse

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// removal is one owned handler dropped from settings.json.
type removal struct{ Event, Engine string }

// ownedRaw reports whether a raw handler is this adapter's: a command handler
// whose argv parses to `<engine> hook muse ...`.
func ownedRaw(h gjson.Result) (invocation, bool) {
	if !h.IsObject() || h.Get("type").String() != "command" {
		return invocation{}, false
	}
	return parseInvocation(h.Get("command").String())
}

// dropOwned removes every owned handler from one event's matcher groups by
// path, so nothing else in the document is re-encoded. A group left with no
// handlers goes whole; a group that also holds foreign handlers keeps them.
func dropOwned(doc []byte, event string) ([]byte, []removal, error) {
	base := "hooks." + escapeKey(event)
	groups := gjson.GetBytes(doc, base)
	if !groups.IsArray() {
		return doc, nil, nil
	}
	arr := groups.Array()
	out := doc
	var removed []removal
	var err error
	// Back to front: a deletion only moves what sits after it.
	for gi := len(arr) - 1; gi >= 0; gi-- {
		handlers := arr[gi].Get("hooks").Array()
		var owned []int
		var engines []string
		for hi, h := range handlers {
			if inv, ok := ownedRaw(h); ok {
				owned = append(owned, hi)
				engines = append(engines, inv.Engine)
			}
		}
		switch {
		case len(owned) == 0:
			continue
		case len(owned) == len(handlers):
			out, err = sjson.DeleteBytes(out, base+"."+strconv.Itoa(gi))
		default:
			for k := len(owned) - 1; k >= 0 && err == nil; k-- {
				out, err = sjson.DeleteBytes(out, base+"."+strconv.Itoa(gi)+".hooks."+strconv.Itoa(owned[k]))
			}
		}
		if err != nil {
			return doc, nil, err
		}
		for k := len(engines) - 1; k >= 0; k-- {
			removed = append([]removal{{event, engines[k]}}, removed...)
		}
	}
	return out, removed, nil
}

// RemoveHooks takes every OpenBox registration out of one Muse settings file
// and reports what it removed as "event engine" lines. It holds the install's
// line on ownership: a handler is OpenBox's only when its command parses to
// `<engine> hook muse ...`. schema_version and every other key, handler and
// byte of formatting stay as they were. An event or hooks block that this
// removal emptied goes with it, so an install followed by an uninstall leaves
// the file as it was.
//
// An absent file is success: uninstall walks every surface unconditionally. A
// file Muse cannot read is refused untouched, since editing it would only make
// it harder to repair.
func RemoveHooks(settingsPath string) (removed []string, err error) {
	path := settingsPath
	if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
		path = resolved
	}
	before, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("muse uninstall: read %s: %w", path, err)
	}
	if _, vErr := ValidateSettings(before); vErr != nil {
		return nil, fmt.Errorf("muse uninstall: %s exists but Muse cannot read it (%w); fix or remove it by hand", path, vErr)
	}

	out := before
	hooks := gjson.GetBytes(out, "hooks")
	if !hooks.IsObject() {
		return nil, nil
	}
	var events []string
	hooks.ForEach(func(k, _ gjson.Result) bool {
		events = append(events, k.String())
		return true
	})
	for _, ev := range events {
		var dropped []removal
		out, dropped, err = dropOwned(out, ev)
		if err != nil {
			return removed, fmt.Errorf("muse uninstall: %s in %s: %w", ev, path, err)
		}
		if len(dropped) == 0 {
			continue
		}
		for _, d := range dropped {
			removed = append(removed, d.Event+" "+d.Engine)
		}
		if g := gjson.GetBytes(out, "hooks."+escapeKey(ev)); g.IsArray() && len(g.Array()) == 0 {
			if out, err = sjson.DeleteBytes(out, "hooks."+escapeKey(ev)); err != nil {
				return removed, fmt.Errorf("muse uninstall: %s in %s: %w", ev, path, err)
			}
		}
	}
	if len(removed) > 0 {
		if h := gjson.GetBytes(out, "hooks"); h.IsObject() && len(h.Map()) == 0 {
			if out, err = sjson.DeleteBytes(out, "hooks"); err != nil {
				return removed, fmt.Errorf("muse uninstall: hooks in %s: %w", path, err)
			}
		}
	}
	out = keepTrailingSpace(out, before)
	if bytes.Equal(out, before) {
		return removed, nil
	}
	if _, vErr := ValidateSettings(out); vErr != nil {
		return nil, fmt.Errorf("muse uninstall: removal would leave %s unreadable (%w); nothing was changed", path, vErr)
	}
	perm := os.FileMode(0o600)
	if info, statErr := os.Stat(path); statErr == nil {
		perm = info.Mode().Perm()
	}
	if err := hookflow.AtomicWriteFile(path, out, perm); err != nil {
		return removed, fmt.Errorf("muse uninstall: commit %s: %w", path, err)
	}
	return removed, nil
}

// HookInvocationMarkers are the substrings that identify an OpenBox
// registration in Muse's settings file. Every handler this installer writes
// carries `hook muse ` whether or not a --home or --fail-closed follows it, so
// one marker covers them all. It only tells "this file may carry something of
// ours" from "it certainly does not"; ownership itself is parsed (RemoveHooks).
func HookInvocationMarkers() []string { return []string{"hook muse "} }
