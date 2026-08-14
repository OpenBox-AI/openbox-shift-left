# Phase 03 — `openbox doctor` warns on more than one registered OpenBox engine

## Context links

- Plan: [plan.md](plan.md)
- Issue "Also worth considering": `plans/reports/issue-260814-0106-stale-hook-registration-and-posture-gap.md:80-84`
- Research Q5: `research/researcher-01-hook-registration.md:100-121`
- Depends on: [phase-01](phase-01-stale-engine-hook-merge.md) (the classifier)

## Overview

- **Date:** 2026-08-14
- **Description:** `openbox doctor` reads the current directory's `.claude/settings.local.json`,
  classifies hook handlers with Phase 01's owned-shape parser, and warns when more than one
  OpenBox registration is live (a second engine path, or the same path registered twice).
- **Priority:** P2 (the failure mode is invisible without it; cuttable — this is the issue's
  optional addendum)
- **Implementation status:** complete
- **Review status:** reviewed

## Key Insights

- Doctor cannot see this today: no reference to `settings.local.json`, `writeLocalHooks`,
  `localHookEvents` anywhere in `cli/cmd/openbox/doctor.go` (full file, 171 lines). Everything it
  reports about hooks is the **global** managed-settings state
  (`doctor.go:110-114` → `managed.ProviderState`).
- Doctor and the installer must not hold two opinions about what "ours" means — the exact-string
  vs shape divergence is the bug in Phase 01. So the classifier gets exported from the adapter
  and doctor calls it; doctor does not re-derive a parse.
- Import direction is already legal: `cli/go.mod` requires
  `adapters/claude-code` ("the CLI registers the real Claude Code installer"), and `doctor.go`
  already imports two sibling modules (`hookflow`, `devconfig`, `doctor.go:1-12`). No `go.mod`
  or `go.work` change.
- Project dir = CWD is the right default and agrees with `init`: local scope defaults
  `ProjectDir` to `os.Getwd()` (`cli/cmd/openbox/main.go:459-465`), and
  `printGovernedScope` names `<ProjectDir>/.claude/settings.local.json`
  (`cli/cmd/openbox/scope.go:133-136`).
- Doctor has no pass/warn helper — every section is raw `Fprintf` with a literal `WARNING:`
  prefix (`doctor.go:74,85,90`). Imitate the "Managed OpenBox config" three-way
  present/unreadable/active block (`doctor.go:68-94`); do not introduce an abstraction for one
  new section.
- An absent settings file is the **normal** state (global-scope install, or a directory that was
  never initialized) — it must read as a fact, not a fault, exactly like
  `withPresence`/`lastDecisionSummary` treat absent files (`doctor.go:141-160,165-171`).

## Requirements

1. New exported read-only API in `adapters/claude-code` returning, for a project dir: the
   settings path, whether it exists/parsed, the distinct engine paths classified as OpenBox-owned,
   and the events carrying more than one owned handler. No writes.
2. `doctor` prints a section naming the settings path and the engine(s) found.
3. `WARNING:` when >1 distinct engine path, or when any event has >1 owned handler — with the
   consequence (every hook fires once per engine; stored events double) and the remedy (re-run
   `openbox init` in this directory, which now replaces stale entries).
4. Absent file, unreadable file, or invalid JSON each read as a stated condition, never a crash;
   doctor's exit code is unchanged in all cases.
5. Zero owned handlers + file present ⇒ say the project is not governed by hooks here.

## Architecture

```
runDoctor (doctor.go:24)
  … Identity / Enforcement / Managed OpenBox config / Policy decisions
  … Provider managed configuration (110-114)          ← GLOBAL state
  + Project hook registration                          ← NEW, LOCAL state
        os.Getwd()  →  claudecode.AuditLocalHooks(dir)  →  print / WARNING
  … What this does and does not prove (116)
```

New in `adapters/claude-code/localhooks.go` (same file as the classifier — single owner, which is
why this phase is sequential after Phase 01):

