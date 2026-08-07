package app

// pcb_score_protection_test.go — dimProtection 的离线单测（纯结构体字面量喂纯函数，
// 不连 daemon）。范式抄 pcb_check_dfm2_test.go。
//
// 重点钉三件事：
//  1. **分类不能自指**：USBLC6 型号里带 "USB"，它必须是保护件而不是端子（否则拿自己当
//     参照物、距离恒 0、永远满分）。
//  2. **量表与判据一致**：measureDecaps 里超预算的位号集合，必须与 findDecapTooFar 报
//     出来的完全相同——这是「计数与判定分离处必查一致性」那条教训的机械化。
//  3. **归因可加**：Σ contributor.Penalty == 100 − Score 是恒等式，精修环靠它预测
//     「动掉谁能涨多少分」。

import (
	"math"
	"strings"
	"testing"
)

// ── fixture 助手 ────────────────────────────────────────────────────────────

func protPad(num, net string, x, y float64) boardPad {
	return boardPad{Number: num, Net: net, Layer: pcbSideTop, X: x, Y: y}
}

func protComp(des, device string, pads ...boardPad) boardComp {
	return boardComp{ID: "p-" + des, Designator: des, Device: device, Layer: pcbSideTop, Pads: pads}
}

// protBoard 是本测试文件的基准板，几何刻意取整数好手算：
//
//	J1 Type-C     VBUS @(0,0)          —— 端子（参照物）
//	F1 自恢复保险丝 VBUS @(100,0)        —— 距端子 100mil ≤250 预算，合格
//	D1 TVS(SMAJ)  VBUS @(400,0)        —— 距端子 400mil，超标 → sev=0.4+0.6×0.6=0.76
//	U1 主控        3V3  @(1000,0)       —— 去耦的被服务对象
//	C1 去耦        3V3  @(1040,0)       —— 距 IC 40mil ≤100 预算，合格
//	C2 去耦        3V3  @(1400,0)       —— 距 IC 400mil，超标 → sev 封顶 1.0
//
// 两族都在场 → 族权重各 0.5：
//
//	D1 扣 0.5×100×0.76/2 = 19.0     C2 扣 0.5×100×1.0/2 = 25.0     总分 100−44 = 56.0
func protBoard() *boardSnapshot {
	return &boardSnapshot{Components: []boardComp{
		protComp("J1", "TYPE-C-31-M-12", protPad("A4", "VBUS", 0, 0), protPad("A1", "GND", 0, 50)),
		protComp("F1", "MF-MSMF050-2500MA", protPad("1", "VBUS", 100, 0), protPad("2", "VSYS", 140, 0)),
		protComp("D1", "SMAJ5.0A", protPad("1", "VBUS", 400, 0), protPad("2", "GND", 440, 0)),
		protComp("U1", "ESP32-S3-WROOM-1", protPad("1", "3V3", 1000, 0), protPad("2", "GND", 1000, 50)),
		protComp("C1", "0402-100nF", protPad("1", "3V3", 1040, 0), protPad("2", "GND", 1080, 0)),
		protComp("C2", "0402-100nF", protPad("1", "3V3", 1400, 0), protPad("2", "GND", 1440, 0)),
	}}
}

func protScore(snap *boardSnapshot) scoreDimension {
	return protectionScorer{}.score(&scoreCtx{snap: snap, opts: layoutScoreOpts{}})
}

func protContributor(d scoreDimension, des string) (scoreContributor, bool) {
	for _, c := range d.Contributors {
		if c.Designator == des {
			return c, true
		}
	}
	return scoreContributor{}, false
}

func protFindings(d scoreDimension, typ string) []pcbCheckFinding {
	var out []pcbCheckFinding
	for _, f := range d.Findings {
		if f.Type == typ {
			out = append(out, f)
		}
	}
	return out
}

// ── 1. 器件分类 ─────────────────────────────────────────────────────────────

