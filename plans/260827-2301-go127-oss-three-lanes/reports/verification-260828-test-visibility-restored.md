# Test visibility restored — 635 tests were reported green by omission

**Date:** 2026-08-28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture` · **Scope:** whole plan (unblocks stage A
acceptance 2 and every stage-B phase report that said "the tests did not run")

## Verdict

**The blocker report undercounted the damage by 2×, and in the worse direction.**
It said ~334 listener-dependent tests "could not run". The true figure was **635
tests that produced no verdict at all**, and the mechanism was not "the test
fails" — it was `httptest.NewServer` **panicking**, which kills the whole test
binary, so every test declared after the first panic site never ran and was
neither passed nor failed nor skipped. It was invisible.

`gateway` ran **1 of its 81 tests**. `client` ran 7 of 127. `adapters/claude-code`
ran 15 of 196. Those packages reported one or two failures and nothing else,
which reads as "two known problems" rather than "this package is 92%
unmeasured".

**Now: 1117 of 1117 tests across all 13 modules produce a verdict. Zero
invisible. All 13 modules pass.** 21 tests skip, each naming the host capability
it needs.

## What was actually wrong

Two separate facts, and conflating them is what hid the second one.

1. The sandbox denies `bind`. Re-tested and **widened**: it is denied for every
   address family, TCP **and unix-domain** (`net.Listen("unix", …)` fails
   identically). The earlier report tested only TCP.
2. `net/http/httptest.newLocalListener` **panics** when both loopback binds fail
   (`httptest/server.go:226`). A panic in one test aborts the test binary. So the
   count of *failures* was never the count of *unrun tests*, and the inventory —
   built as "every Test func in a file that constructs a listener" — was
   measuring the wrong thing in both directions at once: it over-counted files
   whose listener sites are in one test, and it could not see that a panic takes
   the rest of the binary with it.

The general rule this earns: **a package reporting `FAIL` with two named tests is
not a package with two problems.** Count declared tests against tests that
produced a verdict; the difference is what nobody is looking at.
`go test -list '.*'` vs `--- PASS|FAIL|SKIP` is the whole measurement.

## The fix

`client/memhttptest` — one package, no module boundary crossed, no `go.mod`
touched anywhere.

- A `net.Listener` whose `Accept` returns one end of an in-memory pipe. No
  syscall.
- A registry keyed by synthetic address, and `http.DefaultTransport` replaced
  once per test binary by a **clone** of itself with only `DialContext` changed.
  Unknown addresses fall through to the original dialer, so a test that means to
  reach an unreachable host still does.
- Servers advertise `http://127.0.0.1:<synthetic port>`. The host is literal
  loopback **because the code under test frequently validates that its target is
  loopback** (`gateway.Config.Validate`, the CLI's non-loopback flag). A URL like
  `http://memhttp.invalid` would be rejected by the very check the test exists to
  exercise, and the test would then pass for the wrong reason.

### Why this is evidence and not a fake

This repo's rule is that a fake at each end of a seam proves nothing about the
seam (the `WithCapture` bug). That rule is not tripped here, and the distinction
is mechanical: **the production code path is the thing between the fixture and
the assertion.** The real `client` marshals, signs, gates, redacts and caps; the
real `net/http` client frames and transmits; the real `net/http` server parses;
the handler asserts the bytes it received. Only the `net.Conn` underneath is
substituted, through a seam that **already exists in production**
(`client/client.go:49`, `Config.HTTP` — never set by production, so the client
falls through to `http.DefaultTransport`). No test-only branch was added to
production code.

What it is **not** evidence for, and must never be read as: `bind`, `listen`,
TLS, the dialer, or anything a child process must reach. Those are the 21 skips.

### One production change, and why it was the narrow one

`gateway/proxy.go` only: the relay's upstream `DialContext` moved into a package
variable (`upstreamDialContext`), same seam shape as the CLI's existing
`installUnitFn`/`uninstallUnitFn`. Production never assigns it.

The gateway is the one place in the repo that builds its **own**
`http.Transport`, so the `DefaultTransport` substitution cannot reach it. The
tempting fix — let the test supply the whole Transport — would have bypassed
`DisableCompression`, `ForceAttemptHTTP2` and the idle-pool settings, **which are
exactly what the byte-identity assertions exist to prove**. Replacing only the
dial keeps every one of them in the path. A port that made 80 tests run while
quietly making them prove less would have been worse than leaving them blocked.

### The flake, and why buffering is more faithful rather than less

First port left `TestVerboseReportsArrivalAndOutcome` failing 4 runs in 5. Cause:
a raw `net.Pipe` is **unbuffered**, so a handler's final `Write` cannot return
until the client has read those bytes — which **inverts** an ordering every HTTP
test quietly depends on. Over a socket the write lands in the kernel buffer and
returns, so the handler runs its post-response work (logging the outcome) *while*
the client is still reading. Over a raw pipe that work happens strictly *after*
the client's read completes, and a test that reads the response and then inspects
what the handler recorded becomes a race it never used to be.

So the send side is buffered and drained by its own goroutine, with `Close`
draining first (bounded at 2s so a peer that stopped reading cannot wedge a
test). This makes the substitution **more** faithful: the thing being emulated is
a buffered transport. Stable over 6 consecutive runs per module, and green under
`-race`.

## Drills proving the transport does not mask the controls it now carries

The obvious objection to this change is that 635 tests started passing at the
same moment their transport was replaced. Two drills answer it directly, both on
the gateway — the module with the most to lose, since byte-identity and
per-chunk streaming are its whole point.

| drill | mutation | result |
|---|---|---|
| byte-identity | relay injects `X-Openbox-Drill` into the forwarded request | **`TestForwardIdentity` RED** |
| per-chunk streaming | relay's per-chunk `ctl.Flush()` deleted | **`TestResponseIsNotBuffered`, `TestStreamChunkBoundariesPreserved`, `TestCaptureDoesNotBufferTheStream` all RED** |

The second is the one that mattered, because this change *added* buffering and
per-chunk flush is load-bearing for SSE. It does not mask it, and the reason is
structural rather than lucky: `memhttptest`'s buffer sits **below**
`http.Server`, so deleting the handler's flush leaves the server's own buffering
to swallow the chunk boundaries long before any byte reaches the pipe. The buffer
decouples the handler from the reader; it does not coalesce what the handler
already chose to flush.

Both mutations were reverted and the module re-verified green.

## What this unblocks — measured, not inferred

| | before | after |
|---|---|---|
| tests with a verdict / declared | 205 / 840 (affected modules) | **1117 / 1117** (all 13) |
| invisible (declared, no verdict) | **635** | **0** |
| modules passing | 7 of 13 | **13 of 13** |

### Plan acceptance criterion 2 is now met

**Conformance: 38 numbered cases run, 38 pass, 0 fail.** The census, because a
report arguing "count declared tests against verdicts" cannot carry an
off-by-one of its own: exactly 38 `t.Run("C…")` subtests exist — C1–C7,
C10–C16, C18–C38, C40–C42. **Three numbers do not exist**: C8 and C9 were
deliberately deleted under ADR-0006 (`enforce_conformance_test.go:208,213`) and
**C17 under ADR-0017** (`enforce_evaluate_test.go:573` — there is no local
verdict left to short-circuit on). **C39 is not a subtest at all**: it runs as
`TestContentCaptureCredentialCoverage`
(`content_conformance_test.go:512`), and it passes. The assertions are
**unmodified**; what changed is the listener
underneath them, which is the caveat to carry rather than round off.

That retires the standing "C1–C41 DID NOT RUN" caveat on **phase 08**, whose
report rested acceptance criterion 2 on the inference that "the wire payload
carries no `schema_version` key at all … so the bump should move zero outbound
bytes — *should*, by inference." It is now measured: the v1.6 contract bump moves
zero outbound bytes, asserted on real POSTed payloads.

## What still does not run, and exactly why

19 new guards, each skipping with the capability it needs named in the message.
2 further skips are **pre-existing** opt-ins unrelated to this change
(`TestAcceptanceStockCoreAcceptsEmittedEvents` needs a live core;
`TestRealInstallWritesTheExpectedArtifact` writes a real launchd unit).

**Need a real socket a child process can dial** (`memhttptest.RequireBind`) — an
in-memory listener is not a substitute, because the pipe lives in the parent's
address space:

- `TestHookBinary_BlockVerdictRecordsAdvisoryExitsZero`, `TestHookRealtimeDelivery`
  — compile the binary and run it as a child.
- `TestGatewayCommandActuallyCaptures`, `TestGatewayWithoutADIDStillRelays`,
  `TestSpooledGatewayEventReachesTheWire` — start the real daemon.
- The eight gateway install/proof-order tests
  (`TestGatewaySetupWritesEnvOnlyAfterTheListenerIsUp`,
  `TestGatewayEnvIsNotWrittenWhenTheDaemonDoesNotStart`,
  `TestGatewayEnvIsNotWrittenWhenTheListenerNeverComesUp`,
  `TestRemoveGatewayUnsetsEnvBeforeRemovingTheDaemon`,
  `TestOccupiedPortIsRefusedRatherThanAdopted`,
  `TestReInstallReplacesOurOwnGatewayInsteadOfRefusing`,
  `TestAForeignProcessOnThePortIsStillRefused`,
  `TestAFailedInstallLeavesNoUnitBehind`) — the whole point is "prove it is
  listening", which requires a listener.
- `TestListenVerifiesWhatTheKernelReturned`, `TestUnreachableUpstreamStillProducesEvidence`,
  `TestUserOwnedConfigIsBaseTier`, `TestManagedPathWithoutRootOwnershipIsNotTheMDMTier`,
  `TestReportNeverClaimsPrevention`.

**Needs real DNS** (`memhttptest.RequireResolvableHost`):
`TestNonLoopbackTargetIsFlagged` asserts a *reachable* provider URL is not
described as failing safe, which is vacuous where `api.anthropic.com` does not
resolve (it does not, here).

A skip is strictly better than the status quo in both directions: it is
**visible** where an aborted binary was not, and it runs normally on any host
that can bind.

## Gates

All 13 modules × 4 gates = **52 checks, 52 pass**: `go test -race`, `go vet`,
cross-compile `windows/amd64` **and** `linux/arm64`, and `GOWORK=off` build (the
release path). Re-run after the 6-copies→1-package consolidation.

Guard tests unaffected and green: `gateway/guard_test.go`,
`decision/guard_test.go`, `telemetry/guard_test.go`,
`conformance/deps_test.go`. No `go.mod` in the repo changed, so no allowlist was
widened — which matters, because ADR-0023's rule is that widening an allowlist to
make an import pass inverts its reasoning. `client` was already a direct,
allowlisted requirement of every affected module, and
`gateway/guard_test.go`'s `moduleSources` excludes `_test.go`
(`guard_test.go:132`), so a test-only import is outside its scope by design.

## Decisions worth not re-litigating

- **One shared package, not six copies.** The first working version copied the
  helper into six modules. That is the shape `CLAUDE.md` names as the original
  sin (`the engine used to be copy-pasted per adapter … the copies drifted`), so
  it was consolidated into `client/memhttptest` — reachable from all six because
  `client` is already a direct requirement of each.
- **No `testing` import.** `memhttptest` lives in the shipped `client` module, and
  importing `testing` from a non-test package registers test flags on any binary
  that links it. A local `TB` interface (`Helper`/`Cleanup`/`Fatalf`/`Skipf`) is
  all these helpers ever needed; `*testing.T` satisfies it structurally.
- **`http.DefaultTransport` is replaced by a clone and never restored.**
  Restoring per-server would race tests sharing the process. It is safe because
  the clone is behaviour-preserving for every address not in the registry.
- **Assertions were not touched.** The diff is transport plumbing:
  `httptest.NewServer(` → `memhttptest.NewServer(t, `, `*httptest.Server` →
  `*memhttptest.Server`, two raw `net.Dial` calls → `memhttptest.DialContext`
  (they write malformed request lines Go's client cannot produce), `probeClient`'s
  own transport given the registry dialer, plus the 19 guards. One production
  file changed: `gateway/proxy.go`.

## Unresolved questions

1. **Should the guards stay permanently, or become a build tag?** They are honest
   on any host and inert on a capable one, so they are cheap — but 19 tests that
   skip silently on a misconfigured CI runner would look like a green build.
   Worth a CI assertion that the skip count is **0** on the real runner, so a
   capability regression there is loud.
2. **Does CI actually bind?** If the CI runner has the same restriction, this
   repo has been reporting green-by-omission in CI too, and the 52-gate table
   above is the first honest measurement. Not checkable from here.
3. The 2.2 GB `proxy/` corpus in the sibling logger run is untouched; only
   `otel/` was read, for schema.
