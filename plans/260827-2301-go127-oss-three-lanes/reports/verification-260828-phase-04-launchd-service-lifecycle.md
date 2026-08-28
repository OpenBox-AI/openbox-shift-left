# Phase 04 verification — launchd service lifecycle (D-OSS-3)

**Date:** 2026-08-28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture` · **Decision:** D-OSS-3, adopted by owner
ruling after the gate found two blockers.

## Verdict

Implemented. `kardianos/service` v1.3.0 owns the unit-file lifecycle
(install/uninstall); the repo keeps unit CONTENT, install ORDERING, the readiness
proof, the rollback, and the `launchctl` start/stop cascade. gofmt/vet clean, both
cross-compiles clean, `GOWORK=off` 12/12, `gatewayservice` suite green.

**Requirement 2's artifact assertion is OPT-IN, and requirement 5–6 could not run
here.** Both follow from what the gate found. See
[Not verified](#not-verified) — please run the two commands there.

## The gate found two blockers, not one

Step 1 asked one question (can the library pin our log path?). The answer was yes-ish
and then three more facts surfaced, each read from v1.3.0's source rather than
inferred.

**1. Log paths — branch (b).** `LogDirectory` IS settable (issue #281's fix landed;
for a user service it defaults to `$HOME`, not `/usr/local/var/log`). But
`getLogPath` is `fmt.Sprintf("%s/%s.%s.log", logDir, s.Name, logType)` — hardcoded
filenames, and **two files** (`.out.log`, `.err.log`) where the gateway needs ONE
tailable `~/.openbox/gateway.log`. So the pre-decided branch (b) applies: supply the
whole plist via `LaunchdConfig`, and the systemd unit via `SystemdScript`. Content
stays ours; the LOC saving is small and the gain is mechanics, exactly as branch (b)
told us to state.

**2. The unit's path is not caller-specifiable.** `darwinLaunchdService.getHomeDir`
calls `user.Current()` FIRST and falls back to `$HOME` only if that errors, and **no
Option overrides the path** (checked every documented key; `Prefix` is Solaris-only).
Measured on this host:

```
HOME env         = /tmp/claude-501/fake-home
user.Current()   = /Users/phuongvu      <-- what kardianos uses
os.UserHomeDir() = /tmp/claude-501/fake-home
```

So `Install()` writes to the real `~/Library/LaunchAgents` regardless of a test's
`t.Setenv("HOME", …)`. systemd is unaffected — it uses `os.UserHomeDir()`. The
problem is darwin-only, i.e. the platform that actually runs the daemon.

**3. Its Start/Stop are weaker than the calls they would replace.** kardianos runs
`launchctl load` / `unload`; this repo runs `launchctl bootstrap gui/<uid>` first and
falls back to `load -w`, and `bootout` before `unload`. `bootstrap` is the modern
spelling and the reason the install works on current macOS.

**4. `Install()` REFUSES when the unit already exists** — `os.Stat` then
`"Init already exists"`, on both platforms (`service_darwin.go:175`,
`service_systemd_linux.go:152`). The `os.WriteFile` it replaced overwrote. That
difference is load-bearing: re-running `init --gateway` is how a unit written by an
older binary gets refreshed, and **this repo already shipped and fixed the bug where
that path refused** — a moved binary then left launchd restarting a path that no
longer existed, with the recommended remedy ("re-run init") being the one thing that
could not work.

Blockers 2–4 were reported to the owner with the measurements before any code was
written. **Ruling: adopt fully, accept the isolation loss.**

## What was built

| Piece | Owner |
|---|---|
| unit file write / removal | **library** (`Install`, `Uninstall`, `Reinstall`) |
| launchd plist + systemd unit BODY | ours (`unit.go`), handed over as `LaunchdConfig` / `SystemdScript` |
| `launchctl bootstrap`→`load -w`, `bootout`→`unload` | **ours** — see below |
| install ordering, readiness proof, rollback, env activation record | ours, unchanged |

New `cli/internal/gatewayservice/service.go`:

- **`Reinstall` = Uninstall-then-Install**, restoring the overwrite semantics
  blocker 4 removed.
- **`serviceName` is per-platform.** The library derives the filename from `Name`
  (`<Name>.plist`, `<Name>.service`) and this repo's platforms do not share a
  convention — launchd is reverse-DNS `ai.openbox.gateway`, systemd is
  `openbox-gateway.service`. One shared value would silently rename one of them, and
  `UnitPath`, `openbox doctor` and the re-install check all read that path.
  `TestServiceNameMatchesTheRepoPaths` pins both against `LaunchdPath`/`SystemdPath`.
- **`controlOnly`** is the null `service.Interface`: the gateway is not run through
  the library's `Run()` loop — the unit executes the binary in the foreground and the
  OS supervisor owns restart, which is the availability story.
- `rollbackUnit` now removes via the library rather than `os.Remove(unitPath)`,
  because the library chose where the unit went; removing the caller-computed path
  would leave the real unit in place whenever the two disagree.

### Two deliberate deviations from "fully", both stated rather than smuggled

**Start/Stop stayed ours.** Adopting kardianos' would drop `bootstrap` and the `-w`
flag — a functional downgrade on current macOS — and the `run` seam it uses is what
the ordering/rollback tests inject supervisor failure through. Replacing it would
delete the tests that prove requirement 4.

**Production installs through the library; the nine `setupGateway` tests do not.**
`skipUnlessSupervised` only skips non-darwin/linux, so without a seam every
`go test ./...` on a developer's Mac would install a live launchd unit into their
home. Those nine tests are the ones proving the property this repo calls "the one
that would actually hurt a developer" — that `ANTHROPIC_BASE_URL` is never written
while nothing listens. Gating them off by default would remove that proof from every
ordinary run, a worse trade than routing them through the path-explicit writer.
`installUnitFn` / `uninstallUnitFn` are the seam, alongside the file's existing
`run`, `currentUID`, `waitForListenerFn` seams; `stubSupervisor` swaps them.

The two paths write **identical bytes** — same renderers — and
`TestSuppliedTemplatesSurviveRendering` pins that the library's `text/template`
render is an identity transform over our bodies, so asserting one artifact asserts
the other. That pin is also the tripwire for a future edit introducing `{{` into a
unit body, which would otherwise corrupt a unit silently.

`WriteUnit`/`RemoveUnit` survive for that reason and are documented as no longer the
production path.

## Evidence

| Check | Result |
|---|---|
| `gofmt -l`, `go vet` (12 modules) | clean |
| `GOOS=windows GOARCH=amd64` | clean — kardianos ships a real `service_windows.go`; no renameio-style empty-package trap |
| `GOOS=linux GOARCH=arm64` | clean |
| per-module `GOWORK=off go build` | **12/12 ok** |
| `cli/internal/gatewayservice` suite | green, incl. 4 new tests |
| the 6 fully-runnable modules | green |
| `x/term` still unpinned at v0.45.0 after `go mod tidy` | verified (it regressed once this session) |
| systemd persistence preserved | kardianos' systemd `Install()` does `enable` then `daemon-reload`, so the unit still survives reboot |

New tests: `TestSuppliedTemplatesSurviveRendering` (3 bodies),
`TestServiceNameMatchesTheRepoPaths`, `TestUnsupportedPlatformIsAnError`,
`TestUninstallIsIdempotent`.

## Not verified

Two of the phase's steps need a machine this sandbox cannot provide (no listener
bind, no `launchctl`, no writes to `~/Library/LaunchAgents`).

**1. Requirement 2's artifact assertion — written, opt-in, never run.**
`TestRealInstallWritesTheExpectedArtifact` installs, reads the plist off disk,
asserts both log keys name the single `~/.openbox/gateway.log`, plus `Label`,
`ProgramArguments`, `KeepAlive`, `RunAtLoad`, `ExitTimeOut`, then uninstalls and
asserts the file is gone. It skips unless opted in, because unguarded it writes a
live unit into the runner's home:

```
OPENBOX_TEST_REAL_SERVICE_INSTALL=1 go test ./cli/internal/gatewayservice/ -run TestRealInstall -v
```

It refuses to run if a unit already exists, so it will not disturb a real install.

**2. Steps 5–7 — the install/uninstall cycle and the log file.** Every
`setupGateway` test fails here at `net.Listen`, before reaching any changed code:

```
go test ./cli/... 2>&1 | grep -E "Gateway|ReInstall|FailedInstall|Occupied|Foreign"
```

That run is what confirms the seam works and the ordering/rollback still hold. Step
6 — that `~/.openbox/gateway.log` actually receives output from a running gateway —
needs a real `openbox init --gateway`, and remains the requirement whose purpose is
unproven by me.

## Unresolved questions

1. **Is the deviation on Start/Stop acceptable?** The library performs
   install/uninstall; `launchctl` start/stop stayed ours because kardianos' is
   strictly older and the seam is load-bearing for the safety tests. If you want
   literal full adoption, say so — it is a small change and a known downgrade.
2. **`UnitPath(goos, homeDir)` is now advisory on darwin.** It is what gets printed
   and what the re-install check reads, and it is correct wherever `homeDir` equals
   the real home — which is production, but not a test. If a future caller passes a
   non-real home in production, the reported path and the written path diverge
   silently. A `Path()` accessor on the library's side would fix it; none exists.
3. **Windows is now packageable but not packaged.** kardianos ships a real Windows
   service implementation, so `WriteUnit`'s "no daemon packaging" error is no longer
   a library limitation — only a decision. Requirement 7 says Windows stays
   build-verified, so nothing was claimed; worth revisiting deliberately.
