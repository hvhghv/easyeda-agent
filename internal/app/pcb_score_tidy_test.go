package app

// pcb_score_tidy_test.go — 齐整度维（#153）的离线单测。全部是纯结构体字面量喂纯
// 函数，不连 daemon、不发 action，照 pcb_check_dfm2_test.go 的范式。
//
// 坐标取自 #153 的真机实测（BBClaw 69 器件）：C2(635.0015, 1109.998) 这类亚 mil
// 漂移是这一维存在的直接理由，所以它进 fixture 而不是随手编一个 0.5mil 偏移。

import (
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// fixture 助手
// ---------------------------------------------------------------------------

// tidyTestComp 造一个两脚贴片件：bbox 以 anchor 为中心（让 center()==anchor，
// 方位/阵列断言不必再绕一层），焊盘 50mil 间距（整 mil ⇒ 英制，不会被公制排除误伤）。
func tidyTestComp(des string, x, y, rot float64) boardComp {
	return boardComp{
		ID: "id-" + des, Designator: des, Layer: pcbSideTop,
		X: x, Y: y, Rotation: rot,
		BBox: &layoutBBox{MinX: x - 20, MinY: y - 10, MaxX: x + 20, MaxY: y + 10},
		Pads: []boardPad{
			{Number: "1", Net: "N1", Layer: pcbSideTop, X: x - 25, Y: y, W: 20, H: 20},
			{Number: "2", Net: "N2", Layer: pcbSideTop, X: x + 25, Y: y, W: 20, H: 20},
		},
	}
}

// tidyTestIC 造一个多脚件，焊盘沿 X 等距排开（pitch 单位 mil）。
func tidyTestIC(des string, x, y, pitch float64, n int) boardComp {
	c := boardComp{
		ID: "id-" + des, Designator: des, Layer: pcbSideTop, X: x, Y: y,
		BBox: &layoutBBox{MinX: x - 50, MinY: y - 50, MaxX: x + 50, MaxY: y + 50},
	}
	for i := 0; i < n; i++ {
		c.Pads = append(c.Pads, boardPad{
			Number: string(rune('1' + i)), Net: "N", Layer: pcbSideTop,
			X: x + float64(i)*pitch, Y: y, W: 10, H: 10,
		})
	}
	return c
}

// tidyTestSilk 造一条位号丝印，(cx,cy) 是它的**中心**（bbox 直接给出，避开 #155 的
// 「X/Y 是左下 anchor」陷阱）。
func tidyTestSilk(compID, text string, cx, cy, font float64) pcbSilkText {
	return pcbSilkText{
		ID: "silk-" + text, Kind: "attribute", Key: "Designator", Text: text,
		Layer: silkTopLayer, FontSize: font, CompID: compID, CompLayer: pcbSideTop,
		X: cx - 10, Y: cy - 5,
		BBox: &pcbRect{MinX: cx - 10, MinY: cy - 5, MaxX: cx + 10, MaxY: cy + 5},
	}
}

func tidyScoreOf(snap *boardSnapshot) scoreDimension {
	return scoreTidy(&scoreCtx{snap: snap, opts: layoutScoreOpts{}})
}

func tidyMetric(t *testing.T, d scoreDimension, key string) float64 {
	t.Helper()
	v, ok := d.Metrics[key]
	if !ok {
		t.Fatalf("metric %q missing (have %v)", key, d.Metrics)
	}
	return v
}

func tidyContributor(d scoreDimension, des string) (scoreContributor, bool) {
	for _, c := range d.Contributors {
		if c.Designator == des {
			return c, true
		}
	}
	return scoreContributor{}, false
}

// ---------------------------------------------------------------------------
// ① off-grid
// ---------------------------------------------------------------------------

// #153 的核心证据：C2(635.0015, 1109.998) 目视和 (635, 1110) 没区别，但它不落 5mil
// 栅。容差放松到 0.005 就会把这条放过 —— 这个维度要抓的东西正好全被放过。
func TestTidy_OffGrid_CatchesSubMilDrift(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		tidyTestComp("C1", 635.0015, 1109.998, 0), // #153 实测漂移坐标
		tidyTestComp("C2", 635, 1210, 0),
		tidyTestComp("C3", 700, 1310, 0),
	}}
	d := tidyScoreOf(snap)

	if d.Status == dimSkipped {
		t.Fatalf("三个器件在板上，这一维不该 skipped：%+v", d)
	}
	if got := tidyMetric(t, d, "offGridCount"); got != 1 {
		t.Errorf("offGridCount = %v, want 1（只有 C1 漂了）", got)
	}
	if got := tidyMetric(t, d, "onGridRatio"); math.Abs(got-2.0/3) > 0.002 {
		t.Errorf("onGridRatio = %v, want ≈0.667", got)
	}
	// 报的必须是「离最近格点多远」而不是布尔标记。
	if got := tidyMetric(t, d, "worstOffGridMil"); math.Abs(got-0.002) > 1e-6 {
		t.Errorf("worstOffGridMil = %v, want 0.002（1109.998 离 1110 最远）", got)
	}
	if _, ok := tidyContributor(d, "C1"); !ok {
		t.Errorf("C1 必须进归因，实际 %+v", d.Contributors)
	}
	if _, ok := tidyContributor(d, "C2"); ok {
		t.Errorf("C2 落格，不该出现在归因里：%+v", d.Contributors)
	}
}

