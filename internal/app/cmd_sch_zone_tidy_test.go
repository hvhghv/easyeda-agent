package app

// cmd_sch_zone_tidy_test.go — planZonePack 表驱动单测 + zone-tidy 纯 helper 单测
// (契约 §3:两组上下堆叠 / 三组换行 / 锚不动 / 间距断言 / 装不下诊断 / 确定性)。

import (
	"reflect"
	"strings"
	"testing"
)

// findMove 按 ID 取 move(测试辅助)。
func findMove(t *testing.T, plan zonePackPlan, id string) zonePackMove {
	t.Helper()
	for _, m := range plan.Moves {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("no move for %s in %+v", id, plan.Moves)
	return zonePackMove{}
}

func effRect(g zonePackGroup, m zonePackMove) layoutBBox {
	return zonePackOffset(g.BBox, m.DX, m.DY)
}

// ── planZonePack:两组上下堆叠(用户点名的上下布局) ─────────────────────────

func TestPlanZonePack_TwoGroupsStackBelowAnchor(t *testing.T) {
	band := layoutBBox{MinX: 0, MinY: 0, MaxX: 500, MaxY: 800}
	g1 := zonePackGroup{ID: "g1", BBox: layoutBBox{MinX: 100, MinY: 500, MaxX: 300, MaxY: 700}, IsAnchor: true}
	g2 := zonePackGroup{ID: "g2", BBox: layoutBBox{MinX: 350, MinY: 600, MaxX: 450, MaxY: 680}}
	plan := planZonePack([]zonePackGroup{g1, g2}, band, 117, 40)
	if !plan.Fits || plan.Diag != nil {
		t.Fatalf("expected fit, got %+v", plan)
	}
	if len(plan.Moves) != 2 {
		t.Fatalf("want 2 moves, got %+v", plan.Moves)
	}
	// 锚在首位、不动。
	if plan.Moves[0].ID != "g1" || !plan.Moves[0].Anchor || plan.Moves[0].DX != 0 || plan.Moves[0].DY != 0 {
		t.Fatalf("anchor should stay put and come first: %+v", plan.Moves[0])
	}
	// g2 贴带左、顶对齐到锚下方 vGap 处(上下堆叠)。
	m2 := findMove(t, plan, "g2")
	if m2.DX != -350 || m2.DY != -220 {
		t.Fatalf("g2 move want Δ(-350,-220), got Δ(%g,%g)", m2.DX, m2.DY)
	}
	e2 := effRect(g2, m2)
	if e2.MaxY != g1.BBox.MinY-40 {
		t.Errorf("g2 top %.1f should sit vGap=40 below anchor bottom %.1f", e2.MaxY, g1.BBox.MinY)
	}
	if e2.MinX != band.MinX {
		t.Errorf("g2 left %.1f should start at band left %.1f", e2.MinX, band.MinX)
	}
	if err := zonePackValidate([]zonePackGroup{g1, g2}, plan.Moves, band); err != nil {
		t.Errorf("pack invariants violated: %v", err)
	}
}

// ── planZonePack:三组行满换行 ──────────────────────────────────────────────

func TestPlanZonePack_ThreeGroupsRowWrap(t *testing.T) {
	band := layoutBBox{MinX: 0, MinY: 0, MaxX: 300, MaxY: 1000}
	anchor := zonePackGroup{ID: "g1", BBox: layoutBBox{MinX: 0, MinY: 800, MaxX: 200, MaxY: 1000}, IsAnchor: true}
	g2 := zonePackGroup{ID: "g2", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 150, MaxY: 100}}  // 面积 15000,先放
	g3 := zonePackGroup{ID: "g3", BBox: layoutBBox{MinX: 200, MinY: 0, MaxX: 340, MaxY: 80}} // 面积 11200,后放
	plan := planZonePack([]zonePackGroup{anchor, g2, g3}, band, 117, 40)
	if !plan.Fits {
		t.Fatalf("expected fit, got %+v", plan.Diag)
	}
	m2, m3 := findMove(t, plan, "g2"), findMove(t, plan, "g3")
	e2, e3 := effRect(g2, m2), effRect(g3, m3)
	// g2 行 1:顶 = 锚底 - 40 = 760。
	if e2.MaxY != 760 || e2.MinX != 0 {
		t.Fatalf("g2 should open row 1 at (0,·)..top 760, got %+v", e2)
	}
	// g3 行 1 放不下(150+117+140 > 300)→ 换行:顶 = 行1最低点(660) - 40 = 620。
	if e3.MaxY != 620 || e3.MinX != 0 {
		t.Fatalf("g3 should wrap to row 2 at (0,·)..top 620, got %+v (moves %+v)", e3, plan.Moves)
	}
	if m3.DX != -200 || m3.DY != 540 {
		t.Errorf("g3 move want Δ(-200,540), got Δ(%g,%g)", m3.DX, m3.DY)
	}
	if err := zonePackValidate([]zonePackGroup{anchor, g2, g3}, plan.Moves, band); err != nil {
		t.Errorf("pack invariants violated: %v", err)
	}
}

