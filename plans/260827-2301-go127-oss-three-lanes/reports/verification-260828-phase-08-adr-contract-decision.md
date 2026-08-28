# Phase 08 verification — ADR-0022, contract v1.6, ADR-0021 amendments

**Date:** 2026-08-28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture` · **Gates:** phases 09–14

Decisions before code. **ADR: [ADR-0022](../../../docs/adr/ADR-0022-native-telemetry-and-transport-lanes.md)**
(accepted); ADR-0021 §5 reversed, §8 completed, §10 decided.

## Verdict

Done, with **one scope expansion** (argued below) and **one verification gap that
is environmental, not optional** (stated before the evidence, so it cannot be read
as a footnote).

The gap: **this sandbox denies every TCP bind** — proven, not assumed:
`net.Listen` fails on `127.0.0.1:0`, `[::1]:0` and `:0` with
`bind: operation not permitted`. So **322 listener-dependent tests across 6
modules could not run** — enumerated by name in
[`bind-blocked-tests-260828.txt`](bind-blocked-tests-260828.txt), because a count
is not an artifact. Among them is
`adapters/claude-code.TestContentCaptureConformance`, which carries **C1–C41**.
The plan's acceptance criterion 2 is literally *"conformance C1–C41 pass
unmodified"* — **that criterion is UNVERIFIED here.** It is not weak evidence; it
is no evidence. This repo's own rule names the failure it would be to omit it:
*"a verdict-set diff over tests that cannot run is not evidence."*

## The scope expansion, stated first

The phase named `oneOf[8]` (`TurnCompleted`) and the `session_rollup` repair. I
also repaired **`TurnStarted`**, which the phase did not name.

Why it is not scope creep: the phase's own success criterion is *"`session_rollup`
event validates"*, and Codex emits the rollup as a **pair** — `MapUsageRollup`
builds one `base` and returns it as both halves. `TurnStarted` required `turn_index`
**unconditionally**, so the opening half fails no matter what is done to `oneOf[8]`.
The criterion is literally unreachable without this repair.

Stated precisely, because I first wrote it too broadly: v1.5's half-repair cost the
**gateway** nothing, since `gatewayemit.EventFor` emits `TurnCompleted` only and
deliberately so. What it broke is the Codex rollup pair — and what it *would* break
is any later lane emitting a pair, which is what phases 09–11 intend. So the repair
is required by a shape shipping today, not only by a prospective one.

Under `--yagni` this survives: YAGNI cuts scope not needed for the stated outcome,
and this is the minimum that makes the phase's own success criteria true.

Both halves now `$ref` **one** definition (`$defs.turnProducer`) rather than
restating the rule, because restating it per branch is precisely how the two halves
drifted apart. A sixth producer is added once.

## Empirical findings — the risk table's open question, resolved

The phase pre-decided: *"if the rollup already validates, drop the repair and record
the no-op finding."* It does **not** validate. Measured before any edit:

| Shape | v1.5 verdict | Cause |
|---|---|---|
| Codex rollup `TurnCompleted` | **REJECTED** | `session_rollup` undeclared + `additionalProperties:false`; and the nested `oneOf` required `turn_index` or `gateway_request_id` |
| Codex rollup `TurnStarted` | **REJECTED** | same, plus `turn_index` required unconditionally |
| any **paired** non-hook lane's `TurnStarted` | **would be REJECTED** | `turn_index` required unconditionally — v1.5 repaired only the close. Latent rather than live, since the gateway emits no opening half |

So **Codex session usage has been failing its own contract since v1.1** — both
halves, every session. Nothing noticed because no fixture carried the shape and no
adapter test validated a turn shape as real mapper output. Five fixtures and one
mapper-seam test now do.

## What changed

| Surface | Change |
|---|---|
| `docs/adr/ADR-0022-…md` | **NEW**, accepted — lanes as claims, the election, both namespaces, OD1(c)/OD2/OD3/OD4 with dates, D-OSS-1/2/3 + D-GO-1, the goproxy-is-not-Docker/mitmproxy statement, the `transport/` module consequence, and the 8 sentinel tests later phases must satisfy |
| `docs/adr/ADR-0021-…md` | §5 **reversed** (original kept, with what changed and why); §8 coverage question **completed** with the 2026-08-27 measurement; §10 **decided** (detection-only for OAuth, fingerprint refusal for API keys); header rewritten — 3 TBDs → 1 (§9 alone) |
| `docs/adr/README.md` | 0022 row added; 0021's status row corrected |
| `schema/dev-event.schema.json` | `x-schema-version`/`$id`/`schema_version.const` → **1.6**; `x-changelog["1.6"]`; **3 new properties** (`session_rollup`, `otel_request_id`, `proxy_request_id`); **NEW `$defs.turnProducer`** (5-branch `oneOf`) `$ref`'d from **both** turn branches |
| `client/event.go` | `OtelRequestID`/`ProxyRequestID` (`omitempty`, declared last before `WorkspaceID`); `SchemaVersion` → `1.6` |
| `client/payload.go` | `turnActivityIDFor` gains `:proxy:` and `:otel:` branches |
| `client/turn_key_pin_test.go` | **+3 tests** — new-lane pins, 6-shape disjointness, a 5-rung precedence ladder. **No existing pin altered** (additions only, verified by diff) |
| `conformance/discriminator_test.go` | **NEW** — 6 tests over **both** turn halves, incl. the `session_rollup:false` edge and a schema↔test binding assertion |
| `conformance/testdata/valid/` | **+4 fixtures**; 26 existing fixtures bumped to `1.6` |
| `adapters/codex/conformance_test.go` | **+`TestUsageRollupPairIsConformant`** — closes the mapper→schema seam; no adapter test validated ANY turn shape as real mapper output before this |
| `reports/bind-blocked-tests-260828.txt` | **NEW** — the 322 tests this host could not run, by name |
| `conformance/schema_guard_test.go` | `maxLength` added to the reviewed keyword set, with the review recorded |
| `MAPPING.md` | ADR-0022 banner; **+4 field rows**; §7 items **31–33** |
| `COVERAGE.md` | both lanes recorded as **contracted, not built**, with the claim asymmetry stated |

## Evidence

### Mutation drills — both mechanisms proven load-bearing

Not "the tests pass", but "the tests fail when the mechanism is deleted":

| Drill | Expected | Result |
|---|---|---|
| delete `$ref` from the `TurnCompleted` branch | `TestTwoDiscriminatorsRejected` **RED** | pass — 10 pairs went green-when-they-should-fail |
| delete `maxLength` + `pattern` from both new ids | `TestNewRequestIDsAreBounded` **RED** | pass — newline/space/control/empty all accepted |
| **revert `TurnStarted` to v1.5** (`required:[event_type,turn_index]`, no `$ref`) | the half-repair is caught | pass — **`TestUsageRollupPairIsConformant` RED**, plus 3 conformance tests. This is the direction that shipped once |
| **swap the `gateway` and `otel` rungs** in `turnActivityIDFor` | precedence pin **RED** | pass — caught at the `gateway` rung. The single-assertion pin I wrote first would have missed this |
| restore each | green | pass |

### Why the tests are green for the right reason

`TestTwoDiscriminatorsRejected`, `TestNoDiscriminatorRejected` and
`TestNewRequestIDsAreBounded` **already passed before the change** — vacuously, via
`additionalProperties:false` rejecting undeclared fields and `TurnStarted` rejecting
everything without `turn_index`. `TestOneDiscriminatorValidates` is the control that
retires that: once all five fields are declared **and** validate alone, a rejection
can only come from the `oneOf`. It was RED before and is green now.

### The `$ref`-sibling hazard, checked

Draft 2020-12 applies keywords sitting beside `$ref` (draft-07 ignored them). If a
validator ignored them, `event_type`'s `const` would stop discriminating, both turn
branches would reduce to the same `$ref`, and every turn event would match **two**
top-level branches. That fails the top-level `oneOf` **loudly** — it cannot degrade
silently — and every turn fixture validating is the standing proof it does not
happen.

### Contract bump moves no outbound bytes — direct evidence

**`client/golden_test.go`'s 20 golden wire fixtures pass unchanged with
`SchemaVersion = "1.6"` in effect and zero edits under `client/testdata/`**
(`git status client/testdata` → 0 files). That is a direct observation of the
outbound bytes, not an argument about them, and it covers the rollup wire shape too
(`activity_usage_rollup_started`).

Supporting, and now secondary: `client/payload.go` never writes a `schema_version`
key into the wire payload at all, and no Go file anywhere hardcodes `"1.5"` — every
reference reads `client.SchemaVersion`, and every non-test use is an assignment.
That also settles the mixed-spool case: a 1.5-stamped spooled event flushed by a
1.6 binary maps to identical wire bytes, because nothing reads the field at
runtime.

### Test sweep

| Module | Non-listener tests | Could NOT run (bind denied) |
|---|---|---|
| `client` | **green** (92) | 31 |
| `contracts/dev-event/conformance` | **green** (all) | 0 |
| `adapters/claude-code` | **green** | 82 — **incl. C1–C41** |
| `adapters/codex` | **green** | 37 |
| `cli` | **green** | 101 |
| `gateway` | **green** (29) | 52 |
| `actions/openbox-git-action` | **green** | 31 |
| `devconfig`, `git`, `hookflow`, `decision`, `provider` | **green** | 0 |

`-race` on both changed modules: green. `go build` and `go vet` over all 12
modules: green. Cross-compiles `windows/amd64` and `linux/arm64`: green.

## What is NOT verified

1. **C1–C41 did not run.** Acceptance criterion 2 is open. The mitigating
   evidence above (no `schema_version` on the wire, no hardcoded version) says the
   bump *should* be inert for them; that is inference, and inference is what this
   repo's rules exist to distrust. **Re-run on a host that can bind before merge.**
2. **Nothing ran against a live stack.** MAPPING.md §7 items 31–33 are the list:
   whether core stores a `<session>:usage:rollup` pair as one row (item 31 — the
   repair's first real evidence, since this shape has never validated), whether ids
   stay disjoint across lanes in storage, and whether the election holds.
3. **Neither new lane exists.** The contract carries their discriminators and no
   producer emits them. COVERAGE.md says so explicitly so a declared field is not
   mistaken for a shipped lane.
4. **`gateway_request_id` keeps only its imperative bound.** Deliberate, recorded in
   both the schema and MAPPING.md: tightening a field a shipped gateway already
   emits would be a contract break wearing a repair's costume. The asymmetry between
   it and the two new ids is a decision, not an oversight.

## Changed after advisory review

An advisory pass found one **real defect** and several gaps. All were verified
independently before acting, and all are fixed:

- **`session_rollup: false` (defect).** The branch was presence-based (`required`)
  while the client derivation is truthiness-based (`if ev.SessionRollup`). Verified
  broken in **both** directions: `false` alone *validated* (an event the client can
  mint no `activity_id` for), and `turn_index` **+** `false` was *rejected* (it
  matched two branches, so a legitimate hook turn failed). Fixed by `const: true` on
  the branch — the schema moves, not the client, because making the client
  presence-based would change when `:usage:rollup` is minted. Pinned by
  `TestRollupFalseIsNotADiscriminator`, both directions.
- **The mapper→schema seam was untested for every turn shape.** `TestEmittedEventsAreConformant`
  validates *real mapper output* with no listener — and covered **zero** turn shapes
  in either adapter, because the rollup comes from `MapUsageRollup`, not `Map`. So
  the one shape v1.6 repairs was proven only against hand-built JSON: a fake at each
  end of a seam, the pattern `CLAUDE.md` records for the gateway emitter. Closed by
  `adapters/codex.TestUsageRollupPairIsConformant`, and the revert drill above
  confirms it is load-bearing.
- **The precedence pin held only its top rung** — a swap of any two rungs beneath
  passed. Peeled into five assertions; the swap drill above now catches it.
- **`turnDiscriminators` was bound to the schema by a comment.** Now asserted
  (`TestDiscriminatorListMatchesTheSchema`): a sixth lane added to one side alone
  fails immediately instead of silently reducing coverage.
- **Two prose overstatements corrected.** The changelog claimed "every 1.5 event
  that passed still passes", which is false modulo the const-pinned version marker
  itself. And ADR-0022 implied v1.5's half-repair broke the gateway's opening half —
  it did not: `gatewayemit.EventFor` is `TurnCompleted`-only by deliberate design,
  so what v1.5 actually broke was the **Codex rollup** pair. Both corrected.
- **The unrunnable set is now a named artifact**, not a count:
  [`bind-blocked-tests-260828.txt`](bind-blocked-tests-260828.txt) lists **322
  tests** by name across the six modules, `TestContentCaptureConformance` among
  them.

## Changed after code review

A second review pass (four angles) found three more real items, all fixed, plus two
declined with reasons:

- **I built one safeguard and skipped its twin, in the same diff.** The client had
  *two* hand-maintained producer lists (disjointness, precedence) with no self-check,
  while I gave `conformance.turnDiscriminators` exactly such a check. Collapsed to one
  `turnLanes` list, bound to the contract by `TestTurnLanesMatchTheContract`. Drilled:
  a sixth lane added to the schema alone now fails **both** self-checks by name.
- **Two tests my change superseded were left standing.**
  `TestGatewayAndHookTurnIDsNeverCollide` became a strict subset of the new
  6-shape disjointness test, and `TestOneOfDiscriminatorSemantics` was a
  self-described throwaway "on the shape phase 08 will use" — phase 08 shipped.
  Both retired, each replaced by a comment saying where the coverage went. Leaving
  them would have re-created, at the Go level, the same "restated in two places,
  drifted apart" failure this ADR repairs in the schema.
- **A fourth copy of the gateway overstatement, in `MAPPING.md`.** My own report
  above claimed it was "both corrected" — it was not; the banner still said the
  gateway's opening half failed. Corrected, and this bullet exists because the
  report overclaimed a correction.
- **The precedence test built each rung by mutating one shared event.** One wrong
  closure would have failed every rung after it, none naming the mistake. Each rung
  now builds its own event in its own subtest; the swap drill fails at exactly one.

**Declined, with reasons:**

- **Caching the compiled schema** (`ValidateDevEvent` recompiles per call; this diff
  took the package 41 → 89 calls). Measured rather than assumed, which the reviewer
  could not do: the whole package runs in **~0.3s** (0.357/0.287/0.295 over three
  runs). Adding package-level mutable state to a test-support package to save a
  fraction of a second is a bad trade. If a later lane makes it material — the
  pairwise test grows as 2·C(n,2) — the fix is a `sync.Once` and nothing here blocks
  it.
- **A test binding the schema's declared id bound to `gatewayemit.printableASCII`.**
  Real risk, but **no producer sets either id yet**, so the coupling does not exist
  to test. Recorded instead as ADR-0022 sentinel **#9**, owned by whichever phase
  adds the first producer — where it will actually bite.
- ~~**Retrofitting `gateway_request_id`'s declarative bound**~~ — **REVERSED on
  evidence.** A later pass traced every assignment: the field has exactly ONE
  production path (`gatewayemit.EventFor` <- `Emitter.requestID`), already gated by
  `usableRequestID` at `maxRequestIDLen` = **128**, the same number. So "tightening
  would reject what a shipped gateway emits" was false, and leaving it out put three
  fields of one kind at two contract depths with the oldest as the copy template.
  Retrofitted, and `gatewayemit.TestGatewayIDBoundMatchesTheContract` now holds the
  declarative and imperative bounds together for **all three** ids — so phases 09/11
  inherit a live check instead of an obligation. Reversing it meant correcting four
  artifacts that recorded the old call (schema changelog, schema property, MAPPING.md,
  ADR-0022 §4 + sentinel 9); a fifth cited a test name that no longer existed.
- **Making the client presence-based on `SessionRollup`**: unchanged; it would move
  when `:usage:rollup` is minted.

## Changed after a second review round

Five reviewers ran over the change; the substantive results:

- **A `turnAssistantSpan` collision I had not seen.** `turnSpanID` hashes
  `turnActivityIDFor`, which returns `""` for a turn naming no producer — so every
  such turn would share one fixed hash across all sessions. Core dedupes spans on
  `(span_id, stage)`, so the second would be absorbed and its assistant text dropped
  silently: the exact collision the namespaces exist to prevent, through the one path
  that never consulted them. Unreachable from the shipped adapters, but the
  discriminator set just went 2 -> 5. Guarded, pinned by
  `TestTurnWithNoProducerGetsNoSpan`.
- **The Claude Code turn seam, which I had declined.** I closed the mapper->schema
  seam for the Codex rollup and listed Claude Code as an open question. It is the
  highest-volume producer of the shape v1.6 changed, so `TestTurnPairIsConformant`
  now validates its real `MapTurn` output — three postures — and drills red when
  `MapTurn` sets two discriminators or drops its index. (It correctly does NOT bite
  on the `TurnStarted` revert: that lane always sets an index and was never affected.)
- **The `gateway_request_id` reversal above.**
- **Four more copies of the gateway overstatement**, in the two schema descriptions,
  `CLAUDE.md` and `client/event.go`. This report had already claimed the correction
  was complete — twice, wrongly. The sweep is now grep-verified at zero rather than
  asserted.
- **Two understating/stale claims:** `client/event.go` still framed requirement 8 as
  "the two producers", and `docs/architecture.md` still said the OAuth coverage
  question was unmeasured — the understating mirror of the same failure, in the file
  `CLAUDE.md` names as the record of what the evidence proves.
- **A cross-file citation this change made stale** (`gatewayemit` cited a
  `client/payload.go` line number; the branch moved). Re-cited by symbol.
- **Positive-boundary case added:** the id bound was tested only by rejections, so an
  off-by-one that started rejecting a legal 128-character id would have shipped.

**A tooling incident worth recording.** Running `go mod tidy` in `cli` produced a
`go.sum` in which two real base64 checksums had been replaced by the literal
`${OPENBOX_REDACTED_ENTROPY}` — the repo's own secret-redaction placeholder, matching
high-entropy hash strings. Builds then failed with a checksum mismatch. This is
exactly the false-positive class `CLAUDE.md` already documents ("the enforce-path
redactor REWRITES file bodies, so false positives corrupt files"), observed on a real
file. Reverted; `go.sum` is clean and verified, and the test that had needed the new
dependency was rewritten to use stdlib only, so **no `go.mod`/`go.sum` change ships in
this phase**. The open `generic-api-key` item in `CLAUDE.md` now has a second
demonstrated victim class: lockfile checksums.

## Unresolved questions

1. **Should phase 13's fixtures replace my four hand-built ones?** Phase 13 promises
   sanitized *real* fixtures. Mine should be superseded, not accumulated beside them.
2. **Should the Claude Code adapter get the same mapper-seam coverage the Codex
   rollup just got?** Its `Stop`-derived turn pair is also listener-free (the
   transcript window is a temp file) and is also absent from
   `TestEmittedEventsAreConformant`. I stopped at the shape v1.6 actually repairs;
   extending it is phase 13's natural home, but it could equally be argued as
   belonging here.
