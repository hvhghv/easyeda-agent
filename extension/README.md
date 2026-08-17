# EDA Agent Connector

面向 EasyEDA（嘉立创EDA专业版）的 AI 原生自动化连接器。

`easyeda-agent` 把官方 EasyEDA 扩展 API 变成一套**有类型、可观测、Skill 友好**的系统。这个连接器插件本身保持很薄：它只负责连接本地 `easyeda-agent` daemon，并把 typed actions 分发到官方 `eda.*` API；真正的工作流、校验、确认、产物处理和多步编排，都在 Go CLI/daemon 与 Skill 层完成。

```text
Skill / CLI -> Go daemon -> EDA Agent Connector -> official eda.* API
```

- GitHub 仓库：https://github.com/zhoushoujianwork/easyeda-agent
- 安装脚本：https://raw.githubusercontent.com/zhoushoujianwork/easyeda-agent/main/install.sh

## 效果演示

下面两段录屏来自真实 EasyEDA 画布：AI 从空白页开始生成原理图，再切到 PCB 完成布局、板框、铺铜和丝印。它不是生成一张图片，而是在编辑器里一步步执行 typed actions。

**原理图从空白页生成：**

![AI 在 EasyEDA 中从空白页生成原理图](https://image.lceda.cn/extensions/images/ef5b8c6950034244b68d08ccd4080de4.png)

**PCB 布局与铺铜：**

![AI 在 EasyEDA 中完成 PCB 布局、板框和铺铜](https://image.lceda.cn/extensions/images/dc39ac080e6c46a5a267e6db142cdc86.gif)

下面这块板由 agent 驱动完整 PCB 流程产出：**自动布局 -> 板框贴合 -> 规则感知布线 -> 4 层电源平面 -> 丝印碰撞避让**，并在真实 EasyEDA 画布上验证。

![ESP32-S3 成品板：4 层电源平面 + 圆角板框 + 位号对齐](https://image.lceda.cn/extensions/images/4bc708fd5af1463c88cd0bf388bcdcab.png)

## 它是什么

这是一个真实可打包、可导入的 **EasyEDA Pro 扩展**：一个**常驻连接器**，桥接本地 `easyeda-agent` Go daemon 与官方 `eda.*` API，并且是整个系统中**唯一直接调用 `eda.*` 的组件**。

它主要负责：

- 本地 WebSocket 传输：端口扫描、握手、注册、上下文同步、心跳、自愈重连。
- typed action 分发：把 daemon 下发的结构化动作映射到官方 `eda.*` 调用。
- 结果序列化：把执行结果、警告、错误、上下文回传给 daemon。
- 产物传输：把截图、BOM、网表等二进制结果编码后回传。

## 已支持的典型能力

- 原理图：放真实器件、布线、网络标志、选择、截图、DRC、结构校验（bridge-check：短路/悬空判据）、BOM 导出、网表导出。CLI 侧以此为底座提供原理图全流程（S0–S6）交付与 `sch gate --strict` 五关机械门禁。
- PCB：新建板、导入、自动布局、板框贴合、铺铜、禁布区、叠层设置、截图、DSN 往返。
- 基础设施：自动重连、上下文感知、结构化错误、`debug.exec_js` 逃生口。

完整能力清单见 GitHub 仓库中的 `docs/FEATURES.md`。

## 0.26.1 新增：bridge-check 的 ORPHAN_TREE（悬空树）判据

`schematic.bridgeCheck` 新增 **ORPHAN_TREE** 判据：识别**不触及任何引脚的导线树**——
典型形态是挪动器件后残留在原地的网络标志 + 桩线，或纯裸死线。此前的
orphan-stub（要求树触到引脚）与 orphan-flag（要求标志不挨任何导线）两个判据对这种
形态**双双结构性盲区**，只能靠人工看图才能发现。现在 summary 返回 `orphanTrees`
计数，配套 CLI（同版及以上）将其渲染为 `orphan-tree` 告警，`sch gate --strict`
会阻塞放行；旧版 CLI 读新连接器只是忽略新字段，不破坏兼容。

## 安装

这个连接器只是薄桥接层，**必须配套本地 `easyeda` CLI/daemon 一起用**。

**1. 装本连接器**（两条通道任选）：

- 在[立创官方插件市场](https://jlc-ext.com/item/zhoushoujian/easyeda-agent-connector)点击「安装」—— 平台可原地自动更新，最省心；
- 或从 [GitHub Release](https://github.com/zhoushoujianwork/easyeda-agent/releases/latest) 侧载 `easyeda-agent-connector.eext` —— 与 CLI **严格同版**。

> **版本配套**：连接器与 CLI 遵循**同一版本号**（四件套——CLI/daemon、连接器、Skill、EasyEDA——同版约定）。市场上架版本可能**滞后于 CLI**（市场无发布 API，每版需人工重新提交）。需严格同版时，请以 GitHub Release 里与 CLI 对齐的 `.eext` 为准；市场版胜在平台自动更新，但可能落后。

> **更名说明（2026-08）**：应市场管理规范要求，本插件**显示名**改为
> **EDA Agent Connector**（不再含 "easyeda" 字样）。内部包名与 uuid 均保持不变，
> 同一条目重新上传 —— 已装用户的原地自动更新不受影响，无需任何操作。

**2. 装 CLI/daemon 并启动**：

```bash
curl -fsSL https://raw.githubusercontent.com/zhoushoujianwork/easyeda-agent/main/install.sh | sh
```

然后在 EasyEDA 中确认：

1. 已安装本连接器（市场安装或 `.eext` 导入）。
2. 已开启「允许外部交互 / Allow external interaction」。
3. 已启动本地 `easyeda-agent` daemon。

> 完整上手、版本对齐与升级注意事项见 [快速开始 & 使用注意事项](https://github.com/zhoushoujianwork/easyeda-agent/blob/main/docs/quick-start.md) —— 四件套(CLI / 连接器 `.eext` / Skill / EasyEDA)需同版本同时在位;升级时三方一起升,否则 `easyeda daemon health` 会把落后的连接器标成 stale。

## 重新打包

```bash
make eext
```

这会 bump patch 版本、执行 typecheck，并生成新的 `.eext` 包。

## 说明

这个 README 是面向插件导入页/插件包展示的精简版说明。更完整的架构、路线图、能力矩阵与实战案例，请直接查看 GitHub 仓库：

https://github.com/zhoushoujianwork/easyeda-agent
