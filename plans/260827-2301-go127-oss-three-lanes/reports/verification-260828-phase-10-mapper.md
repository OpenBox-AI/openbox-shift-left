# Phase 10 — the model-call mapper (partial: the turn, not the bodies)

**Date:** 2026-08-28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture` · **Gates:** phases 12, 13

## Verdict

**One slice of phase 10 is done and genuinely verified; four are deferred with
named reasons, and one of those reasons is a security control that cannot be
built yet.** Stated first so it cannot read as a footnote.

Delivered: `api_request` → a conformant `TurnCompleted` under the `:otel:`
namespace, carrying the model id and all four token counts where
`ExtractModelMetricsFromActivity` reads them; the election-suppression invariant;
identity safety on the provider's request id; the synthesized-span honesty
marker; and a sentinel asserted on real POSTed bytes. **Nine mutation drills,
all red on deletion, all run rather than claimed.**

Deferred: body attachment, `tool_decision`/`tool_result`, hook engine-health, and
OD4's silence finding. Reasons below — none of them is "ran out of time".

**The mapper has no production caller.** That is the `WithCapture` shape and it
is named here rather than left to be discovered: its caller is the receiver
daemon, whose half of phase 09 is blocked on `bind`. Nothing emits in the field,
which is also why nothing can double-count in the field.

## What was built

| File | What |
|---|---|
| `cli/internal/telemetryemit/mapper.go` | `Policy`, `Mapper`, `EventFor`; id validation; token parsing |
| `cli/internal/telemetryemit/mapper_test.go` | 11 tests over the corpus shape |
| `cli/internal/telemetryemit/sentinel_test.go` | the no-content-on-wire sentinel, both postures |
| `cli/internal/telemetryemit/bound_test.go` | the cross-module bound relation, wire cap MEASURED |
| `client/gatewayspan.go` | `observedSpanAttributes` — marks the otel lane synthetic |
| `client/observedspan_test.go` | + the synthetic-marker test across all three lanes |
| `telemetry/record.go` | `MaxAttrValueBytes` exported so the relation is pinnable |
| `client/memhttptest/` | closed-server refusal; the test-only tripwire |
| `cli/internal/gatewaycheck/check_test.go` | the overstatement guard split to run bind-free |

Why it lives in `cli/internal/telemetryemit` and not in `telemetry/`:
`telemetry/guard_test.go` allows the collector family and nothing else, and that
guard is what quarantines a 492-package tree. A mapper needs `client`. Same
reason, same shape as `cli/internal/gatewayemit`.

## Verified, on the real wire bytes

The payload a real POST carries, through the real signing client:

```
activity_id      "sess-1:otel:req_011CeSoFqW2HfEh9jxCds86Y"
event_type       "ActivityCompleted"
activity_type    "llm_completion"
activity_output  {"model":"claude-opus-4-8","usage":{"input_tokens":2,"output_tokens":173,
                  "cache_read_input_tokens":90485,"cache_creation_input_tokens":333}}
duration_ms      4210
spans[0]         attributes {"http.method":"POST","http.url":"…/v1/messages",
                  "openbox.span_synthetic":true}
