# Project assurance

Run your agent project in the OpenBox sandbox, then ask OpenBox to evaluate what
it did. The run happens on your machine; the evaluation, the report and the rule
suggestions happen in the OpenBox backend ([ADR-0023](adr/ADR-0023-backend-owned-security-evaluation.md)).

```text
openbox project evaluate --image IMAGE --env-file FILE --openbox-agent AGENT_ID [--wait]
```

## What happens

1. The CLI runs the standard `Entrypoint + Cmd` of one local `linux/arm64` image
   as the **main process** of an OpenBox Sandbox project run. It accepts no
   project path, replacement entrypoint, scenario, port or mount.
2. The project's own OpenBox SDK sends its events to Core through a run-scoped
   relay (the guest can reach only the host gateway). Core stores them exactly as
   it stores any session.
3. When the run ends the CLI asks the backend to evaluate that run
   (`POST /agent/{agentId}/security-evaluations`). That request is the **only**
   thing that starts an evaluation; nothing evaluates on its own.
4. The backend waits for the session's terminal event, reads the session from its
   own tables, judges it with the organization's decision model, sends what that is
   unsure of to the organization's language model, validates the result, and
   stores a report. Without `--wait` the CLI prints the evaluation id
   and exits; with `--wait` it polls and prints a summary.
5. The report appears in the dashboard's Evaluation tab. A suggested rule is
   applied only when a person clicks Accept; the evaluator never writes a
   control.

The lane does **not** drive OpenShell. It speaks the OpenBox Sandbox service
protocol over mutual TLS (`cli/internal/assurance/sandboxclient`), and that
service owns the OpenShell contract, its version pin and the security floor.

OpenShell supplies reproducible development runs. It is not a production security
boundary or the OpenBox enforcement plane. Landlock and PID-limit gaps, and the
absence of SDK request signing, are limitations, not findings. Three consequences
of running the workload as the sandbox's main process are real regressions:
workload stdout and stderr are not retained, per-process egress decisions are not
collected, and the model route is not independently receipted.

## What a run needs

Four independent things: one per machine, one per OpenBox environment, one per
organization, one per project.

### 1. Infra — once per machine

| Requirement | Pinned | Owner |
|---|---|---|
| Docker | running | operator |
| OpenShell gateway + VM driver | **exactly `0.0.111`** | openbox-sandbox (`packaging/launcher/src/pin.rs`) |
| OpenBox Sandbox service | provisioned with ProjectRun on | `OPENBOX_PROJECT_RUN_V2=1 obs provision` |
| Host platform | `darwin/arm64` | `evaluate/preflight.go` |

This repo does not assert the OpenShell version. The sandbox service answers one
question during preflight — whether it will run this workload — and a deployment
that does not advertise `project_run_v2` is a `not_runnable` result, not a
fallback.

### 2. Connector — once per OpenBox environment

```sh
OPENBOX_BASE_URL=https://core.uat.openbox.ai      # Core, the relay's upstream
OPENBOX_BACKEND_URL=https://backend.uat.openbox.ai
OPENBOX_SANDBOX_PROVIDER=obx-openbox-uat          # OpenShell provider holding
                                                  # that environment's key
```

Precedence is environment, then `~/.openbox/dev.json`, then the local-stack
default — the same precedence `openbox auth` and `openbox init` use. `OPENBOX_API_KEY`
is the one governed credential: the generated sandbox policy names the provider in
a `credential_binding`, the gateway resolves it, and it appears in no request this
lane sends.

### 3. Model connectors — once per organization

Evaluation uses the **organization's own models** (bring your own key), and needs
both ([ADR-0024](adr/ADR-0024-decision-model-cascade.md)). An org admin sets them in
the dashboard:

- a **decision model**: a TypeSafe System One endpoint (`POST {base}/v1/systemone`).
  Production uses TypeSafe's hosted `jev-latest` with a key. For local experiments,
  Ollama serves the same API for the open `tev1:0.8b`, with no key;
- a **language model**: an OpenAI-compatible base URL, a model name and an API key.

Keys are write-only — no API ever returns them — and stored sealed. Without both,
the request is refused with `connector_required`, and the CLI says which is missing.

