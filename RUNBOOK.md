# OpenBox Shift-Left — Local End-to-End Runbook

Bring up the **entire** OpenBox stack locally and run shift-left developer-runtime
governance end to end: register a developer agent with a real backend, emit real
Claude Code session events + git commit→deploy lineage, observe them stored in
real openbox-core, and **view them in the openbox-fe developer dashboard**. Every
step here was executed and verified on Linux.

> **This is the contributor runbook** for standing up the whole stack from source
> against a local (or hybrid) backend. If you just want to govern your own Claude
> Code sessions against an existing OpenBox, use **[QUICKSTART.md](QUICKSTART.md)**
> instead (`curl | bash` → `openbox dev init`).

Three paths, pick your depth:
- **Path A — core only (§2–§4):** register → emit → verify with `psql`. No Keycloak/FE.
- **Path B — full dashboard (§5):** adds a **local** Keycloak + `openbox-fe` so you
  log in and see the data rendered. Builds on Path A.
- **Path C — hybrid (§6):** local data plane (Postgres/backend/core) but **auth,
  Guardrail, and OPA point at a shared UAT env over VPN**. Fastest inner loop —
  no local Keycloak realm bootstrap, log in as your real UAT identity. Builds on A+B.

> **Posture:** observe-first + opt-in enforce (Epic E6). Enable enforce at onboarding
> with `openbox dev init … --enforce` (persisted to `dev.json`) or per-session with
> `OPENBOX_ENFORCE=1`; default is observe, fail-open — never blocks a tool call or a
> commit. Enforcement is evaluated **in-process** by the hook — no daemon, no socket
> (ADR-0006). **Content capture is ON by default as of 2026-07-15**: the session
> **prompt** is captured
> and egressed **unredacted** (redaction-at-source is inert, `[EXT-guardrail-redaction]`).
> Opt out with `content_capture:false` in `dev.json` or `OPENBOX_CONTENT_CAPTURE=0`
> to restore metadata-only. Tool commands, file bodies, and output are **never**
> egressed on observe regardless (SL3-SEC-3).

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
        FE["openbox-fe :3233<br/>React dashboard"]
        BE["openbox-backend :3000<br/>NestJS control plane<br/>register agent → obx_ key + DID"]
        CORE["openbox-core :8086 + governance-worker<br/>Go data plane<br/>ingest → Temporal workflow → store"]
        BE <-- "shared Postgres" --> CORE
    end

    subgraph infra["Infra (Docker)"]
        PG[("Postgres :5432")]
        RD[("Redis :6379")]
        KC["Keycloak :8080<br/>realm = org"]
        TP["Temporal :7233"]
    end

    DEVUSER["you (browser)"] -- "login (Keycloak realm=org)" --> FE
    FE -- "REST + JWT (org-scoped)" --> BE
    BE -- "password grant / JWKS" --> KC
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
| **Node ≥ 20 + pnpm** (Path B) | run `openbox-fe` | `node -v && pnpm -v` |
| Sibling repos checked out next to this one | backend + core (+ `openbox-fe` for Path B) | `ls ../openbox-backend ../openbox-core ../openbox-fe` |
| Free ports | services | 3000, 5432, 6379, 8080, 7233, 8233, 8086, 3233 |

> **Org name.** This runbook uses the org id **`localdev.io`** throughout. It must
> be a **dotted** name: the dashboard login (Path B) binds *org = Keycloak realm =
> the user's email domain*, and both the email validator and realm need a TLD.
> The dotted name works for the `psql`/api-key path (Path A) too, so one org id
> serves both.

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
 VALUES ('localdev.io','dev-cli','$HASH',left('$TOKEN',12),ARRAY['create:agent','read:agent'],true,now(),now());"
