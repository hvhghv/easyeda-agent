package app

// pcb_score_geom_test.go — 可布性维 / 装配间距维的离线单测。
// 全部是纯结构体字面量喂纯函数，不连 daemon、不读磁盘（照 pcb_check_dfm2_test.go 的范式）。

import (
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 造板助手
// ---------------------------------------------------------------------------

// sgTestComp 造一个方形封装（中心 x,y，边长 size），可带若干网名的焊盘。
func sgTestComp(des string, x, y, size float64, nets ...string) boardComp {
	h := size / 2
	c := boardComp{
		Designator: des, ID: "p_" + des, Layer: pcbSideTop,
		X: x, Y: y,
		BBox: &layoutBBox{MinX: x - h, MinY: y - h, MaxX: x + h, MaxY: y + h},
	}
	for i, n := range nets {
		c.Pads = append(c.Pads, boardPad{
			Number: string(rune('1' + i)), Net: n, Layer: pcbSideTop,
			X: x, Y: y, W: 20, H: 20,
		})
	}
	return c
}

// sgTestCtx 组装打分上下文。outline 传 nil 表示板框读不到。
func sgTestCtx(snapComps []boardComp, outline *boardOutline, layout *pcbLayoutReport) *scoreCtx {
	snap := &boardSnapshot{Components: snapComps, Outline: outline}
	return &scoreCtx{snap: snap, layout: layout, rules: defaultPcbRules()}
}

// sgTestOutline 造一块矩形板（对角线 = hypot(w,h)）。
func sgTestOutline(w, h float64) *boardOutline {
	return &boardOutline{BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: w, MaxY: h}, Source: "bbox"}
}

func sgScoreRoutable(ctx *scoreCtx) scoreDimension  { return sgRoutableScorer{}.score(ctx) }
func sgScoreClearance(ctx *scoreCtx) scoreDimension { return sgClearanceScorer{}.score(ctx) }

// ---------------------------------------------------------------------------
// 注册
// ---------------------------------------------------------------------------

func TestScoreGeom_ScorersRegistered(t *testing.T) {
	for _, id := range []string{dimRoutable, dimClearance} {
		if scorerFor(id) == nil {
			t.Fatalf("dimension %q has no registered scorer", id)
		}
	}
}

// ---------------------------------------------------------------------------
// 可布性维
// ---------------------------------------------------------------------------

// 没有信号网 = 没得测。**绝不能返回 100** —— 一块什么都不用布的板拿"可布性满分"
// 会把综合分抬高，让 #167 第五层的校准判据失效。
func TestScoreGeom_RoutableSkipsWithoutSignalNets(t *testing.T) {
	ctx := sgTestCtx(
		[]boardComp{sgTestComp("H1", 100, 100, 50), sgTestComp("H2", 400, 100, 50)},
		sgTestOutline(600, 800),
		&pcbLayoutReport{SignalNets: 0},
	)
	d := sgScoreRoutable(ctx)
	if d.Status != dimSkipped {
		t.Fatalf("status = %q, want %q", d.Status, dimSkipped)
	}
	if d.Score != 0 {
		t.Errorf("skipped dimension must not carry a passing score, got %.1f", d.Score)
	}
	if d.Reason == "" {
		t.Error("skipped dimension must state why")
	}
}

