# Validation — OSS reuse vs from-scratch (plan 260827-1602-three-lanes-convergence)

Date: 2026-08-27 · Questions: 3 (asked twice — once cold, once against evidence;
owner held all three answers) · Mode: prompt

> **2026-08-27:** the validated plan was merged into
> `plans/260827-2301-go127-oss-three-lanes/` (together with
> `260827-2245-go127-and-oss-consolidation`); this report's action items were
> applied to that plan's phases 08–14 during the merge.

Prompt: *"plan mentions writing the proxy from scratch rather than using existing
ready product/package/library (e.g. mitmproxy like openbox-logger). Find what other
components are written from scratch and whether trusted OSS exists."*

## 1. Audit — what the plan actually writes from scratch

| Component | Plan's stance | Written from scratch? | Trusted OSS exists |
|---|---|---|---|
| Streaming relay, capture, tee, SSE per-chunk flush | **reuse** shipped `gateway/` | No | (would be *displaced*, not filled) |
| Secret detection / redaction | **reuse** `decision/` | No | — |
| AIP signing, spool, flush | **reuse** `client/` | No | — |
| CONNECT + TLS terminate + **CA/leaf mint** + host allowlist | **new**, ~300–400 loc | **Yes** | goproxy, martian/mitm, mkcert-class |
| OTLP loopback receiver + decode | **new**, thin | **Yes** (partial) | OTel Collector `otlpreceiver`, `proto/otlp` |
| launchd unit / install / rollback | **mirror** shipped `gatewayservice` | No (copy of ours) | kardianos/service |
| Contract mappers `:otel:`/`:proxy:` | **new** | No — is the openbox contract | none exists |
| Election, doctor, env activation | **new** | No — product logic | none exists |

Correction to the premise: the proxy **engine** is not from scratch. Only the
CONNECT/cert front-end is. `grep` confirms zero `CONNECT`/`tls.Server`/`x509`/
`Hijack` in `gateway/` today — that surface is genuinely new. The relay it would
attach to is shipped and tested.

mitmproxy specifically: Python product, cannot embed in the static Go binary;
already refused by **OD2** (2026-08-27). Not revisited.

## 2. Counter-evidence presented to the owner (held anyway)

- `gateway/proxy.go:20–35` (package doc) records that `httputil.NewSingleHostReverseProxy`
  **was measured** to fail the byte-identity test — appended `X-Forwarded-For`,
  injected `Accept-Encoding: gzip`. A general net/http proxy stack owning the
  round-trip is the same class of risk.
- `gateway/guard_test.go:194–241` enumerates a two-module allowlist and fails on a
  third, and its whole point is that credential-handling code stays inside the
  scan scope.
- Collector-as-library is ~98 module requires for three loopback JSON routes.

Owner held all three after seeing this. Recorded as decided (review rules: an
owner decision is not reversed by an abstract concern).

## 3. Decisions (owner, 2026-08-27, second validation round)

- **D-OSS-1 — proxy: adopt `github.com/elazarl/goproxy`** for phase 04's
  CONNECT/TLS/CA front-end. Maintained (v1.9.0, 2026-08-06).
- **D-OSS-2 — OTLP: adopt `go.opentelemetry.io/collector/receiver/otlpreceiver`**
  as a library for phase 02. Maintained (v0.159.0, 2026-08-17).
- **D-OSS-3 — service: adopt `github.com/kardianos/service`** for phases 02/04/05
  unit lifecycle. Maintained (v1.3.0, 2026-07-06).

## 4. Hard consequences — verified, must be handled

### 4.1 Language floor (blocking at latest versions)

Repo holds **go 1.23.0** across all twelve modules and `go.work`; local toolchain
go1.23.4. `x/term` is pinned at v0.34.0 for exactly this reason (CLAUDE.md).
Go ≥1.21 makes a dependency's `go` directive a hard build requirement.

| Module | latest | floor | 1.23-compatible pin |
|---|---|---|---|
| goproxy | v1.9.0 | **go 1.24.0** ✗ | **v1.8.2** (go 1.23.0) ✓ |
| collector/otlpreceiver | v0.159.0 | **go 1.25.0** ✗ | **v0.120.0** (go 1.23.0) ✓ |
| kardianos/service | v1.3.0 | go 1.23.0 ✓ | v1.3.0, no pin needed |
| proto/otlp (if needed) | v1.11.0 | go 1.25.0 ✗ | v1.5.0 (go 1.22.0) ✓ |

**Action:** pin goproxy v1.8.2 and otlpreceiver v0.120.0, with the `x/term`
treatment — reason in the require block, and `go mod tidy` / `go get -u` must not
be allowed to bump them. Otherwise the floor rises for all twelve modules and both
cross-compiles. Alternative if the owner prefers latest: raise the floor
deliberately as its own decision (not a side effect of this plan).

### 4.2 Credential guard breach (structural, has a clean fix)

