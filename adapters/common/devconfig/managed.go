package devconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
)

// The managed OpenBox layer (E8-S9).
//
// Everything in dev.json is resolved from a file the developer owns, with an env
// var able to override it. That is right for a single developer and useless as an
// org mandate: `enforce` can be flipped off in either place (report SL-01).
//
// A root-owned /etc/openbox/dev.json fixes it, but only if it beats BOTH the user
// file and the environment. Beating the user file alone would be theater —
// `OPENBOX_ENFORCE=0` would still disable the gate — so for fields the org marks
// locked, the precedence is deliberately inverted:
//
//	locked managed field  >  env  >  user config  >  built-in default
//
// Fields present but NOT locked act as org defaults the developer may still
// override, which gives orgs a real distinction between "we recommend" and "we
// require" instead of forcing every setting to be a mandate.
//
// This is a local-filesystem control, so it is only as strong as the file's
// permissions: it stops a developer editing their own config, not root. That is
// the same trust model as the provider-managed config it complements (E8-S8), and
// the reason posture reports the source of each field — a machine whose managed
// file is missing shows up as user-controlled rather than looking compliant.

// EnvManagedConfig overrides the managed-config path. Primarily for tests; also
// lets an org relocate the file, at the cost of an env var that must then itself
// be trusted.
const EnvManagedConfig = "OPENBOX_MANAGED_CONFIG"

// ManagedConfig is the org-controlled layer: the same fields as DevConfig plus an
// explicit list of which ones are mandates.
type ManagedConfig struct {
	DevConfig
	// Locked names the fields the org requires. Anything listed here overrides
	// both the user config and the environment; anything present but unlisted is
	// an org default the developer may override.
	//
	// Names are the JSON field names ("enforce", "content_capture", …) so the
	// file reads the same way as the config it governs.
	Locked []string `json:"locked,omitempty"`
}

// ManagedConfigPath is where the org-controlled config lives.
func ManagedConfigPath() string {
	if p := os.Getenv(EnvManagedConfig); p != "" {
		return p
	}
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "OpenBox", "dev.json")
	default:
		return "/etc/openbox/dev.json"
	}
}

// managedState is the parsed managed layer plus whether reading it was possible.
type managedState struct {
	cfg ManagedConfig
	// present means a managed file exists at the expected path.
	present bool
	// readable means it was parsed successfully. present-but-unreadable is
	// reported rather than ignored: it means the machine is *intended* to be
	// managed and something is wrong, which an operator needs to see.
	readable bool
	locked   map[string]bool
	// keys is the set of top-level keys actually present in the file.
	keys map[string]bool
	// unknownKeys names top-level keys no setting recognizes. They no longer
	// invalidate the file (OD-RF-2) but an operator still needs to see them: an
	// org that misspells a field believes it set something it did not.
	unknownKeys []string
}

// loadManaged reads the managed layer. Every failure degrades to "not managed"
// for resolution purposes while still reporting present/readable, because
// refusing to resolve at all would take down every session on the machine over a
// malformed org file — and a hook that cannot resolve config cannot observe
// either, so the org would lose the evidence too.
func loadManagedUncached() managedState {
	path := ManagedConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return managedState{}
	}
	st := managedState{present: true}
	// Parse leniently, then report anything unrecognized (OD-RF-2).
	//
	// This used to be a strict parse of the whole file, so ONE unknown key
	// anywhere in it made the file unreadable and dropped every mandate for
	// resolution — enforce included — leaving the machine developer-controlled
	// and saying so only in `doctor`. A typo in an unrelated field should not
	// be able to un-govern a machine, and if the file is ever group-writable
	// that downgrade is inducible by appending junk.
	//
	// So an unknown key no longer costs the mandates; it is surfaced instead
	// (UnknownKeys → posture evidence and doctor). Structural damage — the file
	// is not JSON, or `locked` is not a list of strings — still makes it
	// unreadable, because then the mandate itself cannot be trusted.
	if err := json.Unmarshal(raw, &st.cfg); err != nil {
		return st // present, not readable: not even JSON
	}
	st.unknownKeys = unknownManagedKeys(raw)
	st.readable = true
	st.keys = configKeysUncached(path)
	st.locked = make(map[string]bool, len(st.cfg.Locked))
	for _, f := range st.cfg.Locked {
		st.locked[strings.TrimSpace(f)] = true
	}
	return st
}

