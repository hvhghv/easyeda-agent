# EDA Agent Connector

**让 AI Agent 替你画板子。** 这是 easyeda-agent 系统在 EasyEDA(嘉立创EDA专业版)内的官方连接器:配合本地 `easyeda` CLI/daemon 与 Agent Skill,AI 可以在真实编辑器里从一份客户口吻的需求文档出发,完成选型、放置、连线、分区标注、机械门禁校验,直到交付原理图与 PCB。

```text
Skill / CLI -> Go daemon -> EDA Agent Connector -> 官方 eda.* API
```

一行看懂:Skill 描述专家工作流,Go CLI/daemon 提供有类型、可观测的动作与校验,本连接器把这些 typed actions 桥接到官方 `eda.*` API——它是整个系统中**唯一直接调用 `eda.*` 的组件**,每一步操作最终都落在嘉立创自己开放的插件能力上。

- GitHub 仓库:https://github.com/zhoushoujianwork/easyeda-agent
- 最新 Release:https://github.com/zhoushoujianwork/easyeda-agent/releases/latest

## 效果演示

AI 从空白页开始生成原理图——不是生成一张电路图图片,而是在编辑器里一步步执行 typed actions,放真实 LCSC 库件、连真实导线:

![AI 在 EasyEDA 中从空白页生成原理图](https://image.lceda.cn/extensions/images/ef5b8c6950034244b68d08ccd4080de4.png)

切到 PCB:自动布局、板框贴合、铺铜、丝印,全程在真实画布上执行并回读校验:

![AI 在 EasyEDA 中完成 PCB 布局、板框和铺铜](https://image.lceda.cn/extensions/images/dc39ac080e6c46a5a267e6db142cdc86.gif)

由 agent 驱动完整 PCB 流程产出的 ESP32-S3 成品板:自动布局 -> 板框贴合 -> 规则感知布线 -> 4 层电源平面 -> 丝印碰撞避让,DRC 在真实 EasyEDA 画布上验证通过:

![ESP32-S3 成品板:4 层电源平面 + 圆角板框 + 位号对齐](https://image.lceda.cn/extensions/images/4bc708fd5af1463c88cd0bf388bcdcab.png)

## v1.0.0:原理图功能正式上线

- **原理图全流程 S0–S6 正式可交付**:从方案书 -> 分页 -> 分区 -> 摆放 -> 布线 -> 机械门禁 -> 交付,输入只需一份不含 BOM/网表的客户口吻需求文档。真机回归用例:3 页原理图 / 26 个真实 LCSC 库件 / 18 网黄金表逐脚全对 / 复用 6 个电路块,逐页门禁通过。
- **`sch gate --strict` 五关机械门禁**:一条命令依次过 layout-lint(真实渲染 bbox 查重叠)-> clusters -> check(悬空脚/交叉/压引脚/标签折叠等逐项 finding)-> bridge-check(短路/悬空判据)-> drc,顺序与阻塞判据固定在代码里——「看着对」换成「机械判对」。
- **bridge-check 新增 orphan-tree 悬空树判据**:识别不触及任何引脚的导线树(挪件残留的网络标志 + 桩线、纯裸死线),此前 orphan-stub 与 orphan-flag 两个判据对这种形态双双结构性盲区,只能人工看图发现;现在 summary 返回 `orphanTrees` 计数,`sch gate --strict` 会阻塞放行。
- **三层布局体系 Sheet -> Zone -> Group**:分区框 + 区名 + 每模块电路说明全部由算法计算落位,生成与校验用同一把尺;多器件页未分区会被 `sch check` 机械拦下。
- **电路块库 37 个**(19 ready / 13 verified / 5 draft):CH340 USB 串口、ESP32 自动下载、按键去抖、USB-HUB、降压……`easyeda blocks ls/show/search` 离线可查(无需 daemon/窗口),`sch block-apply` 一条命令完成放件 + 连线 + 网表对账,引脚用功能名引用零改号。
- **跨页网名审计与 netlist 黄金表对账**:`sch nets --strict` 机械拦截网名变体/单引脚网(如 `+3V3` vs `3V3` 这类让主控静默断电的坑),`sch reconcile` 对账设计意图,netlist 黄金表逐脚比对——「接得合法」与「接对没有」分别有门。

## 已支持能力概览

**原理图**

- 器件与库:从立创/LCSC 库按 uuid 放真实器件、换型号、符号/封装重绑、C 号确定性解析;库优先,手绘符号只是兜底。
- 连线:`connect`/`autoconnect`(打分器自选方向,碰撞/穿件/图签全几何成本)、netflag/netport 自动补偿平台旋转存储的坑、成对删除。
- 布局与可读性:模块感知自动布局(模板/官方双引擎)、对齐/等距/刚体平移、分页 reconcile、数据驱动分区框与电路说明。
- 校验与导出:五关门禁、结构校验、跨页网名审计、`sch read` 一次读全(器件+网络+检查)、BOM 导出(自动补 LCSC C 号)、网表导出、页面导图 SVG/PNG/PDF、原生截图。

**PCB**

- 布局:新建板并绑定原理图、模块感知自动布局(间距规则感知)、板框贴合/圆角、布局质量与可布性评分、丝印位置感知避让重排、自由丝印字串(板注/LED 极性标记)。
- 布线与铜:启发式短线布线(规则感知线宽、障碍感知)、铺铜/禁铺区/过孔缝合、4 层电源分配(GND 内电层 + 电源平面 + 焊盘过孔缝合)、挖槽。
- 叠层与制造:铜层数与内层类型设置、读取板子实时 DRC 规则并全链路遵循(缺失时回退 JLCPCB 工艺参考)、DRC/`pcb check`、Specctra DSN 导出/回导(对接外部 Freerouting)。

**基础设施**

- Typed action 协议:`--help` 自描述、动作目录可枚举,结构化输入输出,AI 每一步可观测、可验收、可回放。
- 连接器自愈重连看门狗(daemon 重启/窗口后台都能自动回来)、daemon 防抖自动保存、审计日志、窗口内非阻塞 toast 播报进度。
- `debug.exec_js` 原型逃生口(需二次确认,不作为主工作流)。

完整能力清单与路线图见仓库 `docs/FEATURES.md`。

## 连接器本身做什么

这是一个真实可打包、可导入的 EasyEDA Pro 扩展,刻意保持很薄:

- 本地 WebSocket 传输:端口扫描、握手、注册、上下文同步、心跳、自愈重连;
- typed action 分发:把 daemon 下发的结构化动作映射到官方 `eda.*` 调用;
- 结果序列化:执行结果、警告、错误、上下文回传 daemon;
- 产物传输:截图、BOM、网表等二进制结果编码回传。

真正的工作流、校验、确认、产物处理和多步编排都在 Go CLI/daemon 与 Skill 层完成——所以**必须配套本地 `easyeda` CLI/daemon 一起用**,单装本插件没有任何效果。

## 安装与开始

**1. 装本连接器**(两条通道任选):

- 在本市场页面点击「安装」——平台可原地自动更新,最省心;
- 或从 GitHub Release(https://github.com/zhoushoujianwork/easyeda-agent/releases/latest)侧载 `easyeda-agent-connector.eext`——与 CLI 严格同版。

**2. 装 CLI/daemon + Skill**(一行脚本,自动检测 Claude Code / Codex 并装好 Skill):

```bash
curl -fsSL https://raw.githubusercontent.com/zhoushoujianwork/easyeda-agent/main/install.sh | sh
```

**3. 在 EasyEDA 中确认三件事**:

1. 已安装本连接器(市场安装或 `.eext` 导入);
2. 已开启「允许外部交互 / Allow external interaction」——否则连接器的 WebSocket 连不上本地 daemon;
3. 已启动本地 daemon(`easyeda daemon start`),`easyeda health` 能看到已连接窗口。

**之后升级不必再跑脚本**:`easyeda update` 一键升 CLI + Skill;`easyeda update --check` 只读三方(cli / skill / connector)版本对齐表。

### 版本配套约定

连接器与 CLI 遵循**同一版本号**——四件套(CLI/daemon、连接器、Skill、EasyEDA)需同版本同时在位,落后的连接器会被 `easyeda daemon health` 标成 stale。两条安装通道的取舍:

- **市场版**:平台可原地自动更新,最省心;但市场无发布 API,每版需人工重新提交,**上架版本可能滞后于 CLI**。
- **GitHub Release 侧载版**:与 CLI **严格同版**,需严格版本对齐时以它为准;代价是无原地自动更新,升级需手动卸载旧版再导入。

完整上手、版本对齐与升级注意事项见仓库 `docs/quick-start.md`。

## 更名说明(2026-08)

应市场管理规范要求,本插件**显示名**改为 **EDA Agent Connector**。内部包名与 uuid 均保持不变,同一条目重新上传——已装用户的原地自动更新不受影响,无需任何操作。

## 链接

- GitHub 仓库(架构、路线图、能力矩阵、实战案例):https://github.com/zhoushoujianwork/easyeda-agent
- Releases(严格同版 `.eext` + CLI 各平台二进制):https://github.com/zhoushoujianwork/easyeda-agent/releases
- 完整实战案例:一份需求文档 -> AI 全自动画完 ESP32-S3 四层板,见仓库 `docs/showcase-esp32-mini.md`

MIT 许可,欢迎 star 与共建电路块库(一次贡献,署名可追,永久收益)。
