# Scout 01 — gateway service lifecycle (the template both new services extend)

Read-only map, verified against source 2026-08-27.

> Note: this file deliberately does not spell out the credential env-var names —
> writing them tripped the repo's own secret detector during planning. See
> `adapters/common/devconfig/paths.go` and `creds.go` for the current names.

## A. `gateway/` package internals

| File | What |
|---|---|
| `proxy.go` | `Gateway{}` (upstream, client, emitter, evaluator, gated, logf); `Emitter` + `Evaluator` interfaces; `WithCapture` (132–135); `WithGate` (145–149); `New(Config)` (152–195); `ServeHTTP` (203–425); `capturableBody` (447–455); `captureSink` bounded tee (471–503); `streamTo` per-chunk flush for SSE (514–547) |
| `capture.go` | `Captured` (185–194), `RequestCapture` (203–211); `CaptureRequest` fingerprints BEFORE redacting (220–233); `Complete` (240–251); `credentialFingerprint` (107–117; header preference order documented there); `captureBody` (146–162) redacts via `decision.NewRedactor()` then caps to `captureBodyRunes`; `stripQuery` (282–287) |
| `config.go` | `Config{Addr, Upstream}`; `DefaultAddr = "127.0.0.1:8788"`; `DefaultUpstream`; **`Validate()` enforces loopback-only** + upstream URL + self-loop detection (40–74) |
| `gate.go` | `Decision{Forward, Verdict, Reason, Evaluated, Unreachable}`; `Decide(...)` — evaluation attempted BEFORE any synthesized refusal; `evaluateTimeout = 10s` |
| `refuse.go` | `refusalStatus = 403`, `refusalErrorType = "openbox_policy_refusal"` (both PROVISIONAL, probe A); `WriteRefusal` |
| `verbose.go` | `WithVerbose` (32–35); `vlog` (42–46) logs method/path/status/duration, **never** headers/bodies/credentials |

**Dependency tripwire:** `gateway/go.mod` requires exactly `client` + `decision`, and
**`gateway/guard_test.go` enumerates that allowlist and FAILS on a third**. Any new
external dependency (e.g. protobuf) cannot land in this module.

**Wiring bug precedent:** `WithCapture` had no production caller → `g.emitter` nil →
all capture discarded while both sides' fakes stayed green.
`cli/cmd/openbox/gatewaycapture_test.go` is the control (real command → real spool).

## B. launchd/systemd lifecycle — `cli/internal/gatewayservice/unit.go`

`LaunchdLabel = "ai.openbox.gateway"`; `SystemdUnitName = "openbox-gateway.service"`;
`StopTimeout = 30s`. `gatewayArgv` (53–64) → `LaunchdPlist` (71–106; `KeepAlive`,
`RunAtLoad`, `StandardOutPath`/`StandardErrorPath` → `~/.openbox/gateway.log` 102–103)
/ `SystemdUnit` (124–146). `LogPath` (113–115). `WriteUnit` (182–203, dir 0755 file
0644). `UnitPath` (207–216). `RemoveUnit` (219–236).

## C. `openbox init` wiring

- `cli/cmd/openbox/main.go`: flags 356–362 (`--gateway`, `--remove-gateway`,
  `--gateway-addr`, `--gateway-upstream`, `--gateway-verbose`); mutual exclusion +
  Claude-Code-only check 469–483; `printGatewayPlan` (`--dry-run`) 528–533;
  **removal early-exit 539–558 — BEFORE `requireCredentials`**; `setupGateway` call 603+.
