package app

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
)

// ── 关系约束求解器:纯函数层(issue #180 P2)────────────────────────────────

func bslTestBlock(t *testing.T, extra map[string]any) blocks.Block {
	t.Helper()
	raw := map[string]any{
		"id":   "block.bsl_test",
		"desc": "t",
		"parts": map[string]any{
			"U":     map[string]any{"part": "ic.ch340c", "qty": 1},
			"J":     map[string]any{"part": "conn.usb_c_16p", "qty": 1},
			"C_VCC": map[string]any{"part": "cap.100nf_0402", "qty": 1},
			"C_V3":  map[string]any{"part": "cap.100nf_0402", "qty": 1},
			"R_A":   map[string]any{"part": "res.5k1_0402", "qty": 1},
			"R_B":   map[string]any{"part": "res.5k1_0402", "qty": 1},
		},
		"internal_nets": []any{
			[]any{"U.VCC", "C_VCC.1", "J.VBUS*"},
			[]any{"U.V3", "C_V3.1"},
			[]any{"U.D+", "J.DP1"},
			[]any{"U.D-", "J.DN1"},
			[]any{"J.CC1", "R_A.1"},
			[]any{"J.CC2", "R_B.1"},
		},
	}
	for k, v := range extra {
		raw[k] = v
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var blk blocks.Block
	if err := json.Unmarshal(b, &blk); err != nil {
		t.Fatal(err)
	}
	blk.Raw = b
	return blk
}

// 锚件 = 被 attach 指向最多的 role —— 「谁是主芯片」的电路学定义。
func TestBslAnchorRole_MostAttachedWins(t *testing.T) {
	blk := bslTestBlock(t, nil)
	rel := bslRelations{Attach: map[string]string{"C_VCC": "U.VCC", "C_V3": "U.V3"}}
	got, err := bslAnchorRole(blk, rel, bslBlockNets(blk))
	if err != nil {
		t.Fatal(err)
	}
	if got != "U" {
		t.Errorf("锚件应为被 attach 指向 2 次的 U, got %q", got)
	}
}

// 没有 attach 时退到半宽最大者(conn 90 > cap/res 50);ic 100 更大。
func TestBslAnchorRole_FallsBackToBiggest(t *testing.T) {
	blk := bslTestBlock(t, nil)
	got, err := bslAnchorRole(blk, bslRelations{}, bslBlockNets(blk))
	if err != nil {
		t.Fatal(err)
	}
	if got != "U" { // ic.ch340c 半宽 100,最大
		t.Errorf("无 attach 时应取半宽最大的 U, got %q", got)
	}
}

// 显式 anchor 指向不存在的 role → **拒绝出计划**,绝不悄悄回退推导。
func TestBslAnchorRole_BogusExplicitAnchorIsFailClosed(t *testing.T) {
	blk := bslTestBlock(t, nil)
	_, err := bslAnchorRole(blk, bslRelations{Anchor: "NOPE"}, bslBlockNets(blk))
	if err == nil {
		t.Fatal("不存在的 anchor 必须报错,不能回退推导")
	}
}

// 同输入同输出:全并列时按字典序,保证计划可复现。
func TestBslAnchorRole_DeterministicTieBreak(t *testing.T) {
	raw := map[string]any{
		"id": "block.tie", "desc": "t",
		"parts": map[string]any{
			"B": map[string]any{"part": "res.5k1_0402", "qty": 1},
			"A": map[string]any{"part": "res.5k1_0402", "qty": 1},
		},
		"internal_nets": []any{[]any{"A.1", "B.1"}},
	}
	j, _ := json.Marshal(raw)
	var blk blocks.Block
	_ = json.Unmarshal(j, &blk)
	blk.Raw = j
	for i := 0; i < 20; i++ { // map 遍历顺序随机,跑多次证明稳定
		got, err := bslAnchorRole(blk, bslRelations{}, bslBlockNets(blk))
		if err != nil || got != "A" {
			t.Fatalf("字典序 tie-break 不稳定: got=%q err=%v", got, err)
		}
	}
}

// 间距必须**跟着数据变**,不是常量:跨接网越多通道越宽、网名越长标签越宽。
func TestBslFlowGap_ScalesWithDataNotConstant(t *testing.T) {
	// 三项取 max,所以要在 marker 伸出**不主导**的量级上验通道项:
	// reach 下限是 schStubLen+31=61,两侧 122;要让通道项当家需 >8 条跨接网。
	oneLane := bslFlowGap(1, bslReach("D"), bslReach("D"))
	manyLanes := bslFlowGap(12, bslReach("D"), bslReach("D"))
	if !(manyLanes > oneLane) {
		t.Errorf("跨接网多到主导时通道必须变宽: 1条=%v 12条=%v", oneLane, manyLanes)
	}
	// 而在通道项不主导时,间距由 marker 伸出决定 —— 两项各自当家的区间都要覆盖。
	// 两支标签朝着对方伸,中间还要留一个视觉间隙 —— 否则首尾相接,看着就是黏成一条。
	if got, want := oneLane, 2*bslReach("D")+bslPartGap; got != want {
		t.Errorf("少量跨接网时应由 marker 伸出 + 视觉间隙决定: got %v want %v", got, want)
	}
	shortNet := bslFlowGap(1, bslReach("D"), bslReach("D"))
	longNet := bslFlowGap(1, bslReach("USB_DP_SHIELD"), bslReach("USB_DP_SHIELD"))
	if !(longNet > shortNet) {
		t.Errorf("网名变长时 marker 伸出必须变大: 短=%v 长=%v", shortNet, longNet)
	}
	// 下限:任何情况下不小于视觉间隙
	if bslFlowGap(0, 0, 0) < bslPartGap {
		t.Errorf("间距下限必须是 bslPartGap")
	}
}

// reach 必须 = 桩长 + 标签实测宽度,且与 relayout 消费同一个桩长常量。
func TestBslReach_SharesStubLengthWithRelayout(t *testing.T) {
	net := "GND"
	if got, want := bslReach(net), schStubLen+relayoutPortWidth(net); got != want {
		t.Errorf("reach = %v, want %v", got, want)
	}
	if schStubLen != 30 {
		t.Errorf("桩长常量变了(%v)—— relayout 的 connect_pin offset 必须同步", schStubLen)
	}
}

// 跨接网计数:只数**两端都沾**的网。
func TestBslCrossNets(t *testing.T) {
	blk := bslTestBlock(t, nil)
	nets := bslBlockNets(blk)
	if got := bslCrossNets(nets, "U", "J"); got != 3 { // VBUS/D+/D-
		t.Errorf("U↔J 应有 3 条跨接网, got %d", got)
	}
	if got := bslCrossNets(nets, "C_VCC", "R_A"); got != 0 {
		t.Errorf("无关的两件不该有跨接网, got %d", got)
	}
}

// 贴脚侧向(ADR-0003 §2.5 的 A 方案):引脚在**左/右列时件放到宿主的上/下侧** ——
// 同侧是 marker 通道,把去耦塞进去就是整块重叠;引脚在上/下沿时才同侧。
func TestBslAttachSide_LeavesTheMarkerChannel(t *testing.T) {
	for _, c := range []struct {
		pinSide, orient string
		wantSide        string
		wantVertical    bool
	}{
		{"left", "", "up", true},  // 左列的脚 → 让开左侧通道,走上/下
		{"right", "", "up", true}, // 右列同理
		{"up", "", "up", false},   // 上沿的脚本来就朝上,同侧不冲突
		{"down", "", "down", false},
		{"up", "vertical", "up", true},      // orient 只管横竖
		{"left", "horizontal", "up", false}, // 侧向不再由 orient 决定
	} {
		side, vertical := bslAttachSide(c.pinSide, c.orient)
		if side != c.wantSide || vertical != c.wantVertical {
			t.Errorf("pinSide=%q orient=%q → (%q,%v), want (%q,%v)",
				c.pinSide, c.orient, side, vertical, c.wantSide, c.wantVertical)
		}
	}
	if side, _ := bslAttachSide("left", ""); side == "left" {
		t.Error("左列的脚绝不能再把去耦放回左侧 —— 那是 marker 通道")
	}
}

// 上下二选一:走离引脚最近的那一头。
func TestBslAttachClearSide_TakesTheShorterWayOut(t *testing.T) {
	host := layoutBBox{MinX: 654, MinY: 414, MaxX: 726, MaxY: 506}
	if got := bslAttachClearSide(500, host); got != "up" {
		t.Errorf("引脚在上半 → up: %q", got)
	}
	if got := bslAttachClearSide(420, host); got != "down" {
		t.Errorf("引脚在下半 → down: %q", got)
	}
	if got := bslAttachClearSide(420, layoutBBox{}); got != "up" {
		t.Errorf("读不到宿主高度时退回 up: %q", got)
	}
}

// 种子点:上/下侧必须**让开宿主本体**(引脚的 y 还在本体高度里,从引脚算会把件按在芯片上),
// x 对齐目标引脚那一列,让人一眼看出它属于哪只脚。
func TestBslAttachSeed_ClearsTheHostBody(t *testing.T) {
	host := layoutBBox{MinX: 654, MinY: 414, MaxX: 726, MaxY: 506}
	const pinX, pinY, ownHalf float64 = 654, 468, 10

	x, y := bslAttachSeed(pinX, pinY, host, "up", ownHalf)
	if x != pinX {
		t.Errorf("x 应对齐目标引脚那一列: %v", x)
	}
	if want := host.MaxY + bslPartGap + ownHalf; y != want {
		t.Errorf("上贴 y = %v, want %v(本体上沿 + 间隙 + 半宽)", y, want)
	}
	if y <= host.MaxY {
		t.Error("上贴必须整体在本体之上,否则就是压在芯片身上")
	}
	_, dy := bslAttachSeed(pinX, pinY, host, "down", ownHalf)
	if want := host.MinY - bslPartGap - ownHalf; dy != want {
		t.Errorf("下贴 y = %v, want %v", dy, want)
	}
	// 离脚仍然近:比一个 marker 的伸出还短,才叫"贴"。
	if y-pinY > bslReach("3V3")+host.MaxY-host.MinY {
		t.Errorf("贴脚距离不该大到读不出归属: %v", y-pinY)
	}
	// 左/右侧(引脚在上下沿的情形)仍从引脚算,且左右对称。
	rx, ry := bslAttachSeed(pinX, pinY, host, "right", ownHalf)
	lx, _ := bslAttachSeed(pinX, pinY, host, "left", ownHalf)
	if ry != pinY || math.Abs((pinX-lx)-(rx-pinX)) > 0.001 {
		t.Errorf("左右贴应与引脚齐平且对称: left=%v right=%v y=%v", lx, rx, ry)
	}
}

// 并列 pitch 跟着成员宽度与组内最长网名走。
func TestBslPairPitch(t *testing.T) {
	narrow := bslPairPitch(20, []string{"CC1"})
	wide := bslPairPitch(60, []string{"CC1"})
	if !(wide > narrow) {
		t.Errorf("成员越宽 pitch 越大: %v vs %v", narrow, wide)
	}
	longNet := bslPairPitch(20, []string{"CC1", "USB_CONFIG_CHANNEL"})
	if !(longNet > narrow) {
		t.Errorf("组内有长网名时 pitch 必须变大: %v vs %v", narrow, longNet)
	}
}

// 关系投影:legacy 模板不该被当成关系形态。
func TestBslRelationsFrom_RejectsLegacy(t *testing.T) {
	legacy := &blocks.SchematicLayout{Roles: map[string]blocks.SchematicLayoutHint{"U": {DX: 0, DY: 0}}}
	if _, ok := bslRelationsFrom(legacy); ok {
		t.Error("legacy 模板不该投影成关系")
	}
	rel := &blocks.SchematicLayout{Flow: []string{"J", "U"}}
	got, ok := bslRelationsFrom(rel)
	if !ok || len(got.Flow) != 2 {
		t.Errorf("关系模板投影失败: %+v ok=%v", got, ok)
	}
	// nil 安全
	if _, ok := bslRelationsFrom(nil); ok {
		t.Error("nil 模板不该投影成关系")
	}
}

// ── 连锁推让(marker 通道不够就把器件让开)──────────────────────────────────
//
// 锚件统一用 [0,100]×[-60,60],左侧推让;件的 box 由 bslPartBox 算:
// tvs/res/cap 半宽 10、conn 半宽 90。所有期望值都是手算出来的边到边间隙,
// 不是跑一遍记下来的 —— 判据变了这些数就该跟着变。

func bslPushTestAnchor() layoutBBox {
	return layoutBBox{MinX: 0, MinY: -60, MaxX: 100, MaxY: 60}
}

// plan 里放一串件;role 与位号同名,便于断言读起来是位号。
func bslPushTestPlan(anchorRole string, parts ...bapPlacement) *bapPlan {
	return &bapPlan{AnchorRole: anchorRole, Placements: parts}
}

func bslPart(role, key string, x, y float64) bapPlacement {
	return bapPlacement{Role: role, PartKey: key, Designator: role, X: x, Y: y}
}

func bslMoveOf(t *testing.T, units []bslPushUnit, res bslPushResult, label string) float64 {
	t.Helper()
	for i, u := range units {
		if u.Label == label {
			return res.Move[i]
		}
	}
	t.Fatalf("unit %q 不在列表里: %+v", label, units)
	return 0
}

// 连锁:推 D1 会把它挤到 J1 身上 —— J1 必须跟着让,让量 = D1 的位移减去两者的富余。
func TestBslPushSolve_CascadesToTheNextPart(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("D1", "tvs.sm712_sot23", -60, 0), // box [-70,-50]
		bslPart("J1", "conn.usb_c_16p", -200, 0), // box [-290,-110]
	)
	units := bslPushUnitsOf(plan, bslRelations{}, bslEstimatedBox)
	res := bslPushSolve(units, nil, nil, bslPushTestAnchor(), "left", 100)

	// D1 与锚件只有 50,要 100 → 让 50;D1 与 J1 有 40,富余 40−20=20 → J1 让 30。
	if got := bslMoveOf(t, units, res, "D1"); got != -50 {
		t.Errorf("D1 该让 50: %v", got)
	}
	if got := bslMoveOf(t, units, res, "J1"); got != -30 {
		t.Errorf("J1 该连锁让 30(50 − 富余 20): %v", got)
	}
	if res.Capped != "" {
		t.Errorf("空地上不该报被顶住: %q", res.Capped)
	}
}

