# esp32Mini E2E 原理图 —— 结果报告与缺陷交接

**日期**：2026-08-19 · **工程**：`ceshi`（一次性测试工程）· **连接器**：1.0.2 · **CLI**：v1.0.1-8-gd9039ec
**输入**：`esp32MiniRequire.md`（客户口吻原始需求，无 BOM/UUID/网表）
**范围**：只做原理图 S0–S6，**未做 PCB**（用户明确不做）
**交接目的**：原理图电路本身已达标并落盘；**`sch note --zone` 的落点行为不符合设计**（说明未落进分区框的说明带），本报告给出取证、根因、复现与修复方向，交由下一个 agent 继续。

---

## 一、原理图交付结果（电路侧：达标）

**3 页 · 29 件 · 20 张网 · 已存盘**

### 需求逐条落实

| 需求 | 实现 | 机械证据 |
|---|---|---|
| 5V 供电端子 | J2 KF301-5.0-2P → `+5V` | `+5V` deg=8 含 `J2.1` |
| 插 USB 也能供电（共用一路 5V） | J1 VBUS 并入同一 `+5V` | `+5V` 含 `J1.A4B9` `J1.B4A9` |
| 降压 5V→3V3 | SY8089 同步 Buck 2A（用户拍板，替 LDO 免 0.85W 发热） | `+3V3` deg=9；FB 分压 R2 45.3k / R1 10k |
| CH340 USB 转串口烧录 | U2 CH340C + 双取向 USB-C | `MCU_TX`=U2.3+U3.37、`MCU_RX`=U2.2+U3.36（交叉正确）；DP/DM 各并 A/B 两侧 |
| BOOT / RESET 按键 | SW2→IO0、SW3→EN | `MCU_IO0` deg=4、`MCU_EN` deg=5 |
| 点灯 LED | LED1 + R9 1k ← U3.IO2 | `LED_CTRL` deg=2 |
| 自动下载（加分） | Q1/Q2 双三极管 DTR/RTS 交叉 | `USB_DTR`/`USB_RTS` deg=3 |
| 4 层叠层 / 丝印 / M3 四角孔 | 写进 S0 spec，属 PCB 阶段（本次不做） | `.easyeda/s0-ceshi.json` |

### 页与模块

| 页 | UUID | 模块 | 件数 |
|---|---|---|---|
| P1_POWER | `e5742678b6a33644` | POWER_IN（J2）、sy8089_buck_3v3(C1) | 8 |
| P2_MCU_IO | `9f0bc0139a87cf86` | esp32s3_wroom1_module(C6)、tactile_boot_reset(SW2)、led_indicator_gpio(LED1) | 10 |
| P3_USB_DL | `6f9db991b83c5655` | ch340c_usb_serial(C4) 的 D_ESD / J_USB / U 三子群、esp32_autodownload(Q1) | 11 |

### 校验状态（三页一致）

- **PASS** `layout-lint`：0 重叠 / 0 引脚重合 / 0 出图纸 / 0 off-grid
- **PASS** `clusters`：0 组间重叠 / 0 出图纸 / 0 过近
- **PASS** `bridge-check`：0 短路 0 孤儿（P1 19 / P2 27 / P3 44 条线树）
- **PASS** `sch nets --strict`（全工程跨页）：20 张网，无同轨异名、无单引脚网
- **PASS** `reconcile`：5 个块逐条对上活体网表（SY8089 因组溯源丢失改走网表人工对账，逐脚命中）
- **遗留** `missing-titleblock`：图签写入按 SKILL 铁律禁用（写路径损毁 sheet 引用致重启丢图框），如实挂账
- DRC：0 fatal / 0 error / 1 warn（平台不给逐条明细，已知限制）

### 用户决策与代价（已执行）

1. **5V 直连共网**（无 OR 二极管）：端子外供电时勿同时插 USB —— 已写进 P1 电路说明。
2. **32 个 GPIO 全部打 NC**：`floating-pin` WARN 已清零。代价：本板无对外 IO 引出，「提供必要接口」仅由 USB-C + 5V 端子满足；将来接外设需先 `easyeda sch no-connect --designator U3 --pin <n> --clear`。
3. **分页 2 页 → 3 页**：初选 2 页被机械判据推翻（P1 放电源+USB 两块时 `zone-arrange` 连续 blocked，工具两次给出「拆页」为唯一出路）。

---

## 二、待修主问题：`sch note --zone` 说明没落进分区框

### 期望标准（用户给的目标图）

每个功能子群一个**紧框**；**区名在框左上角、电路说明在框左下角的「说明带」，两者都在框内**；框之间不重叠，`partitionOverlap = 0 / titleBlockHits = 0 / sheetOverflow = 0` 三态 pass。
SKILL 也是这么写的：「`sch note --zone <模块>` 会直接落进说明带，**别手填坐标**」。

