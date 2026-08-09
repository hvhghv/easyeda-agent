package app

// pcb_place_legalize.go — 规划器合法化阶段（#167 闭环审计遗留②）。
//
// place-constrained 的增量占用格在两类盲区上会制造硬错（真板实测：参考板 +1
// component-overlap、车机板 +3 blocking 含 1 跨网短路；C36 转 -90° 后 bbox 换形
// 叠上 J1 就是典型）：① 旋转后 bbox 换形，占用格记的还是旋转前的形；② 焊盘级
// 跨网接触，占用格只看 bbox。与其把这些几何都塞进规划器，不如在规划完成后用
// **layout-score / layout-lint 同一个纯核**（analyzePcbLayout）对虚拟落子结果
// 复算一遍 blocking —— 判定与门/打分不可能打架（单一真源），规划器未来的任何
// 盲区也都会被这里兜住。
//
// 语义边界：
//   - 只对「本次规划**新引入**的 blocking」动手 —— 板上原有的问题不归这轮管
//     （按 finding key 与基线做差）。
//   - 新引入者先绕原目标螺旋找合法位（候选点全在 5mil 锚点格上，与
//     snapMovesToAnchorGrid 的约定一致）；找不到就**弃子**（该件保持规划前的
//     位置，宁可留着旧问题也不制造新问题），diags 记 legalize:dropped。
//   - 判定坐标 = 落地坐标定律（block-apply 学费）：候选点检查用的几何与最终
//     写入的一致，不存在「查一个永不存在的坐标」。

import (
	"fmt"
	"math"
	"sort"
)

// legalizeRelocatePasses 是允许**重定位**的轮数：第 1 轮修初始冲突，第 2 轮兜
// 重定位彼此打架的余波。之后进入 drop-only 轮直到不动点 —— 弃子会级联（被弃的
// 件回到原位，别的 move 恰好规划进了那块空间，真板实测 2 条就是这么漏的），而
// drop 只会把件退回基线位置、基线本身零新增 blocking，所以 drop-only 迭代必然
// 收敛（最坏情况 = 全部弃光 = 回到基线）。
const legalizeRelocatePasses = 2

// legalizeSpiralMaxRad 是绕原目标找合法位的最大半径（mil）。超过它说明目标
// 区域整体挤满了，硬塞只会把问题推给下一个件。
const legalizeSpiralMaxRad = 600.0

// legalizeResult 汇总合法化动作，供 CLI 输出。
type legalizeResult struct {
	Checked  int `json:"checked"`            // 复算覆盖的 move 数
	Adjusted int `json:"adjusted,omitempty"` // 目标被螺旋重定位的 move 数
	Dropped  int `json:"dropped,omitempty"`  // 被弃掉的 move 数
}

// vComp 是合法化用的虚拟器件投影：规划 move 套用后的几何。
type vComp struct {
	id, designator string
	layer          int
	bbox           *layoutBBox
	pads           []pcbLPad
	moved          bool
}

