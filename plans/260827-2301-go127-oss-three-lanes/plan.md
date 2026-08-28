---
title: "Go 1.27 + OSS foundation, then Three Lanes, One Pipeline"
description: "One sequence, two stages: raise the Go floor and replace hand-rolled components with maintained libraries, then fold openbox-logger's telemetry + transport observation into shift-left as native Go services under the 2026-08-27 owner rulings."
status: in-progress
priority: P1
effort: ~89h (14 phases: stage A ~36h, stage B ~53h)
branch: feat/tool-content-capture
tags: [go-version, dependencies, oss, refactor, secret-detection, json-schema, launchd, gateway, telemetry, transport-proxy, otel, adr, convergence, single-repo]
created: 2026-08-27
---

# Go 1.27 + OSS foundation, then Three Lanes, One Pipeline

**Merged 2026-08-27** from `260827-2245-go127-and-oss-consolidation` and
`260827-1602-three-lanes-convergence`. The merge is causal, not cosmetic: the
second plan's round-2/3 validations produced the decisions the first plan
executes (D-GO-1, D-OSS-1..3), and left action items requiring the second's
phases to be re-cut against them — done in this merge. One plan, two stages:

- **Stage A — foundation (phases 01–07, ~36h):** raise the Go floor to 1.27.0,
  then replace five hand-rolled components with maintained packages — retiring
  the version-pin scheme and closing a real TOML posture bug.
- **Stage B — convergence (phases 08–14, ~53h):** bring openbox-logger's proven
  capabilities — native OpenTelemetry capture and a scoped TLS transport relay —
  **into this repository as Go services**, closing the gateway's desktop/OAuth
  dead-ends. openbox-logger stays a lab/reference artifact; nothing here depends
  on its Python stack or Docker at runtime (OD2). All new lanes feed the pipeline
  shift-left already has: map → redact → cap → spool → AIP-sign → flush →
  `/evaluate`. No new control-plane table, endpoint, or service.

Sources: [audit-260827-2227](../reports/audit-260827-2227-oss-replacement-shipped-code.md)
(shipped-code audit) ·
[validation-260827-2154](../reports/validation-260827-2154-oss-reuse-vs-from-scratch.md)
(OSS-reuse validation) · proposal
`plans/visuals/260827-1439-three-lanes-one-pipeline.html` (artifact 30d03ca3) ·
evidence run: openbox-logger `20260827T063932Z-225cac`.

Scouting: [gateway lifecycle](scout/scout-01-gateway-service-lifecycle.md) ·
[capture→contract→conformance](scout/scout-02-capture-contract-conformance.md) ·
[replacement seams](scout/scout-01-replacement-seams.md). Library APIs:
[researcher-01](research/researcher-01-jsonschema-gitleaks-apis.md) ·
[researcher-02](research/researcher-02-kardianos-go127-migration.md).
Phase reports land in `reports/`.

## Owner rulings (2026-08-27) — the fixed premises

- **OD1(c)** oversized model-call bodies egress full through the standard
  redact→cap leg; ~95% truncate at 65,536 runes, accepted. No digest/excerpt
  scheme; no local evidence store.
- **OD2** the transport proxy is a **product**, a **native launchd service** (no
  Docker, no mitmproxy at runtime), **one command in / one command out**.
  Formally reverses ADR-0021 §5.
- **OD3** ride the beta surface now (`CLAUDE_CODE_ENHANCED_TELEMETRY_BETA`,
  `OTEL_LOG_RAW_API_BODIES`) with version-pinned probes + doctor silence
  detection.
- **OD4** telemetry silence on an otherwise-active session is a **finding**; the
  late HALT (next-boundary latch) is accepted.

## Decisions — the fixed premises (owner, 2026-08-27)

