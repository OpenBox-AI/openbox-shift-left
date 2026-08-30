---
title: "Phase 13 verification — replay conformance, fixtures, and probe A's instrument"
phase: 13
date: 2026-08-29
status: implemented; unit- and replay-verified bind-free; no socket, no launchd, no stack
---

# Phase 13 — what is now proven, and what still is not

Every claim below is split by evidence strength. The short version: **a real
recorded model call has crossed the CONNECT path byte-identically for the first
time**, the telemetry lane maps a real recorded export end to end, and both are
proven on a host that cannot bind a socket. Nothing here touches the testbed's
standing "has NOT run".

## The headline

Phase 11 closed with "**no response body has ever traversed this lane**", and the
goproxy spike closed with "the gate exercised the plain-HTTP path only — running
the CONNECT path is the FIRST thing the conformance work should do." Both are now
retired:

- a recorded 564,718-rune request and its response cross a real CONNECT, a real
  TLS handshake against the real project CA and the real `gateway.Gateway`,
  **byte-identical in both directions**, and the capture reaches a real spool file;
- a recorded **60-frame SSE response** streams through the same path **per chunk**,
  with the buffering failure BOUNDED rather than hanging.

One substitution makes that possible on a bind-denied host, and it is the smallest
one available: the relay's upstream **dial**. The relay's own Transport stays in
the path, and `TestTransportAddsNoCompressionHeaderOfItsOwn` is what proves it did.

## What was built

| | |
|---|---|
| `cli/internal/corpusfixture/` | the sanitizer (`Sanitize`/`Scan`) and the permanent gate on every committed fixture |
| `cli/cmd/corpusfixture/` | the one-time extractor; its write is GATED on `Scan` reporting nothing |
| `telemetry/replay.go` | `Receiver.ConsumeLogsJSON` — the production decode, reachable without HTTP |
| `telemetry/replay_test.go` | the 16-event-type census on the recorded export |
| `cli/cmd/openbox/telemetryreplay_test.go` | the full chain: decode → mapper → emitter → spool, elected and not |
| `gateway/internal/dialhook`, `gateway/gatewaytest` | the dial seam and its test-only mutator, each with a tripwire |
| `cli/cmd/openbox/transportreplay_test.go` | the CONNECT-path identity, SSE and compression cases |
| `cli/cmd/openbox/transportsoak_test.go` | what a real model call costs, and OD1(c) on a real oversized body |
| `testbed/46-otel-lane.sh`, `47-transport.sh` | dormant; the live-stack claims only |
| `probes/refusal-injector/` + `plans/260827-2301-go127-oss-three-lanes/probes/RUNBOOK.md` | probe A's instrument and its pre-decided outcomes |

Fixtures: `telemetry/testdata/corpus/otel-logs.json` (20 records, 16 event types),
`transport/testdata/corpus/messages-json.json` (564,718-rune request — the
oversized one), `messages-sse.json` (60 SSE frames).

## Measured on the real corpus — numbers that did not exist before

Run `20260827T063932Z-225cac`, 82,772 recorded proxy events.

| | |
|---|---|
| model calls (`/v1/messages`, excl. `count_tokens`) | 5,049 · 3,507 with SSE responses |
| **request bodies over the 65,536-rune cap** | **96.75%** (4,885 of 5,049) · p50 529,175 · p95 1,705,840 · max 2,566,660 |
| response bodies over the cap | 0.06% (3 of 5,044) · p50 1,938 · max 85,903 |
| response `content-encoding` | gzip 3,569 (every event-stream) · br 1,573 (every JSON) · none 5 |
| `x-stainless-retry-count` | present on all 5,231 model-call requests · **every value `0`** |
| spool cost per model call (soak, 8 iterations) | **70,080 bytes**, ~674 ms/call → ~334 MB per 5,000-call session |
| OD1(c) on a real oversized body | 564,718-rune request → **69,848-rune POSTed payload** |

**OD1(c)'s "~95% truncate" is confirmed at 96.75%** — measured rather than
estimated, for the first time.

**Three denominators appear above and they are not the same population**, which is
worth stating rather than leaving to look like an inconsistency: **5,340** is every
recorded `/v1/messages` request; **5,231** of those carry the retry-count header;
**5,049** are the ones whose response was also recorded and could be flow-paired,
which is the only population where a body-size comparison means anything. Every
number here comes from one machine, one workload, one run —
`20260827T063932Z-225cac`, all subscription-OAuth — and generalizes no further than
that.

**More than half of the recorded model calls carried this repository's own
redaction markers in their context**, because the corpus was recorded during
governed sessions in this repo. Three numbers, because collapsing them overstates
the harm in one direction and understates the cause in another:

| | |
|---|---|
| model-call requests whose context carries ≥1 `${OPENBOX_REDACTED_*}` | **2,820 of 5,340 — 52.8%** |
| total marker occurrences across them | 22,060 |
| **distinct rewrite SITES** (category + 80 chars of surrounding context) | **~200** |

