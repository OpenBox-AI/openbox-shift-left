package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/tidwall/gjson"
)

// envManagedSettingsPath relocates the Claude Code managed-settings file.
// Primarily for tests: the real path is under /Library or /etc, which a test
// must neither read from the developer's machine nor write to.
const envManagedSettingsPath = "OPENBOX_CLAUDE_MANAGED_SETTINGS"

// This file answers the question absence cannot: on an org-managed machine the
// hooks can be installed, correct, and never run. That is the failure mode
// worth the most attention here, because it is silent and total -- `init`
// prints success, the machine is ungoverned, and a doctor that read none of
// these keys agreed it was fine.
//
// Three keys do it, and they do NOT share a scope, which is why one lookup
// cannot answer for all three:
//
//   - allowManagedHooksOnly       managed only. Blocks user, project, local and
//     plugin hooks; only hooks the managed policy declares survive.
//   - strictPluginOnlyCustomization  managed only. The reference defines an
//     object whose .hooks locks hooks to plugin and managed sources; the
//     template this repo ships writes a bare boolean, and the reference does
//     list the bare parent key as settable. Both shapes are honoured, because
//     handling one and not the other reports a blocked machine as governed.
//   - disableAllHooks             settable at managed, user, project and local.
//     Resolved by PRECEDENCE, not presence: a project's false overrides a
//     user's true. A reader that reported "blocked" on finding the key
//     anywhere would be wrong on exactly the machine where somebody turned it
//     back on.
//
// Managed always wins. Only a managed-level disableAllHooks can disable
// managed hooks, so where the org's own policy installs hooks and a user file
// disables all, ours are off and theirs are running -- "nothing is governed"
// would be the wrong answer and the dangerous one.

// hookBlockState is the effective outcome, not a list of keys found.
type hookBlockState struct {
	// blocked reports that THIS install's user-level hooks will not fire.
	blocked bool
	// governedElsewhere reports that the machine is nonetheless governed,
	// because the managed policy declares OpenBox's hooks itself. Separate from
	// blocked because the two need opposite words: one is a gap to chase, the
	// other is a mandated fleet working as designed.
	governedElsewhere bool
	summary           string
	detail            []string
}

// claudeManagedSettingsPath is derived here rather than through
// internal/cli/managed, which is CLI-lifecycle code with a different
// lifetime; doctor should not stop being able to read this file because that
// package goes away.
func claudeManagedSettingsPath() string {
	if p := os.Getenv(envManagedSettingsPath); p != "" {
		return p
	}
	switch runtime.GOOS {
	case "linux":
		return filepath.Join("/etc", "claude-code", "managed-settings.json")
	case "darwin":
		return filepath.Join("/Library", "Application Support", "ClaudeCode", "managed-settings.json")
	case "windows":
		// The current location. The legacy C:\ProgramData\ClaudeCode path is no
		// longer read by the tool, so reporting from it would describe a file
		// that has no effect.
		return filepath.Join(`C:\Program Files\ClaudeCode`, "managed-settings.json")
	default:
		return ""
	}
}

// managedDropIns are the managed-settings.d/*.json files that merge with the
// main managed file. A reader that consulted only the main file would report
// "nothing disables hooks" on a fleet whose lock arrives in a drop-in.
func managedDropIns() []string {
	main := claudeManagedSettingsPath()
	if main == "" {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(main), "managed-settings.d", "*.json"))
	if err != nil {
		return nil
	}
	sort.Strings(paths)
	return paths
}

// settingsLevel is one file plus how it ranks. Later in the slice wins, and
// managed wins over everything.
type settingsLevel struct {
	label string
	path  string
}

// hookSettingsLevels are the four files, in ascending precedence.
func hookSettingsLevels() []settingsLevel {
	levels := []settingsLevel{{"user", filepath.Join(homeDirForSettings(), ".claude", "settings.json")}}
	if wd, err := os.Getwd(); err == nil {
		levels = append(levels,
			settingsLevel{"project", filepath.Join(wd, ".claude", "settings.json")},
			settingsLevel{"local", filepath.Join(wd, ".claude", "settings.local.json")},
		)
	}
	if p := claudeManagedSettingsPath(); p != "" {
		levels = append(levels, settingsLevel{"managed", p})
	}
	return levels
}

