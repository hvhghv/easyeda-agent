package app

// pcb_score_flow_test.go — 信号流向维（dimFlowOrder）的离线单测。
//
// 全部纯结构体字面量喂纯函数，不连 daemon、不读磁盘 —— 照 pcb_check_dfm2_test.go
// 的范式。这一维的判据里有好几条是「反直觉但必须成立」的（反向不扣分、缺阶段不
// 冤枉、面积加权改变结论），那几条各有一个专门的测试钉住。

import (
	"math"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// flowComp 造一个以 (cx,cy) 为 bbox 中心、w×h 大小的器件。
func flowComp(des string, cx, cy, w, h float64) boardComp {
	return boardComp{
		ID: "prim-" + des, Designator: des, Layer: pcbSideTop,
		X: cx, Y: cy,
		BBox: &layoutBBox{MinX: cx - w/2, MinY: cy - h/2, MaxX: cx + w/2, MaxY: cy + h/2},
	}
}

// flowSnap 造一块带矩形板框的板。w×h 决定 auto 轴取长边。
func flowSnap(w, h float64, comps ...boardComp) *boardSnapshot {
	return &boardSnapshot{
		Components: comps,
		Outline: &boardOutline{
			BBox:   layoutBBox{MinX: 0, MinY: 0, MaxX: w, MaxY: h},
			Source: "polygon",
			Points: [][2]float64{{0, 0}, {w, 0}, {w, h}, {0, h}},
		},
	}
}

func flowMod(kind string, parts ...string) spec.Module {
	return spec.Module{Name: strings.ToLower(kind) + "-mod", Kind: kind, Parts: parts}
}

func flowScore(snap *boardSnapshot, s *spec.Spec) scoreDimension {
	return flowOrderScorer{}.score(&scoreCtx{snap: snap, spec: s})
}

func nearly(t *testing.T, label string, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Errorf("%s = %g, want %g (±%g)", label, got, want, eps)
	}
}

// ---------------------------------------------------------------------------
// 门：没测 ≠ 满分
// ---------------------------------------------------------------------------

// 没有 spec 就没有声明的流向可比 —— 必须 skipped 且带原因，绝不能默认满分
// （满分会让 #167 第五层「好板得高分」的校准判据失效）。
func TestFlowOrder_SkipsWithoutSpec(t *testing.T) {
	d := flowScore(flowSnap(2000, 1000, flowComp("U1", 500, 500, 100, 100)), nil)
	if d.Status != dimSkipped {
		t.Fatalf("status = %q, want %q", d.Status, dimSkipped)
	}
	if d.Score == 100 {
		t.Errorf("an unmeasured dimension must not score 100")
	}
	if d.Reason == "" {
		t.Errorf("skipped dimension must explain itself")
	}
}

// flow 少于 2 段表达不了顺序。
func TestFlowOrder_SkipsShortFlow(t *testing.T) {
	snap := flowSnap(2000, 1000, flowComp("U1", 500, 500, 100, 100))
	s := &spec.Spec{Flow: []string{"POWER"}, Modules: []spec.Module{flowMod("POWER", "U1")}}
	if d := flowScore(snap, s); d.Status != dimSkipped {
		t.Fatalf("single-stage flow: status = %q, want skipped (%+v)", d.Status, d)
	}
	// 重复项在归一后也只算一段（spec.Validate 把重复报 ERROR，打分侧不该因此崩）。
	s2 := &spec.Spec{Flow: []string{"POWER", "power", " POWER "}, Modules: []spec.Module{flowMod("POWER", "U1")}}
	if d := flowScore(snap, s2); d.Status != dimSkipped {
		t.Fatalf("duplicate-only flow: status = %q, want skipped", d.Status)
	}
}

