package app

// cmd_sch_zone_plan.go — `sch zone-plan` + `sch zone-draw --mode partition`:
// a DATA-DRIVEN A4 functional-partition planner (issue #149).
//
// The legacy `zone-draw` (zones mode) resolves each claim to a FIXED 3×2 grid
// cell (zoneRect) — it can't express "carve the whole sheet into sensible
// functional regions and leave the bottom-right title block a gap". This planner
// instead derives partition rectangles from the LIVE geometry: usable sheet minus
// a margin, split into columns/rows at the NATURAL gaps between module clusters
// (not fixed fractions), each partition lifted clear of the title-block keep-out
// and reserving a big-font title band. Pure core (planPartitions) → unit-testable
// against the issue's real 6-module A4 page; the draw path goes through the same
// debug.exec_js graphics hatch `zone-draw` uses, persisted per-page (documentUuid).

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// partitionModule is one functional module: a name + the union bbox of its parts.
type partitionModule struct {
	Name string `json:"name"`
	// BBox 是画框口径(器件 ∪ 近旁 marker——旗要被框住,live 2026-08-12:GND 全
	// 垂出框外);CoreBBox 是校验口径(仅器件):moduleOutsideZone / titleBlockHits
	// / labelCollisions 用它——旗贴图签安全带或与区名带擦边是注释级余量问题,
	// 不该 hard-block 整个分区框。CoreBBox 零值时回退 BBox(手写测试兼容)。
	BBox     layoutBBox `json:"bbox"`
	CoreBBox layoutBBox `json:"coreBBox,omitempty"`
}

// moduleCoreBBox 返回校验口径 bbox(CoreBBox 未设置时回退 BBox)。
func moduleCoreBBox(m partitionModule) layoutBBox {
	z := layoutBBox{}
	if m.CoreBBox == z {
		return m.BBox
	}
	return m.CoreBBox
}

// partitionRect is one planned partition: the rectangle, its title band, and the
// modules assigned to it.
type partitionRect struct {
	Modules   []string   `json:"modules"`
	BBox      layoutBBox `json:"bbox"`
	TitleBBox layoutBBox `json:"titleBBox"`
	// NoteBBox 是框内底部留给电路说明的一条带(区名在顶、说明在底,都在框内)。
	NoteBBox layoutBBox `json:"noteBBox"`
}

// partitionValidation counts every way a plan can be wrong (all should be 0).
type partitionValidation struct {
	SheetOverflow     int `json:"sheetOverflow"`
	PartitionOverlap  int `json:"partitionOverlap"`
	TitleBlockHits    int `json:"titleBlockHits"`
	ModuleOutsideZone int `json:"moduleOutsideZone"`
	LabelCollisions   int `json:"labelCollisions"`
	// SheetMarginHits counts frame edges that hug the sheet border closer than
	// sheetEdgeMinGap — a frame flush against the printed sheet frame reads as a
	// confusing double line (live feedback 2026-08-11).
	SheetMarginHits int `json:"sheetMarginHits"`
}

func (v partitionValidation) clean() bool {
	return v.SheetOverflow == 0 && v.PartitionOverlap == 0 && v.TitleBlockHits == 0 &&
		v.ModuleOutsideZone == 0 && v.LabelCollisions == 0 && v.SheetMarginHits == 0
}

// titleBlockSafety is the extra clearance (schematic units) kept between a
// partition frame and the DERIVED title-block keep-out, and the tolerance the
// validator checks against. The keep-out is a ratio ESTIMATE (known-template-ratio,
// see deriveSheetGeometry) that can undershoot the rendered table; lifting by
// gutter/2=6 alone let a frame's bottom edge visibly cross the 原理图/Schematic1
// row while validation (checked against the same bare estimate) still read
// titleBlockHits=0 — a false green (live 2026-08-11). One constant is shared by
// BOTH the lift and the check so "how far we lift" and "what we gate on" can
// never drift apart again — that drift (lift by gutter/2, validate against the
// bare keepout) was the root cause. 30 (not more): HeightFrac 0.24 already covers
// the rendered table, so this is pure margin; legitimate boards place modules as
// close as ~34 above the keep-out (real six-module fixture) and must stay clean.
const titleBlockSafety = 30.0

// sheetEdgeMinGap is the minimum distance a partition frame edge must keep from
// the sheet border (the printed frame), feeding SheetMarginHits.
const sheetEdgeMinGap = 12.0

