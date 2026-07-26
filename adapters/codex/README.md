# OpenBox Codex adapter (STORY-SL7-A observe + STORY-SL7-B enforce)

The second realization of the generic Provider Adapter Contract (architecture
§1b), a 1:1 structural port of the Claude Code adapter: it maps Codex CLI's
native hooks onto the normalized developer event contract (STORY-SL-1) and
emits them through the shared AIP-signed transport (STORY-SL-3) on the E7 flat
hook wire. **Observe-only, fail-open** — this leg can never block, deny, or
slow a Codex tool call (INV-3; the enforce leg is STORY-SL7-B).

**Version pin: codex-cli >= 0.145.0** — hooks are stable and ON by default
(no feature flag to flip), `tool_use_id` exists on Pre/PostToolUse, the
`SessionEnd` hook exists, and `CODEX_THREAD_ID` is injected into every exec
env. Surface truth: spike S5's 2026-07-23 addendum, `codex-rs` @ tag
`rust-v0.145.0`, and the hook payload JSON Schemas embedded in the 0.145.0
binary (`<event>.command.input`).

```
Codex hook (stdin JSON)
   └─ openbox hook codex <event>          # hooks.json wires each event here
        ├─ map → normalized SL-1 DevEvent # mapper.go (no tool content — SL3-SEC-3)
        ├─ append → local spool           # spool.go (hot path: local I/O only)
        └─ exit 0, empty stdout           # Codex parses hook stdout as output JSON — we emit none
   SessionEnd / `openbox hook codex flush`
        └─ drain spool → client.Emit → POST /api/v1/governance/evaluate  (off the hot path)
```

The spool/flush/recovery design (why not emit inline, at-most-once +
undelivered-remainder recovery, deterministic `event_id` — INV-5) is identical
to the CC adapter; see `adapters/claude-code/README.md`. Codex-specific: the
spool lives under `…/openbox/codex-spool` so a machine running both tools never
cross-drains, and the deterministic ids are namespaced `cdx-`.

## Event mapping (SL-1 contract)

| Codex hook | SL-1 `event_type` | Span (`semantic_type`) |
|---|---|---|
| `SessionStart` | `SessionStarted` | — |
| `UserPromptSubmit` | `PromptSubmitted` | — |
| `PreToolUse` | `ToolCall` | by tool (`stage=started`) |
| `PostToolUse` | `ToolResult` | by tool (`stage=completed`) |
| `SessionEnd` | `SessionEnded` | — |

The other six 0.145.0 events (`PermissionRequest`, `Subagent*`, `*Compact`,
`Stop`) are deliberate non-goals. Codex **session ≡ thread**: `session_id` is
the OpenBox session id.

Tool classification (`classifyTool`), grounded in `codex-rs`
`core/src/tools/hook_names.rs` + `registry.rs` @ rust-v0.145.0 and pinned by
`TestClassifyTool_GroundedLiterals`:

- `"Bash"` (yes — Codex serializes the Claude-compatible literal for its
  shell/unified-exec paths) → `shell`/`internal`
- `"apply_patch"` → `file`/`file_write` (its `Write`/`Edit` matcher aliases are
  never serialized as `tool_name`); no `file_path` — Codex's tool_input is the
  patch body (content), which we never decode
- `mcp__<server>__<tool>` → `mcp`/`mcp_tool_call`
- everything else (`web_search`, `update_plan`, `view_image`, `spawn_agent`, …)
  → the coarse `shell`/`internal` catch-all; the real name rides `tool.name` +
  `metadata.tool_name`

## tool_use_id pairing (the improvement over Claude Code)

Codex gives every tool invocation a `tool_use_id` shared by its Pre/Post hooks.
The mapper threads it through the client's deterministic id derivation (the
`span.function` pairing slot for non-MCP tools — wire-neutral, verified by
`TestWire_ToolUseIDNeverRidesTheWire`), so:

- a call's started+completed spans share **exact** `span_id`/`activity_id`
  (two identical sequential Bash calls no longer collide — the CC adapter's
  documented limitation);
