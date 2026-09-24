# OpenBox Codex adapter

The second realization of the generic Provider Adapter Contract (architecture
§1b), a 1:1 structural port of the Claude Code adapter: it maps Codex CLI's
native hooks onto the normalized developer event contract and emits
them through the shared AIP-signed transport on the E7 flat hook
wire. The **telemetry leg** described here is observe-only and fail-open: it can
never block, deny, or slow a Codex tool call (INV-3). The **enforce leg**
(`enforce.go`, `outputcontract.go`, `promptgate.go`, `permissiongate.go`) is a
separate path and does deny — three gate classes (`PreToolUse`,
`UserPromptSubmit`, `PermissionRequest`), each evaluated by `/evaluate`, on by
default. A session-terminating HALT on the prompt gate latches, and every later
gated call in that session replays it locally. See `capabilities.go`'s
`verdict.apply` and `enforce.rewrite` for what each may do, **and for the limits
that bound every enforcement claim here** — hooks are a guardrail, not a complete
boundary.

**Version floor: codex-cli >= 0.145.0. Verified against codex-cli
0.150.0-alpha.8.** Hooks are stable and ON by default (`codex features list`
reports `hooks  stable  true`; no flag to flip), `tool_use_id` exists on
Pre/PostToolUse, the `SessionEnd` hook exists, and `CODEX_THREAD_ID` is injected
into every exec env. This is a floor, not a pin: it says what the adapter needs,
and names the newest build it has actually been run against.

Surface truth: spike S5's 2026-07-23 addendum, `codex-rs` @ tag `rust-v0.145.0`,
and the hook payload JSON Schemas embedded in the binary
(`<event>.command.input`). The 0.150.0-alpha.8 schemas and live behaviour are
recorded in the phase 00 probe record
(`plans/260917-0225-codex-parity-with-claude-code/probes/`), which is the
citation for every "on this binary" claim below.

```
Codex hook (stdin JSON)
   └─ openbox hook codex <event>          # hooks.json wires each event here
        ├─ map → normalized the event contract DevEvent # mapper.go (no tool content)
        ├─ append → local spool           # spool.go (hot path: local I/O only)
        └─ exit 0, empty stdout           # Codex parses hook stdout as output JSON; we emit none
   SessionEnd / `openbox hook codex flush`
        └─ drain spool → client.Emit → POST /api/v1/governance/evaluate  (off the hot path)
```

The spool/flush/recovery design (why not emit inline, at-most-once +
undelivered-remainder recovery, deterministic `event_id`; INV-5) is identical to
the CC adapter; see `internal/adapters/claude-code/README.md`. Codex-specific:
the spool lives under `…/openbox/codex-spool` so a machine running both tools
never cross-drains, and the deterministic ids are namespaced `cdx-`.

## Event mapping

| Codex hook | the event contract `event_type` | Span (`semantic_type`) |
|---|---|---|
| `SessionStart` | `SessionStarted` |; |
| `UserPromptSubmit` | `PromptSubmitted` |; |
| `PreToolUse` | `ToolCall` | by tool (`stage=started`) |
| `PermissionRequest` | `PermissionRequest` |; |
| `PostToolUse` | `ToolResult` | by tool (`stage=completed`) |
| `Stop` | *(no signal)* — a turn pair, see below |; |
| `SubagentStart` | `SubagentStarted` |; |
| `SubagentStop` | *(no signal)* — a sidechain turn pair |; |
| `PreCompact` | `PreCompact` |; |
| `PostCompact` | `PostCompact` |; |
| `SessionEnd` | `SessionEnded` |; |

`Stop` and `SubagentStop` map to **no signal on purpose**. A turn boundary is not
a signal: each emits an `llm_completion` activity **pair** instead, under
`<session>:turn:N` (or `<session>:agent:<id>:turn:N`). That is the same shape the
Claude Code adapter uses, and it is why no `SubagentStopped` event type exists in
the shared vocabulary — adding one would be a second way to say what a turn pair
already says.