// 富余吃得下就不传播:外侧件离得远,链到此为止(推让不做无谓的全局放大)。
func TestBslPushSolve_SlackAbsorbsTheChain(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("D1", "tvs.sm712_sot23", -60, 0),
		bslPart("J1", "conn.usb_c_16p", -300, 0), // 与 D1 间隙 140,富余 120 > 50
	)
	units := bslPushUnitsOf(plan, bslRelations{}, bslEstimatedBox)
	res := bslPushSolve(units, nil, nil, bslPushTestAnchor(), "left", 100)
	if got := bslMoveOf(t, units, res, "J1"); got != 0 {
		t.Errorf("富余够时 J1 不该动: %v", got)
	}
}

// 顶住时**整条链一起截短**:绝不出现「内侧推了、外侧没让」的半推重叠。
func TestBslPushSolve_ClampsWholeChainNeverHalfPushes(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("D1", "tvs.sm712_sot23", -60, 0),
		bslPart("J1", "conn.usb_c_16p", -200, 0),
	)
	usable := &layoutBBox{MinX: -300, MinY: -500, MaxX: 1000, MaxY: 500}
	units := bslPushUnitsOf(plan, bslRelations{}, bslEstimatedBox)
	res := bslPushSolve(units, nil, usable, bslPushTestAnchor(), "left", 100)

	// J1 左沿 −290,可用区 −300 → 只能让 10;D1 因此只能让 10+富余 20 = 30。
	dJ := bslMoveOf(t, units, res, "J1")
	dD := bslMoveOf(t, units, res, "D1")
	if dJ != -10 || dD != -30 {
		t.Fatalf("整条链该按最紧的一级截短: D1=%v J1=%v(期望 -30 / -10)", dD, dJ)
	}
	if res.Capped == "" {
		t.Error("推不满必须如实说被谁顶住")
	}
	// 落位后两件仍隔着 bslPartGap —— 推让不许自己制造 part×part 重叠。
	dBox := layoutBBox{MinX: -70 + dD, MaxX: -50 + dD}
	jBox := layoutBBox{MinX: -290 + dJ, MaxX: -110 + dJ}
	if gap := dBox.MinX - jBox.MaxX; gap < bslPartGap {
		t.Errorf("推让把 D1 按到了 J1 身上: 间隙 %v < %v", gap, bslPartGap)
	}
	if jBox.MinX < usable.MinX {
		t.Errorf("J1 被推出可用区: %v < %v", jBox.MinX, usable.MinX)
	}
}

