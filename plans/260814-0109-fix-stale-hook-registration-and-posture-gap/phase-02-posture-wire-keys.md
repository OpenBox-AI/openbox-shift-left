# Phase 02 — Posture wire keys + dead bundle-key deletion

## Context links

- Plan: [plan.md](plan.md)
- Issue §2: `plans/reports/issue-260814-0106-stale-hook-registration-and-posture-gap.md:88-136`
- Research: `research/researcher-02-posture-wire.md`
- The claim being honoured: `:222-252` (promise at 237-239)

## Overview

- **Date:** 2026-08-14
- **Description:** Add `decision_authority` and `failure_policy` to `Posture.Metadata()`; delete
  the five bundle-era keys from `Metadata()` and their fields from `Posture`, plus the
  `Staleness` type and consts nothing outside tests reads.
- **Priority:** P2 (low-moderate — a governance product describing evidence it does not send)
- **Implementation status:** complete
- **Review status:** reviewed

## Key Insights

- `Metadata()` (`posture.go:242-271`) is the only path onto the wire, and its string map
  (`:247-256`) lists neither field. The struct fields exist (`:109`, `:114`) and are set on every
  session (`:151-157`); `openbox doctor` prints them off the struct
  (`cli/cmd/openbox/doctor.go:101-102`). Local view complete, remote view not.
- How the gap shipped: `TestPostureReportsDecisionProvenance`
  (`devconfig/posture_test.go:198-231`) asserts the two fields **on the struct** and never calls
  `.Metadata()`. Population and serialization were each tested in isolation. The new assertion
  must cross that seam.
