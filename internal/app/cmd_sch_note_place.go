package app

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ── sch note 自动落点:把说明文字当成和器件同级的布局对象 ────────────────────
//
// 用户纠偏(2026-08-13):「每个编组对象还有 title、注释,属于同级别的;计算摆放
// 位置的时候可以计算现有虚拟组的 xy 和长宽碰撞,自动算出对齐和层叠方式——这块
// 要在代码里实现」。
//
// 在此之前 `sch note` 的 --x/--y 是**必填**:落点全靠调用方(人或 agent)拿
// `sch list --include-bbox` 自己估,于是三条说明齐刷刷压在器件和网标上——不是
// 因为缺少碰撞判据(zone-plan 早有 boxesOverlap / LabelCollisions,note 的尺寸
// 估算 schNoteBBoxEstimate 也早就存在),而是因为**文字只被动地"被框住",从没
// 主动参与避让求解**。(注:登记的说明曾被 foldZoneNotesIntoModules 折进分区框
// 口径 —— 那正是「说明带自增长反馈环」的根因 C,2026-08-19 已从 zone-plan 路径
// 移除;说明的家是构造出来的说明带,不反哺框几何。)
//
// 本文件补上那一步:note 和器件、marker、已有文字、标题栏 keep-out 一起进同一
// 张碰撞表,自动求一个不压任何东西的锚点。尺寸估算复用 schNoteBBoxEstimate ——
// 判据与生成必须同源,两套估算一旦漂移就会"算的时候不撞、画出来撞"。

// noteGap 是说明与任何已有图元之间的最小视觉间隙(原理图单位)。比 marker 的
// 重叠阈值大得多:贴着边不算重叠,但读起来仍然是"糊在一起"。
const noteGap = 16.0

// noteAnchorStep 是候选锚点的扫描步长(落在 5 格连接网格上)。
const noteAnchorStep = 20.0

// noteCorridorTiers 是区外走廊(正下方/正上方/左右侧)每个方向扫描的档数:
// 每档沿远离区框的方向退一个 noteAnchorStep。此前「区正下方」只有单个候选点,
// 一撞就整个跌进整页扫描,说明被甩到页角(真机症状)。
const noteCorridorTiers = 5

// noteMinReadableWidth 是说明允许被折到的最窄渲染宽度。再窄就成了竖排单字,
// 不叫说明。**窄框必须为说明横向扩边到这个宽度** —— 2026-08-19 真机:POWER_IN
// 区里只有一个 2 脚接线端子,框宽 68,任何可读说明都比 68 宽,于是「装不进框」
// 与「note-outside-zone 必报」同时成立,判据变成一条永远响、又给不出可执行
// 修法的噪音。框由器件 bbox 推导,但**可以为说明扩边**。
const noteMinReadableWidth = 120.0

// noteBandDepthSteps 是说明带为躲开「伸进带里的外来图元」最多下探的档数
// (每档一个 noteAnchorStep)。真机取证:P1_POWER 的说明带 y[472,528] 里
// x[604,686] 被邻区 L1 的桩线/marker 占住,435 宽的说明在带内唯一的横向候选点
// 上必撞 —— 旧行为跌进「区外走廊」把说明踢出框,新行为是把框底下探到占用之下,
// 说明仍在自己的框里。
const noteBandDepthSteps = 8

// requiredNoteWidth 把「说明渲染宽」换算成能装下它的**框宽**(左右各留 noteGap)。
// 与 requiredNoteBand(高)配对:预留说明位置是二维的,此前只有高度参与预留,
// 宽度既不参与预留也不参与落点判定,而 note-outside-zone 的判据却是严格的框包含
// —— 生成侧与判定侧两把尺,必然出框外说明。
func requiredNoteWidth(noteW float64) float64 { return noteW + 2*noteGap }

// noteWrapWidth 把框宽换算成说明的折行宽度上限:框内可用宽度,但不低于
// noteMinReadableWidth(低于它就为说明扩边,而不是把字切成竖排)。
func noteWrapWidth(frameW float64) float64 {
	if w := frameW - 2*noteGap; w > noteMinReadableWidth {
		return w
	}
	return noteMinReadableWidth
}