// 三条排除（conventions §9.1）：locked / 机械锚定 / 公制间距件都不进落格分母 ——
// 硬吸它们会把 pad 推离原生子栅或把安装孔挪出结构位置。
func TestTidy_OffGrid_ExcludesLockedMetricAndMechanical(t *testing.T) {
	locked := tidyTestComp("J1", 100.7, 200.3, 0)
	locked.Locked = true
	mech := tidyTestComp("H1", 137.795, 137.795, 0) // 3.5mm 距角，来自结构图
	ic := tidyTestIC("U1", 300.4, 400.4, 19.685, 8) // 0.5mm pitch → 公制
	snap := &boardSnapshot{Components: []boardComp{
		locked, mech, ic, tidyTestComp("R1", 100.5, 200, 0),
	}}
	d := tidyScoreOf(snap)

	if got := tidyMetric(t, d, "gridEligible"); got != 1 {
		t.Fatalf("gridEligible = %v, want 1（只剩 R1）", got)
	}
	for _, des := range []string{"J1", "H1", "U1"} {
		if _, ok := tidyContributor(d, des); ok {
			t.Errorf("%s 被排除后不该出现在归因里：%+v", des, d.Contributors)
		}
	}
	if _, ok := tidyContributor(d, "R1"); !ok {
		t.Errorf("R1 不落格必须被抓到：%+v", d.Contributors)
	}
}

// 公制判据是机械的（看焊盘间距）而不是型号白名单 —— placed 件的 name 常是
// "={Manufacturer Part}" 模板，只有坐标可信。
func TestTidyMetricPitchPart(t *testing.T) {
	cases := []struct {
		name   string
		comp   boardComp
		metric bool
	}{
		{"0.5mm pitch QFN", tidyTestIC("U1", 0, 0, 19.685, 8), true},
		{"0.8mm pitch", tidyTestIC("U2", 0, 0, 31.496, 8), true},
		{"2.0mm 连接器", tidyTestIC("J1", 0, 0, 78.740, 6), true},
		{"100mil 排针（整 mil ⇒ 英制）", tidyTestIC("J2", 0, 0, 100, 8), false},
		{"50mil 排针", tidyTestIC("J3", 0, 0, 50, 8), false},
		{"两脚 0402（脚数不足，仍该吸栅）", tidyTestComp("C1", 0, 0, 0), false},
		{"3 脚 SOT-23（脚数不足）", tidyTestIC("Q1", 0, 0, 37.4, 3), false},
	}
	for _, c := range cases {
		if got := tidyMetricPitchPart(c.comp); got != c.metric {
			t.Errorf("%s: tidyMetricPitchPart = %v, want %v", c.name, got, c.metric)
		}
	}
}