// 核心设计判据：交叉扣分看**密度**不看绝对数。一块 10 网 5 交叉的小板和一块
// 100 网 50 交叉的大板布局水准相同，必须得同一个分 —— 旧 layout-lint 的
// `-4×crossings` 会把它们判成 80 分 vs 0 分。
func TestScoreGeom_RoutableUsesDensityNotAbsoluteCount(t *testing.T) {
	outline := sgTestOutline(600, 800) // 对角线 1000mil
	// 飞线长度都压在 clean 线以下（0.1 < 0.15），把飞线子项的影响清零，只比交叉。
	small := sgTestCtx(nil, outline, &pcbLayoutReport{
		SignalNets: 10, CrossingCount: 5, RatsnestLenMil: 10 * 1000 * 0.1,
	})
	big := sgTestCtx(nil, outline, &pcbLayoutReport{
		SignalNets: 100, CrossingCount: 50, RatsnestLenMil: 100 * 1000 * 0.1,
	})
	ds, db := sgScoreRoutable(small), sgScoreRoutable(big)
	if ds.Score != db.Score {
		t.Fatalf("same crossing density scored differently: small=%.1f big=%.1f", ds.Score, db.Score)
	}
	if ds.Metrics["crossPerNet"] != 0.5 || db.Metrics["crossPerNet"] != 0.5 {
		t.Errorf("crossPerNet = %.2f / %.2f, want 0.5 both", ds.Metrics["crossPerNet"], db.Metrics["crossPerNet"])
	}
	if ds.Score >= 100 {
		t.Errorf("density 0.5 is above the clean threshold %.2f — expected a penalty, got %.1f",
			sgCrossPerNetClean, ds.Score)
	}
	if ds.Metrics["ratsnestPenalty"] != 0 {
		t.Errorf("ratsnest below the clean threshold must cost nothing, got %.1f", ds.Metrics["ratsnestPenalty"])
	}
}

// 干净的板拿满分（密度低于 clean 线 + 飞线短），且状态是 scored 不是 degraded。
func TestScoreGeom_RoutableCleanBoardScoresFull(t *testing.T) {
	ctx := sgTestCtx(nil, sgTestOutline(600, 800), &pcbLayoutReport{
		SignalNets: 20, CrossingCount: 2, RatsnestLenMil: 20 * 1000 * 0.1,
	})
	d := sgScoreRoutable(ctx)
	if d.Status != dimScored {
		t.Fatalf("status = %q (%s), want scored", d.Status, d.Reason)
	}
	if d.Score != 100 {
		t.Errorf("clean board scored %.1f, want 100", d.Score)
	}
	if len(d.Findings) != 0 {
		t.Errorf("clean board must not emit findings: %+v", d.Findings)
	}
}

// #167 侦查点名的缺口：crossFinding 只有网名 + 交点，没有位号。这一维必须用
// netMembers() 把网名反查回器件，并按「离交点多近」挑嫌疑人 —— 远处的同网器件
// 不该被冤枉。
func TestScoreGeom_RoutableAttributesCrossingToNearbyMembers(t *testing.T) {
	comps := []boardComp{
		sgTestComp("R1", 1000, 1000, 60, "NET_A"), // 紧挨交点
		sgTestComp("R2", 1100, 1000, 60, "NET_A"), // 稍远
		sgTestComp("R5", 5000, 5000, 60, "NET_A"), // 板子另一头 —— 不该被归因
		sgTestComp("R3", 1000, 1050, 60, "NET_B"),
		sgTestComp("R4", 1050, 1100, 60, "NET_B"),
	}
	layout := &pcbLayoutReport{
		SignalNets: 2, CrossingCount: 6, RatsnestLenMil: 1, // 飞线极短 → 只剩交叉扣分
		Crossings: []crossFinding{{NetA: "NET_A", NetB: "NET_B", X: 1010, Y: 1010}},
	}
	// CrossingCount 与 Crossings 长度故意不同：密度按计数算，归因按明细摊。
	d := sgScoreRoutable(sgTestCtx(comps, sgTestOutline(6000, 6000), layout))

	got := map[string]float64{}
	for _, c := range d.Contributors {
		got[c.Designator] = c.Penalty
	}
	if len(d.Contributors) == 0 {
		t.Fatal("crossings cost points but nobody was blamed — the refinement loop has no gradient to follow")
	}
	if got["R1"] == 0 || got["R3"] == 0 {
		t.Errorf("nearest members of both nets must be blamed, got %+v", got)
	}
	if _, blamed := got["R5"]; blamed {
		t.Errorf("a member 5000mil away from the crossing must not be blamed: %+v", got)
	}
	// 排序契约：Penalty 降序。
	for i := 1; i < len(d.Contributors); i++ {
		if d.Contributors[i-1].Penalty < d.Contributors[i].Penalty {
			t.Fatalf("contributors not sorted by penalty desc: %+v", d.Contributors)
		}
	}
	if d.Contributors[0].Detail == "" {
		t.Error("contributor must explain itself (which crossing, where)")
	}
}

