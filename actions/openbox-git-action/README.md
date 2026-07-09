# openbox-git-action (STORY-SL-6)

Server-side, push-time **deploy-lineage** resolver for OpenBox. At push/deploy it
binds the pushed commit to the OpenBox session(s) that produced it (reading the
`OpenBox-Session:` trailers that the SL-5 hook wrote) and emits a **`Deploy`**
governance event, through the shared SL-3 client, carrying the resolved session
set. It is the read/resolve counterpart to the write side in
[`adapters/common/git`](../../adapters/common/git) (SL-5).

## What it guarantees (INV-6)

- **Resolves against the REAL pushed SHA**, never a pre-push SHA (git hooks are
  local; SHAs are unstable until push — spike S3 §1).
- Dedups to a session **set** (0..N); a squash/merge fans multiple sessions in.
- **Never a silent wrong attribution.** Every deploy is marked:
  - `attributed` — ≥1 session **verified** as owned by the authenticated pusher;
  - `inferred` — session id(s) recovered but not verified (a *claim*);
  - `unattributed` — none, with a `reason` (`no-trailer` | `trailer-stripped` |
    `non-agent`).
- **No silent caps:** a walk that hits `MaxCommits` records `scope_walked` /
  `scope_total`.

## Security — the trailer is an untrusted claim (SL5-SEC-1)

Anyone who can author a commit can name **any** session id (including a victim's,
visible in their pushed commits). A raw trailer is therefore a claim, not proof.
Each resolved id is passed through an `OwnershipVerifier` that must confirm it
belongs to a session owned by the **authenticated pusher** before it is marked
`verified` (→ `attributed`) — mirroring how SL-3 cross-binds the DID.

Phase-1 default is `NoopVerifier` (verifies nothing) because the
session-ownership read API is external/deferred (EXT-lineage / FR-7): a
well-formed deploy resolves as `inferred` with every claim flagged
`verified=false`. Wiring a real verifier promotes owned sessions to `attributed`
with no change to this package.

The emitted `metadata` carries the full qualified `sessions` array (each with
`verified`/`source`/`reason`) **and** a flat `verified_session_ids` list that
contains **only verified** ids — the collision-free shape a future FR-7 lineage
JOIN can bind to without ever trusting an unverified/forged claim (empty in
Phase-1). Bounds: at most `MaxSessions` (default 4096) distinct claims and a
1 MiB per-message read; a hit is disclosed in the resolution note, never silent
(SEC-6-1).

## How resolution works

1. **Scope** — a single commit resolves itself; a range resolves `base..target`;
   a merge with no base resolves `<merge> ^<merge>^1` (the reachable originals).
2. **Read** — authoritative trailing trailer block via
   `%(trailers:key=OpenBox-Session,…)` (S3 R7). **SL6-SCAN**: also full-body-scans
   for column-0 `OpenBox-Session:` lines to recover ids left mid-body by a squash
   done *before* SL-5's hook (marked `source: body-scan`).
3. **Trailer-stripped fallback** — if nothing is found, recover from the SL-5
   git-notes mirror (`refs/notes/openbox`) → `inferred` / `trailer-stripped`.
4. **Bind** — ownership-verify each id (SL5-SEC-1), then classify.

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
| `--dir` | — | repo working dir (default: cwd) |
| `--dry-run` | — | resolve + print the event as JSON; **do not emit** (no creds needed) |

Client identity (an OpenBox agent minted by openbox-backend `POST /agent/create`):
`OPENBOX_BASE_URL`, `OPENBOX_API_KEY` (`obx_…`), `OPENBOX_DID` (`did:aip:<uuid>`),
`OPENBOX_SEED` (base64 Ed25519 seed). Secrets ride only in headers, never logged
(INV-1).

**Exit codes:** `0` = resolved (emit success or fail-open drop, INV-3);
`2` = usage/precondition fault (bad `--sha`, missing creds) the operator must fix.
It never exits non-zero over a telemetry transport failure.

## Emission & the EXT-core dependency

The `Deploy` event and the resolved session set ride in `metadata` (the S6 §4
metadata-JSONB stopgap — no external schema needed to *write* the link; the
queryable session→commit→deploy JOIN is FR-7, external/deferred). `deploy_did`
(`did:aip:deploy-<shortsha>-<unixts>`) is a synthetic lineage label in metadata;
the client's **signing** identity stays the agent's real `did:aip:<uuid>`.

Like SL-3/4/5, end-to-end ingestion is gated on **EXT-core**: openbox-core's
`/evaluate` accept-list does not yet include the developer-runtime event types,
so it currently answers `Deploy` with HTTP 400 (the documented additive core
extension — architecture D4 / INV-8). Until it lands, the fail-open client
logs-and-drops the 400; the action never breaks CI.

## Requirements

Git **≥ 2.24** (Nov 2019) — the resolver passes `--end-of-options` to every git
invocation so a `-`-leading ref/SHA can never be read as a flag (SEC-6-3). On
older git that guard is absent and `verifyCommit` errors out (fail-closed,
precondition exit 2) rather than resolving unsafely.

## Test

```sh
go build ./... && go test ./...   # simulated push incl. squash/fixup/force-push/merge
```
