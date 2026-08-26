// Package gatewayservice writes the machine-level configuration the local
// gateway needs: the user-scope env block that points Claude Code at it, and the
// OS supervisor unit that keeps it running.
//
// Two lessons from this repo's own history shape every function here, and both
// are about WRITES rather than reads:
//
//   - Ownership is decided by what we recognise, not by exact-match. `init` once
//     matched its own hook entries by exact command string, so an entry written
//     under a different HOME read as FOREIGN, was preserved, and both engines
//     fired — every governed call stored twice. Foreign keys are preserved here;
//     only keys we own are replaced.
//   - A plain re-run must never revert a deliberate opt-out. `Enforce` had to
//     become a `*bool` for exactly this: because the flag defaulted to true, its
//     value alone could not distinguish "asked for it" from "said nothing", and
//     every plain `init` silently rewrote a deliberate `false`. Fifteen green
//     tests missed it because each ran `init` once. The rule that came out of it
//     is the one this package follows: check reads and writes separately, and
//     test the SECOND invocation.
package gatewayservice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// EnvKey is the one variable this package owns in a settings env block.
//
// It is the whole owned set, deliberately. Pass-through auth deleted the need for
// ANTHROPIC_AUTH_TOKEN, and `forceLoginMethod` dropped to optional org-side
// hardening — less to write is less to revert wrongly.
const EnvKey = "ANTHROPIC_BASE_URL"

// ownedEnvKeys is what a write may replace or remove. Anything else in the env
// block belongs to the developer or their org and is preserved untouched.
var ownedEnvKeys = map[string]bool{EnvKey: true}

// SettingsPath is the user-scope settings file the gateway config goes in.
//
// User scope, not project scope, and that is the ADR-0016 amendment rather than a
// preference: ANTHROPIC_BASE_URL is read from managed settings and
// ~/.claude/settings.json, and background agents need settings rather than shell
// exports. A project-scoped write would report success while governing nothing —
// the same failure mode ADR-0016 fixed for the default install scope.
func SettingsPath(homeDir string) string {
	return filepath.Join(homeDir, ".claude", "settings.json")
}

// priorEnvPath is where the value WriteEnv displaced is remembered, so RemoveEnv
// can put it back.
//
// Under homeDir rather than via devconfig.Home() on purpose: every function in
// this package takes homeDir explicitly so a test can point the whole gateway
// step at a temp dir, and a helper that read the real HOME instead would make the
// restore untestable and, worse, write real state during a test run.
func priorEnvPath(homeDir string) string {
	return filepath.Join(homeDir, ".openbox", "gateway-prior-env.json")
}

// WriteEnv points the tool at the gateway, preserving everything it does not own.
//
// Returns the keys it replaced, so a caller can print what changed rather than
// claiming success silently.
//
// A displaced FOREIGN value is remembered, not just reported. The package's rule
// was stated as key-ownership — "foreign keys are preserved; only keys we own are
// replaced" — and that missed the case where the KEY is ours and the VALUE is
// theirs: an org pointing Claude Code at its own LiteLLM/Bedrock relay through
// ANTHROPIC_BASE_URL, which is exactly the setup docs/gateway-mdm-recipe.md
// targets. Install printed the old URL and overwrote it; uninstall deleted the
// key. After that round trip the org's relay existed nowhere, every model call
// went straight to the provider — silently bypassing the org's own egress point —
// and `openbox doctor` reported "not set", which reads as clean rather than as
// damage.
func WriteEnv(homeDir, addr string) (replaced []string, err error) {
	path := SettingsPath(homeDir)
	settings, err := readSettings(path)
	if err != nil {
		return nil, err
	}

	env, _ := settings["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}

	want := "http://" + addr
	if existing, present := env[EnvKey]; present {
		if s, _ := existing.(string); s != want {
			replaced = append(replaced, fmt.Sprintf("%s: %v -> %s", EnvKey, existing, want))
			// Remember it, unless a record already exists: a second install must
			// not overwrite the ORIGINAL foreign value with our own gateway URL
			// from the first one. First writer wins, which is the only order that
			// preserves what was there before OpenBox.
			if s != "" && !hasPriorEnv(homeDir) {
				if err := savePriorEnv(homeDir, s); err != nil {
					return replaced, err
				}
			}
		}
	}
	env[EnvKey] = want
	settings["env"] = env

	return replaced, writeSettings(path, settings)
}

// RemoveEnv is the uninstall half. It removes ONLY owned keys, and removes the
// env block itself only when nothing else is left in it — an org that put its own
// variables there must not lose them because OpenBox was uninstalled.
//
// A remembered prior value is RESTORED rather than deleted, which is the other
// half of WriteEnv's record. Returns the keys it removed and the ones it restored
// so a caller can say which happened; a restore is not a removal and reporting it
// as one would tell an operator their machine is unconfigured when it is back to
// what the org configured.
func RemoveEnv(homeDir string) (removed []string, err error) {
	removed, _, err = removeEnv(homeDir)
	return removed, err
}

// RemoveEnvDetailed is RemoveEnv with the restore reported separately.
func RemoveEnvDetailed(homeDir string) (removed []string, restored string, err error) {
	return removeEnv(homeDir)
}

func removeEnv(homeDir string) (removed []string, restored string, err error) {
	path := SettingsPath(homeDir)
	settings, err := readSettings(path)
	if err != nil {
		return nil, "", err
	}
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		return nil, "", nil
	}
	prior := loadPriorEnv(homeDir)
	for key := range ownedEnvKeys {
		if _, present := env[key]; !present {
			continue
		}
		if key == EnvKey && prior != "" {
			env[key] = prior
			restored = prior
			continue
		}
		delete(env, key)
		removed = append(removed, key)
	}
	if len(env) == 0 {
		delete(settings, "env")
	} else {
		settings["env"] = env
	}
	if len(removed) == 0 && restored == "" {
		return nil, "", nil
	}
	if err := writeSettings(path, settings); err != nil {
		return nil, "", err
	}
	// Only after the settings write succeeded: dropping the record first would
	// lose the org's value if the write then failed.
	if restored != "" {
		_ = os.Remove(priorEnvPath(homeDir))
	}
	return removed, restored, nil
}

