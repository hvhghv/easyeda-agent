package app

import (
	"math"
	"testing"
)

// ── sch destagger 规划器(issue #171)────────────────────────────────────────
//
// 场景取自 issue #171 评论里的真实会话(2026-08-12,ESP32 板加 CH340 + 自动下载
// 块):密脚器件上多支同宿主 GND/+5V 旗互撞,AI 当时靠临场猜 offset 手改三轮才
// 收敛,中途还引入过一次 multi-net-wire 短路。这些用例把那条路径钉死。

// dsFlag 造一支带桩线的 netflag:锚在 (ax,ay),符号沿 dir 外伸。
// 返回 comp + 它的两点桩线(宿主端在 host)。
func dsFlag(id, net string, hostX, hostY, ax, ay float64, family string) (layoutComp, schGroupWire) {
	dir := "up"
	switch {
	case ay < hostY:
		dir = "down"
	case ax > hostX:
		dir = "right"
	case ax < hostX:
		dir = "left"
	}
	rot := flagBodyRotation[family][dir]
	// 旗符号本体 10×21(实测,见 flagTextBand 注释):沿桩向外伸 21,横向 ±5。
	var box layoutBBox
	switch dir {
	case "up":
		box = layoutBBox{MinX: ax - 5, MinY: ay, MaxX: ax + 5, MaxY: ay + 21}
	case "down":
		box = layoutBBox{MinX: ax - 5, MinY: ay - 21, MaxX: ax + 5, MaxY: ay}
	case "right":
		box = layoutBBox{MinX: ax, MinY: ay - 5, MaxX: ax + 21, MaxY: ay + 5}
	default:
		box = layoutBBox{MinX: ax - 21, MinY: ay - 5, MaxX: ax, MaxY: ay + 5}
	}
	c := layoutComp{
		ID: id, ComponentType: "netflag", Net: net,
		X: ax, Y: ay, AnchorAvailable: true, Rotation: &rot, BBox: &box,
	}
	w := schGroupWire{ID: "w_" + id, Points: []float64{hostX, hostY, ax, ay}}
	return c, w
}

// applyMoves 把计划落到 comps 上(用规划器自己预测的 bbox + 新 anchor),用于
// "改完到底还撞不撞"的自洽校验。真机上真实 bbox 由平台说了算,所以落地侧还有
// 一道真实 `sch check` 复验;这里校验的是**规划器自身不自相矛盾**。
func applyMoves(comps []layoutComp, plan destaggerPlan) []layoutComp {
	moved := map[string]destaggerMove{}
	for _, m := range plan.Moves {
		moved[m.FlagID] = m
	}
	out := make([]layoutComp, 0, len(comps))
	for _, c := range comps {
		if m, ok := moved[c.ID]; ok {
			u := destaggerDirs[m.ToDir]
			c.X = m.HostX + u[0]*m.ToOffset
			c.Y = m.HostY + u[1]*m.ToOffset
			b := m.NewBBox
			c.BBox = &b
			if rot, ok := destaggerRotationFor(c, m.ToDir); ok {
				c.Rotation = &rot
			}
		}
		out = append(out, c)
	}
	return out
}

