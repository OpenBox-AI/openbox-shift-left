# STORY-E6-S1 — Synchronous evaluate path (enforce mode)

**Epic:** E6 (enforcement — the `apply` leg). **Risk:** high (this is the first path that puts a **synchronous, pre-execution** wait on a Claude Code tool call — the INV-3b carve-out. A wrong timeout or a non-fail-open fault would hang or silently mis-gate the dev loop). **Status target:** review (build + validations + both reviews, pending brian G3 + Sam G_SEC).

## Source
- **Backlog:** `.fab7/sdlc/stories/E6-backlog.md` §E6-S1 — "a new **enforce mode** where `PreToolUse` obtains a decision *before* the tool runs — via the local sidecar (OD6) — and returns the `client.Evaluation`. Reuses SL-9's `Evaluation`; SL-15's fail-closed error handling is the model. The current async observe spool path is untouched for observe/advisory sessions." Write scope: `adapters/claude-code/`, likely a new sidecar client. Deps: E6-S0, SL-9, **E6-S5 (DONE)**. Gates: G3, G_SEC. Invariants: INV-3b, INV-1.
- **E6-S5 (DONE 2026-07-13, committed 0d4c30e):** the `sidecar/` module ships the exact primitive this story imports — `sidecar.Client` (dials the per-user Unix socket, hard ~50 ms budget, **fail-open ALLOW on any fault**), `sidecar.DecisionRequest`/`DecisionResponse` wire types, and `sidecar.Decision{Evaluation, FailOpen, Source, Stale}`. This story does **not** build a new sidecar client — it *uses* `sidecar.Client`.
- **Spike S2 (DONE):** direct sync `POST /evaluate` = ~0.8–1.6 s → NO-GO; the local sidecar is the only viable decision path; hook budget ≈ **50 ms** (`sidecar.DefaultDecisionTimeout`), fail-open (OD9).
- **ADR-0002 (accepted):** INV-3b — an enforce path MAY block, but only at `PreToolUse`, only within the hard ~50 ms per-call timeout, **fail-open by default**.
- **Cross-repo recon (openbox-temporal-sdk-python, 2026-07-14):** the SDK's pre-execution gate awaits `GovernanceClient.evaluate_event` (POST `/evaluate`) on `ActivityStarted`, *then* runs `enforce_verdict` **before** the activity executes (`activity_interceptor.py:227–256`). The fail-open/closed knob is `on_api_error` (default `"fail_open"` → `None` verdict → proceed; `client.py:204–213`). **This story is the analog of the *obtain-the-verdict* half only** — the `enforce_verdict` cascade (HALT>BLOCK>guardrails>REQUIRE_APPROVAL>CONSTRAIN>ALLOW → CC `permissionDecision`) is **E6-S2's `apply`**, not this story.

## Scope boundary (why this is a distinct slice from E6-S2)
- **E6-S1 (this story):** in enforce mode, `PreToolUse` **synchronously obtains** a `client.Evaluation` from the local sidecar before the tool runs, records it (durable enforcement-decision record + terse stderr line), and returns it through a seam. It **does not yet block** — nothing is written to stdout; the tool still proceeds (fail-open). This proves the synchronous path, the 50 ms bound, and fail-open, with zero risk of a wrong deny.
- **E6-S2 (next):** maps `Evaluation.Verdict` → CC `permissionDecision` (`deny`/`ask`/`allow`) on stdout — the moment `WouldBlock()` becomes a real block. It consumes the `sidecar.Decision` this story surfaces.
- Keeping the *teeth* out of E6-S1 is deliberate: the observe→enforce **flip** (config flag) and the sync-gate plumbing land and are validated here **without** ever being able to deny a call. INV-3 (exit 0, nothing to stdout) still holds verbatim for E6-S1 in every mode.

