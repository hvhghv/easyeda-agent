# 回归与判据可信度测试 — 2026-08-16

> 执行:subagent(只测、只读、不改任何代码)
> 时间窗:2026-08-15 18:15:20 → 18:21:52 UTC(墙钟 5.8 分钟)
> 被测:HEAD `eeae996`(本轮 8 提交:`sch nets` / `sch status` / `audit cost` /
> 图签假失败复核 / zone-plan 装不下 vs 摆得不好 / clusters 同组豁免 / `settleRead` /
> `ruler_consistency_test`)
> 工程:`ceshi`(P1_POWER / P2_MCU / P3_USB_DEBUG / P4_IO)

---

## 1. 结论

**离线全量 0 FAIL,真机四页与预期一致(唯一阻塞项就是 missing-partition),三条判据的
负对照全部会响且都能还原干净 —— 但记录到 8 处「输出/文档/实际行为不一致」的地方,外加
1 处 `make blocks-audit` 的既有块库缺陷和 1 处我自己测试留下的不可逆副作用。**

没有发现任何一条判据「造了缺陷却不报」。所有问题都在**判据说的话**和**判据做的事**
之间,或者在**报告的可读性**上。

---

## 2. 离线全量验证

### 2.1 `go build ./...`

```
BUILD_EXIT=0
```

### 2.2 `go test -count=1 ./internal/...`(强制不吃缓存)

```
TEST_EXIT=0
ok  	.../internal/apidoc	0.496s
ok  	.../internal/app	4.099s
ok  	.../internal/blocks	1.136s
ok  	.../internal/daemon	2.326s
ok  	.../internal/pcb/svgimport	1.557s
ok  	.../internal/protocol	2.324s
ok  	.../internal/selfupdate	2.889s
ok  	.../internal/spec	3.348s
?   	.../internal/version	[no test files]
ok  	.../internal/workflow	3.391s
```

本轮新增的尺子一致性测试单独跑过,5 条全过:

```
--- PASS: TestRuler_ClusterGapMatchesSolverGap (0.00s)
--- PASS: TestRuler_GateDefaultsShared (0.00s)
--- PASS: TestRuler_CircuitNoteCountSingleSource (0.00s)
--- PASS: TestRuler_SettleReadBudgetSane (0.00s)
--- PASS: TestRuler_ConnectPinBudgetExceedsConnectorWorstCase (0.00s)
```

### 2.3 `make lint-test`

```
LINT_EXIT=0
✓ orientation table: spec derives to frozenTable; cycle law holds
✓ connector (actions.ts) facts match orientation.json
✓ bulk-connect parses enveloped sch check findings
✓ fixture bad_orientation / clean_board / flag_on_pin / floating_pin / sop_spatial / zero_wire
✓ diff edit
all rule-trust checks passed
```

### 2.4 `make blocks-audit` — **非零退出(既有块库缺陷,按要求未修)**

```
BLOCKS_EXIT=2
refs judged: 1027  unique=1025 fanout=2 missing=0 unknown=0

block.ams1117_ldo_3v3
  FANOUT   U.VOUT           → pins ['2', '4']   ⇒ write "U.VOUT*"
  FANOUT   U.VOUT           → pins ['2', '4']   ⇒ write "U.VOUT*"

2 bad reference(s). FANOUT → add the `*` suffix; MISSING → use the symbol's real pin name.
make: *** [blocks-audit] Error 1
```

---

## 3. 真机四页状态复核

### 3.1 `sch gate --strict`(逐页)

| 页 | 结果 | 退出码 | 阻塞项 | drc |
|---|---|---|---|---|
| P1_POWER | **PASS** | 0 | 无 | 0 fatal / 0 error / 1 warn |
| P2_MCU | **FAIL** | 1 | `check: missing-partition×1` | 0 fatal / 0 error / 1 warn |
| P3_USB_DEBUG | **FAIL** | 1 | `check: missing-partition×1` | 0 fatal / 0 error / 1 warn |
| P4_IO | **PASS** | 0 | 无 | 0 fatal / 0 error / 1 warn |

