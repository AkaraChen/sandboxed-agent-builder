// Package agentserver embeds the agent control API (Hono over @openai/codex-sdk) that
// every image carries at /opt/agent/server; see docs/agent-api.md.
package agentserver

import "embed"

// Files are the server's sources and lockfile; dependencies are installed
// in the image with npm ci.
//
//go:embed package.json package-lock.json src
var Files embed.FS
