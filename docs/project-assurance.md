# Project assurance

Project assurance provides passive project inspection, one-shot local
development evaluation, native-host issue analysis, and sealed advisory
reporting. It remains separate from developer-runtime hook governance.

## Available commands

```text
openbox project inspect [path] [--output DIR]
openbox project evaluate --image IMAGE --env-file FILE --openbox-agent AGENT_ID --output DIR
openbox project verify PACK
openbox project finalize --evaluation OBSERVATION_PACK --analysis CANDIDATE_JSON --output REPORT_PACK
openbox project report --pack DIR [--format markdown|json|sarif]
openbox project propose --pack DIR [--format json|markdown]
```

`inspect` copies selected source into run-owned temporary storage, performs
bounded lexical discovery, writes exactly `project-snapshot.json`,
`project-model.json`, and `sdk-coverage.json`, and removes the temporary copy. It
does not execute or import project code and does not contact OpenBox or a model.

`verify` dispatches by the committed manifest. It preserves the historical v1
audit-pack verifier, fully reconstructs and schema-validates sealed Phase 2
observation packs, and independently reconstructs sealed Phase 4 security
report packs. `report` emits a verified audit- or security-report projection;
it does not render an observation pack. `propose` remains historical-only.
None of these commands applies, publishes, approves, or deploys an OpenBox
control.

## Development evaluation direction

`openbox project evaluate` is the narrow local execution and observation lane.
It runs the standard OCI `Entrypoint + Cmd` of one pre-existing `linux/arm64`
local image as the **main process** of an OpenBox Sandbox project run, then
reads the exact terminal backend session through the bounded dashboard API
contract. Complete success produces a manifest-last, owner-only
`ai.openbox.project-observation/v1` pack. A failed run retains only the mutually
exclusive `.incomplete` diagnostic form. It accepts no project path, replacement
entrypoint, scenario, service port, health/invoke route, or mount.

The lane does **not** drive OpenShell. It speaks the OpenBox Sandbox service
protocol over mutual TLS (`cli/internal/assurance/sandboxclient`), and that
service owns the OpenShell contract, its version pin, and the security floor.
The `openshell` CLI is invoked nowhere in `cli/internal/assurance`.

The workload runs as the sandbox's main process rather than as an exec session,
because only the main process receives the environment OpenShell builds from
attached provider profiles — a workload run through exec silently loses every
credential the providers were attached to supply.

OpenShell supplies reproducible development-run orchestration and available
runtime observations. It is not a production security boundary or the OpenBox
enforcement plane. Landlock, PID-limit, signing, and other unavailable controls
remain explicit coverage limitations, and so do three consequences of the
main-process model, each recorded in every pack
(`cli/internal/assurance/evaluate/run.go`):

- workload stdout and stderr are **not retained**. The gateway API carries
  supervisor records only — `GetSandboxLogs` and `WatchSandbox` both return
  `SandboxLogLine` — so a failing image reports an exit code and nothing else.
  Get the image running under plain `docker run` with the same environment
  first;
- per-process egress decisions and violations are **not collected**, because
  those arrive on an exec result and a main process produces none. The
  `sandbox_isolation` channel therefore reports zero records;
- the model route is **not independently receipted**. The retired CLI lane
  proved it by substring-matching a gateway log line; no typed receipt has
  replaced that, so `model_route` reports `missing`.

Production workloads, identities, endpoints, data, and automatic control
publication are excluded.

The retained one-shot Mastra image is a conformance asset for that adapter and
SDK path. It does not create a customer security report and does not substitute
a fake SDK receiver for local OpenBox Core in the product workflow.

## What a run needs

Three independent things, and it is worth keeping them apart: one is set up
once per machine, one per OpenBox environment, and one per project.

### 1. Infra — once per machine

| Requirement | Pinned | Owner |
|---|---|---|
| Docker | running | operator |
| OpenShell gateway + VM driver | **exactly `0.0.111`** | openbox-sandbox (`packaging/launcher/src/pin.rs`) |
| OpenBox Sandbox service | provisioned with ProjectRun on | `OPENBOX_PROJECT_RUN_V2=1 obs provision` |
| Host platform | `darwin/arm64` | `evaluate/preflight.go` |

