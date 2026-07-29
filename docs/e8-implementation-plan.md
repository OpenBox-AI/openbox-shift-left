# E8 Implementation Plan — Assurance & Truth (Enterprise Governance Enhancement)

Date: 2026-07-29
Inputs: `docs/enterprise-agentic-cli-governance-report.md` (external assessment), `docs/governance-report-verification-and-plan.md` (verification + phase sketch), and a source-level exploration of the sibling repos (`openbox-core` @ main `8ea33bc`, `openbox-backend` @ main `9beb0c5`) for the exact reuse seams.
Scope: the ten E8 stories, now specified to implementation level — design, file-level touch points, sibling-repo work, wire impact, tests, and sequencing.

## 0. Ground rules

- **Reuse, don't rebuild** (CLAUDE.md): every story below names the existing table/endpoint/service it reuses. Net-new backend/core surfaces are called out and each carries an ADR.
- **Invariants preserved**: INV-1 (no secret values in config/argv/logs), INV-2 (content egress gated on content-capture; structural fields always safe), INV-3 (observe path fail-open; enforcement never turns on by error), INV-3b (decision module does no network I/O).
- **Sibling-repo work is committed locally, never pushed** until brian asks (established convention; session key lacks push access anyway).
- **OD gates**: OD-E8-1 (content-capture default in managed profile), OD-E8-2 (managed tier vs Cursor priority), OD-E8-3 (enforce posture on unverifiable bundle) must be decided by brian before the stories that depend on them start (marked per story).

## 1. What the sibling exploration changed

Four findings from the openbox-core / openbox-backend exploration revise the earlier sketch:

1. **The dedupe seam shift-left assumed does not exist.** The spool comment "event_id dedupe handles it once EXT-core lands" (`adapters/codex/spool.go:95-96`) is not satisfied by anything in core: there is no `Idempotency-Key` handling, no `event_id` on the wire contract (`GovernanceEventPayload` is a closed struct — unknown top-level keys are **silently discarded**), and the existing event dedupe (`CheckExistingEventActivity`, `internal/services/activities/governance/validation.go:75`) keys on `(agent_id, workflow_id, run_id, activity_id, event_type)` and **bails out entirely for lifecycle events** (empty `activity_id`). E8-S7 must build the dedupe, not just "turn it on". The good news: a natural receipt already exists — `governance_event_id` in `GovernanceVerdictPublicResponse` (`internal/content/governance.go:373`).
2. **Metadata placement is the whole ballgame for E8-S3/S4/S5.** Fields placed in `span.attributes` (map JSONB) or event-level `payload.metadata` pass through core untouched, land in Postgres, are **covered by the Merkle leaf hashes for free** (`attestation/merkle.go:35-56`), and flow to the FE unmodified (`SessionResponseDto.metadata` / `AgentLogResponseDto.metadata` are documented passthroughs). Fields added at span root or body root are silently dropped. So the session-tree and posture stories cost **zero core/backend changes** if we place fields correctly — and we should, because that's also the only placement that gets tamper evidence.
3. **The backend never signs anything today.** No `SignCommand` exists in openbox-backend `src/`; policy serving (`GET /agent/:agentId/policies/current` → `PolicyService.getCurrentPolicy`) returns the raw `PolicyEntity` plus `version_hash` — a SHA-256 fingerprint (`src/common/utils/governance-version-hash.util.ts`), **not** a signature. All KMS keys are per-agent and there is no org-level signing key and no public-key/JWKS endpoint. (Correction from re-verification 2026-07-29: `agent_kms_keys` holds only the **P-256** key — `kms.service.ts:98-108` `ECC_NIST_P256`, SPKI in `agent_kms_keys.public_key`; the AIP **Ed25519** key is KMS-alias-resident with only `agents.did` persisted, `aws-kms-provider.ts:63-75`.) E8-S6 therefore includes a small but genuinely new backend surface (org signing key + public-key route + signed policy response), reusing the existing Ed25519 generate/wrap/KMS-import machinery (`src/modules/did/aws-kms-provider.ts:122-171`, `crypto.ts`).
4. **Verified provenance has a pre-built landing zone.** `deploy_session_links.verified boolean NOT NULL DEFAULT false` already exists (backend migration `1781100000000`, rendered as "inferred" when false), core already writes the rows (`StoreDeploySessionLinksActivity`), and core already knows how to verify an Ed25519 signature by DID-derived KMS alias (`internal/services/agent.go:183`, `did.KMSVerifier`). E8-S10 is mostly plumbing an attestation through the existing Deploy event and flipping a column that's already there.

Also material: `origin/develop` of openbox-backend ships **policy-builder v2** (`PolicyBuilderConfigV1/V2`, commit `1378488`) — any E8 work touching bundle translation must parse v1 **and** v2; and core's dev-runtime branch is fully merged to `main`, so all core work targets `main` directly.

## 2. Stories

### Phase 0 — truth & hygiene (no ADR, no sibling work)

---

#### E8-S1 — Test isolation from ambient agent context (report SL-11) — **size S**

**Problem.** `adapters/common/git/session.go:106-131` reads `CODEX_THREAD_ID` (Tier-0, outranking `OPENBOX_SESSION`). Reproduced: with `CODEX_THREAD_ID` exported, 14 tests fail in the `adapters/common/git` module and 1 in `cli` (`TestUnifiedBinaryGitHookStampsCommit`).

**Design.**
1. Add a shared test helper (per test module — the git module and `cli` are separate Go modules) `clearAmbientSessionEnv(t *testing.T)` that `t.Setenv`s to empty every Tier-0/ambient var the resolver consults: `CODEX_THREAD_ID`, `CLAUDE_SESSION_ID` (if read), `OPENBOX_SESSION`, plus registry-dir override vars. Call it from every test that exercises the zero-value resolver (`registry_test.go`, `integration_test.go`, `cli/cmd/openbox` git-hook tests).
2. For subprocess-spawning integration tests (the CLI git-hook test execs the binary), pass a scrubbed environment explicitly (`cmd.Env = append(os.Environ(), "CODEX_THREAD_ID=")` is insufficient — build the env list without the ambient vars).
3. Add one **regression test** that sets `CODEX_THREAD_ID=ambient-contamination` and asserts the harness-scrubbed path does *not* stamp it — locking the isolation in.
4. Verify no test consults the real user registry: audit for `os.UserConfigDir()`/`HOME` reads in test paths; where found, pin via the existing env overrides (`OPENBOX_CONFIG`, registry dir override).