// 外部图元(别的块的件)是推不动的墙:链在它面前截短,不许压上去。
func TestBslPushSolve_ForeignPartIsAWall(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("D1", "tvs.sm712_sot23", -60, 0),
	)
	wall := layoutBBox{MinX: -200, MinY: -30, MaxX: -100, MaxY: 30}
	units := bslPushUnitsOf(plan, bslRelations{}, bslEstimatedBox)
	res := bslPushSolve(units, []layoutBBox{wall}, nil, bslPushTestAnchor(), "left", 100)

	// D1 左沿 −70 到墙右沿 −100 有 30,留 20 → 只能让 10。
	if got := bslMoveOf(t, units, res, "D1"); got != -10 {
		t.Errorf("撞墙该只让 10: %v", got)
	}
	if res.Capped == "" {
		t.Error("被外部图元顶住必须如实说")
	}
}

// pair 组整体平移:等距是电路语义,推让不许把它拆散。
func TestBslPushSolve_PairMovesAsOneRigidBody(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("R_A", "res.5k1_0402", -80, 0),
		bslPart("R_B", "res.5k1_0402", -40, 0),
	)
	rel := bslRelations{Pair: [][]string{{"R_A", "R_B"}}}
	units := bslPushUnitsOf(plan, rel, bslEstimatedBox)
	if len(units) != 1 || len(units[0].Idx) != 2 {
		t.Fatalf("pair 组该合成一个刚体: %+v", units)
	}
	res := bslPushSolve(units, nil, nil, bslPushTestAnchor(), "left", 100)
	// 组的 box 是并集 [-90,-30],与锚件间隙 30 → 让 70。
	if res.Move[0] != -70 {
		t.Fatalf("pair 组该整体让 70: %v", res.Move[0])
	}
	for _, i := range units[0].Idx {
		plan.Placements[i].X += res.Move[0]
	}
	if pitch := plan.Placements[2].X - plan.Placements[1].X; pitch != 40 {
		t.Errorf("推让后 pair 的等距被破坏: %v(原 40)", pitch)
	}
	if math.Mod(plan.Placements[1].X, schAnchorGrid) != 0 {
		t.Errorf("推让后必须仍在连接网格上: %v", plan.Placements[1].X)
	}
}

