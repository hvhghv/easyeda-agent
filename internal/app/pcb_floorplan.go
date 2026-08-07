package app

// pcb_floorplan.go — `pcb floorplan`：按 S0 的信号流向把板面切成**有序**功能带。
//
// 为什么现有的 zone 不够用。`pcb zones` 的词汇是固定的 3 列 × 2 行九宫格
// （left/center/right × top/bottom），矩形由 pcbZoneRect 按三等分/二等分硬编码切出。
// 它能表达「MCU 在中间、IO 在下边」这种**位置**意图，但表达不了 #167 要的东西：
//
//   - **顺序**：flow ["POWER","MCU","RF","ANT"] 是一条链，九宫格没有"谁在谁之后"；
//   - **比例**：一个 166 器件的 MCU 域和一个 3 器件的 ANT 域不该各占三分之一；
//   - **段数**：flow 可能有 2 段也可能有 6 段，九宫格只有 3 列。
//
// 所以 floorplan 另起一套矩形：沿流向轴切 N 条带，带宽按该段的**器件面积**分配。
// 这不是要取代 zones —— zones 仍是人手声明位置意图的入口，floorplan 是从 flow
// 自动推出布局骨架，两者可以并存（floorplan --apply 会写成 zone claim 的超集）。
//
// concepts.md 说得很直白：T3 主芯片档「工具不做主芯片 floorplan——种子由 agent 给」，
// 并把这条列为「偏散板」的更大根因。这个文件就是补那一块。

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

// floorplanBand 是一条功能带：flow 里的一个阶段在板面上分到的矩形。
type floorplanBand struct {
	Kind  string   `json:"kind"`  // 功能域（POWER/MCU/RF/…）
	Order int      `json:"order"` // 在 flow 里的序号（0 起）
	Rect  cpRect   `json:"-"`     // 内部用；JSON 走下面四个显式字段，便于人读
	MinX  float64  `json:"minX"`
	MinY  float64  `json:"minY"`
	MaxX  float64  `json:"maxX"`
	MaxY  float64  `json:"maxY"`
	Parts []string `json:"parts,omitempty"`
	// AreaMil2 是该段器件的渲染 bbox 面积和 —— 带宽就是按它分配的。
	AreaMil2 float64 `json:"areaMil2"`
	// Modules 是贡献这一段的模块名（人读用：分带分错时能一眼看出是哪个模块归错了域）。
	Modules []string `json:"modules,omitempty"`
}

// floorplanPin 是一个被钉到板边的连接器。
type floorplanPin struct {
	Designator string  `json:"designator"`
	Edge       string  `json:"edge"`
	Facing     string  `json:"facing,omitempty"`
	Source     string  `json:"source"` // spec | block | heuristic
	TargetX    float64 `json:"targetX"`
	TargetY    float64 `json:"targetY"`
}

// floorplanReport 是规划结果。它是**建议**不是落笔：`--apply` 才写进 zone claim，
// 器件真正搬家仍然走 place-constrained（floorplan 只负责给它一张有序的地图）。
type floorplanReport struct {
	OK       bool            `json:"ok"`
	Axis     string          `json:"axis"`     // x | y
	Reversed bool            `json:"reversed"` // 流向是否沿轴反向（见 planFloorplan 注释）
	Bands    []floorplanBand `json:"bands"`
	Pins     []floorplanPin  `json:"pins,omitempty"`
	Unzoned  []string        `json:"unzoned,omitempty"` // 板上有、但 flow 里没归属的器件
	Warnings []string        `json:"warnings,omitempty"`
	Summary  string          `json:"summary"`
}

// floorplanOpts 是规划参数。
type floorplanOpts struct {
	// MarginMil 是板边留白：功能带不贴到板框，给连接器和布线通道留出来。
	MarginMil float64
	// MinBandMil 是一条带的最小宽度 —— 面积占比极小的阶段（比如只有一根天线的
	// ANT 段）不能被压成 0 宽，否则里面的件无处可放。
	MinBandMil float64
}

func defaultFloorplanOpts() floorplanOpts {
	return floorplanOpts{
		// 待校准初值。300mil ≈ 7.6mm，够放一排连接器 + 一条布线通道；
		// 真板校准（#167 第五层）时应该按板子尺寸做成比例而不是定值。
		MarginMil: 300,
		// 一条带至少 400mil ≈ 10mm，放得下一个 SOP8 + 间距。
		MinBandMil: 400,
	}
}

