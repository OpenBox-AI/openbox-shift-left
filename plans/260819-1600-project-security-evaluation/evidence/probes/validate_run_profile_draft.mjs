#!/usr/bin/env node

import crypto from "node:crypto";
import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import process from "node:process";


const [ajvRootArg, schemaArg, exampleArg] = process.argv.slice(2);
if (!ajvRootArg || !schemaArg || !exampleArg) {
  throw new Error("usage: validate_run_profile_draft.mjs AJV_ROOT SCHEMA EXAMPLE");
}

const ajvRoot = path.resolve(ajvRootArg);
const schemaPath = path.resolve(schemaArg);
const examplePath = path.resolve(exampleArg);
const require = createRequire(import.meta.url);
const Ajv2020 = require(path.join(ajvRoot, "dist", "2020")).default;
const packageJSON = require(path.join(ajvRoot, "package.json"));
if (packageJSON.name !== "ajv" || packageJSON.version !== "8.17.1") {
  throw new Error(`unexpected Ajv package: ${packageJSON.name}@${packageJSON.version}`);
}

const schemaBytes = fs.readFileSync(schemaPath);
const exampleBytes = fs.readFileSync(examplePath);
const maxProfileBytes = 262144;
const maxProfileDepth = 32;
const maxTemplateBytes = 65536;
const maxTemplateDepth = 16;

function lexicalJSONDepth(bytes) {
  let depth = 0;
  let maximum = 0;
  let inString = false;
  let escaped = false;
  for (const character of bytes.toString("utf8")) {
    if (inString) {
      if (escaped) escaped = false;
      else if (character === "\\") escaped = true;
      else if (character === '"') inString = false;
    } else if (character === '"') {
      inString = true;
    } else if (character === "{" || character === "[") {
      depth += 1;
      maximum = Math.max(maximum, depth);
    } else if (character === "}" || character === "]") {
      depth -= 1;
    }
  }
  return maximum;
}

if (exampleBytes.length > maxProfileBytes) throw new Error("example exceeds profile byte limit");
const exampleLexicalDepth = lexicalJSONDepth(exampleBytes);
if (exampleLexicalDepth > maxProfileDepth) throw new Error("example exceeds profile depth limit");
const schema = JSON.parse(schemaBytes);
const example = JSON.parse(exampleBytes);
const ajv = new Ajv2020({ allErrors: true, strict: true });
const validate = ajv.compile(schema);

const allowedTemplateVariables = new Set([
  "fixture.poison.url",
  "fixture.sink.url",
  "model.url",
  "run.marker",
  "scenario.id",
]);
const bindingNamesBySource = new Map([
  ["application.listen_host", "APP_HOST"],
  ["application.listen_port", "APP_PORT"],
  ["fixture.poison_url", "POISON_FIXTURE_URL"],
  ["fixture.sink_url", "SAFE_SINK_URL"],
  ["fixture.model_url", "MODEL_BASE_URL"],
  ["run.marker", "SCENARIO_MARKER"],
  ["scenario.id", "SCENARIO_ID"],
]);

function inspectTemplate(value) {
  const found = new Set();
  const errors = [];
  const pending = [{ value, location: "$" }];
  while (pending.length > 0) {
    const current = pending.pop();
    if (Array.isArray(current.value)) {
      current.value.forEach((item, index) => {
        pending.push({ value: item, location: `${current.location}[${index}]` });
      });
    } else if (current.value !== null && typeof current.value === "object") {
      for (const [key, item] of Object.entries(current.value)) {
        if (/[{}]/.test(key)) {
          errors.push(`template syntax is forbidden in object key: ${current.location}.${key}`);
        }
        pending.push({ value: item, location: `${current.location}.${key}` });
      }
    } else if (typeof current.value === "string" && /[{}]/.test(current.value)) {
      const match = /^\{\{([^{}]+)\}\}$/.exec(current.value);
      if (!match) {
        errors.push(`template token must occupy a complete string value: ${current.location}`);
      } else if (!allowedTemplateVariables.has(match[1])) {
        errors.push(`unsupported template variable: ${match[1]}`);
      } else {
        found.add(match[1]);
      }
    }
  }
  return { found, errors };
}

function decimalCost(profile) {
  return Number(profile.budgets?.maxCostUsd);
}

function valueDepth(value) {
  let maximum = 0;
  const pending = [{ value, depth: 1 }];
  while (pending.length > 0) {
    const current = pending.pop();
    maximum = Math.max(maximum, current.depth);
    if (Array.isArray(current.value)) {
      for (const item of current.value) pending.push({ value: item, depth: current.depth + 1 });
    } else if (current.value !== null && typeof current.value === "object") {
      for (const item of Object.values(current.value)) {
        pending.push({ value: item, depth: current.depth + 1 });
      }
    }
  }
  return maximum;
}