So roughly **200 corrupted places, amplified ~110×** by context replay: one
rewritten file read early in a session poisons every later call's context. The
22,060 is not 22,060 rewrite events, and saying so would be the same
looks-like-a-measurement error this phase already made four times.

**The attribution is the part that changes a decision.** By distinct site:

| rule | distinct sites | |
|---|---|---|
| `redactEntropy` (ours) | 56 | 28% |
| `secret_assignment` (ours) | 55 | 27% |
| `generic-api-key` (gitleaks) | 26 | 13% |
| `AI_API_KEY` (ours) | 21 | 10% |
| everything else | 42 | 21% |

**Our own two hand-rolled generic rules account for 55% of the sites; the gitleaks
`generic-api-key` rule that plan.md Open Q1 proposes disabling accounts for 13%.**
Disabling that rule alone would leave the majority of the corruption in place. That
is new information about a decision already on the table, and it is why the item
belongs in front of the owner rather than in a report's footer.

**Not every marker is a false positive** — some are correct redactions of real
secrets, and nothing here distinguishes them without inspecting each of the ~200
sites. What makes the false-positive reading the likely one is the category mix:
`ENTROPY` leading, over a corpus of source code full of git SHAs, UUIDs, base64
test vectors and `go.sum` hashes, is exactly the behaviour CLAUDE.md already
documents for that rule.

It was also **invisible until bodies were decompressed**: while a response was
gzipped the marker sat inside the compressed bytes and matched nothing — the same
mechanism this repo already documents for a content-encoded body defeating its own
detector, observed here on its own artifacts.

## The seam, and why it is shaped this way

Making a response body cross the CONNECT path on a bind-denied host needed the
relay's upstream dial to be replaceable from another module. Three options were
considered and the middle one was rejected on advice:

- **A `DialContext` field on `gateway.Config`** — rejected. The hazard is not
  injection (anything that can set `Config` already sets `Config.Upstream`, where
  the credential goes); it is DRIFT. A future production feature populating the
  field would silently stop the byte-identity suite from describing the production
  dial path, and `nil ⇒ default` makes that invisible.
- **`RequireBind` and accept it never runs here** — rejected. This branch has no
  upstream, so CI has never run it, and this plan's own record is that five of six
  socket-test failures were bugs in the tests, authored blind.
- **Adopted:** `gateway/internal/dialhook` holds the variable (no module outside
  `gateway` can import it at all), and `gateway/gatewaytest` is the test-only
  mutator, guarded by a walk that fails on any non-test importer — the same shape
  `client/memhttptest` already carries. The dial is read **per dial** rather than
  captured at construction, so a swap cannot depend on construction order.

`transport/` cannot host these tests: its dependency guard allows exactly
`{goproxy, gateway}`, and `transport/spike_test.go` explicitly declines the
`memhttptest` dependency. Verified, not assumed. The in-memory work therefore lives
in `cli/cmd/openbox`, which already imports all three.

**The gateway import guard was corrected, not widened.** It flagged
`gateway/internal/dialhook` as an external import on the premise that "the
credential guard only scans this module". That premise is false for a subpackage of
the module — `moduleSources` already walks it. The exemption is exactly one module
prefix with a trailing slash, and `TestSelfModuleExemptionIsNarrow` pins that
`client`, `decision`, `transport` and a lookalike `gatewayfoo` stay outside it.
This is not the allowlist widening that decision forbids.

## Drills — run, not claimed

| drill | result |
|---|---|
| inject `X-Forwarded-For` in the relay | **RED** |
| drop `DisableCompression` | **GREEN first** — see below — then **RED** against the corrected control |
| unwire `WithCapture` in `transport.newRelay` | **RED** |
| remove the per-chunk `Flush` | **RED**, bounded, reporting "the relay BUFFERED the response" |
| disable `Scan` in the sanitizer | **RED** |

**The `DisableCompression` drill going green is the most useful result in the
table.** The assertion it was aimed at — "the client's own `Accept-Encoding`
arrived unchanged" — *cannot* detect that mutation: `net/http` only adds the header
when the request does not already carry one, so with the fixture's own header
present both settings behave identically. The fix was a second case sending **no**
`Accept-Encoding` and asserting none arrives. Without the drill, an assertion that
reads exactly like a control would have shipped as one.

## Four mistakes in this phase's own work, recorded because the shapes recur

1. **A drill reported RED without measuring anything.** The sanitizer drill was
   first run with `-run` pointed at a package that does not contain the test. It
   printed RED and would have been recorded as evidence. Redone against the right
   package, where it is genuinely red.
2. **A drill's revert corrupted the source.** The `Flush` drill's revert anchor
   (`_ = ctl`) also matched `_ = ctl.SetWriteDeadline(...)` earlier in the file,
   producing `ctl.Flush().SetWriteDeadline(...)`. Caught by the full sweep, not by
   the drill. Redone with a unique multi-line anchor.
