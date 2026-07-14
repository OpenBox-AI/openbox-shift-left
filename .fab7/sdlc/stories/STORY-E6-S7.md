# STORY-E6-S7 — Enforcement conformance + INV-3b evidence (+ E6-S3 INFO-1 reconciliation)

**Epic:** E6 (enforcement — the `apply` leg). **Risk:** medium-high. Two parts: (1) a durable **conformance suite** that pins the INV-3b contract as executable evidence (finalizes ADR-0002 — no ratification, evidence only); (2) a small but **security-load-bearing correctness fix** — the E6-S3 G_SEC INFO-1 "sidecar up but UNBUNDLED" quadrant, where a fail-closed org is today silently ungoverned. A bug in (2) either leaves the hole open, or makes fail-closed deny on a real allow. **Status target:** review (build + validations + both reviews, pending brian G3 + Sam G_SEC).

## Source
- **Backlog:** `.fab7/sdlc/stories/E6-backlog.md` §E6-S7 — "a conformance suite proving: an enforced BLOCK **denies** the call (pre-execution); a sidecar-down/timeout case **fails open** within the bound (OD9); observe/advisory sessions still uphold INV-3 verbatim; fail-closed (when opted in) denies on outage. Finalizes ADR-0002." Write scope: `adapters/claude-code/` (+ conformance). Deps: E6-S1..S3. Gates: G3. Invariants: INV-3b.
- **[CARRY-FORWARD from E6-S3 G_SEC INFO-1]** (backlog §E6-S7, verbatim): the conformance suite MUST cover the **"sidecar up but UNBUNDLED"** quadrant — today a reachable sidecar with no bundle replies `Source="fail-open:no-bundle"` but a well-formed reply → the client marks the Decision `FailOpen=false` → `applyFailurePolicy` treats it as a **real verdict** and does **NOT** engage fail-closed, so a fail-closed org is **silently ungoverned** while the daemon is up but unbundled. Since core ships no real OPA bundle yet (`[EXT-opa-bundle]`, E6-S5), this is a plausible LIVE state. E6-S7 should (a) add the conformance case, and (b) **reconcile the semantic mismatch** — the server should signal "no real verdict obtained" (not a `FailOpen==false` decision carrying a `fail-open:no-bundle` source) so the client-side failure policy can engage.
- **ADR-0002** (INV-3b enforcement carve-out) — **ACCEPTED** (G_ADR, brian 2026-07-13; S2 supplied the ≈50 ms timeout). E6-S7 supplies the conformance **evidence**, not the ratification.

## Cross-repo recon (openbox-temporal-sdk-python, 2026-07-14 — Explore) — the reference has NO equivalent state
The reference SDK has **no notion of "reachable-but-no-authoritative-verdict."** `evaluate` has exactly three outcomes (`client.py:110-148`): a parsed verdict, `None` (fail-open/proceed), or a synthesized `Verdict.HALT`. `_handle_api_error` (`client.py:204-208`) synthesizes the fail-closed HALT **only** on a raised exception or an HTTP ≥ 400 status (call sites `client.py:118-121`, `145-148`). A successful-but-empty/degenerate 200 → `ALLOW` (`types.py:39-54` `from_string` defaults everything to `ALLOW`; there is no `VerdictUnknown`); an unparseable 200 body → `None` → **proceed even under fail-closed** (`client.py:140-143`). So the reference would let an "unbundled but reachable" evaluation PROCEED.

**Why we deviate (and why it is the consistent choice, not a fresh policy):** the SDK's evaluation endpoint (core) always has policy resident (rego in Postgres) — "reachable-but-unbundled" cannot occur there. In shift-left the **LOCAL sidecar can genuinely be up-but-unbundled** (core distributes no real OPA bundle yet — `[EXT-opa-bundle]`), a state the reference architecture has no analog for. Critically, **E6-S3 already shipped the deviation on the adjacent axis**: a malformed/unusable sidecar reply already yields `Decision.FailOpen=true` → a fail-closed org denies (`allowFailOpen("sidecar response malformed")`, more conservative than the SDK's proceed-on-unparseable-200). The unbundled case is the **same class** — "reachable, no usable verdict" — and was missed only because the server returns a *well-formed* no-bundle reply. Closing it makes E6-S3's fail-closed posture **internally consistent** ("block whenever no real verdict was obtained, however that happened"), which is the coherent meaning of a fail-closed opt-in ("block when you cannot govern"). This deviation is deliberate, security-favorable, documented in code, and surfaced for brian's G3.

