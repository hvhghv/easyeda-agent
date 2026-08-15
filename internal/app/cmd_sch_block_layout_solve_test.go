package app

import (
	"encoding/json"
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
	if got, want := oneLane, 2*bslReach("D"); got != want {
		t.Errorf("少量跨接网时应由 marker 伸出决定: got %v want %v", got, want)
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

// 贴脚侧向:引脚在左右列 → 竖放(与宿主 marker 方向正交,天然不撞);
// 上下沿 → 横放。orient 显式声明优先。
func TestBslAttachSide(t *testing.T) {
	for _, c := range []struct {
		pinSide, orient string
		wantVertical    bool
	}{
		{"left", "", true},
		{"right", "", true},
		{"up", "", false},
		{"down", "", false},
		{"up", "vertical", true},      // 显式覆盖推导
		{"left", "horizontal", false}, // 同上
	} {
		side, vertical := bslAttachSide(c.pinSide, c.orient)
		if vertical != c.wantVertical {
			t.Errorf("pinSide=%q orient=%q → vertical=%v, want %v", c.pinSide, c.orient, vertical, c.wantVertical)
		}
		if c.orient == "" && side != c.pinSide {
			t.Errorf("侧向应跟随引脚实测方向: got %q want %q", side, c.pinSide)
		}
	}
}

// 种子点必须把 marker 的伸出让出来 —— 这是「贴脚不撞标签」的算术依据。
func TestBslAttachSeed_HugsThePin(t *testing.T) {
	const pinX, pinY, ownHalf float64 = 1000, 500, 10
	net := "3V3"
	x, y := bslAttachSeed(pinX, pinY, "right", net, ownHalf)
	if y != pinY {
		t.Errorf("右贴时 y 应与引脚齐平: %v", y)
	}
	// attach = 同网直连,中间不挂 marker,所以只留「间隙 + 自身半宽」。
	want := pinX + bslPartGap + ownHalf
	if math.Abs(x-want) > 0.001 {
		t.Errorf("x = %v, want %v(gap+半宽,不含 marker 伸出)", x, want)
	}
	if x-pinX > bslReach(net) {
		t.Errorf("贴脚距离不该大到能塞下一个 marker(%v > %v)", x-pinX, bslReach(net))
	}
	// 左贴是镜像
	lx, _ := bslAttachSeed(pinX, pinY, "left", net, ownHalf)
	if math.Abs((pinX-lx)-(x-pinX)) > 0.001 {
		t.Errorf("左右贴应对称: left=%v right=%v", lx, x)
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
	units := bslPushUnitsOf(plan, bslRelations{})
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
	units := bslPushUnitsOf(plan, bslRelations{})
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
	units := bslPushUnitsOf(plan, bslRelations{})
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
	units := bslPushUnitsOf(plan, bslRelations{})
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
	units := bslPushUnitsOf(plan, rel)
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

	units := bslPushUnitsOf(plan, bslRelations{Attach: map[string]string{"C_VCC": "U.VCC"}})
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
	units := bslPushUnitsOf(plan, bslRelations{})
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
	units := bslPushUnitsOf(plan, bslRelations{})
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
	units := bslPushUnitsOf(plan, bslRelations{})
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
	u1, u2 := bslPushUnitsOf(p1, bslRelations{}), bslPushUnitsOf(p2, bslRelations{})
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
	again := bslPushSolve(bslPushUnitsOf(p1, bslRelations{}), nil, nil, bslPushTestAnchor(), "left", 100)
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
	units := bslPushUnitsOf(plan, bslRelations{})
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
	pins := map[string]acPin{
		"5": {X: 5, Y: 10, Designator: "U1", PinNumber: "5", PinName: "D+"},
		"6": {X: 5, Y: -10, Designator: "U1", PinNumber: "6", PinName: "D-"},
		"9": {X: 95, Y: 0, Designator: "U1", PinNumber: "9", PinName: "NC"}, // 不在网里,不算
	}
	var log strings.Builder
	notes := bslExpandForMarkers(plan, bslRelations{}, bslPushTestAnchor(), pins, nil, nil, &log)

	// 左侧 2 个 marker → 需 92,与 D1 只有 50 → 让 42,落格下取整 40。
	if plan.Placements[1].X != -100 {
		t.Errorf("D1 该被推到 −100: %v", plan.Placements[1].X)
	}
	if len(notes) != 0 {
		t.Errorf("空地上推得动就不该有告警: %v", notes)
	}
	if !strings.Contains(log.String(), "D1 让 40") {
		t.Errorf("日志要把算术写清楚: %q", log.String())
	}
}
