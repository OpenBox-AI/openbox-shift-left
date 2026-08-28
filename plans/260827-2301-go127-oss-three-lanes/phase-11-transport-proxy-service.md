# Phase 11 — transport proxy as a native service (`:proxy:`, goproxy)

## Context links

- Parent: [plan.md](plan.md) · Depends: [phase-10](phase-10-telemetry-mappers.md)
  (contract from [phase-08](phase-08-adr-contract-decision.md); sequenced after the
  telemetry lane by validation ruling 2026-08-27 — not parallel) and
  [phase-04](phase-04-launchd-service-lifecycle.md) (service lifecycle)
- Scout: [scout-01](scout/scout-01-gateway-service-lifecycle.md)
- Rulings: **OD2 (2026-08-27)** — product, not lab; native service, not Docker;
  one command in, one command out. Formally reverses ADR-0021 §5.
  **D-OSS-1** — the CONNECT/TLS/CA front-end is `github.com/elazarl/goproxy`
  (latest under D-GO-1), not hand-rolled.

## Overview

- Date: 2026-08-27 · Priority: P1 · Effort: 15h (12h + 3h goproxy spike/tee,
  validation round 2)
- Implementation status: pending · Review status: pending
- The in-path point for model calls the base-URL gateway cannot reach: the desktop
  app and any client that honours proxy env but not `ANTHROPIC_BASE_URL`.

## Key insights

- **This is a deliberate, owner-ruled reversal of a standing ADR.** ADR-0021 §5
  rejected MITM because a substituting gateway bought the same assurance; §8 then
  measured that it does not (desktop ignores base-url). The premise changed; the
  ruling followed. Phase 08 records it — the code must not be the only place the
  reversal exists. goproxy keeps OD2 intact: a Go library compiled into our own
  binary, neither Docker nor mitmproxy.
- **The relay half already exists; only the front end is new.** `gateway/`'s
  `Captured`, `CaptureRequest`/`Complete`, `credentialFingerprint`, `captureBody`
  (redact → cap), `captureSink`, `streamTo` (per-chunk SSE flush), `Decide` and
  `WriteRefusal` are all transport-agnostic. goproxy supplies what `gateway/` has
  zero of today: `CONNECT` handling, TLS termination, CA/leaf minting. A parallel
  capture implementation is exactly the copy-paste drift the repo's core rule
  forbids.
- **`transport/` is its own module** (validation round 2, structural consequence):
  goproxy inside `gateway/` would breach `guard_test.go`'s two-entry allowlist and
  put credential-path code outside the credential scan. Consequence to own
  explicitly: transport reuses gateway's capture path **across a module
  boundary**, so `gateway/` exposes a small, deliberate, exported seam (the
  capture/gate/refusal surface transport needs — nothing more). Widening
  gateway's exported surface is reviewed at gateway's guard and named in the
  phase report; **forking the capture path instead is forbidden**.
