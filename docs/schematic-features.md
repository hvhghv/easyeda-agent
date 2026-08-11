# 原理图功能支持全景(CLI 视角)

`easyeda sch` 域的**当前能力清单 + 待支持路线**。定位:让 AI agent(或人)可以完全通过
typed CLI 操作嘉立创EDA专业版的原理图——每个动作可观测、可校验、可回滚推理,不依赖 GUI 手工。

> 动作目录的机器可读真值是 `make actions` / `easyeda actions`;本文是**人读的功能地图**,
> 按「AI 操作原理图需要什么」组织。设计流程(何时用哪个命令)见
> [`skills/easyeda-agent/references/design-flow.md`](../skills/easyeda-agent/references/design-flow.md) S0–S6。

## 一、已支持(按功能域)

### 1. 器件与库

| 能力 | 命令 | 说明 |
|---|---|---|
| 库搜索 | `sch place` 配套(`schematic.library.search`) | 立创/LCSC 库自由搜索,返回 libraryUuid+deviceUuid |
| 放置真实器件 | `sch place` | 按 uuid 放置,`--designator` 原子分配位号(免 place→list→modify 往返) |
| 换型号 | `sch replace` | 换库器件,pinDiff 非空提示需重接线 |
| 符号/封装重绑 | `sch rebind-symbol` / `rebind-footprint` | 五步 rebind(modify→delete→create→restore) |
| C 号解析 | `sch resolve-lcsc` | 已放置器件 → 真实 LCSC C 号(确定性,绝不模糊兜底);dry-run 默认 |
| 属性修改 | `sch modify` | 位置/位号/BOM 标志/自定义属性;**merge 语义**:只 patch 顶层字段(如 supplierId)时自动保留全部 otherProperty 并回报 `propertiesPreserved`(#175 修复,曾静默清空) |
| 删除 | `sch delete` / `sch prim-delete` / `sch clear` | 按 id 删器件 / 删任意图元(含文本、图形)/ 整页清空(dry-run 可数) |

### 2. 连线与网络

| 能力 | 命令 | 说明 |
|---|---|---|
| 画线 | `sch wire` | 折线;netflag 必须经真 wire 连(重叠坐标不算连接,平台规则) |
| 引脚出线+标志 | `sch connect` | pin → 短 stub → netflag/netport,显式方向/offset;自动补偿平台「旋转存储取负」的坑 |
| 智能连接 | `sch autoconnect` | **打分器**自选方向/offset:碰撞/穿件/图签/fanout 通道全几何成本,含 **netport 竖排折叠惩罚**(密集引脚列不再把标签翻竖);幂等(已连跳过),`--replace` 换网 |
| 断开 | `sch disconnect` | connect 的逆操作:stub+flag 成对删(免孤儿桩) |
| NC 标记 | `sch no-connect` | 引脚非连接标识(check 的 floating-pin 出的清单可直接喂) |
| 网表 | `sch netlist` / `sch read` | 导出网表 / 一次调用语义快照(器件+网络+检查) |

### 3. 布局与整理

| 能力 | 命令 | 说明 |
|---|---|---|
| 模块感知自动布局 | `sch autolayout` | 双引擎:`template`(spec 驱动,核心放分区中心+外围环绕,确定性,布线前用)/ `official`(平台 @beta 兜底,破坏性,`--rewire` 网表重建) |
| 空隙打包 | `sch autoplace-free` | 无分区场景往空白处塞件 |
| 对齐/等距 | `sch align` / `sch distribute` | 按渲染 bbox 对齐(left/right/top/…)/ 单轴等距摊开;默认 dry-run |
| 刚体平移 | `sch group-move` | 器件+桩线+flag 一起搬(**无状态**虚拟组:每次传全 ids;持久编组见路线 §1) |
| 布局硬门 | `sch layout-lint` | 真实渲染 bbox 查重叠(ERROR 非零退出)/紧间距/off-grid/分区违规 |
| 布局质量分 | `sch layout-score` | **五维诊断**:标签折叠 / 标签反向(背离核心)/ 外围贴芯片距离 / 长链散乱 / 框贴合——逐项归因**带可执行 fix 命令**(AI 照抄即修);诊断视角,门仍是 layout-lint+check |

### 4. 页面组织与分区(可读性三件套)

| 能力 | 命令 | 说明 |
|---|---|---|
| 分页管理 | `sch pages` / `page-new` / `page-rename` / `page-delete` | 页集合要 reconcile 到模块计划(改名成功能名/补页/删多余空页) |
| 分区认领 | `sch zones set/status/clear` | 模块→分区格的持久化认领,layout-lint 消费出 zone-violation |
| 数据驱动分区规划 | `sch zone-plan` | 按模块真实 bbox 切整纸分区,校验六项(越界/重叠/**压图签**/模块出区/标签碰撞/**贴纸边**)全 0 才许画;抬升与校验共用同一安全余量常量(治「按 A 抬按 B 校」假绿) |
| 画分区框 | `sch zone-draw` | 虚线框+区名(`--mode partition` 整纸版式,框贴合模块内容不拉满页,给图签留缺口) |
| 电路说明 | `sch note` / `sch text-list` | 每模块 1~3 行说明(作用+关键参数);text-list 枚举全部文本 |
| 图纸/图签 | `sch sheet-geometry` / `titleblock` / `titleblock-get` | 图纸边界+图签 keep-out(A4 校准比例,provenance 标注)/ 明细表读写 |

### 5. 校验与门禁

| 能力 | 命令 | 说明 |
|---|---|---|
| 逐项设计检查 | `sch check` | 平台 DRC 只给聚合数,check 从图元重建逐项 finding:悬空脚/几何-网表错配/导线交叉/压引脚/零长线/悬空线/重合标志/压图签/标志互压/**多器件页未分区**(missing-partition,铁律#15 机械兜底)/**netport 竖排折叠**(folded-net-label) |
| 桥接检测 | `sch bridge-check` | 树粒度:共线合并短路 BRIDGE / 孤儿桩 ORPHAN(单线视角看不全的盲区) |
| 官方 DRC | `sch drc` | SDK 门(可能仅聚合) |
| S5 一条龙门 | `sch gate` | 固定顺序 layout-lint→check→bridge-check→drc 出一张报告;`--strict` 全部 WARN 也拦;`verdict=blocked`=检查器没跑成≠板子有问题 |

### 6. 电路块库(拓扑复用)

| 能力 | 命令 | 说明 |
|---|---|---|
| 浏览/查找 | `easyeda blocks ls/show/search` | 离线,20 块/11 类目;块携带 internal_nets/ports/parts/pcb_layout/silk 多维知识 |
| 一键实例化 | `sch block-apply` | 放件+内部连线+端口绑定+落位避让+失败补偿回滚;带 `schematic_layout` 模板的块按人审过的几何落 |
| 模板反推 | `sch extract-layout` | 真板摆好的实例 → 反向导出块模板 JSON(「摆好一次→固化」数据管线) |

### 7. 导出与视图

| 能力 | 命令 | 说明 |
|---|---|---|
| 页面导图 | `sch export-image` | SVG/PNG/PDF,选区或整页,不依赖前台视口(残留进度条已在导出后主动销毁) |
| 视口截图 | `sch snapshot` | 需前台,stale 检测 |
| BOM/网表 | `sch export`(bom/netlist) | BOM 自动补 LCSC C 号(`bom-enrich.py`) |

## 附:易混命令辨析(AI 选型速查)

同族命令的边界与参数差异——**每条都来自 agent 真实误用**(2026-08-11 一次会话踩 5 次),
读这张表可以一次选对:

| 你想做 | 用这个 | 别用/别混 | 参数注意 |
|---|---|---|---|
| 给引脚接网络标志(常规) | `sch autoconnect --pin R1:1 --kind netport --net EN` | — | `--pin` 是 `位号:脚号` 一参式 |
| 给引脚接标志(指定方向) | `sch connect --x <px> --y <py> --direction right` | ⚠ connect **没有** `--pin`,只认坐标 | 坐标从 list/check 的 pinDetails 拿 |
| 断开引脚的 stub+flag | `sch disconnect --pin C4:1` | ⚠ 不是 `--designator C4 --pin 1` 两参式 | 也可 `--flag-id`/`--wire-id` |
| 挪器件/改属性 | `sch modify --id <pid> --patch '{"x":100,"y":200}'` | ⚠ **没有** `--x/--y` flag(place 才有) | patch 是 JSON 对象 |
| 删器件 | `sch prim-delete --ids '["id1","id2"]'` | `sch delete` 是它的器件子集,统一用 prim-delete | ⚠ `--ids` 是 **JSON 数组字符串**,不是 CSV |
| 清整页 | `sch clear` | — | 破坏性,先 dry-run/确认 |
| 列页/切页 | `sch pages` / `sch open` | `doc ls`/`doc switch` 是跨域老入口,功能重叠 | sch 域内优先用 sch 命令 |
| 出图给人看 | `sch export-image` | `snapshot` 是**视口截图**(需前台、会 stale) | export 不依赖前台 |
| 读电路状态 | `sch read`(=list+nets+check 聚合) | 只要器件清单用 `list`;只要检查用 `check` | read 最贵但一次拿全 |
| 分区框(整纸版式) | `zones set` → `zone-plan`(校验)→ `zone-draw --mode partition` | ⚠ 固定九宫格 claim 对宽模组会误报 zone-violation——partition 画完后 `zones clear` | 三段链,顺序固定 |
| 建裸网络标志 | **尽量别用** `sch netflag` | 裸 flag 不经 wire = 假连接(铁律 9) | 用 connect/autoconnect |

**参数风格差异是历史债**(connect 坐标式 / autoconnect 位号式 / modify patch 式 / delete JSON 数组),
统一化在收敛路线上(见下)。

## 二、待支持 / 路线(按 AI 可操作性缺口排序)

### 1. 持久化编组(用户点名;sch 侧对齐 PCB 的 #173)

**为什么**:AI 操作原理图要「把这几件当一个整体一起动」——现在 `group-move` 是无状态的
(每次调用传全 ids,布局动作也不知道组的存在),UI 里用户手工编的组 CLI 读不出,
后续 align/autolayout/单件 modify 会拆散用户确认过的模块。

**平台墙(真机探测 2026-08-11,EasyEDA Pro 3.2.121)**:`eda.*` 无通用编组 API
(`api search group` 仅 pcb_Drc 等长组);`sch_PrimitiveComponent` 实例遍历全部 70 个
方法/属性,**零 group/parent 字段**——UI 原生组对扩展 API 完全不可见。

**方案(与 #173 的 fallback 一致)**:easyeda-agent 自己的 **virtual group**,按
documentUuid 持久化(同 zones claims 的 workflow-state 模式):
- `sch group create --ids …` / `list` / `add` / `remove` / `ungroup`
- `sch group-move --group <id>`(免传全 ids;成员的桩线+flag 自动纳入)
- align / distribute / autolayout / autoplace-free **默认把组当不可拆刚体**,显式解组才拆
- 验收:CLI 建组 → reload → 读回相同成员;布局动作不再单独移动组内成员;组绑定 documentUuid 不跨页串组

### 2. `sch layout-score`(实现中)

五维布局质量打分(折叠/反向/贴芯片/长链/框贴合),逐项归因带可执行 fix 命令——
「识别出来 + AI 知道怎么修」。落地后接 **sch refine**(打分驱动的自动精修环,对齐 `pcb refine`)。

### 3. 布局引擎 v2:外围贴芯片 + 顺信号流

block-apply 无模板时的 fallback 从「per-row 等分栅格」升级为:
**等分只定核心芯片位置;外围件贴自家核心上下排布**,件的轴向顺服务引脚的出线方向
(块 `internal_nets` 可推导「谁服务谁」),相邻件间距 ≥117(两个相向水平 netport 标签的实测最小距)。
ceshi 真机已按此手工验证:标签 10/10 自然水平、框贴合、可读性达标。

### 4. 分区框 content 纳入 note/flag

框几何目前只算器件 bbox——模块的说明文字和引脚 netflag/netport 会落在框外。
计划把「归属该模块的 note + 成员件的 marker」并进 content 再画框。

### 5. zone-draw 的 stale bbox

modify 挪件后**立即** zone-draw 会按旧 bbox 画框(隔几个命令重画就对)。
需要在 zone-draw 取数前强制 fresh 读(schematic 侧的 stale 时序与 PCB reload 同族)。

### 6. netlabel(实线中途标网名)

connect/autoconnect 只有端点挂 netflag/netport 一种表达;同模块内「实线直连 + 线上标网名」
的行业画法(如上拉电阻串进引线)需要 netlabel 创建 API 封装。

---

*本文与 [`docs/FEATURES.md`](./FEATURES.md)(全域 action 清单+roadmap)互补:那边是动作粒度,
这边是「AI 操作原理图」的功能域视角。改动原理图相关命令后请同步本文。*
