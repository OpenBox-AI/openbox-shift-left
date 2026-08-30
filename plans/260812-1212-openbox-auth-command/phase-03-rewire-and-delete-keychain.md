# Phase 03 — Rewire credentials, delete keychain, rename to platform names

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 2 (codec + paths)
- Blocks: phase 5 (`auth` writes what this reads)
- **Largest and riskiest phase.** Cross-module; touches 7 of 11 modules.

## Overview

- **Date:** 2026-08-12
- **Description:** Make `~/.openbox/.env` a credential source in
  `ResolveCredentials`, delete keychain support and the `(service, account)`
  namespacing, and rename the private-key variable to the platform's documented
  name.
- **Priority:** P1
- **Implementation status:** implemented 2026-08-13
- **Review status:** self-reviewed; awaiting code-reviewer

## Key Insights

- **`ResolveCredentials` is a single funnel** (`devconfig.go:457`). Env is already
  checked first (`:491` api key, `:505` seed) with the store as fallback. Swapping
  the store for the `.env` map is a small, local change — not a rewrite.
- **The precedence has two halves, and conflating them is the trap** (D2):
  - **secrets** (api key, private key): **real env > `~/.openbox/.env` > unset**.
    The `.env` replaces the store in exactly the position the store held.
  - **coordinates** (DID, base URL): **unchanged** — `FirstNonEmpty(os.Getenv(…),
    cfg.…)`, i.e. real env > `dev.json` > default (`devconfig.go:471-474`).
  `.env` is never consulted for a coordinate, so there is no second DID store and
  no drift to reconcile. A user who wants to override a DID sets the env var, which
  already works.
- **The adapter lookups are package variables** — `adapters/claude-code/creds.go:97`
  and `adapters/codex/creds.go:68` are each `var secretLookup = devconfig.OSSecretLookup`.
  One line each. This is why the deletion is tractable.
- **THREE names exist for one value.** Verified:
  - `OPENBOX_ED25519_SEED` — `devconfig.go:59` (`EnvSeedDirect`)
  - `OPENBOX_SEED` — `actions/openbox-git-action/cmd/openbox-git-action/main.go:107`
  - `OPENBOX_AGENT_PRIVATE_KEY` — **the platform's own documented SDK variable**
  A developer following the published docs sets a variable shift-left ignores.
  That is a live defect, independent of this feature. Converge on the documented
  name.
- **The divergence is this one variable, and no other** (validated 2026-08-13).
  `OPENBOX_AGENT_DID`, `OPENBOX_API_KEY`, `OPENBOX_AGENT_ID`, `OPENBOX_BASE_URL`,
  `OPENBOX_BACKEND_URL` and `OPENBOX_CONTROL_TOKEN` already match the platform docs
  (`devconfig.go:35-67`). Do not "align" names that are already aligned — the
  rename is one constant plus the git action's alias.
- `SeedB64` spans 7 modules: `cli`, `client`, `adapters/{claude-code,codex}`,
  `adapters/common/{devconfig,git}`, `actions/openbox-git-action`. The rename is
  mechanical but wide — it needs its own step and an all-modules build gate.
- **The two-DID-stores bug dissolves here.** With no keychain there is one DID in
  one file, so the revert loop (`devinit.go:200` reading a stale keychain DID and
  writing it into dev.json) cannot happen. Recorded so a reader knows it was
  handled, not forgotten.
- `secret.Store` / `Detect` / `Open` / `accounts()` lose their only reason to
  exist. `approve.go:111` also reads the approver **control token** from the store
  — it needs the same treatment or an explicit decision, or `openbox approve`
  breaks.

## Requirements

1. `ResolveCredentials` resolves **secrets** real env var → `.env` map, and leaves
   **coordinate** resolution exactly as it is (env → `dev.json` → default). Missing
   api key or private key ⇒ clear error naming `~/.openbox/.env` and `openbox auth`.
   On macOS/Linux that error also names the manual keychain-read command
   (`security find-generic-password -s <service> -a <account> -w` /
   `secret-tool lookup service <service> account <account>`), because an existing
   install's credentials are stranded there by D1 and this error is where the user
   meets that fact.
2. Rename `EnvSeedDirect` → `EnvAgentPrivateKey` = `"OPENBOX_AGENT_PRIVATE_KEY"`;
   `Credentials.SeedB64` → `PrivateKeyB64` across all 7 modules.
