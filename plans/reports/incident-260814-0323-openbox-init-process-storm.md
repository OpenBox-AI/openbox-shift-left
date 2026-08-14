# Incident: concurrent `openbox init` runs convoyed on an APFS directory lock and crashed the machine twice

**Date:** 2026-08-14 · **Repo:** openbox-shift-left @ `98c62d2` (+ uncommitted work) · **Reported by:** developer
**Revised:** 2026-08-14 12:20 after a second, worse recurrence. The first version of this report is
corrected below rather than replaced — what it got wrong, and why, is the most useful thing in it.
**Status:** root cause understood and fixed; fixes now DEPLOYED (they were not, first time round);
the driver that invoked `init` hundreds of times is still unidentified.

## Corrections to the 03:23 version

| Claim | Verdict | What is true |
|---|---|---|
| "no transcript written in any project … the entire storm window" ⇒ Claude Code sessions ruled out | **Wrong inference** | Claude Code's own PreToolUse hook IS `openbox`. Once the convoy forms, hooks block, sessions stop making tool calls and write nothing. The 09:53 spindump shows **31 Claude Code and 57 `go` processes hung** alongside. Silence was the symptom, not evidence of absence. |
| "Status: contained" | **Wrong** | Recurred ~7 h later, far worse, and took the machine down. |
| Three fixes "make a recurrence harmless" | **Inert** | They were written to source and never deployed. The installed engine had no `sweepStaleEngineTemps` symbol (7,882,530 B vs 9,655,106 B fresh). **Incident 2 ran entirely on unfixed code.** A source fix to an installed engine does nothing until `init` re-runs — the report should have said so and did not. |
| "full-suite run leaves **zero** residue … (checked before/after)" | **Unsound method** | The check used `ls -1`, which does not list dotfiles, and the residue is `.openbox-*.tmp`. It could not have detected what it claimed to rule out. Re-verified since with a dotfile-aware count. |
| "a lockfile … a bigger contract than the corruption warrants" | **Reversed** | New evidence (the convoy) overturns it. A lock is implemented; see Fix 5. |
| "Remediation applied" | **Did not hold** | `settings.local.json` was re-corrupted at 09:57 with an identical signature; residue returned (247 files, 760 MB). |
| Q4: "the rest of the tree was not swept" for tests at real default paths | **Now swept** | `adapters/claude-code` (1 of 25 test files isolated `OPENBOX_HOME`), `cmd/openbox-cc-hook` (5 child-spawn sites, none contained), plus three ambient sinks and a fourth found by the new guard. |

The first version also never explained **why the machine died** — it attributed harm to disk fill.
The actual killer is below.

## Symptom

**Incident 1 (02:17–02:43).** 100+ concurrent `openbox` processes from
`~/.claude/plugins/openbox-observe/bin/openbox`. Stopped manually:

```
02:38:51  rm ~/.local/bin/openbox
02:41:05  pkill -TERM -x openbox        # spawn rate INCREASED after this
02:42:01  rm ~/.claude/plugins/openbox-observe/bin/openbox
02:42:24  pkill -KILL -x openbox        # stopped
```

Left behind: **397 `.openbox-<rand>.tmp` files, 1.8 GB**, in the directory Claude Code executes the
engine out of. Repo-root `.claude/settings.local.json` corrupted.

**Incident 2 (overnight → reboot 09:58:52).** Machine crashed unattended. 247 tmp files (760 MB),
settings corrupted again, five `logd userspace_watchdog_timeout` events 09:25→09:53.

## Root cause — the crash mechanism

The logd watchdog spindump
(`/Library/Logs/DiagnosticReports/logd_2026-08-14-095359_*.userspace_watchdog_timeout.spin`)
caught **5,838 live `openbox` processes**, ~0.008 s CPU each, all blocked in the same place:

```
libsystem_kernel → kernel → apfs → krwlock
  (blocked by krwlock for writing owned by openbox [69832] thread 0x9609e)
```

An **APFS directory-lock convoy.** Concurrent `create temp + write ~8–10 MB + rename` in ONE
directory serialize on a kernel rwlock for that directory. Past a few dozen writers the queue
drains slower than it fills; every arrival makes it worse; the processes never exit. Ages from
`Time Since Fork`:

| Age at 09:53 | 6 h | 5 h | 4 h | 3 h | 2 h | 1 h | <1 h |
|---|---|---|---|---|---|---|---|
| processes | 1212 | 1880 | 1061 | 469 | 456 | 565 | 195 |

Six hours of monotonic accumulation, then resource exhaustion. Two amplifiers made it unbounded:
Claude Code times out a blocked hook at 30 s and proceeds **without killing it**, so every
subsequent gated tool call adds another permanently-stuck process; and the realtime flusher is
spawned detached (`Setsid` + `Process.Release()`), so nothing ever reaps it either.

This also explains incident 1's odd signature — the rate *rose* after `pkill -TERM`. Killing lock
holders drained the convoy, so queued work completed in milliseconds instead of seconds. It reads
like a tight respawn loop and is not one.

## Root cause — four defects on the `openbox init` path

### 1. Tests wrote into the developer's real machine

`init` defaults to PROJECT scope from process cwd (`cli/cmd/openbox/main.go:459-464`, ADR-0016).
Helpers pinned `OPENBOX_HOME`/`OPENBOX_CONFIG` but **not `HOME` and not cwd**:

| Leak | Destination | Evidence |
|---|---|---|
| cwd not isolated | hook registrations into the source tree | `cli/cmd/openbox/.claude/settings.local.json` on disk |
| `HOME` not isolated | ~10 MB engine copy into the REAL plugin bundle | that residue registered the real `~/.claude/plugins/…/bin/openbox`, not a temp path |

Three further sinks — `enforcements.jsonl`, `advisories.jsonl`, `pending-approvals/` — resolve from
`os.UserConfigDir()` and **not** from `OPENBOX_HOME`. That split is deliberate
(`adapters/common/devconfig/paths.go:20-22`), so pinning `OPENBOX_HOME` alone left the real audit
trail accumulating fixture records: **808 of 2,962** at cleanup, from session ids `s1`, `s`,
`sess-xyz`, `rt`, `sess-1`.

`TestClaudeCodeInstallsForRealExitsZero` bypassed the helper entirely; its doc comment claimed
"nothing lands in the developer's real home" — true for HOME, false for the project dir. Its own
temp path (`…/TestClaudeCodeInstallsForRealExitsZero1599344392/001/…`) was found inside the
repo-root `.claude/settings.local.json`.

**An equivalent guard already existed in `adapters/codex/testmain_test.go`**, written after an
earlier incident in which "stub-era tests once drove a real installer at DEFAULT paths and wrote the
developer's actual home". It was never ported to `adapters/claude-code`. The same class of escape
then recurred there at far larger scale.

### 2. `writeLocalHooks` was the only non-atomic durable write in the repo

`os.WriteFile` = truncate-then-write, two steps a concurrent writer interleaves with. Both runs
truncate to zero, then write at their own offsets → the shorter document lands inside the longer
one. Result: a complete JSON document followed by 1031 bytes of another. **Invalid JSON**, which
makes Claude Code drop every hook in that file, and blocks `openbox init` outright (the merge parses
before writing — observed at 11:00, which is how the redeploy first failed).

Every other durable write already commits through a rename — `placeEngineBinary`,
`cli/internal/managed/managed.go:247`, `adapters/common/devconfig/envfile.go:195`,
`adapters/codex/installer.go:194`. This was the one a developer can trigger twice at once.

### 3. `placeEngineBinary` rewrote ~10 MB per run and never reclaimed residue

- Copied the whole engine on **every** `init`, including the common case where the placed engine is
  already byte-identical. Re-running `init` is encouraged (new hook keys only register on re-init).
- `defer os.Remove(tmpName)` covers every ordinary path but **cannot survive SIGKILL**, and nothing
  swept `.openbox-*.tmp`.

Residue forensics: partial sizes are exact multiples of 32 KiB (io.Copy buffer), mode 0600 → killed
mid-`io.Copy`, before `tmp.Chmod(0755)`.

### 4. No concurrency bound on `init`

Nothing limited how many installs could touch one bundle at once. Defects 1–3 supplied the writers;
this is what let their number grow without limit.

## Timeline

