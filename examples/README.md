# 示例

每个 JSON 都是 `POST /builds` 的请求体（API 见 [docs/api.md](../docs/api.md)）。

| 文件 | 内容 |
| --- | --- |
| [minimal.json](minimal.json) | 全部用默认值：Debian slim trixie、最新 Codex、最新稳定版 oar、本地镜像 |
| [registry.json](registry.json) | 装一个 apt 包，推送到内置 registry（需要在配置里启用 registry），交付 `repository@digest` |
| [go-pstack/](go-pstack/) | Go 开发环境 + Cursor 团队的 [pstack](https://github.com/cursor/plugins/tree/main/pstack) 插件，并用自己的模型服务驱动 Codex |

```sh
curl -X POST http://127.0.0.1:8080/builds \
  -H "Authorization: Bearer $TOKEN" -H 'Idempotency-Key: minimal-1' \
  -d @examples/minimal.json
```

## Go + pstack

[go-pstack/build.json](go-pstack/build.json) 构建的镜像包含：

- **Go 工具链**：Go 1.27.2，从 go.dev 下载，按官方 sha256 校验（amd64/arm64）；另装 `gcc`，使 `go test -race` 可用；`gopls` v0.23.0。
- **pstack**：取自 `cursor/plugins` 锁定提交 `d73344be` 的 51 个 skill，安装为 Codex 的 admin skill（`/etc/codex/skills/pstack--<skill>`）。
- **未包含的部分**：pstack 自带两个 Cursor subagent（`agents/`），在 Codex 里没有对应物，所以示例里写了 `"exclude": ["agents"]`。不写会在提交时被拒绝，不会被悄悄丢弃。`poteto-mode` 这类原本要派 subagent 的 skill，会由 Codex 在当前会话里直接执行。
- **启动脚本**：把 `/workspace` 加入 git 的 safe.directory。

[go-pstack/run.sh](go-pstack/run.sh) 走完整个流程：提交构建 → 跟踪日志 → 启动镜像（控制 API）→ 用你的 OpenAI 兼容服务打开 Codex 会话 → 让 Codex 按 pstack 的 `tdd` skill 写一个 Go 函数并运行 `go test -race`。

```sh
BUILDER=http://127.0.0.1:8080 BUILDER_TOKEN=... \
PROVIDER_BASE_URL=https://api.example.com/v1 PROVIDER_API_KEY=sk-... MODEL=<模型 id> \
examples/go-pstack/run.sh
```

模型服务要支持 OpenAI Responses API（`POST {baseUrl}/responses`），Codex 只走这个接口。base URL 和 key 在打开会话时通过控制 API 的 `provider` 传入（见 [docs/agent-api.md](../docs/agent-api.md#模型服务)）：key 只进入该会话 Codex 进程的环境变量，不会写进镜像，也不会出现在响应或日志里。镜像里不需要 Codex 登录。

实测结果见 [docs/verification.md](../docs/verification.md#示例go--pstack)。