// planFloorplan 是纯核：给板框 + S0 意图 + 器件清单，切出有序功能带。
//
// 轴与方向的处理，与 flow-order 维必须一致（两边不一致会出现「floorplan 摆完，
// 打分说流向错了」的荒谬情况）：
//   - 轴：spec.FlowAxis 显式指定优先，auto 时用板框长边；
//   - 方向：**不强制**流向必须沿轴正方向。板上从右到左走电源→天线是同样好的布局，
//     流向是相对的。所以当板上已有器件、且它们的实际分布更接近反向时，按反向切带
//     （Reversed=true）—— 这样 floorplan 不会把一块本来就对的板整个翻过来重排。
//     空板（没有可参考的既有分布）按正向切。
func planFloorplan(s *spec.Spec, snap *boardSnapshot, opts floorplanOpts) floorplanReport {
	rep := floorplanReport{}
	if opts.MarginMil <= 0 && opts.MinBandMil <= 0 {
		opts = defaultFloorplanOpts()
	}
	if s == nil || len(s.Flow) < 2 {
		rep.Summary = "no usable flow in the S0 spec (need at least 2 stages) — nothing to plan"
		return rep
	}
	if snap == nil || snap.Outline == nil {
		rep.Summary = "board outline unavailable — cannot allocate bands"
		return rep
	}

	board := cpRect{
		x0: snap.Outline.BBox.MinX + opts.MarginMil,
		y0: snap.Outline.BBox.MinY + opts.MarginMil,
		x1: snap.Outline.BBox.MaxX - opts.MarginMil,
		y1: snap.Outline.BBox.MaxY - opts.MarginMil,
	}
	if board.x1 <= board.x0 || board.y1 <= board.y0 {
		rep.Summary = fmt.Sprintf("board is smaller than 2×margin (%.0f mil) — shrink --margin", opts.MarginMil)
		return rep
	}

	axis := s.Axis()
	if axis == "auto" {
		axis = snap.Outline.longAxis()
	}
	rep.Axis = axis

	// 每个 flow 阶段收集它的器件与面积。
	byDes := snap.byDesignator()
	partModule := s.PartModule()
	claimed := map[string]bool{}

	stages := make([]*fpStage, 0, len(s.Flow))
	seen := map[string]bool{}
	for _, f := range s.Flow {
		k := strings.ToUpper(strings.TrimSpace(f))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		stages = append(stages, &fpStage{kind: k, modules: map[string]bool{}})
	}
	byKind := map[string]*fpStage{}
	for _, st := range stages {
		byKind[st.kind] = st
	}

	for des, m := range partModule {
		kind := m.KindOf()
		st := byKind[kind]
		if st == nil {
			continue
		}
		comp, onBoard := byDes[des]
		st.parts = append(st.parts, des)
		st.modules[m.Name] = true
		claimed[des] = true
		if !onBoard {
			continue // spec 声明了但板上还没有这个件：计入归属，不计入面积/质心
		}
		a := comp.area()
		if a <= 0 {
			a = 1 // 没有 bbox 的件仍应有话语权，给一个最小权重
		}
		cx, cy := comp.center()
		pos := cx
		if axis == "y" {
			pos = cy
		}
		st.area += a
		st.centroid += a * pos
	}
	for _, st := range stages {
		if st.area > 0 {
			st.centroid /= st.area
		} else {
			st.centroid = math.NaN()
		}
	}

	// 方向判定：把「有器件的阶段」按 flow 序号与实际质心做一次相关性比较。
	// 正相关 → 正向；负相关 → 板上是反着摆的，按反向切带。
	rep.Reversed = flowRunsReversed(stages)

	// 带宽按面积比例分配，但每条带不低于 MinBandMil。面积全为 0（空板）时等分。
	span := board.x1 - board.x0
	if axis == "y" {
		span = board.y1 - board.y0
	}
	widths := allocateBandWidths(stages, span, opts.MinBandMil, &rep)

	order := make([]int, len(stages))
	for i := range order {
		order[i] = i
	}
	if rep.Reversed {
		for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
			order[i], order[j] = order[j], order[i]
		}
	}

	cursor := board.x0
	if axis == "y" {
		cursor = board.y0
	}
	placed := make([]floorplanBand, len(stages))
	for _, idx := range order {
		st := stages[idx]
		w := widths[idx]
		var r cpRect
		if axis == "x" {
			r = cpRect{x0: cursor, y0: board.y0, x1: cursor + w, y1: board.y1}
		} else {
			r = cpRect{x0: board.x0, y0: cursor, x1: board.x1, y1: cursor + w}
		}
		cursor += w
		mods := make([]string, 0, len(st.modules))
		for m := range st.modules {
			mods = append(mods, m)
		}
		sort.Strings(mods)
		sort.Strings(st.parts)
		placed[idx] = floorplanBand{
			Kind: st.kind, Order: idx, Rect: r,
			MinX: round1(r.x0), MinY: round1(r.y0), MaxX: round1(r.x1), MaxY: round1(r.y1),
			Parts: st.parts, AreaMil2: math.Round(st.area), Modules: mods,
		}
		if len(st.parts) == 0 {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("flow stage %s has no parts on this board — its band is reserved but empty", st.kind))
		}
	}
	rep.Bands = placed

	// 板上有、但没被任何 flow 阶段认领的器件。它们不是错误（去耦、测试点常常不归域），
	// 但要报出来 —— 如果这个列表很长，说明 spec 的 modules[] 覆盖不全，
	// 后面 partition / flow-order 两维也会跟着失真。
	for _, c := range snap.Components {
		if !claimed[strings.ToUpper(c.Designator)] && !claimed[c.Designator] {
			rep.Unzoned = append(rep.Unzoned, c.Designator)
		}
	}
	sort.Strings(rep.Unzoned)
	if n := len(rep.Unzoned); n > 0 && len(snap.Components) > 0 {
		if pct := float64(n) / float64(len(snap.Components)); pct > 0.5 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(
				"%d/%d parts (%.0f%%) belong to no flow stage — the spec's modules[] cover less than half the board, so flow-order and partition scores will be weak",
				n, len(snap.Components), pct*100))
		}
	}

	rep.Pins = planEdgePins(s, snap, board)
	rep.OK = len(rep.Bands) >= 2
	dir := "→"
	if rep.Reversed {
		dir = "←"
	}
	rep.Summary = fmt.Sprintf("%d band(s) along %s %s, %d edge pin(s), %d unzoned part(s)",
		len(rep.Bands), axis, dir, len(rep.Pins), len(rep.Unzoned))
	return rep
}

