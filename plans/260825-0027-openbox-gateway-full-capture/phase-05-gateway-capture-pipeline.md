# Phase 05 — Capture, identity, and account evidence

## Context links

- Parent: [plan.md](plan.md) · Previous: [phase-04](phase-04-gateway-passthrough-core.md)
- Core wire: `openbox-core/internal/content/governance.go:276-304` (SpanData — verified live)
- Classification: `openbox-core/internal/content/session.go:451-476`
- Account design: [advise-260825-0236](../reports/advise-260825-0236-local-gateway-detection-tier.md) req 5–6
- Depends on: 04, P1's answer (org-id matchability)

## Overview

- Date: 2026-08-25 (local-gateway revision)
- Description: turn the passthrough tee into `SpanData` on the existing pipeline — headers,
  bodies, session/agent identity — attach **account evidence** (credential fingerprint +
  local account metadata), and retire ADR-0018's fabricated span attributes where the span
  is genuinely observed.
- Priority: P1
- Implementation status: pending
- Review status: not reviewed

## Key insights

- **The receiving contract already exists.** Core's `SpanData` carries `request_headers`,
  `response_headers`, `request_body`, `response_body`, `http_*`. Verified against the
  sibling repo this cycle. The Python SDK's `to_core_span_data()` is the field discipline
  to copy.
- **Fingerprint-then-redact, in that order, at capture.** The credential fingerprint
  (one-way SHA-256 of the bearer/x-api-key value) is computed from the header BEFORE the
  key-name redaction that strips it. It is derived evidence, like `status` — but unlike
  `status` it derives FROM a secret, so the raw value's absence from outbound bytes gets
  its own conformance case, not an assumption.
- **Account metadata has two independent sources.** The gateway attaches per-request
  evidence (fingerprint; org id if P1 found one matchable). The claude-code adapter stamps
  session-level evidence at SessionStart from Claude Code's local account state (email /
  org UUID) — attribution works even where the gateway isn't. Shapes verified in P1.
- **OpenBox-side identity is unchanged.** The gateway signs events with the machine's
  existing credentials via `client/` — same DID, same AIP signing as the hooks. Session
  join key: `x-claude-code-session-id` header → `DevEvent.SessionID` (`openbox_session_id`
  + `run_id` on the wire), asserted equal to the hook-observed id in phase 08.
- **Classification reads attributes, not root fields.** `isLLMCall` checks
  `attributes["http.method"]` + LLM domain in `attributes["http.url"]`. Set both the
  attributes and the root fields. ADR-0018's synthetic marker retires only where the span
  is genuinely observed; hook-sourced turn spans keep it until openbox-core#130.
- **Redact the copy, never the forward.** Ordering is the control: fingerprint → redact →
  attach → cap → sign. Volume: v1 ships **cap-only** (64KB); the `body_ref` sink is a
  phase-08 contingency, built only if measured volume demands it (validated 2026-08-25).
- **Account evidence is org UUID + email, nothing more** (validated 2026-08-25). Local
  state also exposes `organizationName`/`organizationRole` — explicitly NOT bound; the
  allowlist discipline from INV-2 applies to account fields too.

## Requirements

1. Request/response headers captured; credential headers redacted **by key name** at
   capture (`authorization`, `proxy-authorization`, `cookie`, `set-cookie`, `x-api-key`,
   `api-key`, `x-auth-token`, `x-amz-security-token`).
2. `credential_fingerprint` computed at capture, before redaction; raw credential in zero
   outbound bytes — conformance-asserted.
3. Bodies captured, secret-redacted, capped at 64KB. No sink in v1 — deferred to phase-08
   evidence.
4. `attributes["http.method"]`/`["http.url"]` set so core classifies `llm_completion`.
5. Session, agent, and parent-agent identity mapped from the `x-claude-code-*` headers.
6. Account metadata stamped by the adapter at SessionStart (email / org UUID from local
   state), riding session metadata — never `signal_args`.
7. Events enter through `Spool.Append` — same client, auth, signing, idempotency.
8. Gateway spans and hook turn events never claim the same `activity_id`.
9. Schema v1.4: span header/body/http fields + `credential_fingerprint` + account metadata.
   Purely additive.

## Architecture

Tee → fingerprint → normalize → redact → attach → cap → `Spool.Append`. The
gateway runs the same `client/` code as the hook path, so signing and idempotency are
inherited rather than reimplemented.

Account evidence contract (also the phase-03 backend ask): per-span
`credential_fingerprint` (+ `org_id` if P1 positive); per-session `account_email`,
`account_org_uuid` from the adapter. Core policy matches against the org registry and
returns HALT/BLOCK — evaluated server-side, rendered by phase 06.

