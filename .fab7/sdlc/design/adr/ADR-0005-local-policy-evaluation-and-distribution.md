# ADR-0005: Local policy evaluation strategy + policy distribution (closing `[EXT-opa-bundle]`)

## Status
accepted — brian 2026-07-15 (Epic E6, story E6-S8). Supersedes design `sidecar-policy-sync.md` **OD-SYNC-1** (which had ratified "embed OPA rego"); resolves **OD-SYNC-2** (dependency posture) and **OD-SYNC-4** (sync auth artifact).

<!-- G_ADR gate. Decision owner: brian. This ADR records the choice that closes the
[EXT-opa-bundle] seam left by E6-S5: HOW the local sidecar obtains org policy from
OpenBox and HOW it evaluates a developer tool call with verdict parity to
openbox-core. It reverses a previously-ratified design decision (embed OPA rego),
so it is recorded as its own decision, not a silent design edit. -->

## Context

E6-S5 shipped the sidecar with two pluggable seams — `BundleSource` (`sidecar/sync.go`)
and `Evaluator` (`sidecar/evaluator.go`) — but only a local-file source and a
metadata-only JSON `bundleEvaluator`. There is no sync of real policy from OpenBox
and no faithful evaluation of a real per-agent policy. That gap is `[EXT-opa-bundle]`.

The design `sidecar-policy-sync.md` (ratified 2026-07-14) chose **embed OPA rego**
(OD-SYNC-1): compile the fetched `rego_code` in-process with the OPA Go `rego`
package and evaluate it, for byte-parity with core across both builder-authored
**and** hand-written raw-rego policies.

Two facts, established during E6-S8 recon (cross-repo Explore, 2026-07-15), forced a
re-examination of that choice:

1. **OD17 is "single static Go binary; no cgo."** The only genuinely *lightweight*
   Rego interpreters — Microsoft **Regorus** (Rust) and **rego-cpp** (C++) — are
   reached from Go **via CGo**, which violates OD17 and adds a Rust/C++ toolchain to
   the release. The only **pure-Go** Rego implementation is **OPA itself**, whose
   `rego` package pulls a large transitive dependency tree into `bin/openbox` (the
   binary every developer installs). So "a small in-process rego interpreter with
   full parity" does **not exist** for a pure-Go, no-cgo product: you may have at most
   two of {lightweight, pure-Go/no-cgo, full-rego parity}.

2. **openbox-core does not embed OPA and distributes no bundle.** Core is a *pure OPA
   HTTP client* (POSTs `{"input":…}` to an external OPA server; `internal/services/opa.go`);
   per-agent rego lives in the backend's Postgres (`PolicyEntity.rego_code`). There is
   no S3/`bundle.tar.gz` for a dev laptop to pull. The realistic policy *source* is the
   backend control-plane read `GET /agent/:agentId/policies/current`, which returns the
   full `PolicyEntity` — including both the compiled `rego_code` **and** the pre-compilation
   structured `config.policy_builder` (when the policy was authored in the builder UI).

## Decision

**1. Evaluate the `policy_builder` structured config natively in pure Go — do not embed a rego engine.** (Reverses OD-SYNC-1.)

- A new pure-Go `builderEvaluator` (Evaluator impl) evaluates a parsed
  `PolicyBuilderConfig` directly, replicating the backend's builder→rego compilation
  semantics (`openbox-backend policy-builder.util.ts`) so its verdict equals what core's
  external OPA would return for the same event: the 9 operators
  (`equals`/`not_equals`/`contains`/`greater_than`/`greater_than_or_equal`/`less_than`/`less_than_or_equal`/`exists`/`not_exists`),
  `valueType` coercion (string/number/boolean; `count`→number), `transform` (`value`/`count`),
  `matchMode` (`all`=AND, `any`=OR with `[_]` existential over arrays), **first-match-by-rule-order**
  precedence, and the default `ALLOW`. Decisions map through the existing
  `decisionToVerdict` (uppercase `ALLOW|REQUIRE_APPROVAL|BLOCK|HALT`, case-insensitive).
- Zero new heavyweight dependency; no cgo; `bin/openbox` stays small (honors OD17 + OD-SYNC-2).

**2. Accept a documented fidelity residual for hand-written raw rego.** (parity-with-deviation, extends OD-SYNC-7)