// ---------------------------------------------------------------------------
// ② rotation-inconsistent
// ---------------------------------------------------------------------------

// #153 实测：U 组 n=10 出现 {0,90,180,270} 四种朝向；C 组 {0:7, 90:12} 只有两种，
// 按 §9.3「默认 0/90 两态」是合法的，**不该报**。
func TestTidy_RotationInconsistent(t *testing.T) {
	// 位置两轴都错开 >2mil，避免顺带触发阵列规则，让断言只反映朝向。
	snap := &boardSnapshot{Components: []boardComp{
		tidyTestComp("U1", 100, 100, 0),
		tidyTestComp("U2", 300, 250, 0),
		tidyTestComp("U3", 500, 400, 90),
		tidyTestComp("U4", 700, 550, 180),
		tidyTestComp("U5", 900, 700, 270),
		tidyTestComp("U6", 1100, 850, 270),
	}}
	d := tidyScoreOf(snap)

	if got := tidyMetric(t, d, "rotationKinds"); got != 4 {
		t.Errorf("rotationKinds = %v, want 4", got)
	}
	// 多数派两态 = {0(2), 270(2)}；异类 = U3(90) + U4(180)。
	if got := tidyMetric(t, d, "rotationOutliers"); got != 2 {
		t.Errorf("rotationOutliers = %v, want 2", got)
	}
	if got := tidyMetric(t, d, "rotationOddCount"); got != 3 {
		t.Errorf("rotationOddCount = %v, want 3（U4/U5/U6 在 180/270）", got)
	}
	// U4 同时是「不在多数派」+「180°」，扣分必须高于只占一条的 U3。
	u4, ok4 := tidyContributor(d, "U4")
	u3, ok3 := tidyContributor(d, "U3")
	if !ok3 || !ok4 {
		t.Fatalf("U3/U4 都必须进归因：%+v", d.Contributors)
	}
	if u4.Penalty <= u3.Penalty {
		t.Errorf("U4(%.2f) 犯两条，扣分该高于 U3(%.2f)", u4.Penalty, u3.Penalty)
	}
	var warn, info int
	for _, f := range d.Findings {
		if f.Type != tidyRuleRotation {
			continue
		}
		switch f.Level {
		case "WARN":
			warn++
		case "INFO":
			info++
		default:
			t.Errorf("齐整度是 cosmetic，不该出现 %s：%+v", f.Level, f)
		}
	}
	if warn != 1 || info != 1 {
		t.Errorf("rotation findings = %d WARN / %d INFO, want 1/1", warn, info)
	}
}

// 只有两种朝向（§9.3 的 0/90 默认两态）不报。
func TestTidy_RotationTwoStatesIsFine(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		tidyTestComp("C1", 100, 100, 0),
		tidyTestComp("C2", 300, 250, 90),
		tidyTestComp("C3", 500, 400, 0),
		tidyTestComp("C4", 700, 550, 90),
	}}
	d := tidyScoreOf(snap)
	if got := tidyMetric(t, d, "rotationOutliers"); got != 0 {
		t.Errorf("0/90 两态不该判异类，rotationOutliers = %v", got)
	}
	if got := tidyMetric(t, d, "scoreRotationInconsistent"); got != 100 {
		t.Errorf("scoreRotationInconsistent = %v, want 100", got)
	}
}

// #153 的 H 组 {90:2, 270:2} 是假阳性：安装孔旋转对称，rotation 不可观测。
// 判据是机械的 —— 焊盘数 ≤1 的件不参与朝向统计。
func TestTidy_RotationIgnoresSinglePadParts(t *testing.T) {
	hole := func(des string, x, y, rot float64) boardComp {
		c := tidyTestComp(des, x, y, rot)
		c.Pads = c.Pads[:1]
		return c
	}
	snap := &boardSnapshot{Components: []boardComp{
		hole("SW1", 100, 100, 90), hole("SW2", 300, 250, 270),
		hole("SW3", 500, 400, 180), hole("SW4", 700, 550, 0),
	}}
	d := tidyScoreOf(snap)
	if _, ok := d.Metrics["rotationPopulation"]; ok {
		t.Errorf("全是 ≤1 焊盘件时朝向规则该退出加权，实际给了 metrics: %v", d.Metrics)
	}
	if d.Reason == "" {
		t.Error("退出加权的子规则必须写进 Reason")
	}
}

