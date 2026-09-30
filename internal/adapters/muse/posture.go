package muse

import (
	"os/exec"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// adapterVersion identifies this adapter build in the recorded posture.
const adapterVersion = "muse/1"

func effectivePosture() devconfig.Posture {
	p := devconfig.EffectivePosture()
	p.Adapter = adapterVersion
	p.AdapterVersion = adapterVersion
	p.ProviderVersion = providerVersion()
	// Where Muse's managed policy lives on macOS and Linux is undocumented, so
	// whether it constrains this session cannot be read; unknown, never false.
	p.ProviderManaged = "unknown"
	return p
}

const providerVersionTimeout = 2 * time.Second

// providerVersion is `muse --version`, bounded, or "" when muse is not on PATH.
func providerVersion() string {
	path, err := exec.LookPath("muse")
	if err != nil {
		return ""
	}
	done := make(chan []byte, 1)
	cmd := exec.Command(path, "--version")
	go func() {
		out, err := cmd.Output()
		if err != nil {
			out = nil
		}
		done <- out
	}()
	select {
	case out := <-done:
		return strings.TrimSpace(string(out))
	case <-time.After(providerVersionTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return ""
	}
}
