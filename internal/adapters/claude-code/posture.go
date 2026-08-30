package claudecode

import (
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// adapterVersion identifies this adapter build in the recorded posture.
const adapterVersion = "claude-code/1"

func effectivePosture() devconfig.Posture {
	p := devconfig.EffectivePosture()
	p.Adapter = adapterVersion
	p.AdapterVersion = adapterVersion
	p.ProviderVersion = providerVersion()
	p.ProviderManaged = providerManaged()
	return p
}

func providerVersion() string {
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	out, err := runWithTimeout(providerVersionTimeout, path, "--version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

const providerVersionTimeout = 2 * time.Second

func runWithTimeout(d time.Duration, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
		return out, err
	case <-time.After(d):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, os.ErrDeadlineExceeded
	}
}

// providerManaged reports whether this provider's own managed configuration is
// deployed and names the OpenBox hook (E8-S8).
func providerManaged() string {
	paths := []string{"/etc/claude-code/managed-settings.json", "/Library/Application Support/ClaudeCode/managed-settings.json"}
	sawPath := false
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				sawPath = true // exists but unreadable by this user
			}
			continue
		}
		if strings.Contains(string(raw), "hook claude-code") {
			return "true"
		}
		return "false" // managed, but not by us
	}
	if sawPath {
		return "unknown"
	}
	return "false"
}