// partitionContentPad is how far a partition frame extends beyond its modules'
// union bbox. Frames used to span the FULL column/row band, so a single-module
// column drew a near-page-height frame around a 230-unit cluster (visual bloat,
// live 2026-08-11); now the frame hugs content + this pad, clamped to its band.
const partitionContentPad = 24.0

// inflatedTitleKeepout grows the estimated keep-out by titleBlockSafety on every
// side — the shared basis for the partition lift AND the validator.
func inflatedTitleKeepout(keepout *layoutBBox) *layoutBBox {
	if keepout == nil {
		return nil
	}
	return &layoutBBox{
		MinX: keepout.MinX - titleBlockSafety, MinY: keepout.MinY - titleBlockSafety,
		MaxX: keepout.MaxX + titleBlockSafety, MaxY: keepout.MaxY + titleBlockSafety,
	}
}

type partitionPlan struct {
	Sheet      layoutBBox          `json:"sheet"`
	Keepout    *layoutBBox         `json:"keepout,omitempty"`
	Partitions []partitionRect     `json:"partitions"`
	Validation partitionValidation `json:"validation"`
}

type partitionOpts struct {
	Margin    float64
	Gutter    float64
	TitleBand float64
	// NoteBand 是分区**底部**留给电路说明的一条带。顶上有标题带,底下就该有说明带 ——
	// 否则说明只能挤在器件缝里,挤不下就掉到框外(实测:自动落点退到框下方 y=215,
	// 用户一眼看出「说明跑到框外面了」)。版式是:区名左上、说明左下,**都在框内**。
	NoteBand float64
	MaxCols  int
	MaxRows  int
}

func defaultPartitionOpts() partitionOpts {
	// Margin 20 → 28 (2026-08-11): at 20 the frame sat 26 units from the sheet
	// edge, hugging the printed sheet frame like a double line.
	return partitionOpts{Margin: 28, Gutter: 12, TitleBand: 30, NoteBand: 26, MaxCols: 3, MaxRows: 2}
}

