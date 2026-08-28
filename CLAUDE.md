# CLAUDE.md

Guidance for Claude Code (and other agents) working in the **openbox-shift-left**
repo. User-facing documentation lives in `README.md` and `docs/`; this file is only
the working context an agent needs.

## What this repo is

The developer-runtime half of OpenBox governance: one static Go binary that governs
the agentic coding tools (Claude Code, Codex) developers use, feeding the same
pipeline the agent runtime already uses. `README.md` for the product,
`docs/architecture.md` for the shape.

## Core principle: reuse, don't rebuild

Shift-left onboards the developer runtime onto OpenBox's **existing** pipeline rather
than a parallel one:

- register the tool install as an OpenBox agent (`kind=developer`), session as a
  child record — `POST agent/create` → runtime key + DID;
- emit events through the **same** `/api/v1/governance/evaluate` with the **same**
  auth (`Bearer obx_` + AIP signing);
- store in the **same** tables (`sessions` → `governance_events`, plus Merkle
  leaves) and read through the **same** services. Dev sessions write no `spans`
  rows for tool calls (ADR-0013) — with exactly ONE exception: a content-capturing
  model turn carries one span, because core's alignment reader accepts no other
  shape (ADR-0018).

**Rule:** prefer reusing an existing table/endpoint/service over adding one. A new
table, endpoint or service requires an ADR in `docs/adr/`.

## Architecture in one line

Provider-agnostic engine + one thin adapter per tool behind a normalized event
contract. Adding a provider is an adapter, not an engine change.

The SPI (`provider/`) is `Installer` (install time) + `HookEngine` (runtime +
capabilities). The engine is `adapters/common/hookflow`: spool, duration stash,
advisory sink, findings loop, the enforce cascade and its gate sequence, inline
evaluation, approval hold, rewake. An adapter is four things — its
native hook shape, its mapper, an `OutputContract`, its installer.

**The engine used to be copy-pasted per adapter** (~85% of non-test adapter code was
a rename-level fork, on the enforcement path) and the copies drifted. Do not
reintroduce that: if something is provider-agnostic it goes in `hookflow` or
`devconfig`.

## Where things live

| Path | What |
|---|---|
| `provider/` | the SPI |
| `adapters/common/hookflow/` | the engine every adapter runs on |
| `adapters/common/devconfig/`, `adapters/common/git/` | shared config/posture; trailer, notes, attestation |
| `adapters/claude-code/`, `adapters/codex/` | one thin adapter each |
| `client/` | core client: payload, AIP signing, verdicts |
| `decision/` | local secret detection + redaction — all that survives ADR-0017 |
| `gateway/` | the local model-call relay: byte-identical forward, capture, the gate (ADR-0021) |
| `cli/` | the `openbox` CLI, incl. `cli/internal/approver` (ADR-0012), `cli/internal/prompt` (masked input) and the gateway's install/inspect/emit halves (`gatewayservice`, `gatewaycheck`, `gatewayemit`) |
| `actions/openbox-git-action/` | commit→deploy lineage for CI |
| `contracts/dev-event/` | event schema, wire mapping, conformance |
| `testbed/` | the mock-free end-to-end suite (`docs/testbed/e2e.md`) |
| `docs/` | user documentation — keep it true, and keep it short |

`.claude/` and `.fab7/` are local tooling and git-ignored — do not commit them.

## Working conventions

- **Credentials are plaintext, deliberately.** `~/.openbox/.env`, `0600` on
  macOS/Linux and with **no at-rest protection on Windows** (ADR-0015). Anything
  running as the developer — including the governed agent — can read the signing
  key, so attestation proves origin-of-config, not tamper-resistance. On an approver
  install the same file holds an org key with fleet-wide create/rotate authority: a
  strictly larger blast radius, named separately in the ADR for that reason. Do not
  let a doc imply the file is protected.
