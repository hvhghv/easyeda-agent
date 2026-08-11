# CLI 功能索引

`easyeda` CLI 的功能地图入口——**只记最终功能形态**,按域分文档;每个动作都以 typed
Cobra 子命令暴露(`--help` 自描述),机器可读真值是 `easyeda actions` / `make actions`。

| 域 | 状态 | 文档 | 一句话 |
|---|---|---|---|
| **原理图**(`easyeda sch` + `blocks`) | ✅ 已支持(40+ 子命令) | [schematic.md](./schematic.md) | 器件/连线/布局/分区三件套/校验门/电路块库/导出,含布局质量五维打分(归因带可执行 fix) |
| **PCB**(`easyeda pcb` + `workflow`) | ✅ 已支持(50+ 子命令) | [pcb.md](./pcb.md) | 同步/布局/布线/铺铜/丝印/叠层规则/制造导出,九维布局打分 + 机械阶段门禁 |
| **3D 外壳设计** | 🚧 规划中(未实现) | [enclosure-3d.md](./enclosure-3d.md) | 板框/安装孔/接口开孔驱动的外壳生成,复用块库的插拔件 openings 声明 |

## 通用约定(全域一致)

- **路由**:`--project <名>`(推荐,窗口重连不失效)或 `--window <id>`(同项目多窗口时必须);`--doc <页>` 钉住目标页防错页落子。
- **判对错看数据不看截图**:`list / check / drc / layout-lint / layout-score` 是判据;截图会 stale。
- **变更即校验**:mutate 前 inspect,mutate 后跑对应 lint/check;阶段门(`sch gate` / `workflow advance`)机械强制。
- **保存**:编辑只在内存,daemon 有防抖 autosave 兜底,阶段过门后仍需显式 `save`。

> 设计流程(何时用哪个命令、阶段门顺序)见
> [`skills/easyeda-agent/references/design-flow.md`](../../skills/easyeda-agent/references/design-flow.md);
> 全域 action 清单与实现状态见 [`docs/FEATURES.md`](../FEATURES.md)。
