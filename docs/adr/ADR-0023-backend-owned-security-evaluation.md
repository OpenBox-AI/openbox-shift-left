# ADR-0023 — Backend-owned security evaluation

Status: Accepted. Date: 2026-10-02.

Plan: [`plans/261002-backend-security-evaluation/plan.md`](../../plans/261002-backend-security-evaluation/plan.md).

## Context

Project assurance ran its whole judgement on the developer's machine: the CLI
re-downloaded the session the backend already held, sealed it into a pack, a
host agent running the analyst skill produced a candidate, `project finalize`
validated it, read the target's policies with a broad control token and sealed a
report, and a demo script posted the report to the backend. Three problems:

- **Provenance was self-attested.** The same binary made and verified the pack;
  nothing was signed, and ADR-0022 §4 says the backend does not verify seals.
- **The analyst was unattested** (a free-text `analyzer` label) and the
  standards and rule catalogs were compiled into the CLI, so a stale CLI could
  suggest the wrong rule.
- **The CLI needed a read-everything credential** (eight permissions) only to
  crawl and snapshot data the backend already owns.

The SDK already sends every behavior event to Core, which stores it. The
evidence is in the backend before the CLI asks for it.

## Decision

1. **The backend produces the report.** A developer runs the project in the
   OpenBox sandbox exactly as now. The CLI then makes one request —
   `POST /agent/:agentId/security-evaluations` — and the backend reads its own
   tables, runs the analyst, validates, maps rules and stores the report.
2. **Evaluation is explicit.** Only a request starts one: the CLI as its last
   step, or the dashboard on a session. No automatic or sampled evaluation
   exists or is planned. The cost is the org's own (BYOK), so the org decides
   each time.
3. **BYOK.** An org configures one model connector (OpenAI-compatible base URL,
   model id, optional headers, API key). Without one the request is refused with
   `connector_required`; there is no degraded mode. The key is write-only: no
   read returns it (ADR-0022 §6 precedent) and it is never logged.
4. **The analyst is a Mastra agent embedded in the backend as a library**, with
   the connector as its per-request model (`{url, id, apiKey, headers}`) and a
   Zod schema as `structuredOutput`. The Mastra server, storage, memory and
   tracing are not used.
5. **The deterministic half is ported, not reinterpreted.** Candidate
   validation, citation resolution, the forbidden-key check, `*_assertion`
   relabelling, the recommendation catalog and POST-a-new-policy-version
   delivery keep the invariants ADR-0022 and CLAUDE.md name: `severity` is the
   literal `unavailable`; `security_pass` is `false`; the model finds and
   deterministic code prescribes; `inconclusive` needs a missing *required*
   authority; captured content is data, never instructions.
6. **The local lane is deleted, with no fallback.** The analyst skill and its
   installer, `project finalize`, `inspect`, `verify`, `report`, `propose`, the
   pack packages and their contracts, the effect and model relays, and the
   control-token crawl go in the same release as the cut-over. Shift-left has no
   users, so there is no migration or cleanup code.
7. **Minimum permissions.** The shared org key shrinks from eight permissions to
   four: `create:agent`, `read:agent`, `update:agent` (all used by `openbox auth`
   to register and rotate the project agent) and one new
   `evaluate:agent_security`, which requests an evaluation and reads its result
   and grants nothing else. Connector management uses the existing org
   permissions; it is a dashboard action, not a CLI one.
8. **No sandbox key or signing work.** Runs stay bearer-observed
   (`signing_required=false`) and reports say signed attribution is absent.

### New tables (this repo's rule: a new table needs an ADR)

Created by one backend migration, as in ADR-0022:

- `llm_connectors` — one per organization: base URL, model id, headers, API key
  ciphertext, timestamps.
- `security_evaluations` — one per request: agent, session or run id, status
  (`queued`, `waiting_for_session`, `analyzing`, `complete`, `failed`), the
  evaluator version, connector model id, prompt digest, standards-catalog
  version, evidence-snapshot digest and failure reason.
- `security_reports` (ADR-0022) is reshaped: the report is written by the
  backend, linked to its evaluation, and the client-supplied
  `manifest.pack_digest` is gone.

### Provenance

| Layer | Source | Trust |
|---|---|---|
| Behavior events | Core's stored, Merkle-attested events | backend-corroborated |
| Analyst | connector model id, prompt digest, catalog and evaluator versions, recorded by the backend | backend-recorded |
| Report | backend-computed; records the evidence-snapshot digest (ordered event ids plus their stored hashes) | backend-computed |

Sandbox-side facts (image, policy hash, isolation) are no longer evidence
inputs, and signed attribution is not claimed.

## Findings from the Phase 0 checks (2026-10-02)