- `cli/cmd/openbox/initgateway.go`:
  - `setupGateway` (54–139) — **THE INSTALL ORDER**: validate → port/ownership check
    (84–97) → `WriteUnit` (104–111) → `loadUnit` (113–118, rollback on error) →
    `waitForListenerFn` prove listening (120–123, rollback on timeout) → **only then**
    `WriteEnv` (127–137).
  - `removeGateway` (142–176) — **REVERSE**: `RemoveEnvDetailed` first (145–159) →
    `unloadUnit` best-effort (161–166) → `RemoveUnit` (168–174).
  - `loadUnit` (181–199) launchctl bootstrap/load | systemctl daemon-reload + enable
    --now; `unloadUnit` (203–213); `waitForListenerFn` (229–240, 200ms poll);
    `portOccupied` (272–281); `unitDescribesAddr` (382–391); `rollbackUnit` (406–414).
  - Test seams as package vars: `waitForListenerFn`, `waitForPortFreeFn`, `run`,
    `currentUID`, `portOccupied`, `unitDescribesAddr`.

## D. Transactional env — `cli/internal/gatewayservice/env.go`

`EnvKey = "ANTHROPIC_BASE_URL"` (34) — **the ONLY owned key**; `ownedEnvKeys` (38);
`SettingsPath` → `~/.claude/settings.json` (47–49); `priorEnvPath` →
`~/.openbox/gateway-prior-env.json` (58–60); `WriteEnv` (77–108) with
**first-writer-wins** prior capture (91–102); `RemoveEnv` (119–122) /
`RemoveEnvDetailed` (125–127) restore only owned keys; `readSettings` (231–253)
preserves unknown keys, refuses unreadable JSON; `writeSettings` (255–286) +
`writeFileAtomic` temp+rename (282–312).

**Gap this plan must close:** this module is built around **one** key with **one**
prior file. Phase 02 needs ~13 OTel keys and phase 04 needs ~5 proxy keys.
→ generalize to a managed-key **set** with an activation record (the logger's
`activation.json` discipline: managed map + original presence/value per key +
before/after SHA-256 + refuse-on-conflict). DRY: one mechanism, three callers.

`adapters/common/devconfig`: `write.go` `Update` tri-state (`*bool` = nil means "not
supplied") + `WriteConfig` (50–79) + `setBoolPtr`; `envfile.go` `ParseEnvFile` /
`WriteEnvFile` (0600, sorted); `paths.go` `Home()` (42–58, `$OPENBOX_HOME` or
`~/.openbox`), `ensureHome()` 0700, `EnvFilePath`, `DevConfigPath` /
`DevConfigWritePath`.

## E. doctor

`cli/cmd/openbox/doctor.go` `runDoctor` (29–166) → `reportGateway` (224–294) calls
`gatewaycheck.Inspect(home, managedPath, 750ms, getenv)` and prints five lines:
configured / from(+tier) / reachable / environment(+precedence) / log.
`cli/internal/gatewaycheck/check.go`: `Report{Alive, ConfiguredAddr, SettingsPath,
TargetsGateway, Tier, OwnerUID, BypassCapable, BypassNotes, EnvValue,
EnvDiffersFromSettings}`; `Inspect` (99–150+); **tier inferred from file OWNERSHIP**
(root ⇒ mdm, else base).

## F. `~/.openbox` layout

```
~/.openbox/
  .env                       credentials, 0600 (names: see devconfig/creds.go)
  dev.json                   coordinates + posture, 0600
  approver.json              approver identity, 0600
  gateway.log                supervised stdio (launchd sends stdio to /dev/null otherwise)
  gateway-prior-env.json     displaced ANTHROPIC_BASE_URL, first-writer-wins
  ~/Library/LaunchAgents/ai.openbox.gateway.plist        (macOS)
  ~/.config/systemd/user/openbox-gateway.service         (Linux)
```
Real env vars beat the `.env` file.

## Risks carried into the plan

- Forgetting `WithCapture` ⇒ silent total capture loss (precedent above).
- `portOccupied` races; mitigated by `unitDescribesAddr` ownership check.
- Ownership-based tier inference is unknown on Windows (uid −1).
- Atomic write is not a lock: concurrent init+tool ⇒ lost update, not corruption.
- A crash between `WriteEnv` and the prior-record write would mis-record the prior
  (guarded by "only if no prior record exists").

## Unresolved

None for the mapped surface. The env-module generalization (D) is a plan decision,
not an unknown.