| ID | Decision |
|---|---|
| **D-GO-1** | Go floor **1.23.0 → 1.27.0** across `go.work` + all 12 modules; every adopted dependency resolves at latest with no pin; `x/term` unpinned (v0.34.0 → latest) |
| **D-OSS-1** | transport proxy CONNECT/TLS/CA front-end → `github.com/elazarl/goproxy` (latest under D-GO-1) — neither Docker nor mitmproxy, OD2 intact |
| **D-OSS-2** | OTLP intake → `go.opentelemetry.io/collector/receiver/otlpreceiver` as a library (latest) — supersedes round 1's vendored-protobuf fallback; handles both encodings |
| **D-OSS-3** | service lifecycle → `github.com/kardianos/service` v1.3.0 (Zlib) — unit writing only; proof-order install, rollback-removes-unit, stdio→file and the activation record stay custom |
| **D-OSS-4** | secret detection → `gitleaks` v8.30.1 (MIT) |
| **D-OSS-5** | JSON Schema validation → `santhosh-tekuri/jsonschema` v6.0.3 (Apache-2.0) |
| **D-OSS-6** | TOML key scan → `pelletier/go-toml/v2` (MIT) |
| **D-OSS-7** | `.env` parsing → `joho/godotenv` (MIT) |
| **D-OSS-8** | atomic writes → `google/renameio/v2` (Apache-2.0) |

TruffleHog excluded on AGPL-3.0 — copyleft on a distributed binary. mitmproxy
stays refused (Python; already OD2).

**Structural consequence (validation round 2):** transport is its **own module**
(`transport/`, in `go.work`) rather than `gateway/transport/`, with its own
dependency guard — goproxy inside `gateway/` would breach `guard_test.go`'s
allowlist and put credential-path code outside the credential scan.

## Non-goals