| Time | Event | Evidence |
|---|---|---|
| 00:44:23 | manual `openbox init` — last clean run | zsh history; `~/.openbox/dev.json` mtime |
| 02:17 → 02:43 | incident 1: 58 → 167 → 141 → 30 inits/min | tmp mtimes |
| 02:24:21 | settings corrupted | `settings.local.json` mtime |
| 03:15–03:25 | three fixes written to source — **not deployed** | engine symbol check |
| ~03:53 | incident 2 accumulation begins | `Time Since Fork` |
| 09:25 → 09:53 | five logd watchdog timeouts | DiagnosticReports |
| 09:53:59 | 5,838 hung `openbox`, 57 `go`, 31 Claude Code | spindump |
| 09:57 | 247 tmp files; settings re-corrupted | mtimes |
| 09:58:52 | reboot | `kern.boottime` |
| 11:00:23 | fixed engine deployed | engine mtime + symbols |

Residue only survives a kill, so a storm's first residue is a lower bound on when its loop started,
not the start itself.

## Ruled out

cron (none), launchd (no openbox agent), watchman (no roots; the running instance was started by the
investigation itself), scheduled tasks (none), the realtime flusher as a *self*-respawner (`flush`
returns before `RealtimeTrigger.Maybe`, `adapters/claude-code/hookrun.go:62-66` — it never calls
`Install`), and the ambient SessionStart install (git hooks only, default off, `hookrun.go:368`).

**Not ruled out, contrary to the first version: Claude Code sessions.** See Corrections.

`Installer.Install` has exactly one caller: `devinit.applyConfig`
(`cli/internal/devinit/devinit.go:377`) ← `devinit.Run` ← the `init` command (`main.go:474`, `:494`).
`auth` uses `devinit.Register`, which does not install. So the storms were `openbox init`, hundreds
of times, concurrent.

## Fixes

| # | File | Change |
|---|---|---|
| 1 | `adapters/claude-code/localhooks.go` | `writeFileAtomic` (temp + rename, same dir) replaces `os.WriteFile`; explicit chmod so CreateTemp's 0600 does not leak |
| 2 | `adapters/claude-code/installer.go` | skip the copy when byte-identical (size, then SHA-256), still asserting the exec bit; `sweepStaleEngineTemps` reclaims `.openbox-*.tmp` older than 1 h |
| 3 | `adapters/claude-code/installer.go` | `acquireInstallLock` — one installer per bundle, **refuses rather than queues** |
| 4 | `testmain_test.go` × 3 (`adapters/claude-code`, `cli/cmd/openbox`, `adapters/claude-code/cmd/openbox-cc-hook`) | asserted-hermeticity guards: contain `HOME`/`XDG_CONFIG_HOME`/cwd, then fail the suite naming anything written under the sentinel |
| 5 | `creds_test.go`, `main_test.go` (`isolateConfig`, `isolateHome`, `setHookEnv`, child envs) | pin the four ambient sinks, only when unset so helper call order cannot matter |

Design points worth not re-litigating:

- **The lock refuses, it does not queue.** Queueing reproduces the same pile-up with the waiting
  moved into userspace. Stale locks self-heal after a minute, so a killed install cannot wedge the
  bundle permanently — that would turn a crash into a worse failure than the one being prevented.
- **The sweep is age-gated**, or it would delete a temp a concurrent install is mid-copy into and
  break that run's rename.
- **Atomicity is narrow by design**: a reader sees the old file or the new one, never a splice. It
  is *not* mutual exclusion; that is what Fix 3 is for.
- **The guards assert, not merely contain.** Containment alone would have hidden the fourth sink
  (the session registry) — which is exactly how it was found.
- **The sink split was not rerouted.** `OPENBOX_HOME` deliberately does not relocate
  `os.UserConfigDir()` state; the tests were fixed to pin each sink instead of reversing that
  decision.

## Tests added

`localhooks_atomic_write_test.go`, `installer_engine_test.go`, `cli/cmd/openbox/isolation_test.go`,
three `testmain_test.go` guards. Each behavioural fix is proven to fail against pre-fix code:

- reverting to `os.WriteFile` ⇒ `a reader saw an unparsable settings file mid-write: unexpected end
  of JSON input` — reproduces the reported corruption class;