func TestScoreProtectionClassify(t *testing.T) {
	cases := []struct {
		des, device      string
		protection, port bool
	}{
		{"F1", "MF-MSMF050-2500MA", true, false}, // 位号强前缀 + 型号双命中
		{"FU2", "CFS12V6T2R0", true, false},      // FU 前缀
		{"RV1", "MYG05K271", true, false},        // 压敏
		{"D1", "SMAJ5.0A", true, false},          // D* 靠型号才算保护件
		{"D2", "1N4148WS", false, false},         // 普通开关二极管：不是保护件
		{"D3", "KT-0603R", false, false},         // LED：位号也是 D*，绝不能误判
		{"ESD1", "", true, false},                // 位号强前缀，型号缺失也认
		{"J1", "TYPE-C-31-M-12", false, true},    // 端子
		{"CN2", "", false, true},                 // 端子（位号）
		{"DC1", "DC-005", false, true},           // DC 电源座
		{"U9", "HEADER-2P-2.54", false, true},    // 位号不守约定，靠型号兜住
		{"U1", "ESP32-S3-WROOM-1", false, false}, // 主控：两边都不是
		{"C1", "0402-100nF", false, false},       // 电容：两边都不是
		{"FB1", "GZ2012D601TF", false, false},    // 磁珠不是保护件（EMI 器件，判据不同）
		{"R7", "0402-10K", false, false},         // 电阻
	}
	for _, c := range cases {
		comp := protComp(c.des, c.device)
		if got := isProtectionPart(comp); got != c.protection {
			t.Errorf("isProtectionPart(%s %q) = %v, want %v", c.des, c.device, got, c.protection)
		}
		if got := isPortPart(comp); got != c.port {
			t.Errorf("isPortPart(%s %q) = %v, want %v", c.des, c.device, got, c.port)
		}
	}
}

// USBLC6 的型号里带 "USB"。如果先判端子后判保护件，它会把自己认成端子，然后自己到自己
// 的距离恒为 0 —— 这一维就永远满分。这条测试钉住「先排保护件」的顺序。
func TestScoreProtectionClassifyUSBLCNotAPort(t *testing.T) {
	u := protComp("U5", "USBLC6-2SC6")
	if !isProtectionPart(u) {
		t.Fatalf("USBLC6 must classify as a protection part")
	}
	if isPortPart(u) {
		t.Fatalf("USBLC6 must NOT classify as a port — it would become its own reference and score 100 forever")
	}
}

// ── 2. 测距 ────────────────────────────────────────────────────────────────

func TestScoreProtectionMeasure(t *testing.T) {
	hits := measureProtection(protBoard())
	if len(hits) != 2 {
		t.Fatalf("measureProtection = %d hits, want 2 (F1,D1): %+v", len(hits), hits)
	}
	byDes := map[string]protectHit{}
	for _, h := range hits {
		byDes[h.Designator] = h
	}
	if h := byDes["F1"]; !h.Matched || h.Dist != 100 || h.PortRef != "J1.A4" || h.Net != "VBUS" {
		t.Errorf("F1 = %+v, want matched J1.A4 on VBUS at 100mil", h)
	}
	if h := byDes["D1"]; !h.Matched || h.Dist != 400 {
		t.Errorf("D1 = %+v, want matched at 400mil", h)
	}
}

// GND 把全板连成一片：一个只有地网可配的保护件必须判为「测不了」，而不是被配到板另一头
// 的某个端子上凑一个毫无物理含义的距离。
func TestScoreProtectionMeasureIgnoresGnd(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		protComp("J1", "TYPE-C-31-M-12", protPad("A1", "GND", 0, 0)),
		protComp("D1", "PESD5V0L4UG", protPad("1", "SENSE_CLAMP", 5000, 0), protPad("2", "GND", 5040, 0)),
	}}
	hits := measureProtection(snap)
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	if hits[0].Matched {
		t.Fatalf("D1 shares only GND with J1 — must be unmatched, got %+v", hits[0])
	}
	if hits[0].Dist != 0 {
		t.Errorf("unmatched hit must carry Dist 0 (not +Inf, which would poison the metrics max), got %v", hits[0].Dist)
	}
}

// 同网端子有多个时取最近的 —— 最近的那个就是它实际保护的对象。
func TestScoreProtectionMeasureNearestPort(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		protComp("J1", "TYPE-C-31-M-12", protPad("A4", "VBUS", 0, 0)),
		protComp("J2", "HEADER-2P-2.54", protPad("1", "VBUS", 900, 0)),
		protComp("F1", "MF-MSMF050-2500MA", protPad("1", "VBUS", 800, 0), protPad("2", "VSYS", 840, 0)),
	}}
	hits := measureProtection(snap)
	if len(hits) != 1 || hits[0].PortRef != "J2.1" || hits[0].Dist != 100 {
		t.Fatalf("F1 must measure against the NEAREST same-net port (J2.1 @100mil), got %+v", hits)
	}
}