**`Interrupt` is not wired, and the reason is not a scope decision.** It is not a
Codex hook event at all: the binary embeds no `interrupt.command.input` or
`.output` schema and no `Interrupt` member of `HookEventNameWire`
(0.150.0-alpha.8). The only `interrupt` on this surface is the reserved boolean
inside `PermissionRequestDecisionWire`, which is documented to fail closed.

Codex **session ≡ thread**: `session_id` is the OpenBox session id.

Tool classification (`classifyTool`), grounded in `codex-rs`
`core/src/tools/hook_names.rs` + `registry.rs` @ rust-v0.145.0 and pinned by
`TestClassifyTool_GroundedLiterals`:

- `"Bash"` (yes; Codex serializes the Claude-compatible literal for its
  shell/unified-exec paths) → `shell`/`internal`
- `"apply_patch"` → `file`/`file_write` (its `Write`/`Edit` matcher aliases are
  never serialized as `tool_name`); no `file_path`; Codex's tool_input is the
  patch body (content), which we never decode
- `mcp__<server>__<tool>` → `mcp`/`mcp_tool_call`
- Everything else (`web_search`, `update_plan`, `view_image`, `spawn_agent`, …)
  → the coarse `shell`/`internal` catch-all; the real name rides `tool.name` +
  `metadata.tool_name`

## tool_use_id pairing (the improvement over Claude Code)

Codex gives every tool invocation a `tool_use_id` shared by its Pre/Post hooks.
The mapper threads it through the client's deterministic id derivation (the
`span.function` pairing slot for non-MCP tools; wire-neutral, verified by
`TestWire_ToolUseIDNeverRidesTheWire`), so:

- A call's started+completed halves share an **exact** `activity_id` (two
  identical sequential Bash calls no longer collide; the CC adapter's documented
  limitation);