- the E7-S8 duration stash is keyed per invocation (concurrent same-tool calls
  can't swap start times);
- `event_id` (INV-5) is per-invocation distinct.

MCP tools keep `span.function` = the real MCP function (it IS wire data) and
fall back to the CC-parity derivation; `tool_use_id`/`turn_id` ride tool-event
`metadata` for audit either way.

## Privacy (SL3-SEC-3 / INV-2)

**Content capture is ON by default (2026-07-15)** — the developer's **prompt**
is the only gated field: carried (capped) on `PromptSubmitted` unless the org
opts out (`content_capture:false` / `OPENBOX_CONTENT_CAPTURE=0`).
Unconditionally: shell command strings, `apply_patch` bodies, and tool output
**never** ride an observe event — `tool_input` is kept as an opaque blob the
observe path never decodes, and `tool_response` is not even bound to a field.
Asserted by `TestMap_NoContentLeak`, `TestWire_NoContentLeakEndToEnd`, and the
cli real-binary E2E (`TestCodexUnifiedBinaryObserveE2E`).

## Commit attribution (SL-5 parity, simpler)

Codex injects `CODEX_THREAD_ID` into **every** tool/shell exec environment, so
the shared prepare-commit-msg hook (`adapters/common/git`) stamps
`OpenBox-Session:` directly from the env — highest precedence, **no liveness
registry** (the CC mechanism stays untouched and CC sessions never set the
var). Ambient hook install on SessionStart is gated by
`dev init --install-git-hook`, exactly like CC. Commits typed in the user's own
terminal are OD-SL7-USERTERM (deferred).

## Credentials & config (INV-1)

Identity comes from the same `openbox dev init` flow and the same
`~/.config/openbox/dev.json` + OS/file secret store as every provider, via the
shared `adapters/common/devconfig` module (OD-SL7-SHARE ruling (a), ADR-0007).
The hook reads the **DID only** on the hot path; the obx_ key + Ed25519 seed
are read only at flush, straight into the client. `hooks.json` carries the
engine path + event names only — no key, DID, or URL.

## Packaging & install

`openbox dev init --provider codex` writes OpenBox-owned entries for the five
events into `$CODEX_HOME/hooks.json` (default `~/.codex/hooks.json`), each
`{"type":"command","command":"\"<engine>\" hook codex <Event>","timeout":5}`
(15 s for SessionEnd; Codex timeouts are in seconds), with matcher `"*"` on the
tool hooks. The engine path is this running `openbox` binary
(`os.Executable()` via the CLI registry — the CC `EngineBinary` precedent; no
bundle, no binary copy). The merge is **idempotent and ownership-aware**:
re-install updates entries whose command invokes `… hook codex …` (or a
mangled `… hook claude-code …` import — Codex can migrate Claude Code configs)
in place, never duplicates, and never touches foreign entries; an unparsable
pre-existing file is refused, not clobbered.

**Trust step:** Codex hash-trusts non-managed hooks — after (re-)install, run
`/hooks` inside Codex to trust the OpenBox entries or they will not run.
`--disable hooks` and `--dangerously-bypass-hook-trust` remain user-side
bypass vectors — acceptable for the observe posture (NFR-5 parity with CC's
opt-in pilot); requirements.toml-managed hooks and the Codex plugin channel are
the recorded hardening/distribution options (OD-SL7-DIST).

## Known limitations (honest, no silent caps)

- **No tokens/cost.** Codex hooks expose no usage. The rollout transcript
  (flushed before the SessionEnd hook runs) carries `token_count` — extraction
  is the noted SL-16-parity follow-up. `Capabilities()` →
  `telemetry.tokens=false`.
- **At-most-once delivery** — same contract and caveats as the CC adapter.

## Enforce leg (STORY-SL7-B)

**Opt-in, default observe.** Enable at onboarding — `openbox dev init --provider
codex --enforce` persists `enforce`/`tier2`/`findings` to `dev.json` (ADR-0006;
no runtime env needed). With enforce **off** the SL7-A observe path is
**byte-identical** — the decider is never invoked (asserted:
`TestObserveByteParity_EnforceOff`). Enforcement gates **only** the PreToolUse
hook, pre-execution, hard-bounded, fail-open by default (OD9 / INV-3b). Exit code
is always 0 — we speak Codex's output JSON, never the exit-2 block signal.

The cascade is the shipped Claude Code E6 stack (`decision/` consumed unchanged,
ADR-0006 in-process decider — no socket, no daemon, microseconds, no network on
the T1 path): **obtain** → **failure policy** (fail-open default / opt-in
fail-closed on outage only) → **apply** onto Codex's PreToolUse contract, plus
Tier-2 sync `/evaluate` escalation for high-risk classes (Bash / `mcp__*`) and the
Tier-3 findings loop. Only the two provider EDGES differ from CC; the middle is
shared.

### Codex-shaped deltas (each grounded @ `rust-v0.145.0` + the binary output schemas, recorded in the SL7-B probes)

- **permissionDecision literals.** Codex's PreToolUse enum is `allow|deny|ask`
  (`schema.rs`), but the runtime output parser **rejects** `ask`, a bare `allow`
  (without `updatedInput`), and `updatedInput` without `allow`
  (`output_parser.rs`). A rejected output is discarded and the tool **proceeds**
  (probe **P1**: a failed/timed-out PreToolUse hook fails **open**). So the only
  usable levers are **`deny` + reason** (block) and **`allow` + `updatedInput`**
  (redact-and-proceed).
- **REQUIRE_APPROVAL → `deny`** (ruled **OD-SL7-ASK**). CC maps it to `ask`;
  Codex rejects `ask`, and a no-decision fallthrough under `approval_policy=never`
  **auto-runs** the tool ungoverned (probe **P3**, live). No approval-policy mode
  could be *proven* to surface a native prompt within the harness (`codex exec` is
  non-interactive), so per the ruling **every** REQUIRE_APPROVAL quadrant emits a
  content-free DENY — strictly tighter, never a silent proceed.
- **Redaction content field is `"command"`** (delta from CC's `content`/
  `new_string`). `apply_patch`'s PreToolUse `tool_input` is
  `{"command":<raw patch text>}` and `updatedInput` is re-parsed via
  `updated_hook_command` → `updated_input["command"]` (core `ApplyPatchHandler` +
  `handlers/mod.rs`). Tier-1 secret detection scans the patch body and rewrites the
  `"command"` field only; every structural field is carried over verbatim.
- **Tighten-only preserved (OD-SL7-ALLOW-REWRITE).** `permissionDecision:"allow"`
  is emitted **only** bundled with a non-empty redacting `updatedInput`; a bare
  allow is structurally impossible here. Codex resolves competing hooks by
  "any deny wins" and offers no approval-bypass hook lever
  (`PreToolUseHookResult` is `Continue{updated_input}` | `Blocked`), so
  allow+updatedInput = "proceed via Codex's own approval/sandbox flow, with
  redacted input" — never a grant. *(Flagged for G3/G_SEC ratification — it
  departs from the CC-derived "never emit allow" wording, which assumed CC's
  updatedInput-alone contract.)*