// attach 件(贴脚去耦)永不推;锚件与已落地的件也不进推让列表。
func TestBslPushUnitsOf_SkipsAnchorAttachAndPlaced(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("C_VCC", "cap.100nf_0402", -40, 20),
		bslPart("D1", "tvs.sm712_sot23", -60, 0),
	)
	plan.Placements[0].PrimitiveID = "prim-anchor"
	landed := bslPart("R9", "res.5k1_0402", -120, 0)
	landed.PrimitiveID = "prim-old"
	plan.Placements = append(plan.Placements, landed)

	units := bslPushUnitsOf(plan, bslRelations{Attach: map[string]string{"C_VCC": "U.VCC"}}, bslEstimatedBox)
	if len(units) != 1 || units[0].Label != "D1" {
		t.Fatalf("只有 D1 该可推: %+v", units)
	}
}

// 通道带外的件不推:marker 占的是与锚件同高的一条带,推带外的件既不腾空间
// 又破坏关系语义(第一版要推下方的 pair 电阻,就是这个错)。
func TestBslPushSolve_OnlyPartsInTheLaneBand(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("R5", "res.5k1_0402", -60, -200), // 锚件下方 200,不在带上
	)
	units := bslPushUnitsOf(plan, bslRelations{}, bslEstimatedBox)
	res := bslPushSolve(units, nil, nil, bslPushTestAnchor(), "left", 100)
	if res.Head >= 0 {
		t.Errorf("带外的件不该被当成挡路的: head=%d", res.Head)
	}
	if got := bslMoveOf(t, units, res, "R5"); got != 0 {
		t.Errorf("带外的件不该被推: %v", got)
	}
}

