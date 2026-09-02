import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const imageName = process.env.OPENBOX_MASTRA_IMAGE ?? "ai.openbox/mastra-conformance:local";
const { stdout } = await execFileAsync("docker", ["image", "inspect", imageName], {
  encoding: "utf8",
  maxBuffer: 4_194_304
});
const images = JSON.parse(stdout);
if (!Array.isArray(images) || images.length !== 1) throw new Error("image_not_unique");
const image = images[0];
const labels = image.Config?.Labels ?? {};
const openboxLabels = Object.fromEntries(
  Object.entries(labels).filter(([name]) => name.startsWith("ai.openbox.project-evaluation."))
);
if (
  image.Os !== "linux" ||
  image.Architecture !== "arm64" ||
  image.Config?.User !== "1000:1000" ||
  JSON.stringify(image.Config?.Entrypoint) !== JSON.stringify(["/usr/local/bin/node"]) ||
  JSON.stringify(image.Config?.Cmd) !== JSON.stringify(["/app/src/index.mjs"]) ||
  JSON.stringify(openboxLabels) !== JSON.stringify({
    "ai.openbox.project-evaluation.contract": "v1"
  }) ||
  image.Config?.Healthcheck ||
  image.Config?.ExposedPorts
) {
  throw new Error("image_contract_mismatch");
}
console.log(JSON.stringify({ image: imageName, id: image.Id, status: "passed" }));
