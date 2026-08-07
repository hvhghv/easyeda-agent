package app

// 功能分区维（#167 dimPartition）的离线单测。
//
// 全部是纯结构体字面量喂纯函数，不连 daemon、不读磁盘：这一维的输入就是
// boardSnapshot + spec，两者都是可字面量化的值类型（这正是 #167 地基把「取数」
// 和「解析」拆开的目的）。

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

// partScoreTestComp 造一个「中心在 (cx,cy)、渲染 bbox 为 w×h」的器件。
//
// 注意 boardComp.X/Y 是 anchor 不是中心，但打分一律走 center()（有 bbox 就用
// bbox 中心）——这里让两者重合，测试意图才不会被 anchor 偏移搅浑。
func partScoreTestComp(des string, cx, cy, w, h float64, nets ...string) boardComp {
	c := boardComp{
		Designator: des,
		Layer:      pcbSideTop,
		X:          cx,
		Y:          cy,
		BBox:       &layoutBBox{MinX: cx - w/2, MinY: cy - h/2, MaxX: cx + w/2, MaxY: cy + h/2},
	}
	for i, n := range nets {
		c.Pads = append(c.Pads, boardPad{
			Number: strconv.Itoa(i + 1), Net: n, Layer: pcbSideTop, X: cx, Y: cy, W: 20, H: 20,
		})
	}
	return c
}

// partScoreTestNoBBox 是同一个器件但连接器没给渲染 bbox（真板上 POLYGON/异形封装会这样）。
func partScoreTestNoBBox(des string, x, y float64, nets ...string) boardComp {
	c := partScoreTestComp(des, x, y, 0, 0, nets...)
	c.BBox = nil
	return c
}

func partScoreTestCtx(comps []boardComp, s *spec.Spec) *scoreCtx {
	return &scoreCtx{snap: &boardSnapshot{Components: comps}, spec: s}
}

// partScoreTestPowerBlock 是一个 4 件的降压模块，靠 SW/FB/NODE_A 三条本地信号网自成一簇。
// 中心排成边长 120mil 的方阵（件宽 100mil，间隙 20mil）——真板上紧凑模块的样子。
func partScoreTestPowerBlock(ox, oy float64) []boardComp {
	return []boardComp{
		partScoreTestComp("U1", ox, oy, 100, 60, "SW", "FB"),
		partScoreTestComp("L1", ox+120, oy, 100, 60, "SW", "NODE_A"),
		partScoreTestComp("C1", ox, oy+120, 100, 60, "NODE_A", "GND"),
		partScoreTestComp("C2", ox+120, oy+120, 100, 60, "FB", "NODE_A"),
	}
}

// partScoreTestMcuBlock 是一个 4 件的主控模块，靠 SDA/SCL/BOOT 自成一簇。
func partScoreTestMcuBlock(ox, oy float64) []boardComp {
	return []boardComp{
		partScoreTestComp("U2", ox, oy, 100, 60, "SDA", "SCL", "BOOT"),
		partScoreTestComp("R2", ox+120, oy, 100, 60, "SDA", "GND"),
		partScoreTestComp("R3", ox, oy+120, 100, 60, "SCL", "GND"),
		partScoreTestComp("C5", ox+120, oy+120, 100, 60, "BOOT", "GND"),
	}
}

func partScoreTestMetric(t *testing.T, d scoreDimension, key string) float64 {
	t.Helper()
	v, ok := d.Metrics[key]
	if !ok {
		t.Fatalf("metric %q missing (have %v)", key, d.Metrics)
	}
	return v
}

// ── 注册 ─────────────────────────────────────────────────────────────────────

// 忘了在 init() 里注册，这一维在报告里会变成 "not implemented yet" 的 skipped ——
// 分数照出、没人报错，正是最难发现的那种坏法。
func TestPartitionScorerRegistered(t *testing.T) {
	if scorerFor(dimPartition) == nil {
		t.Fatal("partition scorer is not registered — analyzeLayoutScore will report it as unimplemented")
	}
}

// ── 干净分区 = 满分 ──────────────────────────────────────────────────────────

