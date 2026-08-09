# 金标准好板回归 fixtures（#167 第五层 LEARNING）

`easyeda pcb layout-score` 的九维权重和阈值，注释里大多标着「**待校准初值**」。
这个目录是把那句「待校准」变成一条能失败的断言的地方。

判据来自 #167 原文：

> 拿一块「人类公认的好板」跑 `layout-score`，它就该得高分；**若某维在好板上得了
> 低分 → 是度量错了，回去校准**。把一批好板收进回归 fixtures：度量改了 / 规则加了，
> 好板一跑分数不该掉。

跑它：

```bash
make layout-calibrate                                         # 仓库根
go test ./internal/app/ -run TestLayoutScore_GoldenBoards -v   # 等价
```

全离线、不连编辑器、~1 秒。harness 在
[`internal/app/pcb_layoutscore_golden_test.go`](../../pcb_layoutscore_golden_test.go)。

---

## ⚠️ 范围声明：现在这里**只有合成板**，它证明不了阈值是对的

当前两块 fixture 都是**合成的**，是照着九维判据手工摆出来的。参考板拿满分是
**同义反复**，不是校准证据。

它们能证明的：

| 能证明 | 靠哪块板 |
|---|---|
| 度量在一块摆对了的板上**不误报**（改动引入的新扣分会立刻现形） | `reference-*` |
| 度量对缺陷**仍有反应**（某一维退化成恒返 100 会立刻现形） | `degraded-*` |
| 报告的自洽性（verdict ↔ blocking、skipped 必有原因、归因按扣分降序） | 两块都验 |

它们**证明不了**的：拐点定在 0.20 还是 0.15、保护件预算 250mil 还是 400mil、
九维权重那张表对不对。**这些只能拿真板校准** —— 见下一节。

> 只有正对照的回归有个经典死法：度量退化成恒返 100，好板照样满分，测试全绿。
> 所以这里的约定是**参考板必须配一块负对照**，负对照断言九维全部掉到 100 以下。

---

## 加一块真板（这才是校准）

**哪些板可以入库（先看这条再 dump）**：只有**开源/官方参考板**（oshwhub 公开工程、
官方对标板）可以把 dump 提交进本目录；**商业设计一律不入库**——本地跑
`EASYEDA_BENCH_BOARD=<dump路径> go test -run TestLayoutBenchmark` 走环境变量通道
（`pcb_layout_benchmark_test.go` 的约定，两处约定以此段为准）。

真板 dump 需要 EasyEDA 打开对应工程、PCB 在**前台**（`pcb.outline.get` 在后台返
null，板框读不到 → 所有「到板边」的维会降级或跳过）。

```bash
# 1. 抓板（PCB 切前台后再抓）
easyeda doc switch PCB1 --project <工程名>
easyeda pcb dump --project <工程名> \
  --out internal/app/testdata/boards/<板名>.json

# 2. 看它现在得几分（这一步是判读，不是走过场）
easyeda pcb layout-score --from internal/app/testdata/boards/<板名>.json \
  --spec internal/app/testdata/boards/<板名>.spec.json --all
```

**第 2 步出来的分数不要直接抄进期望值。** 这块板是被当成「人类公认的好板」收进来的
——它某一维得低分，按 #167 的判据**先怀疑度量**：

1. 打开报告里那一维的归因，看被扣分的是哪几个器件；
2. 拿真实的板对照：那几个器件真的摆错了吗？
   - 真错了 → 这块板不够格当参考板，换一块，或者收成 `degraded` 负对照；
   - 没错 → **度量误报**。回 `internal/app/pcb_score_*.go` 改判据/阈值，
     然后**回头重跑本目录全部 fixture**（改阈值可能把别的板打掉）；
3. 校准完再把期望下限定在实测分下方留一点余量（合成板用的是 5 分，真板噪声更大，
   10 分是合理起点），并在 `note` 里写清这块板是什么、为什么这么定。

三条落地要求：

- **来源要写在 `note` 里**（哪个工程 / 哪块开源板 / 谁的设计），否则半年后没人知道
  凭什么信它；
- 抓 `oshwhub` 上的开源板做参考板时，只读、别改人家的工程（见项目记忆
  `oshwhub-training-source`）；
- 参考板与负对照**成对**加。负对照最省事的做法就是把参考板复制一份、逐维注入一个
  缺陷（`degraded-4stage-compact.expect.json` 的 note 里列了十三处，照抄那个套路）。

---

## 文件约定

| 文件 | 是什么 | 谁生成 |
|---|---|---|
| `<板名>.json` | 板级几何快照，**就是 `easyeda pcb dump` 的原样输出** | 机器 |
| `<板名>.spec.json` | 这块板的 S0 spec（`internal/spec` 的形状） | 人 |
| `<板名>.expect.json` | 人工核定的期望值 | 人 |

三条硬约定：

1. **`<板名>.json` 必须与 `pcb dump` 的输出逐字节同形。** 所以期望值放 sidecar，
   不塞进快照里当 `_expect` 字段 —— 否则「抓真板 → 直接落进 testdata」不再是一条
   命令的事，而且重抓刷新几何时会把人工核定的期望一起覆盖掉。测量数据与人的判断
   分开放，也和本仓 `fetchBoardSnapshot`(取数) / `analyzeLayoutScore`(判定) 的既有
   分法一致。
2. **每块板必须有 `.expect.json`。** 缺了是**硬失败**：一块没有期望值的 fixture 会
   躺在目录里一条断言都不参与，看起来还多了个绿勾 —— 比没有 fixture 更危险。
3. **`.expect.json` 拒绝未知字段。** 拼错的键（`minDimensions` / `maxBlockings`…）
   静默失效等于这条期望从未生效，所以解析时开了 `DisallowUnknownFields`。

