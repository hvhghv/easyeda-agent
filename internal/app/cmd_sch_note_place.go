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

// wrapNoteContent 把一段可能含 \n 的说明按 maxWidth 折行。**必须先按 \n 拆行再
// 逐行 wrap**:此前把整段当一行传给 wrapNoteLines,宽度累计跨过换行符继续加,
// 于是「首行完整、第二行开头 3~4 字就被折断」(2026-08-18 P2 LED 说明真机定案:
// "丝印标正负极性" 折成 "丝印标正/负极性",与宽度无关、纯粹是账没清零)。
func wrapNoteContent(content string, maxWidth float64) string {
	return strings.Join(wrapNoteLines(strings.Split(content, "\n"), maxWidth), "\n")
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
		b := noteAnchorBBox(bx, by, w, h)
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
		if sx, sy, hit := try(noteBand.MinX+noteGap, noteBand.MinY+h+noteGap); hit {
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
	// **文字比框宽时先折行** —— 否则说明带塞不下,落点会一路退到整页扫描、跑到
	// 框外面去(实测 D_ESD 框宽 96,一行说明 200,落到了 x=50)。折行口径与区框里
	// 的电路说明一致(wrapNoteLines),不新造一套。
	//
	// solverObstacles = 页面图元障碍 + 分区框障碍(根因 B)。分区框只喂给自动
	// 求解器 —— 显式 --x/--y 落在自己区框内是完全合法的,不该被框障碍误警。
	var zoneRect, noteBand *layoutBBox
	solverObstacles := obstacles
	if zoneName != "" {
		if plan, _, zerr := computePartitionPlan(cfg, window, docUUID, defaultPartitionOpts()); zerr == nil {
			var otherRects []layoutBBox
			zoneRect, noteBand, otherRects, zoneMatched = matchNotePartition(plan.Partitions, zoneName)
			if zoneMatched {
				if wrapped := wrapNoteContent(*content, zoneRect.MaxX-zoneRect.MinX-2*noteGap); wrapped != *content {
					*content = wrapped
					w, h = noteSizeOf(*content, fontSize)
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

	nx, ny, ok := planNoteAnchor(w, h, solverObstacles, zoneRect, noteBand, *sheet, keepout)
	if !ok {
		return warns, zoneMatched, fmt.Errorf("这一页找不到能放下这条说明(%.0f×%.0f)且不压任何图元的空位 —— 缩短文字/减小 --font-size,或腾出版面后重试", w, h)
	}
	*x, *y = nx, ny
	return warns, zoneMatched, nil
}
