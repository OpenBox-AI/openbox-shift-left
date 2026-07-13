// Package sidecar is the OpenBox developer-runtime local decision daemon
// (STORY-E6-S5) — the resident process the enforce-mode PreToolUse hook (E6-S1)
// asks for a governance decision BEFORE a tool call runs.
//
// Why it exists (spike S2, 2026-07-13): a synchronous POST to openbox-core's
// /api/v1/governance/evaluate costs ~0.8–1.6 s (the Temporal governance
// workflow, even on loopback) — ~16–33× a tolerable per-tool-call budget. You
// cannot block a developer's Bash/Edit/Read on a second-plus round-trip. So the
// enforcement decision MUST be made LOCALLY, from a policy bundle synced
// out-of-band, in the single-digit-ms band. This package is that local
// evaluator; ADR-0003 records its home.
//
// The one structural invariant (ADR-0002 INV-3b + ADR-0003):
//
//	The synchronous decision path takes NO network I/O. The daemon answers only
//	from its LOCAL, already-synced bundle, within the hard per-call timeout
//	(~50 ms hook budget; sidecar decision target <10 ms). Bundle sync and the
//	async telemetry emit to /evaluate are out-of-band ONLY, never on the hot
//	path. If the daemon is absent / slow / down, the hook-side Client fails OPEN
//	(allow; degrade to observe) — an infra failure never blocks the dev loop
//	(OD9). Per-org fail-closed is a later opt-in (E6-S3), not this module.
//
// Shape of the module:
//
//   - protocol.go — the small Unix-socket wire contract (DecisionRequest /
//     DecisionResponse). This is a LOCAL IPC contract, deliberately separate
//     from client/ (the AIP-signed core egress) — same separation ADR-0001 used.
//   - client.go   — Client: what the enforce hook imports. Dials the socket with
//     a hard timeout and FAILS OPEN on any fault.
//   - server.go   — Server: the concurrency-safe Unix-socket decision server.
//   - evaluator.go / bundle.go — the local decision evaluator + the synced
//     policy bundle it reads. Ports the reference SDK's verdict priority cascade
//     (openbox-temporal-sdk-python verdict_handler.enforce_verdict).
//   - sync.go     — the out-of-band background bundle refresh (off the hot path).
//   - serve.go    — Serve: the `openbox sidecar serve` lifecycle (bind, serve,
//     graceful shutdown). cli/ wires the subcommand (WIRE-2: one binary).
//
// Observe/advisory sessions never talk to this daemon; their async spool path
// (adapters/claude-code) is untouched and INV-3 holds verbatim for them.
package sidecar
