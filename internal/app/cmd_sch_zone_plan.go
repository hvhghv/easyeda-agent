package app

// cmd_sch_zone_plan.go — `sch zone-plan` + `sch zone-draw --mode partition`:
// a DATA-DRIVEN A4 functional-partition planner (issue #149).
//
// The legacy `zone-draw` (zones mode) resolved each claim to a FIXED 3×2 grid
// cell (zoneRect) — it couldn't express "carve the whole sheet into sensible
// functional regions and leave the bottom-right title block a gap". This planner
// instead derives partition rectangles from the LIVE geometry: **一个虚拟组 /
// zone 认领 = 一个分区**(partitionGrouping,与 `zone-arrange` phase B 同一把尺),
// 框 = 成员 L1 虚拟组体积并集 + 边距 + 区名带 + 说明带,再让开图签 keep-out 与纸边。
// Pure core (planPartitions) → unit-testable against the issue's real 6-module A4
// page; the draw path goes through the same debug.exec_js graphics hatch
// `zone-draw` uses, persisted per-page (documentUuid).

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
	// NoteHeight / NoteWidth 是该模块**已登记说明**(claim.NoteIDs)里最大的一条
	// 渲染高/宽(noteSizeOf,内容+字号 → 行数×行高 / 最长行宽)。说明带按这两个
	// 维度预留(见 planPartitions 的第二遍 reserveZoneNoteArea)。
	// **只许由内容+字号推导,绝不从 note 的落点 bbox 反推** —— 落点依赖框、框又
	// 依赖带,读落点就会重新引入「放一条 note → 带变 → 框动 → note 又不在带内」
	// 的自增长反馈环(根因 C 的翻版)。内容与字号不随框动,幂等。
	//
	// 宽度这一维是 2026-08-19 补的:此前只有高度参与预留,而 note-outside-zone
	// 判的是严格框包含 —— 435 宽的说明配 435 宽的框,生成侧说"放得下"、判定侧说
	// "在框外",两把尺。
	NoteHeight float64 `json:"noteHeight,omitempty"`
	NoteWidth  float64 `json:"noteWidth,omitempty"`
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
	// BaseBBox 是**为说明扩边之前**的框。扩边界(不许越过哪个邻框)必须钉在基础
	// 框上:`sch note` 看到的是「本区还没登记说明」的计划,planner 重算时看到的是
	// 「已登记」的计划,两边只有都拿基础框当邻居,算出的扩边界才逐字段相同。
	BaseBBox layoutBBox `json:"baseBBox"`
}

// baseRect 返回扩边前的框(老数据/手写测试没填 BaseBBox 时回退 BBox)。
func (p partitionRect) baseRect() layoutBBox {
	if (p.BaseBBox == layoutBBox{}) {
		return p.BBox
	}
	return p.BaseBBox
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
	// ModuleOutsideDetail 逐条说出**是谁**探出了框、超了多少、往哪个方向 ——
	// 判据的价值不在报数,在给出能执行的下一步(两把尺那条复发病的另一半)。
	ModuleOutsideDetail []string `json:"moduleOutsideDetail,omitempty"`
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
	// Capacity 回答「这一页是不是根本装不下」——与「摆得不好」是两种病,
	// 修法完全不同(收敛/拆页 vs 挪一挪)。见 sch_zone_capacity.go。
	Capacity schZoneCapacity `json:"capacity"`
	// LabelScopeDegraded:模块 bbox 的「标签范围」口径不可信 —— 标签入框是硬约束
	// (用户裁定),降级必须可见,漏掉的旗恰恰是判据看不见的那种。
	//
	// **没有 omitempty 是有意的**:2026-08-20 查三份真机 JSON 快照,这个字段一次
	// 都没出现过(值为 false 就被 omitempty 抹掉),于是「降级信号」在输出里根本
	// 不存在,等于哑的。降级标志必须**恒定出现**,读的人才能区分「没降级」与
	// 「这版没有这个字段」。
	LabelScopeDegraded bool `json:"labelScopeDegraded"`
	// LabelScope 是同一件事的可归因版本:降不降级、为什么降、涉及哪些位号。
	LabelScope schZoneLabelScope `json:"labelScope"`
	// SheetAssumed:页上没有图框图元,按 A4-only 域界假定 1170×825 规划
	// (图框需人工在 UI 重放;见 schSheetOrA4)。
	SheetAssumed bool `json:"sheetAssumed,omitempty"`
}

type partitionOpts struct {
	Margin    float64
	Gutter    float64
	TitleBand float64
	// NoteBand 是分区**底部**留给电路说明的一条带。顶上有标题带,底下就该有说明带 ——
	// 否则说明只能挤在器件缝里,挤不下就掉到框外(实测:自动落点退到框下方 y=215,
	// 用户一眼看出「说明跑到框外面了」)。版式是:区名左上、说明左下,**都在框内**。
	NoteBand float64
}

// defaultNoteBandLines 是区还没登记说明时,说明带按几行文字预留。SKILL 要求
// 「每模块 1~3 行」,取 2 行 —— 旧常数 26 恰好只是 2 行裸文字高(2×10×1.3),
// 没算带内底距 noteGap,带内候选点(bbox 抬高 noteGap)必然顶进器件区,于是
// 2~3 行说明结构上塞不进带、被回退链踢到框外(REPORT-esp32mini-round2 新 1)。
const defaultNoteBandLines = 2

// requiredNoteBand 把「一条说明的渲染高度」换算成能装下它的说明带高度:
// 自动落点的带内候选把说明 bbox 底沿抬离带底 noteGap(planNoteAnchor),
// 所以带高 ≥ 说明高 + noteGap 才装得进;说明顶与器件区之间的间隙由
// partitionContentPad(24 ≥ noteGap)天然满足。**生成(planPartitions 的带高)
// 与预测(placeSchNote 的预扩)必须走同一个函数** —— 两把尺一分家,就会出现
// 「落点按 A 算、框按 B 画」的框外说明。
func requiredNoteBand(noteH float64) float64 { return noteH + noteGap }

// ── 外框的唯一函数(用户裁定 2026-08-20)────────────────────────────────────
//
// > zone 虚拟框**就是算法算出来的**,应该依据「虚拟组(内含器件元素 + 网络标签)
// > + title + notes」**直接计算**外框。做 title + note 的时候,没有和其他虚拟组
// > 一起进收紧布局,然后才画框。
//
//	frame = f(成员 L1 虚拟组全图元的并集, 区名带高, 说明带高)
//
// 三个消费者共用这一个函数本体,不许各算各的:
//   - `zone-plan` 第一遍(planPartitions)—— 画框 + 判据的基准框;
//   - `zone-arrange` phase A 的现状口径框(rawW/rawH)与收敛后框(FrameW/FrameH);
//   - 任何新的排布/收紧路径。
//
// 此前 phase A 自己拿 `2*pad + TitleBand + NoteBand(常量)` 拼了一份 —— 而
// zone-plan 侧的说明带是按**已登记说明的实际渲染高度**算的(真机 42/55,随行数变),
// 两套带账 = 两把尺:按常量带收紧出来的框,画完再放 note 就装不下,note 探出框外。
// 这正是 9ee3e13 自己挂的那笔账(「NoteBand 是另一套带账,不知登记说明高度」)。
//
// **带高只许由内容+字号推导(noteSizeOf),绝不读 note 的落点坐标** —— 落点依赖框、
// 框又依赖带,读落点就重新引入「放一条 note → 带变 → 框动 → note 又不在带里」的
// 自增长反馈环(9ee3e13 建立的不变式,不要回退)。
//
// 说明**宽度**这一维有意不进本函数:窄框为说明横向扩边需要知道邻框与页面障碍,
// 那是规划器第二遍(reserveZoneNoteArea)的事,而第二遍与 `sch note` 的落点侧
// 共用同一个函数本体 —— 已经是同一把尺。把宽度也塞进第一遍会让「登记前 note 侧
// 算出的框」与「登记后 planner 重算的框」再次分家(先右后左扩 vs 对称扩)。
func partitionFrameRect(content layoutBBox, titleBand, noteBand float64) layoutBBox {
	return layoutBBox{
		MinX: content.MinX - partitionContentPad,
		MinY: content.MinY - partitionContentPad - noteBand,
		MaxX: content.MaxX + partitionContentPad,
		MaxY: content.MaxY + partitionContentPad + titleBand,
	}
}

