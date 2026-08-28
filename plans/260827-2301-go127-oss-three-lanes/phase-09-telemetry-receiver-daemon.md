# Phase 09 — telemetry receiver daemon (otlpreceiver over loopback)

## Context links

- Parent: [plan.md](plan.md) · Depends: [phase-08](phase-08-adr-contract-decision.md)
  (contract + namespaces), [phase-04](phase-04-launchd-service-lifecycle.md)
  (settled service lifecycle; otlpreceiver also needs phase 01's go 1.25+ floor)
- Decision: **D-OSS-2** — OTLP intake via
  `go.opentelemetry.io/collector/receiver/otlpreceiver` as a library, latest
  (unpinned under D-GO-1)
- Scouts: [scout-01](scout/scout-01-gateway-service-lifecycle.md) (lifecycle template)
- Evidence: openbox-logger run `20260827T063932Z-225cac`; `config/otel-collector.yaml`,
  `src/openbox_logger/settings.py` in the sibling repo (reference only — nothing here
  depends on it at runtime).

## Overview

- Date: 2026-08-27 · Priority: P1 · Effort: 8h
- Implementation status: **partial (2026-08-28)** — module done, daemon blocked on `bind`
- Report: [verification-260828-phase-09](reports/verification-260828-phase-09-telemetry-receiver.md)
- Stand up a loopback OTLP receiver inside the openbox daemon and activate Claude
  Code's own telemetry into it. This phase **receives and spools raw records**;
  phase 10 maps them onto the contract.

## Key insights

- This is the lane that closes D1: it reaches the embedded engine on **both** the
  CLI and the desktop app, because it rides the `env` block of
  `~/.claude/settings.json` rather than per-client routing. Measured 2026-08-27.
- **The intake is the collector's own receiver, not hand-rolled decode**
  (D-OSS-2, validation round 2 — supersedes round 1's "http/json +
  `encoding/json`, zero new dependencies" design *and* its vendored-protobuf
  fallback). `otlpreceiver` handles **both** OTLP encodings (protobuf and JSON)
  and both signals' endpoints, so the wire-format probe informs *config* — which
  encoding this client version actually emits, which endpoints to enable — not
  lane survival. Do not hand-roll an OTLP parser in any branch.
- **The dependency cost is real and quarantined.** otlpreceiver pulls a large
  transitive tree (~98 requires measured at audit time). That is why `telemetry/`
  is its **own module** in `go.work` with its **own dependency guard test**
  (phase 05's per-module boundary): `gateway/`'s two-entry allowlist is untouched,
  and the tree is recorded as an accepted cost in
  [phase 14](phase-14-coverage-and-docs.md). Measure and record it here too —
  binary size and `go list -deps` delta, the phase-06 discipline.
- launchd sends stdio to `/dev/null` by default — the gateway learned this. The
  receiver must log to a file, or a silently-not-recording daemon looks healthy.
- The existing env module owns exactly **one** key. This phase needs ~13. Generalize
  rather than copy (see phase 12 for the shared mechanism; this phase consumes it).
- Service mechanics come from phase 04's settled kardianos path — whichever
  branch its gate step chose (config option or template). Mirror it; do not
  resurrect the string-concatenation plist.

## Requirements

1. A new module `telemetry/` (in `go.work`) exposing an OTLP receiver **built on
   `otlpreceiver`**, bound to loopback only, accepting `/v1/logs`, `/v1/traces`,
   `/v1/metrics`.
2. Records land in the existing spool through the same emitter seam the gateway
   uses — **wired in production**, with a control test that uses no fake at either
   end (the `WithCapture` precedent).
3. Env activation writes the telemetry keys transactionally through the shared
   mechanism, and the receiver is only pointed at after it is proven listening.
4. Service lifecycle mirrors the phase-04 `gatewayservice`: unit label,
   `KeepAlive`, stdio to `~/.openbox/telemetry.log`, install/rollback/uninstall.
5. `openbox doctor` reports the lane: configured / reachable / **recording**.
6. Posture: a `telemetry` key resolved by the existing `resolveBoolWithSource`
   tri-state pattern, **on by default once the lane is installed** (validation
   ruling 2026-08-27 — installing is the opt-in; a second switch would leave the
   lane inert, the ADR-0016 `ResolveFinops` lesson). Content still rides the
   existing `content_capture` gate. Two mechanisms are required, per the ADR-0016
   precedent: `*bool` so `omitempty` cannot drop an explicit `false`, and
   `flagPassed` so a plain re-run never reverts a deliberate opt-out — with the
   **second-invocation** test that catches the latter.
7. `telemetry/` has its own `guard_test.go`: direct requires enumerated
   (otlpreceiver + collector companions + `client` + `decision`), so the
   constraint is enforced, not remembered.

## Architecture

```
Claude Code (CLI or desktop)
  └─ env block in ~/.claude/settings.json  (13 keys, transactional)
       └─ OTLP (protobuf or JSON) → 127.0.0.1:<port>/v1/{logs,traces,metrics}
            └─ telemetry receiver (openbox daemon, launchd)
                 └─ otlpreceiver → consumer funcs over pdata
                      └─ normalized raw record → spool (phase 10 maps)
```

Integration surface: build the receiver from its factory with a config whose
endpoints are loopback-only; wire `consumer.Logs`/`consumer.Traces`/
`consumer.Metrics` funcs that bind **only the fields phase 10 consumes**, in the
`usage.go` spirit (unbound siblings are ignored, not errors). Read the pdata
API from the pinned source, not docs summaries.

Env keys written (values per the logger's proven set; protocol per the step-1
probe): telemetry enable + enhanced-telemetry beta; the three exporter
selectors; the protocol; the three endpoint URLs; the export interval; the three
content switches (user prompts, tool details, tool content); and the raw-API-bodies
file sink pointed at a directory under `~/.openbox/`.

**Loopback-only** is enforced in config validation, exactly as `gateway/config.go`
`Validate()` does (40–74). Bind to a deterministic port; on "address in use",
identify the stale owner rather than incrementing.

## Related code files

- new: `telemetry/` module — `receiver.go` (otlpreceiver wiring), `config.go`,
  `consume.go` (pdata → raw record), `emit.go`, `guard_test.go`
- new: `cli/internal/telemetryservice/` — unit + install (mirror the phase-04
  `gatewayservice` shape)
- new: `cli/internal/telemetrycheck/` — doctor inspection (mirror `gatewaycheck`)
- edit: `go.work`, `cli/cmd/openbox/main.go` (flags), `cli/cmd/openbox/doctor.go`
- reference: `gateway/config.go:40–74`, `cli/internal/gatewayservice/` (post
  phase 04), `cli/cmd/openbox/initgateway.go:54–176`

## Implementation steps

1. **Probe first (config input, not survival):** with the daemon stubbed, set the
   OTel protocol env var per candidate encoding on a throwaway session and record
   which encodings/endpoints this client version actually emits, in
   `plans/reports/`. The result decides the env values written and the
   otlpreceiver endpoints enabled — the lane proceeds regardless (OD3's
   version-pinned probe discipline).
2. Create the `telemetry/` module; add to `go.work`. `go get` otlpreceiver at
   latest; measure and record the dependency tree + binary-size delta. Add the
   module's own `guard_test.go` allowlist (direct requires only, any host —
   phase 05's boundary).
3. `config.go`: loopback-only `Validate()`, deterministic port distinct from the
   gateway's `8788`.
4. `receiver.go`: build otlpreceiver from its factory; enable the three signal
   endpoints; reject non-loopback remote addrs defensively even though the
   listener is bound; bound request bodies (confirm the library's own limits and
   set them explicitly rather than inheriting defaults).
5. `consume.go`: consumer funcs binding only what phase 10 consumes; everything
   else ignored, not errored.
6. `emit.go`: the `Emitter` seam. Wire it **in the command**, not only in tests.
7. `telemetryservice`: unit generation via the phase-04 mechanism,
   `WriteUnit`/`RemoveUnit`, stdio → `~/.openbox/telemetry.log`.
8. Install path in the same proof order: unit → start → prove listening → **then**
   env. Any failure after `WriteUnit` must also remove the unit (`KeepAlive` would
   otherwise restart-loop a daemon nobody was told about).
9. Doctor: add a telemetry block; include a **recording** line (records seen in the
   last N minutes), because reachable ≠ recording.
10. Control test: real command → real receiver → real spool, no fake at either end.

## Pins inherited from phase 10's mapper (2026-08-28)

The mapper exists and is suppressed until elected; these are requirements on the
DAEMON that will call it, recorded here because the mapper has no way to satisfy
them alone.

- **A drop must be countable.** `EventFor` collapses "not an api_request", "no
  session", "no id", "malformed id" and "zero timestamp" into one silent `false`.
  That is correct for the first case and dangerous for the rest: this phase's own
  argument is that erroring on an unfamiliar event NAME would turn upstream drift
  into a lane outage — and id-format drift is the same class, with the same
  compensating control (OD4) also unbuilt. The daemon must count or
  throttled-warn on drops, the shape `gatewayemit`'s emitter already uses. A lane
  that goes quiet because every record now fails validation must not look
  identical to a quiet session.
- **Two validations are already done in the mapper** and must not be re-relaxed:
  `session.id` is charset-checked because every spool consumer turns it into a
  filename (`<session>.jsonl`), and a zero record timestamp is dropped rather
  than formatted into year 0001.

## Todo

- [ ] encoding/endpoint probe + report (config input) — **BLOCKED: needs a listener + live client**
- [x] `telemetry/` module + `go.work` + its own dependency guard test
- [x] dependency tree + binary-size delta measured and recorded — **+16.5 MB, DECIDED (OD5): one binary, mirroring `openbox gateway`**
- [x] loopback-only config + deterministic port (8789; drilled)
- [x] otlpreceiver wired, three signals, bounded reads set explicitly (8MiB body, 10s header)
- [x] consumers bound to consumed fields only
- [ ] emitter wired in production code — seam exists, **no production caller yet** (the WithCapture bug, still open)
- [ ] `telemetryservice` unit via phase-04 mechanism + log path
- [ ] install proof-order + rollback-removes-unit — **BLOCKED: "prove listening" needs a bind**
- [ ] doctor: configured / reachable / recording
- [ ] control test with no fakes — **BLOCKED: needs a bind**
- [ ] default-on posture + **second-invocation** re-run test
- [x] `go test ./...` per module, `-race`, both cross-compiles — green across 13 modules

## Success criteria

- A real desktop session and a real CLI session both produce spooled records.
- Killing the receiver mid-session loses records but never blocks or breaks the
  session (the lane is additive by construction).
- `doctor` distinguishes reachable-but-silent from recording.
- Install failure at any step leaves no unit loaded and no env key written.
- Exactly one new direct dependency family in `telemetry/` (otlpreceiver + its
  collector companions), enumerated by the module's own guard; `gateway/` and
  `decision/` allowlists untouched.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| **otlpreceiver's library API churns** (collector is v0.x; D-GO-1 says latest, no pin) | Read the API from the resolved source at implementation time; the guard test names the module so a breaking bump is a visible, deliberate act | Compile break on a future `go get -u` | Fix forward at the seam (factory + consumers are narrow); never fork the receiver |
| The client emits an encoding/endpoint shape the config didn't enable | Step-1 probe decides config; otlpreceiver accepts both encodings | Receiver reachable but recording silent | Re-run the probe against the installed client version; adjust env values (OD3's expected failure mode) |
| Emitter not wired in production (the `WithCapture` bug) | Control test with no fake at either end | Control test is the only red test | Fix before proceeding; this bug is silent in every other test |
| Beta env keys change or vanish (OD3 accepted this) | Version-pin probes + doctor recording line | Recording goes silent while hooks still fire | Phase 10's OD4 finding fires; investigate client version |
| Port collision with another local service | Deterministic port + stale-owner identification | Bind fails on install | Identify and stop the stale owner; never auto-increment |
| launchd stdio to `/dev/null` hides all diagnostics | Explicit stdout/stderr paths in the unit | Empty `telemetry.log` on a failing daemon | Treated as a bug in the unit, not the daemon |
| 13 env keys conflict with a developer's own | Transactional activation refuses on conflict | Install aborts naming the conflicting key | Correct behaviour — report, do not force |
| The heavy dependency tree leaks beyond `telemetry/` | Own module + own guard; gateway/decision guards unchanged | Another module's go.mod grows collector requires | Cut the import; the module boundary is the control |

## Security considerations

- **Content arrives here.** Prompts, tool inputs/outputs and full model bodies reach
  this process. Nothing egresses in this phase (spool only), but on-disk records are
  as sensitive as `~/.openbox/.env`: create under 0700 dirs, 0600 files.
- Bind loopback only and validate it; an OTLP receiver reachable off-host is an
  unauthenticated content firehose.
- Bound every read — set the library's limits explicitly. An unbounded body read
  on a loopback listener is a local DoS.
- The receiver must never log record bodies — mirror `verbose.go`'s rule
  (method/route/status/duration only).
- Raw-API-body files inherit the same posture; their directory is deleted by
  phase 12's removal command.
- otlpreceiver is the largest third-party surface this product will link. The
  module guard makes its presence enumerable; phase 14 records the accepted
  cost. A future bump is a deliberate review, not `go get -u` housekeeping.

## Next steps

Phase 10 maps these records onto the contract.