// legalizeConstrainedMoves 用 lint 纯核复算规划结果，消掉新引入的 blocking。
//
// snap 缺失（离线/降级）时原样放行 —— 合法化是增强不是门，没有数据时不能
// 假装检查过（结果里 Checked=0 如实暴露）。
func legalizeConstrainedMoves(snap *boardSnapshot, moves []apMove) ([]apMove, []apDiag, legalizeResult) {
	var res legalizeResult
	if snap == nil || len(moves) == 0 || len(snap.Components) == 0 {
		return moves, nil, res
	}
	res.Checked = len(moves)

	minGap := snap.Rules.toPcbRules().clearanceMil
	outline := outlineBBoxOf(snap)

	// 基线 blocking：板上本来就有的问题，不归这轮管。
	baseComps, basePads := snap.toLayoutComps()
	base := analyzePcbLayout(baseComps, basePads, outline, minGap)
	baseKeys := blockingKeySet(&base)

	byID := make(map[string]boardComp, len(snap.Components))
	for _, c := range snap.Components {
		byID[c.ID] = c
	}
	kept := make([]apMove, len(moves))
	copy(kept, moves)
	// moveIdx 只建一次、只索引**未过滤**的 kept —— 用过滤后切片的下标去索引
	// kept 是错位(第一版真板复跑实锤:弃子一发生,后续 offender 全改错件)。
	moveIdx := make(map[string]int, len(kept))
	for i, m := range kept {
		moveIdx[m.Designator] = i
	}
	dropped := map[string]bool{}

	var diags []apDiag
	// 轮数上限只是防御性护栏：drop-only 轮每轮至少弃一个 move,len(moves)+3
	// 理论上到不了。
	for pass := 0; pass < len(moves)+3; pass++ {
		virtual := buildVirtualComps(snap, activeMoves(kept, dropped))
		after := layoutOfVirtual(virtual, outline, minGap)
		offenders := newBlockingOffenders(&after, baseKeys, virtual)
		if len(offenders) == 0 {
			break
		}
		lastPass := pass >= legalizeRelocatePasses
		for _, des := range offenders {
			mi, ok := moveIdx[des]
			if !ok || dropped[des] {
				continue
			}
			m := kept[mi]
			orig, ok := byID[m.ID]
			if !ok {
				continue
			}
			if !lastPass {
				if nx, ny, found := findLegalSpot(virtual, orig, m, outline, minGap); found {
					kept[mi].NewX, kept[mi].NewY = nx, ny
					res.Adjusted++
					diags = append(diags, apDiag{Designator: des, Reason: fmt.Sprintf(
						"legalize:relocated (%.0f,%.0f)→(%.0f,%.0f): planned spot introduced a blocking issue", m.NewX, m.NewY, nx, ny)})
					// 更新虚拟态，让同轮后续 offender 看到新位置。
					virtual = buildVirtualComps(snap, activeMoves(kept, dropped))
					continue
				}
			}
			dropped[des] = true
			res.Dropped++
			diags = append(diags, apDiag{Designator: des, Reason:
				"legalize:dropped: planned move introduces a blocking issue (overlap/short/off-board) and no legal spot within " +
					fmt.Sprintf("%.0fmil — part left at its previous position", legalizeSpiralMaxRad)})
			virtual = buildVirtualComps(snap, activeMoves(kept, dropped))
		}
	}

	if len(dropped) == 0 {
		return kept, diags, res
	}
	out := make([]apMove, 0, len(kept))
	for _, m := range kept {
		if !dropped[m.Designator] {
			out = append(out, m)
		}
	}
	return out, diags, res
}

// activeMoves 过滤掉已弃的 move。
func activeMoves(moves []apMove, dropped map[string]bool) []apMove {
	if len(dropped) == 0 {
		return moves
	}
	out := make([]apMove, 0, len(moves))
	for _, m := range moves {
		if !dropped[m.Designator] {
			out = append(out, m)
		}
	}
	return out
}

// buildVirtualComps 把 moves 套到快照上，产出虚拟器件表。
func buildVirtualComps(snap *boardSnapshot, moves []apMove) []vComp {
	byID := make(map[string]apMove, len(moves))
	for _, m := range moves {
		byID[m.ID] = m
	}
	out := make([]vComp, 0, len(snap.Components))
	for _, c := range snap.Components {
		if m, moved := byID[c.ID]; moved {
			out = append(out, applyMoveToVComp(c, m))
			continue
		}
		out = append(out, vComp{
			id: c.ID, designator: c.Designator, layer: c.Layer,
			bbox: c.BBox, pads: padsOf(c),
		})
	}
	return out
}

// padsOf 投影一件的焊盘（与 toLayoutComps 同形）。
func padsOf(c boardComp) []pcbLPad {
	out := make([]pcbLPad, 0, len(c.Pads))
	for _, p := range c.Pads {
		out = append(out, pcbLPad{
			Designator: c.Designator, Number: p.Number, Net: p.Net,
			Layer: p.Layer, X: p.X, Y: p.Y, W: p.W, H: p.H,
		})
	}
	return out
}

