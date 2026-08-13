# Advice — resolving the unresolved questions on inline evaluation

Date: 2026-08-13 · Scope: the 5 unresolved items from
`advise-260813-0103-inline-evaluation-vs-tiers.md`, **latency excluded** (platform/core
owns "fast and reliable") · Status: advice, not a plan

## Reframing

Latency is out of scope by user decision. That **deletes the Phase 0 measurement gate**
from the prior advice: deletion of the local evaluator no longer waits on a p95 number.
Two of five items are also out of scope (U2 latency, U5 capacity) — but each leaves a
*client-side* residual that is a correctness guarantee, not a performance one, and those
residuals stay in scope.

Decisions taken this round:
- **Content attaches for all gated classes**, content_capture-gated as today.
- **Local secret redaction runs first**; core receives the redacted body.

## Verdict

**Four of five resolve cleanly, and two resolve for free.** `policy_id` already exists on
the wire; the content-gating machinery already exists and already degrades honestly. The
genuinely open work is smaller than it looked: one privacy-doc rewrite, one SPI addition,
and one regression check on a bug you just fixed.

**The one thing I'd flag hard:** your prior fix `a901969 "store one ActivityStarted per
gated tool call"` was scoped to high-risk escalations. Under inline-everything, *every*
call is escalated — so that fix's correctness stops covering a subset and starts covering
100% of events. Re-verify it before deleting anything.

---

## U1 — content_capture × inline enforcement → **RESOLVED**

### What the code already does (verified)

- Structural fields **always** egress: `tool_name`, `kind`, `file_path`,
  `file_operation`, `mcp_server`, `mcp_tool`. Core runs Guardrails stage 0 over exactly
  these (`client/payload.go:480-511`, citing `services/guardrail.go:180`).
- Content (`command` / `arguments`) rides `activity_input` **only** when an escalation
  carried it, and is content-gated: `stripContent` nils `Content` when the org has
  content capture off — "so it is simply absent then" (`payload.go:512-522`).
- ⇒ `content_capture:false` orgs **already** escalate today, on structural fields only.
  "Fidelity scales with what you let leave the machine" is already implemented.
- ⇒ Every tool event already egresses via the spool. Inline evaluation changes **when**
  data leaves, not **what** — except for the delta below.

### The delta your decision creates

Newly-gated classes (Write/Edit/Read) now attach content. Concretely: **Write/Edit file
bodies begin egressing on ordinary enforcement** when `content_capture:true`.

Required consequences:

1. **`payload.go:485` becomes false.** Its "single documented exception of the escalation
   context" widens from one class to all gated classes. Update the comment *and* whatever
   test guards the content-free property, or the next reader trusts a stale invariant.
2. **`docs/data-and-privacy.md` needs a rewrite, not an edit**, and README's "Tool
   commands and file bodies are **never** sent on ordinary telemetry — only on an
   approval request" becomes untrue. New true statement: *on enforcement, the tool's
   content is sent for evaluation when content capture is on; secrets are redacted first.*
3. **Existing `content_capture:true` installs change behavior silently.** File bodies
   that never left start leaving. That needs a release note at minimum; for any org with
   a data-handling commitment, a notice.
4. **Server-side evaluation is bounded at 64KB.** `capBody` truncates at
   `maxBodySize = 65536` (`payload.go:676-692`). A policy matching on content sees at
   most the first 64KB of a large write — disclose it as a limit; do not let anyone
   assume full-file evaluation.

### Derived decision (taken): redact before sending

Order: local secret scan → redact → send redacted body for evaluation. This preserves the
one in-transit control that survives the change. The gate already computes local redaction
before escalating and carries it onto the escalated decision
(`gate.go:109,126-129`), so the ordering is a small change, not a new mechanism.

Consequence to accept: server-side policy cannot match a secret's literal value. Local
detection already owns that class, and it is **unbounded** where the server's view is
capped at 64KB — a second reason `decision/secrets.go` must survive the deletion.

---

## U2 — /evaluate latency & rate limits → **OUT OF SCOPE**, two residuals stay

Platform owns fast+reliable. Deleting the measurement gate is fine. But two client-side
guarantees are correctness, not performance, and survive:

1. **The hook must always write a verdict before the platform kills it.** A killed hook
   ⇒ the tool proceeds ⇒ **uncontrolled fail-open that no `fail_closed` can close**.
   Keep `maxEnforceHookBudget` (`enforce_tier2.go:36-42`) strictly under the provider
   ceiling. This holds however fast core is — it is the client's own failure boundary.
2. **No inline retries.** A failed evaluation must not retry inside the gate; that turns
   a core hiccup into a client-side amplifier. Apply `fail_closed` and move on.

---

## U3 — policy identity in the verdict → **RESOLVED, free**

`/evaluate` already returns `policy_id`, and the client already parses it into the verdict
(`client/verdict.go:116,195,275`). So replacing bundle-derived posture evidence is
**plumbing, not a backend ask**: carry the verdict's `policy_id` into session posture and
`openbox doctor` where `bundle_version` / `bundle_integrity` / `bundle_sha256` are
reported today (`doctor.go:30-47`).

Residual: only `policy_id` was found — **no epoch/version field**. If the control plane
needs "which *version* of the policy decided", that is a backend ask. Decide whether id
alone is sufficient evidence; for drift-over-time questions it is not.

---

## U4 — hook-kill ceiling → **OPEN, cheap, and now provider-shaped**

Verified: Claude Code is 30s PreToolUse / 5s every other hook
(`enforce_tier2.go:20-34`). **Codex's ceiling is unverified** — and under
inline-everything a killed Codex hook is an ungoverned call, so this must be read out of
Codex's own docs/config before shipping.