// 两支同宿主区的 GND 旗贴在一起 → 规划必须把它们分开,且改完真的不再重叠。
func TestPlanDestagger_TwoGroundFlagsSeparate(t *testing.T) {
	// U1 的两个相邻 GND pin,间距 10(密脚器件的典型间距),都朝上出旗 → 文字带
	// "GND"(6×3=18 宽)必然互叠,正是实测抓到的那种"挤成一坨"。
	g1, w1 := dsFlag("g1", "GND", 100, 200, 100, 240, "ground")
	g2, w2 := dsFlag("g2", "GND", 110, 200, 110, 240, "ground")
	comps := []layoutComp{g1, g2}
	wires := []schGroupWire{w1, w2}

	before := markerOverlapFindings(comps, 1)
	if len(before) == 0 {
		t.Fatalf("fixture broken: the two stacked GND flags must overlap to begin with")
	}

	plan := planDestagger(comps, wires, 1)
	if plan.OverlapsBefore != len(before) {
		t.Errorf("OverlapsBefore = %d, want %d", plan.OverlapsBefore, len(before))
	}
	if len(plan.Moves) == 0 {
		t.Fatalf("planner must move at least one flag; skips=%+v", plan.Skips)
	}
	// 宿主端一字不动 —— 电气拓扑的锚(安全原语的核心)。
	for _, m := range plan.Moves {
		var want [2]float64
		switch m.FlagID {
		case "g1":
			want = [2]float64{100, 200}
		case "g2":
			want = [2]float64{110, 200}
		}
		if m.HostX != want[0] || m.HostY != want[1] {
			t.Errorf("%s host moved to (%v,%v), want (%v,%v) — host end must never move",
				m.FlagID, m.HostX, m.HostY, want[0], want[1])
		}
		if m.Kind != "ground" {
			t.Errorf("%s kind = %q, want ground", m.FlagID, m.Kind)
		}
	}
	after := markerOverlapFindings(applyMoves(comps, plan), 1)
	if len(after) >= len(before) {
		t.Fatalf("de-stagger did not reduce overlap: before=%d after=%d, moves=%+v",
			len(before), len(after), plan.Moves)
	}
}

// **power/gnd 旗永远不许被扶去横躺**(2026-08-13 真机验收抓到的真缺陷:初版把
// left/right 也列进地/电的候选方向,规划器真把一支 GND 旗扶去了 left)。横躺的
// power/gnd 旗文字竖排侧向渲染 —— skill conventions 的铁律「信号链末端的电源/
// 地旗必须竖直」。netport 不受此限(顺导线方向摆布)。
func TestPlanDestagger_PowerGroundStayVertical(t *testing.T) {
	// 一排挤在一起的地/电旗,逼规划器把候选方向翻遍。
	var comps []layoutComp
	var wires []schGroupWire
	for i, spec := range []struct {
		id, net, fam string
	}{
		{"g1", "GND", "ground"}, {"g2", "GND", "ground"}, {"g3", "GND", "ground"},
		{"p1", "+5V", "power"}, {"p2", "+5V", "power"},
	} {
		x := 100 + float64(i)*10
		dir := 1.0 // power 朝上
		if spec.fam == "ground" {
			dir = -1
		}
		c, w := dsFlag(spec.id, spec.net, x, 500, x, 500+dir*40, spec.fam)
		comps = append(comps, c)
		wires = append(wires, w)
	}
	plan := planDestagger(comps, wires, 1)
	if len(plan.Moves) == 0 {
		t.Fatalf("fixture must produce moves; skips=%+v", plan.Skips)
	}
	for _, m := range plan.Moves {
		if m.ToDir == "left" || m.ToDir == "right" {
			t.Errorf("%s(%s) planned to %s — power/gnd flags must stay vertical (up/down)",
				m.Net, m.FlagID, m.ToDir)
		}
	}
}

// de-stagger 的本义是**同向错开**:原方向拉得开就别换朝向(视觉扰动最小)。
// 初版"方向外层、桩长内层"的循环把一支本可拉长 45 就避开的 GND 旗直接扶去了
// 别的方向 —— 2026-08-13 真机 dry-run 抓到。
func TestPlanDestagger_PrefersLongerStubOverTurning(t *testing.T) {
	g1, w1 := dsFlag("g1", "GND", 300, 950, 300, 910, "ground") // 朝下
	g2, w2 := dsFlag("g2", "GND", 310, 950, 310, 910, "ground")
	plan := planDestagger([]layoutComp{g1, g2}, []schGroupWire{w1, w2}, 1)
	if len(plan.Moves) == 0 {
		t.Fatalf("fixture must produce a move; skips=%+v", plan.Skips)
	}
	moved := plan.Moves[0]
	if moved.ToDir != moved.FromDir {
		t.Errorf("%s turned %s→%s; an open column below should be solved by a longer stub instead",
			moved.FlagID, moved.FromDir, moved.ToDir)
	}
	if moved.ToOffset <= moved.FromOffset {
		t.Errorf("same-direction move must lengthen the stub: %v → %v", moved.FromOffset, moved.ToOffset)
	}
}

