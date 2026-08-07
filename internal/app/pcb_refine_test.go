package app

import (
	"math"
	"strings"
	"testing"
)

func refComp(des string, x, y float64, locked bool, dev string) boardComp {
	return boardComp{
		ID: "p_" + des, Designator: des, Device: dev, Layer: 1,
		X: x, Y: y, Locked: locked, BBox: bb(x-20, y-10, x+20, y+10),
	}
}

func TestBuildImmovableSet_LockedAndConfirmedTiers(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		refComp("C1", 100.5, 200.3, false, "CAP0402"),
		refComp("U1", 500, 500, true, "ESP32"), // 编辑器里锁了
		refComp("H1", 50, 50, false, "M3 HOLE"),
		refComp("J1", 900, 100, false, "USB3.1TYPE-C16P"),
		refComp("R1", 300, 300, false, "RES0402"),
	}}
	tiers := map[int][]string{
		1: {"H1"},       // 孔
		2: {"J1"},       // 边缘接口件，朝向经用户确认
		3: {"U1"},       // 主芯片
		4: {"C1", "R1"}, // 卫星
	}
	set, list := buildImmovableSet(snap, tiers, false)

	// 锁定件 + tier1 + tier2 必须进不可动集合
	for _, want := range []string{"U1", "H1", "J1"} {
		if _, ok := set[want]; !ok {
			t.Errorf("%s must be immovable (locked / tier-1 / tier-2)", want)
		}
	}
	// tier 3/4 是几何摆放的结果，精修动它们是本分 —— 不该被保护
	for _, moveable := range []string{"C1", "R1"} {
		if why, blocked := set[moveable]; blocked {
			t.Errorf("%s is a tier-3/4 part and should stay refinable, got blocked: %s", moveable, why)
		}
	}
	// 原因必须人读得懂：报告要靠它解释"为什么这件没动"
	for _, e := range list {
		if e.Reason == "" {
			t.Errorf("%s blocked without a reason", e.Designator)
		}
	}
	if len(list) != 3 {
		t.Errorf("immovable list = %d, want 3", len(list))
	}
}

func TestBuildImmovableSet_IncludeLockedOptOut(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{refComp("U1", 0, 0, true, "IC")}}
	set, _ := buildImmovableSet(snap, nil, true)
	if _, blocked := set["U1"]; blocked {
		t.Error("--include-locked must let locked parts through")
	}
}

func TestBudgetMoves_RejectsOverBudgetInsteadOfClamping(t *testing.T) {
	// 截断会把件放到既不是原位也不是目标的第三个位置 —— 比不动更糟。
	moves := []refineMove{
		{ID: "a", Designator: "C1", FromX: 0, FromY: 0, ToX: 3, ToY: 0, HasOriginal: true},  // 3mil，放行
		{ID: "b", Designator: "C2", FromX: 0, FromY: 0, ToX: 40, ToY: 0, HasOriginal: true}, // 40mil，超预算
	}
	kept, rejects := budgetMoves(moves, nil, 5)
	if len(kept) != 1 || kept[0].Designator != "C1" {
		t.Fatalf("kept = %+v, want only C1", kept)
	}
	if len(rejects) != 1 || !strings.Contains(rejects[0], "exceeds") {
		t.Errorf("rejects = %v, want an explicit over-budget reason", rejects)
	}
	// 被拒的那件的目标坐标绝不能被改写成边界值
	if moves[1].ToX != 40 {
		t.Error("an over-budget move must be dropped, never clamped")
	}
}

func TestBudgetMoves_RefusesUnrollbackable(t *testing.T) {
	// 没有原位 = 回不去。在"新增 finding 就回滚"的护栏下这是不可接受的。
	moves := []refineMove{
		{ID: "", Designator: "C1", ToX: 1, HasOriginal: true},             // 无 primitive id
		{ID: "b", Designator: "C2", FromX: 0, ToX: 1, HasOriginal: false}, // 无原位
		{ID: "c", Designator: "C3", FromX: 0, FromY: 0, ToX: 1, HasOriginal: true},
	}
	kept, rejects := budgetMoves(moves, nil, 5)
	if len(kept) != 1 || kept[0].Designator != "C3" {
		t.Fatalf("kept = %+v, want only the rollbackable C3", kept)
	}
	if len(rejects) != 2 {
		t.Fatalf("rejects = %v, want both unrollbackable moves refused", rejects)
	}
	for _, r := range rejects {
		if !strings.Contains(r, "unrollbackable") {
			t.Errorf("reject reason should name the cause: %q", r)
		}
	}
}

