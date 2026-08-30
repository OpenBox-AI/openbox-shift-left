---
title: "Three lanes, one pipeline — every claim by evidence strength"
plan: 260827-2301-go127-oss-three-lanes
phase: 14
date: 2026-08-30
status: docs reconciled; two lanes replay-verified; nothing live
---

# Three lanes — what is proven, and how strongly

The plan-wide record for stage B (phases 08–14). Phase reports carry the detail;
this one exists to stop the three lanes being averaged into a sentence, and to make
the unproven list specific enough to act on.

**Labels used throughout.** `MEASURED` — a number produced by running something over
real data, reported rather than enforced. `CONFORMANCE` — asserted on real outbound
bytes by a numbered conformance case. `REPLAY` — real recorded traffic through the
shipped code path on a host that cannot bind a socket. `UNIT` — asserted in a test
that constructs its own inputs. `UNPROVEN` — no evidence either way.

`REPLAY` is new in this plan and is the label most likely to be misread. It is
stronger than `UNIT`: the bytes are real, the code path is the shipped one, and the
drills are red on deletion. It is strictly weaker than running: **no socket, no
supervisor, no control plane.**

**Two wording rules the evergreen docs now follow**, because the overstatement this
phase exists to prevent is a single verb. "Verified" alone is never used of the two
new lanes — it is always "verified by replay", and the substitution is named within
the same sentence. And "byte-identical" never stands as a product property: it is
"byte-identical on replayed recorded exchanges, dial substituted", because the
*gateway's* byte identity IS socket-verified (81 tests green over real TCP,
2026-08-28) while the *transport CONNECT path's* is not, and averaging the two would
launder the weaker claim.

**How the phase's success criterion is read here.** Taken literally — "no document
claims a capability that phase 13 did not demonstrate" — the docs would have to drop
true, evidenced claims that phases 09–12 earned: the goproxy spike's 5/5 socket gate,
the owner's 25/25 socket-verified suite, and the OTLP intake run above. The criterion
is read as the phase overview states it — *what phases 09–13 actually proved* — with
each claim citing which phase and how strongly.

## The one sentence this phase exists to protect

> Both new lanes are verified by REPLAY: real recorded traffic through the shipped
> code path, bind-free, with the relay's upstream dial substituted
> (`gateway/gatewaytest`) and no socket anywhere. That proves the bytes the relay
> forwards and captures, the mapping, the gate and the caps — it proves nothing
> about bind, listen, TLS to a real socket, the OTLP HTTP intake, or what core
> stores. Those live only in the dormant `testbed/46-otel-lane.sh` and
> `47-transport.sh`.

It is now in `contracts/dev-event/COVERAGE.md` above the lane matrix, and its
substance in `CLAUDE.md`'s phase 13 block, `docs/architecture.md#assurance`,
`README.md` and `docs/getting-started.md`.

**One clause in it is easy to over-read.** "It proves nothing about … the OTLP HTTP
intake" is a statement about what *replay* reaches, not about the intake being unrun.
The intake has been crossed end to end by a synthetic export on a bind-capable host
(phase 09). Both facts are true and neither substitutes for the other; the docs state
them together wherever they appear.

## Claims by strength

### MEASURED

Every number below comes from openbox-logger run `20260827T063932Z-225cac` unless
stated: **one machine, one workload, all subscription-OAuth**. It generalizes no
further than that, and the three denominators are different populations — 5,340
recorded `/v1/messages` requests, 5,231 carrying `x-stainless-retry-count`, 5,049
flow-paired with a recorded response (the only population where a body-size
comparison means anything).

