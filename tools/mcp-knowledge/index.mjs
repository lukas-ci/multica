#!/usr/bin/env node
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";

const BASE = process.env.MULTICA_API_URL || "http://192.168.3.172:8088";
const WORKSPACE_SLUG = process.env.MULTICA_WORKSPACE_SLUG || "";
const TOKEN = process.env.MULTICA_AUTH_TOKEN || "";

const server = new Server(
  { name: "multica-knowledge", version: "1.0.0" },
  { capabilities: { tools: {} } },
);

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: "knowledge_search",
      description:
        "Search indexed knowledge sources (Confluence, GitHub, etc.) linked to the current workspace. Use this when you need context from documentation, runbooks, or code references.",
      inputSchema: {
        type: "object",
        properties: {
          query: { type: "string", description: "Search query" },
          limit: { type: "integer", description: "Max results (default 10)" },
        },
        required: ["query"],
      },
    },
  ],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: args } = request.params;
  if (name !== "knowledge_search") {
    throw new Error(`Unknown tool: ${name}`);
  }

  const headers = { "Content-Type": "application/json" };
  if (WORKSPACE_SLUG) headers["X-Workspace-Slug"] = WORKSPACE_SLUG;
  if (TOKEN) headers["Authorization"] = "Bearer " + TOKEN;

  const resp = await fetch(`${BASE}/api/mcp`, {
    method: "POST",
    headers,
    body: JSON.stringify({
      method: "tools/call",
      params: { name: "knowledge_search", arguments: args },
    }),
  });

  const data = await resp.json();
  if (data.error) {
    throw new Error(data.error.message || "search failed");
  }

  const results = data.result?.content ?? [];
  return {
    content: [
      {
        type: "text",
        text: results.length === 0
          ? "No results found."
          : results
              .map((r) => `[${r.score?.toFixed(2)}] ${r.title}\n${r.url}\n${r.snippet}`)
              .join("\n\n"),
      },
    ],
  };
});

const transport = new StdioServerTransport();
await server.connect(transport);
