# openbox-git-action

Server-side, push-time **deploy-lineage** resolver for OpenBox. At push/deploy
it binds the pushed commit to the OpenBox session(s) that produced it (reading
the `OpenBox-Session:` trailers that the the commit trailer hook wrote) and emits a
**`Deploy`** governance event, through the shared the client client, carrying the
resolved session set. It is the read/resolve counterpart to the write side in
[`internal/adapters/common/git`](../../adapters/common/git).

## What it guarantees (INV-6)

- **Resolves against the real pushed SHA**, never a pre-push SHA (git hooks are
  local; SHAs are unstable until push; spike S3 §1).
- Dedups to a session **set** (0..N); a squash/merge fans multiple sessions in.
- **Never a silent wrong attribution.** Every deploy is marked:
  - `attributed`; ≥1 session **verified** as owned by the authenticated pusher;
  - `inferred`; session id(s) recovered but not verified (a *claim*);
  - `unattributed`; none, with a `reason` (`no-trailer` | `trailer-stripped` |
    `non-agent`).
- **No silent caps:** a walk that hits `MaxCommits` records `scope_walked` /
  `scope_total`.

## Security; the trailer is an untrusted claim

Anyone who can author a commit can name **any** session id (including a
victim's, visible in their pushed commits). A raw trailer is therefore a claim,
not proof. Each resolved id is passed through an `OwnershipVerifier` that must
confirm it belongs to a session owned by the **authenticated pusher** before it
is marked `verified` (→ `attributed`); mirroring how the client cross-binds the DID.

Phase-1 default is `NoopVerifier` (verifies nothing): a well-formed deploy
resolves as `inferred` with every claim flagged `verified=false`. Enabling the
real verifier promotes owned sessions to `attributed` with no change to the
resolver.

**The real verifier.** `apiVerifier` (in `verifier.go`) reads
openbox-backend's existing, org-scoped session endpoint and promotes a claim
only when a returned session's **`run_id`** equals the trailer value (the field
a trailer value maps to; *not* the backend session `id` PK):

```
GET <backend>/agent/<agentID>/sessions?search=<sessionID>
    X-API-Key: <obx_key_… org key with read:agent_session>
200 → { "status": 200, "data": { "data": [ { "run_id": "…", "agent_id": "<agentID>", … } ] } }
```

Rows are at `data.data[]`; the backend wraps every 2xx in a global `{status,
data}` envelope around the `SessionListResponseDto` (verified live). The backend
double-scopes the result by `agentID` **and** the key's `organization_id`, so a
returned row genuinely belongs to that agent in that org. Every fault (transport
error, non-2xx, malformed body, timeout, no matching row) fails **closed**; the
claim is never promoted, so the worst case is honest under-attribution, never a
silent wrong `attributed`.

It is **OFF by default** and gated by:

| Env | Meaning |
|---|---|
| `OPENBOX_OWNERSHIP_VERIFY=1` | enable verification (default: off ⇒ `NoopVerifier`) |
| `OPENBOX_OWNERSHIP_API_URL`  | openbox-backend origin (https, or http on loopback); **bare, no path prefix** |
| `OPENBOX_AGENT_ID`           | the deploy agent's UUID (from `POST /agent/create`) |
| `OPENBOX_ORG_API_KEY`        | org `X-API-Key` (`obx_key_…`) holding `read:agent_session` |

**INV-4 binding.** There is no DID→agentId lookup; an agent's DID
is `did:aip:uuidv5(agentID, namespace)` (one-way). So CI supplies the agent's
UUID directly, and at startup the verifier recomputes `uuidv5(OPENBOX_AGENT_ID)`
and **requires it to equal `OPENBOX_DID`** (the deploy/attribution identity). A
misconfigured id that names a *different* principal is rejected → degrades to
`NoopVerifier`. Combined with the per-row `agent_id` check, the verifier can
only ever read; and attribute; the deploy principal's own sessions. A
missing/misconfigured/unreachable verifier; or `--dry-run`; degrades to
`NoopVerifier`: it **never breaks CI and never over-attributes**.

