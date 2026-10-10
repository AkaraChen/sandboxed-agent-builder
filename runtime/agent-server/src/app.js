// The agent control API (runtime contract 1, docs/agent-api.md): Codex
// sessions over HTTP. Each session is a Codex thread driven through the
// official SDK (@openai/codex-sdk, the same version as the image's Codex);
// a turn is one `codex exec` run. The server keeps each session's events
// for polling and SSE.
import { randomUUID } from "node:crypto";
import { Codex } from "@openai/codex-sdk";
import { Hono } from "hono";
import { bearerAuth } from "hono/bearer-auth";
import { HTTPException } from "hono/http-exception";
import { streamSSE } from "hono/streaming";

const OPEN_FIELDS = ["cwd", "model", "effort", "resume", "env", "provider"];
const EFFORTS = ["minimal", "low", "medium", "high", "xhigh", "max"];

function fail(status, code, message) {
  return new HTTPException(status, { res: Response.json({ error: { code, message } }, { status }) });
}

async function body(c) {
  try {
    const value = await c.req.json();
    if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error();
    return value;
  } catch {
    throw fail(400, "invalid_request", "the body must be a JSON object");
  }
}

function checkOpen(req) {
  const unknown = Object.keys(req).filter((k) => !OPEN_FIELDS.includes(k));
  if (unknown.length) throw fail(400, "invalid_request", `unknown fields: ${unknown.join(", ")}`);
  for (const k of ["cwd", "model", "resume"]) {
    if (req[k] !== undefined && (typeof req[k] !== "string" || req[k] === "")) throw fail(400, "invalid_request", `${k} must be a string`);
  }
  if (req.effort !== undefined && !EFFORTS.includes(req.effort)) throw fail(400, "invalid_request", `effort must be one of ${EFFORTS.join(", ")}`);
  if (req.env !== undefined && (typeof req.env !== "object" || req.env === null || Object.values(req.env).some((v) => typeof v !== "string"))) {
    throw fail(400, "invalid_request", "env must map names to strings");
  }
  const p = req.provider;
  if (p === undefined) return;
  if (p === null || typeof p !== "object" || Object.keys(p).some((k) => k !== "baseUrl" && k !== "apiKey")) {
    throw fail(400, "invalid_request", "provider is {baseUrl, apiKey}");
  }
  let url = null;
  try {
    url = new URL(p.baseUrl);
  } catch {}
  if (!url || (url.protocol !== "https:" && url.protocol !== "http:")) throw fail(400, "invalid_request", "provider.baseUrl must be an http(s) URL");
  if (typeof p.apiKey !== "string" || p.apiKey === "") throw fail(400, "invalid_request", "provider.apiKey is required");
  if (!req.model) throw fail(400, "invalid_request", "model is required with provider (the provider's model id)");
}

/**
 * @param {object} o
 * @param {string} o.codexPath the codex executable
 * @param {object} o.manifest /opt/agent/manifest.json
 * @param {string} [o.version] `codex --version`
 * @param {string} [o.token] bearer token; undefined disables auth
 * @param {string} [o.cwd] default working directory of new sessions
 * @param {Record<string,string>} [o.env] base environment of Codex runs
 * @param {number} [o.maxSessions]
 * @param {number} [o.maxEvents] events kept per session
 */