// 交叉为零但飞线拉得很长时也要扣分，而且归因得落到"离自己网重心最远"的那个件上
// —— 否则这一半扣分是无主的，精修环不知道动谁。
func TestScoreGeom_RoutableRatsnestSprawlIsAttributed(t *testing.T) {
	comps := []boardComp{
		sgTestComp("U1", 100, 100, 100, "NET_A"),
		sgTestComp("C1", 150, 120, 40, "NET_A"),
		sgTestComp("C9", 5000, 4000, 40, "NET_A"), // 被扔到板子另一头
	}
	outline := sgTestOutline(6000, 6000) // 对角线 ≈ 8485
	layout := &pcbLayoutReport{
		SignalNets: 1, CrossingCount: 0,
		RatsnestLenMil: 8485 * 0.5, // 每网占板跨距 50% → 明显超过 clean 线
	}
	d := sgScoreRoutable(sgTestCtx(comps, outline, layout))
	if d.Metrics["ratsnestPenalty"] <= 0 {
		t.Fatalf("sprawling ratsnest must cost points, metrics=%+v", d.Metrics)
	}
	if len(d.Contributors) == 0 || d.Contributors[0].Designator != "C9" {
		t.Fatalf("the part farthest from its net centroid must top the blame list, got %+v", d.Contributors)
	}
	var sprawl bool
	for _, f := range d.Findings {
		if f.Type == "ratsnest-sprawl" {
			sprawl = true
		}
	}
	if !sprawl {
		t.Error("expected a ratsnest-sprawl finding")
	}
}

// 板框读不到时（PCB 不在前台，平台返 null）用摆放范围当分母 —— 结果只是相对参考，
// 必须标 degraded 并说明，不能假装算准了。
func TestScoreGeom_RoutableDegradesWithoutOutline(t *testing.T) {
	comps := []boardComp{
		sgTestComp("U1", 100, 100, 100, "NET_A"),
		sgTestComp("C1", 2000, 2000, 40, "NET_A"),
	}
	d := sgScoreRoutable(sgTestCtx(comps, nil, &pcbLayoutReport{
		SignalNets: 4, CrossingCount: 1, RatsnestLenMil: 500,
	}))
	if d.Status != dimDegraded {
		t.Fatalf("status = %q, want degraded when the outline is missing", d.Status)
	}
	if !strings.Contains(d.Reason, "placement extent") {
		t.Errorf("reason must name the substitute denominator, got %q", d.Reason)
	}
	if _, ok := d.Metrics["ratsnestPerNetSpan"]; !ok {
		t.Error("placement-extent fallback still yields a ratsnest ratio — expected the metric")
	}
}

// 连摆放范围都算不出来时，飞线子项**退出计分**（既不算满分也不算 0），扣分预算
// 拉伸回 100 由交叉独扛，并说明少测了什么。
func TestScoreGeom_RoutableDropsRatsnestSubMetricWhenUnmeasurable(t *testing.T) {
	layout := &pcbLayoutReport{SignalNets: 10, CrossingCount: 10, RatsnestLenMil: 999999}
	d := sgScoreRoutable(sgTestCtx(nil, nil, layout))
	if d.Status != dimDegraded {
		t.Fatalf("status = %q, want degraded", d.Status)
	}
	if _, ok := d.Metrics["ratsnestPerNetSpan"]; ok {
		t.Error("an unmeasurable sub-metric must not be reported as if it were measured")
	}
	if d.Metrics["ratsnestPenalty"] != 0 {
		t.Errorf("unmeasurable sub-metric must not contribute a penalty, got %.1f", d.Metrics["ratsnestPenalty"])
	}
	// crossPerNet = 1.0 → ramp (1.0-0.25)/(2.0-0.25) ≈ 0.4286，拉伸后 ×100 ≈ 42.9。
	want := clampScore(100 - sgRamp(1.0, sgCrossPerNetClean, sgCrossPerNetBad)*100)
	if d.Score != want {
		t.Errorf("score = %.1f, want %.1f (crossing budget stretched to the full 100)", d.Score, want)
	}
}

