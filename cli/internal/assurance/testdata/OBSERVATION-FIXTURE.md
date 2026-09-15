# `ai.openbox.project-observation/v1` fixture

A sealed observation pack in `observation/`, with the analyzer candidates that
cite it in `observation-candidates/`. The `securityreport` and `cmd/openbox`
suites read these so they exercise the current contract against a pack that was
actually assembled and validated, rather than one hand-written to pass.

## Provenance

`backend.json` is real. It is the backend evidence from the 2026-08-26 Mastra
run `…dashboard-observation-04`, which was deleted when the lane moved to the
sandbox service — see
`plans/260825-1623-lean-openshell-project-assurance/evidence/RETIRED-OBSERVATION-PACKS.md`.
The backend half of the contract did not change in that migration, so reusing
those responses keeps the fixture grounded in traffic that happened.

`sandbox-evidence.json` is synthetic: an empty egress record. That run predates
the sandbox service and produced no typed evidence, and inventing egress
decisions that were never observed would be worse than recording none.
`observed: false` means the provider recorded nothing — never that nothing
happened.

`run.json`, `effects.json`, `behavior.json` and `coverage.json` are derived by
`observation.Assemble` from those two inputs, so the pack reconstructs and
verifies exactly as a freshly sealed one does.

## Regenerating

Assemble a pack from a real run and copy it here, then update the pinned digest
in `securityreport_test.go` and `project_verify_unix_test.go` and the
`pack_digest` each candidate cites.

Do not hand-edit. The manifest digest covers the canonical bytes of every
payload, and `Validate` reconstructs `behavior.json` and `coverage.json` from
the others — an edit that looks local will fail somewhere else.
