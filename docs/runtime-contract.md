# 镜像运行契约（contract 1）

builder 交付的镜像遵循本契约；镜像 label `io.sandboxed-agent-builder.contract=1` 与 `/opt/agent/manifest.json` 的 `contract` 字段标明版本。调用方据此启动容器，builder 不运行容器。

## 入口

`ENTRYPOINT ["/usr/local/bin/agent-entrypoint"]`，默认 `CMD ["serve"]`，`EXPOSE 8080`。入口：

1. 安装调用方提供的 Codex 凭据：`CODEX_AUTH_JSON_FILE` 指向的 auth.json 复制到 `$CODEX_HOME/auth.json`；否则若设置 `OPENAI_API_KEY` 且尚无 auth.json，执行 `codex login --with-api-key`。镜像本身不含任何凭据。
2. 按文件名顺序以 `/bin/sh -e` 执行 `/opt/agent/startup/*.sh`（规格中的 `scripts.startup`），每次容器启动都执行。任一失败则退出码 70，不启动 Codex。
3. 参数为 `serve` 时启动 [Agent 控制 API](agent-api.md)（Hono，默认端口 8080，需要 `AGENT_API_TOKEN`）；其他参数原样交给 `exec oar "$@"`，即容器参数就是 oar 的命令行。

```sh
docker run --rm \
  -v /secure/codex-auth.json:/run/codex-auth.json:ro -e CODEX_AUTH_JSON_FILE=/run/codex-auth.json \
  -e MCP_TOKEN_GITHUB_GITHUB=... \     # resolvedSpec.runtimeEnv 中的变量
  -v agent-workspace:/workspace \
  IMAGE run codex "Fix the failing test" --record /workspace/.oar/run.jsonl
```

调用方通常用默认的控制 API 管理会话（打开、输入、中止、事件流、恢复），见 [agent-api.md](agent-api.md)。直接使用 oar CLI 的方式（`oar run`、`--resume`、`oar mcp` 等）见 oar 文档。

## 布局

| 路径 | 内容 |
| --- | --- |
| `/usr/local/bin/{node,npm,codex,oar,akhook}` | 锁定版本的 Node.js、Codex、oar（npm 全局安装）、akhook（从锁定提交静态编译） |
| `/etc/codex/config.toml` | 系统级 Codex 配置：`sandbox_mode = "danger-full-access"`（容器即隔离边界）和插件 MCP 服务器 |
| `/etc/codex/requirements.toml` | managed hooks：`PreToolUse`、`UserPromptSubmit`、`Stop` 均交给 `akhook codex hook …`，`allow_managed_hooks_only = true` |
| `/etc/codex/skills/<plugin>--<dir>/` | 插件 skill（Codex admin scope） |
| `/etc/akhook/plugins.yml` | 合并后的插件 akhook 规则（`AKHOOK_ADDITIONAL_CONFIG_PATH`） |
| `/opt/agent/plugins/<name>/` | 插件目录（`${PLUGIN_ROOT}`） |
| `/opt/agent/startup/` | 启动脚本 |
| `/opt/agent/server/` | Agent 控制 API（Hono），使用 oar CLI 安装的同版本 oar 库 |
| `/opt/agent/manifest.json` | 构建来源：基础镜像 digest、Node 版本、`agent`（`{"type":"codex","codex":{"package","version"}}`）、oar 版本、akhook 提交、插件、启动脚本、runtimeEnv |
| `/workspace` | 工作目录，属主 `agent`（uid 1000），供调用方挂载 |
| `/home/agent/.codex` | `CODEX_HOME`，会话与 auth.json |

容器以非 root 用户 `agent` 运行。build 脚本以 root 执行（可安装系统包）。

## 环境变量

| 变量 | 提供方 | 说明 |
| --- | --- | --- |
| `CODEX_AUTH_JSON_FILE` 或 `OPENAI_API_KEY` | 调用方 | Codex 凭据 |
| `AGENT_API_TOKEN`（或 `AGENT_API_INSECURE=1`） | 调用方 | 控制 API 的 bearer token；其余 `AGENT_API_*` 见 agent-api.md |
| `resolvedSpec.runtimeEnv` 中的变量 | 调用方 | 插件 HTTP MCP 服务器的 bearer token（`MCP_TOKEN_…`） |
| `CODEX_HOME`、`AKHOOK_ADDITIONAL_CONFIG_PATH` | 镜像 | 不应覆盖 |

