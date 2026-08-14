# easyeda-agent × DeepSeek Harness (DSH) 集成

DSH（`@deepseek-ai/dsh`，Cordis 插件化框架）原生支持 skill 与 MCP client 两种
插件形态，easyeda-agent 恰好两种资产都已具备，所以接入是配置级工作而非开发级：

| DSH 形态 | 本项目资产 | 落地方式 | 开发量 |
|---|---|---|---|
| **Skill**（SKILL.md 自动发现） | `skills/easyeda-agent/SKILL.md` | 软链进 DSH skill 根 | 0 |
| **MCP client**（`dsh-mcp-client` 桥接） | `mcp/`（stdio MCP server，11 工具） | `cordis.patch.yml` 加一行插件实例 | 几行 YAML |
| **Bundle 插件包**（`dsh.bundle.patch` 声明） | 仓库根 `package.json` + `cordis.patch.yml` | `dsh plugin add github:zhoushoujianwork/easyeda-agent#<tag>` 一行装 | 已完成 |
| **原生 Cordis 插件**（`ctx.tools` / client-plugin UI） | 暂无 | 新建 npm 包，注册结构化工具 / daemon 状态面板 | 中等，跟 rc 版本 |

## 团队/他人接入（推荐：Bundle 一键安装）

**任何人 `dsh plugin add` 一行装完**（skill + MCP 全部就位，无需 clone、无需改配置）：

```sh
dsh plugin --profile web add "github:zhoushoujianwork/easyeda-agent#<tag>"
# 重启 dsh web 生效；升级/卸载走 Settings → Plugins
```

仓库根声明了 `dsh.bundle.patch`（见根 `cordis.patch.yml`），安装后自动成为
profile 的活跃 bundle 层，注入两个行：

1. `easyeda-mcp` —— `dsh-mcp-client` 实例（in-box 插件，走 fallback 解析），
   MCP server 路径由 `!!js` 表达式基于 loader 的 `ctx.baseUrl`（= profile 目录）
   定位包内 `mcp/src/server.mjs`；`EASYEDA_BIN` 默认取 PATH，可用环境变量覆盖。
2. `easyeda-skill-fs` —— 独立的 `dsh-skill-filesystem` 实例
   （`providerName: easyeda`、`includeDefaultRoots: false`），只扫包内
   `skills/easyeda-agent`，注册进 skill 注册表 global layer（web 下 host 的
   skill-filesystem 被官方 bundle 禁用、preset 自有发现，故用隔离实例，不冲突）。

**已验证（2026-08-14）**：`dsh plugin add file:...` 到 headless profile → 自动
提升为 bundle 层 → headless 会话实测模型可见全部 11 个 `mcp__easyeda__*` 工具
+ `easyeda-agent` skill。`.npmignore` 已排除 bin/dist 等构建产物，`github:`
  安装只会打包 package.json / cordis.patch.yml / mcp/ / skills/ 等。

**版本同步**：根 `package.json` 的 `version` 应与 release tag 对齐（`make release`
目前不自动改它，发版前手动同步一次即可）。

## 团队/他人接入（兜底：一键脚本）

如果不想走 bundle（比如还没发版、想本地开发态接入），clone 后跑
`scripts/dsh-install.sh`（幂等；自动探测仓库路径、注入/更新 `cordis.patch.yml`、
防重复）：

```bash
git clone https://github.com/zhoushoujianwork/easyeda-agent.git
bash easyeda-agent/scripts/dsh-install.sh                  # profile 默认 web
# bash easyeda-agent/scripts/dsh-install.sh --profile headless
# DSH_HOME=/custom/.dsh bash easyeda-agent/scripts/dsh-install.sh
```