> **Security note.** `OPENBOX_ORG_API_KEY` is an org-scoped key that can read any
> agent's sessions in the org; scope the CI secret to `read:agent_session` only.
> The `uuidv5` namespace mirrors openbox-backend `src/modules/did/aip-namespace.ts`
> (verified cross-repo 2026-07-13); if backend DID derivation ever changes, the
> bind fails safe (verification disables, never over-attributes).

The emitted `metadata` carries the full qualified `sessions` array (each with
`verified`/`source`/`reason`) **and** a flat `verified_session_ids` list that
contains **only verified** ids; the collision-free shape a lineage join can bind
to without ever trusting an unverified/forged claim. Bounds: at most
`MaxSessions` (default 4096) distinct claims and a 1 MiB per-message read; a hit
is disclosed in the resolution note, never silent (SEC-6-1).

## How resolution works

1. **Scope**; a single commit resolves itself; a range resolves `base..target`;
   a merge with no base resolves `<merge> ^<merge>^1` (the reachable originals).
2. **Read**; authoritative trailing trailer block via
   `%(trailers:key=OpenBox-Session,…)` (S3 R7). ****: also
   full-body-scans for column-0 `OpenBox-Session:` lines to recover ids left
   mid-body by a squash done *before* the commit trailer's hook (marked `source: body-scan`).
3. **Trailer-stripped fallback**; if nothing is found, recover from the the commit trailer
   git-notes mirror (`refs/notes/openbox`) → `inferred` / `trailer-stripped`.
4. **Bind**; ownership-verify each id, then classify.

## Usage (CI)

```sh
openbox-git-action --sha "$GITHUB_SHA" --repo "$GITHUB_REPOSITORY" --environment production
```

| Flag | Env fallback | Meaning |
|------|--------------|---------|
| `--sha` | `GITHUB_SHA` | pushed/deployed commit (real pushed SHA) |
| `--base` | `OPENBOX_DEPLOY_BASE` | optional range base (`base..sha`) |
| `--repo` | `GITHUB_REPOSITORY` | repo slug (metadata) |
| `--environment` | `OPENBOX_DEPLOY_ENV` | deploy environment (default `production`) |
| `--dir` |; | repo working dir (default: cwd) |
| `--dry-run` |; | resolve + print the event as JSON; **do not emit** (no creds needed) |

Client identity (an OpenBox agent minted by openbox-backend `POST
/agent/create`): `OPENBOX_BASE_URL`, `OPENBOX_API_KEY` (`obx_…`), `OPENBOX_DID`
(`did:aip:<uuid>`), `OPENBOX_SEED` (base64 Ed25519 seed). Secrets ride only in
headers, never logged (INV-1).

**Exit codes:** `0` = resolved (emit success or fail-open drop, INV-3); `2` =
usage/precondition fault (bad `--sha`, missing creds) the operator must fix. It
never exits non-zero over a telemetry transport failure.

## Emission & the EXT-core dependency

The `Deploy` event and the resolved session set ride in `metadata` (the S6 §4
metadata-jsonb stopgap; no external schema needed to *write* the link; the
queryable session→commit→deploy join is FR-7, external/deferred). `deploy_did`
(`did:aip:deploy-<shortsha>-<unixts>`) is a synthetic lineage label in metadata;
the client's **signing** identity stays the agent's real `did:aip:<uuid>`.

Like the client/4/5, end-to-end ingestion is gated on **EXT-core**: openbox-core's
`/evaluate` accept-list does not yet include the developer-runtime event types,
so it currently answers `Deploy` with HTTP 400 (the documented additive core
extension; architecture D4 / INV-8). Until it lands, the fail-open client
logs-and-drops the 400; the action never breaks CI.

## Requirements

Git **≥ 2.24** (Nov 2019); the resolver passes `--end-of-options` to every git
invocation so a `-`-leading ref/SHA can never be read as a flag (SEC-6-3). On
older git that guard is absent and `verifyCommit` errors out (fail-closed,
precondition exit 2) rather than resolving unsafely.

## Test

```sh
go build./... && go test./...   # simulated push incl. squash/fixup/force-push/merge
```
