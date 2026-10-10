// Minimal MCP stdio server (newline-delimited JSON-RPC), no dependencies.
import { createInterface } from "node:readline";
const send = (m) => process.stdout.write(JSON.stringify(m) + "\n");
createInterface({ input: process.stdin }).on("line", (line) => {
  if (!line.trim()) return;
  const msg = JSON.parse(line);
  if (msg.id === undefined) return;
  switch (msg.method) {
    case "initialize":
      return send({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: msg.params?.protocolVersion ?? "2025-06-18", capabilities: { tools: {} }, serverInfo: { name: "probe", version: "1.0.0" } } });
    case "tools/list":
      return send({ jsonrpc: "2.0", id: msg.id, result: { tools: [{ name: "probe_magic_word", description: "Returns the probe plugin's magic word.", inputSchema: { type: "object", properties: {} } }] } });
    case "tools/call":
      return send({ jsonrpc: "2.0", id: msg.id, result: { content: [{ type: "text", text: `MAGIC-${process.env.PROBE_TOKEN ?? "missing"}-${process.env.PROBE_MODE ?? "missing"}` }] } });
    default:
      return send({ jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: "method not found" } });
  }
});
