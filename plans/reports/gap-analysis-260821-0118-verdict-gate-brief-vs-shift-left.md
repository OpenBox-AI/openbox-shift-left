# Gap analysis — "OpenBox Verdict Gate for Claude Code" brief (OB-CC-GATE-v1.0) vs openbox-shift-left

**Date:** 2026-08-21 · **Repo state:** `main` @ `d27efbb` · **Brief:** OB-CC-GATE-v1.0, 2026-08-21, owner Tahir
**Method:** clause-by-clause read of the brief against code (file:line cited), decision records, and the prior
verification report `plans/reports/verification-260820-1854-engineering-brief-claims.md` (executed-proof facts
reused).

## TL;DR

The brief specifies, as a ~6–7-week greenfield build, a system that is **mostly already shipped in this repo** —
often in a more evolved form than the brief asks for. The brief appears unaware of this: it cites
`openbox-shift-left` only as "the commit pipeline" (§5.5) and proposes a new shim + a new Gate API service, which
would violate this repo's core principle (reuse the existing `/evaluate` pipeline; new endpoint/service ⇒
decision record).

- **~M1 and most of M2 are done.** PreToolUse gating with deny/ask, prompt gating, fail-closed capability,
  redact-before-send, approval hold + auto-resume, idempotent delivery, offline spool, commit-trailer lineage,
  subagent coverage, monitor/enforce modes, conformance + real-session testbed.
- **Genuine gaps** (the real work): client-visible receipt chain + session seal + `openbox verify`; Slack approval
  surface; hash-preserving redaction / `input_hash`; shipped default policy pack incl. self-protection; signed
  binaries/SBOM; latency SLOs.
- **Five brief decisions conflict with shipped owner decisions** (fail-closed default, Mode-A local approval,
  new Gate API, hashed-inputs default, keychain). These are OD items for the owner, not silent adoptions.
- Most of **M0 is already answered empirically** by this repo's probes/conformance; 3 items remain genuinely open.

## 0. Concept mapping (brief term → existing reality)

| Brief | Exists as |
|---|---|
| `openbox-cc-hook` shim | plugin engine `bin/openbox` + `adapters/claude-code` on shared `hookflow` engine |
| Gate API `/v1/gate/evaluate` | `POST /api/v1/governance/evaluate` (openbox-core), `client/client.go:17` |
| Gate API `/v1/gate/sessions` | implicit: machine agent via `POST agent/create` (auth), session = child record keyed by native `session_id` |
| `GET /v1/gate/approvals/{id}` long-poll | `client.PollApproval` + bounded hold (`hookflow/approvalhold.go`) + rewake watcher |
| `/v1/gate/events` batched ingest | spool + `RealtimeTrigger` debounced flusher (`hookflow/spool.go`, `realtime.go`) |
| receipts (hash-linked leaves) | `governance_events` + Merkle leaves, server-side in core; client sees none |
| `policy_rule_id` | `Evaluation.PolicyID` (surfaced in reasons + enforcement audit) |
| monitor / enforce | observe (`enforce:false`) / enforce (default ON since) |
| `idempotency_key` | `Idempotency-Key` header (INV-5, `client/client.go:259-262`); core dedupes (Redis, 24h, fail-open; shipped `911b67f` 2026-07-29) |
| org token | `Bearer obx_` **plus AIP Ed25519 request signing** (`client/signing.go`) — stronger than brief |
| `OpenBox-Session:` trailer (§5.5 "[A] small change") | **shipped**: `adapters/common/git/trailer.go:14` + signed attestation via git notes + deploy join + read side |

## 1. Already shipped (brief asks; repo has — with deltas noted)

1. **Hook coverage.** Brief registers 5 events; repo registers **11** (`localhooks.go:57-77`): SessionStart,
   UserPromptSubmit, PreToolUse (+ async rewake watcher), PostToolUse, PostToolUseFailure, Stop, SubagentStop,
   SubagentStart, PermissionDenied, StopFailure, SessionEnd. Local-scope and plugin bundle pinned together by test.