## Inlined context (verified — builder need not re-read)
- **The hook engine** is `claudecode.RunHook` (`adapters/claude-code/hookrun.go`), the single observe engine shared by `openbox hook claude-code <event>` and the retired `openbox-cc-hook` alias. Its safety contract: writes NOTHING to stdout, swallows every failure, caller exits 0. The enforce branch is **additive** and gated — when enforce is off the function is byte-identical.
- **Config-flag pattern** (`adapters/claude-code/creds.go`): `ResolveFinops()` / `ResolveInstallGitHook()` read a `DevConfig` field then an env override via `isTruthy`, default false, fail-safe on a missing/unreadable config. Mirror this exactly for `ResolveEnforce()` (`DevConfig.Enforce` + `OPENBOX_ENFORCE`).
- **Tool classification** already exists (`mapper.go`): `classifyTool(name)` → `(kind, sem, fileOp, mcpServer, function)`; `HookEvent.filePath()` → structural file path. Reuse both to build the `DecisionRequest` — do not re-classify.
- **DecisionRequest shape** (`sidecar/protocol.go`) mirrors core's `buildOPAInput` axes: `SessionID` (required — the decision subject), `DeveloperDID`, `EventType`, `Tool{Name,Kind,MCPServer}`, and `Attributes` (the metadata axes a local policy matches on — e.g. `command`, `file_path`, `file_operation`, `permission_mode`). `Content` is the GATED field (E6-S4); this story leaves it nil.
- **INV-2 and the command string:** the `DecisionRequest` is sent **only over the local Unix socket** to the resident daemon and is evaluated **locally** — there is no egress on this path (the sidecar mirror-to-`/evaluate` telemetry is not implemented, and even when it is, content stays gated). The sidecar protocol therefore classifies the shell `command` and `file_path` as **matchable metadata axes** (see `protocol.go` / `socketpath.go:62`), and the SDK analog sends the full `activity_input` to its gate. So the enforce request MAY carry the shell command for local matching (the canonical `rm -rf` rule) — extracted by a dedicated local-only method, documented as **never egressed**. The Mapper's egress path is unchanged and still never decodes the command (INV-2 for the observe/telemetry path is untouched).
- **`sidecar.Client`** (`sidecar/client.go`): `NewClient(ClientConfig{SocketPath, Timeout})` never fails; `Decide(ctx, req) Decision` never errors — every fault (socket absent, dial refused, timeout, malformed reply) yields `Decision{FailOpen:true, Evaluation:{Verdict:VerdictUnknown}}`. Socket path: `OPENBOX_SIDECAR_SOCKET` env (the same env `openbox sidecar serve` reads) else `sidecar.DefaultSocketPath()`.

## Acceptance Criteria
1. **Enforce-mode flag** — `ResolveEnforce()` (config `DevConfig.Enforce` + `OPENBOX_ENFORCE` override, default **false = observe**), mirroring `ResolveFinops`. Fail-safe: a missing/unreadable config is `false`.
2. **Synchronous pre-execution gate** — when enforce is on AND the hook is `PreToolUse`, `RunHook` builds a `sidecar.DecisionRequest` from the hook payload (reusing `classifyTool`/`filePath`) and calls `sidecar.Client.Decide` **before returning**, obtaining the `client.Evaluation`. The call is hard-bounded by the sidecar Client's ~50 ms timeout (INV-3b) — the worst-case latency added to a tool call is that bound, then proceed.
3. **Fail-open, always** — on sidecar-absent / dial-refused / timeout / malformed reply, `Decide` returns a fail-open ALLOW (`VerdictUnknown`) and the tool proceeds. E6-S1 **never** writes a blocking signal (no stdout, exit 0) in **any** mode — the actual deny/ask is E6-S2. A test proves: enforce on + sidecar absent → decision obtained (FailOpen) → nothing to stdout → allow within the bound.
4. **Observe/advisory path untouched** — with enforce **off** (the default), `RunHook` is byte-for-byte the current observe path: the sidecar is **never dialed**, no new file/env is read on the hot path beyond the one cheap `ResolveEnforce` gate, and Pre/PostToolUse/SessionStart/… behave identically (INV-3 verbatim). A test asserts the sidecar is not contacted when enforce is off.
5. **Decision surfaced + recorded** — the obtained `sidecar.Decision` is returned through a seam E6-S2 will consume, and recorded off the safety-critical path: one terse, secret-free stderr line (verdict / source / fail_open / stale). Recording is best-effort and never blocks (INV-3). **A DURABLE enforcement-decision record is deferred to E6-S2** (where the decision is actually applied): it naturally belongs with the apply, and keeping E6-S1 to the stderr diagnostic avoids conflating a new sink with SL-9's Advisory-on-flush sink and adds no extra content-egress surface (a G_SEC-favorable posture). This resolves the earlier AC-5-vs-write-scope contradiction (G3 F1): E6-S1's record IS the stderr diagnostic; the durable record is E6-S2's.
6. **Only PreToolUse gates** — non-`PreToolUse` hooks never dial the sidecar even in enforce mode (the pre-execution gate is a `PreToolUse` concept). PostToolUse/SessionEnd/etc. keep observing exactly as today.
7. **Wiring** — `adapters/claude-code/go.mod` gains `require` + `replace → ../../sidecar`; no second binary, no change to the plugin's `hooks.json` (the same `openbox hook claude-code PreToolUse` invocation now honors enforce mode when the flag is set).

