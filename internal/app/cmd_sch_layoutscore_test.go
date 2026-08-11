package app

// cmd_sch_layoutscore_test.go — `sch layout-score` 纯核表驱动测试。
//
// fixture 用 ceshi 板的真实几何(主会话 live 核实的坐标):U2 @(880,470) 大核心,
// R1 rot180 @(700,475),R3 @(960,460),LED1 @(1080,460),折叠 netport LED_CTRL
// @(940,440) 11×31,水平正常 netport IO0 @(850,240) 31×11。

import (
	"strings"
	"testing"
)

func lsPart(id, desig string, x, y float64, bb layoutBBox, pins ...layoutPin) layoutComp {
	return layoutComp{
		ID: id, Designator: desig, ComponentType: schLayoutPartType,
		X: x, Y: y, AnchorAvailable: true, BBox: &bb,
		Pins: pins, PinsAvailable: true, PinsProofKnown: true,
	}
}

func lsMarker(id, ctype, net string, x, y float64, bb layoutBBox) layoutComp {
	return layoutComp{
		ID: id, ComponentType: ctype, Net: net,
		X: x, Y: y, AnchorAvailable: true, BBox: &bb,
	}
}

// ceshi 核心件:U2 bbox[844.5,259.5→915.5,680.5](41 pins 的大件,取代表 pin)。
func ceshiU2() layoutComp {
	return lsPart("u2", "U2", 880, 470,
		layoutBBox{MinX: 844.5, MinY: 259.5, MaxX: 915.5, MaxY: 680.5},
		layoutPin{Number: "3", X: 845, Y: 470},
		layoutPin{Number: "25", X: 850, Y: 260},
		layoutPin{Number: "30", X: 915, Y: 470},
	)
}

// R1 rot180 @(700,475) bbox[689.5,470.5→710.5,479.5]。
func ceshiR1() layoutComp {
	return lsPart("r1", "R1", 700, 475,
		layoutBBox{MinX: 689.5, MinY: 470.5, MaxX: 710.5, MaxY: 479.5},
		layoutPin{Number: "1", X: 690, Y: 475},
		layoutPin{Number: "2", X: 710, Y: 475},
	)
}

// R3 @(960,460)。
func ceshiR3() layoutComp {
	return lsPart("r3", "R3", 960, 460,
		layoutBBox{MinX: 949.5, MinY: 455.5, MaxX: 970.5, MaxY: 464.5},
		layoutPin{Number: "1", X: 950, Y: 460},
		layoutPin{Number: "2", X: 970, Y: 460},
	)
}

// LED1 @(1080,460)(位号前缀 LED,不算 R/C/L 无源件)。
func ceshiLED1() layoutComp {
	return lsPart("led1", "LED1", 1080, 460,
		layoutBBox{MinX: 1069.5, MinY: 450.5, MaxX: 1090.5, MaxY: 469.5},
		layoutPin{Number: "1", X: 1070, Y: 460},
		layoutPin{Number: "2", X: 1090, Y: 460},
	)
}

func dimOf(t *testing.T, rep schLayoutScoreReport, id string) *schScoreDimension {
	t.Helper()
	d := rep.dimension(id)
	if d == nil {
		t.Fatalf("dimension %q missing from report", id)
	}
	return d
}

// ── 维度 1:folded-labels ───────────────────────────────────────────────────

func TestSchLayoutScoreFoldedNetportCaught(t *testing.T) {
	// live 命中:netport LED_CTRL @(940,440) bbox 11×31(竖排)。宿主 = R3:1
	// (950,460),核心 = U2(全页最大件,在 R3 左边)→ fix 方向 left。
	comps := []layoutComp{
		ceshiU2(), ceshiR3(),
		lsMarker("np-led", "netport", "LED_CTRL", 940, 440,
			layoutBBox{MinX: 934.5, MinY: 424.5, MaxX: 945.5, MaxY: 455.5}),
	}
	rep := analyzeSchLayoutScore(comps)
	d := dimOf(t, rep, schDimFolded)
	if d.Status != schDimScored || len(d.Attributions) != 1 {
		t.Fatalf("folded: want scored + 1 attribution, got status=%s attrs=%d", d.Status, len(d.Attributions))
	}
	if d.Score != 100-schScoreFoldedPenalty {
		t.Fatalf("folded score = %v, want %v", d.Score, 100-schScoreFoldedPenalty)
	}
	a := d.Attributions[0]
	if a.Target != "LED_CTRL" {
		t.Fatalf("folded target = %q, want LED_CTRL", a.Target)
	}
	for _, frag := range []string{"disconnect --pin R3:1", "--x 950 --y 460", "--kind netport", "--net LED_CTRL", "--direction left"} {
		if !strings.Contains(a.Fix, frag) {
			t.Fatalf("folded fix %q missing %q", a.Fix, frag)
		}
	}
}

