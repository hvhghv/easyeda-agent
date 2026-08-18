# ADR 0004: 原理图「挪动」收敛为单一安全 move 内核

## Status

Accepted. (2026-08-19) — 用户拍板,源自 issue #181 两份独立 E2E 复盘 +
2026-08-17/18 esp32Mini E2E 一手实录。v1.0.2 发版推迟至本 ADR 首批落地。

## Context

ADR-0003 把「往哪放」收敛成了三层刚体求解,但**没有建模「怎么挪」**。而三份
独立 E2E 复盘(#181 主帖、#181 补充评论、esp32Mini 会话实录)一致显示:
**≥80% 的耗时不在初次摆放,而在迭代挪动**——放错了要挪、门禁不过要挪、
分区拥挤要挪。

挪动在 EasyEDA 平台上是高危手术,因为平台没有给我们原子性:

- **无 move-with-wires API**:带线搬必断线(ADR-0003 附录已定案「删净→重连」);
- **删除会撒谎**:并入共享树/共线段的 wire,delete 返回 true 实际 no-op(实录
  R5/C4;v1.0.1 已在 disconnect 加回读验证,但其他删除点未覆盖);
- **相邻共线导线自动合并**:删桩线触发合并 → 新桩被邻居吞 → **真短路**
  (marker-move-breaks-on-wire-merge 三次三败定案;#181 两份复盘各自复现);
- **响应可能缺失/超时但写已落地**(负载停摆假失败),盲重试造重复 marker;
- **无程序化 undo**:失败后只能靠自己的快照恢复。

而我们把「挪动」散落在**至少五个命令**里各自实现,各有一套不完整的
快照/恢复逻辑,同一个病有五个名字:

| 命令 | 失败实录 |
|---|---|
| `sch group-move` | 先清桩线→逐件 modify→任一件失败即弃(J1 组全悬空);落点 off-grid 致重连全拒 |
| `sch zone relayout --apply` | 在多物理脚连接器块上 sweep+重连制造 wire-bridge,回滚后残留 3 个 bridge,只能 clear 整页 |
| `sch destagger --apply` | 真机三次三败(删桩触发合并串网),已禁用只留 dry-run |
| `sch zone-arrange --apply` | 半途 4/11 落位,修复轮大量假失败,断言②遗留 3 处红 |
| `sch modify`(裸挪) | 有线时被拒是对的,但调用方只能自己拼「disconnect→modify→reconnect」三步,每步都能静默半途死 |

配套的三个放大器:

1. **`--zone` 命名空间三套并存**(#181 补充第 1 条):`zones set` 记模块名、
   `block-apply` 记虚拟组名/子组名,不同命令各认一套,报错才泄露真相;
2. **dry-run 不纯**(#181 主帖第 4 条):有 dry-run 真落件的路径,没有机制保证;
3. **删件不级联**:删 component 不清它的独占桩线/flag,残留物被后放件静默继承
   网名(幽灵连接;v1.0.1 的 orphan-tree 判据只能事后抓,不能事前防)。

## Decision

### 1. 单一 move 内核,五步管线,失败自动恢复

新建 `schMoveKernel`(internal/app),**所有**改变已连线器件位置的路径必须走它:

```
┌ 1. 快照   电气快照(pin→net 全表)+ 涉及图元清单(件/桩线/flag,整树粒度)
├ 2. 删证   整树删除 + 回读证实(删除撒谎 → 计入 partial,绝不带病进入下一步)
├ 3. 移动   逐件 modify,坐标先 snap 到 5 网格(off-grid 重连全拒的根因)
├ 4. 重连   按快照重连全部涉及 pin(autoconnect 评分避让;器件在原位也能连回)
└ 5. 对账   网表逐 pin 与快照比对;新增 bridge 检查(合并短路当场抓)
```

失败语义(与 #151 部分应用约定一致):

- 任何一步失败 → **立即进入恢复段**:对当前实际位置的全部件按快照重连,
  然后返回结构化结果(`moved`/`recovered`/`stillBroken[]`),绝不留
  「桩线已清、器件没挪、连接全断」的 PARTIAL 尸体;
- 恢复本身失败 → 如实列出仍断 pin(可直接喂 `sch connect`),非零退出;
- **对账不过 = 失败**,即使几何都成功了(判据是电气不是坐标)。

`group-move` 的失败恢复段(v1.0.1-4 落地)是本内核的雏形,升格为公共内核后
它变成第一个调用方。

### 2. 五命令收敛为内核调用方

`group-move` / `zone move` / `zone relayout --apply` / `zone-arrange --apply` /
`destagger --apply`(解禁的前提)全部改为:**规划自己做,执行只准调内核**。
规划层只产出「哪些刚体 → 目标位移」,不再各自实现删/移/连。

推论:同块多子组必须**一次内核调用整体移动**(#181 补充第 3 条:逐子组 move
必撕裂共享导线)——内核输入是「刚体集合」,天然支持;命令层把同块子组默认
并成一个集合。

### 3. zone/group 命名空间统一为一张注册表

一页只有一张「布局对象注册表」:模块认领(zones set)、块虚拟组、块子组是
同一张表里的三种来源标签,不再是三个存储。所有吃 `--zone`/`--group` 参数的
命令走同一个解析器;解析失败的报错**必须**列出本页全部可用名并标注来源
(`POWER (module claim)` / `esp32s3_wroom1_module(C1) (block)` /
`…/D_ESD (subgroup)`)。

### 4. dry-run 纯计算铁律(机械保证,不靠自觉)

`--dry-run` / 默认 dry-run 的路径**不允许发出任何 Mutates=true 的 action**。
机械保证在 CLI 派发层:dry-run 模式设置进程内标志,`dispatch` 对 Mutates
action 直接拒绝(带清晰报错)。这样任何新命令想在 dry-run 里偷偷落件,
第一次跑就炸,而不是让用户在画布上发现幽灵件。

### 5. 删件级联清理

`schematic.component.delete` 级联删除该器件的**独占**桩线与 flag:
判定复用 orphan-tree 的整树逻辑——删件后属于它的树若不再触及任何其他 pin,
整树一并删除并回读证实。共享树(还连着别人)只断开不删除。CLI 报告级联
清理了什么(`cascaded: N wires, M flags`)。

## 范围外(明确不做/后续)

- **求解闭环**(落地实测回填修正):属 ADR-0003 实现补遗,zone-arrange 两遍法
  先行,不在本 ADR;
- **拆页方案生成**(zone-plan 判拆页时给出建页/移组指令,#181 主帖第 2 条):
  独立议题,另行立项;
- 平台侧根治(move-with-wires / 事务 API):upstream 无此能力,持续跟踪。

## Consequences

- 「挪动」的失败模式收敛到一处,修一次全体受益——不再打地鼠;
- destagger 有了解禁路径(它死于没有安全执行层,不是死于规划);
- 内核对账强制「判据是电气不是坐标」,把 marker-move 三败的教训固化成代码;
- 代价:内核是热路径上的新单点,必须带全套失败注入测试(删除撒谎/超时假失败/
  合并短路三个平台病各一组);
- 命名空间迁移期需要兼容读旧存储(一次性迁移 + 双读一个版本)。

## References

- issue #181(两份 E2E 复盘,本 ADR 的直接动因)
- ADR-0003(位置求解;本 ADR 是它缺失的另一半)
- memory: marker-move-breaks-on-wire-merge / rigid-body-move-delete-then-reconnect /
  connector-wedge-fake-failure-under-load / platform-delete-lies-and-pin-truth-table
- v1.0.1-4 已落地的雏形:group-move 恢复段、disconnect 回读验证、orphan-tree 判据