// noteRuneWidth 是 noteSizeOf 的逐字口径 —— 折行必须用**同一把尺**量。此前
// `sch note` 借用 wrapNoteLines(组说明用的那把尺,常量 groupNoteFontSize=10.2),
// 而尺寸回读走 noteSizeOf(--font-size,默认 10):折出来的行按 10.2 量刚好不超,
// 按 10 量也没超,但两边的"框宽预算"从来对不上,再叠上吸格(snapNote 可把落点
// 右移 2.5)就会出现「按 A 算不出框、画出来出框」。两把尺是本仓的复发病。
func noteRuneWidth(r rune, fontSize float64) float64 {
	if r > 0x2E80 { // CJK 全宽,与 noteSizeOf 同口径
		return fontSize
	}
	return fontSize * 0.55
}

// wrapNoteContent 把一段可能含 \n 的说明按 maxWidth 折行,**按它自己的字号量**。
// 必须先按 \n 拆行再逐行 wrap:此前把整段当一行 wrap,宽度累计跨过换行符继续加,
// 于是「首行完整、第二行开头 3~4 字就被折断」(2026-08-18 P2 LED 说明真机定案:
// "丝印标正负极性" 折成 "丝印标正/负极性",与宽度无关、纯粹是账没清零)。
func wrapNoteContent(content string, fontSize, maxWidth float64) string {
	if fontSize <= 0 {
		fontSize = schNoteDefaultFontSize
	}
	if maxWidth < noteMinReadableWidth {
		maxWidth = noteMinReadableWidth
	}
	var out []string
	for _, ln := range strings.Split(content, "\n") {
		line, w := make([]rune, 0, len(ln)), 0.0
		for _, r := range ln {
			rw := noteRuneWidth(r, fontSize)
			if w+rw > maxWidth+acOverlapEps && len(line) > 0 {
				out = append(out, string(line))
				line, w = line[:0], 0
			}
			line = append(line, r)
			w += rw
		}
		out = append(out, string(line)) // 空行照留:说明的分段是作者的意图
	}
	return strings.Join(out, "\n")
}

// noteSizeOf 估算一段说明文字的渲染尺寸。schNoteBBoxEstimate 是它的 bbox 版本
// (锚点=左上角,y-UP 向下排行) —— 两者共用同一套字宽/行高口径。
func noteSizeOf(content string, fontSize float64) (w, h float64) {
	if fontSize <= 0 {
		fontSize = schNoteDefaultFontSize
	}
	lines := strings.Split(content, "\n")
	for _, ln := range lines {
		lw := 0.0
		for _, r := range ln {
			if r > 0x2E80 { // CJK 全宽
				lw += fontSize
			} else {
				lw += fontSize * 0.55
			}
		}
		if lw > w {
			w = lw
		}
	}
	return w, float64(len(lines)) * fontSize * 1.3
}

// noteAnchorBBox 把「锚点 + 尺寸」还原成 bbox(锚点=左上角,文字向下排行)。
func noteAnchorBBox(x, y, w, h float64) layoutBBox {
	return layoutBBox{MinX: x, MinY: y - h, MaxX: x + w, MaxY: y}
}

