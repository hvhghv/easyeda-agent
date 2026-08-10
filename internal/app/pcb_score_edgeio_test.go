package app

// dimEdgeIO（对外接口与板沿）的离线单测。板框统一 4000×2400 mil，y-UP：底边 y=0。
//
// 这一维的测试重点不是"分数等于多少"，而是三条硬约定：
//   - 测不了必须 skipped（Score 0 + Reason），绝不能给 100；
//   - 输入近似必须 degraded 并写清哪里近似；
//   - Contributors 必须能把扣分归到具体器件，且 Σ 归因 == 本维扣掉的总分。

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

func edgeIOScore(snap *boardSnapshot, s *spec.Spec) scoreDimension {
	return edgeIOScorer{}.score(&scoreCtx{snap: snap, spec: s, opts: layoutScoreOpts{}})
}

// 归因梯度的自洽性：扣了多少分，就必须归到器件头上多少分。精修环按 Penalty 排序
// 决定先动谁，这条不成立的话排序就是骗人的。
func assertContribSum(t *testing.T, d scoreDimension) {
	t.Helper()
	sum := 0.0
	for _, c := range d.Contributors {
		sum += c.Penalty
	}
	if want := 100 - d.Score; sum-want > 0.05 || want-sum > 0.05 {
		t.Errorf("contributors sum %.2f but the dimension lost %.2f — attribution must account for the whole penalty (%+v)", sum, want, d.Contributors)
	}
}

// ── skip：测不了就说测不了 ──────────────────────────────────────────────────

// 板框读不到（PCB 不在前台时平台返 null）→ 每条判据的分母都没了，必须 skipped。
// 这是硬约定①最容易破的地方：返回 100 会让一块什么都没测的板拿满分。
func TestEdgeIOScorer_NoOutlineSkips(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 1000, 140, 360, 280, "VBUS", "GND"),
	}}
	d := edgeIOScore(snap, nil)
	if d.Status != dimSkipped {
		t.Fatalf("status = %s, want skipped", d.Status)
	}
	if d.Score != 0 {
		t.Errorf("a skipped dimension must not carry a score; got %v", d.Score)
	}
	if d.Reason == "" {
		t.Error("skipped must explain itself — the report has to answer \"why is this dimension blank\"")
	}
}

// 板上根本没有连接器 → 没有对外口可摆，同样是 skipped 而不是满分。
func TestEdgeIOScorer_NoConnectorSkips(t *testing.T) {
	snap := &boardSnapshot{
		Outline:    testOutline(),
		Components: []boardComp{mkBoardConn("C1", "0402 100nF", 1, 2000, 1200, 40, 20, "3V3", "GND")},
	}
	d := edgeIOScore(snap, nil)
	if d.Status != dimSkipped || d.Score != 0 {
		t.Fatalf("want skipped/0, got %s/%v (%s)", d.Status, d.Score, d.Reason)
	}
}

// 板上只有一个 edge="any" 的天线座：聚边无从谈起、没有内部件、也凑不出一对 ——
// 三个子判据全无主体，skipped。
func TestEdgeIOScorer_LoneAntennaSocketSkips(t *testing.T) {
	snap := &boardSnapshot{
		Outline:    testOutline(),
		Components: []boardComp{mkBoardConn("J9", "IPEX-1 天线座", 1, 200, 1200, 120, 120, "RF_ANT", "GND")},
	}
	d := edgeIOScore(snap, nil)
	if d.Status != dimSkipped {
		t.Fatalf("status = %s, want skipped (%s)", d.Status, d.Reason)
	}
	if !strings.Contains(d.Reason, "edge-agnostic") {
		t.Errorf("reason should name the edge=any role: %s", d.Reason)
	}
}

// ── 好板拿高分（#167 第五层的校准判据）────────────────────────────────────