export function createAgentApi({ codexPath, manifest, version, token, cwd = "/workspace", env = process.env, maxSessions = 4, maxEvents = 20000 }) {
  const sessions = new Map();

  const get = (c) => {
    const s = sessions.get(c.req.param("id"));
    if (!s) throw fail(404, "not_found", "no such session");
    return s;
  };

  const view = (s) => ({
    sessionId: s.id,
    threadId: s.thread.id,
    createdAt: s.createdAt,
    cwd: s.cwd,
    model: s.model ?? null,
    provider: s.provider,
    status: s.run ? "running" : "idle",
    lastTurn: s.lastTurn,
    lastSeq: s.seq,
  });

  const record = (s, event) => {
    const e = { seq: ++s.seq, time: new Date().toISOString(), ...event };
    s.events.push(e);
    if (s.events.length > maxEvents) {
      s.events.splice(0, s.events.length - maxEvents);
      s.dropped = true;
    }
    for (const l of s.listeners) l(e);
  };

  // run drives one turn to its end and records every event; it resolves
  // with the turn's outcome and never rejects.
  const run = (s, text) => {
    const controller = new AbortController();
    const done = (async () => {
      let finalResponse = "";
      let usage = null;
      let outcome = { kind: "completed" };
      try {
        const { events } = await s.thread.runStreamed(text, { signal: controller.signal });
        for await (const event of events) {
          record(s, event);
          if (event.type === "item.completed" && event.item.type === "agent_message") finalResponse = event.item.text;
          if (event.type === "turn.completed") usage = event.usage;
          if (event.type === "turn.failed") outcome = { kind: "failed", error: event.error.message };
        }
      } catch (err) {
        // Codex exits non-zero after a failed turn; its own error says more.
        if (controller.signal.aborted) outcome = { kind: "aborted" };
        else if (outcome.kind !== "failed") outcome = { kind: "failed", error: String(err?.message ?? err) };
      }
      s.lastTurn = { ...outcome, finalResponse, usage, endedAt: new Date().toISOString() };
      s.run = null;
      record(s, { type: "turn.ended", outcome: outcome.kind });
      return s.lastTurn;
    })();
    s.run = { controller, done };
    return done;
  };

  const app = new Hono();

  app.onError((err, c) => {
    if (err instanceof HTTPException) return err.getResponse();
    console.error("agent api:", err);
    return c.json({ error: { code: "internal", message: String(err?.message ?? err) } }, 500);
  });

  app.get("/healthz", (c) => c.json({ status: "ok" }));

  if (token) app.use("/v1/*", bearerAuth({ token }));

  app.get("/v1/agent", (c) => c.json({ contract: manifest.contract, codex: manifest.codex, version: version ?? null, sessions: sessions.size, maxSessions }));

  app.get("/v1/sessions", (c) => c.json({ items: [...sessions.values()].map(view) }));

  app.post("/v1/sessions", async (c) => {
    const req = await body(c);
    checkOpen(req);
    if (sessions.size >= maxSessions) throw fail(429, "too_many_sessions", `at most ${maxSessions} sessions; close one first`);
    // Credentials stay in this session's Codex options and environment.
    const codex = new Codex({
      codexPathOverride: codexPath,
      env: { ...env, ...req.env },
      ...(req.provider ? { baseUrl: req.provider.baseUrl, apiKey: req.provider.apiKey } : {}),
    });
    const options = {
      model: req.model,
      modelReasoningEffort: req.effort,
      workingDirectory: req.cwd ?? cwd,
      skipGitRepoCheck: true,
      sandboxMode: "danger-full-access",
      approvalPolicy: "never",
    };
    const s = {
      id: randomUUID(),
      thread: req.resume ? codex.resumeThread(req.resume, options) : codex.startThread(options),
      createdAt: new Date().toISOString(),
      cwd: options.workingDirectory,
      model: req.model,
      provider: req.provider ? { baseUrl: req.provider.baseUrl } : null,
      lastTurn: null,
      run: null,
      seq: 0,
      events: [],
      dropped: false,
      listeners: new Set(),
    };
    sessions.set(s.id, s);
    return c.json(view(s), 201);
  });

  app.get("/v1/sessions/:id", (c) => c.json(view(get(c))));

  app.post("/v1/sessions/:id/input", async (c) => {
    const s = get(c);
    const req = await body(c);
    if (typeof req.text !== "string" || req.text === "") throw fail(400, "invalid_request", "text is required");
    const timeoutMs = req.timeoutMs ?? 600_000;
    if (!Number.isInteger(timeoutMs) || timeoutMs <= 0) throw fail(400, "invalid_request", "timeoutMs must be a positive integer");
    if (s.run) throw fail(409, "busy", "a turn is running; wait for it or abort it");
    const done = run(s, req.text);
    if (!req.wait) return c.json({ status: "running", lastSeq: s.seq }, 202);
    let timer;
    const result = await Promise.race([done, new Promise((r) => (timer = setTimeout(() => r(null), timeoutMs)))]);
    clearTimeout(timer);
    // On timeout the turn keeps running; the client can poll or abort.
    return result ? c.json(result) : c.json({ kind: "timeout", status: "running" }, 202);
  });

  app.post("/v1/sessions/:id/abort", async (c) => {
    const s = get(c);
    if (!s.run) throw fail(409, "no_active_turn", "no turn is running");
    s.run.controller.abort();
    return c.json(await s.run.done);
  });

  const parseAfter = (value) => {
    if (value === undefined || value === null || value === "") return 0;
    const n = Number(value);
    if (!Number.isInteger(n) || n < 0) throw fail(400, "invalid_request", "after must be a sequence number");
    return n;
  };

  app.get("/v1/sessions/:id/events", (c) => {
    const s = get(c);
    const after = parseAfter(c.req.query("after"));
    const limit = c.req.query("limit") ? Number(c.req.query("limit")) : 500;
    if (!Number.isInteger(limit) || limit < 1 || limit > 5000) throw fail(400, "invalid_request", "limit must be 1-5000");
    const items = s.events.filter((e) => e.seq > after).slice(0, limit);
    return c.json({ items, nextAfter: items.length ? items[items.length - 1].seq : after, truncated: s.dropped });
  });

  app.get("/v1/sessions/:id/events/stream", (c) => {
    const s = get(c);
    let last = parseAfter(c.req.header("Last-Event-ID") ?? c.req.query("after"));
    return streamSSE(c, async (stream) => {
      const pending = [];
      let wake = null;
      let closed = false;
      // null means the session was closed.
      const listener = (e) => {
        if (e === null) closed = true;
        else pending.push(e);
        wake?.();
      };
      s.listeners.add(listener);
      stream.onAbort(() => wake?.());
      try {
        pending.push(...s.events.filter((e) => e.seq > last));
        while (!stream.aborted && !closed) {
          for (const e of pending.splice(0)) {
            if (e.seq <= last) continue;
            await stream.writeSSE({ id: String(e.seq), event: e.type, data: JSON.stringify(e) });
            last = e.seq;
          }
          if (pending.length > 0 || closed) continue;
          let timer;
          await new Promise((resolve) => {
            wake = resolve;
            timer = setTimeout(resolve, 15_000);
          });
          clearTimeout(timer);
          wake = null;
          if (pending.length === 0 && !closed && !stream.aborted) await stream.writeSSE({ event: "ping", data: "{}" });
        }
        if (closed) await stream.writeSSE({ event: "closed", data: "{}" });
      } finally {
        s.listeners.delete(listener);
      }
    });
  });

  app.delete("/v1/sessions/:id", async (c) => {
    const s = get(c);
    sessions.delete(s.id);
    if (s.run) {
      s.run.controller.abort();
      await s.run.done;
    }
    for (const l of s.listeners) l(null);
    return c.body(null, 204);
  });

  async function close() {
    await Promise.allSettled([...sessions.values()].map((s) => (s.run?.controller.abort(), s.run?.done)));
    for (const s of sessions.values()) for (const l of s.listeners) l(null);
    sessions.clear();
  }

  return { app, close };
}