func TestTidyNormalizeRot(t *testing.T) {
	cases := map[float64]float64{
		0: 0, 90: 90, 180: 180, 270: 270,
		360: 0, -90: 270, 450: 90,
		90.5: 90, // 浮点噪声吸到 90° 步进
		45:   45, // 自由角自成一类（§9.3 硬禁，靠「>2 种朝向」抓）
	}
	for in, want := range cases {
		if got := tidyNormalizeRot(in); got != want {
			t.Errorf("tidyNormalizeRot(%g) = %g, want %g", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// ③ silk-side-inconsistent
// ---------------------------------------------------------------------------

// #153 实测：位号方位左 20 / 上 21 / 右 10 / 下 18，四面开花。
func TestTidy_SilkSideInconsistent(t *testing.T) {
	comps := []boardComp{
		tidyTestComp("R1", 100, 100, 0),
		tidyTestComp("R2", 300, 250, 0),
		tidyTestComp("R3", 500, 400, 0),
		tidyTestComp("R4", 700, 550, 0),
	}
	snap := &boardSnapshot{Components: comps, Silk: []pcbSilkText{
		tidyTestSilk("id-R1", "R1", 100, 150, 32), // 上
		tidyTestSilk("id-R2", "R2", 300, 300, 32), // 上
		tidyTestSilk("id-R3", "R3", 500, 450, 32), // 上
		tidyTestSilk("id-R4", "R4", 650, 550, 32), // 左 —— 异类
	}}
	d := tidyScoreOf(snap)

	if got := tidyMetric(t, d, "silkSideMajorityRatio"); math.Abs(got-0.75) > 1e-9 {
		t.Errorf("silkSideMajorityRatio = %v, want 0.75", got)
	}
	if got := tidyMetric(t, d, "silkSideTop"); got != 3 {
		t.Errorf("silkSideTop = %v, want 3", got)
	}
	if got := tidyMetric(t, d, "silkSideLeft"); got != 1 {
		t.Errorf("silkSideLeft = %v, want 1", got)
	}
	if _, ok := tidyContributor(d, "R4"); !ok {
		t.Fatalf("方位异类 R4 必须进归因：%+v", d.Contributors)
	}
	// #153 实测结论：silk-align --side 是 soft hint，顶不了一致性；而且它自己会
	// 造 silk-over-pad 回归。建议文案必须把这两件事说清楚，否则下游会照着跑一遍
	// 然后以为修好了。
	var msg string
	for _, f := range d.Findings {
		if f.Type == tidyRuleSilkSide {
			msg = f.Message
		}
	}
	if msg == "" {
		t.Fatal("silk-side-inconsistent 必须出一条 finding")
	}
	for _, want := range []string{"soft hint", "pcb check"} {
		if !strings.Contains(msg, want) {
			t.Errorf("finding 必须提到 %q，实际：%s", want, msg)
		}
	}
}

// 方位判定：y-UP，+y 是 top；取偏移绝对值大的那个轴。
func TestTidySilkSideOf(t *testing.T) {
	cases := []struct {
		sx, sy float64
		want   apEdge
	}{
		{0, 50, edgeTop}, {0, -50, edgeBottom},
		{-50, 0, edgeLeft}, {50, 0, edgeRight},
		{60, 20, edgeRight}, // |dx|>|dy| → 横向赢
	}
	for _, c := range cases {
		got, ok := tidySilkSideOf(0, 0, c.sx, c.sy)
		if !ok || got != c.want {
			t.Errorf("tidySilkSideOf(0,0,%g,%g) = %v/%v, want %v", c.sx, c.sy, got, ok, c.want)
		}
	}
	// 丝印压在本体正中：判不出方位，不该硬塞一个进多数派统计。
	if _, ok := tidySilkSideOf(0, 0, 0.2, 0.2); ok {
		t.Error("死区内必须返回 ok=false")
	}
}

// ---------------------------------------------------------------------------
// ④ silk-style-inconsistent
// ---------------------------------------------------------------------------

// #153 实测：Designator 的 fontSize 分布 {32:68, 45:1} —— 一个漏网件。
func TestTidy_SilkStyleInconsistent(t *testing.T) {
	comps := []boardComp{
		tidyTestComp("R1", 100, 100, 0),
		tidyTestComp("R2", 300, 250, 0),
		tidyTestComp("R3", 500, 400, 0),
		tidyTestComp("R4", 700, 550, 0),
	}
	snap := &boardSnapshot{Components: comps, Silk: []pcbSilkText{
		tidyTestSilk("id-R1", "R1", 100, 150, 32),
		tidyTestSilk("id-R2", "R2", 300, 300, 32),
		tidyTestSilk("id-R3", "R3", 500, 450, 32),
		tidyTestSilk("id-R4", "R4", 700, 600, 45), // 漏网的 45mil
	}}
	d := tidyScoreOf(snap)

	if got := tidyMetric(t, d, "fontSizeOutliers"); got != 1 {
		t.Errorf("fontSizeOutliers = %v, want 1", got)
	}
	if got := tidyMetric(t, d, "scoreSilkStyleInconsistent"); got != 75 {
		t.Errorf("scoreSilkStyleInconsistent = %v, want 75（4 条里 1 条离群）", got)
	}
	if _, ok := tidyContributor(d, "R4"); !ok {
		t.Fatalf("字号离群的 R4 必须进归因：%+v", d.Contributors)
	}
	// 「报离群的那几个 id」是 #153 的原话。
	var prims []string
	for _, f := range d.Findings {
		if f.Type == tidyRuleSilkStyle {
			prims = f.Primitives
		}
	}
	if len(prims) != 1 || prims[0] != "silk-R4" {
		t.Errorf("finding.Primitives = %v, want [silk-R4]", prims)
	}
}

// 连接器没返回 fontSize（老版本）时不能把 0 当成「0mil 字号」去比 —— 这条子规则
// 必须退出加权。
func TestTidy_SilkStyleSkipsWhenFontUnknown(t *testing.T) {
	comps := []boardComp{
		tidyTestComp("R1", 100, 100, 0),
		tidyTestComp("R2", 300, 250, 0),
		tidyTestComp("R3", 500, 400, 0),
	}
	silk := []pcbSilkText{
		tidyTestSilk("id-R1", "R1", 100, 150, 0),
		tidyTestSilk("id-R2", "R2", 300, 300, 0),
		tidyTestSilk("id-R3", "R3", 500, 450, 0),
	}
	d := tidyScoreOf(&boardSnapshot{Components: comps, Silk: silk})
	if _, ok := d.Metrics["fontSizeOutliers"]; ok {
		t.Errorf("字号未知时不该产出 fontSizeOutliers：%v", d.Metrics)
	}
	if !strings.Contains(d.Reason, tidyRuleSilkStyle) {
		t.Errorf("Reason 必须点名退出加权的子规则，实际：%s", d.Reason)
	}
}

// ---------------------------------------------------------------------------
// ⑤ array-irregular
// ---------------------------------------------------------------------------

// #153 的 199.1 / 200 / 205.2 —— 「差一点」正是丑源。判定用「中位步距 + 中位截距」
// 的残差，所以只有真正错位的那一件被点名，它后面的件不会被连累。
func TestTidy_ArrayIrregular(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		tidyTestComp("C1", 100, 500, 0),
		tidyTestComp("C2", 200, 500, 0),
		tidyTestComp("C3", 310, 500, 0), // 该在 300，差 10mil
		tidyTestComp("C4", 400, 500, 0),
		tidyTestComp("C5", 500, 500, 0),
	}}
	d := tidyScoreOf(snap)

	if got := tidyMetric(t, d, "arrayRuns"); got != 1 {
		t.Errorf("arrayRuns = %v, want 1", got)
	}
	if got := tidyMetric(t, d, "arrayMembers"); got != 5 {
		t.Errorf("arrayMembers = %v, want 5", got)
	}
	if got := tidyMetric(t, d, "arrayIrregularCount"); got != 1 {
		t.Fatalf("arrayIrregularCount = %v, want 1（只有 C3 错位）", got)
	}
	if _, ok := tidyContributor(d, "C3"); !ok {
		t.Fatalf("C3 必须进归因：%+v", d.Contributors)
	}
	for _, des := range []string{"C4", "C5"} {
		if _, ok := tidyContributor(d, des); ok {
			t.Errorf("%s 位置正确，不该被 C3 的错位连累：%+v", des, d.Contributors)
		}
	}
}

// 规则阵列（去耦排：等距 100mil）不报 —— #153 的 85/85/85 那组就是这种。
func TestTidy_ArrayRegularIsClean(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		tidyTestComp("C1", 100, 500, 0),
		tidyTestComp("C2", 185, 500, 0),
		tidyTestComp("C3", 270, 500, 0),
		tidyTestComp("C4", 355, 500, 0),
	}}
	d := tidyScoreOf(snap)
	if got := tidyMetric(t, d, "arrayIrregularCount"); got != 0 {
		t.Errorf("等距 85mil 阵列不该报，arrayIrregularCount = %v", got)
	}
	if got := tidyMetric(t, d, "scoreArrayIrregular"); got != 100 {
		t.Errorf("scoreArrayIrregular = %v, want 100", got)
	}
}

