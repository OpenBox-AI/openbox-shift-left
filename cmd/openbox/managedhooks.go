package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

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
	blocked bool
	summary string
	detail  []string
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
		return filepath.Join(`C:\ProgramData\ClaudeCode`, "managed-settings.json")
	default:
		return ""
	}
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

	// Managed-only keys first: either one means our user-level hooks never run,
	// whatever any other file says.
	if managed != nil {
		if gjson.GetBytes(managed, "allowManagedHooksOnly").Bool() {
			state.blocked = true
			state.summary = "NO; allowManagedHooksOnly is set by managed policy, so only hooks that policy " +
				"declares run. OpenBox's are user-level, so they never fire."
			state.detail = append(state.detail,
				fmt.Sprintf("set in %s. Only your administrator can change it; deploy OpenBox's hooks through", managedPath),
				"managed settings instead (see deployments/managed/) if this machine must stay locked.")
			return state
		}
		strict := gjson.GetBytes(managed, "strictPluginOnlyCustomization")
		// Object form names a category; the bare boolean is the parent key. Both
		// are treated as locking hooks, because the shape our own template ships
		// is the bare one.
		if strict.Get("hooks").Bool() || (strict.Type == gjson.True) {
			state.blocked = true
			state.summary = "NO; strictPluginOnlyCustomization locks hooks to plugin and managed sources, " +
				"so OpenBox's user-level hooks never fire."
			state.detail = append(state.detail,
				fmt.Sprintf("set in %s. Only your administrator can change it.", managedPath))
			return state
		}
	}

	// disableAllHooks, resolved across every level by precedence.
	decided, decidedBy, found := resolveDisableAllHooks()
	if !found || !decided {
		state.summary = "yes; nothing on this machine disables hooks."
		if managedErr != nil {
			state.summary = "probably; nothing readable disables hooks, but see the managed file below."
		}
		return state
	}

	state.blocked = true
	state.summary = fmt.Sprintf("NO; disableAllHooks resolves to true (set in %s).", decidedBy.path)
	if decidedBy.label == "managed" {
		state.detail = append(state.detail,
			"Set at the managed level, so a user, project or local file cannot override it.")
		return state
	}
	state.detail = append(state.detail,
		fmt.Sprintf("Set at the %s level. A higher-precedence file setting it to false re-enables hooks.", decidedBy.label))
	// The org's own hooks are unaffected by a non-managed disableAllHooks, so
	// this machine may still be governed -- by them, not by us.
	if managed != nil && gjson.GetBytes(managed, "hooks").Exists() {
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