// planNoteAnchor 求一个不与任何障碍碰撞的说明锚点。
//
// 候选顺序体现「说明属于它那个区」的语义:先贴着区内容的下沿(最常见的读图习惯
// ——先看电路再看下面那行说明),再区内上沿(标题带之下),然后区外正下方,最后
// 整页从下往上扫。zoneRect 为 nil 时直接走整页扫描。
//
// 纯函数:障碍、图纸、尺寸进,锚点出,不碰网络。
func planNoteAnchor(w, h float64, obstacles []layoutBBox, zoneRect, noteBand *layoutBBox, sheet layoutBBox, keepout *layoutBBox) (x, y float64, ok bool) {
	free := func(bx, by float64) bool {
		return noteSpotFree(noteAnchorBBox(bx, by, w, h), obstacles, sheet, keepout)
	}
	// try 先把候选吸到网格再判碰:**判定坐标必须 = 落地坐标**。吸附后再判,才不会
	// 出现「按原始候选算不撞、按吸附后的落点画出来擦上」的半格假阴性。
	try := func(bx, by float64) (float64, float64, bool) {
		sx, sy := snapNote(bx), snapNote(by)
		if free(sx, sy) {
			return sx, sy, true
		}
		return 0, 0, false
	}

	var cands [][2]float64
	// **说明带优先**:分区框底部留出来的那条带就是给它的(区名在顶、说明在底,
	// 都在框内)。带里放不下才退到下面那串兜底候选 —— 那些会把说明挤出框外。
	if noteBand != nil {
		frame := *noteBand
		if zoneRect != nil {
			frame = *zoneRect
		}
		if sx, sy, hit := scanNoteBand(*noteBand, frame, w, h, obstacles, sheet, keepout); hit {
			return sx, sy, true
		}
	}
	if zoneRect != nil {
		z := *zoneRect
		// ① 区内容下沿之下(区内);② 区内上沿之下;③ 区左/右外侧同高;④ 区正下方。
		for _, dy := range []float64{0, -noteAnchorStep, -2 * noteAnchorStep} {
			cands = append(cands, [2]float64{z.MinX + noteGap, z.MinY + h + noteGap + dy})
		}
		for _, dy := range []float64{0, noteAnchorStep} {
			cands = append(cands, [2]float64{z.MinX + noteGap, z.MaxY - noteGap - dy})
		}
		cands = append(cands,
			[2]float64{z.MinX + noteGap, z.MinY - noteGap},
			[2]float64{z.MaxX + noteGap, z.MaxY - noteGap},
			[2]float64{z.MinX - w - noteGap, z.MaxY - noteGap},
		)
		// ⑤ 区外走廊多档扫描:框内(和上面那几个单点)全满时,先沿「正下方 → 正上方
		//   → 右侧 → 左侧」四条走廊逐档找位置,而不是直接跌进整页扫描把说明甩到页角
		//   —— 走廊里的落点仍然「贴着自己的区」,读图时一眼能对上。
		cands = append(cands, noteCorridorCandidates(z, w, h)...)
		for _, c := range cands {
			if sx, sy, hit := try(c[0], c[1]); hit {
				return sx, sy, true
			}
		}
	}
	// 整页扫描(最后的兜底)。无区时保持传统:从图纸下方往上、从左往右 —— 左下角
	// 通常是图签之外最大的连续空白,也是工程图放总说明的传统位置。**有区时按离区
	// 中心的距离升序试**:说明属于它那个区,兜底也该落在尽量近的地方,而不是按扫描
	// 序落到图纸左下角(真机症状:框内无空位 → 说明跑到页角)。
	var pageCands [][2]float64
	for by := sheet.MinY + h + noteGap; by <= sheet.MaxY-noteGap; by += noteAnchorStep {
		for bx := sheet.MinX + noteGap; bx <= sheet.MaxX-w-noteGap; bx += noteAnchorStep {
			pageCands = append(pageCands, [2]float64{bx, by})
		}
	}
	if zoneRect != nil {
		cx := (zoneRect.MinX + zoneRect.MaxX) / 2
		cy := (zoneRect.MinY + zoneRect.MaxY) / 2
		dist2 := func(c [2]float64) float64 {
			// 候选 bbox 中心到区中心的平方距离(锚点=左上角,文字向下排行)。
			dx := (c[0] + w/2) - cx
			dy := (c[1] - h/2) - cy
			return dx*dx + dy*dy
		}
		sort.SliceStable(pageCands, func(i, j int) bool { return dist2(pageCands[i]) < dist2(pageCands[j]) })
	}
	for _, c := range pageCands {
		if sx, sy, hit := try(c[0], c[1]); hit {
			return sx, sy, true
		}
	}
	return 0, 0, false
}

// noteCorridorCandidates 生成区框四周走廊的多档候选锚点,按「正下方 → 正上方 →
// 右侧 → 左侧」的优先序;每个方向 noteCorridorTiers 档,逐档远离区框一个
// noteAnchorStep,同档内沿走廊按离区起点近的方向步进。锚点=bbox 左上角(y-UP,
// 文字向下排行),越界/压图元由调用方的 free() 统一裁决。
func noteCorridorCandidates(z layoutBBox, w, h float64) [][2]float64 {
	var out [][2]float64
	// 走廊横向扫到「说明右沿不超出区右沿」为止;区比说明还窄时只试左对齐一列。
	xEnd := math.Max(z.MinX+noteGap, z.MaxX-w)
	// 正下方走廊:说明整体在区下沿之下(bbox 顶 = 锚点 y)。
	for k := 0; k < noteCorridorTiers; k++ {
		y := z.MinY - noteGap - float64(k)*noteAnchorStep
		for x := z.MinX + noteGap; x <= xEnd+acOverlapEps; x += noteAnchorStep {
			out = append(out, [2]float64{x, y})
		}
	}
	// 正上方走廊:说明整体在区上沿之上(bbox 底 = y-h ≥ z.MaxY+noteGap)。
	for k := 0; k < noteCorridorTiers; k++ {
		y := z.MaxY + noteGap + h + float64(k)*noteAnchorStep
		for x := z.MinX + noteGap; x <= xEnd+acOverlapEps; x += noteAnchorStep {
			out = append(out, [2]float64{x, y})
		}
	}
	// 右侧走廊:从区上沿往下扫(与 ③ 的右侧单点同一读图习惯)。
	for k := 0; k < noteCorridorTiers; k++ {
		x := z.MaxX + noteGap + float64(k)*noteAnchorStep
		for y := z.MaxY - noteGap; y >= z.MinY+h-acOverlapEps; y -= noteAnchorStep {
			out = append(out, [2]float64{x, y})
		}
	}
	// 左侧走廊。
	for k := 0; k < noteCorridorTiers; k++ {
		x := z.MinX - w - noteGap - float64(k)*noteAnchorStep
		for y := z.MaxY - noteGap; y >= z.MinY+h-acOverlapEps; y -= noteAnchorStep {
			out = append(out, [2]float64{x, y})
		}
	}
	return out
}