func TestSchLayoutScoreHorizontalNetportNotFolded(t *testing.T) {
	// live 干净样本:netport IO0 @(850,240) bbox 31×11(水平)→ 不得误报。
	comps := []layoutComp{
		ceshiU2(),
		lsMarker("np-io0", "netport", "IO0", 850, 240,
			layoutBBox{MinX: 819.5, MinY: 234.5, MaxX: 850.5, MaxY: 245.5}),
	}
	rep := analyzeSchLayoutScore(comps)
	d := dimOf(t, rep, schDimFolded)
	if d.Score != 100 || len(d.Attributions) != 0 {
		t.Fatalf("horizontal netport falsely flagged as folded: score=%v attrs=%d", d.Score, len(d.Attributions))
	}
}

// ── 维度 2:reversed-labels ─────────────────────────────────────────────────

func TestSchLayoutScoreReversedNetport(t *testing.T) {
	// 正例(实际案例校准):R1@(700,475) 服务核心 U2@(880,470)(U2 在右)。EN
	// netport 挂在 R1:1 (690,475),anchor(660,475),bbox 体在 anchor 左侧
	// → 朝左 = 背离 U2 = 反向。
	comps := []layoutComp{
		ceshiU2(), ceshiR1(),
		lsMarker("np-en", "netport", "EN", 660, 475,
			layoutBBox{MinX: 629.5, MinY: 469.5, MaxX: 660.5, MaxY: 480.5}),
	}
	rep := analyzeSchLayoutScore(comps)
	d := dimOf(t, rep, schDimReversed)
	if len(d.Attributions) != 1 || d.Score != 100-schScoreReversedPenalty {
		t.Fatalf("reversed: want 1 hit score %v, got attrs=%d score=%v",
			100-schScoreReversedPenalty, len(d.Attributions), d.Score)
	}
	a := d.Attributions[0]
	if a.Target != "R1:1" {
		t.Fatalf("reversed target = %q, want R1:1", a.Target)
	}
	if !strings.Contains(a.Message, "U2") {
		t.Fatalf("reversed message should name the core U2: %q", a.Message)
	}
	for _, frag := range []string{"disconnect --pin R1:1", "--x 690 --y 475", "--net EN", "--direction right"} {
		if !strings.Contains(a.Fix, frag) {
			t.Fatalf("reversed fix %q missing %q", a.Fix, frag)
		}
	}
}

func TestSchLayoutScoreNetportFacingCoreClean(t *testing.T) {
	// 负例:同一 EN netport 改挂 R1:2 (710,475),anchor(740,475),bbox 体在
	// anchor 右侧 → 朝右 = 面向 U2 = 正确,不得记反向。
	comps := []layoutComp{
		ceshiU2(), ceshiR1(),
		lsMarker("np-en", "netport", "EN", 740, 475,
			layoutBBox{MinX: 739.5, MinY: 469.5, MaxX: 770.5, MaxY: 480.5}),
	}
	rep := analyzeSchLayoutScore(comps)
	d := dimOf(t, rep, schDimReversed)
	if d.Score != 100 || len(d.Attributions) != 0 {
		t.Fatalf("facing-core netport falsely flagged reversed: score=%v attrs=%d", d.Score, len(d.Attributions))
	}
}

// ── 维度 3:proximity ───────────────────────────────────────────────────────