// ---------------------------------------------------------------------------
// 装配间距维
// ---------------------------------------------------------------------------

// 组一份带 tight pair 的布局结果。gap 全为 0（贴住）= 最严重。
func sgTightLayout(minGap float64, pairs int) *pcbLayoutReport {
	rep := &pcbLayoutReport{MinGapMil: minGap, AccessMil: 40}
	names := []string{"R1", "R2", "R3", "R4", "R5", "R6", "R7", "R8", "R9", "R10"}
	for i := 0; i < pairs; i++ {
		rep.TightPairs = append(rep.TightPairs, pcbLFinding{
			Type: "spacing", A: names[(2*i)%len(names)], B: names[(2*i+1)%len(names)],
			Side: "top", Gap: 0,
		})
	}
	return rep
}

func sgTwoComps() []boardComp {
	return []boardComp{sgTestComp("R1", 100, 100, 40), sgTestComp("R2", 300, 100, 40)}
}

// 修掉侦查发现的老矛盾：evalLayoutGate 里「有 tight pair 就 FAIL」，旧 score 里
// 一对只扣 1 分 → 出现「score 95 分照样 FAIL」。这一维必须让分数和门同号：
// **任何**一处越线都要把分压到 good 档（75）以下。
func TestScoreGeom_ClearanceTightPairDropsBelowGoodBand(t *testing.T) {
	d := sgScoreClearance(sgTestCtx(sgTwoComps(), sgTestOutline(600, 800), sgTightLayout(8, 1)))
	if d.Score > 75 {
		t.Fatalf("one tight pair scored %.1f — the layout-lint gate would FAIL this board while the score still reads 'good'", d.Score)
	}
	if d.Metrics["tightPairs"] != 1 {
		t.Errorf("tightPairs metric = %.0f, want 1", d.Metrics["tightPairs"])
	}
	// 两端器件同等嫌疑（挪走任何一个都能解决），扣分对半分。
	if len(d.Contributors) != 2 {
		t.Fatalf("both ends of the pair must be blamed, got %+v", d.Contributors)
	}
	if math.Abs(d.Contributors[0].Penalty-d.Contributors[1].Penalty) > 0.01 {
		t.Errorf("a pair's blame must split evenly: %+v", d.Contributors)
	}
	if len(d.Findings) != 1 || !strings.Contains(d.Findings[0].Message, "规范 §3.4") {
		t.Errorf("assembly-gap finding must cite the design-rules section: %+v", d.Findings)
	}
}

// 越多越低，且多到一定程度必须掉到任何合理门限之下。
func TestScoreGeom_ClearanceScoreFallsWithPairCount(t *testing.T) {
	comps := sgTwoComps()
	outline := sgTestOutline(600, 800)
	one := sgScoreClearance(sgTestCtx(comps, outline, sgTightLayout(8, 1))).Score
	three := sgScoreClearance(sgTestCtx(comps, outline, sgTightLayout(8, 3))).Score
	five := sgScoreClearance(sgTestCtx(comps, outline, sgTightLayout(8, 5))).Score
	if !(one > three && three > five) {
		t.Fatalf("score must fall monotonically with tight-pair count: 1=%.1f 3=%.1f 5=%.1f", one, three, five)
	}
	if five > 40 {
		t.Errorf("5 touching pairs still scored %.1f — that is above plausible gates", five)
	}
}

