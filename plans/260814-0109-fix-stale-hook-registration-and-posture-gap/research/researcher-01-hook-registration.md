# Research: stale hook registration (Issue 1) — hook merge, tests, other surfaces, doctor

## Q1 — Merge/match mechanics (`adapters/claude-code/localhooks.go`)

JSON shape: `settings["hooks"][EventName] = []entry`, `entry = {"matcher":str, "hooks":[]handler}`,
`handler = {"type":"command","command":str,"timeout":int,"statusMessage"?:str,"asyncRewake"?:bool}`
(localhooks.go:85-104).

- `writeLocalHooks(projectDir, engine)` (localhooks.go:56-119): reads/unmarshals settings.local.json,
  for each event in `localHookEvents` builds `command := localHookCommand(engine, "hook claude-code "+ev.Event)`
  (line 80), checks `hasLocalHookCommand(entries, command)` (line 82) — if true, `continue` (skip, no
  write); else appends a **whole new entry** `{"matcher":ev.Matcher,"hooks":handlers}` to
  `hooks[ev.Event]` (lines 100-104). Duplicate detection is per-event, and a miss appends a sibling
  entry rather than replacing anything — this is the mechanism that leaves two live registrations.
- `hasLocalHookCommand(entries, command)` (localhooks.go:133-147): scans every existing entry's
  `hooks[].command`, compares via `unquoteHookCommand(got) == unquoteHookCommand(want)` (line 141) —
  **exact string equality after stripping `"` chars only** (localhooks.go:152-154). No argv parsing.
- `localHookCommand(engine, args)` (localhooks.go:170-172): returns `"` + engine + `" ` + args — engine
  path is quoted, embedded verbatim as the leading token, so any change to the engine's absolute path
  (new plugin version dir, reinstall elsewhere) changes the whole string → `hasLocalHookCommand` misses
  it → old entry preserved, new entry appended (both fire).
- Doc comment (localhooks.go:25-27) *claims* "existing... foreign hook entries are preserved; our entry
  is appended only if the exact command is not already present" — accurate description of current
  (buggy) behavior, not yet updated for the fix.

## Q2 — Full registration surface

`localHookEvents` (localhooks.go:34-51), 11 events, one `hook claude-code <Event>` command each via
`localHookCommand` (line 80):

| Event | Matcher | Timeout | Extra |
|---|---|---|---|
| SessionStart | — | 5 | |
| UserPromptSubmit | — | 5 | |
| PreToolUse | `*` | `preToolUseHookTimeoutSec` | statusMessage; **+ 2nd handler** |
| PostToolUse | `*` | 5 | |
| PostToolUseFailure | `*` | 5 | |
| Stop | — | 5 | |
| SubagentStop | — | 5 | |
| SubagentStart | — | 5 | |
| PermissionDenied | `*` | 5 | |
| StopFailure | — | 5 | |
| SessionEnd | — | 15 | |

PreToolUse gets a 2nd handler (localhooks.go:90-98): `rewake claude-code`, `asyncRewake:true`,
timeout `rewakeHookTimeoutSec`. **Every entry fits one of the two argv shapes** — `hook claude-code
<Event>` (10 events) or `rewake claude-code` (1 extra PreToolUse handler). None deviate; no third shape
exists in this file.

## Q3 — Tests pinning current behavior

File is `adapters/claude-code/localhooks_quote_test.go` (not `localhooks_test.go` — no such file exists).

- `TestLocalHookCommandQuotesTheEnginePath` (line 15): asserts every written command is prefixed
  `"<engine>"`. Unaffected by an argv-shape fix as long as output stays quoted.
- `TestLocalHooksIdempotentAgainstAnUnquotedLegacyEntry` (line 52): pre-seeds an **unquoted** legacy
  entry for SessionStart using the **same** engine path, re-inits with that same engine, asserts exactly
  1 handler (line 81-83). This only exercises quote-normalization on an identical engine string — it
  does NOT cover a differing engine path, so it does not currently guard against the reported bug.