3. **The OD1(c) test passed with the body absent rather than capped.** It reported
   a 564,718-rune body "egressing as 941 runes" — `stripContent` had removed it,
   because content capture is a `client.Config` field, not the env var assumed.
   Now presence-anchored: the head must be on the wire before the missing tail
   means anything.
4. **The first corpus measurement was wrong by 8×.** A prefilter searching for
   `"/v1/messages` (leading quote) matched almost nothing, yielding 585 model calls
   and "100% over cap". The corrected filter yields 5,049 and 96.75%.

All four are the same shape: **a result that looks like a measurement and is not**.

## Evidence strength, per claim

**Strong — asserted on real bytes, bind-free:**

- the recorded OTLP export decodes through the collector's own unmarshaler and
  produces 20 records across 16 event types (`telemetry/replay_test.go`);
- `api_request` → a conformant `TurnCompleted` with the lane discriminator, model,
  four token counts and an `llm_completion` span, five distinct turns from five
  recorded requests, no id reused (`telemetryreplay_test.go`);
- the other 15 event types emit nothing **and are counted as drops** — so a lane
  dropping everything is distinguishable from a quiet session;
- an un-elected lane spools nothing, asserted **against the same fixture** that
  yields five turns when elected;
- the capture-OFF half for `:proxy:` holds **by composition**, not by a per-lane
  case: `cli/internal/gatewayemit/event_test.go` (`TestCaptureOffStripsBodiesAndHeadersButKeepsTheFingerprint`)
  covers lane-independent code. That is sound today and becomes a gap the moment
  `EventFor` branches on `Lane` — recorded here so the composition is a decision
  rather than an oversight;
- CONNECT-path byte identity in both directions, no injected `X-Forwarded-For`, no
  relay-added `Accept-Encoding`;
- per-chunk SSE delivery across the CONNECT path;
- OD1(c) truncation on a real 564,718-rune body, presence-anchored;
- every committed fixture is free of the sentinel classes, gated by a test that
  DISCOVERS `testdata/corpus` directories rather than listing them.

**Measured, not asserted:** every number in the corpus table above. They are
reported, not enforced, except the spool-cost bound, which is a test.

**Not proven, and named:**

- **the OTLP HTTP layer.** The replay enters one layer below it. Its control test
  (`TestTelemetryCommandActuallyRecords`) is bind-guarded and skips here.
- **bind, listen, TLS to a real socket, the real dialer.** Everything above runs
  over `net.Pipe` and an in-memory upstream.
- **anything stack-dependent.** That core stores an `:otel:`/`:proxy:` turn as its
  own row, that the span classifies as `llm_completion` after ingest, and that
  exactly one producer emits in the field. `46-otel-lane.sh` and `47-transport.sh`
  hold those and are **dormant**.
- **probe A itself.** The instrument exists and its logic is tested bind-free; the
  run needs a bind-capable host, a real install and credentials.
- **brotli-encoded exchanges.** Excluded from the fixtures — 1,573 of the recorded
  JSON responses — because the standard library cannot decode them and taking a
  brotli dependency to widen a fixture set is not a decision a test-data script
  should make.

## Gates

**Verdict census first**, because this repo's rule is that a package reporting FAIL
with two named tests is not a package with two problems. Counting declared tests
(`go test -list '.*'`) against tests that produced a verdict (`--- PASS|FAIL|SKIP`)
across all 15 modules: **1,278 declared, 1,860 verdicts** (subtests inflate the
second number), **29 skips, and no module where verdicts fell short of declared**.
Nothing is invisible. The 29 skips are capability guards naming what they need —
20 in `cli`, 5 in `transport`, 2 in `gateway`, 1 each in `client` and
`adapters/claude-code`; none were added by this phase, whose tests are all
bind-free.

**61 of 61 gates green, 0 failures.** 15 modules × {`-race`, `vet`, `windows/amd64`,
`linux/arm64`} = 60, plus `cli` under `GOWORK=off`. Plain `go test ./...` is green
in all 15 as well. Every
pre-existing test passes unedited except `gateway/guard_test.go` and
`gateway/memdial_test.go`, both of which follow the dial seam and gained
assertions rather than losing them.

## Unresolved

- **The redaction-corruption finding needs an owner ruling, and it is no longer the
  question plan.md Open Q1 asks.** That question offers "disable `generic-api-key`
  alone" as the narrow option; the site census says that rule is 13% of the
  problem and our own two generic rules are 55%. Surfaced, not decided — detection
  scope is a posture call (OD-class). Two bind-free experiments would make the
  fork measured rather than a matter of taste: (a) classify the ~200 sites as true
  or false positives by inspection, (b) re-run the detector over the corpus with
  each generic rule disabled in turn and diff what uniquely disappears.
- `messages-json.json` (624 KB, mostly one recorded request body) **stays in git**
  — resolved, not left open. It is the oversized body OD1(c) needs, no smaller
  clean exchange exists in the corpus, and the committed-fixture gate's
  `redactedMarkerRe` means a future redactor rewrite of it goes red in CI instead
  of silently turning every assertion built on it into a statement about the
  accident.