// noteSpotFree 是「这个落点能不能用」的**唯一**判据:图纸边距 + 图签禁区 +
// 与任何障碍留 noteGap。planner(reserveZoneNoteArea)与 sch note 落点共用它。
func noteSpotFree(b layoutBBox, obstacles []layoutBBox, sheet layoutBBox, keepout *layoutBBox) bool {
	if b.MinX < sheet.MinX+noteGap || b.MaxX > sheet.MaxX-noteGap ||
		b.MinY < sheet.MinY+noteGap || b.MaxY > sheet.MaxY-noteGap {
		return false
	}
	if keepout != nil && boxesGapOverlap(b, *keepout, 0) {
		return false
	}
	for _, ob := range obstacles {
		if boxesGapOverlap(b, ob, noteGap) {
			return false
		}
	}
	return true
}

// scanNoteBand 在说明带里从左往右扫一个可用落点,**并要求落点整体落在框内**。
//
// 框内包含此前不是构造保证:带内候选只判纸边/图签/图元,不判框。于是「说明宽
// 435、框宽 435」的真机场景里,带内唯一候选算出来的 bbox 右沿本来就探出框外,
// 再叠上吸格右移 2.5,画出来必然框外 —— 而 note-outside-zone 判的正是框包含。
// 判定与生成同一把尺:能用的落点必须先在框里。
func scanNoteBand(band, frame layoutBBox, w, h float64, obstacles []layoutBBox,
	sheet layoutBBox, keepout *layoutBBox) (x, y float64, ok bool) {
	by := band.MinY + h + noteGap
	xEnd := math.Max(band.MinX+noteGap, band.MaxX-w-noteGap)
	for bx := band.MinX + noteGap; bx <= xEnd+acOverlapEps; bx += noteAnchorStep {
		sx, sy := snapNote(bx), snapNote(by)
		b := noteAnchorBBox(sx, sy, w, h)
		if !bboxContains(frame, b) {
			continue
		}
		if noteSpotFree(b, obstacles, sheet, keepout) {
			return sx, sy, true
		}
	}
	return 0, 0, false
}

// noteExpandCeilX / noteExpandFloorX / noteExpandFloorY 是框为说明扩边时**不许
// 越过**的三条界:纸边(内缩 sheetEdgeMinGap)、图签安全带、以及在对应方向上
// 挡路的邻区框(留一个 gutter)。有了这三条界,「为说明扩边」永远不会自己造出
// partitionOverlap / sheetMarginHits —— 否则 zone-draw 会因为我们自己撑出来的
// 违规而拒绝画框,把「永远报警」换成「永远画不出」。
func noteExpandCeilX(rect layoutBBox, blockers []layoutBBox, sheet layoutBBox, gutter float64) float64 {
	lim := sheet.MaxX - sheetEdgeMinGap
	for _, b := range blockers {
		if b.MaxY <= rect.MinY || b.MinY >= rect.MaxY {
			continue // 纵向不重叠,挡不着
		}
		if b.MinX >= rect.MaxX && b.MinX-gutter < lim {
			lim = b.MinX - gutter
		}
	}
	return lim
}

func noteExpandFloorX(rect layoutBBox, blockers []layoutBBox, sheet layoutBBox, gutter float64) float64 {
	lim := sheet.MinX + sheetEdgeMinGap
	for _, b := range blockers {
		if b.MaxY <= rect.MinY || b.MinY >= rect.MaxY {
			continue
		}
		if b.MaxX <= rect.MinX && b.MaxX+gutter > lim {
			lim = b.MaxX + gutter
		}
	}
	return lim
}