## 插件映射

插件按公开标准 [Agent Plugins](https://agent-plugins.org) 读取，支持 1.0.0 与 1.1.0 两个版本（两者规范性内容相同）。处理规则遵循标准：
- `plugin.json` 违反规范时，拒绝整个插件。
- 未知字段、无效 skill、无效或不支持的 MCP 条目，跳过并记入 `resolvedSpec.plugins[].warnings`，同时写入构建日志。

| 插件内容 | 镜像中的原生形式 |
| --- | --- |
| 插件目录 | `/opt/agent/plugins/<name>`（`PLUGIN_ROOT`） |
| 插件数据 | `/var/lib/agent-plugins/<name>`（`PLUGIN_DATA`，属主 `agent`；挂载卷可跨镜像更新保留） |
| `skills/<name>/SKILL.md`（name 须等于目录名并有 description） | `/etc/codex/skills/<plugin>--<name>`；不同插件出现同名 skill 时，后者跳过并给出警告 |
| `mcp.json` 的 `stdio` 服务器 | `[mcp_servers.<plugin>_<server>]`：`command`（`./x` 解析为插件内路径）、`args`、`env`、`cwd`（默认插件目录），其中 `${PLUGIN_ROOT}`/`${PLUGIN_DATA}` 在构建时展开，其他 `${…}` 保留原文；`env` 另设 `PLUGIN_ROOT`、`PLUGIN_DATA` |
| `mcp.json` 的 `streamable-http` 服务器 | `url`、原样的 `http_headers`，以及 `bearer_token_env_var = "MCP_TOKEN_<PLUGIN>_<SERVER>"`（见下） |
| `mcp.json` 的 `sse` 服务器 | 不支持，跳过并给出警告（标准中为可选） |
| 扩展目录 `app.aghub.akhook/akhook.yml` | 规则 ID 加插件前缀，展开 `${PLUGIN_ROOT}`/`${PLUGIN_DATA}`，合并到 `/etc/akhook/plugins.yml` |

**认证**：标准把认证交给客户端，`headers` 只能是字面值，不能放密钥。本镜像的做法是：每个 `streamable-http` 服务器从环境变量 `MCP_TOKEN_<插件名>_<服务器名>` 读取 bearer token（全部大写，非字母数字字符换成 `_`），变量名列在 `resolvedSpec.runtimeEnv`。调用方可以在启动容器时注入，也可以在控制 API 打开会话时用 `env` 传入。没有提供 token 时，该服务器连接失败，不影响其他组件。

**扩展目录**：hook 不在标准范围内，因此放在本镜像实现的扩展命名空间 `app.aghub.akhook` 下，由 akhook 执行，语义见下节。其他客户端的扩展目录和 `extensions` 字段一律忽略、不做校验。

MCP 工具在配置中设为 `default_tools_approval_mode = "approve"`：oar 以 `approvalPolicy: never` 驱动 Codex，没有人回答审批请求。

不支持、提交时拒绝的写法：`command`/`args`/`cwd`/`url` 中的运行时变量（只展开 `${PLUGIN_ROOT}`；Codex 不在这些字段做环境变量替换）、部分拼接的运行时变量、Claude 风格 `hooks/hooks.json`、akhook 以外的 hook 事件。

## hook 语义（akhook，经 Codex managed hooks）

| akhook `on` | Codex 事件 | 能否阻止 | 行为 |
| --- | --- | --- | --- |
| `prompt_submit` | `UserPromptSubmit` | 否 | 运行 `run` 命令，stdout 的 `{"context": …}` 作为这一轮附加上下文；命令失败只记录到 stderr，轮次照常 |
| `stop` | `Stop` | 否 | 运行 `run` 命令（输入含 `last_assistant_message`），输出忽略 |
| `shell_exec`、`file_change`、`tool_call` | `PreToolUse` | 是 | 规则命中则在工具执行前拒绝（`deny`），提示返回给模型；`ask` 在 Codex 上走 akhook 一次性授权；`decide` 交给脚本 |

边界（来自 akhook 与 Codex 文档，并经实测确认前两类）：只有经 Codex 本地 hook 路径的工具（shell、`apply_patch`、MCP 等）会触发 `PreToolUse`，托管工具（如 web search）不经过；hook 不能中断模型生成；`PostToolUse` 不能撤销副作用（akhook 当前未使用）。