| Claim | Number |
|---|---|
| Request bodies over the 65,536-rune cap (OD1(c)) | **96.75%** — 4,885 of 5,049 · p50 529,175 · p95 1,705,840 · max 2,566,660 |
| Response bodies over the cap | 0.06% — 3 of 5,044 · p50 1,938 · max 85,903 |
| Spool cost per model call | **70,080 bytes**, ~674 ms → ~334 MB per 5,000-call session |
| Model calls whose context carries a `${OPENBOX_REDACTED_*}` marker | **2,820 of 5,340 — 52.8%**, 22,060 occurrences, **~200 distinct rewrite sites** |
| Redaction-site attribution | `redactEntropy` 28%, `secret_assignment` 27% (**ours: 55%**), gitleaks `generic-api-key` 13%, `AI_API_KEY` 10%, other 21% |
| Binary, darwin/arm64 via the release path (`GOWORK=off`), 2026-08-30 | **40,287,986 bytes (38.4 MB)** vs the 17 MB recorded pre-link; OD5 accepted +16.5 MB |
| Direct external requires | **19** across 15 modules. `telemetry`'s tree: **492 transitive packages / 124 modules in graph** (phase 09), vs `gateway`'s 381 / 206; leak check **zero** |
| Verdict census, 15 modules | 1,278 declared, 1,860 verdicts, **0 invisible**, 29 capability skips |
| Gates | **61 of 61 green** — 15 × {`-race`, vet, windows/amd64, linux/arm64} + `cli` under `GOWORK=off` |

Two of these are load-bearing for decisions and must not be restated in their older
form:

- **OD1(c)'s "~95%" is confirmed at 96.75%** — the estimate that justified the
  ruling is now a measurement, and it is slightly worse than the estimate.
- **The redaction corruption is ~200 sites, not 22,060 events**, amplified ~110× by
  context replay. The old "two false positives" sentence is retired. **`generic-api-key`
  is 13% of it**, so the narrow option in the plan's Open Q1 addresses a minority —
  see Unresolved.

### CONFORMANCE

38 numbered cases run, 38 pass (C8/C9 and C17 do not exist — deliberately deleted
under that decision and; C39 runs as `TestContentCaptureCredentialCoverage` rather
than as a subtest). These assert on real POSTed bytes and cover the content gate,
redact-before-send ordering, both failure-policy branches, the enforce cascade, HALT
rendering and the capture-off half of every content class.

The v1.6 contract bump moving **zero** outbound bytes is measured here rather than
inferred, which it was when phase 08 shipped.

### REPLAY (new in this plan; bind-free)

**`:proxy:` (transport)**

- a recorded 564,718-rune request and its response cross a real CONNECT, a real TLS
  handshake against the real project CA and the real `gateway.Gateway`, **byte-identical
  in both directions** (upstream dial substituted), reaching a real spool file;
- a recorded **60-frame SSE response** streams through the same path **per chunk**,
  with the buffering failure BOUNDED rather than hanging;
- no injected `X-Forwarded-For`, no relay-added `Accept-Encoding`;
- OD1(c) truncation on the real oversized body, **presence-anchored** (the head must
  be on the wire before a missing tail means anything);
- five mutation drills red on deletion; a sixth (`DisableCompression`) came back
  **green** and exposed that the assertion aimed at it could not detect it — fixed
  with a second case sending no `Accept-Encoding`.

**`:otel:` (telemetry)**

- a recorded OTLP export decodes through the collector's own unmarshaler: 20 records,
  16 event types;
- `api_request` → a conformant `TurnCompleted` carrying the lane discriminator, the
  model, four token counts and an `llm_completion` span; five distinct turns from
  five recorded requests, no id reused;
- the other **15 event types emit nothing and are COUNTED as drops** — a lane
  dropping everything is distinguishable from a quiet session;
- an un-elected lane spools nothing, asserted against the same fixture that yields
  five turns when elected;
- nine mutation drills red on deletion.

**Both**

- every committed fixture is free of the sentinel classes, gated by a test that
  DISCOVERS `testdata/corpus` directories rather than listing them.

### UNIT

The install choreography (unit → start → prove listening → env; rollback removes the
unit on any later failure), the derived producer election and its precedence, the
activation record's first-writer-wins and key-by-key restore, the lane `Spec`
rendering for all three units, `--remove-all`'s ordering ahead of the credential
gate, and the CA's name constraint. 21 mutation drills; **one was green until its
test was strengthened** — on darwin the install path reaches only
`launchctl bootstrap <path>`, and the assertion passed off the plist path, which
contains the label as a substring.