// partitionFrameSize 是同一个函数的尺寸投影(排布器只关心装多大)。
func partitionFrameSize(content layoutBBox, titleBand, noteBand float64) (w, h float64) {
	r := partitionFrameRect(content, titleBand, noteBand)
	return r.MaxX - r.MinX, r.MaxY - r.MinY
}

// schZoneNoteBandHeight 是说明带高的**唯一口径**:没有登记说明就用默认带,
// 有就按已登记说明里最高的一条推导(requiredNoteBand = 渲染高 + 带内底距)。
func schZoneNoteBandHeight(defaultBand, noteH float64) float64 {
	if noteH > 0 {
		if nb := requiredNoteBand(noteH); nb > defaultBand {
			return nb
		}
	}
	return defaultBand
}

// partitionFirstPassRect 是 zone-plan 第一遍的框(未做纸边/图签收拢、未为说明
// 横向扩边)。与 zoneArrangeRawFrame 同源 —— 见 ruler 配对测试。
func partitionFirstPassRect(content layoutBBox, opts partitionOpts, noteH float64) layoutBBox {
	return partitionFrameRect(content, opts.TitleBand, schZoneNoteBandHeight(opts.NoteBand, noteH))
}

func defaultPartitionOpts() partitionOpts {
	// Margin 20 → 28 (2026-08-11): at 20 the frame sat 26 units from the sheet
	// edge, hugging the printed sheet frame like a double line.
	// NoteBand 26 → requiredNoteBand(2 行):26 只装得下单行(新 1)。
	return partitionOpts{Margin: 28, Gutter: 12, TitleBand: 30,
		NoteBand: requiredNoteBand(defaultNoteBandLines * schNoteDefaultFontSize * 1.3)}
}

// planPartitions is the pure planner: ONE partition per module
// (partitionGrouping — the same answer `zone-arrange` gives), each frame grown
// from its own members' volume, lifted clear of the title-block keep-out and the
// sheet edge, and given a top title band. Deterministic.
func planPartitions(sheet layoutBBox, keepout *layoutBBox, modules []partitionModule, opts partitionOpts) partitionPlan {
	return planPartitionsWithNotes(sheet, keepout, modules, opts, nil)
}