func TestPartitionScore_CleanSeparation(t *testing.T) {
	comps := append(partScoreTestPowerBlock(200, 200), partScoreTestMcuBlock(1500, 200)...)
	d := partitionScorer{}.score(partScoreTestCtx(comps, nil))

	if d.Score != 100 {
		t.Errorf("score = %.1f, want 100 (two tight, well-separated modules)", d.Score)
	}
	if got := partScoreTestMetric(t, d, "moduleCount"); got != 2 {
		t.Errorf("moduleCount = %.0f, want 2 (SW/FB/NODE_A cluster + SDA/SCL/BOOT cluster)", got)
	}
	if got := partScoreTestMetric(t, d, "interleavedPairs"); got != 0 {
		t.Errorf("interleavedPairs = %.0f, want 0", got)
	}
	if got := partScoreTestMetric(t, d, "worstOverlapRatio"); got != 0 {
		t.Errorf("worstOverlapRatio = %.2f, want 0", got)
	}
	if len(d.Contributors) != 0 {
		t.Errorf("contributors on a clean board: %+v", d.Contributors)
	}
	// 没 spec 时模块是**推断**的，必须降级说明 —— 满分不等于「按设计者的意图验过」。
	if d.Status != dimDegraded {
		t.Errorf("status = %q, want %q (modules inferred from nets, not declared)", d.Status, dimDegraded)
	}
	if !strings.Contains(d.Reason, "inferred") {
		t.Errorf("degraded reason does not say the modules were inferred: %q", d.Reason)
	}
	if partScoreTestMetric(t, d, "specDriven") != 0 {
		t.Error("specDriven should be 0 when there is no spec")
	}
}

// ── 交错 = 扣分 + 归因到闯入者 ───────────────────────────────────────────────

func TestPartitionScore_Interleaved(t *testing.T) {
	// MCU 块整体压进电源块的地盘：两块领地 120×120，交叠 80×80 = 44.4%。
	comps := append(partScoreTestPowerBlock(200, 200), partScoreTestMcuBlock(240, 240)...)
	d := partitionScorer{}.score(partScoreTestCtx(comps, nil))

	if got := partScoreTestMetric(t, d, "interleavedPairs"); got != 1 {
		t.Fatalf("interleavedPairs = %.0f, want 1", got)
	}
	if got := partScoreTestMetric(t, d, "worstOverlapRatio"); math.Abs(got-0.44) > 0.02 {
		t.Errorf("worstOverlapRatio = %.2f, want ~0.44 (6400/14400)", got)
	}
	// 60 × (0.444-0.10)/0.90 ≈ 22.96 分的交错扣分，紧凑度两边都合格。
	if math.Abs(d.Score-77.0) > 1.0 {
		t.Errorf("score = %.1f, want ~77.0", d.Score)
	}
	if len(d.Contributors) == 0 {
		t.Fatal("no contributors — 只给分数不给归因的维等于没做（#167）")
	}
	// 落在对方腹地的是 C2(320,320) 和 U2(240,240)，其余六件都在自己窝里。
	blamed := map[string]bool{}
	for _, c := range d.Contributors {
		blamed[c.Designator] = true
		if c.Penalty <= 0 {
			t.Errorf("contributor %s has zero penalty — 精修环靠 Penalty 排序", c.Designator)
		}
	}
	for _, want := range []string{"C2", "U2"} {
		if !blamed[want] {
			t.Errorf("intruder %s not blamed (got %+v)", want, d.Contributors)
		}
	}
	if len(d.Findings) != 1 || d.Findings[0].Type != "module-interleave" {
		t.Fatalf("findings = %+v, want one module-interleave", d.Findings)
	}
	if !strings.Contains(d.Findings[0].Message, "规范 §3.3") {
		t.Errorf("finding does not cite the design-rule section: %q", d.Findings[0].Message)
	}
}

