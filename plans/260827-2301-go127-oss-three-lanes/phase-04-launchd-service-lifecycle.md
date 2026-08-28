# Phase 04 — launchd service lifecycle → kardianos/service

## Context links

- Parent: [plan.md](plan.md) · Depends: [phase-01](phase-01-go-127-floor-raise.md)
- Decision: **D-OSS-3** (phases [09](phase-09-telemetry-receiver-daemon.md),
  [11](phase-11-transport-proxy-service.md) and
  [12](phase-12-one-command-and-election.md) build two more services on this
  foundation)
- Scout: [scout-01](scout/scout-01-replacement-seams.md) §D-OSS-3
- Research: [researcher-02](research/researcher-02-kardianos-go127-migration.md) §Topic 1
  — **its critical question is unresolved; step 1 resolves it**

## Overview

- Date: 2026-08-27 · Priority: P2 · Effort: 6h (see step 1 — may drop to 4h or
  rise to 8h once the log-path question is answered)
- Implementation status: **done; 2 verification steps need a real machine** · Review status: pending
- Report: [verification-260828-phase-04](reports/verification-260828-phase-04-launchd-service-lifecycle.md)
- Replace the hand-assembled launchd plist in `cli/internal/gatewayservice/unit.go`
  with the library, without losing the two properties that were learned the hard
  way: logs go to `~/.openbox/`, and install proves before it points.

## Key insights

- **The value of this swap is contingent, and step 1 decides how contingent.**
  `StandardOutPath`/`StandardErrorPath` were **verified hardcoded** to
  `/usr/local/var/log/<Name>.{out,err}.log` (kardianos issue #281 — a path that
  often does not exist on Apple Silicon). The issue is closed by PR #307, but the
  fix could not be read (sandbox TLS/404). If the only way to pin our path is
  `LaunchdConfig` — a **full template override of the entire plist** — then we
  are still authoring the plist, just as a Go template rather than string
  concatenation, and the library's contribution narrows to install/start/stop/
  uninstall mechanics plus a future Linux/Windows story. That is still worth
  having; it is just much less than "delete `unit.go`".
- **`~/.openbox/gateway.log` is not cosmetic.** launchd sends stdio to
  `/dev/null` by default, and those throttled warnings are the ONLY signal that a
  perfectly working relay is recording nothing (no DID, or no session header).
  `doctor` reports alive/configured/bypass and never "is it recording". A swap
  that quietly redirects logs to a nonexistent `/usr/local/var/log` re-opens
  exactly the blindness ADR-0021 closed.
- **Install ordering is a safety property and the library does not know it.**
  unit → start → **prove it listens** → only then write the env var; uninstall
  reverses. Any failure after `WriteUnit` must also remove the unit, because
  `KeepAlive`/`Restart=always` would otherwise restart-loop a gateway the
  developer was never told about. kardianos supplies `Install`/`Start`/`Stop`/
  `Uninstall`; the *proof step* and the *rollback* stay ours.
- **`UserService: true` is almost certainly correct** — ADR-0021 specifies a
  per-developer loopback daemon, so `~/Library/LaunchAgents` with no root, not
  `/Library/LaunchDaemons`. Confirm in step 1 rather than assume.
- Uninstall semantics (does it unload before removing the plist?) are unverified
  and directly matter: `--remove-gateway` must leave no loaded unit behind.

## Requirements

1. Service install/start/stop/uninstall performed by `kardianos/service`.
2. `StandardOutPath`/`StandardErrorPath` resolve to `~/.openbox/gateway.log` —
   **verified by reading the written plist**, not by trusting an option.
3. User-level agent (`~/Library/LaunchAgents`), no root required.
4. The proof-order install and its rollback are preserved exactly.
5. `--remove-gateway` leaves no plist and no loaded unit; the displaced-env
   restoration path is untouched.
6. `openbox doctor`'s gateway block reports the same states as before.
7. Windows/Linux remain build-verified only — adopting a cross-platform library
   does **not** license a claim we have not run.

## Architecture