// ── 3. 量表 vs 判据一致性 ───────────────────────────────────────────────────

// findDecapTooFar 是「谁超标」的唯一判据，measureDecaps 只提供分母和距离。两条路径分叉
// 会让报告出现「报了却不扣分」或「扣分却没报」——这个项目在聚合命令上踩过同一类坑
// （0 个阻塞项却 FAIL）。这里用一块含全部豁免分支的板把两边钉在一起。
func TestScoreProtectionDecapAgreesWithFindDecapTooFar(t *testing.T) {
	pads := []pcbPadP{
		// U1: 3V3 轨上的 IC（去耦的服务对象）。
		{Designator: "U1", Number: "1", Net: "3V3", Layer: pcbSideTop, X: 0, Y: 0},
		{Designator: "U1", Number: "2", Net: "GND", Layer: pcbSideTop, X: 0, Y: 50},
		// C1 合格 / C2 超标。
		{Designator: "C1", Number: "1", Net: "3V3", Layer: pcbSideTop, X: 40, Y: 0},
		{Designator: "C1", Number: "2", Net: "GND", Layer: pcbSideTop, X: 80, Y: 0},
		{Designator: "C2", Number: "1", Net: "3V3", Layer: pcbSideTop, X: 400, Y: 0},
		{Designator: "C2", Number: "2", Net: "GND", Layer: pcbSideTop, X: 440, Y: 0},
		// C3: VIN 轨上没有任何 IC 引脚 → bulk/输入电容，两边都必须豁免。
		{Designator: "C3", Number: "1", Net: "VIN", Layer: pcbSideTop, X: 3000, Y: 0},
		{Designator: "C3", Number: "2", Net: "GND", Layer: pcbSideTop, X: 3040, Y: 0},
		// C4: 三脚（可调/带屏蔽） → 不是简单两脚去耦，两边都必须豁免。
		{Designator: "C4", Number: "1", Net: "3V3", Layer: pcbSideTop, X: 3000, Y: 300},
		{Designator: "C4", Number: "2", Net: "GND", Layer: pcbSideTop, X: 3040, Y: 300},
		{Designator: "C4", Number: "3", Net: "GND", Layer: pcbSideTop, X: 3080, Y: 300},
		// C5: 两脚都是信号网（耦合电容） → 不是去耦，两边都必须豁免。
		{Designator: "C5", Number: "1", Net: "AUDIO_L", Layer: pcbSideTop, X: 3000, Y: 600},
		{Designator: "C5", Number: "2", Net: "AUDIO_R", Layer: pcbSideTop, X: 3040, Y: 600},
	}

	over := map[string]bool{}
	for _, h := range measureDecaps(pads) {
		if h.Dist > pcbDecapMaxMil {
			over[h.Designator] = true
		}
	}
	reported := map[string]bool{}
	for _, f := range findDecapTooFar(pads) {
		reported[f.Designator] = true
	}
	if len(over) != len(reported) {
		t.Fatalf("measure says %v over budget, findDecapTooFar reports %v — the two paths diverged", over, reported)
	}
	for d := range reported {
		if !over[d] {
			t.Errorf("findDecapTooFar reports %s but measureDecaps says it is within budget", d)
		}
	}
	if !reported["C2"] {
		t.Fatalf("fixture is broken: C2 must be the (only) offender, got %v", reported)
	}
}

// ── 4. 打分 ────────────────────────────────────────────────────────────────