// 同一条带上并排两件都在通道里 —— 只推最近的那件等于没腾出通道,两件都要让。
func TestBslPushSolve_PushesEveryPartInsideTheChannel(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("D1", "tvs.sm712_sot23", -60, 30),
		bslPart("D2", "tvs.sm712_sot23", -60, -30),
	)
	units := bslPushUnitsOf(plan, bslRelations{}, bslEstimatedBox)
	res := bslPushSolve(units, nil, nil, bslPushTestAnchor(), "left", 100)
	if a, b := bslMoveOf(t, units, res, "D1"), bslMoveOf(t, units, res, "D2"); a != -50 || b != -50 {
		t.Errorf("通道里的两件都该让 50: D1=%v D2=%v", a, b)
	}
}

// 右侧同样成立(方向对称,不是只为左侧写的特例)。
func TestBslPushSolve_RightSideIsSymmetric(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("D1", "tvs.sm712_sot23", 160, 0), // box [150,170],与锚件间隙 50
		bslPart("J1", "conn.usb_c_16p", 300, 0),  // box [210,390],与 D1 间隙 40
	)
	units := bslPushUnitsOf(plan, bslRelations{}, bslEstimatedBox)
	res := bslPushSolve(units, nil, nil, bslPushTestAnchor(), "right", 100)
	if a, b := bslMoveOf(t, units, res, "D1"), bslMoveOf(t, units, res, "J1"); a != 50 || b != 30 {
		t.Errorf("右侧该镜像成立: D1=%v J1=%v(期望 +50 / +30)", a, b)
	}
}

// 收敛与确定性:同输入同输出,且推完一轮再解一次不该再动(否则真机上会来回抖)。
func TestBslPushSolve_DeterministicAndConverges(t *testing.T) {
	build := func() *bapPlan {
		return bslPushTestPlan("U",
			bslPart("U", "ic.ch340c", 50, 0),
			bslPart("D1", "tvs.sm712_sot23", -60, 0),
			bslPart("J1", "conn.usb_c_16p", -200, 0),
		)
	}
	p1, p2 := build(), build()
	u1, u2 := bslPushUnitsOf(p1, bslRelations{}, bslEstimatedBox), bslPushUnitsOf(p2, bslRelations{}, bslEstimatedBox)
	r1 := bslPushSolve(u1, nil, nil, bslPushTestAnchor(), "left", 100)
	r2 := bslPushSolve(u2, nil, nil, bslPushTestAnchor(), "left", 100)
	for i := range r1.Move {
		if r1.Move[i] != r2.Move[i] {
			t.Fatalf("同输入必须同输出: %v vs %v", r1.Move, r2.Move)
		}
	}
	for i, m := range r1.Move {
		for _, idx := range u1[i].Idx {
			p1.Placements[idx].X += m
		}
	}
	again := bslPushSolve(bslPushUnitsOf(p1, bslRelations{}, bslEstimatedBox), nil, nil, bslPushTestAnchor(), "left", 100)
	for _, m := range again.Move {
		if m != 0 {
			t.Errorf("推让该一轮收敛,第二轮还在动: %v", again.Move)
			break
		}
	}
}

