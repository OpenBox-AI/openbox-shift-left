# Phase 11 — the transport lane (`:proxy:`), implemented

**Date:** 2026-08-29 · **Branch:** `feat/tool-content-capture` ·
**Commits:** `0ac3f0d`, `3507c1e`, `a0089ca` ·
**Host:** bind-DENIED sandbox (`listen tcp 127.0.0.1:0: operation not permitted`)

## Verdict

**The lane is implemented and records end to end, proven without a socket.** A
CONNECT to the allowlisted host is TLS-terminated with the project CA and served
by the **existing** `gateway.Gateway`; the capture reaches a real spool file
carrying `proxy_request_id`, the credential fingerprint, redacted headers and
the session id.

What is **not** proven, and cannot be from this host: bind, listen, the dialer,
goproxy's own CONNECT parsing, and — separately — **any response body has ever
traversed this lane**, because the control test's upstream always refuses.

## The design changed, and the change is the substance

The phase specified goproxy's `ConnectMitm` plus a hand-built **streaming tee**
into `gateway`'s capture sink. This does the opposite: goproxy **hijacks** the
allowlisted CONNECT, we terminate TLS, and we serve the existing
`gateway.Gateway` over the resulting connection.

`gateway.ServeHTTP` already holds byte-identical forwarding, per-chunk SSE
streaming, the fingerprint-before-redact ordering and the 64KB cap — 81 passing
tests, including a full identity suite. The tee would have been a **second
implementation of all of it, on the enforcement path**. That is the copy-paste
drift `CLAUDE.md` names as the original sin. It would additionally have run
through goproxy's MITM **response copy**, which the spike explicitly did not
measure (its own unresolved item 3).

Division of labour now: goproxy owns CONNECT parsing, the blind tunnel and the
plain-HTTP forward. We own the allowlist, the CA, and the ~30 lines that turn one
hijacked connection into an `http.Server`. **gateway owns every byte and every
piece of evidence.**

Three phase items became **no-ops** as a result, and are recorded as such rather
than invented:

| phase item | outcome |
|---|---|
| step 2, "prototype the streaming tee" | **moot** — `gateway.streamTo` already tees to `captureSink` |
| step 7, "export the gateway seam" | **no-op** — `New`, `Config`, `WithCapture`, `WithVerbose`, `ServeHTTP`, `Captured`, `Emitter` were already exported and sufficient |
| "edit `client/` for the `:proxy:` span/emit path" | **no-op** — landed in phase 08/ADR-0022; `observedLane`, `observedSpanID` and `turnActivityIDFor` already branch on `ProxyRequestID` |

The credential-path surface is therefore **smaller** than the phase anticipated:
`{goproxy, gateway}`, not `{goproxy, gateway, client, decision}`. Nothing in
`transport/` imports `client` or `decision`; those stay bounded at gateway's own
guard.

## The defect the phase did not name: the self-loop

`gateway.New` sets `Proxy: http.ProxyFromEnvironment` on its upstream client, and
`NewIdentityProxy` sets the same on goproxy's transport. Phase 12 activates this
lane by putting `HTTPS_PROXY=http://127.0.0.1:8790` into the **client's**
environment — and if the daemon inherits it (a launchd `setenv`,
`/etc/environment`, a login-shell export), this relay's upstream leg dials
**itself**: CONNECT → hijack → serve → `Do()` → `HTTPS_PROXY` → CONNECT → … until
sockets run out.

`New` clears the six proxy variables. **In the constructor**, not by asking the
caller to remember: `net/http` caches the environment behind a `sync.Once` on
first use, so a later clear does nothing at all. Reported rather than silent — a
daemon that quietly drops a developer's proxy configuration is its own mystery.

Consequence owned rather than hidden: **this lane does not chain through a
corporate proxy.** That is the right beta default (OD3) and matches the gateway
lane; chaining is an explicit phase-12 re-injection, never something inherited by
accident.

## What was built

