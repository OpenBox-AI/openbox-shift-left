package devconfig

import (
	"os"
	"os/exec"
	"strings"
	"time"
)

// ProviderVersionTimeout bounds one `<tool> --version` probe.
const ProviderVersionTimeout = 2 * time.Second

// ProviderVersion is `<bin> --version`, trimmed, where bin is $binEnv when set
// and defaultBin otherwise; "" when the binary is not on PATH or the probe
// fails or outlives ProviderVersionTimeout.
func ProviderVersion(binEnv, defaultBin string) string {
	bin := os.Getenv(binEnv)
	if bin == "" {
		bin = defaultBin
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	out, err := RunWithTimeout(ProviderVersionTimeout, path, "--version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// RunWithTimeout runs name with args and returns its stdout, killing it and
// reporting os.ErrDeadlineExceeded once d elapses.
func RunWithTimeout(d time.Duration, name string, args ...string) ([]byte, error) {
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

// ProviderManaged reports whether a provider's own managed configuration is
// deployed and constrains the session: the first readable path decides
// ("true" when mandated accepts its contents, else "false"); a path that
// exists but cannot be read, with none readable, is "unknown"; no path at all
// is "false".
func ProviderManaged(paths []string, mandated func(raw []byte) bool) string {
	sawPath := false
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				sawPath = true // exists but unreadable by this user
			}
			continue
		}
		if mandated(raw) {
			return "true"
		}
		return "false" // managed, but not in a way we rely on
	}
	if sawPath {
		return "unknown"
	}
	return "false"
}
