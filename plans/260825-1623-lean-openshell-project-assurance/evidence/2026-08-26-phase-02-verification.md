# Phase 2 implementation verification — 2026-08-26

## Public success evidence

The public command sealed
[`2026-08-26-phase-02-public-mastra-observation-03`](2026-08-26-phase-02-public-mastra-observation-03)
from `ai.openbox/mastra-conformance:local` at immutable local image ID
`sha256:e13c1e89b1daa897f319402d1b0642c329f471c663665142e05aa3fc6f110dc6`.

- Evaluation: `ev-5ad0632a9a7dd16266905fca`
- Agent: `450999ca-ae2a-409c-8a26-d00a71132440`
- Backend session: `dadd238c-61e8-4ef4-8d05-b953a9cde1c7`
- Backend capture: 13 ordered GET responses: seven preflight/control
  responses and six exact-session/log stability responses.
- Behavior: six ordered backend events plus one source-referenced OpenShell
  Granite model invocation and one independent evaluation-ID-bound safe effect.
- Coverage: retrieval/poison is explicitly `missing`; unsigned bearer request
  attribution is explicitly `unsupported`; neither is inferred.
- Cleanup: sandbox, registry tag/container/volume, and loaded model are absent.
- Pack: exact six payloads plus manifest last, root mode `0500`, file modes
  `0400`, with no control/runtime credential-shaped material.

The immutable reader, semantic source-reference validator, and all seven
separate observation contract schemas accepted this retained pack through:

```text
OPENBOX_LIVE_OBSERVATION=<pack-path> \
  go test ./internal/assurance/observation \
  -run TestLiveObservationEvidenceWhenRequested -count=1 -v
```

## Public refusal evidence

[`2026-08-26-phase-02-preflight-refusal-02`](2026-08-26-phase-02-preflight-refusal-02)
records a missing `OPENBOX_CONTROL_TOKEN` refusal. It is `not_runnable`, has
exactly the Phase 1 diagnostic shape, contains no manifest or observation
payload, reports zero safe-sink attempts, and started no sandbox or registry.

## Verification commands

- Every Go module: `go test ./...` from each of the 11 workspace modules.
- Race: `go test -race ./internal/assurance/... ./cmd/openbox`.
- Static checks: `go vet ./...` in `cli`.
- Compile: `go build ./...` for `linux/arm64`, `darwin/arm64`, and
  `windows/amd64`.
- Mastra image: `npm run check`, `npm run typecheck`, and pinned Docker build.
- Hygiene: every observation schema parsed as JSON and `git diff --check`
  passed.

## Post-MVP backlog

Phase 2 and the MVP Mastra conformance lane are verified. Real-project diversity
qualification using an owner-selected SDK-integrated image is retained as
post-MVP backlog and is not an MVP acceptance or release gate.