// 归因梯度必须严格降序 —— 下游精修环直接取前几个决定先动谁。
func TestPartitionScore_ContributorsSortedDescending(t *testing.T) {
	comps := append(partScoreTestPowerBlock(200, 200), partScoreTestMcuBlock(240, 240)...)
	d := partitionScorer{}.score(partScoreTestCtx(comps, nil))
	for i := 1; i < len(d.Contributors); i++ {
		if d.Contributors[i-1].Penalty < d.Contributors[i].Penalty {
			t.Fatalf("contributors not sorted by penalty desc: %+v", d.Contributors)
		}
	}
}

// ── spec 声明优先于网聚类 ────────────────────────────────────────────────────

func TestPartitionScore_SpecDrivenBeatsNetClustering(t *testing.T) {
	comps := append(partScoreTestPowerBlock(200, 200), partScoreTestMcuBlock(1500, 200)...)
	s := &spec.Spec{Modules: []spec.Module{
		{Name: "power", Kind: "POWER", Parts: []string{"U1", "L1", "C1", "C2"}},
		{Name: "mcu", Kind: "MCU", Parts: []string{"U2", "R2", "R3", "C5"}},
		{Name: "ghost", Kind: "IO", Parts: []string{"J9", "J10"}}, // 声明了但没放
	}}
	d := partitionScorer{}.score(partScoreTestCtx(comps, s))

	if partScoreTestMetric(t, d, "specDriven") != 1 {
		t.Error("specDriven = 0 — spec modules[].parts must win over net clustering")
	}
	// spec 给了明确意图 + 每个件都有 bbox ⇒ 没有任何近似，不该降级。
	if d.Status != dimScored {
		t.Errorf("status = %q (reason %q), want %q", d.Status, d.Reason, dimScored)
	}
	if got := partScoreTestMetric(t, d, "specMissingParts"); got != 2 {
		t.Errorf("specMissingParts = %.0f, want 2 (J9/J10 declared but not placed)", got)
	}
	if d.Score != 100 {
		t.Errorf("score = %.1f, want 100", d.Score)
	}
}

// spec 声明的件一个都没落到板上时，退回网聚类而不是让整维死掉。
func TestPartitionScore_SpecWithNoPlacedPartsFallsBackToNets(t *testing.T) {
	comps := append(partScoreTestPowerBlock(200, 200), partScoreTestMcuBlock(1500, 200)...)
	s := &spec.Spec{Modules: []spec.Module{{Name: "ghost", Parts: []string{"J9", "J10"}}}}
	d := partitionScorer{}.score(partScoreTestCtx(comps, s))

	if d.Status == dimSkipped {
		t.Fatalf("dimension skipped instead of falling back to net clustering: %q", d.Reason)
	}
	if partScoreTestMetric(t, d, "specDriven") != 0 {
		t.Error("specDriven = 1 although no declared part is on the board")
	}
	if got := partScoreTestMetric(t, d, "moduleCount"); got != 2 {
		t.Errorf("moduleCount = %.0f, want 2 from the net fallback", got)
	}
}

// ── 单模块：只算紧凑度，且必须说明 ───────────────────────────────────────────

func TestPartitionScore_SingleModuleCompactOnly(t *testing.T) {
	d := partitionScorer{}.score(partScoreTestCtx(partScoreTestPowerBlock(200, 200), nil))

	if got := partScoreTestMetric(t, d, "moduleCount"); got != 1 {
		t.Fatalf("moduleCount = %.0f, want 1", got)
	}
	if d.Status != dimDegraded {
		t.Errorf("status = %q, want %q — 单模块板没有交错可判，读者必须看得见这件事", d.Status, dimDegraded)
	}
	if !strings.Contains(d.Reason, "compactness") {
		t.Errorf("reason must say the score is compactness-only, got %q", d.Reason)
	}
	if got := partScoreTestMetric(t, d, "interleavedPairs"); got != 0 {
		t.Errorf("interleavedPairs = %.0f, want 0", got)
	}
}

