// Package decision is the OpenBox developer-runtime local policy DECISION ENGINE —
// what the enforce-mode PreToolUse hook (E6-S1) asks for a governance decision
// BEFORE a tool call runs. There is NO resident process, NO socket, and NO daemon:
// the decision is computed IN-PROCESS (ADR-0006, which retired the ADR-0003
// Unix-socket daemon — this module was formerly named `sidecar`).
//
// Why local (spike S2, 2026-07-13): a synchronous POST to openbox-core's
// /api/v1/governance/evaluate costs ~0.8–1.6 s (the Temporal governance workflow,
// even on loopback) — ~16–33× a tolerable per-tool-call budget. You cannot block a
// developer's Bash/Edit/Read on a second-plus round-trip. So the enforcement
// decision MUST be made LOCALLY, from a policy bundle synced out-of-band. Since
// E6-S8/ADR-0005 the evaluator is pure-Go and in-memory (no OPA, no cgo, no network
// on the decision path), so the hook — itself a short-lived per-tool-call process —
// loads the local bundle and evaluates it directly in microseconds. No IPC needed.
//
// The one structural invariant (ADR-0002 INV-3b):
//
//	The decision path takes NO network I/O and NO IPC. It answers only from the
//	LOCAL, already-synced bundle. Bundle sync (`openbox dev sync`) and the async
//	telemetry emit to /evaluate are out-of-band ONLY, never on the hot path. When
//	no policy is loaded the decision fails OPEN (allow; degrade to observe) — an
//	infra failure never blocks the dev loop (OD9). Per-org fail-closed is an opt-in
//	(E6-S3) layered on top.
//
// Shape of the module:
//
//   - protocol.go  — the DecisionRequest / DecisionResponse contract. A LOCAL
//     in-memory contract, deliberately separate from client/ (the AIP-signed core
//     egress) — the same separation ADR-0001 used.
//   - inprocess.go — InProcessDecider (the sole decision transport) + the Decision
//     result type the enforce hook consumes.
//   - server.go    — the in-memory engine that holds the loaded evaluator + secret
//     detector and computes decide(). Constructed per hook invocation.
//   - evaluator.go / builder.go / input.go — the native decision evaluator. Ports
//     the reference SDK's verdict priority cascade
//     (openbox-temporal-sdk-python verdict_handler.enforce_verdict).
//   - bundle.go    — the synced policy bundle (parse/load/validate) + its default path.
//   - secrets.go   — the Tier-1 local secret detector (E6-S9), decoupled from the verdict.
//
// Observe/advisory sessions never invoke the decider; their async spool path
// (adapters/claude-code) is untouched and INV-3 holds verbatim for them.
package decision