- **Mastra runs inside the backend** (BE-00-03). `@mastra/core` 1.73.0 loads from
  CommonJS with `require('@mastra/core/agent')`; an `Agent` with
  `model: {url, id, apiKey, headers}` sent exactly one request, to the
  configured URL, carrying the configured bearer and header; `structuredOutput`
  returned the parsed object; the only outbound connection was to that endpoint.
  Evidence: a scratch harness against a local OpenAI-compatible stub, run under
  Node 22.23 and `node:20-alpine`. **Caveat:** the package declares
  `engines.node >=22.13` and the backend image is `node:20-alpine`. It worked
  on 20, but it is outside the dependency's supported range; moving the image to
  Node 22 is recommended and is an owner decision on production infrastructure.
- **`KmsService` cannot encrypt in the local stack** (BE-00-04). `encrypt` and
  `decrypt` are AWS-only and `isEncryptionConfigured()` is false locally, so the
  webhook pattern falls back to storing the secret in plaintext. A BYOK key must
  not take that path: connector creation fails closed when encryption is not
  configured, and `KmsService` gains a `local` provider (AES-256-GCM keyed from
  `OPENBOX_LOCAL_KMS_SECRET`, passed to the backend by local-stack) so the
  local proof can run.
- **Permission naming** (BE-00-04). Existing names are `<verb>:<resource>`
  (`evaluate:compliance` is the nearest). The new one is `evaluate:agent_security`.
- **The Core relay stays** (BE-00-05). The guest reaches only the host gateway
  (`host.openshell.internal`, `allowed_ips 192.168.127.254/32`), so a Core on the
  host's loopback is unreachable without it. The effect and model endpoints
  (`safe_effect_sink`, `model_route`) are removed with their relays.

## What the build changed (2026-10-02)

Decided at build time, each because the first version did not survive contact with
the running system:

- **The backend image moves to Node 22.** `@mastra/core` declares
  `engines.node >=22.13`; it ran on Node 20 but `yarn install` in the Dockerfile
  refuses it, and Node 20 reached end of life in April 2026. Changed in the
  backend's Dockerfile.
- **`KmsService` gains a `local` provider** (AES-256-GCM keyed from
  `OPENBOX_LOCAL_KMS_SECRET`, which local-stack now passes to the backend).
  Behavior change to name: in a local stack, *webhook* secrets are now sealed too;
  existing plaintext ones still read through the legacy fallback.
- **OPA is pinned in the backend image.** The Dockerfile fetched OPA `latest`, so each
  rebuild silently upgraded it; 1.21.1 rejects the behavior-rule Rego the backend
  generates and made every policy deploy fail (found when the Accept proof was run).
  Pinned to 1.19.0, the version the last released image ran. The Rego generator is
  untouched and still needs a fix before OPA can move forward.
- **The connector URL is validated.** The backend calls an org-supplied URL with
  the org's key, so it requires https and refuses internal addresses and embedded
  credentials, unless `LLM_CONNECTOR_ALLOW_HTTP=true` (local-stack sets it for a
  host Ollama). Not covered: a public name that later resolves to a private
  address.
- **The analyst's citations are a plain list of ids.** A 3B model labelled an event
  id as coverage when asked for `{index, id}`, and decorated ids with prose. The
  schema asks for ids only; extraction is tolerant, resolution is not — an id that
  is not an event of the session or a gap of the evidence rejects the issue.
- **One bad finding does not sink the report.** A finding that fails validation is
  listed under `rejected_candidates` with its reason; if every finding fails the
  result is `inconclusive`. A malformed or control-bearing analysis as a whole
  fails the evaluation.
- **Mastra sits behind a seam.** Jest cannot load its ESM-only dependency tree, so
  the model call is an injected `AnalystRunner`, loaded lazily in the app, and
  `yarn check:mastra` verifies the boundary (URL, key, headers, model name,
  `json_schema`, one request) against a stub endpoint in plain Node.
- **One pass per issue type.** A local 3B model asked for every issue type at once
  reported only the most salient one, consistently (temperature 0 made that
  reproducible). The analyst now runs one narrow pass per type (excessive agency,
  prompt injection) under shared rules and the results are merged, with a repeated
  candidate id kept once. Analyst version `security-analyst/3`; sampling is
  temperature 0 so the same record gives the same findings. This was tuned only far
  enough to prove the mechanism end to end; finding quality depends on the model an
  organization brings.
- **The sandbox model comes from the gateway.** With the model relay gone, a
  project reaches its model only through OpenShell's `inference.local` route.

## What this weakens or strengthens

- **Strengthens:** the analyst, its prompt and the catalogs are recorded and
  versioned server-side; no seal can be re-made by the developer; the CLI no
  longer holds a read-everything credential.
- **Weakens:** transcripts now egress from the backend to the org's own model
  endpoint (governed by `content_capture`; `docs/data-and-privacy.md`), and
  secrets that reached an event reach the analyst because Core-side redaction is
  not wired. Independent effect and model-route receipts are no longer gathered,
  so reports rest on SDK-observed behavior only and say so.

## Supersedes

- ADR-0022 §4 (the backend does not verify seals — there is no sealed report to
  verify) and the `publishReport` path.
- ADR-0020's skill, finalize, pack and report sections (already historical).
