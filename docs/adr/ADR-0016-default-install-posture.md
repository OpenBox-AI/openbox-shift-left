# ADR-0016 — What a bare `openbox init` does: project-local scope, enforce ON

Status: Accepted — 2026-08-13.
Implements: `cli/cmd/openbox/init.go` (`--scope local|global`),
`cli/cmd/openbox/main.go` (`printGovernedScope`),
`adapters/common/devconfig/devconfig.go` (`Enforce *bool`, `ResolveEnforce`),
`adapters/common/devconfig/write.go` (`WouldDowngradeEnforce`),
`provider/provider.go` (`CredentialRef.ProjectDir`).
Reverses: the shipped observe-by-default posture — `ResolveEnforce`'s `false`
default and ADR-0006's "`curl | bash`, then `openbox init --enforce`" flow — and
the `--local-hooks` flag text calling project scope "LOCAL TESTING … never set
this in production" (`main.go:359`).
Related: ADR-0015 (where credentials live), ADR-0006 (in-process decider),
ADR-0002 (INV-3b: enforcement may block, but only in-process).

## Context

This ADR answers one question: **what happens when the developer says nothing?**

`openbox init` had seventeen flags and two defaults that a first-time user would
not predict:

- **it governed the whole machine, or nothing** — hook activation was global via
  managed settings, with a project-scoped `--local-hooks <dir>` escape hatch
  labelled for testing only;
- **it observed** — enforcement required `--enforce`.

Both defaults are being changed at once, in the same command, and that is
deliberate: they are one decision. A governed *project* that actually enforces is
coherent. A governed *machine* that silently starts blocking tool calls
everywhere is not. Choosing them separately is how you get the incoherent pair.

Two facts from the code shape the answer.

**Global scope cannot be activated by the CLI.** `Install` "does NOT modify
global/managed Claude Code settings" (`adapters/claude-code/installer.go:99-101`)
— it materializes the plugin bundle, places the engine binary, writes the config,
and then *prints a snippet* for someone else to deploy:

```json
{"enabledPlugins": ["openbox-observe"]}
```

Enabling a plugin org-wide is an administrator's action through managed settings.
So a bare `init` under a global default wrote a correct config, reported success,
and governed **nothing at all** until a separate manual step happened. The first
evidence of the gap is an empty dashboard, which reads as a broken product rather
than an unfinished rollout.

**Project scope, by contrast, `init` completes itself.** It merges the hook
entries into `<dir>/.claude/settings.local.json` (`localhooks.go:45-55`) and they
take effect on the next session in that directory, with no further step.

**Scope is activation, not location.** `Install` unconditionally materializes the
bundle, places the binary and writes posture, *then* optionally merges the project
settings (`installer.go:102-124`). `--scope` selects whether that last merge
happens. It is not "install here versus there".

## Decision

A bare `openbox init --provider claude-code`:

1. **governs the current directory only** — `--scope local` is the default,
   resolved to the process working directory;
2. **enforces** — `--enforce=false` opts out and the opt-out persists.

`--scope global` skips the project merge and prints the managed-settings snippet,
stating that activation is pending rather than implying it happened.

### What project-local scope leaves ungoverned

Quoting `adapters/claude-code/localhooks.go:18` verbatim, because this is the cost
and it should be in the reader's own words, not a summary of them:

> Sessions started in any other directory stay ungoverned.

So on a machine set up with the default:

- a session in any other project produces **no events at all** — not a session
  row, not a tool call, nothing;
- **absence of events is not evidence of absence of work.** An auditor reading a
  developer's event history sees the projects that were initialized and cannot
  distinguish an uninitialized project from an idle week;
- enforcement does not apply there either, so a policy that blocks a command
  blocks it in one directory.

Anyone reasoning about fleet coverage must therefore treat a default install as
**partial by construction**. The fleet answer is `--scope global` plus managed
settings (`deploy/managed/`), and the docs say so where a user will read it —
`docs/getting-started.md`, not only here.

Codex is **user-scoped only**: its hooks live at `$CODEX_HOME/hooks.json` or
`~/.codex/hooks.json`, and the repo-level `.codex/hooks.json` is "an alternative
location this installer deliberately does not touch"
(`adapters/codex/installer.go:353-357`). So `--scope local --provider codex`
**errors**, and a bare `init --provider codex` resolves to global while saying so
on stdout. Silently governing every Codex session on the machine when the user
asked for one project would be the bad kind of default — worse than an error,
because it over-delivers governance without consent.