// ── planZonePack:锚不在带内 → 贴带内基准位(带左上) ───────────────────────

func TestPlanZonePack_AnchorOutsideBandSnapsToBaseline(t *testing.T) {
	band := layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 400}
	anchor := zonePackGroup{ID: "g1", BBox: layoutBBox{MinX: 1000, MinY: 1000, MaxX: 1200, MaxY: 1150}, IsAnchor: true}
	plan := planZonePack([]zonePackGroup{anchor}, band, 117, 40)
	if !plan.Fits {
		t.Fatalf("expected fit, got %+v", plan.Diag)
	}
	m := plan.Moves[0]
	if m.ID != "g1" || !m.Anchor {
		t.Fatalf("unexpected move %+v", m)
	}
	if m.DX != -1000 || m.DY != -750 {
		t.Fatalf("anchor should snap to band top-left: want Δ(-1000,-750), got Δ(%g,%g)", m.DX, m.DY)
	}
	eff := effRect(anchor, m)
	if eff.MinX != band.MinX || eff.MaxY != band.MaxY {
		t.Errorf("anchor should hug band top-left, got %+v", eff)
	}
}

// ── planZonePack:未显式标锚时取最大 bbox 组 ────────────────────────────────

func TestPlanZonePack_NoFlagPicksLargestAsAnchor(t *testing.T) {
	band := layoutBBox{MinX: 0, MinY: 0, MaxX: 600, MaxY: 600}
	small := zonePackGroup{ID: "gA", BBox: layoutBBox{MinX: 10, MinY: 10, MaxX: 60, MaxY: 60}}
	big := zonePackGroup{ID: "gB", BBox: layoutBBox{MinX: 100, MinY: 300, MaxX: 400, MaxY: 550}}
	plan := planZonePack([]zonePackGroup{small, big}, band, 117, 40)
	if !plan.Fits {
		t.Fatalf("expected fit, got %+v", plan.Diag)
	}
	if plan.Moves[0].ID != "gB" || !plan.Moves[0].Anchor {
		t.Fatalf("largest group should anchor: %+v", plan.Moves)
	}
	if plan.Moves[0].DX != 0 || plan.Moves[0].DY != 0 {
		t.Errorf("in-band anchor should not move: %+v", plan.Moves[0])
	}
}

// ── planZonePack:间距不变量(含非网格输入 → 方向性吸附只放大间距) ──────────