Verified gap: `provider/provider.go` and both `capabilities.go` files **declare no hook
timeouts**. Today each adapter hardcodes its own. With the engine now owning one gate for
all classes, the ceiling belongs in the SPI: **add a declared hook-ceiling capability**
and have `hookflow` derive its budget from it. That removes a per-adapter cliff the engine
currently cannot see — and it is the same "provider-agnostic goes in hookflow" rule the
repo already enforces.

---

## U5 — control-plane capacity → **OUT OF SCOPE**, one residual

Capacity is core's. The residual is **event duplication, and it is a live bug class here**:

- Current branch is `fix/tier2-duplicate-activity-started`; commit `a901969` is
  "fix(hookflow): store one ActivityStarted per gated tool call".
- `gate.go:55` notes the escalation "POSTs the identical event" as the spool path.
- ⇒ Today that fix covers only high-risk escalations. **Inline-everything makes every
  call escalate, so the same dedupe correctness now governs 100% of events.**

Re-verify that fix under the widened scope *before* deleting the local path — a
double-count that was 5% of events becomes 100%.

---

## What you should do (ordered)

1. Re-verify `a901969`'s dedupe under "every call escalates". Blocking; do it first.
2. Read Codex's hook timeout ceiling; add a declared hook-ceiling capability to the SPI
   and derive `hookflow`'s budget from it.
3. Implement redact-then-send for the newly-gated classes; pin the ordering with a test
   asserting a known secret never appears in the outbound payload.
4. Plumb the verdict's `policy_id` into session posture + `openbox doctor`; decide whether
   a policy *version* is also needed (backend ask if yes).
5. Rewrite `docs/data-and-privacy.md`; fix README's "never sent" claim; update
   `payload.go:485` and its guarding test; disclose the 64KB cap.
6. Write the release note for existing `content_capture:true` installs.
7. Then delete: evaluator, regoparity, builder, bundle, signature, policysync, `dev sync`,
   staleness, `require_verified_bundle`, `org_signing_*`. **Keep `decision/secrets.go`.**

## What you shouldn't do

- Don't delete `decision/secrets.go` — it is now the only unbounded content control, and
  the only reason the redacted-send decision is safe.
- Don't drop the hook-budget guard because core is fast. It is a failure boundary, not a
  latency knob.
- Don't retry evaluation inside the gate.
- Don't ship the content change without the doc rewrite. A governance product whose
  privacy doc is stale is the failure mode `CLAUDE.md` names explicitly.
- Don't assume `policy_id` == policy version.

## Trade-offs (incl. costs of your own decisions)

- Content-for-all-classes buys maximum policy expressiveness and costs a real privacy
  posture change: file bodies egress, a doc rewrite, and a behavior change for existing
  installs. The redact-first decision recovers most of the risk, not all of it — bodies
  still contain proprietary code.
- Redact-first costs server-side matching on secret literals. Acceptable: local detection
  owns that class and sees the whole file where core sees 64KB.
- Excluding latency means the first real latency signal will be a user complaint rather
  than a measurement. That is a defensible bet on core, and cheap to revisit — nothing in
  this design forecloses adding a cap later.
- **Stops being right if** core's evaluation proves unreliable rather than merely slow:
  with no local evaluator, unreliability degrades straight to the org's `fail_closed`
  branch — silently ungoverned (fail-open) or blocked (fail-closed). Cost to switch away
  then: prefer a coarse static high-risk deny list (~100 LOC) over resurrecting the
  evaluator and its parity tax.

## Work checklist

- [ ] Re-verify the `a901969` dedupe fix with every call escalating (blocking)
- [ ] Read Codex's hook timeout ceiling from its own docs/config
- [ ] Add a declared hook-ceiling capability to the provider SPI; derive hookflow's budget
- [ ] Pin: hook always writes a verdict before the provider ceiling (per adapter)
- [ ] Implement redact-then-send for newly-gated classes
- [ ] Test: a known secret in a Write body never appears in the outbound payload
- [ ] Plumb verdict `policy_id` into session posture and `openbox doctor`
- [ ] Decide: is `policy_id` sufficient evidence, or is a version/epoch a backend ask?
- [ ] Rewrite `docs/data-and-privacy.md` for content-on-enforcement
- [ ] Fix README's "never sent on ordinary telemetry" claim
- [ ] Update `payload.go:485` comment + the test guarding the content-free property
- [ ] Disclose the 64KB `capBody` evaluation limit in docs
- [ ] Write the release note for existing `content_capture:true` installs
- [ ] Delete the local policy path; preserve `decision/secrets.go`
- [ ] No-inline-retry assertion in the gate

## Success metrics

| Metric | Target |
|---|---|
| Duplicate `ActivityStarted` per gated call, all classes escalating | 0, asserted in testbed |
| Known secret appearing in outbound payload | 0 bytes, asserted by test |
| Hook exceeding provider ceiling without writing a verdict | never — pinned per adapter |
| Provider hook ceiling declared in SPI | both adapters |
| Sessions reporting policy identity in posture | 100% with a reachable core |
| Stale privacy claims (`grep -n "never" data-and-privacy.md`, README) | 0 untrue |
| `decision/secrets.go` still applies with core unreachable | yes |
| Inline retries in the gate | 0 |

## Unresolved after this round

1. Does `/evaluate` return a policy **version/epoch**, or only `policy_id`? Backend ask
   if version-level evidence is required.
2. **Codex hook ceiling** — unverified; blocking for Codex enforcement.
3. Notification obligation for existing `content_capture:true` orgs whose file bodies
   begin egressing — product/legal call, not engineering.
4. Whether policy authors must be told their content matching is capped at 64KB, or
   whether the cap should be raised for the evaluation path specifically.
