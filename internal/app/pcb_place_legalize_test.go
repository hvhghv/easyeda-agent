package app

import (
	"strings"
	"testing"
)

// legalize 测试的合成板：几何刻意简单到能手算。
// 板框 0..1000 × -1000..0(y-UP,负半轴),两件已就位的固定件 + 一件被规划移动的件。

func legalizeTestSnap() *boardSnapshot {
	mk := func(id, des string, x, y, w, h float64, rot float64, pads []boardPad) boardComp {
		return boardComp{
			ID: id, Designator: des, Layer: 1, X: x, Y: y, Rotation: rot,
			BBox: &layoutBBox{MinX: x - w/2, MinY: y - h/2, MaxX: x + w/2, MaxY: y + h/2},
			Pads: pads,
		}
	}
	return &boardSnapshot{
		Outline: &boardOutline{BBox: layoutBBox{MinX: 0, MinY: -1000, MaxX: 1000, MaxY: 0}},
		Components: []boardComp{
			// J1:固定大件,占据右下角一片。
			mk("j1", "J1", 800, -800, 300, 300, 0, []boardPad{
				{Number: "1", Net: "VBUS", Layer: 1, X: 750, Y: -800, W: 30, H: 30},
				{Number: "2", Net: "GND", Layer: 1, X: 850, Y: -800, W: 30, H: 30},
			}),
			// U1:固定件,板中央。
			mk("u1", "U1", 400, -400, 100, 100, 0, []boardPad{
				{Number: "1", Net: "3V3", Layer: 1, X: 370, Y: -400, W: 20, H: 20},
			}),
			// C9:待移动的 2 脚件,长条形(120×40)——转 90° 后 bbox 换形,是 C36 事故的微缩版。
			mk("c9", "C9", 100, -100, 120, 40, 0, []boardPad{
				{Number: "1", Net: "3V3", Layer: 1, X: 55, Y: -100, W: 24, H: 24},
				{Number: "2", Net: "GND", Layer: 1, X: 145, Y: -100, W: 24, H: 24},
			}),
		},
	}
}

func TestLegalize_NoConflictPassesThrough(t *testing.T) {
	snap := legalizeTestSnap()
	moves := []apMove{{ID: "c9", Designator: "C9", NewX: 400, NewY: -700}}
	out, diags, res := legalizeConstrainedMoves(snap, moves)
	if len(out) != 1 || out[0].NewX != 400 || out[0].NewY != -700 {
		t.Fatalf("a legal move must pass through untouched, got %+v", out)
	}
	if res.Adjusted != 0 || res.Dropped != 0 || len(diags) != 0 {
		t.Errorf("no conflict but adjusted=%d dropped=%d diags=%v", res.Adjusted, res.Dropped, diags)
	}
	if res.Checked != 1 {
		t.Errorf("checked=%d, want 1", res.Checked)
	}
}

func TestLegalize_OverlapIsRelocated(t *testing.T) {
	// 目标点直接压在 U1 上 → 必须被重定位到近旁合法位,而不是照单落子。
	snap := legalizeTestSnap()
	moves := []apMove{{ID: "c9", Designator: "C9", NewX: 400, NewY: -400}}
	out, diags, res := legalizeConstrainedMoves(snap, moves)
	if len(out) != 1 {
		t.Fatalf("move dropped, want relocated: diags=%v", diags)
	}
	if res.Adjusted != 1 {
		t.Fatalf("adjusted=%d, want 1 (planned spot sits on U1)", res.Adjusted)
	}
	if out[0].NewX == 400 && out[0].NewY == -400 {
		t.Fatal("target unchanged — the overlap with U1 was not seen")
	}
	// 重定位后的落点必须在 5mil 锚点格上(与 snapMovesToAnchorGrid 同约定)。
	if int(out[0].NewX)%5 != 0 || int(out[0].NewY)%5 != 0 {
		t.Errorf("relocated target off the 5mil anchor grid: (%g,%g)", out[0].NewX, out[0].NewY)
	}
	// 复算确认:套用后的虚拟态零新增 blocking。
	after := layoutOfVirtual(buildVirtualComps(snap, out), outlineBBoxOf(snap), 6)
	if n := len(after.Overlaps) + len(after.Shorts) + len(after.OutsideOutline); n != 0 {
		t.Errorf("relocated spot still has %d blocking finding(s)", n)
	}
}

func TestLegalize_RotationInducedOverlapIsCaught(t *testing.T) {
	// C36 事故微缩版:平移目标本身不压件,但同一 move 带了 90° 转向,长条件
	// bbox 换形后扫到 U1。平移-only 的虚拟态看不见这种冲突 —— 旋转感知必须看见。
	snap := legalizeTestSnap()
	// C9 的焊盘在 ±45 x 偏移,转 90° 后展开到 ±45 y 偏移。落点 (370,-350):
	// 不转时两盘在 (325,-350)/(415,-350),离 U1 的盘(370,-400)都不挨;
	// 转 90° 后一盘落到 (370,-395),与 U1 的盘(360..380×-410..-390)相叠。
	// 平移-only 的虚拟态看不见这种冲突 —— 旋转感知必须看见(本体代理口径:
	// 重叠/间距按焊盘并集判,所以冲突必须做到盘级)。
	moves := []apMove{{ID: "c9", Designator: "C9", NewX: 370, NewY: -350, NewRot: 90, SetRot: true}}
	out, _, res := legalizeConstrainedMoves(snap, moves)
	if res.Adjusted+res.Dropped == 0 {
		t.Fatalf("rotation-induced overlap went unseen (adjusted=%d dropped=%d out=%+v)", res.Adjusted, res.Dropped, out)
	}
}