func TestBudgetMoves_DropsFloatNoise(t *testing.T) {
	// 亚 0.01mil 的移动发出去只会白白触发 InvalidatesStage 和 autosave
	// （auto-place 不幂等的老毛病就是这么来的）。
	moves := []refineMove{{ID: "a", Designator: "C1", FromX: 100, FromY: 100, ToX: 100.001, ToY: 100, HasOriginal: true}}
	kept, _ := budgetMoves(moves, nil, 5)
	if len(kept) != 0 {
		t.Errorf("sub-0.01mil noise must be dropped, got %+v", kept)
	}
}

func TestBudgetMoves_HonoursImmovableSet(t *testing.T) {
	moves := []refineMove{{ID: "a", Designator: "H1", FromX: 0, FromY: 0, ToX: 2, HasOriginal: true}}
	kept, rejects := budgetMoves(moves, map[string]string{"H1": "tier-1 confirmed"}, 5)
	if len(kept) != 0 {
		t.Error("an immovable part must never be moved")
	}
	if len(rejects) != 1 || !strings.Contains(rejects[0], "tier-1") {
		t.Errorf("the reject must carry the protection reason, got %v", rejects)
	}
}

func TestPlanGridSnap_CatchesSubMilDrift(t *testing.T) {
	// #153 实测的真实坐标形态：C2(635.0015, 1109.998) —— auto-place / GUI 拖动
	// 留下的亚 mil 漂移，目视看不出来，但让行列对齐永远差一点。
	snap := &boardSnapshot{Components: []boardComp{
		refComp("C2", 635.0015, 1109.998, false, "CAP0402"),
		refComp("C6", 455.0015, 839.998, false, "CAP0402"),
		refComp("R1", 300, 500, false, "RES0402"), // 已落格
	}}
	moves := planGridSnap(snap, 5, nil)
	if len(moves) != 2 {
		t.Fatalf("want 2 off-grid parts, got %d: %+v", len(moves), moves)
	}
	for _, m := range moves {
		if math.Mod(m.ToX, 5) != 0 || math.Mod(m.ToY, 5) != 0 {
			t.Errorf("%s target (%.4f,%.4f) is not on the 5mil grid", m.Designator, m.ToX, m.ToY)
		}
		// 位移必须是亚 mil 级 —— 这是"吸附"不是"重排"
		if m.shift() > 1 {
			t.Errorf("%s shift %.4f mil is a rearrangement, not a snap", m.Designator, m.shift())
		}
		if m.Why == "" {
			t.Errorf("%s move has no explanation", m.Designator)
		}
	}
}

func TestPlanGridSnap_SkipsMetricPitchParts(t *testing.T) {
	// conventions §9.1：吸英制栅会把公制间距件的焊盘推离它们的原生子栅。
	snap := &boardSnapshot{Components: []boardComp{
		refComp("J1", 100.3, 200.7, false, "USB3.1TYPE-C16P"),
		refComp("J2", 300.3, 400.7, false, "PH-3AW JST PH2.0"),
		refComp("U1", 500.3, 600.7, false, "QFN-32"),
		refComp("C1", 700.3, 800.7, false, "CAP0402"),
	}}
	moves := planGridSnap(snap, 5, nil)
	if len(moves) != 1 || moves[0].Designator != "C1" {
		var got []string
		for _, m := range moves {
			got = append(got, m.Designator)
		}
		t.Errorf("only the imperial-pitch C1 should snap, got %v", got)
	}
}

func TestPlanGridSnap_HonoursImmovable(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{refComp("H1", 100.3, 200.7, false, "M3")}}
	if moves := planGridSnap(snap, 5, map[string]string{"H1": "tier-1"}); len(moves) != 0 {
		t.Errorf("immovable parts must not be planned for a snap, got %+v", moves)
	}
}

func TestIsMetricPitchPart(t *testing.T) {
	cases := map[string]bool{
		"USB3.1TYPE-C16P":  true,
		"PH-3AW":           false, // 型号本身不含公制线索
		"JST PH2.0-3P":     true,
		"QFN-32":           true,
		"BGA-121":          true,
		"RES0402":          false,
		"ESP32-S3-WROOM-1": false,
	}
	for dev, want := range cases {
		if got := isMetricPitchPart(boardComp{Device: dev}); got != want {
			t.Errorf("isMetricPitchPart(%q) = %v, want %v", dev, got, want)
		}
	}
}

func TestRefineMove_Shift(t *testing.T) {
	m := refineMove{FromX: 0, FromY: 0, ToX: 3, ToY: 4}
	if got := m.shift(); math.Abs(got-5) > 1e-9 {
		t.Errorf("shift = %v, want 5", got)
	}
}