// 单模块但摊得满板都是：可测配额只有紧凑度那 40 分，必须归一化回 0-100 全额扣，
// 不能因为交错测不了就把分数托在 60 分以上。
func TestPartitionScore_SingleModuleSmearedScoresLow(t *testing.T) {
	comps := []boardComp{
		partScoreTestComp("U1", 0, 0, 100, 60, "SW", "FB"),
		partScoreTestComp("L1", 1000, 0, 100, 60, "SW", "NODE_A"),
		partScoreTestComp("C1", 0, 1000, 100, 60, "NODE_A", "GND"),
		partScoreTestComp("C2", 1000, 1000, 100, 60, "FB", "NODE_A"),
	}
	d := partitionScorer{}.score(partScoreTestCtx(comps, nil))

	if d.Score > 5 {
		t.Errorf("score = %.1f, want ~0 — 4 件摊在 1000mil 见方，spreadRatio 已经饱和", d.Score)
	}
	if got := partScoreTestMetric(t, d, "meanSpreadRatio"); got < partScoreSpreadSaturation {
		t.Errorf("meanSpreadRatio = %.2f, want ≥ %.1f", got, partScoreSpreadSaturation)
	}
	if len(d.Contributors) == 0 {
		t.Fatal("a smeared module must name the outliers")
	}
	if len(d.Findings) != 1 || d.Findings[0].Type != "module-spread" {
		t.Fatalf("findings = %+v, want one module-spread", d.Findings)
	}
}

// ── 「没测」绝不能是「满分」 ─────────────────────────────────────────────────

func TestPartitionScore_SkipWhenNoModuleInferable(t *testing.T) {
	// 三件只挂 GND：signalNets 全空 ⇒ 谁都并不到一起 ⇒ 全是单件簇。
	comps := []boardComp{
		partScoreTestComp("H1", 0, 0, 100, 100, "GND"),
		partScoreTestComp("H2", 900, 0, 100, 100, "GND"),
		partScoreTestComp("H3", 0, 900, 100, 100, "GND"),
	}
	d := partitionScorer{}.score(partScoreTestCtx(comps, nil))

	if d.Status != dimSkipped {
		t.Fatalf("status = %q score = %.1f — 聚不出模块必须 skipped，绝不能默认满分", d.Status, d.Score)
	}
	if d.Score != 0 {
		t.Errorf("skipped dimension score = %.1f, want 0 (skipped 不参与加权，分数无意义)", d.Score)
	}
	if d.Reason == "" {
		t.Error("skipped dimension must explain why — 报告要能回答「这维为什么没分」")
	}
}

func TestPartitionScore_SkipWhenTooFewComponents(t *testing.T) {
	d := partitionScorer{}.score(partScoreTestCtx([]boardComp{partScoreTestComp("U1", 0, 0, 100, 60, "SDA")}, nil))
	if d.Status != dimSkipped || d.Score != 0 {
		t.Fatalf("single-component board: status=%q score=%.1f, want skipped/0", d.Status, d.Score)
	}
}

// ── 高扇出网不得把整板并成一坨 ───────────────────────────────────────────────

func TestPartitionScore_HighFanoutNetDoesNotMergeBoard(t *testing.T) {
	// 10 件共享 "BUS"（扇出 10 > 8，必须跳过），其中两对另有自己的本地网。
	comps := []boardComp{
		partScoreTestComp("A1", 0, 0, 100, 60, "BUS", "PA"),
		partScoreTestComp("A2", 120, 0, 100, 60, "BUS", "PA"),
		partScoreTestComp("B1", 2000, 0, 100, 60, "BUS", "PB"),
		partScoreTestComp("B2", 2120, 0, 100, 60, "BUS", "PB"),
	}
	for i := 0; i < 6; i++ { // 只挂总线的填充件
		comps = append(comps, partScoreTestComp("F"+strconv.Itoa(i), float64(4000+i*200), 0, 100, 60, "BUS"))
	}
	d := partitionScorer{}.score(partScoreTestCtx(comps, nil))

	if got := partScoreTestMetric(t, d, "moduleCount"); got != 2 {
		t.Fatalf("moduleCount = %.0f, want 2 — 扇出 %d 的总线被 union 了，整板并成一坨就判不出任何分区结构",
			got, len(comps))
	}
	if got := partScoreTestMetric(t, d, "singletonModules"); got != 6 {
		t.Errorf("singletonModules = %.0f, want 6 (只挂总线的填充件)", got)
	}
	if got := partScoreTestMetric(t, d, "looseParts"); got != 6 {
		t.Errorf("looseParts = %.0f, want 6", got)
	}
}