func noteExpandFloorY(rect layoutBBox, blockers []layoutBBox, sheet layoutBBox, gutter float64) float64 {
	lim := sheet.MinY + sheetEdgeMinGap
	for _, b := range blockers {
		if b.MaxX <= rect.MinX || b.MinX >= rect.MaxX {
			continue // 横向不重叠
		}
		if b.MaxY <= rect.MinY && b.MaxY+gutter > lim {
			lim = b.MaxY + gutter
		}
	}
	return lim
}

// reserveZoneNoteArea 是「为一条说明在它自己的分区框里留地方」的**唯一实现**
// (纯函数)。planner(planPartitions 的第二遍)与 `sch note` 的自动落点共用它 ——
// 这就是「生成与判定同一把尺」的机械保证:planner 按它算出框/带,note 按它算出
// 落点,落点必然被框包含,note-outside-zone 结构上不再误报。
//
//	rect      : 未为说明扩边前的分区框(内容 ± pad + 标题带 + 按高度预留的带)
//	bandTop   : 说明带顶 = 器件区下沿(= content.MinY - partitionContentPad)。
//	            **扩边时它固定不动** —— 器件区一寸不挤,框只向外长。
//	w,h       : 说明的渲染尺寸(已按框宽折行)
//	obstacles : 说明不许压的图元(器件 / marker 文字带 / 未登记的自由文本)
//	neighbors : 其它分区的**基础框**(扩边前),用来定扩边界
//
// 两个维度都参与预留:
//   - 宽:框宽 < requiredNoteWidth(w) 就横向扩边(先右后左)。窄框(2 脚端子)
//     结构上装不下任何可读说明,唯一讲得通的策略就是让框为说明扩边。
//   - 高:带高至少 requiredNoteBand(h);带内被外来图元(邻区桩线/marker)占住时
//     逐档下探,直到带里有一处可用落点。
//
// ok=false = 这一区在可扩边界内结构上装不下这条说明。**调用方必须显式报出来**,
// 不许静默把说明踢到框外 —— 那正是这次要根治的症状。
func reserveZoneNoteArea(rect layoutBBox, bandTop, w, h float64, obstacles []layoutBBox,
	sheet layoutBBox, keepout *layoutBBox, neighbors []layoutBBox, gutter float64) (outRect, band layoutBBox, ax, ay float64, ok bool) {

	blockers := append([]layoutBBox(nil), neighbors...)
	if safe := inflatedTitleKeepout(keepout); safe != nil {
		blockers = append(blockers, *safe)
	}
	outRect = rect
	// ① 横向扩边:框宽装不下说明就往外长,先右后左,不越过扩边界。
	if need := requiredNoteWidth(w); need > outRect.MaxX-outRect.MinX {
		deficit := need - (outRect.MaxX - outRect.MinX)
		right := math.Min(deficit, math.Max(0, noteExpandCeilX(outRect, blockers, sheet, gutter)-outRect.MaxX))
		outRect.MaxX += right
		if left := deficit - right; left > 0 {
			outRect.MinX -= math.Min(left, math.Max(0, outRect.MinX-noteExpandFloorX(outRect, blockers, sheet, gutter)))
		}
	}
	// ② 纵向:带高 ≥ requiredNoteBand(h),带内有占用就逐档下探。
	floor := noteExpandFloorY(outRect, blockers, sheet, gutter)
	base := math.Min(outRect.MinY, bandTop-requiredNoteBand(h))
	bandOf := func(r layoutBBox) layoutBBox {
		return layoutBBox{MinX: r.MinX, MinY: r.MinY, MaxX: r.MaxX,
			MaxY: math.Min(r.MaxY, math.Max(r.MinY, bandTop))}
	}
	for k := 0; k <= noteBandDepthSteps; k++ {
		r := outRect
		r.MinY = base - float64(k)*noteAnchorStep
		if r.MinY < floor-acOverlapEps {
			break
		}
		b := bandOf(r)
		if x, y, hit := scanNoteBand(b, r, w, h, obstacles, sheet, keepout); hit {
			return r, b, x, y, true
		}
	}
	// 装不下:仍然把「最小预留」形态交出去(带高够、宽度尽力扩过),但 ok=false ——
	// 调用方据此发可执行的告警,而不是假装成功。
	outRect.MinY = math.Min(outRect.MinY, math.Max(base, floor))
	return outRect, bandOf(outRect), 0, 0, false
}

// snapNote 把锚点吸到 5 格网格(与连接网格同口径,避免半格漂移)。
func snapNote(v float64) float64 { return math.Round(v/destaggerGrid) * destaggerGrid }

