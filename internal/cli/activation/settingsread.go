package activation

import (
	"fmt"
	"path/filepath"
)

// SettingsRead is what reading the tool's settings produced. "No lane is routed"
// and "could not be read" were indistinguishable, so a daemon reported a
// configuration choice while reading a relative path resolved against /.
type SettingsRead struct {
	Env         map[string]string
	Path        string
	NotAbsolute bool
	Missing     bool
	Err         error
}

func (r SettingsRead) Problem() string {
	switch {
	case r.Path == "":
		return "the tool's settings path could not be resolved at all, so no lane can be elected"
	case r.NotAbsolute:
		return fmt.Sprintf("the tool's settings path %q is not absolute, so it resolves against "+
			"whatever working directory this process has; a daemon's is not the developer's home", r.Path)
	case r.Err != nil:
		return fmt.Sprintf("the tool's settings at %s could not be read: %v", r.Path, r.Err)
	}
	return ""
}

func (r SettingsRead) Readable() bool { return r.Problem() == "" }

func ReadSettingsEnv(path string) SettingsRead {
	out := SettingsRead{Env: map[string]string{}, Path: path}
	if path == "" {
		return out
	}
	if !filepath.IsAbs(path) {
		out.NotAbsolute = true
		return out
	}
	raw, err := readSettings(path)
	if err != nil {
		out.Err = err
		return out
	}
	if raw == nil {
		out.Missing = true
		return out
	}
	// From the bytes already in hand: CurrentEnv(path) would re-read and
	// re-validate the same file.
	out.Env = envFromRaw(raw)
	return out
}
