# Phase 2 dashboard API reuse verification — 2026-08-26

## Implemented boundary

- Phase 2 calls the existing dashboard session-list, session-detail, and
  chronological-log APIs. It adds no backend endpoint and performs no control
  crawl, database read, or backend build-tuple check.
- Preflight uses `/health`, `/auth/profile`, and a zero-result dashboard session
  search for the fresh evaluation ID.
- Session/log bodies are canonical public dashboard projections. The collector
  removes only internal ORM `agent` relations before hashing and retention and
  rejects any remaining credential-shaped field or value.
- The existing shared organization key contract is unchanged. The runtime
  `OPENBOX_API_KEY` remains confined to the OpenShell provider path.
- The backend source worktree is clean; the implementation is entirely in
  Shift Left plus the existing local-stack shared-key reconciliation.

## Corrected live pack

The public evaluator sealed
`2026-08-26-phase-02-public-mastra-dashboard-observation-04` from
`ai.openbox/mastra-conformance:local` at image ID
`sha256:e13c1e89b1daa897f319402d1b0642c329f471c663665142e05aa3fc6f110dc6`.

- Evaluation: `ev-656213e0d3ddc7400f324769`
- Agent: `450999ca-ae2a-409c-8a26-d00a71132440`
- Session: `4a794d05-076c-4dc6-add4-0615f1af6e80`
- Backend source contract: `dashboard-session-activity/v1`
- Requests: nine GETs — health, profile, zero-result preflight search, terminal
  search, detail, chronological page, and the three stability rechecks.
- Persisted activities: six backend events, all source-referenced.
- Output: exact six payload files plus manifest last; root mode `0500`, files
  mode `0400`.
- Cleanup: sandbox, registry tag/container/volume, and loaded Ollama model are
  absent.

The immutable reader and semantic validator accepted the pack with an absolute
path. A first follow-up command used a relative path and was correctly refused
by the runfs absolute-root invariant; the corrected command passed.

## Deterministic verification

- Focused observation, evaluator, and CLI tests passed.
- All 11 Go workspace modules passed `go test ./...`.
- Race-enabled observation, evaluator, and CLI tests passed.
- `go vet ./...` passed in all 11 modules.
- CLI cross-builds passed for Linux arm64, Darwin arm64, and Windows amd64.
- Every observation schema parsed, schema-backed pack tests passed, local-stack
  bootstrap shell syntax passed, and `git diff --check` passed in all three
  inspected repositories.
