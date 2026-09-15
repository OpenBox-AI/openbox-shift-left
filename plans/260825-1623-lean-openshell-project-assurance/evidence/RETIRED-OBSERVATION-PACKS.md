# Retired: the four 2026-08-26 observation packs

`2026-08-26-phase-02-public-mastra-observation-0{1,2,3}` and
`2026-08-26-phase-02-public-mastra-dashboard-observation-04` were deleted when
the evaluation lane moved from driving OpenShell directly to running through
the OpenBox Sandbox service.

## Why they were deleted rather than kept

They were already partly unreadable. Three of the four carried a flat
`backend_url` string in `run.json`, while the schema under the same
`…run/v1` identifier required a `backend` object and forbade `backend_url` — a
required property had been added and another removed inside a version, so
`openbox project verify` could not read its own sealed evidence.

The sandbox cutover replaced the isolation evidence itself: `openshell.jsonl`,
which was gateway log text wrapped one line per JSON object, became
`sandbox-evidence.json`, the provider's typed record. There is one observation
version, because there is one evidence source.

Keeping the old packs would have meant two different shapes claiming
`ai.openbox.project-observation/v1` at once — the same defect that made them
unreadable, rebuilt on purpose. Deleting them is what makes reusing `/v1` safe.

## What was lost

Real evidence from four Mastra evaluation runs on 2026-08-26. The claims those
runs supported — OS-02-02 and the phase-02 verification record — are no longer
backed by retained artifacts and are marked accordingly where they are cited.
Their backend half survives in
`cli/internal/assurance/testdata/observation-v2/`, which was rebuilt from
`…dashboard-observation-04`'s backend responses; see the fixture's own README
for what is real there and what is synthetic.

Re-establishing these claims needs a fresh run through the sandbox lane.
