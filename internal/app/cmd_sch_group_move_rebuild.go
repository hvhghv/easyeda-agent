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

	wires, werr := fetchSchWirePolylinesStable(cfg, win, docUUID)
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

	// 3. 删净成员整树(旧桩 + 旧旗 + 不触 pin 的残段)。共享树(触到非成员引脚)
	//    会被 tidyDeepSweepPlan 拒绝,零 mutation 退出 —— 删掉它会切断组外的电路。
	deleteIDs, err := tidyDeepSweepPlan(memberSet, comps, wires)
	if err != nil {
		return err
	}
	if len(deleteIDs) > 0 {
		if err := groupRebuildDeleteVerified(cfg, win, docUUID, deleteIDs, stdout); err != nil {
			return err
		}
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

// groupRebuildDeleteVerified 删除一批图元,**分批 + 回读验证**。
//
// 平台的删除 API 有一个已知的撒谎行为:**大批量提交时会静默 no-op 掉一部分,
// 却仍然返回成功**。真机实测:一次提交 90 个 id,清扫后页面上还剩 20 个旧旗
// (30 个应存在 → 实际 50 个),其中一个恰好落在新建的桩线上,把 C7_N6 整条网
// 并进了 GND —— bridge-check 报 "nets=[GND,C7_N6] pins=[J1:A5]",而删除那步
// 报告一切正常。所以删完必须**回读**,不能信返回值。
func groupRebuildDeleteVerified(cfg *appConfig, win, docUUID string, ids []string, stdout io.Writer) error {
	const batch = 40 // 经验安全批量;超过这个量级平台开始静默丢弃
	for i := 0; i < len(ids); i += batch {
		end := i + batch
		if end > len(ids) {
			end = len(ids)
		}
		if _, err := requestAutolayoutAction(cfg, "schematic.primitives.delete", win,
			map[string]any{"primitiveIds": ids[i:end]}, docUUID, "group-move 清扫"); err != nil {
			return fmt.Errorf("清扫第 %d-%d 个旧桩/旗:%w", i+1, end, err)
		}
	}
	// 回读:删除 API 返回成功不代表真删了。
	left, err := groupRebuildStillPresent(cfg, win, docUUID, ids)
	if err != nil {
		fmt.Fprintf(stdout, "  清扫:提交删除 %d 个(回读校验失败 %v —— 若后续报串网,先跑 `sch bridge-check`)\n", len(ids), err)
		return nil
	}
	if len(left) > 0 {
		// 再补一轮:剩下的通常是首轮被静默丢弃的。
		for i := 0; i < len(left); i += batch {
			end := i + batch
			if end > len(left) {
				end = len(left)
			}
			if _, err := requestAutolayoutAction(cfg, "schematic.primitives.delete", win,
				map[string]any{"primitiveIds": left[i:end]}, docUUID, "group-move 清扫补删"); err != nil {
				return fmt.Errorf("补删残留的 %d 个旧桩/旗:%w", len(left), err)
			}
		}
		still, err2 := groupRebuildStillPresent(cfg, win, docUUID, ids)
		if err2 == nil && len(still) > 0 {
			// 残留的旧旗会挂到新桩线上串网,这不是可以「继续试试」的状态。
			return fmt.Errorf("清扫后仍残留 %d 个旧桩线/旗(平台静默丢弃了删除请求)—— 器件尚未移动,画布还是原样;重试本命令,或先手工删除后再移动", len(still))
		}
		fmt.Fprintf(stdout, "  清扫:删除 %d 个旧桩线/旗(补删 %d 个平台首轮静默丢弃的)\n", len(ids), len(left))
		return nil
	}
	fmt.Fprintf(stdout, "  清扫:删除 %d 个旧桩线/旗,回读确认全部消失(器件保持不动)\n", len(ids))
	return nil
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
