# 抽象消融记录

2026-10-10，针对“代码量过多”的反馈，对实现中的每个抽象逐一做消融：去掉或内联它，跑完整检查（goimports、`go vet`（含 `-tags docker`）、`go test -race ./...`），统计非空、非注释行，再判断是否丢失了某项需求。保留下来的删减各对应一个提交（`refactor(消融 Ax)`）。

统计口径：git 跟踪的 `.go` 与 `.js` 文件，生产代码不含 `_test.go` 和 `*.test.js`。

| | 生产代码 | 测试代码 |
| --- | --- | --- |
| 消融前（`a6b89d5`） | 4213 | 1398 |
| 消融后 | 3866（−347，−8.2%） | 1353（−45） |

## 已删除

| 编号 | 抽象 | 生产行数 | 结论与依据 |
| --- | --- | --- | --- |
| A1 | `Clock`、`IDs` 端口和 `adapter/system` 包 | −16 | 测试本来就用真实时间和随机 ID，替身没有带来任何可测性。ID 生成移入 `domain.NewID` |
| A2 | `Planner` 端口和 `plan.Planner` 适配器 | −17 | 构建计划是只依赖 `Fetcher` 的纯函数，worker 直接调用 `plan.Write`。应用层测试因此也走真实的计划生成，覆盖反而更多 |
| A3 | 独立的 `Reconciler` 类型 | −9 | 依赖和 worker 完全相同；改为 `Worker.Reconcile`，由 `Worker.Run` 定期调用 |
| A4 | 存储的 `Expired` 加按 ID `TakeOver` 两步 | −40 | 领取 queued 尝试和接管租约过期的尝试，共用同一个受代次保护的写事务 `claim` |
| A5 | API 层的 `attemptJSON` 视图类型 | −21 | `domain.Attempt` 直接带 JSON 标签，内部字段标 `json:"-"`。响应格式不变 |
| A6 | 日志的后台定时刷新（goroutine、ticker、缓冲） | −40 | 每个输出块同步写一批。实测无缓存的完整构建 459 行日志，序号连续，构建耗时 112 秒，与之前相当 |
| A7 | `Secrets` 端口和按需读取 | −17 | build secret 在启动时读入 map，缺失时直接启动失败。代价：修改 secret 值需要重启 |
| A8 | registry 配置的三份定义（YAML 结构、`registry.Config`、`main` 里的转换），以及 `Pusher` 接口 | −18 | 配置直接内联 `registry.Config`；密码和上传签名密钥固定从 `SAB_REGISTRY_PASSWORD`、`SAB_REGISTRY_HTTP_SECRET` 读取。`Pusher` 只有 Docker 一个实现 |
| A9 | 关闭时的宽限期 | −28 | 关闭时立即停止运行中的构建并记为 interrupted。强杀进程的情况仍由重启后的核对处理 |
| A10 | Node.js `index.json` 下载与版本排序 | −52 | 主版本线改由官方 `latest-v<major>.x/SHASUMS256.txt` 解析出具体版本；musl 版按同一版本号取校验和，取不到时明确拒绝 |
| A11 | 插件内容摘要和计划阶段的复核 | −62 | 插件已经锁定到 git 提交，拉取时会校验 `HEAD` 等于该提交，提交本身就决定了内容 |
| A12 | `hash-token` 子命令 | −13 | 文档改为 `printf %s "$TOKEN" \| sha256sum` |
| A19 | BuildKit 进度的 `io.Pipe` 加拷贝 goroutine | −14 | progressui 直接写入一个把文本转给尝试日志的 `io.Writer` |

（编号沿用实验顺序，A13–A18 是评估后保留或放弃的候选，见下节。）

## 评估后保留

| 抽象 / 功能 | 去掉后可省 | 保留理由 |
| --- | --- | --- |
| `Store` 端口（`app` → `adapter/postgres`） | 约 35 行接口 | 任务明确要求数据库 adapter 分层；去掉后应用层要直接依赖 pgx 类型 |
| `Executor`、`Publisher`、`Resolver` 端口 | 各 10–20 行 | 测试替身依赖它们：应用层测试不需要 Docker、registry 或网络就能覆盖取消、租约丢失、发布失败和核对 |
| `domain` 与 `app` 分包 | 0 行 | 合并省不了代码，只会模糊依赖方向 |
| 失败信息改用单个 JSON 列（A13） | 约 6 行 | 当时 SQLite 下保留为三列；迁移到 PostgreSQL 后已改为 `jsonb`，仍可按 `failure->>'stage'` 查询 |
| 提交时先按幂等键查找（A14） | 约 10 行 | 重放时不重新解析上游，也不受上游故障影响（API 文档承诺了这一点） |
| `noCache`（A15） | 约 15 行 | 唯一能真正重建（不复用层缓存，例如拿到新的 apt 包）的方式 |
| 上游地址可配置（A16） | 约 15 行 | npm、Node.js 下载源和 akhook 仓库在受限网络里需要换成镜像源 |
| 插件运行时变量改名的 sh 包装（A17） | 约 15 行 | Codex 只透传环境变量、不支持改名；AXP 插件的写法（`"XAI_API_KEY": "${AXP_AGENT_KEY}"`）依赖它 |
| `registry-gc` 子命令（A18） | 约 20 行 | 任务要求明确 registry 的清理方式；没有它，就需要另外安装 distribution 的二进制 |

## 消融后的真实验证

- 无缓存完整构建（Debian slim、probe 插件、apt 脚本、使用 secret 的脚本）成功，用时 112 秒：Node.js 24.21.0 经新的解析路径锁定；日志 459 行、序号连续；secret 只以 `[redacted]` 出现。
- 用该镜像运行 `scripts/smoke-image.sh`，真实 Codex 一轮通过：MCP、skill、两个生命周期 hook、`PreToolUse` 拦截、startup 脚本均正常。
- 取消运行中的构建 → `cancelled`；SIGTERM → 立即 `interrupted`；`kill -9` 后重启 → 租约过期后 `interrupted`（`recovered`）；没有残留进程。
- `go vet`、`go test -race ./...`、`-tags docker` 集成测试、staticcheck 和 agent-server 的 `nr test` 全部通过。

## 后续：SQLite 换成 PostgreSQL

按要求去掉 SQLite，只支持 PostgreSQL，迁移改用 goose。生产代码从 3866 行变为 3849 行：goose 替代了自写的迁移器，pgx 的 `CollectRows`/`COPY`/`RETURNING` 也让存储代码更短。
