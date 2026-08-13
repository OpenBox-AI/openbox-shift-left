# Phase 05 — `auth` command wiring

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 3 (resolution), phase 4 (Prompter)
- Blocks: phase 6 (`--rotate` extends this command)
- Fixes defects 1-4 from `scout/scout-01-auth-touchpoints.md`

## Overview

- **Date:** 2026-08-12 (revised 2026-08-13)
- **Description:** `openbox auth` — prompt for every credential field, validate,
  write the secrets to `~/.openbox/.env` and the coordinates to `dev.json`,
  register a new agent when the DID is blank. Re-runnable; every run updates.
  **This command owns authentication outright** (D5): registration lives here, and
  `init` no longer touches credentials at all.
- **Priority:** P1 · **Implementation status:** implemented 2026-08-13 · **Review status:** self-reviewed

## Key Insights

- **`init` structurally cannot update credentials.** `devinit.go:198` short-circuits
  when credentials exist and returns before any network call. So "re-run init" was
  never an update path — this command is the answer, and it must overwrite
  unconditionally.
- **Do NOT use `provider.ConfigUpdate`.** `provider/config.go:28` always sets
  `InstallGitHook: &installGitHook` (non-nil), so it always writes posture.
  `auth` builds `devconfig.Update` **literally**, leaving every posture pointer
  nil. `WriteConfig`'s tri-state merge (`write.go:53`, `setString:99`) then carries
  posture forward untouched.
- **`agent_id` must be prompted, not just warned about.** It is only set on
  `init`'s registration branch (`devinit.go:267`), so reuse-path installs write
  dev.json without it ⇒ `dev sync` and the staleness check cannot fetch policy ⇒
  posture reports `bundle_version: no-policy`. (That *symptom* disappears when
  [inline policy evaluation](../260813-0140-inline-policy-evaluation/plan.md) deletes
  `dev sync` and the bundle — but the underlying need does not: `agent_id` is the
  backend agent identity, and the policy read moves to the per-call verdict rather
  than going away.) The user knows their agent id;
  ask for it — and make its absence *mean* something (register) rather than warn.
- **Prompt order is a UX contract, not an implementation detail.** Cheap non-secrets
  first (org, URLs, agent id), then the branch, then secrets last and only if
  reachable. A first-run user with an org key exported should be able to complete
  `auth` by pressing Enter three times and typing `y` — that is the DX target, and
  the illustrative transcript in the plan's `README.draft.md` is the spec for it.
- **Reject an `obx_key_` org key in the api-key field.** `obx_key_…` is an
  ORGANIZATION control credential; `obx_…` is the agent runtime key
  (`cli/cmd/openbox/credential.go:25-26`). `controlTokenProblem` (`:31`) is the
  mirror-image check — copy its shape. This exact mix-up cost hours of debugging.
- **Warn when real env vars shadow what was just written.** A real env var wins over
  both files, so writing while one is exported produces a config that silently has
  no effect. Under D2 the shadowed target differs per field, and the warning should
  name the right one: `OPENBOX_API_KEY` / `OPENBOX_AGENT_PRIVATE_KEY` shadow
  `.env`; `OPENBOX_AGENT_DID` / `OPENBOX_AGENT_ID` / `OPENBOX_BASE_URL` /
  `OPENBOX_BACKEND_URL` shadow `dev.json`. Warn loudly; do not refuse.
- **There is no public key to collect.** `agent/create` returns `{did, privateKey}`
  only — confirmed in the backend (`did/agent-identity-provider.interface.ts:1-4`)
  and in the platform docs. `public_key?` in `AgentIdentityResponseDto:603` sits
  beside `key_id?`/`key_arn?` and is KMS metadata. If a public key is ever needed
  it is derived from the private key at point of use.

## Requirements

1. `openbox auth [--provider claude-code] [--org X] [--rotate] [--api-key-stdin]
   [--private-key-stdin] [--env-file PATH] [--yes]`, plus the registration flags
   inherited from `init` under D5: `[--agent-name X] [--icon X] [--description X]
   [--force] [--base-url URL] [--backend-url URL]`. **No flag ever takes a secret
   value.**