P3 原样输出:

```
sch gate: FAIL
  PASS   layout-lint    0 overlap, 0 pin-coincidence, 0 tight, 0 off-grid, 0 out-of-sheet, 0 no-bbox, 0 unchecked-pin, 0 unproven-pin, 0 invalid-geometry (zone-check=not-configured sheet-check=checked)
  PASS   clusters       11 个虚拟组:0 重叠 / 0 出图纸 / 0 过近
  FAIL   check          1 finding(s): 0 error/fatal, 1 warn/info
  PASS   bridge-check   0 bridge(short), 0 orphan-stub, 0 orphan-flag (44 wire tree(s))
  PASS   drc            0 fatal, 0 error, 1 warn, 0 info (total 1)

阻塞项:
  • check: 1 个 warn/info finding (--strict): missing-partition×1
告警(不阻塞): check: 1 条告警, drc: 1 条告警
```

**P1/P4 没有 missing-partition,不是漏报**:阈值是 `schPartitionMinParts = 6`
(`internal/app/cmd_sch_marker_geom.go:634`),P1 只有 5 件、P4 只有 2 件,规则按设计不触发。
所以「gate 只应剩 missing-partition」这条预期在四页上都成立。

DRC 的那 1 条 warn 平台不给明细:

```
sch drc: 1 violation(s) — 0 fatal, 0 error, 1 warn, 0 info
  WARN   warn  1 issue(s) — EDA returned no per-item detail
```

### 3.2 `sch clusters --strict`(逐页)

四页全部 **0 重叠 / 0 出图纸 / 0 过近,退出码 0**。原样(P3):

```
clusters — 11 个虚拟组(器件 + 它自己的 marker/桩线;跨器件的连线不计入体积)
  Q1     体积 x=[18,240] y=[334,386]  223×51   本体 11×21   marker 3 / 桩线 3
  R5     体积 x=[100,342] y=[404,416]  243×11   本体 21×9   marker 2 / 桩线 2
  R6     体积 x=[100,342] y=[304,316]  243×11   本体 21×9   marker 2 / 桩线 2
  D1     体积 x=[106,314] y=[442,498]  208×57   本体 48×57   marker 3 / 桩线 3
  Q2     体积 x=[238,460] y=[334,386]  223×51   本体 11×21   marker 3 / 桩线 3
  U3     体积 x=[356,930] y=[414,507]  575×92   本体 71×91   marker 9 / 桩线 9
  R3     体积 x=[404,514] y=[210,306]  109×97   本体 9×21   marker 2 / 桩线 2
  C8     体积 x=[430,450] y=[472,606]  21×134   本体 17×21   marker 2 / 桩线 2
  R4     体积 x=[534,644] y=[210,306]  109×97   本体 9×21   marker 2 / 桩线 2
  J1     体积 x=[592,1018] y=[198,422]  427×223   本体 71×71   marker 14 / 桩线 14
  C7     体积 x=[954,1064] y=[510,606]  109×97   本体 17×21   marker 2 / 桩线 2
✓ 11 个组:0 重叠 / 0 出图纸
```

P1 5 组 / P2 8 组 / P4 2 组,同样 `✓ … 0 重叠 / 0 出图纸`,退出码 0。

### 3.3 `sch status --all-pages`