func TestSchLayoutScoreProximity(t *testing.T) {
	// C5 距核心 U2 边距 100(≤150 满分);C9 距 600(≥500 记 0 → 归因命中)。
	comps := []layoutComp{
		ceshiU2(),
		lsPart("c5", "C5", 1026, 465,
			layoutBBox{MinX: 1015.5, MinY: 460, MaxX: 1036.5, MaxY: 470},
			layoutPin{Number: "1", X: 1016, Y: 465}),
		lsPart("c9", "C9", 1526, 465,
			layoutBBox{MinX: 1515.5, MinY: 460, MaxX: 1536.5, MaxY: 470},
			layoutPin{Number: "1", X: 1516, Y: 465}),
	}
	rep := analyzeSchLayoutScore(comps)
	d := dimOf(t, rep, schDimProximity)
	if d.Status != schDimScored {
		t.Fatalf("proximity skipped unexpectedly: %s", d.Reason)
	}
	// 两件平均:(1 + 0)/2 → 50 分。
	if d.Score != 50 {
		t.Fatalf("proximity score = %v, want 50", d.Score)
	}
	if len(d.Attributions) != 1 || d.Attributions[0].Target != "C9" {
		t.Fatalf("proximity attribution: want exactly C9, got %+v", d.Attributions)
	}
	a := d.Attributions[0]
	if a.Fix == "" || !strings.Contains(a.Fix, "sch modify --id c9") {
		t.Fatalf("proximity fix must move c9 by primitive id, got %q", a.Fix)
	}
	// 建议目标区:核心件 x 中心附近(880),y 在核心上/下方 —— 真实坐标而非占位。
	if !strings.Contains(a.Fix, `"x":880`) {
		t.Fatalf("proximity fix should target core center x≈880, got %q", a.Fix)
	}
}

// ── 维度 4:stub-tidiness ───────────────────────────────────────────────────

func TestSchLayoutScoreLongChain(t *testing.T) {
	// R5(21 长)左右各挂一个长名 netport,联合 bbox 跨度 339 > 250 → 长链。
	comps := []layoutComp{
		lsPart("r5", "R5", 311, 475,
			layoutBBox{MinX: 300.5, MinY: 470.5, MaxX: 321.5, MaxY: 479.5},
			layoutPin{Number: "1", X: 301, Y: 475},
			layoutPin{Number: "2", X: 321, Y: 475}),
		lsMarker("np-a", "netport", "USB_PULLUP_EN", 261, 475,
			layoutBBox{MinX: 141.5, MinY: 469.5, MaxX: 261.5, MaxY: 480.5}),
		lsMarker("np-b", "netport", "LED_DRIVE_CTRL", 361, 475,
			layoutBBox{MinX: 360.5, MinY: 469.5, MaxX: 480.5, MaxY: 480.5}),
	}
	rep := analyzeSchLayoutScore(comps)
	d := dimOf(t, rep, schDimTidiness)
	if len(d.Attributions) != 1 || d.Attributions[0].Target != "R5" {
		t.Fatalf("long chain: want 1 attribution on R5, got %+v", d.Attributions)
	}
	if d.Attributions[0].Fix == "" || !strings.Contains(d.Attributions[0].Fix, "R5:") {
		t.Fatalf("long-chain fix should reconnect one of R5's markers, got %q", d.Attributions[0].Fix)
	}
}

func TestSchLayoutScoreRowCrowding(t *testing.T) {
	// R7 挂水平 netport,同排 R8 净距 79 < 117 → 标签挤压风险。
	comps := []layoutComp{
		lsPart("r7", "R7", 511, 475,
			layoutBBox{MinX: 500.5, MinY: 470.5, MaxX: 521.5, MaxY: 479.5},
			layoutPin{Number: "1", X: 501, Y: 475},
			layoutPin{Number: "2", X: 521, Y: 475}),
		lsPart("r8", "R8", 611, 475,
			layoutBBox{MinX: 600.5, MinY: 470.5, MaxX: 621.5, MaxY: 479.5},
			layoutPin{Number: "1", X: 601, Y: 475},
			layoutPin{Number: "2", X: 621, Y: 475}),
		lsMarker("np-x", "netport", "SIG_A", 551, 475,
			layoutBBox{MinX: 550.5, MinY: 469.5, MaxX: 581.5, MaxY: 480.5}),
	}
	rep := analyzeSchLayoutScore(comps)
	d := dimOf(t, rep, schDimTidiness)
	found := false
	for _, a := range d.Attributions {
		if a.Target == "R7↔R8" {
			found = true
		}
	}
	if !found {
		t.Fatalf("row crowding R7↔R8 not caught: %+v", d.Attributions)
	}
}

// ── 维度 5:frame-fit ───────────────────────────────────────────────────────

func TestSchLayoutScoreFrameFitSkippedIsNotFullMarks(t *testing.T) {
	// 无 text 几何时 frame-fit 必须显式 skipped(带原因),不参与加权,也不出现
	// 在 dimensionScores —— 「没测」绝不冒充「满分」。
	rep := analyzeSchLayoutScore([]layoutComp{ceshiU2(), ceshiR3()})
	d := dimOf(t, rep, schDimFrameFit)
	if d.Status != schDimSkipped || d.Reason == "" {
		t.Fatalf("frame-fit: want skipped with reason, got status=%s reason=%q", d.Status, d.Reason)
	}
	if _, present := rep.DimensionScores[schDimFrameFit]; present {
		t.Fatalf("skipped dimension leaked into dimensionScores")
	}
	if rep.SkippedDims < 1 {
		t.Fatalf("skippedDims not counted: %d", rep.SkippedDims)
	}
}