// 幂等:本来就不重叠的页,一个都不许动(参考 #50 autoconnect 不幂等的教训)。
func TestPlanDestagger_IdempotentOnCleanPage(t *testing.T) {
	g1, w1 := dsFlag("g1", "GND", 100, 200, 100, 240, "ground")
	g2, w2 := dsFlag("g2", "GND", 400, 200, 400, 240, "ground")
	plan := planDestagger([]layoutComp{g1, g2}, []schGroupWire{w1, w2}, 1)
	if len(plan.Moves) != 0 || len(plan.Skips) != 0 {
		t.Fatalf("clean page must plan nothing, got moves=%+v skips=%+v", plan.Moves, plan.Skips)
	}
	if plan.OverlapsBefore != 0 {
		t.Errorf("OverlapsBefore = %d, want 0", plan.OverlapsBefore)
	}
}

// 安全原语:挂在多段折线/长线/斜线上的 marker 一律不搬,并留下可归因的 reason。
func TestStubOfMarker_RefusesUnsafeCarriers(t *testing.T) {
	mk := func(x, y float64) layoutComp {
		rot := flagBodyRotation["ground"]["up"]
		return layoutComp{
			ID: "f", ComponentType: "netflag", Net: "GND", X: x, Y: y,
			AnchorAvailable: true, Rotation: &rot,
			BBox: bb(x-5, y, x+5, y+21),
		}
	}
	cases := []struct {
		name   string
		wire   schGroupWire
		flag   layoutComp
		reason string
	}{
		{
			name: "多段折线(网络主干)",
			wire: schGroupWire{ID: "w", Points: []float64{0, 0, 100, 0, 100, 240}},
			flag: mk(100, 240), reason: "not-a-stub",
		},
		{
			name: "长桩(超过 destaggerStubMaxLen)",
			wire: schGroupWire{ID: "w", Points: []float64{100, 0, 100, 400}},
			flag: mk(100, 400), reason: "stub-too-long",
		},
		{
			name: "斜桩(重连语义不明)",
			wire: schGroupWire{ID: "w", Points: []float64{100, 200, 140, 240}},
			flag: mk(140, 240), reason: "diagonal-stub",
		},
		{
			name: "锚不在任何线上(孤儿旗)",
			wire: schGroupWire{ID: "w", Points: []float64{0, 0, 50, 0}},
			flag: mk(100, 240), reason: "not-a-stub",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub, why := stubOfMarker(tc.flag, []schGroupWire{tc.wire})
			if why != tc.reason {
				t.Fatalf("reason = %q, want %q (stub=%+v)", why, tc.reason, stub)
			}
			if stub != nil {
				t.Errorf("unsafe carrier must yield no stub, got %+v", stub)
			}
		})
	}
}

// 正常短桩解出宿主端/方向/桩长。
func TestStubOfMarker_ResolvesShortStub(t *testing.T) {
	g, w := dsFlag("g1", "GND", 100, 200, 100, 240, "ground")
	stub, why := stubOfMarker(g, []schGroupWire{w})
	if why != "" {
		t.Fatalf("unexpected skip reason %q", why)
	}
	if stub.HostX != 100 || stub.HostY != 200 {
		t.Errorf("host = (%v,%v), want (100,200)", stub.HostX, stub.HostY)
	}
	if stub.Dir != "up" {
		t.Errorf("dir = %q, want up", stub.Dir)
	}
	if math.Abs(stub.Offset-40) > 0.01 {
		t.Errorf("offset = %v, want 40", stub.Offset)
	}
}

// offset 步长必须是**量出来的**(跟着文字带尺寸走),不是拍脑袋常量 —— 网名越长
// 步子越大。这条直接钉死 issue #171 评论里"AI 临场猜 30/40/50/70"的老路。
func TestDestaggerStep_ScalesWithTextBand(t *testing.T) {
	short, _ := dsFlag("a", "GND", 100, 200, 100, 240, "ground")
	long, _ := dsFlag("b", "GND_ANALOG_QUIET", 100, 200, 100, 240, "ground")
	ss, ls := destaggerStep(short), destaggerStep(long)
	if !(ls > ss) {
		t.Fatalf("longer net name must give a bigger step: short=%v long=%v", ss, ls)
	}
	if ss <= 0 {
		t.Fatalf("step must be positive, got %v", ss)
	}
}

