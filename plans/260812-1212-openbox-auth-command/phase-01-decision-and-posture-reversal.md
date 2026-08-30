# Phase 01 — that decision + that decision + docs: plaintext posture, install defaults

## Context links

- Parent: [plan.md](plan.md)
- Depends on: nothing. **Blocks every code phase** — it authorizes both reversals.
- Repo rules: `CLAUDE.md` ("Cite sources in docs"; "prefer an honest limit over a
confident sentence"; a new posture needs a
decision record)
- Docs to change: `docs/data-and-privacy.md`, `docs/architecture.md`
  (`#assurance--what-the-evidence-proves`)
- Prior decision records: `.0014` ⇒ these are **that decision** and **that decision**

## Overview

- **Date:** 2026-08-12 (validated and expanded 2026-08-13)
- **Description:** Two decision records. **that decision** reverses the "keychain default, halt
rather than fall back to plaintext" posture and records `golang.org/x/term` as the
repo's first external dependency. **that decision** records what a bare `openbox
init` now does — project-local scope **and** enforce ON — and what each default
costs. Then make the user-facing docs true about both.
- **Priority:** P1 — no code phase may land before this
- **Implementation status:** implemented 2026-08-13
- **Review status:** self-reviewed; awaiting code-reviewer

## Key Insights

- The posture being reversed is **explicit and deliberate**, not incidental:
  `cli/internal/secret/file.go:14-25` states the file backend is "never selected
  automatically — the operator must ask for it", plaintext-at-rest is "the one
  guarantee it trades away", "hence opt-in only". `secret.go:13` says a platform
  with no store ⇒ `ErrNoStore` ⇒ "the caller HALTS". Deleting that is a
  security-posture change with a documented rationale to argue against.
- **The plaintext seed is the attestation-integrity story.** Anything running as
  the user can read `~/.openbox/.env` and sign governance events as this agent.
  The governed coding agent runs arbitrary commands as that user. This is the
  product's own threat model, so the docs must say it rather than imply
  otherwise.
- **Per-OS asymmetry must be stated.** macOS/Linux keep real `0600` owner-only.
  On Windows `os.Chmod` only toggles the read-only attribute, so `0600` is a
  no-op and other local accounts can read the file. There is no at-rest
  protection there at all.
- **The file becomes the sole copy of a once-shown secret.** `agent/create`
  reveals the API key and private key exactly once (confirmed in the platform
  docs: credentials "not stored by OpenBox"). Losing the file ⇒ rotate or
  re-register. Say so in the file's own header comment, not only in docs.
- `x/term` is needed only for **input UX** now, not storage. Be honest that the
  secret lands in plaintext anyway, so masking defends against terminal
  scrollback, screen sharing and tmux buffers — not against disk access.
- **The org control token is the bigger exposure, and it is easy to miss** (D3).
`.env` also holds `OPENBOX_CONTROL_TOKEN` on approver installs — an `obx_key_`
ORGANIZATION key that can create and rotate agents org-wide, keychain-protected
today (`approve.go:96-114` reads it from the store as a fallback). An agent seed
compromises one agent; this compromises the org's agent fleet. That decision must
name it separately from the seed, not fold it into one sentence about
"credentials".
- **Existing keychain credentials are stranded, by decision** (D1). Nothing reads
the keychain before it is deleted, so an existing macOS/Linux install must
`auth --rotate` (needs an org key) or re-register. That decision states this
as a consequence rather than leaving a reader to discover it.
- **Local-by-default scope is a governance regression on paper, and the honest
default in practice** (D6). It leaves sessions in every other directory ungoverned
(`adapters/claude-code/localhooks.go:18`) — and today's `--local-hooks` flag text
calls project scope "LOCAL TESTING … never set this in production". The argument
for it anyway: `Install` **cannot** activate global scope by itself; it only
prints the `{"enabledPlugins": ["openbox-observe"]}` snippet for managed settings
(`installer.go:99-101`). So project-local is the only scope the CLI can actually
complete, and defaulting to it stops `init` from claiming an activation it did not
perform. Enterprise deployment stays managed settings + global.
- **Enforce-by-default reverses that decision's observe default, and pairs naturally with
local scope.** The two are one decision — a governed *project* that actually enforces
is a coherent default, where a governed *machine* that silently enforces would not
be. Two honest mitigations belong in that decision: enforcement is inert until the
org publishes a policy, and `fail_closed` stays off, so an OpenBox outage never
blocks a developer. That decision must also record the mechanical precondition —
`Enforce` becomes `*bool` before the default flips, for the reason `Finops` did
(commit `42011e0`) — because a reader who sees the flip without that context will
reintroduce the silent-no-op bug the next time a default changes.

## Requirements

1.  recording: the decision, the
   context (cross-platform simplicity vs at-rest protection), what is deleted,
   consequences incl. the Windows asymmetry, and the rejected alternatives
   (keychain-default, DPAPI, Credential Manager) with why each was rejected.
2. That decision names **two** distinct exposures, not one: the agent signing seed, and
   the org `OPENBOX_CONTROL_TOKEN` with its org-wide create/rotate authority (D3).
3. That decision records that **existing keychain credentials are not migrated** (D1),
   and states the recovery path: `auth --rotate`, or re-register, or the manual
   keychain read documented in phase 8's migration note.
4. That decision records the file split: `.env` holds secrets only, `dev.json` keeps the
   non-secret coordinates (D2) — one store per field, and the reason (a second DID
   store is the bug class this avoids).
5.  recording **both** defaults a bare
   `openbox init` now applies, because they are one decision — what an install does
   when the developer says nothing:
   - **project-local scope:** what stays ungoverned, and why local anyway (global
     cannot be self-activated — `installer.go:99-101`). Enterprise deployment is
     managed settings + `--scope global`. Alternatives rejected: default-global, and
     default-by-managed-config-presence.
   - **enforce ON:** reversing that decision's observe-by-default. Note that enforcement
     is inert until the org publishes a policy, and that `fail_closed` stays **off**
     so an OpenBox outage still never blocks a developer. Record that `Enforce` had
     to become `*bool` first, for the reason `Finops` did (commit `42011e0`) — a
     plain bool cannot distinguish an absent field from an explicit opt-out, so the
     flip would have been a silent no-op.
   - state explicitly that this flip covers **`enforce` only** — the `tier2` and
     `findings` fields keep whatever `init` writes today, because
     [inline policy evaluation](../260813-0140-inline-policy-evaluation/plan.md)
     removes the tier concept and deprecates those fields. That decision should note that
     That decision supersedes this area rather than describing a tier model that is about
     to go.
6. `README.md` index updated with both.
7. `docs/data-and-privacy.md` updated: credentials now live in a plaintext file;
   remove/replace any claim of OS-keychain protection.
8. `docs/architecture.md#assurance--what-the-evidence-proves` gains two limits:
   the signing key is readable by anything running as the developer (so attestation
   proves origin-of-config, not tamper-resistance against the developer or the
   agent they run), and project-local scope means absence of events is not evidence
   of absence of activity.
