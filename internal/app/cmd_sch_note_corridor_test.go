package app

import (
	"math"
	"testing"
)

// ── sch note --zone 的区外走廊 + 整页扫描按距排序 ───────────────────────────
//
// 真机症状:框内无空位时,「区正下方」只有单个候选点,一撞就跌进整页扫描;而
// 整页扫描从图纸左下角起扫、不按离 zone 的距离排序 —— 说明落到页角,和它描述
// 的电路隔了半张图纸。

// 框内全满(连四周单点候选也全被挡):落点必须出现在框正下方走廊,而不是页角。
func TestPlanNoteAnchor_ZoneFullFallsToCorridorBelow(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	zone := layoutBBox{MinX: 300, MinY: 300, MaxX: 900, MaxY: 700}
	obstacles := []layoutBBox{
		// 盖满区框并向四周多探 40:框内候选、左右侧/正下方的单点候选全部被挡。
		{MinX: 260, MinY: 260, MaxX: 940, MaxY: 740},
		// 再挡掉正下方走廊靠左的一段,逼扫描沿走廊横向步进。
		{MinX: 200, MinY: 150, MaxX: 500, MaxY: 260},
	}
	w, h := noteSizeOf("走廊测试", 10)
	x, y, ok := planNoteAnchor(w, h, obstacles, &zone, nil, sheet, nil)
	if !ok {
		t.Fatal("正下方走廊有空位,不该求解失败")
	}
	box := noteAnchorBBox(x, y, w, h)
	// 核心断言:落在框正下方的走廊里,不是页角。
	if box.MaxY > zone.MinY {
		t.Errorf("落点应在区下沿之下: %+v vs zone %+v", box, zone)
	}
	corridorFloor := zone.MinY - noteGap - float64(noteCorridorTiers)*noteAnchorStep - h
	if box.MinY < corridorFloor {
		t.Errorf("落点掉出走廊档位(疑似跌回页角): box=%+v floor=%v", box, corridorFloor)
	}
	if box.MinX < zone.MinX {
		t.Errorf("落点应横向贴着区框(x ≥ zone.MinX): %+v", box)
	}
	// 旧行为的落点是页角 (≈16, ≈30) —— 明确排除。
	if box.MinX < 100 && box.MinY < 100 {
		t.Errorf("说明被甩到页角(旧 bug 复现): %+v", box)
	}
	for i, ob := range obstacles {
		if boxesGapOverlap(box, ob, noteGap) {
			t.Errorf("落点与障碍 %d 间隙不足: %+v vs %+v", i, box, ob)
		}
	}
}

// 走廊也全被挡、必须整页扫描时:候选按离 zone 中心的距离升序试,落点应贴着
// 障碍边缘靠近 zone,而不是按行扫顺序落在图纸左下角。
func TestPlanNoteAnchor_PageScanOrderedByZoneDistance(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	zone := layoutBBox{MinX: 800, MinY: 600, MaxX: 1100, MaxY: 780}
	// 一整块障碍盖住 zone + 四周走廊(右/上出界,下/左被它挡死)。
	obstacles := []layoutBBox{{MinX: 560, MinY: 380, MaxX: 1170, MaxY: 825}}
	w, h := noteSizeOf("页角测试", 10)
	x, y, ok := planNoteAnchor(w, h, obstacles, &zone, nil, sheet, nil)
	if !ok {
		t.Fatal("页面左半和下方大片空白,不该求解失败")
	}
	box := noteAnchorBBox(x, y, w, h)
	zcx, zcy := (zone.MinX+zone.MaxX)/2, (zone.MinY+zone.MaxY)/2
	bcx, bcy := (box.MinX+box.MaxX)/2, (box.MinY+box.MaxY)/2
	got := math.Hypot(bcx-zcx, bcy-zcy)
	// 旧行为落在左下页角 (≈16,≈30),离区中心 ≈1140;按距排序后必然近得多。
	cornerX, cornerY := sheet.MinX+noteGap+w/2, sheet.MinY+noteGap+h/2
	corner := math.Hypot(cornerX-zcx, cornerY-zcy)
	if got >= corner {
		t.Errorf("整页扫描没有按离区距离排序: 落点 %+v 距区中心 %.0f, 页角距离 %.0f", box, got, corner)
	}
	if got > 700 {
		t.Errorf("落点离区太远(%.0f), 疑似仍按扫描序取第一个空位: %+v", got, box)
	}
	if boxesGapOverlap(box, obstacles[0], noteGap) {
		t.Errorf("落点与障碍间隙不足: %+v", box)
	}
}

// 无 zone 时整页扫描保持传统顺序(左下角优先)—— 不回归既有行为。
func TestPlanNoteAnchor_NoZoneKeepsBottomLeftTradition(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	obstacles := []layoutBBox{{MinX: 300, MinY: 300, MaxX: 900, MaxY: 600}}
	w, h := noteSizeOf("传统位置", 10)
	x, y, ok := planNoteAnchor(w, h, obstacles, nil, nil, sheet, nil)
	if !ok {
		t.Fatal("不该失败")
	}
	box := noteAnchorBBox(x, y, w, h)
	if box.MinX > 100 || box.MinY > 100 {
		t.Errorf("无 zone 时应保持左下角传统落点: %+v", box)
	}
}

// 走廊候选的生成顺序:正下方最先(读图习惯),同方向逐档远离,先近档后远档。
func TestNoteCorridorCandidates_OrderAndTiers(t *testing.T) {
	z := layoutBBox{MinX: 300, MinY: 300, MaxX: 900, MaxY: 700}
	w, h := 40.0, 13.0
	cands := noteCorridorCandidates(z, w, h)
	if len(cands) == 0 {
		t.Fatal("走廊候选不能为空")
	}
	first := cands[0]
	// 锚点 = 左下角:正下方走廊要让 bbox 顶(y+h)不超过 z.MinY-noteGap。
	if first[0] != z.MinX+noteGap || first[1] != z.MinY-noteGap-h {
		t.Errorf("第一个候选应是正下方第一档左端: %+v", first)
	}
	// 正下方走廊必须是多档(不再是单点):同一 x 至少出现 noteCorridorTiers 个不同 y。
	ys := map[float64]bool{}
	for _, c := range cands {
		if c[0] == z.MinX+noteGap && c[1] <= z.MinY-noteGap {
			ys[c[1]] = true
		}
	}
	if len(ys) < noteCorridorTiers {
		t.Errorf("正下方走廊应有 %d 档, got %d", noteCorridorTiers, len(ys))
	}
}