### 实测结果：7 条说明 **0 条**落在说明带里

| 页 | note | 实际坐标 | 该区说明带（noteBBox） | 判定 |
|---|---|---|---|---|
| P1 | sy8089 说明 `bac9bed3d628d2de` | (250,445) | {226,356}–{671,382} | 框内，但**不在带内** |
| P1 | POWER_IN 说明 `5ca236a056070aaa` | (190,170) | {166,55}–{332,81} | 框内，但**不在带内** |
| P2 | WROOM 说明 `e15bf7d2c48afc38` | (75,575) | {51,228}–{792,254} | **框外**（x=75 < 画框左沿 92） |
| P2 | 按键/LED 说明 `6b05a375a3c6bad7` | (850,250) | {826,198}–{1124,224} | **框外**（y=250 < 画框下沿 328） |
| P3 | 自动下载说明 `65eb4f75c2102078` | (730,500) | — | **框外**（x=730 > 画框右沿 714） |
| P3 | CH340C 说明 `89b827a1396e84e7` | (145,590) | {121,501}–{848,527} | 框内，带外（修复验证时新建） |
| P3 | USB-C 说明 `e48a79c2cac34dfb` | (260,240) | {126,200}–{1003,226} | 手填坐标才勉强就位 |

**当前 P3 处于死锁**：`zone-plan` 报 `partitionOverlap=1` → `zone-draw` 拒绝重画 → 画布上的框仍是**加 note 之前**的旧框。

---

## 三、根因（三条，已逐条取证）

### A. zone 名匹配用「短名」，传注册表全名会静默落空

`internal/app/cmd_sch_note_place.go:290-305`：

```go
if plan, _, zerr := computePartitionPlan(...); zerr == nil {
    for _, p := range plan.Partitions {
        if strInSlice(p.Modules, zoneRef) {   // ← 精确串匹配
```

- 注册表（`sch group list` / `sch zones status`）里块子群叫 **`ch340c_usb_serial(C4)/U`**
- 但 `plan.Partitions[].Modules` 里只有 **`U`**（斜杠后缀）
- 传全名 → 匹配不到 → `zoneRect`/`noteBand` 双双为 `nil` → 直落**整页扫描兜底** → 说明被甩到页角
- **完全静默**：命令返回 `note created ... registered to zone "ch340c_usb_serial(C4)/U"`，看不出区没匹配上

**决定性对照实验**（同一条文字、同一页、同一区）：

| 传入 `--zone` | 落点 | 结果 |
|---|---|---|
| `ch340c_usb_serial(C4)/U`（注册表全名） | **(35,55)** 页面左下角 | 兜底整页扫描 |
| `U`（短名） | **(145,590)** | 命中该区说明带区域 |

### B. 落点求解不把「别的区的矩形」当障碍

用**正确短名** `J_USB` 自动落点 → 落到 **(600,595)**，那是**邻区 D_ESD/U 的框内**（该框 y 501–790）。
后果：J_USB 的区 bbox 被这条 note 拉到 `maxY=649` → 与邻框交叠 → `partitionOverlap=1` → `zone-draw` 拒绝重画 → **死锁，只能靠删 note 解开**。

`planNoteAnchor`（同文件 `:81`）的回退链是「说明带 → 框内其它点 → 区外走廊四向 → 整页扫描」，**每一档的障碍表只有 components + texts，不含其它分区的矩形**，所以求解器认为邻区框内的空白是可用空间。

### C. 说明带自增长反馈环（框每重画一次往下长一截）

说明带是**从区内容 bbox 推出来的**（框底部 26 单位），而 note 落进带里之后**又被计入区内容 bbox**：

- 放 U 说明前：D_ESD/U 框 `minY = 554`
- 放 U 说明（落在旧带 554–580 内）后：同一框 `minY = 501` —— **往下长了 53（≈padding）**，新的说明带也跟着下移到 501–527，于是原来那条 note 又「不在带里」了

这解释了 P1 两条 note「在框内、不在带内」：框在 note 之后被重画，带整体位移了。**只要「note 计入内容 bbox」和「带由内容 bbox 推导」同时成立，这个环就必然存在。**

---

## 四、复现步骤（干净重现，约 2 分钟）

