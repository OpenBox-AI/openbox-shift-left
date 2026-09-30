package muse

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/tidwall/gjson"
)

// settingsSchemaVersion is the settings.json schema this adapter writes and
// understands.
const settingsSchemaVersion = 1

// handlerKeys are the keys a hook handler may carry. Muse reads handlers
// through a closed schema, and a source holding one it rejects contributes
// nothing; so a key outside this set is reported, never written.
var handlerKeys = map[string]bool{"type": true, "command": true, "timeout": true, "onFailure": true, "async": true}

// groupKeys are the keys a matcher group may carry.
var groupKeys = map[string]bool{"matcher": true, "hooks": true}

// handler is one parsed hook handler.
type handler struct {
	Event   string
	Group   int
	Index   int
	Type    string
	Command string
	Timeout int // seconds; 0 when absent
	// Async reports "async": true: the handler runs without being waited for,
	// so its answer can never block anything.
	Async bool
	// Matcher is the enclosing group's matcher, nil when it has none.
	Matcher    *string
	OnFailure  *handler
	Invocation invocation
	// Owned reports that Command parses as this adapter's own invocation.
	Owned bool
}

// settingsDoc is a settings.json read under Muse's rules.
type settingsDoc struct {
	SchemaVersion *int
	Handlers      []handler
	// Warnings are findings that do not make the file unreadable: a key or event
	// this adapter does not know, which a newer Muse may well accept.
	Warnings []string
}

// SettingsReport is what ValidateSettings found in a readable settings file.
type SettingsReport struct {
	// SchemaVersion is the file's schema_version; nil when absent.
	SchemaVersion *int
	// Warnings are findings that leave the file usable.
	Warnings []string
}

// ValidateSettings checks a settings.json the way Muse reads it: valid JSON, a
// top-level object, a schema_version this adapter understands, and a hooks
// block of typed groups and handlers. An error means Muse would drop every
// handler in the file (a malformed source contributes nothing, with only a
// startup warning), so the installer refuses to write into it and doctor says
// so. Install and doctor both call it, so they cannot disagree about what a
// valid file is.
func ValidateSettings(data []byte) (SettingsReport, error) {
	doc, err := parseSettings(data)
	if err != nil {
		return SettingsReport{}, err
	}
	return SettingsReport{SchemaVersion: doc.SchemaVersion, Warnings: doc.Warnings}, nil
}

func parseSettings(data []byte) (settingsDoc, error) {
	var doc settingsDoc
	if !gjson.ValidBytes(data) {
		return doc, fmt.Errorf("not valid JSON: %w", json.Unmarshal(data, new(any)))
	}
	root := gjson.ParseBytes(data)
	if !root.IsObject() {
		return doc, fmt.Errorf("the top level must be a JSON object, found %s", jsonKind(root))
	}
	if sv := root.Get("schema_version"); sv.Exists() {
		if sv.Type != gjson.Number || float64(int(sv.Num)) != sv.Num {
			return doc, fmt.Errorf("schema_version must be an integer, found %s", sv.Raw)
		}
		n := int(sv.Num)
		if n != settingsSchemaVersion {
			return doc, fmt.Errorf("schema_version is %d; this adapter writes and understands version %d only", n, settingsSchemaVersion)
		}
		doc.SchemaVersion = &n
	}
	hooks := root.Get("hooks")
	if !hooks.Exists() {
		return doc, nil
	}
	if !hooks.IsObject() {
		return doc, fmt.Errorf("hooks must be an object keyed by event name, found %s", jsonKind(hooks))
	}
	var firstErr error
	hooks.ForEach(func(eventKey, groups gjson.Result) bool {
		event := eventKey.String()
		if _, err := ParseHookName(event); err != nil {
			doc.Warnings = append(doc.Warnings, fmt.Sprintf("hooks.%s: not an event this adapter knows", event))
		}
		if !groups.IsArray() {
			firstErr = fmt.Errorf("hooks.%s must be an array of matcher groups, found %s", event, jsonKind(groups))
			return false
		}
		for gi, g := range groups.Array() {
			if err := parseGroup(&doc, event, gi, g); err != nil {
				firstErr = err
				return false
			}
		}
		return true
	})
	if firstErr != nil {
		return doc, firstErr
	}
	sort.Strings(doc.Warnings)
	return doc, nil
}