func TestScoreProtectionScoresBothFamilies(t *testing.T) {
	d := protScore(protBoard())
	if d.ID != dimProtection {
		t.Fatalf("id = %q", d.ID)
	}
	if d.Status != dimScored {
		t.Fatalf("status = %q (reason %q), want scored — both families are fully covered", d.Status, d.Reason)
	}
	// 两族等权，各自的越界者按族内候选数摊分（手算见 protBoard 的注释）。
	if got, want := d.Score, 56.0; math.Abs(got-want) > 0.05 {
		t.Errorf("score = %v, want %v", got, want)
	}
	if c, ok := protContributor(d, "D1"); !ok || math.Abs(c.Penalty-19.0) > 0.05 {
		t.Errorf("D1 contributor = %+v, want penalty 19.0", c)
	}
	if c, ok := protContributor(d, "C2"); !ok || math.Abs(c.Penalty-25.0) > 0.05 {
		t.Errorf("C2 contributor = %+v, want penalty 25.0", c)
	}
	// 合格件不该出现在归因里 —— 精修环取前 N 个就动，混进合格件等于瞎动。
	if _, ok := protContributor(d, "F1"); ok {
		t.Errorf("F1 is within budget and must not be a contributor")
	}
	if _, ok := protContributor(d, "C1"); ok {
		t.Errorf("C1 is within budget and must not be a contributor")
	}
	// Contributors 必须按 Penalty 降序（#167 的精修环直接吃这个梯度）。
	for i := 1; i < len(d.Contributors); i++ {
		if d.Contributors[i-1].Penalty < d.Contributors[i].Penalty {
			t.Fatalf("contributors not sorted desc: %+v", d.Contributors)
		}
	}
	if got := len(protFindings(d, "protection-too-far")); got != 1 {
		t.Errorf("protection-too-far findings = %d, want 1", got)
	}
	if got := len(protFindings(d, "decap-too-far")); got != 1 {
		t.Errorf("decap-too-far findings = %d, want 1 (reused from findDecapTooFar)", got)
	}
	for _, k := range []string{"protectionParts", "decapParts", "worstProtectionDistMil", "worstDecapDistMil"} {
		if _, ok := d.Metrics[k]; !ok {
			t.Errorf("metric %q missing — raw quantities are what make the threshold calibratable", k)
		}
	}
	if d.Metrics["worstProtectionDistMil"] != 400 || d.Metrics["worstDecapDistMil"] != 400 {
		t.Errorf("worst distances = %v / %v, want 400 / 400", d.Metrics["worstProtectionDistMil"], d.Metrics["worstDecapDistMil"])
	}
	if d.Metrics["protectionParts"] != 2 || d.Metrics["decapParts"] != 2 {
		t.Errorf("candidate counts = %v / %v, want 2 / 2", d.Metrics["protectionParts"], d.Metrics["decapParts"])
	}
}

// Σ 归因 == 100 − 分数。分数是从归因反推出来的，所以这是恒等式；一旦有人改成「先算分
// 再单独凑归因」，精修环「动掉谁涨多少分」的预测就失效，这条测试会先炸。
func TestScoreProtectionContributorSumInvariant(t *testing.T) {
	for name, snap := range map[string]*boardSnapshot{
		"both-families": protBoard(),
		"decap-only":    protBoardDecapOnly(),
	} {
		d := protScore(snap)
		var sum float64
		for _, c := range d.Contributors {
			sum += c.Penalty
		}
		if math.Abs(100-sum-d.Score) > 0.06 {
			t.Errorf("%s: Σpenalty=%.2f but 100−score=%.2f — attribution must be additive", name, sum, 100-d.Score)
		}
	}
}

// 只有去耦一族在场时它独占全部权重（fw=1），越界者的扣分因此翻倍 —— 否则「板上没有
// 保护件」会白白吃掉一半扣分额度，把一块去耦全放歪的板洗成 75 分。
func protBoardDecapOnly() *boardSnapshot {
	return &boardSnapshot{Components: []boardComp{
		protComp("U1", "ESP32-S3-WROOM-1", protPad("1", "3V3", 1000, 0), protPad("2", "GND", 1000, 50)),
		protComp("C1", "0402-100nF", protPad("1", "3V3", 1040, 0), protPad("2", "GND", 1080, 0)),
		protComp("C2", "0402-100nF", protPad("1", "3V3", 1400, 0), protPad("2", "GND", 1440, 0)),
	}}
}

func TestScoreProtectionSingleFamilyTakesFullWeight(t *testing.T) {
	d := protScore(protBoardDecapOnly())
	if got, want := d.Score, 50.0; math.Abs(got-want) > 0.05 {
		t.Fatalf("score = %v, want %v (1 of 2 decaps over budget at full family weight)", got, want)
	}
	if _, ok := d.Metrics["protectionSubscore"]; ok {
		t.Errorf("protection family is absent — a subscore for it would read as either 'perfect' or 'terrible', both lies")
	}
	if d.Metrics["decapSubscore"] != 50 {
		t.Errorf("decapSubscore = %v, want 50", d.Metrics["decapSubscore"])
	}
}

