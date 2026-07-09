# OpenBox Shift-Left — Local End-to-End Runbook

Bring up the **entire** OpenBox stack locally and run shift-left developer-runtime
governance end to end: register a developer agent with a real backend, emit real
Claude Code session events + git commit→deploy lineage, and observe them stored
in real openbox-core. Every step here was executed and verified on Linux.

> **Phase-1 posture:** observe-only, **metadata-only** (no prompt/command/file
> content leaves the machine), fail-open (never blocks a tool call or a commit).

---

## 0. Architecture at a glance

```mermaid
flowchart LR
    subgraph dev["Developer runtime (this repo)"]
        CLI["openbox CLI<br/>(front door: dev init)"]
        CC["Claude Code"]
        GIT["git commit"]
        CI["CI push / deploy"]
        H["openbox-cc-hook<br/>(SL-4 adapter)"]
        GH["openbox-git-hook<br/>(SL-5 trailer)"]
        GA["openbox-git-action<br/>(SL-6 deploy)"]
        CC -- hooks --> H
        GIT -- prepare-commit-msg --> GH
        CI --> GA
        GH -. "stamps trailer,<br/>resolved by SL-6" .-> GA
    end

    subgraph plane["OpenBox planes (sibling repos)"]
        BE["openbox-backend :3000<br/>NestJS control plane<br/>register agent → obx_ key + DID"]
        CORE["openbox-core :8086 + governance-worker<br/>Go data plane<br/>ingest → Temporal workflow → store"]
        BE <-- "shared Postgres" --> CORE
    end

    subgraph infra["Infra (Docker)"]
        PG[("Postgres :5432")]
        RD[("Redis :6379")]
        KC["Keycloak :8080"]
        TP["Temporal :7233"]
    end

    CLI -- "POST /agent/create<br/>(obx_key_ token)" --> BE
    H -- "AIP-signed POST /evaluate" --> CORE
    GA -- "AIP-signed POST /evaluate" --> CORE
    BE --- PG & RD & KC
    CORE --- PG & RD & TP
```

**Shift-left binaries (this repo):** `openbox` (CLI / front door) · `openbox-cc-hook`
(SL-4 adapter) · `openbox-git-hook` (SL-5 trailer) · `openbox-git-action` (SL-6 deploy).

---

## 1. Prerequisites

| Need | Why | Check |
|---|---|---|
| Linux or macOS | dev/test target | — |
| **Docker** + Compose v2, daemon running | Postgres/Redis/Keycloak/Temporal | `docker info` |
| **Go ≥ 1.23** | build CLI/adapters/core | `go version` |
| **Node ≥ 20 + Yarn** | run openbox-backend | `node -v && yarn -v` |
| **git ≥ 2.24** | SL-6 uses `--end-of-options` | `git --version` |
| **python3** | verify/observe helpers | `python3 -V` |
| Sibling repos checked out next to this one | backend + core | `ls ../openbox-backend ../openbox-core` |
| Free ports | services | 3000, 5432, 6379, 8080, 7233, 8233, 8086 |

