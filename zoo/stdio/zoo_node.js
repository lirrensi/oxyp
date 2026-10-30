#!/usr/bin/env node
// zoo node: stdio MCP server in plain node, no deps. Mirrors zoo_stdio.py clean path.
// Verifies oxyp works against a non-Python runtime.
const readline = require("readline");

const TOOLS = [
  { name: "node_ping", description: "node pong", inputSchema: { type: "object", properties: {} } },
  { name: "node_echo", description: "node echo", inputSchema: { type: "object", properties: { msg: { type: "string" } }, required: ["msg"] } },
];

function respond(id, result, error) {
  const msg = { jsonrpc: "2.0", id };
  if (error) msg.error = error;
  else msg.result = result;
  process.stdout.write(JSON.stringify(msg) + "\n");
}

const rl = readline.createInterface({ input: process.stdin });
rl.on("line", (line) => {
  if (!line.trim()) return;
  let req;
  try { req = JSON.parse(line); } catch { return; }
  const { method, id, params = {} } = req;
  if (method === "initialize") {
    respond(id, { protocolVersion: "2024-11-05", serverInfo: { name: "zoo-node", version: "0.0.1" }, capabilities: {} });
  } else if (method === "notifications/initialized") {
    // noop
  } else if (method === "tools/list") {
    respond(id, { tools: TOOLS });
  } else if (method === "tools/call") {
    const args = params.arguments || {};
    if (params.name === "node_echo") {
      respond(id, { content: [{ type: "text", text: "node-echo:" + (args.msg || "") }] });
    } else {
      respond(id, { content: [{ type: "text", text: "node-pong" }] });
    }
  } else if (method === "ping") {
    respond(id, {});
  } else if (id !== undefined) {
    respond(id, null, { code: -32601, message: "not found: " + method });
  }
});
