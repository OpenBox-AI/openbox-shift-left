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
- Implementation status: **implemented except requirement 5** (identity from the
  session header, which needs P0). Everything else is on the wire and drilled.
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
  org UUID) — attribution works even where the gateway isn't. **Shapes now verified**
  (P1 §3, 2026-08-25): `oauthAccount.organizationUuid` and `oauthAccount.emailAddress` are
  strings at a stable path in `~/.claude.json`, read without touching the credential — so
  this requirement does NOT depend on P0 or on the bearer being parseable. That file also
  exposes `organizationName`/`organizationRole`, which stay unbound by decision. Honest
  limit: it is written by the client this product governs and is writable by anything
  running as the developer, so it is evidence of origin-of-config, not a tamper-resistant
  account claim.
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
9. Schema **v1.5**: span header/body/http fields + `credential_fingerprint` + account
   metadata. Purely additive. **Not v1.4** — this phase was written before phase 02 landed,
   and `schema_version`'s `const` is already `"1.4"` (thinking capture, ADR-0019 P3). The
   bump rewrites the golden fixtures that pin wire bytes, so it is a step of its own rather
   than a field edit.

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
| `contracts/dev-event/schema/` | v1.5 (v1.4 is taken) |
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
9. Retire the synthetic marker where observed. Schema v1.5.

## Todo

- [x] `Span` fields + `stripContent` in one commit — `RequestHeaders`/`ResponseHeaders` added and nil'd by the gate in the same change; `http_*` and `credential_fingerprint` added and deliberately NOT stripped (structural / derived evidence). Golden fixtures unchanged.
- [x] Fingerprint-before-redaction — ordering enforced INSIDE one `Capture` function and asserted on POSTed bytes. Reversing the two lines turns the test red.
- [x] Header redaction at capture, by KEY NAME — asserted at the pipeline AND on outbound bytes, in both postures. Drilled.
- [x] Body cap (65,536 RUNES, after redaction never before) — both drilled.
- [x] Privacy doc: six new summary rows plus an **Account attribution** section — email as PII, the fingerprint, and the seven sibling fields deliberately NOT sent, with the origin-of-config-not-tamper-resistance limit stated.
- [x] Classification attributes set from OBSERVED values (`http.method`, `http.url`, `http.status_code`) AND the root fields. Drilled: emptying the attributes turns the test red. ADR-0018's "synthesized" marker deliberately absent — this span really is observed.
- [ ] **BLOCKED on P0.** Identity mapped from headers; session id equals hook-observed id. Phase 04 proved the header RELAYS verbatim; whether Claude Code SENDS `x-claude-code-session-id` needs real traffic through the gateway.
- [x] SessionStart account stamping — `account_email` + `account_org_uuid` from the local record, metadata only. The bound struct IS the allowlist; a test asserts exactly two keys and that none of the seven sibling fields (name/role/type/tiers/billing) can escape. Every read failure is silent.
- [~] Rides the existing client path (`buildPayload` → sign → POST), so signing and idempotency are inherited. The gateway process calling it is phase 06's wiring, since that is where the gateway gets a verdict round-trip.
- [x] `activity_id` collision test — checked by CONSTRUCTION across all four namespaces (`:gateway:`, `:turn:<n>`, `:usage:rollup`, `cc-act-<hex>`). Drilled: removing the gateway namespace turns it red.
- [x] Synthetic marker retired where observed — the gateway span carries no `synthesized` attribute because it is a real measurement. The hook turn span keeps its marker until openbox-core#130.
- [x] Schema v1.5 — `const` and `x-schema-version` bumped, 25 conformance samples updated, changelog entry written. Golden fixtures show ZERO churn: the new `wireSpan` fields are appended and `omitempty`, so a hook-only install serializes byte-identically.

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
| Header identity assumption false | doc-tier. **Phase 04 asserted pass-through, NOT presence**: its identity test proves an `x-claude-code-*` header the client sends arrives verbatim, which is silent on whether Claude Code emits `x-claude-code-session-id` at all. Confirming that needs real traffic through the gateway, so it needs P0 positive. | header absent or ≠ hook session id | stop and replan identity mapping before this phase builds on it |

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

## Status, 2026-08-25

**Built and mutation-verified** (`gateway/capture.go`, 35 gateway tests):

- `credentialFingerprint` → `redactHeaders` → `captureBody` (redact, then cap), with the
  order enforced inside one `Capture` function so it is a property of real code rather than
  of two functions a caller might sequence wrongly.
- Credential headers redacted **by key name**, not by inspecting the value: a provider
  credential need not match anything the detector recognises, and this repo has already
  measured that the detector's reach is decided by the keyword beside a value.
- The KEY is kept while the value goes, so an unauthenticated call stays distinguishable
  from an authenticated one.
- `http_url` drops the query string — core only needs host+path to classify, and keeping
  the query would make a structural field into an ungated content leak the day a provider
  accepts content or a token there.
- All four mechanisms drilled: removing header redaction, body redaction, or the cap turns a
  test red, and reversing the fingerprint/redact order turns the ordering test red.

**A fixture trap worth not repeating.** Two assertions were VACUOUS on first write: a
secret-shaped literal saved into a file here is rewritten to an `${OPENBOX_REDACTED_*}`
placeholder, so the fixture the test meant to catch was already gone before the code ran.
Both tests compiled, passed, and proved nothing. Fixtures are now assembled at RUNTIME from
low-entropy fragments. Only a mutation drill exposed this — the tests looked fine.

### Resolved: the wire mapping IS built

The earlier note here said the mapping was contract-gated and the `client.Span`
fields were inert. That was too conservative, and re-reading the existing code
settled it: `turnActivityIDFor` already documents how a disjoint id namespace is
added, and ADR-0013/0014 already established the activity carrier. So the shape was
determined by precedent rather than undetermined — the gateway turn takes
`<session>:gateway:<request-id>`, which cannot collide with `:turn:<n>`,
`:usage:rollup`, or `cc-act-<hex>`. No new table, endpoint or service, so no new ADR
is required by the repo's own rule.

`client/gatewayspan.go` builds it, `buildPayload` attaches it, and the two span
producers are kept on separate events on purpose: core's alignment extractor reads
the LAST span's `response_body` as the assistant's REPLY, and a raw provider
response body must never be handed to it.

### Superseded note (kept for the reasoning)

**The wire mapping.** The new `client.Span` fields are currently **INERT**: `client.Span` is
the adapter-facing type and is never serialized — only `wireSpan` reaches `payload.Spans`.
Making the capture egress needs a decision this phase cannot make alone: which event type
carries a gateway span, and how its `activity_id` is derived so it cannot collide with the
hook path's turn (requirement 8). That is a contract decision **ADR-0021 owns**, and
ADR-0021 is a draft whose three `TBD(probe)` slots block acceptance. Inventing it here is
what the plan's own gate exists to prevent, so the fields stop at the struct — stated
plainly, because this repo's rule is that asserting the struct is not asserting the wire.

Consequently requirements **7** (`Spool.Append`), **8** (`activity_id` collision) and **9**
(schema v1.5) are gated on that decision, and **5** (identity from `x-claude-code-session-id`)
is gated on P0.

Requirement **6** (adapter SessionStart account stamping) is NOT gated — P1 §3 confirmed the
local shape — and is simply not built yet.

### Blast radius handled

Adding a map field made `client.Span` non-comparable, which broke `*got.Span != *tt.wantSpan`
in both adapters' mapper tests. Both now use `reflect.DeepEqual`. Golden fixtures show zero
churn: the new fields are `omitempty` and `Span` is not itself serialized.
