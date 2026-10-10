# 镜像内的 Agent 控制 API

每个镜像都带一个用 [Hono](https://hono.dev) 写的 HTTP 服务，用来在容器里驱动 Codex。服务装在镜像的 `/opt/agent/server`，源码在本仓库 `runtime/agent-server`。

服务通过官方 SDK [`@openai/codex-sdk`](https://www.npmjs.com/package/@openai/codex-sdk) 调用 Codex：

- 一个会话就是一个 Codex thread；
- 每一轮是一次 `codex exec`；
- 服务本身只负责把 HTTP 请求转成 SDK 调用，并为轮询和 SSE 保存事件。

SDK 与 Codex CLI 版本相同，且依赖完全相同版本的 `@openai/codex`。构建时只安装锁定版本的 SDK，镜像里的 `codex` 命令就是它带进来的那个 CLI，一个版本同时锁住两者。

## 启动

容器的默认命令就是这个服务（`CMD ["serve"]`）。入口先装好 Codex 凭据、执行 startup 脚本，然后启动服务：

```sh
docker run -d -p 8080:8080 \
  -e AGENT_API_TOKEN="$(openssl rand -hex 32)" \
  -v agent-workspace:/workspace \
  IMAGE
```

模型服务和插件凭据可以在打开会话时传入（见下文），所以容器本身不需要 Codex 登录。如果要用 ChatGPT 登录，就挂载 auth.json 并设置 `CODEX_AUTH_JSON_FILE`，或者设置 `OPENAI_API_KEY`，见[运行契约](runtime-contract.md)。

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `AGENT_API_TOKEN` | 无 | `/v1/*` 的 bearer token。不设置时服务拒绝启动（退出码 64），除非设置了 `AGENT_API_INSECURE=1` |
| `AGENT_API_INSECURE` | 无 | 设为 `1` 时不鉴权，只在可信网络中使用 |
| `AGENT_API_HOST` / `AGENT_API_PORT` | `0.0.0.0` / `8080` | 监听地址 |
| `AGENT_API_CWD` | `/workspace` | 新会话的默认工作目录 |
| `AGENT_API_MAX_SESSIONS` | `4` | 同时打开的会话数上限 |

容器参数不是 `serve` 时，会原样交给 `codex` 命令执行，例如 `IMAGE exec --skip-git-repo-check "…"`。

## 接口

除 `/healthz` 外，所有接口都需要请求头 `Authorization: Bearer $AGENT_API_TOKEN`。出错时返回 `{"error": {"code", "message"}}`。

| 方法与路径 | 说明 |
| --- | --- |
| `GET /healthz` | 存活检查 |
| `GET /v1/agent` | 镜像契约版本、锁定的 Codex 包与版本、`codex --version`、会话数 |
| `POST /v1/sessions` | 打开会话，返回 201 |
| `GET /v1/sessions` | 列出会话 |
| `GET /v1/sessions/{id}` | 会话状态 |
| `POST /v1/sessions/{id}/input` | 开始新的一轮 |
| `POST /v1/sessions/{id}/abort` | 中止正在运行的一轮 |
| `GET /v1/sessions/{id}/events?after=0&limit=500` | 按序号增量读取事件 |
| `GET /v1/sessions/{id}/events/stream` | 用 SSE 推送事件 |
| `DELETE /v1/sessions/{id}` | 关闭会话（先中止正在运行的一轮），返回 204 |

### 打开会话

```json
POST /v1/sessions
{"model": "gpt-6-astra", "effort": "high", "cwd": "/workspace", "env": {"MCP_TOKEN_GITHUB_GITHUB": "ghp_..."},
 "provider": {"baseUrl": "https://llm.example.com/v1", "apiKey": "sk-..."}}
```

字段都可选。不认识的字段返回 400。

| 字段 | 说明 |
| --- | --- |
| `model` | 模型 id，对应 `codex --model` |
| `effort` | `minimal` / `low` / `medium` / `high` / `xhigh` / `max` |
| `cwd` | 工作目录，默认 `/workspace` |
| `resume` | 要接着运行的 Codex thread id，即之前会话的 `threadId`。thread 保存在 `$CODEX_HOME`，跨容器恢复需要调用方持久化 `/home/agent/.codex` |
| `env` | 只给这个会话的 Codex 进程用的环境变量，叠加在服务自身的环境之上。典型用途是插件 HTTP MCP 服务器的 token `MCP_TOKEN_<PLUGIN>_<SERVER>`，变量名列在构建结果的 `resolvedSpec.runtimeEnv` |
| `provider` | `{baseUrl, apiKey}`，见下节 |

每个会话固定以 `sandboxMode: danger-full-access`（容器本身就是隔离边界）、`approvalPolicy: never` 和 `skipGitRepoCheck` 运行。

返回 201：

```json
{"sessionId": "…", "threadId": null, "createdAt": "…", "cwd": "/workspace", "model": "gpt-6-astra",
 "provider": {"baseUrl": "https://llm.example.com/v1"}, "status": "idle", "lastTurn": null, "lastSeq": 0}
```

新会话的 `threadId` 一开始是 `null`，第一轮开始后 Codex 才会报出 thread id；用 `resume` 打开的会话一开始就有。会话数达到上限时返回 429 `too_many_sessions`。

### 模型服务

`provider` 让会话直接使用指定的 OpenAI 兼容服务，容器里不需要 Codex 登录：

- `baseUrl` 必须是支持 OpenAI Responses API 的地址（`POST {baseUrl}/responses`）。
- 填了 `provider` 时 `model` 必须填，否则 400，因为 Codex 默认的模型名在其他服务上通常不存在。
- 实现方式是 SDK 的 `baseUrl`/`apiKey` 选项：`baseUrl` 变成 `--config openai_base_url=…`，`apiKey` 只放进该会话 Codex 进程的环境变量 `CODEX_API_KEY`。
- key 不会出现在命令行参数、事件、会话信息或任何响应里，会话信息只回显 `provider.baseUrl`。
- `resume` 一个会话时要再传一次 `provider`，服务不保存它。

不传 `provider` 时，使用容器里的 Codex 登录。

### 运行一轮

```json
POST /v1/sessions/{id}/input
{"text": "Fix the failing test", "wait": true, "timeoutMs": 600000}
```

- **`wait: true`**：等这一轮结束再返回，200：`{"kind": "completed" | "failed" | "aborted", "finalResponse": "最后一条回复", "usage": {...}, "error": "失败原因", "endedAt": "…"}`。如果先超时，返回 202 `{"kind": "timeout", "status": "running"}`，这一轮继续在跑，可以再查状态或中止。
- **不带 `wait`**：立即返回 202 `{"status": "running", "lastSeq": n}`，之后看事件或会话状态。
- **会话正忙**：已经有一轮在跑时返回 409 `busy`。Codex 的 `exec` 不支持在一轮中途追加输入，所以没有 steer 或排队，只能等这一轮结束或先中止。

### 中止

`POST /v1/sessions/{id}/abort` 会结束这一轮的 `codex exec` 进程，等它退出后返回这一轮的结果，`kind` 为 `aborted`。没有正在运行的一轮时返回 409 `no_active_turn`。

### 会话状态

`GET /v1/sessions/{id}` 返回与打开会话相同的结构：

- `status`：`idle` 或 `running`；
- `lastTurn`：上一轮的结果，结构同 `wait` 的返回值；
- `lastSeq`：最新事件序号。

### 事件

事件就是 Codex 的 thread 事件（`codex exec --experimental-json` 输出的 JSONL）：

- `thread.started`（带 `thread_id`）
- `turn.started`
- `item.started` / `item.updated` / `item.completed`：item 类型有 `agent_message`、`reasoning`、`command_execution`、`file_change`、`mcp_tool_call`、`web_search`、`todo_list`、`error`
- `turn.completed`（带 `usage`）、`turn.failed`、`error`

服务给每条事件加上递增的 `seq` 和 `time`。每一轮结束时，服务还会追加一条自己的 `{"type": "turn.ended", "outcome": "completed" | "failed" | "aborted"}`。

- **轮询**：`GET …/events?after=<上次的 nextAfter>`，返回 `{"items", "nextAfter", "truncated"}`。
- **SSE**：`GET …/events/stream`，可以带 `?after=` 或 `Last-Event-ID`。服务先补发之后的事件，再推送实时事件：`event:` 为事件类型，`id:` 为 `seq`，`data:` 为事件 JSON。空闲时每 15 秒发一次 `ping`，会话关闭时发 `closed`。

每个会话最多在内存里保留 20000 条事件，超出后丢弃最早的，并把 `truncated` 标为 true。服务重启后事件不保留，但 Codex thread 本身还在 `$CODEX_HOME`，可以用 `resume` 接着运行。

## 示例

```sh
U=http://127.0.0.1:8080; H="Authorization: Bearer $AGENT_API_TOKEN"
S=$(curl -s -XPOST $U/v1/sessions -H "$H" \
  -d '{"model":"gpt-6-astra","provider":{"baseUrl":"https://llm.example.com/v1","apiKey":"'"$KEY"'"}}' | jq -r .sessionId)
curl -sN $U/v1/sessions/$S/events/stream -H "$H" &              # 实时事件
curl -s -XPOST $U/v1/sessions/$S/input -H "$H" -d '{"text":"List the files here","wait":true}' | jq .finalResponse
curl -s -XPOST $U/v1/sessions/$S/input -H "$H" -d '{"text":"Now count them"}'
curl -s -XPOST $U/v1/sessions/$S/abort -H "$H"
curl -s -XDELETE $U/v1/sessions/$S -H "$H"
```

完整流程见 [examples/go-github](../examples/go-github/run.sh)。

## 边界

- 这个服务只管容器里的 Codex 会话。容器的启停、重启、工作区挂载和凭据轮换由调用方负责。
- 会话只存在服务进程的内存里；服务重启后，用 `resume` 加 `threadId` 重新打开。
- 鉴权只有一个共享 token。需要按用户区分权限时，在调用方的网关里做。

## 开发

```sh
cd runtime/agent-server
ni          # 安装依赖（含测试用的 @openai/codex-sdk）
nr test     # node:test；SDK 指向 fixtures/fake-codex.mjs 这个假的 codex 可执行文件，不需要模型或登录
```

builder 用 `go:embed` 把 `package.json`、`package-lock.json` 和 `src/` 编进二进制，每次构建时复制到构建上下文；镜像里再额外安装与锁定 Codex 版本相同的 `@openai/codex-sdk`。
