package devconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
}

// loadManaged reads the managed layer. Every failure degrades to "not managed"
// for resolution purposes while still reporting present/readable, because
// refusing to resolve at all would take down every session on the machine over a
// malformed org file — and a hook that cannot resolve config cannot observe
// either, so the org would lose the evidence too.
func loadManaged() managedState {
	path := ManagedConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return managedState{}
	}
	st := managedState{present: true}
	if err := unmarshalStrict(raw, &st.cfg); err != nil {
		return st // present, not readable
	}
	st.readable = true
	st.keys = configKeys(path)
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

	managed := loadManaged()
	if managed.readable && managed.keys[fieldName] {
		if v := field(managed.cfg.DevConfig); v != nil {
			// An org mandate short-circuits everything below it.
			if managed.locked[fieldName] {
				return *v, SourceManaged
			}
			value, source = *v, SourceManagedDefault
		}
	}
	if cfg, err := load(); err == nil && configKeys(DefaultConfigPath())[fieldName] {
		if v := field(cfg); v != nil {
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
func configKeys(path string) map[string]bool {
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
}

// Managed returns the managed layer's status.
func Managed() ManagedStatus {
	st := loadManaged()
	return ManagedStatus{
		Path:     ManagedConfigPath(),
		Present:  st.present,
		Readable: st.readable,
		Locked:   st.cfg.Locked,
	}
}