// 声明了 3 段但只有 1 段落到板上 —— 顺序无从谈起，skipped 而不是拿剩下那段编个分。
func TestFlowOrder_SkipsWhenTooFewStagesOnBoard(t *testing.T) {
	snap := flowSnap(2000, 1000, flowComp("U1", 300, 500, 200, 200))
	s := &spec.Spec{
		Flow: []string{"POWER", "MCU", "RF"},
		Modules: []spec.Module{
			flowMod("POWER", "U1"),
			flowMod("MCU", "U2"), // 没放到板上
			// RF 干脆没有模块
		},
	}
	d := flowScore(snap, s)
	if d.Status != dimSkipped {
		t.Fatalf("status = %q, want skipped (%+v)", d.Status, d)
	}
	if !strings.Contains(d.Reason, "MCU") || !strings.Contains(d.Reason, "RF") {
		t.Errorf("reason must name the stages that dropped out: %q", d.Reason)
	}
}

// ---------------------------------------------------------------------------
// 主判据
// ---------------------------------------------------------------------------

// 左→右 电源→数字→RF→天线：教科书式的好板，满分、无归因。
func TestFlowOrder_PerfectOrder(t *testing.T) {
	snap := flowSnap(2000, 1000,
		flowComp("U1", 200, 500, 200, 200),
		flowComp("U2", 800, 500, 300, 300),
		flowComp("U3", 1400, 500, 200, 200),
		flowComp("E1", 1800, 500, 100, 100),
	)
	s := &spec.Spec{
		Flow: []string{"POWER", "MCU", "RF", "ANT"},
		Modules: []spec.Module{
			flowMod("POWER", "U1"), flowMod("MCU", "U2"),
			flowMod("RF", "U3"), flowMod("ANT", "E1"),
		},
	}
	d := flowScore(snap, s)
	if d.Status != dimScored {
		t.Fatalf("status = %q reason=%q, want scored", d.Status, d.Reason)
	}
	if d.Score != 100 {
		t.Errorf("score = %v, want 100", d.Score)
	}
	nearly(t, "tau", d.Metrics["tau"], 1, 0.001)
	nearly(t, "reversed", d.Metrics["reversed"], 0, 0.001)
	nearly(t, "axis", d.Metrics["axis"], 0, 0.001) // 2000×1000 → 长边 x
	nearly(t, "stageCount", d.Metrics["stageCount"], 4, 0.001)
	if len(d.Contributors) != 0 {
		t.Errorf("a perfect flow must blame nobody, got %+v", d.Contributors)
	}
}

// 板上从右往左排 = 坐标系相反，不是坏布局。这是这一维最关键的一条设计判断：
// 不这么做的话，一块完美的板会因为作者写 spec 时脑子里的方向而拿 0 分。
func TestFlowOrder_ReversedIsNotPenalized(t *testing.T) {
	snap := flowSnap(2000, 1000,
		flowComp("U1", 1800, 500, 200, 200), // POWER 在右
		flowComp("U2", 1200, 500, 300, 300),
		flowComp("U3", 600, 500, 200, 200),
		flowComp("E1", 200, 500, 100, 100), // ANT 在左
	)
	s := &spec.Spec{
		Flow: []string{"POWER", "MCU", "RF", "ANT"},
		Modules: []spec.Module{
			flowMod("POWER", "U1"), flowMod("MCU", "U2"),
			flowMod("RF", "U3"), flowMod("ANT", "E1"),
		},
	}
	d := flowScore(snap, s)
	if d.Score != 100 {
		t.Fatalf("a monotonic-but-mirrored flow must score 100, got %v (%+v)", d.Score, d.Metrics)
	}
	nearly(t, "tau", d.Metrics["tau"], -1, 0.001) // 原始 tau 仍是 -1
	nearly(t, "tauEffective", d.Metrics["tauEffective"], 1, 0.001)
	nearly(t, "reversed", d.Metrics["reversed"], 1, 0.001)
	if len(d.Contributors) != 0 {
		t.Errorf("mirrored flow must blame nobody, got %+v", d.Contributors)
	}
	if !hasFindingType(d.Findings, "flow-reversed") {
		t.Errorf("expected a flow-reversed INFO finding, got %+v", d.Findings)
	}
}

