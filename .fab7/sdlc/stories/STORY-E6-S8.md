# STORY-E6-S8 — Sidecar native policy evaluator + pull-at-init policy distribution (`[EXT-opa-bundle]` close)

**Epic:** E6 (enforcement — the real policy SOURCE + a faithful local evaluator that makes the sidecar's verdict match openbox-core for a real per-agent policy). **Risk:** high (this is the first path that (a) obtains a real org policy over the network, (b) evaluates it to a verdict that can DENY a tool call, and (c) blocks a fail-closed session on staleness; a bug either mis-evaluates a policy (wrong deny/allow), leaks the org key/policy across INV-1, or introduces network I/O onto the decision hot path across INV-3b). **Status target:** review (build + validations + both reviews, pending brian G3 + Sam G_SEC).

## Source
- **Design:** `.fab7/sdlc/design/sidecar-policy-sync.md` §3 (distribution + regoEvaluator), §5 (conformance), §8 (slice E6-S8). **ADR-0005** (accepted brian 2026-07-15) — the ratified strategy; **read it first**, it supersedes design OD-SYNC-1.
- **Decisions ratified (brian 2026-07-15, this build):**
  - **Scope = full story** — native evaluator + `dev sync` fetch/PIN + session-start staleness + conformance.
  - **OD-SYNC-2 = lightweight, pure-Go, no OPA** → evaluate the `policy_builder` structured config natively (no embedded rego engine, no cgo). Raw hand-written rego does **not** localize → fail-open-local + flag (accepted §2a fidelity floor / OD-SYNC-7; covered by T2/T3 server-side).
  - **OD-SYNC-4 = org control-plane key** for the policy read (`read:agent_policy` is org-scoped), not the agent runtime `obx_` key.
- **Reverses OD-SYNC-1** (embed OPA rego) — see ADR-0005 §Context/Decision. The `Evaluator` seam is unchanged, so a rego/Regorus evaluator can drop in later.

## Cross-repo recon (Explore, 2026-07-15 — per the user's cross-repo directive)

### A. openbox-backend — `GET /agent/:agentId/policies/current` (the policy source)
- Route `agent.controller.ts:661` (`@Get(':agentId/policies/current')` → `getCurrentPolicy`), base `@Controller('agent')`. Matches on `is_current_version` (not `is_active`), `policy.service.ts:301-303`.
- Response envelope: global `TransformInterceptor` wraps EVERY response as **`{ "status": 200, "data": <payload> }`** (`transform.interceptors.ts:22-25`). Payload = the raw `PolicyEntity` (snake_case columns).
- Fields (`policy.entity.ts`): `id` (uuid), `rego_code` (raw rego text), `config` (jsonb — holds `config.path` and, for builder policies, **`config.policy_builder`**), `updated_at` (timestamptz), plus `agent_id`, `name`, `is_active`, `is_current_version`.
- **No active/current policy → `{ "status": 200, "data": null }`** (HTTP 200, not 404).
- Auth: global `JwtAuthGuard` accepts header **`x-api-key`** = an `obx_key_…` **org** key (`api-key.service.ts:24`), OR a Keycloak Bearer JWT (+ `x-openbox-client`). Requires permission **`read:agent_policy`** (`@Permissions(PermissionEnum.ReadAgentPolicy)`, `agent.controller.ts:662`). `OrganizationGuard` is a no-op here (no org param), so scoping is by the key's org server-side.
- `config.path` = `org/<sanitizedOrgId>/policy_<uuidNoHyphens>` (`policy.service.ts:121-124`, `format-rego-code.ts`); the rego **package** = that with `/`→`.`. The output rule is **`result`** (`{decision, reason}`), so the OPA query is `data.<pkg>.result` — NOT `data.<path>.decision`.

### B. openbox-core — `buildOPAInput` / `buildSpanMap` (the input shape parity target; `internal/services/opa.go`)
- Core is a **pure external-OPA HTTP client** (POST `{OPA_URL}/v1/data/<path>` body `{"input":…}`); NO in-process rego, NO `open-policy-agent` in go.mod. So parity = replicate the **`input` JSON**.
- `buildOPAInput` (opa.go:459-477) top-level keys: `event_type`, `source`, `workflow_id`, `run_id`, `workflow_type`, `task_queue`, `timestamp`, `span_count`; `addAgentInfo` adds `agent_id`, `trust_tier`/`risk_tier`, `trust_score`; `addActivityFields` adds `activity_*`; `addSpansToInput` adds `spans` (array). No top-level `org` (org is in the query path).
- `buildSpanMap` (opa.go:619-719) — **no per-tool-kind branching, no `command` key, no `tool_kind` key.** Always: `span_id`, `trace_id`, `name`, `semantic_type`, `start_time`, `end_time`, `attributes`. Conditional (only when the source field is set): `file_path`, `file_mode`, `file_operation`, `bytes_read`, `bytes_written`, `lines_count`; `function`, `module`, `args`, `result`; `http_method`, `http_url`, `http_status_code`; `db_*`; `request_body`, `response_body`; `hook_type`; `duration_ns`; `status`, `stage`, `error`. A shell command surfaces via `attributes` (and/or a function-call span), NOT a top-level `command` key. `attributes` is passed through as-is.
- Decision→action (opa.go:257-270), input lowercased: `continue|allow`→allow, `block`→block, `stop|halt`→halt, `require_approval|require-approval`→require_approval, default→allow. `constrain` is NOT a policy decision (behavior/guardrail only). Undefined result→allow. **The existing `sidecar/bundle.go decisionToVerdict` already mirrors this exactly.**

### C. openbox-backend — `policy-builder.util.ts` (the evaluator parity oracle)
- `config.policy_builder`: `{ version:1, rules: PolicyBuilderRule[] }`. Rule: `{ id, name, decision, reason, matchMode:'all'|'any', conditions: Condition[] }`. **No `priority`.**
- Condition: `{ id, field, operator, transform:'value'|'count', value:string, valueType:'string'|'number'|'boolean' }`.
- **9 operators** (the complete set): `equals`, `not_equals`, `contains`, `greater_than`, `greater_than_or_equal`, `less_than`, `less_than_or_equal`, `exists`, `not_exists`. (No regex/in/starts_with/not_contains.)
- **Decisions:** `ALLOW | REQUIRE_APPROVAL | BLOCK | HALT` (uppercase). **No CONSTRAIN** in the builder.
- **field → input path:** direct prefix. `normalizeFieldPath` (`:341-352`): if `field` == `input` or starts with `input.` use verbatim, else prefix `input.`. So `field="file_path"` → `input.file_path`; author writes `spans[_].file_path` → `input.spans[_].file_path`. `[_]` = existential over an array (match if ANY element satisfies). The path resolves against the `buildOPAInput` tree (recon B).
- **operator semantics** (per `buildConditionExpression :397-423`):
  - `equals`/`not_equals` → typed `==`/`!=` (string vs number vs bool per valueType).
  - `contains` → **case-insensitive substring**: `contains(lower(sprintf("%v",[path])), lower(value))`. Always treats value as a string (ignores valueType). NOT array membership.
  - `greater_than`/`_or_equal`/`less_than`/`_or_equal` → native `>`/`>=`/`<`/`<=` (numeric only when valueType=number; else lexical string compare).
  - `exists`/`not_exists` → path resolves to ≥1 value / 0 values (comprehension `count>0` / `==0`).
- **valueType coercion:** `number`→numeric (NaN→0); `boolean`→`true` iff the string is exactly `"true"`; `string`→the string. `contains` always uses the raw string. `transform:'count'` forces valueType=number.
- **transform:** `value` (target = resolved path value) | `count` (target = collection length as a number; only applied when operator ∉ {contains, exists, not_exists}; `[_]` path → count of matched elements, else count of the collection).
- **matchMode:** `all`=AND of all conditions; `any`=OR.
- **precedence:** **FIRST-MATCH by `rules` array order** (guard clauses `not rule_0_match … not rule_{i-1}_match` ensure earlier rules win). NOT max-severity. No matches → default `{decision:"ALLOW", reason:null}`.

## The design

### 1. `builderEvaluator` (new pure-Go `Evaluator`, `sidecar/builder.go`)
- Parses `PolicyBuilderConfig` (the recon-C types) and evaluates a `DecisionRequest` (via its `BuildOPAInput` doc) to a `client.Evaluation`, replicating recon-C semantics **exactly**:
  - Resolve `field` (strip/require `input.` prefix, walk dotted path, `[_]` existential over `[]any`) against the `BuildOPAInput(req)` map.
  - Apply `transform` (`count` where supported), coerce `value` per `valueType`, apply the operator, combine per `matchMode`.
  - **First rule** whose predicate holds → its decision (uppercase) via `decisionToVerdict` + its `reason` + the policy id. No match → default ALLOW. **Not max-severity.**
- Pure, no I/O, concurrency-safe (matches the `Evaluator` contract). Rego is NOT parsed/evaluated.
- Drops in behind `Evaluator` with ZERO change to `Server.decide` or the E6-S2/S3/S4/S6/S9 apply cascade.

### 2. `BuildOPAInput` parity fix (`sidecar/input.go`) — the load-bearing correctness crux
- Reshape to match core's `buildSpanMap` (recon B): span keys `name`, `semantic_type`, `attributes` (+ conditional `file_path`, `file_operation`, `function`, `args`, `http_*`, …). Move `command` INTO `attributes` (core has no top-level `command` span key); drop the ad-hoc `tool_kind` top-level span key. Top-level input keys align to `buildOPAInput` (`event_type`, `source`, `run_id`, `workflow_id`, `agent_id`, `span_count`, `spans`).
- Conformance test asserts the emitted field NAMES match core's `buildSpanMap` for the tool kinds a CC hook produces (file read/write, mcp, shell). A name mismatch silently no-fires a rule — §5.2.

### 3. Local bundle carries the PIN + policy source (`sidecar/bundle.go`)
- `Bundle` gains `PolicyID string` and `UpdatedAt string` (the PIN — opaque; staleness compares them) and, for a builder policy, the parsed `PolicyBuilder *PolicyBuilderConfig` (or the CLI translates it into the existing `Rules` — see below). Keep `Version` for telemetry.
- **Translation choice (builder → local bundle):** because builder precedence is FIRST-MATCH (recon C) and the existing `Bundle.Rules`/`bundleEvaluator` is MAX-SEVERITY, do **not** reuse `Rules` for builder policies — that would change semantics. Carry the `PolicyBuilderConfig` on the bundle and evaluate it with `builderEvaluator`. `SetBundle` selects `builderEvaluator` when `PolicyBuilder != nil`, else the legacy `bundleEvaluator` (hand-authored local `Rules`), else cold-start fail-open. `LoadBundleFile`/`validate` accept the new fields.

### 4. `openbox dev sync` (CLI) + `cli/internal/backend` read
- New `backend.Client.GetCurrentPolicy(ctx, agentID) (*Policy, error)` — `GET /agent/<agentID>/policies/current`, parses the `{status, data: PolicyEntity|null}` envelope (data==null → no policy → nil, nil). Reuses the existing auth auto-classification (org `obx_key_` → `X-API-Key`; else Bearer JWT + `x-openbox-client`).
- New `openbox dev sync` verb (and `dev init`'s last step): resolve `OPENBOX_CONTROL_TOKEN` + `OPENBOX_BACKEND_URL` + the **agent id** (persisted at init — see §6 — or `OPENBOX_AGENT_ID`), fetch, translate `config.policy_builder` → a `sidecar.Bundle{PolicyBuilder, PolicyID:id, UpdatedAt:updated_at, Version:…}`, write it to `sidecar.DefaultBundlePath()` (or `--bundle`/`OPENBOX_SIDECAR_BUNDLE`), 0600. A raw-rego-only policy (no `policy_builder`) → write a bundle that carries the PIN + a `raw_rego_unlocalized:true` flag so the evaluator serves fail-open-local and the CLI prints a non-secret warning (residual, ADR-0005 §Decision-2). `data==null` → write an empty (no-policy) bundle (allow). Never print the org key/rego (INV-1). Exit non-zero on fetch/auth failure with a mapped hint (SL-10 style).

### 5. Session-start staleness check (`adapters/claude-code`)
- On **SessionStart**, best-effort: resolve `OPENBOX_CONTROL_TOKEN` + backend URL + agent id + the local bundle PIN. If all present, `GetCurrentPolicy` and compare `(id, updated_at)` to the PIN.
  - **Match / can't-determine (no org key in the hook env, offline, fetch error) →** proceed on the last-good bundle; never deny at fetch time (ADR-0005 §Decision-3).
  - **Mismatch, fail-open (default) →** emit a non-secret warning to stderr AND (SessionStart can add context) surface "OpenBox policy changed — run `openbox dev sync`" via the SessionStart `additionalContext` stdout channel; proceed stale.
  - **Mismatch, fail-closed →** write a per-session **stale marker** (a small local file keyed by session id, e.g. under the SL-5 session registry dir or `~/.config/openbox/stale/<session>`), content-free. The **PreToolUse enforce gate** checks the marker and, when present, DENIES (reusing `applyDecision`'s deny path with a "stale policy — run `openbox dev sync`" reason) until the marker is cleared. `dev sync` clears markers. (CC has no SessionStart "deny session" primitive; the block is realized where enforce already has teeth. Closes the under-enforcement window without a new CC mechanism.)
- SessionStart must NEVER block a session or emit a non-additionalContext stdout in fail-open, and must be fully fail-safe (any error → proceed). It runs off the tool hot path.

### 6. Persist the agent id at init (`cli/internal/devinit` + `DevConfig`)
- `dev init` already has `Registration.AgentID`; persist it to `dev.json` as `agent_id` (non-secret) so `dev sync` + the staleness check can read it. `OPENBOX_AGENT_ID` overrides. Add `DevConfig.AgentID` + a `ResolveAgentID()` resolver (env then config; no secret I/O).

### 7. Retire the background poll (`sidecar/sync.go`, `sidecar/serve.go`)
- The daemon does zero network I/O; the 60 s `syncLoop` is no longer the freshness mechanism. Keep `FileBundleSource` as the loader and its mtime-gated prime, but make the periodic ticker inert (or a long/disabled interval) so an unchanged file is not re-polled needlessly. `--sync-interval` stays accepted (no-op/local-file-remtime only) for back-compat; document the change. Do NOT remove `FileBundleSource` (it is how the daemon loads what `dev sync` wrote).

## Scope boundary (what this story IS and is NOT)
- **IS:** `sidecar/builder.go` (native builder evaluator, first-match); `BuildOPAInput` reshape to `buildSpanMap` parity; `Bundle` PIN + `PolicyBuilder` carrier + `SetBundle` evaluator selection; `backend.GetCurrentPolicy`; `openbox dev sync` verb + `dev init` last-step fetch; persist `agent_id`; session-start staleness check + PreToolUse stale-block for fail-closed; retire the poll; conformance tests (§ below). ADR-0005 + this story + ledger.
- **IS NOT:** embedding a rego engine (ADR-0005 — not now, seam preserved); raw-rego local evaluation (documented residual); behavior rules / multi-span (`AGEResult`); guardrails redaction (`[EXT-guardrail-redaction]`); AGE/goal-drift; Tier-2 sync `/evaluate` (E6-S10); Tier-3 findings loop (E6-S11); the E6-S2/S3/S4/S6/S9 apply cascade (UNCHANGED — this story only changes how the Evaluation is PRODUCED and the bundle is SOURCED); a server-side 409/426 staleness backstop (OD-SYNC-5 deferred). **NO openbox-core or openbox-backend code change** (reuse the existing read endpoint; CLAUDE.md reuse rule).

## Acceptance Criteria
1. **Native builder verdict parity** — for a set of fixture `policy_builder` configs + fixture `DecisionRequest`s covering all 9 operators, both `valueType`s that matter, `count` transform, `all`/`any`, `[_]` existential, and first-match precedence, `builderEvaluator` yields the verdict the backend's compiled rego + core's decision→action switch would (`decisionToVerdict(rule.decision)`; no-match→ALLOW). First-match (earlier rule wins over a later higher-severity rule) is asserted explicitly.
2. **Input-shape parity (load-bearing)** — `BuildOPAInput` emits the field NAMES core's `buildSpanMap`/`buildOPAInput` produce for a file read, file write, mcp tool call, and shell command; `command` appears under `attributes`, not as a top-level span key; no `tool_kind` top-level span key. A builder policy keyed on `spans[_].file_path` / `spans[_].semantic_type` / `attributes.command` fires exactly as it would in core.
3. **`dev sync` fetch + translate + PIN** — `openbox dev sync` fetches `GET /agent/<id>/policies/current` with the org key, parses the `{status,data}` envelope, translates `config.policy_builder` into a local bundle carrying the PIN `(id, updated_at)`, and writes it 0600 to the bundle path; the daemon then loads it and enforces it. `data==null` → an allow (no-policy) bundle. A raw-rego-only policy → a fail-open-local bundle + a non-secret CLI warning. Auth/fetch failure → non-zero exit + a mapped hint. The org key and rego text are NEVER printed/logged (INV-1).
4. **Session-start staleness — fail-open** — a backend policy newer than the local PIN, fail-open: SessionStart warns (stderr + additionalContext "run `openbox dev sync`") and proceeds on the stale bundle; NO deny attributable to staleness; `dev sync` re-pins so the next session matches.
5. **Session-start staleness — fail-closed** — same mismatch, fail-closed: SessionStart writes a content-free stale marker; the next PreToolUse enforce gate DENIES ("stale policy — run `openbox dev sync`"); after `dev sync` the marker is cleared and tools proceed. A test drives the marker→deny path and the clear→proceed path.
6. **Offline / no-key never denies at fetch time** — a `dev sync` fetch failure keeps the last-good pinned bundle (never denies at fetch); a SessionStart with no org key in the environment / an unreachable backend proceeds on the last-good bundle (never blocks), under BOTH policies. Decision-time daemon faults still follow E6-S3/S7 (unchanged).
7. **Daemon does zero network I/O (INV-3b)** — the decision path (`Server.decide`, `sidecar.Client`) makes no network call; the background poll is retired; a test/inspection confirms the daemon loads only from the local file. The only network calls in this story are the CLI `dev sync` and the adapter SessionStart check — both off the tool hot path.
8. **Apply cascade unchanged / tighten-only preserved** — `mapVerdict`/`applyDecision`/`applyFailurePolicy`/`sidecar.Client.Decide` logic is untouched; a builder ALLOW/no-match proceeds byte-identically to observe; only BLOCK/HALT→deny, REQUIRE_APPROVAL→ask, and the staleness fail-closed deny ever tighten. Enforce OFF (default) → the whole story is inert (byte-identical to E6-S9).

## Write Scope
- `sidecar/builder.go` (new) — `PolicyBuilderConfig`/`PolicyBuilderRule`/`PolicyBuilderCondition` types; `builderEvaluator` (first-match); field-path resolver (`input.` prefix + dotted + `[_]` existential); operator/transform/valueType engine mirroring recon C.
- `sidecar/builder_test.go` (new) — AC-1 (all operators/transforms/matchModes/`[_]`/first-match/default) + `-race`.
- `sidecar/input.go` — reshape `BuildOPAInput` to `buildSpanMap` parity (AC-2).
- `sidecar/bundle.go` — `Bundle`: add `PolicyID`, `UpdatedAt`, `PolicyBuilder *PolicyBuilderConfig`, `RawRegoUnlocalized bool`; extend `validate`/`ParseBundle` (DisallowUnknownFields still holds).
- `sidecar/server.go` — `SetBundle` selects `builderEvaluator` when `PolicyBuilder != nil` (else legacy `bundleEvaluator`/cold-start); expose the PIN for staleness telemetry if needed. `decide`/secret-redaction path UNCHANGED.
- `sidecar/sync.go`, `sidecar/serve.go` — retire the periodic poll (keep `FileBundleSource` + its prime); document.
- `cli/internal/backend/client.go` — `Policy` type + `GetCurrentPolicy(ctx, agentID)` (envelope + null handling).
- `cli/internal/devinit/devinit.go` — persist `agent_id` to `dev.json`; call the policy fetch as `dev init`'s last step (best-effort — a fetch failure warns, does not fail init).
- `cli/cmd/openbox/main.go` — `dev sync` verb (fetch → translate → write bundle + PIN); wire into `runDev`; usage text.
- `adapters/claude-code/creds.go` — `DevConfig.AgentID` + `ResolveAgentID()`; a `ResolveControlToken()`/backend-URL resolver for the staleness check (no secret store — the org key is a control-plane env, not the runtime secret).
- `adapters/claude-code/staleness.go` (new) — the session-start compare + stale-marker write/read/clear; the PreToolUse stale-block hook-in.
- `adapters/claude-code/hookrun.go` — call the staleness check on SessionStart; check the stale marker in the PreToolUse enforce branch (deny when set + fail-closed).
- `adapters/claude-code/enforce_conformance_test.go`, `enforce_test.go`, `staleness_test.go` (new), `cli/.../*_test.go`, `sidecar/*_test.go` — the ACs.
- `.fab7/sdlc/design/adr/ADR-0005-…md` (written), `.fab7/sdlc/design/sidecar-policy-sync.md` (mark OD-SYNC-1 superseded by ADR-0005; note the `data.<pkg>.result` correction), `.fab7/sdlc/stories/E6-backlog.md` + `.fab7/sdlc/status.yaml` (E6-S8 DONE).

## Invariants
- **INV-1 (secrets):** the org control-plane key + the fetched `rego_code`/policy body are read straight into the client and NEVER logged/printed/placed on argv; the credential comes from env only, never a flag.
- **INV-2 (metadata-only egress):** this story adds NO egress. The policy read is an INGRESS (backend→CLI). `BuildOPAInput`/`DecisionRequest` stay metadata-only; the stale marker is content-free (session id only). No tool content is introduced anywhere new.
- **INV-3 / INV-3b:** the daemon's decision path stays network-free; SessionStart/`dev sync` network calls are off the tool hot path. Enforce OFF → inert. SessionStart in fail-open never blocks and never writes non-additionalContext stdout.
- **Tighten-only (E6-S2):** only deny/ask (incl. the staleness fail-closed deny) ever tighten; never emit `allow`.
- **OD9 fail-open default:** every fault (fetch, offline, no key, malformed policy, daemon absent) proceeds under fail-open; fail-closed denies only on a real outage/stale condition, never on a real allow.

## Validation commands
```
# build + vet + race across all three modules
(cd sidecar && go build ./... && go vet ./... && go test -race ./...)
(cd cli && go build ./... && go vet ./... && go test -race ./...)
(cd adapters/claude-code && go build ./... && go vet ./... && go test -race ./...)
# conformance
(cd adapters/claude-code && go test -race -run Conformance ./...)
# whole binary builds (OD17 single binary; confirm no cgo / no OPA pulled in)
(cd cli && CGO_ENABLED=0 go build -o /tmp/openbox ./cmd/openbox && echo OK)
go list -m all 2>/dev/null | grep -i open-policy-agent && echo "FAIL: OPA pulled in" || echo "OK: no OPA dep"
```
Plus a **live E2E** (per repo convention): boot `openbox sidecar serve` against a bundle written by a real/faked `dev sync`; drive `openbox hook claude-code PreToolUse` for a tool that a fixture builder policy BLOCKs (→deny), ALLOWs (→proceed), and REQUIRE_APPROVALs (→ask); flip a policy version and confirm fail-open warns+proceeds / fail-closed denies-until-`dev sync`; assert no secret/rego/key in any stdout/stderr/audit/egress.

## Stop conditions (HALT + surface to brian; do not guess)
- If `BuildOPAInput`/`buildSpanMap` parity cannot be achieved for a tool kind without a core change → STOP (a core change needs an ADR + is out of scope).
- If the backend envelope or `policy_builder` shape differs from recon C in a way that changes evaluator semantics → STOP and report (the recon is the oracle; a divergence is a parity risk).
- If closing the fail-closed staleness block genuinely requires a CC SessionStart "deny session" primitive that does not exist → the PreToolUse stale-block is the sanctioned realization (ADR-0005); do not invent a new CC mechanism.
- Any need to touch openbox-core/openbox-backend source → STOP (reuse-only; ADR required).
