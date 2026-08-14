# Phase 01 — Stale-engine-aware hook merge

## Context links

- Plan: [plan.md](plan.md)
- Issue §1: `plans/reports/issue-260814-0106-stale-hook-registration-and-posture-gap.md:10-84`
- Research: `research/researcher-01-hook-registration.md`
- Precedent to port: `adapters/codex/installer.go:217-302`

## Overview

- **Date:** 2026-08-14
- **Description:** `writeLocalHooks` recognizes OpenBox hook handlers by argv shape regardless
  of engine path, drops the ones pointing at a different engine, appends the current entry, and
  prints what it replaced. Foreign hooks untouched.
- **Priority:** P1 (high — silent double-counting of every governed tool call)
- **Implementation status:** complete
- **Review status:** reviewed

## Key Insights

- The bug is one function: `hasLocalHookCommand` compares whole command strings after only
  stripping quotes (`localhooks.go:133-147`, `:152-154`). Engine path is the leading token
  (`localhooks.go:170-172`), so a moved path ⇒ miss ⇒ sibling entry appended
  (`localhooks.go:100-104`). Both engines then fire.
- Codex already solved exactly this, and its doc comment gives the same rationale
  (`adapters/codex/installer.go:262-268`): strip the leading engine token, then require the
  remainder to be *exactly* an owned invocation — a compound command that merely embeds one is
  foreign and kept. `stripEngineToken` (`:287-302`) handles quoted and bare tokens.
- CC's owned vocabulary is two shapes, not one: `hook claude-code <Event>` (11 events,
  `localhooks.go:79-80`, `localHookEvents` `:34-51`) and `rewake claude-code` (the second
  PreToolUse handler, `:90-98`). Codex's `ownedInvocation` regex (`installer.go:35`) would
  reject `rewake`, so the vocabulary is ported, not the regex.
- `writeLocalHooks`'s signature must not change: the three existing call sites in
  `localhooks_quote_test.go:18,65,128` are what keep `TestReInitAddsTheNewHooksExactlyOnce`
  "passing unchanged". The notice therefore goes to `os.Stderr` from inside the function, and
  the *decision* is factored into a pure function that the new test asserts directly — the
  same split `devconfig.warnDeprecatedKeys` / `deadKeysPresent` uses
  (`devconfig.go:466-497`, asserted at `devconfig/posture_test.go:242,258`).
- Quote-insensitivity must survive: `TestLocalHooksIdempotentAgainstAnUnquotedLegacyEntry`
  (`localhooks_quote_test.go:52`) seeds an **unquoted** command at the **same** engine path and
  expects exactly one handler. So engine-token comparison compares unquoted forms.

## Requirements

1. A handler is OpenBox-owned iff: `type == "command"`, and after stripping one leading engine
   token the remainder is exactly `hook claude-code <the event key being merged>` or exactly
   `rewake claude-code`.
2. Owned + engine token ≠ engine being installed ⇒ drop it. Entries left with zero handlers are
   removed.
3. Owned + same engine (quote-insensitive) ⇒ keep; do not append a duplicate.
4. Not owned ⇒ preserved untouched, including compound commands that embed our invocation.
5. When anything was dropped, print to stderr: how many, which engine path(s), which events,
   the settings file, and that the old engine will no longer run.
6. `TestReInitAddsTheNewHooksExactlyOnce`, `TestLocalHooksIdempotentAgainstAnUnquotedLegacyEntry`,
   `TestLocalHookCommandQuotesTheEnginePath` pass **with the file byte-identical** (new tests go
   in a new file).
7. The file's own doc comment (`localhooks.go:25-27`) stops claiming append-only-on-exact-match.

## Architecture

Data flow, unchanged shape: `Installer.Install` (`adapters/claude-code/installer.go:120-126`)
→ `writeLocalHooks(projectDir, engine)` → read+unmarshal `settings.local.json`
(`localhooks.go:64-73`) → per-event merge (`:79-105`) → `MarshalIndent` + write (`:111-117`).

Only the per-event merge changes, from *test-then-append* to *sweep-then-append*:

```
for ev := range localHookEvents:
    entries := hooks[ev.Event]
    entries, dropped := sweepStale(entries, ev.Event, engine)   // new, pure
    record dropped
    if hasLocalHookCommand(entries, command) { hooks[ev.Event] = entries; continue }
    entries = append(entries, freshEntry(ev, engine))            // unchanged
    hooks[ev.Event] = entries
after the loop: if len(dropped) > 0 → fmt.Fprintf(os.Stderr, …)
```

New unexported helpers in `localhooks.go` (names describe behavior, no plan/phase labels):

- `ownedLocalHook(command, event string) (engine string, ok bool)` — pure classifier; returns
  the unquoted engine token. Ports `isOpenBoxHandler` + `stripEngineToken`.
