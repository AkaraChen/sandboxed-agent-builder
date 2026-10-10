# sandboxed-agent-builder

为 AXP 等调用方构建可运行 Agent 的 Docker 镜像，持久化管理异步构建的状态、日志、失败与重试。

调用方提交完整构建参数，获得 `buildId` 后轮询进度；成功后取得镜像 ID 或 registry digest，自行启动容器。

## 当前阶段

Go + chi 实现的可运行服务，采用干净架构；PostgreSQL 持久化（goose 迁移），经 Docker socket 使用 daemon 内置的 BuildKit 构建，可选内置 Distribution registry。验证记录见 [docs/verification.md](docs/verification.md)。

## 运行

需要 Go 1.26、git、Docker（BuildKit 随 Docker daemon 提供）和 PostgreSQL 17+。

```sh
go build -o builder ./cmd/builder
printf %s "$TOKEN" | sha256sum   # 填入配置 auth.callers[].tokenSha256
./builder serve -config deploy/builder.example.yaml
```

容器部署见 [deploy/compose.yaml](deploy/compose.yaml)（挂载 Docker socket 等同于宿主机 root 权限，只在可信环境运行）。

```sh
go vet ./... && go test -race ./...                       # 数据库测试用 SAB_TEST_DATABASE_URL，未设置时自动启动 sab-test-postgres 容器
go test -race -tags docker ./internal/adapter/docker   # 需要 Docker
cd runtime/agent-server && ni && nr test                # 镜像内控制 API（Node.js 24）
```

## 职责

- 校验 runtime 与插件组合，生成构建计划。
- 通过 Docker socket 执行构建，利用 Docker/BuildKit 的 layer 缓存。
- 在数据库中保存构建规格、执行尝试、日志与镜像结果。
- 提供构建提交、查询、取消及重试接口。

AXP 保存 Agent 环境配置和构建参数，关联 `buildId` 与镜像结果，负责容器运行、重启、工作区和会话。builder 的交付物是镜像。

服务由 API、构建 worker、数据库和 Docker 执行适配组成。产物镜像里装有 oar，还带一个 Hono 写的控制 API，调用方通过它在容器里管理 agent 会话；builder 本身不运行 Agent。

## 产品选择

- 基础镜像只选官方 Debian slim（默认）和完整 Debian，不提供 Alpine 和 Ubuntu。
- Agent runtime 只支持 Codex，通过构建时最新稳定版 oar 驱动，记录实际解析版本。
- 兼容 Agent Plugin 的 hook、MCP 和 skill；hook 的 runtime 适配贡献给 akhook 上游。
- 支持逐项生成 RUN 步骤的 build 脚本，以及打包到镜像、启动时执行的 startup 脚本。
- 默认交付本地 Docker 镜像；可选内置 Distribution registry，推送确认后交付 `repository@digest`。

## 文档

- [API](docs/api.md)
- [镜像运行契约](docs/runtime-contract.md)
- [镜像内的 Agent 控制 API](docs/agent-api.md)
- [验证记录](docs/verification.md)
- [抽象消融记录](docs/ablation.md)
- [架构与构建生命周期](docs/architecture.md)
- [设计交付与实现顺序](docs/roadmap.md)
- [构建参数、产物与插件计划](docs/build-contract.md)

## 相关项目

- [AXP](https://github.com/AkaraChen/axp)
- [oar](https://github.com/botiverse/oar)