// ── 5. 「没测」不能伪装成「测了满分」 ───────────────────────────────────────

func TestScoreProtectionSkipsWhenNothingToMeasure(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		protComp("R1", "0402-10K", protPad("1", "NET1", 0, 0), protPad("2", "NET2", 40, 0)),
		protComp("R2", "0402-10K", protPad("1", "NET2", 100, 0), protPad("2", "NET3", 140, 0)),
	}}
	d := protScore(snap)
	if d.Status != dimSkipped {
		t.Fatalf("status = %q, want skipped — a board with neither protection parts nor decaps must NOT collect 100", d.Status)
	}
	if d.Score != 0 {
		t.Errorf("skipped score = %v, want 0", d.Score)
	}
	if d.Reason == "" {
		t.Errorf("skipped dimension must state why")
	}
}

func TestScoreProtectionSkipsEmptyBoard(t *testing.T) {
	if d := protScore(&boardSnapshot{}); d.Status != dimSkipped || d.Reason == "" {
		t.Fatalf("empty board = %+v, want skipped with a reason", d)
	}
}

// 有保护件、但一个都配不上端子（网表还没导进 PCB，或端子位号不守约定）：这是「测不了」，
// 必须 skip 并把原因写清，不能因为「没找到超标的」就给 100。
func TestScoreProtectionSkipsWhenProtectionUnpairable(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		protComp("F1", "MF-MSMF050-2500MA", protPad("1", "", 0, 0), protPad("2", "", 40, 0)),
		protComp("D1", "SMAJ5.0A", protPad("1", "", 500, 0), protPad("2", "", 540, 0)),
	}}
	d := protScore(snap)
	if d.Status != dimSkipped {
		t.Fatalf("status = %q, want skipped (score %v)", d.Status, d.Score)
	}
	if !strings.Contains(d.Reason, "F1") || !strings.Contains(d.Reason, "D1") {
		t.Errorf("reason must name the parts that could not be judged: %q", d.Reason)
	}
}

// 部分保护件判不了 → 分数只反映另一部分 → degraded（仍参与加权，但要说明）。
func TestScoreProtectionDegradedOnUnmatchedPart(t *testing.T) {
	snap := protBoard()
	snap.Components = append(snap.Components,
		protComp("D9", "PESD5V0L4UG", protPad("1", "SENSE_CLAMP", 5000, 0), protPad("2", "GND", 5040, 0)))
	d := protScore(snap)
	if d.Status != dimDegraded {
		t.Fatalf("status = %q, want degraded", d.Status)
	}
	if !strings.Contains(d.Reason, "D9") {
		t.Errorf("reason must name the uncovered part: %q", d.Reason)
	}
	if got := len(protFindings(d, "protection-unmatched")); got != 1 {
		t.Errorf("protection-unmatched findings = %d, want 1", got)
	}
	// 判不了的那件既不进分母也不扣分，所以分数与基准板一致。
	if math.Abs(d.Score-56.0) > 0.05 {
		t.Errorf("score = %v, want 56.0 (an unjudgeable part must not silently change the score)", d.Score)
	}
	if d.Metrics["protectionUnmatched"] != 1 || d.Metrics["protectionJudged"] != 2 {
		t.Errorf("unmatched/judged = %v/%v, want 1/2", d.Metrics["protectionUnmatched"], d.Metrics["protectionJudged"])
	}
}

// 有对外端子却一颗保护件都没有：保护那半边整体没测 → degraded + 一条 INFO，但**不扣分**
// （加不加保护件是电气决策，不是布局问题）。
func TestScoreProtectionDegradedWhenProtectionAbsent(t *testing.T) {
	snap := protBoardDecapOnly()
	snap.Components = append(snap.Components,
		protComp("J1", "TYPE-C-31-M-12", protPad("A4", "VBUS", 0, 0), protPad("A1", "GND", 0, 50)))
	d := protScore(snap)
	if d.Status != dimDegraded {
		t.Fatalf("status = %q, want degraded — the protection half was never measured", d.Status)
	}
	if got := len(protFindings(d, "protection-absent")); got != 1 {
		t.Fatalf("protection-absent findings = %d, want 1", got)
	}
	if math.Abs(d.Score-50.0) > 0.05 {
		t.Errorf("score = %v, want 50.0 — a missing protection part must not add a penalty", d.Score)
	}
}