function semanticErrors(profile, trustedRelayDescriptors = new Map()) {
  const errors = [];
  const bindings = profile.environment?.generatedBindings ?? [];
  const bindingNames = bindings.map((binding) => binding.name);
  const bindingSources = bindings.map((binding) => binding.source);
  if (new Set(bindingNames).size !== bindingNames.length) {
    errors.push("generated binding names must be unique");
  }
  if (new Set(bindingSources).size !== bindingSources.length) {
    errors.push("generated binding sources must be unique");
  }
  const staticNames = (profile.environment?.static ?? []).map((binding) => binding.name);
  if (new Set(staticNames).size !== staticNames.length) {
    errors.push("static environment names must be unique");
  }
  if (bindingNames.some((name) => staticNames.includes(name))) {
    errors.push("static and generated environment names must not overlap");
  }
  for (const binding of bindings) {
    if (bindingNamesBySource.get(binding.source) !== binding.name) {
      errors.push(`binding name does not match source: ${binding.name}:${binding.source}`);
    }
  }

  const model = profile.fixtures?.model;
  const modelBearer = model?.bearerEnvironment;
  if (modelBearer && (bindingNames.includes(modelBearer) || staticNames.includes(modelBearer))) {
    errors.push("model relay bearer environment must be runner-owned");
  }
  const requiredBindings = [
    [profile.application?.listen?.portEnvironment, "application.listen_port"],
    [profile.fixtures?.poison?.urlEnvironment, "fixture.poison_url"],
    [profile.fixtures?.sink?.urlEnvironment, "fixture.sink_url"],
    [model?.urlEnvironment, "fixture.model_url"],
  ];
  for (const [name, source] of requiredBindings) {
    if (!bindings.some((binding) => binding.name === name && binding.source === source)) {
      errors.push(`missing generated binding ${name}:${source}`);
    }
  }

  const template = inspectTemplate(profile.application?.stimulus?.bodyTemplate);
  errors.push(...template.errors);
  const templateValue = profile.application?.stimulus?.bodyTemplate;
  if (valueDepth(templateValue) > maxTemplateDepth) {
    errors.push("stimulus body template exceeds depth limit");
  }
  const templateBytes = Buffer.byteLength(JSON.stringify(templateValue), "utf8");
  if (templateBytes > maxTemplateBytes || templateBytes > profile.budgets?.maxRequestBytes) {
    errors.push("stimulus body template exceeds byte limit");
  }

  const readiness = profile.application?.readiness;
  const budgets = profile.budgets;
  if (readiness?.intervalMs > readiness?.startupTimeoutMs) {
    errors.push("readiness interval must not exceed startup timeout");
  }
  if (readiness?.startupTimeoutMs + budgets?.cleanupGraceMs > budgets?.maxDurationMs) {
    errors.push("readiness timeout plus cleanup grace must fit within maxDurationMs");
  }

  if (model?.mode === "authorized_relay") {
    const descriptor = trustedRelayDescriptors.get(model.descriptor);
    if (!descriptor) {
      errors.push(`relay descriptor is not trusted: ${model.descriptor}`);
    } else {
      for (const field of [
        "urlEnvironment",
        "bearerEnvironment",
        "provider",
        "destination",
        "pathFamily",
        "method",
        "followRedirects",
      ]) {
        if (model[field] !== descriptor[field]) {
          errors.push(`relay ${field} does not match trusted descriptor`);
        }
      }
      if (!descriptor.models.has(model.model)) {
        errors.push("relay model does not match trusted descriptor");
      }
    }
    if (budgets?.maxInputTokens < 1 || budgets?.maxOutputTokens < 1) {
      errors.push("relay token budgets must be non-zero");
    }
    if (decimalCost(profile) !== 0) {
      errors.push("local relay monetary cost budget must be zero");
    }
  } else if (model?.mode === "deterministic_local" && decimalCost(profile) !== 0) {
    errors.push("deterministic local model cost budget must be zero");
  }
  return errors;
}

function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

function schemaRejects(name, mutate, base = example) {
  const candidate = clone(base);
  mutate(candidate);
  const valid = validate(candidate);
  if (valid) throw new Error(`schema accepted negative fixture: ${name}`);
  return {
    name,
    rejected: true,
    keywords: [...new Set((validate.errors ?? []).map((error) => error.keyword))].sort(),
  };
}