- **Privacy and security are first-class.** Content capture is **ON by default**
  (2026-07-15, reversing the original metadata-only posture): prompt text egresses
  unless an org opts out (`content_capture:false` / `OPENBOX_CONTENT_CAPTURE=0`).
  Usage capture is **also ON by default** (2026-08-11, ADR-0014): four token counts
  plus a model id per turn, opt out with `finops:false` / `OPENBOX_FINOPS=0`.
  Guardrail redaction at source is **not wired yet**, so prompt text egresses
  unredacted. **Thinking capture is ON by default too** (2026-08-25, ADR-0019 P3 /
  the ADR-0014 amendment): the turn's extended-thinking text egresses under the
  same `content_capture` key — which goes FURTHER than the provider's own telemetry,
  since Anthropic's OTel export redacts extended thinking unconditionally. **SL3-SEC-3 is retired** (ADR-0019 P1, 2026-08-25): tool commands,
  file bodies, tool output and the refusal free text now egress on ORDINARY tool
  events, not only on a gated call — all under the one `content_capture` key. Local
  secret detection redacts the body **before** it is attached, and that ordering is
  the only in-transit control there is (pinned on outbound bytes by C18/C26/C34).
  The server sees at most the first 65,536 RUNES (`capBody` counts characters, not
  bytes, so non-ASCII content can exceed 64KB on the wire). What that control catches is
  measured, not assumed (C39 + `TestRedact_JSONShapedSecrets`): **the keyword
  decides, not the charset** — a high-entropy value beside an unrecognized key name
  is invisible, because hex cannot clear the 4.5-bit entropy floor.
  **gitleaks ADDS 222 maintained rules BENEATH which the nine hand-rolled named
  formats REMAIN as a floor** (D-OSS-4, 2026-08-28). Deleting the nine was tried and
  regressed six conformance cases (C18/C26/C34/C42, CDX-C10, the finops thinking
  sentinel), because gitleaks **allowlists published documentation keys** — AWS's
  `AKIA…IOSFODNN7EXAMPLE`, the fixture in all six — and `AWS_ACCESS_KEY_ID=` is
  invisible to `secret_assignment` since `_ID` sits between the keyword and the
  delimiter. The nine are LOOSE where gitleaks is PRECISE, and that looseness is
  what covers values gitleaks intentionally skips. Our floor runs FIRST, so audit
  categories stay `aws_key`/`private_key`/`jwt` for formats that already had them;
  gitleaks rule ids appear only where it alone matches. Three more things.
  (a) The order is our patterns → gitleaks → entropy and it is LOAD-BEARING —
  gitleaks replaces a finding's text wholesale and does not go through the
  value-group + terminator-trim path, so running it first made JSON parseability
  depend on how it drew its capture group. (b) A verdict-set diff over tests that CANNOT RUN is not evidence: the
  six cases above are listener-dependent, never executed in a sandbox that denies
  binds, and were reported green by omission.
  (c) gitleaks is PRECISE where ours was LOOSE: it adds charset, length and
  entropy on top of format, and allowlists AWS's published doc key — so
  format-only fixtures stopped matching, and a provider changing its key length
  silently stops matching until the pack is refreshed. `secret_assignment` and
  `redactEntropy` are the backstop for exactly that, which is why they were kept.
  **The false-positive soak did NOT clear the enforce path:** `generic-api-key`
  matched a Go identifier and a credential FINGERPRINT (deliberately egressed,
  deliberately not secret) in this repo's own tree, and the enforce-path redactor
  rewrites developer files. Open item; disabling that one rule removes both. Lowering that
  floor is NOT the fix: below 4.0 every git SHA and UUID matches, and the
  enforce-path redactor REWRITES file bodies, so false positives corrupt files.
  Nested-JSON blindness WAS a second gap and is closed (2026-08-25) — both generic
  patterns now tolerate JSON quoting/escaping, which matters because a
  `tool_response` is JSON and every MCP result arrives escaped. **The JSON-escape
  boundary lives in the REPLACEMENT step, not in the pattern, and moving it back
  would reopen a hole.** Expressing "the value must not end in a backslash" as a
  regex made a value of exactly 8 characters ending in one match NOTHING — no
  split satisfies both the 8-char floor and a non-backslash tail — so a real
  secret went out unredacted while the JSON case looked fixed.
  `TestRedact_ValueEndingInBackslash` and
  `TestRedact_JSONShapedSecrets/escaping_survives` pin the two directions
  together; a change that satisfies one alone has shipped once already. One deliberate
  exception stands: **a gated shell/MCP call sends its command VERBATIM**, because
  policy must judge the command that will actually run (ADR-0017 §Content, amended).
  Keep `docs/data-and-privacy.md` true.
