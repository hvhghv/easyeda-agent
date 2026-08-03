# 设计:原理图暴露面收敛(sch surface convergence)

> **状态**:设计待评审 · **提出** 2026-08-03 · **决策人**:用户已定方向「真合并,允许破坏兼容」
> **验收判据**:esp32Mini E2E 中 agent 实际调用的**不同 sch 子命令数**与**错路回退次数**下降

## 1. 问题陈述

不是「bug 还没修完」,是**能力表面积超过了维护力与 agent 的决策力**。

### 1.1 硬数据(2026-08-03)

| 指标 | 数值 |
|---|---|
| `sch` 唯一子命令数 | **42** |
| 其中在整个 `skills/` 目录 **0 引用** | **12**(`align` `distribute` `connect` `wire` `netflag` `delete` `list` `modify` `select` `set` `open` `pages`) |
| 连接器 `extension/src/actions.ts` | **8308 行单文件 / 88 handler**,测试 786 行 |
| Go `internal/app` | 45781 行 / 66 测试文件 / 449 用例 |
| 唯一端到端回归基准 | `esp32MiniRequire.md`,**42 分钟人工真机** |

### 1.2 语义重叠:同一个任务有多条路

| 任务 | 现有命令 | 条数 |
|---|---|---|
| 把件摆好 | `autolayout`(含 template/official **两套引擎**)、`autoplace-free`、`align`、`distribute`、`group-move`、`block-apply` 自带 `schematic_layout` | **6** |
| 划功能区 | `zone-plan`、`zones`(set/status/clear)、`zone-draw` | **3** |
| 检查 | `check`、`drc`、`layout-lint`、`bridge-check`、`netlist` 对账 | **5** |
| 读状态 | `read`、`list`、`status`、`snapshot`、`pages` | **5** |

**这些路之间的前置/后置条件不互通**:`autoplace-free` 会不会破坏 `zones` 的声明?
`block-apply` 自带 layout 与 `autolayout` 冲突时谁赢?——无数据判据,只有散文描述,
agent 现场猜。**猜错即不稳定**。

### 1.3 主干与能力脱节

- `block-apply`(旗舰块库实例化入口,memory `block-validation-must-be-block-apply`
  明确「块验证必须走 block-apply」)在 skill 里**只被引用 1 次**
- `sheet-geometry`(只读报告)被引用 **8 次** —— 每次布局 agent 都要手查图纸边界再自算,
  这本应封装进 `autolayout`
- `align`/`distribute` 真机验过(memory `sch-placement-methodology-v1`),**从未进主干**

### 1.4 audit log 实证(决定性证据)

`~/.easyeda-agent/audit/*.jsonl`,**76121 条真实 dispatch,2026-06-25 → 07-30,33 天**。
原理图侧 **28 个 typed action / 31155 次调用**(CLI 却暴露 42 个子命令)。

**调用极度集中在头部:**

| 档 | action | 调用占比 | 失败率 |
|---|---|---|---|
| 头部 8 | `connect_pin`(13128) `pages.list` `component.place` `components.list` `component.modify` `save` `check` `read` | **93.3%** | **2.5%** |
| 长尾 15 | `primitives.delete` `drc.check` `titleblock.get` `wire.create` `page.create` `titleblock.modify` `library.get_by_lcsc` `export.bom` `page.delete` `page.rename` `component.delete` `export.netlist` `pin.set_no_connect` `page.open` `group.move` | **1.55%**(484 次) | **12.6%** |

> **长尾的失败率是主干的 5 倍,而它只占 1.55% 的调用量。**
> 这不是「用得少所以没优化」,是「用得少所以坏了没人知道」—— 每一次调用都是一次踩雷。

**活标本:`schematic.titleblock.modify` 调用 32 次,成功 0 次。**
20 次 `EDA_CALL_FAILED: Failed to modify schematic page title block.`(2026-06-26)+
12 次 `NO_CONNECTOR`(2026-07-23)。**这个命令从未成功过一次**,却仍在 skill 文档里被引用。

其余长尾故障画像:`group.move` 60% 失败(陈旧 id,"Pull fresh ids first")·
`page.open` 28.6%(多页场景打不开)· `page.create` 23.5% · `wire.create` 17.5%
(`Failed to create wire.` 无细节)· `export.bom` 28.6%。

**错路回退实测(agent 失败后 60s 内改调他者):**

| 次数 | 回退路径 |
|---|---|
| 146 | `components.list [NO_CONNECTOR]` → `snapshot` |
| 114 | `connect_pin [EDA_CALL_FAILED]` → `components.list` |
| 46 | `connect_pin [EDA_CALL_FAILED]` → `save` |
| 39 | `connect_pin [EDA_CALL_FAILED]` → `check` |
| 38 | `connect_pin [EDA_CALL_FAILED]` → `pages.list` |

top1 的 146 次是 **agent 在连接器根本没连上时瞎试别的命令**,而不是先 `health`。
后四条同源:`connect_pin` 失败 442 次后,agent 分别改调 `components.list`/`save`/
`check`/`pages.list` —— **同一个失败,四种不同的猜法**,没有一种是被规定的。
这正是「无数据判据、只有散文描述」的代价。

## 2. 根因:三个结构性问题