// ── 退化模块（成员共线）仍要抓得到交错 ───────────────────────────────────────

func TestPartitionScore_DegenerateModuleStillDetected(t *testing.T) {
	// P 是两颗排成一行的件（领地高度为 0），整个躺在 Q 的腹地里。
	// 面积比在这里恒为「不重叠」（oy==0），必须靠闯入比例兜底才抓得到。
	comps := []boardComp{
		partScoreTestComp("Q1", 0, 0, 100, 60, "QA", "QB"),
		partScoreTestComp("Q2", 120, 0, 100, 60, "QA", "GND"),
		partScoreTestComp("Q3", 0, 120, 100, 60, "QB", "QC"),
		partScoreTestComp("Q4", 120, 120, 100, 60, "QC", "GND"),
		partScoreTestComp("P1", 40, 60, 100, 60, "PNET", "GND"),
		partScoreTestComp("P2", 80, 60, 100, 60, "PNET", "GND"),
	}
	d := partitionScorer{}.score(partScoreTestCtx(comps, nil))

	if got := partScoreTestMetric(t, d, "interleavedPairs"); got != 1 {
		t.Fatalf("interleavedPairs = %.0f, want 1 — 退化模块的交错漏检了", got)
	}
	if got := partScoreTestMetric(t, d, "worstOverlapRatio"); got != 1 {
		t.Errorf("worstOverlapRatio = %.2f, want 1 (P 的两件全在 Q 腹地里)", got)
	}
	// 交错吃满 60 分配额，两个模块的紧凑度都合格 ⇒ 100-60 = 40。
	if math.Abs(d.Score-40) > 0.1 {
		t.Errorf("score = %.1f, want 40.0", d.Score)
	}
	blamed := map[string]bool{}
	for _, c := range d.Contributors {
		blamed[c.Designator] = true
	}
	if !blamed["P1"] || !blamed["P2"] {
		t.Errorf("intruders P1/P2 not blamed: %+v", d.Contributors)
	}
}

// ── 无渲染 bbox：降级而不是假装算准了 ────────────────────────────────────────

func TestPartitionScore_NoBBoxDegrades(t *testing.T) {
	comps := []boardComp{
		partScoreTestNoBBox("U1", 200, 200, "SW", "FB"),
		partScoreTestNoBBox("L1", 320, 200, "SW", "NODE_A"),
		partScoreTestNoBBox("U2", 1500, 200, "SDA", "SCL"),
		partScoreTestNoBBox("R2", 1620, 200, "SDA", "GND"),
	}
	d := partitionScorer{}.score(partScoreTestCtx(comps, nil))

	if d.Status != dimDegraded {
		t.Fatalf("status = %q, want %q", d.Status, dimDegraded)
	}
	if !strings.Contains(d.Reason, "bbox") {
		t.Errorf("reason must mention the missing bbox, got %q", d.Reason)
	}
	// 紧凑度没有面积就测不了；可测的只剩交错那 60 分配额，两块分得很开 ⇒ 100。
	if d.Score != 100 {
		t.Errorf("score = %.1f, want 100 (interleaving is still measurable from anchors)", d.Score)
	}
	if partScoreTestMetric(t, d, "spreadPenalty") != 0 {
		t.Error("spreadPenalty must be 0 when compactness is unmeasurable")
	}
}

// 只有一个模块 **且** 成员全无 bbox：两半都测不了 ⇒ skipped，不是 100。
func TestPartitionScore_SingleModuleNoBBoxSkips(t *testing.T) {
	comps := []boardComp{
		partScoreTestNoBBox("U1", 200, 200, "SW"),
		partScoreTestNoBBox("L1", 320, 200, "SW"),
	}
	d := partitionScorer{}.score(partScoreTestCtx(comps, nil))
	if d.Status != dimSkipped || d.Score != 0 {
		t.Fatalf("status=%q score=%.1f, want skipped/0 — 两半都测不了却给分是最坏的假阳性",
			d.Status, d.Score)
	}
}

