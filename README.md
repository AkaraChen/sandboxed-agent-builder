# sandboxed-agent-builder

为 AXP 等调用方构建可运行 Agent 的 Docker 镜像，持久化管理异步构建的状态、日志、失败与重试。

调用方提交完整构建参数，获得 `buildId` 后轮询进度；成功后取得镜像 ID 或 registry digest，自行启动容器。

## 当前阶段

架构设计阶段，尚未实现可运行服务。设计依据为 AXP 任务 #43，采用干净架构。

## 职责

- 校验 runtime 与插件组合，生成构建计划。
- 通过 Docker socket 执行构建，利用 Docker/BuildKit 的 layer 缓存。
- 在数据库中保存构建规格、执行尝试、日志与镜像结果。
- 提供构建提交、查询、取消及重试接口。

AXP 保存 Agent 环境配置和构建参数，关联 `buildId` 与镜像结果，负责容器运行、重启、工作区和会话。builder 的交付物是镜像。

服务由 API、构建 worker、数据库和 Docker 执行适配组成。oar 安装在产物镜像中，由调用方运行；builder 不运行 Agent。

## 产品选择

- 基础镜像只选官方 Alpine、Debian slim 和完整 Debian；默认 Debian slim，Alpine 经过兼容性验证后开放。不使用 Ubuntu。
- Agent runtime 只支持 Codex，通过构建时最新稳定版 oar 驱动，记录实际解析版本。
- 兼容 Agent Plugin 的 hook、MCP 和 skill；hook 的 runtime 适配贡献给 akhook 上游。
- 支持逐项生成 RUN 步骤的 build 脚本，以及打包到镜像、启动时执行的 startup 脚本。
- 默认交付本地 Docker 镜像；规划可选的内置 Distribution registry。

## 文档

- [架构与构建生命周期](docs/architecture.md)
- [设计交付与实现顺序](docs/roadmap.md)
- [构建参数、产物与插件计划](docs/build-contract.md)

## 相关项目

- [AXP](https://github.com/AkaraChen/axp)
- [oar](https://github.com/botiverse/oar)

实现语言、数据库选型及部署拓扑尚待设计评审确定。