function semanticRejects(name, mutate, base = example, descriptors = new Map()) {
  const candidate = clone(base);
  mutate(candidate);
  if (!validate(candidate)) {
    throw new Error(`semantic negative failed schema before semantic check: ${name}: ${JSON.stringify(validate.errors)}`);
  }
  const errors = semanticErrors(candidate, descriptors);
  if (errors.length === 0) throw new Error(`semantic validator accepted negative fixture: ${name}`);
  return { name, schemaValid: true, semanticRejected: true, errors };
}

if (!validate(example)) {
  throw new Error(`valid example failed schema: ${JSON.stringify(validate.errors)}`);
}
const positiveSemanticErrors = semanticErrors(example);
if (positiveSemanticErrors.length !== 0) {
  throw new Error(`valid example failed semantics: ${positiveSemanticErrors.join("; ")}`);
}

const relayDeclaration = clone(example);
relayDeclaration.fixtures.model = {
  mode: "authorized_relay",
  urlEnvironment: "MODEL_BASE_URL",
  bearerEnvironment: "MODEL_API_KEY",
  descriptor: "ollama.chat.granite4.1-3b.6fd349357287",
  provider: "ollama",
  model: "granite4.1:3b",
  destination: "http://127.0.0.1:11434",
  pathFamily: "/api/chat",
  method: "POST",
  followRedirects: false,
  dataPosture: "prompt_and_completion",
};
relayDeclaration.budgets.maxCostUsd = "0.00";
const qualificationRelayDescriptors = new Map([
  [
    "ollama.chat.granite4.1-3b.6fd349357287",
    {
      provider: "ollama",
      urlEnvironment: "MODEL_BASE_URL",
      bearerEnvironment: "MODEL_API_KEY",
      destination: "http://127.0.0.1:11434",
      pathFamily: "/api/chat",
      method: "POST",
      followRedirects: false,
      models: new Set(["granite4.1:3b"]),
    },
  ],
]);
const driftedRelayDescriptors = new Map([
  [
    "ollama.chat.granite4.1-3b.6fd349357287",
    {
      ...qualificationRelayDescriptors.get("ollama.chat.granite4.1-3b.6fd349357287"),
      destination: "http://127.0.0.1:11435",
    },
  ],
]);
if (!validate(relayDeclaration)) {
  throw new Error(`authorized relay shape failed: ${JSON.stringify(validate.errors)}`);
}
const relaySemanticErrors = semanticErrors(relayDeclaration, qualificationRelayDescriptors);
if (relaySemanticErrors.length !== 0) {
  throw new Error(`trusted relay tuple failed semantics: ${relaySemanticErrors.join("; ")}`);
}

