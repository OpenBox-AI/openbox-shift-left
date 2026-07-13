# STORY-SL-9 — Advisory-tier verdict & guardrail consumption (record, never block)

**Risk:** medium (touches the shared client return shape + both emit callers; must NOT cross INV-3 into blocking)

## Source
- **Architecture:** `architecture.md` §1b "Derived governance level" — the middle tier **Advisory** ("+ guardrail/policy signals recorded but not blocking"), between Observe and Enforce; D7 (verdict handling); INV-3 (observation never blocks).
- **SDK parity target:** `openbox-temporal-sdk-python` — `types.py` `GovernanceVerdictResponse` (the rich fields: `trust_tier`, `risk_score`, `alignment_score`, `behavioral_violations`, `constraints`, `policy_id`, `approval_id`, `guardrails_result`), `verdict_handler.py` `enforce_verdict()` (the semantics we *record* instead of enforce).
- **Session:** SDK↔shift-left gap analysis (2026-07-13) — bucket #2 centerpiece; enforcement (the `apply` leg) is explicitly the deferred *next* increment, NOT this story.

## User Value
OpenBox's verdict + guardrail signals for each dev-runtime event are captured and made inspectable locally — the "Advisory" governance tier — so an org can see *what would be enforced* (blocks, constraints, guardrail hits, risk/trust) before any enforcement is switched on, with zero risk to the developer's tool call.

## Inlined context (verified — builder need not re-read)
- **The 5-tier verdict enum + wire parsing already EXISTS** (`client/verdict.go`: `VerdictAllow/Constrain/RequireApproval/Block/Halt`, `wireToVerdict`, `legacyActionToVerdict`, `parseVerdict`). This story does **not** re-add tiers — it consumes the result and the rich sibling fields the current `verdictResponse` struct drops.
- **Today the verdict is discarded:** `adapters/claude-code/adapter.go:63` does `_, err := em.Emit(ctx, ev)`. `actions/openbox-git-action/action.go:79` already captures `verdict` but does little with it.
- **`Emit` signature (`client/client.go:131`):** `Emit(ctx, ev) (Verdict, error)`, fail-open `(VerdictUnknown, nil)` on any transport failure (INV-3). `verdictResponse` (`client/verdict.go`) currently parses only `verdict` + legacy `action`.
- **SDK's rich response fields** to mirror (all forward-compatible — ignore if core omits): `trust_tier` (int), `risk_score`/`alignment_score` (float), `behavioral_violations` ([]), `constraints` ([]string), `policy_id`, `approval_id`, `guardrails_result{ validation_passed bool, reasons[] }`.
- **Guardrail reasons are categories, not content** (e.g. `{type:pii, field:email}`) — safe under INV-2. Do NOT capture `redacted_input`/content bodies (that is the Phase-2 enforcement/redaction-application story, out of scope here).

## Acceptance Criteria
- **Shared refactor:** `Emit` returns a richer `Evaluation` value (`{ Verdict; TrustTier; RiskScore; AlignmentScore; Constraints []string; PolicyID; Guardrail *GuardrailResult{Passed bool; Reasons []GuardrailReason}; ... }`), parsed from the `/evaluate` response; unknown/absent fields degrade cleanly (never error). Both callers updated (adapter, git-action); git-action keeps current behavior.
- **Advisory recording:** on flush, when the evaluation is non-`ALLOW` OR carries any guardrail hit / constraint / non-trivial risk, the adapter writes a structured **advisory record** — `{event_id, session_id, event_type, verdict, would_block bool, trust_tier, risk_score, constraints, guardrail_reasons, ts}` — to a local sink (`~/.config/openbox/advisories.jsonl`, mirroring the spool/recovery pattern; overridable) plus one stderr summary line.
- **`would_block`** is computed from the recorded verdict (BLOCK/HALT → true) purely as a *label* — it drives no control flow.
- **INV-3 preserved (the load-bearing AC):** a `BLOCK`/`HALT` verdict produces an advisory record **and** the hook still exits 0 with empty stdout; nothing is denied, delayed past budget, or errored. Prove with a test that feeds a BLOCK response and asserts (record written) ∧ (exit 0, empty stdout).
- **Metadata-only (INV-2):** advisory records contain only categories/ids/scores — no prompt/command/file/output content, no `redacted_input`.
- Advisory recording is **off the hot path**: it happens during flush (SessionEnd/`flush`), never on Pre/PostToolUse spool.

## Nonfunctional Requirements
- **security:** advisory sink is local, metadata-only (INV-2); no secret (INV-1). Reuse the spool file's perms posture.
- **reliability:** advisory write is best-effort — a sink-write failure logs and is dropped, never fails the flush or the tool call (INV-3).
- **compatibility:** the `Evaluation` parse must not break on a core response that omits the rich fields (Phase-1 core may only send `verdict`).

## Write Scope
- `client/` — expand `verdictResponse`/add `Evaluation` (`verdict.go`), change `Emit` return (`client.go`).
- `adapters/claude-code/` — new `advisory.go` (record sink) + wire into the flush path (`adapter.go`).
- `actions/openbox-git-action/` — update the `Emit` call site to the new return (record a deploy advisory too).

## Dependencies
- **Hard:** STORY-SL-3 (client), STORY-SL-4 (adapter flush path).
- **Soft:** STORY-SL-10 (shared client edits touch `client.go`; sequence SL-10 first to avoid a rebase).
- **External (assumed-satisfied):** [EXT-core] — core returns the rich advisory fields for dev events; until then only `verdict` is populated and records still form (degraded but valid).

## Invariants
- **INV-2:** advisory records are metadata/categories only.
- **INV-3:** consumption is record-only; never blocks, delays, or errors a tool call; hook exits 0 / empty stdout.
- **INV-1:** no secret in records.

## Human Gates
| Gate | Question | Owner | Evidence Needed | Allowed Outcomes |
|---|---|---|---|---|
| G1_READY | Bless the **Advisory tier** as an explicit Phase-1.5 increment (record-not-block) + confirm the advisory sink location/format | brian (product) | this story + a one-line architecture §1b note | confirm / revise |
| G3_REVIEW | Is the consumption strictly record-only (INV-3 intact) and the `Evaluation` parse forward-compatible? | brian | diff review + the BLOCK→exit-0 test | approve / revise |

## Validation
```bash
cd client && go build ./... && go vet ./... && go test ./...   # Evaluation parse: full-field fixture + verdict-only fixture (degraded)
cd adapters/claude-code && go build ./... && go vet ./... && go test -race ./...
# key test: a mock /evaluate returning verdict=block + a guardrail reason →
#   (1) an advisory record is written with would_block=true and the guardrail category
#   (2) RunHook still exits 0 with EMPTY stdout and denies nothing (INV-3)
#   (3) no content/secret substring in the record (INV-1/INV-2)
# live check: drive a real Claude Code session against the running stack; confirm an advisory
#   record appears carrying the verdict/trust_tier and that nothing blocked.
```

## Stop conditions
- If honoring a verdict would require making Pre/PostToolUse **synchronous** or returning a non-zero exit / non-empty stdout → STOP: that is enforcement (the deferred next increment, gated on spike S2 / OD9), not this story. Advisory is strictly record-only.
- If core returns a response shape that can't be parsed into `Evaluation` without breaking the existing verdict parse → keep `parseVerdict` as the fallback and record verdict-only; route a note, don't force a schema.