func homeDirForSettings() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// resolveHookBlock reports whether OpenBox's own hooks can run on this machine.
func resolveHookBlock() hookBlockState {
	managedPath := claudeManagedSettingsPath()
	managed, managedErr := readSettings(managedPath)

	state := hookBlockState{}
	if managedErr != nil {
		state.detail = append(state.detail, fmt.Sprintf(
			"%s exists but could not be read (%v), so whether it blocks hooks is UNKNOWN.", managedPath, managedErr))
	}

	// Managed-only keys first: either one stops our user-level hooks running,
	// whatever any other file says.
	for _, m := range managedLayers(managedPath, managed) {
		if key, ok := hookLockKey(m.raw); ok {
			state.blocked = true
			// The lock does NOT mean the machine is ungoverned. OpenBox's own
			// managed template inlines these hooks and sets this key: on that
			// fleet -- the one the key exists for -- the managed copy is
			// governing and only our user-level duplicate is inert. Saying
			// "nothing is governed" there sends somebody hunting a gap that is
			// not there.
			if managedHooksAreOurs(managedPath, managedLayers(managedPath, managed)) {
				state.governedElsewhere = true
				state.summary = "not this install's copy, and it does not need to be: " + key +
					" is set by managed policy and that policy installs OpenBox's own hooks. " +
					"This machine IS governed, by the managed copy."
				state.detail = append(state.detail,
					fmt.Sprintf("both are declared in %s. The user-level copy this command wrote is", m.path),
					"redundant and will not fire, which is the intended shape for a mandated fleet.")
				return state
			}
			state.summary = "NO; " + key + " is set by managed policy, so only hooks that policy " +
				"declares run. OpenBox's are user-level, so they never fire."
			state.detail = append(state.detail,
				fmt.Sprintf("set in %s. Only your administrator can change it; deploy OpenBox's hooks", m.path),
				"through managed settings instead (see deployments/managed/) if this machine must stay locked.")
			return state
		}
	}

	// disableAllHooks, resolved across every level by precedence.
	decided, decidedBy, found := resolveDisableAllHooks()
	if !found || !decided {
		state.summary = "yes; nothing in this machine's settings files disables hooks."
		if managedErr != nil {
			state.summary = "probably; nothing readable disables hooks, but see the managed file below."
		}
		// Said plainly, because the answer is only as good as what was read: a
		// policy delivered by MDM or from the console outranks the file and is
		// not visible here.
		state.detail = append(state.detail,
			"Read from the settings files only. A policy delivered by MDM or from the console"+
				" outranks them and is not inspected; the tool's own `/status` is authoritative.")
		return state
	}

	state.blocked = true
	state.summary = fmt.Sprintf("NO; disableAllHooks resolves to true (set in %s).", decidedBy.path)
	if decidedBy.label == "managed" {
		state.detail = append(state.detail,
			"Set at the managed level, so a user, project or local file cannot override it.")
		return state
	}
	switch decidedBy.label {
	case "project", "local":
		state.detail = append(state.detail,
			fmt.Sprintf("Set at the %s level, so it disables hooks for sessions started in THIS directory.", decidedBy.label),
			"The user-wide install still governs sessions started anywhere else.")
	default:
		state.detail = append(state.detail,
			fmt.Sprintf("Set at the %s level. A higher-precedence file setting it to false re-enables hooks.", decidedBy.label))
	}
	// A non-managed disableAllHooks cannot disable managed hooks, so this
	// machine may still be governed -- and where those managed hooks are
	// OpenBox's own, it is governed BY US, which is the opposite of what a bare
	// "nothing is governed" would say.
	if managed != nil && gjson.GetBytes(managed, "hooks").Exists() {
		if managedHooksAreOurs(managedPath, managedLayers(managedPath, managed)) {
			state.governedElsewhere = true
			state.detail = append(state.detail,
				fmt.Sprintf("This machine is STILL GOVERNED: %s declares OpenBox's own hooks, and a", managedPath),
				"non-managed disableAllHooks cannot turn managed hooks off. Only the user-level copy is off.")
			return state
		}
		state.detail = append(state.detail,
			fmt.Sprintf("This machine is still governed by managed policy: %s installs its own hooks, and a", managedPath),
			"non-managed disableAllHooks cannot turn those off. What is off is OpenBox's half.")
	}
	return state
}

