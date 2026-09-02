# Runbook — Shift-Left security evaluation + project assurance, end to end

Run the full `inspect → evaluate → analyze → finalize` lane against a local
OpenBox stack, from a cold machine to a sealed, verifiable security report.

Everything here is local. No production credential, endpoint, workload, or
customer data is involved, and nothing in this lane publishes or applies an
OpenBox control.

---

## 0. What a green run does and does not prove

Read this before showing the output to anyone.

**It proves**, for one image under one bounded run:

- the agent's semantic actions reached OpenBox Core through the framework SDK;
- an independent receipt — not the agent's own trace — recorded the external
  effect;
- the sealed evidence reconstructs byte-for-byte and every report claim cites a
  retained record.

**It does not prove** the project is secure, that any control was enforced, or
that unobserved paths are absent. `no_supported_issue` and `inconclusive` are
explicitly **not** security passes, and the report hard-codes
`security_pass: false` in every case. OpenShell supplies bounded development
observation; it is **not** a production security boundary and not the OpenBox
enforcement plane.

---

## 1. Prerequisites

The project-assurance lane is **macOS or Linux only**. Every platform-split file
has an erroring stub elsewhere, which matches the release matrix
(`.goreleaser.yaml` ships `linux` + `darwin` only).

| Requirement | Pinned / expected | Check |
|---|---|---|
| Docker | running, ~8 GB free | `docker ps` |
| OpenShell | **exactly `0.0.111`**, connected + authenticated | `openshell status -o json` |
| Ollama | `granite4.1:3b`, digest `6fd349357287` | `ollama show granite4.1:3b` |
| Registry image | `registry:2.8.3@sha256:a3d8aaa6…` | `docker image inspect …` |
| Go, Node, jq | recent | `go version && node -v && jq --version` |

The OpenShell and Ollama versions are pinned in code (`evaluate/types.go`), not
merely recommended. A different version is a hard `not_runnable`.

---

## 2. Bring up the local stack

```sh
cd local-stack
./scripts/up.sh          # clone, build, start, then bootstrap automatically
```

First run builds three images from source — **budget 10–15 minutes**. After
that it is seconds. `up.sh` checks for port conflicts first and names the `.env`
variable to change for each one.

If containers are already running, bootstrap alone is enough and is safe to
repeat — it never destroys data:

```sh
./scripts/bootstrap.sh
```

`bootstrap.sh` provisions Keycloak, syncs roles via the backend's own
`patch-permissions`, enables every org feature flag, mints (or **reconciles**)
the org control token, and seeds one agent + policy so OPA has a bundle to
serve.

> **The reconcile step matters.** The control token must carry *exactly* the
> eight shared lifecycle + read permissions. Finalization rejects both missing
> reads **and** unrelated write/approval authority. A token minted before this
> reconcile existed will fail Phase 4 until `bootstrap.sh` runs again.

**Verify:**

```sh
curl -sf http://127.0.0.1:3000/health >/dev/null && echo "backend ok"
curl -sf http://127.0.0.1:8086/       >/dev/null && echo "core ok"
ls -l .state/control-token             # must be 0600
```

| Service | URL |
|---|---|
| Backend (control plane) | `http://127.0.0.1:3000` |
| Core (data plane) | `http://127.0.0.1:8086` |
| Dashboard | `http://localhost:3233` |
| Temporal UI | `http://localhost:8233` |

---

## 3. Authenticate and install the skill

```sh
cd ../openbox-shift-left
export OPENBOX_CONTROL_TOKEN="$(cat ../local-stack/.state/control-token)"

openbox auth
openbox init --provider claude-code
```

At the `auth` prompts:

| Prompt | Answer |
|---|---|
| Backend URL | `http://127.0.0.1:3000` |
| Core URL | `http://127.0.0.1:8086` |
| Agent id | **leave blank** — registers a new agent |

> **Use `127.0.0.1`, not `localhost`.** The preflight string-matches these two
> values in `~/.openbox/dev.json`. `localhost` is a silent mismatch that only
> surfaces later.

`auth` writes `~/.openbox/.env` (secrets, `0600`) and `~/.openbox/dev.json`
(coordinates). `init` registers the hooks **and installs the canonical
`openbox-security-evaluation` skill** for the selected provider.

**Verify:**

```sh
jq '{agent_id, backend_url, base_url}' ~/.openbox/dev.json
jq '{name, version, digest}' ~/.claude/skills/openbox-security-evaluation/bundle.json
```

The skill digest must read
`sha256:817e35e1db637d3c9a68ea7b0adf444aa1b5e9c2ad3eaa75c22496506ce0fe13`.

---

## 4. Preflight

```sh
./testbed/project-assurance/mastra-security-demo/prepare-demo.zsh
```

This runs the source and image contract tests, builds
`ai.openbox/mastra-security-demo:local`, rebuilds the CLI, and asserts every
service, credential mode, and the skill digest. It is idempotent.

Expect it to end with:

```text
demo preflight passed
next: ./testbed/project-assurance/mastra-security-demo/launch-claude.zsh
```

Any failure here is a setup problem, not a demo result. Fix it before going on.

---

## 5. Run it

Two lanes. **Lane A** is the demo you show people; **Lane B** is the same
product path with nothing between you and the CLI.

### Lane A — guided, through Claude Code

```sh
./testbed/project-assurance/mastra-security-demo/launch-claude.zsh
```

It mints a fresh no-clobber run directory, exports `OPENBOX_DEMO_*`, and starts
Claude Code. Then use the three prompts in [DEMO.md](DEMO.md) **verbatim**.