3. **Back-compat reads:** `OPENBOX_ED25519_SEED` and `OPENBOX_SEED` still honoured
   as deprecated aliases; warn once (stderr, never stdout — hooks must not write
   stdout) naming the replacement. Write only the new name.
4. Delete: `cli/internal/secret/backends.go`, the GOOS cases in
   `devconfig.OSSecretLookup` (and the function), `secret.Detect`/`Open`/`Store`/
   `Service`, `fileStore`, `DefaultFilePath`, `devinit.accounts()`,
   `DevConfig.SecretFile`, `EnvSecretFile`, `FileSecretLookup`, `SecretLookup`,
   and `openStore` in `main.go`.
5. `--secret-backend` removed (and its warning at `main.go:462`); its presence must
   error with a pointer to `openbox auth`, not be silently ignored. **The rest of
   `init`'s flag surface is phase 7's** — this phase touches only the flag whose
   backing code it deletes.
6. Approver: `approve.go` reads its control token from `~/.openbox/.env`
(`OPENBOX_CONTROL_TOKEN`), `approver.json` keeps the non-secret config. Writing
that token becomes `auth`'s job (phase 5), not `init --role approver`'s. That
decision must already name this escalation (phase 1, req 2).
7. All 11 modules build, vet and test green.

## Architecture

```
secrets:      real env var > ~/.openbox/.env                    ─┐
coordinates:  real env var > dev.json > built-in default         ├─> Credentials
                                  (single funnel, devconfig.go:457)┘
```

Two source chains, one funnel. The `.env` sits exactly where the secret store sat
and nowhere else, so no field has two files that can disagree.

`SecretLookup` and everything behind it is deleted; there is no injectable store
seam any more. Tests inject via `OPENBOX_HOME` pointing at `t.TempDir()` plus
`t.Setenv`, which is simpler than the old lookup-func injection.

## Related code files

| Path | Action |
|---|---|
| `adapters/common/devconfig/devconfig.go:457-519` | rewire resolution to env > .env |
| `adapters/common/devconfig/devconfig.go:539-566` | delete `OSSecretLookup` |
| `adapters/common/devconfig/devconfig.go:521-538` | delete `FileSecretLookup` |
| `adapters/common/devconfig/devconfig.go:59,213` | rename `EnvSeedDirect`, `SeedB64` |
| `cli/internal/secret/*` | delete package (or reduce to nothing) |
| `adapters/claude-code/creds.go:47,76,89,97,217,222-226` | rename + drop lookup var |
| `adapters/codex/creds.go:33,48,61,68,92` | same |
| `client/client.go:37,43,90` | rename `SeedB64` |
| `adapters/common/git/attestation.go:103-128`, `attesthook.go:11,77` | rename |
| `actions/openbox-git-action/.../main.go:107` | `OPENBOX_SEED` → new name + alias |
| `cli/cmd/openbox/{main.go:51,68,207,219,444; attest.go:22-29; approve.go:111}` | drop `openStore`, pass nothing |
| `cli/internal/devinit/devinit.go:106,163,198-208,279-287,343` | delete `accounts()` (a method on `Options`, not a free function), rewrite store writes |
| `cli/cmd/openbox/init.go` | **not owned here** — phase 7 owns every `init` surface change except deleting `--secret-backend` |

## Implementation Steps

1. **Rename first, behaviour unchanged.** Mechanical `SeedB64` → `PrivateKeyB64`
   and `EnvSeedDirect` → `EnvAgentPrivateKey` across all modules, keeping
   `OPENBOX_ED25519_SEED` as the value. Build all 11 modules. Commit alone — a
   pure rename that compiles is a safe checkpoint.
2. Change the variable's value to `OPENBOX_AGENT_PRIVATE_KEY` and add the two
   deprecated aliases with a once-only stderr warning. Test all three names.
3. Add `.env` as the secret source in `ResolveCredentials` (env > .env), leaving
   coordinate resolution untouched. Test the full precedence matrix, incl. a `.env`
   value with CRLF already stripped, **and** a case pinning that a DID in `.env` is
   ignored — that assertion is what keeps the second-store bug from creeping back.
4. Point both adapter `secretLookup` vars at the new resolution; delete the
   `osSecretLookup` wrappers.
5. Delete the keychain: `backends.go`, `OSSecretLookup`, `FileSecretLookup`,
   `SecretLookup`, `secret.Store`/`Detect`/`Open`, `fileStore`, `openStore`.
