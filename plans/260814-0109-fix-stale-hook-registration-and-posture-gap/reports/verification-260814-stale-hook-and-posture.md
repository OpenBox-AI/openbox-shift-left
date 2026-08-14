# Verification — stale hook registration + posture wire gap

Date: 2026-08-14 · Repo: openbox-shift-left @ `98c62d2` + this change · Plan:
[plan.md](../plan.md)

Every claim split by evidence strength. The repo rule holds: **unit tests are not
evidence that a hook works.**

## Sweep run (mirrors `.github/workflows/ci.yml:40-85`)

| Gate | Command | Result |
|---|---|---|
| Formatting | `gofmt -l .` | **failed on the first pass**, green now |
| go.work covers every module | `find . -name go.mod \| wc -l` vs `go list -m \| wc -l` | 11 = 11 ✅ |
| Build | `go build $MODS` | green |
| Vet | `go vet $MODS` | green |
| Test | `go test -race $MODS` | 22 packages, all green |
| Cross-compile | `GOOS=windows GOARCH=amd64 go build $MODS` | green |
| Cross-compile | `GOOS=linux GOARCH=arm64 go build $MODS` | green |
| Tier vocabulary in docs | `ci.yml:100-122` grep over `docs/` + `README.md` | green |

`$MODS` must be expanded under **bash**; zsh does not word-split an unquoted
parameter, so `go build $MODS` silently matches no packages and still exits 0.

**The formatting gate is CI's FIRST step and the original sweep did not run it.**
Deleting `TestPosture_StalenessNamesTheSkipReason` from both adapters left a
trailing blank line in one file and a doubled one in the other, so `gofmt -l .`
named `adapters/claude-code/posture_test.go` and `adapters/codex/posture_test.go`
and CI would have failed before it compiled anything. The general lesson is the
sweep's, not the change's: a sweep that mirrors CI has to mirror ALL of CI, and
the gates that are not `go test` are the ones a hand-written checklist drops.

## Proven by unit test