func TestPlanZonePack_GapInvariantsWithOffGridInput(t *testing.T) {
	band := layoutBBox{MinX: 0, MinY: 0, MaxX: 900, MaxY: 900}
	groups := []zonePackGroup{
		{ID: "g1", BBox: layoutBBox{MinX: 100.3, MinY: 600.7, MaxX: 400.1, MaxY: 880.2}, IsAnchor: true},
		{ID: "g2", BBox: layoutBBox{MinX: 0.1, MinY: 0.2, MaxX: 200.4, MaxY: 100.9}},
		{ID: "g3", BBox: layoutBBox{MinX: 500.6, MinY: 0.3, MaxX: 650.8, MaxY: 90.1}},
	}
	hGap, vGap := 117.0, 40.0
	plan := planZonePack(groups, band, hGap, vGap)
	if !plan.Fits {
		t.Fatalf("expected fit, got %+v", plan.Diag)
	}
	if err := zonePackValidate(groups, plan.Moves, band); err != nil {
		t.Fatalf("pack invariants violated: %v", err)
	}
	byID := map[string]zonePackGroup{}
	for _, g := range groups {
		byID[g.ID] = g
	}
	// 所有 dx/dy 必须是 5 单位网格倍数(组内既有对齐整体保留)。
	for _, m := range plan.Moves {
		for _, v := range []float64{m.DX, m.DY} {
			q := v / zonePackGridSnap
			if q != float64(int64(q)) {
				t.Errorf("move %s Δ(%g,%g) is not grid-snapped", m.ID, m.DX, m.DY)
			}
		}
	}
	// 行内相邻组水平间距 ≥ hGap(ceil 吸附只右移),行间垂直间距 ≥ vGap。
	e2 := effRect(byID["g2"], findMove(t, plan, "g2"))
	e3 := effRect(byID["g3"], findMove(t, plan, "g3"))
	if gap := e3.MinX - e2.MaxX; gap < hGap {
		t.Errorf("horizontal gap %.2f < hGap %.2f", gap, hGap)
	}
	anchorEff := effRect(byID["g1"], findMove(t, plan, "g1"))
	if gap := anchorEff.MinY - e2.MaxY; gap < vGap {
		t.Errorf("vertical gap to anchor %.2f < vGap %.2f", gap, vGap)
	}
}

// ── planZonePack:装不下 → 结构化诊断(需要的最小 band 尺寸),不硬塞 ────────

func TestPlanZonePack_NoFitDiagnostics(t *testing.T) {
	band := layoutBBox{MinX: 0, MinY: 0, MaxX: 200, MaxY: 100}
	anchor := zonePackGroup{ID: "g1", BBox: layoutBBox{MinX: 0, MinY: 50, MaxX: 150, MaxY: 100}, IsAnchor: true}
	g2 := zonePackGroup{ID: "g2", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 180, MaxY: 60}}
	plan := planZonePack([]zonePackGroup{anchor, g2}, band, 117, 40)
	if plan.Fits || len(plan.Moves) != 0 {
		t.Fatalf("expected no-fit with no moves, got %+v", plan)
	}
	if plan.Diag == nil {
		t.Fatal("expected structured diagnostics")
	}
	d := plan.Diag
	if d.BandW != 200 || d.BandH != 100 {
		t.Errorf("diag band dims want 200×100, got %g×%g", d.BandW, d.BandH)
	}
	// 需要的最小尺寸:宽足够(200),高 = 锚(50) + vGap(40) + g2(60) = 150。
	if d.NeedW != 200 || d.NeedH != 150 {
		t.Errorf("diag need dims want 200×150, got %g×%g", d.NeedW, d.NeedH)
	}
	if !strings.Contains(d.Reason, "need at least") {
		t.Errorf("reason should carry the minimal band size: %q", d.Reason)
	}
}

func TestPlanZonePack_TooNarrowBandDiagnostics(t *testing.T) {
	band := layoutBBox{MinX: 0, MinY: 0, MaxX: 100, MaxY: 500}
	anchor := zonePackGroup{ID: "g1", BBox: layoutBBox{MinX: 0, MinY: 400, MaxX: 90, MaxY: 500}, IsAnchor: true}
	wide := zonePackGroup{ID: "g2", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 250, MaxY: 50}}
	plan := planZonePack([]zonePackGroup{anchor, wide}, band, 117, 40)
	if plan.Fits {
		t.Fatalf("a 250-wide group cannot fit a 100-wide band: %+v", plan)
	}
	// needW = 最宽组 250 + 一格吸附余量 5。
	if plan.Diag == nil || plan.Diag.NeedW != 255 {
		t.Fatalf("needW want 255, got %+v", plan.Diag)
	}
}

// ── planZonePack:确定性(同输入同输出;输入重排同输出) ────────────────────

