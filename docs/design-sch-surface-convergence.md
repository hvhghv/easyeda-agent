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
| `sch prim-delete` | `delete`(**已移除**,2026-08) | 唯一删除入口(任意图元类型);`disconnect` 保留语义化断开 stub+flag |
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

## 6. 进展

### ✅ 第一刀:`sch gate`(2026-08-03 落地)

选它先行是因为**它零破坏**:纯新增聚合命令,四个单命令原样保留,却直接消灭了
「跑哪个检查、什么顺序、谁的退出码算数」这个每次都要重做且没有判据的决策。

- `internal/app/cmd_sch_gate.go` — 固定流水线 `layout-lint → check → bridge-check → drc`
  (顺序理由写在代码注释里:几何最便宜且解释力最强,DRC 最慢且需前台故垫底)
- **`blocked` verdict 是本刀的核心设计**:把「检查器没跑起来」和「板子有问题」分开。
  §1.4 里 146 次 `components.list [NO_CONNECTOR] → snapshot` 的盲试,根因就是二者混同 ——
  agent 把 infra 失败当板子失败,于是去试别的命令。现在第一个 stage 报 `error` 就停,
  后续 stage 标 `skipped` 而不是继续撞同一堵墙,报告直接指向 `health` / `doc switch`。
- **每个失败 stage 自带规定的下一步**(`gateAdviceFor`)—— 修法跟着失败走,不靠 skill 散文。
- `--only`/`--skip` 拼错 stage 名**直接报错**,绝不静默少跑一关(少跑一关的绿灯比红灯更危险)。
- 复用现有 `parseCheckReport`/`parseBridgeReport`/`parseDrcReport`,只从 `runLayoutLint`
  提取了 `collectLayoutLint`(纯提取,行为不变)——**没有重写任何检查器逻辑**,回归面最小。
- 13 个单测钉住:流水线顺序、`--only` 按流水线序而非参数序、未知 stage 报错、
  三态判定(含 `blocked` 优先于 `fail`)、error stage 不产出告警、渲染带「下一步」。
- Skill 同步:`SKILL.md` 停点表 ②、`design-flow.md` S5/S6、`schematic.md`、
  `auto-layout-sop.md` 最终验证门 —— 主干路径全部改走 gate。

**✅ 真机已验(2026-08-04,web 编辑器 `pro.lceda.cn` + 官方「示例工程_快速入门」,只读)**:
默认档 **0.86s 四关全过**;DRC **没有**卡后台(窗口前台时正常返回),此前担心的
「DRC 前台要求会让 gate 默认 blocked」未发生。

**但 `--strict` 档当场暴露 3 个会误导 agent 的缺陷,已修并回归**:

| # | 缺陷 | 症状 | 修法 |
|---|---|---|---|
| 1 | layout-lint 的 summary 不含 strict 判据字段 | 报告写「0 overlap, 0 pin-coincidence, 0 tight…」却 FAIL,**自相矛盾** | summary 补 no-bbox / unchecked-pin / unproven-pin / invalid-geometry |
| 2 | `Errors` 计数与 `Status` 判据脱节 | blocker 行写「**0 个阻塞项**」却判失败(strict 失败在被提升的告警上,不在 error 计数里) | 新增 `BlockingReasons[]`,status 与 blocker 都由它决定;Errors/Warnings 降为纯展示用的严重度统计 |
| 3 | 建议按 **stage 名**给,不看实际失败项 | 0 bridge 却教「拆掉真短路」,0 overlap 却教「重排几何」 | 建议表改为按 **reason 关键词**匹配(`gateAdviceRules`) |

第 3 个最恶劣 —— **把 agent 引向不存在的问题**,正是本文档要根治的病;它在单测里没被抓到,
因为我构造的 stage 都是 `Errors>0` 才 fail,没覆盖「strict 提升告警致 fail 但 Errors=0」这个组合。
**真机验证的价值就在这里**:纯几何/纯逻辑的单测证明不了「报告读起来对不对」。

修复后同一块板:
```
• layout-lint: 34 unproven pin geometry (--strict;连接器未给 pinsAvailable 契约)
• check: 23 个 warn/info finding (--strict): wire-crossing×13, dangling-wire×8, zero-length-wire×2
• bridge-check: 11 orphan-stub (--strict)
→ 引脚几何未经证明:连接器太旧没给 pinsAvailable 契约 —— 升级连接器,或本轮去掉 `--strict`(不是电路问题)
```
新增 8 个回归测试钉住(含两条「建议不得指向板子没有的问题」的负向断言)。

**副产物**:34 个 unproven-pin 的真因是**市场版连接器 0.17.3 滞后 CLI 0.18.3**,不发
`pinsAvailable` 契约 —— 印证了 CLAUDE.md 记的市场版滞后问题,且现在 gate 会**直接说出来**
而不是让 agent 去挪器件。

### 下一刀(未开始)

摆放那 6 条路(`autolayout` 两引擎 / `autoplace-free` / `align` / `distribute` /
`block-apply` 自带 layout)—— **这刀真会破坏兼容**,需要先定「谁吸收谁」。

## 7. 不做什么

- **不在本轮加新功能**,也不给 Tier 2/3 补测试 —— 先砍再建,否则等于给冗余配基建。
- 「离线回归基准(真机快照 fixture 重放)」是下一轮,**必须在收敛之后**,否则 fixture
  会固化掉即将被删的接口。
