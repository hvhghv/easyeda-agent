package app

import (
	"encoding/json"
	"math"
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
func TestBslAttachSeed_LeavesRoomForMarker(t *testing.T) {
	const pinX, pinY, ownHalf = 1000, 500, 10
	net := "3V3"
	x, y := bslAttachSeed(pinX, pinY, "right", net, ownHalf)
	if y != pinY {
		t.Errorf("右贴时 y 应与引脚齐平: %v", y)
	}
	want := pinX + bslReach(net) + bslPartGap + ownHalf
	if math.Abs(x-want) > 0.001 {
		t.Errorf("x = %v, want %v(reach+gap+半宽)", x, want)
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