### Why enforce, and the two mitigations that make it defensible

Observe-by-default optimizes for a fleet rollout where an operator flips
enforcement on deliberately after watching telemetry. That is the right shape for
the agent runtime. For a developer runtime installed one machine at a time, it
produces a product whose headline feature is off, and the field evidence for that
pattern is in this repo already: `ResolveFinops()` was default-off, and
off-by-default is exactly why the token rollup that *did* exist reached no
dashboard ([ADR-0014](ADR-0014-turn-as-activity-and-identifier-allowlist.md)).

Two honest mitigations, both of which must stay true for this default to remain
defensible:

- **Enforcement is inert until the org publishes a policy.** The decider
  evaluates the org's bundle; with no bundle there is nothing to deny, so a fresh
  install with enforce on behaves like an observing install and gains
  local secret redaction on `Write`/`Edit`. Nothing is blocked until an
  organization decides something should be.
- **`fail_closed` stays OFF.** An OpenBox outage, an unreachable backend, a
  corrupt bundle — none of them block a tool call. Enforce-by-default never means
  a developer's session stops working because a governance service is down. That
  default is not touched here and must not be flipped as a follow-on "for
  consistency"; the two decisions are unrelated.

The residual risk is a developer surprised by a blocked tool call on a machine
they thought was observing. Accepted and disclosed: `openbox doctor` states the
posture in effect with the provenance of each value, `--enforce=false` is one
flag, and the README says the default at the point of install.

### This flips `enforce` only

`--enforce` currently sets three fields — `Enforce`, `Tier2` and `Findings`
together (`main.go:391`). This ADR changes the **default of `enforce`** and leaves
the `tier2` and `findings` writes exactly as they are.

That is not tidiness deferred; it is scope kept out of a doomed area. The tier
model is being removed wholesale: inline policy evaluation makes
`/evaluate` the single decision authority, Tier-2 becomes the only decision path,
and `tier2`/`tier2_timeout_ms` become deprecated config. A successor ADR
supersedes this section. Do not read the tier arrangement described here as a
design being endorsed, and do not spend effort rationalizing a coupling that is
about to be deleted.

### The mechanical precondition: `Enforce` must become `*bool` first

This repo has already been bitten by exactly this bug, and the fix order is part
of the decision rather than an implementation note.

`CLAUDE.md` records why `Finops` had to change type before its default could
flip (commit `42011e0`): as a plain `bool`, "an absent config field and an
explicit `false` were indistinguishable, so the flip would have been a silent
no-op."

`Enforce` was in a similar shape, and the danger was checked against the running
code rather than inferred from it — which matters, because half of what the
`Finops` precedent suggested turned out not to apply:

- **The read side was already safe.** `ResolveEnforce` passed
  `func(c DevConfig) *bool { b := c.Enforce; return &b }`, which returns `&b`
  unconditionally — the exact shape that made `Finops` unflippable. But
  `resolveBoolWithSource` guards every layer behind a **key-presence map** built
  from the JSON actually in the file (`managed.go:141-147` names this case in so
  many words: "Several DevConfig flags are plain bools (Enforce, FailClosed), so
  their accessors always yield a pointer to false when the key is absent"). With
  `enforce` absent the accessor is never called, so the default argument *does*
  reach the caller. A probe confirmed it: absent key + `def=true` resolves to
  `true`. Flipping the default alone would have worked.
- **The write side was not.** The field was `Enforce bool` with
  `json:"enforce,omitempty"`, so an explicit `false` marshals to *nothing*.
  Verified by writing it: `WriteConfig(Update{Enforce: &false})` produced
  `{"developer_did":"…"}` — no `enforce` key at all — and the next read saw an
  absent field and resolved to the default. **Under a default of `true` that
  makes `--enforce=false` silently un-appliable**: the user opts out, the command
  reports success, and enforcement is back on the next time anything reads the
  config.

So the type change is required, for the second reason only. One field becomes
`*bool` with a tri-state accessor (`func(c DevConfig) *bool { return c.Enforce }`),
mirroring `Finops`.

The round-trip test — write `--enforce=false`, assert the key is **present and
false** in the file, re-read, still false — is the assertion that proves it, and
it is load-bearing: a default that cannot be turned off is not a default, it is a
mandate.

Recording the correction rather than the tidier story, because a future reader
flipping another default needs the real rule: **check both directions
separately.** The presence map protects reads; nothing protects writes but the
field's type. A plain `bool` with `omitempty` cannot express an explicit false,
and that is invisible until someone tries to opt out.

`WouldDowngradeEnforce` needed the same treatment, and this one *was* broken as
predicted. It read `prior.Enforce && next != nil && !*next` (`write.go:91-97`), so
for a config that never wrote the field it saw `prior.Enforce == false` and
reported "no downgrade" even though the *effective* posture was enforce. Once
absent means on, the guard has to compare **resolved** postures — otherwise the
one message that tells a user their posture just weakened goes silent in precisely
the common case, which is the case it exists for.

The next person to flip a default in this repo should read this section before
writing the one-line change.

## Consequences

**Gained**

- `init`'s exit status matches what it accomplished. Under the old default,
  success meant "a config was written"; now it means "these sessions are
  governed", and `printGovernedScope` names which.
- A working install in one command after `auth`, with no administrator in the
  loop — which is the difference between a tool a developer can try and a tool
  that requires a rollout.
- Enforcement, secret redaction and the approval loop are on for anyone who
  installs, so the product's primary features are exercised rather than dormant.
- Seven flags instead of seventeen. `--help` fits on a screen.
- `Enforce` gains a persistable opt-out it did not have. `--enforce=false` was
  already broken before this ADR (it marshalled to nothing); fixing it is a bug
  fix that happens to be a precondition.

**Lost — the accepted trade-offs**

- **Partial coverage is now the default state of a fleet.** Every claim about what
  a developer did is scoped to initialized directories, and nothing in the data
  distinguishes "not governed" from "not working". This is the largest cost and
  the reason this ADR exists.
- **Enforcement can surprise.** Mitigated by the inert-without-policy property and
  fail-open, not eliminated.
- **`init` now writes into project directories by default**
  (`.claude/settings.local.json`). Per-developer and git-ignored by Claude Code
  convention — but a committed one would push one developer's absolute engine path
  onto their whole team. The existing merge guarantees (additive, idempotent,
  foreign entries preserved, hard error rather than silent overwrite on invalid
  JSON — `localhooks.go:56-60`) are what keep this safe and must not be relaxed.
- **Two commands to get running** (`auth`, then `init`) where there was one. The
  split is what makes `init` credential-free, which is its own security gain
  ([ADR-0015](ADR-0015-plaintext-credential-file.md)): after it, a command run in
  every developer's shell can no longer read, write or prompt for a secret.

## Alternatives rejected

**Keep global as the default.** Matches the production posture the docs described
and needs no ADR. Rejected because the CLI cannot deliver it: a bare `init` would
keep claiming success while governing nothing, waiting on a managed-settings
deployment it cannot perform. A default the tool cannot complete is a default
that lies about its own exit code.

**Default the scope by whether a managed config is present** — global when the org
already mandates, local otherwise. Genuinely clever and rejected on cost of
explanation: two defaults, two code paths, two test matrices, and a user who
cannot predict what `init` will do without first knowing their machine's managed
state. `printGovernedScope` would have to explain the inference as well as the
result.

**Keep observe as the default and make `--enforce` more prominent** — better help
text, a post-install nudge. Cheapest option, no behaviour change. Rejected on the
`Finops` evidence: a default-off feature does not get turned on by better
documentation, it just stays off and the telemetry proves it.

**Flip enforce and leave scope global.** The incoherent pair. A machine-wide
silent switch to blocking tool calls in every project, from a command whose
activation step the user has not yet performed — so the enforcement arrives later,
from a rollout the developer did not run. Rejected on the framing in Context: the
two defaults only make sense together.

**Flip the default without changing `Enforce`'s type.** The tempting one-line
version, and it would *appear* to work: the read path honours the new default
correctly (see the precondition above), so a fresh install would enforce and a
reviewer running it would see exactly the intended behaviour. Rejected because
`--enforce=false` would not survive being written — `omitempty` drops it, and the
next read re-enables enforcement. Shipping a mandate labelled as a default is
worse than shipping no flip at all, and it is the failure a reviewer is least
likely to catch, because the happy path is flawless.