// priorEnv is the on-disk record. A struct rather than a bare string so a future
// second remembered key does not need a format change.
type priorEnv struct {
	BaseURL string `json:"anthropic_base_url"`
}

func hasPriorEnv(homeDir string) bool {
	_, err := os.Stat(priorEnvPath(homeDir))
	return err == nil
}

func savePriorEnv(homeDir, value string) error {
	path := priorEnvPath(homeDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.Marshal(priorEnv{BaseURL: value})
	if err != nil {
		return err
	}
	return writeFileAtomic(path, raw, 0o600)
}

// loadPriorEnv returns "" for every failure. A missing or unreadable record means
// "there was nothing to restore", which is the same outcome as before this
// existed — never a reason to fail an uninstall.
func loadPriorEnv(homeDir string) string {
	raw, err := os.ReadFile(priorEnvPath(homeDir))
	if err != nil {
		return ""
	}
	var p priorEnv
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.BaseURL
}

// CurrentEnv reports what the settings file currently sets, so a caller can
// decide whether it has anything to say — the read side of the opt-out rule.
func CurrentEnv(homeDir string) (value string, present bool) {
	settings, err := readSettings(SettingsPath(homeDir))
	if err != nil {
		return "", false
	}
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		return "", false
	}
	v, ok := env[EnvKey]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// readSettings loads the file as a generic map so unknown keys survive a
// round-trip. Decoding into a typed struct is how a writer silently deletes
// configuration it was never taught about.
func readSettings(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gatewayservice: reading %s: %w", path, err)
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		// Refuse rather than overwrite. A settings file we cannot parse is a
		// file we cannot safely rewrite, and clobbering it would destroy
		// configuration the developer did not ask us to touch.
		return nil, fmt.Errorf("gatewayservice: %s is not valid JSON, refusing to rewrite it: %w", path, err)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	return settings, nil
}

func writeSettings(path string, settings map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("gatewayservice: creating %s: %w", filepath.Dir(path), err)
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("gatewayservice: encoding settings: %w", err)
	}
	out = append(out, '\n')
	// ATOMIC: temp file then rename, not os.WriteFile.
	//
	// A crash or a full disk part-way through a plain WriteFile truncates
	// ~/.claude/settings.json to invalid JSON — and readSettings' own "not valid
	// JSON, refusing to rewrite it" guard then blocks every later repair, so the
	// developer is left with a broken tool config that this command will not fix.
	// A rename is the difference between "the old file or the new file" and "an
	// arbitrary prefix of the new file".
	//
	// This does NOT make concurrent writers safe. Two inits, or an init racing the
	// tool's own writer, still read-modify-write the same file and the last rename
	// wins with no error either way. Atomicity bounds the damage to a lost update
	// rather than a corrupt file; a lock is what would close the race, and this
	// package deliberately does not claim to have one.
	//
	// 0644 because the tool reads it and it is the DEVELOPER's config. Its
	// permissions are not an assurance boundary — doctor reports ownership as the
	// tier signal precisely because a user-owned file is user-changeable.
	if err := writeFileAtomic(path, out, 0o644); err != nil {
		return fmt.Errorf("gatewayservice: writing %s: %w", path, err)
	}
	return nil
}

// writeFileAtomic writes via a temp file in the same directory, then renames.
// Same approach as the adapter's settings writer; kept local rather than exported
// across a module boundary for one caller.
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
	// CreateTemp makes it 0600; match the intended perm rather than silently
	// tightening a file other tools read.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
