# 构建 API

所有接口返回 JSON。`/healthz`、`/readyz` 之外都需要 `Authorization: Bearer <token>`；token 在配置里以 sha256 登记（`printf %s "$TOKEN" | sha256sum`），每个 token 对应一个调用方作用域。构建只对创建它的调用方可见，其他调用方得到 404。

错误统一为：

```json
{"error": {"code": "invalid_request", "message": "the request is invalid", "details": ["base.family: ubuntu is not offered; ..."]}}
```

| HTTP | code | 含义 |
| --- | --- | --- |
| 400 | `invalid_request` | 规格或参数不合法，`details` 列出全部问题 |
| 400 | `idempotency_key_required` | 缺少 `Idempotency-Key`（1-200 字符） |
| 401 | `unauthorized` | 无有效 token |
| 404 | `not_found` | 构建/尝试不存在或不属于调用方 |
| 409 | `idempotency_conflict` | 同一幂等键对应了不同内容 |
| 409 | `active_attempt` | 重试时当前尝试仍为 queued/running |
| 502 | `resolve_failed` | 解析上游（Docker Hub、npm、Node.js、git）失败；未创建构建，可用同一幂等键重试 |

## POST /builds

提交构建。必须带 `Idempotency-Key`。请求体：

```json
{
  "spec": {
    "base": {"family": "debian-slim", "version": "trixie"},
    "platform": "linux/amd64",
    "node": {"version": "24"},
    "agent": {"type": "codex", "codex": {"version": "latest"}},
    "oar": {"version": "latest-stable"},
    "plugins": [
      {"source": "https://github.com/org/plugins.git", "revision": "v1.2.0", "path": "plugins/memory"}
    ],
    "scripts": {
      "build": [
        {"name": "dev tools", "run": "apt-get update && apt-get install -y --no-install-recommends jq && rm -rf /var/lib/apt/lists/*"},
        {"name": "private deps", "run": "NPM_TOKEN=$(cat /run/secrets/npm_token) npm i -g private-cli", "secrets": ["npm_token"]}
      ],
      "startup": [
        {"name": "prepare workspace", "run": "git config --global --add safe.directory /workspace"}
      ]
    },
    "output": {"type": "local"}
  },
  "noCache": false
}
```

除 `spec` 外均可省略；默认值见上例。字段说明：

