# 设计交付与实现顺序

目标是具有数据库的异步镜像构建服务，交付物为镜像；调用方负责 Agent 运行。

第 1-8 步已实现并验证（[verification.md](verification.md)），第 9 步的 AXP 对接在 AXP 侧进行。未完成项：PostgreSQL adapter、插件 MCP URL 中的运行时变量、arm64 与多节点验证。

## 实现顺序

1. 确定构建规格、API、状态转换、数据库模型和 Docker adapter 契约。
2. 验证 Docker socket 构建、取消、进度、镜像 ID 与重启后核对机制。
3. 实现规格持久化、提交幂等、构建查询及 worker 领取。
4. 实现官方 Debian slim/完整 Debian 基础环境和 Codex/oar 组装，锁定最新稳定版 oar（Alpine 已移除）。
5. 审计并贡献 akhook 上游 runtime hook 适配，验证通过 oar 驱动的 Codex 能实际执行插件 hook；完成 MCP/skill 原生配置安装。
6. 实现插件 layer 缓存、逐项 RUN 的 build 脚本、打包的 startup 脚本和临时 build secret。
7. 实现日志增量读取、失败原因、取消、重试历史及中断恢复。
8. 实现本地镜像交付；验证并增加可选内置 Distribution registry，明确发布状态、持久化、认证和清理。
9. 对接 AXP 提交和轮询，明确运行契约、日志/产物保留及 OTel 诊断。

## builder 验收

- 提交后立即获得 buildId，可持续轮询直到最终状态。
- 完整规格、失败原因和历史尝试在服务重启后仍可查询。
- 日志按序号增量分页，断线后可以补查。
- 重复提交与重试请求幂等，同一参数可以显式再次构建。
- 重试产生新的 attempt，原错误和日志保留。
- 排队及执行期间取消的结果明确，不将取消请求误报为完成。
- 服务、worker 或 Docker 连接中断后核对结果；不能确认的尝试标记 interrupted。
- 本地 image ID 对应指定 daemon 中实际存在的镜像。
- registry 模式以推送完成为成功条件，返回可跨机器拉取的 digest 引用。
- 不兼容 runtime/插件在构建前被拒绝。
- 密钥不进入镜像层、规格快照与日志。
- 使用 buildId 和 attemptId 关联数据库、日志与 OTel。
- 新构建解析最新稳定版 oar，重试不改变已锁定的依赖版本。
- 每个 build 脚本对应独立 RUN 步骤；startup 被正确打包且不在构建期间执行。
- Codex 实际加载 skill/MCP，akhook 的 hook 适配通过原生事件验证；缺少拦截能力的组合明确拒绝。
- registry 启用后通过实际 push/pull 验证产物，重启仍保留镜像；地址可从 daemon 和调用方访问。

## 调用方集成验收

由 AXP 等调用方完成：保存参数和构建关联，取得镜像后运行 Agent，验证插件、oar 消息控制和会话能力，管理重启、环境切换及工作区持久化。这些流程不进入 builder 的运行职责。
