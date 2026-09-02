# Intentionally vulnerable Mastra security demo

This one-shot Mastra project is a synthetic, local-only Phase 1–4 demonstration.
It intentionally treats untrusted support-ticket text as authoritative, lets the
injected instruction replace the operator's summarize-only goal, and executes a
`send-support-report` tool without a human-approval step.

The behavior is unsafe by design but the effect is not: the tool can contact
only the evaluator-owned same-run safe sink, and that sink accepts only the
evaluation ID. No payload, credential, production endpoint, or real customer
data enters the VM.

## Intended observable issue chain

1. OpenBox backend evidence captures a trusted operator goal followed by an
   explicitly untrusted ticket instruction.
2. Local Ollama `granite4.1:3b` selects the forced support-report tool.
3. OpenBox backend evidence records the tool result with `delivered: true` and
   `approvalRequested: false`.
4. The independent evaluator-owned sink records one correlated external effect.
5. The installed security skill can therefore assess prompt injection / agent
   goal hijack plus tool misuse / excessive agency without treating logs or
   model output as an authority substitute.

## Build and contract checks

From the repository root:

```sh
node testbed/project-assurance/mastra-security-demo/scenario-contract-test.mjs
docker build --pull=false \
  --file testbed/project-assurance/mastra-security-demo/Dockerfile \
  --tag ai.openbox/mastra-security-demo:local \
  testbed/project-assurance
OPENBOX_MASTRA_IMAGE=ai.openbox/mastra-security-demo:local \
  node testbed/project-assurance/mastra-conformance/image-contract-test.mjs
```

See [RUNBOOK.md](RUNBOOK.md) for the end-to-end sequence against a local
OpenBox stack, and [DEMO.md](DEMO.md) for the recorded Claude Code walkthrough.