### `.expect.json` 字段

```jsonc
{
  "kind": "reference",          // reference（人工核定的好板）| degraded（注入缺陷的负对照）
  "title": "…",                 // 人读标题
  "note": ["…", "…"],           // 字符串或字符串数组：这块板是什么、维持分数的几何关系
  "spec": "other.spec.json",    // 可选：覆盖「同名 .spec.json」的约定（相对本目录）
  "noSpec": false,              // 可选：显式声明这块板不配 spec（意图类维度理应 skipped）

  "minOverall": 96,             // 综合分下限
  "maxOverall": 77,             // 综合分上限（负对照用）
  "minBlocking": 1,             // 阻塞项条数下限（负对照用：注入的硬错必须被抓到）
  "maxBlocking": 0,             // 阻塞项条数上限

  "minDimension": { "partition": 95, … },   // 逐维下限
  "maxDimension": { "protection": 40, … },  // 逐维上限

  "requireScored": ["partition", …],        // 这些维必须参与加权（status ∈ scored|degraded）
  "expectSkipped": ["rf"],                  // 这些维必须是 skipped（真板缺数据时的诚实声明）

  "gridMil": 5,                 // 可选：对齐 --grid
  "minGapMil": 8                // 可选：对齐 --min-gap
}
```

`requireScored` 值得单说：它守的是硬约定①「**没测 ≠ 测了满分**」的反面。一次改动
如果让某维悄悄变成 `skipped`，`minDimension` 是查不出来的（skipped 维不参与比较），
报告还是一片绿，实际上少测了一维。要这条断言才拦得住。

**断言一律是阈值型，不是精确值。** 本仓既有的 Go 测试全是「构造 N 个器件 → 断言
exact 数字」；那种写法在这里是毒药：度量本来就是要被校准的，权重和拐点一动就得重新
冻结全部 golden，于是没人再敢动权重 —— 恰好把 #167 想要的校准闭环锁死。

---

## 现有 fixture

| 板 | kind | 是什么 |
|---|---|---|
| `reference-4stage-compact` | reference | 合成参考板。POWER→MCU→RF→ANT 四段流向，16 器件，2000×1150mil 四层板；九维各 100 分、0 阻塞项 |
| `degraded-4stage-compact` | degraded | 同一块板注入十三处缺陷的负对照；九维全部 < 100，1 条阻塞项 |

两块板的详细说明（维持分数的几何关系 / 注入了哪些缺陷 ↔ 该响哪一维）在各自的
`.expect.json` 的 `note` 字段里。改 fixture 前先读那一段，否则会莫名其妙掉分。

---

## 一条给后人的更正

#167 原文说把好板「收进 `skills/easyeda-agent/scripts/tests/`」——**那是错的**。
那个 harness（`run.py`）是原理图 linter 专用，`check_fixtures` 会无条件把
`fixtures/*.json` 喂给 `lint.py`，塞一份 PCB dump 进去会直接崩。金标准板走 Go 侧
`testdata`，也就是这里。

---

## 2026-08-10 真板校准记录（首轮，5 块公认好板）

用户工程内的开源板 `pcb dump` 后离线打分（dump 未入库——官方 _copy 工程虽开源，
但 fixture 要配人工核定的 .expect.json，等 clearance 曲线校准后再入）：

| 板 | 件数 | 综合 | blocking(修错前→后) |
|---|---|---|---|
| 立创·庐山派K230 _copy | 405 | 43.6 [blocked] | 15 → 9 |
| 立创·实战派ESP32-S3 | 180 | 48.5→58.5 | **104 → 1** |
| 瑞芯微RK3568四层 _copy | 362 | 46.6 [blocked] | **143 → 8** |
| 立创·3.1寸MIPI扩展板 _copy | 34 | 38.4 [blocked] | 5 → 4 |
| BBClaw开源AI语音终端 | 69 | 67.7 [fair] | 0 → 0 |

**本轮修掉的度量错误**（好板掉分 → 度量错了，README 判据的第一次真实兑现）：
1. **渲染 bbox 当 courtyard** —— 含丝印比本体大 40%+，专业密板贴装间距小于丝印
   外扩 → 267 条 blocking 噪声。修：重叠/间距判定改**焊盘并集**本体代理
   （无盘件退渲染框）。
2. **圆盘按矩形算** —— 圆盘四角的"假铜"把挨着安装孔环的 LED 盘判成跨网短路
   （K230 两条）。修：圆参与的接触判定用真实圆几何（圆-圆/圆-矩最近点）。

**遗留校准队列**（数据在手，待改）：
- `clearance` 在三块密板上仍归零 —— 6mil 铜规则当装配间距阈值 + 不按密度归一，
  professional reflow 密板全员"过近"。需要：按装配工艺分档的间隙曲线 + 密度归一。
- `routable` 在**已布线成品板**上恒 26~40 —— ratsnest 交叉不知道板子已经布完了。
  快照不含铜线数据；至少该在 detail 里声明"成品板上该维系统性偏低"。
- 官方板的**装配选项堆叠**（同位置放两个 fit 选项、只焊一个）会留 1~9 条真实
  盘级 overlap —— 不是度量错误，但把好板压成 [blocked]。需要 BOM fit 数据或
  显式豁免语义才能分辨。
- `partition` 无 spec 时 37~56（已知粒度问题）；K230 `edge-io` 16（板框 ARC 降级
  AABB + 凸出连接器，待真多边形弧线支持）。MIPI `compact` 10 —— 结构件形态板
  （大板少件）被当"太空"罚，需要形态豁免或坡度放缓。
