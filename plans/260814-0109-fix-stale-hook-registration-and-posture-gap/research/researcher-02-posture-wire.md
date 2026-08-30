# Posture wire gap — research (Issue 2)

## Q1 — Posture facts (`adapters/common/devconfig/posture.go`)

Struct (60-125): booleans `Enforce,FailClosed,Tier2,SecretDetection,ContentCapture,Findings,Finops,RealtimeFlush` (62-71); adapter-supplied `Adapter,AdapterVersion,ProviderVersion,BundleVersion,BundlePolicyID,BundleSHA256,Staleness Staleness,BundleIntegrity` (75-87); `DecisionAuthority string` (109), `FailurePolicy string` (114); `ConfigSource map[string]string` (120); `ProviderManaged string` (124).

Set in `EffectivePosture()` (130-159): booleans resolved via `postureFields()` loop (133-137); `warnDeprecatedKeys()` called (147); then unconditionally:
```
p.DecisionAuthority = DecisionAuthorityControlPlane   // "control_plane" (153,164)
p.FailurePolicy = FailurePolicyFailOpen               // "fail_open" (154,165)
if p.FailClosed { p.FailurePolicy = FailurePolicyFailClosed }  // "fail_closed" (155-156,166)
```
`Flags()` (177-184): one bool per `postureFields()` entry — `enforce,fail_closed,tier2,secret_detection,content_capture,findings,finops,realtime_flush` (202-230). `require_verified_bundle` deliberately absent (214-217, "reporting it would be reporting a control that cannot engage").

`Metadata()` (242-271): starts from `Flags()` (244-246, so the 8 booleans incl. `fail_closed` are already on the wire), then a string map (247-256) — `adapter,adapter_version,provider_version,bundle_version,bundle_policy_id,bundle_sha256,staleness,bundle_integrity,provider_managed` — each dropped if `""` or secret-shaped (258-259). **`decision_authority` and `failure_policy` are never added anywhere in `Metadata()`** — confirmed bug.

## Q2 — Wire trace

`ev.Metadata["posture"] = m.Posture.Metadata()` fires **only** in `case HookSessionStart:` — `adapters/claude-code/mapper.go:182`, `adapters/codex/mapper.go:173`. Confirmed SessionStarted-exclusive by both adapters' `TestPosture_OnSessionStartOnly` (`posture_test.go:12`, both adapters) and `TestPosture_AbsentWhenNotSupplied` (:44, asserts `Metadata["posture"]` absent on other hooks, :36/:47).

`client.DevEvent.Metadata map[string]any` (`client/event.go:332-334`, generic per-event-type additive keys) → `buildMetadata(ev)` (`client/payload.go:522-524`, merges `ev.Metadata` + finops keys) → `governanceEventPayload.Metadata json.RawMessage \`json:"metadata,omitempty"\`` (`client/payload.go:77`), assigned at `payload.go:198-202`. Wire field name is literally `"metadata"`; `posture` is a nested key inside it.

`Metadata` (the payload field) is generic and rides on every event type; the **`posture` sub-key** is exclusive to `SessionStarted` because only the `HookSessionStart` branch writes it — no other event carries `decision_authority`/`failure_policy`/bundle_* regardless of the fix.

## Q3 — Safe-to-delete analysis

**No production writer sets the 5 target fields, ever.** `adapters/claude-code/posture.go:25-32` and `adapters/codex/posture.go:25-32` (`effectivePosture()`) call `devconfig.EffectivePosture()` then set only `Adapter,AdapterVersion,ProviderVersion,ProviderManaged` — never `BundleVersion/BundlePolicyID/BundleSHA256/BundleIntegrity/Staleness`. `EffectivePosture()` itself never sets them either. So in every real session they are Go zero-values, and `Metadata()`'s `if v==""` guard (258) drops all 5 today unconditionally — this matches the issue's claim exactly, confirmed structurally not just observationally.