- The five keys are structurally dead, not just empirically: `effectivePosture()` in both
  adapters (`adapters/claude-code/posture.go:25-32`, `adapters/codex/posture.go:25-32`) sets only
  `Adapter/AdapterVersion/ProviderVersion/ProviderManaged`, and `EffectivePosture()` never sets
  them, so the `if v == ""` guard (`posture.go:258-259`) already drops all five in every real
  session. The live `SessionStarted` that *did* carry `bundle_sha256` came from the stale engine
  (Phase 01's bug) — which is the evidence they are live code paths for a deleted subsystem.
- Precedent for full deletion over dormancy: `require_verified_bundle` was removed from
  `postureFields()`/`Flags()` and its **absence** is asserted (`posture.go:214-217`,
  `posture_test.go:166-169`). D4 follows it.
- **Naming collision — do not touch:** `adapters/common/git/attestation.go:82-83,99-100,145-146`
  and `attesthook.go:13-14,73-74` define their own `BundlePolicyID`/`BundleSHA256` for the
  commit-lineage attestation envelope, asserted by `testbed/50-lineage.sh:96` and documented at
  `docs/testbed/e2e.md:247`. Different struct, shipping feature. A repo-wide rename breaks it.
- Disjoint from the deprecated-key contract: `warnDeprecatedKeys`/`deadKeysPresent`
  (`devconfig.go:466-497`) read `DevConfig.Tier2`/`Tier2TimeoutMS`/`RequireVerifiedBundle` —
  config/env fields on a different struct. Untouched here.
- No contract change: `contracts/dev-event/schema/dev-event.schema.json:96-99` types `metadata`
  as a bare object with no SessionStarted sub-schema, and
  `contracts/dev-event/conformance/testdata/valid/session_started.json` carries no `posture` key
  at all. No pinned bytes move, no `schema_version` bump (`COVERAGE.md:58`).

## Requirements

1. `Metadata()` includes `decision_authority` and `failure_policy` for any posture produced by
   `EffectivePosture()` (both are always non-empty there, `posture.go:153-156`).
2. `Metadata()` no longer emits `bundle_version`, `bundle_policy_id`, `bundle_sha256`,
   `staleness`, `bundle_integrity` under any input.
3. `Posture` no longer declares `BundleVersion`, `BundlePolicyID`, `BundleSHA256`,
   `BundleIntegrity`, `Staleness`; the `Staleness` type and its 7 consts are deleted (D4).
4. One test asserts the emitted posture contains `decision_authority` (and `failure_policy`) and
   no `bundle_*`/`staleness` key — crossing the struct→map seam that let the gap ship.
5. `Flags()`'s 8 booleans, `config_source`, `looksLikeSecret`/`truncate` handling, and the
   `require_verified_bundle` absence assertion all keep their current behavior.
6. `adapters/common/git`'s same-named fields are not touched.

## Architecture

Wire path (unchanged): `Posture.Metadata()` → `ev.Metadata["posture"]`, written **only** in
`case HookSessionStart` (`adapters/claude-code/mapper.go:182`, `adapters/codex/mapper.go:173`) →
`client.DevEvent.Metadata` (`client/event.go:332-334`) → `buildMetadata`
(`client/payload.go:522-524`) → payload `metadata` (`client/payload.go:77`, assigned `:198-202`).
So the two new keys ride SessionStarted only, which is correct: they are session-scoped facts.

Change is confined to the string map at `posture.go:247-256` plus the struct/type deletions.
Put the new keys in the **same** map so they inherit the existing `looksLikeSecret` + `truncate`
guards (DRY); their values are internal constants (`posture.go:163-167`) so neither guard fires.

Blast radius = 3 modules, all `_test.go` except `posture.go`:

| Module | File | Action |
|---|---|---|
| `adapters/common/devconfig` | `posture.go` | delete 5 fields + type + consts; edit map |
| | `posture_test.go` | update 2 tests, add 1 |
| `adapters/claude-code` | `posture_test.go` | update `TestPosture_OnSessionStartOnly`, delete `TestPosture_StalenessNamesTheSkipReason` |
| `adapters/codex` | `posture_test.go` | same two edits |

## Related code files

| Path | Lines | What |
|---|---|---|
| `adapters/common/devconfig/posture.go` | 28-55 | `Staleness` type + 7 consts — delete |
| | 78-87 | the 5 struct fields — delete |
| | 109, 114 | `DecisionAuthority`, `FailurePolicy` — keep, now reported |
| | 151-157 | where both are set (always non-empty) |
| | 163-167 | the const vocabulary to reference in the map |
| | 242-271 | `Metadata()` — the edit; string map at 247-256 |
| | 204-217 | `Flags()` + the `require_verified_bundle` deletion precedent |
| `adapters/common/devconfig/posture_test.go` | 76-96 | `TestPostureMetadata_UnknownStringsOmitted` — drop 4 keys from the list (80), rebuild the `full` posture (89-90), replace the staleness assertion (92-93) |
| | 104-116 | `TestPostureMetadata_NoSecretShapedValues` — drop the 4 bundle/staleness hostile inputs (111-114); keep `Adapter`/`AdapterVersion`/`ProviderVersion` |
| | 166-169 | absence-assertion pattern to imitate |
| | 198-231 | `TestPostureReportsDecisionProvenance` — extend or add a sibling |
| `adapters/claude-code/posture_test.go` | 14, 25 | `Staleness: StalenessFresh` + `got["staleness"]` |
| | 55-71 | enum-only test — delete with the type |
| `adapters/codex/posture_test.go` | 14, 25, 55-71 | identical |
| `adapters/claude-code/conformance_parity_test.go` | 350 | prose in a `note:` string — no compile dep; reword only if it becomes misleading |
| `adapters/claude-code/enforce_conformance_test.go` | 206 | comment prose, same |
| `adapters/common/git/attestation.go`, `attesthook.go` | 82-83, 99-100, 145-146 / 13-14, 73-74 | **do not touch** |

## Implementation Steps

1. `posture.go`: delete the `Staleness` type + 7 consts (`:28-55`) and the five struct fields
   (`:78-87`). Keep the surrounding doc comment's honest framing of what posture may carry.
2. Replace the five map entries (`:251-255`) with
`"decision_authority": p.DecisionAuthority, "failure_policy": p.FailurePolicy`. Add a short
comment: reported because that decision makes them posture's policy-provenance evidence, and
the bundle coordinates they replaced are gone rather than empty.
3. `devconfig/posture_test.go`: trim the omitted-key list (`:80`) to the three adapter strings;
   rebuild `full` (`:89-90`) from `Adapter/AdapterVersion/ProviderVersion` and assert
   `provider_version` only.
4. Same file: strip the four hostile bundle/staleness inputs from
   `TestPostureMetadata_NoSecretShapedValues` (`:111-114`); the secret-shaped assertions on the
   surviving three fields carry the invariant.
5. Add the seam test inside `TestPostureReportsDecisionProvenance` (`:198`) as a subtest — e.g.
   "the emitted posture carries both" — using `isolateConfig(t)` + `EffectivePosture().Metadata()`
   (NOT `Posture{}`, whose empty strings are dropped by the guard) and asserting: both keys
   present and equal to the consts; no key with prefix `bundle_`; no `staleness` key. Name it for
   the behavior, not the issue.
6. `adapters/claude-code/posture_test.go` + `adapters/codex/posture_test.go`: drop the
   `Staleness` field from the constructed posture (`:14`), change the `:25` assertion to
   `got["decision_authority"]`/`got["adapter"]`, delete
   `TestPosture_StalenessNamesTheSkipReason` (`:55-71`).
7. `go test -race ./...` in all three modules; both cross-compiles.
8. Confirm no drift into `adapters/common/git`: `git diff --stat adapters/common/git` empty.

## Todo list

- [x] 5 fields + `Staleness` type/consts deleted from `posture.go`
- [x] map emits `decision_authority` + `failure_policy`, no bundle keys
- [x] `TestPostureMetadata_UnknownStringsOmitted` updated
- [x] `TestPostureMetadata_NoSecretShapedValues` updated
- [x] seam subtest asserting map contents added
- [x] both adapters' `posture_test.go` updated, enum-only tests deleted
- [x] `go test -race ./...` green in devconfig, claude-code, codex
- [x] both cross-compiles green
- [x] `adapters/common/git` untouched (git diff proof)

## Success Criteria

- `EffectivePosture().Metadata()` contains `decision_authority == "control_plane"` and
  `failure_policy` ∈ {`fail_open`,`fail_closed`}, tracking `fail_closed`.
- No key beginning `bundle_` and no `staleness` key can be produced by any `Posture` value —
  enforced by absence assertions, not by an empty-value guard.
- `grep -rn "BundleVersion\|BundleIntegrity\|Staleness" adapters/common/devconfig adapters/claude-code adapters/codex` returns only prose in comments/notes.
- Three modules green under `-race`; both cross-compiles green.
- that decision:237-239 becomes a true statement about shipped behavior with no decision record edit (D2).

## Risk Assessment

| Risk | L×I | Mitigation |
|---|---|---|
| R1 Global rename catches `adapters/common/git`'s identically-named attestation fields ⇒ breaks lineage | Low×High | Delete by explicit line edit in `posture.go` only; no sed/rename. Verify with `git diff --stat adapters/common/git` (must be empty). `testbed/50-lineage.sh:96` is the end-to-end tripwire when a stack exists. |
| R2 Core rejects or mis-stores a SessionStarted whose posture lost 5 keys / gained 2 | Low×Med | `metadata` is an unconstrained object in the schema (`dev-event.schema.json:96-99`) and the golden fixture carries no `posture` key, so nothing is pinned. All five keys were already absent in every real session (structural, see Key Insights) — the *removal* is a no-op on the wire; only the two additions are new bytes. |
| R3 Deleting `Staleness` loses a vocabulary a future freshness signal would want | Low×Low | Git holds it; `require_verified_bundle` set the precedent that a dead control must not linger as reportable surface. Reintroduction would be a new decision with a new shape. |
| R4 (assumption) Nothing outside tests reads the five fields | Low×High | Verified by repo-wide grep excluding `adapters/common/git` — only `_test.go` hits plus two prose comments. **Signal it broke:** compile failure in a module outside the three listed, or CI's cross-compile failing on an unexpected package. **Pre-decided response:** adjust in-plan — if a real reader exists, keep that field and its key, delete the rest, and record why in the phase file. |
| R5 (assumption) `decision_authority` reaching `governance_events` needs no backend change | Med×Low | `metadata` is stored generically; core already stores unknown metadata keys (the stale engine's `bundle_sha256` was stored, which is the observed proof). **Signal:** `testbed/30-enforce.sh:185` still fails after this ships with a live stack. **Pre-decided response:** stop-and-replan into a backend ask; do not widen the client. |

## Security Considerations

- Both new values are internal constants, never user or provider input, so the
  `looksLikeSecret`/`truncate` guards (`posture.go:258-262`) are belt-and-braces rather than
  load-bearing — but they stay, because they are what makes the map safe for the adapter-supplied
  strings sharing it (INV-1, `TestPostureMetadata_NoSecretShapedValues`).
- Deleting the bundle keys removes a reporting surface that could only ever have overstated:
"a control that cannot engage must not appear as one" (that
decision:224-227).
- `failure_policy: fail_open` is an honest admission that enforcement is reachability-dependent
  (`docs/architecture.md#assurance--what-the-evidence-proves`); reporting it does not weaken
  anything, it names an existing limit to the control plane.

## Next steps

Phase 04 verifies that decision:222-252 needs no amendment under D2, checks the generic
posture sentences at `docs/architecture.md:128` and `docs/data-and-privacy.md:99`, and
records that `testbed/30-enforce.sh:185-187` is the dormant end-to-end proof for this
phase.