```
sch status — 工程 "ceshi" · 4 页 · **全部实测自活体,不落盘、不会过期**

  页                  图纸     页名     组      框/说明   器件     导线
  P1_POWER           ✓      ✓      1      0/1      5      12
  P2_MCU             ✓      ✓      2      0/3      8      23
  P3_USB_DEBUG       ✓      ✓      4      0/2      11     44
  P4_IO              ✓      ✓      1      0/1      2      4

  ✓ S1  图纸/分页      4 页,页名皆有功能语义
  ◐ S2  编组/分区      4/4 页有组、0 页有分区框、4 页有电路说明(交付前三者都要有)
  ✓ S3  摆放         26 件已落位
  ✓ S4  布线         83 段导线(接得对不对由 S5 判)
  ? S5  校验门        未验 —— status 只报进度;逐页 `sch gate --strict --doc <页>`
  ? S6  存盘         平台不暴露脏标记,无法从活体判定 —— …

next: easyeda sch zone-plan --json  →  easyeda sch zone-draw
```

与 `sch check` 口径一致:0 页有分区框 ↔ P2/P3 报 missing-partition;4 页有电路说明 ↔
check 未报 missing-note。**S2 的「电路说明」新口径复核通过。**

### 3.4 `sch nets`(全工程)

```
sch nets — 18 张网(**全工程,跨页**)

✓ 18 张网,无同轨异名,无单引脚网
NETS_EXIT=0
```

`--all` 基线(测试前后逐字一致,见 §4.1):

```
  3V3   9 脚 C1,C3,C4,C6,R1,R2,U1,U2      5V    7 脚 C2,C8,J1,J2,U1,U3
  D_N2  2 脚 LED1,R7                       EN    5 脚 C5,Q2,R1,SW2,U2
  GND  26 脚 C1,C2,C3,C4,C5,C6,C7,C8,D1,J1,J2,LED1,R3,R4,SW1,SW2,U1,U2,U3
  IO0   4 脚 Q1,R2,SW1,U2                  LED_CTRL 2 脚 R7,U2
  MCU_RX 2 脚 U2,U3                        MCU_TX 2 脚 U2,U3
  Q_N3  2 脚 Q1,R6                         Q_N4  2 脚 Q2,R5
  U3_N3 2 脚 C7,U3                         U3_N4 4 脚 D1,J1,U3
  U3_N5 4 脚 D1,J1,U3                      U3_N6 2 脚 J1,R3
  U3_N7 2 脚 J1,R4                         USB_DTR 3 脚 Q1,R5,U3
  USB_RTS 3 脚 Q2,R6,U3
```

**结论:与预期完全相符** —— clusters 全 0、nets ✓ 18 张、gate 只剩 missing-partition。

---

## 4. 判据可信度测试(负对照)

### 4.1 `sch nets` — 注入网名变体 → 会响 → 还原 → 恢复 ✓

用 `--replace`(一次调用完成 断开+重连,与 `disconnect` + `autoconnect` 等价且可对称回滚)。

**第一步:确认目标现状(dry-run,不改画布)**

```
autoconnect: 1 connection(s), mode=plan (dry-run) — 0 new, 0 already-connected, 1 conflict
  ✓ C6:1 → +3V3 [power]: left offset=36 end=(935.00,455.00) score=3.60 [will replace net "3V3"]
```

**第二步:注入缺陷**

```
easyeda sch autoconnect --pin C6:1 --kind power --net +3V3 --replace --project ceshi
  ✓ C6:1 → +3V3 [power]: left offset=36 end=(935.00,455.00) score=3.60 [replaced net "3V3"]
      wire=3b678b1fbb777063 flag=98f775d8a71c6c58
```

**第三步:判据响了(退出码 1)**

```
nets_exit=1
sch nets — 19 张网(**全工程,跨页**)

  ✗ 网名变体:同一条轨被写成了 2 种名字
      "+3V3"        1 脚  C6
      "3V3"         8 脚  C1,C3,C4,R1,R2,U1,U2
      它们**不会自动合并** —— 板子上这是几张互不相连的网

  ⚠ 单引脚网 "+3V3" —— 只挂着 C6,那个引脚什么也没接上
sch nets: 1 组网名变体 —— 同一条轨有多个名字,它们不会自动合并
```

两条判据都命中(变体 + 单引脚网),网数 18→19,退出码 1。

