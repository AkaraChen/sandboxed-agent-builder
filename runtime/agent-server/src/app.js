// The agent control API (runtime contract 1, docs/agent-api.md): a thin
// HTTP face over oar sessions of the image's agent runtime. oar owns the
// agent process and its record stream; this file only maps requests to
// oar calls and keeps each session's events for polling and SSE.
import { Hono } from "hono";
import { bearerAuth } from "hono/bearer-auth";
import { HTTPException } from "hono/http-exception";
import { streamSSE } from "hono/streaming";
import { promptAndWait, SessionNotFoundError, UnsupportedOptionError } from "@botiverse/oar";

const OPEN_OPTIONS = ["cwd", "model", "effort", "serviceTier", "resume", "systemPrompt", "appendSystemPrompt", "env", "provider"];
const WHEN = ["now", "after_turn", "when_idle"];

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

// The Codex model provider a session's provider option defines. The key
// reaches Codex only through this variable in the session's environment.
const PROVIDER_ID = "sab_provider";
const PROVIDER_KEY_ENV = "SAB_PROVIDER_API_KEY";

/**
 * codexProvider turns {baseUrl, apiKey} into Codex launch arguments (a
 * custom model provider on the Responses API) and environment.
 */
export function codexProvider(provider) {
  if (provider === null || typeof provider !== "object" || Array.isArray(provider)) {
    throw fail(400, "invalid_request", "provider must be an object with baseUrl and apiKey");
  }
  const unknown = Object.keys(provider).filter((k) => k !== "baseUrl" && k !== "apiKey");
  if (unknown.length) throw fail(400, "invalid_request", `provider: unknown fields: ${unknown.join(", ")}`);
  let url;
  try {
    url = new URL(provider.baseUrl);
  } catch {
    url = null;
  }
  if (!url || (url.protocol !== "https:" && url.protocol !== "http:")) {
    throw fail(400, "invalid_request", "provider.baseUrl must be an http(s) URL, e.g. https://api.example.com/v1");
  }
  if (typeof provider.apiKey !== "string" || provider.apiKey === "") throw fail(400, "invalid_request", "provider.apiKey is required");
  // A JSON string is a valid TOML basic string.
  const set = (key, value) => ["-c", `${key}=${JSON.stringify(value)}`];
  return {
    launchArgs: [
      ...set("model_provider", PROVIDER_ID),
      ...set(`model_providers.${PROVIDER_ID}.name`, url.host),
      ...set(`model_providers.${PROVIDER_ID}.base_url`, provider.baseUrl),
      ...set(`model_providers.${PROVIDER_ID}.env_key`, PROVIDER_KEY_ENV),
      ...set(`model_providers.${PROVIDER_ID}.wire_api`, "responses"),
    ],
    env: { [PROVIDER_KEY_ENV]: provider.apiKey },
  };
}

function control(outcome) {
  return outcome.kind === "accepted"
    ? { kind: "accepted", seq: outcome.seq, requestId: outcome.requestId }
    : { kind: "rejected", seq: outcome.seq, requestId: outcome.requestId, code: outcome.code, reason: outcome.reason };
}

/**
 * @param {object} o
 * @param {import("@botiverse/oar").Runtime} o.runtime the image's agent runtime
 * @param {object} o.installation its available installation
 * @param {object} o.manifest /opt/agent/manifest.json
 * @param {string} [o.token] bearer token; undefined disables auth
 * @param {string} [o.cwd] default working directory of new sessions
 * @param {number} [o.maxSessions]
 * @param {number} [o.maxEvents] events kept per session for replay
 */