// 三个对外口全部贴在底边、KF301 开口朝外、间距足够 → 满分。
// 但因为 Type-C 的开口方向块库没声明，这一维只能是 degraded：说"我没法验它的开口"
// 比默默按满分算诚实。
func TestEdgeIOScorer_GroupedPortsScoreFull(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("J1", "KF301-5.0-2P", 1, 500, 120, 400, 200, "VIN", "GND"),
			mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 2000, 140, 360, 280, "VBUS", "GND", "CC1"),
			mkBoardConn("J2", "KF301-5.0-2P", 1, 3500, 120, 400, 200, "VOUT", "GND"),
		},
	}
	d := edgeIOScore(snap, nil)
	if d.Score != 100 {
		t.Fatalf("a clean I/O edge must score 100; got %v (%+v)", d.Score, d.Contributors)
	}
	if d.Status != dimDegraded {
		t.Errorf("status = %s — the Type-C opening direction is unverifiable, that has to show as degraded", d.Status)
	}
	if !strings.Contains(d.Reason, "opening") {
		t.Errorf("reason must name the missing input: %s", d.Reason)
	}
	if got := d.Metrics["edgeConcentration"]; got != 1 {
		t.Errorf("edgeConcentration = %v, want 1 (all three ports on one edge)", got)
	}
	if got := d.Metrics["outlinePolygon"]; got != 1 {
		t.Errorf("outlinePolygon metric = %v, want 1", got)
	}
}

// ── 扣分与归因 ──────────────────────────────────────────────────────────────

// 三种意图违背各扣一档，且都能归到具体器件上：
//
//	J4 停在板中央（对外口压根没贴边）      −20
//	J2 在底边但开口朝板内（KF301 rot180）  −15
//	J3 独自跑到左边（对外口没聚一条边）    −12
func TestEdgeIOScorer_ScatterInwardAndOffEdge(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("J1", "KF301-5.0-2P", 1, 1000, 120, 400, 200, "VIN", "GND"),
			mkBoardConn("J2", "KF301-5.0-2P", 1, 3000, 120, 400, 200, "VOUT", "GND"),
			mkBoardConn("J3", "TYPE-C-31-M-12", 1, 200, 1200, 360, 280, "VBUS", "GND"),
			mkBoardConn("J4", "TYPE-C-31-M-12", 1, 2000, 1200, 360, 280, "VBUS2", "GND"),
		},
	}
	snap.Components[1].Rotation = 180 // KF301 局部开口 -y，转 180° 后朝 +y = 板内
	d := edgeIOScore(snap, nil)
	if want := 100 - 20.0 - 15.0 - 12.0; d.Score != want {
		t.Fatalf("score = %v, want %v (%+v)", d.Score, want, d.Contributors)
	}
	assertContribSum(t, d)
	if len(d.Contributors) != 3 || d.Contributors[0].Designator != "J4" {
		t.Fatalf("contributors must be ranked worst-first (J4 −20): %+v", d.Contributors)
	}
	if d.Contributors[1].Designator != "J2" || d.Contributors[2].Designator != "J3" {
		t.Errorf("ranking = %+v, want J4 > J2 > J3", d.Contributors)
	}
	if got := d.Metrics["openingInward"]; got != 1 {
		t.Errorf("openingInward = %v, want 1", got)
	}
	if got := d.Metrics["offEdge"]; got != 1 {
		t.Errorf("offEdge = %v, want 1", got)
	}
	if got := d.Metrics["strayEdge"]; got != 1 {
		t.Errorf("strayEdge = %v, want 1", got)
	}
	types := map[string]int{}
	for _, f := range d.Findings {
		types[f.Type]++
	}
	for _, want := range []string{"external-port-off-edge", "external-port-scattered", "connector-opening-inward"} {
		if types[want] != 1 {
			t.Errorf("finding %s appeared %d times, want 1 (%v)", want, types[want], types)
		}
	}
}

