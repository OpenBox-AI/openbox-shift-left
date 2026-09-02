# Phase 3 deterministic verification

**Date:** 2026-08-27

## Implemented scope

- CLI observation/audit manifest dispatch and exact public observation output
- seven embedded observation schema copies with contract byte-parity tests
- issue-only candidate and fixed standards catalog contracts
- canonical `openbox-security-evaluation` `1.0.0` bundle
- shared Claude/Codex managed-directory transaction and Cursor manual result
- no-overwrite hard-link publisher
- internal-only candidate/citation qualification oracle and test-only variant
  generator
- Phase 3 eval prompts, installed-host review, and issue-only Phase 4 handoff

No backend, Core, OpenShell provider, evaluator image, credential format, or
control-plane implementation was changed for Phase 3. The repository was
already dirty with the preceding phases; unrelated changes were preserved.

## Public verifier

The corrected Phase 2 pack returned exactly:

```text
project observation verified: ai.openbox.project-observation/v1
  pack_digest: sha256:2e724ab506e2eeea2c40b873fa05135940f0d6ad0fb0bf82609e7f2dca73fe25
```

Focused verifier, observation, runfs, installer, bundle, publisher, schema, and
oracle tests passed. The generated captured-instruction and
missing-external-authority packs passed public verification; the unreconciled
tampered pack failed with `observation: manifest payload mismatch`.

## Repository checks

All 11 `go.work` modules passed:

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `CGO_ENABLED=0 go build ./...` for `darwin/amd64`, `linux/amd64`, and
  `windows/amd64`

Every JSON document under the observation contracts/runtime copies and security
analysis contracts/bundle parsed with `jq empty`. `git diff --check` passed.

## Installed-host qualification

See `2026-08-27-phase-03-skill-evaluation-review.md` for host/model versions,
timing, outputs, oracle grades, unfavorable baselines, and the human-review
checklist. OS-03-04 remains `in_progress` only for that explicit human
inspection gate; Phase 4 remains unimplemented.
