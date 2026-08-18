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
// ADR-0003 记的「时间窗」洞察 —— ADR-0004 把这套管线升格为公共内核
// (sch_move_kernel.go 的 schMoveKernel),本命令是它的第一个调用方:规划
// (边界收拢)自己做,执行只准调内核。

// groupMoveRebuild 平移一个持久虚拟组(单组入口,groupsMoveRebuild 的薄封装)。
func groupMoveRebuild(cfg *appConfig, window, groupRef string, dx, dy float64,
	stdout, stderr io.Writer) error {
	return groupsMoveRebuild(cfg, window, []string{groupRef}, dx, dy, stdout, stderr)
}

// groupsMoveRebuild 平移一个或多个持久虚拟组 —— **一次内核调用整体移动**
// (ADR-0004 Decision 2 推论:同块多子组逐组 move 必撕裂共享导线;内核输入是
// 刚体集合,多组并成一个集合天然支持)。规划(边界收拢)自己做,执行只准调
// schMoveKernel(快照→删证→移动→重连→对账,失败自动恢复)。
func groupsMoveRebuild(cfg *appConfig, window string, groupRefs []string, dx, dy float64,
	stdout, stderr io.Writer) error {

	pinned, win, docUUID, _, st, _, err := loadSchGroupsContext(cfg, window)
	if err != nil {
		return err
	}
	// --group/--groups 走统一注册表解析(ADR-0004 Decision 3):组 id / 组名 /
	// 子组末段都认;命中模块认领时报类型不适配并指路 `sch zone move`。
	table := layoutObjectTableFromState(st, docUUID)
	memberSet := map[string]bool{}
	var picked []*schGroup
	seen := map[string]bool{}
	for _, ref := range groupRefs {
		obj, ferr := resolveLayoutObject(table, ref)
		if ferr != nil {
			return ferr
		}
		g, ferr := requireLayoutGroup(obj, "sch group-move")
		if ferr != nil {
			return ferr
		}
		if seen[g.ID] {
			continue
		}
		seen[g.ID] = true
		picked = append(picked, g)
		for _, m := range g.Members {
			memberSet[strings.ToUpper(m)] = true
		}
	}
	var names []string
	for _, g := range picked {
		names = append(names, describeSchGroup(g))
	}
	groupLabel := strings.Join(names, "+")

	// 1. 读场景 —— 规划(边界收拢)所需的几何。执行侧的电气快照由内核自己采
	//    (快照与执行同一份场景,不会拿 stale 数据 mutate)。
	res, err := requestAutolayoutAction(pinned, "schematic.components.list", win,
		map[string]any{"includeBBox": true, "includePins": true}, docUUID, "group-move 读场景")
	if err != nil {
		return fmt.Errorf("读场景:%w", err)
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return fmt.Errorf("解析场景:%w", err)
	}
	var movable []groupRebuildMember
	for _, c := range comps {
		if (c.ComponentType == "" || c.ComponentType == schLayoutPartType) && memberSet[strings.ToUpper(c.Designator)] {
			movable = append(movable, groupRebuildMember{ID: c.ID, Designator: c.Designator, X: c.X, Y: c.Y})
		}
	}
	if len(movable) == 0 {
		return fmt.Errorf("组 %s 在本页没有可移动的成员(位号可能已过时,用 `sch group list` 核对)", groupLabel)
	}

	wires, werr := fetchSchWirePolylinesStable(pinned, win, docUUID)
	if werr != nil {
		return fmt.Errorf("读导线:%w", werr)
	}

	// 2b. **边界收拢**:每一层都要自己保证不出界(ADR-0003 §6)。group-arrange 走的
	//     是有边界的排布器,而手工 group-move 过去完全不查 —— 实测 Δ=(40,60) 就把
	//     整个组推出图纸,layout-lint 报 5 out-of-sheet 而命令一声不吭。
	//     收拢而不是拒绝:调用方要的是「挪一挪」,把它按到可用区里仍然满足这个意图。
	if box, ok := groupOccupancy(comps, wires, memberSet); ok {
		if sheet := sheetBBoxOf(comps); sheet != nil {
			// 收拢用**整页可用区**,图签 keepout 单独按相交判(见 clampDeltaAvoidingKeepout)。
			// arrangeBoundsOf 会把下界整条抬到图签上沿 —— 那对多组铺排是可接受的简化,
			// 对「挪一挪」不是:图签只占右下角,页面**左**下那片(x < 图签左沿)本来能用,
			// 抬掉等于凭空少一条 198 高的地(2026-08-15 esp32Mini E2E:MCU 组 421 高、
			// 上面还挂着去耦,只有把它落到左下才装得下)。
			bounds := layoutBBox{
				MinX: sheet.MinX + sheetEdgeMinGap, MinY: sheet.MinY + sheetEdgeMinGap,
				MaxX: sheet.MaxX - sheetEdgeMinGap, MaxY: sheet.MaxY - sheetEdgeMinGap,
			}
			ko, provisional := titleBlockKeepout(sheet)
			if provisional {
				ko = nil // 猜出来的图签框不拿来收拢(与 arrangeBoundsOf 同口径)
			}
			ndx, ndy := clampDeltaAvoidingKeepout(box, dx, dy, bounds, ko)
			if ndx != dx || ndy != dy {
				fmt.Fprintf(stderr, "note: Δ=(%.0f,%.0f) 会让组出图纸可用区,已收拢到 Δ=(%.0f,%.0f)\n", dx, dy, ndx, ndy)
				dx, dy = ndx, ndy
			}
		}
	}
	if dx == 0 && dy == 0 {
		fmt.Fprintln(stdout, "✓ 组已在可用区内且无需移动(零位移,未改动画布)")
		return nil
	}

	// 3. 执行只准调内核(ADR-0004):快照 → 删证 → 移动(snap 5)→ 重连 → 对账,
	//    任一步失败自动进入恢复段。删除撒谎/假失败/合并短路三个平台病都在内核
	//    一处治,本命令不再自带删/移/连的实现。
	items := make([]moveItem, 0, len(movable))
	for _, m := range movable {
		items = append(items, moveItem{Designator: m.Designator, HasTarget: true, X: m.X + dx, Y: m.Y + dy})
	}
	rep, kerr := schMoveKernel(pinned, win, docUUID, items,
		moveKernelOpts{Label: "group-move", Stdout: stdout, Stderr: stderr})
	if kerr != nil {
		return kerr
	}
	fmt.Fprintf(stdout, "✓ 组 %s 平移 %d 件 Δ=(%.0f,%.0f);内核对账绿(网表逐引脚一致,无新增 bridge)\n",
		groupLabel, len(rep.Moved), dx, dy)
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
		// 真器件的 componentType 可能是 "part" 也可能为空(连接器版本差异),
		// 与 zone/tidy 家族同口径地都接受。
		if (c.ComponentType != "" && c.ComponentType != schLayoutPartType) || !memberSet[strings.ToUpper(c.Designator)] {
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

// groupRebuildStillPresent 回读页面,返回 ids 中仍然存在的那些。
func groupRebuildStillPresent(cfg *appConfig, win, docUUID string, ids []string) ([]string, error) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", win,
		map[string]any{"includeBBox": false, "includePins": false}, docUUID, "group-move 清扫回读")
	if err != nil {
		return nil, err
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return nil, err
	}
	var left []string
	for _, c := range comps {
		if want[c.ID] {
			left = append(left, c.ID)
		}
	}
	wires, werr := fetchSchWirePolylinesStable(cfg, win, docUUID)
	if werr == nil {
		for _, w := range wires {
			if want[w.ID] {
				left = append(left, w.ID)
			}
		}
	}
	sort.Strings(left)
	return left, nil
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

// clampDeltaToBounds 把一次平移收拢进可用区:先按请求平移,越界的方向往回推,
// 结果仍吸附在 5 格上(判定坐标 = 落地坐标)。组比可用区还大时只保证左上角对齐,
// 那种情况该拆页,不是靠挪。
func clampDeltaToBounds(box layoutBBox, dx, dy float64, bounds layoutBBox) (float64, float64) {
	grid := float64(schAnchorGrid)
	snap := func(v float64) float64 { return math.Round(v/grid) * grid }
	nx, ny := dx, dy
	if box.MaxX+nx > bounds.MaxX {
		nx = bounds.MaxX - box.MaxX
	}
	if box.MinX+nx < bounds.MinX {
		nx = bounds.MinX - box.MinX
	}
	if box.MaxY+ny > bounds.MaxY {
		ny = bounds.MaxY - box.MaxY
	}
	if box.MinY+ny < bounds.MinY {
		ny = bounds.MinY - box.MinY
	}
	// **收拢只许减小位移,绝不许反号**:组当前就已越界时(marker 探出上沿是常态),
	// 上面两条会把「往下挪 30」算成「往上挪 40」—— 调用方要的是往下,工具却把它
	// 推得更糟(2026-08-15 esp32Mini E2E 实测 Δ=(20,-30) → 收拢成 (20,+40))。
	// 收拢的语义是「你要的方向走不了那么远」,不是「换个方向走」。走不了就 0。
	return snap(clampNoFlip(dx, nx)), snap(clampNoFlip(dy, ny))
}

// clampNoFlip 保证收拢后的位移与请求同号且不更大;反号或超量一律退回 0。
func clampNoFlip(want, got float64) float64 {
	if want == 0 {
		return 0
	}
	if want > 0 {
		if got < 0 {
			return 0
		}
		return math.Min(got, want)
	}
	if got > 0 {
		return 0
	}
	return math.Max(got, want)
}

// clampDeltaAvoidingKeepout 在 clampDeltaToBounds 之上再避开图签 keepout —— 但只在
// 移动**后**真的与它相交时才管。图签是右下角一个矩形,不是整条底边:组落在它左边
// (或上边)时,页面下部那片地照常可用。keepout 为 nil 时退化成纯边界收拢。
func clampDeltaAvoidingKeepout(box layoutBBox, dx, dy float64, bounds layoutBBox, keepout *layoutBBox) (float64, float64) {
	nx, ny := clampDeltaToBounds(box, dx, dy, bounds)
	if keepout == nil {
		return nx, ny
	}
	moved := layoutBBox{MinX: box.MinX + nx, MinY: box.MinY + ny, MaxX: box.MaxX + nx, MaxY: box.MaxY + ny}
	after := boxIntersectArea(moved, *keepout)
	if after == 0 {
		return nx, ny // 移动后不压图签
	}
	// **只在把事情弄得更糟时才拦**。组常常移动前就已经压着图签(marker 伸进去是
	// 常态),此时"必须一步到位挪干净"是个做不到的要求 —— 旧实现于是把每一次 y
	// 移动都收成 0,连"往好的方向挪一点"都做不了(2026-08-15 esp32Mini E2E:LDO 组
	// 的 +5V 标签伸到 y=-22,想整组上移 40 被拒,页面卡在 out-of-sheet 上)。
	if after <= boxIntersectArea(box, *keepout) {
		return nx, ny
	}
	// 变糟了:优先把 y 收回到图签上沿之上。
	if lift := keepout.MaxY - box.MinY; lift <= 0 {
		return nx, clampNoFlip(dy, lift)
	}
	return nx, 0
}

// boxIntersectArea 是两个矩形的相交面积(不相交为 0)。
func boxIntersectArea(a, b layoutBBox) float64 {
	w := math.Min(a.MaxX, b.MaxX) - math.Max(a.MinX, b.MinX)
	h := math.Min(a.MaxY, b.MaxY) - math.Max(a.MinY, b.MinY)
	if w <= 0 || h <= 0 {
		return 0
	}
	return w * h
}
