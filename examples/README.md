# 示例

每个 JSON 都是 `POST /builds` 的请求体（API 见 [docs/api.md](../docs/api.md)）。

| 文件 | 内容 |
| --- | --- |
| [minimal.json](minimal.json) | 全部用默认值：Debian slim trixie、最新 Codex、最新稳定版 oar、本地镜像 |
| [registry.json](registry.json) | 装一个 apt 包，推送到内置 registry（需要在配置里启用 registry），交付 `repository@digest` |
| [go-github/](go-github/) | Go 开发环境 + GitHub 官方的 Agent Plugin，并用自己的模型服务驱动 Codex |

```sh
curl -X POST http://127.0.0.1:8080/builds \
  -H "Authorization: Bearer $TOKEN" -H 'Idempotency-Key: minimal-1' \
  -d @examples/minimal.json
```

## Go + GitHub

[go-github/build.json](go-github/build.json) 构建的镜像包含：

- Go 1.27.2：从 go.dev 下载，按官方 sha256 校验，支持 amd64/arm64。
- `gcc`：让 `go test -race` 可用。
- gopls v0.23.0。
- GitHub 的 Agent Plugin：[`github/github-mcp-server` 的 `agent-plugin/`](https://github.com/github/github-mcp-server/tree/main/agent-plugin)，锁定提交 `eb47a99d`。它是一个标准插件（Agent Plugins 1.0.0），包含一个 `streamable-http` MCP 服务器 `https://api.githubcopilot.com/mcp/`。镜像把它编译成 Codex 的 `mcp_servers.github_github`。

标准不规定认证方式。镜像约定从 `MCP_TOKEN_GITHUB_GITHUB` 读取这个服务器的 bearer token，构建结果的 `resolvedSpec.runtimeEnv` 里也会列出这个变量。

[go-github/run.sh](go-github/run.sh) 走完整个流程：提交构建 → 跟踪日志 → 启动镜像（控制 API）→ 打开 Codex 会话 → 让 Codex 用 GitHub MCP 查 `github/github-mcp-server` 的最新 release，写一个返回该 tag 的 Go 函数和测试，再运行 `go test -race`。

打开会话时同时传入：

- `provider`：你的 OpenAI 兼容服务，需支持 Responses API。见 [docs/agent-api.md](../docs/agent-api.md#模型服务)。
- `env.MCP_TOKEN_GITHUB_GITHUB`：GitHub token。

这两个凭据都只进入该会话 Codex 进程的环境变量，不进镜像、响应或日志。

```sh
BUILDER=http://127.0.0.1:8080 BUILDER_TOKEN=... \
PROVIDER_BASE_URL=https://api.example.com/v1 PROVIDER_API_KEY=sk-... MODEL=<模型 id> \
GITHUB_TOKEN=$(gh auth token) examples/go-github/run.sh
```

实测结果见 [docs/verification.md](../docs/verification.md#示例go--github)。
