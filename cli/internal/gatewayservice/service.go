package gatewayservice

import (
	"fmt"
	"strings"

	"github.com/kardianos/service"
)

// service.go adopts kardianos/service for the gateway's supervisor lifecycle
// (D-OSS-3): install, start, stop, uninstall.
//
// WHAT THE LIBRARY OWNS: the launchctl / systemctl invocations and the unit file
// write and removal.
//
// WHAT IT DOES NOT OWN, deliberately:
//
//   - **the unit CONTENT.** Both bodies are still rendered by unit.go and handed
//     to the library as its own template overrides (`LaunchdConfig`,
//     `SystemdScript`). Its generated launchd plist cannot express what this repo
//     requires: its `LogDirectory` option is settable but the filenames are
//     hardcoded `<Name>.out.log` / `<Name>.err.log` (service_darwin.go
//     `getLogPath`), i.e. TWO files with derived names, where the gateway needs
//     ONE tailable `~/.openbox/gateway.log`. Supplying the whole template keeps
//     that, and keeps `ExitTimeOut` matched to `--shutdown-grace`;
//   - **the install ORDERING, the readiness proof, the rollback, or the env
//     activation record.** Those live in the caller (initgateway.go) and are the
//     safety property: unit → start → PROVE listening → env, and any failure after
//     the install removes the unit again.
//
// TWO CONSEQUENCES OF ADOPTING IT, both accepted by owner ruling rather than
// discovered later:
//
//  1. **The unit's own path is no longer caller-specified.** On darwin the library
//     derives the home directory from `user.Current()` and only falls back to
//     `$HOME` if that errors (service_darwin.go `getHomeDir`), and no Option
//     overrides the path. Measured: with `HOME=/tmp/…`, `user.Current().HomeDir`
//     still returns the real home. So `Install()` writes to the REAL
//     `~/Library/LaunchAgents` regardless of a test's `t.Setenv("HOME", …)`.
//     Consequence: an automated test of the install path would write a live
//     launchd unit onto whatever machine runs it, so the artifact assertion is
//     opt-in (see service_test.go). systemd is unaffected — it uses
//     `os.UserHomeDir()`, which does respect `$HOME`.
//  2. **Its Start/Stop are weaker than the calls they replace.** kardianos runs
//     `launchctl load` / `unload`; this repo ran `launchctl bootstrap gui/<uid>`
//     first and fell back to `load -w`, and `bootout` before `unload`. `bootstrap`
//     is the modern spelling and the reason the install works on current macOS.
//     `Start`/`Stop` below therefore keep the repo's own cascade and use the
//     library for install/uninstall only — adopting a regression to satisfy a
//     decision's letter would be the wrong trade.
//
// The LOG path is still ours end to end: it is content, not location, so
// `~/.openbox/gateway.log` survives adoption unchanged.

// serviceName is the library's `Name`, and it is PER-PLATFORM on purpose.
//
// The library derives the unit's filename from it — `<Name>.plist` on darwin,
// `<Name>.service` on systemd — and this repo's two platforms do not share a
// naming convention: the launchd label is a reverse-DNS `ai.openbox.gateway`, the
// systemd unit is `openbox-gateway.service`. One shared value would silently
// rename one of them, and `UnitPath`, `openbox doctor` and the re-install
// detection all read the path this produces.
func serviceName(goos string) (string, error) {
	switch goos {
	case "darwin":
		return LaunchdLabel, nil
	case "linux":
		// SystemdUnitName carries the ".service" suffix the library appends itself.
		return "openbox-gateway", nil
	default:
		return "", fmt.Errorf("gatewayservice: no daemon packaging for %s yet — run `openbox gateway` in the foreground, or supervise it with the platform's own service manager", goos)
	}
}

// controlOnly satisfies service.Interface without running anything.
//
// The gateway is NOT run through the library's `Run()` loop: the unit executes the
// binary in the foreground and the OS supervisor owns restart, which is the
// availability story unit.go documents. `service.New` requires an Interface even
// when only install/uninstall are used, so this is the null implementation. If it
// were ever reached, that would mean the gateway had been restructured to run
// under the library's loop — a different design, not a bug to patch here.
type controlOnly struct{}

func (controlOnly) Start(service.Service) error { return nil }
func (controlOnly) Stop(service.Service) error  { return nil }

