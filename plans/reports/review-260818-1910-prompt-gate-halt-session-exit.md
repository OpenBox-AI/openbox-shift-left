# Review: prompt gate + HALT session exit (pending changes, max effort, --yagni)

**Date:** 2026-08-18 · **Scope:** uncommitted working tree (26 modified + 4 new files, +666/−69)
**Pipeline:** 10 finder angles → dedup → 3-state verify → gap sweep → fixes applied → re-verified
**Plan:** [260818-1714-prompt-gate-halt-session-exit](../260818-1714-prompt-gate-halt-session-exit/plan.md)

## Verdict

**PASS — ship-ready pending the testbed run.** Spec compliance PASS, YAGNI audit found no
cuts, 7 findings (1 minor correctness, 6 cleanup) — all 7 fixed in-tree and re-verified.

## Stage 1 — spec compliance (PASS)

- Prompt gate: UserPromptSubmit → inline `/evaluate` pre-processing; HALT/BLOCK blocks +
  erases (C28–C30 assert stdout bytes).
- HALT session exit: `continue:false` stops the turn; per-session latch refuses every later
  prompt/tool locally, resume included, zero re-evaluation (C27). Protocol-max "exit"
  accepted by owner (no process-exit exists in the hook contract).
- All 7 validated decisions hold, each pinned by a test. Observe mode byte-identical.

## YAGNI audit (no cuts)

ADR-0020/docs: required by repo truth rules + semantics-ADR precedent. Testbed §A3 +
`OPENBOX_HALT_DIR`: verification infra the repo's evidence discipline demands. Latch `TS`:
kept on evidence (plan 260814-2235 phase-03 exists because enforcement records lacking ts
hurt the Aug-14 diagnosis). Deliberately not built (validated de-scopes): doctor listing,
unlatch cmd, Codex latch, hard-kill. New exports all consumed.

## Findings (all fixed)

| # | Sev | Finding | Fix |
|---|---|---|---|
| 1 | correctness (CONFIRMED) | latched-path diagnostic logged hook name, not tool name | `ReplaySessionHalt` takes `toolName`; caller passes `ev.ToolName`/"prompt" |
| 2 | altitude (PLAUSIBLE) | provider-agnostic replay helper lived in the adapter | moved to `hookflow.ReplaySessionHalt` (takes `SessionHaltInfo`, drops the double latch read) |
| 3 | simplification (CONFIRMED) | two `EnforceGate` constructions | one construction; hook picks contract/target/record |
| 4 | simplification (CONFIRMED) | duplicated latched-branch dispatch | single dispatch, one replay call |
| 5 | reuse (CONFIRMED) | 3rd copy of config-dir resolution | shared `openboxConfigDir()` (consts_paths.go); enforce/advisory/halt paths all call it |
| 6 | reuse (CONFIRMED) | `haltLogger` duplicated `nopLogger` | reuse `nopLogger`, helper deleted |
| 7 | stale-comment (CONFIRMED) | `applied_decision` enum comment omitted halt/block | comment updated |

Refuted (not applied): 3 efficiency claims (`devconfig.Pin()` freezes config per hook run;
`ReadFile`-on-ENOENT is already minimal — Stat-first would add a syscall); 1 comment-clarity
claim (comment already exists). Angle B returned 8 inverted items (described protections the
diff adds) — not findings. Gap sweep: empty.

## Verification evidence (fresh, post-fix)

- `go test -race ./...`: ok — hookflow, claude-code, codex, cli (all suites, 0 failures).
- Cross-compiles ok post-fix: windows/amd64 + linux/arm64 on all four touched modules.
- gofmt clean.

## Unresolved questions

1. Testbed §A3 (raw-rego halt → turn stop + latch end-to-end) still awaits a live stack —
   the one verification gap; also settles whether core mints pollable approval rows for
   prompt REQUIRE_APPROVAL verdicts (client-side failure modes all land on block).
2. `statusMessage` on UserPromptSubmit registrations is assumed ignored-if-unknown on older
   Claude Code (matches the probe's unknown-key behavior for hooks); not re-probed.