// Source names where a resolved value came from, for `openbox doctor` and for the
// posture the control plane sees.
type Source string

const (
	SourceDefault Source = "default"
	SourceUser    Source = "user"
	SourceEnv     Source = "env"
	// SourceManaged means an org mandate produced this value: it overrode both
	// the user config and the environment.
	SourceManaged Source = "managed"
	// SourceManagedDefault means the managed file supplied the value but did not
	// lock it, so the developer could still override it (and did not).
	SourceManagedDefault Source = "managed_default"
)

// resolveBoolWithSource is the full precedence chain for one boolean field.
// fieldName is the JSON name used in the managed file's "locked" list.
//
// Presence is decided by whether the key is actually in the file, not by whether
// the decoded accessor returns non-nil. Several DevConfig flags are plain bools
// (Enforce, FailClosed), so their accessors always yield a pointer to false when
// the key is absent — which would make every layer look like it had set the
// value, and report a mandate or a user choice where there was neither.
func resolveBoolWithSource(fieldName string, field func(DevConfig) *bool, def bool, envKey string) (bool, Source) {
	value, source := def, SourceDefault

	managed := cachedManaged()
	if managed.readable && managed.keys[fieldName] {
		if v := field(managed.cfg.DevConfig); v != nil {
			// An org mandate short-circuits everything below it.
			if managed.locked[fieldName] {
				return *v, SourceManaged
			}
			value, source = *v, SourceManagedDefault
		}
	}
	if user := cachedUser(); user.err == nil && user.keys[fieldName] {
		if v := field(user.cfg); v != nil {
			value, source = *v, SourceUser
		}
	}
	if v, ok := os.LookupEnv(envKey); ok {
		value, source = IsTruthy(v), SourceEnv
	}
	return value, source
}

// configKeys returns the top-level keys actually present in a config file, so
// "absent" and "explicitly false" can be told apart. A missing or malformed file
// has no keys.
func configKeysUncached(path string) map[string]bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	keys := make(map[string]bool, len(obj))
	for k := range obj {
		if strings.HasPrefix(k, "//") {
			continue // documentation, not a setting (see unmarshalStrict)
		}
		keys[k] = true
	}
	return keys
}

// ManagedStatus summarizes the managed layer for `openbox doctor`.
type ManagedStatus struct {
	Path     string
	Present  bool
	Readable bool
	Locked   []string
	// UnknownKeys names top-level keys in the managed file that match no
	// setting. They are ignored rather than invalidating the file (OD-RF-2),
	// so they must be reported or an org would not know its file has a typo.
	UnknownKeys []string
	// UnknownLocked names entries in `locked` that match no known setting.
	// A typo there locks nothing and used to do so in complete silence, so an
	// org could believe a mandate was in force when it was not.
	UnknownLocked []string
}

// Managed returns the managed layer's status.
func Managed() ManagedStatus {
	st := cachedManaged()
	return ManagedStatus{
		Path:          ManagedConfigPath(),
		Present:       st.present,
		Readable:      st.readable,
		Locked:        st.cfg.Locked,
		UnknownLocked: unknownLocked(st.cfg.Locked),
		UnknownKeys:   st.unknownKeys,
	}
}

// unknownManagedKeys returns the file's top-level keys that match no setting
// and no mandate field. Documentation keys ("// …") are excluded, matching the
// convention the config format already allows.
func unknownManagedKeys(raw []byte) []string {
	var all map[string]json.RawMessage
	if json.Unmarshal(raw, &all) != nil {
		return nil
	}
	known := lockableFields()
	known["locked"] = true
	var out []string
	for k := range all {
		if strings.HasPrefix(k, "//") || known[k] {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lockableFields are the settings an org mandate can pin. Derived from the
// DevConfig json tags so it cannot drift from the schema.
func lockableFields() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(DevConfig{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// unknownLocked returns the `locked` entries that name no known setting, so
// doctor and the posture report can say so instead of silently locking nothing.
func unknownLocked(locked []string) []string {
	known := lockableFields()
	var out []string
	for _, f := range locked {
		if f = strings.TrimSpace(f); f != "" && !known[f] {
			out = append(out, f)
		}
	}
	return out
}