// 缺口越深扣得越多（贴住 vs 差一点点到线）。
func TestScoreGeom_ClearanceSeverityScales(t *testing.T) {
	comps, outline := sgTwoComps(), sgTestOutline(600, 800)
	touching := sgTightLayout(8, 1) // gap 0
	marginal := sgTightLayout(8, 1)
	marginal.TightPairs[0].Gap = 7.5 // 只差 0.5mil 到门限
	hard := sgScoreClearance(sgTestCtx(comps, outline, touching)).Score
	soft := sgScoreClearance(sgTestCtx(comps, outline, marginal)).Score
	if hard >= soft {
		t.Fatalf("a touching pair must cost more than a marginal one: touching=%.1f marginal=%.1f", hard, soft)
	}
	if soft > 75 {
		t.Errorf("even a marginal violation must stay out of the good band, got %.1f", soft)
	}
}

// AccessBlocked 只在 hand-solder profile 下才有数据，reflow 板恒空。空数组
// **不等于**通过 —— 报告必须写明这一段压根没检查。
func TestScoreGeom_ClearanceFlagsUncheckedSolderAccess(t *testing.T) {
	d := sgScoreClearance(sgTestCtx(sgTwoComps(), sgTestOutline(600, 800),
		&pcbLayoutReport{MinGapMil: 8, AccessMil: 0}))
	if d.Status != dimDegraded {
		t.Fatalf("status = %q, want degraded when the iron-access corridor was never checked", d.Status)
	}
	if !strings.Contains(d.Reason, "hand-solder") {
		t.Errorf("reason must say the hand-solder profile is missing, got %q", d.Reason)
	}
}

// 四面被围死的器件按个扣分并被点名。
func TestScoreGeom_ClearanceBlamesBoxedInParts(t *testing.T) {
	layout := &pcbLayoutReport{MinGapMil: 8, AccessMil: 40, AccessBlocked: []pcbLAccessFinding{
		{Designator: "U3", BestGap: 12, Sides: map[string]float64{"left": 12, "right": 5, "top": 8, "bottom": 6}},
	}}
	d := sgScoreClearance(sgTestCtx(sgTwoComps(), sgTestOutline(600, 800), layout))
	if d.Status != dimScored {
		t.Fatalf("status = %q (%s), want scored — every sub-check ran", d.Status, d.Reason)
	}
	if d.Score != clampScore(100-sgAccessPenalty) {
		t.Errorf("score = %.1f, want %.1f", d.Score, clampScore(100-sgAccessPenalty))
	}
	if len(d.Contributors) != 1 || d.Contributors[0].Designator != "U3" {
		t.Fatalf("the boxed-in part must be named: %+v", d.Contributors)
	}
	if len(d.Findings) != 1 || d.Findings[0].Type != "solder-access-blocked" {
		t.Errorf("expected a solder-access-blocked finding, got %+v", d.Findings)
	}
}

// 间距是成对的量：不足两个能量出轮廓的器件时没得测，必须 skipped 而不是 100。
func TestScoreGeom_ClearanceSkipsWithoutBBoxes(t *testing.T) {
	comps := []boardComp{{Designator: "U1", Layer: pcbSideTop, X: 10, Y: 10}} // 无 bbox
	d := sgScoreClearance(sgTestCtx(comps, sgTestOutline(600, 800),
		&pcbLayoutReport{MinGapMil: 8, AccessMil: 40}))
	if d.Status != dimSkipped {
		t.Fatalf("status = %q, want skipped", d.Status)
	}
	if d.Score != 0 || d.Reason == "" {
		t.Errorf("skipped dimension must score 0 with a reason, got %.1f / %q", d.Score, d.Reason)
	}
}

// 门限本身太松（默认吃的是 6mil 电气 clearance，不是 §3.4 的 0.2mm 装配下限）时，
// "零 tight pair" 只证明没有几乎贴住的对，必须降级说明。
func TestScoreGeom_ClearanceDegradesOnElectricalClearanceThreshold(t *testing.T) {
	d := sgScoreClearance(sgTestCtx(sgTwoComps(), sgTestOutline(600, 800),
		&pcbLayoutReport{MinGapMil: defaultPcbRules().clearanceMil, AccessMil: 40}))
	if d.Status != dimDegraded {
		t.Fatalf("status = %q, want degraded on a %.1fmil threshold", d.Status, defaultPcbRules().clearanceMil)
	}
	if !strings.Contains(d.Reason, "规范 §3.4") {
		t.Errorf("reason must cite the assembly-spacing rule, got %q", d.Reason)
	}
}

