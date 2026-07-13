# STORY-E6-S5 — Local decision sidecar (the resident daemon the enforce hook calls)

**Epic:** E6 (enforcement — the `apply` leg). **Risk:** high (first resident/stateful process in the repo; it is the mechanism the INV-3b bounded wait depends on — a wrong timeout/fail-open would either hang the dev loop or silently allow a BLOCK).

## Source
- **Backlog:** `.fab7/sdlc/stories/E6-backlog.md` §E6-S5 — "the resident daemon E6-S1 calls (Unix socket, OPA bundle, out-of-band core sync, fail-safe-absent → fail-open); lifecycle (start/own/sync). S2 proved direct HTTP is a ~1.6 s NO-GO, so this is the only viable decision path — not optional. Target single-digit-ms local decision."
- **Spike S2** (`discovery/spikes/S2-enforcement-latency.md`, DONE 2026-07-13): direct sync `POST /evaluate` = ~0.8–1.6 s (Temporal workflow, loopback floor) → **NO-GO**; a local sidecar is **MANDATORY**; hook budget ≈ **50 ms**, sidecar decision target **<10 ms**; `/evaluate` stays the async telemetry channel only.
- **ADR-0003** (accepted, brian 2026-07-13): the sidecar's home — a new top-level `sidecar/` module, invoked as `openbox sidecar serve`; depends on `client/`; adapters/`cli` depend on it; one shipped binary preserved (WIRE-2). MUST NOT put a network round-trip on the synchronous decision path.
- **ADR-0002** (accepted): **INV-3b** — an enforce path MAY block, but only at `PreToolUse`, only within the hard ~50 ms per-call timeout, **fail-open by default** (OD9).

## Inlined context (verified — builder need not re-read)
- **The verdict shape already exists** (`client/verdict.go`): `client.Evaluation` (`Verdict`, `Reason`, `PolicyID`, `RiskScore`, `Constraints`, `Guardrail`, …) + `Verdict` enum (`ALLOW`/`CONSTRAIN`/`REQUIRE_APPROVAL`/`BLOCK`/`HALT`/`""`=Unknown) + `WouldBlock()`. The sidecar's decision RETURNS a `client.Evaluation` so E6-S1's hook and E6-S2's `apply` consume the same value the Advisory tier already records. Reuse it — do not invent a parallel verdict type.
- **The socket protocol is a NEW, small, local IPC contract** — deliberately NOT in `client/` (which is the AIP-signed egress transport; same separation ADR-0001 used). `sidecar/` exports the request/response wire types + a thin Go client both the daemon and the enforce hook import.
- **Telemetry reuse:** `client.Client.Emit` (`client/client.go`) is the fire-and-forget `/evaluate` egress the sidecar uses **out-of-band** to mirror decisions as observations. `client.Client.Validate` (`client/validate.go`) is the signed-GET preflight pattern the out-of-band bundle sync reuses (method-agnostic signer). Neither is ever on the synchronous decision path.
- **Module convention:** each component is its own `go.mod` wired by `replace ../…` (no `go.work`); Go 1.23; no cgo; single static binary (OD17). `sidecar/` follows this: own `go.mod`, `require`+`replace` on `client/`; `cli/` and `adapters/claude-code/` add `require`+`replace → ../sidecar`.
- **Composition root:** `cli/cmd/openbox/main.go` wires subcommands (`dev`, `hook`); add `sidecar` → `openbox sidecar serve`. The hook path's INV-3 discipline (always exit 0, nothing to stdout) is unchanged; `serve` is a long-lived foreground process, not a hook.

