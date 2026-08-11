# PCB 功能支持全景(CLI 视角)

`easyeda pcb` 域(含 `workflow` 阶段门)的**当前能力清单 + 待支持路线**。定位:AI agent
从原理图同步到制造导出,全程 typed CLI 操作,每步可观测、可校验、被阶段门机械把关。

> 动作目录真值:`easyeda actions`;流程编排(P0–P10 何时用哪条)见
> [`design-flow.md`](../../skills/easyeda-agent/references/design-flow.md);
> 设计规范手册(线宽/间距/过孔/铺铜,DRC 报错的 `[规范 §N]` 指向)见
> [`pcb-design-rules.md`](../../skills/easyeda-agent/references/pcb-design-rules.md)。

## 一、已支持(按功能域)

### 1. 板与同步

| 能力 | 命令 | 说明 |
|---|---|---|
| 新建板并绑定 | `pcb new-board` | 从原理图建板+空 PCB 页并绑定(CLI 版「原理图转 PCB」);`--force` 对已绑板是破坏性操作 |
| 网表同步 | `pcb import-changes` | 原理图 → PCB 增量同步(平台对 API 新增器件是 no-op,首次同步前放完整电路) |
| 单件补挂 | `pcb add-component` | 往已有 PCB 加单个器件并连接焊盘网络(绕过失效的增量同步) |
| 文档/视图 | `doc reload` / `pcb snapshot` / `pcb view-mode` 相关 | mutation 后必须 reload 再读(stale 防线);快照带 stale 检测 |

### 2. 布局

| 能力 | 命令 | 说明 |
|---|---|---|
| 板框 | `pcb outline` / `outline-fit` / `outline-round` | 真板框对象(锁定 polyline,DRC 认);贴合器件 / 圆角矩形;`pcb.outline.get` 返回真多边形 |
| 角色感知自动摆放 | `pcb auto-place` / `place-constrained` | 卫星贴其所连芯片侧、2 脚件自动转向、间距规则感知;规划后按 blocking 复算合法化(重叠/短路/出框就地重定位) |
| 分区规划 | `pcb floorplan` / `pcb zones` | S0 spec 驱动的有序带切分(只读)+ 分区认领 |
| 分档确认 | `pcb stage confirm-tier 1-4` / `set-assembly` | 孔→边缘件→主芯片+RF→卫星逐档落盘;装配档案(手焊 40mil/烙铁通道 60mil)持久化进 lint 门 |
| 布局硬门 | `pcb layout-lint --gate` | 重叠/紧间距/可布性(飞线 MST+交叉)/手焊可达性(no-access);唯一的布局门 |
| 布局质量分 | `pcb layout-score` | 九维 0-100+逐器件归因(partition/flow-order/edge-io/protection/tidy/compact/rf/routable/clearance),blocking 一票否决;`--part` 器件聚焦视角;金标准五真板校准(`make layout-calibrate`) |
| 打分驱动精修 | `pcb refine` | 读归因对最弱维做确定性变换,每步复核可回滚;锁定件/已签字档不动 |
| 编组式移动 | `pcb components move` 类 | 无状态刚体移动(持久编组见路线 §1) |

### 3. 布线

| 能力 | 命令 | 说明 |
|---|---|---|
| 短线启发式 | `pcb route-short` | 每网 MST、规则感知线宽(按网络角色给宽)、障碍感知 L 朝向、默认跳电源/地(该铺铜) |
| 关键网先行 | `pcb route-critical` | P7.0 一条命令:电源按层数走 planes/pour → 差分对双源识别成对布线+skew 实测 → 自动 `track-lock` |
| 外部自动布线 | `pcb export-dsn` / `import-autoroute` / `pcb autoroute` | Specctra DSN 往返(带禁布区注入),Freerouting 兜底;稠密板默认交编辑器原生自动布线 |
| 拆线 | `pcb rip-up` | 按网/按范围拆 |
| 锁定 | `pcb track-lock` | 手布关键线锁死,防被自动布线/pour-rebuild 冲掉 |

### 4. 铺铜与平面

| 能力 | 命令 | 说明 |
|---|---|---|
| 铺铜 | `pcb pour` / `pour-fit` / `pour-rebuild` | 规则感知内缩;`pour-fit --replace` 默认清跨层同网 pour(顶/底要显式关) |
| 4 层电源树 | `pcb power-planes` | GND+电源各占专用内平面+每焊盘过孔缝合;GND 内层翻成真 PLANE 的验证配方 |
| 缝合/填充 | `pcb via-stitch` / `pcb fill` | 接地缝合过孔阵 / 实心填充 |
| 禁布区 | `pcb region` / `pcb antenna-keepout` | 禁铺/禁走线区;天线 keepout 按块库声明全层生成 |
| 挖槽 | `pcb slot` | 板内挖空(MULTI 层) |

### 5. 丝印与标注

| 能力 | 命令 | 说明 |
|---|---|---|
| 位号避让重排 | `pcb silk-align` | 位置感知:4 方向打分避开焊盘/器件体/禁区/板框/其它标签;挤死的如实报告 |
| 自由丝印 | `pcb silk-add` / `silk-set` | 板注/极性标记,层/字号/线宽/旋转可配;`--align --ref` 对齐参考 |
| 矢量图形 | `pcb silk-import-svg` | SVG(logo/品牌)转填充丝印图元,dry-run 预览 |

### 6. 叠层、规则与制造

| 能力 | 命令 | 说明 |
|---|---|---|
| 叠层 | `pcb stackup` | 2–32 铜层 + 内层类型(信号↔内电层) |
| 规则 | `pcb drc-rules` / `pcb net-classes` | 读 live DRC 规则全链路遵循;缺失回退 JLCPCB 工艺地板(clamp,绝不低于制造最小值) |
| 检查 | `pcb drc` / `pcb check` | 官方 DRC + 重建的逐项检查(电源未铺铜/线宽不达规范/丝印压焊盘/连接器贴边与插拔通道等,报错带 `[规范 §N]` 指向手册章节) |
| 阶段门禁 | `workflow status/advance` | 布线前 `outline_confirmed`+`pre_route_passed`、布完 `post_route_checked` 机械强制(daemon 派发层拦截,raw 调用绕不过);确认绑定文档指纹,门外改动自动失效 |

## 二、待支持 / 路线

1. **持久化编组**(#173,用户点名):平台无编组 API(UI 组对扩展不可见)→ virtual group 按 documentUuid 持久化,`pcb group list/create/add/remove/ungroup/move`;布局动作默认把组当不可拆刚体。与原理图侧同方案同期实现。
2. **接插件逐脚丝印 `pcb silk-pins`**(P9):端子/排针按脚自动标网络简称,辅助接线;8 条设计标准即校验门(几何+语义双查)。见 [FEATURES.md](../FEATURES.md) roadmap。
3. **tidy-lint / tidy**(#153):布局/丝印一致性审计与一键 cleanup。
4. **2D/3D 渲染切换 `pcb view-mode`**(#169):snapshot 前确定性选视图。
5. **受控阻抗**:平台墙(叠层 Er/介质厚读不到);网长可读,等长/skew 报告可做。
6. **泪滴**:无 typed API,文档源注入路径未验证。

---

*本文只记最终功能形态;实现历史与状态细节见 [`docs/FEATURES.md`](../FEATURES.md)。
改动 PCB 命令后请同步本文。*
