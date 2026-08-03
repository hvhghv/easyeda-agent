# Roadmap

战略路线图——**接下来做什么、为什么、怎么算完成**。本仓库的文档分层:

| 文档 | 粒度 | 性质 |
|---|---|---|
| [`vision.md`](./vision.md) | 原则 / 非目标 | 静态愿景 |
| [`architecture.md`](./architecture.md) | 组件 / 责任 | 静态架构 |
| [`FEATURES.md`](./FEATURES.md) | 功能 / typed action 状态 | 滚动 + 含细粒度待办 |
| [`optimization-loop.md`](./optimization-loop.md) | 探针轮次 / A-B-C 改进 | 滚动机制 |
| **`ROADMAP.md`(本文件)** | **方向 / 优先级 / 完成判据** | **滚动战略** |

滚动更新;每完成一项把对应行移到"Done"区,每月 review 一次调整优先级。
**优先级是我的建议,实际可调**——结合真机探针结果和用户反馈。

---

## 现状(2026-08-03)

- **能力**:94 typed actions(`make actions` 为准);26 个电路块(`easyeda blocks ls` 为准)
- **真机验收基线**:`esp32MiniRequire.md` 端到端跑通,42 min,0 短路,0 fatal
- **沉淀**:60+ 条踩坑 memory;`docs/FEATURES.md` / `optimization-loop.md` 持续滚动
- **分发**:Skill 已上 ClawHub;connector 已上 jlc-ext 市场(滞后 CLI 数版)

---

## P0 — 补齐已知半成品(本周 / 本月)

> 这些是**已经在 SOP/记忆里有方案,只差落地**的项目。性价比最高。

### P0.2 块成熟度看板

- **背景**:`circuit-block-library-core.md` "下一步 CH340C 块 ceshi 跑通填 validated";库内 26 块大部分停留在 `validated: null / "pending"`
- **现状**:JSON 里已有 `validated` 字段,缺一个**总览命令**
- **目标**:`easyeda blocks maturity` 输出一张表:每块在哪一档(`draft` / `ceshi-validated` / `production`),带"待验清单"和"生产用过次数"
- **验收**:看一眼能识别"哪些块可放心用 / 哪些是雷区";高优块的 validated 自动空缺提醒

---

## P1 — 放大沉淀价值(本季度)

> 这些是**已经积累的资产还没有充分发挥**的项目。改动中等,但价值放大倍数高。

### P1.1 智能重试 + 错误根因解释

- **背景**:60+ 条 memory 沉淀了大量踩坑,但当前是**人去翻**才能用;失败时 daemon 只报 stack trace / 通用错误
- **现状**:例如 block-apply 短路四因(`block-apply-shorts-four-causes.md`)、铺铜 reflow 快照(`pour-reflow-divergence-and-rules-api.md`)、import-changes 卡弹框(`import-changes-no-incremental-add.md`) 都是已知模式,但每次踩都从头排
- **目标**:daemon 错误处理层加 `errorAdvisory` 字段;按 `(action_name, error_pattern)` 匹配 memory slug,自动带"为什么 + 怎么修"
- **验收**:触发已知 5 类失败(block-apply 短路 / 铺铜 stale / import-changes 卡弹框 / via-on-pad / autoLayout @beta 长操作)时,错误消息自动带可操作的修复建议 + 引用 memory 路径

### P1.2 块库填 `validated`(选 3-5 个高频块跑 ceshi)

- **背景**:CH340C / ESP32 auto-download / buck 等高频块待验
- **目标**:每个块跑一次完整流程(`block-apply` → sch check → DRC=0),填 `validated: {date, board, drcResult}`
- **验收**:3-5 个块标 production 档,带可重现的 board 路径 + DRC 报告

---

## P2 — 战略升级(下季度)

> 从"流程编排"到"端到端智能"的跨越。

### P2.1 需求驱动端到端

- **背景**:`esp32MiniRequire.md` 当前是**手工跑一次**作为回归基线;本质上是"客户一句话到加工文件"的端到端测试
- **现状**:S0–S6 + P0–P10 流程已文档化,但每步需要 agent 编排;块库已上线但 `sch block apply` 还在 phase-2 待做
- **目标**:从"自然语言需求 → 加工文件"全自动
  - 需求解析器:把需求 → 块组合 + 选型(基于块库的 `intent_match` 字段或新增)
  - 端到端编排器:选型 → `block-apply` → 布局 → 布线 → DRC → 导出
  - 验收脚本:对照需求逐条落实(和 `esp32MiniRequire.md` 一样的口吻)
- **验收**:跑 `esp32MiniRequire.md` 全程零人工,DRC=0,需求条条落实(灯泡亮、5V→3V3、CH340 烧录、M3 固定等)

### P2.2 DFM 主动检查 + 设计评审报告

- **现状**:`pcb check` 是"做完检查",可以做成"做时主动避坑"
- **目标**:设计完自动出报告
  - DFM 评分(对标 IPC-2221 / JLC 工艺能力,见 `pcb-design-rules.md`)
  - BOM 成本估算(LCSC 实时价)
  - 电源树审计(关键网是否都有完整供电回路)
  - 信号完整性粗评(差分对长度匹配、晶振布局、阻抗连续性)