// resolveDisableAllHooks returns the value left after precedence applies, and
// which file decided it.
func resolveDisableAllHooks() (value bool, decidedBy settingsLevel, found bool) {
	for _, level := range hookSettingsLevels() {
		raw, err := readSettings(level.path)
		if err != nil || raw == nil {
			continue
		}
		key := gjson.GetBytes(raw, "disableAllHooks")
		if !key.Exists() || (key.Type != gjson.True && key.Type != gjson.False) {
			continue
		}
		value, decidedBy, found = key.Bool(), level, true
	}
	return value, decidedBy, found
}

// managedLayer is one managed source: the main file, or a drop-in that merges
// with it.
type managedLayer struct {
	path string
	raw  []byte
}

func managedLayers(mainPath string, main []byte) []managedLayer {
	layers := make([]managedLayer, 0, 2)
	if main != nil {
		layers = append(layers, managedLayer{path: mainPath, raw: main})
	}
	for _, p := range managedDropIns() {
		if raw, err := readSettings(p); err == nil && raw != nil {
			layers = append(layers, managedLayer{path: p, raw: raw})
		}
	}
	return layers
}

// hookLockKey reports which managed-only key, if any, locks hooks away from
// user and project sources.
func hookLockKey(raw []byte) (string, bool) {
	// Present-and-not-false, rather than Bool(). A malformed value resolves to
	// true in the tool, so reading "yes" or 1 as false would report a locked
	// machine as governed -- the one direction that must never happen here.
	if k := gjson.GetBytes(raw, "allowManagedHooksOnly"); k.Exists() && k.Type != gjson.False {
		return "allowManagedHooksOnly", true
	}
	strict := gjson.GetBytes(raw, "strictPluginOnlyCustomization")
	switch {
	case strict.Type == gjson.True:
		// The bare parent key locks all four categories, and it is the shape
		// this repo's own template ships.
		return "strictPluginOnlyCustomization", true
	case strict.Get("hooks").Bool():
		return "strictPluginOnlyCustomization.hooks", true
	case strict.IsArray():
		// The array form names the categories it locks.
		for _, v := range strict.Array() {
			if v.String() == "hooks" {
				return "strictPluginOnlyCustomization", true
			}
		}
	}
	return "", false
}

// managedHooksAreOurs reports whether the managed policy installs OpenBox's own
// hooks, in ANY of its layers. The tool merges the main file with every
// managed-settings.d drop-in, so hooks declared in one and a lock declared in
// another are the same policy -- and auditing only the main file reported a
// governed fleet as ungoverned.
//
// Classified through the same registry the installer and doctor use, so the
// three cannot hold different opinions about what "ours" means.
func managedHooksAreOurs(mainPath string, layers []managedLayer) bool {
	for _, m := range layers {
		if audit, err := providers.AuditHooks(m.path); err == nil && len(audit.Engines) > 0 {
			return true
		}
	}
	// The main path may not be among the layers when it could not be read.
	audit, err := providers.AuditHooks(mainPath)
	return err == nil && len(audit.Engines) > 0
}

// readSettings returns nil for an absent file and an error only for one that
// exists and cannot be used, so a caller can tell "not managed" from "managed
// and unreadable".
func readSettings(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !gjson.ValidBytes(raw) {
		return nil, fmt.Errorf("not valid JSON")
	}
	return raw, nil
}
