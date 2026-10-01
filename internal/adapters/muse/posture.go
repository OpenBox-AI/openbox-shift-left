package muse

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// adapterVersion identifies this adapter build in the recorded posture.
const adapterVersion = "muse/1"

// effectivePosture is the posture a run records. cacheDir is where the
// provider version is remembered between hooks ("" disables the cache).
func effectivePosture(cacheDir string) devconfig.Posture {
	p := devconfig.EffectivePosture()
	p.Adapter = adapterVersion
	p.AdapterVersion = adapterVersion
	p.ProviderVersion = cachedProviderVersion(cacheDir, time.Now())
	// Where Muse's managed policy lives on macOS and Linux is undocumented, so
	// whether it constrains this session cannot be read; unknown, never false.
	p.ProviderManaged = "unknown"
	return p
}

// providerVersionTimeout bounds `muse --version`; a var so a test can shorten it.
var providerVersionTimeout = 2 * time.Second

// providerVersionTTL is how long a read version is reused. A hook that opens a
// run, a gated one included, would otherwise pay for a subprocess every time;
// an upgraded Muse is picked up within the hour.
const providerVersionTTL = time.Hour

const providerVersionCacheFile = "provider-version.json"

type versionCache struct {
	Version   string `json:"version"`
	CheckedAt int64  `json:"checked_at"`
}

// cachedProviderVersion is the version read within providerVersionTTL, else a
// fresh `muse --version`, remembered when it produced one. A cache that cannot
// be read or written is a miss, never an error.
func cachedProviderVersion(dir string, now time.Time) string {
	file := ""
	if dir != "" {
		file = filepath.Join(dir, providerVersionCacheFile)
		if raw, err := os.ReadFile(file); err == nil {
			var c versionCache
			if json.Unmarshal(raw, &c) == nil && c.Version != "" {
				if age := now.Sub(time.Unix(0, c.CheckedAt)); age >= 0 && age < providerVersionTTL {
					return c.Version
				}
			}
		}
	}
	v := providerVersion()
	if v != "" && file != "" {
		if data, err := json.Marshal(versionCache{Version: v, CheckedAt: now.UnixNano()}); err == nil && os.MkdirAll(dir, 0o700) == nil {
			_ = hookflow.AtomicWriteFile(file, data, 0o600)
		}
	}
	return v
}

// providerVersion is `muse --version`, bounded, or "" when muse is not on PATH.
func providerVersion() string {
	path, err := exec.LookPath("muse")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// A killed muse whose child still holds the pipe must not outlast the bound.
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