- `TestReInitAddsTheNewHooksExactlyOnce` (line 92, named in the issue context) — the load-bearing one:
seeds a pre-that decision hook set (7 events) plus a foreign `PostToolUse` handler `"my-own-linter"`
(no args, not our shape) (lines 119-122), all using **one fixed** `engine` value (line 94), then
re-inits with that **same** engine and asserts (a) every event registered exactly once — `n==0` and
`n>1` both fail (lines 153-157) — and (b) `"my-own-linter"` survives untouched (lines 160-171). **It
never varies the engine path across the two writes**, so it currently says nothing about stale-path
replacement; it does pin "same-path re-init stays 1-to-1" and "literal foreign command untouched."

No test in this file constructs the two-different-engine-paths scenario from the issue — the fix needs
a new test for that.

## Q4 — Other surfaces with the same failure mode

- **(a) Global/managed-settings path** — `cli/internal/managed/managed.go`: `PlanInstall`(72),
`planProvider`(95), `render`(129), `claudeCodeDir`(138), `codexDir`(156), `Apply`(190), `applyFile`(205),
`writeFile`(241), `wouldWeaken`(302), `uncommented`(317), `Privileged`(334), `ProviderState`(366),
`mandates`(406). Grep for `settings.local.json|ProjectDir` in this file: **zero hits** — confirms this is
the separate global/org-managed surface, structurally a whole-file template render + anti-downgrade guard
(`wouldWeaken`), not a JSON hook-array merge with string-dedup. The exact "additive-merge, string-match"
failure mode in Q1 does not apply here in the same shape. Whether `wouldWeaken`/`applyFile`'s force logic
can itself leave a stale engine path baked into a deployed managed file (a different failure mode) is
**not verified** — bodies not read (budget).
- **(b) Codex adapter — already fixed, is the precedent to port.** `adapters/codex/installer.go` does
  NOT use exact-string matching. `isOpenBoxHandler(raw json.RawMessage) bool` (line 269) + helper
  `stripEngineToken(cmd string) (rest string, ok bool)` (line 287) recognize an owned handler by **argv
  shape**: unmarshal the handler, require `Type=="command"` (line 274), strip the leading engine token
  (line 277), and (per doc comment lines 263-267) check the remainder is "exactly the shape
  `hookCommand` generates, **regardless of which engine** [path] ... a foreign command that embeds such
  an invocation — is foreign (kept)." Doc comment at lines 20-21/30 states matching is by "the EXACT
  command **shape**", explicitly because scanning/exact-string wasn't "enough" — same rationale the
  issue proposes for claude-code. `mergeEvent(existing, ev)` (line 220) is presumably where
  replace-vs-append is decided; call sites/bodies of `mergeEvent`/`isOpenBoxHandler`/`stripEngineToken`
  were not fully read (only signatures + doc comments via grep) — **read installer.go:220-330 in full
  before porting this pattern.**