func TestSchLayoutScoreFrameFitTextOverPart(t *testing.T) {
	// components.list 暴露 text bbox 时:说明文字压 R3 → 扣分归因。
	comps := []layoutComp{
		ceshiU2(), ceshiR3(),
		lsMarker("t1", "text", "", 960, 460,
			layoutBBox{MinX: 950, MinY: 456, MaxX: 1010, MaxY: 466}),
	}
	// text 不是 marker,lsMarker 只是借壳造图元;componentType 才是判据。
	rep := analyzeSchLayoutScore(comps)
	d := dimOf(t, rep, schDimFrameFit)
	if d.Status != schDimScored || len(d.Attributions) != 1 || d.Attributions[0].Target != "R3" {
		t.Fatalf("text-over-part: want scored + 1 attribution on R3, got status=%s attrs=%+v", d.Status, d.Attributions)
	}
	if d.Score != 100-schScoreTextOverPenalty {
		t.Fatalf("frame-fit score = %v, want %v", d.Score, 100-schScoreTextOverPenalty)
	}
}

// ── 综合:加权 / verdict 一致性 / 空页 ──────────────────────────────────────

func TestSchLayoutScoreOverallWeightingAndVerdict(t *testing.T) {
	// 折叠 1 条,其余干净,frame-fit skipped:
	// overall = (85·1.2 + 100·1.2 + 100·1.0 + 100·0.8) / 4.2 = 95.71 → 95.7。
	comps := []layoutComp{
		ceshiU2(), ceshiR3(), ceshiLED1(),
		lsMarker("np-led", "netport", "LED_CTRL", 940, 440,
			layoutBBox{MinX: 934.5, MinY: 424.5, MaxX: 945.5, MaxY: 455.5}),
	}
	rep := analyzeSchLayoutScore(comps)
	if rep.Overall != 95.7 {
		t.Fatalf("overall = %v, want 95.7", rep.Overall)
	}
	if rep.Verdict != schScoreVerdict(&rep) {
		t.Fatalf("verdict %q not derived from schScoreVerdict (single source)", rep.Verdict)
	}
	if rep.Verdict != "excellent" {
		t.Fatalf("verdict = %q, want excellent for %v", rep.Verdict, rep.Overall)
	}
	if rep.ScoredDims != 4 || rep.SkippedDims != 1 {
		t.Fatalf("scored/skipped = %d/%d, want 4/1", rep.ScoredDims, rep.SkippedDims)
	}
}

func TestSchLayoutScoreEmptyPageUnscored(t *testing.T) {
	rep := analyzeSchLayoutScore(nil)
	if rep.Verdict != "unscored" || rep.ScoredDims != 0 {
		t.Fatalf("empty page: want unscored/0, got %q/%d", rep.Verdict, rep.ScoredDims)
	}
	for _, d := range rep.Dimensions {
		if d.Status != schDimSkipped || d.Reason == "" {
			t.Fatalf("empty page: dim %s must be skipped with reason", d.ID)
		}
	}
}

func TestSchScoreVerdictBands(t *testing.T) {
	for _, tc := range []struct {
		overall float64
		want    string
	}{{95, "excellent"}, {90, "excellent"}, {80, "good"}, {75, "good"}, {60, "fair"}, {55, "fair"}, {40, "poor"}} {
		rep := schLayoutScoreReport{Overall: tc.overall, ScoredDims: 5}
		if got := schScoreVerdict(&rep); got != tc.want {
			t.Fatalf("verdict(%v) = %q, want %q", tc.overall, got, tc.want)
		}
	}
}

func TestIsPowerNetName(t *testing.T) {
	for _, n := range []string{"GND", "AGND", "VCC", "VBUS", "3V3", "+5V", "3.3V", "12V0", "VDD_3V3"} {
		if !isPowerNetName(n) {
			t.Fatalf("%q should be a power net", n)
		}
	}
	for _, n := range []string{"EN", "IO0", "LED_CTRL", "USB_DP", "V_SENSE_A"} {
		if isPowerNetName(n) {
			t.Fatalf("%q should NOT be a power net", n)
		}
	}
}
