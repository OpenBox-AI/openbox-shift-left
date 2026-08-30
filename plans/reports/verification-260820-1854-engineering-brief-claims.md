# Verification — engineering remediation brief claims

**Verified at:** `d27efbb` (same commit the brief reviewed)
**Date:** 2026-08-20
**Scope:** truth of the brief's claims. Priority per owner: SL-1, SL-2, SL-4, SL-5.
SL-3/SL-6/SL-7 confirmed factually only (owner: known); SL-8 noted for deletion.
**Method:** citation-by-citation read, plus two throwaway in-package probes run
against real code and the real shipped profile (removed after; tree clean).
Sibling repos `openbox-core` / `openbox-backend` read directly — the brief could
not, and that is where two of its premises turn out to be wrong.

## Verdicts

| | Claim | Verdict |
|---|---|---|
| SL-1 | Locked URLs silently lock nothing | **TRUE — proven by execution** |
| SL-2 | Shipped profile makes fail-closed unreachable | **TRUE — proven by execution** |
| SL-3 | No govulncheck / SBOM / signing | TRUE (one overstatement) |
| SL-4 | Idempotency contract self-contradicts | **TRUE, but premise inverted — fix already shipped** |
| SL-5 | HALT-without-policy unfixed | **TRUE — still live in core** |
| SL-6 | Repo hygiene | TRUE |
| SL-7 | Ed25519, not KMS-held ECDSA | TRUE |
| SL-8 | No offline verifier | TRUE (claim to be retired per owner) |

Every file:line citation in the brief resolved to real code. One off-by-two
(`backend_url` tag is `devconfig.go:203`; 201–202 is its comment). No fabrications.

---

## SL-1 — CONFIRMED, and stronger than stated

Proven by running code, not reading:

```
lockableFields()[base_url]              = true
lockableFields()[backend_url]           = true
unknownLocked([base_url, backend_url])  = []      <- doctor reports NO problem
unknownLocked([bogus_field])            = [bogus_field]   <- control: typos ARE caught

ResolveEnforce()      locked enforce:true + OPENBOX_ENFORCE=0  -> true   <- lock works
ResolveCoordinates()  locked base_url    + OPENBOX_BASE_URL    -> the ENV value
ResolveBackendURL()   locked backend_url + OPENBOX_BACKEND_URL -> the ENV value
```

The control case matters: the validator is not broken, and locks are not broken.
The validator simply does not know which fields have resolvers, so it green-lights
the two that do not. Exactly the failure `unknownLocked`'s own comment
(`managed.go:257-258`) exists to prevent.

Structural, not incidental: `load()` is `Load(DefaultConfigPath())`
(`devconfig.go:257`) — user file only, no managed overlay — so no URL resolver can
reach the managed layer at all. `cachedManaged()` has two callers:
`resolveBoolWithSource` (the only resolver) and `Managed()` (status). The brief's
"reachable from exactly one resolver" is precise.

Two additions to the brief:

- The proposed registry fix is well-founded. `posture.go:165-180` is already a
  table of `{name, accessor, default, env, target}` — the exact shape needed. Not a
  new pattern, an extension of one.
- `ResolveBackendURL` never applies `DefaultBackendURL` (returns `""` when
  unconfigured). The default is applied on the `auth` path instead. "Both planes
  default to SaaS" is true in effect; the two defaults do not live in one place,
  which matters when routing both through one helper.

## SL-2 — CONFIRMED verbatim, plus one thing the brief missed

Profile quoted exactly right. Against the **real** `deploy/managed/openbox/dev.json`:

```
ResolveFailClosed() clean                          -> false
ResolveFailClosed() with OPENBOX_FAIL_CLOSED=1     -> false
ResolveFailClosed() with OPENBOX_FAIL_CLOSED=true  -> false
```

Fail-closed is unreachable for that fleet. Confirmed: `"brian's 2026-07-15
decision"` and `"NOTE: this decision (OD-E8-1) is not yet ratified"` both present
in the public enterprise artefact. `MaxRecoveryAttempts = 5` and its comment quoted
correctly; the audit-durability concern that follows is sound.

`fail_open_until` / any time-boxed fail-open: **grepped, absent repo-wide** across
`*.go`/`*.json`/`*.md`. The brief is right that the proposal claim has nothing behind it.

**Missed by the brief:** the same file sets `"tier2": true`, a deprecated no-op.
Loading it emits, to stderr:

> ``openbox: `tier2` set but ignored — every gated tool call is evaluated by OpenBox … Remove from dev.json / the environment to silence this.``