```go
type LocalHookAudit struct {
    SettingsPath string
    Present      bool
    Engines      []string  // distinct owned engine paths, sorted
    DuplicateEvents []string // events with >1 owned handler
}
func AuditLocalHooks(projectDir string) (LocalHookAudit, error)
```

It reuses `ownedLocalHook` — the same function `sweepStale` uses — so the two can never disagree.
Invalid JSON returns an error; doctor renders it as a condition line.

Placement in doctor: immediately after "Provider managed configuration" (`doctor.go:110-114`),
because that is the only other place the CLI reports per-provider hook/config state, and before
"What this does and does not prove" (`:116`) so the caveat paragraph still lands last.

## Related code files

| Path | Lines | Role |
|---|---|---|
| `adapters/claude-code/localhooks.go` | Phase 01's `ownedLocalHook` | classifier being reused |
| | 34-51 | `localHookEvents` — the event keys to walk |
| | 64 | settings path construction to mirror exactly |
| `cli/cmd/openbox/doctor.go` | 1-12 | imports (add `claudecode`) |
| | 68-94 | present/unreadable/active section to imitate |
| | 110-114 | insertion point (after this block) |
| | 116-120 | must stay last |
| | 123-128 | `orUnset` helper available |
| `cli/cmd/openbox/main.go` | 459-465 | `os.Getwd()` = init's project dir default |
| `cli/cmd/openbox/scope.go` | 133-141 | how the settings path is described to users today |
| `cli/go.mod` | 8-10 | `adapters/claude-code` already required |

New test: `cli/cmd/openbox/doctor_test.go` (or the existing doctor test file if one exists —
check before creating) driving `runDoctor` in a temp dir with a seeded two-engine settings file.

## Implementation Steps

1. Add `LocalHookAudit` + `AuditLocalHooks` to `localhooks.go`, directly beneath the classifier,
   with a doc comment stating it exists so doctor and the installer share one definition of "ours".
2. Walk `hooks[ev.Event]` for every event in `localHookEvents`; for each handler with
   `type == "command"`, classify with `ownedLocalHook`; collect distinct engines (sorted) and
   events with a count > 1.
3. In `doctor.go`, resolve CWD (on `os.Getwd()` error, print the condition and continue — doctor
   must not fail because of a missing optional check), call the audit, print:
   - path + `(present)`/`(absent)` using the existing `withPresence` idiom;
   - one line per engine found;
   - `WARNING:` + consequence + remedy when the duplicate condition holds;
   - a plain line when exactly one engine is registered, and a distinct line for zero.
4. Test cases: two distinct engines ⇒ WARNING text + both paths; one engine ⇒ no WARNING;
   file absent ⇒ absent line, no WARNING, exit unchanged; invalid JSON ⇒ condition line, no crash.
5. `go test -race ./...` in `cli` and `adapters/claude-code`; both cross-compiles for both.

## Todo list

- [x] `LocalHookAudit` + `AuditLocalHooks` exported, reusing `ownedLocalHook`
- [x] doctor section inserted after "Provider managed configuration"
- [x] WARNING covers both >1 engine and >1 handler per event
- [x] absent / unreadable / invalid-JSON paths render as conditions, exit code unchanged
- [x] doctor tests: two engines, one engine, absent, invalid JSON
- [x] `go test -race ./...` green in `cli` + `adapters/claude-code`
- [x] both cross-compiles green

## Success Criteria

- A settings file holding OpenBox entries at two engine paths ⇒ `openbox doctor` prints
  `WARNING:` naming both paths, states events are duplicated, and points at `openbox init`.
- The same file after one `openbox init` (Phase 01) ⇒ one engine, no WARNING. This pairing is the
  phase's real acceptance test: the check and the fix agree.
- Doctor's exit code and every pre-existing section's output are unchanged (existing doctor tests
  green).
- The audit and `sweepStale` share one classifier — verifiable by grep: exactly one
  `ownedLocalHook` definition.

## Risk Assessment