2. Field set, each prompted with `[current]` where a value exists; blank keeps
   current. **The Written-to column is load-bearing** (D2) — secrets go to `.env`,
   coordinates to `dev.json`, and nothing goes to both:

   | # | Field | Blank means | Secret | Written to |
   |---|---|---|---|---|
   | 1 | org | `local` | no | `dev.json` (naming only) |
   | 2 | backend URL | `https://api.openbox.ai` | no | `dev.json` |
   | 3 | base URL (core) | `https://core.openbox.ai` | no | `dev.json` |
   | 4 | **agent id** | **register a new agent, then stop asking** | no | `dev.json` |
   | 5 | DID | keep current | no | `dev.json` |
   | 6 | api key (`obx_`) | keep current | **yes** | `.env` |
   | 7 | private key (b64) | keep current | **yes** | `.env` |
   | 8 | control token (`obx_key_`, approver installs only) | keep current | **yes** | `.env` |

   **Agent id is the registration trigger, and it short-circuits.** A blank agent id
   (field 4) means "I have no agent" — so after confirming, `auth` registers and
   **skips fields 5-7 entirely**, because registration returns the DID, api key and
   private key. Prompting for values the server is about to hand over is the kind of
   friction this command exists to remove. Fields 5-7 are reached only when an agent
   id was given.

   Both URL defaults are new constants (`DefaultBackendURL`, and the existing
   `DefaultBaseURL` — `devconfig.go:71`). Today there is **no** backend URL default:
   `main.go:163` errors when it is unset, so this phase adds one.
3. Validation: reject `obx_key_` in the api-key field; reject a private key that
   is not valid base64 of 32 bytes; reject a DID not matching `did:aip:<uuid>`.
4. Blank **agent id** ⇒ confirm, then register via the existing `devinit` registration
   path (needs `OPENBOX_CONTROL_TOKEN`); store the returned api key, private key, DID
   and agent id, and prompt for nothing further. Under D5 this is the **only**
   registration entry point — `init` loses its own, so the duplicate-name detection
   and `--force` semantics (`devinit.go:214-240`) must survive the move intact.
5. Writes: secrets to `~/.openbox/.env` via `WriteEnvFile`, then coordinates to
   dev.json via a literal `devconfig.Update`. Order documented; a crash between
   them is recoverable by re-running. **Never write a coordinate to `.env` or a
   secret to dev.json** — the split is the D2 invariant.
6. Re-run display, **no secrets**: `obx_…a91f (57 chars)` for the api key,
   a SHA256 fingerprint of the **derived public key** for the private key (never
   fingerprint the seed), DID and agent id in full.
7. Summary + `[y/N]` confirm before any write; `--yes` skips for automation.
8. `main.go` dispatch gains `case "auth"`, and `--help` lists it — as the **first**
   step of the documented flow, with `init` second (D5).
9. On success, close by naming the next command: `openbox init` (and, when the
   current directory is not a git repo or the user is elsewhere, that `init`
   defaults to project-local scope). `auth` never installs hooks.

## Architecture

```
auth.go
  ├─ collect(Prompter, existing) -> fields      (phase 4 seam, unit-testable)
  ├─ validate(fields)                           (obx_key_, base64, DID shape)
  ├─ maybeRegister(fields)                      (blank DID; reuses devinit)
  ├─ write: WriteEnvFile(secrets)  then  WriteConfig(literal Update: coordinates)
  └─ summary(fields)                            (masked display)
```

Two writes, two files, disjoint key sets. A test should be able to assert that the
`.env` byte stream contains no coordinate key and the `dev.json` byte stream
contains no secret value.

`collect` takes a `Prompter` and returns a struct; no I/O globals. Registration
reuses `devinit`'s existing HALT-on-4xx and once-only-credential handling rather
than a second copy of that logic.

## Related code files

| Path | Why |
|---|---|
| `cli/cmd/openbox/auth.go` | new — the command |
| `cli/cmd/openbox/main.go:81-105` | dispatch + usage |
| `cli/cmd/openbox/credential.go:25-49` | `controlTokenProblem` — copy the shape for the inverse check |
| `cli/internal/devinit/devinit.go:214-290` | registration path to reuse (duplicate-detection + once-only credentials) |
| `adapters/common/devconfig/write.go:16-36,53` | tri-state `Update`; posture pointers stay nil |
| `provider/config.go:17-31` | **do not use** — always writes posture |

## Implementation Steps

