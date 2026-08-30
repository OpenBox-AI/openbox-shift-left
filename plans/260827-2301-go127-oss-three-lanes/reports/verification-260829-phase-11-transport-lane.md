# Phase 11 — the transport lane (`:proxy:`), implemented

**Date:** 2026-08-29 · **Branch:** `feat/tool-content-capture` ·
**Commits:** `0ac3f0d`, `3507c1e`, `a0089ca`, `2be7902`, `c7fb3a1`, `4840a0d` ·
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
| "edit `client/` for the `:proxy:` span/emit path" | **no-op** — landed in phase 08/; `observedLane`, `observedSpanID` and `turnActivityIDFor` already branch on `ProxyRequestID` |

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

Eight, not six: two more defects were found by re-reading the code after the
first six passed, and each got its own control and drill.

| mutation | control that went red |
|---|---|
| unset `Lane` defaults to gateway | `TestAnUnsetLaneIsRefused…`, `TestEmitRefusesAnUnconfiguredLane` |
| `clearInheritedProxyEnv()` removed | `TestNewClearsInheritedProxyEnv` |
| `ConnState` listener close removed | `TestInterceptedRequestReachesTheHandlerOverRealTLS` (goroutine leak per tunnel) |
| allowlist matches by suffix | `TestAllowlistRefusesEverythingElse` |
| CA name constraint removed | `TestCAShapeIsWhatThePhaseSpecifies`, `TestServerConfigRefusesAHostOutside…` |
| `h2` added to ALPN | `TestServerConfigNeverNegotiatesHTTP2`, the choreography test |
| `New` drops its variadic options | `TestNewAppliesItsOptions` |
| blind tunnel returns `goproxy.OkConnect` | `TestGoproxysBundledCAIsNeverReferenced` |

### Gates

| gate | result |
|---|---|
| 14 modules under `-race` | **green** |
| `vet` | clean |
| `windows/amd64`, `linux/arm64` cross-compile (`cli`, `transport`) | **green** |
| declared tests vs tests with a verdict | `transport` 39/39 · `gatewayemit` 30/30 · `cli/cmd/openbox` 131/131 — **nothing invisible** |

The declared-vs-verdict count is here because a "green" run that executed nothing
is this plan's own most expensive lesson.

## Four things the first draft got wrong

**`New` accepted `opts ...Option` and dropped them.** A variadic option that is
accepted and ignored is the worst kind of no-op: every call site reads as
configured while the daemon runs unconfigured. Here it meant `--verbose` would
log nothing, which is indistinguishable from a relay nothing reaches.

**The blind-tunnel branch returned `goproxy.OkConnect`, which names goproxy's
built-in CA** — whose private key ships in the library source. `ConnectAccept`
never reads `TLSConfig`, so it was inert; inert is not safe, and a value naming a
public-key CA on the interception path is one refactor from being used. This
module now names it nowhere, and an AST guard holds that.


**The redaction assertion was vacuous in one direction.** "The secret is absent"
passes trivially if the header never reached the record at all. The header's
**presence** is asserted now, alongside its redaction.

**The enforce-path redactor rewrote the test's own fixture.** A literal API-key
string became `${OPENBOX_REDACTED_AI_API_KEY}` silently on save, after which the
assertion was searching for a string the request no longer carried. Fourth
demonstrated victim class for the open `generic-api-key` false positive. The
fixture is **derived in code** now, the mitigation `CLAUDE.md` already names.

## OD2's "one command in, one command out" is REASSIGNED to phase 12

Said explicitly rather than left to be inferred from a table, because dropping it
silently is how a ruling looks met when it is not. Phase 11 ships the **relay
daemon** — `openbox transport`, foreground, supervisable. Phase 12 ships the
**command**: install, activate, roll back, `doctor`. Until then a developer
cannot turn this lane on, so **OD2 is unmet and phase 12 owns it.**

## What is NOT done, and where it went