// MCU 和 RF 互换了位置：扣分、归因指向这两段的代表器件，扣分之和 == 100−Score。
func TestFlowOrder_SwappedStagesAreBlamed(t *testing.T) {
	snap := flowSnap(2000, 1000,
		flowComp("U1", 200, 500, 200, 200),  // POWER 位置对
		flowComp("U2", 1400, 500, 300, 300), // MCU 跑到 RF 的位置
		flowComp("U3", 800, 500, 200, 200),  // RF 跑到 MCU 的位置
		flowComp("E1", 1800, 500, 100, 100), // ANT 位置对
	)
	s := &spec.Spec{
		Flow: []string{"POWER", "MCU", "RF", "ANT"},
		Modules: []spec.Module{
			flowMod("POWER", "U1"), flowMod("MCU", "U2"),
			flowMod("RF", "U3"), flowMod("ANT", "E1"),
		},
	}
	d := flowScore(snap, s)
	if d.Status != dimScored {
		t.Fatalf("status = %q reason=%q, want scored", d.Status, d.Reason)
	}
	// C=5 D=1 n0=6 → tau=0.667 → (0.667+1)/2×100 = 83.3
	nearly(t, "tau", d.Metrics["tau"], 0.67, 0.02)
	nearly(t, "score", d.Score, 83.3, 0.2)

	if len(d.Contributors) != 2 {
		t.Fatalf("expected the two swapped stages' leads, got %+v", d.Contributors)
	}
	blamed := map[string]bool{}
	var sum float64
	for _, c := range d.Contributors {
		blamed[c.Designator] = true
		sum += c.Penalty
	}
	if !blamed["U2"] || !blamed["U3"] {
		t.Errorf("expected U2+U3 blamed, got %+v", d.Contributors)
	}
	if blamed["U1"] || blamed["E1"] {
		t.Errorf("stages that sit correctly must not be blamed: %+v", d.Contributors)
	}
	nearly(t, "sum(penalty)", sum, 100-d.Score, 0.2)
	// 归因必须按扣分降序（精修环靠这个梯度决定先动谁）。
	for i := 1; i < len(d.Contributors); i++ {
		if d.Contributors[i-1].Penalty < d.Contributors[i].Penalty {
			t.Fatalf("contributors not sorted by penalty desc: %+v", d.Contributors)
		}
	}
	nearly(t, "maxDisplacement", d.Metrics["maxDisplacement"], 1, 0.001)
}

// 偏移 ≥2（跨过了至少一个别的域）报 WARN，相邻互换只报 INFO。
func TestFlowOrder_SeriousDisplacementIsWarn(t *testing.T) {
	// ANT 跑到最前面：实际序 ANT,POWER,MCU,RF → ANT 偏移 3。
	snap := flowSnap(2000, 1000,
		flowComp("U1", 600, 500, 200, 200),
		flowComp("U2", 1100, 500, 300, 300),
		flowComp("U3", 1600, 500, 200, 200),
		flowComp("E1", 100, 500, 100, 100),
	)
	s := &spec.Spec{
		Flow: []string{"POWER", "MCU", "RF", "ANT"},
		Modules: []spec.Module{
			flowMod("POWER", "U1"), flowMod("MCU", "U2"),
			flowMod("RF", "U3"), flowMod("ANT", "E1"),
		},
	}
	d := flowScore(snap, s)
	var warn int
	for _, f := range d.Findings {
		if f.Type == "flow-order-stage" && f.Level == "WARN" {
			warn++
			if !strings.Contains(f.Message, "规范 §3.3") {
				t.Errorf("finding must carry the design-rule pointer: %q", f.Message)
			}
			if f.At == nil {
				t.Errorf("finding must pin the stage centroid: %+v", f)
			}
		}
	}
	if warn == 0 {
		t.Fatalf("a 3-position displacement must raise a WARN, got %+v", d.Findings)
	}
}

