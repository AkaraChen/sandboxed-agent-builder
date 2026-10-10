# 镜像内的 Agent 控制 API

每个镜像都带一个用 [Hono](https://hono.dev) 写的 HTTP 服务（`/opt/agent/server`，源码在本仓库 `runtime/agent-server`）。它把容器里 agent 的会话控制暴露给调用方，是一层很薄的胶水：会话、事件和控制语义都来自 [oar](https://github.com/botiverse/oar)，服务只负责把 HTTP 请求映射到 oar 调用，并为轮询和 SSE 保存每个会话的事件。

## 启动

容器的默认命令就是这个服务（`CMD ["serve"]`）。入口先装好 Codex 凭据、执行 startup 脚本，然后 `exec node /opt/agent/server/src/main.js`：

```sh
docker run -d -p 8080:8080 \
  -v /secure/codex-auth.json:/run/codex-auth.json:ro -e CODEX_AUTH_JSON_FILE=/run/codex-auth.json \
  -e AGENT_API_TOKEN="$(openssl rand -hex 32)" \
  -e PROBE_KEY=...                       # resolvedSpec.runtimeEnv 里的变量
  -v agent-workspace:/workspace \
  IMAGE
```

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `AGENT_API_TOKEN` | 无 | `/v1/*` 的 bearer token。未设置时拒绝启动（退出码 64），除非设 `AGENT_API_INSECURE=1` |
| `AGENT_API_INSECURE` | 无 | `1` 表示不鉴权，只在可信网络里用 |
| `AGENT_API_HOST` / `AGENT_API_PORT` | `0.0.0.0` / `8080` | 监听地址 |
| `AGENT_API_CWD` | `/workspace` | 新会话的默认工作目录 |
| `AGENT_API_MAX_SESSIONS` | `4` | 同时打开的会话上限 |

容器参数不是 `serve` 时，仍然原样交给 oar CLI（例如 `IMAGE run codex "…"`）。agent 类型取自 `/opt/agent/manifest.json` 的 `agent.type`（目前为 `codex`）。`manifest.controlApi` 记录了 API 的版本和端口。

服务用的 oar 库就是镜像里 oar CLI 安装的那一份，版本与构建锁定的 oar 版本一致，构建时会检查。服务自身的依赖（`hono`、`@hono/node-server`）由 `package-lock.json` 锁定，用 `npm ci` 安装。

## 接口

除 `/healthz` 外都需要 `Authorization: Bearer $AGENT_API_TOKEN`。错误格式为 `{"error": {"code", "message"}}`。

| 方法与路径 | 说明 |
| --- | --- |
| `GET /healthz` | 存活检查 |
| `GET /v1/agent` | 镜像契约版本、`agent`（类型与锁定版本）、oar 版本、安装探测结果、会话数 |
| `GET /v1/agent/models` | 当前登录下可用的模型（oar `listModels`：`ok` / `unauthenticated` / `unsupported`） |
| `POST /v1/sessions` | 打开会话，返回 201 |
| `GET /v1/sessions` | 列出本进程打开的会话 |
| `GET /v1/sessions/{id}` | 状态、模型、effort、token 用量、上下文占用、最新事件序号 |
| `POST /v1/sessions/{id}/input` | 发送输入 |
| `POST /v1/sessions/{id}/abort` | 中止当前轮 |
| `GET /v1/sessions/{id}/events?after=-1&limit=500` | 按序号增量读取事件 |
| `GET /v1/sessions/{id}/events/stream` | SSE 事件流 |
| `DELETE /v1/sessions/{id}` | 关闭会话，结束 agent 进程，返回 204 |

### 打开会话

```json
POST /v1/sessions
{"model": "gpt-6.1-sol", "effort": "high", "cwd": "/workspace", "appendSystemPrompt": "Run the tests before you finish."}
```

可选字段与 oar `SessionOptions` 一致：`cwd`、`model`、`effort`、`serviceTier`、`resume`、`systemPrompt`、`appendSystemPrompt`、`env`，另加 `provider`（见下节）。未知字段返回 400。`resume` 填之前的 `sessionId`（Codex 的 thread id），用来接着原会话继续；会话历史保存在 `$CODEX_HOME`，所以要跨容器恢复，调用方需要持久化 `/home/agent/.codex`。

| 状态码 | code | 含义 |
| --- | --- | --- |
| 201 | | `{"sessionId", "status", "model", "effort", "cwd", "createdAt", "lastSeq"}` |
| 400 | `unsupported_option` | runtime 不接受这个选项（oar `UnsupportedOptionError`） |
| 404 | `resume_not_found` | 找不到要恢复的会话 |
| 429 | `too_many_sessions` | 已达 `AGENT_API_MAX_SESSIONS` |
| 502 | `open_failed` | agent 进程启动失败，例如未登录 |

### 模型服务

打开会话时可以直接指定 Codex 使用的模型服务，不需要镜像里有 Codex 登录：

```json
POST /v1/sessions
{"model": "gpt-6-astra", "provider": {"baseUrl": "https://llm.example.com/v1", "apiKey": "sk-..."}}
```

- **前提**：`baseUrl` 必须是支持 OpenAI Responses API 的地址（`POST {baseUrl}/responses`），Codex 只走这个接口。`model` 必填，填该服务的模型 id。
- **实现方式**：服务在启动 Codex 时追加一组配置，定义一个名为 `sab_provider` 的自定义 provider：
  ```
  -c model_provider="sab_provider"
  -c model_providers.sab_provider.base_url="<baseUrl>"
  -c model_providers.sab_provider.env_key="SAB_PROVIDER_API_KEY"
  -c model_providers.sab_provider.wire_api="responses"
  ```
- **key 的去向**：`apiKey` 只放进该会话 Codex 进程的环境变量 `SAB_PROVIDER_API_KEY`，不进入启动参数、oar 记录、事件、会话列表或任何响应。会话信息里只回显 `provider.baseUrl`。
- **作用范围**：每个会话可以用不同的服务和 key；不带 `provider` 时，仍使用容器里的 Codex 登录（`CODEX_AUTH_JSON_FILE` / `OPENAI_API_KEY`）。
- **限制**：目前只对 Codex 生效，其他 agent 类型返回 400 `unsupported_option`。`resume` 一个会话时要再传一次 `provider`，因为它不会被保存。

### 发送输入

```json
POST /v1/sessions/{id}/input
{"text": "Fix the failing test", "wait": true, "timeoutMs": 600000}
```

- `wait: true`：开启新一轮并等它结束（oar `promptAndWait`）。返回 200：`{"kind": "ended" | "interrupted", "outcome": {"kind": "completed" | "aborted" | …}, "text": "本轮助手文本"}`。超时时 `kind` 为 `interrupted`，轮次本身不会被中止。会话正忙时返回 409（`code: "busy"`）。
- 不带 `wait`：用 oar `deliver` 投递，立即返回 202 `{"landed": "prompted" | "steered" | "queued", "inputId"}`。空闲时开启新一轮；正在运行时按 `when` 处理：`now` 表示插入当前轮（steer），`after_turn` 表示排在当前轮之后，`when_idle` 表示等空闲后再开新一轮；不填由 oar 决定。被拒绝时返回 409，带 `code` 和 `reason`。

输入被接受，不代表 agent 已经处理完；结果请看事件或会话状态。

### 中止

`POST /v1/sessions/{id}/abort`：接受时返回 200 `{"kind": "accepted"}`，被拒绝（例如没有正在运行的轮）时返回 409。轮次实际结束后，会话状态变为 `{"kind": "idle", "lastTurnOutcome": {"kind": "aborted"}}`。

### 事件

事件就是 oar 的 `Event`：`user_message`、`turn_started`、`text_delta`、`reasoning`、`tool_call_started` / `tool_call_ended`、`usage`、`turn_ended` 等，每条都带 `seq`、`sessionId`、`agentPath`。字段定义见 oar 的 record stream 规范。文本按最多 250 ms 合并成块。

- 轮询：`GET …/events?after=<上次的 nextAfter>`，返回 `{"items", "nextAfter", "oldestSeq", "truncated"}`。同一 `seq` 可能对应多条事件（一条原生记录可以投影出几个事件），所以下一次请求用 `nextAfter` 作 `after`。
- SSE：`GET …/events/stream`，可带 `?after=` 或 `Last-Event-ID`。先补发之后的事件，再推送实时事件。`event:` 是事件类型，`id:` 是 `seq`，`data:` 是事件 JSON。空闲时每 15 秒发一次 `ping`，会话关闭时发 `closed`。

每个会话最多在内存里保留 20000 条事件，超出后丢弃最早的并标记 `truncated`。服务进程重启后事件不保留；完整记录请用 oar CLI 的 `--record`，或由调用方自己保存。

## 示例

```sh
U=http://127.0.0.1:8080; H="Authorization: Bearer $AGENT_API_TOKEN"
S=$(curl -s -XPOST $U/v1/sessions -H "$H" -d '{}' | jq -r .sessionId)
curl -sN $U/v1/sessions/$S/events/stream -H "$H" &                     # 实时事件
curl -s -XPOST $U/v1/sessions/$S/input -H "$H" -d '{"text":"List the files here","wait":true}' | jq .text
curl -s -XPOST $U/v1/sessions/$S/input -H "$H" -d '{"text":"Also count them","when":"now"}'
curl -s -XPOST $U/v1/sessions/$S/abort -H "$H"
curl -s -XDELETE $U/v1/sessions/$S -H "$H"
```

## 边界

- 这个服务只管容器里的 agent 会话。容器本身的启停、重启、工作区挂载和凭据轮换都由调用方负责。
- 会话状态只在服务进程的内存里；容器重启后需要用 `resume` 重新打开。
- 鉴权只有一个共享 token，不区分用户。需要更细的权限控制时，在调用方的网关里做。

## 开发

```sh
cd runtime/agent-server
ni          # 安装依赖（含测试用的 @botiverse/oar）
nr test     # node:test，基于 oar 的 scripted runtime，不需要 Codex 或登录
```

builder 用 `go:embed` 把 `package.json`、`package-lock.json` 和 `src/` 编进二进制，每次构建都复制进构建上下文。