func parseGroup(doc *settingsDoc, event string, gi int, g gjson.Result) error {
	where := fmt.Sprintf("hooks.%s[%d]", event, gi)
	if !g.IsObject() {
		return fmt.Errorf("%s must be an object, found %s", where, jsonKind(g))
	}
	var err error
	g.ForEach(func(k, v gjson.Result) bool {
		switch {
		case !groupKeys[k.String()]:
			doc.Warnings = append(doc.Warnings, fmt.Sprintf("%s: unrecognised key %q", where, k.String()))
		case k.String() == "matcher" && v.Type != gjson.String:
			err = fmt.Errorf("%s.matcher must be a string, found %s", where, jsonKind(v))
		}
		return err == nil
	})
	if err != nil {
		return err
	}
	hs := g.Get("hooks")
	if !hs.IsArray() {
		return fmt.Errorf("%s.hooks must be an array of handlers, found %s", where, jsonKind(hs))
	}
	for hi, raw := range hs.Array() {
		h, warns, err := parseHandler(raw, fmt.Sprintf("%s.hooks[%d]", where, hi))
		if err != nil {
			return err
		}
		if m := g.Get("matcher"); m.Exists() {
			matcher := m.String()
			h.Matcher = &matcher
		}
		h.Event, h.Group, h.Index = event, gi, hi
		doc.Handlers = append(doc.Handlers, h)
		doc.Warnings = append(doc.Warnings, warns...)
	}
	return nil
}

func parseHandler(raw gjson.Result, where string) (handler, []string, error) {
	var h handler
	var warns []string
	if !raw.IsObject() {
		return h, nil, fmt.Errorf("%s must be an object, found %s", where, jsonKind(raw))
	}
	var err error
	raw.ForEach(func(k, v gjson.Result) bool {
		key := k.String()
		if !handlerKeys[key] {
			warns = append(warns, fmt.Sprintf("%s: unrecognised key %q; Muse reads handlers through a closed schema", where, key))
			return true
		}
		switch key {
		case "type", "command":
			if v.Type != gjson.String {
				err = fmt.Errorf("%s.%s must be a string, found %s", where, key, jsonKind(v))
				return false
			}
		case "async":
			if v.Type != gjson.True && v.Type != gjson.False {
				err = fmt.Errorf("%s.async must be a boolean, found %s", where, jsonKind(v))
				return false
			}
			h.Async = v.Type == gjson.True
		case "timeout":
			if v.Type != gjson.Number || v.Num <= 0 || float64(int(v.Num)) != v.Num {
				err = fmt.Errorf("%s.timeout must be a positive whole number of seconds, found %s", where, v.Raw)
				return false
			}
			h.Timeout = int(v.Num)
		}
		return true
	})
	if err != nil {
		return h, nil, err
	}
	h.Type = raw.Get("type").String()
	h.Command = raw.Get("command").String()
	if h.Type != "command" {
		warns = append(warns, fmt.Sprintf("%s: handler type %q is not one this adapter knows", where, h.Type))
	} else if h.Command == "" {
		return h, nil, fmt.Errorf("%s is a command handler with no command", where)
	}
	if of := raw.Get("onFailure"); of.Exists() {
		succ, w, err := parseHandler(of, where+".onFailure")
		if err != nil {
			return h, nil, err
		}
		warns = append(warns, w...)
		h.OnFailure = &succ
	}
	if h.Type == "command" {
		h.Invocation, h.Owned = parseInvocation(h.Command)
	}
	return h, warns, nil
}

func jsonKind(r gjson.Result) string {
	switch r.Type {
	case gjson.Null:
		return "null"
	case gjson.False, gjson.True:
		return "a boolean"
	case gjson.Number:
		return "a number"
	case gjson.String:
		return "a string"
	}
	if r.IsArray() {
		return "an array"
	}
	return "an object"
}