```
cli/internal/gatewayservice/
  unit.go      : plist assembly            → kardianos service.Config
                                              (+ LaunchdConfig template IF step 1
                                               shows it is the only path to our
                                               log paths)
  install path : unit → start → PROVE listening → env      [OURS, UNCHANGED]
  rollback     : any failure after WriteUnit removes unit  [OURS, UNCHANGED]
  env.go       : displaced-value activation record         [UNTOUCHED]
```

The library replaces *plist generation and launchctl invocation*. It does not
replace the ordering, the proof, the rollback, or the activation record.

## Related code files

- edit: `cli/internal/gatewayservice/unit.go` (plist assembly; `xmlEscape` likely
  becomes dead — delete only if truly unused)
- edit: `cli/internal/gatewayservice/` install/uninstall path (call the library,
  keep the ordering)
- edit: `cli/go.mod`
- untouched: `cli/internal/gatewayservice/env.go`, `cli/internal/gatewaycheck/`
- reference: `cli/cmd/openbox/initgateway.go:54-176` (the proof-order caller)

## Implementation steps

1. **Gate step — answer the log-path question before writing anything.**
   `go get github.com/kardianos/service@v1.3.0` then `go doc -all
   github.com/kardianos/service`; grep for `StandardOutPath`, `StandardErrorPath`,
   `LaunchdConfig`, `UserService`, and the Install/Uninstall doc comments. Record
   the answer in the phase report. Pre-decided branches:
   - **a dedicated option exists** → use it; `unit.go` shrinks to a `service.Config`
     (effort ~4h);
   - **only `LaunchdConfig` works** → port the current plist into a template,
     adopt the library for lifecycle only, and **state in the phase report that
     the LOC saving is small and the gain is mechanics + future platforms**
     (effort ~8h);
   - **neither pins our path** → **stop and report.** Do not accept
     `/usr/local/var/log`; that silently re-opens the not-recording blindness.
     Escalate as an owner decision, since D-OSS-3 was made on the assumption the
     library could serve this.
2. Write a test that reads the **generated plist** and asserts the two log paths,
   `Label`, `ProgramArguments`, `KeepAlive`, `RunAtLoad`. Assert on the artifact,
   not on the config struct — this repo has already shipped a bug where the struct
   was asserted and the wire was not.
3. Swap plist generation; keep the install path's ordering and its rollback.
4. Confirm `UserService` targets `~/Library/LaunchAgents` and needs no root.
5. Exercise install → verify listening → uninstall against the **real binary** on
   a real machine, then assert: no plist on disk, nothing in `launchctl list`.
6. Verify `~/.openbox/gateway.log` actually receives output from a running
   gateway — the whole point of the requirement.
7. Confirm `doctor`'s gateway block is unchanged in behavior.
8. `-race`, both cross-compiles. Confirm the library does not drag a build
   constraint that breaks the Windows/linux-arm64 cross-compile.

## Todo

- [ ] **gate step answered and recorded** (option / template / stop)
- [ ] generated-plist assertion test (artifact, not struct)
- [ ] plist generation swapped; ordering + rollback preserved
- [ ] `UserService` → `~/Library/LaunchAgents`, no root
- [ ] real-binary install → uninstall leaves nothing
- [ ] `~/.openbox/gateway.log` receives real output
- [ ] `doctor` gateway block unchanged
- [ ] `-race` + both cross-compiles

## Success criteria

- The generated plist contains `StandardOutPath`/`StandardErrorPath` pointing at
  `~/.openbox/gateway.log`, asserted by reading the file.
- A running gateway's stderr reaches that file.
- Install failure after unit creation leaves **no** unit loaded and **no** env key
  written (the existing rollback still holds).