func TestLegalize_CrossNetShortIsRejected(t *testing.T) {
	// 落点让 C9 的 GND 焊盘正好压上 J1 的 VBUS 焊盘 → 跨网短路,必须动。
	// C9 pad1 相对锚点 -45,pad 落 J1.VBUS(750,-800) → 锚点 (795,-800)。
	snap := legalizeTestSnap()
	moves := []apMove{{ID: "c9", Designator: "C9", NewX: 795, NewY: -800}}
	_, diags, res := legalizeConstrainedMoves(snap, moves)
	if res.Adjusted+res.Dropped == 0 {
		t.Fatalf("cross-net pad short went unseen: diags=%v", diags)
	}
}

func TestLegalize_UnresolvableIsDroppedNotForced(t *testing.T) {
	// 板框缩到只装得下固定件:被移动件无处可去 → 必须弃子(保持原位),
	// 而不是硬塞或截断到第三个位置。
	snap := legalizeTestSnap()
	snap.Outline = &boardOutline{BBox: layoutBBox{MinX: 340, MinY: -460, MaxX: 460, MaxY: -340}}
	// 板框只比 U1(350..450)大 10mil 一圈:C9(120×40)塞进去必压 U1,
	// 躲开 U1 的位置焊盘又必然出板框 —— 无解,只能弃。
	moves := []apMove{{ID: "c9", Designator: "C9", NewX: 400, NewY: -400}}
	out, diags, res := legalizeConstrainedMoves(snap, moves)
	if res.Dropped != 1 || len(out) != 0 {
		t.Fatalf("dropped=%d out=%d, want the move dropped entirely", res.Dropped, len(out))
	}
	found := false
	for _, d := range diags {
		if d.Designator == "C9" && strings.Contains(d.Reason, "legalize:dropped") {
			found = true
		}
	}
	if !found {
		t.Errorf("dropped move must be named in diags, got %v", diags)
	}
}

func TestLegalize_PreexistingBlockingIsNotOurProblem(t *testing.T) {
	// 板上本来就有一对重叠的固定件:合法化不该动它们,也不该因此拦下无关的 move。
	snap := legalizeTestSnap()
	dup := snap.Components[1]
	dup.ID, dup.Designator = "u2", "U2"
	dup.Pads = nil
	snap.Components = append(snap.Components, dup) // U2 与 U1 完全重叠(基线问题)
	moves := []apMove{{ID: "c9", Designator: "C9", NewX: 700, NewY: -200}}
	out, _, res := legalizeConstrainedMoves(snap, moves)
	if len(out) != 1 || res.Adjusted != 0 || res.Dropped != 0 {
		t.Fatalf("pre-existing overlap gated an unrelated move: adjusted=%d dropped=%d", res.Adjusted, res.Dropped)
	}
}

func TestLegalize_DroppedPartnerCascadesToFollowers(t *testing.T) {
	// 幽灵跟随(#21 真板实锤):J2 的 move 被合法化弃掉,跟着它规划的 TVS/ESD
	// 却停在「伙伴本要去而没去」的半路 —— protection 从贴身变 800+mil。
	// 血缘级联:伙伴被弃,跟随者连坐,哪怕跟随者自己的 move 完全合法 ——
	// 留在原位才保得住与伙伴的原始贴身关系。
	snap := legalizeTestSnap()
	moves := []apMove{
		// J1 的目标在板外深处(距板框 700mil > 螺旋上限 600):无解,必被弃。
		{ID: "j1", Designator: "J1", NewX: 1700, NewY: -500},
		// C9 跟着 J1 规划;它自己的落点完全合法。
		{ID: "c9", Designator: "C9", NewX: 400, NewY: -700, FollowsID: "j1"},
	}
	out, diags, res := legalizeConstrainedMoves(snap, moves)
	if res.Dropped != 2 {
		t.Fatalf("dropped=%d, want both the partner and its follower", res.Dropped)
	}
	if len(out) != 0 {
		t.Fatalf("follower move survived its partner's drop: %+v", out)
	}
	cascaded := false
	for _, d := range diags {
		if d.Designator == "C9" && strings.Contains(d.Reason, "follows J1") {
			cascaded = true
		}
	}
	if !cascaded {
		t.Errorf("cascade drop must be named in diags, got %v", diags)
	}
}

func TestLegalize_NilSnapshotPassesThroughHonestly(t *testing.T) {
	moves := []apMove{{ID: "c9", Designator: "C9", NewX: 1, NewY: 2}}
	out, diags, res := legalizeConstrainedMoves(nil, moves)
	if len(out) != 1 || len(diags) != 0 {
		t.Fatalf("nil snapshot must pass moves through, got out=%d diags=%d", len(out), len(diags))
	}
	if res.Checked != 0 {
		t.Errorf("checked=%d — without a snapshot nothing was checked, the result must say so", res.Checked)
	}
}