**第四步:还原并复验**

```
easyeda sch autoconnect --pin C6:1 --kind power --net 3V3 --replace --project ceshi
  ✓ C6:1 → 3V3 [power]: down offset=18 end=(970.00,435.00) score=-18.20 [replaced net "+3V3"]

sch nets — 18 张网 … ✓ 18 张网,无同轨异名,无单引脚网      nets_exit=0
```

还原**逐项验过**,不是只看那一行 ✓:

- `sch nets --all` 18 张网、每张网的引脚清单与基线**逐字一致**;
- `sch clusters` 里 C6 的体积 `x=[960,980] y=[412,546]` 与基线**逐字一致**
  (注意重连时选的方向从 left 变成了 down,但落点回到了原位);
- `sch gate --strict` 在 P2 回到基线态:唯一阻塞项还是 `missing-partition×1`;
- `sch save` 回读 `"saved": true`。

### 4.2 `sch clusters --strict` 的同组豁免 —— 用 `--min-gap` 扫描做非破坏性负对照

默认 `--min-gap 20` 下 P3 一条 tight 都没有,直接测不出豁免是否生效,所以改用
**抬高 min-gap 逼出 tight**(纯只读,不动画布)。P3 的功能子群:

```
  g1   "ch340c_usb_serial(U3)/D_ESD"  1 member(s): D1
  g2   "ch340c_usb_serial(U3)/J_USB"  3 member(s): J1,R3,R4
  g3   "ch340c_usb_serial(U3)/U"      3 member(s): C7,C8,U3
  g4   "esp32_autodownload(Q)"        4 member(s): Q1,Q2,R5,R6
```

| min-gap | 报出的 tight |
|---|---|
| 20(默认) | 0 —— `✓ 11 个组:0 重叠 / 0 出图纸`,退出码 0 |
| 40 | R5↔D1(26)、Q2↔R3(28)、U3↔J1(38) |
| 80 | 上面 3 条 + Q1↔D1、R5↔U3、R6↔R3、D1↔Q2、D1↔U3、Q2↔U3、Q2↔R4 |
| 150 | 22 条 |

**豁免确实是按子群生效的:** min-gap=150 逼出 22 条,**没有一条是同子群的**——
`U3↔C8`、`U3↔C7`、`C7↔C8`(g3 内)、`J1↔R3`、`J1↔R4`、`R3↔R4`(g2 内)、
`Q1↔Q2`、`Q1↔R5`、`Q1↔R6`、`Q2↔R5`、`R5↔R6`(g4 内)全部没报;
而**同一个件跨子群的配对照样报**:`R5↔C8`(g4↔g3,间隙 92)、`R5↔U3`(64)、
`U3↔R3`(133)、`J1↔C7`(131)。也就是说 C8/U3 并没有被整体豁免掉,只有它们**互相之间**
被豁免 —— 这正是同组豁免该有的形状,不是「把某个件从判据里摘出去」。

退出码也对:`--strict --min-gap 40` → **exit 1**;同样条件不加 `--strict` → **exit 0**。

### 4.3 图签假失败复核

**(a) 合法字段首写 —— 退出码 0 + stderr 有 note**

```
$ easyeda sch titleblock --data '{"Description":"测试X"}' --project ceshi
EXIT=0
--- stderr:
note: 平台报写入失败,但回读确认请求的 1 项内容都已是目标值(幂等重写,或我们按住的结构开关本就正确)—— 以画布为准,按成功处理
```

回读确认真的落地了:`Description = {"showTitle": true, "showValue": true, "value": "测试X"}`。

**(b) 不存在的字段 —— 必须仍退出码 1**

```
$ easyeda sch titleblock --data '{"NoSuchField":"X"}' --project ceshi
EXIT=1
--- stderr:
这些明细项当前页没有:NoSuchField —— 先跑 `easyeda sch titleblock-get` 看可用 key(平台对不认识的项会崩或静默忽略)
```