```bash
easyeda health                                   # 确认 ceshi 窗口在线
easyeda doc switch 6f9db991b83c5655 --project ceshi

# 复现 A：传注册表全名 → 落页角
easyeda sch note --zone "ch340c_usb_serial(C4)/U" --text "测试" --project ceshi
easyeda sch text-list --project ceshi            # 看坐标 ≈ (35,55)

# 对照：传短名 → 落进带
easyeda sch note --zone "U" --text "测试2" --project ceshi

# 复现 B/C：连放两条，看 zone-plan 的 partitionOverlap 从 0 变 1
easyeda sch zone-plan --json --project ceshi | python3 -c "import json,sys;print(json.load(sys.stdin)['validation'])"
easyeda sch zone-draw --mode partition --project ceshi    # 会被拒绝重画
```

---

## 五、修复建议（按优先级）

1. **A 必修且最小**：`cmd_sch_note_place.go:292` 的匹配改成「全名 + 斜杠后缀」双向命中；**匹配不到时必须显式警告**（现在静默兜底是最坏的组合 —— 用户以为登记成功了）。可加一句 `warn: 区 "<name>" 不在本页分区计划里，说明改为整页避让落点`。
2. **B**：`planNoteAnchor` 的障碍表加入**其它分区的 bbox**，让回退链永远不会把说明放进别人的框。
3. **C**：切断反馈环 —— 二选一：
   - 计算区内容 bbox 时**排除本区已登记的 note**（推荐，最直接）；或
   - 说明带改成**框外固定预留**而不是从内容 bbox 推导。
4. **回归**：`cmd_sch_note_place_test.go` 补三个用例——全名 zoneRef、邻区避让、放 note 后重算 bbox 不位移。真机验收用本工程三页重跑，判据是 `text-list` 坐标落在 `zone-plan` 的 `noteBBox` 内 **且** 连放 N 条后 `partitionOverlap` 恒为 0。

---

## 六、本轮暴露的其它缺陷（同一场跑出来，建议一并开票）

| # | 缺陷 | 证据 | 严重度 |
|---|---|---|---|
| 1 | **`zone-arrange --apply` 会串网** | P2 跑完 `GND` 整网消失、9 个地脚被灌进 `+3V3`，`LED_CTRL` 塌进 `MCU_IO0`；它自己的断言② 抓到了并拒绝声明成功，但页面已毁，只能删页重建。与 memory 里「删桩线→共线合并」同一条路 | **P0** |
| 2 | 删器件不级联删组注册 | 新落的 R1 被早已不存在的 `led_indicator_gpio(LED1)` 陈旧组吃掉，整个块归组失败 | P1 |
| 3 | 删除 API 撒谎 | block-apply 回滚的 `component.delete` 报 `deleted=false`；`zone-draw` 批量删旧框报 `survived=4`。两处都靠 `prim-delete` **逐个**删兜底（逐个 100% 成功，批量不可靠） | P1 |
| 4 | 组的块溯源无法手工恢复 | `group create` 附不上 blockId，组注册一坏该模块**永久**失去 `reconcile` 机械对账能力（本次 SY8089 即是） | P1 |
| 5 | `sch note` 文本未转义 | 含 `~` 或 `+/-` 的说明让 exec_js 直接挂掉（`debug.exec_js failed`），换中文写法即通过 | P2 |
| 6 | dry-run 与 apply 布局引擎不一致 | CH340C 块 dry-run 显示全 `grid`，实际 apply 走关系式模板（anchor/attach/pair） | P2 |
| 7 | `sch status --all-pages` page drift | 3 页里 2 页「读不到」（exec_js context 不跟切页走），status 诚实降级为 unknown（行为正确，但命令实际不可用） | P2 |

---

## 七、给接手 agent 的注意事项

- **禁用 `sch zone-arrange --apply`**（缺陷 1，会毁页）。挪版面一律用 `sch group-move` / `sch zone move`，它们每次都做网表逐引脚对账。
- **删图元批量不可信**：`prim-delete` 传多个 id 有时静默不删，**逐个删**成功率 100%；删完必回读确认。
- **`sch reconcile` 之后紧跟 `modify` 会失败**（读引脚毒化后续写），中间插一条别的命令即可。
- **连接器会停摆**：报 `connector did not respond` / `exec_js failed` 后，先用轻读（`sch pages`）探活，**并回读确认那条写是否其实已落地**，别盲重试造重复。
- 工程 `ceshi` 是一次性测试工程，可随意清空重建。S0 方案书在 `.easyeda/s0-ceshi.json`（`easyeda spec validate --strict` 通过）。
- 成本画像已记台账：墙钟 81 分钟，daemon 侧 17.5 分钟（22%），耗时头名 `connect_pin` 585s/55.6%（287 次，失败率 6%）。

---
*Generated by easyeda-agent skill · 判据全部来自 `sch gate` / `zone-plan` / `text-list` / `sch read` 实测数据，无截图判定*