// 间隙必须算上被推件自己的身宽 —— 旧版 bapHalfExtentFn(0,nil) 恒返 0,
// 每次都少推一个半宽(conn 差 90),这是「判定与生成同一把尺」的又一次显形。
func TestBslPushSolve_GapCountsThePartsOwnWidth(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("J1", "conn.usb_c_16p", -140, 0), // 中心距锚件 140,右沿只有 −50
	)
	units := bslPushUnitsOf(plan, bslRelations{}, bslEstimatedBox)
	res := bslPushSolve(units, nil, nil, bslPushTestAnchor(), "left", 100)
	if res.Gap != 50 {
		t.Errorf("间隙该按 J1 的右沿算(−50)而不是中心: %v", res.Gap)
	}
	if got := bslMoveOf(t, units, res, "J1"); got != -50 {
		t.Errorf("该让 50: %v", got)
	}
}

// 端到端:从 plan.Nets + 实测引脚数出每侧 marker 数,再走连锁推让写回 plan。
func TestBslExpandForMarkers_CountsMarkersAndPushes(t *testing.T) {
	plan := bslPushTestPlan("U",
		bslPart("U", "ic.ch340c", 50, 0),
		bslPart("D1", "tvs.sm712_sot23", -60, 0),
	)
	plan.Placements[0].Designator = "U1"
	plan.Nets = []bapNet{{Net: "USB_DP", Members: []string{"U1:5", "D1:1"}},
		{Net: "USB_DM", Members: []string{"U1:6", "D1:2"}}}
	// 两只脚 y 只差 10,而 netport 标签 11 高 —— 一条 lane 装不下,要排 2 条。
	pins := map[string]acPin{
		"5": {X: 5, Y: 5, Designator: "U1", PinNumber: "5", PinName: "D+"},
		"6": {X: 5, Y: -5, Designator: "U1", PinNumber: "6", PinName: "D-"},
		"9": {X: 95, Y: 0, Designator: "U1", PinNumber: "9", PinName: "NC"}, // 不在网里,不算
	}
	var log strings.Builder
	notes := bslExpandForMarkers(plan, bslRelations{}, bslPushTestAnchor(), pins, nil, nil, &log)

	// 左侧 2 支 marker 排 2 条 lane;通道 = 我这侧的伸出 + 另一条 lane 的步长
	// + D1 自己的 marker 伸出 + 视觉间隙 = 214,与 D1 只有 50 → 让 160(落格)。
	// lane 步长现在含网名(与 autoconnect 的 laneStepFor 同一个数),通道更深。
	if plan.Placements[1].X != -260 {
		t.Errorf("D1 该被推到 −260: %v", plan.Placements[1].X)
	}
	if len(notes) != 0 {
		t.Errorf("空地上推得动就不该有告警: %v", notes)
	}
	if !strings.Contains(log.String(), "D1 让 200") {
		t.Errorf("日志要把算术写清楚: %q", log.String())
	}
}

// ── 实测推让(place 之后 connect_pin 之前,用真实 bbox 再解一次)──────────────
//
// 几何照抄真机(ceshi 单块 ch340c)的实测值,包括那条最要命的事实:**锚点不在 bbox
// 中心** —— D1 锚点 370,bbox 却是 [358,406],右侧伸出 36。估算尺(tvs 半宽 10)
// 在这里必然算错通道,这正是要用实测再解一次的原因。

const bslLiveGeometry = `{"components":[
 {"componentType":"sheet","bbox":{"minX":0,"minY":0,"maxX":1170,"maxY":825}},
 {"componentType":"part","designator":"U3","primitiveId":"pid-u",
  "bbox":{"minX":654,"minY":414,"maxX":726,"maxY":506},"pinsAvailable":true,
  "pins":[{"pinNumber":"1","pinName":"GND","x":654,"y":420},
          {"pinNumber":"2","pinName":"TXD","x":654,"y":436},
          {"pinNumber":"3","pinName":"RXD","x":654,"y":452},
          {"pinNumber":"4","pinName":"V3","x":654,"y":468},
          {"pinNumber":"5","pinName":"D+","x":654,"y":484},
          {"pinNumber":"6","pinName":"D-","x":654,"y":500},
          {"pinNumber":"7","pinName":"NC","x":726,"y":460}]},
 {"componentType":"part","designator":"D1","primitiveId":"pid-d",
  "bbox":{"minX":590,"minY":432,"maxX":638,"maxY":488},"pinsAvailable":true,
  "pins":[{"pinNumber":"1","x":590,"y":460}]},
 {"componentType":"part","designator":"J1","primitiveId":"pid-j",
  "bbox":{"minX":532,"minY":420,"maxX":580,"maxY":490},"pinsAvailable":true,
  "pins":[{"pinNumber":"A1","x":580,"y":455}]},
 {"componentType":"part","designator":"C8","primitiveId":"pid-c8",
  "bbox":{"minX":700,"minY":560,"maxX":720,"maxY":580},"pinsAvailable":true,
  "pins":[{"pinNumber":"1","x":700,"y":570}]},
 {"componentType":"part","designator":"X9","primitiveId":"pid-foreign",
  "bbox":{"minX":300,"minY":430,"maxX":%d,"maxY":480},"pinsAvailable":true,
  "pins":[{"pinNumber":"1","x":300,"y":455}]}
]}`