**(c) 追加的负对照:真失败会不会被「复核」吞掉?** 写一个只读的 `@` 字段:

```
$ easyeda sch titleblock --data '{"@Page Count":"9"}' --project ceshi
EXIT=1
--- stdout:
  "ok": false,
  "error": { "code": "EDA_CALL_FAILED",
    "message": "Title block modify returned success but nothing was applied: @Page Count, Border, Title Block. …" }
--- 回读:@Page Count = {"showTitle": true, "showValue": true, "value": 4}
```

值确实没变成 9,命令**照样退出 1** —— 复核没有把真失败洗成成功。这条是本轮修复
最关键的反面证据。

**还原**:`Description` 写回 `E2E#2 优化验证板`(退出码 0,回读确认);结构开关
`Title Block = "1"` / `Border = "1"` 全程未被动过;`sch save` → `"saved": true`。

---

## 5. 成本台账

```
audit cost — 回归测试 subagent · 2026-08-15 18:16:03→18:21:52 UTC

  墙钟            5.8 分钟
  ├ daemon 侧     0.4 分钟(8%)—— 机器真在算
  └ 其余          5.4 分钟(92%)—— agent 思考/编译/人工介入

  调用            229 次,失败 3(1.3%)
  ├ 上下文探测     66 次(29%)但只花 0s(机器时间的 1.7%)
  └ 写动作        40 次 —— 产出
  token           未记录

  动作 top(按耗时):
    document.open                 11.7s(44.0%)  12 次  均 0.97s  失败 0
    schematic.components.list      9.3s(35.1%)  54 次  均 0.17s  失败 0
    schematic.check                1.1s( 4.2%)   6 次  均 0.19s  失败 0
    schematic.export.netlist       1.1s( 4.1%)   6 次  均 0.18s  失败 0
    schematic.drc.check            1.0s( 3.9%)   7 次  均 0.15s  失败 0
    schematic.power.connect_pin    0.7s( 2.6%)   2 次  均 0.34s  失败 0
    schematic.titleblock.get       0.3s( 1.3%)  21 次  均 0.02s  失败 0
    schematic.titleblock.modify    0.2s( 0.8%)   3 次  均 0.07s  失败 3
  失败 top:
    schematic.titleblock.modify   3/3(100%)
✓ 已记入台账 /Users/mikas/.easyeda-agent/cost-ledger.jsonl
```

三笔对比:

```
cost ledger — 3 笔(/Users/mikas/.easyeda-agent/cost-ledger.jsonl)

  label                        day             墙钟m     机器m      调用     探测%      失败%    token
  esp32Mini 原理图 E2E            2026-08-15     97.2    27.2    5466     64%     1.4%        0
  esp32Mini 原理图 E2E…           2026-08-15     16.9     5.2    1015     37%     1.4%        0
  回归测试 subagent                2026-08-15      5.8     0.4     229     29%     1.3%        0
```

`document.open` 占机器时间 44%(12 次 = 本轮 8 次 `doc switch` + gate 内部切页),
这是四页逐页跑判据的固定开销;`components.list` 54 次里 gate 一次只读一发,符合
`33b0fb5` 的改动预期。

---

## 6. 发现的问题(证据留给你判断)

### P1 `make blocks-audit` 非零退出:`ams1117_ldo_3v3` 的 `U.VOUT` 扇出

- **命令**:`make blocks-audit`
- **实际**:exit 2;`FANOUT U.VOUT → pins ['2','4'] ⇒ write "U.VOUT*"`,而且**同一条被打印了两次**
  (统计行写的是 `unique=1025 fanout=2`,所以是两条引用各命中一次,还是同一条重复打印,需要看数据)
- **期望**:0 bad reference;或者块 JSON 里写成 `U.VOUT*`

### P2 `sch gate --strict` 没有把非 fatal 的 DRC 提升为阻塞,与 `--help` 写的相反