- `base.family`：`debian-slim`（默认）或 `debian`，版本 `trixie`（默认）或 `bookworm`。其他值（包括 `alpine`、`ubuntu`）一律拒绝。
- `platform`：只接受 Docker daemon 的原生平台（不做跨架构模拟）。
- `node.version`：主版本线（`24`，取该线最新版本）或精确版本，至少 24（oar 的要求）。
- `agent`：Agent runtime。`type` 目前只能是 `codex`（默认），其配置放在与类型同名的字段里：`agent.codex.version` 为 `latest` 或精确版本。这样以后增加 runtime 时只新增类型和对应字段，现有请求不变；与 `type` 不符的配置或未知类型会被拒绝。
- `oar.version`：`latest-stable` 或精确版本。oar 是驱动所有 runtime 的入口，独立于 `agent`。
- `plugins[]`：插件所在 git 仓库（默认只允许 `https://`）、`revision`（提交、tag 或分支，提交时锁定为提交），以及仓库内的 `path`。插件须符合 [Agent Plugins](https://agent-plugins.org) 1.0.0 或 1.1.0 标准，映射规则见[运行契约](runtime-contract.md#插件映射)。
  - `plugin.json` 不合规时，提交返回 400。
  - 无效的 skill 或 MCP 条目会被跳过，并在 `resolvedSpec.plugins[].warnings` 中说明。
- `scripts.build[]`：按顺序每项一个 `RUN`，以 `/bin/sh -e` 执行；`secrets` 引用运维配置的 build secret，挂载在 `/run/secrets/<id>`，仅该步可见。
- `scripts.startup[]`：打包进镜像，容器每次启动时由入口按顺序执行，构建时不执行，不能引用 build secret。
- `output`：`{"type":"local"}`（默认）或 `{"type":"registry","repository":"agents/demo"}`（需启用内置 registry）。
- `noCache`：不使用 BuildKit 缓存。它影响构建方式而非内容，不计入规格摘要。

服务先检查幂等键：

- 新键：校验 → 解析并锁定（基础镜像 digest、Node.js 版本与 sha256、Codex/oar 版本、akhook 提交、插件提交）→ 持久化 → **202**，`Location: /builds/{id}`。持久化之后客户端断开不影响构建。
- 同键同内容（JSON 规范化后比较，键序与空白无关）：**200** 返回原构建，`Idempotent-Replayed: true`，不再解析。
- 同键不同内容：**409**。
- 同一规格换新键：新构建（规格摘要相同，重新解析 latest）。

响应（`GET /builds/{id}` 相同）：

```json
{
  "buildId": "bld_06gj8k56mmgqye4qy21pwytzj4",
  "createdAt": "2026-10-10T05:32:45.605Z",
  "status": "succeeded",
  "specDigest": "sha256:f2c8…",
  "noCache": false,
  "attempts": 1,
  "currentAttempt": {
    "attemptId": "att_06gj8k56mpq1p64wgdx4fgvdxr",
    "number": 1,
    "status": "succeeded",
    "createdAt": "…", "startedAt": "…", "endedAt": "…",
    "cancelRequestedAt": null,
    "failure": null,
    "artifact": {
      "imageId": "sha256:4204…",
      "daemonRef": "sandboxed-agent-builder/bld_…:att_…",
      "platform": "linux/amd64",
      "repository": "registry.example:5000/agents/demo",
      "digest": "sha256:…",
      "registryRef": "registry.example:5000/agents/demo@sha256:…"
    },
    "logsTruncated": false,
    "recovered": false
  },
  "spec": { "...原始请求的 spec..." },
  "resolvedSpec": { "...锁定后的规格，含 runtimeEnv..." }
}
```

`artifact.imageId` 与 `daemonRef` 只在本 builder 的 Docker daemon 中有效；registry 字段只有在推送完成且 registry 中确认了 manifest digest 后才出现。使用 containerd 镜像存储的 daemon 中 image ID 恰好等于 manifest digest，但两者语义不同，调用方跨机器只能用 `registryRef`。

`resolvedSpec.runtimeEnv` 列出镜像运行时读取的环境变量，目前是插件 HTTP MCP 服务器的 bearer token（`MCP_TOKEN_<PLUGIN>_<SERVER>`），由调用方在启动容器或打开会话时提供。

## GET /builds/{id}

当前状态。非终态时带 `Retry-After: 2`（建议轮询间隔，秒）。

尝试状态：`queued → running → succeeded | failed | cancelled | interrupted`，排队中取消直接 `cancelled`。`failure` 为 `{stage, code, message}`：

| stage | code | 含义 |
| --- | --- | --- |
| `plan` | `plan_failed` | 生成构建上下文失败（插件内容与锁定摘要不符、secret 缺失等） |
| `build` | `build_failed` | Docker/BuildKit 构建失败，包括脚本退出非 0 |
| `publish` | `publish_failed` | 推送或 manifest 核对失败；本地 `artifact` 保留，registry 字段为空 |
| 任意 | `cancelled` | 取消请求生效且构建已停止 |
| 任意 | `interrupted` | builder 关闭或 worker 失联，无法确认结果；需显式重试 |

`recovered: true` 表示结果由重启后的核对写入，而不是执行它的 worker。

## GET /builds/{id}/attempts?limit=20&cursor=

按尝试序号升序分页。`limit` 1-100；响应 `{"items": [...], "nextCursor": "2"}`，`nextCursor` 为 `null` 表示没有更多。

## GET /builds/{id}/logs?attempt=&after=0&limit=500

读取一个尝试的日志（默认当前尝试），返回序号大于 `after` 的条目。`limit` 1-2000。

```json
{
  "attemptId": "att_…", "status": "running",
  "entries": [{"seq": 1, "time": "…", "stage": "plan", "stream": "builder", "text": "attempt 1 of build bld_…"}],
  "nextAfter": 1, "complete": false, "truncated": false
}
```

用 `nextAfter` 作下一次的 `after` 增量读取。`complete` 为真表示尝试已结束且已读到末尾。`stream`：`builder`（服务自身）、`buildkit`（BuildKit plain 进度，含每个 RUN 的输出）、`push`。每个尝试的日志有大小上限（默认 8 MiB），超过后追加一条截断说明并 `truncated: true`。已配置的 build secret 值在入库前替换为 `[redacted]`。worker 进程崩溃时尚未写入的少量日志会丢失。

## POST /builds/{id}/cancel

请求取消当前尝试，返回尝试的实际状态：

- queued：立即 `cancelled`，**200**。
- running：记录 `cancelRequestedAt`，仍为 `running`，**202**；worker 在下一次租约续期（租约的 1/3，默认 10 秒内）看到请求、停止 BuildKit、确认停止后才变为 `cancelled`。如果构建在此之前已完成，结果保持 `succeeded`。
- 已结束：原样返回，**200**。

## POST /builds/{id}/retry

必须带 `Idempotency-Key`。当前尝试结束后（任何终态，含成功——用于主动重建同一锁定规格）创建新尝试，**202**；同键重放 **200**。新尝试沿用已锁定的版本与提交，不重新解析 latest；旧尝试的状态、失败与日志保留。

## GET /healthz、GET /readyz

`/healthz` 只表示进程存活；`/readyz` 检查数据库和 Docker daemon。

## 轮询示例

```sh
B=$(curl -s -XPOST $URL/builds -H "Authorization: Bearer $T" -H 'Idempotency-Key: env-42-v7' -d @spec.json | jq -r .buildId)
after=0
while :; do
  page=$(curl -s "$URL/builds/$B/logs?after=$after" -H "Authorization: Bearer $T")
  echo "$page" | jq -r '.entries[].text'
  after=$(echo "$page" | jq .nextAfter)
  [ "$(echo "$page" | jq .complete)" = true ] && break
  sleep 2
done
curl -s $URL/builds/$B -H "Authorization: Bearer $T" | jq .currentAttempt.artifact
```