// New returns the supervisor handle for the gateway on this platform.
//
// homeDir is still meaningful: it selects the LOG path inside the rendered unit.
// It no longer selects where the unit itself lands — see the consequence note
// above.
func New(goos, homeDir, binPath, addr, upstream string, verbose bool) (service.Service, error) {
	name, err := serviceName(goos)
	if err != nil {
		return nil, err
	}
	argv := gatewayArgv(binPath, addr, upstream, verbose)
	cfg := &service.Config{
		Name:        name,
		DisplayName: "OpenBox local gateway",
		Description: "Relays model calls through OpenBox for governance (ADR-0021).",
		Executable:  binPath,
		// argv[0] is the binary, which Executable already carries.
		Arguments: argv[1:],
		Option: service.KeyValue{
			// A per-developer agent, never a system daemon: the base tier is
			// user-owned by definition and ADR-0016 puts the install in the
			// developer's hands, so needing root would be the wrong shape.
			"UserService": true,
			// Both bodies are ours. They contain no template actions, so the
			// library's text/template render is an identity transform — pinned by
			// TestSuppliedTemplatesSurviveRendering so a future edit that
			// introduces `{{` cannot silently corrupt a unit.
			"LaunchdConfig": LaunchdPlist(homeDir, binPath, addr, upstream, verbose),
			"SystemdScript": SystemdUnit(binPath, addr, upstream, verbose),
			// Set even though the supplied templates already carry them: if a
			// template is ever dropped, the generated fallback should still
			// restart a crashed gateway and start one at login.
			"KeepAlive": true,
			"RunAtLoad": true,
			"Restart":   "always",
		},
	}
	return service.New(controlOnly{}, cfg)
}

// Install writes the unit through the library. It does NOT start it: keeping the
// write separate from the load is what makes "failed to configure" and "failed to
// start" distinguishable, the same reason doctor separates alive from actually
// used.
func Install(goos, homeDir, binPath, addr, upstream string, verbose bool) error {
	svc, err := New(goos, homeDir, binPath, addr, upstream, verbose)
	if err != nil {
		return err
	}
	if err := svc.Install(); err != nil {
		return fmt.Errorf("gatewayservice: installing the %s unit: %w", goos, err)
	}
	return nil
}

// Reinstall writes the unit, replacing one that is already there.
//
// The library's Install() REFUSES when the unit file exists — both platforms do
// `os.Stat` and return "Init already exists" (service_darwin.go:175,
// service_systemd_linux.go:152). The file write this replaced used os.WriteFile,
// which overwrites, and that difference is load-bearing rather than cosmetic:
// re-running `init --gateway` is how a unit written by an OLDER binary gets
// refreshed. This repo already shipped and fixed the bug where that path refused —
// a moved binary then left launchd restarting an executable that no longer existed,
// and the remedy the error text recommended ("re-run init") was the one thing that
// could not work. Uninstalling first restores overwrite semantics.
//
// Not a stop: the caller unloads the running job before this, because launchctl's
// fallback spelling has to READ the plist to identify the job.
func Reinstall(goos, homeDir, binPath, addr, upstream string, verbose bool) error {
	if err := Uninstall(goos, homeDir); err != nil {
		return err
	}
	return Install(goos, homeDir, binPath, addr, upstream, verbose)
}

// Uninstall removes the unit through the library. Absent is success: `--remove-gateway`
// must be safe to run on a machine that never had one.
func Uninstall(goos, homeDir string) error {
	svc, err := New(goos, homeDir, "", DefaultProbeAddr, DefaultProbeUpstream, false)
	if err != nil {
		return err
	}
	if err := svc.Uninstall(); err != nil {
		// The library returns a bare os error for a missing plist; treat any
		// not-found as done rather than parsing its message.
		if IsNotInstalled(err) {
			return nil
		}
		return fmt.Errorf("gatewayservice: removing the %s unit: %w", goos, err)
	}
	return nil
}

// DefaultProbeAddr / DefaultProbeUpstream are placeholders for the uninstall path,
// which needs a Config to address the unit but no working argv — nothing is
// rendered or started. They are named rather than inlined so a reader does not
// mistake them for a default the install path uses.
const (
	DefaultProbeAddr     = "127.0.0.1:8788"
	DefaultProbeUpstream = "https://api.anthropic.com"
)

// IsNotInstalled reports whether an Uninstall error means there was nothing to
// remove.
//
// Matched on the message because the library returns a bare formatted error for a
// missing unit rather than something wrapping fs.ErrNotExist, so errors.Is cannot
// see it. Deliberately generous: over-reporting "already gone" makes
// `--remove-gateway` idempotent, which is the direction that cannot hurt — the
// caller reports what it removed and a stale unit would still be caught by
// `openbox doctor`.
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
