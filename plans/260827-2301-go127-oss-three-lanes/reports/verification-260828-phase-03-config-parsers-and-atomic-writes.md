# Phase 03 verification — config parsers and atomic writes

**Date:** 2026-08-28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture` · **Decisions:** D-OSS-6, D-OSS-7, D-OSS-8

Three swaps. One fixed a real posture bug. Two ran into facts the plan did not
have, were escalated, and were **ruled on by the owner mid-phase**.

## Verdict

All three landed. Whole-workspace verdict set moved by exactly the three tests
added; FAIL count unchanged at 19 (the sandbox listener set), no data races, both
cross-compiles green, all 12 modules green under `GOWORK=off`.

**The `.env` swap removed a deliberate credential-safety control and added a
credential-disclosure path.** Both are owner-ruled and both are pinned by tests.
Read [D-OSS-7](#d-oss-7--env--godotenv-owner-ruled-behaviour-changes) before
touching that file.

## D-OSS-6 — TOML: a real bug, reproduced first

`devconfig/toml.go`'s scanner set `inTable = true` on **any** line whose first
character was `[`, then skipped the rest of the file. TOML permits a continuation
line to begin with `[`, so a valid mandate file could silence every later
top-level key. The consumer is `codexMandated`, so the failure direction was a
**mandated machine reading as UNMANDATED** — enforcement reported absent while it
was in force.

**The first reproduction attempt was wrong and passed**, which is worth recording
because it nearly produced a "fixed" claim with no bug behind it. `key = [`
followed by indented elements does NOT trip the scanner: those continuation lines
start with a quote or a digit. It takes a line whose own first character is `[`.
The two shapes that do it, both now pinned:

- a multi-line basic string containing a bracketed line (a `notice` documenting a
  TOML snippet — entirely plausible in a managed requirements file);
- a wrapped array-of-arrays (`pairs = [\n[1, 2],\n]`).

`TestTopLevelTOMLKeys_BracketLeadingContinuationDoesNotHideLaterKeys` fails on
the old scanner (both legs) and passes on go-toml.

**Divergence check before deleting**, as the phase's risk table required. Both
shipped `requirements.toml` files and both `managed_config.toml` files yield
identical key sets under the new parser
(`{allowed_approval_policies, allowed_sandbox_modes}` and
`{approval_policy, sandbox_mode}`), so `codexMandated` answers the same on every
real file. One test assertion did change, deliberately:

> `TestTopLevelTOMLKeys_Edges` asserted that `hooks.allow_managed_hooks_only = true`
> is reported as the literal string `"hooks.allow_managed_hooks_only"`. A real TOML
> parse binds a dotted key as nesting, and `hooks = {…}` is indistinguishable from
> a `[hooks]` header once decoded. **The E8-S8 safety half is unchanged and still
> asserted** — asking for `allow_managed_hooks_only` must not match — and nothing
> ever consumed the verbatim form (`codexRequirementKeys` are all bare names). The
> assertion now states the parse-based truth.

Also newly documented in the code: an **inline table** (`key = {…}`) is now
excluded where the scanner included it. Under-reporting, which is the direction
the scanner's own doc comment committed to, and none of the three mandate keys is
plausibly an inline table.

## D-OSS-7 — `.env` → godotenv: owner-ruled behaviour changes

The plan's mitigation was "use its plain `Read`" so godotenv would not expand
variables. **That mitigation does not exist:** `Read` → `Parse` → the same
parser, and `expandVariables` is called unconditionally (`parser.go:157,177`).
Verified in the vendored source, not inferred.

A differential over 18 cases produced **5 divergences, every one a loss**:

| Case | Hand-rolled | godotenv |
|---|---|---|
| `K=old` then `K=new` | **error**, naming key + line | **last-wins**, silent |
| `K=a$HOME` | `a$HOME` | `a` (expanded) |
| `K="a$HOME b"` | `a$HOME b` | `a b` |
| `K="line1\nline2"` | literal `\n` | a real newline |
| `K=abc # note` | `abc # note` | `abc` |

Plus one found while implementing, which is a **disclosure rather than a parse
difference**: godotenv's error for a malformed line **echoes that line**. On a
file whose entire purpose is credentials, a line that is a bare secret (a pasted
key that lost its `KEY=`) puts the secret into the error string and from there
into whatever prints it. The retired parser named the file and line number and
never the content, and the suite asserted exactly that.

**Owner ruling (2026-08-28): follow the package default and standard; do not
modify or work around anything.** So none of the five is compensated for, and the
duplicate-key refusal is gone. The reasoning it encoded is preserved in
`ParseEnvFile`'s comment rather than in code: two lines setting one credential
means the user believes something the file does not say, and the loser surfaces
later as an unexplained 401.

What that required in tests — four subtests updated, each labelled
`BEHAVIOUR CHANGE, D-OSS-7`, plus one new test:

- `duplicate key is last-wins, not an error` (was: an error);
- `empty key is accepted, bound to the empty name` (was: an error);
- `trailing hash starts a comment` (new; the old parser treated it as data);
- `line with no equals` — message changed, so the substring assertion moved;
- `TestWriteEnvFileRefusesToOverwriteAnUnparseableFile` used a duplicate-key file
  as its unparseable example. That now parses, so the fixture became a line with
  no `=`. **The property under test is unchanged** — an unreadable credential file
  must stop the write, and it still does;
- **`TestParseEnvFileErrorEchoesTheOffendingLine` (new)** pins the disclosure in
  the direction of the truth. A test that quietly stopped checking would leave the
  exposure invisible; this one makes it show up in the suite, so removing it later
  is a decision. It `t.Skip`s with an explanatory message if godotenv ever stops
  echoing, which is the signal to update the comment.

Only the READ side moved. `WriteEnvFile` is untouched: 0600, the header, the
merge-unknown-keys behaviour, and its refusal of values containing `'`/`\n`/`\r`
all stand. `unquote` was deleted as dead.

## D-OSS-8 — renameio: the package is unix-only

`renameio/v2` declares a `!windows` constraint on **every** file, so on Windows
the package is empty and `renameio.WriteFile` is undefined. This repo
cross-compiles windows/amd64 in CI (`ci.yml:127`), and that build broke — caught
by the cross-compile, invisible to the workspace build and to every test.

**Owner ruling: follow the package's default and standard, do not work around
it.** Taken as: honour the package's *own* declared platform scope. Use it where
it applies, leave the pre-existing code where the package offers nothing. That is
respecting the constraint the library itself wrote, not routing around it.

Shape: one unexported `atomicWriteFile` / `writeFileAtomic` per module, split
`_windows.go` / `!windows`. Four duplicated `CreateTemp→Write→Close→Rename`
blocks collapsed into two helpers, which is a DRY win independent of the swap.

- unix: `renameio.WriteFile` — fsyncs the file **and its parent directory** before
  renaming, and keeps its temp in the destination directory (the v2 fix; v1 could
  use `/tmp`, which would both break atomicity across devices and put session
  data outside `~/.openbox`);
- windows: byte-for-byte the previous temp+rename. **No regression, and no gain** —
  the asymmetry is stated in both files rather than averaged away.

`advisory.go` untouched, verified by an empty `git diff` — it is `O_APPEND` and
atomic by POSIX for small writes, deliberately.

**Modes are asserted, not assumed** (phase requirement 5).
`TestAtomicWritesKeepModes` checks 0600 on the duration record and turn cursor,
0700 on the session dir, and that a **rewrite** does not widen them — the last
matters because renameio's `WithExistingPermissions()` preserves an existing
file's mode, so a drift would be silent. Drilled: writing 0644 instead ⇒ red on
both the create and the rewrite leg.

## Evidence

| Check | Result |
|---|---|
| `gofmt -l` | clean |
| `go vet` (12 modules) | clean |
| `GOOS=windows GOARCH=amd64` | clean — **this is the check that caught D-OSS-8** |
| `GOOS=linux GOARCH=arm64` | clean |
| per-module `GOWORK=off go build` | **12/12 ok** |
| full suite `-race` | no `DATA RACE`; same 6 sandbox-red modules |
| whole-workspace verdict-set diff | **+3 tests, FAIL count unchanged at 19, nothing else moved** |
| TOML regression | fails on the old scanner (both legs), passes on go-toml |
| real-fixture key sets | identical old vs new on all 4 shipped TOML files |
| mode drill | 0644 ⇒ red on create and rewrite |

The verdict-set diff tracks TOP-LEVEL tests only, so the four `TestParseEnvFile`
subtest changes above do not appear in it. They are listed in D-OSS-7 instead.

**A chore this phase surfaced:** a new dependency in a shared module
(`devconfig`, `hookflow`) needs `go mod tidy` in every module that transitively
depends on it, or `GOWORK=off` builds fail with "missing go.sum entry" while the
workspace build stays green. It bit twice. Worth a line in phase 07's docs.

## Unresolved questions

1. **Is the `.env` credential-echo acceptable long-term?** It is owner-ruled and
   pinned, but it is a real disclosure into logs and it was found after the
   ruling, not before it. If the answer changes, `TestParseEnvFileErrorEchoesTheOffendingLine`
   is the test that flips and the fix is a wrapped error — which the current
   ruling excludes.
2. **Windows durability asymmetry.** Unix now fsyncs, Windows does not. Windows is
   build-verified only, so nothing observes the difference today. Flagged because
   "atomic write" now means two things depending on platform.
3. **`godotenv` expansion vs. `WriteEnvFile`'s refusal.** The writer refuses
   `'`/`\n`/`\r` because the old parser unescaped nothing; godotenv DOES unescape
   in double-quoted values. The two halves of the codec no longer share one model
   of the format. Not a live bug — the writer only ever emits single quotes — but
   the invariant that made the pair safe is gone.