Dedupe: two producers describe the same turn from two vantage points; id namespaces stay
disjoint (`<session>:turn:<n>` vs span ids). Core's span dedupe is (span_id, stage) scoped
by session_id.

## Related code files

| Path | Change |
|---|---|
| `gateway/capture.go` | new — tee → fingerprint → redact → attach → cap |
| `docs/data-and-privacy.md` | account rows: email (PII) + fingerprint egress; name/role excluded |
| `client/event.go` | `Span` gains headers/HTTP fields; `CredentialFingerprint`; account metadata |
| `client/payload.go` | serialize new fields; `stripContent` covers content-bearing ones |
| `client/turnspan.go` | retire synthesized attributes where the span is observed |
| `adapters/claude-code/mapper.go` | SessionStart account stamping (metadata, not signal_args) |
| `contracts/dev-event/schema/` | v1.4 |
| `docs/adr/ADR-0018-dev-turn-content-carrier.md` | note which half retires |

## Implementation steps

1. Extend `client.Span` + `stripContent` in the same commit — a gated field added without
   its gate is the failure mode.
2. Fingerprint at capture; then header redaction by key name; conformance case asserts the
   raw credential absent and the fingerprint present on outbound bytes.
3. Body capture with 64KB cap (sink deferred to phase 08's evidence).
4. Classification attributes; assert core would classify `llm_completion`.
5. Identity from the three `x-claude-code-*` headers → `DevEvent.SessionID` etc.
6. Adapter-side SessionStart account stamping (shape from P1's local-state check).
7. `Spool.Append` wiring; signing and idempotency unchanged.
8. `activity_id` collision test vs hook-path turn events.
9. Retire the synthetic marker where observed. Schema v1.4.

## Todo

- [ ] `Span` fields + `stripContent` in one commit
- [ ] Fingerprint-before-redaction, asserted on outbound bytes (raw absent, hash present)
- [ ] Header redaction at capture, asserted on outbound bytes
- [ ] Body cap (64KB; sink deferred to phase 08)
- [ ] Privacy doc: account email (PII) + fingerprint rows; name/role exclusion stated
- [ ] Classification attributes set
- [ ] Identity mapped from headers; session id equals hook-observed id (test)
- [ ] SessionStart account stamping (metadata only)
- [ ] `Spool.Append` wiring
- [ ] `activity_id` collision test
- [ ] Synthetic marker retired where observed
- [ ] Schema v1.4

## Success criteria

- Headers and bodies present on stored `governance_events` for a real session.
- Core classifies the span `llm_completion` without the synthesized marker.
- Credential header values never appear in stored rows or outbound bytes; fingerprints do.
- Account email/org UUID present on session records where local state exists.
- Parent-agent tree reconstructable, with ids that join to hook-side agent ids.
- No duplicate rows for a turn covered by both producers.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| New gated field ships without a gate | field + `stripContent` in one commit; leak-scan test | capture OFF still emits headers | block merge |
| Fingerprint computed after redaction | ordering test on outbound bytes | fingerprint empty on every span | fix ordering; the case is the control |
| Raw credential leaks via logs/sink | no header logging (phase 04 rule); sink stores post-redaction copy only | credential bytes in sink or rows | stop; treat as an incident, not a bug |
| Account metadata rides signal_args | metadata-only rule; `TestNewSignalsCarryNoSignalArgs` pattern | core goal overwritten by account data | revert binding; core reads signal_args as a NEW GOAL |
| Double-store with the hook path | distinct `activity_id`; collision test | duplicate turn rows | separate id derivation; never dedupe client-side |
| Volume overwhelms core | cap + sink switch; measured in phase 08 | ingest latency or rejects | sink-only mode; backend ask |
| Header identity assumption false | doc-tier; phase 04 asserts presence + equality first | header absent or ≠ hook session id | stop and replan identity mapping before this phase builds on it |

## Security considerations

- Header capture remains the highest-risk class: the developer's live credential is on
  every request. Fingerprint-then-redact at capture, never at serialization; gateway logs
  carry no headers at any level.
- The fingerprint is one-way over a high-entropy secret; store no salt material that would
  narrow it. It exists to answer "which registered credential was this" — nothing else.
- If phase 08's volume evidence forces the body sink, it becomes a new at-rest surface on
  the developer machine holding full conversations — location, rotation, and retention get
  named in an ADR-0021 amendment before it is built, not defaulted.

## Next steps

Phase 06 adds the verdict; account HALT rides it with zero extra machinery.