Phase 04 as written puts the proxy in `gateway/transport/` — *inside* the gateway
module. goproxy there breaches `guard_test.go`'s allowlist **and** puts
credential-path code outside the guard's scan.

**Action:** make transport its **own module** (`transport/`, in `go.work`) with its
own `guard_test.go` allowlist naming goproxy — the same quarantine the plan already
uses for `telemetry/`. Keeps `gateway/` at zero external deps; no reversal of D-OSS-1.

### 4.3 Byte-identity + SSE must be re-proven on the goproxy path

Byte-identical forward is load-bearing (reordered `system` poisons the prompt cache
silently; stripped `anthropic-beta` disables a capability silently), and "never
buffer a response" is invariant #2 (180s watchdog; SSE pings keep long thinking
alive).

**Action:** spike goproxy **first thing in phase 04**, before any other work —
assert no `X-Forwarded-For`, no injected `Accept-Encoding`, `DisableCompression` on
its RoundTripper, header order preserved, and per-chunk SSE flush with first-byte
latency. Run the existing byte-identity suite against the goproxy path. If SSE
buffers and cannot be configured out → **stop and report**; that is silent
correctness damage, the phase's own stop condition.

### 4.4 Capture tee across goproxy's hooks

The shipped `Captured`/`captureSink`/`streamTo` path assumes this package owns the
round-trip. Under goproxy's `OnRequest`/`OnResponse` the request copy must stay
re-readable and the response tee must not become a buffer.

**Action:** phase 04 must state how the tee attaches to goproxy hooks without
displacing `streamTo`'s flush-per-read, or explicitly adopt goproxy's streaming and
re-assert both invariants.

### 4.5 Dependency-tree footprint (accepted, document it)

otlpreceiver pulls ~98 module requires (gRPC, confmap, pdata) into a repo with one
external dependency today. Binary stays single (library import), one-command shape
survives. **Action:** record the size in that decision as an accepted cost; keep
it inside `telemetry/` only, guard-tested.

### 4.6 kardianos/service does not cover the hard parts

Proof-order install (unit → start → prove listening → **then** env), rollback-
removes-unit, stdio→file (launchd defaults to `/dev/null`), and the activation
record all remain custom on top. **Action:** phases 02/04/05 keep those; kardianos
supplies unit writing only. Windows/Linux service capability comes along free —
does not change the "Windows build-verified only" claim without a real run.

## 5. Plan revisions required (phase files NOT modified — this is the list)

- **phase-01** — that decision must record all three adoptions, the two version pins with
reasons, the floor constraint, and D-OSS-1 as a second amendment to that decision's
§5 area (OD2 said "no mitmproxy/Docker"; goproxy is neither, but that decision must
say so explicitly rather than leave it inferred).
- **phase-02** — replace the hand-thin receiver + "vendor a protobuf decoder"
  fallback with otlpreceiver v0.120.0; validation-round-1's protobuf fallback
  ruling is **superseded** (the collector handles both encodings, so the
  http/json probe stops being a lane-risk and becomes an optimization). Keep the
  probe; it now informs config, not survival.
- **phase-04** — new module `transport/` not `gateway/transport/`; goproxy front-end;
  **spike-first** step order; keep CA lifecycle, allowlist, dormant refusal,
  injection mode as written (goproxy supplies cert minting, not policy).
- **phase-05** — kardianos for units; keep proof-order + activation record custom.
- **phase-06** — add byte-identity + SSE conformance on the goproxy path.
- **phase-07** — `COVERAGE.md`/`data-and-privacy.md`: the CA statement stands; add
  the three dependencies and their pins to the dependency story (repo goes from 1
  external dep to 4 module-scoped ones).
- **plan.md** — effort up: phase 02 −2h (receiver bought) but +2h integration;
  phase 04 +3h (spike + tee re-work) net; total ~50h → ~53h.

## 6. Recommendation

**Revise before implementing.** Not because the decisions are wrong — they are the
owner's and recorded — but because three of them change phase inputs materially
(module layout, pinned versions, spike-first ordering) and one supersedes a
ruling from validation round 1. Implementing against the current phase text would
build the guard breach in 4.2 and discover 4.3 late.

Order: phase-01 decision record first (it now carries three adoptions + two pins),
then re-cut 02/04/05, then 06/07.

## Unresolved questions

1. **Floor:** accept the two version pins (recommended, `x/term` precedent), or
   raise the repo to go 1.24/1.25 deliberately? Pins chosen by default here.
2. **4.3 stop condition:** if goproxy cannot stream SSE per-chunk, is the fallback
   the stdlib front-end (plan as originally written) or does phase 04 stop
   entirely? Assumed: fall back to stdlib front-end, since the CA/allowlist work is
   independent of the round-trip engine.
3. **`transport/` as its own module** is my structural fix for 4.2 — it preserves
   D-OSS-1 without touching `gateway/`. Confirm, or accept widening the gateway
   guard allowlist instead (weakens the credential-scan boundary on the path that
   carries the developer's live credential).
