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
// 估算 schNoteBBoxEstimate 也早就存在,还会被 foldZoneNotesIntoModules 折进
// 画框口径),而是因为**文字只被动地"被框住",从没主动参与避让求解**。
//
// 本文件补上那一步:note 和器件、marker、已有文字、标题栏 keep-out 一起进同一
// 张碰撞表,自动求一个不压任何东西的锚点。尺寸估算复用 schNoteBBoxEstimate ——
// 判据与生成必须同源,两套估算一旦漂移就会"算的时候不撞、画出来撞"。

// noteGap 是说明与任何已有图元之间的最小视觉间隙(原理图单位)。比 marker 的
// 重叠阈值大得多:贴着边不算重叠,但读起来仍然是"糊在一起"。
const noteGap = 16.0

// noteAnchorStep 是候选锚点的扫描步长(落在 5 格连接网格上)。
const noteAnchorStep = 20.0

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
func planNoteAnchor(w, h float64, obstacles []layoutBBox, zoneRect *layoutBBox, sheet layoutBBox, keepout *layoutBBox) (x, y float64, ok bool) {
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

	var cands [][2]float64
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
		for _, c := range cands {
			if free(c[0], c[1]) {
				return snapNote(c[0]), snapNote(c[1]), true
			}
		}
	}
	// 整页扫描:从图纸下方往上、从左往右 —— 左下角通常是图签之外最大的连续空白,
	// 也是工程图放总说明的传统位置。
	for by := sheet.MinY + h + noteGap; by <= sheet.MaxY-noteGap; by += noteAnchorStep {
		for bx := sheet.MinX + noteGap; bx <= sheet.MaxX-w-noteGap; bx += noteAnchorStep {
			if free(bx, by) {
				return snapNote(bx), snapNote(by), true
			}
		}
	}
	return 0, 0, false
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

// placeSchNote 是自动落点的 I/O 外壳:拉一次页面几何(图元 + 已有文字 + 图纸 +
// 图签 keep-out + 该区的分区矩形),求锚点写回 *x/*y。
//
//   - auto=true(调用方没给 --x/--y):求解失败 = 硬错误。宁可不画,也不把说明
//     糊在电路上——那正是这次要根治的症状。
//   - auto=false(调用方显式给了坐标):坐标一字不改,但仍做一次碰撞回读,压到
//     东西就返回一句警告(第二个返回值),让人知道自己压了什么。
//
// 几何读取失败一律降级为「照给定坐标画」并给出提示:说明是注释,不该因为读不到
// 布局就阻断。
func placeSchNote(cfg *appConfig, window, docUUID, zoneRef, content string, fontSize float64, auto bool, x, y *float64) (warn string, err error) {
	w, h := noteSizeOf(content, fontSize)

	res, rerr := requestAutolayoutAction(cfg, "schematic.components.list", window,
		map[string]any{"includeBBox": true}, docUUID, "read layout for note placement")
	if rerr != nil {
		if auto {
			return "", fmt.Errorf("自动落点需要页面几何,但 components.list 失败:%w(可显式给 --x/--y 绕过)", rerr)
		}
		return "note 落点未做碰撞校验(读取页面几何失败)", nil
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		if auto {
			return "", fmt.Errorf("自动落点需要页面几何,但解析失败:%w(可显式给 --x/--y 绕过)", perr)
		}
		return "note 落点未做碰撞校验(页面几何解析失败)", nil
	}
	sheet := sheetBBoxOf(comps)
	if sheet == nil {
		if auto {
			return "", fmt.Errorf("自动落点需要图纸边框(sheet)bbox,本页读不到——请显式给 --x/--y")
		}
		return "note 落点未做碰撞校验(读不到图纸 bbox)", nil
	}
	keepout, _ := titleBlockKeepout(sheet)

	var texts []zoneMoveText
	if tres, terr := requestAutolayoutAction(cfg, "schematic.text.list", window,
		map[string]any{}, docUUID, "read existing notes"); terr == nil {
		texts = parseZoneMoveTexts(tres.Result)
	}
	obstacles := collectNoteObstacles(comps, texts)

	// 目标区的矩形:优先用 zone-plan 给该区算出的分区框(说明就该待在自己区里),
	// 拿不到就退化成整页扫描。
	var zoneRect *layoutBBox
	if zoneRef != "" {
		if plan, _, zerr := computePartitionPlan(cfg, window, docUUID, defaultPartitionOpts()); zerr == nil {
			for _, p := range plan.Partitions {
				if strInSlice(p.Modules, zoneRef) {
					r := p.BBox
					zoneRect = &r
					break
				}
			}
		}
	}

	if !auto {
		b := noteAnchorBBox(*x, *y, w, h)
		for _, ob := range obstacles {
			if boxesGapOverlap(b, ob, 0) {
				return fmt.Sprintf("说明在 (%g,%g) 压住了已有图元(重叠区 x[%.0f,%.0f] y[%.0f,%.0f]) —— 去掉 --x/--y 可让它自动避让",
					*x, *y, math.Max(b.MinX, ob.MinX), math.Min(b.MaxX, ob.MaxX),
					math.Max(b.MinY, ob.MinY), math.Min(b.MaxY, ob.MaxY)), nil
			}
		}
		return "", nil
	}

	nx, ny, ok := planNoteAnchor(w, h, obstacles, zoneRect, *sheet, keepout)
	if !ok {
		return "", fmt.Errorf("这一页找不到能放下这条说明(%.0f×%.0f)且不压任何图元的空位 —— 缩短文字/减小 --font-size,或腾出版面后重试", w, h)
	}
	*x, *y = nx, ny
	return "", nil
}