- **The spike is a gate, not a warm-up** (validation round 2). Before any service
  code: prove goproxy forwards **byte-identically** — no `X-Forwarded-For`
  injection, no injected `Accept-Encoding` (Go's transport adds `gzip` when the
  caller didn't), header order preserved — and **streams SSE per-chunk**, driven
  against the existing gateway identity suite. Cannot stream per-chunk ⇒ **stop
  and report**; silent correctness damage outranks the feature.
- **The capture tee must not buffer.** State in code how the tee attaches to
  goproxy's response hooks: the response body is teed through a streaming reader
  into `captureSink` while `streamTo`'s flush-per-read behavior reaches the
  client unchanged. Any goproxy helper that reads the whole body to make it
  re-readable is disqualified on this path.
- **Interception must be allowlisted to one host.** Every other `CONNECT` is blind-
  tunnelled, unmodified and uninspected. This is the safeguard that makes the
  reversal defensible.
- Byte-identical forward stays load-bearing: a reordered `system` array poisons the
  prompt cache silently, a stripped `anthropic-beta` disables a capability silently.
- Refusal shares probe A with the gateway. Do not invent a second refusal shape.

## Requirements

1. `transport/` — a new module (in `go.work`) wrapping goproxy as a
   `CONNECT`-capable TLS-intercepting relay:
   - allowlist: intercept `api.anthropic.com` only; blind-tunnel everything else;
   - byte-identical forwarding of intercepted requests and responses;
   - capture via the existing `Captured` path (through gateway's exported seam),
     emitted under `:proxy:`;
   - gate seam wired to the same `Decide`/`WriteRefusal` (dormant until probe A);
   - its own `guard_test.go` allowlist (goproxy + gateway + client + decision),
     phase 05's per-module boundary.
2. CA lifecycle stays ours (stdlib `crypto/x509`): generate on install into
   `~/.openbox/`, 0600 key, 0644 cert, **never** added to the system trust store;
   delete on removal. goproxy consumes this CA for leaf minting; whether leaves
   are minted by goproxy's own cache or ours is decided at the spike and recorded.
3. Service lifecycle via the phase-04 mechanism, stdio → `~/.openbox/transport.log`.
4. Env activation of the proxy keys (proxy URL, no-proxy exclusions, the extra-CA
   path, and the client's cert-store preference) through the shared transactional
   mechanism (phase 12).
5. `openbox doctor`: configured / reachable / recording / **CA present and trusted
   by the client only**.
6. A debug/injection mode: substitute a synthetic response for a matched request.
   This is probe A's instrument (phase 13) and must be off by default and refuse to
   run unless explicitly flagged.

## Architecture

```
client (CLI or desktop, proxy env set)
  └─ CONNECT api.anthropic.com:443 ──► transport service (loopback, goproxy engine)
        ├─ host allowlisted   → terminate TLS with leaf signed by project CA
        │     └─ Captured → gate (dormant) → byte-identical forward → streaming tee → emit :proxy:
        └─ host not allowlisted → blind tunnel, no inspection, no capture
```

Reused from `gateway/` (through its exported seam): `Captured`/`CaptureRequest`/
`Complete` (capture.go), `credentialFingerprint`, `captureBody` (redact→cap),
`captureSink`, `streamTo` semantics, `Decide`/`Decision` (gate.go),
`WriteRefusal` (refuse.go), `vlog` (verbose.go).
Supplied by goproxy: CONNECT handling, TLS termination, leaf minting/caching.
Ours in `transport/`: host allowlist, CA lifecycle, the streaming tee binding
goproxy's hooks to the capture path, config, service wiring.

## Related code files

- new: `transport/` module — `proxy.go` (goproxy wiring), `ca.go`,
  `allowlist.go`, `config.go`, `guard_test.go` + tests; `transport/go.mod`
- new: `cli/internal/transportservice/`, `cli/internal/transportcheck/`
- edit: `go.work` (add `transport/`)
- edit: `gateway/` — export the deliberate capture/gate seam (smallest surface
  that serves transport; reviewed at gateway's guard)
- edit: `client/` — `:proxy:` span/emit path (mirrors `client/gatewayspan.go`)
- edit: `cli/cmd/openbox/main.go`, `doctor.go`
- reference: `gateway/capture.go:107–251`, `gateway/proxy.go:447–547`,
  `gateway/gate.go`, `gateway/refuse.go`, `gateway/config.go:40–74`

## Implementation steps

1. **Spike gate — before any service code.** `go get github.com/elazarl/goproxy`
   at latest; drive it against the existing gateway identity suite: header order,
   no `X-Forwarded-For`, no injected `Accept-Encoding`, `anthropic-beta`
   untouched, `system` array order, SSE chunk boundaries and first-byte latency.
   Record results in the phase report. Pre-decided branches: passes → proceed;
   **cannot stream per-chunk or mutates bytes irreparably → stop and report**
   (owner decision point; do not hand-roll a replacement inside this phase).
2. In the same spike, prototype the **streaming tee**: attach to goproxy's
   response hook with a `TeeReader`-shaped pipe into `captureSink`, assert
   flush-per-read reaches the client (first-byte latency unchanged) while the
   capture receives the full body. Decide goproxy-cache vs our cache for leaf
   minting here.
3. Create the `transport/` module + `go.work` entry + its `guard_test.go`;
   measure and record the dependency/binary delta (the phase-06/09 discipline).
4. `ca.go`: generate CA (P-256, ~2y), persist 0600/0644, refuse to run if the CA
   is world-readable; hand it to goproxy.
5. `allowlist.go`: exact-host matcher, no wildcards, no regex; unit-test that a
   lookalike host (suffix/prefix/unicode-confusable) does **not** match.
6. `config.go`: `Config{Addr, Allowlist, Upstream}` with loopback-only `Validate()`,
   port distinct from gateway and telemetry.
7. `proxy.go`: goproxy wiring — allowlisted hosts TLS-terminate and run
   capture/gate/forward/tee through gateway's exported seam; everything else
   blind-tunnels with no inspection. Export the gateway seam in the same change,
   smallest surface possible.
8. Emit under `:proxy:` reusing the observed-span attribute builder (keeps
   `http_status_code` spelling correct by construction).
9. Wire the gate seam but leave refusal dormant; assert dormancy with a test so it
   cannot be enabled by accident before probe A.
10. `transportservice`: unit via the phase-04 mechanism, log path, install
    proof-order, rollback removes unit.
11. Doctor block including a CA-scope line.
12. Injection mode behind an explicit flag, refusing to start otherwise.
13. Cross-compile checks; Windows is build-verified only (no CA trust story there).

## Todo

- [ ] **spike gate passed and recorded** (byte-identity + per-chunk SSE), or
      stopped and reported
- [ ] streaming tee prototyped: capture without buffering, first-byte latency held
- [ ] `transport/` module + go.work + own guard test; dependency delta recorded
- [ ] CA generate/persist/permission-refusal; goproxy consumes it
- [ ] exact-host allowlist + confusable negative tests
- [ ] loopback-only config, distinct port
- [ ] CONNECT + TLS terminate + blind tunnel fallback (goproxy)
- [ ] gateway capture/gate seam exported (smallest surface), reused — no fork
- [ ] `:proxy:` emit path + span attributes
- [ ] gate wired, refusal dormant + dormancy test
- [ ] service unit via phase-04 mechanism, log, install proof-order, rollback
- [ ] doctor block incl. CA scope
- [ ] injection mode, off by default
- [ ] `-race` + both cross-compiles

## Success criteria

- A desktop session's model calls are captured with real provider request ids.
- Bytes forwarded are identical: header order, `anthropic-beta`, `system` array
  order, and SSE chunk boundaries preserved (asserted, not assumed — on the
  goproxy path, not just the old gateway path).
- A non-allowlisted host is tunnelled with zero capture and zero TLS interception.
- Removing the service deletes the CA; no system trust store entry ever existed.
- `gateway/`'s guard still passes with its two-entry allowlist — goproxy appears
  only in `transport/go.mod`, enumerated by transport's own guard.
- Capture adds no buffering: streaming responses reach the client per-chunk with
  capture enabled.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| **goproxy mutates bytes** (injected `X-Forwarded-For`/`Accept-Encoding`, header reorder) | Spike gate against the identity suite before any service code | An identity assertion fails in the spike | Pin down the mutation source; if not configurable away, **stop and report** (pre-decided) |
| **goproxy cannot stream SSE per-chunk** | Spike gate; first-byte latency + chunk-boundary assertions | Responses arrive in one lump | **Stop and report** — owner decision point; do not hand-roll a replacement inside this phase |
| Capture tee buffers the body | Streaming-tee prototype in the spike; latency assertion stays in the suite | First-byte latency grows with capture on | Rework the tee; a buffering tee is disqualified |
| **CA compromise = impersonation of the provider to this machine** | 0600 key, single-host interception, never system trust, deleted on removal, refuse to run on loose permissions | Key readable by group/other | Refuse to start; regenerate |
| Response mutation poisons the prompt cache or drops a capability, silently | Byte-identity assertions on header order, beta headers, `system` order, chunk boundaries | Cache-read tokens collapse; a capability stops working mid-session | **Stop** — silent correctness damage outranks the feature |
| **Assumption: the client honours proxy env for its model calls.** Measured for the logger's stack; not yet for every client/auth mode | Verify per client early in the phase | No `CONNECT` arrives from a client while it works normally | **Adjust**: that client stays telemetry-only; state it in COVERAGE.md rather than averaging |
| Refusal enabled before probe A | Dormancy test | Dormancy test red | Stop — a wrong refusal shape silently disables a capability for the session |
| Proxy env displaces a corporate proxy | First-writer-wins prior record, restored on removal (the `ANTHROPIC_BASE_URL` precedent, generalized in phase 12) | Developer's corporate proxy stops working | Restore from the activation record; treat as a P0 |
| Gateway's exported seam grows beyond what transport needs | Smallest-surface rule; reviewed at gateway's guard; named in the phase report | Exported API nobody imports | Cut it; the seam is for transport, not a public SDK |
| Windows has no CA trust path here | Build-verify only; document | — | None; stated limit |

## Security considerations

- This phase creates the highest-value secret this product has held on a developer
  machine after the signing key: **a CA that can impersonate the provider to this
  host**. It lives under the same trust boundary as `~/.openbox/.env` (ADR-0015):
  anything running as the developer can read it, including the agent being governed.
  Say so in `docs/data-and-privacy.md` (phase 14) — do not let a doc imply otherwise.
- Single-host interception is the bound that makes the ruling defensible. Widening
  the allowlist is a separate decision, not a config tweak.
- Credential headers are redacted **by key name before the content gate is
  consulted** (the existing capture path already does this — reuse it, do not
  reimplement).
- Blind-tunnelled hosts must be provably uninspected: assert no capture record is
  produced for them.
- The injection mode can fabricate provider responses. It must be impossible to
  enable accidentally and must never ship enabled.
- goproxy sits on the credential path (it sees provider auth headers in the
  clear after TLS termination). That is why it lives in a module with its own
  guard and why its version moves only as a deliberate, reviewed act — D-GO-1's
  "latest, no pin" means reviewed-at-latest, not auto-bumped.

## Next steps

Phase 12 makes install/removal one command each and elects the producer.
