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
| `cli/` | the `openbox` CLI, incl. `cli/internal/approver` (ADR-0012) and `cli/internal/prompt` (masked input) |
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
  unredacted. Tool commands and file bodies never egress on observe events — but
  since ADR-0017 they DO on a **gated** call, every class, content_capture-gated:
  OpenBox cannot decide on content it cannot see. Local secret detection redacts
  the body **before** it is attached, and that ordering is the only in-transit
  control there is (`adapters/*/enforcetarget.go`, pinned by conformance case C18).
  The server sees at most the first 64KB. Keep `docs/data-and-privacy.md` true.
- **`usage.go`'s INV-2 guarantee is an allowlist now, not an impossibility.** It
  used to hold structurally — the transcript projection bound only numeric fields,
  so content had nowhere to land. Binding `message.model` (required: the model id is
  the backend's aggregation key) replaced that with a curated allowlist enforced by
  the sentinel test. **That test is load-bearing.** A change that makes it pass
  trivially is a defect, and a second bound string needs an ADR amendment, not a
  commit.
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
at most the first 64KB of a write, Codex's hook
cannot be mandated by `requirements.toml`, Guardrail redaction at source is not
wired, the production-runtime lineage hop is not joined, Codex reports usage per
session rather than per turn and reports no tool success at all, goal alignment
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

**Project assurance runs end to end now** (`plans/260825-1623-lean-openshell-project-assurance/`).
The public surface is `project inspect`, `evaluate`, `finalize`, `verify`,
`report`, `propose` — six subcommands, `docs/project-assurance.md` for the user
view. Codex, Claude/SRT, Seatbelt, governed-rerun and ProjectRun v2 execution
paths stay retired with no CLI fallback or hidden probe entrypoint; their audit-
pack v1 objects survive only as historical READ contracts. Phases 1–3 are
verified and Phase 4 is implemented through OS-04-03; **OS-04-04 is the one open
task** — a live GET-only finalization and a before/after zero-control-mutation
proof still need a host-side control token, and human report review remains.
Production data, credentials, control publication and automatic fixes stay out
of scope. Four things about this lane are worth not re-litigating:

- **The analyzer is a model, and that is not a contradiction of "no LLM
  verdicts."** The native-host skill may emit only an *issue candidate*;
  `securityreport.Prepare` then re-resolves every evidence citation against the
  sealed pack offline, relabels candidate prose as `*_assertion`, and keeps
  `observed_facts` limited to cited retained records. `severity` is pinned to
  the single literal `unavailable` at three independent layers (skill
  instruction, candidate schema `const`, report schema `const` plus the
  renderer) and `security_pass` is a hard `false`. Loosening any one of those
  turns an assertion into a finding.
- **The offline gate must stay ahead of credentials, network and output.**
  `Prepare` calls `resolveAbsentOutput` before it reads the pack, and
  `project_finalize.go` reads `OPENBOX_CONTROL_TOKEN` only after `Prepare`
  returns. Reordering it means an integrity failure that has already touched a
  credential.
- **The control token needs EXACT permissions.** Finalization rejects missing
  reads and unrelated write/approval authority equally, so
  `local-stack/scripts/bootstrap.sh` reconciles a retained key rather than only
  minting a new one. A key minted before that reconcile passes every earlier
  step and fails at finalize.
- **The model load has its own 30s budget** (`Dependencies.InferenceHTTP`),
  deliberately not the shared 10s `HTTP` client — that one is also the Core
  relay, where a longer per-request budget changes how long a stalled Core holds
  a relayed SDK call. The preflight requires the model UNLOADED, so every load
  is cold; a 2GB cold read took 14s against the old shared budget and made the
  lane an availability coin flip.

**There is deliberately no `assurance/fixture` or `assurance/securityanalysis`
package**, and git history will not show one — both were dropped before the
lane's first commit, so do not go looking for the deletion. Phase 0's ledger
(OS-00-03) retained a poison fixture, a safe sink, an in-process Ollama relay
and a standalone candidate oracle; Phases 1–4 then reimplemented every one of
them somewhere else and left the originals orphaned at 3,626 lines with zero
importers. The live owners are `evaluate/effect_relay.go` for the safe sink,
OpenShell's own provider route for inference (the CLI only *preflights* that
route), and `securityreport.Prepare` for candidate validation. Do not restore
the old packages to "reuse the fixtures" — the poison fixture in particular
belongs to the retired scenario machinery, and the current lane reports
`retrieval_poison` as a `missing` coverage channel on purpose.

The demo is two scripts under
`testbed/project-assurance/mastra-security-demo/` — `prepare-demo.zsh` then
`launch-claude.zsh`, with `RUNBOOK.md` for the end-to-end sequence and the
direct-CLI lane. There is no editor-task dependency; `.zed/` is gitignored.

Next: the Cursor adapter; policy template packs. The one dependency this repo now
has is `golang.org/x/term v0.34.0`, **pinned** — v0.35.0+ declares `go 1.24.0` and
would raise the language floor across all eleven modules; `go mod tidy` and
`go get -u` will both happily do that, so don't let them (the require block in
`cli/go.mod` says so too). (Upstreaming the
`shell`/`mcp`/`tool` hook types to `openbox-sdk-python` is no longer needed —
ADR-0013 retired the Go mirror by deleting the span layer it mirrored.)

The epic-by-epic history is in git, not in the tree: read commit messages and the
ADRs for *why*, and the code for *what is true now*.