// applyMoveToVComp 把一个 move 套到一件上：先绕锚点转（90° 整数倍才转 ——
// 规划器只发 0/90/180/270 的朝向变更；bbox 转四角取 AABB，焊盘中心跟着转、
// 90/270 时 W/H 互换），再整体平移。渲染 bbox 里的丝印文字实际不随件转，
// 这里的旋转 AABB 是近似 —— 但比「假装没转」强得多：C36 事故正是转 -90°
// 后 bbox 换形叠上邻件，平移-only 的虚拟态根本预测不到。
func applyMoveToVComp(c boardComp, m apMove) vComp {
	dx, dy := m.NewX-c.X, m.NewY-c.Y
	delta := 0.0
	if m.SetRot {
		delta = math.Mod(math.Mod(m.NewRot-c.Rotation, 360)+360, 360)
		if math.Abs(delta-math.Round(delta/90)*90) > 1 {
			delta = 0 // 非 90° 整数倍：保守按纯平移处理
		} else {
			delta = math.Round(delta/90) * 90
			if delta == 360 {
				delta = 0
			}
		}
	}
	v := vComp{id: c.ID, designator: c.Designator, layer: c.Layer, moved: true}
	if c.BBox != nil {
		bb := *c.BBox
		if delta != 0 {
			bb = rotateBBoxAround(bb, c.X, c.Y, delta)
		}
		bb.MinX += dx
		bb.MaxX += dx
		bb.MinY += dy
		bb.MaxY += dy
		v.bbox = &bb
	}
	v.pads = make([]pcbLPad, 0, len(c.Pads))
	for _, p := range c.Pads {
		px, py, w, h := p.X, p.Y, p.W, p.H
		if delta != 0 {
			rx, ry := rotate2d(px-c.X, py-c.Y, delta)
			px, py = c.X+rx, c.Y+ry
			if delta == 90 || delta == 270 {
				w, h = h, w
			}
		}
		v.pads = append(v.pads, pcbLPad{
			Designator: c.Designator, Number: p.Number, Net: p.Net,
			Layer: p.Layer, X: px + dx, Y: py + dy, W: w, H: h,
		})
	}
	return v
}

