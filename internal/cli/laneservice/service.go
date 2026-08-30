package laneservice

import (
	"fmt"
	"strings"

	"github.com/kardianos/service"
)

// What IT does NOT OWN, deliberately:

// serviceName is the library's `Name`, and it is PER-platform on purpose.
func (s Spec) serviceName(goos string) (string, error) {
	switch goos {
	case "darwin":
		return s.Label, nil
	case "linux":
		return s.SystemdName, nil
	default:
		return "", s.UnsupportedPlatform(goos)
	}
}

type controlOnly struct{}

func (controlOnly) Start(service.Service) error { return nil }
func (controlOnly) Stop(service.Service) error  { return nil }

// New returns the supervisor handle for this lane on this platform.
func (s Spec) New(goos, homeDir, binPath string) (service.Service, error) {
	name, err := s.serviceName(goos)
	if err != nil {
		return nil, err
	}
	argv := s.Argv(binPath)
	cfg := &service.Config{
		Name:        name,
		DisplayName: s.DisplayName,
		Description: s.ServiceDescription,
		Executable:  binPath,
		Arguments:   argv[1:],
		Option: service.KeyValue{
			"UserService":   true,
			"LaunchdConfig": s.LaunchdPlist(homeDir, binPath),
			"SystemdScript": s.SystemdUnit(binPath),
			"KeepAlive":     true,
			"RunAtLoad":     true,
			"Restart":       "always",
		},
	}
	return service.New(controlOnly{}, cfg)
}

// Install writes the unit through the library.
func (s Spec) Install(goos, homeDir, binPath string) error {
	svc, err := s.New(goos, homeDir, binPath)
	if err != nil {
		return err
	}
	if err := svc.Install(); err != nil {
		return fmt.Errorf("laneservice: installing the %s unit for %s: %w", goos, s.Label, err)
	}
	return nil
}

// Reinstall writes the unit, replacing one that is already there. The file
// write this replaced used os.WriteFile, which overwrites, and that difference
// is load-bearing rather than cosmetic: re-running the install is how a unit
// written by an older binary gets refreshed.
func (s Spec) Reinstall(goos, homeDir, binPath string) error {
	if err := s.Uninstall(goos, homeDir); err != nil {
		return err
	}
	return s.Install(goos, homeDir, binPath)
}

// Uninstall removes the unit through the library. Absent is success: removal
// must be safe to run on a machine that never had one.
func (s Spec) Uninstall(goos, homeDir string) error {
	svc, err := s.New(goos, homeDir, "")
	if err != nil {
		return err
	}
	if err := svc.Uninstall(); err != nil {
		if IsNotInstalled(err) {
			return nil
		}
		return fmt.Errorf("laneservice: removing the %s unit for %s: %w", goos, s.Label, err)
	}
	return nil
}

// IsNotInstalled reports whether an Uninstall error means there was nothing to
// remove. Matched on the message because the library returns a bare formatted
// error for a missing unit rather than something wrapping fs.ErrNotExist, so
// errors.Is cannot see it.
func IsNotInstalled(err error) bool {
	if err == nil {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, sub := range []string{"no such file", "not installed", "does not exist"} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}