## The reconciliation (part b) — server signals "no real verdict", client engages the failure policy
Today the split is:
- **Server** (`sidecar/server.go`) already labels every non-verdict outcome `Source = sourceFailOpenNoBundle`: cold start / no bundle (`decide`, eval==nil), unknown protocol, missing session id, malformed request. Only a resident-evaluator decision is tagged `sourceLocalBundle` (set for ANY `eval != nil` — the rule bundle today and the embedded-OPA evaluator later, ADR-0003).
- **Client** (`sidecar/client.go`) sets `Decision.FailOpen` from the WRONG signal: it is `true` only when the *client itself* synthesized an allow (socket absent / timeout / malformed reply), and `false` for ANY well-formed server reply — including `sourceFailOpenNoBundle`. So a well-formed "no verdict" reply is mistaken for a real verdict.

The fix has two coherent halves, both making `Decision.FailOpen` match its OWN documented meaning ("no real daemon verdict", `client.go`):
1. **Server honesty** — the no-verdict paths (`decide`, eval==nil / bad protocol / missing session) return `client.VerdictUnknown` (honest: "not evaluated"), not `VerdictAllow`. This aligns the server-degrade with the client-degrade (`allowFailOpen` also uses `VerdictUnknown`) and directly implements "the server should signal 'no real verdict obtained'". (Behaviorally inert on its own — a fail-open org proceeds on either verdict; the failure policy keys on `FailOpen`, not the verdict.)
2. **Client mapping** — after a successful response parse, set `Decision.FailOpen` from whether the source is a real evaluated verdict: `FailOpen = !isRealVerdictSource(resp.Source)`, where the ONLY real-verdict source is `sourceLocalBundle`. A degrade source (`sourceFailOpenNoBundle`) or an unrecognized/empty source → `FailOpen=true` (the safe direction — route to the failure policy). `sourceFailOpenClient` is still set exclusively by the client's own `allowFailOpen` (unchanged).

**Preserved crux (E6-S3):** a REAL allow/constrain from a reachable, bundled sidecar is `sourceLocalBundle` → `FailOpen=false` → `applyFailurePolicy` passes it through → PROCEEDS even under fail-closed. Fail-closed still blocks on "no real verdict obtained" ONLY, never on a real allow. A stale-but-loaded bundle is `sourceLocalBundle` (Stale=true) → a real verdict → NOT a fail-closed trigger (staleness never denies).

## The conformance suite (part a) — INV-3b as executable evidence
A new `adapters/claude-code/enforce_conformance_test.go` drives `RunHook("PreToolUse", …)` end-to-end against a REAL `sidecar.Server` (or a deliberately-absent socket) and asserts the exact stdout contract per quadrant. This is the durable evidence ADR-0002/INV-3b is upheld — it breaks HERE if a future change regresses the carve-out. Matrix:

| # | Enforce | Policy | Sidecar state | Expected stdout | Invariant proven |
|---|---|---|---|---|---|
| C1 | on | (either) | up + bundle, rule = BLOCK | `permissionDecision:"deny"` (pre-execution) | enforced BLOCK denies |
| C2 | on | fail-open | absent socket | (empty — proceed) within the bound | outage fails open (OD9) |
| C3 | on | fail-open | up, NO bundle (unbundled) | (empty — proceed) | unbundled fails open |
| C4 | on | fail-closed | absent socket | `deny` (content-free outage reason) | fail-closed denies on outage |
| C5 | on | fail-closed | up + bundle, rule = ALLOW | (empty — proceed) | fail-closed never denies a REAL allow |
| C6 | **on** | **fail-closed** | **up, NO bundle (unbundled)** | **`deny`** | **INFO-1: the closed hole** |
| C7 | off (observe) | — | up + bundle, rule = BLOCK | (empty) | INV-3 verbatim: observe never blocks |
| C8 | on | fail-open | up + bundle, rule = BLOCK, latency > timeout | (empty — proceed) within the bound | timeout fails open within bound |