// 两簇碰巧共线的件不是一个阵列：中间隔着 >500mil 时必须切段，否则那段巨大间距会
// 被算进步距统计，把两簇里的件全判成错位。
func TestTidy_ArraySplitsOnBigGap(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		tidyTestComp("C1", 100, 500, 0),
		tidyTestComp("C2", 200, 500, 0),
		tidyTestComp("C3", 300, 500, 0),
		tidyTestComp("C4", 2000, 500, 0), // 隔了 1700mil
	}}
	d := tidyScoreOf(snap)
	if got := tidyMetric(t, d, "arrayMembers"); got != 3 {
		t.Errorf("arrayMembers = %v, want 3（C4 被切出去）", got)
	}
	if got := tidyMetric(t, d, "arrayIrregularCount"); got != 0 {
		t.Errorf("前三件是规则阵列，不该报：%v", got)
	}
}

// ---------------------------------------------------------------------------
// 维度级契约
// ---------------------------------------------------------------------------

// 硬约定 ①：输入不足必须 skipped，绝不返回 100。
func TestTidy_SkipsOnEmptyBoard(t *testing.T) {
	d := tidyScoreOf(&boardSnapshot{})
	if d.Status != dimSkipped {
		t.Fatalf("空板必须 skipped，实际 %s / %v 分", d.Status, d.Score)
	}
	if d.Reason == "" {
		t.Error("skipped 必须给原因")
	}
	if d.Score == 100 {
		t.Error("「没测」不能等于「满分」")
	}
}

