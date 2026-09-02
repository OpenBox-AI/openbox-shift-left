import { Agent } from "@mastra/core/agent";
import { Mastra } from "@mastra/core/mastra";
import { createTool } from "@mastra/core/tools";
import {
  getOpenBoxRuntime,
  runWithOpenBoxEventMetadata,
  withOpenBox
} from "@openbox-ai/openbox-mastra-sdk";
import { z } from "zod";

const evaluationId = requiredEnvironment("OPENBOX_EVALUATION_ID");
const openboxAgentId = exactUUID(requiredEnvironment("OPENBOX_AGENT_ID"));
const openboxUrl = exactOpenBoxURL(requiredEnvironment("OPENBOX_URL"));
const modelBaseURL = exactModelURL(requiredEnvironment("OPENAI_BASE_URL"));
const modelAPIKey = requiredEnvironment("OPENAI_API_KEY");
const modelName = requiredEnvironment("OPENAI_MODEL");
const safeSinkURL = exactSafeSinkURL(requiredEnvironment("OPENBOX_SAFE_SINK_URL"));

if (modelAPIKey !== "unused") throw new Error("invalid_model_api_key");
if (modelName !== "granite4.1:3b") throw new Error("invalid_model");

// Keep the one-shot command alive long enough for the VM driver to publish its
// Ready transition before the first governed activity can complete.
await delay(2_000);

let toolExecutions = 0;
const recordingTool = createTool({
  id: "recording-tool",
  description: "Record the deterministic local evaluation step.",
  inputSchema: z.object({}).strict(),
  outputSchema: z.object({ recorded: z.literal(true) }),
  async execute() {
    toolExecutions += 1;
    if (toolExecutions !== 1) throw new Error("tool_execution_count");
    return { recorded: true };
  }
});

const agent = new Agent({
  id: "project-evaluation-agent",
  name: "OpenBox project evaluation conformance agent",
  instructions: "Execute the recording-tool exactly once and return no additional work.",
  model: {
    id: `custom/${modelName}`,
    url: modelBaseURL,
    apiKey: modelAPIKey
  },
  tools: { "recording-tool": recordingTool }
});

const mastra = new Mastra({
  agents: { evaluationAgent: agent },
  tools: { "recording-tool": recordingTool }
});

const governed = await withOpenBox(mastra, {
  apiUrl: openboxUrl,
  apiKey: requiredEnvironment("OPENBOX_API_KEY"),
  fetch: diagnosticFetch,
  validate: true,
  onApiError: "fail_closed",
  sendActivityStartEvent: true,
  evaluateMaxRetries: 0,
  governanceTimeout: 5,
  hitlEnabled: true,
  httpCapture: false,
  instrumentDatabases: false,
  instrumentFileIo: false
});

try {
  const generation = await runWithOpenBoxEventMetadata(
    {
      openbox_assurance: {
        input_trust: "trusted",
        source_kind: "local_evaluation",
        source_id: evaluationId,
        scenario_id: evaluationId
      }
    },
    async () => governed.getAgent("evaluationAgent").generate(
      "Perform the single deterministic evaluation activity now.",
      {
        maxSteps: 1,
        runId: evaluationId,
        toolChoice: { type: "tool", toolName: "recording-tool" }
      }
    )
  );
  const selected = generation.toolCalls?.filter(
    call => call.payload.toolName === "recording-tool"
  ) ?? [];
  const completed = generation.toolResults?.filter(
    result => result.payload.toolName === "recording-tool" &&
      isRecordedResult(result.payload.result)
  ) ?? [];
  if (selected.length !== 1 || completed.length !== 1 || toolExecutions !== 1) {
	throw new Error("deterministic_step_incomplete");
  }
  const effectResponse = await fetch(safeSinkURL, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ evaluation_id: evaluationId })
  });
  if (effectResponse.status !== 204) throw new Error("safe_effect_not_receipted");
  console.log(JSON.stringify({
    status: "completed",
    classification: "deterministic_tool_selection_conformance",
    evaluation_id: evaluationId,
    openbox_agent_id: openboxAgentId,
    model: modelName,
    scope: "runner_sdk_inference_cleanup_wiring_only"
  }));
} finally {
  await getOpenBoxRuntime(governed)?.shutdown();
}

function requiredEnvironment(name) {
  const value = process.env[name];
  if (!value || value.length > 4_096 || value.includes("\0") || value.includes("\n")) {
    throw new Error(`invalid_environment_${name.toLowerCase()}`);
  }
  return value;
}

async function diagnosticFetch(input, init) {
  const response = await fetch(input, init);
  if (!response.ok) {
    let error = "unclassified";
    let policy = "unclassified";
    let method = "unclassified";
    let path = "unclassified";
    try {
      const body = await response.clone().json();
      if (typeof body?.error === "string" && /^[a-z][a-z0-9_]{0,63}$/.test(body.error)) {
        error = body.error;
      }
      if (typeof body?.policy === "string" && /^[a-z][a-z0-9_-]{0,63}$/.test(body.policy)) {
        policy = body.policy;
      }
      if (typeof body?.method === "string" && /^(GET|POST)$/.test(body.method)) {
        method = body.method;
      }
      if (typeof body?.path === "string" && /^\/[a-z0-9_/-]{1,127}$/.test(body.path)) {
        path = body.path;
      }
    } catch {
      // Preserve only the bounded classification above; response text is evidence-hostile.
    }
    console.error(JSON.stringify({
      channel: "openbox_http", status: response.status, error, policy, method, path
    }));
  }
  return response;
}

function exactUUID(value) {
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value)) {
    throw new Error("invalid_openbox_agent_id");
  }
  return value;
}

function exactOpenBoxURL(value) {
  const parsed = new URL(value);
  if (
    parsed.href !== `${value}/` ||
    parsed.protocol !== "http:" ||
    parsed.hostname !== "host.openshell.internal" ||
    !parsed.port ||
    parsed.username ||
    parsed.password ||
    parsed.pathname !== "/" ||
    parsed.search ||
    parsed.hash
  ) {
    throw new Error("invalid_openbox_url");
  }
  return value;
}

function exactModelURL(value) {
  if (value !== "https://inference.local/v1") throw new Error("invalid_model_url");
  return value;
}

function exactSafeSinkURL(value) {
  const parsed = new URL(value);
  if (
    parsed.protocol !== "http:" ||
    parsed.hostname !== "host.openshell.internal" ||
    !parsed.port ||
    parsed.pathname !== "/effects/safe" ||
    parsed.search || parsed.hash || parsed.username || parsed.password
  ) {
    throw new Error("invalid_safe_sink_url");
  }
  return value;
}

function isRecordedResult(value) {
  return Boolean(value && typeof value === "object" && "recorded" in value && value.recorded === true);
}

function delay(milliseconds) {
  return new Promise(resolve => setTimeout(resolve, milliseconds));
}
