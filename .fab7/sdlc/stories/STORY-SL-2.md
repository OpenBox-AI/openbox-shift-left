# STORY-SL-2 — `openbox` CLI + `dev init --provider <tool>`

**Risk:** high (mints/handles agent credentials — identity/security boundary)

## Source
- **PRD:** `.fab7/sdlc/design/prd.md` FR-1 (developer/install = agent, session = child), FR-8 (reuse DID/trust namespace), NFR-5 (org mandate substrate, pilot opt-in), NFR-6 (credential hash).
- **Architecture:** `architecture.md` D5 (S6-corrected identity), §1b interface 1 (developer onboarding front door), OD12 (unifying CLI), OD16 (AIP signing), OD17 (Go).
- **Discovery:** spike **S6 §1** (registration reality), **S6 §5** (AIP identity), **S5** (Codex), **S1** (Claude Code).

## User Value
A developer runs one command to onboard their coding tool to OpenBox governance — register identity, obtain credentials, and configure the tool — after which governance is ambient with no further UI.

## Inlined context (verified in S6 — builder need not re-read)
- **Registration endpoint (openbox-backend, exists today):** `POST agent/create`. `CreateAgentDto`: `agent_name` (req), **`agent_type`** (free-form string ≤100 — set `"developer"`, **no migration needed**), optional `model_name`/`description`/`config`/`team_ids`/`tags`, **`aivss_config` (REQUIRED)**, `attestation_mode`, optional caller `key` matching `^(obx_live_|obx_test_)[a-f0-9]{48}$`.
- **What registration returns/mints:** `obx_live_/obx_test_` key (shown once; stored SHA-256 hashed server-side), `did:aip:<uuidv5(agentId, namespace)>`, a KMS/Ed25519 signing key, and an initial trust score computed from `aivss_config`. `signing_required` defaults **true**.
- **Auth to backend** is Keycloak-JWT (S6 §0) — the CLI authenticates the human/org to call `agent/create`; the resulting `obx_` key + Ed25519 private key are the *agent's* runtime credentials used later by SL-3 against openbox-core.
- **Base URLs:** backend control-plane API for registration; `OPENBOX_URL=https://core.openbox.ai` for runtime `/evaluate` (used by SL-3, not here).
- **AIP (OD16):** the private key captured here is what SL-3 uses for Ed25519 request signing — the CLI must store it securely.

## Acceptance Criteria
- `openbox dev init --provider claude-code|codex|cursor` registers a developer agent via `POST agent/create` with `agent_type="developer"` and a **sane default `aivss_config`** (documented default risk profile), then captures the returned `obx_` key, `did:aip:` DID, and Ed25519 private key.
- Credentials are written to the **OS secret store** (e.g. Keychain/Secret Service/DPAPI), never to plaintext files in the repo or shell history (INV-1); config files reference the stored secret, not the value.
- The command **delegates provider-specific config writing** to the selected adapter's installer (SL-4 for `claude-code`); for a provider whose adapter isn't built yet it prints the required manual config and exits non-zero with a clear message.
- `--dry-run` prints the planned registration + config changes and makes **no** network or filesystem writes.
- Re-running `dev init` is **idempotent** (detects an existing developer agent for this org/tool and reuses/updates rather than duplicating).
- Registered agents are `organization_id`-scoped and share the existing DID namespace (INV-4/INV-7).
- The CLI exposes the substrate for org-wide managed-settings force-enable, but Phase-1 default is **opt-in** (no mandate activated) (NFR-5/OD10).

## Nonfunctional Requirements
- **security:** Ed25519 private key + `obx_` key handled via OS secret store; never logged, never in argv/env dumps, never committed (INV-1/NFR-6). Requires security review (Sam).
- **reliability:** partial-failure safe — if config write fails after registration, the command reports the registered agent id and how to resume; no orphaned half-state without a message.
- **usability:** single command; clear errors when the OpenBox org is unreachable or `aivss_config` default is rejected.

## Write Scope
- `cli/` (Go — `cmd/openbox/`, `internal/…`; OD17). Provider config *content* is owned by adapter stories (`adapters/…`), invoked via an installer interface — not written here.

## Dependencies
- None hard for the registration + credential-capture path (uses the existing `agent/create`).
- **Soft:** full `dev init --provider claude-code` end-to-end needs STORY-SL-4 (the Claude Code adapter installer); until then `dev init` registers + stores creds + prints/writes the config skeleton the adapter will own.
- **External (assumed-satisfied):** EXT-signoff — a reachable OpenBox org with `agent/create` available (integration test); unit tests mock the backend.

## Invariants
- **INV-1:** credentials stored/handled only via secret store or hash; never cleartext in repo/logs/argv.
- **INV-4:** all records `organization_id`-scoped.
- **INV-7:** developer agents share the runtime-agent DID namespace (no parallel identity store).

## Human Gates
| Gate | Question | Owner | Evidence Needed | Allowed Outcomes |
|---|---|---|---|---|
| G_SEC | Is the credential-handling design (OS secret store, no plaintext, no leak in argv/logs) sound? | Sam (security reviewer) | security review of `cli/` credential paths | approve / revise / block |
| G1_READY | Default `aivss_config` risk profile for a developer agent — what values? | brian (product/security) | a documented default profile accepted by `agent/create` | confirm profile |
| G1_READY | Pilot org/repo for `dev init` integration test (OD10) | brian (product) | named org/repo | name it / defer to mock-only |

## Validation
```bash
cd cli && go build ./... && go vet ./... && go test ./...
# dry-run makes no writes:
openbox dev init --provider claude-code --dry-run   # prints plan, exits 0, no secret-store/network writes
# integration (needs a test OpenBox org; else mocked in go test):
#   openbox dev init --provider claude-code   -> agent registered (agent_type=developer), creds in secret store, config written
```

## Default `aivss_config` — POSTURE ACCEPTED (brian, 2026-07-07); integers verified at build
Verified against `openbox-backend/src/modules/agent/dto/aivss-config.dto.ts` + `common/utils/calc-aivss-score.ts` (via code-graph explore). Target: **moderate** trust tier — coding agents are capable (shell/file/MCP, code that ships) but human-supervised, observe-only in Phase 1. **Decision:** the moderate posture is accepted; the builder MUST verify the integer→risk direction against `calc-aivss-score.ts` and confirm the resulting trust tier is moderate before hard-coding (the DTO descriptions and the points tables disagree — see warning).
```yaml
base_security:   { attack_vector: 2, attack_complexity: 2, privileges_required: 2, user_interaction: 2, scope: 2 }
ai_specific:     { model_robustness: 2, data_sensitivity: 4, ethical_impact: 2, decision_criticality: 3, adaptability: 4 }
impact:          { confidentiality_impact: 3, integrity_impact: 3, availability_impact: 2, safety_impact: 1 }
```
**⚠️ Verify before pinning:** `calc-aivss-score.ts` points tables **invert** several DTO field descriptions (`data_sensitivity`, `attack_complexity`, `privileges_required`, `user_interaction`, `model_robustness`). The builder MUST confirm the integer→risk direction against `calc-aivss-score.ts` and check the resulting trust tier lands moderate before hard-coding this default. All values validate within the DTO min/max regardless.

## Stop conditions
- If `POST agent/create` rejects `agent_type="developer"` or the default `aivss_config` → HALT; capture the exact 4xx and route to the G1_READY profile gate / S6 re-check.
- If no OS secret store is available on a target platform and no approved fallback exists → HALT (INV-1); do not write plaintext credentials.
- If the org is unreachable and no mock path is defined for tests → block the integration AC (unit ACs still stand).