- `stripEngineToken(command string) (rest string, ok bool)` — port of
  `adapters/codex/installer.go:287-302` (quoted token, bare token, unterminated quote ⇒ false).
- `sweepStale(entries []any, event, engine string) (kept []any, dropped []string)` — walks
  `entry["hooks"]`, keeps foreign + same-engine owned, drops other-engine owned, drops entries
  whose `hooks` becomes empty. Mirrors `Installer.mergeEvent`'s keep/drop discipline
  (`adapters/codex/installer.go:228-242`) but on `map[string]any`.

`hasLocalHookCommand` (`:133-147`) stays as-is and keeps its role (step 3 above).
No signature, SPI, module or `go.mod` change. Codex is not touched (D3).

## Related code files

| Path | Lines | Role |
|---|---|---|
| `adapters/claude-code/localhooks.go` | 25-27 | stale doc comment — rewrite |
| | 34-51 | `localHookEvents`, 11 events + matchers/timeouts |
| | 56-119 | `writeLocalHooks` — merge loop at 79-105 is what changes |
| | 133-147 | `hasLocalHookCommand` — retained for the append decision |
| | 149-154 | `unquoteHookCommand` — reused by the engine-token compare |
| | 170-172 | `localHookCommand` — quoting contract, unchanged |
| `adapters/claude-code/installer.go` | 123 | only production caller |
| `adapters/codex/installer.go` | 220-260, 262-302, 35 | pattern source (read-only) |
| `adapters/claude-code/localhooks_quote_test.go` | 15, 52, 92 | three tests that must stay byte-identical |
| `adapters/common/devconfig/devconfig.go` | 466-497 | stderr-notice + pure-decision precedent |

New file: `adapters/claude-code/localhooks_stale_engine_test.go`.

## Implementation Steps

1. Port `stripEngineToken` into `localhooks.go` verbatim in behavior, with a comment naming
   `adapters/codex/installer.go:287` as the source and D3 as the reason there are two copies.