// 一块器件太少、没有丝印的板：三条子规则无样本，剩下的仍要算，但状态必须 degraded
// 且说清楚哪几条没测 —— 否则「齐整度 100 分」读起来像全面体检。
func TestTidy_DegradedWithoutSilk(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		tidyTestComp("C1", 635.0015, 1109.998, 0),
		tidyTestComp("C2", 635, 1210, 0),
		tidyTestComp("C3", 700, 1310, 0),
	}}
	d := tidyScoreOf(snap)
	if d.Status != dimDegraded {
		t.Fatalf("丝印缺失必须标 degraded，实际 %s", d.Status)
	}
	if !strings.Contains(d.Reason, "丝印") {
		t.Errorf("Reason 必须说明丝印缺失，实际：%s", d.Reason)
	}
	if _, ok := d.Metrics["silkSideMajorityRatio"]; ok {
		t.Error("没测到的量不能编一个 0 出来（会被读成「0% 一致」）")
	}
	if d.Weight != defaultDimensionWeights[dimTidy] {
		t.Errorf("degraded 仍要参与加权，Weight = %v", d.Weight)
	}
}

// 全维度硬约定 ③：Σ contributor.Penalty == 100 − Score。计数与判定同源，否则报告里
// 「归因加起来对不上分数」，判读的人会以为算错了。
func TestTidy_ContributorsAccountForTheWholePenalty(t *testing.T) {
	comps := []boardComp{
		tidyTestComp("R1", 100, 100, 0),
		tidyTestComp("R2", 300, 250, 0),
		tidyTestComp("R3", 500, 400, 90),
		tidyTestComp("R4", 700.5, 550, 180), // 不落格 + 朝向异类 + 180° + 位号异侧 + 字号离群
	}
	snap := &boardSnapshot{Components: comps, Silk: []pcbSilkText{
		tidyTestSilk("id-R1", "R1", 100, 150, 32),
		tidyTestSilk("id-R2", "R2", 300, 300, 32),
		tidyTestSilk("id-R3", "R3", 500, 450, 32),
		tidyTestSilk("id-R4", "R4", 650.5, 550, 45),
	}}
	d := tidyScoreOf(snap)

	var sum float64
	for _, c := range d.Contributors {
		sum += c.Penalty
	}
	if diff := math.Abs(sum - (100 - d.Score)); diff > 0.2 {
		t.Errorf("Σpenalty = %.2f, 100−Score = %.2f（差 %.2f，超出取整容差）", sum, 100-d.Score, diff)
	}
	if len(d.Contributors) == 0 {
		t.Fatal("四条子规则都被违反，Contributors 不能为空")
	}
	// 按 Penalty 降序：精修环取前几个就是该先动的件。
	for i := 1; i < len(d.Contributors); i++ {
		if d.Contributors[i-1].Penalty < d.Contributors[i].Penalty {
			t.Fatalf("Contributors 未按 Penalty 降序：%+v", d.Contributors)
		}
	}
	if d.Contributors[0].Designator != "R4" {
		t.Errorf("犯了四条的 R4 该排第一，实际 %+v", d.Contributors)
	}
	if d.Contributors[0].Detail == "" {
		t.Error("归因必须带 Detail，只给分数无法定位问题")
	}
}