// ── 纯几何件的直接单测 ───────────────────────────────────────────────────────

func TestBuildPartScoreModule_Geometry(t *testing.T) {
	m := buildPartScoreModule("m", partScoreTestPowerBlock(0, 0))
	if m.Box != (layoutBBox{MinX: 0, MinY: 0, MaxX: 120, MaxY: 120}) {
		t.Errorf("box = %+v, want the envelope of member centers", m.Box)
	}
	if m.CX != 60 || m.CY != 60 {
		t.Errorf("centroid = (%.1f,%.1f), want (60,60)", m.CX, m.CY)
	}
	if want := math.Hypot(60, 60); math.Abs(m.Spread-want) > 0.01 {
		t.Errorf("spread = %.2f, want %.2f", m.Spread, want)
	}
	// 4 件 × 100×60 = 24000mil² ⇒ 等面积圆半径 sqrt(24000/π) ≈ 87.4
	if want := math.Sqrt(24000 / math.Pi); math.Abs(m.RIdeal-want) > 0.01 {
		t.Errorf("rIdeal = %.2f, want %.2f", m.RIdeal, want)
	}
}

// 部分成员没 bbox 时按已知面积等比外推，别把未知当 0 —— 当 0 会让 RIdeal 偏小、
// spreadRatio 偏大，凭空冤枉一个其实很紧凑的模块。
func TestBuildPartScoreModule_ExtrapolatesUnknownArea(t *testing.T) {
	members := []boardComp{
		partScoreTestComp("U1", 0, 0, 100, 60, "SW"),
		partScoreTestComp("L1", 120, 0, 100, 60, "SW"),
		partScoreTestNoBBox("C1", 0, 120, "SW"),
		partScoreTestNoBBox("C2", 120, 120, "SW"),
	}
	m := buildPartScoreModule("m", members)
	if m.known != 2 {
		t.Fatalf("known = %d, want 2", m.known)
	}
	// 已知 2 件共 12000mil²，外推到 4 件 = 24000 ⇒ 与全都有 bbox 时同一个半径。
	if want := math.Sqrt(24000 / math.Pi); math.Abs(m.RIdeal-want) > 0.01 {
		t.Errorf("rIdeal = %.2f, want %.2f (extrapolated from the 2 known members)", m.RIdeal, want)
	}
}

func TestPartScoreInterleave_Disjoint(t *testing.T) {
	a := buildPartScoreModule("a", partScoreTestPowerBlock(0, 0))
	b := buildPartScoreModule("b", partScoreTestMcuBlock(2000, 0))
	if ratio, intruders := partScoreInterleave(a, b); ratio != 0 || len(intruders) != 0 {
		t.Errorf("disjoint modules: ratio=%.2f intruders=%v, want 0/none", ratio, intruders)
	}
}

// 完全包含 = 比例 1（较小模块整个被吞）。
func TestPartScoreInterleave_FullyContained(t *testing.T) {
	outer := buildPartScoreModule("outer", []boardComp{
		partScoreTestComp("Q1", 0, 0, 50, 50, "QA"),
		partScoreTestComp("Q2", 400, 0, 50, 50, "QA"),
		partScoreTestComp("Q3", 0, 400, 50, 50, "QB"),
		partScoreTestComp("Q4", 400, 400, 50, 50, "QB"),
	})
	inner := buildPartScoreModule("inner", []boardComp{
		partScoreTestComp("P1", 100, 100, 50, 50, "PN"),
		partScoreTestComp("P2", 300, 100, 50, 50, "PN"),
		partScoreTestComp("P3", 100, 300, 50, 50, "PN"),
	})
	ratio, intruders := partScoreInterleave(outer, inner)
	if math.Abs(ratio-1) > 1e-9 {
		t.Errorf("ratio = %.3f, want 1 (inner envelope is fully inside outer)", ratio)
	}
	if len(intruders) != 3 {
		t.Errorf("intruders = %v, want all three inner members", intruders)
	}
}