- The an earlier decision duration stash is keyed per invocation (concurrent same-tool calls
  can't swap start times);
- `event_id` (INV-5) is per-invocation distinct.

MCP tools keep `span.function` = the real MCP function (it IS wire data) and
fall back to the CC-parity derivation; `tool_use_id`/`turn_id` ride tool-event
`metadata` for audit either way.

## Privacy

**Content capture is ON by default (2026-07-15)**, and three fields ride that one
gate: the developer's **prompt** on `PromptSubmitted`, and the turn's
**assistant text** (`last_assistant_message`) plus its **reasoning summary** on
the completed half of each turn pair. All three are opted out together with
`content_capture:false` / `OPENBOX_CONTENT_CAPTURE=0`.

All three are **redacted before attachment**, through the mapper's
`RedactContent` collaborator rather than at any call site, so a second caller of
`Map`/`MapTurn` inherits the scan instead of having to remember it. That
ordering is the only in-transit control there is. Redaction is keyword-driven,
so an unlabelled high-entropy value below the detector's floor stays invisible to
it: scanned is not the same as safe.

Unconditionally NOT captured: shell command strings, `apply_patch` bodies, and
tool output **never** ride an observe event; `tool_input` is kept as an opaque
blob the observe path never decodes, and `tool_response` is not even bound to a
field. The rollout reader binds an explicit **allowlist** of payload fields
(`rolloutAllowedPayloadFields`) and a test asserts that allowlist is exhaustive,
so a new vendor field cannot ride along unclassified — `encrypted_content` in
particular has no Go field at all. Asserted by
`TestMap_NoContentLeak`, `TestWire_NoContentLeakEndToEnd`, and the cli
real-binary E2E (`TestCodexUnifiedBinaryObserveE2E`).

## Commit attribution

Codex injects `CODEX_THREAD_ID` into **every** tool/shell exec environment, so
the shared prepare-commit-msg hook (`internal/adapters/common/git`) stamps
`OpenBox-Session:` directly from the env; highest precedence, **no liveness
registry** (the CC mechanism stays untouched and CC sessions never set the var).
Ambient hook install on SessionStart is on by default and opted out of with
`OPENBOX_INSTALL_GIT_HOOK=false`, exactly like CC. Commits typed in the user's own terminal
are an owner decision (deferred).

## Credentials & config (INV-1)

Identity comes from the same `openbox auth` / `openbox init` flow and the same
stores as every provider, via shared `internal/adapters/common/devconfig`:
secrets in `~/.openbox/.env` (plaintext, `0600` where the OS allows it),
non-secret coordinates in `~/.openbox/dev.json`.
The hook reads the **DID only** on the hot path; the obx_ key + Ed25519 seed are
read only at flush, straight into the client. `hooks.json` carries the engine
path + event names only; no key, DID, or URL.

## Packaging & install

`openbox init --provider codex` writes OpenBox-owned entries for the eleven
events into `$CODEX_HOME/hooks.json` (default `~/.codex/hooks.json`), each
`{"type":"command","command":"\"<engine>\" hook codex <Event>","timeout":5}` (3
s for SessionEnd, 30 s for the gated hooks; Codex timeouts are in seconds), with
matcher `"*"` on the tool hooks. The **three gating** hooks — `PreToolUse`,
`PermissionRequest` and `UserPromptSubmit` — carry the raised 30 s ceiling
(`installer.go`'s `gatedHooks`, the one source of truth the runtime gate check
reads too), because each can hold for a real approval decision or a gate drain
of its own session's backlog; a tighter bound on any of them would time out
mid-decision and fail the call open. The ceiling is not a delay; the engine
spends it only when a high-risk class escalated and core filed an approval, or
when a gate drains a backlog within its own slack.

SessionEnd's 3 s is **not a choice**: Codex clamps that hook to 3 s and logs
that it did (`clamping SessionEnd hook timeout to 3s in <hooks.json>`, measured
on 0.150.0-alpha.8). Registering the old 15 was fiction, and the engine's drain
budget sat above the real ceiling, so the flush was killed mid-drain every time.
The budget now sits under it: SessionEnd sends inline, in the hook itself,
within its own 2 s window (1 s per attempt, `hookrun.go`'s
`sessionEndInlineWindow`/`sessionEndAttemptTimeout`), not the detached
`flush` subcommand's own 60 s budget (`flushBudget`), which only bounds the
spawned fallback flusher SessionEnd starts when its inline window runs out
before every queued event was attempted. Nothing is lost to the smaller
window: every hook nudges a realtime flush or, for SessionStart/SessionEnd,
attempts delivery inline first, and whatever a session's own inline attempt or
nudge leaves queued is later swept by the periodic sweep (30-day retirement
for a genuinely unattempted backlog) -- single-attempt delivery has no
carry-over: an attempted-and-refused event is ledgered and gone immediately,
never held for a later retry. The engine path is
this running `openbox` binary (`os.Executable` via the CLI registry; the CC
`EngineBinary` precedent; no bundle, no binary copy). The merge is **idempotent
and ownership-aware**: re-install updates entries whose command invokes `… hook
codex …` (or a mangled `… hook claude-code …` import; Codex can migrate Claude
Code configs) in place, never duplicates, and never touches foreign entries; an
unparsable pre-existing file is refused, not clobbered.

**Trust step:** Codex hash-trusts non-managed hooks; after (re-)install, run
`/hooks` inside Codex to trust the OpenBox entries or they will not run.
`--disable hooks` and `--dangerously-bypass-hook-trust` remain user-side bypass
vectors; acceptable for the observe posture (NFR-5 parity with CC's opt-in
pilot); requirements.toml-managed hooks and the Codex plugin channel are the
recorded hardening/distribution options.

## Finops / token usage

Codex hooks expose no usage, but the session's **rollout JSONL**; the file the
SessionEnd payload's `transcript_path` points at, flushed by Codex *before* the
SessionEnd hook runs (spike S5 addendum #10); carries running token counts.
Behind the **default-on** `finops` flag (`dev.json` / `OPENBOX_FINOPS=0` to opt
out; a **separate** flag from `content_capture`), the adapter reads it on
SessionEnd (off the hot path, after the spool flush) and emits two things: the
`client.Tokens` rollup on the `SessionEnded` event, and a session-rollup
`llm_completion` activity pair (`activity_id <session>:usage:rollup`) carrying
the four counts plus the model id read from `turn_context.payload.model`.

**Session, not per turn; by choice.** Codex v0.145.0 exposes a `Stop` hook and
this adapter does not wire it, so usage arrives once per session. That is scope,
not a provider limit: the upgrade path is to subscribe `Stop` and take the
per-turn delta from `last_token_usage`. Claude Code, whose `Stop` *is* wired,
gets per-turn pairs.

**Cache counts are sub-counts here, not siblings.** `cached_input_tokens` and
`cache_write_input_tokens` are already inside `input_tokens` (evidence:
`total_tokens == input_tokens + output_tokens` in both the fixture and 12 real
rollouts), so the reader subtracts them to report pure input; the inverse of
Claude Code, whose cache counts are additive. Adding them would double-count the
cache on every session. `Capabilities` → `telemetry.tokens=true`,
`telemetry.model=true`. `usage.go` / `usage_test.go`.

**Grounded token shape** (codex-rs @ `rust-v0.145.0`, recorded in
`testdata/rollout-poisoned.jsonl`; pinned from the shipped structs + the 0.145.0
binary, *not* guessed; this box carried no live rollout to sample):

```
rollout line   = {"timestamp":…,"type":"event_msg","payload":<EventMsg>}
EventMsg        = {"type":"token_count","info":<TokenUsageInfo>,"rate_limits":…}
TokenUsageInfo  = {"total_token_usage":<TokenUsage>,"last_token_usage":<TokenUsage>,"model_context_window":int|null}
TokenUsage      = {"input_tokens","cached_input_tokens","cache_write_input_tokens",
                   "output_tokens","reasoning_output_tokens","total_tokens"}  (all i64)
```

Two **deliberate divergences from the CC reader**, both source-verified:

- **Aggregation = last snapshot, NOT sum.** `total_token_usage` is a *cumulative
  running session total* (`TokenUsageInfo::append_last_usage` →
  `total_token_usage.add_assign(last)`), so multiple `token_count` lines each
  carry a larger cumulative; the rollup is the **last valid** snapshot. Summing
  (as CC does over per-turn usages) would multiply-count every prior turn.
- **Cache/reasoning counts are subsets, NOT additive.** `cached_input_tokens` /
  `cache_write_input_tokens` / `reasoning_output_tokens` are already *inside*
  `input_tokens` / `output_tokens` (`non_cached_input == input_tokens −
  cached_input_tokens`; `total_tokens == input + output`), so `Input`/`Output`
  are carried **directly** and the sub-counts are never added. CC's cache tokens
  are additive and folded into Input.

**Cost is always nil / `telemetry.cost` stays false**; the Codex token path
carries **no cost field** (`TokenCountEvent = {info, rate_limits}`); cost is
never derived from a pricing table (that would be a fabricated number).

**Invariants.** INV-2 is structural: the rollout is decoded into projection
structs with **only numeric fields** (nested), so every content-bearing key
(prompt, agent message, shell command, apply_patch body, tool output, cwd, …)
has nowhere to land; proven by `TestFinops_NoContentOnWire` (sentinel content in
a fixture rollout, asserted absent from the real signed wire body with
content-capture ON). INV-3: bounded read (`maxRolloutBytes`, 64 MiB; oversized
skipped whole), fail-open (missing/null/malformed/partial rollout → logged to
stderr, skipped; never fails the flush, blocks, or writes stdout). Finops-off is
byte-identical to the pre-the Codex adapter's usage leg path.

> **Fallback:** when `transcript_path` is absent/null the read
> is skipped fail-open; the adapter does **not** reconstruct a
> `~/.codex/sessions/…` path from `session_id`. A real SessionEnd always carries
> `transcript_path` (`session_end.rs` @ `rust-v0.145.0`), and a HOME-derived scan
> would fight the read-only / hermeticity posture.

## Known limitations (honest, no silent caps)

- **No per-turn cost.** Rollout token counts are extracted (opt-in, above), but
  Codex records **no cost/price** in the token path, so `client.Cost` is always
  nil and `telemetry.cost` stays false.
- **At-most-once delivery**; same contract and caveats as the CC adapter.

## Enforce leg

**Unconditional.** `openbox init --provider codex` leaves the posture
enforcing without writing a key for it: a bool that defaults to true cannot
express "said nothing", so an absent key resolves to on. `enforce`/
`OPENBOX_ENFORCE` still parse -- so `openbox doctor` can name them as ignored
-- but neither turns enforcement off any more; `ResolveEnforce()` always
reports `true` (`TestResolveEnforce_Codex`), and there is no observe path left
for a gated hook to fall back to (`TestObserveByteParity_EnforceOff` is gone;
`TestObserveByteParity_NeverGatedHooks` covers only the hook classes that were
never gated in the first place). Enforcement gates **three** hook classes --
`PreToolUse`, `PermissionRequest`, `UserPromptSubmit` -- each pre-execution,
hard-bounded, single-attempt and unconditionally fail-closed (an owner
decision, superseding the earlier fail-open-by-default / INV-3b framing: an
outage now denies the call and, once an attempt is actually sent and
explicitly refused, halts the run). Exit code is always 0; we speak Codex's
output JSON, never the exit-2 block signal.

The cascade is the shipped Claude Code E6 stack (`decision/` consumed unchanged,
an in-process decider; no socket, no daemon, microseconds, no network on the T1
path): **obtain** → **apply** onto Codex's PreToolUse contract (delivery is
unconditionally fail-closed now; there is no failure-policy choice left to
make), plus inline `/evaluate` evaluation of every gated call and the findings
loop. Only the two provider edges differ from CC; the middle is shared.

### Codex-shaped deltas (each grounded @ `rust-v0.145.0` + the binary output schemas, recorded in the enforce-leg probes)

- **PermissionDecision literals.** Codex's PreToolUse enum is `allow|deny|ask`
  (`schema.rs`), but the runtime output parser **rejects** `ask`, a bare `allow`
  (without `updatedInput`), and `updatedInput` without `allow`
  (`output_parser.rs`). A rejected output is discarded and the tool **proceeds**
  (probe **P1**: a failed/timed-out PreToolUse hook fails **open**). So the only
  usable levers are **`deny` + reason** (block) and **`allow` + `updatedInput`**
  (redact-and-proceed).
- **REQUIRE_APPROVAL → `deny`** (ruled **an owner decision**). CC maps it to `ask`;
  Codex rejects `ask`, and a no-decision fallthrough under
  `approval_policy=never` **auto-runs** the tool ungoverned (probe **P3**,
  live). No approval-policy mode could be *proven* to surface a native prompt
  within the harness (`codex exec` is non-interactive), so per the ruling
  **every** REQUIRE_APPROVAL quadrant emits a content-free DENY; strictly
  tighter, never a silent proceed. *(E9 narrows when that fires. A high-risk
  REQUIRE_APPROVAL now escalates rather than being answered locally, so the deny
  is what an undecided approval degrades to after the bounded hold, not the
  first thing tried. Codex has no rewake primitive, so a decision that lands
  later reaches the session through the advisories/findings channel rather than
  a system reminder.)*
- **Redaction content field is `"command"`** (delta from CC's `content`/
  `new_string`). `apply_patch`'s PreToolUse `tool_input` is `{"command":<raw
  patch text>}` and `updatedInput` is re-parsed via `updated_hook_command` →
  `updated_input["command"]` (core `ApplyPatchHandler` + `handlers/mod.rs`).
  Local secret detection scans the patch body and rewrites the `"command"` field
  only; every structural field is carried over verbatim.
- **Tighten-only preserved.**
  `permissionDecision:"allow"` is emitted **only** bundled with a non-empty
  redacting `updatedInput`; a bare allow is structurally impossible here. Codex
  resolves competing hooks by "any deny wins" and offers no approval-bypass hook
  lever (`PreToolUseHookResult` is `Continue{updated_input}` | `Blocked`), so
  allow+updatedInput = "proceed via Codex's own approval/sandbox flow, with
  redacted input"; never a grant. *(Flagged for G3/G_SEC ratification; it
  departs from the CC-derived "never emit allow" wording, which assumed CC's
  updatedInput-alone contract.)*
- **Timeout clamps derived from the installed hook timeout**, **not** copied from CC's 2 s/5 s constants. Probe **P1**
  proved Codex kills a PreToolUse hook at its configured `timeout` and **fails
  open**, so our verdict must land first. The whole-hook budget is
  `installedGateHookTimeout` (the installer's `preToolUseHookTimeoutSec`)
  **minus a margin**; if an org raises the installed timeout, the clamps scale
  with it. The default budgets stay conservative (T1 ≤ 2 s, T2 ≤ the CC value)
  per the ruling; only the E9 approval hold spends the extra headroom, and only
  for a request core actually filed.

### Findings channel (an owner decision, resolved by probe **P2**)

`additionalContext` (→ model) + `systemMessage` (→ user) on UserPromptSubmit +
PostToolUse; **full CC parity, not the degraded systemMessage-only mode**. The
binary output schemas define `hookSpecificOutput.additionalContext` on
PreToolUse/PostToolUse/SessionStart/UserPromptSubmit; `discovery.rs` confirms
these events "can emit additionalContext" while others warn they cannot. The
summary is content-free (categories/counts/booleans), never a decision field.

### Enforcement conformance suite

`enforce_conformance_test.go` drives the real `RunHook` PreToolUse path
end-to-end (`CDX-C1..CDX-C12`) covering every quadrant, including the
degraded-state cases (lesson-e6e7-04): reachable-but-unbundled under fail-closed
(CDX-C6), the stale-policy gate (`TestEnforcementConformance_StaleGate_Codex`),
and the probed hook-timeout fail-open bound (CDX-C8). The cross-adapter parity
matrix (`internal/adapters/claude-code/conformance_parity_test.go`,
`TestCrossAdapterParityMatrix_SL7B`) records that CC and Codex assert the same
invariant set and where Codex's contract forces a documented delta.

### Bypass vectors (documented, unmitigated without managed distribution)

`--disable hooks` and `--dangerously-bypass-hook-trust` let a user run without
the OpenBox hook; the same opt-in-pilot posture as CC. Only requirements.toml-
managed hooks (`allow_managed_hooks_only`, `managed_dir`) are non-disablable;
that enterprise-mandate story and the Codex plugin channel are the recorded
hardening options. `PermissionRequest`-event integration (a second
decision surface) is a deferred follow-up.

**`allow` non-bypass; source-confirmed @ rust-v0.145.0 (Sam G_SEC F1, closed).**
A hook's `permissionDecision:"allow"` on the redaction path merely *continues*
the call through Codex's own approval/sandbox flow; it never grants approval and
never overrides another hook's `deny`. Confirmed at the tag in
`output_parser.rs` (`PreToolUseHookResult = Continue{updated_input} | Blocked`;
`should_block = should_block && invalid_reason.is_none`) and
`pre_tool_use.rs::run` (any-deny-wins aggregation; a blocked result zeroes
`updated_input`), and independently by the an owner decision surface review of the
Codex source (2026-07-24): Codex approval is resolved solely by the local actor
via `Op::ExecApproval`/`PatchApproval`, and a hook `allow` is not an approval
verb. So no live interactive probe is required to close this; the earlier
"harness-unproven" caveat is retired. **Blast radius is still bounded in code to
`apply_patch` writes only:** `buildDecisionRequest` populates `Content` (hence
any `RedactedContent`, hence any `allow`) only when `isFileSemantic(sem)`; Bash
and `mcp__*` are non-file, carry no `Content`, and can only ever receive `deny`
or a silent proceed; they are **never** auto-allowed.

### Audit + privacy

`enforcements.jsonl` records verdict / ids / flags / guardrail **categories**
only; never the command, patch body, or reason free text (INV-1/INV-2). Deny
reasons carry the policy-authored text + policy id (local, stdout → Codex, never
egressed). Redacted content rides only the **local** decision path; the observe
Mapper egress path is untouched (it carries content whenever the content posture
is on, which is the default).

The `CODEX_THREAD_ID` inherited-env edge: a process launched from
within a Codex exec inherits `CODEX_THREAD_ID`, so its commits attribute to the
Codex thread; arguably transitively correct, rare, and the trailer sink still
validates the value; noted for a future explicit-override escape hatch.

## Test / validate

```bash
go test -race ./internal/adapters/codex/...
# plus the cli-level routing + real-binary observe E2E:
go test ./cmd/openbox -run Codex
```