- **Timeout clamps derived from the installed hook timeout** (OD-SL7-T2-TIMEOUT),
  **not** copied from CC's 2 s/5 s constants. Probe **P1** proved Codex kills a
  PreToolUse hook at its configured `timeout` and **fails open**, so our verdict
  must land first. The whole-hook budget is `installedHotHookTimeout` (SL7-A's
  `hotHookTimeoutSec`) **minus a margin**; if an org raises the installed timeout,
  the clamps scale with it. Codex's 600 s default is available headroom, but the
  default budgets stay conservative (T1 ≤ 2 s, T2 ≤ the CC value) per the ruling.

### Tier-3 findings channel (OD-SL7-FINDINGS, resolved by probe **P2**)

`additionalContext` (→ model) + `systemMessage` (→ user) on UserPromptSubmit +
PostToolUse — **full CC parity, not the degraded systemMessage-only mode**. The
binary output schemas define `hookSpecificOutput.additionalContext` on
PreToolUse/PostToolUse/SessionStart/UserPromptSubmit; `discovery.rs` confirms
these events "can emit additionalContext" while others warn they cannot. The
summary is content-free (categories/counts/booleans), never a decision field.

### Enforcement conformance suite

`enforce_conformance_test.go` drives the real `RunHook` PreToolUse path
end-to-end (`CDX-C1..CDX-C12`) covering every quadrant, including the
degraded-state cases (LESSON-E6E7-04): reachable-but-unbundled under fail-closed
(CDX-C6), the stale-policy gate (`TestEnforcementConformance_StaleGate_Codex`),
and the probed hook-timeout fail-open bound (CDX-C8). The cross-adapter parity
matrix (`adapters/claude-code/conformance_parity_test.go`,
`TestCrossAdapterParityMatrix_SL7B`) records that CC and Codex assert the same
invariant set and where Codex's contract forces a documented delta.