> **Sibling repos.** This runbook drives `../openbox-backend` and `../openbox-core`.
> Two small **local-dev enablement patches** are required (see [Appendix A](#appendix-a--sibling-repo-patches)):
> a backend local Ed25519 identity provider (`KMS_PROVIDER=local`) and the
> openbox-core EXT-core accept-list edit. Apply those first.

---

## 2. Install & run each service

Run these in separate terminals (or background them). Order matters: infra →
backend (owns the schema) → Temporal → core.

### 2.1 Infra — Postgres, Redis, Keycloak

Provided by the backend's compose file.

```sh
cd ../openbox-backend
docker compose up -d
docker compose ps                       # postgres:5432, redis:6379, keycloak:8080
docker compose exec -T postgres pg_isready -U postgres
```
Postgres: user `postgres` / pass `password` / db `openbox`.

### 2.2 openbox-backend (control plane, :3000)

Apply [Appendix A.1](#a1-backend--local-identity-provider) first, then:

```sh
cd ../openbox-backend
cp .env.example .env
# Edit .env — minimum for a local API-key-path boot:
#   NODE_ENV=development
#   PORT=3000
#   SUPABASE_DB_URI=postgresql://postgres:password@localhost:5432/openbox
#   REDIS_URL=redis://localhost:6379
#   KMS_PROVIDER=local                     # uses the local identity provider (A.1)
#   AWS_REGION=us-east-1  S3_REGION=us-east-1  KMS_REGION=us-east-1
#   S3_BUCKET_NAME=openbox-local           # any non-empty value (S3Service needs a region+bucket to construct)
#   CSRF_SECRET=<any 32+ chars>
yarn install
yarn migration:run                        # creates the shared schema (agents, governance_events, sessions, …)
yarn start:dev                            # → http://localhost:3000  ("Nest application successfully started")
```

**Mint a control token + org** (direct api-key insert — no Keycloak needed):

```sh
TOKEN="obx_key_$(openssl rand -hex 24)"
HASH=$(printf %s "$TOKEN" | sha256sum | cut -d' ' -f1)
docker compose exec -T postgres psql -U postgres -d openbox -v ON_ERROR_STOP=1 -c \
"INSERT INTO api_keys (organization_id,name,key_hash,key_prefix,permissions,is_active,created_at,updated_at)
 VALUES ('localdev','dev-cli','$HASH',left('$TOKEN',12),ARRAY['create:agent','read:agent'],true,now(),now());"
echo "$TOKEN"      # ← this is OPENBOX_CONTROL_TOKEN; org = localdev
```

### 2.3 Temporal (dev server, :7233) — via Docker

`/evaluate` runs a Temporal workflow synchronously, so Temporal **and** core's
`governance-worker` must be up.

```sh
docker run -d --name obx-temporal -p 7233:7233 -p 8233:8233 \
  temporalio/admin-tools:latest \
  temporal server start-dev --ip 0.0.0.0 --log-level warn
# UI at http://localhost:8233
```

### 2.4 openbox-core (data plane, :8086 + worker)

Apply [Appendix A.2](#a2-openbox-core--ext-core-accept-list) first. Core's `.env`
uses DB user `openbox/openbox`; create that role so core runs unmodified:

```sh
docker compose -f ../openbox-backend/docker-compose.yml exec -T postgres \
  psql -U postgres -d openbox -c \
  "DO \$\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='openbox')
   THEN CREATE ROLE openbox LOGIN PASSWORD 'openbox' SUPERUSER; END IF; END \$\$;"

cd ../openbox-core
go run ./cmd/core governance-worker &      # MANDATORY — executes the governance workflow
go run ./cmd/core server --addr 0.0.0.0:8086 &
# core auto-loads ./.env (DB=localhost:5432/openbox, REDIS, TEMPORAL_HOST=localhost:7233,
# MODE=debug, KMS_PROVIDER=local). "ListenAndServe(debug): 0.0.0.0:8086" = ready.
```

> The worker logs `agent identity verifier: AWS_REGION not configured` — expected
> and harmless locally (no KMS verifier; see §4 signing note).

---

## 3. Install & run shift-left (end to end)

### 3.1 Build the binaries

```sh
cd <this repo>
BIN="$PWD/bin"; mkdir -p "$BIN"
( cd cli                        && go build -o "$BIN/openbox"            ./cmd/openbox )
( cd adapters/claude-code       && go build -o "$BIN/openbox-cc-hook"    ./cmd/openbox-cc-hook )
( cd adapters/common/git        && go build -o "$BIN/openbox-git-hook"   ./cmd/openbox-git-hook )
( cd actions/openbox-git-action && go build -o "$BIN/openbox-git-action" ./cmd/openbox-git-action )
```

### 3.2 Onboard — the real `openbox dev init`

`dev init` registers the agent, stores its credentials, **and** (SL4-WIRE-1)
auto-installs the Claude Code plugin bundle (`~/.claude/plugins/openbox-observe/`)
+ writes the non-secret dev config (`~/.config/openbox/dev.json`).

```sh
export OPENBOX_BACKEND_URL="http://localhost:3000"
export OPENBOX_CONTROL_TOKEN="<the obx_key_ token from 2.2>"
export OPENBOX_ORG="localdev"

# Machines WITH an OS keyring (secret-tool / macOS Keychain):
"$BIN/openbox" dev init --provider claude-code --org localdev

# Machines WITHOUT a keyring (headless / container / WSL) — opt into the 0600 file backend:
export OPENBOX_SECRET_FILE="$HOME/.config/openbox/secrets.json"
"$BIN/openbox" dev init --provider claude-code --org localdev --secret-backend file
```

`--dry-run` prints the full plan and writes nothing. Add `--install-git-hook` to
enable ambient SL-5 commit-trailer install on session start (off by default).

**Local signing note (one-time):** core verifies AIP signatures via KMS, which a
locally-minted agent has no key in. Set the dev agent to skip verification (the
client still really signs):

```sh
DID=$(python3 -c "import json;print(json.load(open('$HOME/.config/openbox/dev.json'))['developer_did'])")
docker compose -f ../openbox-backend/docker-compose.yml exec -T postgres \
  psql -U postgres -d openbox -c "UPDATE agents SET signing_required=false WHERE did='$DID';"
```

### 3.3 Activate the Claude Code hooks

`dev init` materializes the plugin bundle and `dev.json`, but the plugin's hook
commands resolve `${CLAUDE_PLUGIN_ROOT}/bin/openbox-cc-hook` — the engine binary
is placed into the bundle at package time. For a **from-source local run**, put
the built binaries where the hooks expect them and point core at localhost:

```sh
cp "$BIN/openbox-cc-hook" "$BIN/openbox-git-hook" "$HOME/.claude/plugins/openbox-observe/bin/" 2>/dev/null \
  || mkdir -p "$HOME/.claude/plugins/openbox-observe/bin" && cp "$BIN/openbox-cc-hook" "$BIN/openbox-git-hook" "$HOME/.claude/plugins/openbox-observe/bin/"
# Point the dev config at the local core (dev init defaults base_url to prod):
python3 - <<PY
import json,os
p=os.path.expanduser("~/.config/openbox/dev.json"); c=json.load(open(p))
c["base_url"]="http://localhost:8086"
json.dump(c,open(p,"w"),indent=2)
PY
```

Then make your **live** Claude Code fire them. Simplest verified method — add the
five hooks to `~/.claude/settings.json` (they read everything from `dev.json`, no
env needed). Use `/hooks` or the `update-config` skill, or merge this block:

```jsonc
{ "hooks": {
  "SessionStart":     [{ "matcher": "",  "hooks": [{ "type":"command", "command":"<HOME>/.openbox/bin/openbox-cc-hook SessionStart",     "timeout":5  }]}],
  "UserPromptSubmit": [{ "matcher": "",  "hooks": [{ "type":"command", "command":"<HOME>/.openbox/bin/openbox-cc-hook UserPromptSubmit", "timeout":5  }]}],
  "PreToolUse":       [{ "matcher": "*", "hooks": [{ "type":"command", "command":"<HOME>/.openbox/bin/openbox-cc-hook PreToolUse",       "timeout":5  }]}],
  "PostToolUse":      [{ "matcher": "*", "hooks": [{ "type":"command", "command":"<HOME>/.openbox/bin/openbox-cc-hook PostToolUse",      "timeout":5  }]}],
  "SessionEnd":       [{ "matcher": "",  "hooks": [{ "type":"command", "command":"<HOME>/.openbox/bin/openbox-cc-hook SessionEnd",       "timeout":15 }]}]
}}
```

> ⚠️ **Hooks load at session start** — start a **new** Claude Code session for
> them to take effect. `SessionStart`/`PreToolUse`/`PostToolUse` only spool (no
> network on the hot path); `SessionEnd` flushes the session's spool to core.
> To disable later: `/hooks`, or remove the block.

### 3.4 Git lineage (SL-5 → SL-6)

```sh
# Per-repo trailer hook (or use `dev init --install-git-hook` for ambient install):
"$BIN/openbox-git-hook" install <repo>/.git/hooks

# In a session, a commit is auto-stamped `OpenBox-Session: <id>`; then at push/deploy:
export OPENBOX_BASE_URL=http://localhost:8086
export OPENBOX_DID=$DID
export OPENBOX_API_KEY=$(python3 -c "import json;print(json.load(open('$HOME/.config/openbox/secrets.json'))['ai.openbox.dev']['localdev/claude-code/api_key'])")
export OPENBOX_SEED=$(python3 -c "import json;print(json.load(open('$HOME/.config/openbox/secrets.json'))['ai.openbox.dev']['localdev/claude-code/private_key'])")
"$BIN/openbox-git-action" --sha "$GITHUB_SHA" --repo "$GITHUB_REPOSITORY" --environment production
# --base <sha> resolves a range; --dry-run prints the Deploy event without emitting.
```

---

## 4. How to verify the run

### 4.1 Services healthy

```sh
curl -sf http://localhost:3000/api >/dev/null && echo "backend up"
(echo > /dev/tcp/127.0.0.1/7233) 2>/dev/null && echo "temporal up"
ss -ltn | grep -q ':8086' && echo "core up"
```

### 4.2 `dev init` registered a real agent

```sh
docker compose -f ../openbox-backend/docker-compose.yml exec -T postgres \
  psql -U postgres -d openbox -c \
  "SELECT agent_type, did, left(token,9)||'…' AS token, signing_required, organization_id
   FROM agents WHERE did='$DID';"
# expect: developer | did:aip:… | a1e7…(sha256 of obx_test_ key) | f | localdev
```

### 4.3 Claude Code session events reach core

Start a new Claude Code session (hooks active) and run any tool. Then:

```sh
docker compose -f ../openbox-backend/docker-compose.yml exec -T postgres \
  psql -U postgres -d openbox -c \
  "SELECT event_type, verdict, metadata->>'provider' AS provider, created_at
   FROM governance_events ORDER BY created_at DESC LIMIT 10;"
# expect SessionStarted / ToolCall / ToolResult / SessionEnded, verdict 0 (ALLOW)

docker compose -f ../openbox-backend/docker-compose.yml exec -T postgres \
  psql -U postgres -d openbox -c \
  "SELECT status, workflow_id, run_id FROM sessions ORDER BY created_at DESC LIMIT 3;"
# a session row created (pending) then → completed on SessionEnded
```

**Without waiting for a new session**, drive the exact hook command directly (this
is what Claude Code runs — a faithful smoke test):

```sh
SID="verify-$(python3 -c 'import uuid;print(uuid.uuid4())')"
C="\"session_id\":\"$SID\",\"cwd\":\"$PWD\",\"permission_mode\":\"default\""
printf "{$C,\"hook_event_name\":\"SessionStart\",\"source\":\"startup\"}" | "$HOME/.claude/plugins/openbox-observe/bin/openbox-cc-hook" SessionStart
"$HOME/.claude/plugins/openbox-observe/bin/openbox-cc-hook" flush     # SessionStart only spools; flush pushes it
# then query governance_events WHERE run_id='$SID'
```

Contract checks: the hook must **exit 0 with empty stdout** (observe-only), and no
prompt/command/file content may appear in any payload (metadata-only, INV-2).

### 4.4 Commit → deploy lineage reaches core

```sh
# after a real `git commit` (trailer stamped) + `openbox-git-action` (§3.4):
docker compose -f ../openbox-backend/docker-compose.yml exec -T postgres \
  psql -U postgres -d openbox -c \
  "SELECT event_type, metadata->>'commit_sha' AS sha, metadata->>'attribution_status' AS attr,
          metadata->>'repo' AS repo FROM governance_events WHERE event_type='Deploy' ORDER BY created_at DESC LIMIT 3;"
# expect: Deploy | <full-sha> | inferred | <repo>
```

> **Why `inferred`, not `attributed`:** Phase-1 has no session-ownership verifier
> wired (EXT-lineage/FR-7 deferred), so trailers are treated as unverified claims
> (SL5-SEC-1). This is correct.

### 4.5 Reproduce scripts

`docs/runbook/` has ready helpers used to validate this end to end:
`capture_server.py` (a loopback `/evaluate` sink for observing payloads with **no
core at all**), `dogfood.sh` (drives the CC hook path), `dogfood_git.sh` (SL-5
stamping + SL-6 resolution). Point them at `OPENBOX_BASE_URL=http://localhost:8086`
to run against real core, or at the capture sink to just see the wire payloads.

---

## 5. Teardown

```sh
# core (find the two `go run ./cmd/core …` processes) — Ctrl-C or:
pkill -f 'cmd/core (server|governance-worker)'
docker rm -f obx-temporal
cd ../openbox-backend && docker compose down          # add -v to also drop volumes/data
# stop the backend `yarn start:dev` process
# (optional) remove local hooks: delete the OpenBox block from ~/.claude/settings.json (or /hooks)
```

---

## 6. EXT-core caveat (production reality)

Stock openbox-core's `/evaluate` accept-list does **not** include the
developer-runtime event types — it 400s them. [Appendix A.2](#a2-openbox-core--ext-core-accept-list)
is the documented additive extension (architecture D4 / INV-8) that makes core
accept them. Until that ships upstream, a stock core returns HTTP 400 and the
fail-open client (INV-3) logs-and-drops it — telemetry is lost, but **no tool
call or commit is ever blocked**.

---

## Appendix A — sibling-repo patches

Both are minimal, additive, dev-only enablement. They are local working changes
in this setup (not yet upstreamed).

### A.1 backend — local identity provider

`agent/create` provisions the agent Ed25519 identity via AWS KMS
*unconditionally* (`src/modules/did/did.module.ts` binds `AwsKmsProvider`), so a
keyless machine can't register. Add a `LocalIdentityProvider` (generates the
keypair in-process, returns the raw seed base64 — same shape, no KMS) and select
it when `KMS_PROVIDER=local`:

- `src/modules/did/local-identity-provider.ts` — implements `AgentIdentityProvider`,
  reuses `generateEd25519Keypair()` from `./crypto`, returns `{ did: didFor(id), privateKey }`.
- `src/modules/did/did.module.ts` — bind `AGENT_IDENTITY_PROVIDER` via a factory:
  `KMS_PROVIDER=local` → `LocalIdentityProvider`, else `AwsKmsProvider`.

### A.2 openbox-core — EXT-core accept-list

Three additive edits so core accepts the developer-runtime event types:

1. `internal/content/governance.go` — add the constants
   `EventTypeSessionStarted/PromptSubmitted/ToolCall/ToolResult/SessionEnded/CommitCreated/Deploy`.
2. `internal/api/governance.go` — add them to the `isValidGovernanceEventType` switch.
3. `internal/services/activities/governance/storage_session.go` — map
   `SessionStarted` → create session, `SessionEnded` → terminal (rest fall through
   to session lookup).

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `HALT: no OS secret store available` | No keyring. Install `secret-tool` + a keyring daemon, **or** re-run with `--secret-backend file` (§3.2). |
| `dev init` prints manual config / exit 2 | The provider adapter isn't registered (older build). For claude-code, ensure SL4-WIRE-1 is present (`cli/internal/providers`). |
| `/evaluate` 400 `invalid event_type` | EXT-core edit not applied — see Appendix A.2. |
| `/evaluate` 500 / hangs then 500 | Temporal down or no `governance-worker` running (§2.3–2.4). |
| `/evaluate` 401 | Agent token not in `agents` table, or `signing_required=true` with no KMS verifier — set it `false` (§3.2). |
| backend crashes at boot `Region is missing` | Set `AWS_REGION`/`S3_REGION`/`KMS_REGION` + `S3_BUCKET_NAME` in `.env` (§2.2). |
| No CC events after wiring | Start a **new** Claude Code session; hooks load at startup. |
| `refusing plaintext http:// to non-loopback host` | Use `https` for a non-loopback core; only `127.0.0.1`/`localhost` may be http (INV-1). |
