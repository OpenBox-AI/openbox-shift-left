# Architecture decision records

CLAUDE.md's rule is that a new table, endpoint or service requires an ADR. That
rule was unenforceable for most of this repo's life: ADR-0001 through 0007 and
0009 were cited throughout the code and docs — including by ADR-0008 itself —
but existed nowhere on disk. They were presumably written into a git-ignored
tooling directory and lost.

ADR-0001…0007 and 0009 were **reconstructed on 2026-07-31** from the decisions'
citations, the code that implements them, and the commit history. Each is marked
as reconstructed and states what evidence it was rebuilt from. They record what
was decided and why, as accurately as the surviving evidence supports; where the
original reasoning is not recoverable, they say so rather than inventing it.

Every ADR not marked "reconstructed" in the table below is an original.

| ADR | Title | Status |
|-----|-------|--------|
| [0001](ADR-0001-provider-spi-registry.md) | Provider SPI with the registry in the composition root | Accepted (reconstructed) |
| [0002](ADR-0002-inv3b-carve-out.md) | INV-3b: enforcement may block, but only in-process | Accepted in part (reconstructed; clause 3 retired by 0017, clause 1 by 0016) |
| [0003](ADR-0003-decision-module.md) | The decision engine is its own module | Accepted (reconstructed, superseded in part by 0006) |
| [0004](ADR-0004-base-wire-unification.md) | Unify dev telemetry onto the base SDK wire model | Accepted (reconstructed, amended, superseded in part by 0013) |
| [0005](ADR-0005-native-policy-evaluator.md) | Native Go policy evaluator instead of embedded OPA | **Superseded by 0017** (reconstructed) |
| [0006](ADR-0006-in-process-decider.md) | In-process decider; retire the socket sidecar | Accepted (reconstructed) |
| [0007](ADR-0007-shared-devconfig.md) | One shared dev-config module across adapters | Accepted (reconstructed) |
| [0008](ADR-0008-signed-policy-bundles.md) | Signed policy bundles | **Moot for this runtime** since 0017 — there is no bundle to sign |
| [0009](ADR-0009-idempotency-receipts.md) | Server-side idempotency and delivery receipts | Accepted (reconstructed) |
| [0010](ADR-0010-signed-commit-attestation.md) | Signed commit attestation | Accepted |
| [0011](ADR-0011-multi-module-layout.md) | Keep the multi-module layout, with a workspace | Accepted |
| [0012](ADR-0012-autonomous-approver.md) | Autonomous approver: envelope-bounded, host-pluggable, narrowing-only | Accepted |
| [0013](ADR-0013-tool-call-as-activity.md) | A tool call is an Activity; retire the hook-span layer | Accepted |
| [0014](ADR-0014-turn-as-activity-and-identifier-allowlist.md) | A model turn is an Activity; INV-2's usage path becomes an allowlist | Accepted |
| [0015](ADR-0015-plaintext-credential-file.md) | Credentials live in one plaintext file; the OS keychain is deleted | Accepted |
| [0016](ADR-0016-default-install-posture.md) | What a bare `openbox init` does: project-local scope, enforce ON | Accepted |
| [0017](ADR-0017-inline-policy-evaluation.md) | Inline policy evaluation: `/evaluate` is the only decider | Accepted |
| [0018](ADR-0018-dev-turn-content-carrier.md) | Tool status, and one wire span carrying the assistant turn text | Accepted (amends 0013 and 0014) |
| [0019](ADR-0019-full-content-capture.md) | Full content capture, under one org gate | **Proposed** — authorizes nothing until accepted |
| [0020](ADR-0020-prompt-gate-and-halt-session-stop.md) | The prompt gate, and HALT ends the session | Accepted (extends 0017) |
| [0021](ADR-0021-openbox-local-gateway.md) | The OpenBox gateway is a per-developer LOCAL service | **Draft** — 3 probe answers open |