```

That is the shape ADR-0014 specifies and core's extractor reads, in the `:otel:`
namespace ADR-0022 declared, with the classification keys that make core
recompute `semantic_type` as `llm_completion`.

## The nine drills

Each mutation was applied, the named test observed RED, and the mutation
reverted. Two of them were **inconclusive on the first attempt** — the mutation
failed to compile, and a build failure is not a red test. They were redone.

| # | mutation | result |
|---|---|---|
| 1 | election gate deleted | `TestUnelectedMapperEmitsNothing` RED |
| 2 | request-id charset check neutered | 3 of 4 malformed cases RED (the length case correctly still passes — that bound was kept) |
| 3 | `Record.Attrs` copied wholesale into metadata | sentinel RED at **both** postures, 17 leaked-attribute errors |
| 4 | synthetic-marker derivation deleted | `TestOnlyTheTelemetryLaneMarksItsSpanSynthetic` RED |
| 5 | `MaxAttrValueBytes` reverted to 16 KiB | `TestAttrValueBoundExceedsTheWireCap` RED, naming the measured 65,536-rune cap |
| 6 | a production file imports `memhttptest` | `TestMemhttptestStaysTestOnly` RED |
| 7 | a bypass note starts claiming prevention | `TestReportNeverClaimsPreventionWithoutAListener` RED |
| 8 | session-id validation reverted to an emptiness check | **6** traversal cases ship, incl. `../../etc/passwd` |
| 9 | zero-timestamp guard removed | a turn ships stamped `0001-01-01T00:00:00Z` |

Drill 3 is the one that matters most. Copying the attribute map into metadata is
the natural thing to write, and it routes content around the content gate
entirely under key names `contentMetadataKeys` has never heard of. It goes red
with capture **off** as well as on, which is the direction that proves the
sentinel is about the mapper and not about the gate.

Drill 7 also needed redoing: the first injection landed inside an
`OwnerUID < 0` branch, which is Windows-only dead code on this host. A drill
placed in unreachable code proves nothing, and it looked like a passing test.

## Decisions worth not re-litigating

- **`Policy`'s zero value SUPPRESSES.** Not a default — an invariant. Two lanes
  can observe the same call, and core does **not** absorb one as a duplicate
  (the namespaces are disjoint by design, which prevents silent LOSS). Nothing
  prevents silent DOUBLING except one lane emitting, and that election is phase
  12's. Until it exists, the only policy constructible without naming `Elected`
  emits nothing.
- **`TurnCompleted` only, no pair.** `buildPayload` attaches an observed span
  under that case alone, and `gatewayemit.EventFor` already takes the same
  close-only shape. A pair would need a discriminator on the opening half or its
  `activity_id` resolves empty.
- **No redactor parameter.** Nothing this slice binds is content. A redactor with
  no call site reads like a wired control and is not one — which is exactly how
  the gateway came to discard every capture it made. It arrives with the bodies.
- **The otel span is marked `openbox.span_synthetic`; the in-path lanes are
  not.** The gateway and the transport relay SAW their method, URL and status.
  The telemetry export carries neither a method nor a URL, so the client
  synthesizes both to reach `isLLMCall` — the only path to an `llm_completion`
  classification. An unmarked synthetic span is indistinguishable from captured
  traffic in a stored row. The marker is derived from the LANE, not set by the
  caller, so a mapper cannot forget it.
- **A missing count and a malformed count are different.** Missing means not
  applicable: the field stays nil and the total stays meaningful. Malformed means
  a number was reported and could not be read: the field stays nil **and the
  total is withheld**, because a sum that silently omits a component reads as
  authoritative and is wrong.
- **A malformed provider id drops the turn.** `OtelRequestID` becomes part of
  `activity_id`. A gap is recoverable; an ambiguous identity corrupts a stored
  row. `':'` is the specific hazard — `activity_id` reads
  `<session>:otel:<id>`.
- **No id is minted when the provider sends none.** A local id would break INV-5:
  the spool outlives the process that wrote it, and a re-flush must present the
  same key.

## Corpus findings that changed the code

Both measured, not assumed (see
[measure-260828](measure-260828-otel-attribute-inventory.md)):

- **`input_tokens` is PURE input.** input=2 beside cache_read=90485 on the same
  call. That is contract v1.1's redefinition exactly, so the four counts pass
  through unmodified. Adding cache into input would have double-counted ~90k
  tokens per call.
- **The provider types the same attribute differently per event** —
  `duration_ms` is `intValue` on `api_request` and `stringValue` on
  `tool_result`. The mapper parses from text and cannot be bitten, but only
  because `consume.go` flattens every OTLP type through `AsString`. That is
  load-bearing, not lazy; a "typed extraction" optimization would read zero on
  one of them with no error.

## Deferred, with reasons

- **Bodies → observed span.** `body_ref` is a filesystem PATH, and the receiver
  is an unauthenticated loopback listener, so a naive read is a local-file-read
  oracle: any local process can name `~/.openbox/.env` and have it redacted,
  signed and egressed as a model-call body. Containment is `os.Root` +
  `io.LimitReader`, and its root follows phase 09's **unmade** env-key decision.
  **The mapper opens no file today, so no oracle exists** — and the containment
  must land in the same change as the first body read, never after it.
- **`tool_decision` / `tool_result`.** The requirement is "where the hook lane
  did not already report it", which is cross-lane knowledge the election
  supplies. Emitting now would add second rows per tool call and double Tool
  Health with no id collision and no error.
- **Hook engine-health** — cut under `--yagni`: `openbox doctor` already detects
  a duplicate engine; a second continuous path adds no capability.
- **OD4's silence finding** — needs the daemon to schedule a window check, and
  the daemon half is blocked. A pure function with no caller is the shape this
  report already names once.

## Also fixed in this change (from the advisory review)

- **`memhttptest` closed servers now REFUSE instead of falling through.** `Close`
  deleted the registry entry, so a later dial of that URL fell through to a
  **real** dial of `127.0.0.1:<synthetic port>` — which on a bind-capable machine
  can reach an unrelated local service and, worst case, POST a signed governance
  payload at it. No current test does close-then-redial; this is the hardening
  that keeps it that way.
- **A tripwire keeps `memhttptest` test-only.** It is a non-test package in the
  shipped `client` module that replaces `http.DefaultTransport` process-wide, and
  `internal/` is unavailable because six modules import it. Drill 6 is the check.
- **The overstatement guard runs bind-free now.**
  `TestReportNeverClaimsPrevention` — the control that stops the CLI claiming
  bypass is prevented — was skipped in the sandbox where most iteration happens.
  Its wording assertions never depended on the gateway being alive; only the
  address did. The bind-free variant points at a dead port, so it covers the
  not-answering branch too: broader, not weaker.
- **The conformance census in the previous report was incomplete.** It named C8
  and C9 as deleted but not **C17** (ADR-0017,
  `enforce_evaluate_test.go:573`), and did not say that **C39 is not a subtest**
  — it runs as `TestContentCaptureCredentialCoverage`, and it passes. The count
  of 38 was right; the explanation was not, which matters in a report whose
  thesis is careful counting.
- **The bound relation is a test now, not a comment.** `MaxAttrValueBytes` is
  exported and `TestAttrValueBoundExceedsTheWireCap` **measures** the wire cap
  rather than copying it. The worst case is exact rather than comfortable:
  65,536 runes of 4-byte UTF-8 is 262,144 bytes, so a maximal multi-byte value
  fits with no headroom — which means **a future capBody drill on this lane must
  use ASCII fixtures or it is vacuous at the boundary.**

## Gates

1138 of 1138 tests across 13 modules produce a verdict; 0 invisible; 21 skips
(19 capability guards, 2 pre-existing opt-ins). All 13 modules × `-race`, `vet`,
`windows/amd64`, `linux/arm64`, `GOWORK=off` = **52/52**.

`cli/go.mod` gained the `telemetry` require + replace, which `go mod tidy` needed
explicitly — the workspace build was green without it while `GOWORK=off` would
have failed, which is the release path and the exact trap `CLAUDE.md` warns
about.

`adapters/claude-code/usage.go`: **zero diff.** Its INV-2 sentinel still green.

**The binary is still 17 MB.** OD5's +16.5 MB has not materialized because
nothing links the mapper yet — the linker is package-granular, and no production
package imports it. That cost lands with the daemon subcommand, not here.

## Two defects the advisory review found, and one correction to its advice

Both were real, in code committed an hour earlier, and both are now fixed and
drilled (drills 8 and 9).

- **`session.id` was checked only for emptiness.** It is a provider value off the
  same unauthenticated loopback listener as everything else, and it does not
  merely become the `activity_id` prefix and core's `run_id` — every spool
  consumer in this repo turns it into a **filename** (`<session>.jsonl`,
  `hookflow/spool.go:84`). `gatewayemit.usableSessionID` already refused
  `/ \ . ..` for exactly that reason; the asymmetry was the bug. Now
  charset-checked, which makes traversal *unrepresentable* rather than forbidden.
  Drill 8: reverting to the emptiness check lets **6** traversal cases ship,
  including `../../etc/passwd`.
- **A zero record timestamp was formatted and shipped.** `record.go` binds the
  record's own time and explicitly leaves a zero for "the mapper to decide what to
  do about" — and the mapper did not decide. `time.Time{}` formats to a *valid*
  RFC3339 string, so nothing downstream rejects it; the turn is simply filed in
  year 0001 and every window and latency reader quietly disagrees with every other
  lane. Drill 9 confirms.

**The correction:** the review's proposed CI guard, "assert the skip count is 0
on the runner", would **break CI**. Two of the 21 skips are pre-existing opt-ins
that always skip —`TestAcceptanceStockCoreAcceptsEmittedEvents` needs a live core
and `TestRealInstallWritesTheExpectedArtifact` writes a real launchd unit. The
correct assertion is narrower: **no skip may cite a bind or DNS capability.** That
still closes the green-by-omission hole one level up without failing on the
deliberate opt-ins. Not implemented here — it is a CI-config change affecting
everyone's builds, so it is the owner's call.

Verified rather than accepted, from the same review: `isLLMCall` takes no status
input (`openbox-core/internal/content/session.go:451-476`), so omitting
`http_status` does not break the `llm_completion` classification; and no core
reader consumes `total`, so withholding it on a malformed component costs nothing
downstream.

## Unresolved questions

1. **This branch has no upstream, so no run of these 1138 tests over a real
   socket exists anywhere.** The sandbox verifies payload, framing, gate,
   redaction and cap; CI (`ubuntu-latest`) is the socket half and has never seen
   any of this work. Pushing is the single action that can falsify the most.
2. **Should CI assert the skip count is 0?** The 19 guards are honest on any host
   and inert on a capable one — but if a runner ever loses bind, they would skip
   silently and the build would still be green. That is the same
   green-by-omission failure one level up.
3. **The confinement root for `body_ref`** is still phase 09's undecided env-key
   question, and body attachment cannot be finished without it.
4. Whether `assistant_response` / `user_prompt` stay unbound (the hook lane
   already egresses both; binding them here duplicates content under a second
   producer). Recommend unbound, stated in `COVERAGE.md`.