C6 is the regression guard for the INFO-1 fix: pre-fix it PROCEEDS (the hole); post-fix it DENIES. C3 confirms the fix leaves the fail-OPEN default byte-identical (unbundled still proceeds when not opted into fail-closed).

## Scope boundary (what this story is and is NOT)
- **IS:** the INFO-1 reconciliation (server `VerdictUnknown` on no-verdict paths + client `FailOpen` from source, via `isRealVerdictSource`); the enforcement conformance suite (C1–C8) as executable INV-3b evidence; updating the two existing sidecar tests that encode the OLD cold-start semantics; a documented note of the reference-SDK deviation; the E6-S4 §2 cheap hardening (size-guard the `jsonEqual` double-parse) folded in since it touches the same file and is a pure robustness nit.
- **IS NOT:** any change to `mapVerdict`/`applyDecision`/`applyFailurePolicy` LOGIC (they stay verdict/`FailOpen`-agnostic — the fix flows through them unchanged); any new config/env/flag; the real signed OPA-bundle distribution (`[EXT-opa-bundle]`, a separate upstream story); the redaction ENGINE (`[EXT-guardrail-redaction]`). The two other E6-S4 G_SEC INFOs are ENGINE-story work with no live surface today and are **deferred, not silently dropped**: (INFO-1) constrain `updatedInput` to content-only fields — needs a redaction engine to exist first; (INFO-3) `fileText()` under-captures MultiEdit's nested `edits[].new_string` — a capture-completeness nit on a path that is inert until a redaction engine lands. Both are re-flagged in the result artifact for the redaction-engine story.

## Acceptance Criteria
1. **Server signals no-verdict honestly** — `Server.decide` returns `client.VerdictUnknown` (not `VerdictAllow`) on every no-real-verdict path (cold start / eval==nil, unsupported protocol, missing session id). `Source` is unchanged (`sourceFailOpenNoBundle`). A resident-evaluator decision is still `sourceLocalBundle` with the evaluator's verdict.
2. **Client maps degrade source → FailOpen** — after a successful response parse, `Decision.FailOpen = !isRealVerdictSource(resp.Source)`; `isRealVerdictSource` returns true ONLY for `sourceLocalBundle`. A `sourceFailOpenNoBundle` / empty / unknown source → `FailOpen=true`. The client's own fault paths (`allowFailOpen`) are unchanged (`FailOpen=true`, `sourceFailOpenClient`).
3. **INFO-1 hole closed (C6)** — enforce on + fail-closed + a reachable-but-UNBUNDLED sidecar → `deny` on stdout with the content-free fail-closed reason. A conformance test asserts this.
4. **Fail-open default unchanged (C3)** — enforce on + fail-open (default) + reachable-but-unbundled → nothing to stdout (proceed); byte-identical to pre-fix. A test asserts the fix does NOT change the fail-open path.
5. **Crux preserved (C5)** — enforce on + fail-closed + a REAL allow from a reachable, bundled sidecar (`sourceLocalBundle`) → proceeds (no deny). Fail-closed never denies a real allow; staleness never denies.
6. **Conformance suite is executable evidence** — C1–C8 run under `go test -race` and assert the exact stdout per quadrant (deny/ask reason present or empty), driving the real `RunHook` PreToolUse path against a real `sidecar.Server` (or absent socket). Content-free throughout (no command/file/output in any asserted reason — INV-1/INV-2).
7. **No behavioral change to non-fail-closed, non-unbundled paths** — the existing enforce_test.go / server_client_test.go suites still pass (the two cold-start assertions are updated to the new honest semantics with an explaining comment); build/vet/test -race green across adapter + sidecar + cli.
8. **`jsonEqual` size-guard (E6-S4 INFO-2)** — the double-parse in `jsonEqual`/`canonicalJSON` is bounded so an oversized RedactedInput cannot force an unbounded re-parse; behavior on normal inputs unchanged.