// planPartitions is the pure planner: usable sheet (minus margin) carved into
// column/row bands at the natural gaps between module clusters, each partition
// lifted above the title-block keep-out and given a top title band. Deterministic.
func planPartitions(sheet layoutBBox, keepout *layoutBBox, modules []partitionModule, opts partitionOpts) partitionPlan {
	plan := partitionPlan{Sheet: sheet, Keepout: keepout}
	usable := layoutBBox{
		MinX: sheet.MinX + opts.Margin, MinY: sheet.MinY + opts.Margin,
		MaxX: sheet.MaxX - opts.Margin, MaxY: sheet.MaxY - opts.Margin,
	}
	if len(modules) == 0 || usable.MaxX <= usable.MinX || usable.MaxY <= usable.MinY {
		return plan
	}

	cx := make([]float64, len(modules))
	cy := make([]float64, len(modules))
	colIvs := make([]axisInterval, len(modules))
	rowIvs := make([]axisInterval, len(modules))
	for i, m := range modules {
		// 分割/归属判定用 CORE 口径(器件本体):模块间的结构空隙由本体决定——
		// draw 口径的旗/说明外伸(如 U2 右侧 netport 与邻区标签交叠)不该抹掉
		// 本体之间的真实分割空隙(live 2026-08-12:MCU/LED 因此被误合一框)。
		// 框尺寸仍用 draw 口径(rect 段),外伸物在 cell 边界处被 clamp,微露可容。
		core := moduleCoreBBox(m)
		cx[i], cy[i] = bboxCenter(core)
		colIvs[i] = axisInterval{core.MinX, core.MaxX, cx[i]}
		rowIvs[i] = axisInterval{core.MinY, core.MaxY, cy[i]}
	}
	// Split at the natural EMPTY BAND between module bboxes (edge-to-edge), not the
	// midpoint of centers — a tall module (主MCU) whose bbox straddles a center-gap
	// split would end up outside its partition (issue #149). Require the gap to hold
	// the gutter so adjacent partitions don't collide.
	colBounds := boundsFrom(usable.MinX, usable.MaxX, clusterSplits(colIvs, opts.Gutter, opts.MaxCols))
	rowBounds := boundsFrom(usable.MinY, usable.MaxY, clusterSplits(rowIvs, opts.Gutter, opts.MaxRows))

	type cellKey struct{ c, r int }
	cells := map[cellKey][]int{}
	var order []cellKey
	for i := range modules {
		k := cellKey{bandIndex(cx[i], colBounds), bandIndex(cy[i], rowBounds)}
		if _, ok := cells[k]; !ok {
			order = append(order, k)
		}
		cells[k] = append(cells[k], i)
	}
	// Deterministic: visual top (large y, y-UP) first, then left→right.
	sort.Slice(order, func(i, j int) bool {
		if order[i].r != order[j].r {
			return order[i].r > order[j].r
		}
		return order[i].c < order[j].c
	})

	// 图签安全带:说明带撞上它可以缩,**内容不许缩**(见 rect 后的收拢)。
	safe := inflatedTitleKeepout(keepout)
	// 网格只用来**决定谁跟谁同一组**(哪些模块合成一个分区),不再参与矩形的尺寸 ——
	// 尺寸一律由成员虚拟组的并集决定(见下面 rect 的注释)。
	for _, k := range order {
		content := modules[cells[k][0]].BBox
		for _, i := range cells[k][1:] {
			b := modules[i].BBox
			if b.MinX < content.MinX {
				content.MinX = b.MinX
			}
			if b.MinY < content.MinY {
				content.MinY = b.MinY
			}
			if b.MaxX > content.MaxX {
				content.MaxX = b.MaxX
			}
			if b.MaxY > content.MaxY {
				content.MaxY = b.MaxY
			}
		}
		// **框 = 成员虚拟组体积的并集 + 边距 + 上标题带 + 下说明带,不做任何裁剪。**
		//
		// 此前这里把矩形 clamp 到网格单元(`math.Min(cell.MaxX, …)`),于是模块的
		// 虚拟组一旦跨过单元边界,框就被切短 —— 地旗、网络标签垂在框外(用户截图实证
		// D1 的 GND)。「框住自己的内容」必须是**构造保证**而不是检查项:算得出来的东西
		// 不该留给判据去发现、更不该留给人去看图。
		//
		// 去掉 clamp 之后,moduleOutsideZone 结构上恒为 0(它降级成一条后置断言:
		// 真报出来说明这里的算术错了)。**代价是框之间可能重叠** —— 但那不是画框的
		// 毛病,是布局的事实:两个模块的虚拟组本身交叠时,不存在既包住又互不重叠的
		// 一组矩形。那件事由 partitionOverlap 如实报出来,修法是挪件(S3 的组间留通道),
		// 不是把框切短来掩盖。
		rect := layoutBBox{
			MinX: content.MinX - partitionContentPad,
			MinY: content.MinY - partitionContentPad - opts.NoteBand,
			MaxX: content.MaxX + partitionContentPad,
			MaxY: content.MaxY + partitionContentPad + opts.TitleBand,
		}
		// 说明带/标题带是**我们加的预留**,不是内容:它撞上图签就缩,而
		// 「content ± pad」这一圈是构造保证,一步都不让。于是「框住自己的内容」
		// 永远成立,而「不压图签」在装得下时也成立;两者真冲突时(模块自己压到图签)
		// 由 titleBlockHits 如实报出来,修法是把模块挪上去。
		if safe != nil && boxesOverlap(rect, *safe) {
			// 让到**内容下沿**为止:边距和说明带都可以被图签吃掉,内容一寸不让。
			if lift := math.Min(safe.MaxY, content.MinY); lift > rect.MinY {
				rect.MinY = lift
			}
		}
		band := opts.TitleBand
		if h := rect.MaxY - rect.MinY; band > h/2 {
			band = h / 2
		}
		names := make([]string, 0, len(cells[k]))
		for _, i := range cells[k] {
			names = append(names, modules[i].Name)
		}
		sort.Strings(names)
		plan.Partitions = append(plan.Partitions, partitionRect{
			Modules: names,
			BBox:    rect,
			// Title band at the visual TOP (large y).
			TitleBBox: layoutBBox{MinX: rect.MinX, MinY: rect.MaxY - band, MaxX: rect.MaxX, MaxY: rect.MaxY},
			// Note band at the visual BOTTOM (small y) —— 说明就放这儿,框内左下。
			NoteBBox: layoutBBox{MinX: rect.MinX, MinY: rect.MinY, MaxX: rect.MaxX,
				MaxY: math.Min(rect.MaxY, rect.MinY+opts.NoteBand)},
		})
	}
	plan.Validation = validatePartitions(plan, modules, keepout)
	return plan
}

// axisInterval is a module's extent on one axis (min, max) plus its center, used
// to place partition splits in the EMPTY BAND between modules rather than through
// a straddling module's body.
type axisInterval struct{ lo, hi, center float64 }

