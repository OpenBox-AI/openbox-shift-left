# Recorded Claude Code walkthrough

Run the two demo scripts from the repository root, in order:

```sh
./testbed/project-assurance/mastra-security-demo/prepare-demo.zsh
./testbed/project-assurance/mastra-security-demo/launch-claude.zsh
```

`prepare-demo.zsh` is the preflight; it is idempotent and exits non-zero on any
unmet precondition. `launch-claude.zsh` then starts a fresh Claude Code session
from the repository root with the current locally built CLI first on `PATH`, the
local-stack control token in process memory, and new no-clobber output paths.
It prints those non-secret paths before Claude starts.

[RUNBOOK.md](RUNBOOK.md) covers the full end-to-end sequence, including the
direct-CLI lane that needs no agentic host for steps 1 and 3.

Use three prompts. The separation between prompts is a product boundary: the
installed Phase 3 skill may create only an issue candidate, while the separate
Phase 4 finalizer owns OpenBox integration recommendations.

## Prompt 1 — observe the project

```text
Run the prepared OpenBox Mastra security demo evaluation now. Use the image, agent, environment file, and new observation path from OPENBOX_DEMO_IMAGE, OPENBOX_DEMO_AGENT_ID, OPENBOX_DEMO_ENV, and OPENBOX_DEMO_OBSERVATION. Invoke only `openbox project evaluate` with those four inputs. When it finishes, verify the sealed observation pack and summarize what was observed without performing security analysis yet.
```

Expected visible boundary: `project observation sealed`, followed by exact
public verification of `ai.openbox.project-observation/v1`.

## Prompt 2 — issue-only security analysis

```text
Explicitly invoke `openbox-security-evaluation` with OPENBOX_DEMO_OBSERVATION as the verified observation pack and OPENBOX_DEMO_CANDIDATE as the new candidate JSON. Follow the installed skill exactly. Do not finalize the report in this turn.
```

Expected visible boundary: a new mode-0600 issue-only candidate plus the future
`openbox project finalize` command. The skill itself makes no OpenBox call and
does not recommend or apply a control.

## Prompt 3 — feedback and OpenBox integration

```text
Now run `openbox project finalize` using OPENBOX_DEMO_OBSERVATION, OPENBOX_DEMO_CANDIDATE, and the new OPENBOX_DEMO_REPORT directory. Verify the sealed report, render it with `openbox project report --format markdown`, then display the Markdown and walk me through the evidence-backed security feedback and the suggested OpenBox integration. Clearly distinguish observed behavior, coverage limitations, and inert recommendations. Do not apply, publish, approve, or verify any control.
```

Expected visible boundary: a sealed `ai.openbox.project-security-report/v1`
pack and Markdown containing evidence-linked issues plus inert OpenBox
integration recommendations. Read through the issue, evidence, standards,
current posture, recommendations, and limitations on screen.
