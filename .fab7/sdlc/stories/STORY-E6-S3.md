# STORY-E6-S3 — Fail-closed policy engine (per-org override + timeout)

**Epic:** E6 (enforcement — the failure policy). **Risk:** high (this is the ONLY path that turns an OpenBox *outage* into a blocked developer — a fail-closed org that opts in trades availability for a hard guarantee; a bug here either blocks work when it shouldn't, or fails to block when the org demanded it). **Status target:** review (build + validations + both reviews, pending brian G3 + Sam G_SEC).

## Source
- **Backlog:** `.fab7/sdlc/stories/E6-backlog.md` §E6-S3 — "the failure policy — **fail-open default** (OD9); per-org opt-in **fail-closed**; the hard per-call **timeout** (from S2). On timeout/unavailable → the policy decides (open=allow, closed=deny). Mirrors the SDK's `governance_policy`." Write scope: `adapters/claude-code/` (+ config). Deps: E6-S1, S2. Gates: G3, **G_SEC** (fail-closed is a distinct risk profile — an outage blocks work). Invariants: INV-3b.
- **Decision baked in (OD9, brian 2026-07-13):** fail-open is the DEFAULT. Per-org fail-closed is an explicit opt-in, off unless configured.
- **E6-S1 (DONE, committed 268fbfb):** the enforce gate OBTAINS a `sidecar.Decision` synchronously (bounded, fail-open) — `Decision.FailOpen==true` marks "the sidecar could not deliver a real verdict" (socket absent, dial refused, timeout, malformed reply). The fail-**open** primitive lives in `sidecar.Client` (`allowFailOpen` → `VerdictUnknown`, `FailOpen=true`).
- **E6-S2 (DONE, committed ccb7e93):** `mapVerdict` ports the SDK `enforce_verdict` cascade; `applyDecision` writes the CC `permissionDecision`. Both are **verdict-agnostic** — they act on `Evaluation.Verdict`, and a `VerdictUnknown` (fail-open) emits nothing (proceed).
- **Cross-repo recon (openbox-temporal-sdk-python, 2026-07-14 — Explore):** the reference SDK has a **two-value** switch `governance_policy` = `fail_open` (default) / `fail_closed` — `openbox/hook_governance.py:30-31`, `openbox/config.py:91` (`on_api_error`). The decision is **centralized in one helper** `_handle_api_error` (`openbox/client.py:204-208`): on an evaluate failure it returns `None` (fail-open → no verdict → the interceptor's `if governance_verdict:` is falsy → **proceed**) OR a **synthesized `Verdict.HALT`** (fail-closed → the SAME `enforce_verdict` cascade runs → **block/terminate**). Crucially **`enforce_verdict` is policy-agnostic** — the fail-closed path reuses the identical enforcement code as a real Core verdict. SDK `api_timeout` = 30 s (`config.py:94`), a *remote* HTTP budget; our decision is LOCAL (S2: sidecar target <10 ms, ADR-0002 hook bound ~50 ms). No env var / per-org in the SDK — a single worker-level string.

## The port (mirror `_handle_api_error`, keep the cascade policy-agnostic)
E6-S3 is a single transform inserted between OBTAIN (E6-S1) and APPLY (E6-S2), the Go analog of the SDK's `_handle_api_error`:

```
dec := EnforceDecision(…)            // E6-S1: obtain (fail-open primitive; dec.FailOpen marks "no real verdict")
dec  = applyFailurePolicy(dec, pol)  // E6-S3: on dec.FailOpen && fail-closed → synthesize VerdictHalt; else untouched
applied := applyDecision(stdout, dec) // E6-S2: UNCHANGED, verdict-agnostic cascade → deny on the synthetic HALT
```

- **`mapVerdict`/`applyDecision` signatures are UNCHANGED** — exactly like the SDK's policy-agnostic `enforce_verdict`. The fail-closed deny travels the identical apply path as a real BLOCK/HALT.
- **fail-open (default, OD9):** a fail-open `Decision` is returned untouched → `VerdictUnknown` → `mapVerdict` emits nothing → **proceed** (byte-identical to E6-S2 / observe).
- **fail-closed (opt-in):** a fail-open `Decision` has its `Evaluation.Verdict` set to `VerdictHalt` (mirroring the SDK's synthetic HALT) with a content-free reason → `mapVerdict` → **deny**.
- **A REAL verdict (`dec.FailOpen==false`) is NEVER touched** under either policy — the failure policy governs ONLY the evaluation-unavailable case, never a real ALLOW/CONSTRAIN/BLOCK answer.

## The hard per-call timeout (from S2)
E6-S1 hardcoded `sidecar.DefaultDecisionTimeout` (~50 ms, ADR-0002). E6-S3 makes it a knob (`OPENBOX_ENFORCE_TIMEOUT_MS` / `enforce_timeout_ms`), default unchanged. It is **clamped to `maxEnforceTimeout` (2 s)** — this is a CORRECTNESS bound, not a nicety: Claude Code kills the PreToolUse hook at **5 s** (`plugin/hooks/hooks.json`), and a hook-kill lets the tool proceed (a CC-layer fail-**open**) — which would silently defeat a fail-CLOSED org. Clamping the whole enforce wait well under 5 s guarantees a fail-closed deny is actually *delivered*. INV-3b's "hard timeout" is preserved (never unbounded).

## Scope boundary (what this story is and is NOT)
- **IS:** the `FailurePolicy` type + `applyFailurePolicy` transform; `ResolveFailClosed` / `ResolveEnforceTimeout` config+env resolvers; the `enforce_timeout_ms` / `fail_closed` config fields + envs; threading both into the `PreToolUse` enforce branch; the fail-closed reason text; tests + a fail-closed E2E.
- **IS NOT:** the verdict cascade or the stdout writer (E6-S2, unchanged); guardrail redaction / `updatedInput` (E6-S4); the interactive HITL prompt (E6-S6); the conformance suite that finalizes ADR-0002 (E6-S7 — this story supplies the fail-closed *mechanism*, E6-S7 the *evidence*). NO new sidecar/core/backend surface (the `sidecar.Client` fail-open primitive is unchanged; per-org policy is resolved locally from the dev config, consistent with every other coordinate).

## Acceptance Criteria
1. **Two-state policy (SDK parity)** — `FailurePolicy` is `FailOpen` (default) or `FailClosed`. `ResolveFailClosed()` reads the config `fail_closed` then the `OPENBOX_FAIL_CLOSED` env (env wins), default false; a missing/unreadable config is false (fail-safe — an org never becomes fail-closed by accident).
2. **`applyFailurePolicy` transform** — on `dec.FailOpen && FailClosed` it synthesizes `VerdictHalt` (content-free reason); otherwise (fail-open, OR any real verdict under either policy) it returns the decision UNCHANGED. `mapVerdict`/`applyDecision` are not modified.
3. **Fail-open default preserved (OD9)** — with fail_closed off, a sidecar-absent/slow/malformed decision → `VerdictUnknown` → nothing to stdout → tool proceeds; byte-identical to E6-S2. A test asserts the transform is a no-op for fail-open.
4. **Fail-closed denies on outage** — with fail_closed on, a sidecar-absent/timeout decision → synthetic HALT → `deny` on stdout with a content-free reason; the tool is blocked. A real ALLOW/CONSTRAIN from a reachable sidecar still PROCEEDS even under fail-closed (the policy governs outages only, not real allow verdicts).
5. **Configurable, clamped timeout** — `ResolveEnforceTimeout()` reads `enforce_timeout_ms` then `OPENBOX_ENFORCE_TIMEOUT_MS` (env wins); `<=0` → 0 (⇒ `sidecar.DefaultDecisionTimeout`); a value over `maxEnforceTimeout` (2 s) is clamped. Threaded into `newSidecarClient` so the enforce wait is bounded and stays under CC's 5 s hook kill.
6. **Content-free fail-closed reason (INV-2)** — the deny reason is policy-authored + the fail-open cause (a static internal diagnostic string, e.g. "sidecar unavailable"), never the tool command/file/output. The durable audit records the synthetic HALT with `fail_open=true` (⟺ a fail-closed deny, distinguishable from a real-verdict deny).
7. **Enforce-only / PreToolUse-only** — fail-closed only affects the enforce-mode PreToolUse gate (inherited from E6-S1); enforce-off and non-PreToolUse hooks never deny (a test confirms enforce-off is byte-identical even under fail_closed=1).

## Write Scope
- `adapters/claude-code/enforce.go` — `FailurePolicy` type + constants, `resolveFailurePolicy`, `applyFailurePolicy`, `failClosedReason`; extend the header doc.
- `adapters/claude-code/creds.go` — `envFailClosed`/`envEnforceTimeoutMS` constants; `DevConfig.FailClosed`/`DevConfig.EnforceTimeoutMS`; `ResolveFailClosed`, `ResolveEnforceTimeout`; `maxEnforceTimeout`.
- `adapters/claude-code/hookrun.go` — resolve the policy + timeout; insert `applyFailurePolicy` between obtain and apply; pass the timeout into `newSidecarClient`.
- `adapters/claude-code/enforce_test.go` — `applyFailurePolicy` (no-op fail-open, synth-HALT fail-closed, real-verdict untouched), fail-closed E2E (absent sidecar → deny; live allow → proceed), enforce-off-under-fail_closed byte-identical.
- `adapters/claude-code/creds_test.go` — `ResolveFailClosed` precedence/default; `ResolveEnforceTimeout` default/clamp/env.

## Invariants
- **INV-3b:** the block is synchronous, pre-execution, bounded by the (clamped) hard timeout, and — by DEFAULT — fail-open. Fail-closed is the explicit, bounded, opt-in inversion.
- **INV-2:** the fail-closed reason + audit carry only policy-authored text and static internal cause strings — never tool content.
- **INV-1:** no secret on the path (policy + timeout are cheap config+env reads; no secret I/O).

## Human Gates
| Gate | Question | Owner | Outcomes |
|---|---|---|---|
| G3_REVIEW | Does the failure policy mirror the SDK (fail-open default, opt-in fail-closed, verdict-agnostic cascade), with a bounded/clamped timeout that keeps a fail-closed deny deliverable under CC's 5 s kill? | brian | approve / revise |
| G_SEC | Is fail-closed correct (denies on outage, never on a real allow), the reason/audit content-free (INV-2), the wait bounded (INV-3b), and does fail-open stay the safe default? | Sam | approve / revise / block |

## Validation
```bash
cd adapters/claude-code && go build ./... && go vet ./... && go test -race ./...
cd ../../sidecar && go build ./... && go test -race ./...
cd ../cli && go build ./... && go vet ./... && go test ./...
# Live: enforce on + fail_closed on, NO `openbox sidecar serve` running →
#   any tool          → deny (stdout permissionDecision "fail-closed … sidecar unavailable")
# Live: enforce on + fail_closed on + `openbox sidecar serve` (example bundle) →
#   rm -rf /          → deny (real BLOCK)
#   echo hi           → nothing to stdout (real allow PROCEEDS even under fail-closed)
# Live: enforce on + fail_closed OFF, no sidecar →
#   any tool          → nothing to stdout (fail-open, OD9)
```

## Stop conditions
- If fail-closed ever denies on a REAL allow/constrain verdict (not an outage) → STOP: the policy governs the evaluation-unavailable case only.
- If a fail-closed deny reason or audit carries the shell command / file body / tool output → STOP (INV-2).
- If the enforce wait can exceed CC's 5 s PreToolUse timeout → STOP: a hook-kill silently defeats fail-closed (the timeout MUST be clamped below it).
- If this story modifies `mapVerdict`/`applyDecision` semantics or the `sidecar.Client` fail-open primitive → STOP (E6-S3 is a transform layered on top).
- If fail-open stops being the default → STOP (OD9).