// 对称的另一半：有 IC 却没有任何去耦候选，去耦那半边同样没测。
func TestScoreProtectionDegradedWhenDecapAbsent(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		protComp("J1", "TYPE-C-31-M-12", protPad("A4", "VBUS", 0, 0), protPad("A1", "GND", 0, 50)),
		protComp("F1", "MF-MSMF050-2500MA", protPad("1", "VBUS", 100, 0), protPad("2", "VSYS", 140, 0)),
		protComp("U1", "ESP32-S3-WROOM-1", protPad("1", "3V3", 1000, 0), protPad("2", "GND", 1000, 50)),
	}}
	d := protScore(snap)
	if d.Status != dimDegraded || !strings.Contains(d.Reason, "decoupling") {
		t.Fatalf("status/reason = %q / %q, want degraded mentioning the unmeasured decoupling half", d.Status, d.Reason)
	}
	if d.Score != 100 {
		t.Errorf("score = %v, want 100 — the measured half is clean; degraded is carried by Status, not by a fake penalty", d.Score)
	}
}

// ── 6. 严重度与等级 ─────────────────────────────────────────────────────────

func TestScoreProtectionNearnessSeverity(t *testing.T) {
	cases := []struct{ dist, budget, want float64 }{
		{0, 250, 0},         // 贴身
		{250, 250, 0},       // 压线不算超
		{250.001, 250, 0.4}, // 刚越线 = 底值，不能约等于 0
		{400, 250, 0.76},    // 0.4 + 0.6×0.6
		{500, 250, 1.0},     // 2× 预算即封顶
		{5000, 250, 1.0},    // 封顶后不再增长
		{400, 0, 0},         // 预算非法 → 不判
	}
	for _, c := range cases {
		// 容差 1e-3：250.001mil 那条本来就比底值高一丝丝（严重度是连续函数），
		// 这里要钉的是「刚越线就已经在底值上」而不是精确到第 7 位。
		if got := nearnessSeverity(c.dist, c.budget); math.Abs(got-c.want) > 1e-3 {
			t.Errorf("nearnessSeverity(%v,%v) = %v, want %v", c.dist, c.budget, got, c.want)
		}
	}
}

// 保护件「本该贴端子」只是常态而非铁律（共模电感后置、π 型第二级都合法），所以本维
// 一条 ERROR 都不能出——出了就会让 layout-score 的门把正确设计拦下来。
func TestScoreProtectionNeverEmitsError(t *testing.T) {
	snap := protBoard()
	snap.Components = append(snap.Components,
		protComp("D9", "PESD5V0L4UG", protPad("1", "SENSE_CLAMP", 5000, 0), protPad("2", "GND", 5040, 0)))
	for _, f := range protScore(snap).Findings {
		if f.Level != "WARN" && f.Level != "INFO" {
			t.Errorf("finding %s has level %s — this dimension may only WARN/INFO: %+v", f.Type, f.Level, f)
		}
	}
}

// finding 必须带规范回指，否则 agent 拿到告警不知道去看哪一节。
func TestScoreProtectionFindingCitesTheManual(t *testing.T) {
	fs := protFindings(protScore(protBoard()), "protection-too-far")
	if len(fs) != 1 {
		t.Fatalf("want 1 protection-too-far finding, got %d", len(fs))
	}
	if !strings.Contains(fs[0].Message, "pcb-design-rules.md") || !strings.Contains(fs[0].Message, "§7.2") {
		t.Errorf("message must cite §7.2 of the manual: %q", fs[0].Message)
	}
	if fs[0].At == nil || fs[0].At.X != 400 {
		t.Errorf("finding must carry the protection pad location, got %+v", fs[0].At)
	}
}

// ── 7. 注册 ────────────────────────────────────────────────────────────────

func TestScoreProtectionIsRegistered(t *testing.T) {
	if scorerFor(dimProtection) == nil {
		t.Fatalf("protection scorer not registered — analyzeLayoutScore would report it as 'not implemented yet'")
	}
}
