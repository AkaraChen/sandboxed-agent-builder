# 验证记录

2026-10-10 在 linux/amd64（Docker 29.1.3，API 1.52，containerd 镜像存储，buildx v0.37.2）上实测。builder 以 `deploy/builder.example.yaml` 的同类配置运行（本地 `file://` 插件源、内置 registry 监听 `127.0.0.1:15000`）。实际锁定版本：Debian trixie-slim `sha256:a29215f6…`，Node.js 24.21.0，Codex 0.162.1，oar（`@botiverse/oar-cli`）0.51.0，akhook `3f4a7678`，Rust 工具链 `rust:1-alpine@sha256:0cce0a5e…`。

## 自动化测试

```sh
go vet ./...
go test -race ./...
go test -race -tags docker ./internal/adapter/docker   # 需要 Docker socket
```

| 范围 | 覆盖 |
| --- | --- |
| `adapter/postgres`（真实 PostgreSQL 17，每个测试单独建库） | goose 迁移重复执行，两个实例同时迁移；幂等键按调用方作用域唯一；8 个 worker 经两个连接池（模拟两个实例）用 SKIP LOCKED 并发领取 40 个尝试，每个只被领取一次；日志中的 NUL 与非法 UTF-8 可以写入；时间以 UTC 读出；FIFO；租约续期、过期、接管后旧 worker 的续期/日志/完成均被拒绝（代次）；取消 queued/running/已成功；重试（活跃时拒绝、幂等重放、键冲突、保留旧尝试和日志）；尝试与日志分页；日志大小上限与截断标记 |
| `app` | 提交幂等（键序/空白无关、重放不再解析、内容不同 409、新键同规格新构建）；非法规格不解析；成功路径与 secret 脱敏；失败后重试保留历史且不重新解析；运行中取消直到执行器返回才记录 cancelled；租约丢失后不写结果；发布失败记为 publish 阶段且无 registry 引用；核对：有镜像补录成功、无镜像 interrupted、registry 未确认 interrupted、活租约不动；关闭时超出宽限期记 interrupted |
| `api` | 认证、缺幂等键、202/200 重放/409、跨调用方 404、`Retry-After`、重试冲突与重放、尝试分页与 limit 校验、日志增量与 `complete`、取消 |
| `spec`、`plugin`、`plan`、`adapter/resolve` | 默认值与拒绝规则（Ubuntu、未验证 Alpine、未知/启动脚本 secret…）；插件读取与 Codex 配置生成、变量规则、skill 冲突；每个 build 脚本独立 RUN 且 secret 只挂在对应步骤、startup 只复制不执行、插件内容与锁定摘要不符时拒绝；生成的入口脚本按序执行 startup、失败不启动 oar；npm latest 稳定版、预发布拒绝、Node 主版本线选择与 sha256 |
| `adapter/docker`（`-tags docker`） | 经 Docker socket 的 BuildKit 构建、secret 挂载、attempt 标签查找、超时取消在数秒内返回 |

## 真实构建与运行

| 场景 | 结果 |
| --- | --- |
| Debian slim 基础构建（apt 脚本 + 使用 secret 的脚本 + startup） | 冷构建 2 分 13 秒（含编译 akhook），镜像 332 MB；日志中 secret 值为 `[redacted]`；镜像内无 `/run/secrets`；当时的默认命令（`installation codex`，现已改为 `serve`）先执行 startup 再报告 codex-cli 0.162.1 |
| Codex smoke test：`agent-entrypoint run codex "Reply with exactly the word pong…"` | 使用本机 Codex 登录的临时副本注入，`[turn completed]`，输出 `pong`（模型 gpt-6.1-sol） |
| 插件（`testdata/plugins/probe`）Debian slim | `oar mcps codex` 列出 `probe_probe`（1 个工具）；`oar skills codex` 列出 `probe-skill`（admin，`/etc/codex/skills/probe--probe-skill`）；setup 安装的 `probe-setup-check` 可用；真实 Codex 一轮输出 `MCP=MAGIC-k123-static-ok SKILL=KUMQUAT-7 HOOK=ZEBRA-42 TOUCH=blocked`；`prompt_submit` 与 `stop` hook 的输入文件均存在；`/workspace/forbidden.txt` 未创建，oar 记录中含 akhook 的拒绝消息 |
| 插件 Alpine 3.22（musl） | 同上全部通过（Node.js 24.21.0 musl 版来自 unofficial-builds.nodejs.org；Codex 自带 musl 二进制；akhook 静态 musl） |
| 取消运行中的构建（`sleep 300` 脚本） | 取消接口 202 + `running` + `cancelRequestedAt`；9 秒后 `cancelled`，daemon 中无残留 `sleep` |
| `kill -9` builder 后重启（构建进行中） | BuildKit 随会话断开停止构建；租约过期后核对为 `interrupted`（`recovered: true`），未伪造成功 |
| 模拟“镜像已导出但未记录成功”（把成功尝试改回 running、去掉产物、租约过期） | 重启后按 attempt tag 与 label 在 daemon 找到镜像，补录 `succeeded`、`recovered: true` |
| registry 输出，首次 | 推送成功但 daemon 未返回 aux digest，记录为 `publish_failed`（本地产物保留，无 registry 引用）——未误报可拉取。随后修复从状态行读取 digest |
| registry 输出，重试（attempt 2） | 推送并经 registry `HEAD` 核对 digest 一致，返回 `127.0.0.1:15000/agents/demo@sha256:bdb971b3…`；attempt 1 的失败保留；分页 `limit=1` 返回 `nextCursor: "1"` |
| registry 拉取与重启 | 删除本地所有相关 tag，重启 builder（registry 随之重启），`docker pull 127.0.0.1:15000/agents/demo@sha256:bdb971b3…` 成功并可运行；存储目录 316 MB |
| OTel | 设置 `OTEL_EXPORTER_OTLP_ENDPOINT` 指向本地接收端，`build.attempt` span 含 build/attempt ID；JSON 日志每条带 `build_id`、`attempt_id`；secret 值在 trace、数据库（含 WAL）与服务日志中均不存在 |