echo "$TOKEN"      # ← this is OPENBOX_CONTROL_TOKEN; org = localdev.io
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
go run ./cmd/core attestation-worker &     # seals Merkle leaves; omit → "Merkle proof: Pending" forever
go run ./cmd/core server --addr 0.0.0.0:8086 &
# core auto-loads ./.env (DB=localhost:5432/openbox, REDIS, TEMPORAL_HOST=localhost:7233,
# MODE=debug, KMS_PROVIDER=local). "ListenAndServe(debug): 0.0.0.0:8086" = ready.
```

> The worker logs `agent identity verifier: AWS_REGION not configured` — expected
> and harmless locally (no KMS verifier; see §4 signing note).

> **`attestation-worker` is required for tamper-evidence.** The server's scheduler
> (`FinalizeTerminalSessionAttestations`, every `FINALIZE_TERMINAL_SESSION_INTERVAL_SEC`
> ≈30s) finds terminal sessions without attestation and starts
> `FinalizeSessionAttestationWorkflow` → `BuildEventMerkleNode` + `FinalizeSession`,
> which writes one `session_merkle_leaves` row per event. Without this worker the
> workflow is scheduled but never executes, and the dashboard shows **"Merkle proof:
> Pending"** indefinitely. Start it and a completed session seals within ~30s.

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
export OPENBOX_ORG="localdev.io"

# Machines WITH an OS keyring (secret-tool / macOS Keychain):
"$BIN/openbox" dev init --provider claude-code --org localdev.io

# Machines WITHOUT a keyring (headless / container / WSL) — opt into the 0600 file backend:
export OPENBOX_SECRET_FILE="$HOME/.config/openbox/secrets.json"
"$BIN/openbox" dev init --provider claude-code --org localdev.io --secret-backend file
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

`dev init` materializes the plugin bundle **with the engine binary already placed**
at `~/.claude/plugins/openbox-observe/bin/openbox` (SL4-WIRE-2 copies the running
engine during install), and its `hooks/hooks.json` invokes
`"${CLAUDE_PLUGIN_ROOT}/bin/openbox" hook claude-code <event>`. It also wrote the
non-secret `~/.config/openbox/dev.json`. Two things remain for a local run:

```sh
# 1) point the dev config at your LOCAL core (dev init defaults base_url to prod)
#    and, when using the file secret backend, tell the hook where it is:
python3 - <<'PY'
import json, os
p = os.path.expanduser("~/.config/openbox/dev.json"); c = json.load(open(p))
c["base_url"]   = "http://localhost:8086"
c["secret_file"] = os.path.expanduser("~/.config/openbox/secrets.json")  # if you used --secret-backend file
json.dump(c, open(p, "w"), indent=2)
PY
```

2. **Make live Claude Code fire the hooks.** Either enable the bundled plugin
   (`enabledPlugins`), or add the five hooks to `~/.claude/settings.json` pointing
   at the plugin's engine (they read everything from `dev.json`, no env needed).
   Use `/hooks` or the `update-config` skill, or merge:

```jsonc
{ "hooks": {
  "SessionStart":     [{ "matcher":"",  "hooks":[{ "type":"command", "command":"<HOME>/.claude/plugins/openbox-observe/bin/openbox hook claude-code SessionStart",     "timeout":5  }]}],
  "UserPromptSubmit": [{ "matcher":"",  "hooks":[{ "type":"command", "command":"<HOME>/.claude/plugins/openbox-observe/bin/openbox hook claude-code UserPromptSubmit", "timeout":5  }]}],
  "PreToolUse":       [{ "matcher":"*", "hooks":[{ "type":"command", "command":"<HOME>/.claude/plugins/openbox-observe/bin/openbox hook claude-code PreToolUse",       "timeout":5  }]}],
  "PostToolUse":      [{ "matcher":"*", "hooks":[{ "type":"command", "command":"<HOME>/.claude/plugins/openbox-observe/bin/openbox hook claude-code PostToolUse",      "timeout":5  }]}],
  "SessionEnd":       [{ "matcher":"",  "hooks":[{ "type":"command", "command":"<HOME>/.claude/plugins/openbox-observe/bin/openbox hook claude-code SessionEnd",       "timeout":15 }]}]
}}
```

> ⚠️ **Hooks load at session start** — start a **new** Claude Code session for
> them to take effect. `SessionStart`/`PreToolUse`/`PostToolUse` only spool (no
> network on the hot path); `SessionEnd` flushes the session's spool to core.
> To disable later: `/hooks`, or remove the block. (To drive events without a new
> session — e.g. for §4.3 / §5.4 — pipe payloads to the same binary directly.)

### 3.4 Git lineage (SL-5 → SL-6)

```sh
# Per-repo trailer hook (or use `dev init --install-git-hook` for ambient install):
"$BIN/openbox-git-hook" install <repo>/.git/hooks