The three-prompt split is a product boundary, not pacing: the Phase 3 skill may
emit only an issue candidate, and the Phase 4 finalizer separately owns the
OpenBox recommendations. Collapsing them hides the authority line the demo
exists to show.

### Lane B — direct CLI

```sh
CLI=./testbed/.state/project-assurance-demo/bin/openbox
RUN="$(mktemp -d ./testbed/.state/project-assurance-demo/run.XXXXXX)"

export OPENBOX_CONTROL_TOKEN="$(cat ../local-stack/.state/control-token)"
export OPENBOX_BACKEND_URL="http://127.0.0.1:3000"
export OPENBOX_BASE_URL="http://127.0.0.1:8086"
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy   # see §7

# 1 — observe
$CLI project evaluate \
  --image ai.openbox/mastra-security-demo:local \
  --env-file testbed/project-assurance/mastra-security-demo/evaluation.env \
  --openbox-agent "$(jq -er '.agent_id' ~/.openbox/dev.json)" \
  --output "$RUN/observation"
$CLI project verify "$RUN/observation"

# 2 — analyze (explicit skill invocation in the agentic host)
#     openbox-security-evaluation "$RUN/observation" "$RUN/security-analysis.json"

# 3 — finalize
$CLI project finalize \
  --evaluation "$RUN/observation" \
  --analysis   "$RUN/security-analysis.json" \
  --output     "$RUN/security-report"
$CLI project verify "$RUN/security-report"
$CLI project report --pack "$RUN/security-report" --format markdown
```

Step 2 must run in the agentic host — it is the only step a model performs, and
it may create nothing but a mode-`0600` issue candidate.

---

## 6. Expected results

| Step | Expected | Typical |
|---|---|---|
| `evaluate` | `project observation sealed: …` | ~20 s |
| `verify` (observation) | `ai.openbox.project-observation/v1` + digest | instant |
| skill | new `0600` candidate + the future `finalize` command, not run | — |
| `finalize` | `project security report sealed:` + digest | < 1 s |
| `verify` (report) | `ai.openbox.project-security-report/v1` + digest | instant |

A healthy observation pack contains six payloads and shows:

```jsonc
// effects.json
"core_relay":  { "governance_events": 6, "status": "observed" },
"model_route": { "model": "granite4.1:3b", "status": "observed" },
"safe_sink":   { "attempts": 1, "matching_receipts": 1, "status": "observed" }
```

Two coverage channels are **expected to be absent** and are not faults:
`retrieval_poison` is `missing` (the injection vector is not independently
receipted) and `signed_request_attribution` is `unsupported` (bearer-only
evaluation identity). Both must appear as report limitations.

The rendered report should show `Result: issues`, `Security pass: false`,
`severity: unavailable` on every issue, and inert recommendations mapped
`new_gap` against an agent that has no controls yet.

**Confirm nothing was written** — the whole point of the lane:

```sh
docker exec -i openbox-local-postgres-1 psql -U postgres -d openbox -tAc \
  "select 'policies='||count(*) from policies where agent_id='<AGENT_ID>'
   union all select 'guardrails='||count(*) from guardrails where agent_id='<AGENT_ID>'
   union all select 'behavior_rules='||count(*) from agent_behavior_rules where agent_id='<AGENT_ID>';"
```

All three must be `0`.

---

## 7. Troubleshooting

**`proxy environment is forbidden for local backend collection`**
A proxy variable is set in your shell. The collector refuses it deliberately —
a proxy on local evidence collection is a MITM seam. `launch-claude.zsh` does
**not** scrub these, so a corporate-proxy machine hits it on the first run:

```sh
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
```

**`load local Granite model` / `exit_classification: model_load_failure`**
A cold load of the 2 GB model exceeded its budget. The preflight requires the
model to be *unloaded*, so every load is cold; the budget is a dedicated 30 s
client. The slowest case is the first run after a reboot. Do **not** pre-load
the model to speed this up — the preflight rejects an already-loaded model. If
you loaded it by hand, unload it:

```sh
curl -s -X POST http://127.0.0.1:11434/api/generate \
  -H 'content-type: application/json' \
  -d '{"model":"granite4.1:3b","keep_alive":0}'
```

**`output already exists`**
Every output path is no-clobber, including a path left behind by a *failed*
run. Always use a fresh directory; never delete and reuse one mid-demo.

**A failed run left a directory containing `.incomplete`**
Working as designed. A failure retains only the mutually exclusive `.incomplete`
diagnostic form — never a partial pack that could be mistaken for evidence.
`execution.json` inside it carries `exit_classification` and the phase list, and
is the first thing to read.

**`OpenShell Gateway/VM driver tuple must be exactly 0.0.111`**
Version pin, not a suggestion. Match it or the run is `not_runnable`.

**Phase 4 rejects the control token**
Re-run `local-stack/scripts/bootstrap.sh` to reconcile the permission set (§2).

**Skill digest mismatch**
Re-run `openbox init --provider claude-code`.

---

## 8. Re-running and cleanup

`evaluate` cleans up after itself — sandbox, registry tag, container, volume,
and the loaded model are all released, and `execution.json` records each
outcome under `cleanup`.

Each run needs fresh output paths. Old runs are self-contained under
`testbed/.state/project-assurance-demo/` and can be deleted wholesale; the packs
are read-only (`0500`/`0400`), so removal needs `chmod -R u+w` first.

To rehearse without the stack, render a previously sealed pack — verification
and rendering are fully offline:

```sh
openbox project report --pack <sealed-report-pack> --format markdown
```

Keeping one known-good sealed pack is worthwhile: the scenario depends on a
small local model actually selecting the forced tool, and a run where it does
not yields a truthful `no_supported_issue` — correct behavior, but not the
demo you wanted in front of an audience.