export function createAgentApi({ runtime, installation, manifest, token, cwd = "/workspace", maxSessions = 4, maxEvents = 20000 }) {
  /** @type {Map<string, {session: any, createdAt: string, options: object, events: any[], dropped: number, listeners: Set<Function>, unsubscribe: Function}>} */
  const sessions = new Map();
  let opening = 0;

  const get = (c) => {
    const entry = sessions.get(c.req.param("id"));
    if (!entry) throw fail(404, "not_found", "no such session");
    return entry;
  };

  const summary = (id, entry) => ({
    sessionId: id,
    createdAt: entry.createdAt,
    cwd: entry.options.cwd,
    provider: entry.provider,
    status: entry.session.status().value,
    model: entry.session.model().value,
    effort: entry.session.effort().value,
    lastSeq: entry.events.length ? entry.events[entry.events.length - 1].seq : -1,
  });

  const app = new Hono();

  app.onError((err, c) => {
    if (err instanceof HTTPException) return err.getResponse();
    console.error("agent api:", err);
    return c.json({ error: { code: "internal", message: String(err?.message ?? err) } }, 500);
  });

  app.get("/healthz", (c) => c.json({ status: "ok" }));

  if (token) app.use("/v1/*", bearerAuth({ token }));

  app.get("/v1/agent", (c) =>
    c.json({
      contract: manifest.contract,
      agent: manifest.agent,
      oar: manifest.oar,
      installation: { kind: installation.kind, version: installation.version ?? null },
      sessions: sessions.size,
      maxSessions,
    }),
  );

  app.get("/v1/agent/models", async (c) => {
    if (!runtime.listModels) throw fail(501, "unsupported", "the runtime cannot list models");
    return c.json(await runtime.listModels(installation));
  });

  app.get("/v1/sessions", (c) => c.json({ items: [...sessions].map(([id, e]) => summary(id, e)) }));

  app.post("/v1/sessions", async (c) => {
    const req = await body(c);
    const unknown = Object.keys(req).filter((k) => !OPEN_OPTIONS.includes(k));
    if (unknown.length) throw fail(400, "invalid_request", `unknown fields: ${unknown.join(", ")}`);
    for (const k of OPEN_OPTIONS) {
      if (k !== "env" && k !== "provider" && req[k] !== undefined && typeof req[k] !== "string") throw fail(400, "invalid_request", `${k} must be a string`);
    }
    if (req.env !== undefined && (typeof req.env !== "object" || Object.values(req.env).some((v) => v !== null && typeof v !== "string"))) {
      throw fail(400, "invalid_request", "env must map names to strings or null");
    }
    let provider = null;
    const { provider: providerReq, ...rest } = req;
    const options = { ...rest, cwd: req.cwd ?? cwd };
    if (providerReq !== undefined) {
      if (manifest.agent?.type !== "codex") throw fail(400, "unsupported_option", "provider is supported for Codex only");
      if (typeof req.model !== "string" || req.model === "") throw fail(400, "invalid_request", "model is required with provider (the provider's model id)");
      const { launchArgs, env } = codexProvider(providerReq);
      options.launchArgs = launchArgs;
      options.env = { ...req.env, ...env };
      provider = { baseUrl: providerReq.baseUrl };
    }
    if (sessions.size + opening >= maxSessions) throw fail(429, "too_many_sessions", `at most ${maxSessions} sessions; close one first`);
    opening++;
    let session;
    try {
      session = await runtime.session(installation, options);
    } catch (err) {
      if (err instanceof UnsupportedOptionError) throw fail(400, "unsupported_option", err.message);
      if (err instanceof SessionNotFoundError) throw fail(404, "resume_not_found", err.message);
      throw fail(502, "open_failed", String(err?.message ?? err));
    } finally {
      opening--;
    }
    // Only the working directory is kept: env may hold the provider key.
    const entry = { session, createdAt: new Date().toISOString(), options: { cwd: options.cwd }, provider, events: [], dropped: 0, listeners: new Set() };
    entry.unsubscribe = session.events((event) => {
      entry.events.push(event);
      if (entry.events.length > maxEvents) {
        entry.events.splice(0, entry.events.length - maxEvents);
        entry.dropped++;
      }
      for (const l of entry.listeners) l(event);
    }, { coalesceText: { maxHoldMs: 250 } });
    sessions.set(session.id, entry);
    return c.json(summary(session.id, entry), 201);
  });

  app.get("/v1/sessions/:id", (c) => {
    const entry = get(c);
    return c.json({ ...summary(c.req.param("id"), entry), usage: entry.session.usage().value, contextUsage: entry.session.contextUsage().value });
  });

  app.post("/v1/sessions/:id/input", async (c) => {
    const entry = get(c);
    const req = await body(c);
    if (typeof req.text !== "string" || req.text === "") throw fail(400, "invalid_request", "text is required");
    if (req.when !== undefined && !WHEN.includes(req.when)) throw fail(400, "invalid_request", `when must be one of ${WHEN.join(", ")}`);
    if (req.wait) {
      if (req.when !== undefined) throw fail(400, "invalid_request", "wait starts a new turn; it takes no when");
      const timeoutMs = req.timeoutMs ?? 600_000;
      if (!Number.isInteger(timeoutMs) || timeoutMs <= 0) throw fail(400, "invalid_request", "timeoutMs must be a positive integer");
      const run = await promptAndWait(entry.session, req.text, { timeoutMs });
      if (run.kind === "rejected") return c.json({ kind: "rejected", code: run.code, reason: run.reason, result: control(run.result) }, 409);
      return c.json({ kind: run.kind, by: run.by, outcome: run.outcome, text: run.text, result: control(run.result) });
    }
    const res = await entry.session.deliver(req.text, req.when ? { when: req.when } : undefined);
    if (res.landed === "rejected") return c.json({ landed: "rejected", inputId: res.inputId, code: res.code, reason: res.reason }, 409);
    return c.json({ landed: res.landed, inputId: res.inputId, result: control(res.result) }, 202);
  });

  app.post("/v1/sessions/:id/abort", async (c) => {
    const out = control(await get(c).session.abort());
    return c.json(out, out.kind === "accepted" ? 200 : 409);
  });

  const parseAfter = (value) => {
    if (value === undefined || value === null || value === "") return -1;
    const n = Number(value);
    if (!Number.isInteger(n) || n < -1) throw fail(400, "invalid_request", "after must be a sequence number (-1 for the start)");
    return n;
  };

  app.get("/v1/sessions/:id/events", (c) => {
    const entry = get(c);
    const after = parseAfter(c.req.query("after"));
    const limit = c.req.query("limit") ? Number(c.req.query("limit")) : 500;
    if (!Number.isInteger(limit) || limit < 1 || limit > 5000) throw fail(400, "invalid_request", "limit must be 1-5000");
    const items = entry.events.filter((e) => e.seq > after).slice(0, limit);
    const nextAfter = items.length ? items[items.length - 1].seq : after;
    return c.json({ items, nextAfter, oldestSeq: entry.events[0]?.seq ?? null, truncated: entry.dropped > 0 });
  });

  app.get("/v1/sessions/:id/events/stream", (c) => {
    const entry = get(c);
    const after = parseAfter(c.req.header("Last-Event-ID") ?? c.req.query("after"));
    return streamSSE(c, async (stream) => {
      const pending = [];
      let wake = null;
      let closed = false;
      // null means the session was closed.
      const listener = (event) => {
        if (event === null) closed = true;
        else pending.push(event);
        wake?.();
      };
      entry.listeners.add(listener);
      stream.onAbort(() => {
        entry.listeners.delete(listener);
        wake?.();
      });
      try {
        let last = after;
        for (const e of entry.events.filter((e) => e.seq > after)) {
          await stream.writeSSE({ id: String(e.seq), event: e.kind, data: JSON.stringify(e) });
          last = e.seq;
        }
        while (!stream.aborted && !closed) {
          if (pending.length === 0) {
            let timer;
            await new Promise((resolve) => {
              wake = resolve;
              timer = setTimeout(resolve, 15_000);
            });
            clearTimeout(timer);
            wake = null;
            if (closed || stream.aborted) break;
            if (pending.length === 0) {
              await stream.writeSSE({ event: "ping", data: "{}" });
              continue;
            }
          }
          for (const e of pending.splice(0)) {
            if (e.seq <= last) continue;
            await stream.writeSSE({ id: String(e.seq), event: e.kind, data: JSON.stringify(e) });
            last = e.seq;
          }
        }
        if (closed) await stream.writeSSE({ event: "closed", data: "{}" });
      } finally {
        entry.listeners.delete(listener);
      }
    });
  });

  app.delete("/v1/sessions/:id", async (c) => {
    const entry = get(c);
    const id = c.req.param("id");
    sessions.delete(id);
    entry.unsubscribe();
    for (const l of entry.listeners) l(null);
    await entry.session.dispose();
    return c.body(null, 204);
  });

  async function close() {
    for (const e of sessions.values()) for (const l of e.listeners) l(null);
    await Promise.allSettled([...sessions.values()].map((e) => e.session.dispose()));
    sessions.clear();
  }

  return { app, close };
}