重复插件验证：

```sh
scripts/smoke-image.sh IMAGE ~/.codex/auth.json ./evidence
```

镜像需用 `testdata/plugins/probe` 构建（`plugins: [{"source": "<含该目录的 git 仓库>", "revision": "...", "path": "testdata/plugins/probe"}]`），运行时提供 `PROBE_KEY=k123`。

## Agent 控制 API（镜像内 Hono 服务）

- `runtime/agent-server`：`nr test` 共 5 个 node:test 用例，基于 oar 的 scripted runtime（真实的 `Session` 语义）：鉴权；打开会话、等待一轮完成、事件分页、空闲时投递、删除；运行中 steer、中止，以及忙时 `wait` 返回 409；SSE 从 `Last-Event-ID` 补发后继续推送实时事件；参数校验、会话上限和 404。
- 真实镜像（Debian slim + probe 插件，oar 0.52.0，Codex 0.162.1）以默认命令 `serve` 启动：无 token 返回 401；`/v1/agent` 返回 agent 与 oar 版本；`/v1/agent/models` 列出 4 个模型。
- 打开会话后，经 `input`（`wait: true`）跑了一轮真实 Codex，输出 `MCP=MAGIC-k123-static-ok SKILL=KUMQUAT-7 HOOK=ZEBRA-42 TOUCH=blocked`。`forbidden.txt` 没有被创建，两个生命周期 hook 都执行了，SSE 同时收到 14 个事件。
- 让 Codex 执行 `sleep 90`，运行中调用 `abort`，会话变为 `idle` 且 `lastTurnOutcome: aborted`。
- `DELETE` 返回 204。用 `resume` 重新打开同一个 thread 后，Codex 答出了上一轮得到的 `KUMQUAT-7`。用不存在的 id 恢复返回 404。

## PostgreSQL（goose）与多实例

用两个 builder 实例共享同一个 PostgreSQL 17 数据库（同一 Docker daemon，端口和数据目录各自独立），实测结果：

- 两个实例同时启动，goose 迁移只执行一次（`goose_db_version`：0、1）。
- 经实例 1 提交 4 个各需 20 秒的构建：两个实例各领取了一部分，4 个都成功；经实例 2 也能查到实例 1 受理的构建及其日志。
- 构建在实例 1 上运行时，经实例 2 发出取消：返回 202，实例 1 停止构建后状态变为 `cancelled`。
- 构建在实例 1 上运行时 `kill -9` 实例 1：实例 2 在租约过期后接管（generation 1 → 2），记为 `interrupted`、`recovered: true`。
- registry 模式：推送并核对 digest，库里 `artifact->>'registryRef'` 与 API 返回一致；删除本地镜像后按 digest 拉取成功。

## Alpine 结论

Alpine 3.22 在 linux/amd64 上通过与 Debian 相同的全部验证。仍默认关闭（`alpineVerified: false`），原因：Node.js 的 musl 构建来自 Node.js 的非官方构建（experimental 级别）；arm64 未验证；插件自带的原生依赖需要逐个验证。运维在目标平台跑完上面的 smoke test 后再打开。

## akhook 上游核查

akhook（main `3f4a7678`）已具备 builder 需要的全部能力：Codex adapter（`akhook codex hook pre_tool_use|prompt_submit|stop`）、生命周期规则（`prompt_submit` 补充上下文、`stop`）、工具规则（`shell_exec`/`file_change`/`tool_call`，deny/ask/decide）、`AKHOOK_ADDITIONAL_CONFIG_PATH` 附加配置。上述实测确认三类事件在 oar 驱动的 Codex 0.162.1 上均生效，因此本次无需向上游提交 PR，builder 也不包含任何 hook 适配代码。akhook 没有发布版本，builder 在提交时锁定 main 的提交并在镜像中从源码编译。

## 未验证 / 已知限制

- 未在 arm64 或多节点部署上验证；只构建 daemon 原生平台。
- npm 全局安装的 Codex/oar 锁定了包版本，但其传递依赖未锁定；apt/apk 安装的系统包随镜像源变化。基础镜像、Node.js、akhook、插件内容是锁定的。
- 插件 MCP URL 中的运行时变量（如 AXP 的 `${AXP_GATEWAY_URL}/memory/mcp`）当前被拒绝：Codex 不在 `url` 中替换环境变量，需要入口在启动时渲染配置，留作后续。
- 构建不跨 builder 重启续接；重启前未落库的少量日志可能丢失。
- PostgreSQL adapter 未实现（见架构说明的选型）。