| Risk | L×I | Mitigation |
|---|---|---|
| R1 CWD is not the governed project (doctor run from a subdirectory) ⇒ reports "absent" and a developer reads it as ungoverned | Med×Med | Print the resolved path on every run so the reader sees which directory was inspected; word the absent line as "no project hook config in this directory" rather than "not governed". No directory walk-up: `init` does not do one either (`main.go:459-465`), and inventing one here would make doctor and init disagree about what the project is. |
| R2 Doctor gains a new failure mode (unreadable/oversized/malformed settings) that breaks a working command | Low×Med | Every error path prints a condition and continues; no `return`, no exit-code change. Test covers invalid JSON. |
| R3 Warning fires on a legitimately foreign hook that merely embeds our invocation | Low×Med | Same exact-remainder classifier as Phase 01 (compound commands are foreign); the classifier's table test covers it. |
| R4 Exporting from the adapter widens its public surface | Low×Low | Two symbols, read-only, no writes, no new module. Alternative (doctor re-parsing) reintroduces exactly the two-opinions bug this repo just paid for. |
| R5 (assumption) `cli` may import `adapters/claude-code` from `cmd/openbox` | Low×Med | Verified: `cli/go.mod:8-10` requires it with a replace directive; `doctor.go` already imports sibling modules. **Signal it broke:** build failure or an import cycle at compile time. **Pre-decided response:** adjust in-plan — move the audit call into `cli/internal/` (which already imports the adapter via `internal/providers`) and have doctor call that. |
| R6 Scope creep: doctor growing a hook-registration report for Codex too | Med×Low | Deliberately out of scope — Codex's installer already replaces by shape (`adapters/codex/installer.go:220-260`), so it has no equivalent defect to surface. Recorded so it is a decision, not an omission. |

## Security Considerations

- Read-only: the audit opens one file and never writes. Doctor stays safe to run on any machine.
- Printing engine paths reveals filesystem layout only — no credential, no policy content. Hook
  commands carry the engine path and event name only (INV-1).
- This check is diagnostic, not assurance: it proves what is registered in one file, not that
  hooks ran. Wording must not imply otherwise — doctor's closing "What this does and does not
  prove" paragraph (`doctor.go:116-120`) is the standing framing, keep it last.

## Outcome

**R5 fired.** `cli/cmd/openbox` may NOT import the adapter: a standing
architectural test, `TestOnlyTheRegistryImportsAdapters`
(`cli/internal/providers/layering_test.go:17-39`), pins the composition root as
the only importer. The direct import this phase sketched would have failed CI.
Applied the pre-decided response: `providers.AuditProjectHooks` wraps
`claudecode.AuditLocalHooks` and re-declares the result shape, so doctor stays
provider-neutral. Codex is deliberately absent from that wrapper — its installer
has always replaced by argv shape, so it has no equivalent state to report.

Also adjusted: the audit counts per **invocation**, not per handler. PreToolUse
legitimately carries two of ours (the gate and the rewake watcher), so the
phase's literal ">1 owned handler per event" rule would have warned on every
healthy install — the exact way a warning gets trained into background noise.

**The post-implementation review found the pairing half-broken.** This phase's
stated acceptance test is "the check and the fix agree", and
`TestTheAuditAgreesWithWhatReInitRepairs` only ever exercised the two-ENGINE
case. For the other condition this check reports — one invocation registered
twice at ONE engine — `init` was a no-op, so doctor named a remedy that never
cleared its own warning (reproduced against the real binary). Fixed in Phase 01's
`sweepStale` rather than by softening the warning: the condition is real
double-counting, so the honest repair is to repair it.
`TestReInitCollapsesADuplicateRegistrationAtTheSameEngine` now holds that half of
the pairing, and `testbed/10-onboard.sh` carries the dormant end-to-end version.

Evidence: [reports/verification-260814-stale-hook-and-posture.md](reports/verification-260814-stale-hook-and-posture.md)

## Next steps

Phase 04 documents the check in the docs surface that describes `openbox doctor`, and adds the
dormant testbed assertion that pairs the check with the fix.
