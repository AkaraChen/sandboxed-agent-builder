# 构建参数、产物与插件计划

本轮产品约束以本文为准，采用干净架构。已实现；实际请求格式见 [API](api.md)，镜像契约见[运行契约](runtime-contract.md)。下方 YAML 为最初的示意。

## 基础镜像与 runtime

提供官方 Alpine、Debian slim、完整 Debian 三类基础镜像，默认 Debian slim，不提供 Ubuntu。基础镜像限定到明确来源和版本，记录解析后的 digest。Alpine 使用 musl，需验证 Codex、Node.js、oar 和插件原生依赖在目标平台的兼容性后再开放，不以镜像大小代替可运行性验证。

Agent runtime 只支持 Codex，仍通过 oar 驱动。产品参数不提供其他 runtime 选项。Codex 的版本策略需要明确并记录实际安装版本。

oar 默认选择提交新构建时的最新稳定版本，并在入队前解析、持久化为具体版本；重试沿用该版本。升级 oar 通过新构建完成，不在同一构建的重试中重新解析 latest。验证最新版 oar 对所选 Codex、Node.js 和平台的要求。

## 参数示意

```yaml
base:
  family: debian-slim # debian-slim | debian | alpine
  version: "<明确发行版本>"
platform: linux/amd64
codex:
  version: "<版本或明确版本策略>"
oar:
  version: latest-stable
plugins:
  - source: "<插件来源>"
    revision: "<不可变版本或提交>"
scripts:
  build:
    - name: 开发工具
      run: |
        apt-get update
        apt-get install -y --no-install-recommends git curl
        rm -rf /var/lib/apt/lists/*
  startup:
    - name: 准备工作区
      run: |
        # 容器每次启动时执行
        :
output:
  type: local # local | registry
```

示例脚本适用于 Debian。脚本使用所选基础镜像的工具和包管理器；builder 不转换 apt 与 apk。插件配置区分静态安装配置和运行时配置/凭据引用。

提交保存原始请求和解析后的规格：基础镜像 digest、Codex/oar/Node.js/akhook 的实际版本、插件内容及平台。幂等重放返回原构建，主动新构建才能重新解析版本策略。

模型、系统指令、任务、会话恢复参数、工作区挂载、实例身份和运行凭据由调用方启动容器时提供。只有影响镜像内容的配置才属于构建参数。

## 脚本入口

- `scripts.build` 是有序数组，每项生成一个独立 RUN 步骤，以便缓存、记录阶段和定位失败。
- `scripts.startup` 是有序数组，内容打包到镜像，由运行入口在 Codex 启动前执行；builder 不在构建时执行这些脚本。
- 两类脚本均明确使用镜像中的 shell，初始契约以 `/bin/sh` 为基线，不默认假设有 Bash。
- startup 每次容器启动执行；调用方约定工作目录、挂载及环境变量。任一脚本失败则不启动 Codex，日志由运行容器的调用方管理。
- 启动脚本不能覆盖标准通信入口；标准入口和调用方通信协议另行设计并版本化。

## Agent Plugin 兼容

### skill

安装插件技能内容到 Codex 可发现的位置，保留插件命名空间，明确加载和冲突规则。适配安装位置，不重复将技能正文复制进 prompt。

### MCP

将插件声明转换为 Codex 原生 MCP 配置；区分 stdio/HTTP、静态参数和运行时凭据。验证 MCP 服务依赖、启动方式及配置可被 Codex 加载。

### hook 与 akhook 上游

hook 的兼容性由 akhook 内部 runtime adapter 处理，向 https://github.com/aghub-app/akhook 贡献；builder 只负责安装 akhook、编译插件 hook 配置和连接原生事件入口。

akhook 的设计目标是使提供必要 hook 入口的兼容 runtime 能执行 Agent Plugin hook。统一内部事件后处理原生事件名、输入格式、结果、阻止语义、超时及错误。builder 产品仍只支持 Codex，akhook 的通用能力可以独立演进。

runtime 没有对应拦截入口时，adapter 无法凭空实现执行前阻止；必须声明能力缺口并拒绝不兼容 hook。MCP 和 skill 同样需验证实际配置加载，不能仅凭标准名称就宣称全部兼容。

先审计现有 akhook 与 Agent Plugin 事件契约，提交上游设计和验证，再实现适配；避免在 builder 维护一套重复兼容逻辑。验证 hook 经 oar 驱动的 Codex 执行时确实生效，而非只验证配置文件生成。

## 交付的镜像与元数据

镜像包含 Codex、Node.js、oar、akhook、插件、脚本及标准运行入口。调用方负责运行和连接它；builder 不创建 Agent 会话。

返回构建 ID、attempt ID、原始及解析规格引用、规格摘要、平台和产物引用。记录运行契约版本及必要环境变量/挂载/凭据名称，实际通信协议待评审。镜像构建成功不表示运行凭据有效或 Agent 已就绪。

本地输出返回本地 image ID 和 daemon 引用，调用方在相同 daemon 使用镜像。image ID 不等于 registry manifest digest。

## 可选内置 registry

默认仍交付本地 Docker 镜像。规划可选内置 Docker/OCI Distribution registry，使调用方可以从其他机器拉取产物。

优先评估嵌入 `github.com/distribution/distribution/v3` 的 handler 与持久化存储组件，独立封装 registry 模块；不自行实现上传、blob、manifest 等协议。Go 实现与 builder 的实现语言/嵌入方式在技术验证后确定。

`go-containerregistry/pkg/registry` 的源码说明目前主要面向测试，不作为正式产物存储的默认选型。

启用 registry 需要明确持久化目录、对 Docker daemon 和调用方可达的地址、TLS/认证、repository 命名和保留策略。localhost 必须区分 builder、daemon 与调用方各自网络，不能假设 Docker socket 所在宿主能访问 builder 容器的 localhost。

构建完需实际推送到 registry，它不会自动暴露 daemon 中已有镜像。registry 输出以推送完成并确认 manifest digest 为交付成功条件，返回 `registry/repository@sha256:…`；推送失败记录发布阶段失败，不能误报已可跨机器拉取。

registry blob/manifest 存储与构建数据库分别持久化。镜像清理和垃圾回收要有明确维护流程，不删除调用方仍引用的产物。内置 registry 的初始范围为单节点镜像存储与分发。

## 参考

- https://github.com/botiverse/oar
- https://github.com/aghub-app/akhook
- https://github.com/distribution/distribution
- https://github.com/google/go-containerregistry/tree/main/pkg/registry