- **(c) `hasLocalHookCommand` has exactly one caller anywhere in the repo** (grep `hasLocalHookCommand|
  writeLocalHooks` across `*.go`): `localhooks.go` itself (def + call), `installer.go:123` (single call
  site: `adapters/claude-code/installer.go:123` inside claude-code's `Installer.Install`), and
  `localhooks_quote_test.go` (3 test call sites). **No plugin-registration surface** uses it — the bug
  is isolated to this one function/path.

## Q5 — Doctor extensibility (`cli/cmd/openbox/doctor.go`)

Full file read (171 lines). No pass/warn/fail helper abstraction exists — every section is raw
`fmt.Fprintf(a.stdout, ...)` with inline conditional text; "WARNING:" is a literal string prefix typed
per-case, not a shared helper (e.g. lines 74, 85, 90). Sections in order: Identity (36), Enforcement
(42, iterates `p.Flags()` sorted), **Managed OpenBox config** (68, present/unreadable/active
three-way switch — good template to imitate: lines 70-94), Policy decisions (100), **Provider managed
configuration** (110-114, loops `managed.Provider{ClaudeCode, Codex}` → `managed.ProviderState(prov)` —
this is the GLOBAL managed-settings state, not local hooks), "What this does and does not prove" (116).

**Doctor does NOT read `.claude/settings.local.json` today** — confirmed by full-file read, no
reference to that path or to `writeLocalHooks`/`hasLocalHookCommand`/`localHookEvents` anywhere in
doctor.go. A "more than one OpenBox engine registered" check has no existing plumbing to reuse.

Where it would slot in: a new section between "Managed OpenBox config" (68-94) and "Policy decisions"
(100), or right after "Provider managed configuration" (110-114) since that's the only other place the
CLI reports per-provider hook/config state today. It would need to: resolve the project dir (same one
`writeLocalHooks` uses), read+unmarshal `.claude/settings.local.json`, walk `hooks[Event][].hooks[]`,
and for each `command` apply the same argv-shape parse as the fix (strip leading engine token, check
remainder == `"hook claude-code <Event>"` or `"rewake claude-code"`) — reusing/exporting the fix's
parser rather than re-deriving it, since doctor and `writeLocalHooks` must agree on what counts as
"ours." If >1 distinct engine token classifies as ours for the same event, warn.

## Constraints for the fix

- Must keep `TestReInitAddsTheNewHooksExactlyOnce` green unmodified: same-engine re-init stays 1-per-
  event, and the literal foreign `"my-own-linter"` (no leading-token/exact-suffix match) stays untouched
  — the new argv-shape matcher must classify it as foreign, not just "not an exact string match."
- Must keep `TestLocalHooksIdempotentAgainstAnUnquotedLegacyEntry` green: quote-insensitive comparison
  (localhooks.go:152-154 today) must survive being folded into/superseded by the shape-based matcher.
- Must keep `TestLocalHookCommandQuotesTheEnginePath` green: output still quotes the engine path.
- New behavior (currently untested — needs a new test): two entries under one event with **different**
  engine tokens but the same `hook claude-code <Event>`/`rewake claude-code` suffix ⇒ recognized as
  ours, stale one replaced (not kept as a second sibling entry), and the replacement reported (per
  issue's proposed UX).
- Reuse the codex precedent's split: "does this command parse as {leading-token}+{our exact args}" —
  don't scan/contains-match; the whole reason codex's comment gives for shape-parsing over exact-string
  is directly transferable (adapters/codex/installer.go:20-30, 263-267).
- `hasLocalHookCommand`'s only caller is `writeLocalHooks` (Q4c) — the fix is contained to
  localhooks.go; no other file needs a matching change for THIS bug (managed.go's mechanism differs;
  codex is already fixed).

## Unresolved questions

- `cli/internal/managed/managed.go`'s `wouldWeaken`/`applyFile`/`render` bodies not read — unconfirmed
  whether the global/managed-settings path has its own (differently-shaped) stale-engine-path problem.
- `adapters/codex/installer.go:220-330` (`mergeEvent`, full `isOpenBoxHandler`/`stripEngineToken`
  bodies) not read in full — only signatures + doc-comment fragments via grep. Read in full before
  porting the pattern 1:1.
- Whether doctor.go should own the "ours" argv-shape parser or import it from `localhooks.go` (currently
  unexported, same package `claudecode` vs `main` for doctor — a cmd/openbox → adapters/claude-code
  import direction needs checking) is a design choice for the fix plan, not resolved here.

Status: DONE
Summary: Bug is isolated to `hasLocalHookCommand`'s exact-string match (localhooks.go:133-147), one caller (installer.go:123), no other merge surface shares the shape — codex's `isOpenBoxHandler`/`stripEngineToken` (installer.go:269,287) is a ready-made precedent to port. Existing test `TestReInitAddsTheNewHooksExactlyOnce` only exercises same-engine re-init + one foreign-command case, not the differing-engine scenario, so it's compatible with but doesn't yet prove the fix; doctor.go has no settings.local.json plumbing today and would need a new section reusing the fix's shape-parser.
