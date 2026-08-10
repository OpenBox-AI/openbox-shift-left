# OpenBox Shift-Left

**Governance for the coding agents your developers already use.**

OpenBox governs agents at runtime. This extends the same pipeline one step earlier —
to Claude Code, OpenAI Codex and the other agentic tools that *write* the code — so
you can answer, for any commit or deploy:

> **who produced this, with which tools and prompts, at what cost — and was it allowed?**

One command to onboard. No daemon, no proxy, no second dashboard.

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
export OPENBOX_CONTROL_TOKEN=obx_key_…          # your org key, from the dashboard
openbox init --provider claude-code --backend-url https://<your-openbox> --enforce
```

Then just use `claude` as normal. → **[Getting started](docs/getting-started.md)**
(five minutes, including self-hosted and troubleshooting).

---

## What you get

| | |
|---|---|
| **Session telemetry** | every session, prompt, tool call and MCP call as normalized governance events — with token cost |
| **Enforcement** | block, ask-for-approval, or redact secrets *before* a tool runs, from your org policy |
| **Human approval** | a risky call pauses the session; an approver answers from the dashboard or `openbox approve` |
| **Autonomous approval** | a bounded approver answers inside the pause, so routine work never waits ([ADR-0012](docs/adr/ADR-0012-autonomous-approver.md)) |
| **Lineage** | `session → commit → deploy`, with a signed commit attestation |
| **Evidence** | each session reports its own effective posture, so the control plane never has to trust the endpoint's word |

## How it works

```
 claude / codex ──hooks──▶ openbox engine ──▶ local policy (µs, no network)
                                 │                    │
                                 │                    └─▶ allow · deny · ask · redact
                                 ▼
                        spool ──▶ openbox-core /evaluate ──▶ sessions · spans · lineage
                                        (AIP-signed, same endpoint as agent runtime)
```

A single static binary is the whole runtime: it is the CLI, the hook engine, the
git hook and the policy evaluator. Enforcement decides **in-process** in
microseconds ([ADR-0006](docs/adr/ADR-0006-in-process-decider.md)); telemetry is
spooled and delivered off the hot path in near-real-time (a detached, debounced
flusher drains the spool within ~2s of each tool call; SessionEnd remains the
completeness safety net), so a slow or absent OpenBox never slows a tool call
and never blocks one.

Everything provider-agnostic lives in one engine; each tool adds only a thin
adapter behind one SPI. Adding a tool is an adapter, not a fork.
→ **[Architecture](docs/architecture.md)**

## Provider support

| Provider | Telemetry | Enforcement | Approvals | Org mandate |
|---|---|---|---|---|
| **Claude Code** | shipped — hooks + durable spool | deny · ask · redact | full, incl. waking a session on a late decision | managed settings |
| **Codex** | shipped — hooks + durable spool | deny · redact (no native "ask") | deny + findings channel | `requirements.toml` / MDM (hook itself not yet mandatable) |
| **Cursor** | not built | — | — | Team hooks available |

Two capabilities are provider-independent and work with any tool: OpenBox
registration and the git-trailer commit binding — so lineage and cost tracking
still apply where no adapter exists.

## What leaves your machine

Content capture is **on by default**: prompts are sent. Tool commands and file
bodies are **never** sent on ordinary telemetry — only on an approval request,
because a request an approver cannot read is a gate they cannot exercise.
Credentials never leave the OS keychain.

One setting turns the content off (`content_capture: false`), and the exact field
list is in **[Data and privacy](docs/data-and-privacy.md)**.

## What this does *not* prove

A governance tool that overstates its guarantees is the failure it exists to
prevent, so the limits are documented as first-class:

- **Commit attribution is an inferred claim** unless the pipeline fetches the
  signed attestation note — then it is cryptographically verified.
- **Local enforcement prevents mistakes, not motivated bypass**, until the
  provider's managed configuration is deployed (`deploy/managed/`).
- **Egress is recorded, not controlled.** OpenBox does not proxy or allow-list the
  coding tool's traffic to its model provider; it records that posture as evidence.
- **Policy-bundle signatures are verified but not yet issued** — the backend does
  not sign yet, so bundles load as `unsigned` and the state is reported in the
  session's posture.

Details and current status: **[Assurance](docs/architecture.md#assurance--what-the-evidence-proves)**.

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | install → onboard → verify, with troubleshooting |
| [Architecture](docs/architecture.md) | engine, adapters, enforcement tiers, approvals, assurance |
| [Data and privacy](docs/data-and-privacy.md) | exactly what is captured and sent |
| [Lineage](docs/lineage.md) | `session → commit → deploy` and how it is verified |
| [ADRs](docs/adr/) | the decisions, and why |
| [Event contract](contracts/dev-event/) | the normalized event schema and its wire mapping |
| [End-to-end tests](docs/testbed/e2e.md) | `testbed/` — a mock-free suite against a real stack |

## Contributing

Go 1.23+, no cgo. The repo is a Go workspace of small modules
([ADR-0011](docs/adr/ADR-0011-multi-module-layout.md)):

```bash
go build ./cli/...                  # the binary
cd cli && go test ./...             # one module
./testbed/run-all.sh                # the end-to-end suite (needs a local OpenBox stack)
```

Anything provider-agnostic belongs in `adapters/common/`; a new table, endpoint or
service needs an ADR. The end-to-end suite needs an OpenBox stack it can reach —
`testbed/00-preflight.sh` tells you whether the one you have is healthy enough to
trust the results.

## License

[Apache License 2.0](LICENSE).
