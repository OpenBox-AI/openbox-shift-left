# Mastra one-shot evaluation image

This directory builds the deterministic conformance image for
`openbox project evaluate`. It is a self-starting, one-shot Mastra `1.8.0`
application using OpenBox Mastra SDK `1.0.0`. It proves runner, SDK, local
inference, and cleanup wiring; it is not customer security-behavior evidence.

The image contract is standard OCI metadata:

- platform `linux/arm64`;
- user/group `1000:1000`;
- OpenShell VM proxy helpers from Debian `iproute2` and `nftables`;
- `ai.openbox.project-evaluation.contract=v1` as the only OpenBox evaluation
  label;
- `ENTRYPOINT ["/usr/local/bin/node"]` and
  `CMD ["/app/src/index.mjs"]`.

There is no HTTP server, health endpoint, invoke endpoint, port, forward,
scenario, or alternate command. OpenShell replaces image command defaults, so
the evaluator inspects and passes the resolved OCI argv after `--`.

## Checks and build

```sh
npm ci --ignore-scripts
npm run check
npm run typecheck
docker build --pull=false --tag ai.openbox/mastra-conformance:local .
npm run image:test
```

`image:test` only validates the built OCI contract. It does not orchestrate
Docker or OpenShell execution.

## Public evaluation

The developer environment file contains only optional, non-secret application
defaults. OpenBox and OpenAI routing variables are reserved and injected by the
evaluator.

```sh
openbox project evaluate \
  --image ai.openbox/mastra-conformance:local \
  --env-file evaluation.env \
  --openbox-agent <dedicated-bearer-only-agent-uuid> \
  --output /absolute/new/execution-directory
```

The one-shot command requires and consumes:

- `OPENBOX_EVALUATION_ID` as its Mastra/OpenBox run identity;
- `OPENBOX_AGENT_ID` for conformance identity output;
- `OPENBOX_URL` and provider-supplied `OPENBOX_API_KEY`;
- `OPENAI_BASE_URL=https://inference.local/v1`;
- `OPENAI_API_KEY=unused`;
- `OPENAI_MODEL=granite4.1:3b`.

It forces exactly one `recording-tool` selection, emits the normal OpenBox
lifecycle/activity sequence, shuts down the SDK runtime, prints one bounded
non-secret completion object, and exits. A fixed two-second startup hold keeps
the one-shot process alive through the OpenShell VM driver's Ready transition;
it does not add an invocation endpoint or alternate completion signal.
