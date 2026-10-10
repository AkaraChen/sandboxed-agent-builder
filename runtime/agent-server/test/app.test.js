import { test } from "node:test";
import assert from "node:assert/strict";
import { scriptedRuntime } from "@botiverse/oar/testing";
import { createAgentApi } from "../src/app.js";

const manifest = { contract: 1, agent: { type: "codex", codex: { package: "@openai/codex", version: "0.0.0" } }, oar: "0.52.0" };

function setup(turn, extra = {}) {
  const runtime = scriptedRuntime({
    turn: turn ?? (async (t) => {
      await t.tool("shell", "ls", () => "a.txt");
      t.say(`echo: ${t.input}`);
    }),
  });
  const { app, close } = createAgentApi({ runtime, installation: { kind: "available", version: "test" }, manifest, token: "tok", cwd: process.cwd(), ...extra });
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
    return { status: res.status, json, res };
  };
  return { app, close, call };
}

test("auth and agent info", async (t) => {
  const { app, call, close } = setup();
  t.after(close);
  assert.equal((await app.request("/v1/agent")).status, 401);
  assert.equal((await app.request("/healthz")).status, 200);
  const { status, json } = await call("GET", "/v1/agent");
  assert.equal(status, 200);
  assert.equal(json.agent.type, "codex");
  assert.equal(json.installation.version, "test");
});

test("session: prompt and wait, events, input while idle, delete", async (t) => {
  const { call, close } = setup();
  t.after(close);
  const open = await call("POST", "/v1/sessions", { model: "scripted-1" });
  assert.equal(open.status, 201, JSON.stringify(open.json));
  const id = open.json.sessionId;
  assert.equal(open.json.status.kind, "idle");

  const run = await call("POST", `/v1/sessions/${id}/input`, { text: "hello", wait: true, timeoutMs: 10_000 });
  assert.equal(run.status, 200, JSON.stringify(run.json));
  assert.equal(run.json.kind, "ended");
  assert.equal(run.json.outcome.kind, "completed");
  assert.equal(run.json.text, "echo: hello");

  const events = await call("GET", `/v1/sessions/${id}/events?after=-1`);
  const kinds = events.json.items.map((e) => e.kind);
  for (const k of ["turn_started", "tool_call_started", "tool_call_ended", "text_delta", "turn_ended"]) assert.ok(kinds.includes(k), `${k} in ${kinds}`);
  const page = await call("GET", `/v1/sessions/${id}/events?after=${events.json.items[1].seq}&limit=1`);
  assert.equal(page.json.items.length, 1);
  assert.equal(page.json.items[0].seq, events.json.items[2].seq);

  const delivered = await call("POST", `/v1/sessions/${id}/input`, { text: "again" });
  assert.equal(delivered.status, 202);
  assert.equal(delivered.json.landed, "prompted");

  const list = await call("GET", "/v1/sessions");
  assert.equal(list.json.items.length, 1);
  assert.equal((await call("DELETE", `/v1/sessions/${id}`)).status, 204);
  assert.equal((await call("GET", `/v1/sessions/${id}`)).status, 404);
});

test("abort a running turn and steer into it", async (t) => {
  let release;
  const { call, close } = setup(async (turn) => {
    await new Promise((resolve) => {
      release = resolve;
      turn.signal.addEventListener("abort", resolve);
    });
    turn.say(`steered: ${turn.steered.join(",")}`);
  });
  t.after(close);
  const id = (await call("POST", "/v1/sessions", {})).json.sessionId;
  assert.equal((await call("POST", `/v1/sessions/${id}/input`, { text: "long job" })).json.landed, "prompted");
  const busy = await call("POST", `/v1/sessions/${id}/input`, { text: "x", wait: true });
  assert.equal(busy.status, 409);
  const steer = await call("POST", `/v1/sessions/${id}/input`, { text: "also this", when: "now" });
  assert.equal(steer.status, 202);
  assert.equal(steer.json.landed, "steered");
  const abort = await call("POST", `/v1/sessions/${id}/abort`);
  assert.equal(abort.status, 200, JSON.stringify(abort.json));
  for (let i = 0; i < 50; i++) {
    const s = await call("GET", `/v1/sessions/${id}`);
    if (s.json.status.kind === "idle") {
      assert.equal(s.json.status.lastTurnOutcome.kind, "aborted");
      return;
    }
    await new Promise((r) => setTimeout(r, 20));
  }
  assert.fail("turn did not end after abort");
  release?.();
});

test("SSE replays from Last-Event-ID and streams live events", async (t) => {
  const { call, close } = setup();
  t.after(close);
  const id = (await call("POST", "/v1/sessions", {})).json.sessionId;
  await call("POST", `/v1/sessions/${id}/input`, { text: "one", wait: true });
  const all = (await call("GET", `/v1/sessions/${id}/events`)).json.items;
  const from = all[1].seq;
  const { res } = await call("GET", `/v1/sessions/${id}/events/stream`, undefined, { "Last-Event-ID": String(from) });
  assert.equal(res.headers.get("content-type"), "text/event-stream");
  const reader = res.body.getReader();
  let text = "";
  const decoder = new TextDecoder();
  const read = async (until) => {
    while (!until(text)) {
      const { value, done } = await reader.read();
      if (done) break;
      text += decoder.decode(value);
    }
  };
  await read((s) => s.includes(`id: ${all[all.length - 1].seq}`));
  assert.ok(!text.includes(`id: ${from}\n`), "replayed the Last-Event-ID event itself");
  assert.ok(text.includes(`id: ${all[2].seq}`));
  await call("POST", `/v1/sessions/${id}/input`, { text: "two", wait: true });
  await read((s) => s.includes("echo: two"));
  assert.ok(text.includes("event: turn_ended"));
  await reader.cancel();
});

test("validation and limits", async (t) => {
  const { call, close } = setup(undefined, { maxSessions: 1 });
  t.after(close);
  assert.equal((await call("POST", "/v1/sessions", { bogus: 1 })).status, 400);
  assert.equal((await call("POST", "/v1/sessions", { model: 3 })).status, 400);
  const id = (await call("POST", "/v1/sessions", {})).json.sessionId;
  assert.equal((await call("POST", "/v1/sessions", {})).status, 429);
  assert.equal((await call("POST", `/v1/sessions/${id}/input`, {})).status, 400);
  assert.equal((await call("POST", `/v1/sessions/${id}/input`, { text: "x", when: "later" })).status, 400);
  assert.equal((await call("GET", `/v1/sessions/${id}/events?after=abc`)).status, 400);
  assert.equal((await call("GET", "/v1/sessions/nope")).status, 404);
});