// 面积加权是有后果的：三颗贴在左边的去耦电容按算术平均会把 MCU 的质心拽到
// POWER 前面（(1000+100×3)/4 = 325 < POWER 的 400），从而把一块好板判成失序。
// 按面积加权后质心落在主控身上（≈974），顺序正确。
func TestFlowOrder_CentroidIsAreaWeighted(t *testing.T) {
	snap := flowSnap(2000, 1000,
		flowComp("U1", 400, 500, 200, 200),  // POWER
		flowComp("U2", 1000, 500, 500, 500), // MCU 主控（大）
		flowComp("C1", 100, 500, 50, 50),    // MCU 的去耦，贴在板左
		flowComp("C2", 100, 400, 50, 50),
		flowComp("C3", 100, 600, 50, 50),
		flowComp("U3", 1600, 500, 200, 200), // RF
	)
	s := &spec.Spec{
		Flow: []string{"POWER", "MCU", "RF"},
		Modules: []spec.Module{
			flowMod("POWER", "U1"),
			flowMod("MCU", "U2", "C1", "C2", "C3"),
			flowMod("RF", "U3"),
		},
	}
	d := flowScore(snap, s)
	if got := d.Metrics["centroid.MCU.x"]; got < 900 {
		t.Fatalf("MCU centroid = %g, want ≈974 (area-weighted); an unweighted mean would be 325", got)
	}
	if d.Score != 100 {
		t.Errorf("area-weighted order is correct → score must be 100, got %v", d.Score)
	}
	nearly(t, "parts.MCU", d.Metrics["parts.MCU"], 4, 0.001)
}

// 声明了但板上没有的阶段必须被剔除，绝不能当成 (0,0) —— 那会把 tau 算成负数，
// 冤枉一块顺序完全正确的板。剔除后整维标 degraded 并在 Reason 里说明。
func TestFlowOrder_MissingStageDroppedNotZeroed(t *testing.T) {
	snap := flowSnap(2000, 1000,
		flowComp("U1", 200, 500, 200, 200),
		flowComp("U2", 900, 500, 300, 300),
		flowComp("U3", 1600, 500, 200, 200),
	)
	s := &spec.Spec{
		Flow: []string{"POWER", "MCU", "RF", "ANT"},
		Modules: []spec.Module{
			flowMod("POWER", "U1"), flowMod("MCU", "U2"), flowMod("RF", "U3"),
			flowMod("ANT", "E1"), // 板载天线，没有实体器件
		},
	}
	d := flowScore(snap, s)
	if d.Score != 100 {
		t.Fatalf("the three placed stages are in order → 100, got %v (a zeroed ANT would push tau negative)", d.Score)
	}
	if d.Status != dimDegraded {
		t.Errorf("status = %q, want degraded (only 3 of 4 stages were measured)", d.Status)
	}
	if !strings.Contains(d.Reason, "ANT") {
		t.Errorf("reason must name the dropped stage: %q", d.Reason)
	}
	nearly(t, "stageCount", d.Metrics["stageCount"], 3, 0.001)
	nearly(t, "declaredStages", d.Metrics["declaredStages"], 4, 0.001)
}

// 只写了 block 没写 parts 的模块，剔除原因要跟「写了但没放」区分开 —— 两者的修法
// 完全不同（展开块 vs 检查漏放）。
func TestFlowOrder_ReasonDistinguishesWhyAStageDropped(t *testing.T) {
	snap := flowSnap(2000, 1000,
		flowComp("U1", 200, 500, 200, 200),
		flowComp("U2", 900, 500, 300, 300),
		flowComp("U3", 1600, 500, 200, 200),
	)
	s := &spec.Spec{
		Flow: []string{"POWER", "MCU", "RF", "ANT", "USB"},
		Modules: []spec.Module{
			flowMod("POWER", "U1"), flowMod("MCU", "U2"), flowMod("RF", "U3"),
			{Name: "ant", Kind: "ANT", Block: "chip-antenna"}, // 只有块，没有位号
			flowMod("USB", "J9"),                              // 有位号，板上没有
		},
	}
	d := flowScore(snap, s)
	if !strings.Contains(d.Reason, "no parts") {
		t.Errorf("block-only stage must say so: %q", d.Reason)
	}
	if !strings.Contains(d.Reason, "J9") {
		t.Errorf("missing-part stage must name the part: %q", d.Reason)
	}
}