# In a session, a commit is auto-stamped `OpenBox-Session: <id>`; then at push/deploy:
export OPENBOX_BASE_URL=http://localhost:8086
export OPENBOX_DID=$DID
export OPENBOX_API_KEY=$(python3 -c "import json;print(json.load(open('$HOME/.config/openbox/secrets.json'))['ai.openbox.dev']['localdev.io/claude-code/api_key'])")
export OPENBOX_SEED=$(python3 -c "import json;print(json.load(open('$HOME/.config/openbox/secrets.json'))['ai.openbox.dev']['localdev.io/claude-code/private_key'])")
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
# expect: developer | did:aip:… | a1e7…(sha256 of obx_test_ key) | f | localdev.io
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
is what Claude Code runs — a faithful smoke test). SessionStart spools; SessionEnd
flushes the session's spool to core:

```sh
B="$HOME/.claude/plugins/openbox-observe/bin/openbox"
SID="verify-$(python3 -c 'import uuid;print(uuid.uuid4())')"
C="\"session_id\":\"$SID\",\"cwd\":\"$PWD\",\"permission_mode\":\"default\""
printf "{$C,\"hook_event_name\":\"SessionStart\",\"source\":\"startup\"}" | "$B" hook claude-code SessionStart
printf "{$C,\"hook_event_name\":\"SessionEnd\",\"reason\":\"logout\"}"     | "$B" hook claude-code SessionEnd
# then query governance_events WHERE run_id='$SID'
```

Contract checks: the hook must **exit 0 with empty stdout** (observe mode), and no
**command/file/output** content may appear in any payload (SL3-SEC-3, unconditional).
The **prompt** appears only when content-capture is on (the default as of 2026-07-15);
set `OPENBOX_CONTENT_CAPTURE=0` for a metadata-only payload (INV-2).

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

## 5. View in the openbox-fe dashboard (Path B)

The dashboard talks **only to openbox-backend (:3000)** and requires a **real
Keycloak login** — there is no dev/mock bypass. **org = Keycloak realm = the
user's email domain**, and the backend derives the org from the token issuer. So
to see the data from Path A, you provision a Keycloak realm named after your org
(`localdev.io`) and a user in it, then log in.

### 5.1 Patch reCAPTCHA + wire Keycloak env, restart backend