// clusterSplits returns the inner split coordinates (≤ maxK-1 of them) placed at
// the midpoints of the LARGEST empty bands between adjacent module intervals. A
// band smaller than minGap (the gutter) is skipped — there's no room for two
// partitions there — and overlapping intervals (negative band) never split (the
// modules are separable on the OTHER axis instead).
func clusterSplits(ivs []axisInterval, minGap float64, maxK int) []float64 {
	if len(ivs) <= 1 || maxK <= 1 {
		return nil
	}
	s := append([]axisInterval(nil), ivs...)
	sort.Slice(s, func(i, j int) bool { return s[i].center < s[j].center })
	type gap struct{ size, mid float64 }
	var gaps []gap
	for i := 1; i < len(s); i++ {
		band := s[i].lo - s[i-1].hi
		if band < minGap {
			continue
		}
		gaps = append(gaps, gap{band, (s[i].lo + s[i-1].hi) / 2})
	}
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].size != gaps[j].size {
			return gaps[i].size > gaps[j].size
		}
		return gaps[i].mid < gaps[j].mid
	})
	var splits []float64
	for _, g := range gaps {
		if len(splits) >= maxK-1 {
			break
		}
		splits = append(splits, g.mid)
	}
	sort.Float64s(splits)
	return splits
}

func boundsFrom(lo, hi float64, splits []float64) []float64 {
	b := make([]float64, 0, len(splits)+2)
	b = append(b, lo)
	b = append(b, splits...)
	return append(b, hi)
}

// bandIndex returns the band [bounds[i],bounds[i+1]) that v falls into.
func bandIndex(v float64, bounds []float64) int {
	for i := 0; i+1 < len(bounds); i++ {
		if v < bounds[i+1] {
			return i
		}
	}
	return len(bounds) - 2
}

func bboxContains(outer, inner layoutBBox) bool {
	const eps = 0.01
	return inner.MinX >= outer.MinX-eps && inner.MinY >= outer.MinY-eps &&
		inner.MaxX <= outer.MaxX+eps && inner.MaxY <= outer.MaxY+eps
}

func validatePartitions(plan partitionPlan, modules []partitionModule, keepout *layoutBBox) partitionValidation {
	var v partitionValidation
	ps := plan.Partitions
	// Same inflated basis the planner lifts with (titleBlockSafety): validating
	// against the bare estimate while lifting by a different amount is exactly the
	// false-green this replaced.
	safe := inflatedTitleKeepout(keepout)
	for _, p := range ps {
		if !bboxContains(plan.Sheet, p.BBox) {
			v.SheetOverflow++
		}
		if safe != nil && boxesOverlap(p.BBox, *safe) {
			v.TitleBlockHits++
		}
		// A frame edge hugging the printed sheet frame reads as a double line.
		if p.BBox.MinX-plan.Sheet.MinX < sheetEdgeMinGap || plan.Sheet.MaxX-p.BBox.MaxX < sheetEdgeMinGap ||
			p.BBox.MinY-plan.Sheet.MinY < sheetEdgeMinGap || plan.Sheet.MaxY-p.BBox.MaxY < sheetEdgeMinGap {
			v.SheetMarginHits++
		}
	}
	for i := 0; i < len(ps); i++ {
		for j := i + 1; j < len(ps); j++ {
			if boxesOverlap(ps[i].BBox, ps[j].BBox) {
				v.PartitionOverlap++
			}
		}
	}
	partOf := map[string]layoutBBox{}
	for _, p := range ps {
		for _, name := range p.Modules {
			partOf[name] = p.BBox
		}
	}
	for _, m := range modules {
		pb, ok := partOf[m.Name]
		// **按整个 L1 虚拟组判,不是按器件本体**:框的职责是「框住这个模块」,而模块的
		// 体积包含它自己的 marker/桩线。只判本体时,框可以把地旗、网络标签甩在外面
		// 却依然报 clean —— 用户截图一眼看出 D1 的 GND 垂在 ESD 框外,而 validation
		// 五项全 0。判据必须判所见。
		if !ok || !bboxContains(pb, m.BBox) {
			v.ModuleOutsideZone++
		}
	}
	// A title band overlapping a module body would put the big title on top of a
	// symbol (label collision).
	for _, p := range ps {
		for _, m := range modules {
			if strInSlice(p.Modules, m.Name) && boxesOverlap(p.TitleBBox, moduleCoreBBox(m)) {
				v.LabelCollisions++
			}
		}
	}
	return v
}