**Test-only dependents (would need updating, all `_test.go`):**
- `adapters/common/devconfig/posture_test.go:76-96` `TestPostureMetadata_UnknownStringsOmitted` — constructs `Posture{BundleVersion,BundlePolicyID,BundleSHA256,Staleness:...}`, asserts 4 of the 5 keys (`bundle_version,bundle_policy_id,bundle_sha256,staleness` — NOT `bundle_integrity`) omit-when-empty and render when set.
- `:104-116` `TestPostureMetadata_NoSecretShapedValues` — same 4 fields with secret-shaped values, asserts redaction.
- `adapters/claude-code/posture_test.go:14,21,25` and `adapters/codex/posture_test.go:14,21,25` `TestPosture_OnSessionStartOnly` — sets `Staleness: StalenessFresh`, asserts `got["staleness"]`.
- Both adapters' `TestPosture_StalenessNamesTheSkipReason` (:55-71) — tests the `Staleness` enum's 7 consts are distinct/non-empty; independent of `Metadata()`.
- **Gap found**: `TestPostureReportsDecisionProvenance` (`devconfig/posture_test.go:198-231`) asserts `p.DecisionAuthority`/`p.FailurePolicy` **on the struct only** — never calls `.Metadata()`, never checks the map. No existing test asserts the wire map contains these two keys. This is exactly how the gap shipped: struct-population and map-serialization were tested in isolation, never together.

**Precedent already in the codebase**: `require_verified_bundle` was already fully deleted from `postureFields()`/`Flags()` (comment 214-217) — not merely left empty — and `TestEffectivePosture_MatchesResolvers` (:166) asserts its **absence** from `Flags()`. That's the pattern the fix should follow for the 5 dead keys (absent, not empty).

**Deprecated-key warning path — confirmed disjoint, no overlap.** `warnDeprecatedKeys()`/`deadKeysPresent()` (`devconfig.go:466-497`) check only `DevConfig.Tier2` (json `tier2`, :167), `.Tier2TimeoutMS` (json `tier2_timeout_ms`, :173), `.RequireVerifiedBundle` (json `require_verified_bundle`, :187) via env/config presence — these are **config-file/env-var** fields on a different struct (`DevConfig`), never touching `devconfig.Posture`'s `BundleVersion/BundlePolicyID/BundleSHA256/BundleIntegrity/Staleness` (adapter-supplied, no `Env*` const, no json tag, never read from config at all). Deleting the Posture fields/Metadata keys does **not** touch `warnDeprecatedKeys`, `ResolveTier2`, or the "deprecated keys stay parseable" contract.

**Naming collision to flag — do not touch.** `adapters/common/git/attestation.go:82-83,99-100,145-146` and `attesthook.go:13-14,73-74` define their **own** unrelated `BundlePolicyID string`/`BundleSHA256 string` fields for the commit-lineage attestation envelope (`canonical_b64`), asserted by `testbed/50-lineage.sh:96` (`assert_contains ... "bundle_sha256"`) and documented at `docs/testbed/e2e.md:247`. Same field names, completely different struct/feature (commit→deploy lineage, still shipping). A global find-replace on these names would break lineage attestation.

## Q4 — Contract impact

`contracts/dev-event/schema/dev-event.schema.json:96-99` — `metadata` is `{"type":"object"}`, generically described, no sub-schema for SessionStarted (its `oneOf` branch at 103-107 requires only `event_type`; contrast `CommitCreated`/`Deploy` at 130-163 which DO pin required metadata sub-keys). Top-level `additionalProperties:false` (101) governs `DevEvent`'s own properties, not keys inside `metadata`.

Golden fixture `contracts/dev-event/conformance/testdata/valid/session_started.json` metadata = `{provider,tool_version,repo,cwd}` — **no `posture` key at all**. Confirms posture's inner shape is not pinned by any fixture or schema today.

Conclusion: adding `decision_authority`/`failure_policy` or removing the 5 dead keys inside `metadata.posture` changes **no pinned bytes** and needs **no schema_version bump** — current v1.2 (COVERAGE.md:58 states bumps are for schema-level additions; this change is inside an already-unconstrained object). New coverage belongs in `devconfig`/adapter unit tests (extend `TestPostureReportsDecisionProvenance` to also assert `.Metadata()`), not in `contracts/dev-event`.

