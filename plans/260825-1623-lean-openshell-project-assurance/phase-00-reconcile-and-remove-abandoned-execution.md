# Phase 0 — Reconcile and remove the abandoned execution path

**Status:** verified

**Parent:** [Lean local OpenShell project security evaluation](plan.md)

## Goal

Leave a small, data-only project-assurance foundation before implementing the
OpenShell development evaluator. Preserve passive inspection and historical
pack consumption; remove every native project runner and its coupled execution
architecture.

## Decisions

- The public Phase 0 surface is `project inspect|verify|report|propose`.
- `project test`, `project rerun`, hidden probes, driver selection, profiles,
  scenarios, and governed mutation are removed rather than stubbed.
- Frozen v1 packs remain historical read contracts. Their semantic validator is
  private to the report reader and grants no execution authority.
- No read-only backend client existed to retain. Phase 2 owns a new bounded
  client for the normal local OpenBox backend APIs.
- The Mastra OCI image and Docker/OpenShell harnesses remain conformance assets.
  The Go fixture package retains only poison, safe-sink, and exact Ollama relay
  services with direct later-phase consumers.

## Completed tasks

| ID | Result | Evidence |
|---|---|---|
| OS-00-01 | Retained CLI behavior and historical projection bytes were captured; focused tests cover passive inspection, pack consumers, removed routes, v1 profile semantics, and judgment predicates. | `evidence/phase-00-reconcile-validation.json` |
| OS-00-02 | ADR-0020/0021, public docs, and superseded plans identify native execution as historical and the lean plan as sole execution authority. | Documentation scans and parent ledger |
| OS-00-03 | Native sandbox, run-profile, scenario, receiver, governed, trusted-native-fixture, runtime normalization/correlation, and pack-producer paths were removed. | Package/import inventory in validation evidence |
| OS-00-04 | Retained commands, historical reports, fixtures, all Go modules, race/vet, cross-build, testbed conformance, scope, and whitespace gates passed. | Validation evidence command results |

## Exit state

The CLI cannot execute a project in Phase 0. Passive inspection remains local
and non-executing. Historical packs remain verifiable and render byte-identical
reports. OpenShell execution, backend crawling, new evidence normalization,
security analysis, and OpenBox-specific recommendations remain owned by later
phases and are not implied by this completion.