// #167 第五层的校准判据：一块真整齐的板必须拿高分，而且状态是 scored（不是靠
// 「什么都没测」拿的满分）。
func TestTidy_TidyBoardScoresFull(t *testing.T) {
	comps := []boardComp{
		tidyTestComp("R1", 100, 500, 0),
		tidyTestComp("R2", 200, 500, 0),
		tidyTestComp("R3", 300, 500, 0),
		tidyTestComp("R4", 400, 500, 0),
	}
	snap := &boardSnapshot{Components: comps, Silk: []pcbSilkText{
		tidyTestSilk("id-R1", "R1", 100, 550, 32),
		tidyTestSilk("id-R2", "R2", 200, 550, 32),
		tidyTestSilk("id-R3", "R3", 300, 550, 32),
		tidyTestSilk("id-R4", "R4", 400, 550, 32),
	}}
	d := tidyScoreOf(snap)

	if d.Status != dimScored {
		t.Fatalf("五条子规则都有样本，状态该是 scored，实际 %s（Reason: %s）", d.Status, d.Reason)
	}
	if d.Score != 100 {
		t.Errorf("整齐的板该拿满分，实际 %v（findings: %+v）", d.Score, d.Findings)
	}
	if len(d.Contributors) != 0 || len(d.Findings) != 0 {
		t.Errorf("干净板不该有归因/findings：%+v / %+v", d.Contributors, d.Findings)
	}
	// 五条子分全在，证明满分来自「测了都过」而不是「没测」。
	for _, k := range []string{
		"scoreOffGrid", "scoreRotationInconsistent", "scoreSilkSideInconsistent",
		"scoreSilkStyleInconsistent", "scoreArrayIrregular",
	} {
		if v := tidyMetric(t, d, k); v != 100 {
			t.Errorf("%s = %v, want 100", k, v)
		}
	}
}