So the reference enterprise profile makes every developer on it see a deprecation
warning naming a root-owned file they cannot edit. Same file, same fix pass as the
name and the unratified note.

## SL-4 — contradiction CONFIRMED; premise INVERTED; recommended fix ALREADY SHIPPED

The contradiction is real, is worse than described, and caused the bug the brief
cites. But the brief's factual premise is out of date, so its remedy is wrong.

**Core ships server-side dedupe on `Idempotency-Key`.**

- `openbox-core` `internal/api/idempotency.go`, commit `911b67f`
  *"feat(governance): honor Idempotency-Key on /governance/evaluate"*, **2026-07-29**
- on `develop` **and** `origin/develop` (`git branch -a --contains`) — merged, not in flight
- dedupes **before** the workflow runs, deliberately: *"a duplicate should not
  re-enter the workflow at all… A unique index on `governance_events` would prevent
  the duplicate row but only after all of that had happened."*

So `spool.go:28` and `client/client.go:163` are now **correct**, and
`realtime.go:53` is the stale one. Consequences:

1. **Option A is done.** Nothing to implement in core.
2. **"a decision record is warranted" — that decision already exists.** *Server-side idempotency
and delivery receipts*, `Status: Accepted — reconstructed 2026-07-31`. Written
**two days after** core shipped the half it says is outstanding. Line 40 — *"Until
core ships its half, a retry after a lost 200 can double-count"* — is now false,
and line 42 already prescribes the fix: *"the client comments should say which
half is live."* Amend; do not write a new one.
3. **The guarantee is conditional, not unconditional.** Redis-backed, `idempotencyTTL
   = 24h`, and **fails open** on cache miss/absence (*"refusing the event would turn
   a telemetry-durability feature into an outage"*). A Redis outage or a >24h
   retry gap still double-stores. The brief's "makes the guarantee unconditional"
   would have been wrong even as a forecast.
4. **Unconsumed client half — not in the brief.** Core annotates responses with
   `receipt_key` + `duplicate` explicitly *"so the client can tell a deduplicated
   delivery from a fresh evaluation and mark its spooled copy delivered."*
   shift-left reads **neither** (grep: zero hits). Core built a client contract
   nothing consumes.

**Stale-site inventory** — the brief found 2; there are ~8 in production code:

| Asserts core does NOT dedupe (now stale) | Asserts core DOES (now correct) |
|---|---|
| `hookflow/realtime.go:53` | `hookflow/spool.go:28` |
| `hookflow/gate.go:65` | `client/client.go:163` |
| `hookflow/engine.go:81` | `codex/delivery_test.go:18,111` |
| `hookflow/evaluate.go:77` | `claude-code/enforce_evaluate_test.go:151` |
| `client/payload.go:493` | `codex/enforce_evaluate_test.go:80` |
| `claude-code/mapper.go:758` | |
| `claude-code/hookrun.go:128` ("even after… lands") | |
| docs: that decision:40, that decision:206, that decision:184, CLAUDE.md:158 | |

**Do not blanket-edit.** The distinction is load-bearing: the client still *sends*
two copies on a gated call by design, and core now collapses them. Comments
explaining why sending two is acceptable became **more** true. Only sentences
asserting a surviving double-*store* are wrong — that includes the test rationale
in `observecopy_test.go:23,71`, whose assertions (two sends) stay valid while their
reasoning does not.

Batch-ingest aside: still a fair question, untouched by the above.

## SL-5 — CONFIRMED unfixed

README disclosure accurate to the word. The defect is live in core at
`internal/services/governance_workflow.go:240-253`: a non-pending session returns
`VerdictHalt` with `Reason: "Session is no longer active"` and metadata carrying
`event_type` / `workflow_id` / `session_status` — **no policy id**. Purely
operational, indistinguishable at the client from a governance decision.

"Core-side fix in flight" is generous but not false: a plan exists
(`plans/260814-2235-dev-session-unauthored-halt-fix/`, phase-02 targets core). I
found no landed fix on any local core branch. Planned, not shipped. The brief's
ask — land it or attach a date — stands.

**One correction:** the brief's secondary suggestion (client treats a policy-less
HALT differently) reopens a decision already closed. `plan.md:21`: *"untouched — no
discrimination, no `fallback_used` wiring, no failure-policy change."* CLAUDE.md
records it as an owner decision. Feasible — `metadata.session_status` is right
there — but it is a settled decision, not an oversight, and the brief presents it
as new.

## SL-3 / SL-6 / SL-7 / SL-8 — factual check only