// ---------------------------------------------------------------------------
// 轴
// ---------------------------------------------------------------------------

// 竖板（500×2000）的 auto 轴是 y。若误取 x，三个阶段的 x 全相同 → 全并列 →
// tau=0 → 50 分，所以这个用例真的能分辨轴选错。
func TestFlowOrder_AutoAxisFollowsBoardLongEdge(t *testing.T) {
	snap := flowSnap(500, 2000,
		flowComp("U1", 250, 200, 200, 200),
		flowComp("U2", 250, 1000, 300, 300),
		flowComp("U3", 250, 1700, 200, 200),
	)
	s := &spec.Spec{
		Flow:    []string{"POWER", "MCU", "RF"},
		Modules: []spec.Module{flowMod("POWER", "U1"), flowMod("MCU", "U2"), flowMod("RF", "U3")},
	}
	d := flowScore(snap, s)
	nearly(t, "axis", d.Metrics["axis"], 1, 0.001)
	if d.Score != 100 {
		t.Errorf("score = %v, want 100 (bottom→top flow on a tall board)", d.Score)
	}
}

// spec 显式写死轴时优先于板框长边。
func TestFlowOrder_ExplicitAxisWins(t *testing.T) {
	// 板是横的（长边 x），但器件按 y 排 —— 显式 flowAxis:"y" 必须让它得满分。
	snap := flowSnap(2000, 1000,
		flowComp("U1", 1000, 100, 200, 200),
		flowComp("U2", 1000, 500, 300, 300),
		flowComp("U3", 1000, 900, 200, 200),
	)
	s := &spec.Spec{
		FlowAxis: "y",
		Flow:     []string{"POWER", "MCU", "RF"},
		Modules:  []spec.Module{flowMod("POWER", "U1"), flowMod("MCU", "U2"), flowMod("RF", "U3")},
	}
	d := flowScore(snap, s)
	nearly(t, "axis", d.Metrics["axis"], 1, 0.001)
	if d.Score != 100 {
		t.Errorf("score = %v, want 100", d.Score)
	}
}

// 板框读不到（PCB 不在前台，平台返 null）时用器件质心分布的主轴，并标 degraded。
func TestFlowOrder_NoOutlineUsesSpreadAxisAndDegrades(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		flowComp("U1", 250, 200, 200, 200),
		flowComp("U2", 250, 1000, 300, 300),
		flowComp("U3", 250, 1700, 200, 200),
	}}
	s := &spec.Spec{
		Flow:    []string{"POWER", "MCU", "RF"},
		Modules: []spec.Module{flowMod("POWER", "U1"), flowMod("MCU", "U2"), flowMod("RF", "U3")},
	}
	d := flowScore(snap, s)
	if d.Status != dimDegraded {
		t.Fatalf("status = %q, want degraded (axis was inferred, not read)", d.Status)
	}
	if !strings.Contains(d.Reason, "outline unavailable") {
		t.Errorf("reason must say the outline was missing: %q", d.Reason)
	}
	nearly(t, "axis", d.Metrics["axis"], 1, 0.001) // y 方向铺得开
	if d.Score != 100 {
		t.Errorf("score = %v, want 100", d.Score)
	}
}

// ---------------------------------------------------------------------------
// 输入降级
// ---------------------------------------------------------------------------

