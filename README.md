# sandboxed-agent-builder

为 AXP 提供自定义 Agent 环境的构建和运行管理服务。

用户或 Agent 选择 runtime、插件及环境脚本，服务构建版本化 Docker 镜像，通过 oar 驱动容器内的 Agent，并管理重建、重启与环境切换。

## 当前阶段

架构设计阶段，尚未实现可运行服务。设计依据为 AXP 任务 #43，采用干净架构。

## 职责

- 管理环境定义、不可变版本与插件兼容性。
- 执行镜像构建，提供构建状态、日志和产物。
- 管理运行实例及环境版本切换。
- 通过 runtime bridge 接入 oar，提供会话控制和事件。

AXP 保留 Agent 身份、任务、聊天界面和用户连接管理，调用本服务提供的环境与执行接口。

## 文档

- [架构与生命周期](docs/architecture.md)
- [设计交付与实现顺序](docs/roadmap.md)

## 相关项目

- [AXP](https://github.com/AkaraChen/axp)
- [oar](https://github.com/botiverse/oar)

语言、API 协议、存储、事件传输及部署拓扑尚待设计评审确定。