## Nonfunctional Requirements
- **security (G_SEC required):** the decision path takes **no network I/O** (INV-3b — it dials only the local per-user socket); no secret is read on the PreToolUse hot path (identity DID only, as today — INV-1); the command/file metadata in the `DecisionRequest` never egresses and is never logged (INV-2); the enforcement-decision record and stderr line carry categories/ids/verdict only, never tool content.
- **reliability/performance (NFR-2 / INV-3b):** worst-case added latency is the sidecar Client's hard timeout; a dead/slow/absent sidecar degrades to observe (allow), never hangs; a panic in the enforce branch is recovered and fails open.

## Write Scope
- **NEW** `adapters/claude-code/enforce.go` — `ResolveEnforce`, the `DecisionRequest` builder (reusing mapper classification), the local-only command/attribute extraction, and the `PreToolUse` enforce-gate function returning `sidecar.Decision`.
- **NEW** `adapters/claude-code/enforce_test.go`.
- `adapters/claude-code/hookrun.go` — additive, gated enforce branch on `PreToolUse`.
- `adapters/claude-code/creds.go` — `DevConfig.Enforce` field + `OPENBOX_ENFORCE` env constant.
- `adapters/claude-code/hookevent.go` — a local-only `command()` accessor (never egressed) if the request carries the shell command.
- `adapters/claude-code/go.mod` — `require`/`replace` on `sidecar/`.
- (Optional) a small enforcement-decision sink or reuse of the Advisory sink for the durable record.

## Dependencies
- **Hard:** E6-S5 (`sidecar/`, DONE), SL-9 (`client.Evaluation`), SL-3 (`client`). ADR-0002 + ADR-0003 accepted.
- **Feeds:** E6-S2 (`apply` consumes the `sidecar.Decision`), E6-S3 (fail-closed policy layers on the fail-open Client), E6-S7 (conformance asserts the sync gate + fail-open + observe-untouched).

## Invariants
- **INV-3b:** the decision is synchronous, pre-execution, bounded by the hard timeout, fail-open by default; NO network on the decision path.
- **INV-3 (verbatim for E6-S1):** still exit 0, still nothing to stdout — E6-S1 obtains + records but does not apply.
- **INV-1:** no secret on the hot path / in the record / on stderr. **INV-2:** command/file metadata stays local, never egressed, never logged as content.

## Human Gates
| Gate | Question | Owner | Evidence | Outcomes |
|---|---|---|---|---|
| G3_REVIEW | Does enforce mode synchronously obtain the verdict before the tool runs, fail open within the bound, and leave the observe path byte-unchanged (and NOT yet block — that's E6-S2)? | brian | diff review + enforce/fail-open/observe-untouched tests + `openbox hook` smoke with sidecar up/down | approve / revise |
| G_SEC | Is the decision path network-free, the hot path secret-free, and the command/file metadata local-only (never egressed/logged as content)? | Sam (security reviewer) | review of the request build + Decide call + record/stderr surfaces | approve / revise / block |

## Validation
```bash
cd adapters/claude-code && go build ./... && go vet ./... && go test -race ./...
cd ../../sidecar && go build ./... && go test -race ./...
cd ../cli && go build ./... && go vet ./... && go test ./...
# enforce on, sidecar absent -> allow (fail-open), nothing to stdout:
OPENBOX_ENFORCE=1 openbox hook claude-code PreToolUse < pretooluse.json   # exits 0, no stdout
# enforce on, sidecar up -> decision obtained from the local socket (stderr diagnostic shows verdict/source)
```

## Stop conditions
- If obtaining the decision would take a network round-trip (core `/evaluate`, backend, Guardrail API) → STOP (INV-3b breach — that is the ~1.6 s wall S2 rejected). Only the local `sidecar.Client` socket dial is allowed.
- If E6-S1 writes ANY blocking signal (stdout permissionDecision, non-zero exit) → STOP: the *apply* (turning `WouldBlock()` into a real deny/ask) is **E6-S2**, deliberately not this story. E6-S1 obtains + records only.
- If enforce-off would change the observe path in any observable way → STOP (AC-4 / INV-3).
