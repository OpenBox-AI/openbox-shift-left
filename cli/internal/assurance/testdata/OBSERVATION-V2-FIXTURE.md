# `ai.openbox.project-observation/v2` fixture

A sealed v2 observation pack, used by the `securityreport` and `cmd/openbox`
suites so they exercise the current contract rather than a retired one.

## Provenance

`backend.json` is the real backend evidence from the committed v1 run
`2026-08-26-phase-02-public-mastra-dashboard-observation-04`. The backend half
of the contract did not change in the migration, so reusing it keeps the
fixture grounded in traffic that actually happened.

`sandbox-evidence.json` is synthetic — an empty egress record. The v1 run
predates the sandbox service and produced no typed evidence, and inventing
egress decisions that were never observed would be worse than recording none:
`observed: false` means the provider recorded nothing, never that nothing
happened.

`run.json`, `effects.json`, `behavior.json` and `coverage.json` are derived by
`observation.Assemble` from those two inputs, so the pack reconstructs and
verifies exactly as a freshly sealed one does.

## Regenerating

Assemble a pack from a real run and copy it here. Do not hand-edit: the
manifest digest covers the canonical bytes of every payload, and `Validate`
reconstructs `behavior.json` and `coverage.json` from the others.