### UNPROVEN — specific, and non-empty

1. **No control plane has ever received an event from either new lane.** That core
   stores an `:otel:`/`:proxy:` `TurnCompleted` as its own row, that the span
   classifies as `llm_completion` after ingest, and that exactly one producer emits
   per session. Held by the dormant `testbed/46-otel-lane.sh` and `47-transport.sh`;
   `MAPPING.md` §7 items 34–39.
2. **The intake's PROTOBUF path — and any real client export.** The intake itself is
   *not* unrun, and saying so would erase evidence the product earned:
   `TestTelemetryCommandActuallyRecords` POSTs a real OTLP export to a real receiver
   on a real port and reads the governance event back off disk, and it **passed on a
   bind-capable host** (phase 09, plan.md footnote †). But it POSTs
   `application/json` (`telemetrycapture_test.go:67,168`), the replay decodes with
   `plog.JSONUnmarshaler`, and production sets
   `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`
   (`cli/internal/activation/keys.go:64`). **So no test in this repository drives the
   collector's protobuf decoder — the only path real traffic takes.** On top of that,
   Claude Code itself has never exported to this lane at all: its resource
   attributes and its batching are unseen too.

   *Found late, by a review pass, after this phase's first draft had already written
   "a synthetic export crossed the intake end to end" without the encoding caveat.
   True, and incomplete in exactly the direction this phase exists to prevent — the
   correction is now carried in all five documents that state it.*
3. **The 13 telemetry env key names.** The one claim this repo cannot test about
   itself: every test asserts JSON we wrote, and the client silently ignores a name it
   does not recognise, so a rename yields a green suite and a receiver that never gets
   a record. Copied verbatim from the run that produced the corpus and pinned as a
   literal list.
4. **The desktop-app and subscription-OAuth coverage both lanes exist for.** This is
the motivating claim of that decision and it is **intent, not measurement**. Do
not read "built for it" as "covers it".
5. **A real `launchctl`/systemd install.** No supervisor has run one of these units.
6. **Bind, listen, TLS to a real socket, the real dialer.** Everything above runs over
   `net.Pipe` and an in-memory upstream.
7. **Refusal, on all three in-path lanes.** Written, tested, and called by nothing.
Probe A is the only source for; `probes/refusal-injector/` is now its instrument and
needs a bind-capable host, a real install and credentials. **The all-zero
`x-stainless-retry-count` in the corpus is evidence the header exists, not evidence
about retry-around behaviour.**
8. **Brotli-encoded exchanges.** Excluded from fixtures (1,573 of the recorded JSON
   responses) because the standard library cannot decode them.
9. **`GOWORK=off` for `transport/`.** Unverifiable on the dev host — `x/net v0.50.0`
   absent from the module cache. `cli`, which the release artifact builds, **is**
   verified.
10. **Windows** stays build-verified only.

## What this phase changed, and what it found

Docs reconciled: `contracts/dev-event/COVERAGE.md` (new §1b lane matrix + the replay
sentence), `contracts/dev-event/MAPPING.md` (§7 items 34–39),
`docs/architecture.md#assurance` (lane limits, CA blast radius, OD1(c), OD4, the
per-module dependency table), `docs/data-and-privacy.md` (three lanes, the CA, the
removal section), `docs/getting-started.md`, `README.md`, `CLAUDE.md`.

**Four false claims were found and fixed**, which is the point of the sweep:

| Where | Claimed | Actually |
|---|---|---|
| `docs/architecture.md:223` | "**Neither lane exists yet** — the contract carries their discriminators and nothing emits them" | both built and installable since phase 12 |
| `contracts/dev-event/COVERAGE.md` | "`:proxy:` … no response body has traversed it yet" | retired by phase 13 — byte-identical in both directions, plus 60-frame SSE |
| `cli/cmd/openbox/main.go:396` (`--remove-all` help) | deletes "the CA, logs **and spool**" | `purgeLaneData` **deliberately keeps the spool** and says so in a comment — it is outside `~/.openbox/` and shared with the hook path |
| `docs/getting-started.md` | same spool claim, in prose | same |

