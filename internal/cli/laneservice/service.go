package laneservice

import (
	"fmt"
	"strings"

	"github.com/kardianos/service"
)

// service.go adopts kardianos/service for the supervisor lifecycle (D-OSS-3):
// install and uninstall.
//
// WHAT THE LIBRARY OWNS: the unit file write and removal.
//
// WHAT IT DOES NOT OWN, deliberately:
//
//   - **the unit CONTENT.** Both bodies are still rendered by unit.go and handed
//     to the library as its own template overrides. Its generated launchd plist
//     cannot express what this repo requires: `LogDirectory` is settable but the
//     filenames are hardcoded `<Name>.out.log` / `<Name>.err.log`, i.e. TWO files
//     with derived names, where each lane needs ONE tailable
//     `~/.openbox/<lane>.log`. Supplying the whole template keeps that, and keeps
//     ExitTimeOut matched to --shutdown-grace.
//   - **the install ORDERING, the readiness proof, the rollback, or the env
//     activation record.** Those live in the caller and are the safety property:
//     unit → start → PROVE listening → env, and any failure after the install
//     removes the unit again.
//   - **start and stop.** kardianos runs `launchctl load`/`unload`; this repo
//     runs `launchctl bootstrap gui/<uid>` with `load -w` as the fallback, and
//     `bootout` before `unload`. `bootstrap` is the modern spelling and the
//     reason the install works on current macOS. Adopting a regression to satisfy
//     a decision's letter would be the wrong trade, so the cascade stays with the
//     caller.
//
// ONE CONSEQUENCE, accepted by owner ruling rather than discovered later: on
// darwin the library derives the home directory from `user.Current()` and only
// falls back to `$HOME` if that errors, and no Option overrides the path. So
// `Install()` writes to the REAL ~/Library/LaunchAgents regardless of a test's
// t.Setenv("HOME", …) — which is why every automated test of an install path
// goes through a seam rather than through this file. systemd is unaffected: it
// uses os.UserHomeDir(), which does respect $HOME.

// serviceName is the library's `Name`, and it is PER-PLATFORM on purpose.
//
// The library derives the unit's filename from it — `<Name>.plist` on darwin,
// `<Name>.service` on systemd — and this repo's two platforms do not share a
// naming convention: the launchd label is reverse-DNS, the systemd unit is
// hyphenated. One shared value would silently rename one of them, and UnitPath,
// `openbox doctor` and the re-install detection all read the path it produces.
func (s Spec) serviceName(goos string) (string, error) {
	switch goos {
	case "darwin":
		return s.Label, nil
	case "linux":
		// SystemdName carries no ".service" suffix; the library appends it.
		return s.SystemdName, nil
	default:
		return "", s.UnsupportedPlatform(goos)
	}
}

// controlOnly satisfies service.Interface without running anything.
//
// No lane runs through the library's `Run()` loop: the unit executes the binary
// in the foreground and the OS supervisor owns restart, which is the availability
// story unit.go documents. `service.New` requires an Interface even when only
// install/uninstall are used, so this is the null implementation. If it were ever
// reached, that would mean a lane had been restructured to run under the
// library's loop — a different design, not a bug to patch here.
type controlOnly struct{}

func (controlOnly) Start(service.Service) error { return nil }
func (controlOnly) Stop(service.Service) error  { return nil }

// New returns the supervisor handle for this lane on this platform.
//
// homeDir still selects the LOG path inside the rendered unit. It does not
// select where the unit itself lands — see the consequence note above.
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
		// argv[0] is the binary, which Executable already carries.
		Arguments: argv[1:],
		Option: service.KeyValue{
			// A per-developer agent, never a system daemon: the base tier is
			// user-owned by definition and that decision puts the install in
			// the developer's hands, so needing root would be the wrong
			// shape.
			"UserService": true,
			// Both bodies are ours. They contain no template actions, so the
			// library's text/template render is an identity transform — pinned by
			// TestSuppliedTemplatesSurviveRendering so a future edit that
			// introduces `{{` cannot silently corrupt a unit.
			"LaunchdConfig": s.LaunchdPlist(homeDir, binPath),
			"SystemdScript": s.SystemdUnit(binPath),
			// Set even though the supplied templates already carry them: if a
			// template is ever dropped, the generated fallback should still
			// restart a crashed daemon and start one at login.
			"KeepAlive": true,
			"RunAtLoad": true,
			"Restart":   "always",
		},
	}
	return service.New(controlOnly{}, cfg)
}

// Install writes the unit through the library. It does NOT start it.
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

// Reinstall writes the unit, replacing one that is already there.
//
// The library's Install() REFUSES when the unit file exists — both platforms
// os.Stat and return "Init already exists". The file write this replaced used
// os.WriteFile, which overwrites, and that difference is load-bearing rather
// than cosmetic: re-running the install is how a unit written by an OLDER binary
// gets refreshed. This repo already shipped and fixed the bug where that path
// refused — a moved binary then left launchd restarting an executable that no
// longer existed, and the remedy the error text recommended ("re-run init") was
// the one thing that could not work. Uninstalling first restores overwrite
// semantics.
//
// Not a stop: the caller unloads the running job before this, because launchctl's
// fallback spelling has to READ the plist to identify the job.
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
		// The library returns a bare os error for a missing unit; treat any
		// not-found as done rather than parsing its message.
		if IsNotInstalled(err) {
			return nil
		}
		return fmt.Errorf("laneservice: removing the %s unit for %s: %w", goos, s.Label, err)
	}
	return nil
}

// IsNotInstalled reports whether an Uninstall error means there was nothing to
// remove.
//
// Matched on the message because the library returns a bare formatted error for
// a missing unit rather than something wrapping fs.ErrNotExist, so errors.Is
// cannot see it. Deliberately generous: over-reporting "already gone" makes
// removal idempotent, which is the direction that cannot hurt — the caller
// reports what it removed and a stale unit would still be caught by doctor.
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
