# Phase 04 — Content: redact locally, then send

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 3 (all classes gated)
- Parallel-safe with: phase 5 · Authorized by: that decision (E7 + E8)
- **The privacy-sensitive phase.** Everything here is about what leaves the machine.

## Overview

- **Date:** 2026-08-13
- **Description:** Newly-gated classes attach their content so policy can decide on it
  (E7) — and the body is locally redacted first, so a secret never leaves (E8).
- **Priority:** P1 · **Implementation status:** done · **Review status:** pending

## Key Insights

- **The mechanism already exists; only its scope changes.** `Content.ToolInput` already
  rides `activity_input` for escalated calls, keyed `command` for shell and `arguments` for
  MCP, and `stripContent` already nils it when the org has content capture off — "so it is
  simply absent then" (`client/payload.go:512-522`). This phase adds Write/Edit bodies to
  what gets attached; it does not build a new content channel.
- **`content_capture:false` orgs need no special case.** They already escalate today on
  structural fields only (`tool_name`, `kind`, `file_path`, `file_operation`, `mcp_server`,
  `mcp_tool` — `payload.go:488-511`), and core already runs Guardrails stage 0 over exactly
  those. Their enforcement gets coarser, not broken, and that is the honest design: fidelity
  scales with what you let leave the machine.
- **Ordering is the whole security property.** The gate already computes local redaction
  before escalating and carries it onto the escalated decision
  (`gate.go:109,126-129`), so redact-then-send is a small reordering of existing steps — but
  if it is got wrong, secrets are transmitted to the control plane and land in governance
  event storage, which is the hardest place to purge them from.
- **`payload.go:485`'s comment becomes false.** It documents content as "the single
  documented exception of the escalation context". After this phase the exception covers
  every gated class. Whatever test guards that property must be updated deliberately, not
  discovered failing.
- **The server's view is capped at 64KB** (`capBody`, `maxBodySize = 65536`). Local
  detection has no such cap. That asymmetry is a reason `decision/secrets.go` survives, and
  a limit phase 7 must disclose: content-based policy is not a complete check on a large
  write.

## Requirements

1. Gated calls attach `Content.ToolInput` for all classes, not just shell/MCP: the Write/Edit
   body, and each tool's arguments where they exist.
2. **Local secret detection runs before attachment, and the attached body is the redacted
   one.** No code path attaches a pre-redaction body.
3. When `content_capture:false`, no content is attached for any class — the existing
   `stripContent` behaviour, unchanged and asserted.
4. When secret detection is disabled but content capture is on, the body is attached
   unredacted (the org disabled the local control) — and that combination is documented in
   phase 7, not silently different.
5. `payload.go:485`'s comment updated to describe the widened exception; the test guarding
   the content-free property updated in the same commit.
6. A test asserts a known secret placed in a Write body **never appears** in the outbound
   payload bytes.
7. The 64KB cap is unchanged here; phase 7 discloses it.
8. All 11 modules green.

## Architecture

```
Write/Edit body ──▶ local secret scan (whole body, no cap)
                      ├─ redacted body ──▶ Content.ToolInput ──▶ /evaluate  (if content_capture)
                      └─ redacted body ──▶ applied to the tool call (redact-and-continue)
                    content_capture:false ⇒ nothing attached; structural fields only
```

One body, redacted once, used for both the verdict request and the applied redaction.

## Related code files

| Path | Action |
|---|---|
| `adapters/common/hookflow/gate.go:107-130` | redaction already precedes escalation; attach the redacted body |
| `client/payload.go:485` | comment becomes false — update it |
| `client/payload.go:512-522` | the content attachment point; widen beyond shell/MCP |
| `client/payload.go:676-692` | `capBody` / `maxBodySize` — unchanged, disclosed later |
| `decision/secrets.go` | the local detector; **must survive phase 6** |
| whatever test pins the content-free property | update alongside the comment |

## Implementation Steps

1. Find and read the test(s) guarding the content-free property before changing anything;
   the change is only safe if you know what currently asserts it.
2. Attach the redacted body for the newly-gated classes at the existing attachment point.
3. Write the secret-never-egresses test first: a known token in a Write body, assert it is
   absent from the serialized payload. It must fail before step 2's ordering is right.
4. Assert `content_capture:false` attaches nothing, for every class.
5. Assert secret-detection-off + content-capture-on attaches the unredacted body — the
   documented combination, pinned so it cannot change by accident.
6. Update `payload.go:485` and the guarding test in one commit.
7. All 11 modules: build, vet, `-race`.

## Todo list

- [x] Existing content-free guard test located and read
- [x] Secret-never-egresses test written and initially failing
- [x] Redacted body attached for all gated classes
- [x] `content_capture:false` attaches nothing, asserted per class
- [x] Detection-off behaviour pinned
- [x] `payload.go:485` comment + guard test updated together
- [x] All 11 modules green

## Success Criteria

- A known secret in a Write body never appears in outbound bytes.
- A Write to `.env` can be denied by a path-based policy with no content attached
  (`content_capture:false`), proving structural enforcement still works.
- A Write whose body matches a content policy is denied when content capture is on.
- No test asserts the old "content only on approval escalation" property any more — it is
  updated, not deleted.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Raw body attached anywhere | M×H | the secret test fails, or a payload contains a live token | **Stop.** This defeats the one in-transit control the plan preserves. Fix before proceeding. |
| Ordering regresses later (someone attaches before redacting) | M×H | the secret test fails in CI | **Accepted, guarded:** the test is the tripwire; never weaken it. |
| The content-free guard test is deleted rather than updated | M×H | grep finds no assertion about structural-only fields | **Adjust:** the property still exists for `content_capture:false`; it must keep a test. |
| 64KB truncation splits a secret so local redaction catches it but the server sees half | L×M | inconsistent verdicts on large files | **Accepted:** local redaction is authoritative for secrets and is uncapped; phase 7 discloses the server-side cap. |
| Orgs discover file bodies egressing without warning | M×H | complaint after upgrade | **Adjust:** phase 7 owns the release note; do not ship this phase in a release without it. |

## Security Considerations

- The attached body must be byte-identical to the redacted body applied to the tool call —
  two different redactions would mean the server judged text the developer never wrote.
- Never log the attached content, at any level.
- `content_capture:false` must remain a hard gate, not a best-effort filter: assert absence,
  not "usually absent".
- This phase widens what egresses. It must not ship before that decision (phase 2) and must not
  reach users before phase 7's privacy-doc rewrite and release note.

## Next steps

Phase 5 replaces the bundle-derived posture evidence with the deciding policy identity.