// boxesGapOverlap 报告两个 bbox 在外扩 gap 后是否相交 —— gap=0 即普通相交。
func boxesGapOverlap(a, b layoutBBox, gap float64) bool {
	return a.MinX-gap < b.MaxX && a.MaxX+gap > b.MinX &&
		a.MinY-gap < b.MaxY && a.MaxY+gap > b.MinY
}

// collectNoteObstacles 汇总一页上所有「说明不许压」的东西:器件与 marker 的判定
// bbox(marker 含文字带,与 sch check 的 marker-overlap 同口径)、已有文字的估算
// bbox(标题、别的说明)。图纸边框本身(componentType=sheet)不算障碍。
func collectNoteObstacles(comps []layoutComp, texts []zoneMoveText) []layoutBBox {
	var out []layoutBBox
	for _, c := range comps {
		if c.BBox == nil || c.ComponentType == "sheet" {
			continue
		}
		out = append(out, markerJudgeBBox(c))
	}
	for _, t := range texts {
		out = append(out, schNoteBBoxEstimate(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MinY != out[j].MinY {
			return out[i].MinY < out[j].MinY
		}
		return out[i].MinX < out[j].MinX
	})
	return out
}

// filterUnregisteredTexts 去掉所有**已登记到某个区**的说明,留下自由文本。
//
// 说明带的预留(reserveZoneNoteArea 的下探判定)必须看不见已登记说明,否则
// 「放一条 note → note 进带 → 带被它自己顶得再下探 → note 又不在带里」就是
// 根因 C 那个自增长反馈环的翻版。登记说明的家是构造出来的带,它不参与带的推导。
func filterUnregisteredTexts(texts []zoneMoveText, registered map[string]bool) []zoneMoveText {
	if len(registered) == 0 {
		return texts
	}
	out := make([]zoneMoveText, 0, len(texts))
	for _, t := range texts {
		if !registered[t.ID] {
			out = append(out, t)
		}
	}
	return out
}

// registeredNoteIDs 汇总一页上所有登记在区名下的说明 id。
func registeredNoteIDs(zones map[string]*schZoneClaim) map[string]bool {
	out := map[string]bool{}
	for _, zc := range zones {
		if zc == nil {
			continue
		}
		for _, id := range zc.NoteIDs {
			out[id] = true
		}
	}
	return out
}

// partitionBaseRects 返回**除 zoneName 之外**每个分区的基础框(为说明扩边之前
// 的形态)。扩边界必须钉在基础框上,而不是别人扩过之后的框:`sch note` 拿到的
// 是「本区还没登记这条说明」的计划,planner 重算时拿到的是「已登记」的计划,
// 两边只有都用基础框,算出来的扩边界才逐字段相同(同一把尺)。
func partitionBaseRects(parts []partitionRect, zoneName string) []layoutBBox {
	var out []layoutBBox
	for _, p := range parts {
		if strInSlice(p.Modules, zoneName) {
			continue
		}
		out = append(out, p.baseRect())
	}
	return out
}

// matchNotePartition 在分区计划里找 zoneName 归属的分区(纯函数)。
//
// 命中:返回该区的框与说明带,以及**其它所有分区的矩形**(根因 B:回退链的每
// 一档都必须把邻区的框当硬障碍,否则求解器会把"邻区框内的空白"当可用空间,
// 说明落进别人的框、把本区 bbox 拉炸、partitionOverlap=1 死锁)。
// 未命中:matched=false,且返回**全部**分区矩形 —— 落不进任何区的说明只能整页
// 避让,但绝不许落进任何分区框里。
func matchNotePartition(parts []partitionRect, zoneName string) (zoneRect, noteBand *layoutBBox, others []layoutBBox, matched bool) {
	idx := -1
	for i, p := range parts {
		if strInSlice(p.Modules, zoneName) {
			idx = i
			break
		}
	}
	for i, p := range parts {
		if i != idx {
			others = append(others, p.BBox)
		}
	}
	if idx < 0 {
		return nil, nil, others, false
	}
	r := parts[idx].BBox
	nb := parts[idx].NoteBBox
	return &r, &nb, others, true
}

// placeSchNote 是自动落点的 I/O 外壳:拉一次页面几何(图元 + 已有文字 + 图纸 +
// 图签 keep-out + 该区的分区矩形),求锚点写回 *x/*y。
//
//   - auto=true(调用方没给 --x/--y):求解失败 = 硬错误。宁可不画,也不把说明
//     糊在电路上——那正是这次要根治的症状。
//   - auto=false(调用方显式给了坐标):坐标一字不改,但仍做一次碰撞回读,压到
//     东西就往 warns 里加一句警告,让人知道自己压了什么。
//
// 返回值:warns 是**必须**转给用户 stderr 的降级/未命中警告(绝不静默——根因 A
// 的最坏形态就是"匹配不到 → 整页兜底 → 还报登记成功");zoneMatched 表示
// --zone 是否在本页分区计划里命中了一个分区(命中才有说明带可落)。
//
// 几何读取失败一律降级为「照给定坐标画」并给出提示:说明是注释,不该因为读不到
// 布局就阻断。
func placeSchNote(cfg *appConfig, window, docUUID, zoneRef string, content *string, fontSize float64, auto bool, x, y *float64) (warns []string, zoneMatched bool, err error) {
	w, h := noteSizeOf(*content, fontSize)

	// 根因 A:--zone 先过统一注册表解析(ADR-0004 Decision 3,resolveLayoutObject)
	// —— 注册表全名 `ch340c_usb_serial(C4)/U`、末段短名 `U`、组 id、唯一前缀命中的
	// 都是同一个条目,zoneName() 投影出的短名正是分区计划 Modules 里的名字。
	// 解析失败在**创建任何图元之前**硬报错(报错自带本页全部可用名)——此前拿
	// 原始引用与 plan 短名做精确串匹配,传全名静默落空、跌进整页兜底,命令还照样
	// 报 "registered to zone" 成功(2026-08-19 真机 E2E 定案)。
	zoneName := ""
	if zoneRef != "" {
		obj, _, _, rerr := resolveLayoutZone(cfg, window, docUUID, zoneRef)
		if rerr != nil {
			return nil, false, rerr
		}
		zoneName = obj.zoneName()
	}

	res, rerr := requestAutolayoutAction(cfg, "schematic.components.list", window,
		map[string]any{"includeBBox": true}, docUUID, "read layout for note placement")
	if rerr != nil {
		if auto {
			return nil, false, fmt.Errorf("自动落点需要页面几何,但 components.list 失败:%w(可显式给 --x/--y 绕过)", rerr)
		}
		return []string{"note 落点未做碰撞校验(读取页面几何失败)"}, false, nil
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		if auto {
			return nil, false, fmt.Errorf("自动落点需要页面几何,但解析失败:%w(可显式给 --x/--y 绕过)", perr)
		}
		return []string{"note 落点未做碰撞校验(页面几何解析失败)"}, false, nil
	}
	sheet := sheetBBoxOf(comps)
	if sheet == nil {
		if auto {
			return nil, false, fmt.Errorf("自动落点需要图纸边框(sheet)bbox,本页读不到——请显式给 --x/--y")
		}
		return []string{"note 落点未做碰撞校验(读不到图纸 bbox)"}, false, nil
	}
	keepout, _ := titleBlockKeepout(sheet)

	var texts []zoneMoveText
	if tres, terr := requestAutolayoutAction(cfg, "schematic.text.list", window,
		map[string]any{}, docUUID, "read existing notes"); terr == nil {
		texts = parseZoneMoveTexts(tres.Result)
	}
	obstacles := collectNoteObstacles(comps, texts)

	// 目标区的矩形:优先用 zone-plan 给该区算出的分区框(说明就该待在自己区里),
	// 拿不到就退化成整页扫描——但**绝不静默**:未命中/计划不可用都要出警告。
	//
	// **折行按框宽、按说明自己的字号量**(同一把尺,见 wrapNoteContent):折完的
	// 宽高再一起进 reserveZoneNoteArea —— 预留是二维的,宽度装不下就把框横向扩边,
	// 带内被邻区桩线占住就把框底下探,**绝不把说明踢出框**(2026-08-19 真机:
	// 435 宽说明 + 435 宽框 + 带内占用 → 旧行为落到框外下方 (250,435))。
	//
	// solverObstacles = 页面图元障碍 + 分区框障碍(根因 B)。分区框只喂给自动
	// 求解器 —— 显式 --x/--y 落在自己区框内是完全合法的,不该被框障碍误警。
	var zoneRect, noteBand *layoutBBox
	var bandAnchor *[2]float64
	solverObstacles := obstacles
	if zoneName != "" {
		opts := defaultPartitionOpts()
		if plan, zoneClaims, zerr := computePartitionPlan(cfg, window, docUUID, opts); zerr == nil {
			var otherRects []layoutBBox
			zoneRect, noteBand, otherRects, zoneMatched = matchNotePartition(plan.Partitions, zoneName)
			if zoneMatched {
				baseRect := *zoneRect
				if wrapped := wrapNoteContent(*content, fontSize, noteWrapWidth(zoneRect.MaxX-zoneRect.MinX)); wrapped != *content {
					*content = wrapped
					w, h = noteSizeOf(*content, fontSize)
				}
				// 预留 = planner 登记之后会重算出的几何(同一个函数,同一份
				// 障碍口径:已登记说明一律排除,否则又是自增长反馈环)。
				bandObstacles := collectNoteObstacles(comps,
					filterUnregisteredTexts(texts, registeredNoteIDs(zoneClaims)))
				zr, nb, ax, ay, fit := reserveZoneNoteArea(*zoneRect, noteBand.MaxY, w, h,
					bandObstacles, *sheet, keepout, partitionBaseRects(plan.Partitions, zoneName), opts.Gutter)
				zoneRect, noteBand = &zr, &nb
				if fit {
					// 同区已有说明(登记过的)不在 bandObstacles 里 —— 带内再扫一遍
					// 完整障碍表,让第二条说明并排右移而不是压在第一条上。扫描仍被
					// 「框内包含」约束,所以怎么挪都还在自己的框里。
					if !noteSpotFree(noteAnchorBBox(ax, ay, w, h), obstacles, *sheet, keepout) {
						ax, ay, fit = scanNoteBand(nb, zr, w, h, obstacles, *sheet, keepout)
					}
				}
				if fit {
					bandAnchor = &[2]float64{ax, ay}
				} else {
					warns = append(warns, fmt.Sprintf(
						"区 %q 的说明带(%.0f×%.0f)在可扩边界内装不下这条说明(%.0f×%.0f):说明只能落到框外,"+
							"`sch check` 会报 note-outside-zone —— 缩短文字或减小 --font-size,"+
							"或用 `sch group-move` 把邻近模块挪开给这个区腾地方",
						zoneName, nb.MaxX-nb.MinX, nb.MaxY-nb.MinY, w, h))
				}
				if zr != baseRect {
					warns = append(warns, fmt.Sprintf(
						"分区框已为这条说明扩边:(%.0f,%.0f)..(%.0f,%.0f) → (%.0f,%.0f)..(%.0f,%.0f) —— "+
							"画布上的框还是旧的,记得重跑 `sch zone-draw --mode partition`",
						baseRect.MinX, baseRect.MinY, baseRect.MaxX, baseRect.MaxY,
						zr.MinX, zr.MinY, zr.MaxX, zr.MaxY))
				}
			} else {
				warns = append(warns, fmt.Sprintf("区 %q(解析为 %q)不在本页分区计划里,说明改为整页避让落点", zoneRef, zoneName))
			}
			solverObstacles = append(append([]layoutBBox(nil), obstacles...), otherRects...)
		} else {
			warns = append(warns, fmt.Sprintf("本页分区计划不可用(%v),区 %q 的说明改为整页避让落点", zerr, zoneRef))
		}
	}

	if !auto {
		b := noteAnchorBBox(*x, *y, w, h)
		for _, ob := range obstacles {
			if boxesGapOverlap(b, ob, 0) {
				warns = append(warns, fmt.Sprintf("说明在 (%g,%g) 压住了已有图元(重叠区 x[%.0f,%.0f] y[%.0f,%.0f]) —— 去掉 --x/--y 可让它自动避让",
					*x, *y, math.Max(b.MinX, ob.MinX), math.Min(b.MaxX, ob.MaxX),
					math.Max(b.MinY, ob.MinY), math.Min(b.MaxY, ob.MaxY)))
				break
			}
		}
		return warns, zoneMatched, nil
	}

	// 带里已经算出落点就直接用它 —— 它与 planner 重算出的框逐字段同源(同一个
	// reserveZoneNoteArea),所以 note-outside-zone 结构上不会再响。
	if bandAnchor != nil {
		*x, *y = bandAnchor[0], bandAnchor[1]
		return warns, zoneMatched, nil
	}

	nx, ny, ok := planNoteAnchor(w, h, solverObstacles, zoneRect, noteBand, *sheet, keepout)
	if !ok {
		return warns, zoneMatched, fmt.Errorf("这一页找不到能放下这条说明(%.0f×%.0f)且不压任何图元的空位 —— 缩短文字/减小 --font-size,或腾出版面后重试", w, h)
	}
	*x, *y = nx, ny
	return warns, zoneMatched, nil
}
