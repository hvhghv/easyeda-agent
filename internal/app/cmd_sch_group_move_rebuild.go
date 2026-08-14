package app

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// ── 组的整体平移:删净 → 平移 → 一遍性重连(ADR-0003)────────────────────────
//
// **为什么不能带着线一起搬。** 连接器的 schematic.group.move 对导线和旗只能
// delete + recreate(`sch_PrimitiveComponent.modify` 是 element-only,对 flag 无效),
// 而平台会把**共享端点的同网导线合并成一根**。于是逐根删建的过程里,新建的桩线
// 被相邻共线的邻居吞掉 —— 真机可复现:ch340c 块平移 (60,-40),移动前 29 根导线、
// 移动后 26 根,GND 网静默丢掉 C7.2 / J1.8 / J1.9 三个引脚,而命令本身报告
// 「expanded: 7 component(s) + 29 stub wire(s) + 29 flag(s)」一切正常。
// 这与 `sch destagger --apply` 当初三次真机失败是同一个机制(那次为此禁用了命令)。
//
// **正确做法**:先把成员的桩线和旗**删净**,此刻器件身上没有任何导线,平移就退化成
// 纯粹的 component.modify(零合并风险),再由 autoconnect 一遍性重连。这正是
// ADR-0003 记的「时间窗」洞察,也是 `sch zone relayout --apply` 已在用的管线。

// groupMoveRebuild 平移一个持久虚拟组:删净成员的桩线/旗 → 逐件 modify → 重连 →
// 电气自检。返回前后 netlist 的差异(空 = 电气完全不变)。
func groupMoveRebuild(cfg *appConfig, window, groupRef string, dx, dy float64,
	stdout, stderr io.Writer) error {

	pinned, win, docUUID, _, _, groups, err := loadSchGroupsContext(cfg, window)
	if err != nil {
		return err
	}
	g, err := findSchGroup(groups, groupRef)
	if err != nil {
		return err
	}
	memberSet := map[string]bool{}
	for _, m := range g.Members {
		memberSet[strings.ToUpper(m)] = true
	}

	// 1. 读场景 —— 一次拿全:器件几何、引脚当前网、已有导线。
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", win,
		map[string]any{"includeBBox": true, "includePins": true}, docUUID, "group-move 读场景")
	if err != nil {
		return fmt.Errorf("读场景:%w", err)
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return fmt.Errorf("解析场景:%w", err)
	}

	// 2. **先记下每个成员引脚现在连着哪条网** —— 这是重连的唯一依据。必须在删除
	//    之前采集:删完就无从得知这根桩线原本属于哪条网了。
	live, _, lerr := readLiveNets(pinned, win)
	if lerr != nil {
		return fmt.Errorf("读网表(重连的唯一依据):%w", lerr)
	}
	conns, movable := groupRebuildConnSpecs(comps, memberSet, live)
	if len(movable) == 0 {
		return fmt.Errorf("组 %s 在本页没有可移动的成员(位号可能已过时,用 `sch group list` 核对)", describeSchGroup(g))
	}
	before := groupRebuildSnapshotOf(live)

	// 3. 删净成员整树(旧桩 + 旧旗 + 不触 pin 的残段)。共享树(触到非成员引脚)
	//    会被 tidyDeepSweepPlan 拒绝,零 mutation 退出 —— 删掉它会切断组外的电路。
	wires, err := fetchSchWirePolylinesStable(cfg, win, docUUID)
	if err != nil {
		return fmt.Errorf("读导线:%w", err)
	}
	deleteIDs, err := tidyDeepSweepPlan(memberSet, comps, wires)
	if err != nil {
		return err
	}
	for _, c := range comps {
		if schGroupFlagTypes[c.ComponentType] && groupRebuildFlagBelongs(c, comps, memberSet) {
			deleteIDs = append(deleteIDs, c.ID)
		}
	}
	if len(deleteIDs) > 0 {
		if _, err := requestAutolayoutAction(cfg, "schematic.primitives.delete", win,
			map[string]any{"primitiveIds": deleteIDs}, docUUID, "group-move 清扫"); err != nil {
			return fmt.Errorf("清扫 %d 个旧桩/旗:%w", len(deleteIDs), err)
		}
		fmt.Fprintf(stdout, "  清扫:删除 %d 个旧桩线/旗(器件保持不动)\n", len(deleteIDs))
	}

	// 4. 平移器件本体。此刻它们身上没有任何导线,modify 不会触发任何合并。
	for _, m := range movable {
		if _, err := requestAutolayoutAction(cfg, "schematic.component.modify", win,
			map[string]any{"primitiveId": m.ID, "patch": map[string]any{"x": m.X + dx, "y": m.Y + dy}},
			docUUID, "group-move 平移"); err != nil {
			return fmt.Errorf("平移 %s:%w(已清扫旧桩线,请用 `sch autoconnect` 手工重连或重放这一块)", m.Designator, err)
		}
	}
	fmt.Fprintf(stdout, "  平移:%d 件 Δ=(%.0f,%.0f)\n", len(movable), dx, dy)

	// 5. 一遍性重连 —— 落点由 autoconnect 按**新几何**重新评分,而不是把旧 marker
	//    平移过去:旧落点是在旧邻居关系下算出来的,平移后未必还是最优,更未必不撞。
	if len(conns) > 0 {
		if err := runAutoconnect(pinned, win, conns, defaultAutoconnectRules(),
			false, false, false, false, stderr, stderr); err != nil {
			fmt.Fprintf(stderr, "warn: 重连有失败项(%v)—— 器件已在新位置,补连失败的引脚即可\n", err)
		}
	}

	// 6. **电气自检**:平移是刚体操作,网表必须逐引脚一致。这条判据是本命令存在的
	//    理由 —— 旧实现正是在这里静默丢了三个引脚而无人察觉。
	if before != nil {
		after, aerr := groupRebuildNetSnapshot(cfg, win)
		if aerr != nil {
			fmt.Fprintf(stderr, "warn: 取不到移动后的网表(%v)—— 请手工跑 `sch check`\n", aerr)
			return nil
		}
		if diffs := groupRebuildNetDiff(before, after); len(diffs) > 0 {
			for _, d := range diffs {
				fmt.Fprintf(stderr, "✗ %s\n", d)
			}
			return fmt.Errorf("整体平移改变了 %d 条网的连接 —— 器件已在新位置,用 `sch check` / `sch bridge-check` 核对后手工补连", len(diffs))
		}
		fmt.Fprintf(stdout, "✓ 电气自检:%d 条网逐引脚一致(刚体平移未改变任何连接)\n", len(before))
	}
	return nil
}