- disabling skip + sweep ⇒ all three engine tests fail;
- fresh-lock case refuses deterministically; the concurrent case measured **1 proceeded / 23
  refused** on three consecutive runs;
- `TestProjectScopedInitWritesNoSettingsIntoTheSourceTree` also asserts the install landed
  *somewhere*, so it cannot pass for the wrong reason.

`TestWriteLocalHooksPublishesAReadableSettingsFile` (0644) does **not** discriminate the fix; it
guards the new implementation's chmod. Noted so it is not mistaken for a repro.

## Verification (fresh, 2026-08-14 12:1x)

- all 11 modules green under `-race`;
- `windows/amd64` + `linux/arm64` cross-compiles build; gofmt clean;
- deployed engine is the fixed build — 9,672,370 B, carries both `sweepStaleEngineTemps` and the
  lock message; `doctor` reports exactly one registered engine;
- dotfile-aware residue count in the plugin bin: **0**;
- **the sound leak check**: after a full workspace run, new `enforcements.jsonl` records carrying a
  *fixture* session id = **0** (6 new records, all genuine UUID sessions).

That last check replaces the earlier "sinks byte-identical before/after", which is **not** a valid
invariant: other live Claude Code sessions write to the same sink concurrently, so the file grows
during any run long enough to overlap one. Byte-equality passed at 11:45 and failed at 12:15 with no
code change between — the discriminator has to be the session id, not the size.

Not run: `testbed/run-all.sh` (needs a live stack). Nothing here touches wire format or the hook
contract, so testbed exposure is low.

## Remediation applied

- Residue cleared: 247 files / 760 MB → 0. The sweep did this itself on the first real `init`,
  which is end-to-end evidence for Fix 2 rather than a unit-test claim.
- Repo-root `.claude/settings.local.json` repaired twice (backups
  `.claude/settings.local.json.corrupt-backup`, `.corrupt-260814-1100`, both git-ignored); 11 hook
  events, one engine, `permissions.allow` preserved each time.
- Audit sink filtered, not truncated: **808 fixture records dropped, 2,154 genuine kept**; backup at
  `enforcements.jsonl.pre-cleanup`. `sessions/sess-xyz.json` removed; `m4`/`manual-1..3` left alone
  (dated 2026-08-13, developer's own manual-test artifacts).
- Test residue removed from the source tree.
- Fixed engine deployed at 11:00:23.

## What remains unproven

**The driver was never identified, across both incidents.** No parent survived; no cron, launchd,
scheduled task, shell history or session transcript covers either window, and logd was already dead
during incident 2 so the unified log holds nothing. Defect 1 explains the mechanism and every
artifact, and repeated `go test` runs are the plausible driver — that is inference, not evidence.

What changed is the blast radius, not the diagnosis: a repeated `init` is now a read and no write, a
concurrent one is refused, residue self-reclaims, and tests cannot reach the real paths at all. A
recurrence should now be invisible rather than fatal.

## Unresolved questions

1. What invoked `openbox init` hundreds of times, twice? Unanswerable from surviving state.
2. **New, and possibly more serious than anything above:** after the re-init, a live session was
   hard-denied on *every* tool call with `OpenBox governance: Session is no longer active`
   (a HALT, confirmed by the findings banner), then recovered on its own ~10 min later. `init`
   advertises enforcement as "inert until your org publishes a policy, and fail-open". A transient
   blanket HALT with no org policy published is neither, and the session-liveness check appears to
   sit *before* that guarantee. Needs its own investigation.
3. Should the hook engine bound its own concurrency, independent of `init`? The convoy needed
   hundreds of writers in one directory; `init` is no longer able to supply them, but nothing stops
   a future path from doing so.
4. Does the equivalent problem exist in global/managed scope (`cli/internal/managed/managed.go`)?
   `managed.go:247` is already temp+rename so the corruption class does not apply; the engine-copy
   amplification and a concurrency bound were not checked there.
5. `init --dry-run` still prints that it would "register developer agent" and "write credentials" —
   both moved to `auth` in ADR-0015. Stale text, harmless, misleading.
6. `adapters/claude-code/posture_test.go` is unformatted — belongs to concurrent in-flight work,
   left untouched.