// 插头打架：一对冲突两边各担一半，Σ 归因仍然等于本维扣掉的总分。
func TestEdgeIOScorer_PlugConflictSplitsPenalty(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 1000, 140, 360, 280, "VBUS", "GND"),
			mkBoardConn("J_VEH", "KF301-5.0-3P", 1, 1512, 160, 600, 320, "VEH_12V", "GND", "ACC"),
		},
	}
	d := edgeIOScore(snap, nil)
	if want := 100 - edgeIOPlugPenalty; d.Score != want {
		t.Fatalf("score = %v, want %v (%+v)", d.Score, want, d.Contributors)
	}
	assertContribSum(t, d)
	if len(d.Contributors) != 2 {
		t.Fatalf("both sides of the pair must be attributed: %+v", d.Contributors)
	}
	for _, c := range d.Contributors {
		if c.Penalty != edgeIOPlugPenalty/2 {
			t.Errorf("%s carries %v, want half the pair penalty", c.Designator, c.Penalty)
		}
	}
	if got := d.Metrics["plugConflicts"]; got != 1 {
		t.Errorf("plugConflicts = %v, want 1", got)
	}
}

// internal-on-edge 的两档置信度必须在**分数**上也分开，不只是 finding 的 Level：
// 启发式误判一个接箱外传感器的 XH 座，不该跟 spec 违背扣一样多。
func TestEdgeIOScorer_InternalOnEdgeSeverityTiers(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("J1", "PH2.0-3P", 1, 1000, 120, 300, 200, "VBATT", "GND", "TS_NTC"),
			mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 2500, 140, 360, 280, "VBUS", "GND"),
		},
	}

	heur := edgeIOScore(snap, nil)
	if want := 100 - edgeIOInternalHeurPenalty; heur.Score != want {
		t.Fatalf("heuristic internal-on-edge score = %v, want %v (%+v)", heur.Score, want, heur.Contributors)
	}
	assertContribSum(t, heur)

	s := &spec.Spec{Interfaces: []spec.Interface{{Name: "backup cell", Ref: "J1", Internal: true}}}
	decl := edgeIOScore(snap, s)
	if want := 100 - edgeIOInternalSpecPenalty; decl.Score != want {
		t.Fatalf("spec-declared internal-on-edge score = %v, want %v (%+v)", decl.Score, want, decl.Contributors)
	}
	if decl.Score >= heur.Score {
		t.Error("a spec-declared violation must cost MORE than a heuristic guess")
	}
	if got := decl.Metrics["internalOnEdgeSpec"]; got != 1 {
		t.Errorf("internalOnEdgeSpec = %v, want 1", got)
	}
}

// AABB 板框（连接器没给多边形）→ 照算但必须 degraded：异形板上「到板边距离」正好
// 在最要紧的地方算错（Type-C 突出部位的件明明贴边，AABB 看却离边很远）。
func TestEdgeIOScorer_AabbOutlineDegrades(t *testing.T) {
	snap := &boardSnapshot{
		Outline: &boardOutline{BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 4000, MaxY: 2400}, Source: "bbox"},
		Components: []boardComp{
			mkBoardConn("J1", "KF301-5.0-2P", 1, 1000, 120, 400, 200, "VIN", "GND"),
			mkBoardConn("J2", "KF301-5.0-2P", 1, 3000, 120, 400, 200, "VOUT", "GND"),
		},
	}
	d := edgeIOScore(snap, nil)
	if d.Status != dimDegraded {
		t.Fatalf("status = %s, want degraded", d.Status)
	}
	if !strings.Contains(d.Reason, "AABB") {
		t.Errorf("reason must name the approximation: %s", d.Reason)
	}
	if got := d.Metrics["outlinePolygon"]; got != 0 {
		t.Errorf("outlinePolygon = %v, want 0", got)
	}
	if d.Score != 100 {
		t.Errorf("an approximate input still gets a real score; got %v", d.Score)
	}
}

