# Lineage: session → commit → deploy

Lineage answers: *for this commit or deploy, which coding session produced it,
with which agent, and how sure are we?*

## How the chain is built

```mermaid
flowchart LR
  S["coding session<br/>(events, sealed at end)"]
  C(["commit"])
  D["Deploy event<br/>run_id = deploy-&lt;env&gt;-&lt;sha&gt;"]
  L[("deploy_session_links")]
  S -- "commit hook stamps<br/>OpenBox-Session: &lt;session&gt;" --> C
  C -- "openbox-git-action reads the trailer" --> D
  D --> L
  L --> S
```

Three producers, all in this repo:

1. **The session** sends its events as it runs. When it ends, the platform
   seals it with a signed Merkle root.
2. **The commit hook** (`prepare-commit-msg`, installed by `openbox init`)
   adds an `OpenBox-Session: <id>` trailer to each commit made during a
   governed session, and mirrors it into `refs/notes/openbox`.
3. **The deploy action** (`openbox-git-action`, in `cmd/openbox-git-action/`)
   runs in CI. It resolves the pushed commits back to sessions and sends one
   Deploy event per environment. It is idempotent on `deploy-<env>-<sha>`, so
   re-running a pipeline does not duplicate it.

The platform then fills `deploy_session_links`, which makes the chain
queryable.

A deploy is linked to its session by reference, never added to it: by deploy
time the session is sealed, and a sealed session rejects new events. That
rejection is the tamper-evidence guarantee.

## How sure are we

| State | Means |
|---|---|
| `unattributed` | no trailer: a human commit, or a tool with no adapter. Shown as a gap, never a guess |
| `inferred` | the trailer names the session that was live. A trailer can be hand-written, so this is a claim |
| `verified` | the platform confirmed ownership **and** accepted a signed attestation |

**Today, `verified` cannot be reached by a `keycloak_workload` agent.** Commit
attestations are Ed25519 signatures, and a workload agent holds only an RSA
key. The commit hook skips signing rather than fake a signature: the trailer
is still stamped, so `inferred` works, but no `refs/notes/openbox-attest` note
is written.

## Join keys

| Link | Key |
|---|---|
| session | `workflow_id` = the agent's derived attribution id, `run_id` = the tool's session id |
| commit → session | the `OpenBox-Session` trailer |
| deploy → commit → session | `deploy_session_links (deploy_id, commit_sha, session_run_id, session_id, verified, source)` |
| deploy | `run_id = deploy-<env>-<sha>` |

A push containing commits from several sessions gets one link row per session.

## Reading it back

The platform exposes the chain from any starting point:
`GET /lineage/deploys`, `/lineage/commits/:sha` and
`/lineage/sessions/:id/chain`. The dashboard renders each hop with its
evidence. A deployed commit with no authoring session has no chain: the
commit view returns 404.

## Try it

```bash
# in a governed session, commit as usual, then:
git log -1 --format='%(trailers:key=OpenBox-Session,valueonly)'

# in CI, after the push
openbox-git-action --sha "$GITHUB_SHA" --repo "$GITHUB_REPOSITORY" --environment production
```

Add `--dry-run` to print the resolved event without sending it. Use `--base`
to resolve a specific commit range (`base..sha`).