1. **暴露面 > 维护力**。42 个命令各有失败模式;连接器改一处要重打 `.eext` + 全退
   EasyEDA,反馈循环 10 分钟起,而它几乎没有测试。
2. **回归成本 42 分钟且必须活 GUI**。CI 跑不了 ⇒ 多数改动没跑全量回归 ⇒ 回归只在
   下次 E2E 暴露(commit `a1c7c92` 原话:「**集成才暴露**」)。
3. **同一件事两套模型**。Go 侧估算几何(半宽估算、`0.22×0.14` 类比例常数)vs 平台
   真实渲染 + 网格吸附。「判定坐标 ≠ 落地坐标」族 bug(`a3aff56` `8eafc6b` `5c9e2cb`
   `72e402b` `74994cf`)全是同一模板印出来的。

## 3. 方案:三层分档

### Tier 1 — 主干(agent 默认只见这些,目标 ≤12)

每个都必须:① 有测试覆盖 ② 三态返回契约(`ok` / `partial`+`notApplied` / `fail`,
见 #151 样板)③ 在 `design-flow.md` 有明确的流程位置。

| 命令 | 吸收 | 说明 |
|---|---|---|
| `sch read` | `list` `status` `pages` `snapshot` | 统一读入口,形态用 `--view components\|nets\|pages\|image` |
| `sch place` | — | 放单件 |
| `sch block-apply` | — | 放块(旗舰),提升到主干显要位置 |
| `sch autolayout` | `autoplace-free`(→ `--mode free`)、`sheet-geometry`(内建图纸边界查询,不再让 agent 手算) | **引擎收敛为一个默认**;`--engine official` 见 §4 |
| `sch autoconnect` | `connect`(单发退化为 batch size=1) | 连线唯一入口 |
| `sch zone` | `zone-plan` `zones` `zone-draw` | 子命令 `plan\|apply\|draw\|status\|clear` |
| `sch gate` | `check` `layout-lint` `bridge-check` `drc` `netlist` 对账 | **固定顺序跑,出一张统一报告**;非零退出可 gate |
| `sch modify` | — | #151 三态契约样板,保留 |
| `sch delete` | `prim-delete` `disconnect` | 按 kind 守卫统一删除 |
| `sch page` | `page-new` `page-rename` `page-delete` `open` | 页生命周期 |
| `sch save` | — | |
| `sch clear` | — | |

### Tier 2 — 附属(保留但标 `advanced`,不进 skill 主干路径)

`align` `distribute` `group-move`(人工微调)· `wire` `netflag` `no-connect`(原语级)·
`rebind-footprint` `rebind-symbol` · `titleblock` `titleblock-get` · `extract-layout`

出问题**不阻塞主流程**,不承担三态契约与测试硬门。

### Tier 3 — 砍

- **`autolayout --engine official`**:@beta 平台 API,memory `official-sch-autolayout-beta-works`
  实测「连通性放射状比我们模板**散**」,且长操作 >60s 需前台轮询。留着是纯负债 ——
  降级为 `debug` 域实验命令或直接移除。
- **`select` / `set`**:0 引用,语义已被各命令的 `--ids` 取代。

## 4. 迁移与兼容

- 被吸收的命令保留 **deprecated 别名一个版本**,执行时 stderr 打印新写法后照常工作;
  下一个 minor 移除。
- Skill 侧**同步改**(项目首要准则:Skill 优先)——`design-flow.md` / `schematic.md` /
  `SKILL.md` 的命令引用全部换成主干写法,Tier 2 从流程脊柱移出到「微调工具」附录。

## 5. 验收

**主判据(用户选定)**:跑一次 `esp32MiniRequire.md` E2E,统计
1. agent 实际调用的**不同 sch 子命令数**
2. **错路回退次数**(调了 A 发现不对改调 B)

**基线已从 audit log 离线测得(2026-08-03,见 §1.4,无需跑真机)**:

| 指标 | 基线(33 天真实流水) |
|---|---|
| 原理图 action 种类 | **28**(CLI 暴露 42 子命令) |
| 头部 8 个 action 覆盖 | 93.3% 调用 / 2.5% 失败率 |
| 长尾 15 个 action | 1.55% 调用 / **12.6% 失败率** |
| 原理图错路回退(失败→60s 内改调他者) | **≥520 次**,top5 路径见 §1.4 |
| 从未成功过的 action | `schematic.titleblock.modify`(0/32) |

收敛后重跑同一脚本(`scratchpad/audit_baseline.py` 已固化,建议入库到
`skills/easyeda-agent/scripts/`),要求:**长尾失败率与主干持平**(冗余已删,不存在
「用得少所以坏了没人知道」的角落)、**错路回退路径 top5 消失或收敛到单一规定路径**。

**辅助判据(静态,不用真机)**:
- Tier 1 命令 100% 有测试覆盖 + 三态返回契约
- `sch --help` 顶层条目 ≤12

## 6. 不做什么

- **不在本轮加新功能**,也不给 Tier 2/3 补测试 —— 先砍再建,否则等于给冗余配基建。
- 「离线回归基准(真机快照 fixture 重放)」是下一轮,**必须在收敛之后**,否则 fixture
  会固化掉即将被删的接口。
