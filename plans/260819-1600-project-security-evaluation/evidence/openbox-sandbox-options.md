# OpenBox Sandbox ProjectRun option decision

Task: SE-08-03  
Decision: accept a versioned v2 capability as the design direction, defer its
implementation to a separate cross-repository plan. The original decision
retained native Codex as the current runner; that support row was withdrawn on
2026-08-23, so active execution now remains unsupported without changing this
v2 design choice.

Scores are comparative: 1 is poor/high-risk, 5 is strong. Effort and operations
score higher when simpler.

| Option | Security | Project fidelity | CI/remote | Cross-host stability | Delivery effort | Operations | Decision |
|---|---:|---:|---:|---:|---:|---:|---|
| Keep native-only permanently | 4 | 5 | 1 | 1 | 5 | 4 | Retain as current MVP, but it does not satisfy the later reproducibility/remote goal. |
| Wrap v1 using shell bootstrap, mounts, or inherited env | 1 | 3 | 2 | 2 | 3 | 2 | Reject. It erases v1's empty-workspace/empty-env boundary and creates a bootstrap loophole. |
| Add a versioned `ProjectRun` capability beside v1 | 5 | 5 | 5 | 4 | 2 | 3 | Selected design direction. Reuses authenticated lifecycle, durable cleanup, providers, and evidence without changing v1 bytes. |
| Build a separate project-run service | 4 | 5 | 5 | 4 | 1 | 1 | Defer. It duplicates authentication, provider adapters, lifecycle recovery, cleanup, release, and operations without a demonstrated isolation benefit. |

## Why the selected direction wins

- It is the only option that can materially improve reproducible environment,
  durable cleanup, typed egress/denial evidence, remote/CI execution, and
  cross-host policy stability together.
- It keeps the existing Phase 05 and Phase 07 interfaces available for parity
  without treating historical native runs as current support.
- It reuses v1's strongest components—strict framing, mTLS, asset identity,
  prepare/commit authority, durable reconciliation, provider adapters, bounded
  output, and terminal absence—without adding optional fields to v1 requests.
- It avoids a second operational service and avoids smuggling preparation into
  arbitrary shell argv.

## Acceptance conditions

The design direction is conditional, not a backend claim. Implementation may
start only in a separate accepted plan after:

1. the operation and media type are version-distinct from v1;
2. all 15 provider-neutral requirements have an owner and conformance case;
3. the credential/network/retention threat model is accepted;
4. a production client boundary and rollback path are owned; and
5. no host mount, ambient environment, broad egress, raw-process fallback, or
   weaker evidence is introduced.

If those conditions cannot be met, the decision reverts to native-only. There
is no v1 wrapper fallback.

## Evidence limits

- This is a source- and execution-evidence-backed architecture choice, not an
  implementation, prototype, benchmark, provider qualification, or release.
- Current OpenShell live integration is `not_runnable` without its exact
  provisioned gateway tuple; current macOS SRT denial-category live assertions
  fail and remain explicit inputs to the later conformance plan.
- Operational cost is estimated comparatively here; a later plan must measure
  image preparation, cold start, cache, transfer, execution, cleanup, and
  retained artifact costs before release.