The OpenShell version has one owner. This repo does not assert it, check it, or
record it; the sandbox service answers one question during preflight — whether
it will run this workload — and a deployment that does not advertise
`project_run_v2` is a `not_runnable` result, not a fallback.

### 2. Connector — once per OpenBox environment

Local, UAT and production differ only in coordinates. Nothing in the lane is
hardcoded to a local stack.

```sh
OPENBOX_BASE_URL=https://core.uat.openbox.ai      # Core, the relay's upstream
OPENBOX_BACKEND_URL=https://backend.uat.openbox.ai
OPENBOX_SANDBOX_PROVIDER=obx-openbox-uat          # OpenShell provider holding
                                                  # that environment's key
```

Precedence is environment, then `~/.openbox/dev.json`, then a local-stack
default — the same precedence `openbox auth` and `openbox init` already use, so
a machine pointed at UAT by `auth` stays pointed there without a flag. All three
resolve once, in preflight (`evaluate.resolveConnector`), so no later step can
disagree about which Core a run was pointed at.

The workload never sees a connector host. It reaches
`host.openshell.internal:<relayPort>` and the evaluator's in-process relay makes
the outbound call, so `OPENBOX_URL` inside the guest is the relay address.

`OPENBOX_API_KEY` is the one governed credential: the generated policy names the
provider in a `credential_binding`, the gateway resolves it, and it appears in
no request this lane sends.

### 3. Project — per project, in `.env.sandbox`

An ordinary dotenv file. Copy the project's own `.env` to it and change what the
sandbox needs; nothing is renamed and nothing is prefixed.

```sh
NODE_ENV=production
OPENAI_BASE_URL=https://inference.local/v1
OPENAI_MODEL=granite4.1:3b
OPENAI_API_KEY=unused
PAYMENTS_API_KEY=sk-live-...

OPENBOX_SANDBOX_MODEL_ROUTE=local-ollama
OPENBOX_SANDBOX_MODEL_DIGEST=sha256:6fd349357287c7ffc9e38189a93b48ea175d24fc566b38f09cfc564fb7f303eb
```

It is a **separate file from `.env` on purpose.** The evaluator never reads
`.env` or `.env.local`, so a run carries only what was copied here deliberately,
and a project can point the sandbox at test credentials while local development
keeps its own.

Which model backend serves `inference.local` — Ollama, OpenAI, Gemini,
OpenRouter — is a gateway-side choice, configured as an OpenShell route and
provider. The project declares only the model name it expects.

Two names are **runner directives, not project environment**. They keep an
`OPENBOX_SANDBOX_` prefix so they cannot collide with a variable the workload
wants, are matched by exact name, and are never passed to the guest:

- `OPENBOX_SANDBOX_MODEL_ROUTE` — `local-ollama` requires the host to prove the
  model is present and cold before the run; `gateway` says the route is served
  by something this host cannot see, so there is nothing local to preflight and
  the route is recorded as unproven.
- `OPENBOX_SANDBOX_MODEL_DIGEST` — required, because the v1 effects schema
  requires a sha256 `model_digest` and offers no way to express a route that
  publishes none. For a hosted route there is no truthful value; the field wants
  to be `expected_model_digest` or optional, which is a v2 effects change rather
  than something to synthesise. Stated here as the limitation it is.

The five connector values are reserved and a project may not set them:
`OPENBOX_EVALUATION_ID`, `OPENBOX_AGENT_ID`, `OPENBOX_URL`, `OPENBOX_API_KEY`,
`OPENBOX_SAFE_SINK_URL`.

## How credentials reach the workload

Two paths, and the difference is the point.

| | Held by | Can the workload use it off-policy? |
|---|---|---|
| Policy `credential_binding` | the proxy | **No** — it never receives the secret |
| `.env.sandbox` variable | the workload | Yes; the egress policy is the only control |