- **验收**:跑完一块板自动出 PDF 报告,人工 review 时间从"全板 DRC + 翻规范"降到"看报告红黄绿"

### P2.3 市场分发策略决断

- **背景**:`connector-reimport-stale-window.md` + `marketplace-readme-rules.md`——CLI 已上架 jlc-ext(约 v0.9.0),但**滞后 CLI 数个版本**,且无发布 API,只能手动 web 提审
- **选项**:
  - A. **放弃 jlc-ext**,主推 sideload(`.eext` via GitHub Release)+ ClawHub skill;市场仅作"知道有这项目"的入口
  - B. **投入自动化发版**——web 自动化脚本提审(playwright 模拟),跟 CLI 同步发布
  - C. **接受滞后**,只把 `v0.x.0`(主版本)同步到市场,中间版走 sideload
- **验收**:选 A/B/C 并固化进 `make release` 流程

---

## P3 — 基础设施(持续)

> 长期价值,不绑定具体业务节奏。

### P3.1 audit log 升级:`before/after` state + 可回滚

- **现状**:`audit-log` 记录事件流,但**没结构化存 diff**;playbook 回放能复现,但不能回滚到中间态
- **目标**:每次 mutation 记录可逆的 `{primitiveId, before: {state...}, after: {state...}}`;支持 `easyeda audit rollback --to <step-id>`
- **验收**:回滚后 primitive state 跟目标步一致,后续 action 可继续跑

### P3.2 录制回放 + diff 回归 CI

- **现状**:`record-replay-loop-shipped.md` 已落地 `apply <playbook.json>` 确定性回放
- **目标**:录两次回放,自动 diff 哪个 action 导致 DRC / 几何变化;PR 时自动跑 `examples/*.playbook.json`,出"本 PR 影响哪些 board 的哪些 metric"
- **验收**:PR 触发自动回归;影响范围自动报告

### P3.3 多语言 + 行业垂直包

- **现状**:Skill 主要英文场景
- **目标**:中文 Skill(主要用户群);垂直场景包(电源板 / 电机驱动 / IoT 节点 / USB Hub)——每包 = 一套精选块 + 工艺参数 + 评审 checklist
- **验收**:中文 Skill 真机跑通;垂直包至少 2 个场景模板化

---

## 决断记录

> 记录**为什么这么排优先级 / 为什么砍 / 为什么加**。每条带日期 + 上下文。

- **2026-08-03**:本路线图首次落。基于 esp32Mini E2E 通过 + 块库上线 + `FEATURES.md` 已有 94 actions 后的 review;以"补齐半成品 → 放大沉淀 → 战略升级 → 基础设施"四档分优先级。P0 选 track-lock + 块成熟度是因为它们**只在等工具,不需要新调研**;P2.3 市场策略单列是因为这是一个**等待决断**的悬而未决项。

---

## Done(完成后回填)

- **✅ P0.1 `pcb route-critical` + `pcb track-lock` 工具**(commit `0c94481`,connector 0.15.2,#127,
  2026-07-19 落地):
  - **`pcb track-lock`** (`cmd_pcb.go:1173-1237`) — `--net/--ids/--all` 锁定 + `--unlock` 释放 +
    `--no-fills` 控制填包含;dispatch typed `pcb.track.lock` action(graduated from debug.exec_js);
    tracks + arcs(beautify 拐角)+ vias + net-bound fills 都在范围内,覆铜永不碰;
    `route.delete` 自动跳过 locked 锁死的图元,实现"锁了自动不动"
  - **`pcb route-critical`** (`pcb_route_critical.go`,17KB) — 一条命令完成 P7.0 全流程:
    电源大面积块(2 层 pour / 4 层 planes)+ 差分对(从块库 `signals` + 网表 name-pattern 双源去重)
    → 短路由规划 → 实测长度 + skew 报告(默认 5 mil,块声明的 `length_match_mm` 覆盖);
    skew 超差**报告**而非自动蛇形(短距场景成对短优先,#127 范围);真耦合/蛇形仍在路线图
  - 验证:`pcb_track_lock_test.go` 头注释明记"旧 JS builder 已移除,typed handler 由 real-machine flow 覆盖",
    Go flag contract 由 `pcb_route_critical_test.go` 隐式覆盖

---

## 引用

- [`vision.md`](./vision.md) — 愿景、原则、非目标
- [`FEATURES.md`](./FEATURES.md) — 当前功能状态 + 细粒度待办(每个 typed action 的详情)
- [`optimization-loop.md`](./optimization-loop.md) — 探针驱动的滚动改进(A/B/C 分类)
- [`e2e-automation-acceptance.md`](./e2e-automation-acceptance.md) — 端到端验收判据
- [`esp32MiniRequire.md`](../esp32MiniRequire.md) — 端到端探针需求(客户原始口吻)
- `~/.claude/projects/-Users-mikas-github-easyeda-agent/memory/` — 60+ 条踩坑沉淀