## Write Scope
- `sidecar/server.go` — `decide` returns `VerdictUnknown` on the no-verdict paths (cold start / bad protocol / missing session); refresh the doc comments to say "no real verdict → Unknown + sourceFailOpenNoBundle".
- `sidecar/client.go` — `isRealVerdictSource`; set `Decision.FailOpen` from it on the success-parse branch; refresh the `Decision.FailOpen` / `Decide` docs; note the reference-SDK deviation.
- `sidecar/server_client_test.go` — update `TestColdStart_FailOpenNoBundle` (now `FailOpen=true`, `Source=sourceFailOpenNoBundle`, `Verdict=Unknown`) + any sibling assertion of the old semantics; add a `isRealVerdictSource` unit table.
- `adapters/claude-code/enforce_conformance_test.go` — NEW: the C1–C8 conformance suite (real server / absent socket, `RunHook` PreToolUse, per-quadrant stdout assertions).
- `adapters/claude-code/enforce.go` — `jsonEqual` size-guard (E6-S4 INFO-2); a code comment on the fail-closed-on-unbundled deviation where `applyFailurePolicy` is documented.
- (no change to `mapVerdict`/`applyDecision`/`applyFailurePolicy`/`resolveFailurePolicy` logic; no new config/env.)

## Invariants
- **INV-3b:** enforcement blocks only pre-execution, only within the bounded (clamped) timeout, fail-open by DEFAULT (OD9). The conformance suite is the durable proof. The INFO-1 fix only ever converts a would-be PROCEED into a DENY under an explicit fail-closed opt-in — tighten-only, still pre-execution and bounded.
- **INV-3 (verbatim, observe/advisory):** C7 proves an observe-mode session never writes a blocking signal even for a BLOCK-worthy tool.
- **INV-1 / INV-2:** every asserted reason is policy-authored / static-cause text; no command, file body, or output on any path the suite exercises.

## Human Gates
| Gate | Question | Owner | Outcomes |
|---|---|---|---|
| G3_REVIEW | Does the conformance suite faithfully prove the INV-3b contract per quadrant, and does the INFO-1 reconciliation close the unbundled-fail-closed hole without touching the verdict cascade or changing the fail-open default? Is the documented deviation from the reference SDK (fail-closed denies on unbundled, where the SDK would proceed) acceptable posture for a fail-closed opt-in? | brian | approve / revise |
| G_SEC | Is the reconciliation correct end-to-end (unbundled+fail-closed → deny; unbundled+fail-open → proceed; real allow → proceed under either policy; staleness never denies), is `FailOpen` now consistent with its documented meaning, and is the suite content-free (INV-1/INV-2)? | Sam | approve / revise / block |

## Validation
```bash
cd adapters/claude-code && go build ./... && go vet ./... && go test -race ./...
cd ../../sidecar && go build ./... && go vet ./... && go test -race ./...
cd ../cli && go build ./... && go vet ./... && go test ./...
# Live (real openbox + openbox sidecar serve):
# enforce on + fail_closed on + sidecar serve with NO bundle loaded (unbundled)  → any tool → deny (INFO-1 closed)
# enforce on + fail_closed OFF + same unbundled sidecar                          → any tool → nothing (fail-open unchanged)
# enforce on + fail_closed on + sidecar serve + example bundle, echo hi          → nothing (real allow proceeds under fail-closed)
# enforce on + fail_closed on + sidecar serve + example bundle, rm -rf /         → deny (real BLOCK)
```

## Stop conditions
- If the fix ever makes fail-closed deny on a REAL allow/constrain from a reachable, bundled sidecar (`sourceLocalBundle`) → STOP (the crux clause; the policy governs no-verdict only).
- If a fail-open (default) org's behavior changes on ANY path (unbundled must still proceed) → STOP (OD9; the fix is fail-closed-only in effect).
- If closing INFO-1 requires modifying `mapVerdict`/`applyDecision`/`applyFailurePolicy` LOGIC → STOP (they stay `FailOpen`-agnostic; the fix is upstream, in how `FailOpen` is set).
- If a conformance assertion or the fix surfaces the shell command / file body / tool output in any reason or record → STOP (INV-1/INV-2).
- If staleness (a real but old `sourceLocalBundle` verdict) starts triggering fail-closed → STOP (staleness never denies).
- If the enforce wait can exceed CC's 5 s PreToolUse kill on any conformance path → STOP (INV-3b bound).