// 地/电的"正位"偏好:地优先朝下、电优先朝上(「电上地下」约定),且换向后
// rotation 走的是与 reversed-net-flag 判据同一张真值表 —— 生成侧和判据侧永远
// 同源(2026-08-12 那次双盲错了两个月的教训)。
func TestDestaggerRotationSharesTruthTable(t *testing.T) {
	g, _ := dsFlag("g", "GND", 100, 200, 100, 240, "ground")
	p, _ := dsFlag("p", "+5V", 300, 200, 300, 240, "power")
	for _, tc := range []struct {
		c   layoutComp
		dir string
		fam string
	}{{g, "down", "ground"}, {g, "left", "ground"}, {p, "up", "power"}, {p, "right", "power"}} {
		rot, ok := destaggerRotationFor(tc.c, tc.dir)
		if !ok {
			t.Fatalf("no rotation for %s/%s", tc.c.Net, tc.dir)
		}
		if want := flagBodyRotation[tc.fam][tc.dir]; rot != want {
			t.Errorf("%s/%s rotation = %v, want %v (must come from flagBodyRotation)", tc.c.Net, tc.dir, rot, want)
		}
		// 反查必须闭环:按这个 rotation 读回来的方向就是我们要的方向。
		if got, _ := flagDirectionOf(tc.fam, rot); got != tc.dir {
			t.Errorf("round-trip: rotation %v reads back as %q, want %q", rot, got, tc.dir)
		}
	}
}

// 挤到无处可放时宁可不动(记 no-free-slot),绝不硬塞一个还撞的位置 —— 挪了还撞
// 等于白改,还白担一次网络手术的风险。
func TestPlanDestagger_NoFreeSlotIsSkippedNotForced(t *testing.T) {
	g1, w1 := dsFlag("g1", "GND", 100, 200, 100, 240, "ground")
	g2, w2 := dsFlag("g2", "GND", 110, 200, 110, 240, "ground")
	// 四周用大器件封死,任何方向/桩长都撞。
	wall := func(id string, x0, y0, x1, y1 float64) layoutComp {
		return layoutComp{ID: id, ComponentType: "part", Designator: id, BBox: bb(x0, y0, x1, y1)}
	}
	comps := []layoutComp{
		g1, g2,
		wall("W1", -200, 250, 400, 600),  // 上
		wall("W2", -200, -400, 400, 195), // 下
		wall("W3", -200, 195, 95, 250),   // 左
		wall("W4", 118, 195, 400, 250),   // 右
	}
	plan := planDestagger(comps, []schGroupWire{w1, w2}, 1)
	for _, m := range plan.Moves {
		box := m.NewBBox
		for _, c := range comps {
			if c.ComponentType != "part" {
				continue
			}
			if ox, oy, hit := overlapExtent(box, *c.BBox); hit && math.Min(ox, oy) > 1 {
				t.Fatalf("planner placed %s into %s (overlap %.1f×%.1f) — must skip instead",
					m.FlagID, c.ID, ox, oy)
			}
		}
	}
	if len(plan.Moves) == 0 && len(plan.Skips) == 0 {
		t.Fatal("a boxed-in overlap must be reported as a skip, not silently dropped")
	}
}

// marker×part 的重叠只许挪 marker:器件位置是布局决定的,不归本命令管。
func TestPlanDestagger_NeverMovesParts(t *testing.T) {
	g, w := dsFlag("g1", "GND", 100, 200, 100, 240, "ground")
	part := layoutComp{ID: "R1", ComponentType: "part", Designator: "R1", BBox: bb(90, 235, 130, 275)}
	plan := planDestagger([]layoutComp{g, part}, []schGroupWire{w}, 1)
	for _, m := range plan.Moves {
		if m.FlagID == "R1" {
			t.Fatalf("planner must never move a part: %+v", m)
		}
	}
	for _, s := range plan.Skips {
		if s.FlagID == "R1" {
			t.Fatalf("a part must not even appear as a skip candidate: %+v", s)
		}
	}
}