// 不重复扣分：重叠 / 短路 / 出板框已经进了报告的 Blocking，这一维碰它们就等于
// 同一个缺陷被罚三遍，综合分失真。
func TestScoreGeom_ClearanceDoesNotDoubleCountBlockingIssues(t *testing.T) {
	// 两个 bbox 真重叠的器件（R1/R2 中心相距 20mil，边长各 100）。
	comps := []boardComp{sgTestComp("R1", 100, 100, 100), sgTestComp("R2", 120, 100, 100)}
	layout := &pcbLayoutReport{
		MinGapMil: 8, AccessMil: 40,
		Overlaps:       []pcbLFinding{{Type: "overlap", A: "R1", B: "R2", Side: "top", OvX: 80, OvY: 100}},
		Shorts:         []pcbLShort{{A: "R1.1", NetA: "N1", B: "R2.1", NetB: "N2", Layer: "top"}},
		OutsideOutline: []pcbLFinding{{Type: "outside-outline", A: "R1"}},
	}
	d := sgScoreClearance(sgTestCtx(comps, sgTestOutline(600, 800), layout))
	if d.Score != 100 {
		t.Fatalf("score = %.1f — blocking issues belong to the report's Blocking list, not to this dimension", d.Score)
	}
	if len(d.Contributors) != 0 {
		t.Errorf("no spacing violation ⇒ nobody to blame here, got %+v", d.Contributors)
	}
	// 但真实最紧间距指标要如实反映"最紧处是 0"（重叠对 rectGap = 0）。
	if d.Metrics["worstGapMil"] != 0 {
		t.Errorf("worstGapMil = %.2f, want 0 for an overlapping pair", d.Metrics["worstGapMil"])
	}
}

// worstGapMil 是自己算的全板真实最小同面间距（layout-lint 只报低于门限的对，读不到
// "余量还剩多少"）。它必须与 rectGap 一致，且不被对侧器件干扰。
func TestScoreGeom_WorstGapUsesSameSideOnly(t *testing.T) {
	top1 := sgTestComp("R1", 0, 0, 100)   // x ∈ [-50,50]
	top2 := sgTestComp("R2", 200, 0, 100) // x ∈ [150,250] → 间距 100
	bottom := sgTestComp("R3", 60, 0, 100)
	bottom.Layer = pcbSideBottom // 对侧：不该把最小间距拉到 10
	bottom.BBox = &layoutBBox{MinX: 10, MinY: -50, MaxX: 110, MaxY: 50}

	ctx := sgTestCtx([]boardComp{top1, top2, bottom}, nil, &pcbLayoutReport{MinGapMil: 8})
	if got := sgWorstGap(ctx); math.Abs(got-100) > 0.01 {
		t.Fatalf("worst same-side gap = %.2f, want 100 (the bottom-side part must not count)", got)
	}
}

// ---------------------------------------------------------------------------
// 斜坡函数
// ---------------------------------------------------------------------------

func TestScoreGeom_Ramp(t *testing.T) {
	cases := []struct{ v, want float64 }{
		{0, 0}, {1, 0}, {2, 0.5}, {3, 1}, {9, 1},
	}
	for _, c := range cases {
		if got := sgRamp(c.v, 1, 3); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("sgRamp(%.1f,1,3) = %.3f, want %.3f", c.v, got, c.want)
		}
	}
	if got := sgRamp(5, 3, 3); got != 0 {
		t.Errorf("degenerate range must not blow up, got %.3f", got)
	}
	if got := sgRamp(math.NaN(), 1, 3); got != 0 {
		t.Errorf("NaN input must be neutral, got %.3f", got)
	}
}