// bslLiveTestGeom 是锚件的实测几何(落地回读那一步的产物);推让这一步只用它 + 自己
// 读回来的 bbox,**不再读引脚** —— 带引脚的回读会毒化紧随其后的 modify。
func bslLiveTestGeom() *bslAnchorGeom {
	pins := map[string]acPin{}
	for _, p := range []acPin{
		{PinNumber: "1", PinName: "GND", X: 654, Y: 420, Designator: "U3"},
		{PinNumber: "2", PinName: "TXD", X: 654, Y: 436, Designator: "U3"},
		{PinNumber: "3", PinName: "RXD", X: 654, Y: 452, Designator: "U3"},
		{PinNumber: "4", PinName: "V3", X: 654, Y: 468, Designator: "U3"},
		{PinNumber: "5", PinName: "D+", X: 654, Y: 484, Designator: "U3"},
		{PinNumber: "6", PinName: "D-", X: 654, Y: 500, Designator: "U3"},
		{PinNumber: "7", PinName: "NC", X: 726, Y: 460, Designator: "U3"},
	} {
		pins[p.PinNumber] = p
		pins[p.PinName] = p
	}
	return &bslAnchorGeom{BBox: layoutBBox{MinX: 654, MinY: 414, MaxX: 726, MaxY: 506}, Pins: pins}
}

// bslLiveTestDaemon 回放实测几何;wallMaxX 决定那个「不属于本块」的外部图元有多近。
func bslLiveTestDaemon(t *testing.T, wallMaxX int) (*appConfig, *blockApplyTestDaemon, func()) {
	t.Helper()
	return newBlockApplyTestDaemon(t, func(call blockApplyTestCall) string {
		if call.Action != "schematic.components.list" {
			return ""
		}
		if call.Payload["includePins"] == true {
			t.Errorf("实测推让绝不能带 includePins:那次回读会跑 netlist 导出,之后的 modify 会被平台拒")
		}
		return `{"ok":true,"result":` + fmt.Sprintf(bslLiveGeometry, wallMaxX) + `}`
	})
}

func bslLiveTestPlan() *bapPlan {
	return &bapPlan{
		BlockID: "block.ch340c_usb_serial", Relational: true, AnchorRole: "U",
		Placements: []bapPlacement{
			{Role: "U", PartKey: "ic.ch340c", Designator: "U3", PrimitiveID: "pid-u", X: 690, Y: 460},
			{Role: "D_ESD", PartKey: "tvs.sm712_sot23", Designator: "D1", PrimitiveID: "pid-d", X: 610, Y: 460},
			{Role: "J_USB", PartKey: "conn.usb_c_16p", Designator: "J1", PrimitiveID: "pid-j", X: 556, Y: 455},
			{Role: "C_VCC", PartKey: "cap.100nf_0402", Designator: "C8", PrimitiveID: "pid-c8", X: 710, Y: 570},
		},
		// 左侧 6 个引脚挂 marker(第 7 脚不在网里 → 不算);右侧因此需求为 0。
		Nets: []bapNet{{Net: "N", Members: []string{"U3:1", "U3:2", "U3:3", "U3:4", "U3:5", "U3:6"}}},
	}
}

// 6 支 marker 的标签在 y 上互不相撞 → 只排 1 条 lane(旧阶梯口径会要 6 条)。
// 通道 = U3 这侧的 marker 伸出 + D1 自己的伸出 + 视觉间隙;实测只有 16 → D1 让 65,
// 而 D1 与 J1 只有 10(富余 0)→ J1 连锁跟着让 65。下发顺序必须**由外向内**。
func TestBslExpandLive_PushesByRealBBoxAndCascades(t *testing.T) {
	var log strings.Builder
	cfg, daemon, cleanup := bslLiveTestDaemon(t, 400)
	defer cleanup()

	plan := bslLiveTestPlan()
	moves, notes := bslExpandLive(cfg, "w1", plan, bslLiveTestGeom(), &log)
	if len(notes) != 0 {
		t.Fatalf("空地上不该有告警: %v", notes)
	}
	if len(moves) != 2 {
		t.Fatalf("该挪 D1 与 J1 两件: %+v", moves)
	}
	var got []string
	for _, c := range daemon.snapshot() {
		if c.Action != "schematic.component.modify" {
			continue
		}
		patch, _ := c.Payload["patch"].(map[string]any)
		got = append(got, fmt.Sprintf("%v@%v", c.Payload["primitiveId"], patch["x"]))
	}
	if len(got) != 2 || got[0] != "pid-j@491" || got[1] != "pid-d@545" {
		t.Errorf("下发顺序应由外向内、坐标按实测算: %v(期望 [pid-j@491 pid-d@545])", got)
	}
	if plan.Placements[1].X != 545 || plan.Placements[2].X != 491 {
		t.Errorf("plan 必须跟着更新(manifest 的 AT 要如实): D1=%v J1=%v", plan.Placements[1].X, plan.Placements[2].X)
	}
	if plan.Placements[0].X != 690 || plan.Placements[3].X != 710 {
		t.Errorf("锚件与 attach 件永远不动: U3=%v C8=%v", plan.Placements[0].X, plan.Placements[3].X)
	}
	if !strings.Contains(log.String(), "实测") {
		t.Errorf("日志要标明这是实测那一遍: %q", log.String())
	}
}

