# 原理图布局三层体系(设计契约)

用户拆解的最终蓝图(2026-08-12 立项,ultracode 全量实现):

```
纸张 Sheet(A4 可用区 + 图签 keepout)
 └─ 功能区 Zone(功能电路分区,如 POWER)     区间叠加布局 → 适配纸张
     └─ 虚拟组 Group(器件+导线+标志的刚体)   组间叠加布局 → 组成功能区
         └─ Primitive(元器件 / stub 导线 / netflag·netport)
```

**每层两能力**:`tidy`(层内布局计算)+ `move`(刚体移动,上层动带下层)。
移动链:**zone move → 带动区内全部 group → 带动器件+导线+标志**。
不重叠不变量:组内 lint 保证组内无重叠;组 bbox 互不重叠;区 bbox 互不重叠
⇒ 全局无重叠(逐层局部保证推出全局)。

## 数据模型(全部已存在,零新存储)

| 层 | 数据源 | 说明 |
|---|---|---|
| Zone | `sch zones` claims(workflow state,name→designators,按 documentUuid) | 分区框/区名由 `zone-draw` 派生(SchZoneFrameIdsByPage 记录框图元 id) |
| Group | `workflow.State.GroupsByPage`(id/name/members 位号) | `expandSchGroupForMove` 展开附着物(桩线+旗,树语义) |
| Primitive | components.list / exec_js wire getAll | pin 实测,禁假设 |

Zone⊇Group:组的全部成员被某 zone 认领 ⇒ 组属于该区;区内未入组的认领件=散件
(移动时按"单件虚拟组"展开附着物)。跨区组 = 配置错误,报错拒绝。

## 命令契约(v1)

### 1. `sch group tidy --group <id|name> [--pattern auto] [--dry-run] [--apply]`

组内布局计算。v1 patterns:

- **`power-updown`**(双电源旗无源件):器件竖放、上电源旗/下 GND 旗、**文字朝外**;
  横排等距(默认间距 50,可 `--spacing`);IC(若组内有)为锚不动,无 IC 时以组
  bbox 中心为锚。
- **`signal-row`**(带信号 netport 的件):保持横放,信号流左入右出,netport 水平。
- **`auto`**(默认):逐件判型——每 pin 的目标旗从**现有连接**读(net + 旗类型);
  双{power,gnd}旗 → power-updown 处理;含 netport → signal-row 处理;IC → 锚。

**执行铁则**(全部已实战校准,违反必返工):
1. **pin 实测**:任何 rot 之后必须 fresh 重读 pin 实位再连(同规格不同库件符号存在
   镜像:cl05b104 需 rot90、grm21/cl21 需 rot270 才 pin1 朝上——不能按位号/规格猜,
   **按"哪个 pin 在上"实测判**);
2. **stale 防线**:mutation(modify/rot/disconnect)后读取要 settle(double-read
   一致或 ≥350ms 间隔),否则 connect 吃旧 pin 位 → 两 stub 同点起步被平台共线合并
   成贯穿线 = **真短路**(实测);高危步直接用已实测的显式 `--x/--y`;
3. **文字朝外 rotation(真机校准 2026-08-12)**:connect 显式传
   `--rotation`——power up=**0**(文字上)/ power down=180 / gnd down=**0**
   (文字下)/ gnd up=180。做成表 `tidyLabelRotation(kind, direction)`,勿散写;
4. **netport 永不竖放**(长条标竖排=折叠);
5. 收尾自检:`layout-lint`(0 overlap)+ `bridge-check`(0 bridge)不过即
   **逐步回滚**(记录每步前几何,失败恢复),不许半成品落地。

### 2. `sch zone move --zone <name> --dx --dy [--redraw-frame]`

功能区整体刚移。展开集:
- 区内每个 group 的完整 move 集(`expandSchGroupForMove`,组去重);
- 散件:未入组的认领件,按临时单件组展开附着物;
- 区内 note 文本(text 图元中心落在区 bbox 内、且 id 不在 zone-draw 的框记录里);
  文本无 modify-in-place 时 delete+recreate(内容/字号保留);
- **分区框图元不搬**:move 完成后默认自动重画(`--redraw-frame`,内部走
  zone-plan 校验六项 0 → zone-draw;`--redraw-frame=false` 跳过并提示)。
完整性:复用 group-move 的预检语义(残骸 graze 拒绝、终止于异脚的树按共享警告);
目的地净空由调用方保证(压他区 = layout-lint 会红,move 前 dry 检查目标 bbox 与
其它区 bbox 相交则警告)。

### 3. `sch zone tidy --zone <name> [--dry-run] [--apply]`

组间叠加布局(区内排布):
- 锚组:含最大 IC 的组(或最大 bbox 组)定区内锚位(保持不动或居中);
- 其余组按 bbox 当刚体**上下堆叠/行排**(用户点名上下布局优先):水平相邻组
  bbox 间距 ≥ **117**(两个相向水平 netport 标签实测最小距),垂直间距 ≥ 40;
- 求解顺序:大组先占位,小组填空;所有组 bbox 落在区带内(区带=zone-plan 的
  cell 或现有框内);无解(区带装不下)→ 报告需要的最小区尺寸,不硬塞;
- 每组的移动走 `expandSchGroupForMove` 全集刚移(等价多次 group-move);
- 收尾:layout-lint + bridge-check 自检,红即回滚。

## 层间联动与全局收敛(v1 手动编排,v2 一键)

v1:AI 按 `group tidy(逐组)→ zone tidy(逐区)→ zone move(区间调整)→
zone-plan/draw(框)` 编排;v2 目标:`sch autolayout --engine hierarchy` 一键
(读 S0 spec 的 modules → 建组 → 三层求解),对齐用户蓝图"最终去适配纸张信息的,
应该是功能区布局调整"。

## 实现分工(并行 worktree)

| Agent | 文件(新建,不碰他人) | 构造函数(主会话统一注册进 cmd_sch.go) |
|---|---|---|
| A | `internal/app/cmd_sch_group_tidy.go` + `_test.go` | `newSchGroupTidyCommand(...)` |
| B | `internal/app/cmd_sch_zone_move.go` + `_test.go` | `newSchZoneMoveCommand(...)` |
| C | `internal/app/cmd_sch_zone_tidy.go` + `_test.go` | `newSchZoneTidyCommand(...)` |

共享依赖(只读复用,不改):`cmd_sch_group.go` 的 expandSchGroupForMove /
fetchSchWirePolylinesStable / groupExpandInput 家族;`cmd_sch_zones*.go` 的
claims 读取;`cmd_sch_zone_plan.go` 的 zone 几何。若需小 helper 导出,各自文件内
私有实现,不改共享文件(合并时主会话去重)。
