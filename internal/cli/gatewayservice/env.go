// Package gatewayservice writes the machine-level configuration the local
// gateway needs: the user-scope env block that points Claude Code at it, and
// the OS supervisor unit that keeps it running.
//   - Ownership is decided by what we recognise, not by exact-match.
//   - A plain re-run must never revert a deliberate opt-out.
package gatewayservice

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// EnvKey is the one variable this package owns in a settings env block. It is
// the whole owned set, deliberately.
const EnvKey = "ANTHROPIC_BASE_URL"

var ownedEnvKeys = map[string]bool{EnvKey: true}

// SettingsPath is the user-scope settings file the gateway config goes in.
func SettingsPath(homeDir string) string {
	return filepath.Join(homeDir, ".claude", "settings.json")
}

func priorEnvPath(homeDir string) string {
	return filepath.Join(homeDir, ".openbox", "gateway-prior-env.json")
}

// WriteEnv points the tool at the gateway, preserving everything it does not
// own. Returns the keys it replaced, so a caller can print what changed rather
// than claiming success silently.
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
			if s != "" && !ourGatewayURL(s) && !hasPriorEnv(homeDir) {
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

// RemoveEnvDetailed is the uninstall half. It removes only owned keys, and
// removes the env block itself only when nothing else is left in it; an org
// that put its own variables there must not lose them because OpenBox was
// uninstalled.
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
	if restored != "" {
		_ = os.Remove(priorEnvPath(homeDir))
	}
	return removed, restored, nil
}

type priorEnv struct {
	BaseURL string `json:"anthropic_base_url"`
}

func ourGatewayURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
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
	if err := writeFileAtomic(path, raw, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

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
// decide whether it has anything to say; the read side of the opt-out rule.
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

// readSettings decoding into a typed struct is how a writer silently deletes
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
	if err := writeFileAtomic(path, out, 0o644); err != nil {
		return fmt.Errorf("gatewayservice: writing %s: %w", path, err)
	}
	return nil
}