| file | what it holds |
|---|---|
| `transport/allowlist.go` | exact-host matcher, **ASCII-only** fold, zero value allows nothing |
| `transport/ca.go` | P-256 ~2y CA, 0600/0644, refuse-on-loose-key, **name-constrained**, stdlib leaf minting, ALPN http/1.1 only, idempotent removal |
| `transport/config.go` | loopback-only `Validate`, port 8790, `UpstreamFor` |
| `transport/proxy.go` | CONNECT dispatch, hijack → TLS → `http.Serve(gateway.Gateway)`, self-loop guard, `ServeIntercepted` |
| `cli/internal/gatewayemit` | `Lane` — `LaneGateway` / `LaneProxy`, required, never defaulted |
| `cli/cmd/openbox/transport.go` | the daemon: `openbox transport` |
| `cli/cmd/openbox/transportcapture_test.go` | the control |

### Two decisions worth not re-litigating

**The ASCII-only fold is a security property, not a shortcut.** `strings.ToLower`
folds Unicode, and some non-ASCII runes fold **into** ASCII letters — U+212A
KELVIN SIGN lowercases to `k`. A Unicode-aware fold can therefore make a host
that is not the provider's compare equal to one that is. After an ASCII-only
fold, any non-ASCII byte survives and fails the match. The confusable cases are
**built in code**, not pasted: a Cyrillic homoglyph written as a literal is
invisible to a reviewer, which is the attack itself.

**The lane is a parameter on the existing emitter, not a second emit package.** A
mirror package would have duplicated session-header extraction, lazy DID
resolution, warn throttling, usable-id bounding, spool and flush. An unset `Lane`
is **refused** — `EventFor` errors, `Emit` warns and drops — because a transport
emitter defaulting to `:gateway:` would have its events absorbed by core's dedupe
against the real gateway lane's, and half the evidence would vanish with no error
anywhere. The `eventID` hash deliberately does **not** include the lane name:
adding it would change every shipped gateway event's idempotency key, after which
core would count a redelivered call twice.

## Evidence

### The control test, and why it runs here

`TestTransportLaneRecordsThroughTheRealChain` drives the real chain with **no
fake in it**: a real CONNECT, a real TLS handshake against the real project CA,
the real `gateway.Gateway`, the real `gatewayemit.Emitter`, a real spool file
read back off disk.

It exists because of a bug this repo shipped: package `gateway` tested its relay
against a stub `Emitter`, package `client` tested its span builder against a
hand-written `DevEvent`, both suites were green, and nothing joined them — so
`g.emitter` was nil in the binary and every capture was discarded. Here that gap
is **structurally unreachable**: `transport.New` refuses to construct without an
emitter.

**One substitution:** a `net.Pipe` instead of a socket, and an upstream at a
refused loopback port. `gateway/proxy.go` emits a capture on the
upstream-unreachable path, so the whole evidence chain runs with no upstream in
existence — deterministic, no DNS, no egress.

### Mutation drills — RUN, all red on deletion

| mutation | control that went red |
|---|---|
| unset `Lane` defaults to gateway | `TestAnUnsetLaneIsRefused…`, `TestEmitRefusesAnUnconfiguredLane` |
| `clearInheritedProxyEnv()` removed | `TestNewClearsInheritedProxyEnv` |
| `ConnState` listener close removed | `TestInterceptedRequestReachesTheHandlerOverRealTLS` (goroutine leak per tunnel) |
| allowlist matches by suffix | `TestAllowlistRefusesEverythingElse` |
| CA name constraint removed | `TestCAShapeIsWhatThePhaseSpecifies`, `TestServerConfigRefusesAHostOutside…` |
| `h2` added to ALPN | `TestServerConfigNeverNegotiatesHTTP2`, the choreography test |

### Gates