func TestPlanZonePack_Deterministic(t *testing.T) {
	band := layoutBBox{MinX: 0, MinY: 0, MaxX: 800, MaxY: 900}
	groups := []zonePackGroup{
		{ID: "g1", BBox: layoutBBox{MinX: 100, MinY: 600, MaxX: 400, MaxY: 850}, IsAnchor: true},
		{ID: "g2", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 200, MaxY: 120}},
		{ID: "g3", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 150, MaxY: 90}},
		{ID: "g4", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 150, MaxY: 90}}, // 与 g3 完全同尺寸 → ID 破平
		{ID: "g5", BBox: layoutBBox{MinX: 300, MinY: 200, MaxX: 380, MaxY: 260}},
	}
	first := planZonePack(groups, band, 117, 40)
	second := planZonePack(groups, band, 117, 40)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same input produced different output:\n%+v\n%+v", first, second)
	}
	// 输入顺序重排不得改变输出(锚选择与放置排序都是全序)。
	shuffled := []zonePackGroup{groups[4], groups[2], groups[0], groups[3], groups[1]}
	third := planZonePack(shuffled, band, 117, 40)
	if !reflect.DeepEqual(first, third) {
		t.Fatalf("input permutation changed the output:\n%+v\n%+v", first, third)
	}
	if !first.Fits {
		t.Fatalf("expected fit, got %+v", first.Diag)
	}
	// 同尺寸的 g3/g4 按 ID 升序放置:g3 在 g4 左侧或上方。
	byID := map[string]zonePackGroup{}
	for _, g := range groups {
		byID[g.ID] = g
	}
	e3 := effRect(byID["g3"], findMove(t, first, "g3"))
	e4 := effRect(byID["g4"], findMove(t, first, "g4"))
	if !(e3.MaxY > e4.MaxY || (e3.MaxY == e4.MaxY && e3.MinX < e4.MinX)) {
		t.Errorf("g3 should place before g4 (ID tiebreak): g3=%+v g4=%+v", e3, e4)
	}
}

// ── planZonePack:空输入与单锚 ─────────────────────────────────────────────

func TestPlanZonePack_EmptyAndAnchorOnly(t *testing.T) {
	if plan := planZonePack(nil, layoutBBox{0, 0, 100, 100}, 117, 40); !plan.Fits || len(plan.Moves) != 0 {
		t.Fatalf("empty input should trivially fit: %+v", plan)
	}
	band := layoutBBox{MinX: 0, MinY: 0, MaxX: 300, MaxY: 300}
	anchor := zonePackGroup{ID: "g1", BBox: layoutBBox{MinX: 50, MinY: 50, MaxX: 250, MaxY: 250}}
	plan := planZonePack([]zonePackGroup{anchor}, band, 117, 40)
	if !plan.Fits || len(plan.Moves) != 1 || plan.Moves[0].DX != 0 || plan.Moves[0].DY != 0 {
		t.Fatalf("lone in-band group should stay put: %+v", plan)
	}
}

// ── zoneTidyUnits:组/散件切分 + 跨区组拒绝 ────────────────────────────────

func TestZoneTidyUnits_GroupsAndSingles(t *testing.T) {
	claimed := []string{"U3", "C5", "C6", "R1"}
	groups := []*schGroup{
		{ID: "g1", Name: "power", Members: []string{"C5", "U3"}},
		{ID: "g2", Members: []string{"J1"}}, // 区外组 → 忽略
	}
	units, err := zoneTidyUnits(claimed, groups)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []zoneTidyUnit{
		{Ref: "g1", Name: "power", Members: []string{"C5", "U3"}, IsGroup: true},
		{Ref: "C6", Members: []string{"C6"}},
		{Ref: "R1", Members: []string{"R1"}},
	}
	if !reflect.DeepEqual(units, want) {
		t.Fatalf("units mismatch:\n got %+v\nwant %+v", units, want)
	}
}

func TestZoneTidyUnits_CrossZoneGroupRejected(t *testing.T) {
	claimed := []string{"U3", "C5"}
	groups := []*schGroup{
		{ID: "g3", Members: []string{"C5", "R9"}}, // C5 区内,R9 区外 → 跨区
	}
	_, err := zoneTidyUnits(claimed, groups)
	if err == nil {
		t.Fatal("cross-zone group must be rejected")
	}
	for _, frag := range []string{"g3", "C5", "R9"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error should name %q: %v", frag, err)
		}
	}
}

// ── band 来源 helpers ──────────────────────────────────────────────────────