The connector key takes the first path. Everything a project declares takes the
second, because binding requires an endpoint and many credentials have none this
lane can name — non-HTTP protocols, SDKs that sign their own requests, endpoints
not known until runtime. Refusing those would not make such projects safer, it
would make them unevaluable.

So the sealed pack **discloses** rather than refuses. Every credential-shaped
name is listed in `coverage_limitations`:

```text
credential-shaped variable PAYMENTS_API_KEY was supplied to the workload
in plaintext and was not bound to an endpoint
```

Two things about that wording are deliberate. It says *credential-shaped
variable*, not *credential*, because the run classified by name: it observed
that the variable was plaintext and unbound, never that the value is really a
secret, and a pack should not guess inside its own evidence. And the heuristic
errs toward listing — a stand-in like `OPENAI_API_KEY=unused` is listed too —
because over-reporting a setting costs one line while under-reporting a real
credential would make the pack assert something false.

A value is never rendered, logged, or written to any sealed file. A credential
baked into the image's own `Config.Env` is still refused outright: the run
cannot disclose what it never saw declared, and a layer outlives the run.

## Native-host analysis and finalization

`openbox init` installs the canonical `openbox-security-evaluation` skill for
the selected Claude Code or Codex provider. The developer explicitly invokes
it with only a verified observation pack and a new candidate path:

```text
openbox-security-evaluation OBSERVATION_PACK NEW_CANDIDATE_JSON
```

The mode-0600 candidate is untrusted and issue-only. The skill makes no OpenBox
request, reads no credential, reruns no project, and never recommends or applies
a control.

`project finalize` first verifies the full observation and candidate offline.
Any integrity, schema, citation, identity, permission, path, or output-preflight
failure occurs before credential lookup, network access, or output creation.
Only after that gate does it use the existing host-side
`OPENBOX_CONTROL_TOKEN` against the exact local backend to capture two matching,
GET-only safe projections of the target agent's current posture. It maps valid
issues through the frozen inert recommendation catalog and publishes an
owner-only, no-clobber `ai.openbox.project-security-report/v1` pack.

The report embeds the verified observation, accepted candidate, standards and
recommendation catalogs, safe target posture, and matching JSON, Markdown, and
SARIF projections. `no_supported_issue` and `inconclusive` both have zero
recommendations and are explicitly not security passes. Verification and
rendering of a sealed report are offline.

## Historical compatibility

The frozen `openbox.audit-pack/v1` contracts remain readable so prior evidence
can still be verified and rendered. Their run-profile, sandbox-posture,
scenario, judgment, and policy-proposal objects are historical read contracts;
they are not the schema or execution design for the OpenShell workflow.

Historical native Codex, Claude/SRT, Seatbelt and governed-rerun plans and ADR
sections remain decision records only. No corresponding runner, probe, profile,
scenario, receiver, or rerun command is reachable from the CLI.

**ProjectRun v2 is no longer historical.** It is the execution path this lane
uses, implemented in openbox-sandbox and negotiated by capability before any
mutation. The plan section that described it as a deferred decision record is
superseded by the running code.

## Data and authority

- Passive inspection, skill analysis, pack verification, and rendering remain
  local. Finalization makes only the bounded local GET-only posture read after
  its offline gate.
- Reports and historical proposals write to stdout unless redirected by the
  caller.
- The OpenBox evaluation key is provider-bound and never leaves the gateway.
  Project credentials in `.env.sandbox` DO reach the workload in plaintext and
  are disclosed by name in the pack; see *How credentials reach the workload*.
  A credential embedded in the image's `Config.Env` is refused.
- Finalization reads the organization control token only from the environment;
  no credential, header, raw policy/guardrail body, or arbitrary backend
  response enters the report pack.
- Missing or invalid evidence fails closed; it never becomes a positive claim.
- OpenBox control suggestions are inert. Apply, approval, publication,
  deployment, and effectiveness verification require separate authority.
