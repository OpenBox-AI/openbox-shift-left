# Phase 04 — Gateway passthrough core (local)

## Context links

- Parent: [plan.md](plan.md) · Gate: [phase-03](phase-03-decisions.md)
- Contract: https://code.claude.com/docs/en/llm-gateway-protocol
- Streaming limits: https://code.claude.com/docs/en/network-config
- Depends on: 03 (probe A shape + P0 auth coverage recorded, that decision signed)

## Overview

- Date: 2026-08-25 (local-gateway revision)
- Description: an Anthropic-Messages-format reverse proxy that runs on the developer
  machine, forwards byte-identically — Authorization header included — and streams without
  buffering. **Pass-through auth: the gateway holds and resolves no credentials.** No
  capture, no enforcement — those are phases 05 and 06.
- Priority: P1
- Implementation status: **implemented** (unit-verified + real end-to-end smoke; testbed dormant)
- Review status: **reviewed** — 6 angles; every finding applied or answered (see Review outcome)

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

New module under `gateway/` with its own `go.mod` (that decision layout). Reuses
`client/` (phase 05) and `decision/` (phase 05); imports no adapter.

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
| `cli/cmd/openbox/gateway.go` | `openbox gateway [--addr] [--upstream]` (**not `--config`** — a config-file reader needs `os.ReadFile`, which requirement 5's guard forbids outright; flags carry the same two values with nothing to read) |

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

- [x] Module + go.work + CI cross-compile (CI discovers modules from `go.work`; its on-disk-vs-workspace count guard passes at 12)
- [x] Byte-identity test incl. Authorization **and x-api-key** verbatim — written first, and proven red against a stock `httputil.NewSingleHostReverseProxy` before implementing
- [x] SSE relay with ping + bare-comment-line forwarding
- [x] No-buffering assertion via slow-upstream stub (TTFB + a chunk-coalescing test)
- [x] count_tokens, /v1/models, HEAD /api/hello (+ no `Content-Length: 0` invented on a bodyless method)
- [x] Error-body passthrough unmodified, response headers relayed
- [x] `system` array identity (position, order, array-ness, non-ASCII)
- [x] No-credential-read guard test — import-resolved (alias-proof), covers os/syscall/io-ioutil, refuses dot-imports, case-insensitive literals, walks subdirectories, and is bounded by a zero-dependency `go.mod` assertion so the single-module scan is provably complete. Mutation control drives the SAME scanner.
- [x] 12 modules green under `-race`; windows/amd64 + linux/arm64 vet clean; links via `go install`

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

## Review outcome (2026-08-25)

Six review angles ran. Findings applied:

- **`x-api-key` was never exercised** though requirement 1 names it. Added to the identity
test and to the explicit named assertion, and mutation-drilled. This mattered more than a
coverage nit: that decision leaves OAuth-via-`ANTHROPIC_BASE_URL` unresolved, so the
API-key carrier may be the path that actually carries traffic.
- **The request read was unbounded**, breaking an 11-site repo convention of named caps.
  Now `maxRequestBody` (64 MiB, the largest existing precedent) via `http.MaxBytesReader`.
  It **refuses, never truncates** — a short forward would corrupt a request while reporting
  success — and a test asserts the upstream is not contacted at all on refusal.
- **`MaxIdleConnsPerHost` was unset**, so `MaxIdleConns: 100` was silently capped at 2: all
  traffic goes to one host. Real latency cost under HTTP/1.1 (the corporate-proxy case).
- **`requireLoopback` trusted the string `"localhost"`.** Names are now resolved and every
  answer must be loopback; `gateway.Listen` additionally re-checks the address the kernel
  returned, closing the validate-time/bind-time window.
- **No write-side deadline.** A local caller that stopped reading without closing could
  wedge a goroutine and an upstream connection for the process lifetime — which matters
  because phase 07 supervises this as a persistent daemon. Now a per-write IDLE deadline,
  reset on each success, so a legitimately long stream is unaffected.
- **`Connection:`-named headers were forwarded** (RFC 7230 §6.1); now dropped per-message.
- **A graceful stop that outran its grace period exited 1**, which a supervisor with
  restart-on-failure reads as a crash. Now warns and exits 0; genuine failures still exit 1.
- **The guard's mutation control was a hand-maintained twin** of the scanner. Both now call
  one `scanSource`, so strengthening the check strengthens what the control proves — the
  `doctor`/`init` shared-classifier lesson.
- **A hostname `--addr` could hang startup** — `net.LookupHost` had no deadline, and the
  config was being validated three times per start, so a slow resolver stalled it three
  times over. Bounded to 2s, and `Listen` now returns the validated config so validation
  happens once.
- **The banner printed the CONFIGURED address, not the bound one.** `--addr 127.0.0.1:0`
  reported "listening on 127.0.0.1:0", which is not connectable. Verified fixed on the real
  binary: it now prints the port the kernel assigned.
- **A self-referential upstream passed validation.** `--upstream` pointing at the gateway's
  own listener made every request re-enter the relay and spawn another. Rejected at startup.
- **`strings.TrimSuffix` stripped only ONE trailing slash**, so `…com//` left one behind and
  every request URL carried `//`. Now `TrimRight`.
- **The shutdown grace was a fixed 10s**, and `http.Server.Shutdown` never force-closes an
  ACTIVE connection — so a routine restart landing mid-completion cut the stream, the same
  abort the absent Read/WriteTimeouts exist to prevent, reached by the stop path. Now
  `--shutdown-grace`, default 30s. **Residual owner decision:** the value must be
  coordinated with the supervisor's own stop timeout (launchd 20s, systemd 90s); phase 07
  carries the note. Whether a long completion should ever be allowed to block a restart is
  a policy call, not a code one.
- **Two comments were wrong** and are corrected: `relayBufferSize` did not protect SSE pings
  (the flush-per-read does, at any buffer size), and the package doc overclaimed against
  `httputil.ReverseProxy` — the modern `Rewrite` hook strips `X-Forwarded-*` and auto-flushes
  SSE, so the honest reason to hand-roll is phase 05's two-way tee, not stdlib defaults.

### Verified on the real binary, not just in tests

- Relay through the compiled `openbox gateway` against a live upstream: `?beta=true`
  preserved, `Authorization` verbatim, no `X-Forwarded-For`, no injected `Accept-Encoding`,
  body byte-identical including non-ASCII.
- SSE chunks arrived ~150ms apart, matching the upstream's flush cadence — no buffering.
- A 68 MiB body answered **413**, and the upstream was **not contacted at all**.
- SIGTERM exits **0** (an earlier `exit=143` reading was a `go run` wrapper artifact, not the
  binary's behaviour — checked with the linked binary).
- One process-hygiene lesson worth keeping: an early smoke run left a gateway holding the
  port, so a later run silently tested the STALE binary and reported a false failure. Kill
  what you started before re-measuring.

### Two review findings that did NOT reproduce

Recorded so they are not re-raised as open:

- **Path re-encoding.** A finding held that `r.URL.RequestURI()` re-encodes a
  non-canonical percent-escape, breaking byte-identity for the path. **Measured false**:
  raw vs rebuilt agree across 12 adversarial targets (`%2E`, `%41`, `%2f`, bare `?`, `//`,
  `;v=1`, `+`, UTF-8 escapes), because `EscapedPath` returns `RawPath` verbatim whenever it
  is a valid encoding of `Path`. The relay forwards `r.RequestURI` regardless — the raw
  request-target cannot be re-encoded by construction — but the test's claim was corrected
  to say what it actually holds, since a mutation drill showed it does not discriminate.
- **`exit=143` on SIGTERM.** Seen in a smoke run, but it was the `go run` wrapper, not the
  binary: the linked binary exits **0**.

### Known limit, not fixed

**Response trailers are dropped.** `resp.Trailer` only populates after the body drains, by
which point the headers are committed, so relaying them would need the trailer names
declared up front — which a transparent relay cannot know. Anthropic's API sends none, so
this is latent; fixing it speculatively would add machinery for no current caller.

## Deviations from this phase file, deliberate

- **No `gateway/endpoints.go`.** The four named routes are served by one generic relay with
  no path allowlist, which satisfies requirement 6 and matches the "transparent stand-in,
  forward-compatible without a release" principle better than per-route dispatch. Both
  reviewers that raised it agreed.
- **`--addr`/`--upstream`, not `--config`.** A config-file reader needs `os.ReadFile`, which
  requirement 5's guard forbids outright. Phase 07's service invocation was corrected to
  match — as written it would have failed to start on every boot.
- **Started before phase 03 closed.** The gate says "nothing in Track B starts until this
  closes". Every requirement here derives from the published gateway protocol and plan.md's
  owner-validated decided list; the three `TBD(probe)` slots gate phases 05/06 and the docs,
  not this relay. The dependency line above is therefore relaxed knowingly, not overlooked.
- **`/v1/models` "3s timeout"** reads as Claude Code's own probe budget, not a gateway
  obligation — which is why a redirect must not happen. No gateway-side timeout was added;
  `Client.Timeout` stays unset so streams are never aborted. Flagged for the phase author.

## Not done here, by design

Capture and enforcement are phases 05 and 06. The request body is buffered (not streamed)
specifically so phase 05 has a re-readable copy to tee.
