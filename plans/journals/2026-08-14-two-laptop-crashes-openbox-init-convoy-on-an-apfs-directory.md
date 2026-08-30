---
title: "Two laptop crashes: openbox init convoy on an APFS directory lock"
date: 2026-08-14
summary: "Test hermeticity leak drove concurrent openbox init runs into an APFS lock convoy; 5,838 hung processes crashed the machine twice. Guards, atomic write, copy-skip and an install lock landed."
---

# Two laptop crashes: openbox init convoy on an APFS directory lock

## What happened

Two crashes, same root cause, different failure modes.

**Incident 1 (02:17–02:43).** ~400 concurrent `openbox init` runs left 397 `.openbox-*.tmp` files (1.8 GB) in `~/.claude/plugins/openbox-observe/bin/` and spliced `.claude/settings.local.json` into invalid JSON (complete document + 1031-byte tail).

**Incident 2 (overnight, reboot 09:58:52).** The logd watchdog spindump caught 5,838 live `openbox` processes, 0.008s CPU each, all blocked in `apfs` on `krwlock` — "blocked by krwlock for writing owned by openbox [PID]". A directory-lock convoy: hundreds of concurrent create + write 7.8 MB + rename in one directory, arrivals outpacing drains. Ages spread over six hours; 57 `go` and 31 Claude Code processes hung alongside. Five `logd userspace_watchdog_timeout` events, then the machine died.

## Root causes

1. **Test hermeticity.** `init` defaults to project scope from cwd (`main.go:459-464`). Tests pinned `OPENBOX_HOME` but not `HOME` or cwd, so runs wrote hook registrations into the source tree and ~10 MB engine copies into the real plugin dir. Three ambient sinks (`enforcements.jsonl`, `advisories.jsonl`, `pending-approvals/`) resolve from `os.UserConfigDir()`, not `OPENBOX_HOME` — a deliberate split (`devconfig/paths.go:20-22`) — so 2,940 fixture records reached the real audit trail.
2. **Non-atomic write.** `writeLocalHooks` used `os.WriteFile` (truncate-then-write); concurrent writers spliced the file. Every other durable write in the repo already committed through a rename.
3. **Unconditional copy + no sweep.** `placeEngineBinary` rewrote the whole engine every run; `defer os.Remove(tmp)` cannot survive SIGKILL, and nothing reclaimed residue.
4. **No concurrency bound** on `init`.

## Corrections to earlier conclusions

- The 03:23 incident report inferred "no Claude Code session was active" from transcript silence. Wrong — Claude Code's own PreToolUse hook is `openbox`; once the convoy forms, hooks block, sessions freeze and write nothing. Silence was the symptom.
- Day-1 fixes were reported as effective but never deployed. The installed engine had no `sweepStaleEngineTemps` symbol (7,882,530 vs 9,655,106 bytes). Incident 2 ran entirely on unfixed code.
- The guard `adapters/codex/testmain_test.go` already existed from a prior incident and was never ported to `adapters/claude-code`.

## Changes

- Hermeticity `TestMain` guards (contain + assert) in `adapters/claude-code`, `cli/cmd/openbox`, `adapters/claude-code/cmd/openbox-cc-hook`; the assert found a fourth sink (session registry) that containment alone would have hidden.
- Sink pinning in `isolateConfig` / `isolateHome` / `setHookEnv` / child envs, pinned-only-when-unset so call order cannot matter.
- `writeFileAtomic` in `localhooks.go`.
- Copy-skip when byte-identical + age-gated `sweepStaleEngineTemps`.
- `acquireInstallLock` — refuses rather than queues; measured 1 proceeded / 23 refused across three runs.

## Outcomes

247 tmp files → 0, 760 MB → 9.2 MB (the sweep did it on first real run). 808 fixture records purged, 2,154 genuine kept. All 11 modules green under `-race`; both cross-compiles; a full suite run leaves the real sinks byte-identical.

## Next steps

- Correct the 03:23 incident report's "no session active" inference.
- Investigate: after re-init, a live session is hard-denied with "Session is no longer active" on every tool call — contradicts the documented inert/fail-open guarantee.
- Stale `init --dry-run` banner still claims it registers an agent and writes credentials (both moved to `auth` in).
- Driver of the ~400 init runs still unidentified across both incidents.

> Historical work record — not durable authority. Prefer docs/specs for current decisions.