2. **Decision output contract** (brief §4.2, graded [C]). Implemented and conformance-pinned on real stdout bytes:
`hookSpecificOutput.permissionDecision` deny/ask + `permissionDecisionReason` (`outputcontract.go:41-70`).
Beyond the brief: HALT renders `continue:false` + stopReason (session kill) and a local halt latch refuses
every later gated hook in that session.
3. **ALLOW = defer** (brief decision 2). Proceed path writes *nothing* — byte-identical to observe
   (`Render` returns empty; tighten-only invariant in `hookflow/enforce.go:39-62`). Exactly the brief's rationale,
   already enforced structurally. No signed explicit-allow path exists (brief's "use sparingly" row) — arguably fine.
4. **Gate everything, decide server-side** (brief decision 3). That decision: no local verdicts, no client pre-filtering,
   matcher `*`, every gated class evaluated by `/evaluate` (`gate.go:129-139`).
5. **`updatedInput` rewriting** — brief defers to v1.1 as unverified [C]; repo **uses it today** for
   redact-and-continue (`enforce.go:330-393`), with structural-fields-preserved guarantees.
6. **Client-side redaction before egress** (brief §6 intent). `decision/` secret detection redacts the body
   *before* attach; ordering asserted on outbound bytes (conformance C18). Delta: placeholders are
   `${OPENBOX_REDACTED_<CAT>}` (`decision/secrets.go:44`), **not** hash-preserving HMAC — see gap G3.
7. **Fail-closed enforcement** (brief §4.3 core differentiator). Capability shipped: `ApplyFailurePolicy` runs
strictly *after* evaluation (C1 pinned), synthesizes HALT with content-free reason, both branches
conformance-tested. Deltas: **default is fail-open** (`failurepolicy.go:11-17`) — see conflict D1; no persisted
circuit-breaker/degraded state (per-call policy only) — see gap G6.
8. **Approvals** (brief §7). REQUIRE_APPROVAL → request filed by control plane → bounded hold
(default 20s, 500ms poll, clamped to remaining hook budget — `approvalhold.go:37-42,116-121`) → decided
(approve→proceed, reject→deny) or undecided→**deny with approval ref** → **rewake watcher** (async, 2700s,
`rewake.go:23`) wakes the session when the decision lands. This is the brief's Mode B + §7.3 fallback, already
engineered around the hook-timeout ceiling the brief flags as an M0 risk — without depending on a 300s hook
timeout. Plus the **autonomous approver** (that decision, `openbox approve`/`approveauto`) the brief doesn't
have. Delta: approval surface is dashboard/CLI, not Slack — gap G2.
9. **Prompt handling** (brief §4.4 wants hash-and-record). Repo goes further: UserPromptSubmit **gates**
(that decision, same EnforceGate; block/erase, hold, HALT→session stop) and prompt text is captured
under `content_capture` (default ON). Delta on hashing posture — conflict D4.
10. **Outcome telemetry** (brief §4.4 PostToolUse). Shipped richer: tool ActivityCompleted with
    `status: completed|failed`, PostToolUseFailure/PermissionDenied/StopFailure signals, token usage +
    model per turn, one content span per turn for alignment.
11. **Offline queue** (brief §4.4). Session spool (O_APPEND JSONL, atomic line writes, `spool.go:41-54`),
    recovery with `MaxRecoveryAttempts=5`, near-real-time debounced flusher (`OPENBOX_REALTIME=0` opt-out).
    Delta: no fsync-before-ack (brief wants fsync'd; minor).
12. **Session lifecycle.** SessionStarted carries `metadata.cwd` (`mapper.go:643`) etc.; SessionEnded flushes.
    Delta: no git remote/branch/OS-user fingerprint on session events (branch/repo ride commit/deploy lineage
    events only, `payload.go:740-742`) — small gap G10.
13. **Subagent coverage** (brief §12). SubagentStart/SubagentStop wired; turn cursor partitions main/subagent.
14. **Enterprise distribution** (brief §4.1). Managed-config posture layer with `locked` fields +
    `deploy/managed/` profiles; global scope prints the managed-settings snippet (that decision scope semantics).
    Caveat: SL-1 (proven) — URL fields ignore org locks; see gap G4b.
15. **Idempotent evaluate** (brief §5.2). Done both halves — client `Idempotency-Key` + core dedupe
    (before-workflow, Redis-backed, 24h TTL, fail-open on cache miss). Residual lost-200 window documented.
16. **Testing** (brief §12). Conformance C1–C31 assert real `RunHook` stdout bytes over HTTP stubs (deny/allow,
    redact-before-send, both failure-policy branches); `testbed/` drives real headless sessions against a real
    stack (dormant assertions waiting on a stack run). 11 modules green under `-race` + 2 cross-compiles.
17. **Commit linkage** (brief §5.5). Shipped and stronger: trailer (claim) + signed attestation via git notes
    (proof) + deploy lineage + queryable read side. Brief's version (env var + trailer only) would be a regression.
18. **HTTP-hook vs command shim** (brief decision 1). Same conclusion already embodied: command hooks own
    fail-closed; no reliance on the native HTTP hook type.

## 2. Decision conflicts — brief vs shipped owner decisions (surface, don't adopt)

| # | Brief says | Repo/owner decided | Status |
|---|---|---|---|
| D1 | `enforce` defaults **fail-closed**; "the differentiator" | Default **fail-open** (that decision mitigation: `fail_closed` stays off; OD-E8-1 unratified). Worse: shipped managed profile **locks** `fail_closed:false` making it unreachable for that fleet (SL-2, proven) | Direct conflict. Owner call. If the brief's positioning stands, minimum change = unlock in `deploy/managed/openbox/dev.json`; default flip = ratify OD-E8-1 |
| D2 | Mode A: REQUIRE_APPROVAL → `ask` (local dev decides), ship first | **Rejected** (OD-E9-1, E9 §3.7): once filed, "the only principal that may answer it is an approver" — dev approving own request isn't four-eyes; undecided **denies** (`approvalhold.go:145-166`) | Conflict. `ask` exists only as an unreachable backstop literal (`outputcontract.go:29`) |
| D3 | New **Gate API** service + `/v1/gate/*` schema | Core principle: reuse `/evaluate`, same tables, same auth; new endpoint/service ⇒ decision record. All five proposed endpoints have existing equivalents (§0 table) | Conflict with repo's reason to exist. The brief's [A] "reconcile with existing contracts" resolves to: they exist, use them |
| D4 | `store_inputs: hashed` default; raw input never leaves host | Content capture **ON by default** (owner, 2026-07-15; memory pin: don't re-litigate metadata-only). Gated calls egress redacted content ≤64KB — "OpenBox cannot decide on content it cannot see" | Conflict. Brief §6 re-litigates a settled posture. Note: brief's own policy pack (curl-pipe-sh detection, §8) *requires* content visibility its default denies the server |
| D5 | Token via env "or OS keychain ref" | Keychain **deleted, not demoted** : plaintext `~/.openbox/.env` 0600, argued against the prior rationale | Conflict if read as a requirement; agrees with repo if env-only |
| D6 | 300s PreToolUse hook timeout bounds approvals | 30s hook + 20s hold + 2700s async rewake (`enforce_evaluate.go:31`, `rewake.go:23`) — deny-and-auto-resume, no dependence on an unverified ceiling | Different architecture, repo's is strictly safer; brief's §7.3 fallback is the repo's *primary* path with automation on top |

## 3. Genuine gaps (brief asks; nothing shipped) — with owning repo

- **G1 — Evidence UX: client-visible receipt chain, session seal, offline verify.** No `leaf_hash`/`prev_hash`
  on evaluate responses, no `POST …/seal`, no anchored-root printout at SessionEnd, no `openbox verify`
  (SL-8: confirmed absent; owner retired the external claim). Core stores Merkle leaves, but the *productized*
  T₀→T₁ verification ritual — the brief's acceptance demo and sales demo — does not exist. Owners: core/backend
  (seal+anchor+read API), this repo (SessionEnd seal call + `verify` CLI). Largest genuine delta.
- **G2 — Slack slice.** No Slack Attestor, no Block Kit approvals, no `/openbox status`. Approval surface today =
  dashboard + autonomous approver. Owner: backend/new Bolt app; this repo unchanged (hold/rewake already poll
  whatever decides).
- **G3 — Redaction upgrades.** No hash-preserving HMAC placeholders (reuse detection), no `input_hash` over
  unredacted input, no `none|hashed|encrypted` tiers. Repo has category placeholders + INV-2 allowlist. Owner:
  this repo (`decision/`), but D4 must be settled first.
- **G4 — Default policy pack incl. self-protection.** No shipped pack at all ("policy template packs" =
declared Next). Brief's §8 table (destructive FS, secrets touch, fetch-and-run, protected-branch push,
deploy/migrate, manifest edits) is expressible in org rego today but nothing ships it. Self-protection
(BLOCK edits to `.claude/settings*`, hook binary, `.git/hooks`; alert on attempt) is absent AND honestly
disclaimed (that decision: origin-of-config, not tamper-resistance). **G4b:** SL-1 (proven) — `base_url`/
`backend_url` ignore org locks, so the governed agent can re-point the control plane; that is the live
self-protection hole to fix first, and it's already top-priority from the prior brief.
- **G5 — Supply chain.** No release signing, no SBOM, no govulncheck, checksums only (SL-3, proven); no
  signed/reproducible-build claim; installer doesn't verify signatures. Owner: CI/.goreleaser.
- **G6 — Circuit breaker / persisted degraded state.** Each hook process independent; failure policy per-call.
  (Halt latch exists but only for server HALT.)
- **G7 — Latency SLOs + soak.** No p95/p99 targets, no 10k-call soak, no RSS budget. Current per-call evaluate
  budget 3.5s (`evaluate.go:23`) vs brief's 1.5s client budget / 250ms server p95.
- **G8 — AIVSS on the verdict wire.** `cli/internal/aivss` pins *registration* risk posture; evaluate responses
  carry PolicyID/reason, no score/vector. Owner: backend (if wanted at all).
- **G9 — Slack-origin session metadata** (M4). Absent; brief itself grades the mechanism unverified.
- **G10 — Session host fingerprint.** git remote/branch/HEAD/OS-user not on SessionStarted (cwd is).
- **G11 — `--dangerously-skip-permissions` live proof.** `permission_mode` is parsed (`hookevent.go:108`) but
  no test/doc verifies a deny holds under bypass mode. Genuinely open here too; cheap testbed addition.

## 4. Repo capabilities the brief lacks (would be lost if built to spec)

Provider-agnostic engine + **Codex adapter** (brief is CC-only); session **HALT kill + latch**; prompt
**gating** (not just hashing); autonomous approver; token usage/model finops telemetry; tool outcome +
failure signals + alignment span; goal-alignment feed; AIP request signing; installer stale-engine sweep +
`openbox doctor` duplicate-engine diagnostics; Windows cross-compile; `attest`/lineage read side; managed
posture layer with provenance (`decision_authority` on the wire).

## 5. Brief's M0 checklist — already answered vs still open

Answered by shipped code/probes (`plans/reports/probe-260813-2329-claude-code-hook-surface.md`, conformance,
installed 2.1.229 probe): stdin schemas incl. `permission_mode`; decision JSON field names (Q3, §2.2);
matcher semantics; `SessionEnd` vs `Stop` (both registered, distinct duties); `updatedInput` support (Q5 — in
production use); statusMessage; older-CC unknown-hook-key tolerance; hold-open approval pattern viability
(superseded by hold+rewake); env available to hooks (plugin uses `${CLAUDE_PLUGIN_ROOT}`).
**Still open:** Q — deny under `--dangerously-skip-permissions` (G11); max hook `timeout` ceiling (moot for
gating given rewake, still worth recording); managed-settings paths per OS as a rollout kit doc (Q8);
HTTP-hook fail-open confirmation (academic — command shim already chosen); Slack-hosted sessions loading
managed hooks (Q1, external); code-channel APIs (Q6/Q7, external).

## 6. Milestone re-estimate against reality

| Brief milestone | Actual status |
|---|---|
| M0 (2–3d) | ~80% answered; residue = G11 test + per-OS managed paths doc + external Slack questions |
| M1 gate MVP (2wks) | **Shipped** except default policy pack (G4) — which is backend/template work |
| M2 approvals (1.5–2wks) | Plumbing shipped stronger; remaining = Slack Attestor slice (G2) + signed-decision receipt (backend) |
| M3 evidence & distribution (1.5wks) | Lineage shipped; remaining = seal/anchor/verify (G1, mostly core/backend), signing/SBOM (G5), threat-model doc (partial in `docs/architecture.md#assurance`), bypass battery (partial; testbed run pending) |
| M4 Slack enrichment | Absent, externally gated — unchanged |

Net: the brief's ~6–7 dev-weeks buys mostly things that exist. Re-scoped to true gaps: G1 (the demo-able
evidence ritual) + G2 (Slack slice) + G4 (policy pack + SL-1 fix) + G5 (signing) ≈ 2–3 weeks across repos,
*after* the owner settles D1–D5. Caveat: testbed still hasn't run against a live stack — several shipped
claims above are unit/conformance-verified only, per CLAUDE.md status notes.

## Unresolved questions

1. D1 — does the fail-closed-by-default positioning win over that decision's fail-open default (and OD-E8-1)?
   Determines G6's priority too.
2. D3 — does the owner want the `/v1/gate/*` naming as a public facade over existing endpoints, or is the brief
   simply to be corrected to the existing contract?
3. D4 — hashed-inputs default vs full-capture posture: settled 2026-07-15, but the brief re-opens it for
   "regulated customers"; is an `encrypted`/`hashed` tier wanted as an *option* (not default)?
4. G1 — is session seal/anchor core work already planned anywhere (backend roadmap), or net-new?
5. Whether Mode-A-style local `ask` should exist at all for orgs with no approver configured (today: undecided
   denies after 20s + rewake) — UX question for pilots.
6. Who owns the Slack Attestor (new repo vs openbox-backend module)?