**Acceptance.** `CODEX_THREAD_ID=x go test ./...` passes in all modules; the new regression test fails if someone removes the scrub.

---

#### E8-S2 — Doc truth (report SL-04, SL-07) — **size S**

**Design.**
1. `README.md:36-40`: rename the "Egress proxy" column to what it is — **"Telemetry base-URL"** (routing of OpenBox's own telemetry, not developer-tool egress control) — and add a one-line footnote that egress control is provider-native (CC sandbox network allowlists / Codex `experimental_network`) + enterprise network controls, recorded not enforced by OpenBox. Correct "native OTel push" phrasing to hooks+spool where it appears.
2. `contracts/dev-event/COVERAGE.md`: refresh §1/§2 for the shipped Codex adapter — SessionEnd hook is real ≥0.145.0, per-session token extraction is real (`adapters/codex/capabilities.go:18,21`); remove the "synthesize SessionEnded" and "tokens absent" rows.
3. `README.md`/`QUICKSTART.md`: one short "assurance" paragraph — the `OpenBox-Session` trailer is an *inferred* claim until E8-S10; enforce-mode assurance depends on managed deployment (E8-S8/S9); link the tier table (E8-S5).
4. Sweep `docs/` for any other claim the verification flagged as stale (the phase framing note in CLAUDE.md already covers historical docs; only fix live-status docs).

**Acceptance.** No live doc claims an egress or provenance control that code doesn't implement; COVERAGE.md agrees with `Capabilities()` for both adapters.

---

### Phase 1 — session-tree identity + posture visibility (adapters/schema only; zero sibling changes)

---

#### E8-S3 — Claude Code correlation + subagent fields (report SL-06 half 1) — **size M**

**Problem.** CC's `HookEvent` (`adapters/claude-code/hookevent.go:52-86`) parses no `tool_use_id`, `agent_id`, `agent_type`; the Codex adapter already carries `tool_use_id` (`adapters/codex/hookevent.go:83-85`) and pairs spans with it. CC docs confirm: PreToolUse/PostToolUse carry `tool_use_id`; SubagentStart/SubagentStop carry `agent_id` + `agent_type`; tool calls inside a subagent also carry `agent_id`.

**Design.**
1. `adapters/claude-code/hookevent.go`: add `ToolUseID string \`json:"tool_use_id,omitempty"\``, `AgentID string \`json:"agent_id,omitempty"\``, `AgentType string \`json:"agent_type,omitempty"\`` to the hook-event struct (structural IDs — INV-2-safe, no content).
2. Mapper: mirror the Codex pattern (`adapters/codex/mapper.go:172,207`) — emit `tool_use_id` into the tool event's metadata **and** use it for deterministic span-id pairing of ToolCall→ToolResult (replacing/augmenting the current heuristic pairing; keep the existing derivation as fallback when the field is absent, e.g. older CC versions). Emit `agent_id`/`agent_type` into event metadata when present.
3. **Wire placement rule (from core exploration):** these go in `span.attributes` / event `metadata` — never new root-level `SpanData` keys (silently dropped by core). No core change needed; fields land in `spans.attributes` JSONB and are Merkle-covered.
4. Handle `SubagentStart`/`SubagentStop` hooks: register them in the CC installer hook config; map to `SignalReceived` lifecycle events (the E7-S5 lifecycle pattern — signals require `workflow_type` too) with `agent_id`/`agent_type`/`parent` metadata, giving the session a reconstructable subagent tree.
5. Update `contracts/dev-event/schema/dev-event.schema.json`: add optional `tool_use_id`, `agent_id`, `agent_type` to the relevant $defs; bump `schema_version` (additive).
6. Conformance: extend the enforcement/observe conformance suites with a fixture asserting a ToolResult pairs to its ToolCall by `tool_use_id` and a subagent tool call carries `agent_id`.

**Acceptance.** A recorded CC session with one subagent and two parallel tool calls reconstructs an unambiguous tree; every ToolResult pairs with its invocation by id, not heuristics. `COVERAGE.md` updated.

**Dependency/risk.** Confirm current CC hook JSON field names against a live `claude` install before coding (docs verified 2026-07-28; a quick `PreToolUse` dump fixture is cheap insurance). Span-pairing change must not break E7-S8 duration stash keying — key the stash by `tool_use_id` when present.

---

#### E8-S4 — Codex session-tree fields (report SL-06 half 2) — **size S**

**Problem.** `adapters/codex/hookevent.go:47-48` hard-codes `session ≡ thread`. Verified Codex rule: *root threads use their own thread id as the session id; forked threads keep the root's session id* — correct for unforked CLI runs, wrong under forks/app-server.

**Design.**
1. Parse both ids from the hook payload: keep `session_id` as the OpenBox session identity (the root/continuity id — this preserves today's behavior for unforked runs AND becomes correct under forks, since Codex sends the root's session id), and additionally carry `thread_id` when it differs.
2. Emit `thread_id` (and `root_session_id` explicitly, for investigator clarity) as event metadata on every mapped event when `thread_id != session_id`. Wire: metadata passthrough — zero core change.
3. `adapters/common/git/session.go`: no behavior change (the ambient `CODEX_THREAD_ID` remains the commit-attribution key; note in a comment that under a forked thread the trailer names the thread while the event stream names the root — the metadata linkage above is what joins them).
4. Schema: optional `thread_id` field, same additive `schema_version` bump as E8-S3 (do both stories against one schema rev).
5. Test: fixture with `session_id != thread_id` (forked-thread shape) asserting the event carries both and the session key is the root id.

**Acceptance.** A forked-thread fixture produces events attributed to the root session with the fork's `thread_id` recorded; unforked behavior byte-identical to today.

---

#### E8-S5 — Posture-on-SessionStart (assurance tiers T0–T2 without MDM) — **size M**

**Goal.** Make the control plane able to see the *effective* endpoint posture per session — turning "staleness silently skipped" (report SL-03 nuance) and "user flipped enforce off" (SL-01) into recorded evidence.

**Design.**
1. New shared helper `adapters/common/devconfig.EffectivePosture()` returning a struct of **structural, non-secret** fields: `enforce`, `fail_closed`, `tier2`, `secret_detection`, `content_capture`, `findings`, `bundle_version`, `bundle_policy_id`, `bundle_sha256` (hash of the local bundle file), `staleness` (`fresh|stale|skipped_no_token|skipped_no_config|error`), `adapter`, `adapter_version`, `provider_version`, `config_source` (per-field `user|env|managed` once E8-S9 lands). All already resolvable from existing devconfig resolvers + a bundle read; the staleness enum requires the staleness check (`adapters/*/staleness.go`) to *return* its outcome instead of only logging — small refactor of `staleness.go:58-61`.
2. Both adapters attach the posture object to the **SessionStarted** event's `metadata` (`posture: {...}`). Core passthrough: lands in `governance_events.metadata`, Merkle-covered, echoed in the verdict, visible in `GET /agent/:id/sessions/:sessionId/logs` today with **zero sibling changes**.
3. Compute a client-side `assurance_tier` field (`T0` unmanaged / `T1` observed / `T2` managed-config-present) by simple rules (enforce+fresh bundle+secret-detection ⇒ report the inputs; the *tier label* itself is honest only as a claim — the server may later recompute it). Include the rule inputs so the server can re-derive.
4. **Optional core follow-up (recommended, tiny):** merge the posture subset into `sessions.metadata` at session create — core writes only `{"sdk_version"}` there today (`storage_session.go:78-83`) and the backend session list/detail passes `sessions.metadata` to the FE unmodified. One-line-ish extension of `handleSessionCreate` to lift `payload.metadata.posture` into the session row. This is what makes a per-session posture badge cheap in the dashboard. (openbox-core, local commit.)
5. Conformance: posture emitted exactly once per session; no secret values (INV-1 lint: assert no `obx_`/seed-shaped strings in the posture JSON); content-capture flag accurately reflects effective state.

**Acceptance.** UAT: start one observe session and one enforce session; both show posture in session logs; killing the control token yields `staleness: skipped_no_token` in the next session's posture instead of only a stderr line.

**OD gate:** none (pure evidence; changes no behavior).

---

### Phase 2 — policy integrity + delivery assurance (sibling work begins)

---

#### E8-S6 — Signed policy bundles (report SL-02) — **size L** — **ADR-0008** — **OD-E8-3 required first**

**Goal.** A local user can currently edit/replace/roll back `~/.config/openbox/policy-bundle.json` or be served raw-rego fail-open. After this story: the decider only *trusts* policy content whose signature verifies, whose epoch is monotonic, and which hasn't expired — and in an enforce profile, unverifiable policy no longer silently degrades to allow for high-risk classes.

**Design — backend (openbox-backend, local commits):**
1. **Org signing key** (net-new, template: `agent_kms_keys`): `org_signing_keys` table (`org_id`, `key_id`, `key_arn`, `public_key` b64, `algorithm='Ed25519'`, `created_at`, `active`). Provision lazily on first signed-policy request via the **existing** Ed25519 generate→RSA-wrap→KMS-import path (`aws-kms-provider.ts:122-171`, `crypto.ts:8-31`) under alias `alias/openbox-org/<org-uuid>`; add the missing `SignCommand` call (`SigningAlgorithmSpec.Ed25519Sha512`, `MessageType.RAW` — matching what core's `KMSVerifier` and shift-left's own Ed25519 usage expect).
2. **Public key route:** `GET /organization/:organizationId/signing-keys` (org-scoped read permission; also usable by the CLI with the org `X-API-Key`). Returns `[{key_id, algorithm, public_key_b64, active}]`. (No `@Public()` JWKS in v1 — the CLI is authenticated anyway.)
3. **Signed policy response:** extend `getCurrentPolicy` to include a `signature` envelope: `{key_id, algorithm, signed_at, expires_at, policy_epoch, sig_b64}` where the canonical signed bytes are `stableStringify({policy_id, agent_id, updated_at, policy_epoch, expires_at, version_hash, policy_builder})` — reusing `stableHash`'s stable-stringify (`governance-version-hash.util.ts`). `policy_epoch` = a monotonic integer bumped on every policy create/update for the agent (new column on `policies`, backfilled from version history count). `expires_at` = `signed_at + configurable TTL` (default 7 days — long enough for offline work, short enough to force re-sync).
4. **Parse-compat:** the translate path must accept policy-builder **v1 and v2** (develop's `PolicyBuilderConfigV1/V2`).

**Design — shift-left:**
5. **Bundle v2 format** (`decision/bundle.go`, additive): new optional `SignedPolicy` block on `Bundle`: `{key_id, algorithm, policy_epoch, signed_at, expires_at, sig_b64, canonical_b64}` where `canonical_b64` is the exact signed bytes from the backend (the raw policy_builder JSON travels *inside* the signed canonical payload). `dev sync` (`cli/cmd/openbox/main.go`): fetch signing key (cache it in `dev.json` as a non-secret coordinate: `org_signing_pubkey`, `org_signing_key_id`), verify the envelope signature **before** translate, verify `policy_epoch >= last-pinned epoch` (rollback protection; pin persisted next to the stale-marker files), then write the bundle carrying the envelope. Unsigned backend response (old backend) ⇒ bundle written as today with a `unsigned: true` note (compat mode).
6. **Verify-at-load in the decider** (`decision/`, keeping INV-3b — verification is local crypto, not network): when a bundle carries `SignedPolicy`, `LoadBundleFile` verifies sig over `canonical_b64` with the pinned public key (passed in via config — the decision module stays dependency-free; the adapter resolves the pubkey coordinate and hands it over), checks expiry, **re-translates** `policy_builder` from the verified canonical bytes in-process (translation logic moves/duplicates from `cli` `translateBundle` into `decision` — it already has `PolicyBuilderConfig`), and uses *only* the re-derived rules. The stored `Rules`/`PolicyBuilder` fields become a cache/debug view — tampering with them is inert. This is what makes user-local file edits detectable end to end.
7. **Failure posture (OD-E8-3):** in enforce mode, `verify failed | expired | epoch rollback | RawRegoUnlocalized` maps per the decided risk-class split — recommended: high-risk classes (Bash/`shell`, MCP) get **Tier-2 sync escalation if enabled, else deny with a clear reason**; low-risk classes stay fail-open with the degradation recorded in posture (E8-S5's `staleness`/new `bundle_integrity` field). Observe mode: never blocks, only records. Fail-open remains the observe-path law (INV-3).
8. Conformance additions (the C1–C9 suite): tampered-bundle, expired-envelope, epoch-rollback, unsigned-compat, and raw-rego-in-enforce cases.

**Acceptance.** Editing one byte of a rule in the local bundle file causes the enforce gate to treat the bundle as unverified (and posture records it); re-running `dev sync` restores. A replayed older signed bundle is refused by epoch. Old backend without signing keeps today's behavior with `unsigned:true` visible in posture.

**Sequencing note.** Backend signing (steps 1–4) and shift-left verification (5–8) are separately shippable: ship 5–8 first in compat mode if backend work queues.

---

#### E8-S7 — Server dedupe + delivery receipts (report SL-08) — **size M/L** — openbox-core work — **ADR-0009 (new response field + Redis usage)**

**Goal.** Close the at-most-once gap honestly: today a re-drained orphan spool can double-deliver (`spool.go:93-96`) and a per-event delivery error is dropped forever (`spool.go:153`); lifecycle events get zero server dedupe.

**Design — openbox-core (local commits):**
1. **Idempotency at the handler** (least surgery, per exploration): in `EvaluateGovernanceEvent` (`internal/api/governance.go`, before the workflow call), read the `Idempotency-Key` header **shift-left already sends** (`client/client.go:206-209`). Key a Redis `SETNX`-style guard (reuse the injected `caching.Cache`, same pattern as the nonce replay cache `internal/services/agent.go:202-227`) on `idem:<agent_id>:<key>` with a TTL (24h). On hit: return the cached prior response (store the marshaled `GovernanceVerdictPublicResponse` as the value) — replay-safe, not just drop-safe. Miss: process, then cache the response. Scope: only when the header is present, so SDK/agent-runtime traffic is untouched.
2. **Receipt:** no new field needed for v1 — `governance_event_id` already returns in the response. Document its two caveats for shift-left (hook path returns the parent event's id; some early-return paths return `uuid.Nil`) and additionally echo the `Idempotency-Key` back as `metadata.receipt_key` via the existing metadata echo path so the client can correlate even on `uuid.Nil` paths.

**Design — shift-left:**
3. **Retry-not-drop in the spool drain:** change `drainRotated` (`adapters/codex/spool.go:153` and the CC twin) so a *delivery error* (fn returns non-nil) keeps the line in the undelivered set → written to the recovery file at the end of the drain, instead of `_ = fn(...)`. Bound it: a per-line `attempts` counter embedded in the recovery filename or a sidecar count (cap, e.g. 5 drains) so a poison event can't loop forever — after the cap, drop and log (still fail-open, INV-3). Since the server is now idempotent on the key, re-sending is safe: delivery upgrades from at-most-once to effectively at-least-once + server dedupe = effectively-once.
4. **Client:** derive the `Idempotency-Key` deterministically from the stable `event_id` (already the case — verify and lock with a test), so a respooled line reuses the same key.
5. **Evidence completeness (cheap first cut):** at SessionEnd flush, if the spool/recovery files drain to zero → emit `evidence_state: complete` in the SessionEnded metadata; anything left ⇒ `degraded` (+ counts). This is the report's `complete/degraded` state without new infrastructure. (Per-session sequence + prev-hash chain: **deferred** — core's Merkle `leaf_index` already gives server-side ordering/tamper evidence, and the exploration showed a client-side prev-hash chain would fight `RebuildSessionLeavesActivity`; revisit only if an auditor requires client-side chaining.)

**Acceptance.** Kill the network mid-flush: re-running flush delivers the remainder exactly once (server sees no duplicates — verify by `governance_events` count on UAT); orphan-reclaim double-delivery test shows one stored event. SessionEnded carries `evidence_state`.

---

### Phase 3 — managed deployment tier (report SL-01) — **OD-E8-2 gates the phase, OD-E8-1 gates step 3**

---

#### E8-S8 — Managed provider-config templates + `--managed` install — **size M**

**Design.**
1. New `deploy/managed/` directory with **reference templates + a README that is explicit about what each does and doesn't guarantee**:
   - Claude Code `managed-settings.json`: OpenBox hooks under managed `hooks` + `allowManagedHooksOnly: true`, `disableSideloadFlags`, sandbox `{enabled, failIfUnavailable, allowUnsandboxedCommands:false}` for the strict profile, version floor. Target paths documented per OS (`/etc/claude-code/managed-settings.json` Linux, `/Library/Application Support/ClaudeCode/...` macOS, plist/registry variants).
   - Codex `/etc/codex/requirements.toml` + `managed_config.toml`: managed OpenBox hook, `allow_managed_hooks_only = true`, pinned `allowed_approval_policies`/`allowed_sandbox_modes`, `[experimental_network]` allowlist stanza (commented, org-specific), `[features].hooks`.
2. `openbox dev init --managed` (and standalone `openbox managed install`): when run privileged, writes the provider managed files from the templates (idempotent, backs up an existing file, refuses to *weaken* an existing managed file); when not privileged, prints the exact files + paths for the MDM team. It does **not** invent a config-management system — MDM distribution is the org's plane; we ship the payload.
3. Posture integration: `EffectivePosture()` (E8-S5) gains `provider_managed: true|false|unknown` by checking for the managed files' existence/containing our hook (read-only check; CC also exposes effective settings via `claude config` paths where feasible).

**Acceptance.** On a Linux box: `sudo openbox managed install --provider claude-code,codex` yields sessions whose posture reports `provider_managed:true`; deleting the user-level hook file does not remove the hook (provider loads the managed one).

---

#### E8-S9 — System-level devconfig precedence (the managed OpenBox layer) — **size M**

**Problem.** `devconfig` resolution is user config → env override (`devconfig.go:446-457` region); a developer can flip `enforce`/`content_capture` freely (report SL-01).

**Design.**
1. New managed config path: `/etc/openbox/dev.json` (Linux; `os`-appropriate equivalents mirroring the provider conventions), root-owned. Loaded by `devconfig.load()` alongside the user file.
2. **Precedence inversion for managed-set fields only:** a field *present* in the managed file wins over both user config and env (env override exists for tests/CI — managed must beat it or it's theater); fields absent from the managed file keep today's user→env behavior. Implement as a merge step in `load()` + a `Source(field)` accessor so `EffectivePosture()` can report `config_source: managed|user|env` per field.
3. Managed file schema = same `DevConfig` struct + optional `"locked": ["enforce","content_capture",...]` list for explicitness (only listed fields are enforced-managed; unlisted present fields act as defaults the user may still override — gives orgs a default-vs-mandate distinction).
4. **OD-E8-1 lands here:** the shipped managed template sets `content_capture` per brian's decision (recommended compromise: template defaults `content_capture:false` in the *managed enterprise profile* while the product default stays ON — both intents preserved). Template comment cites the decision.
5. `openbox doctor` (new small subcommand, also serves E8-S8): prints effective posture, per-field source, managed-file presence/hash, bundle signature state, staleness — the debugging surface the report's DX section asks for.
6. Tests: managed-beats-env, absent-field-falls-through, unreadable-managed-file behavior (fail toward the *stricter* value for `enforce`/`fail_closed`? No — unreadable managed file must not silently disable enforcement mandates; treat unreadable-but-present as "managed indeterminate" → posture flags it and enforce-mode hooks treat it as managed-locked for the `locked` semantics we ship. Keep observe fail-open).

**Acceptance.** With `/etc/openbox/dev.json` locking `enforce:true`, `OPENBOX_ENFORCE=0` and user `dev.json` edits do not disable the gate, and posture shows `enforce: managed`. Without the managed file, behavior is byte-identical to today.

---

### Phase 4 — verified provenance (report SL-05)

---

#### E8-S10 — Signed post-commit attestation → `verified` lineage — **size L** — **ADR-0010**

**Goal.** Upgrade lineage from `attributed` (ownership) to `verified` (cryptographic proof the session's keyholder attested this exact commit), landing in the **existing** `deploy_session_links.verified` column.

**Design — shift-left:**
1. **Attestation creation (post-commit hook):** extend the git integration with a `post-commit` hook (alongside the existing `prepare-commit-msg`): build canonical bytes `stable_json({v:1, repo_remote_canonical, commit_sha, tree_sha, parent_shas, session_id, thread_id?, bundle_policy_id, bundle_sha256, adapter, adapter_version, did, signed_at})`, sign with the **existing dev-agent Ed25519 seed** (same key AIP request signing uses — no new key material, INV-1 path already exists via `ResolveCredentials`), store as a **git note** under `refs/notes/openbox` on the commit (survives push with `git push origin refs/notes/openbox`; the trailer stays as the human-readable inferred claim). Fail-open: no seed / no session ⇒ no note, commit proceeds (INV-3).
2. **Git action (`actions/openbox-git-action`):** on deploy, for each session-trailer commit, read the note; if present, include the attestation object in the Deploy event's `metadata.sessions[]` entry (new optional `attestation` field alongside the existing `{session_id, source, commit, verified}`).
3. **Push the notes ref** in the action docs/workflow snippet (`fetch: refs/notes/openbox`) — document that a missing note is not an error, it just stays `inferred`.

**Design — openbox-core (local commits):**
4. `StoreDeploySessionLinksActivity` (`storage_deploy_session_links.go`): when a session entry carries `attestation`, verify: (a) signature over the canonical bytes via the **existing** `did.KMSVerifier` + DID→KMS-alias resolution (`agent.go:142,183` pattern — reuse, the alias is derivable from the DID in the attestation); (b) `commit_sha` in the attestation equals the entry's commit; (c) the DID's agent owns the claimed session (the existing ownership join). All pass ⇒ write the link with `verified:true`; any failure ⇒ `verified:false` + a structural `verify_fail_reason` in the link's source field or event metadata (never blocks the deploy event — lineage stays non-fatal, matching today's error-swallowing at `governance_workflow.go:844-851`).
5. **No backend schema change** — `verified` exists; develop's `commit-derived` consumer surfaces it. Optional FE follow-up: render `verified` distinctly from `inferred` (openbox-fe, out of E8 scope).

**Threat honesty (for the ADR):** this proves *the session keyholder attested this commit* — it does not prove the session's model produced the diff (nothing can, locally). It defeats the report's "stamp an owned-but-unrelated session id" attack for `verified` links only if the attacker lacks the seed; a compromised endpoint can still self-attest. That residual is exactly why T4 also requires the managed tier (E8-S8/S9) — say so in the ADR.

**Acceptance (UAT):** a real commit→deploy run produces a `deploy_session_links` row with `verified:true`; forging the trailer on a foreign commit (no note / wrong sig) produces `verified:false`. Existing no-attestation flows are unchanged.

---

## 3. Sequencing & dependency graph

```
E8-S1 (test isolation)  ──┐
E8-S2 (doc truth)       ──┼── independent, start immediately, no gates
                          │
E8-S3 (CC fields) ──┬── one shared schema_version bump ── E8-S4 (Codex fields)
                    │
E8-S5 (posture) ────┴──> feeds E8-S8/S9 (config_source, provider_managed fields)
                          and E8-S6 (bundle_integrity posture field)

OD-E8-3 ──> E8-S6 (signed bundles: backend 1–4 ∥ shift-left 5–8, separately shippable)
ADR-0009 ──> E8-S7 (core dedupe ∥ shift-left retry — core first, retry depends on server idempotency)

OD-E8-2 ──> Phase 3:  E8-S8 (templates/installer) ── E8-S9 (managed devconfig, needs OD-E8-1)
ADR-0010 ──> E8-S10 (attestation; independent of Phases 2–3, needs only E8-S1's clean git tests)
```

Recommended order of execution: **S1 → S2 → S3+S4 → S5 → S7(core) → S7(client) → S6 → S8 → S9 → S10**, with S10 movable earlier if lineage demos need it (it has no dependency on S6–S9). Phase-2/3 boundary respects OD-E8-2.

## 4. Review gates & conventions

- Every story: brian G3 review + Sam G_SEC for anything touching enforcement, crypto, or egress (S5, S6, S7, S9, S10 mandatory G_SEC per repo convention).
- ADRs: **ADR-0008** signed policy bundles (backend org key + bundle v2 + verify-at-load), **ADR-0009** idempotency/receipts (new Redis usage + response semantics in core), **ADR-0010** commit attestation (git notes format, canonical bytes, threat model). E8-S3/S4's schema bump is additive under the existing `schema_version` mechanism — no ADR, but note in `contracts/dev-event/`.
- Sibling commits: openbox-core and openbox-backend work targets their `main` locally (core's dev-runtime branch is fully merged; backend E8 work should note the develop policy-builder-v2 divergence in the commit message). No pushes.
- UAT (`.node.lat`) remains the test target; use the `secrets-e2e.json` identity recipe (memory: `test-target-uat`).

## 5. Effort summary

| Story | Size | Repos touched | Gate |
| --- | --- | --- | --- |
| E8-S1 test isolation | S | shift-left | G3 |
| E8-S2 doc truth | S | shift-left | G3 |
| E8-S3 CC correlation/subagent fields | M | shift-left | G3 |
| E8-S4 Codex session-tree fields | S | shift-left | G3 |
| E8-S5 posture-on-SessionStart | M | shift-left (+1-line core opt.) | G3 + G_SEC |
| E8-S6 signed bundles | L | shift-left + backend | G3 + G_SEC, ADR-0008, OD-E8-3 |
| E8-S7 dedupe + receipts | M/L | shift-left + core | G3 + G_SEC, ADR-0009 |
| E8-S8 managed templates/installer | M | shift-left | G3 |
| E8-S9 managed devconfig layer | M | shift-left | G3 + G_SEC, OD-E8-1 |
| E8-S10 commit attestation | L | shift-left + core | G3 + G_SEC, ADR-0010 |

Explicitly out of E8 (recorded in the verification doc §3): OIDC short-lived tickets, egress proxy, approval broker (**framing corrected 2026-07-29 — not actually blocked on Codex ask-support; a deny-and-retry loop works on both tools today, see §8** — Codex ask-support would only improve the medium tier), capability registry service.

## 6. Verification addendum (2026-07-29 re-verification)

The four §1 findings were independently re-verified against the same sibling HEADs (core `8ea33bc`, backend `9beb0c5`) plus a fresh external-landscape pass. All hold. Corrections and new nuances the stories must absorb:

### Wire-placement refinements (affects E8-S3/S4/S5/S7)

- **Hook-path event metadata is NOT Merkle-covered.** The event leaf is written only when `IsNewEvent` (core `attestation/merkle.go:33-34`); hook evaluations pass `IsNewEvent:false` (`governance_workflow.go:754,398`) and write span leaves only. Rule of thumb: **hook-stage correlation fields (tool_use_id, agent_id, thread_id) go in `span.attributes`** (span leaf hashes exactly `span.Attributes`, `merkle.go:52-56`); lifecycle-event fields (posture on SessionStarted) may use event `metadata` — lifecycle events are new events, so their leaf covers it.
- **Avoid reserved metadata key names.** `ExtractEventMetadata` silently drops colliding keys before hashing (`governance_metadata.go:115-120`): don't name posture/correlation fields `timestamp`, `run_id`, `event_type`, `status`, `duration_ms`, `error` (nest under `posture:{...}` / `session_tree:{...}` — already the plan's shape, now load-bearing).
- **`SpanData.Data` is an arbitrary-JSON escape hatch but is NOT span-leaf-hashed** — never place evidence there; `attributes` only.

### E8-S6 corrections (backend signing)

- `agent_kms_keys` = P-256 only (see §1.3 correction); the Ed25519 org-key design stands — it matches core's `KMSVerifier` (`Ed25519Sha512`/`RAW`) and shift-left's client crypto. The per-agent P-256 key already has a stored SPKI but **no code path ever calls `Sign`** anywhere in backend `src/` (re-confirmed: zero `SignCommand`), so either curve requires the new signing call.
- Policy route auth is **JWT-or-`X-API-Key` + `ReadAgentPolicy` permission** (global guard stack, `agent.controller.ts:700-704` via an `AgentService` pass-through) — not API-key-specific; the signing-key route should mirror that pattern.
- `policy_epoch` prior art for the migration: `agent_control_set_versions` (per-set snapshot + `version_hash`) and `agent_governance_snapshots` (`policy_version_ids/hashes` arrays); today versioning is only `is_current_version` rows — backfill epoch from history-row count per agent as planned.

### E8-S7 confirmations

- The receipt caveats are exactly as documented and slightly worse: **4** early-return paths hand back `uuid.Nil` (Handoff validate/success, session-not-pending, session-halted — `governance_workflow.go:165-188,244-254,293-298`), and every hook request against one activity returns the **same** parent `governance_event_id` — so the receipt correlates *storage*, not *delivery*; the echoed `Idempotency-Key` (`metadata.receipt_key`) is the per-request correlator, as designed.
- Lifecycle events truly have zero dedupe today (`validation.go:86-93` bails on empty `activity_id` **or** empty `event_type`); the handler-level idempotency guard covers them uniformly. `caching.Cache` is injectable everywhere needed (`container.go:81-84`; nonce-replay precedent `agent.go:202-227`). Core hardening note for the ADR: the nonce replay cache is **silently skipped when Redis is absent** (`agent.go:203-205`) — the idempotency guard must at minimum log its own degraded state.

### E8-S10 confirmation

- `deploy_session_links` DDL lives in **backend** migration `1781100000000` (not core); rows are written only by core's `StoreDeploySessionLinksActivity`; backend's commit-derived path stamps `session_verified:false` metadata on push events (`project.service.ts:2061-2098`) and never writes the table. The core-side verify-and-flip design is the right (only) seam.

### Landscape deltas (2026-07-29)

- **MCP spec rev 2026-07-28 is stateless** (no `Mcp-Session-Id`): any future MCP correlation must key on `_meta`/headers, not MCP session ids — noted for E8-S3's MCP-class events.
- Codex `SessionEnd` independently confirmed via upstream PR #33895 (merged 2026-07-17, fires for root threads, subagents excluded) — supports the COVERAGE.md refresh in E8-S2. The Codex fork/session-id rule is only partially sourceable from public docs (originally verified from source) — keep the E8-S4 fixture as the guard.
- Crowding: Endor Labs (hooks guardrails), Entire ($60M seed; checkpoints on a hidden git ref), SentinelOne consolidating Nightfall + Prompt Security, Netra Security, Lunen.ai. None combine cross-provider receipts + provenance end-to-end — E8-S6/S7/S10 are the differentiation, which argues for their priority (input to OD-E8-2).
- Threat validation: CSA-documented Claude Code GitHub-Action prompt-injection→key-exfil chain and "agentjacking" research reinforce the report's threat model; nothing found invalidates the managed-config + signed-bundle + receipts + attestation architecture.

## 7. Third-pass addendum (2026-07-29, after second restore)

**Provenance note:** this file and `docs/enterprise-agentic-cli-governance-report.md` vanished from `docs/` a **second** time (both untracked) and were restored from session transcripts again — the report byte-identical from its attachment record, this plan from the original Write plus the two recorded §6 edits. **Commit all three governance docs (report, verification, this plan) to stop the recurrence.**

Shift-left source re-verified a third time at HEAD `cfc93f7`: all SL-01…SL-11 hold cite-exact; SL-11 reproduced again (14 failures in `adapters/common/git` + 1 in `cli` with `CODEX_THREAD_ID` exported, 0 without). Repro notes for E8-S1: the repo is multi-module — run each module from its own root — and `-count=1` is required for the `cli` module because the env var is read by a spawned child binary, invisible to Go's test cache.

### Backend refinements (SL-03 wording + one new OD)

- **Manual rotate/revoke endpoints for agent keys DO exist**: `POST :agentId/rotate-api-key` (`agent.controller.ts:971`, instant-cutover single-token) and `POST :agentId/revoke-api-key` (`agent.controller.ts:996`, token→NULL), plus DID key rotation (`:956`). The accurate SL-03 claim is "no expiry/TTL/scheduled rotation/grace window," not "no rotation anywhere."
- Org `api_keys` already model the full credential lifecycle (`valid_from`/`expires_at`/`ip_whitelist`/`last_used_at`/soft-delete — `api-key.entity.ts:20-54`) — the template if agent keys get lifetimes. Keycloak **device-authorization grant is already enabled** realm-side (`auth.service.ts:418-421`) — ready substrate for the deferred dev-identity program.
- **OD-E8-4 (new) — agent-key lifetime.** Options: (a) accept manual-only rotation for now, record in the threat model; (b) add `token_expires_at` to `AgentEntity` + enforcement in core's token validation (core validates `obx_` tokens; the backend never does); (c) fold into the deferred identity program. (b) is small and mostly core-side; (a) is honest if the identity program is near.
- Housekeeping: `src/modules/did/local-identity-provider.ts` is **wired into DidModule but untracked** in openbox-backend — commit it or fresh clones break under `KMS_PROVIDER=local`.

### Landscape deltas (third pass — new beyond §6)

- **Cursor shipped hooks on 2026-07-10 (v3.11)**: `beforeSubmitPrompt`, `afterAgentResponse`, `afterAgentThought`, `stop`, `subagentStart` — SL-8's surface-spike risk drops materially. Cursor also ships an admin/security **audit log** (~19 admin event types, SIEM/S3/webhook streaming) — correct the earlier "no audit log" claim to "no per-prompt/per-tool-call **AI-activity** log," which remains true and remains our gap to fill. Strong input to OD-E8-2.
- **CC hardening wave** (changelog, post-2.1.126): `sandbox.credentials` deny (2.1.187) / mask (2.1.199), `sandbox.network.strictAllowlist` (2.1.219), `processWrapper` (2.1.210), `enforceAvailableModels` (2.1.175), `requiredMinimumVersion`/`requiredMaximumVersion` (2.1.163 — **fail open by design**: version floors are hygiene, not enforcement; note this in the E8-S8 template), OTel enrichment (`agent_id`/`parent_agent_id` on tool spans, `OTEL_LOG_ASSISTANT_RESPONSES`), self-hosted Claude apps gateway. Also confirmed: `agent_id`/`agent_type` ride **all** hook events fired inside subagents, not only SubagentStart/Stop — E8-S3 should parse them on every event. OTel `user.id` is a random anonymous id (account-bound ids are `user.account_uuid`/`user.account_id`).
- **Medium-confidence items to verify during E8-S2's conformance recheck** (secondary sources only): CC v2.1.200 reportedly made "Manual" the default permission mode (re-run the E6 conformance suite against that baseline; check headless flows don't stall); a CC deny-rule bypass (>50 subcommands degrading block→ask) patched ~2.1.90 — check our own matcher for analogous cap-style degradation.
- **First-party governance squeeze accelerating**: Anthropic Compliance API + 28 SIEM/identity integrations (May 21) and admin spend/analytics controls (Jul 2); Codex merged into ChatGPT desktop with enterprise admin controls (Jul 9), Windows native sandbox + PowerShell classifier hardening (0.144.5). Differentiation keeps concentrating in cross-tool normalization + in-session enforcement + receipts/provenance (E8-S6/S7/S10) — consistent with §6's OD-E8-2 input.
- **New entrants**: Codenotary AgentMon 3 (tamper-proof decision-ledger audit — the same pitch as our SessionMerkleLeaf path; expect it as RFP table stakes), Portal26 AMP (free observe tier for Claude/Claude Code), Sysdig and Alterion "Draco" forming a runtime-security-for-coding-agents category.
- **Incidents**: Sentry-DSN "agentjacking" quantified (85% success, 2,388 exposed orgs, cross-tool via MCP) and Microsoft's poisoned-MCP-tool-descriptions research → both support Bash+MCP as OD-E8-3's fail-closed class; Sophos: credential access = 56.2% of blocked coding-agent activity (validates E6-S9 Tier-1 secret detection); Claude Code weaponized in the Mexican-government breach (~195M taxpayer records); npm supply-chain via unsupervised agents at 2.6× 2025 volume. Headless/CI remains the least-governed surface — E8-S10's threat model.
- **Regulatory/identity**: EU AI Act omnibus (2026-06-16) postponed most high-risk deployer obligations to Dec 2027/Aug 2028; the 2026-08-02 GPAI-provider milestone stands — "audit-ready session evidence mapped to AU/AC controls" stays the framing, without deadline panic. NIST COSAiS SP 800-53 agent overlays (AC/IA/AU/SR) expected late-2026/2027; OWASP Agentic Top 10 (ASI01-10) becoming shared vocabulary. Agent identity converging on human-bound short-lived credentials: Microsoft Entra Agent ID GA, Okta Cross App Access on IETF-adopted ID-JAG, AuthZEN MCP + access-request drafts approved — the deferred identity program should federate with these, not compete.
- Caveat kept from this pass's fact-check: gitsign/in-toto as *AI-code-attribution* practice is our inference (substrate bet), not an established industry pattern — don't cite it as precedent in customer-facing docs.

## 8. Approval blocking design (deny-and-retry) — upgrades the deferred approval-broker item

Added 2026-07-29 (brian's question: how can CC `ask` / Codex PermissionRequest *block* until approval, given today it's a self-approval yes/no the developer can just accept?).

### 8.1 What the native mechanisms actually are

- **CC `ask`** (our REQUIRE_APPROVAL mapping, E6-S6): CC pauses the tool call and prompts **the developer themself** — CC has no remote-approver concept. It is a *self-approval* control: an accountability speed bump with attribution, not prevention. "Bypass" is clicking yes.
- **Codex PermissionRequest**: an *observation* hook (fires when Codex would prompt); the hook decision parser accepts only allow/deny (rejects `ask`) — hence OD-SL7-ASK's REQUIRE_APPROVAL→deny.
- **Beneath both**: without the managed tier (E8-S8/S9), the developer can remove the hook, flip `dev.json`, or run bypass-permissions mode. **Any local block is advisory until `allowManagedHooksOnly` / `allow_managed_hooks_only` + `disableBypassPermissionsMode` are managed-deployed** — prevention assurance is a Phase-3 property, full stop.

### 8.2 How to block for a real (four-eyes) approval — three patterns

1. **Synchronous hold in the hook — rejected.** Poll the backend from inside PreToolUse until decided: hooks run under kill timeouts (our Tier-2 budget already lives under CC's 5 s kill), the whole session freezes, and a timeout kill lands in fail-open/closed ambiguity. Only viable for seconds-SLA auto-approvers.
2. **Deny-and-retry with a server-side approval record — recommended; ~80% of machinery exists.**
   - High-risk action → Tier-2 escalation → core returns REQUIRE_APPROVAL with a server-minted `approval_id` (= governance event UUID; backend decide endpoint + `approval.created/decided/expired` WS events already exist — §5 note).
   - Adapter returns **deny** with the approval id/URL in the reason: "Blocked pending approval #123 — approve in dashboard, then retry." Session stays live; the agent can do other work.
   - Approver decides in the dashboard. Developer/agent retries → core's **approval-bypass fingerprint cache** (`services/governance.go:367`) matches the approved action → PROCEED. Same-action-retry-after-approval already works server-side.
   - **Notification gap, closed by reuse**: no agent-facing poll endpoint exists — but the Tier-3 findings loop (E6-S11) already tails `advisories.jsonl` into UserPromptSubmit/PostToolUse. An "approval #123 granted — retry" advisory rides that channel; no polling in the hot path.
3. **CC `ask` as the deliberate self-approval tier — keep it**, and record the developer's yes/no as an audit event: "recorded acceptance with attribution," not bypass. (Think sudo, not firewall.)

### 8.3 Should we block? (UX position)

Not broadly, and never synchronously for human-latency approvals: hard blocks break flow and push developers to ungoverned terminals (the shadow-AI outcome is worse than a logged self-approval); broad blocking trains click-through and destroys signal on the hits that matter; and pre-managed-tier blocks are theater anyway. Tiered model (aligns with OD-E8-3's risk-class split):

| Tier | Action class | Mechanism |
| --- | --- | --- |
| Prevent | High-risk, rare (prod credentials, destructive Bash, unknown MCP servers, protected-branch writes) | Deny-until-approved (pattern 2), time-boxed SLA, pre-approval for recurring patterns |
| Account | Medium-risk | CC `ask` + audited response; Codex deny-with-reason until an ask-equivalent ships |
| Observe | Everything else | Today's default + Tier-3 advisories |

Enterprise framing: `ask` = accountability control (who accepted the risk, tamper-evident); deny-and-retry = prevention control (narrow, four-eyes); prevention *assurance* exists only on the managed tier. Codex's Compliance API and Cursor's audit log can do neither — this is differentiation.

### 8.4 Candidate story + OD

- **E8-S11 (candidate, not yet scheduled) — deny-and-retry approval loop.** Size M. Shift-left: REQUIRE_APPROVAL→deny-with-approval-reference on both adapters (CC keeps `ask` for the Account tier); approval-granted advisory into the E6-S11 findings channel. Backend: none expected (decide endpoint + WS exist); core: none expected (fingerprint cache exists) — verify the approval-cache fingerprint matches across retried hook invocations (same span shape) early, it's the one technical risk. Gate: G3 + G_SEC. Depends on OD-E8-3's risk-class split for which classes route to Prevent.
- **OD-E8-5 — approver model + SLA.** Who may approve (org role? team lead? security only?), the approval TTL/expiry, and whether pre-approved recurring patterns are org-configurable. Product/priority call — brian's, not inferred. The §5 deferred-item framing is corrected accordingly: the broker is *not* blocked on Codex ask-support; ask-support would only improve the Account tier.
