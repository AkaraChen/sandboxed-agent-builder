// Entry of the agent control API: `agent-entrypoint serve` runs this after
// the startup scripts. Configuration is environment variables only
// (docs/agent-api.md).
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { serve } from "@hono/node-server";
import { createAgentApi } from "./app.js";

const env = process.env;
const manifest = JSON.parse(readFileSync(env.AGENT_MANIFEST ?? "/opt/agent/manifest.json", "utf8"));
const token = env.AGENT_API_TOKEN || undefined;
if (!token && env.AGENT_API_INSECURE !== "1") {
  console.error("agent api: set AGENT_API_TOKEN (or AGENT_API_INSECURE=1 on a trusted network)");
  process.exit(64);
}
const codexPath = env.AGENT_CODEX_PATH ?? "/usr/local/bin/codex";
let version;
try {
  version = execFileSync(codexPath, ["--version"], { encoding: "utf8" }).trim();
} catch (err) {
  console.error(`agent api: ${codexPath} does not run`, err.message);
  process.exit(69);
}

const { app, close } = createAgentApi({
  codexPath,
  manifest,
  version,
  token,
  cwd: env.AGENT_API_CWD ?? "/workspace",
  maxSessions: Number(env.AGENT_API_MAX_SESSIONS ?? 4),
});

const port = Number(env.AGENT_API_PORT ?? 8080);
const hostname = env.AGENT_API_HOST ?? "0.0.0.0";
const server = serve({ fetch: app.fetch, port, hostname }, () => {
  console.error(`agent api: ${version} on http://${hostname}:${port}`);
});

let stopping = false;
for (const signal of ["SIGTERM", "SIGINT"]) {
  process.on(signal, async () => {
    if (stopping) process.exit(1);
    stopping = true;
    server.close();
    await close();
    process.exit(0);
  });
}