// 读不到渲染 bbox 的器件用「全板平均面积」当替身权重，而不是被静默丢掉；
// 整维标 degraded 并说明。
func TestFlowOrder_MissingBBoxDegrades(t *testing.T) {
	noBox := boardComp{ID: "p-U2", Designator: "U2", Layer: pcbSideTop, X: 900, Y: 500}
	snap := flowSnap(2000, 1000,
		flowComp("U1", 200, 500, 200, 200),
		noBox,
		flowComp("U3", 1600, 500, 200, 200),
	)
	s := &spec.Spec{
		Flow:    []string{"POWER", "MCU", "RF"},
		Modules: []spec.Module{flowMod("POWER", "U1"), flowMod("MCU", "U2"), flowMod("RF", "U3")},
	}
	d := flowScore(snap, s)
	if d.Status != dimDegraded {
		t.Fatalf("status = %q, want degraded", d.Status)
	}
	if !strings.Contains(d.Reason, "no rendered bbox") {
		t.Errorf("reason must mention the bbox fallback: %q", d.Reason)
	}
	// anchor 被当作质心，顺序仍然成立 → 满分。
	if d.Score != 100 {
		t.Errorf("score = %v, want 100", d.Score)
	}
	nearly(t, "centroid.MCU.x", d.Metrics["centroid.MCU.x"], 900, 0.01)
}

// 一个位号同时属于两个 flow 阶段 → 两段质心不再独立，报 WARN 让人知道分数可疑。
func TestFlowOrder_DuplicateDesignatorAcrossStages(t *testing.T) {
	snap := flowSnap(2000, 1000,
		flowComp("U1", 200, 500, 200, 200),
		flowComp("U2", 900, 500, 300, 300),
		flowComp("U3", 1600, 500, 200, 200),
	)
	s := &spec.Spec{
		Flow: []string{"POWER", "MCU", "RF"},
		Modules: []spec.Module{
			flowMod("POWER", "U1"),
			flowMod("MCU", "U2"),
			flowMod("RF", "U3", "U2"), // U2 被 RF 也认领了
		},
	}
	d := flowScore(snap, s)
	if !hasFindingType(d.Findings, "flow-stage-overlap") {
		t.Fatalf("expected a flow-stage-overlap WARN, got %+v", d.Findings)
	}
}

// 两段质心在流向轴上重合：tau-b 把这一对剔掉（不当成任意顺序白拿相关性），
// 分数因此不满，归因落到重合的那两段而不是没人。
func TestFlowOrder_TiedStagesLoseCreditAndAreBlamed(t *testing.T) {
	snap := flowSnap(2000, 1000,
		flowComp("U1", 200, 300, 200, 200),
		flowComp("U2", 1000, 300, 200, 200), // MCU 与 RF 的 x 完全相同
		flowComp("U3", 1000, 700, 200, 200),
	)
	s := &spec.Spec{
		FlowAxis: "x",
		Flow:     []string{"POWER", "MCU", "RF"},
		Modules:  []spec.Module{flowMod("POWER", "U1"), flowMod("MCU", "U2"), flowMod("RF", "U3")},
	}
	d := flowScore(snap, s)
	if d.Score >= 100 {
		t.Fatalf("a tied pair must not get full credit, got %v", d.Score)
	}
	if len(d.Contributors) == 0 {
		t.Fatalf("a tie must still be attributed to the overlapping stages")
	}
	var sum float64
	for _, c := range d.Contributors {
		sum += c.Penalty
		if c.Designator == "U1" {
			t.Errorf("POWER is not part of the tie, must not be blamed: %+v", c)
		}
	}
	nearly(t, "sum(penalty)", sum, 100-d.Score, 0.2)
}

// ---------------------------------------------------------------------------
// Kendall tau-b 本体
// ---------------------------------------------------------------------------

