package devconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// EnvManagedConfig overrides the managed-config path. Primarily for tests;
// also lets an org relocate the file, at the cost of an env var that must then
// itself be trusted.
const EnvManagedConfig = "OPENBOX_MANAGED_CONFIG"

// ManagedConfig is the org-controlled layer: the same fields as DevConfig plus
// an explicit list of which ones are mandates.
type ManagedConfig struct {
	DevConfig
	// Locked names the fields the org requires.
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

type managedState struct {
	cfg         ManagedConfig
	present     bool
	readable    bool
	locked      map[string]bool
	keys        map[string]bool
	unknownKeys []string
}

func loadManagedUncached() managedState {
	path := ManagedConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return managedState{}
	}
	st := managedState{present: true}
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

// Source names where a resolved value came from, for `openbox doctor` and for
// the posture the control plane sees.
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

func resolveBoolWithSource(fieldName string, field func(DevConfig) *bool, def bool, envKey string) (bool, Source) {
	value, source := def, SourceDefault

	managed := cachedManaged()
	if managed.readable && managed.keys[fieldName] {
		if v := field(managed.cfg.DevConfig); v != nil {
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

// resolveBoolAt is resolveBoolWithSource generalized to a caller-supplied
// dev.json path instead of the ambient cachedUser() (which always reads
// DefaultConfigPath(), i.e. whichever tool BoundProvider() names). It applies
// the identical precedence -- default, then a readable managed layer (a
// locked key wins outright), then the file at path, then the env override --
// just scoped to the file the caller actually asked about. It is not
// memoized: callers use it a handful of times at process startup (one lookup
// per configured tool), never on a hot path.
func resolveBoolAt(fieldName string, field func(DevConfig) *bool, def bool, envKey string, path string) bool {
	value := def

	managed := cachedManaged()
	if managed.readable && managed.keys[fieldName] {
		if v := field(managed.cfg.DevConfig); v != nil {
			if managed.locked[fieldName] {
				return *v
			}
			value = *v
		}
	}

	if cfg, err := Load(path); err == nil {
		if keys := configKeysUncached(path); keys[fieldName] {
			if v := field(cfg); v != nil {
				value = *v
			}
		}
	}

	if v, ok := os.LookupEnv(envKey); ok {
		value = IsTruthy(v)
	}
	return value
}

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
			continue // documentation, not a setting
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
	// UnknownKeys names top-level keys in the managed file that match no setting.
	// They are ignored rather than invalidating the file (OD-RF-2), so they must
	// be reported or an org would not know its file has a typo.
	UnknownKeys []string
	// UnknownLocked names entries in `locked` that match no known setting.
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

func unknownManagedKeys(raw []byte) []string {
	var all map[string]json.RawMessage
	if json.Unmarshal(raw, &all) != nil {
		return nil
	}
	known := lockableFields()
	var out []string
	for k := range all {
		if strings.HasPrefix(k, "//") || k == "locked" || known[k] {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lockableFields derived from the DevConfig json tags so it cannot drift from
// the schema. Built once: the tags are a compile-time property of the type,
// and every reader below treats the map as read-only.
var lockableFields = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(DevConfig{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
})

// deprecatedFields are parsed so they can warn, never honoured. A lock on one
// is reported alongside a misspelled lock because the consequence is the same
// and doctor already words it correctly: these lock NOTHING. Without this an
// org locks `tier2`, is told the lock is active, and nothing anywhere says the
// key is inert.
var deprecatedFields = map[string]bool{
	"tier2":                   true,
	"tier2_timeout_ms":        true,
	"require_verified_bundle": true,
}

func unknownLocked(locked []string) []string {
	known := lockableFields()
	var out []string
	for _, f := range locked {
		if f = strings.TrimSpace(f); f != "" && (!known[f] || deprecatedFields[f]) {
			out = append(out, f)
		}
	}
	return out
}