- **SL-3 TRUE.** `.goreleaser.yaml`: `checksum:` present, no `sboms:`, no `signs:`,
  `goos: [linux, darwin]`. Zero hits for `govulncheck|cosign|syft|sigstore` across
  `.github/`, `.goreleaser.yaml`, `install.sh`. *Overstatement:* README:265 already
  says *"Windows is build-verified, not runtime-verified"* — it does not claim a
  release artefact, and the brief lists that same disclosure under known bounded
  behaviours. The actionable half (no Windows artefact exists) holds.
- **SL-6 TRUE.** 6 `krnl-labs.atlassian.net` links across 5 files; both named
  incident/journal files exist; personal attribution in `dev.json` confirmed.
- **SL-7 TRUE.** Zero `crypto/ecdsa` imports repo-wide; `ed25519.Sign` at
`client/signing.go:106` exactly as cited; that decision's *"the coding agent
under governance"* and *"origin-of-config, not tamper-resistance"* both
accurate.
- **SL-8** cited comment present verbatim at `actions/openbox-git-action/resolve.go:80-84`.
  Per owner the capability claim is retired — note that the comment itself is
  accurate code rationale; what goes is the external claim, not this text.

## What actually changes

1. **SL-4 is a documentation-reconciliation task, not a core feature request.**
~8 code sites + 4 doc sites + an that decision amendment. Cheapest high-value
item in the brief, and the brief would have sent the work to the wrong repo.
2. **SL-1 and SL-2 stand exactly as written** — both now backed by executed proof
   rather than reading. SL-1 remains the top item.
3. Two additions worth folding in: the `receipt_key`/`duplicate` integration gap
   (SL-4) and the `tier2` deprecation warning in the enterprise profile (SL-2).
4. Two brief suggestions to drop: "write a decision record" for SL-4 (that decision exists) and
   client-side HALT discrimination for SL-5 (decided, rejected).

## Unresolved

- Is core's Redis-backed, 24h, fail-open dedupe sufficient to close the lost-200
  window for the buyer's purposes, or does the durable path (unique index /
  outbox) still get filed? Determines whether SL-4 is purely editorial.
- Should shift-left consume `receipt_key` / `duplicate` to mark spooled copies
  delivered, or is core's replay-returns-original-verdict enough?
- Is there an unmerged core PR for the SL-5 HALT fix? I checked local branches
  only — no remote PR list consulted.

---

## Addendum — owner pushback, probed against the live config

Both questions probed against the real `~/.openbox/dev.json` (read-only) and the
real shipped profile. Probes removed after running.

### SL-1 — "I set up other URLs and it still works"

Correct, and expected: that is the **user** layer, and it is not what SL-1 is about.
With an org mandate locking both URLs *and* `enforce`, in one run:

```
managed locked=[base_url backend_url enforce]  unknownLocked=[]   <- doctor: no complaint
base_url    : org LOCKED core.org-mandated.example -> "https://openbox-core.node.lat"
backend_url : org LOCKED api.org-mandated.example  -> "https://openbox-api.node.lat"
enforce     : org LOCKED false, user file says true -> false
```

Same file, same `locked` list, same run: the bool obeys the mandate, both URLs
ignore it. The user config wins over an explicit org lock.

So the working self-hosted setup **is** the demonstration. Pointing at a
different core is legitimate here (the owner is the operator) — the gap is that
nothing distinguishes that from a developer re-pointing prompts, assistant text
and gated file bodies at a host of their choosing, and the org has no mechanism
to refuse it. Unchanged verdict: SL-1 stands, top priority.

### SL-2 — "this should be configurable within dev.json, right?"

Yes — and confirmed configurable:

```
user dev.json parses fail_closed = true                (the key IS configurable)
ResolveFailClosed() WITH shipped managed profile -> false   source: managed
ResolveFailClosed() with NO managed profile      -> true     (user file honoured)
```

The finding is **conditional on deploying the reference profile as-is**. Absent
`/etc/openbox/dev.json`, `fail_closed` behaves normally in `dev.json`. Deploy
`deploy/managed/openbox/dev.json` unmodified and `fail_closed` is in `locked`, so
the permissive `false` short-circuits both the user file and the environment.

Scope note: this is a bad default in one artefact, not an architectural lock-out.
An org admin who edits the profile can fix it — the brief's wording ("or by an
org admin who doesn't edit the profile") is precise. Minimum fix is removing
`"fail_closed"` from `locked`, which demotes it from mandate to overridable org
default. The sibling `dev.regulated.json` is a larger, optional choice on top.

### Incidental

The live `~/.openbox/dev.json` carries `"tier2": true` — the deprecated no-op key
— so this machine emits the `tier2 set but ignored` warning once per hook process.
Same key as the enterprise-profile finding above; harmless, removable.
