# Phase 04 — Gateway passthrough core (local)

## Context links

- Parent: [plan.md](plan.md) · Gate: [phase-03](phase-03-decisions-and-adrs.md)
- Contract: https://code.claude.com/docs/en/llm-gateway-protocol
- Streaming limits: https://code.claude.com/docs/en/network-config
- Depends on: 03 (probe A shape + P0 auth coverage recorded, ADR-0021 signed)

## Overview

- Date: 2026-08-25 (local-gateway revision)
- Description: an Anthropic-Messages-format reverse proxy that runs on the developer
  machine, forwards byte-identically — Authorization header included — and streams without
  buffering. **Pass-through auth: the gateway holds and resolves no credentials.** No
  capture, no enforcement — those are phases 05 and 06.
- Priority: P1
- Implementation status: pending
- Review status: not reviewed

## Key insights

- **Inspect without modifying is the whole design.** Pass-through auth is that invariant
  applied to the Authorization header: the developer's own credential (subscription OAuth
  or API key, per P0's coverage) relays untouched. The obx_→credential-swap from the
  central design is deleted — with it goes every secret the gateway would have managed.
- **Passthrough is what defeats the treadmill.** Unchanged: open header/body lists; a proxy
  that never transforms is forward-compatible by construction.
- **Localhost binding IS the caller boundary.** The gateway listens on `127.0.0.1` at a
  deterministic port. It needs no caller authentication: a local process calling it gains
  nothing it didn't already have, because pass-through means every caller must present its
  own provider credential.
- **Pings are load-bearing, not noise.** Unchanged: Claude Code counts every relayed byte
  incl. SSE `ping`/comment lines; 180s byte watchdog. Tee, never buffer.
- **The system array is positional.** Unchanged: prepend/reorder/stringify breaks the
  attribution strip and poisons the prompt-cache key.
- **Match on path, not URL** — requests arrive as `/v1/messages?beta=true`.

## Requirements

1. `POST /v1/messages` forwards byte-identical request bodies AND headers upstream —
   `Authorization`/`x-api-key` explicitly included in the identity test.
2. `anthropic-version`, `anthropic-beta`, `anthropic-workspace-id` forwarded unchanged;
   unknown `anthropic-*` / `x-claude-code-*` passed through.
3. Response streamed as it arrives; pings and comment lines relayed unmodified.
4. Error response bodies forwarded unmodified — no envelope.
5. No credential handling: no swap, no injection, no storage. The binary must not read
   provider credentials from any config.
6. `POST /v1/messages/count_tokens`, `GET /v1/models`, `HEAD /api/hello` served.
7. Deterministic `127.0.0.1` listen config; no port scanning; runnable foreground
   (`openbox gateway`) and daemon-friendly (no TTY assumptions — phase 07 wraps it).

## Architecture

New module under `gateway/` with its own `go.mod` (ADR-0011 layout). Reuses `client/`
(phase 05) and `decision/` (phase 05); imports no adapter.

Streaming is a pure relay with a tee into a capture buffer (phase 05). Buffer-then-forward
is forbidden by the watchdogs. Request bodies teed the same way — read once, forward the
exact bytes, keep the copy.

`/v1/models` must serve directly at the base URL: 3s timeout and **any redirect is treated
as failure**, including http→https.

## Related code files

| Path | Change |
|---|---|
| `gateway/` | new module |
| `gateway/proxy.go` | passthrough + SSE tee |
| `gateway/endpoints.go` | messages, count_tokens, models, hello |
| `go.work` | add module (11 → 12) |
| `cli/cmd/openbox/gateway.go` | `openbox gateway --config` |

## Implementation steps

1. Skeleton module + `go.work` entry; CI cross-compiles from day one.
2. `POST /v1/messages` passthrough with a byte-identity test as the first test written —
   headers asserted verbatim, Authorization named explicitly in the assertion.
3. SSE relay: pings and comment lines survive; no-buffering asserted via slow-upstream
   stub (first-byte latency).
4. `count_tokens` + `/v1/models` + `HEAD /api/hello`.
5. Error passthrough test: upstream 400 wording reaches the client unmodified.
6. `system` array identity test: order, position, and array-ness preserved.
7. Negative test: the binary makes zero credential reads — no env, no file (grep-level
   guard test on the module, same spirit as `TestOnlyTheRegistryImportsAdapters`).

## Todo

- [ ] Module + go.work + CI cross-compile
- [ ] Byte-identity test incl. Authorization verbatim (first test written)
- [ ] SSE relay with ping forwarding
- [ ] No-buffering assertion via slow-upstream stub
- [ ] count_tokens, /v1/models, HEAD /api/hello
- [ ] Error-body passthrough unmodified
- [ ] `system` array identity
- [ ] No-credential-read guard test
- [ ] 12 modules green under `-race`, both cross-compiles

## Success criteria

- Forwarded request bytes byte-identical to received, asserted not inspected by eye —
  Authorization included.
- `system` array position, order and shape unchanged; attribution block still first.
- Pings relayed within 1s of upstream emission.
- Time-to-first-byte through the gateway within 50ms p95 of direct (localhost hop).
- An unknown `anthropic-beta` value passes through untouched.
- Upstream error wording reaches the client unmodified.
- The module contains no credential resolution path.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| Accidental body mutation via JSON round-trip | never unmarshal-then-remarshal the forwarded copy | byte-identity test fails | fix immediately; this invariant is the design |
| Buffering creeps in | slow-upstream stub asserts first-byte latency | TTFB grows with response size | revert to streaming relay |
| Pings stripped by a framework layer | explicit relay test for comment lines | stream aborts in long thinking pauses | hand-roll the SSE relay |
| `/v1/models` redirects | serve directly at base URL | picker silently loses gateway models | remove redirect; discovery failure is silent |
| Auth handling creeps back in | no-credential-read guard test | guard test fails on a diff | reject the diff; pass-through is a decided item |
| Non-loopback binding | deterministic 127.0.0.1 config; test rejects 0.0.0.0 | gateway reachable off-machine | fix before merge; loopback IS the caller boundary |

## Security considerations

- The gateway holds **no secrets**. Its threat surface is the relay itself: never log
  request headers at any level (the developer's live credential transits every request).
- Loopback-only binding is a hard requirement, tested — an exposed listener would relay
  arbitrary traffic to Anthropic under whatever credential a remote caller presents, and
  leak captured content locally.
- No TLS interception anywhere. If a CA appears in a diff, it is out of scope.

## Next steps

Phase 05 attaches the tee to the pipeline and adds identity + account evidence.