func TestZoneTidyBandFromPlan(t *testing.T) {
	plan := partitionPlan{Partitions: []partitionRect{
		{
			Modules:   []string{"POWER"},
			BBox:      layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 600},
			TitleBBox: layoutBBox{MinX: 0, MinY: 570, MaxX: 400, MaxY: 600},
		},
		{
			Modules:   []string{"MCU", "USB"},
			BBox:      layoutBBox{MinX: 500, MinY: 0, MaxX: 900, MaxY: 600},
			TitleBBox: layoutBBox{MinX: 500, MinY: 570, MaxX: 900, MaxY: 600},
		},
	}}
	band, ok := zoneTidyBandFromPlan(plan, "POWER")
	if !ok {
		t.Fatal("exclusive partition should yield a band")
	}
	want := layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 570} // 扣掉顶部 title band
	if band != want {
		t.Fatalf("band want %+v, got %+v", want, band)
	}
	if _, ok := zoneTidyBandFromPlan(plan, "MCU"); ok {
		t.Error("shared partition must NOT be used as a band (would pack over another module)")
	}
	if _, ok := zoneTidyBandFromPlan(plan, "GONE"); ok {
		t.Error("missing zone must not yield a band")
	}
}

func TestZoneTidyContentBandAndUnionBBox(t *testing.T) {
	band, ok := zoneTidyContentBand([]layoutBBox{
		{MinX: 10, MinY: 20, MaxX: 100, MaxY: 200},
		{MinX: 50, MinY: 0, MaxX: 150, MaxY: 90},
	}, 24)
	if !ok {
		t.Fatal("content band should resolve")
	}
	want := layoutBBox{MinX: -14, MinY: -24, MaxX: 174, MaxY: 224}
	if band != want {
		t.Fatalf("content band want %+v, got %+v", want, band)
	}
	if _, ok := zoneTidyContentBand(nil, 24); ok {
		t.Error("empty content must not yield a band")
	}

	u, ok := zoneTidyUnionBBox(
		[]layoutBBox{{MinX: 0, MinY: 0, MaxX: 10, MaxY: 10}},
		[][]float64{{-5, 3, 12, 3}}, // 桩线折线拉宽 x
		[][2]float64{{4, 25}},       // 旗锚点拉高 y
	)
	if !ok {
		t.Fatal("union bbox should resolve")
	}
	want = layoutBBox{MinX: -5, MinY: 0, MaxX: 12, MaxY: 25}
	if u != want {
		t.Fatalf("union bbox want %+v, got %+v", want, u)
	}
}

// ── 锚单元选择与 claim 查找 ────────────────────────────────────────────────

func TestZoneTidyAnchorRef(t *testing.T) {
	units := []zoneTidyUnit{
		{Ref: "g1", Members: []string{"C5", "U3"}, IsGroup: true},
		{Ref: "R1", Members: []string{"R1"}},
	}
	areas := map[string]float64{"C5": 100, "U3": 5000, "R1": 300}
	if ref := zoneTidyAnchorRef(units, areas); ref != "g1" {
		t.Errorf("unit holding the largest part (U3) should anchor, got %q", ref)
	}
	if ref := zoneTidyAnchorRef(units, map[string]float64{}); ref != "" {
		t.Errorf("no measurable area should yield no explicit anchor, got %q", ref)
	}
}

// --zone 解析已统一走 resolveLayoutZone,用例移植到 sch_layout_objects_test.go。

// ── 双认领图元:计划期 dedup 与执行期差集过滤一致 ──────────────────────────

func TestZoneTidySubtractShared(t *testing.T) {
	cases := []struct {
		name   string
		ids    []string
		shared map[string]bool
		want   []string
	}{
		{"nil shared keeps everything", []string{"c1", "w1"}, nil, []string{"c1", "w1"}},
		{"empty shared keeps everything", []string{"w1"}, map[string]bool{}, []string{"w1"}},
		{"drops shared ids, keeps order", []string{"c1", "w1", "w2", "f1"},
			map[string]bool{"w1": true, "f1": true}, []string{"c1", "w2"}},
		{"unrelated shared ids are inert", []string{"c1", "w2"},
			map[string]bool{"w9": true}, []string{"c1", "w2"}},
		{"all shared → empty set", []string{"w1", "f1"},
			map[string]bool{"w1": true, "f1": true}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]string(nil), tc.ids...)
			got := zoneTidySubtractShared(in, tc.shared)
			if len(got) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("want %v, got %v", tc.want, got)
				}
			}
			if !reflect.DeepEqual(in, tc.ids) {
				t.Errorf("input slice was mutated: %v → %v", tc.ids, in)
			}
		})
	}
}

