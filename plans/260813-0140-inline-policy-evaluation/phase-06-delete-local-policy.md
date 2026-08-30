# Phase 06 — Delete the local policy path

## Context links

- Parent: [plan.md](plan.md) · Depends on: phases 4 and 5
- Blocks: phase 7 · Authorized by
- **Largest deletion in the repo's history.** Nothing here changes behaviour — phase 3
  already made this code unreachable. If a deletion changes behaviour, something was still
  wired and that is a finding, not a merge conflict.

## Overview

- **Date:** 2026-08-13
- **Description:** Remove the local policy evaluator, the bundle it consumed, the sync that
  fetched it, the staleness gate that policed it, and the signing/pinning machinery around
  it. Keep local secret detection.
- **Priority:** P1 · **Implementation status:** done · **Review status:** pending

## Key Insights

- **`decision/secrets.go` survives, and this is the single most important line in the phase.**
  Secret detection is content protection, not policy evaluation: it redacts a Write/Edit body
  locally so secrets never leave, and it scans the **whole** body where the server sees at
  most 64KB. Deleting it with its neighbours would silently remove the one in-transit control
  the plan preserves — and would break phase 4's ordering guarantee.
- Check what `secrets.go` depends on before deleting siblings. It likely needs shared input
  types; keep the minimum, delete the rest, and do not leave the evaluator alive "because it
  compiles together".
- **`KeepTighter` loses its purpose here.** It existed to stop a server verdict loosening a
  local policy verdict. With no local policy verdict there is nothing to compare — the local
  side carries only redaction, which phase 4 carries forward explicitly. Delete it in this
  phase, not phase 3.
- **The staleness gate goes with the bundle.** `StaleGateDecision` (`gate.go:98-104`)
  synthesizes a HALT when the local bundle is stale, which is a concept that stops existing:
  there is no local bundle to be stale. Its fail-closed behaviour is replaced by the ordinary
  failure policy on an unreachable core.
- **`require_verified_bundle` and `org_signing_key_id`/`org_signing_pubkey` become dead
  config.** Removing them is a user-visible config break: parse-and-ignore with a once-only
  stderr warning for one release, then remove. Same treatment as `tier2*` from phase 3.
- **that decision becomes historical.** It is not wrong, and it should not be deleted or edited
phase 7 marks it superseded-in-part with a.
- The `decision` module may end up small enough to fold into `hookflow`. **Do not** — merging
modules is a separate decision (that decision governs the layout) and it would hide
this deletion inside a refactor.

## Requirements

1. Delete from `decision/`: the evaluator, builder, bundle, signature, regoparity, protocol
   and in-process server surfaces, with their tests. **Keep `secrets.go`** and the minimum it
   needs to compile and be tested.
2. Delete `cli/internal/policysync/` and the `openbox dev sync` command, including its
   dispatch, usage line and help text.
3. Delete `adapters/common/hookflow/staleness.go` and `StaleGateDecision`'s call site.
4. Delete `KeepTighter` and the local-policy verdict path in `gate.go`.
5. Config: `require_verified_bundle`, `org_signing_key_id`, `org_signing_pubkey`, `tier2`,
`tier2_timeout_ms`, and the bundle-path resolution — parse-and-ignore with a once-only
**stderr** warning naming. Remove the resolvers.
6. Delete bundle-path resolution from `hookflow` and any epoch-pin/stale-marker files it
   wrote; document in phase 7 that leftover files on disk are inert and can be removed.
7. No behaviour change. Any test that fails is either testing deleted behaviour (delete it
   deliberately, noting what it covered) or revealing that something was still wired
   (**stop** and investigate).
8. All 11 modules build, vet, `-race` green.

## Architecture

Before: local bundle → local evaluator → verdict, optionally escalated for high-risk.
After: `/evaluate` → verdict. Local code retains exactly one decision-adjacent
responsibility — secret redaction — and it runs before the call.

## Related code files