The last two are the same defect in two places, and they are the shape this repo
already names: **a doc and the code that contradicts it, where the code is right.**
The help string was a user-facing claim about data destruction, so it was corrected
rather than only documented — the code's behaviour was not touched. `cli` is green.

**Also corrected:** the dependency and module counts in `CLAUDE.md` were stale at
stage-A values (seven direct requires, twelve modules) against an actual 19 and 15,
and the binary-size figure had never been measured after the telemetry link.

## Unresolved

1. **The redaction-corruption finding needs an owner ruling, and it is no longer the
   question the plan's Open Q1 asks.** That question offers "disable `generic-api-key`
   alone" as the narrow option; the site census says that rule is **13%** and our own
   two generic rules are **55%**. Two bind-free experiments would make the fork
   measured rather than a matter of taste: (a) classify the ~200 sites as true or
   false positives by inspection; (b) re-run the detector over the corpus with each
   generic rule disabled in turn and diff what uniquely disappears. Surfaced, not
   decided — detection scope is OD-class.
2. **`CLAUDE.md` is over the repo's 800-line docs bound** (~1,140 lines), and was
   already 1,051 before this phase. Every phase adds a block in the house voice and
   none has ever been retired. Trimming it is a judgement about which hard-won
   paragraphs are safe to lose, which is an owner call, not a docs-reconciliation
   one.
3. **The backend ask is unchanged and now carries a capacity number:** server-side
   dedupe on developer events (the lost-200 double-store window is irreducible
   client-side), against ~334 MB of spool per 5,000-call session.
4. **that decision stays DRAFT on §9 alone** — what refusal shape Claude Code does not
   retry around. §5 is reversed (OD2), §8 is answered by measurement, §10 is decided.
   Filling §9 in by inference is the overstatement this product exists to prevent.

## Review outcome

A code review of this phase found **four defects in its own output**, all now fixed.
They are recorded rather than quietly corrected, because three of the four are the
same shape and it is the shape that transfers.

1. **A FIFTH copy of the false spool claim**, in `CLAUDE.md`'s phase-12 paragraph —
   outside every hunk this phase touched, so a sweep run over the files being edited
   missed it. Four of five is what grepping your own diff gets you.
2. **A matrix cell re-erased the nuance the prose had just gained.** `COVERAGE.md`
   §1b's evidence row still read "HTTP intake unrun" after the intro above it had
   been corrected. The table is the part a reader scans; a qualifier that survives
   only in prose has not survived.
3. **"a cost, a duration and two request ids" was FALSE in three documents.**
   `turnFor` never sets `ev.Cost` (the server derives cost from a pricing table) and
   `requestIDFrom` returns ONE id chosen from two candidate attribute names. The
   root cause is worth more than the fix: **the claim was copied from
   `mapper.go`'s own doc comment, which had already drifted from the function
   beneath it.** Trusting a comment is not reading the code. The comment is now
   corrected at source, and says why.
4. **The dependency count was ambiguous**: 19 distinct external modules, 20 by
   per-module entry (`renameio` is required by both `cli` and `hookflow`), and the
   enumeration omitted `decision`/gitleaks entirely, so a reader summing the list
   could not reconcile it. Both numbers are now stated with the reason they differ.

Findings 1, 2 and 3 are one failure mode: **a fact repeated across documents is
corrected per document.** The qualifier survived in the longest copy each time —
`MAPPING.md` for the protobuf gap, the COVERAGE intro for the intake, the
architecture table for the dependency count — and was lost in every terser one. For
a claim that appears in N places, the check is not "is it true here" but "is it
equally qualified in all N".

## Deviation from the phase file

The phase names this report `plans/reports/verification-260827-three-lanes.md`. It is
written at `verification-260830-0022-three-lanes.md` instead, following the
repository's live report-naming convention and today's date — a 2026-08-27 stamp on a
2026-08-30 document would be a small falsehood in a phase about not overstating.