## Acceptance Criteria
1. **New `sidecar/` module** (`github.com/openbox-ai/openbox-shift-left/sidecar`, own `go.mod`, `replace → ../client`). Builds, vets, tests green in isolation.
2. **Unix-domain-socket decision server**: listens on a per-user socket path (e.g. `$XDG_RUNTIME_DIR/openbox/sidecar.sock`, `0700` dir / `0600` socket — INV-1), accepts a decision request (session/DID + tool name/kind + action + attributes), evaluates locally, and returns a `client.Evaluation` (verdict + reason + policy id + constraints). Concurrency-safe.
3. **Local decision evaluator**: makes the verdict decision **entirely locally** from a synced policy bundle — NO network round-trip on the decision path (INV-3b structural invariant). Faithful to core's policy input/output shape to the extent a local evaluator can reproduce (per cross-repo recon); where a signal genuinely requires the full core workflow, the local decision is documented as a bounded approximation, never a silent wrong deny.
4. **Fail-safe-absent → fail-open**: the shared **client** the enforce hook uses (exported from `sidecar/`) dials the socket with a hard timeout (default ~50 ms, ADR-0002); on dial-fail / timeout / no-socket / malformed response it returns an **allow** (VerdictUnknown, degrade-to-observe) — never an error that blocks. A test proves: sidecar absent → allow; sidecar hung past the timeout → allow within the bound.
5. **Out-of-band bundle sync** (off the hot path): a background loop that refreshes the local policy bundle from core on an interval (reusing the signed-request pattern), with a staleness-tolerant default and a safe cold-start (no bundle yet → fail-open allow, never deny). Sync failure never affects an in-flight decision.
6. **Lifecycle**: `openbox sidecar serve` starts the daemon (create socket dir, bind, serve, graceful shutdown on SIGINT/SIGTERM, remove stale socket on start). **OD-SIDECAR-LIFECYCLE** (who owns the process — systemd/launchd user unit vs spawned-on-demand) is surfaced, not silently chosen; Phase-1 ships the manual `serve` + fail-safe-absent contract so it is safe either way.
7. **Observe/advisory path untouched**: `adapters/claude-code` observe spool + `openbox hook …` are byte-unchanged; observe/advisory sessions never dial the sidecar (INV-3 verbatim for them).
8. **Wiring**: `openbox sidecar serve` is a subcommand of the one `openbox` binary (WIRE-2); `cli` imports `sidecar/`. No second shipped binary.

## Nonfunctional Requirements
- **security (G_SEC required):** socket is per-user, not world-accessible (INV-1); no secret/content on a log or the socket error path (INV-1/INV-2); the decision path takes NO network I/O (INV-3b); a malformed/oversized request is rejected without crashing the daemon (bounded read). Bundle sync is signed like every data-plane call.
- **reliability/performance (NFR-2):** decision p95 target single-digit ms (measure once the daemon exists — S2 follow-on); the hook-side dial timeout hard-bounds worst case; a dead/slow/absent sidecar degrades to observe, never hangs a tool call.

## Write Scope
- **NEW** `sidecar/` (module: server, socket protocol + shared client, local evaluator, bundle sync, `Serve` entrypoint).
- `cli/cmd/openbox/main.go` (+ `cli/go.mod`) — wire `openbox sidecar serve`.
- (No change to `adapters/claude-code` in this story — the enforce hook that dials the sidecar is **E6-S1**.)

## Dependencies
- **Hard:** E6-S0 (spike S2, DONE), `client/` (SL-3, Evaluation + signer). **ADR-0003 + ADR-0002 accepted.**
- **Feeds:** E6-S1 (PreToolUse → sidecar socket), E6-S3 (fail-open/closed policy configures the timeout), E6-S4 (local redaction runs in the sidecar).
- **EXT-core** merged (`c18bbc8`) — dev event types accept-listed for the async telemetry emit.

## Invariants
- **INV-3b:** decision is synchronous, pre-execution, bounded by the hard timeout, fail-open by default; NO network on the decision path.
- **INV-1:** socket perms per-user; no secret on log/argv/socket-error; bundle sync signed.
- **INV-2:** any tool-input content the request carries (for later redaction) stays local; never logged.

## Human Gates
| Gate | Question | Owner | Evidence | Outcomes |
|---|---|---|---|---|
| G3_REVIEW | Does the daemon decide locally, fail open when absent/slow within the bound, and leave the observe path untouched? | brian | diff review + socket/fail-open/timeout tests + `serve` smoke | approve / revise |
| G_SEC | Is the socket per-user, the decision path network-free, and no secret/content leaked; is the fail-open bound honest? | Sam (security reviewer) | review of socket perms + decision path + bundle-sync signing + bounded-read | approve / revise / block |

## Validation
```bash
cd sidecar && go build ./... && go vet ./... && go test -race ./...
cd ../cli && go build ./... && go vet ./... && go test ./...
openbox sidecar serve --help            # subcommand wired into the one binary
# socket round-trip: a request over the Unix socket returns an Evaluation;
# absent socket -> client returns allow (fail-open); hung server -> allow within the timeout bound.
```

## Stop conditions
- If any decision path would take a network round-trip (core `/evaluate`, backend, Guardrail API) → STOP (INV-3b breach; that is the ~1.6 s wall S2 rejected). Bundle sync + telemetry emit are out-of-band ONLY.
- If the sidecar-absent / timeout case can return anything other than **allow** by default → STOP (OD9 fail-open; fail-closed is E6-S3's opt-in, not this story).
- If core exposes NO real OPA bundle / distributable policy the sidecar can load → build the evaluator against a documented local policy representation seam (pluggable), ship the daemon + socket + fail-open contract, and record the bundle-format decision as a follow-up (do NOT block the daemon on an unconfirmed upstream bundle endpoint; mirror SL-15's "build the seam, flag the external dependency" discipline).
```