2. Add `ownedLocalHook(command, event)`: unmarshal-free (handlers are already `map[string]any`
   here — take the command string and the handler's `type` from the caller), `strings.TrimSpace`,
   strip token, compare remainder against the two owned literals. Return the token
   `unquoteHookCommand`-normalized.
3. Add `sweepStale`. Preserve every other key on the entry map (`matcher`, unknown keys) by
   mutating `entry["hooks"]` in place rather than rebuilding the entry.
4. Rewire the merge loop (`:79-105`) to sweep first, accumulate `dropped` across events as
   `engine path → []event`.
5. Print the notice after the loop, before the write, to `os.Stderr`. One line per distinct
   stale engine path; name `settingsPath` and the installed `engine`.
6. Rewrite the doc comment at `:25-27`: the merge preserves foreign entries and *replaces* our
   own at any other engine path; state the space-in-unquoted-path limit (R2).
7. New test file with:
   - `TestReInitReplacesAnOpenBoxEntryAtAStaleEnginePath` — seed every event at engine A
     (quoted, plus PreToolUse's rewake handler) + a foreign `PostToolUse` handler; run
     `writeLocalHooks(dir, B)`; assert per event exactly one owned handler, its engine == B,
     zero occurrences of A, foreign handler present, no empty `hooks` arrays.
   - `TestOwnedLocalHookRecognizesOurArgvShapeOnly` — table: quoted ours, unquoted ours,
     `rewake claude-code`, wrong event name, `my-own-linter`, `my-linter && "/p/openbox" hook
     claude-code Stop` (foreign), unterminated quote, empty string.
8. Run the module's tests; `git diff --stat adapters/claude-code/localhooks_quote_test.go` must
   be empty.

## Todo list

- [x] `stripEngineToken` ported with source citation
- [x] `ownedLocalHook` classifier
- [x] `sweepStale` with empty-entry pruning
- [x] merge loop rewired, `dropped` accumulated
- [x] stderr notice (count, stale path(s), events, settings path, new engine)
- [x] doc comment `:25-27` corrected
- [x] `localhooks_stale_engine_test.go` (2 tests)
- [x] `go test -race ./...` in `adapters/claude-code`
- [x] both cross-compiles for that module
- [x] `git diff` proves `localhooks_quote_test.go` untouched

## Success Criteria

- Seeded stale-path install + one re-init ⇒ exactly one owned handler per event, all at the new
  engine; zero occurrences of the old path in `settings.local.json`.
- Foreign `my-own-linter` still present; compound command embedding our invocation still present.
- stderr names the replaced engine path and the affected events; silent when nothing was stale.
- Three pre-existing tests green with the file unmodified (verified by `git diff`).
- `adapters/claude-code`: `go test -race ./...` green; both cross-compiles green.

## Risk Assessment

| Risk | L×I | Mitigation |
|---|---|---|
| R1 Classifier too broad ⇒ deletes a developer's hook | Low×High | Exact-remainder match after one-token strip (never `Contains`), ported from a shipping implementation; table test includes the compound-command case; `TestReInitAddsTheNewHooksExactlyOnce`'s `my-own-linter` assertion is the standing tripwire. Direction of error is fixed: over-keep, never over-delete. |
| R2 Unquoted engine path containing a space is unrecognized (token split at the first space) ⇒ stale entry survives | Med×Low | Accepted, documented in the doc comment. Only a pre-quoting build could write it, and those hooks never started at all (`localhooks.go:156-169`), so the leftover is dead weight, not a second engine. Phase 03 surfaces it. |
| R3 An owned handler misfiled under the wrong event key is classified foreign and kept | Low×Low | Deliberate: per-event vocabulary keeps the safe direction. Phase 03's distinct-engine count still flags it when the path differs. |
| R4 Pre-existing gap: a same-path entry missing PreToolUse's `rewake` handler is not repaired (entry-level append decision, `localhooks.go:82`) | Low×Med | Out of scope — unchanged behavior, not a regression. Recorded here so a future reader does not read it as this phase's bug. |
| R5 (assumption) Claude Code ignores an entry whose `hooks` array we removed / a rewritten file | Low×High | **Signal it broke:** after a re-init in the testbed project, a real session emits no events, or Claude Code logs a settings parse error. **Pre-decided response:** stop-and-replan — do not ship a merge that can produce an unreadable settings file; fall back to leaving empty entries in place (harmless litter) rather than pruning. |
| R6 (assumption) The two argv shapes are the complete owned vocabulary | Low×Med | Verified against `localHookEvents` (`:34-51`) + the rewake handler (`:90-98`) — no third shape exists in the file. **Signal:** `TestLocalHooksMirrorPluginBundle` (`hookwiring_test.go:68`) fails, or the plugin bundle grows a differently-shaped command. **Pre-decided response:** adjust in-plan — extend the owned literal list in `ownedLocalHook`. |

## Security Considerations

- Replacing a governing binary's registration silently is itself the failure class this repo
  warns about — the stderr notice is the control (D1), and it must name the old path so the
  developer can see which build was running.
- The classifier only ever *removes* commands it can prove are ours; it never rewrites a foreign
  command, and it never executes anything.
- No secret touches this path: the command line carries the engine path and event name only
  (INV-1, same property as `adapters/codex/installer.go:304-308`).
- `settings.local.json` is per-developer and git-ignored by convention
  (`cli/cmd/openbox/scope.go:140-141`); this phase writes nothing new into it beyond the hooks
  block it already owns.

## Outcome

Shipped as planned. Two signature deviations, both to avoid splitting the
definition of "ours" across call sites — the divergence that caused the bug:

- `ownedLocalHook(hookType, command, event)` takes the handler `type` rather than
  leaving that check at the caller, because R1 makes `type == "command"` part of
  the ownership definition and Phase 03 reuses the classifier.
- `splitEngineToken(cmd) (engine, rest string, ok bool)` returns the token as well
  as the remainder. The path comparison needs it and so does the notice; the
  phase's `stripEngineToken(cmd) (rest, ok)` would have forced a second parse.

R5 (Claude Code rejecting a rewritten file) and R6 (a third owned argv shape)
did not fire and stay unproven without a live session. No risk required a
stop-and-replan.

**Two corrections from the post-implementation review:**

- **The sweep now de-duplicates, not only replaces.** R3 as written ("owned +
  same engine ⇒ keep") left our own invocation registered TWICE at one path
  untouched — which double-counts exactly as a second engine does, and which
  Phase 03 warns about while naming this command as the remedy. Reproduced
  against the real binary: doctor warned, `init` changed nothing, doctor warned
  again, forever. `sweepStale` now keeps the FIRST handler of each owned
  invocation at the installed engine and drops later copies, keyed by invocation
  (not by event) so PreToolUse's gate and watcher cannot collapse into each
  other. It removes only a command byte-equivalent-modulo-quoting to one it
  keeps, so the over-keep direction is unchanged for anything foreign. Reported
  through a second, separately-worded stderr notice: "which build was also
  running" and "your file had our entry twice" are different facts.
- **Both notices moved to AFTER the write.** Step 5 put them before it, and the
  message ends "The old engine no longer runs for this project" — a claim about
  the file on disk, stated ahead of a write that can still fail.

Evidence: [reports/verification-260814-stale-hook-and-posture.md](reports/verification-260814-stale-hook-and-posture.md)

## Next steps

Phase 03 exports a read-only audit built on this classifier so `openbox doctor` and the
installer cannot disagree about what "ours" means. Phase 04 updates
`docs/getting-started.md:148`, `README.md:93`, and adds the dormant testbed assertion.
