import { test } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { createAgentApi } from "../src/app.js";

const codexPath = fileURLToPath(new URL("../fixtures/fake-codex.mjs", import.meta.url));
const manifest = { contract: 1, codex: { package: "@openai/codex-sdk", version: "0.0.0" } };

function setup(extra = {}) {
  const { app, close } = createAgentApi({ codexPath, manifest, version: "codex-cli test", token: "tok", cwd: process.cwd(), ...extra });
  const call = async (method, path, body, headers = {}) => {
    const res = await app.request(path, {
      method,
      headers: { Authorization: "Bearer tok", "Content-Type": "application/json", ...headers },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (res.headers.get("content-type") === "text/event-stream") return { status: res.status, res };
    const text = await res.text();
    let json;
    try { json = JSON.parse(text); } catch { json = text; }
    return { status: res.status, json, text };
  };
  return { app, close, call };
}

const waitIdle = async (call, id) => {
  for (let i = 0; i < 100; i++) {
    const s = (await call("GET", `/v1/sessions/${id}`)).json;
    if (s.status === "idle" && s.lastTurn) return s;
    await new Promise((r) => setTimeout(r, 20));
  }
  throw new Error("turn did not end");
};

test("auth and agent info", async (t) => {
  const { app, call, close } = setup();
  t.after(close);
  assert.equal((await app.request("/v1/agent")).status, 401);
  assert.equal((await app.request("/healthz")).status, 200);
  const { json } = await call("GET", "/v1/agent");
  assert.equal(json.version, "codex-cli test");
  assert.equal(json.codex.version, "0.0.0");
});

test("a session runs turns, keeps events and resumes its thread", async (t) => {
  const { call, close } = setup();
  t.after(close);
  const open = await call("POST", "/v1/sessions", { model: "m" });
  assert.equal(open.status, 201);
  assert.equal(open.json.threadId, null);
  const id = open.json.sessionId;

  const run = await call("POST", `/v1/sessions/${id}/input`, { text: "hello", wait: true });
  assert.equal(run.status, 200);
  assert.equal(run.json.kind, "completed");
  assert.equal(run.json.finalResponse, "echo: hello");
  assert.equal(run.json.usage.output_tokens, 2);

  const s = (await call("GET", `/v1/sessions/${id}`)).json;
  assert.match(s.threadId, /^thread-/);
  const events = (await call("GET", `/v1/sessions/${id}/events`)).json.items;
  assert.deepEqual(events.map((e) => e.type), ["thread.started", "turn.started", "item.completed", "turn.completed", "turn.ended"]);
  const page = (await call("GET", `/v1/sessions/${id}/events?after=2&limit=1`)).json;
  assert.deepEqual(page.items.map((e) => e.seq), [3]);

  // The second turn resumes the same thread.
  const again = await call("POST", `/v1/sessions/${id}/input`, { text: "env", wait: true });
  assert.match(again.json.finalResponse, new RegExp(`resumed=${s.threadId}`));

  // Another session can resume it by id.
  const other = (await call("POST", "/v1/sessions", { resume: s.threadId })).json;
  assert.equal(other.threadId, s.threadId);
  assert.equal((await call("DELETE", `/v1/sessions/${id}`)).status, 204);
  assert.equal((await call("GET", `/v1/sessions/${id}`)).status, 404);
});

test("async input, busy, abort and failure", async (t) => {
  const { call, close } = setup();
  t.after(close);
  const id = (await call("POST", "/v1/sessions", {})).json.sessionId;
  assert.equal((await call("POST", `/v1/sessions/${id}/input`, { text: "slow" })).status, 202);
  assert.equal((await call("POST", `/v1/sessions/${id}/input`, { text: "x" })).json.error.code, "busy");
  assert.equal((await call("GET", `/v1/sessions/${id}`)).json.status, "running");
  const aborted = await call("POST", `/v1/sessions/${id}/abort`);
  assert.equal(aborted.json.kind, "aborted");
  assert.equal((await call("POST", `/v1/sessions/${id}/abort`)).status, 409);

  const failed = await call("POST", `/v1/sessions/${id}/input`, { text: "fail", wait: true });
  assert.equal(failed.json.kind, "failed");
  assert.match(failed.json.error, /boom/);

  const timeout = await call("POST", `/v1/sessions/${id}/input`, { text: "slow", wait: true, timeoutMs: 50 });
  assert.equal(timeout.status, 202);
  assert.equal(timeout.json.kind, "timeout");
  await call("POST", `/v1/sessions/${id}/abort`);
  await waitIdle(call, id);
});

test("provider and env reach this session's Codex only, never a response", async (t) => {
  const { call, close } = setup();
  t.after(close);
  assert.equal((await call("POST", "/v1/sessions", { provider: { baseUrl: "https://llm.example/v1", apiKey: "k" } })).status, 400, "model required");
  assert.equal((await call("POST", "/v1/sessions", { model: "m", provider: { baseUrl: "ftp://x", apiKey: "k" } })).status, 400);
  const open = await call("POST", "/v1/sessions", { model: "m", env: { MCP_TOKEN_X: "gh-token" }, provider: { baseUrl: "https://llm.example/v1", apiKey: "secret-key" } });
  assert.equal(open.status, 201);
  assert.ok(!open.text.includes("secret-key"));
  assert.deepEqual(open.json.provider, { baseUrl: "https://llm.example/v1" });
  const run = await call("POST", `/v1/sessions/${open.json.sessionId}/input`, { text: "env", wait: true });
  assert.match(run.json.finalResponse, /base="https:\/\/llm.example\/v1" key=set token=gh-token model=m/);
  const plain = (await call("POST", "/v1/sessions", {})).json.sessionId;
  const other = await call("POST", `/v1/sessions/${plain}/input`, { text: "env", wait: true });
  assert.match(other.json.finalResponse, /base=none key=unset token=unset/);
  for (const path of ["/v1/sessions", `/v1/sessions/${open.json.sessionId}/events`]) {
    assert.ok(!(await call("GET", path)).text.includes("secret-key"), path);
  }
});

test("SSE replays from Last-Event-ID and streams live events", async (t) => {
  const { call, close } = setup();
  t.after(close);
  const id = (await call("POST", "/v1/sessions", {})).json.sessionId;
  await call("POST", `/v1/sessions/${id}/input`, { text: "one", wait: true });
  const { res } = await call("GET", `/v1/sessions/${id}/events/stream`, undefined, { "Last-Event-ID": "2" });
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let text = "";
  const read = async (until) => {
    while (!until(text)) {
      const { value, done } = await reader.read();
      if (done) break;
      text += decoder.decode(value);
    }
  };
  await read((s) => s.includes("id: 5"));
  assert.ok(!text.includes("id: 2\n") && text.includes("id: 3"));
  await call("POST", `/v1/sessions/${id}/input`, { text: "two", wait: true });
  await read((s) => s.includes("echo: two"));
  assert.ok(text.includes("event: turn.completed"));
  await reader.cancel();
});

test("validation and limits", async (t) => {
  const { call, close } = setup({ maxSessions: 1 });
  t.after(close);
  assert.equal((await call("POST", "/v1/sessions", { bogus: 1 })).status, 400);
  assert.equal((await call("POST", "/v1/sessions", { effort: "huge" })).status, 400);
  const id = (await call("POST", "/v1/sessions", {})).json.sessionId;
  assert.equal((await call("POST", "/v1/sessions", {})).status, 429);
  assert.equal((await call("POST", `/v1/sessions/${id}/input`, {})).status, 400);
  assert.equal((await call("GET", `/v1/sessions/${id}/events?after=-1`)).status, 400);
  assert.equal((await call("GET", "/v1/sessions/nope")).status, 404);
});