// planPartitionsWithNotes 是带「说明预留」的完整规划器。noteObstacles 是说明不许
// 压的页面图元(器件 / marker 文字带 / **未登记**的自由文本)—— 有它,说明带的
// 预留才能看见「带内被邻区桩线占住」这种情况并把框底下探;没有它(nil,纯几何
// 单测)预留退化成「只按尺寸留」。
//
// **登记说明必须排除在 noteObstacles 之外**:说明落进带 → 带被它自己顶得下探 →
// 说明又不在带里,那正是根因 C 自增长反馈环的翻版。
func planPartitionsWithNotes(sheet layoutBBox, keepout *layoutBBox, modules []partitionModule,
	opts partitionOpts, noteObstacles []layoutBBox) partitionPlan {
	plan := partitionPlan{Sheet: sheet, Keepout: keepout}
	plan.Capacity = diagnoseZoneCapacity(sheet, keepout, modules, opts)
	usable := layoutBBox{
		MinX: sheet.MinX + opts.Margin, MinY: sheet.MinY + opts.Margin,
		MaxX: sheet.MaxX - opts.Margin, MaxY: sheet.MaxY - opts.Margin,
	}
	if len(modules) == 0 || usable.MaxX <= usable.MinX || usable.MaxY <= usable.MinY {
		return plan
	}

	// 图签安全带:说明带撞上它可以缩,**内容不许缩**(见 rect 后的收拢)。
	safe := inflatedTitleKeepout(keepout)
	groups := partitionGrouping(modules)
	noteNeeds := make([]partitionNoteNeed, 0, len(groups))
	for _, grp := range groups {
		content := modules[grp[0]].BBox
		for _, i := range grp[1:] {
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
		// 说明带高度**按该区已登记说明的实际渲染高度预留**(新 1):带不够高时,
		// 多行说明在带内候选点必撞器件区,被回退链踢到框外。带加高 = rect.MinY
		// 进一步下探(向外扩),器件区(content ± pad)一寸不挤。高度来自登记记录
		// 的文字尺寸(NoteHeight,内容+字号),不来自落点 bbox —— 不构成反馈环。
		var noteW, noteH float64
		for _, i := range grp {
			if h := modules[i].NoteHeight; h > noteH {
				noteH = h
			}
			if w := modules[i].NoteWidth; w > noteW {
				noteW = w
			}
		}
		noteBand := schZoneNoteBandHeight(opts.NoteBand, noteH)
		// **框 = 成员虚拟组体积的并集 + 边距 + 上标题带 + 下说明带,不做任何裁剪。**
		//
		// 此前这里把矩形 clamp 到网格带单元(`math.Min(cell.MaxX, …)`),于是模块的
		// 虚拟组一旦跨过单元边界,框就被切短 —— 地旗、网络标签垂在框外(用户截图实证
		// D1 的 GND)。「框住自己的内容」必须是**构造保证**而不是检查项:算得出来的东西
		// 不该留给判据去发现、更不该留给人去看图。
		//
		// 去掉 clamp 之后,moduleOutsideZone 结构上恒为 0(它降级成一条后置断言:
		// 真报出来说明这里的算术错了)。**代价是框之间可能重叠** —— 但那不是画框的
		// 毛病,是布局的事实:两个模块的虚拟组本身交叠时,不存在既包住又互不重叠的
		// 一组矩形。那件事由 partitionOverlap 如实报出来,修法是挪件(S3 的组间留通道),
		// 不是把框切短来掩盖。
		rect := partitionFrameRect(content, opts.TitleBand, noteBand)
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
		// 纸边与图签同理(同一把尺,2026-08-18 P2 真机定案):pad/标题带/说明带是
		// **我们加的预留**,撞纸边(sheetEdgeMinGap)就缩,内容一寸不让 —— 此前只有
		// 图签方向这么做,纸边四周没有,于是「内容顶 770.5(离纸边 54.5,完全装得下)
		// + pad24 + 标题带30 = 824.5」贴到纸边,planner 自己产生的框被自己的
		// SheetMarginHits 拒绝,zone-draw 永远画不出来。clamp 后 marginHit 只剩一种
		// 触发方式:内容本体自己贴边 —— 那才是真该报的。
		if lim := sheet.MinX + sheetEdgeMinGap; rect.MinX < lim {
			rect.MinX = math.Min(lim, content.MinX)
		}
		if lim := sheet.MinY + sheetEdgeMinGap; rect.MinY < lim {
			rect.MinY = math.Min(lim, content.MinY)
		}
		if lim := sheet.MaxX - sheetEdgeMinGap; rect.MaxX > lim {
			rect.MaxX = math.Max(lim, content.MaxX)
		}
		if lim := sheet.MaxY - sheetEdgeMinGap; rect.MaxY > lim {
			rect.MaxY = math.Max(lim, content.MaxY)
		}
		band := opts.TitleBand
		if h := rect.MaxY - rect.MinY; band > h/2 {
			band = h / 2
		}
		names := make([]string, 0, len(grp))
		for _, i := range grp {
			names = append(names, modules[i].Name)
		}
		sort.Strings(names)
		// 带顶 = 器件区下沿,**与带高无关**:框为说明加高/下探时它固定不动
		// (器件区一寸不挤)。`sch note` 拿 NoteBBox.MaxY 当带顶复算预留,两边
		// 因此对得上 —— 带顶若跟着带高走,登记前后就是两个不同的基准。
		bandTop := math.Min(rect.MaxY, math.Max(rect.MinY, content.MinY-partitionContentPad))
		plan.Partitions = append(plan.Partitions, partitionRect{
			Modules:  names,
			BBox:     rect,
			BaseBBox: rect,
			// Title band at the visual TOP (large y).
			TitleBBox: layoutBBox{MinX: rect.MinX, MinY: rect.MaxY - band, MaxX: rect.MaxX, MaxY: rect.MaxY},
			// Note band at the visual BOTTOM (small y) —— 说明就放这儿,框内左下。
			// **带的定义只有 zoneNoteBand 一个函数**(落点求解与 note-outside-zone
			// 判据读的是同一条带),这里不许再写一遍字面量。
			NoteBBox: zoneNoteBand(rect, bandTop),
		})
		noteNeeds = append(noteNeeds, partitionNoteNeed{W: noteW, H: noteH, BandTop: bandTop})
	}
	reserveNoteAreas(&plan, noteNeeds, sheet, keepout, opts, noteObstacles)
	plan.Validation = validatePartitions(plan, modules, keepout)
	return plan
}

// partitionNoteNeed 是一个分区「已登记说明要占多大地方」的规划输入(与坐标无关)。
type partitionNoteNeed struct{ W, H, BandTop float64 }

// reserveNoteAreas 是规划器的**第二遍**:逐区把框扩到装得下它自己的说明。
//
// 为什么必须是第二遍:扩边界要看邻区的框,而邻区的框在第一遍才算出来。扩边界一律
// 钉在**基础框**(BaseBBox,扩边前)上 —— `sch note` 手上只有「本区还没登记说明」
// 的计划,planner 重算时手上是「已登记」的计划,两边只有都用基础框,算出来的框
// 才逐字段相同(生成与判定同一把尺)。
//
// 扩不动(可扩边界内仍装不下)时**不硬撑**:框保持最小预留形态,由
// note-outside-zone 带着「缩短文字/腾地方」的可执行修法如实报出来。
func reserveNoteAreas(plan *partitionPlan, needs []partitionNoteNeed, sheet layoutBBox,
	keepout *layoutBBox, opts partitionOpts, noteObstacles []layoutBBox) {
	if len(needs) != len(plan.Partitions) {
		return
	}
	bases := make([]layoutBBox, len(plan.Partitions))
	for i := range plan.Partitions {
		bases[i] = plan.Partitions[i].BaseBBox
	}
	for i := range plan.Partitions {
		n := needs[i]
		if n.H <= 0 {
			continue // 这个区没登记说明:按默认带留着就行
		}
		var neighbors []layoutBBox
		for j := range bases {
			if j != i {
				neighbors = append(neighbors, bases[j])
			}
		}
		rect, band, _, _, _ := reserveZoneNoteArea(plan.Partitions[i].BBox, n.BandTop, n.W, n.H,
			noteObstacles, sheet, keepout, neighbors, opts.Gutter)
		titleBand := opts.TitleBand
		if hgt := rect.MaxY - rect.MinY; titleBand > hgt/2 {
			titleBand = hgt / 2
		}
		plan.Partitions[i].BBox = rect
		plan.Partitions[i].NoteBBox = band
		plan.Partitions[i].TitleBBox = layoutBBox{MinX: rect.MinX, MinY: rect.MaxY - titleBand,
			MaxX: rect.MaxX, MaxY: rect.MaxY}
	}
}

// ── 分区归属的唯一函数(2026-08-20 定案)────────────────────────────────────
//
// **一个模块(= 一个 L1 虚拟组 / zone 认领)就是一个分区。**
//
// 这与 `zone-arrange` 的答案逐字相同:phase B 每个认领折一个 zaZone
// (planZoneArrangeScene),求解器每个 zaZone 落一个框(zonesArrange),
// 落地复判(zaaLandedRecheck)每个区量一个实测框、逐对判零重叠。也就是说
// **画框侧与排布侧现在问的是同一个问题、拿的是同一个答案** ——
// TestRuler_ZonePartitionGroupingMatchesArrange 把这条配对钉住。
//
// ── 被删掉的那把尺:网格带归组 ─────────────────────────────────────────────
//
// 此前这里把整页按「模块间的自然空隙」切成列带/行带(clusterSplits +
// bandIndex),**同一格里的模块合成一个分区**。那是 issue #149 首版的遗留:
// 当时框会被 clamp 到格子里,格子决定几何,归组只是副产品。后来 clamp 被去掉
// (框 = 成员并集 + 带,构造保证「框住自己的内容」),格子就只剩归组这一个作用 ——
// 而它给出的答案与 zone-arrange 不一样,这就是本缺陷:
//
//	真机 ceshi / MCU_IO(A4,zone-arrange --apply 断言③全绿、区框零重叠):
//	  行分割找不到能把 led_indicator_gpio 与 tactile_boot_reset(左列上下叠)
//	  分开的**全局**空带(wroom-passives 的 y 区间横跨两者),于是两区被并成
//	  一个分区,并集宽到 x=274,而 wroom-passives 从 229 起 → partitionOverlap=1
//	  → zone-draw 拒绝画框。单看任何一个区的框,没有任何重叠。
//
// 网格带的表达能力(全局的一刀切)结构上装不下「同列上下叠 + 邻列横跨」这种
// 排布,而 zone-arrange 的货架装箱恰恰经常产生它。两把尺一分家,交付就被自己
// 卡死(画分区框是 SKILL 铁律 15)。
//
// 归组不再看几何,所以「没跑过 zone-arrange 的页」(手工搭的页、随手摆的件)
// 也照样有确定的分区:每个认领一个框。这条路径没有被砍掉,它和跑过的页走的
// 是同一段代码 —— 一个页面只有一套答案,与跑没跑过排布无关。
//
// 返回值是按**绘制序**排好的分组(视觉上方优先 → 左→右 → 名字),每组一个分区。
func partitionGrouping(modules []partitionModule) [][]int {
	idx := make([]int, len(modules))
	for i := range modules {
		idx[i] = i
	}
	// 确定性:输入顺序不参与判决(与 zaOrderZones 同一条纪律)。
	sort.SliceStable(idx, func(a, b int) bool {
		i, j := idx[a], idx[b]
		// 分区序用 CORE 口径(器件本体)的质心:draw 口径的旗/说明外伸会随上一轮
		// 布线漂移,拿它排序会让「画了几个框、谁先谁后」跟着残局走。
		ix, iy := bboxCenter(moduleCoreBBox(modules[i]))
		jx, jy := bboxCenter(moduleCoreBBox(modules[j]))
		if iy != jy {
			return iy > jy // y-UP:视觉上方(大 y)在前
		}
		if ix != jx {
			return ix < jx
		}
		return modules[i].Name < modules[j].Name
	})
	out := make([][]int, 0, len(idx))
	for _, i := range idx {
		out = append(out, []int{i})
	}
	return out
}

func bboxContains(outer, inner layoutBBox) bool {
	const eps = 0.01
	return inner.MinX >= outer.MinX-eps && inner.MinY >= outer.MinY-eps &&
		inner.MaxX <= outer.MaxX+eps && inner.MaxY <= outer.MaxY+eps
}

// partitionJudge 是 moduleOutsideZone 的**独立判定输入**。
//
// 为什么必须独立:判据此前复用生成侧的 `m.BBox` —— 而那个 bbox 已经被上游削过
// (clusterOf 查不到就静默退回器件本体、无引脚几何时整组体积退化成本体),于是
// 「生成漏掉的标签,判定也看不见」,判据在验一个被削过的集合,结构上恒报 0。
// 真机取证 2026-08-20:POWER 页 8 个 L1 组里 5 个探出框外,moduleOutsideZone=0。
//
// 判定侧只与生成侧共享**一件事实**:谁属于哪个区(认领 / 虚拟组成员表)。
// 几何一律从活体的 L1 组表(`sch clusters` 口径)现取。
type partitionJudge struct {
	// PartsOf:区名 → 成员位号(已解析到本页的那些)。
	PartsOf map[string][]string
	// ClusterOf:位号 → 该件 L1 虚拟组的实测体积(器件本体 ∪ 它自己的 marker/桩线)。
	ClusterOf map[string]layoutBBox
	// Scope:口径可信度。不可信的位号按「探出框外」计(fail-closed)——
	// 标签入框是硬约束,验不了就不许报绿。
	Scope schZoneLabelScope
}

func validatePartitions(plan partitionPlan, modules []partitionModule, keepout *layoutBBox) partitionValidation {
	return validatePartitionsWithJudge(plan, modules, keepout, nil)
}

// validatePartitionsWithJudge 是带独立判定输入的完整校验。judge=nil 时退回旧的
// 「按生成侧 m.BBox 判」——**只有纯几何单测和 zaValidate(框级四项)能用**,
// 活体路径必须给 judge,否则判据看不见上游漏掉的标签。
func validatePartitionsWithJudge(plan partitionPlan, modules []partitionModule,
	keepout *layoutBBox, judge *partitionJudge) partitionValidation {
	var v partitionValidation
	ps := plan.Partitions
	// Same inflated basis the planner lifts with (titleBlockSafety): validating
	// against the bare estimate while lifting by a different amount is exactly the
	// false-green this replaced.
	safe := inflatedTitleKeepout(keepout)
	coreOf := map[string]layoutBBox{}
	for _, m := range modules {
		coreOf[m.Name] = moduleCoreBBox(m)
	}
	for _, p := range ps {
		if !bboxContains(plan.Sheet, p.BBox) {
			v.SheetOverflow++
		}
		if safe != nil && boxesOverlap(p.BBox, *safe) {
			// 分层(2026-08-18):框压到**裸 keepout**(真图签表格)= hard hit;
			// 只擦到膨胀安全带时,仅当某成员模块的 CoreBBox(器件本体)也侵入
			// 安全带才计 —— 旗/标签探进安全带是注释级余量问题(partitionModule
			// 注释既有的设计意图),此前框因包住 marker 而擦线就整个 hard-block,
			// 与「titleBlockHits 用 CoreBBox」的声明是两把尺。
			hard := keepout != nil && boxesOverlap(p.BBox, *keepout)
			if !hard {
				for _, name := range p.Modules {
					if core, ok := coreOf[name]; ok && boxesOverlap(core, *safe) {
						hard = true
						break
					}
				}
			}
			if hard {
				v.TitleBlockHits++
			}
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
	// **按整个 L1 虚拟组判,不是按器件本体**:框的职责是「框住这个模块」,而模块的
	// 体积包含它自己的 marker/桩线。只判本体时,框可以把地旗、网络标签甩在外面
	// 却依然报 clean —— 用户截图一眼看出 D1 的 GND 垂在 ESD 框外,而 validation
	// 五项全 0。判据必须判所见。
	//
	// judge 非空时**逐个 L1 组独立重算**(不碰 m.BBox);为空时退回旧口径。
	for _, m := range modules {
		pb, ok := partOf[m.Name]
		if !ok {
			v.ModuleOutsideZone++
			v.ModuleOutsideDetail = append(v.ModuleOutsideDetail,
				fmt.Sprintf("%s:没有分配到任何分区框", m.Name))
			continue
		}
		if judge == nil {
			if !bboxContains(pb, m.BBox) {
				v.ModuleOutsideZone++
				v.ModuleOutsideDetail = append(v.ModuleOutsideDetail,
					fmt.Sprintf("%s:模块体积 %s 探出框 %s", m.Name, bboxText(m.BBox), bboxText(pb)))
			}
			continue
		}
		if why := judgeModuleLabels(m.Name, pb, judge); len(why) > 0 {
			v.ModuleOutsideZone++
			v.ModuleOutsideDetail = append(v.ModuleOutsideDetail, why...)
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

// judgeModuleLabels 逐个 L1 虚拟组判「有没有被这个框罩住」,返回可执行的说明。
// 不可信(引脚几何缺失 / 无 L1 组 / 导线读不到)一律按**探出**处理:验不了就不许
// 报绿 —— 那正是 2026-08-20 那次「五个组探出框外、判据全 0」的成因。
func judgeModuleLabels(name string, frame layoutBBox, judge *partitionJudge) []string {
	var out []string
	parts := judge.PartsOf[name]
	if len(parts) == 0 {
		return []string{fmt.Sprintf("%s:判定侧拿不到成员位号 —— L1 组口径不可信,不予放行", name)}
	}
	for _, d := range parts {
		key := strings.ToUpper(d)
		cb, has := judge.ClusterOf[key]
		if !has || judge.Scope.untrusted(key) {
			out = append(out, fmt.Sprintf(
				"%s/%s:读不到可信的 L1 虚拟组体积(标签范围口径降级)—— 先修数据再谈框:`easyeda sch clusters --members`",
				name, key))
			continue
		}
		if bboxContains(frame, cb) {
			continue
		}
		out = append(out, fmt.Sprintf("%s/%s:L1 组体积 %s 探出框 %s(%s)",
			name, key, bboxText(cb), bboxText(frame), overhangText(frame, cb)))
	}
	return out
}

// overhangText 说清超出的方向与量 —— 判据必须给能执行的下一步。
func overhangText(frame, inner layoutBBox) string {
	var parts []string
	if d := frame.MinX - inner.MinX; d > 0.01 {
		parts = append(parts, fmt.Sprintf("左超 %.0f", d))
	}
	if d := inner.MaxX - frame.MaxX; d > 0.01 {
		parts = append(parts, fmt.Sprintf("右超 %.0f", d))
	}
	if d := frame.MinY - inner.MinY; d > 0.01 {
		parts = append(parts, fmt.Sprintf("下超 %.0f", d))
	}
	if d := inner.MaxY - frame.MaxY; d > 0.01 {
		parts = append(parts, fmt.Sprintf("上超 %.0f", d))
	}
	if len(parts) == 0 {
		return "边界擦线"
	}
	return strings.Join(parts, "、")
}

func bboxText(b layoutBBox) string {
	return fmt.Sprintf("(%.0f,%.0f)..(%.0f,%.0f)", b.MinX, b.MinY, b.MaxX, b.MaxY)
}

func strInSlice(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// schMarkerFoldReach 是**降级路径**(读不到导线)里 marker 收编的最大距离。
//
// 旧值 schModuleMarkerReach=60 是拍的,而且注释自承「会漏远处的旗」:实测桩线
// offset 常见 18–98,再加上标签渲染宽度,轻松超过 60 —— 漏掉的恰恰是判据也看不见
// 的那种旗。这里改用 `buildSchClusters` 自己的无线兜底同一把尺(6*schStubLen),
// 两条归属路径不再各拿一把尺;而且这条路径现在**恒被标记为降级**(见
// schZoneLabelScope),所以它的职责只剩「尽量别漏」,不再假装可信。
const schMarkerFoldReach = 6 * schStubLen

// schZoneLabelScope 是「模块 bbox 到底罩住了多少标签」的可信度记录。
//
// 标签入框是硬约束(用户裁定 2026-08-16),所以「口径不可信」必须是**一等公民**:
// 生成侧漏掉的旗,判定侧也看不见(它俩曾共用同一个 m.BBox),于是判据恒报 0 而
// 用户截图一眼看出标签垂在框外。降级从此显式序列化,并让 moduleOutsideZone
// 按「不可信」处理(fail-closed),不许静默。
type schZoneLabelScope struct {
	Degraded bool `json:"degraded"`
	// WiresUnavailable:导线读不到 —— L1 归属整体退回距离启发式。
	WiresUnavailable bool `json:"wiresUnavailable,omitempty"`
	// NoPinGeometry:认领的件在本次回读里没有引脚几何。**这是 2026-08-20 真机
	// 那次假绿的真凶**:`computePartitionPlan` 拉几何时只要了 includeBBox,没要
	// includePins,于是 buildSchClusters 一个 marker/桩线都归不了属,每个 L1 组的
	// 体积退化成器件本体 —— clusterOf 非空、每个键都在,degraded 却是 false。
	NoPinGeometry []string `json:"noPinGeometry,omitempty"`
	// NoCluster:认领的件在本页有 bbox,却没有对应的 L1 组记录。
	NoCluster []string `json:"noCluster,omitempty"`
	// OffPage:认领的件不在本页(跨页认领,信息项,不算降级)。
	OffPage []string `json:"offPage,omitempty"`
	// UnownedMarkers:既不沾任何导线、离谁都远的 marker 数(信息项)。
	UnownedMarkers int `json:"unownedMarkers,omitempty"`
}

// untrusted 报告某个位号的 L1 组体积是否不可信(判定侧据此 fail-closed)。
func (s schZoneLabelScope) untrusted(desig string) bool {
	if s.WiresUnavailable {
		return true
	}
	key := strings.ToUpper(desig)
	return strInSlice(s.NoPinGeometry, key) || strInSlice(s.NoCluster, key)
}

// modulesFromClaims 是旧签名(丢掉可信度记录)—— 只给不做标签判定的排布类
// 消费者用(`sch sheet tidy`)。**画框/判据路径一律走 modulesFromClaimsScoped**。
func modulesFromClaims(zones map[string]*schZoneClaim, comps []layoutComp,
	clusterOf map[string]layoutBBox) []partitionModule {
	m, _ := modulesFromClaimsScoped(zones, comps, clusterOf)
	return m
}

// modulesFromClaimsScoped 把认领折成模块 bbox,**并报出每一处口径降级**。
//
// clusterOf 是**按导线归属**算出的「器件 + 只挂在它自己引脚上的 marker/桩线」体积
// (`buildSchClusters`)。有它就用它 —— 框住的必须是整个 L1 虚拟组,而不是器件本体:
// 用户截图实证 D1 的 GND 旗垂在 ESD 框外面。
//
// **不再静默退回器件本体**:某个位号在 clusterOf 里查不到时,旧代码悄悄
// `grow(&u, *c.BBox)`,该件的旗/桩线就此蒸发 —— 而判定用的是同一个 u,于是
// 「生成漏掉的标签,判定也看不见」。现在这条路仍然给出一个可画的框(否则整页
// 画不出来),但会把位号记进 NoCluster,判定侧据此把该模块按不可信处理。
func modulesFromClaimsScoped(zones map[string]*schZoneClaim, comps []layoutComp,
	clusterOf map[string]layoutBBox) ([]partitionModule, schZoneLabelScope) {
	scope := schZoneLabelScope{WiresUnavailable: clusterOf == nil}
	byDesig := map[string]layoutComp{}
	for _, c := range comps {
		if c.Designator != "" && c.BBox != nil {
			byDesig[strings.ToUpper(c.Designator)] = c
		}
	}
	// Markers with a bbox, for the degraded (no-wire) nearest-part fold below.
	var markers []layoutComp
	for _, c := range comps {
		if isSchMarker(c.ComponentType) && c.BBox != nil && c.AnchorAvailable {
			markers = append(markers, c)
		}
	}
	var names []string
	for n := range zones {
		names = append(names, n)
	}
	sort.Strings(names)

	// 降级路径的归属表:每支 marker 归给**离它最近的认领件**(全页择优,不是
	// 逐模块各判各的 —— 旧写法同一支旗可以同时被两个模块收编,体积双算)。
	ownerOfMarker := map[int]string{}
	if clusterOf == nil {
		type claimed struct {
			key  string
			bbox layoutBBox
		}
		var parts []claimed
		for _, name := range names {
			zc := zones[name]
			if zc == nil {
				continue
			}
			for _, d := range zc.Parts {
				if c, ok := byDesig[strings.ToUpper(d)]; ok {
					parts = append(parts, claimed{strings.ToUpper(d), *c.BBox})
				}
			}
		}
		for mi, m := range markers {
			best, bestD := "", math.Inf(1)
			for _, p := range parts {
				if d := pointBoxDist(m.X, m.Y, p.bbox); d < bestD {
					best, bestD = p.key, d
				}
			}
			if best != "" && bestD <= schMarkerFoldReach {
				ownerOfMarker[mi] = best
			}
		}
	}

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
				scope.OffPage = appendUniqueStr(scope.OffPage, key)
				continue
			}
			grow(&core, *c.BBox) // core = 器件本体(压图签时按它收拢)
			// 引脚几何缺失 = L1 归属结构上做不成(marker 顺着桩线找宿主靠的就是
			// 引脚坐标)。**必须在这里报**,不能等 clusterOf 查得到就当没事:
			// 查得到的那个体积此时恰好等于器件本体,静默通过 = 判据永远看不见旗。
			if clusterOf != nil && !c.PinsAvailable {
				scope.NoPinGeometry = appendUniqueStr(scope.NoPinGeometry, key)
			}
			if cb, has := clusterOf[key]; has {
				grow(&u, cb) // 画框口径 = 整个 L1 虚拟组(本体 ∪ 它自己的 marker/桩线)
				continue
			}
			if clusterOf != nil {
				scope.NoCluster = appendUniqueStr(scope.NoCluster, key)
			}
			grow(&u, *c.BBox)
			if clusterOf == nil {
				for mi := range markers {
					if ownerOfMarker[mi] == key {
						grow(&u, markerJudgeBBox(markers[mi]))
					}
				}
			}
		}
		if u != nil {
			out = append(out, partitionModule{Name: name, BBox: *u, CoreBBox: *core})
		}
	}
	sort.Strings(scope.NoPinGeometry)
	sort.Strings(scope.NoCluster)
	sort.Strings(scope.OffPage)
	scope.Degraded = scope.WiresUnavailable || len(scope.NoPinGeometry) > 0 || len(scope.NoCluster) > 0
	return out, scope
}

// pointBoxDist 是点到矩形的距离(点在矩形内时为 0)。
func pointBoxDist(x, y float64, b layoutBBox) float64 {
	dx := math.Max(0, math.Max(b.MinX-x, x-b.MaxX))
	dy := math.Max(0, math.Max(b.MinY-y, y-b.MaxY))
	return math.Hypot(dx, dy)
}

func appendUniqueStr(ss []string, s string) []string {
	if strInSlice(ss, s) {
		return ss
	}
	return append(ss, s)
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

// buildPartitionGraphicsPayload maps a validated partition plan onto the typed
// annotation action. Schematic rectangles are anchored at their visual
// top-left, so y is MaxY and height extends toward -y.
func buildPartitionGraphicsPayload(plan partitionPlan, fontSize float64, color string) map[string]any {
	rectangles := make([]map[string]any, 0, len(plan.Partitions))
	texts := make([]map[string]any, 0, len(plan.Partitions))
	for _, p := range plan.Partitions {
		w, h := p.BBox.MaxX-p.BBox.MinX, p.BBox.MaxY-p.BBox.MinY
		if w <= 0 || h <= 0 {
			continue
		}
		rectangles = append(rectangles, map[string]any{
			"x": p.BBox.MinX, "y": p.BBox.MaxY, "width": w, "height": h,
			"color": color, "lineWidth": 1, "lineType": 1,
		})
		texts = append(texts, map[string]any{
			"x": p.TitleBBox.MinX + 4, "y": p.TitleBBox.MaxY - fontSize,
			"content": strings.Join(p.Modules, " / "), "color": color, "fontSize": fontSize,
		})
	}
	return map[string]any{"rectangles": rectangles, "texts": texts}
}

// newSchZonePlanCmd builds `sch zone-plan` — compute + print the partition plan
// (no mutation). --json emits the full plan + validation.
func newSchZonePlanCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var asJSON bool
	var margin, gutter, titleBand float64
	c := &cobra.Command{
		Use:   "zone-plan",
		Short: "Plan data-driven A4 functional partitions from the live sheet + module bboxes (no mutation)",
		Long: `Compute a whole-sheet functional partition plan (issue #149) from the LIVE
geometry: **一个虚拟组 / zone 认领 = 一个分区**(与 ` + "`zone-arrange`" + ` phase B 的
落位框一一对应,同一把尺),框由该区成员的实测体积撑出来,再让开图签 keep-out
与纸边、留出区名带。Reads modules from ` + "`sch zones`" + ` claims / virtual groups.

**外框是一个纯函数**:frame = f(成员 L1 虚拟组全图元并集, 区名带, 说明带) ——
` + "`zone-arrange`" + ` phase A 的收紧用的是同一个函数本体、同一份带高(带高由已登记
说明的内容+字号推导,绝不读 note 落点),所以收紧完再画框,而不是按常量带收紧、
画完框再放 note 装不下。

Pure计算 — prints the plan + validation (sheetOverflow / partitionOverlap /
titleBlockHits / moduleOutsideZone / labelCollisions, all should be 0).
` + "`moduleOutsideZone`" + ` 判的是**整个 L1 虚拟组**(器件 ∪ 它自己的 marker/桩线),
且判定侧**独立重算**几何 —— 生成侧漏掉的标签它看得见。归属做不成时
(读不到导线 / 无引脚几何 / 无 L1 组记录)输出里的 ` + "`labelScopeDegraded`" + `
为 true 并点名位号,该模块按「不可信」计入 moduleOutsideZone(验不了不报绿)。

Draw it with ` + "`sch zone-draw --mode partition`" + `.`,
		Example: `  easyeda sch zones set --spec s0.json --project ceshi
  easyeda sch zone-plan --project ceshi --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			pinnedCfg, win, docUUID, err := pinZonePage(cfg, *window)
			if err != nil {
				return err
			}
			plan, _, err := computePartitionPlan(pinnedCfg, win, docUUID,
				partitionOptsFrom(margin, gutter, titleBand))
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
	c.Flags().Float64Var(&gutter, "gutter", def.Gutter, "为说明扩边时与邻框/障碍保持的间距(容量诊断同用);**分区怎么分与它无关**——一区一框")
	c.Flags().Float64Var(&titleBand, "title-band", def.TitleBand, "height of each partition's title band")
	return c
}

func partitionOptsFrom(margin, gutter, titleBand float64) partitionOpts {
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
	zones, project, err := loadSchZoneModules(cfg, window, docUUID)
	if err != nil {
		return partitionPlan{}, nil, err
	}
	if len(zones) == 0 {
		return partitionPlan{}, nil, fmt.Errorf("%q 这一页既没有虚拟组也没有 zone 认领 —— 用 `sch block-apply` 落块(自动按功能子群归组),或手工 `sch group create` / `sch zones set`", project)
	}
	if err := ensureActiveDoc(cfg, window); err != nil {
		return partitionPlan{}, nil, fmt.Errorf("zone-plan: restore pinned page %s: %w", docUUID, err)
	}
	// **includePins 是必需的,不是可选的**(根因,2026-08-20 真机+代码双取证):
	// `buildSchClusters` 靠**引脚坐标**把桩线连通块认到器件头上,marker 再顺着自己
	// 的桩线找宿主。此前这里只要了 includeBBox —— 一个引脚都没有 → 没有一根线归得了
	// 属 → 每个 L1 组的体积原样退化成器件本体,而 clusterOf 非空、每个键都在,
	// `labelScopeDegraded` 于是恒为 false。真机症状:框只罩住器件本体,POWER 页
	// 8 个 L1 组里 5 个的旗/桩线垂在框外,moduleOutsideZone 却报 0。
	//
	// 排序注意(见 memory pins-readback-poisons-modify):带引脚的回读会让**紧随
	// 其后的 `schematic.component.modify`** 被平台拒(cmdKey)。本路径下游只有
	// 只读 + exec_js 建图元(zone-draw)/ 建文字(sch note),不发 modify;要挪件请走
	// `sch zone-arrange --apply` 自己的取数(它本来就带引脚,且清扫在前)。
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", window,
		map[string]any{"includeBBox": true, "includePins": true}, docUUID, "read partition geometry")
	if err != nil {
		return partitionPlan{}, nil, err
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return partitionPlan{}, nil, perr
	}
	// 图框图元缺失时按 A4-only 域界假定(真机 2026-08-16:P3 的图框在连接器停摆期
	// 的 save 中丢失,平台无重建 API,唯一修复是人工重放)。假定必须可见,不许静默。
	sheet, sheetAssumed := schSheetOrA4(comps)
	keepout, _ := titleBlockKeepout(sheet)
	// 框住的是「器件 + 它自己的 marker/桩线」(L1 虚拟组),不是器件本体 ——
	// 归属走导线,不靠距离。读不到导线就退回旧的距离启发式(会漏远处的旗)。
	// **标签入框是硬约束(用户裁定 2026-08-16)**:降级路径必须在输出里可见
	// (labelScopeDegraded),不许静默 —— 距离启发式漏掉的旗恰恰是判据看不见的那种。
	var clusterOf map[string]layoutBBox
	unowned := 0
	if wires, werr := fetchSchWirePolylines(cfg, window, docUUID); werr == nil {
		if cs, un := buildSchClusters(comps, wires); len(cs) > 0 {
			unowned = un
			clusterOf = map[string]layoutBBox{}
			for _, c := range cs {
				clusterOf[strings.ToUpper(c.Designator)] = c.Box
			}
		}
	}
	modules, scope := modulesFromClaimsScoped(zones, comps, clusterOf)
	scope.UnownedMarkers = unowned
	if len(modules) == 0 {
		return partitionPlan{}, nil, fmt.Errorf("no module bboxes resolved — the claimed parts aren't on this page (place them / `doc switch`)")
	}
	// **已登记的说明 note 不参与分区框的内容 bbox(根因 C,2026-08-19 真机定案)**:
	// 说明带(NoteBBox)是从内容 bbox 推出的框内预留(框底 26 单位);落进带里的
	// note 若再被 fold 回内容 bbox,框每重画一次就向下长一截 ≈ pad+带高(实测
	// D_ESD 框 minY 554→501),新的说明带随框下移,原来带内的说明又"不在带里"了,
	// 且被拉炸的区 bbox 会与邻框交叠 → partitionOverlap=1 → zone-draw 拒绝重画的
	// 死锁。「note 计入内容 bbox」与「带由内容 bbox 推导」同时成立时这个自增长
	// 反馈环必然存在 —— 因此这里**按登记记录机械排除**:说明的家是构造出来的
	// 说明带(`sch note --zone` 的自动落点优先落带),不反哺框几何。
	// (`sch sheet tidy` 的排布口径仍 fold —— 那是搬动时给说明留地方,不推导带。)
	//
	// 但说明带的**尺寸**要认已登记说明的文字尺寸(新 1 + 宽度补丁):宽高都来自
	// 「登记记录的内容+字号」(text.list 的 content/fontSize → noteSizeOf),与落点
	// 坐标无关,所以带只随「登记了什么说明」变、不随「说明落在哪」变 —— 幂等,
	// 无反馈环。
	texts := applyZoneNoteSizes(cfg, window, docUUID, zones, modules)
	// 说明带的占用表:器件 / marker 文字带 / **未登记**的自由文本。登记说明一律
	// 排除(否则说明自己会把自己的带顶下去,又是自增长反馈环)。与 `sch note`
	// 落点侧构造的那份逐条同源 —— 两边不同源就又是两把尺。
	noteObstacles := collectNoteObstacles(comps, filterUnregisteredTexts(texts, registeredNoteIDs(zones)))
	plan := planPartitionsWithNotes(*sheet, keepout, modules, opts, noteObstacles)
	plan.LabelScope = scope
	plan.LabelScopeDegraded = scope.Degraded
	plan.SheetAssumed = sheetAssumed
	// **判据与生成解耦**:planPartitionsWithNotes 内部那一遍用的是生成侧的
	// m.BBox(纯几何后置断言);活体路径必须用**独立重算**的 L1 组表再判一遍,
	// 否则上游漏掉的标签判定侧同样看不见(恒报 0)。
	plan.Validation = validatePartitionsWithJudge(plan, modules, keepout,
		buildPartitionJudge(zones, comps, clusterOf, scope))
	return plan, zones, nil
}

// buildPartitionJudge 从活体几何**独立重算**判定输入:区名 → 成员位号 →
// 该位号的 L1 虚拟组体积。与生成侧唯一共享的是「谁属于哪个区」这份认领事实。
func buildPartitionJudge(zones map[string]*schZoneClaim, comps []layoutComp,
	clusterOf map[string]layoutBBox, scope schZoneLabelScope) *partitionJudge {
	onPage := map[string]bool{}
	for _, c := range comps {
		if c.Designator != "" && c.BBox != nil {
			onPage[strings.ToUpper(c.Designator)] = true
		}
	}
	j := &partitionJudge{PartsOf: map[string][]string{}, ClusterOf: map[string]layoutBBox{}, Scope: scope}
	for name, zc := range zones {
		if zc == nil {
			continue
		}
		for _, d := range zc.Parts {
			key := strings.ToUpper(d)
			if !onPage[key] {
				continue // 跨页认领:本页判不着,由 OffPage 记录(信息项)
			}
			j.PartsOf[name] = append(j.PartsOf[name], key)
		}
		sort.Strings(j.PartsOf[name])
	}
	for k, v := range clusterOf {
		j.ClusterOf[k] = v
	}
	return j
}

// schNoteBBoxEstimate 估算一条文本的渲染 bbox:锚点为左**下**(y-UP,块向上生长,
// 2026-08-20 getPrimitivesBBox 实测定案,见 noteAnchorBBox),
// 行高 ≈ fontSize×1.3,宽 ≈ 最长行字符宽(CJK ≈ fontSize,ASCII ≈ 0.55×fontSize)。
// 行高系数偏保守:同一次实测量到的真实行高 ≈ fontSize×1.0,估高约 30%,方向是
// 「多留、不擦碰」——收紧它会连带改变所有分区框的说明带高,属另一件事。
// 尺寸口径由 noteSizeOf 独家提供 —— `sch note` 的自动落点求解器用同一个函数
// 估算候选 bbox。两套估算一旦分家,就会出现"求解时说不撞、画框时说撞"。
func schNoteBBoxEstimate(t zoneMoveText) layoutBBox {
	w, h := noteSizeOf(t.Content, t.FontSize)
	return noteAnchorBBox(t.X, t.Y, w, h)
}

// applyZoneNoteSizes 把每个区已登记说明的最大渲染宽/高写进模块的 NoteWidth /
// NoteHeight,供 planPartitions 预留说明位置(宽 + 高两维)。**只读内容+字号
// (noteSizeOf),绝不读落点坐标** —— 这是它与 foldZoneNotesIntoModules 的本质
// 区别:fold 消费几何(禁止进画框推导路径,见下),本函数消费文字尺寸(与框
// 几何解耦,幂等)。返回本页文本供调用方构造说明带的占用表。
// best-effort:text.list 失败保持尺寸=0(带退回默认高度),不阻断画框。
func applyZoneNoteSizes(cfg *appConfig, window, docUUID string, zones map[string]*schZoneClaim, modules []partitionModule) []zoneMoveText {
	texts := fetchZoneNoteTexts(cfg, window, docUUID, zones)
	if texts == nil {
		return nil
	}
	setZoneNoteSizes(zones, modules, texts)
	return texts
}

// setZoneNoteSizes 是 applyZoneNoteSizes 的纯核(可单测):按登记记录
// (claim.NoteIDs)把每个模块的 NoteWidth/NoteHeight 设为其说明里最大的一条。
// 已删说明(id 不在 texts 里)静默跳过 —— 与 fold 对 stale 登记的口径一致。
func setZoneNoteSizes(zones map[string]*schZoneClaim, modules []partitionModule, texts []zoneMoveText) {
	sizes := zoneNoteSizes(zones, texts)
	for i := range modules {
		s := sizes[modules[i].Name]
		if s.H > modules[i].NoteHeight {
			modules[i].NoteHeight = s.H
		}
		if s.W > modules[i].NoteWidth {
			modules[i].NoteWidth = s.W
		}
	}
}

// zoneNoteSize 是一个区「已登记说明里最大的一条」的渲染尺寸。
type zoneNoteSize struct{ W, H float64 }

// zoneNoteSizes 是说明尺寸的**唯一纯核**:区名 → 该区已登记说明的最大渲染宽/高。
// `zone-plan`(带高 → 框)与 `zone-arrange` phase A(带高 → 收紧框)共用它 ——
// 一个读文字尺寸、一个读常量,就是两把尺,收紧出来的框装不下后放的 note。
// 已删说明(id 不在 texts 里)静默跳过;尺寸只由内容+字号决定,与落点无关。
func zoneNoteSizes(zones map[string]*schZoneClaim, texts []zoneMoveText) map[string]zoneNoteSize {
	byID := map[string]zoneMoveText{}
	for _, t := range texts {
		byID[t.ID] = t
	}
	out := map[string]zoneNoteSize{}
	for name, zc := range zones {
		if zc == nil {
			continue
		}
		var s zoneNoteSize
		for _, nid := range zc.NoteIDs {
			t, ok := byID[nid]
			if !ok {
				continue
			}
			w, h := noteSizeOf(t.Content, t.FontSize)
			if h > s.H {
				s.H = h
			}
			if w > s.W {
				s.W = w
			}
		}
		out[name] = s
	}
	return out
}

// fetchZoneNoteTexts 拉本页文本(只在有登记说明时才发请求)。best-effort:
// 读失败返回 nil,带退回默认高度,不阻断规划。
func fetchZoneNoteTexts(cfg *appConfig, window, docUUID string, zones map[string]*schZoneClaim) []zoneMoveText {
	needed := false
	for _, zc := range zones {
		if zc != nil && len(zc.NoteIDs) > 0 {
			needed = true
			break
		}
	}
	if !needed {
		return nil
	}
	res, err := requestAutolayoutAction(cfg, "schematic.text.list", window, map[string]any{}, docUUID, "read zone note sizes")
	if err != nil {
		return nil
	}
	return parseZoneMoveTexts(res.Result)
}

// foldZoneNotesIntoModules 把每个区登记的 note bbox 并进该区模块的 BBox。
// best-effort:text.list 失败只警告。
//
// **只许排布类消费者用(目前仅 `sch sheet tidy`)—— 分区框/说明带的推导路径
// (computePartitionPlan)禁止调用**:说明带由内容 bbox 推出,note 再 fold 回
// 内容 bbox 就是「框每重画一次向下长一截」的自增长反馈环(根因 C,见
// computePartitionPlan 内的注释)。sheet tidy 只拿折叠后的体积做整页装箱,
// 不反哺框几何,fold 在那里是「搬动时给说明留地方」,语义安全。
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

// labelScopeReason 把降级折成一句能执行的话。
func labelScopeReason(s schZoneLabelScope) string {
	var parts []string
	if s.WiresUnavailable {
		parts = append(parts, "读不到导线(L1 归属退回距离启发式)")
	}
	if len(s.NoPinGeometry) > 0 {
		parts = append(parts, fmt.Sprintf("这些件没有引脚几何,marker/桩线归不了属:%s",
			strings.Join(s.NoPinGeometry, " ")))
	}
	if len(s.NoCluster) > 0 {
		// 最常见的一种:位号还没分配(`C?`/`U?`)—— `sch clusters` 那边按
		// `位号@primitiveId` 记账,与认领表里的原始串对不上。刷位号即可。
		parts = append(parts, fmt.Sprintf("这些件没有 L1 虚拟组记录:%s(先看位号是不是还没分配 `C?`/`U?`)",
			strings.Join(s.NoCluster, " ")))
	}
	if s.UnownedMarkers > 0 {
		parts = append(parts, fmt.Sprintf("%d 支 marker 无主", s.UnownedMarkers))
	}
	parts = append(parts, "核对:`easyeda sch clusters --members`")
	return strings.Join(parts, ";")
}

func renderPartitionPlan(plan partitionPlan, w io.Writer) {
	fmt.Fprintf(w, "zone-plan: %d partition(s) on sheet (%.0f,%.0f)..(%.0f,%.0f)\n",
		len(plan.Partitions), plan.Sheet.MinX, plan.Sheet.MinY, plan.Sheet.MaxX, plan.Sheet.MaxY)
	for _, p := range plan.Partitions {
		fmt.Fprintf(w, "  [%s]  (%.0f,%.0f)..(%.0f,%.0f)\n",
			strings.Join(p.Modules, " / "), p.BBox.MinX, p.BBox.MinY, p.BBox.MaxX, p.BBox.MaxY)
	}
	if s := plan.LabelScope; s.Degraded {
		fmt.Fprintf(w, "⚠ 标签范围口径降级(labelScopeDegraded=true)—— 模块 bbox 罩不住的旗/桩线,判据也验不了:%s\n",
			labelScopeReason(s))
	}
	v := plan.Validation
	fmt.Fprintf(w, "validation: sheetOverflow=%d partitionOverlap=%d titleBlockHits=%d moduleOutsideZone=%d labelCollisions=%d\n",
		v.SheetOverflow, v.PartitionOverlap, v.TitleBlockHits, v.ModuleOutsideZone, v.LabelCollisions)
	for _, d := range v.ModuleOutsideDetail {
		fmt.Fprintf(w, "  ✗ %s\n", d)
	}
	if v.clean() {
		fmt.Fprintln(w, "✓ plan is clean")
		return
	}
	// **先分清「装不下」和「摆得不好」**。原来只有一句「adjust margins/gutter or
	// the zone claims」——对一颗 421 高的模组来说那是条做不到的建议,人照着试、
	// 试不动,然后把整条判据当噪音跳过。判据的价值不在报错,在报出能执行的下一步。
	if adv := capacityAdvice(plan.Capacity); adv != "" {
		fmt.Fprintf(w, "✗ %s\n", adv)
		return
	}
	// 归组是「一区一框」之后,--gutter 不再影响分区框怎么分,所以这里不能再建议
	// 「收紧 --gutter」——重叠 = 两个区的体积真的互相压,只有挪件/重排/拆页三条路。
	fmt.Fprintln(w, "✗ plan has violations — 容量是够的,是摆放/间距问题:"+
		"`sch zone-arrange --apply` 整页重排、用 `sch group-move` 挪开互相顶住的组,"+
		"或把模块拆到下一页")
}

// partitionDrawGate 是「这份计划能不能画」的**唯一判据**,两条画框路径
// (runPartitionDraw 与 runPartitionDrawResilient)共用它。
//
// 拆出来的理由与本文件其余的「一把尺」同一条:两处各写一遍 `if !clean` 时,
// 韧性路径的报文丢了 ModuleOutsideDetail 与降级说明,同一次拒绝在两条路径上
// 说的话不一样(判据必须给能执行的下一步)。**判据本身一个字都不许放宽**:
// clean() 五项全 0 才画。
func partitionDrawGate(plan partitionPlan) error {
	if plan.Validation.clean() {
		return nil
	}
	why := ""
	if len(plan.Validation.ModuleOutsideDetail) > 0 {
		why = "\n  " + strings.Join(plan.Validation.ModuleOutsideDetail, "\n  ")
	}
	if plan.LabelScope.Degraded {
		why += "\n  ⚠ 标签范围口径降级:" + labelScopeReason(plan.LabelScope)
	}
	if plan.Validation.PartitionOverlap > 0 {
		// 分区框重叠 = 两个区的 L1 体积本身交叠(框由体积撑出来,不裁剪)。
		// 修法是挪件,不是把框切短、更不是把判据调松。
		why += fmt.Sprintf("\n  ⚠ %d 对分区框重叠:两个区的成员体积本身就交叠 —— "+
			"`sch zone-arrange --apply` 重排(它按 gutter 隔开各区),或 `sch group-move` 手工挪开",
			plan.Validation.PartitionOverlap)
	}
	return fmt.Errorf("partition plan has violations %+v — refusing to draw overlapping/out-of-sheet annotations%s",
		plan.Validation, why)
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
	clearRecorded := func(phase string) (bool, error) {
		prev, source := recordedZoneFrames(st, docUUID)
		if prev == nil {
			return false, nil
		}
		ids := append(append([]string(nil), prev.Rects...), prev.Texts...)
		if len(ids) > 0 {
			if _, err := requestAutolayoutAction(pinnedCfg, "schematic.graphics.delete", win,
				map[string]any{"ids": ids}, docUUID, phase); err != nil {
				return false, err
			}
		}
		removeRecordedZoneFrames(st, docUUID, source)
		fmt.Fprintf(stderr, "cleared %d previous zone-frame primitive(s) on page %s\n", len(ids), docUUID)
		return true, nil
	}

	if clear {
		hadPrevious, cerr := clearRecorded("clear previous partition frames")
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
	if err := partitionDrawGate(plan); err != nil {
		return err
	}
	if fontSize <= 0 {
		fontSize = defaultPartitionZoneFontSize
	}
	if _, err := clearRecorded("clear previous partition frames"); err != nil {
		return err
	}
	res, err := requestAutolayoutAction(pinnedCfg, "schematic.graphics.create", win,
		buildPartitionGraphicsPayload(plan, fontSize, color), docUUID, "draw partition frames")
	if err != nil {
		return fmt.Errorf("%w —— 注意:旧的分区框已被清除、新的没画成,**本页现在没有分区框**;"+
			"先 inspect 当前页再重跑 `sch zone-draw`", err)
	}
	v := map[string]any{
		"ok":    true,
		"rects": asStringSlice(res.Result["rectangles"]),
		"texts": asStringSlice(res.Result["texts"]),
	}
	frames, verr := validateZoneDrawResult(v, len(plan.Partitions))
	if verr != nil {
		ids := append(append([]string(nil), frames.Rects...), frames.Texts...)
		if len(ids) > 0 {
			if _, cerr := requestAutolayoutAction(pinnedCfg, "schematic.graphics.delete", win,
				map[string]any{"ids": ids}, docUUID, "clean up incomplete partition frames"); cerr != nil {
				setRecordedZoneFrames(st, docUUID, "partition", frames)
				_ = savePcbStageState(st)
				return fmt.Errorf("%v; typed cleanup failed: %w; recovery ids retained", verr, cerr)
			}
		}
		return verr
	}
	setRecordedZoneFrames(st, docUUID, "partition", frames)
	if err := savePcbStageState(st); err != nil {
		ids := append(append([]string(nil), frames.Rects...), frames.Texts...)
		if _, cerr := requestAutolayoutAction(pinnedCfg, "schematic.graphics.delete", win,
			map[string]any{"ids": ids}, docUUID, "roll back untracked partition frames"); cerr != nil {
			return fmt.Errorf("persist partition-frame ids: %v; rollback failed: %w", err, cerr)
		}
		return fmt.Errorf("persist partition-frame ids: %w", err)
	}
	if err := saveZoneDocument(pinnedCfg, win, docUUID, "save partition zone frames"); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "drew %d partition frame(s) + %d title(s) on page %s; schematic saved\n",
		len(frames.Rects), len(frames.Texts), docUUID)
	return nil
}