// groupRebuildMember 是一个待平移的成员。
type groupRebuildMember struct {
	ID         string
	Designator string
	X, Y       float64
}

// groupRebuildConnSpecs 采集「每个成员引脚现在连着哪条网」,输出重连规格。
// **网来自实时网表而不是引脚属性**:netlist 是本项目唯一可信的连接判据
// (components.list 的引脚里没有网名字段,而几何重合不等于电气连接)。
// 浮空引脚不出现在网表里,于是天然不产生规格 —— 它本来就没连,重连后也不该
// 凭空多一根桩线。
func groupRebuildConnSpecs(comps []layoutComp, memberSet map[string]bool,
	live map[string]map[string]bool) ([]acConnSpec, []groupRebuildMember) {

	// 反转网表:"DESIG.NUM" → net
	pinNet := map[string]string{}
	for net, pins := range live {
		for ref := range pins {
			pinNet[strings.ToUpper(ref)] = net
		}
	}
	var conns []acConnSpec
	var movable []groupRebuildMember
	for _, c := range comps {
		if c.ComponentType != "part" || !memberSet[strings.ToUpper(c.Designator)] {
			continue
		}
		movable = append(movable, groupRebuildMember{ID: c.ID, Designator: c.Designator, X: c.X, Y: c.Y})
		for _, p := range c.Pins {
			net := pinNet[strings.ToUpper(c.Designator+"."+p.Number)]
			if net == "" {
				continue
			}
			conns = append(conns, acConnSpec{
				PinRef: fmt.Sprintf("%s:%s", c.Designator, p.Number),
				Kind:   bapFlagKind(net),
				Net:    net,
			})
		}
	}
	// **定序必须与 block-apply 同构:先按网分组,组内再按引脚**。
	// 评分器的 scene 随放随长(每落一个 marker 就注册回去当障碍),所以顺序直接决定
	// 落点质量 —— 按引脚名字母序打散会把同网的 marker 拆开穿插,实测 markerOverlaps
	// 从 3 涨到 13。同网连续落地时,后续 marker 能贴着前一个错列成一条 lane;
	// 打散之后每个 marker 面对的都是一堆异网邻居,只能各自挤。
	sort.Slice(movable, func(i, j int) bool { return movable[i].Designator < movable[j].Designator })
	// 档位:电源 → 地 → 信号。与 block-apply 的实际落地顺序一致(块的 NET 表就是
	// 5V/GND 在前)。为什么重要:电源和地的 marker 数量最多、方向最固定(电上地下),
	// 先把它们落满,信号 marker 才能在剩下的空间里绕开;反过来先落信号,后面成片的
	// GND 只能硬挤 —— 实测把 GND 排到最后(字母序的后果)会让 C7_N6 的桩线与 GND
	// 桩线合并**当场串网**(自检抓到:C7_N6 消失,两个引脚并进 GND)。
	kindRank := map[string]int{"power": 0, "gnd": 1, "agnd": 1, "pgnd": 1}
	rank := func(c acConnSpec) int {
		if r, ok := kindRank[c.Kind]; ok {
			return r
		}
		return 2
	}
	sort.Slice(conns, func(i, j int) bool {
		if ri, rj := rank(conns[i]), rank(conns[j]); ri != rj {
			return ri < rj
		}
		if conns[i].Net != conns[j].Net {
			return conns[i].Net < conns[j].Net
		}
		return conns[i].PinRef < conns[j].PinRef
	})
	return conns, movable
}