// rotateBBoxAround 把 AABB 的四角绕 (ax,ay) 旋转 deg 后取新 AABB。
func rotateBBoxAround(bb layoutBBox, ax, ay, deg float64) layoutBBox {
	xs := []float64{bb.MinX, bb.MinX, bb.MaxX, bb.MaxX}
	ys := []float64{bb.MinY, bb.MaxY, bb.MinY, bb.MaxY}
	out := layoutBBox{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	for i := range xs {
		rx, ry := rotate2d(xs[i]-ax, ys[i]-ay, deg)
		x, y := ax+rx, ay+ry
		out.MinX = math.Min(out.MinX, x)
		out.MaxX = math.Max(out.MaxX, x)
		out.MinY = math.Min(out.MinY, y)
		out.MaxY = math.Max(out.MaxY, y)
	}
	return out
}

// layoutOfVirtual 对虚拟态跑 lint 纯核。
func layoutOfVirtual(vs []vComp, outline *layoutBBox, minGap float64) pcbLayoutReport {
	comps := make([]pcbLComp, 0, len(vs))
	var pads []pcbLPad
	for _, v := range vs {
		comps = append(comps, pcbLComp{Designator: v.designator, Layer: v.layer, BBox: v.bbox})
		pads = append(pads, v.pads...)
	}
	return analyzePcbLayout(comps, pads, outline, minGap)
}

// blockingKeySet 把一份 lint 报告的硬错编成可比对的 key 集合。
func blockingKeySet(l *pcbLayoutReport) map[string]bool {
	out := map[string]bool{}
	for _, s := range l.Shorts {
		out["short|"+s.A+"|"+s.B] = true
	}
	for _, f := range l.Overlaps {
		out["ov|"+f.A+"|"+f.B] = true
	}
	for _, f := range l.OutsideOutline {
		out["off|"+f.A] = true
	}
	return out
}

// newBlockingOffenders 找出「相对基线新增的 blocking」里被 move 过的位号，
// 排序保证确定性。
func newBlockingOffenders(after *pcbLayoutReport, baseKeys map[string]bool, vs []vComp) []string {
	movedSet := map[string]bool{}
	for _, v := range vs {
		if v.moved {
			movedSet[v.designator] = true
		}
	}
	// padShort 的 A/B 是 "U1.3" 形；取位号部分。
	desOf := func(label string) string {
		for i := 0; i < len(label); i++ {
			if label[i] == '.' {
				return label[:i]
			}
		}
		return label
	}
	seen := map[string]bool{}
	consider := func(key string, involved ...string) {
		if baseKeys[key] {
			return
		}
		for _, d := range involved {
			if movedSet[d] {
				seen[d] = true
			}
		}
	}
	for _, s := range after.Shorts {
		consider("short|"+s.A+"|"+s.B, desOf(s.A), desOf(s.B))
	}
	for _, f := range after.Overlaps {
		consider("ov|"+f.A+"|"+f.B, f.A, f.B)
	}
	for _, f := range after.OutsideOutline {
		consider("off|"+f.A, f.A)
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// findLegalSpot 绕原目标螺旋找一个不引入 blocking 的落点（5mil 格）。
//
// 快筛三条与 lint 纯核同语义：焊盘中心在板框内（off-board 判据）、同装配面
// bbox 不相交（overlap 判据）、跨网焊盘铜不接触（short 判据，直接调 padShorts）。
func findLegalSpot(vs []vComp, orig boardComp, m apMove, outline *layoutBBox, minGap float64) (float64, float64, bool) {
	for r := 25.0; r <= legalizeSpiralMaxRad; r += 25 {
		steps := int(math.Max(8, math.Round(2*math.Pi*r/50))) // 弧长 ~50mil 一个候选
		for i := 0; i < steps; i++ {
			ang := float64(i) / float64(steps) * 2 * math.Pi
			cx := math.Round((m.NewX+r*math.Cos(ang))/cpAnchorGrid) * cpAnchorGrid
			cy := math.Round((m.NewY+r*math.Sin(ang))/cpAnchorGrid) * cpAnchorGrid
			cand := m
			cand.NewX, cand.NewY = cx, cy
			v := applyMoveToVComp(orig, cand)
			if spotIsLegal(v, vs, outline) {
				return cx, cy, true
			}
		}
	}
	return 0, 0, false
}

// spotIsLegal 检查候选虚拟件是否与其它件/板框冲突。
func spotIsLegal(v vComp, vs []vComp, outline *layoutBBox) bool {
	if outline != nil {
		if len(v.pads) > 0 {
			for _, p := range v.pads {
				if p.X < outline.MinX || p.X > outline.MaxX || p.Y < outline.MinY || p.Y > outline.MaxY {
					return false
				}
			}
		} else if v.bbox != nil {
			if v.bbox.MinX < outline.MinX || v.bbox.MinY < outline.MinY ||
				v.bbox.MaxX > outline.MaxX || v.bbox.MaxY > outline.MaxY {
				return false
			}
		}
	}
	for _, o := range vs {
		if o.id == v.id {
			continue
		}
		if v.bbox != nil && o.bbox != nil {
			if _, _, ov := overlapExtent(*v.bbox, *o.bbox); ov {
				// 铜接触不分装配面（通孔导通），跨网即非法。
				if len(padShorts(v.pads, o.pads)) > 0 {
					return false
				}
				if sameAssemblySide(v.layer, o.layer) {
					return false
				}
			}
		}
	}
	return true
}