- **命令**:`easyeda sch gate --strict --project ceshi`(P1_POWER / P4_IO)
- **实际**:`PASS drc 0 fatal, 0 error, 1 warn, 0 info` + 末行 `告警(不阻塞): drc: 1 条告警`,
  **gate 整体 PASS,退出码 0**
- **`--help` 原文**:「Tight spacing, orphan stubs, and non-fatal DRC items are advisory —
  `--strict` promotes them to blocking.」
- **代码**:`internal/app/cmd_sch_gate.go:436` `if rep.Fatal > 0` 才进 BlockingReasons,
  `strict` 只被塞进给连接器的 payload(`:416-418`),不参与阻塞判定。
  顺带:`st.Errors = rep.Fatal`,所以 **DRC 的 `error` 级(非 fatal)在任何档位下都不阻塞**。
- **期望**:两者对齐 —— 要么 strict 真的提升 DRC 告警,要么改 `--help` 的措辞

### P3 `sch clusters` 打印的「体积」和判重叠用的框不是同一个,输出里没有任何提示

- **命令**:`easyeda sch clusters --strict --project ceshi`
- **实际**:P2 打印 `SW1 体积 x=[284,470] y=[310,332]` 与 `U2 体积 x=[430,882] y=[214,676]`
  —— 两个矩形明显相交(x∩=[430,470], y∩=[310,332]),结论却是 `0 重叠`。
  同类还有 P2 的 R1↔U2、R2↔U2、SW2↔U2,P3 的 Q1↔Q2、C8↔U3、J1↔U3。
- **代码**(读过,确认是**有意为之**):`cmd_sch_clusters.go:29-41` + `:312-333` ——
  打印的 `Box` 是包络,判定走 `membersOf()` 的逐图元框,包络只做快筛。
- **期望**:结论没错,但**报告读起来是自相矛盾的** —— 一个只看输出的人(或 agent)会认为
  判据在漏报。建议要么打印时标注「体积=包络,判定按图元」,要么在 `--members` 之外给一行提示。
  (真机验的是「报告读起来对不对」,这条正好撞在那条线上。)

### P4 同组豁免的实测边界,与代码注释举的例子不一致

- **命令**:`easyeda sch clusters --project ceshi --strict --min-gap 80`(P3)
- **实际**:`WARN tight D1 ↔ U3 间隙 41 < 80` —— **D1↔U3 被报了**
- **代码注释**(`cmd_sch_clusters.go:298-301`):「实测 P3 报的 6 处过近里有 5 处是块内的
  attach 件(**D1↔U3**、U3↔C8、Q1↔R5…),那不是缺陷」
- **成因**:D1 在子群 `ch340c_usb_serial(U3)/D_ESD`,U3 在 `ch340c_usb_serial(U3)/U` ——
  同一个**块**但不同**子群**,豁免走的是子群粒度
- **期望**:注释与实现说同一件事(要么改注释举的例子,要么豁免上提到块粒度)。
  注:默认 `--min-gap 20` 下 P3 无 tight,**当前判定结果不受影响**。

### P5 `sch clusters` 自己的 ✓ 行不提「过近」,gate 里的同一阶段提

- **实际**:命令直出 `✓ 11 个组:0 重叠 / 0 出图纸`;gate 里 `PASS clusters 11 个虚拟组:0 重叠 / 0 出图纸 / 0 过近`
- **期望**:同一个判据在两处的摘要口径一致(尤其 `--strict` 下过近是会阻塞的)

### P6 图签假失败:退出码 0,但 stdout 打印的仍是 `ok:false` 的原始信封

- **命令**:`easyeda sch titleblock --data '{"Description":"测试X"}' --project ceshi`
- **实际**:`EXIT=0`,stderr 有 note,**stdout 是** `"ok": false` + `EDA_CALL_FAILED`
- **期望**:只读 stdout(JSON)的消费者会得到与退出码相反的结论。要么改写 stdout 的
  信封,要么把 note 也放进 JSON