func TestKendallTauB(t *testing.T) {
	cases := []struct {
		name       string
		a, b       []float64
		epsA, epsB float64
		want       float64
	}{
		{"identical order", []float64{0, 1, 2, 3}, []float64{10, 20, 30, 40}, 0, 0, 1},
		{"reverse order", []float64{0, 1, 2, 3}, []float64{40, 30, 20, 10}, 0, 0, -1},
		{"one swap of four", []float64{0, 1, 2, 3}, []float64{10, 30, 20, 40}, 0, 0, 4.0 / 6.0},
		// 一对并列：n0=3, n2=1 → tau = 2/sqrt(3×2) = 0.8165（tau-a 会给 2/3）
		{"one tie", []float64{0, 1, 2}, []float64{10, 20, 20}, 0, 0, 2 / math.Sqrt(6)},
		// eps 让 0.5 的差也算并列，结果与上一例相同 —— 证明容差真的生效
		{"tie via eps", []float64{0, 1, 2}, []float64{10, 20, 20.5}, 0, 1, 2 / math.Sqrt(6)},
		// 全部并列 → 没有任何序信息，相关性无定义，返回 0
		{"all tied", []float64{0, 1, 2}, []float64{7, 7, 7}, 0, 0, 0},
		{"too short", []float64{1}, []float64{1}, 0, 0, 0},
		{"length mismatch", []float64{1, 2}, []float64{1}, 0, 0, 0},
	}
	for _, c := range cases {
		if got := kendallTauB(c.a, c.b, c.epsA, c.epsB); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: tau = %v, want %v", c.name, got, c.want)
		}
	}
}

// 反向 tau 恒等于正向取负 —— 这是「取绝对值大的那个 == 取 |tau|」这条简化的
// 前提，钉住它，将来谁改了 tau 实现能立刻发现。
func TestKendallTauB_ReversingOneRankingNegates(t *testing.T) {
	a := []float64{0, 1, 2, 3, 4}
	b := []float64{100, 40, 380, 220, 260}
	rev := []float64{4, 3, 2, 1, 0}
	fwd := kendallTauB(a, b, 0, 0)
	back := kendallTauB(rev, b, 0, 0)
	if math.Abs(fwd+back) > 1e-9 {
		t.Fatalf("tau(reverse)=%v, want -tau(forward)=%v", back, -fwd)
	}
}

// ---------------------------------------------------------------------------
// 注册与契约
// ---------------------------------------------------------------------------

// 这一维必须在注册表里，否则 analyzeLayoutScore 会走「未实现」分支静默跳过。
func TestFlowOrder_IsRegistered(t *testing.T) {
	sc := scorerFor(dimFlowOrder)
	if sc == nil {
		t.Fatalf("flow-order scorer is not registered")
	}
	if sc.id() != dimFlowOrder {
		t.Errorf("id = %q, want %q", sc.id(), dimFlowOrder)
	}
}

// 走完整的 analyzeLayoutScore：flow-order 维必须真的出现在报告里且带原始量。
func TestFlowOrder_ThroughFullReport(t *testing.T) {
	snap := flowSnap(2000, 1000,
		flowComp("U1", 200, 500, 200, 200),
		flowComp("U2", 900, 500, 300, 300),
		flowComp("U3", 1600, 500, 200, 200),
	)
	s := &spec.Spec{
		Flow:    []string{"POWER", "MCU", "RF"},
		Modules: []spec.Module{flowMod("POWER", "U1"), flowMod("MCU", "U2"), flowMod("RF", "U3")},
	}
	rep := analyzeLayoutScore(snap, s, layoutScoreOpts{only: map[string]bool{dimFlowOrder: true}})
	d := rep.dimension(dimFlowOrder)
	if d == nil {
		t.Fatalf("flow-order missing from the report: %+v", rep.Dimensions)
	}
	if d.Status == dimSkipped {
		t.Fatalf("flow-order should be scored here: %q", d.Reason)
	}
	if len(d.Metrics) == 0 {
		t.Errorf("a scored dimension must expose its raw metrics")
	}
	if d.Title == "" {
		t.Errorf("dimension title not filled in")
	}
}

func hasFindingType(fs []pcbCheckFinding, typ string) bool {
	for _, f := range fs {
		if f.Type == typ {
			return true
		}
	}
	return false
}