// fpStage 是规划中的一个 flow 阶段：它认领的器件、总面积、沿轴的面积加权质心。
type fpStage struct {
	kind     string
	parts    []string
	modules  map[string]bool
	area     float64
	centroid float64 // 沿流向轴的面积加权质心；NaN = 该阶段板上无器件
}

// flowRunsReversed 判断板上既有分布是否与 flow 声明方向相反。
//
// 判据是成对比较：对每一对「都有器件」的阶段，看它们的质心顺序与 flow 序号顺序
// 是否一致。反向多于正向 → 板子是反着摆的，按反向切带。
//
// 为什么要有这一步：流向是**相对**的。板上从右到左走 电源→数字→RF→天线 与
// 从左到右一样好。如果不判方向，floorplan 会把一块本来就摆对的板整个翻过来重排，
// 而这在精修环里会表现成「每轮都在大幅搬件、分数却不涨」的震荡。
//
// 不足两个有器件的阶段（新板）无从判断，返回 false（按正向切）。
func flowRunsReversed(stages []*fpStage) bool {
	var agree, disagree int
	for i := 0; i < len(stages); i++ {
		for j := i + 1; j < len(stages); j++ {
			a, b := stages[i].centroid, stages[j].centroid
			if math.IsNaN(a) || math.IsNaN(b) || a == b {
				continue
			}
			if a < b {
				agree++ // 序号小的质心也小 = 与声明同向
			} else {
				disagree++
			}
		}
	}
	return disagree > agree
}

// allocateBandWidths 按面积比例分配带宽，并保证每条带不低于 minBand。
//
// 先给每条带 minBand 的底，剩余空间再按面积比例分。若 minBand × N 已经超过总跨度，
// 说明板子对这个 flow 太小了 —— 等分并给一条警告，而不是切出负宽度的带。
func allocateBandWidths(stages []*fpStage, span, minBand float64, rep *floorplanReport) []float64 {
	n := len(stages)
	out := make([]float64, n)
	if n == 0 || span <= 0 {
		return out
	}
	if minBand*float64(n) >= span {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"board span %.0f mil cannot give %d stages the %.0f mil minimum band — bands are equal-width and cramped; consider a bigger board or fewer flow stages",
			span, n, minBand))
		for i := range out {
			out[i] = span / float64(n)
		}
		return out
	}
	var total float64
	for _, st := range stages {
		total += st.area
	}
	rest := span - minBand*float64(n)
	for i, st := range stages {
		out[i] = minBand
		if total > 0 {
			out[i] += rest * (st.area / total)
		} else {
			out[i] += rest / float64(n) // 空板：等分
		}
	}
	return out
}

// planEdgePins 把有边意图的连接器钉到目标边的中点。
//
// 只处理 spec 明确给了 ref + edge 的接口 —— 这一步是**布局骨架**，猜错一个连接器
// 的边比不猜代价大得多（边序是装配体验，agent 猜不了，SKILL 的停点表把它列为
// 必须用户确认的项）。没有 ref/edge 的接口留给 place-constrained 的既有分档逻辑。
func planEdgePins(s *spec.Spec, snap *boardSnapshot, board cpRect) []floorplanPin {
	if s == nil {
		return nil
	}
	byDes := snap.byDesignator()
	var out []floorplanPin
	for ref, in := range s.InterfaceByRef() {
		edge := strings.ToLower(strings.TrimSpace(in.Edge))
		if edge == "" || edge == "any" {
			continue
		}
		comp, ok := byDes[ref]
		if !ok {
			// spec 声明了但板上没有 —— 常见于还没导入 PCB 的阶段，不是错误。
			continue
		}
		var tx, ty float64
		switch edge {
		case "left":
			tx, ty = board.x0, (board.y0+board.y1)/2
		case "right":
			tx, ty = board.x1, (board.y0+board.y1)/2
		case "bottom":
			tx, ty = (board.x0+board.x1)/2, board.y0
		case "top":
			tx, ty = (board.x0+board.x1)/2, board.y1
		default:
			continue
		}
		out = append(out, floorplanPin{
			Designator: comp.Designator,
			Edge:       edge,
			Facing:     in.FacingOf(),
			Source:     "spec",
			TargetX:    round1(tx),
			TargetY:    round1(ty),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Designator < out[j].Designator })
	return out
}