`/auth/login` verifies reCAPTCHA unconditionally. Apply the dev bypass
([Appendix A.3](#a3-backend--recaptcha-dev-bypass)), then point the backend at the
local Keycloak and restart it:

```sh
cd ../openbox-backend
# add/replace in .env:
#   KEYCLOAK_BASE_URL=http://localhost:8080
#   KEYCLOAK_CLIENT_ID=openbox-admin        KEYCLOAK_CLIENT_SECRET=openbox-admin-secret   # master admin svc client (5.2)
#   KEYCLOAK_CLIENT_FE_ID=openbox-fe         KEYCLOAK_CLIENT_FE_SECRET=openbox-fe-secret   # the realm's FE client
#   KEYCLOAK_REALM=master                    # fallback realm for the admin client
# restart: stop the running `yarn start:dev`, then `yarn start:dev` again (dotenv loads at boot)
```

### 5.2 Bootstrap a master admin service client

`createOrganization` drives Keycloak via a **client-credentials** service account
(`KEYCLOAK_CLIENT_ID/SECRET`). A fresh Keycloak has none — create it (admin creds
are `admin`/`admin` from compose):

```sh
KC=http://localhost:8080
TOK=$(curl -s "$KC/realms/master/protocol/openid-connect/token" \
  -d client_id=admin-cli -d username=admin -d password=admin -d grant_type=password \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
H="Authorization: Bearer $TOK"
curl -s -H "$H" -H "Content-Type: application/json" -X POST "$KC/admin/realms/master/clients" -d '{
  "clientId":"openbox-admin","enabled":true,"publicClient":false,"serviceAccountsEnabled":true,
  "standardFlowEnabled":false,"directAccessGrantsEnabled":false,
  "clientAuthenticatorType":"client-secret","secret":"openbox-admin-secret"}'
CID=$(curl -s -H "$H" "$KC/admin/realms/master/clients?clientId=openbox-admin" | python3 -c 'import sys,json;print(json.load(sys.stdin)[0]["id"])')
SA=$(curl -s -H "$H" "$KC/admin/realms/master/clients/$CID/service-account-user" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
ADM=$(curl -s -H "$H" "$KC/admin/realms/master/roles/admin")
curl -s -H "$H" -H "Content-Type: application/json" -X POST "$KC/admin/realms/master/users/$SA/role-mappings/realm" -d "[$ADM]"
```

### 5.3 Register the org (realm + FE client + roles + permissions mapper + user)

One public endpoint wires it all. The email domain must equal the org:

```sh
curl -s -X POST http://localhost:3000/organization/register -H "Content-Type: application/json" \
  -d '{"orgId":"localdev.io","orgName":"LocalDev","contactName":"Dev Admin",
       "contactEmail":"admin@localdev.io","recaptchaToken":"dev-bypass"}'
# → {"status":200,"data":{"customer_id":"localdev.io",...}}
```

Set a known password for the created user and clear its setup flags (it's minted
with a random password + `mustChangePassword`; also set `email` — a null email
causes Keycloak's "Account is not fully set up"):

```sh
KC=http://localhost:8080; REALM=localdev.io
TOK=$(curl -s "$KC/realms/master/protocol/openid-connect/token" -d client_id=openbox-admin \
  -d client_secret=openbox-admin-secret -d grant_type=client_credentials \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
H="Authorization: Bearer $TOK"
USERID=$(curl -s -H "$H" "$KC/admin/realms/$REALM/users" \
  | python3 -c 'import sys,json;print(next(u["id"] for u in json.load(sys.stdin) if u.get("username")=="admin@localdev.io")')
curl -s -H "$H" -H "Content-Type: application/json" -X PUT "$KC/admin/realms/$REALM/users/$USERID" \
  -d '{"username":"admin@localdev.io","email":"admin@localdev.io","firstName":"Dev","lastName":"Admin","enabled":true,"emailVerified":true,"requiredActions":[]}'
curl -s -H "$H" -H "Content-Type: application/json" -X PUT "$KC/admin/realms/$REALM/users/$USERID/reset-password" \
  -d '{"type":"password","value":"DevPass123!","temporary":false}'
```

### 5.4 Put your Claude Code data under this org

Do Path A (§2.2, §3.2, §3.3, §4.3) with **`--org localdev.io`** so the dev agent's
`organization_id` matches the realm, and drive a session. Recap:

```sh
# seed a control token for localdev.io (§2.2), then:
"$BIN/openbox" dev init --provider claude-code --org localdev.io --secret-backend file
DID=$(python3 -c "import json;print(json.load(open('$HOME/.config/openbox/dev.json'))['developer_did'])")
docker compose -f ../openbox-backend/docker-compose.yml exec -T postgres \
  psql -U postgres -d openbox -c "UPDATE agents SET signing_required=false WHERE did='$DID';"
# point dev.json at local core + set secret_file (§3.3), then drive a session via the plugin binary:
B="$HOME/.claude/plugins/openbox-observe/bin/openbox"; SID="demo-$(python3 -c 'import uuid;print(uuid.uuid4())')"
C='"session_id":"'$SID'","cwd":"'$HOME'","permission_mode":"default"'
printf "{$C,\"hook_event_name\":\"SessionStart\",\"source\":\"startup\"}"        | "$B" hook claude-code SessionStart
printf "{$C,\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Read\",\"tool_input\":{\"file_path\":\"/tmp/x\"}}" | "$B" hook claude-code PreToolUse
printf "{$C,\"hook_event_name\":\"PostToolUse\",\"tool_name\":\"Read\",\"tool_input\":{\"file_path\":\"/tmp/x\"}}" | "$B" hook claude-code PostToolUse
printf "{$C,\"hook_event_name\":\"SessionEnd\",\"reason\":\"logout\"}"            | "$B" hook claude-code SessionEnd
```

### 5.5 Run openbox-fe

```sh
cd ../openbox-fe
cat > .env.local <<'EOF'
VITE_API_URL=http://localhost:3000
# Google reCAPTCHA v2 TEST key (always passes) — local dev only.
VITE_RECAPTCHA_SITE_KEY=6LeIxAcTAAAAAJcZVRqyHh71UMIEGNQ_MXjiZKhI
EOF
pnpm install
pnpm dev          # → http://localhost:3233   (or: node node_modules/vite/bin/vite.js --port 3233)
```

Open **http://localhost:3233** and log in:

| Field | Value |
|---|---|
| realm / org | `localdev.io` |
| email | `admin@localdev.io` |
| password | `DevPass123!` |

The **Dashboard → governance feed** shows the events; **Agents → the developer
agent → Sessions → the session** shows the full tool trace (SessionStarted →
ToolCall/ToolResult → SessionEnded).

### 5.6 Verify the dashboard is served the data (headless)

Confirm without a browser by calling the exact endpoints the dashboard uses:

```sh
TOKEN=$(curl -s -X POST http://localhost:3000/auth/login -H "Content-Type: application/json" \
  -d '{"realm":"localdev.io","username":"admin@localdev.io","password":"DevPass123!","recaptchaToken":"dev"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["access_token"])')
A=(-H "Authorization: Bearer $TOKEN" -H "X-OpenBox-Client: web")
curl -s "${A[@]}" http://localhost:3000/agent/list                                            # the developer agent
curl -s "${A[@]}" http://localhost:3000/organization/localdev.io/dashboard/governance-feed    # the events
# agent sessions → session logs (use the session UUID from /sessions, not run_id):
curl -s "${A[@]}" http://localhost:3000/agent/<agentId>/sessions
curl -s "${A[@]}" http://localhost:3000/agent/<agentId>/sessions/<sessionUuid>/logs
```

---

## 6. Path C — hybrid: local data plane + UAT auth (fastest inner loop)

Run **Postgres + backend + core locally**, but point **auth (Keycloak), Guardrail,
and OPA at a shared UAT env** reached over VPN. This skips the entire local-Keycloak
realm-bootstrap dance (§5.1–5.3) — you log in as your **real UAT identity** and your
org already exists upstream. Verified 2026-07-13 (org `openbox.ai`).

**Topology:** local = Postgres, Redis, Temporal, backend `:3000`, core `:8086` +
governance-worker + attestation-worker, `openbox-fe :3233`, **KMS local**. UAT (via
VPN) = Keycloak (`identity.node.lat`), Guardrail (`openbox-guardrails.node.lat`),
OPA (`opa.node.lat`).

### 6.1 Connect the VPN, then merge UAT endpoints into the active `.env` (not `.env.local`)

> ⚠️ **Neither app reads `.env.local`.** core does bare `godotenv.Load()` (→ `.env`);
> backend `ConfigModule.forRoot({isGlobal:true})` has no `envFilePath` (→ `.env`).
> A staged `.env.local` is **inert** — you must merge its keys into the active `.env`,
> and **selectively**: keep DB/Redis/Temporal/KMS **local**, only move the service
> URLs to UAT. A blind `cp .env.local .env` on core drags its `DB_HOST` to a
> shared/prod-ish ELB — don't.

- **`openbox-backend/.env`** — set the Keycloak block to UAT (base URL
  `https://identity.node.lat`, the UAT `KEYCLOAK_CLIENT_*` id/secret, redirect/web-origins
  including `http://localhost:3233/*`); keep `SUPABASE_DB_URI` on `localhost`,
  `KMS_PROVIDER=local`, `NODE_ENV=development`.
- **`openbox-core/.env`** — set only `OPA_URL=https://opa.node.lat` and
  `GUARDRAIL_URL=https://openbox-guardrails.node.lat`; keep `DB_HOST=localhost`,
  `KMS_PROVIDER=local`, `WORKFLOW_ID_PREFIX`/task queues as-is (server & worker must match).

Sanity after connecting: `getent hosts identity.node.lat && curl -sf https://identity.node.lat/realms/<org>/.well-known/openid-configuration` (the `.node.lat` hosts are Cloudflare-fronted; a 302/JSON = reachable).

### 6.2 The admin-API constraint — two extra dev-gated backend patches

UAT Keycloak's **admin REST API sits behind an RBAC gateway** (`/admin/*` → `403
"RBAC: access denied"`) that a locally-running backend cannot pass — even a client-
credentials token carrying every realm-management role is rejected at the edge.
**Public OIDC works** (login = password grant on `/realms/<org>/…/token`; JWKS verify),
so login and all DB-backed dashboard reads are fine — but any backend call using
`kcAdminClient` fails. Apply [Appendix A.4](#a4-backend--getorganization-admin-api-fallback)
and [Appendix A.5](#a5-backend--getuserteams-admin-api-fallback) (both `NODE_ENV`-gated).
Without A.5 the dashboard 500s on *every* authenticated route (incl. `POST /auth/csrf`).

### 6.3 Onboard + log in

1. Do §2–§4 with `--org <your-uat-org>` (e.g. `openbox.ai`) so the dev agent's
   `organization_id` matches your Keycloak realm/email-domain. Seed the control
   token for that org (§2.2). Set `signing_required=false` (§3.2).
2. `yarn build` the backend after the A.4/A.5 patches, (re)start it, and run
   `openbox-fe` (§5.5) — deps are usually already installed, so
   `node node_modules/vite/bin/vite.js --port 3233` works without pnpm.
3. Log in at `http://localhost:3233`: realm/org = your UAT org, email = your UAT
   user, password = your **real UAT password**. No realm registration needed.

### 6.4 What the dashboard shows (dev-runtime Phase-1 expectations)

- ToolCall/etc. render **metadata only** — `event_id, tool_name, event_type,
  trust_tier, workflow_id`(=agent DID)`, permission_mode, age_fallback_used`. **No
  Input/Output/file-path**: content is stripped at source (INV-2). The UI *has*
  those fields because it's shared with the agent runtime; observe-only leaves them empty.
- **Activity** column: shows the **tool name** for ToolCall/ToolResult (`Edit`/`Bash`/
  `mcp__…`) and the **event type** for lifecycle/`Deploy` (`SessionStarted`…). The
  client emits `activity_type` on the wire (SL-12) — a pass-through column the shared
  UI reads; without it the column falls back to the literal **"Unknown"**. (The fix is
  shift-left-only; openbox-fe/backend are untouched.)
- `age_fallback_used: true` = core's goal-alignment/AGE couldn't get a real answer
  from the UAT Guardrail/OPA routes → fail-open `ALLOW`. Cosmetic for observe-only;
  for real scoring, point core at a Guardrail/OPA that serves the routes it calls.
- **Merkle proof** seals within ~30s **only if `attestation-worker` is running** (§2.4).

> **Env-load note for the snap toolchain:** if `node`/`yarn` are snap packages, the
> backend and FE dev servers may be killed when launched from a wrapping shell, and
> their logs land in snap's private `/tmp`. Run them in a **plain interactive
> terminal** and log to a `$HOME` path. core (static Go binary) is unaffected.

---

## 7. Teardown

```sh
# openbox-fe: stop the `pnpm dev` / vite process
# core (find the `go run ./cmd/core …` processes) — Ctrl-C or:
pkill -f 'cmd/core (server|governance-worker|attestation-worker)'
docker rm -f obx-temporal
cd ../openbox-backend && docker compose down          # add -v to also drop volumes/data (incl. the Keycloak realm)
# stop the backend `yarn start:dev` process
# (optional) remove local hooks: delete the OpenBox block from ~/.claude/settings.json (or /hooks)
# (optional) drop the Keycloak realm only: DELETE http://localhost:8080/admin/realms/localdev.io (master admin token)
```

---

## 8. EXT-core caveat (production reality)

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

### A.3 backend — reCAPTCHA dev bypass

*(Path B only.)* `/auth/login` and `/organization/register` verify reCAPTCHA
against Google's siteverify unconditionally, which no local stack can pass. Gate
it on env so production still enforces:

- `src/modules/recaptcha/recaptcha.service.ts` — at the top of `verifyToken`,
  return `true` when `process.env.NODE_ENV !== 'production'` (allow a
  `RECAPTCHA_ENFORCE=1` override to force real verification). The DTOs still
  require a non-empty `recaptchaToken` string, so callers pass any placeholder
  (`"dev-bypass"`); the FE gets a real token from the reCAPTCHA **test** site key.

### A.4 backend — `getOrganization` admin-API fallback

*(Path C only.)* The public `GET /organization/:id` org-precheck calls
`kcAdminClient.realms.findOne`. Against a UAT Keycloak whose admin API is
gateway-gated (§6.2) this 403s and surfaces as `NotFoundException("Organization
not found")`, blocking the FE org-entry screen. Make it dev-tolerant:

- `src/modules/organization/organization.service.ts` `getOrganization` — wrap the
  admin lookup in try/catch; on success return the realm profile; on failure (or
  null) in non-production return `{ id: orgId, displayName: orgId }`; in production
  re-throw / keep `throw new NotFoundException('Organization not found')`.

### A.5 backend — `getUserTeams` admin-API fallback

*(Path C only, and the critical one.)* `TeamAccessGuard` runs on **every protected
route** (including `POST /auth/csrf` right after login) and calls
`TeamService.getUserTeams`, which makes two admin-API calls
(`AuthService.getUserGroups` + `getTeams`). Against a gated UAT admin API both throw,
500-ing the entire authenticated surface. Make it dev-tolerant:

- `src/modules/team/team.service.ts` `getUserTeams` — wrap the `Promise.all` in
  try/catch; on failure in non-production return `{ userTeams: [], allTeams: [] }`
  (org admins still get org-wide scope via `teamIds=undefined` in the guard, so data
  stays visible); in production re-throw.

> These join the KMS-provider (A.1) and reCAPTCHA (A.3) patches as **uncommitted,
> `NODE_ENV`-gated, production-safe** local-dev enablement. Rebuild with `yarn build`
> (note: `tsc` strips comments, so grep `dist/` for the fallback **code**, not the comment).

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
| FE login: `reCAPTCHA verification failed` / register 422 `recaptchaToken should not be empty` | Apply the reCAPTCHA bypass (Appendix A.3) and set the FE test site key (§5.5); DTOs still need a non-empty token string. |
| `/organization/register` 500 `invalid_client` | The master admin service client isn't set up or the backend `.env` `KEYCLOAK_CLIENT_ID/SECRET` don't match it — see §5.1–5.2. (Also check no stale backend on :3000 with old env.) |
| Keycloak token: `Account is not fully set up` | The user has required actions or a null `email` — PUT the user with `email` set, `emailVerified:true`, `requiredActions:[]` (§5.3). |
| FE loads but calls fail with CORS | Backend `ALLOWED_ORIGINS` must include `http://localhost:3233` (it's the `.env.example` default). |
| Dashboard is empty after login | The logged-in realm/org must equal the data's `agents.organization_id`. Register the org and run `dev init --org <same>` (§5.3–5.4). |
| Logs endpoint 500 `invalid input syntax for type uuid` | Use the **session UUID** (`id` from `/sessions`), not the `run_id`, in `/sessions/<id>/logs`. |
| Dashboard shows **"Merkle proof: Pending"** forever | `attestation-worker` not running — start `go run ./cmd/core attestation-worker` (§2.4); a terminal session seals within ~30s. |
| (Path C) `GET /admin/...` `403 "RBAC: access denied"`; login OK but dashboard 500s on `POST /auth/csrf`; `/organization/:id` → "Organization not found" | UAT Keycloak's **admin API is gateway-gated** and unreachable from a local backend (§6.2). Apply A.4 (`getOrganization`) + A.5 (`getUserTeams`) fallbacks; `yarn build` + restart. Public OIDC (login) is unaffected. |
| (Path C) backend/FE process dies silently, empty logs | Snap `node`/`yarn`: private `/tmp` hides logs and the wrapping shell may reap the server. Run in a plain terminal, log to `$HOME` (§6.4). |