// 齐整度纯 cosmetic：所有 finding 只能是 WARN/INFO，绝不能阻塞一块电气正确的板。
func TestTidy_FindingsAreNeverErrors(t *testing.T) {
	comps := []boardComp{
		tidyTestComp("R1", 100.3, 100, 0),
		tidyTestComp("R2", 300, 250, 90),
		tidyTestComp("R3", 500, 400, 180),
		tidyTestComp("R4", 700, 550, 45),
	}
	snap := &boardSnapshot{Components: comps, Silk: []pcbSilkText{
		tidyTestSilk("id-R1", "R1", 100, 150, 32),
		tidyTestSilk("id-R2", "R2", 250, 250, 32),
		tidyTestSilk("id-R3", "R3", 500, 350, 45),
		tidyTestSilk("id-R4", "R4", 750, 550, 30),
	}}
	d := tidyScoreOf(snap)
	if len(d.Findings) == 0 {
		t.Fatal("这块板一塌糊涂，必须出 findings")
	}
	for _, f := range d.Findings {
		if f.Level != "WARN" && f.Level != "INFO" {
			t.Errorf("齐整度 finding 不该是 %s：%+v", f.Level, f)
		}
	}
}

// --grid 可覆盖：25mil 是 conventions §9.1 的目标栅，用它当判据时几乎没有件落格 ——
// 这正是默认选 5mil 的理由，也是这条覆盖开关存在的理由。
func TestTidy_GridOptionOverridesDefault(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		tidyTestComp("C1", 105, 500, 0),
		tidyTestComp("C2", 210, 700, 0),
		tidyTestComp("C3", 315, 900, 0),
	}}
	def := scoreTidy(&scoreCtx{snap: snap, opts: layoutScoreOpts{}})
	if got := tidyMetric(t, def, "onGridRatio"); got != 1 {
		t.Errorf("5mil 栅上三件全落格，onGridRatio = %v", got)
	}
	coarse := scoreTidy(&scoreCtx{snap: snap, opts: layoutScoreOpts{gridMil: 25}})
	if got := tidyMetric(t, coarse, "onGridRatio"); got != 0 {
		t.Errorf("25mil 栅上一件也不落格，onGridRatio = %v", got)
	}
	if got := tidyMetric(t, coarse, "gridMil"); got != 25 {
		t.Errorf("gridMil 必须回显实际用的栅距，得到 %v", got)
	}
}

// 维度必须自注册进打分表，否则 layout-score 会把它报成 "not implemented yet"。
func TestTidy_RegisteredAndWiredIntoReport(t *testing.T) {
	if scorerFor(dimTidy) == nil {
		t.Fatal("tidyScorer 未注册")
	}
	snap := &boardSnapshot{Components: []boardComp{
		tidyTestComp("C1", 635.0015, 500, 0),
		tidyTestComp("C2", 300, 700, 0),
		tidyTestComp("C3", 500, 900, 0),
	}}
	rep := analyzeLayoutScore(snap, nil, layoutScoreOpts{only: map[string]bool{dimTidy: true}})
	d := rep.dimension(dimTidy)
	if d == nil {
		t.Fatal("报告里没有 tidy 维")
	}
	if strings.Contains(d.Reason, "not implemented") {
		t.Errorf("tidy 维仍被当成未实现：%+v", d)
	}
	if rep.ScoredDims != 1 || rep.Overall != d.Score {
		t.Errorf("只算一维时综合分该等于该维分数：Overall=%v, tidy=%v, scored=%d",
			rep.Overall, d.Score, rep.ScoredDims)
	}
}