## Q5 — Doc surface

**that decision** `:222-252`, section "## Policy provenance as evidence". Exact promise (237-239):
> "Posture therefore carries **who decides** (`decision_authority: control_plane`) and **what happens when they cannot be reached** (`failure_policy: fail_open | fail_closed`)."

Same section (224-227) is that decision's own justification for the bundle fields' removal from posture's *reported* surface ("Deleting the bundle removes what posture used to report.. version, id, content hash, signature-verification outcome.. the question has changed rather than gone unanswered") — i.e., that decision text already treats `bundle_*`/`staleness` as gone from reporting; code just never followed through in
`Metadata()`.

Other docs: `docs/architecture.md:128` and `docs/data-and-privacy.md:99` mention "posture" generically (reports "effective posture" / "which state was in effect") — no key-level promises, nothing to reconcile beyond staying generally accurate. `contracts/dev-event/MAPPING.md` — zero mentions of posture/bundle_*/staleness/decision_authority/failure_policy; not a blocker, just an existing documentation gap (out of requested scope). `testbed/50-lineage.sh` / `docs/testbed/e2e.md:247` `bundle_policy_id`/`bundle_sha256` mentions are the attestation feature (see Q3), unrelated.

**Redundancy question** (fail_closed bool vs failure_policy string): `fail_closed` bool is already on the wire via `Flags()`→`Metadata()` (posture.go:204-205, 244-246). `FailurePolicy` is derived 1:1 from it (154-157: `if p.FailClosed { ... FailClosed } else FailOpen`) — same bit, two encodings, genuinely redundant as *information*. That decision (237-239, 243-248) frames `failure_policy` as the deliberate paired vocabulary term beside `decision_authority` (a human/`doctor`-readable enum-string pair, "who decides" + "what happens when unreachable"), not as filling an information gap `fail_closed` left open. Read as intentional design, not oversight — worth noting, not
blocking.

## Constraints for the fix

1. Add `"decision_authority": p.DecisionAuthority` and `"failure_policy": p.FailurePolicy` to `Metadata()`'s string map (posture.go:247-256) — both always non-empty post-`EffectivePosture()` (153-156), no `if v==""` risk.
2. Delete `bundle_version,bundle_policy_id,bundle_sha256,staleness,bundle_integrity` entries from `Metadata()`'s map (251-255). If also deleting the struct fields (`BundleVersion,BundlePolicyID,BundleSHA256,BundleIntegrity,Staleness Staleness`, posture.go:78-81,87) and the `Staleness` type+consts (32-55): update the 6 test call-sites listed in Q3 (construct via other means or drop the assertions) — do **not** touch `adapters/common/git/attestation.go`/`attesthook.go`'s same-named-but-different `BundlePolicyID`/`BundleSHA256`.
3. No `contracts/dev-event` fixture/schema change required; no schema_version bump.
4. `warnDeprecatedKeys`/`ResolveTier2`/`deadKeysPresent` (devconfig.go) are untouched by this change — confirmed disjoint.
5. Extend `TestPostureReportsDecisionProvenance` (or add a sibling) to assert `.Metadata()` contains `decision_authority`/`failure_policy` and omits `bundle_*`/`staleness` — closes the exact coverage gap that let this ship.
6. `cli/cmd/openbox/doctor.go:101-102` already reads `p.DecisionAuthority`/`p.FailurePolicy` off the struct directly (not via `Metadata()`) — unaffected either way.

## Unresolved questions

- Whether to delete the `Staleness` type/consts entirely or keep them dormant for a future re-introduction (e.g. if a control-plane-driven freshness signal returns) — a scope call, not a code-safety one; nothing currently reads them outside tests.
- `bundle_integrity` isn't covered by `TestPostureMetadata_UnknownStringsOmitted`'s explicit key list (only 4 of 5 named) — pre-existing minor test gap, orthogonal to this fix.