1. Skeleton + `main.go` dispatch + usage line; `--help` output test.
2. `collect` against a `testPrompter`: current-value display, blank-keeps-current,
   every field. Tests first.
3. Validation with the `obx_key_` rejection, base64/32-byte check, DID shape.
   Borrow wording from `controlTokenProblem` so both messages read alike.
4. Env-shadow warning: check the three real env vars, warn naming each.
5. Write path: `.env` then dev.json with a literal `Update`. **Guard test:** read
   dev.json bytes before and after, assert every posture field identical and
   `WouldDowngradeEnforce` false.
6. Masked summary + confirm gate; `--yes` bypass.
7. Blank-DID registration branch, reusing `devinit`; assert `init`'s existing tests
   still pass unchanged.
8. `--api-key-stdin` / `--private-key-stdin`: one line each, fixed order
   (api key then private key), documented, validated.

## Todo list

- [x] `auth` dispatched, in `--help`
- [x] `collect` unit-tested via `testPrompter`
- [x] `obx_key_` rejection + base64 + DID-shape validation
- [x] env-shadow warning covers both secrets and coordinates, naming the right
      shadowed file per field
- [x] `.env` + dev.json written; posture-untouched guard test green
- [x] masked summary; `[y/N]` gate; `--yes`
- [x] blank DID registers; existing `devinit` tests unchanged
- [x] stdin automation path, no secret on argv

## Success Criteria

- A first run with `OPENBOX_CONTROL_TOKEN` exported completes with three Enters and
  one `y`: no DID, api-key or private-key prompt is ever shown on the register path.
- Both URL prompts prefill with the hosted defaults; accepting them writes those
  values, and a self-hosted user can override either.
- Run twice with different input ⇒ second input present in `.env`.
- dev.json posture bytes identical before/after; `WouldDowngradeEnforce` false.
- `.env` contains no coordinate key; dev.json contains no secret value.
- `agent_id` written whenever supplied or registered.
- Registration still detects a duplicate name and still honours `--force` after
  moving from `init` to `auth`; `devinit`'s own tests cover it unchanged.
- Success output names `openbox init` as the next step.
- Pasting an `obx_key_` into the api-key field is rejected with a message naming
  the right credential and where to find it.
- `grep -n "api-key\|private-key" auth.go` shows no flag that accepts a value.
- With `OPENBOX_API_KEY` exported, `auth` still writes but warns that the env var
  wins.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Routed through `provider.ConfigUpdate` by habit | M×H | posture-untouched test fails; `install_git_hook` flips | **Adjust:** literal `devconfig.Update`. The test is the tripwire — never weaken it. |
| Reusing `devinit`'s registration changes `init` behaviour | M×H | existing devinit tests fail (HALT-on-4xx, once-only credentials) | **Adjust:** if the refactor cannot stay behaviour-preserving, duplicate the call inside `auth` only and record the duplication as debt. Never ship changed `init` semantics. |
| Agent id left unset in the manual branch | M×M | dev.json has no `agent_id`; `dev sync` still broken | **Mitigated by design:** it is now a prompt, not a warning. If left blank, warn naming the consequence. |
| Crash between `.env` and dev.json write | L×M | `.env` new, dev.json stale | **Accepted:** both writes are atomic individually; re-running `auth` reconciles. Summary states what was written. |
| Fixed stdin order confuses users | M×L | wrong value in wrong field | **Mitigated:** validation catches an `obx_` key in the private-key slot and vice versa; document the order in `--help`. |
| Re-prompting every field annoys on re-runs | M×L | user friction, not a defect | **Accepted:** `[current]` + blank-keeps-current is simpler than change detection (KISS). |

## Security Considerations

- No secret on argv, ever (INV-1) — flags name sources, never values.
- Summary shows last-4 + length for tokens and a fingerprint of the **derived
  public key** for the private key; fingerprinting the seed itself risks leaking
  key material.
- The confirm gate must precede any *remote* call as well as any write, so a
  mistyped org cannot register an agent silently.
- `.env` is written `0600` by phase 2's writer; `auth` must not widen it or copy
  the value anywhere else (no temp file outside the atomic write, no logging).

## Next steps

Phase 6 adds `--rotate` for the already-exists-no-credentials case. Phase 7 then
strips registration and credentials out of `init`, which is what makes this
command's ownership exclusive rather than merely preferred.