6. Rewrite `devinit` credential handling: no `accounts()`, write via
   `WriteEnvFile`; keep the once-only-credential and HALT-on-4xx semantics
   (`devinit.go:~256,269`) intact. The registration path itself stays put here —
   phase 7 is what moves ownership of it to `auth`.
7. Approver: point `approve.go` at `.env` for the control token.
8. Remove `--secret-backend`; make it a hard error naming `openbox auth`.
9. `go build ./... && go vet ./... && go test -race ./...` for every module.

## Todo list

- [x] Step 1 rename lands green as its own commit
- [x] `OPENBOX_AGENT_PRIVATE_KEY` primary; both old names read with one warning
- [x] Secrets: env > `.env`, matrix-tested; coordinates provably unchanged
- [x] A DID in `.env` is ignored, asserted
- [x] Both adapters rewired; wrappers deleted
- [x] Keychain + namespacing + `--secret-backend` deleted
- [x] Missing-credential error names the keychain-read escape hatch (D1)
- [x] `devinit` writes `.env`; HALT-on-4xx and once-only semantics preserved
- [x] Approver control token read from `.env`; `openbox approve` still works
- [x] All 11 modules build, vet, `-race` green

## Success Criteria

- A `.env` holding the two secrets, **plus** a `dev.json` holding the coordinates,
  is sufficient for a hook to sign and deliver an event.
- A `.env` **alone** is not sufficient, and fails with the no-DID error rather than
  something obscure — the inverse of what an earlier draft of this plan claimed,
  and the assertion that proves `.env` is not a second coordinate store.
- A DID set in `.env` is ignored; the same DID set as a real env var is honoured.
- Setting `OPENBOX_AGENT_PRIVATE_KEY` as a real env var overrides the file.
- Setting only the deprecated `OPENBOX_ED25519_SEED` still works and warns once.
- `grep -rn "keychain\|secret-tool\|security find-generic-password" --include=*.go`
returns nothing outside tests, that decision, and the one error string that
tells a stranded user how to read their own keychain.
- `openbox approve list` works with no keychain present.
- `openbox init --secret-backend file` fails with a message naming `openbox auth`.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Rename + behaviour change in one commit ⇒ unbisectable breakage | M×H | a module fails to build and the cause is ambiguous | **Adjust:** step 1 is rename-only and must be its own green commit. Do not merge the steps. |
| Approver forgotten ⇒ `openbox approve` breaks | M×H | `approve list` errors on a machine with no keychain | **Adjust:** step 7 is not optional; add an `approve` smoke test to the phase gate. |
| `.env` quietly becomes a coordinate source too (habit, or a "helpful" fallback) | M×H | the DID-in-`.env`-is-ignored test is deleted or relaxed | **Stop:** that test is the tripwire for the second-store bug this plan claims to have avoided. Adding coordinates to `.env` is a D2 reversal and needs the decision reopened, not a commit. |
| Deleting `SecretLookup` removes the seam tests relied on | H×L | many `_test.go` files fail to compile | **Accepted, planned:** migrate those tests to `OPENBOX_HOME` + `t.Setenv`. Budgeted inside this phase's 5h. |
| `devinit`'s HALT-on-4xx / once-only-credential semantics regress | M×H | existing devinit tests fail | **Stop and replan:** those are safety behaviours. Never ship a green build with changed `init` semantics. |
| A 3rd-party consumer depends on `OPENBOX_ED25519_SEED` | L×M | external report after release | **Accepted, mitigated:** aliases kept indefinitely; removal needs its own decision record. |
| `actions/openbox-git-action` is a separate release artifact | M×M | CI action breaks while the CLI is green | **Adjust:** include the action in the all-modules gate; it is easy to forget because it is not in `go.work`'s main flow. |

## Security Considerations

- Deleting the OS-store path removes the product's only at-rest protection; the
  compensating controls are `0600` (unix), the header comment, and honest docs
  from phase 1. Nothing here should re-add a fallback that silently writes
  credentials somewhere else.
- The deprecation warning goes to **stderr only**. A hook writing to stdout can
  inject context into the coding agent (INV-3) — a warning must never do that.
- Error messages on missing credentials must name the file path but never echo a
  value, and must not include the account/coordinate strings that no longer exist.
- Verify no deleted code path leaves a stale `secrets.json` readable with
  credentials still in it; phase 8's docs tell users where to delete it.

## Next steps

Phase 5 wires `auth` to write what this phase reads.