### Bypass vectors (documented, unmitigated without managed distribution)

`--disable hooks` and `--dangerously-bypass-hook-trust` let a user run without
the OpenBox hook — the same opt-in-pilot posture as CC. Only requirements.toml-
managed hooks (`allow_managed_hooks_only`, `managed_dir`) are non-disablable; that
enterprise-mandate story and the Codex plugin channel are the recorded hardening
options (OD-SL7-DIST). `PermissionRequest`-event integration (a second decision
surface) is a deferred follow-up.

**`allow` non-bypass — SOURCE-CONFIRMED @ rust-v0.145.0 (Sam G_SEC F1, closed).**
A hook's `permissionDecision:"allow"` on the redaction path merely *continues* the
call through Codex's own approval/sandbox flow — it never grants approval and never
overrides another hook's `deny`. Confirmed at the tag in `output_parser.rs`
(`PreToolUseHookResult = Continue{updated_input} | Blocked`; `should_block = should_block
&& invalid_reason.is_none()`) and `pre_tool_use.rs::run` (any-deny-wins aggregation;
a blocked result zeroes `updated_input`), and independently by the OD-SL7-ASK spike
(`spike-SL7-ask-20260724`, `.fab7/sdlc/discovery/spikes/S5-codex-surfaces.md` →
"2026-07-24 OD-SL7-ASK resolution"): Codex approval is resolved solely by the local
actor via `Op::ExecApproval`/`PatchApproval`, and a hook `allow` is not an approval
verb. So no live interactive probe is required to close this — the earlier
"harness-unproven" caveat is retired. **Blast radius is still bounded in code to
`apply_patch` writes only:** `buildDecisionRequest` populates `Content` (hence any
`RedactedContent`, hence any `allow`) only when `isFileSemantic(sem)` — Bash and
`mcp__*` are non-file, carry no `Content`, and can only ever receive `deny` or a
silent proceed; they are **never** auto-allowed.

### Audit + privacy

`enforcements.jsonl` records verdict / ids / flags / guardrail **categories**
only — never the command, patch body, or reason free text (INV-1/INV-2). Deny
reasons carry the policy-authored text + policy id (LOCAL, stdout → Codex, never
egressed). Redacted content rides only the **local** decision path; the observe
Mapper egress path is untouched (metadata-only unless content capture is on). The
`CODEX_THREAD_ID` inherited-env edge (SL7-A G3 F-3): a process launched from
within a Codex exec inherits `CODEX_THREAD_ID`, so its commits attribute to the
Codex thread — arguably transitively correct, rare, and the trailer sink still
validates the value; noted for a future explicit-override escape hatch.

## Test / validate

```bash
cd adapters/codex && go build ./... && go vet ./... && go test -race ./...
# plus the cli-level routing + real-binary observe E2E:
cd ../../cli && go test ./cmd/openbox -run Codex
```