| phase item | disposition |
|---|---|
| injection / synthetic-response mode | **phase 13** — the phase file itself says it is probe A's instrument |
| live gate wiring + dormancy test | **phase 13**. Dormancy is **structural** here — no evaluator is constructed — and an AST-walking test forbids `WithGate`/`Decide`/`WriteRefusal`/`RefuseEverything` in `proxy.go`. It parses rather than greps, so a comment explaining the dormancy does not read as a violation of it |
| `transportservice` (unit, install proof-order, rollback), doctor block | **phase 12**, on the same reasoning the plan already applied to `telemetryservice`: phase 12 owns the shared transactional install/env mechanism, and a third hand-copy of `gatewayservice` here is the drift the plan names |
| env activation of the proxy keys, producer election | **phase 12** (already its scope) |

## Open, and honest

1. **The release build was BROKEN and is now fixed; one module gate remains
   unverifiable.** `cli/cmd/openbox/transport.go` imports `transport`, but
   `cli/go.mod` had neither a `require` nor a `replace` for it. The workspace
   resolved it through `go.work`, so all 14 modules were green — while
   `GOWORK=off`, the **only** path `.goreleaser.yaml` runs, could not resolve the
   import at all. Exactly the failure `CLAUDE.md` already names: *a new dependency
   in a shared module needs tidying in every module that transitively depends on
   it, or `GOWORK=off` fails while the workspace stays green — the release path is
   the one that breaks.* Note that `go mod tidy` alone would NOT have fixed it:
   tidy does not write `replace` directives, and without one the `v0.0.0` require
   has no source.

   **Fixed and verified:** `cd cli && GOWORK=off go build ./...` passes.

   Still open: `cd transport && GOWORK=off go build ./...` cannot run on this
   host — standalone, transport's MVS wants `x/net v0.50.0`, which is absent from
   the module cache, and the sandbox denies both the download and the cache lock.
   That gate needs a normal host, along with `go mod tidy` in `cli` and
   `transport` to prune the merged `go.sum` supersets. **It does not affect the
   release artifact**, which builds `cli`.
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

## The risk phase 12 must design for, not discover

**Cross-process double-emission that core's dedupe cannot catch.**
`clearInheritedProxyEnv` protects the transport's OWN upstream leg. It does
nothing for the **gateway**, which is a different process. If both lanes are
installed and phase 12's activation sets `HTTPS_PROXY` where the gateway daemon
inherits it, the gateway's upstream client — `http.ProxyFromEnvironment`, never
cleared there — dials `api.anthropic.com` **through the transport**. The
transport sees the forwarded session header and emits `:proxy:`; the gateway
emits `:gateway:` for the same call.

Two events, two **disjoint** activity_ids — which is exactly the property that
stops core absorbing one as a duplicate of the other — so **both store, and every
token count doubles.** The namespaces built to prevent one lane erasing another
are precisely what let this survive.

So the election must be an **activation-time mutual exclusion across processes**
(installing transport disables the gateway's base-URL redirect, or refuses
co-activation), never a client-side span dedupe. Two supporting hazards in the
same phase:

- **The `sync.Once` ordering.** `clearInheritedProxyEnv` works only because it
  runs in `transport.New` before any outbound HTTP. If activation makes the
  daemon do an HTTP call first — a health check, an auth refresh — the cache
  poisons and the self-loop returns. A guard test that nothing dials before `New`
  is cheap.
- **"Never system trust" (requirement 5) versus Chromium.** The desktop app is
  the reason this lane exists, and Electron reads the OS trust store. Getting it
  to trust a client-only CA collides head-on with that requirement, and it is the
  genuinely hard part of activation.

## Unresolved questions

- Does phase 12's activation need the transport daemon's env to stay cleared, or
  will it want deliberate corporate-proxy chaining at GA? The guard is correct
  either way today; the answer changes whether it becomes configurable.
- `Config.Upstream` has exactly one caller (the control test). It is kept because
  the production rule it bypasses cannot otherwise be exercised without live
  provider traffic in a unit test. If phase 13's fixtures give another way, it
  should be reconsidered rather than left standing.