func strInSlice(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// modulesFromClaims builds the planner input from `sch zones` claims: each module's
// bbox is the union of its parts' live bboxes. Modules whose parts aren't on the
// active page (no bbox) are skipped.
// schModuleMarkerReach is how far beyond a claimed part's bbox its net markers
// (stub flags) are folded into the module bbox: stub offset (~18-24) + flag body
// (~21-35). Without this the partition frame hugs the PART bboxes only and every
// downward GND flag dangles OUTSIDE the frame (live 2026-08-12: all three POWER
// caps' GND rendered below the frame).
const schModuleMarkerReach = 60.0

// modulesFromClaims 把认领折成模块 bbox。
//
// clusterOf 是**按导线归属**算出的「器件 + 只挂在它自己引脚上的 marker/桩线」体积
// (`buildSchClusters`)。有它就用它 —— 框住的必须是整个 L1 虚拟组,而不是器件本体:
// 用户截图实证 D1 的 GND 旗垂在 ESD 框外面。nil 时退回旧的**距离启发式**折叠
// (`foldMarkers`):按锚点离模块 bbox 多远来收编,够不着的旗就漏在框外,而且可能
// 把邻居的旗收进来 —— 这正是「归属靠距离」的老毛病,能拿到导线就别用它。
func modulesFromClaims(zones map[string]*schZoneClaim, comps []layoutComp,
	clusterOf map[string]layoutBBox) []partitionModule {
	byDesig := map[string]layoutComp{}
	for _, c := range comps {
		if c.Designator != "" && c.BBox != nil {
			byDesig[strings.ToUpper(c.Designator)] = c
		}
	}
	// Markers with a bbox, for the marker-reach fold below.
	var markers []layoutComp
	for _, c := range comps {
		if isSchMarker(c.ComponentType) && c.BBox != nil && c.AnchorAvailable {
			markers = append(markers, c)
		}
	}
	foldMarkers := func(u *layoutBBox) {
		for _, m := range markers {
			if m.X >= u.MinX-schModuleMarkerReach && m.X <= u.MaxX+schModuleMarkerReach &&
				m.Y >= u.MinY-schModuleMarkerReach && m.Y <= u.MaxY+schModuleMarkerReach {
				u.MinX = minF(u.MinX, m.BBox.MinX)
				u.MinY = minF(u.MinY, m.BBox.MinY)
				u.MaxX = maxF(u.MaxX, m.BBox.MaxX)
				u.MaxY = maxF(u.MaxY, m.BBox.MaxY)
			}
		}
	}
	var names []string
	for n := range zones {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []partitionModule
	for _, name := range names {
		zc := zones[name]
		if zc == nil {
			continue
		}
		var u, core *layoutBBox
		grow := func(dst **layoutBBox, b layoutBBox) {
			if *dst == nil {
				c := b
				*dst = &c
				return
			}
			(*dst).MinX = minF((*dst).MinX, b.MinX)
			(*dst).MinY = minF((*dst).MinY, b.MinY)
			(*dst).MaxX = maxF((*dst).MaxX, b.MaxX)
			(*dst).MaxY = maxF((*dst).MaxY, b.MaxY)
		}
		for _, d := range zc.Parts {
			key := strings.ToUpper(d)
			c, ok := byDesig[key]
			if !ok {
				continue
			}
			grow(&core, *c.BBox) // core = 器件本体(压图签时按它收拢)
			if cb, has := clusterOf[key]; has {
				grow(&u, cb) // 画框口径 = 整个 L1 虚拟组(本体 ∪ 它自己的 marker/桩线)
				continue
			}
			grow(&u, *c.BBox)
		}
		if u != nil {
			if clusterOf == nil {
				foldMarkers(u) // 没有导线归属时的兜底:按距离收编附近的旗
			}
			out = append(out, partitionModule{Name: name, BBox: *u, CoreBBox: *core})
		}
	}
	return out
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// buildPartitionDrawJS renders the exec_js that draws every partition rect + its
// big-font title, returning their ids. Pure (unit-testable).
func buildPartitionDrawJS(plan partitionPlan, fontSize float64, color string) string {
	var b strings.Builder
	writeZoneDrawPrelude(&b)
	colorJS, _ := json.Marshal(color)
	for _, p := range plan.Partitions {
		if !writeZoneRectangleCreateJS(&b, p.BBox, colorJS) {
			continue
		}
		title, _ := json.Marshal(strings.Join(p.Modules, " / "))
		titleText := strings.Join(p.Modules, " / ")
		fmt.Fprintf(&b, "  if (!rc) throw new Error(%q);\n", "rectangle create returned undefined for "+titleText)
		fmt.Fprintf(&b, "  const rid = rc.getState_PrimitiveId(); if (!rid) { await eda.sch_PrimitiveRectangle.delete(rc); throw new Error(%q); } rects.push(rid);\n",
			"rectangle id missing for "+titleText)
		// Title baseline sits fontSize below the band top (larger y = higher on the
		// y-up canvas) so the rendered glyph box stays inside the frame (issue #149:
		// a 22pt title anchored at the very top spilled ~6 units over the edge).
		tx := p.TitleBBox.MinX + 4
		ty := p.TitleBBox.MaxY - fontSize
		fmt.Fprintf(&b, "  const tt = await eda.sch_PrimitiveText.create(%g, %g, %s, 0, %s, null, %g);\n",
			tx, ty, title, colorJS, fontSize)
		fmt.Fprintf(&b, "  if (!tt) throw new Error(%q);\n", "text create returned undefined for "+titleText)
		fmt.Fprintf(&b, "  const tid = tt.getState_PrimitiveId(); if (!tid) { await eda.sch_PrimitiveText.delete(tt); throw new Error(%q); } texts.push(tid); }\n",
			"text id missing for "+titleText)
	}
	writeZoneDrawEpilogue(&b)
	return b.String()
}

// newSchZonePlanCmd builds `sch zone-plan` — compute + print the partition plan
// (no mutation). --json emits the full plan + validation.
func newSchZonePlanCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var asJSON bool
	var margin, gutter, titleBand float64
	var maxCols, maxRows int
	c := &cobra.Command{
		Use:   "zone-plan",
		Short: "Plan data-driven A4 functional partitions from the live sheet + module bboxes (no mutation)",
		Long: `Compute a whole-sheet functional partition plan (issue #149) from the LIVE
geometry: usable sheet (minus margin) carved into columns/rows at the natural gaps
between module clusters, each partition lifted clear of the title-block keep-out and
given a big-font title band. Reads modules from ` + "`sch zones`" + ` claims (each
module's bbox = union of its parts). Pure计算 — prints the plan + validation
(sheetOverflow / partitionOverlap / titleBlockHits / moduleOutsideZone /
labelCollisions, all should be 0). Draw it with ` + "`sch zone-draw --mode partition`" + `.`,
		Example: `  easyeda sch zones set --spec s0.json --project ceshi
  easyeda sch zone-plan --project ceshi --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			pinnedCfg, win, docUUID, err := pinZonePage(cfg, *window)
			if err != nil {
				return err
			}
			plan, _, err := computePartitionPlan(pinnedCfg, win, docUUID,
				partitionOptsFrom(margin, gutter, titleBand, maxCols, maxRows))
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(plan)
			}
			renderPartitionPlan(plan, stdout)
			if !plan.Validation.clean() {
				return fmt.Errorf("zone-plan: validation not clean (%+v)", plan.Validation)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit the full plan + validation as JSON")
	// Defaults from defaultPartitionOpts — single source, no flag/planner drift.
	def := defaultPartitionOpts()
	c.Flags().Float64Var(&margin, "margin", def.Margin, "page margin inset from the sheet edge")
	c.Flags().Float64Var(&gutter, "gutter", def.Gutter, "gutter between adjacent partitions")
	c.Flags().Float64Var(&titleBand, "title-band", def.TitleBand, "height of each partition's title band")
	c.Flags().IntVar(&maxCols, "max-cols", 3, "maximum partition columns")
	c.Flags().IntVar(&maxRows, "max-rows", 2, "maximum partition rows")
	return c
}

func partitionOptsFrom(margin, gutter, titleBand float64, maxCols, maxRows int) partitionOpts {
	o := defaultPartitionOpts()
	if margin > 0 {
		o.Margin = margin
	}
	if gutter > 0 {
		o.Gutter = gutter
	}
	if titleBand > 0 {
		o.TitleBand = titleBand
	}
	if maxCols > 0 {
		o.MaxCols = maxCols
	}
	if maxRows > 0 {
		o.MaxRows = maxRows
	}
	return o
}

// computePartitionPlan pulls claims + live geometry for one pinned page and runs
// the planner. The claims lookup never consults the mutable foreground tab, and
// the geometry response must prove it came from the same document UUID.
func computePartitionPlan(cfg *appConfig, window, docUUID string, opts partitionOpts) (partitionPlan, map[string]*schZoneClaim, error) {
	// **模块归属的单一事实来源是虚拟组**:`block-apply` 已按功能子群把件封成了组
	// (J_USB / D_ESD / U…),那就是「哪几件是一个功能单元」。让 `sch zones set` 再抄一份
	// 成员列表只会多一处会漂移的副本 —— 件被 group-move 挪走或删掉,认领不会跟着变。
	// 没有组时才回落到 zone 认领:手工搭的页,或只想给 autolayout 指定落位目标格的场景。
	zones := schGroupModules(cfg, window, docUUID)
	project := ""
	if len(zones) == 0 {
		var err error
		zones, project, err = loadSchZoneClaimsForPage(cfg, window, docUUID)
		if err != nil {
			return partitionPlan{}, nil, err
		}
	}
	if len(zones) == 0 {
		return partitionPlan{}, nil, fmt.Errorf("%q 这一页既没有虚拟组也没有 zone 认领 —— 用 `sch block-apply` 落块(自动按功能子群归组),或手工 `sch group create` / `sch zones set`", project)
	}
	if err := ensureActiveDoc(cfg, window); err != nil {
		return partitionPlan{}, nil, fmt.Errorf("zone-plan: restore pinned page %s: %w", docUUID, err)
	}
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", window,
		map[string]any{"includeBBox": true}, docUUID, "read partition geometry")
	if err != nil {
		return partitionPlan{}, nil, err
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return partitionPlan{}, nil, perr
	}
	sheet := sheetBBoxOf(comps)
	if sheet == nil {
		return partitionPlan{}, nil, fmt.Errorf("no sheet bbox on the active page — `easyeda doc switch` to the schematic page first")
	}
	keepout, _ := titleBlockKeepout(sheet)
	// 框住的是「器件 + 它自己的 marker/桩线」(L1 虚拟组),不是器件本体 ——
	// 归属走导线,不靠距离。读不到导线就退回旧的距离启发式(会漏远处的旗)。
	var clusterOf map[string]layoutBBox
	if wires, werr := fetchSchWirePolylines(cfg, window, docUUID); werr == nil {
		if cs, _ := buildSchClusters(comps, wires); len(cs) > 0 {
			clusterOf = map[string]layoutBBox{}
			for _, c := range cs {
				clusterOf[strings.ToUpper(c.Designator)] = c.Box
			}
		}
	}
	modules := modulesFromClaims(zones, comps, clusterOf)
	if len(modules) == 0 {
		return partitionPlan{}, nil, fmt.Errorf("no module bboxes resolved — the claimed parts aren't on this page (place them / `doc switch`)")
	}
	// 功能区对象模型:登记的说明 note(claim.NoteIDs)是区的内置对象——把它们的
	// 估算 bbox fold 进对应模块的画框口径(CoreBBox 不动:说明是注释,不参与
	// 图签/区名带的硬校验)。text 无 bbox API,按内容行数×字号估算;读取失败仅
	// 降级警告(说明不该阻断画框)。
	foldZoneNotesIntoModules(cfg, window, docUUID, zones, modules)
	return planPartitions(*sheet, keepout, modules, opts), zones, nil
}

// schNoteBBoxEstimate 估算一条文本的渲染 bbox:锚点为左上(y-UP 向下排行),
// 行高 ≈ fontSize×1.3,宽 ≈ 最长行字符宽(CJK ≈ fontSize,ASCII ≈ 0.55×fontSize)。
// 尺寸口径由 noteSizeOf 独家提供 —— `sch note` 的自动落点求解器用同一个函数
// 估算候选 bbox。两套估算一旦分家,就会出现"求解时说不撞、画框时说撞"。
func schNoteBBoxEstimate(t zoneMoveText) layoutBBox {
	w, h := noteSizeOf(t.Content, t.FontSize)
	return noteAnchorBBox(t.X, t.Y, w, h)
}

// foldZoneNotesIntoModules 把每个区登记的 note bbox 并进该区模块的 BBox(画框
// 口径)。best-effort:text.list 失败只警告。
func foldZoneNotesIntoModules(cfg *appConfig, window, docUUID string, zones map[string]*schZoneClaim, modules []partitionModule) {
	needed := false
	for _, zc := range zones {
		if zc != nil && len(zc.NoteIDs) > 0 {
			needed = true
			break
		}
	}
	if !needed {
		return
	}
	res, err := requestAutolayoutAction(cfg, "schematic.text.list", window, map[string]any{}, docUUID, "read zone notes")
	if err != nil {
		return // best-effort:说明 fold 失败不阻断画框
	}
	texts := parseZoneMoveTexts(res.Result)
	byID := map[string]zoneMoveText{}
	for _, t := range texts {
		byID[t.ID] = t
	}
	for i := range modules {
		zc := zones[modules[i].Name]
		if zc == nil {
			continue
		}
		for _, nid := range zc.NoteIDs {
			t, ok := byID[nid]
			if !ok {
				continue // 登记的 note 已被删(stale 登记)——list 时静默跳过
			}
			nb := schNoteBBoxEstimate(t)
			b := &modules[i].BBox
			b.MinX = minF(b.MinX, nb.MinX)
			b.MinY = minF(b.MinY, nb.MinY)
			b.MaxX = maxF(b.MaxX, nb.MaxX)
			b.MaxY = maxF(b.MaxY, nb.MaxY)
		}
	}
}

func renderPartitionPlan(plan partitionPlan, w io.Writer) {
	fmt.Fprintf(w, "zone-plan: %d partition(s) on sheet (%.0f,%.0f)..(%.0f,%.0f)\n",
		len(plan.Partitions), plan.Sheet.MinX, plan.Sheet.MinY, plan.Sheet.MaxX, plan.Sheet.MaxY)
	for _, p := range plan.Partitions {
		fmt.Fprintf(w, "  [%s]  (%.0f,%.0f)..(%.0f,%.0f)\n",
			strings.Join(p.Modules, " / "), p.BBox.MinX, p.BBox.MinY, p.BBox.MaxX, p.BBox.MaxY)
	}
	v := plan.Validation
	fmt.Fprintf(w, "validation: sheetOverflow=%d partitionOverlap=%d titleBlockHits=%d moduleOutsideZone=%d labelCollisions=%d\n",
		v.SheetOverflow, v.PartitionOverlap, v.TitleBlockHits, v.ModuleOutsideZone, v.LabelCollisions)
	if v.clean() {
		fmt.Fprintln(w, "✓ plan is clean")
	} else {
		fmt.Fprintln(w, "✗ plan has violations — adjust margins/gutter or the zone claims")
	}
}

// runPartitionDraw draws (or clears) the partition frames, persisted per-page.
func runPartitionDraw(cfg *appConfig, window string, opts partitionOpts, fontSize float64, color string, clear bool, stdout, stderr io.Writer) error {
	pinnedCfg, win, docUUID, err := pinZonePage(cfg, window)
	if err != nil {
		return err
	}
	project, err := resolveStageProject(pinnedCfg, win)
	if err != nil {
		return err
	}
	st, err := loadPcbStageState(project)
	if err != nil {
		return err
	}
	exec := func(phase, code string) (map[string]any, error) {
		return execAutolayoutZoneJS(pinnedCfg, win, docUUID, phase, code)
	}

	if clear {
		hadPrevious, cerr := clearPriorZoneFrames(st, docUUID, exec, stderr)
		if cerr != nil {
			return cerr
		}
		if !hadPrevious {
			fmt.Fprintln(stdout, "no zone frames recorded for this page — nothing to clear")
			return nil
		}
		if err := saveZoneDocument(pinnedCfg, win, docUUID, "save cleared partition frames"); err != nil {
			return err
		}
		if err := savePcbStageState(st); err != nil {
			return fmt.Errorf("persist cleared partition-frame state: %w", err)
		}
		fmt.Fprintln(stdout, "partition frames cleared and schematic saved for this page")
		return nil
	}

	// Finish all read-only planning/validation before deleting a prior good frame.
	plan, _, err := computePartitionPlan(pinnedCfg, win, docUUID, opts)
	if err != nil {
		return err
	}
	if !plan.Validation.clean() {
		return fmt.Errorf("partition plan has violations %+v — refusing to draw overlapping/out-of-sheet annotations", plan.Validation)
	}
	if fontSize <= 0 {
		fontSize = defaultPartitionZoneFontSize
	}
	if _, err := clearPriorZoneFrames(st, docUUID, exec, stderr); err != nil {
		return err
	}
	v, err := exec("draw partition frames", buildPartitionDrawJS(plan, fontSize, color))
	if err != nil {
		return err
	}
	frames, verr := validateZoneDrawResult(v, len(plan.Partitions))
	if verr != nil {
		return compensateZoneDraw(pinnedCfg, win, docUUID, st, "partition", exec, frames, verr)
	}
	setRecordedZoneFrames(st, docUUID, "partition", frames)
	if err := savePcbStageState(st); err != nil {
		return compensateZoneDraw(pinnedCfg, win, docUUID, st, "partition", exec, frames,
			fmt.Errorf("persist partition-frame ids: %w", err))
	}
	if err := saveZoneDocument(pinnedCfg, win, docUUID, "save partition zone frames"); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "drew %d partition frame(s) + %d title(s) on page %s; schematic saved\n",
		len(frames.Rects), len(frames.Texts), docUUID)
	return nil
}