| Path | Action |
|---|---|
| `decision/{evaluator,builder,bundle,signature,regoparity,protocol,inprocess,server}.go` | delete with tests |
| `decision/secrets.go` (+ minimal shared types) | **keep** |
| `cli/internal/policysync/*` | delete |
| `cli/cmd/openbox/main.go` (`dev sync`, usage, bundle flags) | delete the command and its help |
| `adapters/common/hookflow/staleness.go` | delete |
| `adapters/common/hookflow/gate.go:98-104` | delete the stale gate call |
| `adapters/common/hookflow/evaluate.go` | delete `KeepTighter` |
| `adapters/common/devconfig/devconfig.go` | deprecate then remove the bundle/signing/tier2 fields and resolvers |
| `cli/cmd/openbox/doctor.go` | bundle block already replaced in phase 5 |
| — | untouched here; phase 7 marks it superseded-in-part |

## Implementation Steps

1. Map `secrets.go`'s dependencies first. Write down exactly which files must survive.
2. Delete in dependency order, smallest blast radius first: `policysync` and `dev sync`, then
   staleness, then `KeepTighter`, then the evaluator cluster.
3. After each deletion: build all 11 modules. A deletion that needs a behaviour edit to
   compile means something was still wired — stop and investigate.
4. Deprecate the config fields (parse, ignore, warn once to stderr).
5. Delete the resolvers and the bundle-path helpers.
6. Sweep the test suite: for each removed test, record in the commit message what behaviour it
   covered, so a reviewer can see nothing live lost coverage.
7. Full matrix: build, vet, `-race`, all 11 modules.

## Todo list

- [x] `secrets.go` dependency map written before any deletion
- [x] `policysync` + `dev sync` deleted, incl. usage/help
- [x] `staleness.go` and its gate call deleted
- [x] `KeepTighter` and the local-verdict path deleted
- [x] Evaluator cluster deleted; `secrets.go` and its tests still green
- [x] Bundle/signing/tier2 config parsed-but-ignored with one stderr warning
- [x] Every removed test accounted for in a commit message
- [x] All 11 modules build, vet, `-race` green

## Success Criteria

- `grep -rn "rego\|bundle" --include=*.go` returns nothing outside decision record history, the
  deprecated config parse, and comments explaining the deprecation.
- Local secret redaction still applies with core unreachable — the same assertion as phase 4,
  re-run after the deletion.
- No behaviour test changed its expectations in this phase.
- Non-test LOC deleted from `decision/` + `policysync` ≥ 2,000.
- `openbox dev sync` reports that it no longer exists — it does not
  silently vanish for anyone with it in a script.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| `secrets.go` deleted or broken with its neighbours | M×H | the secret-never-egresses test fails or disappears | **Stop.** Step 1 exists to prevent this; restore before continuing. |
| A deletion silently changes behaviour | M×H | a behaviour test needs editing to pass | **Stop and investigate.** Phase 3 should have made this code unreachable; if not, something is still wired. |
| Tests deleted wholesale to get green | M×H | large test-file deletions with no explanation | **Adjust:** each removal is named in a commit message. Untracked coverage loss is how a deletion becomes a regression. |
| `dev sync` in someone's CI script breaks | M×M | external pipeline fails after upgrade | **Accepted, mitigated:** the command errors with a pointer rather than vanishing; phase 7's release note names it. |
| Deleted config causes a hard failure on an existing dev.json | M×H | hooks fail on upgrade | **Adjust:** parse-and-ignore for one release is non-negotiable; never make an old config file fatal. |
| The `decision` module gets folded into `hookflow` for tidiness | L×M | module layout changes in this phase | **Stop:** That decision governs layout; that is a separate decision. |

## Security Considerations

- The deletion removes the client's ability to decide anything about policy. After it, an
  unreachable control plane means the failure policy decides — verify that path still works
  *after* the deletion, not only before.
- Leftover bundle files, epoch pins and stale markers stay on disk. They are inert, but a
  stale local policy file that looks authoritative is confusing; phase 7 tells users they can
  delete them.
- Do not let the config deprecation swallow an org's `require_verified_bundle: true`
  silently — an org that set it believed it was getting signature enforcement, and the
  warning is how they learn it no longer exists.

## Next steps

Phase 7 renames what remains into three plain-language features and rewrites the docs that
are now untrue.