- A policy with **no** `config.policy_builder` (raw rego authored outside the builder)
  cannot be evaluated locally without a rego engine. Such a policy **does not localize**:
  the sidecar evaluates it as *no local verdict* (fail-open-local, source flagged), and
  the session relies on the already-accepted §2a fidelity floor — the async `/evaluate`
  telemetry channel (T3 audit) and, for high-risk classes, the Tier-2 sync `/evaluate`
  escalation (E6-S10). This is honest under-blocking, never over-blocking (OD9).
- The residual is surfaced (a non-secret diagnostic + a `dev sync` warning), never silent.

**3. Distribution = pull-at-init + session-start staleness check; the daemon does zero network I/O.**

- The **CLI** (`openbox dev sync`, and the last step of `openbox dev init`) performs the
  read `GET /agent/:agentId/policies/current` with the **org control-plane credential**
  (`OPENBOX_CONTROL_TOKEN`: an `obx_key_…` org key sent as `X-API-Key`, or a Keycloak JWT —
  the `read:agent_policy` permission is org-scoped; **OD-SYNC-4 resolved: org key, not the
  agent runtime `obx_` key**), reusing the existing `cli/internal/backend` control-plane
  client. It translates `config.policy_builder`→ the local bundle, writes it plus a **PIN
  `(policy.id, updated_at)`** to the bundle file, and preserves `rego_code` opaquely.
- The daemon loads that file via the **unchanged `FileBundleSource`** and does **no network
  I/O at all** — strengthening INV-3b. The 60 s `syncLoop` background poll is retired for the
  online path (the file loader remains).
- **Staleness** is a **client-side compare at session start** (adapter): a best-effort signed
  `GET …/policies/current` compares the backend `(id, updated_at)` to the local PIN. Match →
  proceed; mismatch under fail-open → warn ("run `openbox dev sync`") + proceed on the stale
  bundle; mismatch under fail-closed → mark the session stale so the PreToolUse enforce gate
  **denies until refreshed** (CC has no "deny a session" primitive at SessionStart, so the
  block is realized where enforce already has teeth — the tool-call gate — closing the
  under-enforcement window). Any fetch failure (offline, no org key in the hook environment)
  keeps the last-good bundle and **never denies at fetch time**.

**4. Correct the query/result contract in the design.** The generated policy's output rule is
`result` (an object `{decision, reason}`), so the OPA query is `data.<pkg>.result` then read
`.decision`/`.reason` — **not** `data.<path>.decision` as the design draft stated. The native
evaluator sidesteps the query entirely but the input-shaping (`BuildOPAInput`) must still match
core's `buildSpanMap` key names, since builder field paths (`spans[_].file_path`, …) resolve
directly against that input tree.

## Consequences

- **Positive:** no cgo, no heavy dependency, small binary (OD17 intact); real parity for the
  common case (builder-authored policies, the UI path); a real policy *source* (org-scoped
  per-agent read, no S3 creds, no multi-tenant global bundle); the daemon is network-free on
  every path; reuses `cli/internal/backend` + the SL-15 org-key pattern (no new backend surface,
  no new endpoint, no core change → no CLAUDE.md reuse-rule violation).
- **Negative / accepted:** hand-written raw-rego policies do not enforce locally (fidelity
  residual, flagged, covered by T2/T3); the native evaluator must track the backend's
  builder-compilation semantics — a **conformance suite pinned to `policy-builder.util.ts`** is
  the guard against drift (a builder change that the evaluator doesn't mirror is a parity bug).
- **Reversibility:** the `Evaluator` seam is unchanged, so a future embedded-OPA or Regorus
  evaluator (if OD17 is ever relaxed, or a rego requirement forces it) drops in behind the same
  interface with no protocol/server change — this ADR chooses the *default* evaluator, not the
  seam.

## Alternatives considered
- **Embed OPA `rego` (pure-Go, full parity)** — rejected for E6-S8 on binary-size/dependency
  grounds (OD-SYNC-2); the seam keeps it available later.
- **Regorus/rego-cpp via CGo (lightweight + parity)** — rejected: violates OD17 (no cgo) and
  adds a non-Go toolchain to the release; would itself need an ADR amending OD17.
- **Pull the S3 `bundle.tar.gz`** — rejected (design §4): global multi-org bundle, needs KMS/S3
  creds, no per-org isolation; the per-agent signed read is strictly better for a dev machine.