// groupRebuildFlagBelongs 判断一个 marker 是否属于本组:它的锚点落在某个成员引脚
// 的桩线可达范围内。保守起见只认「离某成员引脚足够近」的,拿不准就不删 —— 删错
// 别人的旗会切断组外电路,而少删一个只是留下一个孤儿 marker(check 会报)。
func groupRebuildFlagBelongs(flag layoutComp, comps []layoutComp, memberSet map[string]bool) bool {
	const reach = 3 * schStubLen // 桩长上限的宽松包络
	for _, c := range comps {
		if c.ComponentType != "part" || !memberSet[strings.ToUpper(c.Designator)] {
			continue
		}
		for _, p := range c.Pins {
			if math.Abs(flag.X-p.X) <= reach && math.Abs(flag.Y-p.Y) <= reach {
				return true
			}
		}
	}
	return false
}

// groupRebuildNetSnapshot 取一份 net → 引脚集合的快照。
func groupRebuildNetSnapshot(cfg *appConfig, window string) (map[string][]string, error) {
	live, _, err := readLiveNets(cfg, window)
	if err != nil {
		return nil, err
	}
	return groupRebuildSnapshotOf(live), nil
}

// groupRebuildSnapshotOf 把 readLiveNets 的结果压成可比对的有序快照。
func groupRebuildSnapshotOf(live map[string]map[string]bool) map[string][]string {
	out := map[string][]string{}
	for net, pins := range live {
		refs := make([]string, 0, len(pins))
		for p := range pins {
			refs = append(refs, p)
		}
		sort.Strings(refs)
		out[net] = refs
	}
	return out
}

// groupRebuildNetDiff 比对前后快照,返回人类可读的差异。刚体平移的**定义**就是
// 这个结果为空。
func groupRebuildNetDiff(before, after map[string][]string) []string {
	var out []string
	seen := map[string]bool{}
	for net, b := range before {
		seen[net] = true
		a, ok := after[net]
		if !ok {
			out = append(out, fmt.Sprintf("网 %s 移动后消失(原有 %d 个引脚:%s)", net, len(b), strings.Join(b, " ")))
			continue
		}
		lost, gained := diffStringSets(b, a)
		if len(lost) > 0 || len(gained) > 0 {
			msg := fmt.Sprintf("网 %s 成员变了", net)
			if len(lost) > 0 {
				msg += fmt.Sprintf(";丢失 %s", strings.Join(lost, " "))
			}
			if len(gained) > 0 {
				msg += fmt.Sprintf(";新增 %s", strings.Join(gained, " "))
			}
			out = append(out, msg)
		}
	}
	for net, a := range after {
		if !seen[net] {
			out = append(out, fmt.Sprintf("网 %s 是移动后新出现的(%d 个引脚:%s)", net, len(a), strings.Join(a, " ")))
		}
	}
	sort.Strings(out)
	return out
}

// diffStringSets 返回 a 有而 b 没有的、以及 b 有而 a 没有的。
func diffStringSets(a, b []string) (onlyA, onlyB []string) {
	inB := map[string]bool{}
	for _, s := range b {
		inB[s] = true
	}
	inA := map[string]bool{}
	for _, s := range a {
		inA[s] = true
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return onlyA, onlyB
}