- **`usage.go`'s INV-2 guarantee is an allowlist now, not an impossibility.** It
  used to hold structurally — the transcript projection bound only numeric fields,
  so content had nowhere to land. Binding `message.model` (required: the model id is
  the backend's aggregation key) replaced that with a curated allowlist enforced by
  the sentinel test. **That test is load-bearing.** A change that makes it pass
  trivially is a defect, and a second bound string needs an ADR amendment, not a
  commit. One has since been added THAT WAY: `message.content[].thinking`, under
  the ADR-0014 amendment (2026-08-25) — and it is CONTENT, not an identifier, so
  the allowlist's contents stopped being self-limiting at the same time. A fifth
  bound field needs its own amendment for the same reason.
- **Decisions only a human can make** (scope, privacy posture, priority) are `OD*`
  decisions: surface them, never infer them.
- **Cite sources in docs** — the repo symbol/path or upstream doc URL behind each
  claim. A governance product that overstates itself is the failure it exists to
  prevent, so prefer an honest limit over a confident sentence.
- **Verify against the real thing.** `testbed/run-all.sh` drives real headless
  sessions against a real local stack and asserts what arrived; unit tests are not
  evidence that a hook works.
- Sibling repos: **openbox-backend** (NestJS control plane), **openbox-core** (Go data
  plane), **openbox-temporal-sdk-python** (agent-runtime SDK).

## Build and test

```bash
go build ./cli/...                 # the binary
cd <module> && go test ./...        # per module (go.work lists them all, ADR-0011)
./testbed/run-all.sh               # end to end, needs a local OpenBox stack
```

## Current state

Shipped and verified end to end: observe telemetry, enforcement, the E9 approval
loop with hold + rewake, the autonomous approver (ADR-0012), lineage (trailer →
signed attestation → deploy → queryable links) including its read side, and the
managed-config posture layer.

**`/evaluate` is the only decider** (ADR-0017, 2026-08-13). The local Go policy
evaluator, the bundle, its signature check, `policysync`, `openbox dev sync` and
the session-start staleness gate are deleted. Enforcement is three independent
named things now, not three tiers: local secret redaction, inline evaluation,
findings. Four things about it are worth not re-litigating:

- **The gate consults nothing local.** `ShouldEscalate` is the only condition;
  every gated class goes to the server, because risk is a property of the POLICY
  and the engine deciding which calls deserved a real verdict was the engine
  second-guessing the thing meant to decide. The narrowing it replaced is why a
  **raw-rego** org was ungoverned on everything but shell and MCP: the evaluator
  could not evaluate their policy at all, so it served fail-open.
- **`ApplyFailurePolicy` must run AFTER the evaluation, never before.** It sat
  between the local verdict and the escalation, which was harmless while the local
  step produced verdicts. A local step that always reports "no verdict" turns an
  early call into an immediate synthesized HALT under `fail_closed`, which then
  reads as "already tightened" and suppresses the round-trip — **a fail-closed org
  would deny every gated call without ever asking.** Conformance case C1 caught it.
- **Deprecated keys must stay reachable to warn.** `tier2`, `tier2_timeout_ms` and
  `require_verified_bundle` still parse and do nothing. `tier2` is deliberately NOT
  honoured: an org that set `tier2:false` under the old design would otherwise
  upgrade into silent enforcement. The warning lives in `EffectivePosture`, not on
  the resolver — ADR-0017 removed the last runtime caller of `ResolveTier2`, so a
  notice hung there could never fire, which is the same as no notice.
- **Approval identity did NOT move, deliberately.** Only shell and MCP have a
  retry-stable operation id; every other class keys on the invocation, so an
  approval for a `Write` cannot be matched after a retry and the call is re-asked.
  Fixing it means changing `activity_id` — this product's event identity, byte-
  pinned and load-bearing for core's dedupe — so it is its own decision.
  `TestUngatedClassesKeepInvocationScopedIdentity` pins the limit and its safe
  direction: over-ask, never over-grant.

**Status: implemented, unit-verified, all 11 modules green under `-race` plus both
cross-compiles — the testbed has NOT run.** The conformance cases drive the real
hook against a real `/evaluate` stub over HTTP, so deny/allow, redact-before-send
(asserted on the outbound bytes) and both failure-policy branches are strongly
covered. What that cannot reach is the claim the ADR rests on: **that a raw-rego
org is now enforced.** `testbed/30-enforce.sh` §A publishes a raw-rego deny through
the backend to prove it and is waiting on a stack. The lost-200 double-store window
is also still open and irreducible client-side — closing it needs server-side
dedupe on developer events, a backend ask.
`plans/260813-0140-inline-policy-evaluation/reports/verification-260813-inline-evaluation.md`
splits every claim by evidence strength;
`plans/260813-0140-inline-policy-evaluation/manual-test-guide.md` is the stack-free
walkthrough.

**Setup is two commands** (ADR-0015 + ADR-0016, 2026-08-13): `openbox auth`
authenticates a MACHINE — collects or registers credentials and writes
`~/.openbox/.env` (secrets) plus `dev.json` (coordinates) — then `openbox init
--provider <tool>` sets up hooks at a scope and writes posture. `auth` takes no
`--provider`, `--org` or `--agent-name`: the agent is machine-scoped, so a
per-tool flag on it was a contradiction, and nothing ever read `Org`. The
agent-id prompt **never prefills**, because `prompt.Line` returns its default on
empty input — offering the stored id made "blank registers a new agent"
unachievable and demanded a key the user did not have. Blanking it would erase
`agent_id`, which feeds `SelfAgentID` in the autonomous approver (ADR-0012), so
the credential-reuse path returns it from `dev.json` like it already did the DID.
Four things changed with the original split, and each has a reason worth not
re-litigating:

- **The OS keychain is deleted, not demoted.** Credentials are one plaintext file,
  `0600` on unix and unprotected on Windows. This is a real security downgrade and
  ADR-0015 argues it against the prior rationale rather than around it: the store
  was unlocked for the whole desktop session and readable by `security`, so it never
  defended against the agent this product governs — while costing three config
  paths, a build-tag split, and the two-DID-stores revert bug. **`init` can no
  longer read, write or prompt for a secret at all**, which is the compensating
  gain.
- **One store per field.** `.env` holds only secrets; `dev.json` only coordinates.
  `TestEnvFileIsNotACoordinateSource` is the tripwire — a DID in `.env` must stay
  ignored. Relaxing it reopens the bug where a stale second copy reverted a
  corrected DID on every install.
- **`init` defaults to project scope and to ENFORCE** (ADR-0016). Project scope
  because it is the only scope the CLI can actually activate — global needs a
  managed-settings deployment — so the old default reported success while governing
  nothing. Enforce because a default-off headline feature stays off; `ResolveFinops`
  proved that once already. Both mitigations must stay true: enforcement is inert
  without an org policy, and `fail_closed` stays off.
- **A persisting opt-out needed TWO mechanisms, and shipping one was a real bug.**
  (a) `Enforce` became `*bool`, because `omitempty` drops an explicit `false` on
  write — so `--enforce=false` would have vanished from the file. The read side was
  already safe: the key-presence map in `resolveBoolWithSource` handles a plain-bool
  accessor, which was checked rather than assumed from the `Finops` precedent.
  (b) **`o.Enforce` must stay nil when a run says nothing about enforce.** Because
  the flag *defaults to true*, its value alone cannot distinguish "asked to enforce"
  from "said nothing" — and assigning it unconditionally made every plain `init`
  write `enforce:true`, silently reverting a deliberate opt-out on the next
  unrelated re-run. `flagPassed` exists for that distinction. Fifteen green enforce
  tests missed it because each ran `init` exactly **once**;
  `TestPlainReInitDoesNotRevertAnEnforceOptOut` is the one that holds it.
  The general rule for the next default flip: **check reads and writes separately,
  and test the second invocation.**

`OPENBOX_ED25519_SEED` became `OPENBOX_AGENT_PRIVATE_KEY` — the name the platform's
own SDK docs use, which this repo had never honoured. Both old names (and the git
action's `OPENBOX_SEED`) still read, warning once to stderr.

**Status: implemented, unit-verified, exercised against the real binary over a PTY,
Windows/linux-arm64 cross-compile added to CI and proven to catch a unix-only call —
but the testbed has NOT run.** No local stack was reachable. What that leaves
unproven is the thing that matters most: that a real session after `auth` → `init`
produces events, and that a session in an uninitialized directory produces none.
`plans/260812-1212-openbox-auth-command/reports/verification-260813-auth-init.md`
splits every claim by evidence strength and carries the per-OS manual checklist.

Near-real-time delivery (`hookflow.RealtimeTrigger`: debounced detached flusher per
session, default on, `OPENBOX_REALTIME=0` opt-out) is implemented and verified at
the binary level (`TestHookRealtimeDelivery` drives the real binary against a mock
core); its testbed phase (`testbed/25-realtime.sh`) exists but has not yet run
against a live local stack.

**Model turns are Activities too** (ADR-0014, 2026-08-11): `TurnStarted`/
`TurnCompleted` → `ActivityStarted`/`ActivityCompleted` with
`activity_type: "llm_completion"` and `{model, usage{4 counts}}` in
`activity_output` — the AI-Agent `llm_completion` span's `response_body` shape on
the activity carrier, because dev sessions wrote no spans at the time (ADR-0018
later added one to this same event, for the assistant TEXT; the usage numbers
stayed on `activity_output`). Claude Code emits one pair
per turn from new `Stop`/`SubagentStop` hooks over a byte-offset cursor
(`hookflow.TurnCursor`, agent-scoped, spool-then-cursor ordering so a crash
over-reports into core's dedupe rather than losing a turn); Codex emits one
`<session>:usage:rollup` pair at SessionEnd, its `Stop` deliberately unwired.
`client.Tokens` gained both cache counts and **`Input` is now pure input** — the
one non-additive change, which is why the contract is **v1.1**. `Finops` became
`*bool` before the default could flip: as a plain bool an absent config field and
an explicit `false` were indistinguishable, so the flip would have been a silent
no-op. **Status: implemented, unit-verified, reviewed, NOT yet run against a live
stack.** The core-side extractor has since **merged** — `ExtractModelMetricsFromActivity`
is on `develop` (PR #125 / PROD-296, merged as `0643ad3`, verified at 68f0398), and
the same change excludes `llm_completion` from core's *tool* metrics. This
paragraph used to say "write-only, awaiting merge" and that the tool-metric
pollution was live; both are retired.

**Tool events are Activities** (ADR-0013, 2026-08-11): `ToolCall` →
`ActivityStarted`, `ToolResult` → `ActivityCompleted`, both span-less and
hook-less — still true for TOOL events after ADR-0018, which added a span to one
turn carrier only. `client/hookspan.go` and `client/spanbuilder.go` are deleted, and with
them ADR-0004's standing mirror obligation. The adapter-facing schema did not
change. **Status: implemented and unit-verified, NOT yet run against a live
stack.** `activity_id` is byte-identical to the old shape (pinned in
`client/approval_key_pin_test.go`), the golden fixtures pin the new wire bytes,
and all 11 modules are green — but core's ingest behavior was established by
reading openbox-core, and this repo's own rule is that reading is not evidence.
The load-bearing unverified claim is that core stores the `ActivityCompleted` as
its own row; its dedupe key includes `event_type`
(`activities/governance/validation.go:96`), which says it should. The testbed
assertions are updated and waiting for a run; `MAPPING.md` §7 lists exactly what
that run must confirm.

Known limits, documented in
`docs/architecture.md#assurance--what-the-evidence-proves`: the signing key is
readable by anything running as the developer, a default install governs one
directory so absence of events is not evidence of absence of work, the backend does
enforcement depends on reaching the control plane and under the default
`fail_closed:false` is disabled by blocking one hostname, content-based policy sees
at most the first 64KB of any body, local secret detection is keyword-driven for
assignment shapes so an unlabelled high-entropy value below the 4.5-bit floor is
invisible to it (the named-format half is gitleaks' 222 rules, precise but brittle
to a provider changing a key format), Codex's hook
cannot be mandated by `requirements.toml`, Guardrail redaction at source is not
wired, the production-runtime lineage hop is not joined, Codex reports usage per
session rather than per turn, reports no tool success at all, captures neither
tool content nor thinking and redacts nothing it sends (so the two providers differ
in both the amount of content AND whether it is scanned — stated in `COVERAGE.md` §3
rather than averaged away), model calls are unobserved unless the opt-in gateway is
installed and are not refused even then, goal
alignment
additionally requires `finops` ∧ `content_capture` (both default-ON, a user
constraint) plus a reachable LlamaFirewall and Redis — without those the widgets
stay empty with a perfect client — and Windows is build-verified only.

Two things about the turn feature stay empirically open until the testbed runs, and
both are pre-decided either way rather than blocking: whether `Stop` fires on a
tool-only turn (window sums are exact regardless of cadence), and whether
`SubagentStop`'s transcript window carries `isSidechain` lines (the partition cannot
double-count in any case; its worst case is a subagent reporting nothing). The
static measurement behind both is in
`plans/260811-1640-coding-agent-token-usage/reports/measure-260811-transcript-turn-surface.md`.

**Tool outcome, failure signals, and one content-bearing span** (ADR-0018,
2026-08-13, contract **v1.2**): `status` (`completed`|`failed`) on tool
`ActivityCompleted`; `PostToolUseFailure`/`SubagentStart`/`PermissionDenied`/
`StopFailure` wired; and ONE span on a `TurnCompleted` carrying the assistant's
reply. Five things are worth not re-litigating:

- **`status` is ungated and tool-only.** Ungated because a two-literal enum
  derived from which hook fired cannot encode content, and Tool Health must not
  depend on a privacy setting. Tool-only because `payload.status` also writes
  `governance_events.workflow_status` for *any* event type, where on a lifecycle
  event it means something else. `client.statusFor` enforces both.
- **The turn span's `http.*` attributes are synthesized and must stay.** Core
  RECOMPUTES `semantic_type` per span, and `isLLMCall` is the only path to
  `llm_completion`. Deleting them does not error — the span still stores,
  classifies as something else, and alignment silently dies. They retire when
  [openbox-core#130](https://github.com/OpenBox-AI/openbox-core/issues/130) lands
  and the text moves to `activity_output.message`.
- **The three new signals must carry NO `signal_args`.** Core reads any
  `SignalReceived` with non-empty `signal_args` as a NEW USER GOAL and overwrites
  the alignment session's goal with it (`age.go:112-137`). Surfacing the denied
  tool there — the field the Verify tab renders as "Input" — would replace the
  developer's prompt as the thing every later turn is scored against.
  `TestNewSignalsCarryNoSignalArgs` holds it.
- **INV-2's transcript line did NOT move.** The assistant text comes from the
  `Stop`/`SubagentStop` hook field, so `usage.go`'s allowlist and its sentinel are
  untouched — and the sentinel gained an adversarial section proving exactly that
  (capture ON + a poisoned transcript ⇒ transcript sentinels still absent). What
  is no longer structural is the guarantee for that ONE field: gate + redaction +
  cap, each asserted on outbound bytes. `secret_detection:false` ⇒ unredacted.
- **The new hooks need a re-init.** They are registered by the installer, so an
  existing install emits none of them until `openbox init` re-runs. An older
  Claude Code silently ignores unknown hook keys (verified), so registering them
  is safe.

Three of the plan's assumptions did not survive the probe against the installed
2.1.229 and should not be re-introduced: `classifier_verdict` does not exist on
`PermissionDenied` (its free-text field is `reason`), `Stop` carries no
`stop_reason`, and `PostToolUseFailure` does carry a structural `is_interrupt`.
**Status: implemented, unit- and conformance-verified (C20–C26 assert on the real
outbound bytes), all 11 modules green under `-race` plus both cross-compiles — the
testbed has NOT run.** `plans/260813-2314-dev-telemetry-and-content-posture/manual-test-guide.md`
is the stack-free walkthrough; `plans/reports/probe-260813-2329-claude-code-hook-surface.md`
is the hook-surface evidence.

**`init` replaces its own stale-path registrations; posture reports who decides**
(2026-08-14, no ADR — no new table, endpoint or service). Two defects found
verifying ADR-0018 against the first real session's 66 events:

- **The double-count was two engines, not an ADR-0018 defect.** `writeLocalHooks`
  matched its own entries by exact command string, so an entry written by an
  install run with a different `HOME` read as a FOREIGN hook and was preserved;
  re-init appended beside it and reported success. Both engines fired, every
  governed tool call stored twice, and the older engine omitted `status` — which
  reads exactly like the new status work being broken. It is not. Ownership is
  decided by ARGV SHAPE now (`ownedLocalHook`, ported from Codex's installer,
  which never had this bug), stale-path entries are replaced, and the swap prints
  what it retired. Genuinely foreign hooks — including a compound command that
  merely embeds our invocation — are still preserved, which is the direction of
  error to keep: over-keep, never over-delete. `openbox doctor` grew a
  duplicate-engine warning built on the SAME classifier, deliberately, so the
  check and the fix cannot disagree about what "ours" means; it reaches the
  adapter through `cli/internal/providers` because `TestOnlyTheRegistryImportsAdapters`
  forbids `cli/cmd/openbox` importing an adapter directly. Two limits stay: the
  repair happens only on the next `init` in that directory, and already-stored
  duplicates are not corrected.
  **The sweep de-duplicates, it does not only replace** — and shipping the
  replace half alone was a real bug. Doctor reports TWO conditions (a second
  engine, and one invocation registered twice at one path) and names `init` as
  the remedy for both; `init` originally repaired only the first, so the second
  warned forever through the command it recommended. Dedupe is keyed by
  INVOCATION, not by event: PreToolUse legitimately carries two of ours (the gate
  and the rewake watcher), and collapsing by event would delete the watcher and
  no approval hold would ever wake. The general rule: **when a diagnostic names a
  remedy, run the remedy in the state it warns about** — a check and a fix built
  on one classifier can still disagree about what the fix covers.
- **`decision_authority` never reached the wire.** ADR-0017:237-239 says posture
  carries it; `Posture.Metadata()` — the only path onto the wire — omitted it and
  `failure_policy` both, while `openbox doctor` printed both off the struct. Local
  view complete, remote view silent: the inverse of what that ADR argues. The gap
  shipped because `TestPostureReportsDecisionProvenance` asserted the STRUCT and
  never called `.Metadata()`; the new subtest crosses that seam, and the rule
  generalizes — **asserting the struct is not asserting the wire.** The five
  bundle-era keys (`bundle_version`, `bundle_policy_id`, `bundle_sha256`,
  `staleness`, `bundle_integrity`), the `Staleness` type and its 7 consts are
  deleted outright, per the `require_verified_bundle` precedent; absence is
  asserted rather than left to the empty-value guard. `adapters/common/git`
  defines its OWN `BundlePolicyID`/`BundleSHA256` for the attestation envelope —
  different struct, shipping feature, do not rename repo-wide.

**Status: implemented, unit-verified, all 11 modules green under `-race` plus both
cross-compiles; `doctor`'s warning exercised against the real binary on a seeded
two-engine project — the testbed has NOT run.** Unproven without a live stack:
that a re-inited settings file still produces events in a real session, that the
double-count disappears end to end, and that `decision_authority` lands in
`governance_events` — `testbed/30-enforce.sh:185-186` already asserts the last one
and failed before this change. `testbed/10-onboard.sh` gained the dormant
stale-path replacement assertions.

**Prompts gate, and HALT ends the session** (ADR-0020, 2026-08-18): UserPromptSubmit
runs the SAME shared EnforceGate as PreToolUse (block/erase on HALT/BLOCK, hold on
REQUIRE_APPROVAL), and a HALT the control plane returns renders `continue:false` +
a local latch (`halted-sessions/`) that refuses every later gated hook in that
session with no re-evaluation — resume included. Four things not to re-litigate:

- **The kill discriminator is exact:** `Verdict==HALT && Source==evaluate &&
  !FailOpen`, marked AFTER the approval hold. `ApprovalUndecided` got its own
  source (`approval:undecided`) BECAUSE it synthesizes a HALT while leaving
  Source=evaluate — without that, every hold timeout would kill the session.
  `TestApplyDecisionSessionHaltSplit` and C27–C31 pin all of it.
- **Every server HALT kills, authored or not** (owner decision): the unauthored-
  HALT core defect now ENDS sessions instead of denying calls until it clears.
  Deliberate — client-side verdict discrimination stays rejected (plan
  260814-2235); the remedy is the pending core fix.
- **Codex renders HALT as deny explicitly.** Its `Render` writes NOTHING for an
  unknown literal, so omitting the `DecisionHalt` case would make HALT silently
  PROCEED there. No latch is written for Codex (keyed on the session stop being
  expressed); Codex session-kill is an open follow-up.
- **The prompt gate needs a re-init** (UserPromptSubmit timeout 5→30 +
  statusMessage, mirrored in localhooks.go AND plugin/hooks/hooks.json — a pin
  test holds them together). Prompt approval keys are session-coarse
  (`activityPairKey` has no span for signals); identity is byte-pinned, so the
  hold's failure modes all land on block — over-ask, never over-grant.

**Status: implemented, unit- and conformance-verified (C27–C31 on real RunHook
stdout bytes), all 11 modules green under `-race` plus both cross-compiles — the
testbed has NOT run.** `testbed/30-enforce.sh` §A3 (raw-rego halt → turn stop +
latch) is dormant, waiting on a stack.

**Tool content capture; SL3-SEC-3 retired** (ADR-0019 P1, contract **v1.3**,
2026-08-25 — phase 01 of plan 260825-0027, which ships alone). The Claude Code
adapter bound `tool_response`, tool input on the **observe** path,
`PostToolUseFailure.error`, `PermissionDenied.reason` and `StopFailure.error_details`;
all gated on the SAME `content_capture` key, redacted before attach, capped at 64KB.
Four things not to re-litigate:

- **`Content.Output` is turn text and must stay that.** Tool output got its own
  `Content.ToolOutput` → `activity_output.output`. One shared field would put turn
  text on tool events and tool output into core's alignment extractor the moment
  either mapping slipped.
- **The signals' free text rides `metadata`, never `signal_args`** — core reads a
  `SignalReceived` with non-empty `signal_args` as a NEW USER GOAL (`age.go:112-137`).
  Keys are `denial_reason` / `error_details`, deliberately NOT the provider's own
  names: `reason` is already a closed enum on SessionEnd metadata, and `error` is
  one JSON key on two hooks (free text on PostToolUseFailure, a closed provider
  enum on StopFailure). Only the routing keeps free text off the UNGATED enum fields.
  No core reader renders these yet — stored and queryable, like `metadata.event_id`.
- **The guarantee got weaker in kind, not just in scope.** SL3-SEC-3 held
  structurally (content had no field to land in); what replaces it is a gate + a
  redaction + a cap, each fallible. So the capture-OFF half is asserted wherever the
  ON half is, on outbound bytes (C32–C39), and the testbed was INVERTED rather than
  deleted — `20-capture.sh` asserts the gate open, `35-telemetry.sh` closed. "The
  marker is nowhere" and "the runtime emitted nothing" are the same observation.
- **C39 measures detector REACH, which C34's ordering case does not.** The boundary
  is the keyword, not the charset; one leg asserts an unlabelled hex token DOES
  leak. That is the honest limit, pinned so closing it is a decision — and it is not
  fixable by lowering the entropy floor (see the privacy bullet above).

**Status: implemented, unit- and conformance-verified, all 11 modules green under
`-race` plus both cross-compiles — the testbed has NOT run.** Unproven without a
stack: that core stores `activity_output.output` as the row's `output`, that the two
new metadata keys survive ingest, and the volume question — 64KB bodies at
tool-call cadence through the realtime flusher. Codex is untouched and binds none of
this, so the two providers now send different amounts of content under one posture
(stated in `COVERAGE.md` §3.4 rather than averaged away).

**Thinking capture; the allowlist is amended** (ADR-0019 P3, contract **v1.4**,
2026-08-25 — phase 02 of plan 260825-0027, Track A complete). The turn's
extended-thinking blocks egress as `activity_output.thinking` on `TurnCompleted`,
under the same `content_capture` key, redacted before attach, capped at 64KB.
Five things not to re-litigate:

- **This is the amendment ADR-0014 warned about, and it changed KIND.** The three
  fields that ADR-0014 bound were an identifier, a timestamp and a bool — none
  could carry a secret, so the allowlist's *contents* were self-limiting even
  after its *form* stopped being structural. `thinking` is the first free-form
  content string in that projection, and the densest content this client carries:
  it restates prompts, file bodies, and any credential the turn saw. What protects
  it is three fallible mechanisms in a fixed order — gate, redact, cap — each
  asserted on outbound bytes. The amendment section at the end of ADR-0014 is the
  decision; the code is the consequence.
- **The sentinel turned around and is mutation-tested, literally.** `TestFinops_NoContentOnWire`
  now asserts absence with capture off (thinking included) and PRESENCE with it on,
  in the decoded `activity_output.thinking`, with every other transcript sentinel
  still absent — so the widening is exactly one field wide. Deleting the redaction
  ⇒ red (raw AWS key on the wire); deleting the cap ⇒ red (tail marker past 65,536
  runes). Both drills were run, not asserted. **A version that passes with either
  mechanism removed is a defect.**
- **`message.content` is bound as `json.RawMessage`, and that is load-bearing.**
  It is a STRING on user lines and an ARRAY on assistant ones; a typed slice would
  fail the whole line's unmarshal on a string and silently drop that line's token
  counts — a finops regression with no error anywhere.
  `TestTurnWindow_StringMessageContentStillCounts` holds it.
- **Thinking must NOT ride the assistant span.** That span exists for one reader,
  core's alignment extractor, which takes it as the assistant's REPLY; chain-of-
  thought there would score every later turn's drift against the model's reasoning
  and fail silently. Asserted at both levels (sentinel §f, conformance C40).
- **Two bounds, ordered deliberately.** `maxThinkingBytes` (4×65,536 BYTES) bounds
  the collection so the streaming window reader keeps its memory guarantee;
  `capBody` (65,536 RUNES) bounds the wire. The collection bound must stay the
  LARGER one or the cap's mutation control goes vacuous —
  `TestThinkingCollectionBoundExceedsTheWireCap` exists so a future "unify the
  constants" commit cannot kill it silently.

Two deliberate narrowings from the plan: intermediate assistant text was NOT bound
(the final reply already egresses from the hook field ADR-0018 bound), and the LIFT
is ungated — only the attachment is gated, because gating a pure parser buys
nothing on the wire and would duplicate the posture decision.

**Status: implemented, unit- and conformance-verified (C40/C41 on real POSTed
bytes), all 11 modules green under `-race` plus both cross-compiles — the testbed
has NOT run.** Unproven without a stack (`MAPPING.md` §7 items 22–24): that core
stores `activity_output.thinking` as its own key, that the sibling key does not
perturb `ExtractModelMetricsFromActivity`, and the volume question. A pre-existing
FALSE claim in `README.md` left over from ADR-0019 P1 ("tool commands and file
bodies are never sent on ordinary telemetry") was corrected in the same change.

**The local model-call gateway** (ADR-0021, contract **v1.5**, 2026-08-25/26): a
per-developer loopback daemon that Claude Code is pointed at via
`ANTHROPIC_BASE_URL`, relays every model call byte-identically, and captures the
exchange as a `TurnCompleted` carrying a real observed span. `openbox init --provider
claude-code --gateway` installs it; `--remove-gateway` backs it out. **Capture is
live; refusal is written and uncalled** — `gate.Decide`/`WriteRefusal` have no
production caller because probe A has not named a refusal shape, and a wrong one
silently disables a Claude Code capability for the rest of the session. Ten things
worth not re-litigating:

- **Opt-in, and Claude Code only.** ADR-0016's default-on lesson does NOT transfer:
  enforcement-by-default is inert without an org policy, this redirects live model
  traffic through a path that has never run against a real stack. And
  `--provider codex --gateway` used to install an Anthropic relay and point
  `~/.claude/settings.json` at it on a machine that reads neither.
- **Install ordering is a safety property, and so is rollback.** unit → start →
  PROVE it listens → only THEN write the env var; uninstall reverses it. Writing the
  var first points the tool at a dead port, and a dead localhost fails closed, so
  every model call on the machine fails while `init` prints success. Any failure
  after `WriteUnit` must ALSO remove the unit: `KeepAlive`/`Restart=always` would
  restart-loop a gateway the developer was never told about, `main.go` downgrades the
  error to a warning so `init` still exits 0, and the port pre-check then blocked the
  "re-run init" the error recommends.
- **`--remove-gateway` runs BEFORE the credential gate.** Removal must not require
  the thing being removed to still be usable — a wiped `~/.openbox` otherwise left
  every model call failing against a dead port with no CLI fix.
- **The span's `http_*` keys and the fingerprint's DUAL home are load-bearing.** Core
  recomputes `semantic_type` and `isLLMCall` is the only path to `llm_completion`;
  deleting the keys does not error, the span just classifies as something else and
  every reader goes quiet. `http_status` must serialize as **`http_status_code`** —
  core's `SpanData` spells it that way and `encoding/json` drops an unrecognized key
  silently, so the short spelling vanished before policy or storage saw it while
  every golden fixture stayed green (*asserting the outbound bytes is not asserting
  the receiving type*). The fingerprint rides `attributes["openbox.credential_fingerprint"]`
  because core has no field for the top-level key.
- **There is no `fail_closed` input to the gate at all, and that absence IS §7.** A
  posture key there would be a switch to turn the gateway's enforcement off. An
  uninterpretable verdict REFUSES — ADR-0020's Codex trap in a new place.
- **`Decision.Evaluated` makes "no synthesized refusal before an evaluation attempt"
  checkable.** The pre-ADR-0017 `ApplyFailurePolicy` bug, worse here: refusal on a
  missing verdict is unconditional, so the mistake turns every control-plane blip
  into a total model-call outage reported as a policy decision no policy made. The
  10s evaluate deadline is part of it — "never answers" is not "unreachable", and
  only a deadline converts the first into the second. A cancelled CALLER is a third
  case (`reasonCallerGone`): every Esc otherwise wrote a durable record blaming the
  control plane.
- **The two turn producers must never share an `activity_id`.** Both describe the
  same turn; core's dedupe would absorb one as a duplicate and half the evidence
  would vanish with no error. Hence `gateway_request_id` and the disjoint `:gateway:`
  namespace, and hence the schema's `oneOf` requiring exactly one discriminator.
  Also: the gateway span carries the provider's RAW response body, not the shape
  core's alignment extractor parses, so a gateway-observed turn contributes nothing
  to goal alignment — a silent gap, not corruption.
- **A displaced `ANTHROPIC_BASE_URL` is REMEMBERED, first-writer-wins.**
  Key-ownership was the stated rule and it missed the case where the key is ours and
  the value is theirs: an org's own LiteLLM/Bedrock relay was overwritten on install
  and deleted on uninstall, after which `doctor` reported "not set", which reads as
  clean rather than as damage.
- **A content-encoded body is NOT captured.** The client's own `Accept-Encoding` is
  relayed verbatim, so an upstream may answer gzip; those bytes matched nothing in
  the detector, `utf8.ValidString` was false and `json.Marshal` rewrote every byte to
  U+FFFD — every redaction guarantee held vacuously and the evidence was destroyed
  anyway. A marker, not decompression: decompressing an upstream body on an
  unauthenticated loopback listener is a bomb surface.
- **launchd sends stdio to `/dev/null` by default**, and the emitter's throttled
  warnings are the ONLY signal that a perfectly working relay is recording nothing
  (no DID, or no session header). `doctor` reports alive/configured/bypass and never
  "is it recording". Hence `StandardOutPath`/`StandardErrorPath` →
  `~/.openbox/gateway.log`.

**How capture came to be absent for as long as it was, because the shape recurs:**
`WithCapture` had no production caller, so `g.emitter` was nil and every capture was
discarded — while package `gateway` tested the relay against a stub `Emitter` and
package `client` tested the span builder against a hand-written `DevEvent`. **A fake
at each end of a seam with no implementation between them keeps both suites green and
proves nothing about the seam.** `cli/cmd/openbox/gatewaycapture_test.go` is the
control: it drives the real command into the real spool with no fake anywhere.

**Status: implemented, unit- and conformance-verified, green under `-race` plus both
cross-compiles — the testbed has NOT run.** `testbed/45-gateway.sh` is written and
dormant; `MAPPING.md` §7 items 25–30 list what a live run must confirm. ADR-0021
stays **DRAFT** deliberately: §§8–10 are empirical questions about a provider we do
not control (does subscription OAuth follow `ANTHROPIC_BASE_URL`; what refusal shape
Claude Code does not retry around; is an org matchable from an OAuth credential), and
filling one in by inference is the overstatement this product exists to prevent.

**Bounds have owners, and reusing one is a silent regression** (2026-08-26, the fix
series around the gateway). Four of these shipped together and the pattern is one
pattern:

- **`MaxCommandLen` (8 KiB) is LOCAL-only** — its own doc says it bounds the
  `DecisionRequest` command and is never egressed. It was reused as the egress bound
  for `content.tool_input`, so a 40 KB source file egressed as 8 KiB while
  `data-and-privacy.md` and this file both said 64KB, and core's Guardrails stage 0
  and every approver saw only that. Egress bounds are `MaxRedactBody` (scan) then
  `capBody` (wire).
- **The prompt was the ONE content field assigned directly** instead of through
  `m.redact`, so it egressed unscanned with `secret_detection` fully ON while
  `Map`'s own doc comment and `README` both described the opposite. **The Codex
  mapper still has no redactor at all** and the prompt is the only content it sends —
  documented in `COVERAGE.md` §3.4 rather than smoothed over.
- **A cap must be tested in the unit it claims.** `capHeaders` tested bytes and cut
  runes, so a CJK value between the two entered the branch and declined to truncate:
  4 bytes × 4096 runes = 16 KiB per value, ~1 MiB per map, exactly the loss the bound
  exists to prevent.
- **`contentMetadataKeys` is a backstop that must list every content key.**
  `arguments` (the MCP class's own key, sibling of two that were listed) and
  `thinking` were missing, so an adapter writing either straight into metadata routed
  around the gate.

Next: the Cursor adapter; policy template packs. The language floor is
`go 1.27.0` across `go.work` and all twelve modules, so every dependency resolves
at latest with no pin. **Dependencies are module-scoped now, not "one for the
repo"**: `cli` has `golang.org/x/term` (masked input, ADR-0015) and
`google/renameio/v2`; `contracts/dev-event/conformance` has
`santhosh-tekuri/jsonschema/v6` (+ `golang.org/x/text`, D-OSS-5), which reaches
the test graphs of both adapters and `client`; `adapters/common/devconfig` has
`pelletier/go-toml/v2` (D-OSS-6) and `joho/godotenv` (D-OSS-7);
`adapters/common/hookflow` has `google/renameio/v2` (D-OSS-8); `cli` also has
`kardianos/service` (D-OSS-3). **Seven external direct dependencies now, up from
one.** Three things follow.
**A new dependency in a shared module needs `go mod tidy` in every module that
transitively depends on it**, or `GOWORK=off` builds fail on a missing go.sum
entry while the workspace build stays green — the release path is the one that
breaks. And **`renameio` is `!windows` on every file**, so it sits behind a
build-tagged `atomicWriteFile` helper: unix fsyncs, Windows keeps the prior
temp+rename. Each module with a non-stdlib dependency carries its own allowlist
test — `gateway/guard_test.go`, `conformance/deps_test.go`, `decision/guard_test.go`
— and adding to one is a decision, which is why they fail first.

**`kardianos/service` owns the gateway's unit file but NOT its path, and that has a
test consequence** (D-OSS-3): on darwin it derives the home from `user.Current()`
and ignores `$HOME`, with no override, so calling `Install()` from a test writes a
live launchd unit into the runner's home. Production goes through the library;
`installUnitFn`/`uninstallUnitFn` in `initgateway.go` are the seam the nine
`setupGateway` tests use so `go test ./...` cannot install a daemon. Both paths emit
identical bytes — `TestSuppliedTemplatesSurviveRendering` pins that the library's
template render is an identity transform over our unit bodies. Its `Install()` also
REFUSES an existing unit, so `Reinstall` uninstalls first; dropping that reopens the
stale-unit bug where a moved binary left launchd restarting a path that no longer
existed. `launchctl` start/stop stayed ours: the library uses the older `load`/
`unload` where this repo uses `bootstrap`/`bootout`.

**The credential guard bounds DIRECT requires only** (ADR-0023). Its go.mod half
used to reject every requirement outside gateway's two-entry allowlist, transitive
ones included; that was untenable once an allowlisted module grew its own tree, and
enumerating the closure would make the allowlist unreadable — the one thing it
exists to be. So the bound moved: direct requires are checked at each module, and
transitive code is bounded at the module that took the dependency. **What is no
longer bounded by any test is arbitrary transitive code**, and that was already true
before — the old check matched only lines starting `github.com/`, so a direct
`golang.org/x/…` require was invisible to it. The host gap is closed while the scope
narrowed. Do not widen an allowlist to make a direct import pass; that inverts the
ADR's reasoning.

**`.env` parsing is godotenv's, at its defaults, and that removed two controls**
(D-OSS-7): a duplicate key is last-wins rather than an error, `$VAR` and `\n`
expand inside values, and **its parse error echoes the offending line**, so a
malformed line that is a bare secret leaks it into logs. Owner-ruled, pinned by
tests, recorded in ADR-0015 — do not "fix" it without reopening that ruling. (Upstreaming the
`shell`/`mcp`/`tool` hook types to `openbox-sdk-python` is no longer needed —
ADR-0013 retired the Go mirror by deleting the span layer it mirrored.)

The epic-by-epic history is in git, not in the tree: read commit messages and the
ADRs for *why*, and the code for *what is true now*.
