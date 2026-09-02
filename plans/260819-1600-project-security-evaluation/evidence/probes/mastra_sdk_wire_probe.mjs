#!/usr/bin/env node

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { createServer } from "node:http";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const sdkRoot = process.argv[2];

if (!sdkRoot) {
  console.error("usage: mastra_sdk_wire_probe.mjs <built-sdk-root>");
  process.exit(2);
}

const requests = [];
const effects = [];
const nonLoopbackAttempts = [];
const server = createServer(async (request, response) => {
  const chunks = [];

  for await (const chunk of request) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  }

  const body = Buffer.concat(chunks);
  const pathname = new URL(request.url ?? "/", "http://127.0.0.1").pathname;
  const parsedBody = body.length === 0 ? {} : JSON.parse(body.toString("utf8"));
  let responseBody;

  if (pathname === "/api/v1/auth/validate") {
    responseBody = Buffer.from('{"valid":true}');
  } else if (pathname === "/api/v1/governance/evaluate") {
    const shouldBlock =
      parsedBody.event_type === "ActivityStarted" &&
      parsedBody.run_id === "mastra-wire-block";
    responseBody = Buffer.from(
      shouldBlock
        ? '{"verdict":"block","reason":"qualification mock only"}'
        : '{"verdict":"allow"}'
    );
  } else {
    responseBody = Buffer.from('{"error":"not_found"}');
  }

  requests.push({
    body_base64: body.toString("base64"),
    body_sha256: sha256(body),
    effect_count_at_receive: effects.length,
    event_type:
      typeof parsedBody.event_type === "string" ? parsedBody.event_type : null,
    headers: {
      authorization:
        request.headers.authorization === "Bearer obx_test_mastra_qualification"
          ? "Bearer obx_test_[REDACTED]"
          : request.headers.authorization ?? null,
      body_sha256: request.headers["x-openbox-body-sha256"] ?? null,
      content_type: request.headers["content-type"] ?? null,
      user_agent: request.headers["user-agent"] ?? null
    },
    method: request.method ?? null,
    path: pathname,
    response_body_base64: responseBody.toString("base64"),
    response_body_sha256: sha256(responseBody),
    run_id: typeof parsedBody.run_id === "string" ? parsedBody.run_id : null,
    status: pathname === "/api/v1/auth/validate" ||
      pathname === "/api/v1/governance/evaluate" ? 200 : 404
  });

  response.writeHead(requests.at(-1).status, {
    "content-type": "application/json"
  });
  response.end(responseBody);
});

await new Promise(resolveListen => server.listen(0, "127.0.0.1", resolveListen));

try {
  const address = server.address();
  assert(address && typeof address !== "string");
  const apiUrl = `http://127.0.0.1:${address.port}`;
  const nativeFetch = globalThis.fetch;

  globalThis.fetch = async (input, init) => {
    const target = new URL(
      typeof input === "string" || input instanceof URL ? input : input.url
    );

    if (target.hostname !== "127.0.0.1") {
      nonLoopbackAttempts.push(target.origin);
      throw new Error(`non-loopback request rejected: ${target.origin}`);
    }

    return nativeFetch(input, init);
  };

  const sdk = await import(
    pathToFileURL(resolve(sdkRoot, "dist/index.js")).href
  );
  const { Mastra } = await import(
    pathToFileURL(resolve(sdkRoot, "node_modules/@mastra/core/dist/mastra/index.js"))
      .href
  );
  const { createTool } = await import(
    pathToFileURL(resolve(sdkRoot, "node_modules/@mastra/core/dist/tools/index.js"))
      .href
  );
  const mastra = new Mastra({
    tools: {
      recording: createTool({
        description: "Record a bounded synthetic effect",
        id: "recording-tool",
        async execute(input) {
          effects.push(input.value);
          return { recorded: input.value };
        }
      })
    }
  });
  const governed = await sdk.withOpenBox(mastra, {
    apiKey: "obx_test_mastra_qualification",
    apiUrl,
    evaluateMaxRetries: 0,
    governanceTimeout: 5,
    hitlEnabled: true,
    httpCapture: false,
    instrumentDatabases: false,
    instrumentFileIo: false,
    onApiError: "fail_closed",
    sendActivityStartEvent: true,
    validate: true
  });
  const runtime = sdk.getOpenBoxRuntime(governed);
  assert(runtime);
  const config = runtime.config;
  const wrapped = governed.getTool("recording");
  const contextFor = runId => ({
    workflow: {
      runId,
      setState() {},
      state: {},
      async suspend() {},
      workflowId: "mastra-wire-workflow"
    }
  });

  const allowResult = await wrapped.execute(
    { value: "allow-effect" },
    contextFor("mastra-wire-allow")
  );
  let blockException = null;

  try {
    await wrapped.execute(
      { value: "blocked-effect" },
      contextFor("mastra-wire-block")
    );
  } catch (error) {
    blockException = error?.constructor?.name ?? String(error);
  }

  assert.deepEqual(allowResult, { recorded: "allow-effect" });
  assert.deepEqual(effects, ["allow-effect"]);
  assert.equal(blockException, "GovernanceHaltError");
  assert.deepEqual(
    requests.map(({ event_type: eventType, path, run_id: runId }) => ({
      event_type: eventType,
      path,
      run_id: runId
    })),
    [
      {
        event_type: null,
        path: "/api/v1/auth/validate",
        run_id: null
      },
      {
        event_type: "ActivityStarted",
        path: "/api/v1/governance/evaluate",
        run_id: "mastra-wire-allow"
      },
      {
        event_type: "ActivityCompleted",
        path: "/api/v1/governance/evaluate",
        run_id: "mastra-wire-allow"
      },
      {
        event_type: "ActivityStarted",
        path: "/api/v1/governance/evaluate",
        run_id: "mastra-wire-block"
      }
    ]
  );
  assert.equal(requests[1].effect_count_at_receive, 0);
  assert.equal(requests[3].effect_count_at_receive, 1);
  assert.deepEqual(nonLoopbackAttempts, []);
  await runtime.shutdown();

  process.stdout.write(
    `${JSON.stringify(
      {
        allow_result: allowResult,
        block_exception: blockException,
        classification: {
          allow: "baseline_allow_wire_observed",
          mock_block: "sdk_mock_interception_only_not_openbox_block_proof"
        },
        configuration: {
          api_key: "obx_test_[REDACTED]",
          api_url: "http://127.0.0.1:[ephemeral]",
          on_api_error: config.onApiError,
          validate: config.validate
        },
        effects,
        non_loopback_attempts: nonLoopbackAttempts,
        requests
      },
      null,
      2
    )}\n`
  );
} finally {
  await new Promise((resolveClose, reject) => {
    server.close(error => error ? reject(error) : resolveClose());
  });
}

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}
