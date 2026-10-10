#!/bin/sh
# Builds the Go + GitHub agent image, starts it, and drives Codex through
# the image's control API with your own OpenAI-compatible endpoint and a
# GitHub token for the GitHub plugin's MCP server.
#
#   BUILDER=http://127.0.0.1:8080 BUILDER_TOKEN=... \
#   PROVIDER_BASE_URL=https://api.example.com/v1 PROVIDER_API_KEY=... MODEL=... \
#   GITHUB_TOKEN=... examples/go-github/run.sh
#
# Needs curl, jq and docker on the machine whose Docker daemon the builder
# uses (the image is local to that daemon).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
: "${BUILDER:=http://127.0.0.1:8080}" "${PROVIDER_BASE_URL:?}" "${PROVIDER_API_KEY:?}" "${MODEL:?}" "${GITHUB_TOKEN:?}"
bcurl() {
  if [ -n "${BUILDER_TOKEN:-}" ]; then curl -fsS -H "Authorization: Bearer $BUILDER_TOKEN" "$@"; else curl -fsS "$@"; fi
}

echo "submitting the build"
build=$(bcurl -X POST "$BUILDER/builds" -H "Idempotency-Key: go-github-$(date +%Y%m%d)" -d @"$here/build.json")
id=$(printf %s "$build" | jq -r .buildId)
echo "build $id; resolved: $(printf %s "$build" | jq -c '.resolvedSpec | {base: "\(.base.image)@\(.base.digest)", node: .node.version, codex: .agent.codex.version, oar: .oar.version, plugins: [.plugins[] | {name, specVersion, commit, mcpServers, warnings}], runtimeEnv}')"

after=0
while :; do
  page=$(bcurl "$BUILDER/builds/$id/logs?after=$after")
  printf %s "$page" | jq -r '.entries[].text'
  after=$(printf %s "$page" | jq .nextAfter)
  [ "$(printf %s "$page" | jq .complete)" = true ] && break
  sleep 2
done
attempt=$(bcurl "$BUILDER/builds/$id" | jq .currentAttempt)
[ "$(printf %s "$attempt" | jq -r .status)" = succeeded ] || { echo "build did not succeed: $attempt" >&2; exit 1; }
image=$(printf %s "$attempt" | jq -r .artifact.daemonRef)
echo "image $image"

token=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
container=$(docker run -d --rm -p 127.0.0.1::8080 -e AGENT_API_TOKEN="$token" "$image")
trap 'docker rm -f "$container" >/dev/null' EXIT
api="http://$(docker port "$container" 8080 | head -1)"
until curl -fsS "$api/healthz" >/dev/null 2>&1; do sleep 1; done
h="Authorization: Bearer $token"

# The GitHub plugin's MCP server authenticates with a bearer token the
# image reads from MCP_TOKEN_GITHUB_GITHUB (see resolvedSpec.runtimeEnv);
# it is given to this session only, like the model provider's key.
session=$(jq -n --arg model "$MODEL" --arg base "$PROVIDER_BASE_URL" --arg key "$PROVIDER_API_KEY" --arg gh "$GITHUB_TOKEN" \
  '{model: $model, provider: {baseUrl: $base, apiKey: $key}, env: {MCP_TOKEN_GITHUB_GITHUB: $gh}}' |
  curl -fsS -X POST "$api/v1/sessions" -H "$h" -d @- | jq -r .sessionId)
echo "session $session"

prompt='Use the GitHub MCP tools to find the latest release of the github/github-mcp-server repository. Then in /workspace create a Go module "demo" with a function LatestRelease() string returning that tag, and a test that checks it looks like vMAJOR.MINOR.PATCH. Run `go test -race ./...` and report the tag and the test result in one line.'
jq -n --arg text "$prompt" '{text: $text, wait: true, timeoutMs: 900000}' |
  curl -fsS -X POST "$api/v1/sessions/$session/input" -H "$h" -d @- | jq -r '.outcome.kind, .text'
docker exec "$container" sh -c 'cd /workspace && ls && go test ./...'
