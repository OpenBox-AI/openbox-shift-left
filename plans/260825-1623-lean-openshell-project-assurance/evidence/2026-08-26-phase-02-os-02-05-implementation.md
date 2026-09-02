# Phase 2 OS-02-05 implementation evidence — 2026-08-26

> Superseded by
> `2026-08-26-phase-02-dashboard-api-reuse-verification.md`. The dedicated
> observation endpoint, backend serialization changes, and build-tuple gate
> documented below were removed from the implementation.

## Implemented boundary

- `openbox-backend` exposes the `read:agent`-guarded, three-field
  `GET /agent/{agentId}/observation-profile` response.
- The broad agent read omits its stored token verifier, and session/search/log
  responses remove embedded agent relations before serialization.
- The local-stack backend image bakes its package version and source identity;
  a dirty checkout is deliberately identified with a non-qualifying `-dirty`
  suffix.
- Shift Left validates every backend body for raw or derived credential
  material before calculating a digest or appending a capture entry. It binds
  the exact backend URL/version/commit, emits a complete current-control index,
  assigns stable identities to every OpenShell and coverage record, and
  reconstructs all three indexes during immutable validation.
- The six observation payload paths and historical audit-pack implementation
  remain unchanged.

## Deterministic verification

- Backend builder image: Nest build passed.
- Backend focused Jest suites: 3 suites and 248 tests passed.
- Backend changed-file Prettier and ESLint checks passed.
- Shift Left: `go test ./...` passed in all 11 workspace modules.
- Shift Left assurance and CLI race tests passed.
- `go vet ./...` passed in `cli`.
- `go build ./...` passed for `linux/arm64`, `darwin/arm64`, and
  `windows/amd64`.
- Observation schema compilation and semantic pack reconstruction tests passed.
- Mastra conformance `check`, `typecheck`, pinned Docker build, and OCI
  `image:test` passed at image ID
  `sha256:e13c1e89b1daa897f319402d1b0642c329f471c663665142e05aa3fc6f110dc6`.

## Live status

The rebuilt local backend truthfully reports version `0.0.1` and the checked-out
backend commit with a `-dirty` suffix because the OS-02-05 backend changes are
not committed. Shift Left therefore rejects this tuple before stimulus, as
required. No existing `OPENBOX_CONTROL_TOKEN` was available in the execution
environment, and this work had no authority to create one.

Consequently OS-02-05 is `implemented`, not `verified`: no public Mastra
observation pack was generated or claimed. Live qualification requires the
owner to commit the backend change, rebuild the backend image so `/version`
contains the clean 40-character commit, supply the existing exact-scope
organization credential, and rerun the public evaluator. The retained
`2026-08-26-phase-02-public-mastra-observation-03` pack remains superseded
collection evidence and is not a Phase 3 input.
