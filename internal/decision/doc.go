// Package decision is the OpenBox developer-runtime local policy decision
// engine — what the enforce-mode PreToolUse hook asks for a governance
// decision before a tool call runs. There is no resident process, no socket,
// and nothing resident: the decision is computed in-process (ADR-0006).
//
// Why local: a synchronous POST to openbox-core's /api/v1/governance/evaluate
// costs ~0.8-1.6s (the Temporal governance workflow, even on loopback) —
// far past a tolerable per-tool-call budget. You cannot block a developer's
// Bash/Edit/Read on a second-plus round-trip. So the enforcement decision
// must be made locally, from a policy bundle synced out-of-band. The
// evaluator is pure-Go and in-memory (no OPA, no cgo, no network on the
// decision path), so the hook — itself a short-lived per-tool-call process —
// loads the local bundle and evaluates it directly in microseconds. No IPC
// needed.
//
// The one structural invariant (ADR-0002 INV-3b):
//
//	The decision path takes no network I/O and no IPC. It answers only from
//	the local, already-synced bundle. Bundle sync (`openbox dev sync`) and
//	the async telemetry emit to /evaluate are out-of-band only, never on the
//	hot path. When no policy is loaded the decision fails open (allow;
//	degrade to observe) — an infra failure never blocks the dev loop.
//	Per-org fail-closed is an opt-in layered on top.
//
// Shape of the module:
//
//   - protocol.go  — the DecisionRequest / DecisionResponse contract. A local
//     in-memory contract, deliberately separate from client/ (the AIP-signed
//     core egress) — the same separation ADR-0001 used.
//   - inprocess.go — InProcessDecider (the sole decision transport) + the
//     Decision result type the enforce hook consumes.
//   - server.go    — the in-memory engine that holds the loaded evaluator +
//     secret detector and computes decide(). Constructed per hook invocation.
//   - evaluator.go / builder.go / input.go — the native decision evaluator.
//     Ports the reference SDK's verdict priority cascade
//     (openbox-temporal-sdk-python verdict_handler.enforce_verdict).
//   - bundle.go    — the synced policy bundle (parse/load/validate) + its
//     default path.
//   - secrets.go   — the Tier-1 local secret detector, decoupled from the
//     verdict.
//
// Observe/advisory sessions never invoke the decider; their async spool path
// (adapters/claude-code) is untouched and INV-3 holds verbatim for them.
//
// DEPENDENCY BOUNDARY. This subtree's imports are held to an allowlist in
// internal/depguard, both external and repo-local (ADR-0023 as amended by
// ADR-0024). Adding an import outside it fails there first, which is the
// point — widening the list to make an import pass inverts the ADR's
// reasoning. This comment is the signpost; depguard is the enforcement.
package decision