// 外部图元把链顶死时:一件都不许动(推一半 = 自己制造 part×part 重叠),并如实说。
func TestBslExpandLive_WallBlocksTheWholeChain(t *testing.T) {
	var log strings.Builder
	cfg, daemon, cleanup := bslLiveTestDaemon(t, 522)
	defer cleanup()

	plan := bslLiveTestPlan()
	moves, notes := bslExpandLive(cfg, "w1", plan, bslLiveTestGeom(), &log)
	if len(moves) != 0 {
		t.Errorf("顶死时不许挪: %+v", moves)
	}
	for _, c := range daemon.snapshot() {
		if c.Action != "schematic.components.list" {
			t.Errorf("顶死时不该改画布,出现了 %q", c.Action)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "J1") {
		t.Errorf("必须说清被谁顶住: %v", notes)
	}
	if plan.Placements[1].X != 610 {
		t.Errorf("顶死时坐标不该变: %v", plan.Placements[1].X)
	}
}

// 还原是精确的:坐标是我们自己写进去的,回滚就是把它写回去。
func TestBslUndoLiveMoves_RestoresExactly(t *testing.T) {
	var log strings.Builder
	cfg, daemon, cleanup := bslLiveTestDaemon(t, 400)
	defer cleanup()

	plan := bslLiveTestPlan()
	moves, _ := bslExpandLive(cfg, "w1", plan, bslLiveTestGeom(), &log)
	bslUndoLiveMoves(cfg, "w1", plan, moves, &log)
	if plan.Placements[1].X != 610 || plan.Placements[2].X != 556 {
		t.Errorf("还原必须精确回到推让前: D1=%v J1=%v", plan.Placements[1].X, plan.Placements[2].X)
	}
	calls := daemon.snapshot()
	last := calls[len(calls)-1]
	patch, _ := last.Payload["patch"].(map[string]any)
	if last.Payload["primitiveId"] != "pid-j" || patch["x"] != float64(556) {
		t.Errorf("还原也该由外向内(最后一步是最外侧的 J1 回到 315): %+v", last.Payload)
	}
}

// lane 数 = y 方向上「同时被覆盖最多的那一层」,不是 marker 支数。
// 这条判据决定通道要留多深:6 支隔得开的 marker 只要 1 条 lane(46),
// 旧的阶梯口径会要 276 —— 器件本体才 71 宽,簇被撑成本体的 6 倍。
func TestBslMarkerLanes_PacksByRealCollisionNotByCount(t *testing.T) {
	span := func(y, h float64) [2]float64 { return [2]float64{y - h/2, y + h/2} }

	// ① 6 支标签(高 11)隔 16 —— 互不相撞,共用最浅那条。
	var apart [][2]float64
	for i := 0; i < 6; i++ {
		apart = append(apart, span(float64(i*16), 11))
	}
	if got := bslMarkerLanes(apart); got != 1 {
		t.Errorf("隔得开的 6 支只要 1 条 lane: %d", got)
	}
	// ② 同样 6 支,但标签高 21(GND 旗)—— 相邻必撞,要 2 条。
	var gnd [][2]float64
	for i := 0; i < 6; i++ {
		gnd = append(gnd, span(float64(i*16), 21))
	}
	if got := bslMarkerLanes(gnd); got != 2 {
		t.Errorf("高 21 的旗隔 16 要 2 条 lane: %d", got)
	}
	// ③ 全叠在一起 → 有几支就要几条。
	var stacked [][2]float64
	for i := 0; i < 4; i++ {
		stacked = append(stacked, span(0, 11))
	}
	if got := bslMarkerLanes(stacked); got != 4 {
		t.Errorf("全叠在一起要 4 条: %d", got)
	}
	// ④ 恰好相接不算重叠(闭区间端点碰端点)。
	if got := bslMarkerLanes([][2]float64{{0, 10}, {10, 20}}); got != 1 {
		t.Errorf("端点相接不算撞: %d", got)
	}
	if got := bslMarkerLanes(nil); got != 0 {
		t.Errorf("空侧应为 0 条: %d", got)
	}
}