| Claim | Evidence |
|---|---|
| An OpenBox entry at a stale engine path is replaced, not kept beside the new one | `TestReInitReplacesAnOpenBoxEntryAtAStaleEnginePath` — every event seeded at engine A (quoted, incl. the rewake handler), asserts exactly one owned handler per event at engine B and zero occurrences of A |
| A developer's own hook survives, including a compound command embedding our invocation | same test (`my-own-linter`, `my-linter && "<A>" hook claude-code PostToolUse`) + `TestReInitAddsTheNewHooksExactlyOnce` (unchanged, still green) |
| The classifier recognizes our argv shape only | `TestOwnedLocalHookRecognizesOurArgvShapeOnly` — 11 cases incl. wrong event key, non-command type, unterminated quote, bare token, trailing arg |
| The replacement is announced, and only when something was replaced | same test asserts the notice names stale path, new engine, settings path, events; `TestReInitAtTheSameEnginePathPrintsNothing` asserts silence otherwise |
| Idempotency across the quoting change still holds | `TestLocalHooksIdempotentAgainstAnUnquotedLegacyEntry` (unchanged, still green) |
| `localhooks_quote_test.go` is byte-identical | `git diff --stat` on that file is empty |
| doctor and init agree about what "ours" means, for BOTH conditions doctor reports | `TestTheAuditAgreesWithWhatReInitRepairs` (2 engines → one `writeLocalHooks` → 1) and `TestReInitCollapsesADuplicateRegistrationAtTheSameEngine` (one invocation registered twice at one path → collapsed, gate and watcher both intact) |
| doctor warns on 2 engines, is silent on 1 (incl. PreToolUse's legitimate 2 handlers), survives absent/invalid-JSON with an unchanged exit code | `cli/cmd/openbox/doctor_test.go`, 5 tests |
| `decision_authority` + `failure_policy` reach the emitted metadata, and no `bundle_*`/`staleness` key can | `TestPostureReportsDecisionProvenance` subtests "both reach the emitted metadata…" and "failure_policy tracks fail_closed onto the wire" — built through `EffectivePosture()`, not `Posture{}`, so a zero value cannot pass vacuously |
| `adapters/common/git`'s identically-named attestation fields untouched | `git diff --stat adapters/common/git` empty |

## Proven against the real binary (no stack)

- `openbox doctor` on a seeded project holding an **unquoted** stale registration
  (a session-scratchpad path, exactly the live shape from the issue) plus the
  current quoted one: both engines listed, both WARNINGs printed, remedy named.
- **A real `openbox init` repairing a real project** (added in review; the first
  pass drove only `doctor` this way). Scratch HOME + OPENBOX_HOME, credentials
  seeded, first `init`, then the whole engine path rewritten to
  `/opt/openbox-from-another-home/bin/openbox` and two foreign hooks added. One
  re-init produced: every event with exactly one owned handler at the new engine;
  the gate and the watcher both present; `my-own-linter` and the compound
  `my-linter && "<stale>" hook claude-code PostToolUse` both preserved (the
  compound is why a naive occurrence count reads as a survivor — it is a foreign
  command we must not touch); the stderr notice naming old engine, new engine,
  settings path and all 11 events; the settings file still valid JSON; and a
  second plain re-init printing nothing.
- **The duplicate-at-one-path repair, end to end on the real binary**: doctor
  warns → `init` prints "removed duplicate OpenBox hook registrations … events:
  Stop" → doctor is clean, with `rewake claude-code` and the PreToolUse gate each
  still present exactly once.

## Proven by construction / static reading

- The five bundle keys were already absent from every real session:
  `effectivePosture()` in both adapters sets only adapter/provider fields and
  `EffectivePosture()` never set them, so the `if v == ""` guard dropped all five.
  The removal is therefore a **no-op on the wire**; only the two additions are new
  bytes. The live `SessionStarted` that *did* carry `bundle_sha256` came from the
  stale engine — which is the same defect Phase 01 fixes.
- `TestOnlyTheRegistryImportsAdapters` (`cli/internal/providers/layering_test.go`)
  forbids `cli/cmd/openbox` importing an adapter. Doctor reaches the audit through
  `providers.AuditProjectHooks`. This was the plan's R5 pre-decided response and
  it fired: the direct import the phase file sketched would have failed CI.

## NOT proven — needs a live stack

| Claim | What would prove it |
|---|---|
| A re-inited settings file still produces events in a real session | `testbed/10-onboard.sh` end to end. **The risk is real**: this change now removes entries and prunes emptied ones, so a merge that produced an unreadable settings file would show up as a silent zero-event session. Pre-decided response (Phase 01 R5): stop-and-replan; fall back to leaving empty entries as harmless litter rather than pruning. |
| The double-count actually disappears end to end | Two engine paths + a live session + a row count in `governance_events`. |
| `decision_authority` lands in `governance_events` | `testbed/30-enforce.sh:185-186` already asserts `control_plane`/`fail_open` on the SessionStarted row and **failed before this change**; Phase 02 is what makes it pass. `:187` (`assert_absent bundle_integrity`) passed before only via the empty-value guard and now passes structurally. No testbed edit was needed for Issue 2. |
| The new dormant assertions in `testbed/10-onboard.sh` are themselves correct | First live run. De-risked as far as possible without one: `bash -n` clean, and the `sed` rewrite was executed against a real-shaped settings file and checked for a valid-JSON result. **The first draft of that `sed` was wrong** — it anchored on `"` but Go encodes the engine path inside *escaped* quotes (`\"`), so it matched nothing while still exiting 0. It now derives the engine path from the file (`grep -o '[^"\\]*/bin/openbox'`) instead of reconstructing it. |

## Known limits that ship with this

- The repair happens **only on the next `openbox init` in that directory**. Nothing
  back-fills already-stored duplicate events.
- An **unquoted** engine path *containing a space* is still unrecognized (the
  leading token ends at the first space). Only a pre-quoting build could have
  written one, and those hooks never started at all, so the leftover is dead
  weight rather than a second live engine. `openbox doctor` surfaces it.
- An owned invocation filed under the **wrong event key** is classified foreign and
  kept — deliberate, to keep the safe direction. Doctor still flags it when the
  path differs.
- A same-path entry **missing** PreToolUse's `rewake` handler is not repaired
  (pre-existing entry-level append decision, unchanged by this work).
- Global/managed scope (`cli/internal/managed/managed.go`) was **not audited** for a
  differently-shaped stale-engine problem. Follow-up, unconfirmed.

## Deviations from the plan, and why

1. **`ownedLocalHook` takes the handler `type` as a parameter** rather than leaving
   that check at the caller. Requirement 1 makes `type == "command"` part of the
   ownership definition, and Phase 03 reuses the classifier — splitting the
   definition across two call sites is the same "two opinions about what ours
   means" that caused the original bug.
2. **`splitEngineToken` returns the token as well as the remainder**, where the
   phase file specified `stripEngineToken(cmd) (rest, ok)`. The comparison needs
   the engine path, and so does the notice; one function returning both beats
   parsing twice.
3. **Doctor reaches the audit through `cli/internal/providers`**, not by importing
   the adapter. Forced by `TestOnlyTheRegistryImportsAdapters`; this is Phase 03 R5's
   documented pre-decided response.
4. **An extra troubleshooting row** in `docs/getting-started.md` for the observed
   symptom ("every tool call appears twice"). The operator-visible symptom is bad
   telemetry, not a hook problem, so the doctor capability line alone would not be
   found by anyone actually hitting this.
5. **The sweep de-duplicates as well as replaces** (added in review). Phase 01 R3
   said "owned + same engine ⇒ keep", which left our own invocation registered
   twice at ONE path in place — the second condition Phase 03's check reports,
   with `openbox init` named as its remedy. On the real binary that loop never
   terminated: warn → run the remedy → warn again. `sweepStale` now keeps the
   first handler of each owned invocation at the installed engine and drops later
   copies, keyed by INVOCATION so PreToolUse's gate and watcher survive, with its
   own stderr notice.
6. **Both notices moved to after the write.** They ended "The old engine no longer
   runs for this project" — a claim about the file on disk, printed before a write
   that can fail.

## Review findings this report was amended for

The implementation was reviewed against the plan after being marked complete.
Four findings, all fixed here:

| # | Finding | Severity |
|---|---|---|
| 1 | `gofmt -l .` — CI's FIRST gate — failed on the two `posture_test.go` files Phase 02 edited. The sweep checklist listed build/vet/test/cross-compile and not this. | CI-blocking |
| 2 | `openbox doctor` warned "registered more than once … Run `openbox init`", and `init` did not repair that case — the warning never cleared. Contradicts Phase 03's own stated acceptance test. | Real defect |
| 3 | The replacement notice claimed the old engine no longer runs *before* the write that makes it true. | Honesty |
| 4 | `TestPostureMetadata_NoSecretShapedValues` says "deliberately hostile input in every adapter-supplied string" but gave `ProviderManaged` a non-secret-shaped value, so that field asserted nothing. | Test integrity |

Everything else in the plan was verified as implemented: the classifier, the
sweep, the notice, the audit, doctor's section, the two posture keys, the five
deletions, all doc surfaces, and the dormant testbed block.

## Unresolved questions

1. Does `openbox init` need to warn when it finds an OpenBox entry it can attribute
   to no engine at all (e.g. the unquoted-spaced-path case)? Today it is silently
   preserved and only `doctor` mentions it.
2. Global/managed scope is unaudited (plan unresolved question 1) — is there an
   equivalent stale-engine path in `managed.applyFile`/`render`?
3. How many existing installs already double-count is unmeasurable from here, and
   nothing back-fills the stored duplicates. Does that need a server-side dedupe
   ask, or is client-side repair + doctor enough?
