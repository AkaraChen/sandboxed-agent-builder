#!/usr/bin/env node
// A stand-in for `codex exec --experimental-json`: reads the prompt on
// stdin and writes thread events, like the real CLI. Prompts: "slow" waits
// to be killed, "fail" fails the turn, "env" reports what it was given.
import { randomUUID } from "node:crypto";

const args = process.argv.slice(2);
const arg = (flag) => (args.includes(flag) ? args[args.indexOf(flag) + 1] : undefined);
const config = (key) => {
  for (let i = 0; i < args.length; i++) if (args[i] === "--config" && args[i + 1].startsWith(key + "=")) return args[i + 1].slice(key.length + 1);
};
const emit = (e) => process.stdout.write(JSON.stringify(e) + "\n");

let prompt = "";
for await (const chunk of process.stdin) prompt += chunk;
const resumed = arg("resume");
emit({ type: "thread.started", thread_id: resumed ?? `thread-${randomUUID()}` });
emit({ type: "turn.started" });
if (prompt === "slow") {
  emit({ type: "item.started", item: { id: "c1", type: "command_execution", command: "sleep 600", aggregated_output: "", status: "in_progress" } });
  setInterval(() => {}, 1000);
} else if (prompt === "fail") {
  emit({ type: "turn.failed", error: { message: "boom" } });
  process.exit(1);
} else {
  const text = prompt === "env"
    ? `base=${config("openai_base_url") ?? "none"} key=${process.env.CODEX_API_KEY ? "set" : "unset"} token=${process.env.MCP_TOKEN_X ?? "unset"} model=${arg("--model") ?? "none"} cwd=${arg("--cd")} resumed=${resumed ?? "no"}`
    : `echo: ${prompt}`;
  emit({ type: "item.completed", item: { id: "m1", type: "agent_message", text } });
  emit({ type: "turn.completed", usage: { input_tokens: 3, cached_input_tokens: 0, cache_write_input_tokens: 0, output_tokens: 2, reasoning_output_tokens: 0 } });
}