| gate | result |
|---|---|
| 14 modules under `-race` | **green** |
| `vet` | clean |
| `windows/amd64`, `linux/arm64` cross-compile (`cli`, `transport`) | **green** |
| declared tests vs tests with a verdict | `transport` 39/39 · `gatewayemit` 30/30 · `cli/cmd/openbox` 131/131 — **nothing invisible** |

The declared-vs-verdict count is here because a "green" run that executed nothing
is this plan's own most expensive lesson.

## Two things the first draft got wrong

**The redaction assertion was vacuous in one direction.** "The secret is absent"
passes trivially if the header never reached the record at all. The header's
**presence** is asserted now, alongside its redaction.

**The enforce-path redactor rewrote the test's own fixture.** A literal API-key
string became `${OPENBOX_REDACTED_AI_API_KEY}` silently on save, after which the
assertion was searching for a string the request no longer carried. Fourth
demonstrated victim class for the open `generic-api-key` false positive. The
fixture is **derived in code** now, the mitigation `CLAUDE.md` already names.

## What is NOT done, and where it went

| phase item | disposition |
|---|---|
| injection / synthetic-response mode | **phase 13** — the phase file itself says it is probe A's instrument |
| live gate wiring + dormancy test | **phase 13**. Dormancy is **structural** here — no evaluator is constructed — and an AST-walking test forbids `WithGate`/`Decide`/`WriteRefusal`/`RefuseEverything` in `proxy.go`. It parses rather than greps, so a comment explaining the dormancy does not read as a violation of it |
| `transportservice` (unit, install proof-order, rollback), doctor block | **phase 12**, on the same reasoning the plan already applied to `telemetryservice`: phase 12 owns the shared transactional install/env mechanism, and a third hand-copy of `gatewayservice` here is the drift the plan names |
| env activation of the proxy keys, producer election | **phase 12** (already its scope) |

## Open, and honest

1. **`GOWORK=off` is unverifiable for `transport/` on this host.** `x/net
   v0.50.0` is absent from the module cache and the sandbox denies the write the
   resolver needs. This was **already** true before this change. `transport/go.sum`
   is currently a **superset** merged from `gateway/go.sum`, pending
   `go mod tidy` where the cache is writable. The release path sets `GOWORK=off`
   and `cli` now imports `transport`, so this needs clearing on a normal host
   before release.
2. **No response body has traversed this lane.** The control's upstream always
   refuses, so request capture, fingerprint, redaction, spool and identity are
   proven while the response half is not. Byte-identity **on the CONNECT path** —
   the spike's own unresolved item 3 — remains unrun.
3. **First-byte latency and per-chunk SSE on the CONNECT path are untested.**
   The spike measured chunk separation on the plain path only.
4. **The client-honours-proxy-env assumption is unverified for Claude Code.**
   Measured for the logger's stack, not for every client and auth mode. If a
   client requires h2 to the provider and refuses http/1.1, it stays
   telemetry-only — a `COVERAGE.md` statement, not an average.
5. **A pipelining client would lose buffered bytes.** goproxy hijacks with
   `proxyClient, _, e := hij.Hijack()` and discards the `*bufio.ReadWriter`, so a
   client that sends its TLS ClientHello without waiting for the CONNECT 200 loses
   those bytes and sees a handshake fail for no visible reason. Inherited, not
   introduced — goproxy's MITM path discards the same buffer, so the tee design
   would have had it too. Every mainstream client waits. Recorded because the
   symptom points nowhere near the cause.
6. `transport/allowlist_test.go`'s Cyrillic case is built at runtime; a reviewer
   reading the source sees `cyrillicA` and a comment, not an invisible glyph.

## Unresolved questions

- Does phase 12's activation need the transport daemon's env to stay cleared, or
  will it want deliberate corporate-proxy chaining at GA? The guard is correct
  either way today; the answer changes whether it becomes configurable.
- `Config.Upstream` has exactly one caller (the control test). It is kept because
  the production rule it bypasses cannot otherwise be exercised without live
  provider traffic in a unit test. If phase 13's fixtures give another way, it
  should be reconsidered rather than left standing.