Replacing the git module (shells out to real `git` deliberately) ·
`client/signing.go` (stdlib Ed25519 + the platform's AIP protocol) · the CLI's
stdlib `flag` usage · the gateway relay engine (byte-identity proven; the
transport front-end wraps it, never forks it) · the hookflow engine and mappers ·
porting the logger's Python CLI or running Docker/mitmproxy in the product
path · changing `activity_id` for ungated classes (D8 is its own decision) ·
in-path modification of LLM traffic (byte-identical forward is load-bearing) ·
joining gateway turns to goal alignment (stays openbox-core#130) · answering
probe A — stage B builds the instrument, not the answer. Stage A changes nothing
about what egresses — no content field, gate or cap moves in phases 01–07.

## Phases

| # | Phase | Depends | Status | Effort |
|---|-------|---------|--------|--------|
| 01 | [Go 1.27 floor raise](phase-01-go-127-floor-raise.md) | — | **done** | 4h |
| 02 | [JSON Schema validator](phase-02-jsonschema-validator.md) | — (floor-independent) | **done** | 5h |
| 03 | [Config parsers & atomic writes](phase-03-config-parsers-and-atomic-writes.md) | 01 | **done** | 5h |
| 04 | [launchd service lifecycle](phase-04-launchd-service-lifecycle.md) | 01 | **done*** | 6h |
| 05 | [Credential-guard scope (ADR)](phase-05-credential-guard-scope.md) | 01 | **done** | 3h |
| 06 | [gitleaks detection engine](phase-06-gitleaks-detection-engine.md) | 01, **05** | **done*** | 10h |
| 07 | [Stage-A docs reconciliation](phase-07-consolidation-docs.md) | 02–06 | **done** | 3h |
| 08 | [ADR-0022 + contract v1.6 + ADR-0021 amendments](phase-08-adr-contract-decision.md) | 01 (02 strongly recommended first) | pending | 4h |
| 09 | [Telemetry receiver daemon (otlpreceiver, loopback)](phase-09-telemetry-receiver-daemon.md) | 04, 08 | pending | 8h |
| 10 | [Telemetry mappers → contract (`:otel:`)](phase-10-telemetry-mappers.md) | 09 | pending | 8h |
| 11 | [Transport proxy as native service (`:proxy:`, goproxy)](phase-11-transport-proxy-service.md) | 04, 10 | pending | 15h |
| 12 | [One-command install/remove + producer election](phase-12-one-command-and-election.md) | 09, 11 | pending | 6h |
| 13 | [Conformance, fixtures & probe-A instrument](phase-13-conformance-fixtures-probe.md) | 10, 11 | pending | 8h |
| 14 | [Coverage matrix & docs reconciliation](phase-14-coverage-and-docs.md) | 09–13 | pending | 4h |

\* Phase 04 is implemented; its real install/uninstall cycle and the gateway.log check need a
machine that can bind a listener and run `launchctl` — see its report.
Phase 06 is implemented and green, with **two open items**: the false-positive soak did not clear the
enforce path (2 false positives from `generic-api-key`), and the mutation drills need a listener the
sandbox denies. See its report.

**Order.** Phase 01 first and **alone** (02 may run beside it — the library
builds at the old floor). Then 03/04/05 in any order; **05 must precede 06** —
gitleaks in `decision/` turns `gateway`'s credential guard red, and a security
test's weakening must not arrive inside the change that needed it. 07 documents
stage A from the phase reports.

Stage B is **serial, by validation ruling:** 08 → 09 → 10 → 11 → 12 → 13 → 14
(13's fixtures can begin as soon as 10 emits bytes). Telemetry before transport —
not parallel: the lower-risk lane proves the shared lifecycle/env generalization
before the CA work starts, and both extend code phase 12 generalizes. Run 02
before 08: the library validator, not the hand-rolled walk, should be what the
three new `oneOf` discriminator branches are stressed on. Phase 09 also inherits
phase 04's settled service-lifecycle mechanics; phase 11 reuses both.

## Acceptance (whole plan)

Stage A — foundation:

1. Every module declares `go 1.27.0`; no pin instruction for `x/term` survives
   anywhere; per-module `GOWORK=off` builds pass.
2. Conformance C1–C41 pass **unmodified** against the library validator, with the
   `x-content-gated` pass still separate.
3. The TOML regression test fails on the old scanner and passes on go-toml;
   `codexMandated` is correct on the new fixture.
4. `~/.openbox/gateway.log` receives real output from a running gateway, asserted
   on the generated plist and on the file.
5. The credential guard is red on a direct unreviewed require (any host), green
   on an indirect one, and its reduction is recorded in an ADR.
6. The three pinned redaction tests pass **unmodified**; both mutation drills
   still go red when their mechanism is deleted; the false-positive soak gates
   the enforce path.

Stage B — convergence:

7. A desktop, subscription-OAuth session produces model-call evidence in
   `governance_events` via telemetry — no `ANTHROPIC_BASE_URL` change.
8. With transport installed, that same session's model calls pass an in-path
   relay (capture live; refusal dormant pending probe A).
9. Exactly one model-call producer emits per session; `activity_id`s never
   collide across `:otel:` / `:gateway:` / `:proxy:`.
10. One command installs+enables everything; one removes everything and a
    system-state diff returns empty (settings restored, services unloaded, CA
    deleted).
11. New mappers pass replay conformance on sanitized real fixtures, asserting
    outbound bytes; `usage.go`'s INV-2 sentinel untouched.
12. All modules green under `-race` + both cross-compiles; docs reconciled at
    both checkpoints (07 and 14).

## The things most likely to go wrong

- **Phase 01 is declaration-only.** If a test's *expectations* need editing,
  real behavior moved across four Go releases — stop and split it out, don't
  re-bless a golden.
- **Phase 04's library may not be able to pin our log path** (kardianos issue
  #281 verified; its fix unread). Step 1 is a gate with three pre-decided
  branches, including **stop and escalate** — `/usr/local/var/log` is not
  acceptable, because silent not-recording is the blindness ADR-0021 closed.
- **Phase 06 can lose coverage while looking like an upgrade.** gitleaks' entropy
  is a per-rule threshold on a regex match, not a standalone high-entropy scan —
  keep the existing generic entropy fallback and measure both directions.
- **Phase 11's spike is a gate.** goproxy must prove byte-identity (no
  `X-Forwarded-For`, no injected `Accept-Encoding`, header order) and per-chunk
  SSE flush against the existing identity suite **before** any service code.
  Cannot stream per-chunk ⇒ stop and report — silent correctness damage outranks
  the feature.
- **Two producers emitting the same turn** halves the evidence silently (core's
  dedupe absorbs one, no error). The election (phase 12) is a correctness
  invariant, not a preference; namespaces make ids disjoint, the election makes
  the count right.

## Verification map

Stage A, phase-level: conformance suite (02) · TOML regression + `.env`
round-trip + mode assertions (03) · generated-plist + real-binary
install/uninstall (04) · seeded guard mutation cases (05) · pinned redaction
tests + mutation drills + false-positive soak (06). **All stack-free.**

Stage B (from proposal §8): V1 replay conformance · V2 desktop+CLI land in
tables (stack) · V3 probe A via transport debug mode · V4 one-producer election ·
V5 volume soak under OD1(c) · V6 activation reversible · V7 one-command in/out
state-diff. Six of seven run stack-free; **V2 needs a live stack** (the standing
D6 limit).

Whole-plan: `-race`, both cross-compiles, per-module `GOWORK=off` build.

## Open questions

1. **Toolchain directive** — add `toolchain go1.27.0`, or leave selection to
   `GOTOOLCHAIN=auto`? Default taken: workspace only. (Phase 01 step 4.)
2. **Phase 05's guard fix shape** — skip `// indirect` (recommended) or another
   shape? It reduces what a security test proves either way; the ADR is the
   record.
3. **Phase 06 abort criterion** — if the soak shows corruption risk, fall back
   to rules-only (embed the rule pack, keep our matcher) or keep the current
   detector? Rules-only assumed.
4. **Phase 04 gate** — resolved at implementation, not now; branches pre-decided.
5. **Phase 11 spike outcome** — pre-decided: goproxy failing byte-identity or
   per-chunk SSE is a stop-and-report, not a workaround.

## Validation history

### Round 1 (three-lanes plan, 2026-08-27) — 7 questions, all resolved

- **Discriminators (now phase 08):** two symmetric fields — `otel_request_id`,
  `proxy_request_id` — each with its own `oneOf` branch. `turnActivityIDFor`
  branches on presence, never on a value. Existing shapes untouched.
- **Election (now phase 12):** automatic precedence **transport > gateway >
  telemetry** — in-path relays outrank a client-asserted lane.
- **OTLP probe fallback:** *superseded in round 2 by D-OSS-2* (the collector
  receiver handles both encodings; the probe informs config, not lane survival).
- **Build order:** telemetry before transport, serial.
- **Telemetry default:** **on once installed** — installing is the opt-in;
  content still rides the existing `content_capture` gate (ADR-0016's
  default-off lesson).
- **Commands (now phase 12):** `openbox init --full` / `openbox init
  --remove-all` — extends the known verb, parallels `--remove-gateway`.
- **Codex rollup bug:** repaired **inside phase 08**, same commit as the two new
  branches.

### Round 2 (OSS reuse, 2026-08-27) — 3 questions, held against counter-evidence

Trigger: *is the proxy being written from scratch when trusted OSS exists, and
what else is?* Findings and decisions: D-OSS-1/2/3 above, the `transport/`
module split, and the spike-first requirement for goproxy. Report:
[validation-260827-2154](../reports/validation-260827-2154-oss-reuse-vs-from-scratch.md).

### Round 3 (audit, 2026-08-27) — version pins retired

Superseded by **D-GO-1** — the Go floor rises to 1.27.0, so every dependency
resolves at latest with no pin; the `x/term` pin is released. Report:
[audit-260827-2227](../reports/audit-260827-2227-oss-replacement-shipped-code.md).
D-GO-1 lands first, alone and green (phase 01), before everything else.

### Merge (2026-08-27)

The two plans were merged into this one. Round 2's re-cut action items are
**done in this merge**:

- [x] phase 08: ADR-0022 records the three adoptions and the go 1.27 floor
      raise (pins retired), and states explicitly that goproxy is neither
      Docker nor mitmproxy (OD2 intact)
- [x] D-GO-1 floor raise is phase 01, blocking, first and alone
- [x] phase 09: otlpreceiver replaces the hand-thin receiver;
      protobuf-fallback ruling marked superseded; probe retained as config input
- [x] phase 11: module moved to `transport/`; goproxy spike FIRST
      (byte-identity + per-chunk SSE against the existing identity suite;
      cannot stream ⇒ stop and report); capture-tee attachment to goproxy's
      hooks stated without buffering `streamTo`'s flush-per-read
- [x] phase 12: kardianos supplies units; custom proof-order/activation record
      retained
- [x] phase 13: byte-identity + SSE conformance added on the goproxy path
- [x] phase 14: dependency story updated (1 external → module-scoped set;
      otlpreceiver's ~98-require tree recorded as an accepted cost)
- [x] effort: three-lanes ~50h → ~53h (transport +3h spike/tee); merged total
      ~89h
