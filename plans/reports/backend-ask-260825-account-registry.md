# Backend ask (DRAFT — not filed): an account registry as policy input

Status: **DRAFT.** This is a ready-to-file issue body for **openbox-backend**, held
here deliberately. Filing it commits this org to a cross-repo conversation, which
is the user's call, not an agent's. Gate: plan `260825-0027` phase 03 step 8.

Suggested title: **Account registry as policy input for developer-runtime account binding**

---

## What we need, in one sentence

A way for a policy to answer "is this model call being paid for by an account that
belongs to this org?" — which needs org-side data core does not have today.

## Why this is your call, not ours

Account binding is **policy**, by our own architecture rule (that decision:
`/evaluate` is the only decider). Our sensors attach evidence; they must not hold
allowlists or cache verdicts. So the matching data has to live where policy is
evaluated. That may be a new surface on your side, which is your decision record
rule and your decision — this issue is the ask plus the exact evidence contract, so
the shape of the data is settled before anyone builds either half.

## What our sensors will send

Per model call, on the gateway span's event (that decision,
`plans/260825-0027-openbox-gateway-full-capture/`):

| Field | Type | Notes |
|---|---|---|
| credential fingerprint | `sha256` hex, truncated | **One-way.** The raw provider credential appears in zero outbound bytes — conformance-asserted on the wire. Stable per credential, so it is matchable across calls without ever being reversible. |
| account org UUID | uuid | read from the tool's local account state |
| account email | string | **PII**, egressed as governance evidence like the developer DID already is; documented in `docs/data-and-privacy.md` |

Deliberately **excluded**: `organizationName`, `organizationRole`. The evidence is
org UUID + email and nothing more — a narrower contract is easier to defend than a
wider one we would have to justify field by field.

**One field is not yet settled.** Whether the org UUID is reachable for
subscription-OAuth sessions depends on a probe we have not run
(`plans/reports/probe-260825-baseurl-auth-coverage.md`, P1). If it is not, the
OAuth branch of any rule you build is **detection-only**, while API-key
fingerprints can still be refused. We would rather tell you that now than discover
it in your implementation.

## What we are asking core/backend for

1. **A registry of allowed accounts per org** — org key fingerprints, allowed org
   UUIDs, allowed email domains — readable at policy-evaluation time.
2. **A verdict on mismatch.** `HALT` or `BLOCK` from `/evaluate` when the evidence
   names an account outside the registry. We render the refusal; we do not decide
   it.
3. **A shape for "no registry configured".** Our strong preference: **allow, and
   say so in the response**, so an org that has not configured this is not
   accidentally enforced against. Silent enforcement from an empty allowlist is the
   failure mode we are most worried about on your side.

## What we will do if this stalls

Ship the sensor half anyway (phase 05): the fingerprint and account metadata
egress, are stored, and are queryable. Policy matching follows whenever core lands
it. So this ask is not blocking our plan — it is what turns stored evidence into an
enforceable rule.

## Cross-references

-  §6 (why matching is policy, not
  gateway logic) and §7 (our always-refuse posture, which interacts with your
  availability)
-  (the rule this follows from)
- Phase 05: `plans/260825-0027-openbox-gateway-full-capture/phase-05-gateway-capture-pipeline.md`

## Unresolved questions

- Is an account registry a new table on your side, or does an existing
  org-configuration surface already fit?
- Do you want the fingerprint construction pinned in a shared contract, or is
  "sha256 of the credential, truncated to N" enough if we document N?
