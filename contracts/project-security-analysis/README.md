# Project security-analysis candidate contracts

`candidate.schema.json` defines the closed, untrusted Phase 3 issue-candidate
shape. It is not a public candidate-validation command and does not grant the
candidate report authority. Phase 4 must independently reverify the observation
and candidate.

`standards.json` is the frozen `2026-08-26-mvp1` standards selection. Its
source digests bind the exact local source snapshots under `sources/`.
Standards can describe an evidence-supported issue; they never establish that
behavior was observed and never establish severity.