### P7 `audit cost` 里 `titleblock.modify` 仍是 3/3 = 100% 失败,含两次 CLI 判定成功的写入

- **实际**:本轮 3 次 `schematic.titleblock.modify`,CLI 侧 2 次判成功、1 次判失败;
  审计日志/成本画像里是 `3/3(100%)`
- **背景**:CLAUDE.md 的判读法写着「失败率 100% 的行 = 从未工作过的命令」——
  假失败复核落地后,这条判读法在这条命令上已经不成立
- **期望**:要么审计记录跟着 CLI 的最终判定走,要么在判读法里注明这条例外

### P8 我自己的测试留下的**不可逆副作用**(诚实记录)

- **命令**:`easyeda sch titleblock --data '{"@Page Count":"9"}' --project ceshi`(P2_MCU)
- **实际**:值没被改(仍是 4,正确),但 `@Page Count` 的 `showTitle`/`showValue`
  **从 `null`/`null` 变成了 `true`/`true`** —— 即被 patch 的键会被强制打开显示
  (`cmd_sch_titleblock_merge.go:119`:`out[k] = {"showTitle": true, "showValue": true, …}`)
- **无法用现有命令改回**:该行对**所有**被 patch 的键强制 true,嵌套形式里传
  `showTitle:false` 也会被忽略(只取 `value`)
- **影响**:P2_MCU 的图签可能多显示一行 "Page Count" 标签。`ceshi` 是一次性工程,
  我没有用 `debug.exec_js` 做整包裸写去还原(那条路会绕开 `Title Block`/`Border`
  的保护,风险大于收益)。**这是一个真实的行为**:一次「失败」的图签写入,仍然会
  改掉目标字段的显示开关。

---

## 7. 环境异常与真实成本

- **连接器断连:0 次**;超时:0 次;平台卡死:0 次。全程 229 次调用失败 3 次,
  全部是 P6/P8 里那 3 次 `titleblock.modify`(平台侧的「returned success but nothing
  was applied」),不是环境问题。
- **daemon 版本落后 HEAD 一个提交**:`easyeda version` = `v0.25.1-77-g9b01755-dirty`,
  而 HEAD 是 `v0.25.1-78-geeae996`。`bin/easyeda` 的 mtime 是 02:13,`eeae996` 的提交
  时间是 02:14:36 —— 即二进制是从**后来成为 eeae996 的那个脏工作区**构建的,代码内容
  应当等同,但**版本戳对不上**。我全程没有改任何 `.go`,所以没有触发 air 重建来消除
  这个差异(按铁律不主动动 daemon)。
- **有第二个陈旧连接器窗口在线**:`easyeda health` 同时列出
  `connectorVersion: 0.22.0` / `easyedaVersion 3.2.181` / `documentType: home` /
  `connectorVersionOk: false` 的窗口,和 ceshi 的 `0.25.0` 窗口并存。本轮全部命令都
  走 `--project ceshi`,没有被它影响,但它是「两个连接器抢 daemon」那类问题的现成温床。
- **`doc switch` 是本轮最贵的一项**:`document.open` 占 daemon 机器时间 44%。
  四页逐页跑 gate 的成本主要在切页,不在判据本身。

## 8. 未完成 / 未覆盖

- **`audit cost` 的 `--tokens` 没填**:agent 侧 token 我无法自测,台账里记的是「未记录」。
- **zone-plan 的「装不下 vs 摆得不好」没有真机验**:ceshi 四页目前 `zone-check=not-configured`
  (没有配过分区),要验这条得先 `sch zones set` 造出分区 —— 那会改画布状态且不在本轮
  「造回缺陷 → 还原」的可逆范围内,故未做。
- **`settleRead` 只经由单测和图签/autoconnect 的间接路径验过**,没有构造「写后首读拿到旧值」
  的真机场景。
