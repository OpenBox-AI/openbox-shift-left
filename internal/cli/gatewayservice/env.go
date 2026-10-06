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

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
	before, err := readSettings(path)
	if err != nil {
		return nil, err
	}
	if err := checkEnvShape(before, path); err != nil {
		return nil, err
	}

	want := "http://" + addr
	if existing := gjson.GetBytes(before, envPath(EnvKey)); existing.Exists() {
		if s := existing.String(); s != want {
			replaced = append(replaced, fmt.Sprintf("%s: %v -> %s", EnvKey, s, want))
			if s != "" && !ourGatewayURL(s) && !hasPriorEnv(homeDir) {
				if err := savePriorEnv(homeDir, s); err != nil {
					return replaced, err
				}
			}
		}
	}
	after, err := sjson.SetBytes(before, envPath(EnvKey), want)
	if err != nil {
		return replaced, fmt.Errorf("gatewayservice: setting %s in %s: %w", EnvKey, path, err)
	}

	return replaced, writeSettings(path, finishSettings(indentIfNew(after, before), before))
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
	before, err := readSettings(path)
	if err != nil {
		return nil, "", err
	}
	if err := checkEnvShape(before, path); err != nil {
		return nil, "", err
	}
	if !gjson.GetBytes(before, "env").Exists() {
		return nil, "", nil
	}
	after := before
	prior := loadPriorEnv(homeDir)
	for key := range ownedEnvKeys {
		if !gjson.GetBytes(after, envPath(key)).Exists() {
			continue
		}
		if key == EnvKey && prior != "" {
			if after, err = sjson.SetBytes(after, envPath(key), prior); err != nil {
				return nil, "", fmt.Errorf("gatewayservice: restoring %s in %s: %w", key, path, err)
			}
			restored = prior
			continue
		}
		if after, err = sjson.DeleteBytes(after, envPath(key)); err != nil {
			return nil, "", fmt.Errorf("gatewayservice: removing %s from %s: %w", key, path, err)
		}
		removed = append(removed, key)
	}
	// An org that put its own variables in the env block must not lose it, and a
	// file that had none must not gain an empty one.
	if envKeyCount(after) == 0 {
		if after, err = sjson.DeleteBytes(after, "env"); err != nil {
			return nil, "", fmt.Errorf("gatewayservice: removing the empty env block from %s: %w", path, err)
		}
	}
	if len(removed) == 0 && restored == "" {
		return nil, "", nil
	}
	if err := writeSettings(path, finishSettings(after, before)); err != nil {
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
	raw, err := readSettings(SettingsPath(homeDir))
	if err != nil {
		return "", false
	}
	v := gjson.GetBytes(raw, envPath(EnvKey))
	if !v.Exists() {
		return "", false
	}
	return v.String(), true
}

// readSettings returns bytes: a typed struct deletes configuration it was never
// taught about, and map[string]any alphabetises and reindents the document.
// The validity check is explicit because sjson edits a malformed document
// without complaint. gjson's validator on purpose -- what decides a document is
// safe to edit must be what edits it -- with encoding/json only explaining.
func readSettings(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gatewayservice: reading %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	if !gjson.ValidBytes(raw) {
		if detail := json.Unmarshal(raw, new(any)); detail != nil {
			return raw, fmt.Errorf("gatewayservice: %s is not valid JSON, refusing to rewrite it: %w", path, detail)
		}
		return raw, fmt.Errorf("gatewayservice: %s is not valid JSON, refusing to rewrite it", path)
	}
	return raw, nil
}

// The helpers below duplicate internal/cli/activation's rather than sharing
// them: a package for thirty lines would be a layout boundary for no gain. The
// escape matters even though EnvKey holds no path syntax today, because gjson
// reads `.` as a separator and `*?#|@` as query syntax, and ownedEnvKeys is a
// set somebody will add to.
func envPath(key string) string {
	var b strings.Builder
	b.WriteString("env.")
	for _, r := range key {
		switch r {
		case '.', '*', '?', '#', '|', '@', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func checkEnvShape(raw []byte, path string) error {
	env := gjson.GetBytes(raw, "env")
	if !env.Exists() || env.Type == gjson.Null {
		return nil
	}
	if !env.IsObject() {
		return fmt.Errorf("gatewayservice: `env` in %s is not a JSON object; refusing to rewrite it", path)
	}
	return nil
}

func envKeyCount(raw []byte) int {
	n := 0
	gjson.GetBytes(raw, "env").ForEach(func(gjson.Result, gjson.Result) bool {
		n++
		return true
	})
	return n
}

// finishSettings ends the written bytes the way the read ones did; sjson's
// splice can consume a trailing newline.
func finishSettings(raw, before []byte) []byte {
	endedWithNewline := len(before) == 0 || before[len(before)-1] == '\n'
	has := len(raw) > 0 && raw[len(raw)-1] == '\n'
	switch {
	case endedWithNewline && !has:
		return append(raw, '\n')
	case !endedWithNewline && has:
		return raw[:len(raw)-1]
	}
	return raw
}

// indentIfNew reindents a document created from nothing: no developer bytes to
// preserve, and sjson splices compactly.
func indentIfNew(raw, before []byte) []byte {
	if len(before) != 0 {
		return raw
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return raw
	}
	return append(out, '\n')
}

func writeSettings(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("gatewayservice: creating %s: %w", filepath.Dir(path), err)
	}
	if err := writeFileAtomic(path, raw, 0o644); err != nil {
		return fmt.Errorf("gatewayservice: writing %s: %w", path, err)
	}
	return nil
}
