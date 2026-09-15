# Project observation contract

`ai.openbox.project-observation/v1` is a separate, sensitive local observation
pack. It does not extend or migrate `openbox.audit-pack/v1`.

The sealed root contains `run.json`, `backend.json`, `openshell.jsonl`,
`effects.json`, `behavior.json`, `coverage.json`, and manifest-last
`manifest.json`. All JSON is RFC 8785 canonical JSON; every JSONL record is
canonical JSON followed by one LF.

`run.json.backend` binds the pack to the exact local backend URL and the closed
`dashboard-session-activity/v1` read contract. `backend.json` contains the
ordered health, auth-profile, session-list, session-detail, and chronological
log reads used by the dashboard. Session and log bodies are retained as a
canonical public projection that removes only internal ORM `agent` relations;
the unprojected response is never hashed or retained. Every retained OpenShell
record has a stable digest-bound entry in `behavior.json`, and every coverage
channel has a stable `coverage:<name>` identity. Validation reconstructs the
indexes and rejects credential material before accepting an observation.