// 包络查不到表时走 bbox 兜底，这也是"输入近似" → degraded 并点名是哪几件。
func TestEdgeIOScorer_FallbackPlugWidthDegrades(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("J1", "WEIRD-CONN-4P", 1, 1000, 120, 200, 200, "SIG1", "GND"),
			mkBoardConn("J2", "KF301-5.0-2P", 1, 3000, 120, 400, 200, "VOUT", "GND"),
		},
	}
	d := edgeIOScore(snap, nil)
	if d.Status != dimDegraded || !strings.Contains(d.Reason, "plug-envelope") {
		t.Fatalf("fallback plug width must degrade with an explanation; got %s / %s", d.Status, d.Reason)
	}
	if got := d.Metrics["plugWidthFallback"]; got != 1 {
		t.Errorf("plugWidthFallback = %v, want 1", got)
	}
}

// 这一维要真的挂在打分骨架上（注册 + 参与 analyzeLayoutScore），否则报告里会静静
// 少一维："dimension not implemented yet"。
func TestEdgeIOScorer_WiredIntoReport(t *testing.T) {
	if scorerFor(dimEdgeIO) == nil {
		t.Fatal("edge-io scorer is not registered")
	}
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("J1", "KF301-5.0-2P", 1, 1000, 120, 400, 200, "VIN", "GND"),
			mkBoardConn("J2", "KF301-5.0-2P", 1, 3000, 120, 400, 200, "VOUT", "GND"),
		},
	}
	rep := analyzeLayoutScore(snap, nil, layoutScoreOpts{})
	d := rep.dimension(dimEdgeIO)
	if d == nil {
		t.Fatal("edge-io missing from the report")
	}
	if strings.Contains(d.Reason, "not implemented") {
		t.Fatalf("edge-io still reports as unimplemented: %+v", d)
	}
	if d.Status == dimSkipped {
		t.Errorf("with an outline and two connectors this dimension is scorable; got skipped (%s)", d.Reason)
	}
}

// 插接面贴边(器件特性,用户校准点名):Type-C/USB/SD 类水平插拔件在边带内但
// 缩在板内 → 扣分点名;齐平与**外突**(bbox 越过板框)都合法 —— off-board 判据
// 用焊盘不用 bbox,正是为了放行外突的插接面。
func TestEdgeIOScorer_PlugFaceMustBeFlushOrProtrude(t *testing.T) {
	// 板 4000×2400。三个 Type-C:齐边 / 缩 130mil / 外突 60mil。
	flush := mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 3990, 200, 20, 180, "VBUS", "GND")
	inset := mkBoardConn("USB2", "TYPE-C-31-M-12", 1, 3840, 800, 60, 180, "VBUS2", "GND")   // bbox 右缘 3870 → 缩 130mil
	protr := mkBoardConn("USB3", "TYPE-C-31-M-12", 1, 4030, 1400, 120, 180, "VBUS3", "GND") // bbox 越框 60mil
	// 外突件的焊盘保持在板内(真实 Type-C 的贴装脚在板上) —— off-board 不该报它。
	for i := range protr.Pads {
		protr.Pads[i].X = 3980
	}
	snap := &boardSnapshot{Outline: testOutline(), Components: []boardComp{flush, inset, protr}}
	d := edgeIOScore(snap, nil)

	pen := map[string]float64{}
	for _, c := range d.Contributors {
		pen[c.Designator] += c.Penalty
	}
	if pen["USB2"] == 0 {
		t.Fatalf("USB2 sits 130mil inboard — the plug face rule must charge it, got %+v", d.Contributors)
	}
	if pen["USB1"] != 0 || pen["USB3"] != 0 {
		t.Errorf("flush (USB1) and protruding (USB3) mating faces are the part's NATURE and must not be charged, got %+v", d.Contributors)
	}
	var flagged bool
	for _, f := range d.Findings {
		if f.Type == "plug-face-not-flush" && strings.Contains(f.Message, "USB2") {
			flagged = true
		}
	}
	if !flagged {
		t.Error("expected a plug-face-not-flush finding naming USB2")
	}
}