### 4. Project — per project, in `.env.sandbox`

An ordinary dotenv file. Copy the project's own `.env` and change what the sandbox
needs; nothing is renamed or prefixed. It is separate from `.env` on purpose: the
evaluator never reads `.env` or `.env.local`, so a run carries only what was copied
here deliberately. Four names are reserved for the connector and may not be set:
`OPENBOX_EVALUATION_ID`, `OPENBOX_AGENT_ID`, `OPENBOX_URL`, `OPENBOX_API_KEY`.

## The key the CLI uses

`OPENBOX_CONTROL_TOKEN` is an organization API key with exactly four permissions:
`create:agent`, `read:agent`, `update:agent` (all used by `openbox auth` to register
and rotate the project agent) and `evaluate:agent_security`. The last one requests
an evaluation and reads its result, and grants nothing else: it cannot read raw
sessions, logs or controls. `openbox auth` rejects a key with any other permission.

## How credentials reach the workload

| | Held by | Can the workload use it off-policy? |
|---|---|---|
| Policy `credential_binding` | the proxy | **No** — it never receives the secret |
| `.env.sandbox` variable | the workload | Yes; the egress policy is the only control |

The connector key takes the first path. Everything a project declares takes the
second, because binding needs an endpoint and many credentials have none this lane
can name. So the CLI **discloses** rather than refuses: every credential-shaped
name is printed as a warning (name only, never a value). It says
*credential-shaped variable*, not *credential*, because classification is by name.
A credential baked into the image's own `Config.Env` is refused outright.

## What the backend does

- **Reads** the session from `governance_events`, spans and the Merkle attestation —
  bounded, with credential-shaped text redacted before the analyst sees it.
- **Judges** each tool activity with the decision model: four yes/no probabilities
  (sends data out, changes state, instructs the agent, the operator goal allows it).
  At p ≥ 0.8 or ≤ 0.2 the answer is certain and code assembles the finding; the
  rules (ran without approval, untrusted input came first) are code, not model.
- **Escalates** what the decision model is unsure of to a Mastra agent on the org's
  language model, one focused pass per issue type (a small model asked for
  everything reports only the most salient issue). The analyst names defects and
  cites event ids; it never writes a control, a severity or an action target. A
  decision model that is configured but fails fails the evaluation: there is no
  silent switch to the language model.
- **Suggests integration steps** from a fixed menu (capture content, attest
  sessions, raise the event budget, add an effect receipt, mark untrusted inputs,
  require approval in the project), from session evidence only.
- **Validates** deterministically: every citation must resolve to an event in that
  session, standards must exist in the pinned catalog, and a finding that fails is
  listed under `rejected_candidates` rather than hidden.
- **Maps** supported defects to OpenBox controls from a server-side catalog.

`severity` is always `unavailable` and `security_pass` is always `false`. A report
with no issues, or an `inconclusive` one, is **not** a security pass.

## What a report suggests

- **Excessive agency (LLM06)** yields a policy rule: `REQUIRE_APPROVAL` when
  `activity_type` equals the cited action, in the policy_builder v2 shape the
  dashboard's policy editor uses. Every policy change is a new version and `POST`
  makes it the only active one, so delivery is a `POST` carrying the current rules
  plus this one. The dashboard's Accept does exactly that (ADR-0022).
- **Prompt injection** yields no rule, and the report says why: OpenBox has no
  semantic prompt-injection guardrail.
- **Effect sequences** yield no rule: a behavior rule only sees prior steps within
  one governance event.

## Provenance

Evidence is Core's stored, Merkle-attested events. The report records the evidence
snapshot digest, the analyst's model id and prompt digest, and the standards and
catalog versions. It does not claim signed attribution: runs are bearer-observed.

## Data and authority

- The session content the analyst reads leaves the backend for the **organization's
  own model endpoints** (decision and language), which it configured. Secrets that reached an event reach the
  analyst because Core-side redaction is not wired; credential-shaped text is
  redacted before it is sent.
- Independent receipts of external effects and of the model route are not collected,
  so a report rests on what the SDK reported and says so.
- Production workloads, identities, endpoints and data are not accepted, and
  automatic control publication is excluded.