9. `cli/go.mod` + `go.sum` gain `golang.org/x/term` (and `golang.org/x/sys`
   transitively), pinned. `go.work.sum` updated.
10. That decision records the dependency decision in the same document (one deliberate
    departure, not two).

## Architecture

No code behaviour. Three artifacts: two decision records, and truthful edits
to two user-facing docs. `cli/go.mod` changes here so no later phase has to
touch a module file that a parallel phase also edits.

Two decision records rather than one long one because they answer different
questions for different readers: that decision is "where do my credentials live and
who can read them", that decision is "what does a bare `openbox init` do to my
machine". The two install defaults share one document because they are one question
— what happens when the developer says nothing.

## Related code files

| Path | Why |
|---|---|
| `cli/internal/secret/file.go:12-32` | the posture being reversed, quoted in that decision |
| `cli/internal/secret/secret.go:13,30-32` | the HALT rationale being deleted |
| `cli/cmd/openbox/approve.go:67,96-114` | the org token's current store-backed path, which that decision moves to plaintext |
| `adapters/claude-code/localhooks.go:10-18` | "sessions in any other directory stay ungoverned" — quote it in that decision |
| `adapters/claude-code/installer.go:90-101` | global activation is a printed snippet, not an action — that decision's core argument |
| `docs/data-and-privacy.md` | must stop claiming keychain protection |
| `docs/architecture.md` | assurance limits section |
| `cli/go.mod`, `go.work.sum` | dependency pin |

## Implementation Steps

1. Read `file.go:12-32` and `secret.go:1-35` and quote the exact prior rationale
in that decision's Context — that decision must argue against the real prior
reasoning.
2. Draft that decision: Status accepted, Date 2026-08-12, Decision, Context,
   Consequences (incl. per-OS table), Alternatives rejected, and a
   "What this weakens" section naming **both** the seed and the org token, plus
   the stranded-keychain consequence and the two-file split.
