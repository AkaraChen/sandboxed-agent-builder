# 镜像运行契约（contract 1）

builder 交付的镜像遵循本契约；镜像 label `io.sandboxed-agent-builder.contract=1` 与 `/opt/agent/manifest.json` 的 `contract` 字段标明版本。调用方据此启动容器，builder 不运行容器。

## 入口

`ENTRYPOINT ["/usr/local/bin/agent-entrypoint"]`，默认 `CMD ["installation", "codex"]`。入口：

1. 安装调用方提供的 Codex 凭据：`CODEX_AUTH_JSON_FILE` 指向的 auth.json 复制到 `$CODEX_HOME/auth.json`；否则若设置 `OPENAI_API_KEY` 且尚无 auth.json，执行 `codex login --with-api-key`。镜像本身不含任何凭据。
2. 按文件名顺序以 `/bin/sh -e` 执行 `/opt/agent/startup/*.sh`（规格中的 `scripts.startup`），每次容器启动都执行。任一失败则退出码 70，不启动 Codex。
3. `exec oar "$@"`：容器参数就是 oar 的命令行。

```sh
docker run --rm \
  -v /secure/codex-auth.json:/run/codex-auth.json:ro -e CODEX_AUTH_JSON_FILE=/run/codex-auth.json \
  -e PROBE_KEY=... \                 # resolvedSpec.runtimeEnv 中的变量
  -v agent-workspace:/workspace \
  IMAGE run codex "Fix the failing test" --record /workspace/.oar/run.jsonl
```

oar 的会话控制（`oar run`、`--resume`、`oar mcp` 等）见 oar 文档；长期通信协议由调用方与 oar 约定，builder 不定义。

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
| `/opt/agent/manifest.json` | 构建来源：基础镜像 digest、Node 版本、`agent`（`{"type":"codex","codex":{"package","version"}}`）、oar 版本、akhook 提交、插件、启动脚本、runtimeEnv |
| `/workspace` | 工作目录，属主 `agent`（uid 1000），供调用方挂载 |
| `/home/agent/.codex` | `CODEX_HOME`，会话与 auth.json |

容器以非 root 用户 `agent` 运行。build 脚本以 root 执行（可安装系统包）。

## 环境变量

| 变量 | 提供方 | 说明 |
| --- | --- | --- |
| `CODEX_AUTH_JSON_FILE` 或 `OPENAI_API_KEY` | 调用方 | Codex 凭据 |
| `resolvedSpec.runtimeEnv` 中的变量 | 调用方 | 插件 MCP 服务器的运行时凭据/参数 |
| `CODEX_HOME`、`AKHOOK_ADDITIONAL_CONFIG_PATH` | 镜像 | 不应覆盖 |

## 插件映射

| Agent Plugin | 镜像中的原生形式 |
| --- | --- |
| `mcp.json` stdio 服务器 | `[mcp_servers.<plugin>_<server>]`：`command`/`args`/`cwd`/静态 `env`；`${VAR}` 整值透传为 `env_vars`，名称不同时经 `sh -c 'export NAME="$VAR"; exec …'` 改名 |
| `mcp.json` HTTP 服务器 | `url`，`Authorization: Bearer ${VAR}` → `bearer_token_env_var`，整值 `${VAR}` 头 → `env_http_headers`，其余 → `http_headers` |
| `skills/<dir>/SKILL.md` | `/etc/codex/skills/<plugin>--<dir>`；同名 skill 跨插件冲突在提交时拒绝 |
| `akhook.yml` | 规则 ID 加插件前缀，`${PLUGIN_ROOT}` 展开，合并到 `/etc/akhook/plugins.yml` |
| `setup` | 构建时在插件层执行一次（`PLUGIN_ROOT`、`PLUGIN_CACHE`、`PLUGIN_BIN=/usr/local/bin`，兼容 `AXP_PLUGIN_*`） |

MCP 工具在配置中设为 `default_tools_approval_mode = "approve"`：oar 以 `approvalPolicy: never` 驱动 Codex，没有人回答审批请求。

不支持、提交时拒绝的写法：`command`/`args`/`cwd`/`url` 中的运行时变量（只展开 `${PLUGIN_ROOT}`；Codex 不在这些字段做环境变量替换）、部分拼接的运行时变量、Claude 风格 `hooks/hooks.json`、akhook 以外的 hook 事件。

## hook 语义（akhook，经 Codex managed hooks）

| akhook `on` | Codex 事件 | 能否阻止 | 行为 |
| --- | --- | --- | --- |
| `prompt_submit` | `UserPromptSubmit` | 否 | 运行 `run` 命令，stdout 的 `{"context": …}` 作为这一轮附加上下文；命令失败只记录到 stderr，轮次照常 |
| `stop` | `Stop` | 否 | 运行 `run` 命令（输入含 `last_assistant_message`），输出忽略 |
| `shell_exec`、`file_change`、`tool_call` | `PreToolUse` | 是 | 规则命中则在工具执行前拒绝（`deny`），提示返回给模型；`ask` 在 Codex 上走 akhook 一次性授权；`decide` 交给脚本 |

边界（来自 akhook 与 Codex 文档，并经实测确认前两类）：只有经 Codex 本地 hook 路径的工具（shell、`apply_patch`、MCP 等）会触发 `PreToolUse`，托管工具（如 web search）不经过；hook 不能中断模型生成；`PostToolUse` 不能撤销副作用（akhook 当前未使用）。