const templateInspection = inspectTemplate(example.application.stimulus.bodyTemplate);
const result = {
  schema: "openbox.project-assurance.run-profile-draft-validation/v1",
  validator: {
    package: packageJSON.name,
    version: packageJSON.version,
    draft: "2020-12",
    strict: true,
    allErrors: true,
  },
  inputs: {
    schema: path.basename(schemaPath),
    schemaSha256: crypto.createHash("sha256").update(schemaBytes).digest("hex"),
    example: path.basename(examplePath),
    exampleSha256: crypto.createHash("sha256").update(exampleBytes).digest("hex"),
  },
  positive: {
    schemaValid: true,
    semanticValid: true,
    trustedRelayTupleValid: true,
    profileBytes: exampleBytes.length,
    profileLexicalDepth: exampleLexicalDepth,
    profileLimits: { maxBytes: maxProfileBytes, maxDepth: maxProfileDepth },
    templateLimits: { maxBytes: maxTemplateBytes, maxDepth: maxTemplateDepth },
    templateVariables: [...templateInspection.found].sort(),
  },
  schemaNegatives: [
    schemaRejects("hidden command", (profile) => { profile.command = ["python", "app.py"]; }),
    schemaRejects("production OpenBox coordinate", (profile) => {
      profile.environment.static.push({ name: "OPENBOX_BASE_URL", value: "core-production" });
    }),
    schemaRejects("OpenBox generated-binding override", (profile) => {
      profile.environment.generatedBindings[0].name = "OPENBOX_BASE_URL";
    }),
    schemaRejects("credential-like generated binding", (profile) => {
      profile.environment.generatedBindings[0].name = "MODEL_API_KEY";
    }),
    schemaRejects("runtime-control static environment", (profile) => {
      profile.environment.static.push({ name: "NODE_OPTIONS", value: "inspect" });
    }),
    schemaRejects("host-control generated binding", (profile) => {
      profile.environment.generatedBindings[0].name = "PATH";
    }),
    schemaRejects("raw retention", (profile) => { profile.retention.mode = "raw"; }),
    schemaRejects("receiver override", (profile) => {
      profile.receiverUrl = "https://core.openbox.example";
    }),
    schemaRejects("URL-valued static environment", (profile) => {
      profile.environment.static.push({ name: "SERVICE_URL", value: "https://core.openbox.example" });
    }),
    schemaRejects("dot-segment readiness path", (profile) => {
      profile.application.readiness.path = "/../admin";
    }),
    schemaRejects("unsupported SDK action class", (profile) => {
      profile.sdk.requiredActionClasses = ["unsupported_action"];
    }),
    schemaRejects("non-MVP SDK descriptor", (profile) => {
      profile.sdk.descriptor = "openbox-langgraph-sdk-python@1.0.0+openbox-sdk-python@1.2.0";
    }),
    schemaRejects("informational completion status", (profile) => {
      profile.application.stimulus.completion.expectedStatuses = [101];
    }),
    schemaRejects("unsafe relay bearer name", (profile) => {
      profile.fixtures.model = clone(relayDeclaration.fixtures.model);
      profile.fixtures.model.bearerEnvironment = "PATH";
    }),
    schemaRejects("remote relay destination", (profile) => {
      profile.fixtures.model = clone(relayDeclaration.fixtures.model);
      profile.fixtures.model.destination = "https://api.openai.com";
    }),
    schemaRejects("relay redirects enabled", (profile) => {
      profile.fixtures.model = clone(relayDeclaration.fixtures.model);
      profile.fixtures.model.followRedirects = true;
    }),
  ],
  semanticNegatives: [
    semanticRejects("unknown template variable", (profile) => {
      profile.application.stimulus.bodyTemplate.marker = "{{env.OPENBOX_API_KEY}}";
    }),
    semanticRejects("malformed template syntax", (profile) => {
      profile.application.stimulus.bodyTemplate.marker = "{{run.marker}";
    }),
    semanticRejects("interpolated template token", (profile) => {
      profile.application.stimulus.bodyTemplate.marker = "prefix-{{run.marker}}";
    }),
    semanticRejects("templated object key", (profile) => {
      profile.application.stimulus.bodyTemplate["{{run.marker}}"] = "value";
    }),
    semanticRejects("aliased poison and sink binding", (profile) => {
      profile.fixtures.sink.urlEnvironment = profile.fixtures.poison.urlEnvironment;
      profile.environment.generatedBindings = profile.environment.generatedBindings.filter(
        (binding) => binding.source !== "fixture.sink_url",
      );
    }),
    semanticRejects("duplicate generated source alias", (profile) => {
      profile.environment.generatedBindings.push({ name: "SCENARIO_ID", source: "run.marker" });
    }),
    semanticRejects("binding name does not match source", (profile) => {
      profile.application.listen.portEnvironment = "SCENARIO_ID";
      profile.environment.generatedBindings[0].name = "SCENARIO_ID";
    }),
    semanticRejects("readiness exceeds total duration", (profile) => {
      profile.budgets.maxDurationMs = 100;
    }),
    semanticRejects("readiness interval exceeds timeout", (profile) => {
      profile.application.readiness.startupTimeoutMs = 100;
      profile.application.readiness.intervalMs = 200;
    }),
    semanticRejects("cleanup exceeds total duration", (profile) => {
      profile.budgets.maxDurationMs = 1000;
      profile.budgets.cleanupGraceMs = 3000;
      profile.application.readiness.startupTimeoutMs = 200;
    }),
    semanticRejects("combined readiness and cleanup exceed duration", (profile) => {
      profile.application.readiness.startupTimeoutMs = 30000;
      profile.budgets.cleanupGraceMs = 30000;
      profile.budgets.maxDurationMs = 30000;
    }),
    semanticRejects("deep stimulus body template", (profile) => {
      let value = "leaf";
      for (let index = 0; index < maxTemplateDepth; index += 1) value = { value };
      profile.application.stimulus.bodyTemplate = value;
    }),
    semanticRejects("oversized stimulus body template", (profile) => {
      profile.application.stimulus.bodyTemplate = Array(5).fill("x".repeat(16384));
    }),
    semanticRejects("deterministic model with non-zero cost", (profile) => {
      profile.budgets.maxCostUsd = "0.01";
    }),
    semanticRejects("untrusted relay descriptor", () => {}, relayDeclaration),
    semanticRejects(
      "trusted relay descriptor destination drift",
      () => {},
      relayDeclaration,
      driftedRelayDescriptors,
    ),
    semanticRejects(
      "local relay with non-zero monetary cost",
      (profile) => { profile.budgets.maxCostUsd = "0.01"; },
      relayDeclaration,
      qualificationRelayDescriptors,
    ),
    semanticRejects(
      "relay with zero token budget",
      (profile) => { profile.budgets.maxInputTokens = 0; },
      relayDeclaration,
      qualificationRelayDescriptors,
    ),
  ],
};

process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