- `--remove-gateway` leaves no plist and no `launchctl list` entry.
- No root required for install or uninstall.
- Both cross-compiles still pass; no new claim about Windows/Linux support.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| **The library cannot pin our log path** (issue #281; PR #307 unread) | Step 1 gate before any code | `go doc` shows no option and `LaunchdConfig` is template-only | Pre-decided: template branch, or **stop** if even that fails. Never accept `/usr/local/var/log` |
| Logs silently go to a nonexistent dir | Assert on the generated plist **and** on real output reaching the file | `gateway.log` empty while the gateway runs | Treat as a bug in the unit, not the daemon (ADR-0021's own rule) |
| **Uninstall does not unload before removing the plist** (unverified) | Real-binary install/uninstall test asserting `launchctl list` | A unit remains loaded after removal | Add an explicit stop before uninstall; do not rely on library ordering |
| Library's Install writes to `/Library/LaunchDaemons` and demands root | Confirm `UserService` in step 1; test as non-root | Permission denied on install | Set `UserService: true`; a root-requiring install is a product regression |
| Proof-order or rollback lost in the refactor | They stay ours; diff the install path | Install writes env before proving listening | **Stop** — this is the safety property, not a detail |
| Adopting a cross-platform library invites an unearned support claim | Docs unchanged for Windows/Linux | A doc says Windows is supported | Revert the claim; build-verified is not run-verified |
| No `ExitTimeOut` equivalent (unverified) | Check in step 1; current unit sets it | Shutdown grace differs | Accept or carry it in the template branch; record which |

## Security considerations

- The unit runs a daemon that relays model traffic and, in
  [phase 11](phase-11-transport-proxy-service.md), will terminate TLS with a
  project CA. **`UserService` (per-user, no root) is the correct blast radius**;
  a system daemon would run this as root for every user on the machine. Do not
  let a library default silently promote it.
- The plist path and the log path both live under the developer's own trust
  boundary (ADR-0015): readable by anything running as the developer, including
  the agent being governed. That is unchanged by this swap and must not be
  described as hardened by it.
- `gateway.log` may contain throttled warnings about missing DIDs or session
  headers. It must not gain request/response content — `verbose.go`'s rule
  (method/route/status/duration only) still governs what is written.
- Uninstall must be complete. A leftover loaded unit after `--remove-gateway` is a
  relay the developer believes is gone.

## Next steps

Phase 05 (credential-guard scope) then phase 06 (gitleaks). Phases
[09](phase-09-telemetry-receiver-daemon.md) and
[11](phase-11-transport-proxy-service.md) build `telemetryservice` and
`transportservice` on whatever this phase settles.

## Outcome (2026-08-28)

Done — see the
[verification report](reports/verification-260828-phase-04-launchd-service-lifecycle.md).

**The gate found FOUR facts, not one.** Log paths landed on pre-decided branch (b)
(`LogDirectory` is settable but filenames are hardcoded and there are TWO files,
where the gateway needs one tailable `~/.openbox/gateway.log`). Three more surfaced
from the library's source and were reported to the owner before any code was
written:

- the unit's path is **not caller-specifiable** — darwin derives home from
  `user.Current()`, ignores `$HOME`, and no Option overrides it (measured);
- kardianos' `Start`/`Stop` are **weaker** than the repo's `bootstrap`→`load -w`
  and `bootout`→`unload` cascade;
- `Install()` **refuses when the unit exists**, reintroducing the re-install bug
  this repo already fixed.

**Owner ruling: adopt fully, accept the isolation loss.** Built accordingly:
`Reinstall` (uninstall-then-install) restores overwrite semantics, per-platform
`serviceName` keeps both unit paths byte-identical to before, and the bodies stay
ours via `LaunchdConfig`/`SystemdScript`.

**Two deviations from "fully", stated rather than smuggled:** `launchctl` start/stop
stayed ours (adopting the library's would be a downgrade on current macOS, and its
`run` seam is what the rollback tests inject through), and the nine `setupGateway`
tests route the write through the path-explicit writer — without that seam every
`go test ./...` on a Mac would install a live launchd unit into the developer's home.
Both paths emit identical bytes, pinned by
`TestSuppliedTemplatesSurviveRendering`.

**Steps 5–6 are NOT verified** and neither is verifiable here: the real
install/uninstall cycle, and that `~/.openbox/gateway.log` receives output from a
running gateway. Requirement 2's artifact assertion is written but **opt-in**
(`OPENBOX_TEST_REAL_SERVICE_INSTALL=1`), because unguarded it writes into the
runner's home. The report carries both commands.