前提：已装 `easyeda` CLI（`curl -fsSL https://raw.githubusercontent.com/zhoushoujianwork/easyeda-agent/main/install.sh | sh`）、
`web` profile 至少启动过一次（先 `dsh web` 初始化）。脚本会：①软链 skill
（watcher 即时发现）；②注入/更新 `cordis.patch.yml` 里的 `easyeda-mcp` 条目
（含 MCP server 与 `EASYEDA_BIN` 的绝对路径）；③打印验证与重启提示。之后
kill 当前 dsh 进程、在原目录重新 `dsh web`，MCP 工具即出现。

## 已落地（本机，2026-08）

### 路径 A — Skill（已生效，无需重启）

```bash
mkdir -p ~/.dsh/skills
ln -sfn <repo>/skills/easyeda-agent ~/.dsh/skills/easyeda-agent
```

DSH 的 skill-filesystem 提供者扫描根：`<projectRoot>/.dsh/skills`、
`<projectRoot>/.agents/skills`、`customSkillDirs`、`~/.dsh/skills`、
`~/.agents/skills`（只认一级目录 `SKILL.md` 或扁平 `.md`，名字必须 kebab-case）。
软链后 watcher 即时发现（无需重启），模型按 `skill` 工具加载，照旧通过 bash 调
`easyeda` CLI 干活——这正是本项目 Skill 的设计工作方式。

### 路径 B — MCP client（已配置，重启 dsh web 后生效）

在 `~/.dsh/profiles/<profile>/cordis.patch.yml`（本机为 `web`）追加：

```yaml
- insert:
    - id: easyeda-mcp
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        serverName: easyeda
        transport: stdio
        command: node
        args:
          - <repo>/mcp/src/server.mjs
        env:
          EASYEDA_BIN: /usr/local/bin/easyeda
```

生效后模型看到 `mcp__easyeda__easyeda_health`、`mcp__easyeda__easyeda_schematic`
等 11 个结构化工具（参数走 MCP 字段而非 shell 文本）。

**版本坑（已踩）**：DSH 的插件解析顺序是 profile 自己的 `node_modules` 优先，
其次才是 `$DSH_HOME/profiles/node_modules` 的扁平 fallback（由 `dsh` 启动时的
`healProfilesModuleFallback` 按安装清单重建的符号链接，指向 dsh 安装目录里的
in-box 插件）。`pnpm add @deepseek-ai/dsh-mcp-client` 会装到 registry 的
**latest（旧版 `0.0.1-rc.1`）**，遮蔽 fallback 里的 `0.1.0-rc.6`，导致插件
API 与当前 dsh 不匹配。**in-box 插件不需要装进 profile**——只写 `cordis.patch.yml`
即可从 fallback 解析；误装后用 `dsh plugin --profile web remove @deepseek-ai/dsh-mcp-client`
删掉。

### 验证

```bash
# 配置合并树（不启动服务）：应出现 easyeda-mcp 条目
dsh --profile web --dump-config | grep -A 16 easyeda-mcp

# MCP server 自身握手：应列出 11 个 easyeda_* 工具
printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}\n' | \
  EASYEDA_BIN=$(which easyeda) node mcp/src/server.mjs
```

host 插件（MCP client）改动需要**重启 dsh web** 才加载（HMR 只覆盖 client-plugin）；
skill 软链则由 watcher 即时发现。重启方式：找到 dsh 进程 kill 后，在原工作目录
重新 `dsh web`。

## 演进方向 — 路径 C（原生插件）

写一个 profile 本地插件 / npm 包：
- `ctx.tools.register()`：把 20 个 typed actions 直接映射成结构化 DSH 工具，
  摆脱 CLI 文本解析（比 MCP 更"第一方"，可拿 `ctx.skills`/approval/scope 集成）；
- `ctx.skills.register()`：runtime 注册 skill（rank 250，可被项目级覆盖）；
- client-plugin：daemon 状态 / 连接器健康 / `layout-lint` 结果 / audit 基线
  做成 Web UI 面板。

代价是跟随 `0.1.0-rc` 的 API 变动，建议 A+B 跑顺后再按需演进。