// TestZoneTidyApplySetExcludesSharedTree 复现评审观察 1 的场景:一棵桩线树同时
// member-touch 两个单元(span 中段压过双方成员脚,不 terminate 在任何一方 —— 两侧
// 展开都会认领),计划期被 zoneTidySharedIDs 标记为双认领并从两个单元剔除(承诺
// 「原地不动」);执行期 expandSchGroupForMove 语义的独立重展开仍会认领它 ——
// 断言 zoneTidySubtractShared 差集过滤后,apply 侧 move 集不含双认领的桩线与旗,
// 而单元自有的桩线/旗保留(执行与计划几何承诺一致)。
func TestZoneTidyApplySetExcludesSharedTree(t *testing.T) {
	// 单元 A:成员脚 (100,100)/(100,200);单元 B:成员脚 (300,100)。
	pinsA := [][2]float64{{100, 100}, {100, 200}}
	pinsB := [][2]float64{{300, 100}}
	wires := []schGroupWire{
		{ID: "wShared", Points: []float64{50, 100, 350, 100}}, // 中段压过 A pin1 与 B pin,两端悬空
		{ID: "wStubA", Points: []float64{100, 200, 100, 240}}, // A 自有桩线(端点接 A pin2)
	}
	flags := []schGroupFlag{
		{ID: "fShared", X: 200, Y: 100}, // 骑在双认领树上
		{ID: "fStubA", X: 100, Y: 240},  // 骑在 A 自有桩线上
	}
	expand := func(member, other [][2]float64) groupExpansion {
		return expandGroupAttachments(groupExpandInput{
			MemberPins: member, OtherPins: other, Wires: wires, Flags: flags,
		})
	}
	// 计划期:两个单元各自展开(computeZoneTidy 的语义)。
	expA, expB := expand(pinsA, pinsB), expand(pinsB, pinsA)
	if len(expA.Suspects) != 0 || len(expB.Suspects) != 0 {
		t.Fatalf("fixture must not trip the residue precheck: %+v / %+v", expA.Suspects, expB.Suspects)
	}
	if !strInSlice(expA.WireIDs, "wShared") || !strInSlice(expB.WireIDs, "wShared") {
		t.Fatalf("fixture must double-claim wShared: A=%v B=%v", expA.WireIDs, expB.WireIDs)
	}
	shared := zoneTidySharedIDs([]zoneTidyUnitExp{
		{wireIDs: expA.WireIDs, flagIDs: expA.FlagIDs},
		{wireIDs: expB.WireIDs, flagIDs: expB.FlagIDs},
	})
	for _, id := range []string{"wShared", "fShared"} {
		if !shared[id] {
			t.Errorf("%s is claimed by both units and must be marked shared, got %v", id, shared)
		}
	}
	for _, id := range []string{"wStubA", "fStubA"} {
		if shared[id] {
			t.Errorf("%s is A-only and must NOT be marked shared", id)
		}
	}
	// 执行期:expandSchGroupForMove 同语义重展开单元 A(不知道计划期 dedup)——
	// 仍会认领双认领树;差集过滤后 move 集不得再含它,自有附着保留。
	applyExp := expand(pinsA, pinsB)
	applyIDs := append([]string{"compA"}, applyExp.WireIDs...)
	applyIDs = append(applyIDs, applyExp.FlagIDs...)
	if !strInSlice(applyIDs, "wShared") {
		t.Fatalf("apply-side re-expansion should still claim wShared before subtraction: %v", applyIDs)
	}
	got := zoneTidySubtractShared(applyIDs, shared)
	want := []string{"compA", "wStubA", "fStubA"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("apply-side move set after shared subtraction:\n got %v\nwant %v", got, want)
	}
}