3. Add the dependency subsection: `x/term` for masked input + TTY detection,
   why no stdlib option works on Windows, and that storage needs no dependency.
4. Draft that decision the same way: quote `localhooks.go:18` for what is ungoverned,
   and `installer.go:99-101` for why global is not self-activating. Reject
   default-global (claims an activation it cannot perform) and
   default-on-managed-config-presence (two defaults to explain and test).
5. Update `README.md` with both entries.
6. Rewrite the credential-storage paragraphs of `docs/data-and-privacy.md`.
7. Add both assurance limits to `docs/architecture.md`, matching the existing
   known-limits list style.
8. `cd cli && go get golang.org/x/term@latest && go mod tidy`; run
   `go build ./...` in cli to confirm the pin resolves; commit go.sum.

## Todo list

- [x] that decision written, prior rationale quoted, alternatives recorded
- [x] that decision names the org token separately from the seed
- [x] that decision states the stranded-keychain consequence + recovery path
- [x] that decision records the `.env`/`dev.json` split and why
- [x] Dependency decision inside
- [x] that decision written: scope default, what is ungoverned, why local anyway
- [x] that decision covers the enforce default: the reversal of that decision's observe
      default, the two mitigations, the `*bool` precondition, and that the flip is
      `enforce`-only because that decision removes the tier model
- [x] `README.md` index updated with both
- [x] `docs/data-and-privacy.md` no longer claims keychain protection
- [x] `docs/architecture.md` gains both assurance limits
- [x] `x/term` pinned; `cli` builds; `go.work.sum` updated

## Success Criteria

- Grep for "keychain" across `docs/` returns no stale claim of protection.
- that decision states the Windows asymmetry explicitly.
- that decision makes the org-token exposure findable on its own — a reader searching
  for `OPENBOX_CONTROL_TOKEN` lands on the escalation, not on a generic sentence.
- that decision exists and a reader can answer both "why is my colleague's other project
  ungoverned?" and "why did a tool call just get blocked on a fresh install?" from it
  alone.
- that decision states the `Enforce *bool` precondition explicitly enough that the next
  person to flip a default does not repeat the plain-bool mistake.
- `cd cli && go build ./...` green with the new pin.
- A reader of `docs/data-and-privacy.md` can tell, without reading code, that the
  signing key sits in plaintext and what that means.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| decision record written as a rubber stamp, not a real argument | M×M | decision record has no Alternatives section or does not quote the prior rationale | **Adjust:** rewrite. a decision record that does not engage the prior decision is worse than none. |
| Org-token escalation folded into one sentence about "credentials" | M×H | grep for `OPENBOX_CONTROL_TOKEN` in that decision returns nothing | **Adjust:** it gets its own subsection. An org-wide credential in plaintext is a different decision from an agent seed in plaintext, and a reader must be able to find it. |
| that decision reads as an excuse rather than a decision | M×M | it states the default without stating what is ungoverned | **Adjust:** rewrite. The regression is the point of the document; quote `localhooks.go:18` verbatim. |
| Docs updated optimistically, understating the weakening | M×H | reviewer cannot tell from docs that the seed is readable | **Stop and replan:** this is the exact failure `CLAUDE.md` names ("a governance product that overstates itself is the failure it exists to prevent"). |
| `x/term` pulls more than `x/sys` | L×L | `go mod graph` shows extra nodes | **Adjust:** record the actual set in that decision; if it is more than x/sys, re-evaluate masking vs no-masking before phase 4. |
| Later phase needs a second dependency | L×M | phase 2-6 wants a dotenv or ACL library | **Stop:** amend that decision first. Dotenv parsing is hand-rolled by decision. |

## Security Considerations

This phase's entire product is an honest account of two security downgrades. It
implements neither — but nothing else may land until they are written, so a future
reader can never find the plaintext store or the local-scope default without also
finding the reasoning and the limits. Do not soften any of:

- on Windows, any local account can read `.env`; `0600` is a no-op there;
- the agent under governance can read its own signing key;
- on an approver install it can also read an org key that creates and rotates
  agents fleet-wide;
- with project-local scope, a session started anywhere else is not governed and
  produces no events — so absence of events is not evidence of absence of work.

## Next steps

Phase 2 (`~/.openbox/` layout + dotenv codec) and phase 4 (Prompter) may then run
in parallel.
