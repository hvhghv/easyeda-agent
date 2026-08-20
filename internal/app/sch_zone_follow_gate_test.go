package app

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// sch_zone_follow_gate_test.go — phase A 的两条机械判据:
//
//	① 挂侧判定用**边界语义**(zfSideOf)—— 高瘦符号上不许把左右两侧的标记判成上下;
//	② 「不得变差」门(zfGateRegression)—— 收敛把本区变得更难排就保留原形。
//
// 两者不互斥、也不互相替代:① 是根因修复(让收敛在大符号上本来就正确),
// ② 是不变式(收敛无论因为什么原因把区顶爆,都不许让它出门)。
//
// ── 缺陷史(真机 ceshi / 页 MCU_IO)────────────────────────────────────────────
//
// 2026-08-20 第一轮:区 esp32s3_wroom1_module 只有一件 U2(ESP32-S3-WROOM-1,
// 本体 71×421、41 脚全在左右两侧)。收敛前 433×541 → 收敛后 244×767,可用高只有
// 765 —— 差 2 个单位排不下,phase B 判 blocked,而不收敛本来排得下。
//
// 2026-08-20 第二轮(同页,已被上一轮落地改过几何):449×737 → 244×863。首版门
// 在这一轮**没有拦住**:它的判据只有 `fits` 一个布尔,而 449×737 本身也不 fits
// (449 > 图签左侧通道 396),于是走了「原形本就排不下 → 收敛是唯一出路」那条
// 放行分支。可这两个「排不下」不是一回事 —— 449×737 只是被图签挡住(高 737 ≤
// 可用高 765),244×863 却连可用域都装不下。判据因此升格成三档阶梯(fitRank)。
//
// 根因(两轮共同):zfGroupFromCluster 首版按 marker 中心相对**本体中心**的主轴
// 判挂侧(|dx| ≥ |dy| 才算左右)。本体 421 高、marker 横向触达只有百来个单位 →
// 上下两端行的 marker |dy| 反而更大,被判成 up/down,进了 zfGenMultiPin 的垂直
// 梯次(一支朝下的 netport 竖起来就 63 高,两支摞下来 161.5)。改成边界语义
// (marker 中心从本体 bbox 哪条边探出去最多)之后同一组标记全判对。

// ── fixture:走生产路径本身,不手搓 zfGroup ─────────────────────────────────

// zfGateMarker 是 fixture 里一支实测端子:pin 坐标 + 标记锚 + 物理挂边。
type zfGateMarker struct {
	net, kind, dir   string
	pinX, pinY       float64
	anchorX, anchorY float64
}

// zfGateBuild 把「本体盒 + 一组实测标记」折成 phase A 的输入 —— **走的是生产
// 路径本身**(zfGroupFromCluster + zfMeasureCluster),不是手写 zfGroup:
// 挂侧判定、实测桩长、端子几何全部由生产代码算,fixture 只提供画布上的坐标。
//
// box 非空时按它当 L1 体积(平台量出来的 bbox 比我们预测的包络略大是常态 ——
// 真机那一区实测 385×421,预测包络 381×421;体积是**观测**,不许拿预测替换)。
// 返回 cluster 是为了负对照能用同一份几何复算「首版判据」下的挂侧。
func zfGateBuild(desig string, body layoutBBox, pinCount int, mks []zfGateMarker, box *layoutBBox) (zfGroup, schCluster) {
	c := schCluster{Designator: desig, Body: body}
	bb := body
	part := layoutComp{ID: desig, Designator: desig, ComponentType: "part", BBox: &bb, PinsAvailable: true}
	var wires []schGroupWire
	var markers []layoutComp
	env, has := body, true
	for i, m := range mks {
		mb := predictedMarkerBBox(m.anchorX, m.anchorY, zfCanonKind(m.kind, m.net), m.dir, m.net)
		mbCopy := mb
		c.Typed = append(c.Typed, schClusterTyped{Kind: m.kind, Net: m.net, BBox: mb})
		part.Pins = append(part.Pins, layoutPin{Number: fmt.Sprint(i + 1), X: m.pinX, Y: m.pinY})
		wires = append(wires, schGroupWire{ID: "w", Points: []float64{m.pinX, m.pinY, m.anchorX, m.anchorY}})
		markers = append(markers, layoutComp{ID: "m", ComponentType: m.kind, Net: m.net,
			X: m.anchorX, Y: m.anchorY, AnchorAvailable: true, BBox: &mbCopy})
		zfGrow(&env, &has, mb)
	}
	c.Box = env
	if box != nil {
		c.Box = *box
	}
	g := zfGroupFromCluster(c, pinCount)
	g.Measured = zfMeasureCluster(c, part, wires, tidyWireRoots(wires), markers)
	return g, c
}

// zfGateLegacySide 是**首版判据的复刻**(marker 中心相对本体中心的主轴)。
// 只活在测试里,存在的唯一理由是给根因修复做常驻负对照:同一份 fixture 在
// 首版判据下必须仍然产出那个排不下的框 —— 修复一旦被改回去,判据当场转红,
// 不必靠人手工做变异实验。
func zfGateLegacySide(body, marker layoutBBox) string {
	bcx, bcy := bboxCenter(body)
	mcx, mcy := bboxCenter(marker)
	dx, dy := mcx-bcx, mcy-bcy
	if absF(dx) >= absF(dy) {
		if dx > 0 {
			return "right"
		}
		return "left"
	}
	if dy > 0 {
		return "up"
	}
	return "down"
}

// zfGateLegacyGroup 用首版判据重算挂侧(其余一切与生产路径相同)。
func zfGateLegacyGroup(c schCluster, pinCount int) zfGroup {
	g := zfGroupFromCluster(c, pinCount)
	i := 0
	for _, m := range c.Typed {
		switch m.Kind {
		case "netport", "netflag", "netlabel":
		default:
			continue
		}
		g.Terms[i].Side = zfGateLegacySide(c.Body, m.BBox)
		i++
	}
	return g
}

// zfFixtureESP32Module 是第一轮真机那一区的重建:一件 41 脚大符号(本体 71×421),
// 8 支标签**横向铺开**(各自贴在自己那一行引脚旁边)。L1 体积按真机实测 385×421 给。
func zfFixtureESP32Module() (zfGroup, schCluster) {
	return zfGateBuild("U2", layoutBBox{MinX: 500, MinY: 200, MaxX: 571, MaxY: 621}, 41,
		[]zfGateMarker{
			{"3V3", "netflag", "left", 500, 240, 460, 240},
			{"GND", "netflag", "left", 500, 220, 460, 220},
			{"EN", "netport", "right", 571, 600, 610, 600},
			{"IO0", "netport", "right", 571, 230, 655, 230},
			{"TXD0", "netport", "left", 500, 440, 425, 440},
			{"RXD0", "netport", "left", 500, 420, 425, 420},
			{"USB_DP", "netport", "left", 500, 560, 425, 560},
			{"USB_DM", "netport", "left", 500, 540, 425, 540},
		}, &layoutBBox{MinX: 340, MinY: 200, MaxX: 725, MaxY: 621})
}

// zfFixtureWroom6 是**第二轮真机那一区**的重建(用户 2026-08-20 现场读数):
// U2 本体 72×420,6 支 marker。真机没留下逐支 marker 的坐标,所以锚点按 WROOM
// 的真实拓扑重建 —— 41 脚全在左右两条长边上,标签一律从左右两侧引出;两支电源
// 旗在中段行,四支 netport 分别贴在最上/最下两组引脚旁。
//
// 这一组几何的判别力(数字见 TestZfSide_TallBodyMarkersHangSideways):
//
//	首版判据(中心主轴)  4 支被判成 up/down → 收敛 230×897 → **连可用域都装不下**
//	边界语义            6 支全判成 left/right → 收敛 325×556.5 → 本页有落点
//
// 230×897 与真机第二轮实测的 244×863 同型(高度都由「每侧两支竖起来的 netport
// 梯次」贡献:20 + 63 + 6 + 63 + 9.5 = 161.5 一侧,420 + 161.5×2 = 743 → 框 863)。
func zfFixtureWroom6() (zfGroup, schCluster) {
	return zfGateBuild("U2", layoutBBox{MinX: 500, MinY: 200, MaxX: 572, MaxY: 620}, 41,
		[]zfGateMarker{
			{"VDD3V3", "netflag", "left", 500, 440, 455, 440},
			{"GND", "netflag", "left", 500, 400, 455, 400},
			{"TXD0", "netport", "right", 572, 600, 617, 600},
			{"RXD0", "netport", "right", 572, 580, 617, 580},
			{"USB_DP", "netport", "left", 500, 240, 437, 240},
			{"USB_DM", "netport", "left", 500, 220, 437, 220},
		}, nil)
}

// zfFixtureWideHeader 是**门的正样本**:一条横放的 10 脚排针(本体 300×30),
// 标签物理上就在上下两侧 —— 挂侧判定怎么改都是 up/down,不存在误判。收敛在这里
// 依然是负优化:垂直梯次让每支竖起来的 netport 占 63 高,五支摞成一列。
//
//	现状 348×311(本页有落点)→ 收敛 369×781(连可用域都装不下)
//
// 门必须回退。它与根因修复正交:这就是「①修好了②仍然必要」的那个证据。
func zfFixtureWideHeader() (zfGroup, schCluster) {
	return zfGateBuild("J1", layoutBBox{MinX: 200, MinY: 400, MaxX: 500, MaxY: 430}, 20,
		[]zfGateMarker{
			{"D0", "netport", "down", 220, 400, 220, 380},
			{"D1", "netport", "down", 260, 400, 260, 380},
			{"D2", "netport", "down", 300, 400, 300, 380},
			{"D3", "netport", "down", 340, 400, 340, 380},
			{"D4", "netport", "down", 380, 400, 380, 380},
			{"D5", "netport", "up", 240, 430, 240, 450},
			{"D6", "netport", "up", 280, 430, 280, 450},
			{"D7", "netport", "up", 320, 430, 320, 450},
			{"D8", "netport", "up", 360, 430, 360, 450},
			{"D9", "netport", "up", 400, 430, 400, 450},
		}, nil)
}

// zfFixtureWideHeaderStaged 是同一条排针**已经被前一轮竖向梯次撑开**之后的现状
// (桩长 380/390,标签甩到上下老远):现状 348×1031 连可用域都装不下,收敛 369×781
// 同样装不下 —— **两个形状都没救**。门在这里不许拦(拦了也没用,而且会把一个
// 更小的框换成更大的),phase B 照常报 blocked,归因必须读得出来。
func zfFixtureWideHeaderStaged() (zfGroup, schCluster) {
	return zfGateBuild("J2", layoutBBox{MinX: 200, MinY: 400, MaxX: 500, MaxY: 430}, 20,
		[]zfGateMarker{
			{"D0", "netport", "down", 220, 400, 220, 20},
			{"D1", "netport", "down", 260, 400, 260, 20},
			{"D2", "netport", "down", 300, 400, 300, 20},
			{"D3", "netport", "down", 340, 400, 340, 20},
			{"D4", "netport", "down", 380, 400, 380, 20},
			{"D5", "netport", "up", 240, 430, 240, 810},
			{"D6", "netport", "up", 280, 430, 280, 810},
			{"D7", "netport", "up", 320, 430, 320, 810},
			{"D8", "netport", "up", 360, 430, 360, 810},
			{"D9", "netport", "up", 400, 430, 400, 810},
		}, nil)
}

// zfFixtureU3V3 是**收敛确实有益**的那一类区(真机 U_3V3 同型):一件竖立的
// 去耦电容,上下两支旗被历史布线甩到老远 —— 现状 72×834 连可用域都装不下,
// 收敛把桩线重生成短桩后 82×244,本页有落点。门必须放行。
func zfFixtureU3V3() (zfGroup, schCluster) {
	return zfGateBuild("C1", layoutBBox{MinX: 600, MinY: 400, MaxX: 610, MaxY: 422}, 2,
		[]zfGateMarker{
			{"+3V3", "netflag", "up", 605, 422, 605, 740},
			{"GND", "netflag", "down", 605, 400, 605, 80},
		}, nil)
}

// zfGateA4Domain 是真机那一页的可行域:A4 1170×825 + 标准图签角 + 默认 opts。
func zfGateA4Domain(opts partitionOpts) (layoutBBox, *layoutBBox, zfDomain) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	keepout, _ := titleBlockKeepout(&sheet)
	return sheet, keepout, zfDomainFor(sheet, keepout, opts)
}

// zfGateMCUIOZones 是真机 MCU_IO 那一页的六个区(其余五个是 2026-08-20 phase A
// 的实测输出值),第一个区的尺寸由参数给。
func zfGateMCUIOZones(w, h float64) []zaZone {
	return []zaZone{
		{Name: "esp32s3_wroom1_module", W: w, H: h, Home: [2]float64{532, 410}},
		{Name: "U_3V3", W: 82, H: 378, Home: [2]float64{200, 600}},
		{Name: "U_EN", W: 175, H: 296, Home: [2]float64{250, 300}},
		{Name: "U_IO0", W: 181, H: 198, Home: [2]float64{800, 600}},
		{Name: "led_indicator_gpio", W: 179, H: 265, Home: [2]float64{900, 400}},
		{Name: "tactile_boot_reset", W: 179, H: 346, Home: [2]float64{850, 250}},
	}
}

// ── ① 根因:挂侧判定的边界语义 ─────────────────────────────────────────────

// zfSideOf 的真值表。第三、四组是本次修复的要害:同一支「探出左边 40」的标记,
// 在方形本体上首版判据也对,在 72×420 的高瘦本体上首版判据就翻车。
func TestZfSideOf_BoundarySemantics(t *testing.T) {
	tall := layoutBBox{MinX: 500, MinY: 200, MaxX: 572, MaxY: 620} // 72×420
	wide := layoutBBox{MinX: 200, MinY: 400, MaxX: 500, MaxY: 430} // 300×30
	square := layoutBBox{MinX: 0, MinY: 0, MaxX: 100, MaxY: 100}   //
	mk := func(cx, cy float64) layoutBBox {                        // 以中心给一个小盒
		return layoutBBox{MinX: cx - 5, MinY: cy - 5, MaxX: cx + 5, MaxY: cy + 5}
	}
	for _, tc := range []struct {
		name       string
		body       layoutBBox
		cx, cy     float64
		want, legc string
	}{
		{"高瘦本体:靠上端行的左侧标记", tall, 460, 610, "left", "up"},
		{"高瘦本体:靠下端行的右侧标记", tall, 620, 210, "right", "down"},
		{"高瘦本体:中段左侧标记", tall, 460, 410, "left", "left"},
		{"高瘦本体:真的在上方", tall, 536, 700, "up", "up"},
		{"宽体:真的在下方", wide, 300, 360, "down", "down"},
		{"宽体:靠左端的下方标记", wide, 210, 360, "down", "left"},
		{"宽体:真的在左侧", wide, 150, 415, "left", "left"},
		{"方形本体:首版与边界语义一致", square, 160, 50, "right", "right"},
		{"标记中心落在本体之内 → 退化成「离哪条边最近」", tall, 540, 400, "right", "down"},
	} {
		if got := zfSideOf(tc.body, mk(tc.cx, tc.cy)); got != tc.want {
			t.Errorf("%s:zfSideOf = %q,want %q", tc.name, got, tc.want)
		}
		if got := zfGateLegacySide(tc.body, mk(tc.cx, tc.cy)); got != tc.legc {
			t.Errorf("%s:首版判据复刻漂了(=%q,该是 %q)—— 负对照失去意义", tc.name, got, tc.legc)
		}
	}
	// 确定性:同输入同输出,平局按 left < right < down < up。
	for i := 0; i < 5; i++ {
		if got := zfSideOf(square, mk(50, 50)); got != "left" {
			t.Fatalf("正中心该按平局序取 left,得到 %q", got)
		}
	}
}

// 正对照(用户 2026-08-20 现场那一组几何):U2 本体 72×420 + 6 支 marker。
//
//	断言 A  6 支全判成 left/right(它们物理上就挂在左右两条长边上)
//	断言 B  phase A 输出的框 fits 于 A4 域(margin 28 / 区名带 30 / 图签 keep-out)
//	断言 C  **常驻变异对照**:同一组几何在首版判据下必须仍然排不下 ——
//	        修复被改回去,这一条当场转红
func TestZfSide_TallBodyMarkersHangSideways(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	g, c := zfFixtureWroom6()

	if w, h := c.Body.MaxX-c.Body.MinX, c.Body.MaxY-c.Body.MinY; w != 72 || h != 420 {
		t.Fatalf("fixture 本体该是真机那个 72×420,得到 %.0f×%.0f", w, h)
	}
	// A:挂侧全部判对。
	side := map[string]string{}
	for _, tm := range g.Terms {
		side[tm.Net] = tm.Side
	}
	for net, want := range map[string]string{
		"VDD3V3": "left", "GND": "left", "TXD0": "right", "RXD0": "right",
		"USB_DP": "left", "USB_DM": "left",
	} {
		if side[net] != want {
			t.Errorf("%s 挂在本体的 %s 侧,却判成 %q", net, want, side[net])
		}
	}

	// B:收敛框在本页有落点。
	conv, err := planZoneFollow("esp32s3_wroom1_module", []zfGroup{g}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if conv.FrameW != 325 || conv.FrameH != 556.5 {
		t.Fatalf("收敛框该是 325×556.5,得到 %g×%g —— 数字变了先确认是改好了还是改坏了",
			conv.FrameW, conv.FrameH)
	}
	if !dom.fits(conv.FrameW, conv.FrameH) {
		t.Fatalf("phase A 的输出 %.0f×%.0f 在 A4 域里没有落点", conv.FrameW, conv.FrameH)
	}
	if r := dom.fitRank(conv.FrameW, conv.FrameH); r != 2 {
		t.Errorf("收敛框该是 2 档(本页有落点),得到 %d(%s)", r, zfRankWhy(r))
	}
	// 门不该在这里响:收敛是货真价实的改善,不是回退。
	gated, err := planZoneFollowGated("esp32s3_wroom1_module", []zfGroup{g}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	if gated.Retained {
		t.Errorf("修好挂侧之后这一区不该再回退:%s", gated.RetainWhy)
	}

	// C:常驻变异对照 —— 首版判据下同一组几何必须仍然排不下。
	leg := zfGateLegacyGroup(c, 41)
	legSides := map[string]string{}
	for _, tm := range leg.Terms {
		legSides[tm.Net] = tm.Side
	}
	for net, want := range map[string]string{"TXD0": "up", "RXD0": "up", "USB_DP": "down", "USB_DM": "down"} {
		if legSides[net] != want {
			t.Fatalf("变异对照失效:首版判据下 %s 该被误判成 %s,得到 %q", net, want, legSides[net])
		}
	}
	legConv, err := planZoneFollow("esp32s3_wroom1_module", []zfGroup{leg}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if legConv.FrameW != 230 || legConv.FrameH != 897 {
		t.Fatalf("变异对照该产出 230×897(与真机 244×863 同型),得到 %.0f×%.0f",
			legConv.FrameW, legConv.FrameH)
	}
	if dom.fitRank(legConv.FrameW, legConv.FrameH) != 0 {
		t.Fatal("变异对照失效:首版判据下的框居然还排得下 —— 这条 fixture 不再有判别力")
	}
	t.Logf("边界语义 %.0f×%.0f(fits)   vs   首版中心主轴 %.0f×%.0f(连可用域都装不下)",
		conv.FrameW, conv.FrameH, legConv.FrameW, legConv.FrameH)
}

// 挂侧判定与**实测口径**必须给同一个答案:收敛侧用 zfSideOf(本体 bbox × marker
// bbox),回退/落地侧用 tidyStubDirection(pin → 标记锚的实测位移)。两处判的是
// 同一件事,分家了就是又造了一把尺 —— 原形保留区会按 A 侧规划、按 B 侧落地。
func TestZfSideOf_AgreesWithMeasuredStubDirection(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func() (zfGroup, schCluster)
	}{
		{"wroom6", zfFixtureWroom6},
		{"esp32-8", zfFixtureESP32Module},
		{"wide-header", zfFixtureWideHeader},
		{"u3v3", zfFixtureU3V3},
	} {
		g, _ := tc.mk()
		if g.Measured == nil {
			t.Fatalf("%s:fixture 该带实测几何", tc.name)
		}
		want := map[string]string{}
		for _, tm := range g.Measured.Terms { // Dir 来自 tidyStubDirection
			want[tm.Net] = tm.Dir
		}
		for _, tm := range g.Terms { // Side 来自 zfSideOf
			if w, ok := want[tm.Net]; ok && w != tm.Side {
				t.Errorf("%s:%s 的挂侧两把尺不一致 —— zfSideOf=%q,实测桩方向=%q",
					tc.name, tm.Net, tm.Side, w)
			}
		}
	}
}

// ── ② 接真链:修好挂侧之后 phase B 必须能把这一区放下 ───────────────────────

func TestZfGate_PhaseBPlacesWroomZoneAfterFix(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet, keepout, _ := zfGateA4Domain(opts)
	g, c := zfFixtureWroom6()

	conv, err := planZoneFollow("esp32s3_wroom1_module", []zfGroup{g}, opts)
	if err != nil {
		t.Fatal(err)
	}
	after := zonesArrange(zfGateMCUIOZones(conv.FrameW, conv.FrameH), sheet, keepout, opts)
	if !after.OK {
		t.Fatalf("修好挂侧后 phase B 必须 OK,仍然 blocked=%s(%s)—— 如实报告:还需要别的手段",
			after.Blocked, after.Tried)
	}
	if len(after.Placed) != 6 {
		t.Fatalf("六个区都该落位,得到 %d", len(after.Placed))
	}
	v := zaValidate(after, sheet, keepout, opts)
	if v.SheetOverflow != 0 || v.PartitionOverlap != 0 || v.TitleBlockHits != 0 || v.SheetMarginHits != 0 {
		t.Errorf("落位该验证全绿,得到 %+v", v)
	}
	for _, p := range after.Placed {
		t.Logf("  %-24s %s shelf=%d [%.0f,%.0f → %.0f,%.0f]", p.Name, p.Edge, p.Shelf,
			p.Rect.MinX, p.Rect.MinY, p.Rect.MaxX, p.Rect.MaxY)
	}

	// 变异对照:首版判据下同一页必须 blocked 在这一区(缺陷可复现,修复才算数)。
	legConv, err := planZoneFollow("esp32s3_wroom1_module", []zfGroup{zfGateLegacyGroup(c, 41)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	before := zonesArrange(zfGateMCUIOZones(legConv.FrameW, legConv.FrameH), sheet, keepout, opts)
	if before.OK {
		t.Fatal("变异对照失效:首版判据下这一页居然排得下 —— 复现不了缺陷,上半条判据就是自证")
	}
	if before.Blocked != "esp32s3_wroom1_module" {
		t.Errorf("blocked 的该是那个大符号区,得到 %q", before.Blocked)
	}
	if !strings.Contains(before.Tried, "纸面放不下") {
		t.Errorf("归因该说得出「纸面放不下」:%s", before.Tried)
	}
	t.Logf("变异对照:blocked=%s tried=%s", before.Blocked, before.Tried)
}

// ── ③ 门:收敛把区变得更难排就回退 ─────────────────────────────────────────

// 门的判据本体(纯函数),用两轮真机的四个数字直接钉:
//
//	第一轮 433×541(有落点)→ 244×767(连可用域都装不下)  掉 2 档
//	第二轮 449×737(被图签挡)→ 244×863(连可用域都装不下)  掉 1 档 ← 首版门漏的就是这一档
func TestZfGateRegression_RealMachineNumbers(t *testing.T) {
	_, _, dom := zfGateA4Domain(defaultPartitionOpts())

	if r := dom.fitRank(449, 737); r != 1 {
		t.Fatalf("449×737 该是 1 档(高 737 ≤ 765,但 449 > 图签左侧通道 396),得到 %d", r)
	}
	if r := dom.fitRank(244, 863); r != 0 {
		t.Fatalf("244×863 该是 0 档(高 863 > 可用高 765),得到 %d", r)
	}
	why := zfGateRegression(449, 737, 244, 863, dom)
	if why == "" {
		t.Fatal("第二轮真机那一组必须回退 —— 门又只会看 fits 这一个布尔了")
	}
	// 归因必须带得出:哪一维涨了、涨到多少、从哪一档掉到哪一档、可用域多大。
	for _, s := range []string{"高 737→863", "图签", "可用 1110×765", "保留原形 449×737"} {
		if !strings.Contains(why, s) {
			t.Errorf("回退理由里读不到 %q:%s", s, why)
		}
	}
	if zfGateRegression(433, 541, 244, 767, dom) == "" {
		t.Error("第一轮真机那一组也必须回退")
	}
	t.Logf("%s", why)
}

// 端到端:门在**真实几何**上把回退兑现成一份完整的原形计划。
func TestZfGate_ConvergenceRegressionRetained(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	g, c := zfFixtureWideHeader()

	rawW, rawH := zoneArrangeRawFrame(c.Box, opts, 0)
	if rawW != 348 || rawH != 311 {
		t.Fatalf("现状口径框该是 348×311,得到 %.0f×%.0f", rawW, rawH)
	}
	conv, err := planZoneFollow("J1_HEADER", []zfGroup{g}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if conv.FrameW != 369 || conv.FrameH != 781 {
		t.Fatalf("未加门的收敛框该是 369×781,得到 %.0f×%.0f", conv.FrameW, conv.FrameH)
	}
	if dom.fitRank(rawW, rawH) != 2 || dom.fitRank(conv.FrameW, conv.FrameH) != 0 {
		t.Fatalf("fixture 失去判别力:现状 %d 档、收敛 %d 档",
			dom.fitRank(rawW, rawH), dom.fitRank(conv.FrameW, conv.FrameH))
	}

	got, err := planZoneFollowGated("J1_HEADER", []zfGroup{g}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Retained {
		t.Fatalf("门没生效:采纳了 %.0f×%.0f 的收敛(它排不下)", got.FrameW, got.FrameH)
	}
	if got.FrameW != rawW || got.FrameH != rawH {
		t.Errorf("原形保留该给回 %.0f×%.0f,得到 %.0f×%.0f", rawW, rawH, got.FrameW, got.FrameH)
	}
	// 回退必须可见:人读(Mode)与机器读(RetainWhy)同一句话,且带得出「哪一维、
	// 从多少涨到多少、掉了哪一档」——否则下一个人只会看到框莫名其妙没收敛。
	for _, s := range []string{"收敛回退", "311", "781", "765"} {
		if !strings.Contains(got.Mode, s) {
			t.Errorf("Mode 里读不到 %q:%q", s, got.Mode)
		}
	}
	if got.RetainWhy == "" || !strings.Contains(got.Mode, got.RetainWhy) {
		t.Errorf("RetainWhy(%q)必须与 Mode 尾巴是同一句话:%q", got.RetainWhy, got.Mode)
	}
	// 原形保留 = 一个单位都不动:落地余量不许再加一圈(加了就自己变差了)。
	if got.Slack != 0 {
		t.Errorf("原形保留是刚体平移,不该再吃落地余量,得到 %g", got.Slack)
	}
	t.Logf("retained: %s → 框 %.0f×%.0f", got.Mode, got.FrameW, got.FrameH)
}

// 原形计划必须是一份**下游能用**的完整计划:--apply 的断言①要求「计划端子网名
// 多重集 == 已连接 pin 网名多重集」,回退时漏端子会让整页拒绝执行。
func TestZfRetainPlan_TermsCoverEveryConnectedPin(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func() (zfGroup, schCluster)
	}{
		{"wide-header", zfFixtureWideHeader},
		{"wroom6", zfFixtureWroom6},
	} {
		g, _ := tc.mk()
		plan, ok := zfRetainPlan(tc.name, []zfGroup{g}, defaultPartitionOpts())
		if !ok || len(plan.Groups) != 1 {
			t.Fatalf("%s:原形计划该有 1 个组,ok=%v groups=%d", tc.name, ok, len(plan.Groups))
		}
		want := map[string]int{}
		for _, tm := range g.Measured.Terms {
			want[tm.Net]++
		}
		got := map[string]int{}
		for _, tm := range plan.Groups[0].Terms {
			got[tm.Net]++
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s:原形计划端子网名多重集 %v ≠ 实测已连接 pin %v —— --apply 断言①会拒整页",
				tc.name, got, want)
		}
		// 端子几何仍走唯一那把尺(zfTermGeom):生成期包络 == 复算期包络。
		// 区内局部坐标与页面绝对坐标的 5 网格相位不同,平移后最多差一格。
		pg := plan.Groups[0]
		got2, want2 := zfLandedGroupBBox(pg, zfStubPlanned), zfGroupBBox(pg)
		for _, d := range []float64{got2.MinX - want2.MinX, got2.MinY - want2.MinY,
			got2.MaxX - want2.MaxX, got2.MaxY - want2.MaxY} {
			if d > acSchGrid || d < -acSchGrid {
				t.Errorf("%s:原形计划也必须只经 zfTermGeom:生成 %+v 复算 %+v 偏差 %.1f",
					tc.name, want2, got2, d)
			}
		}
	}
}

// ── ④ 负对照:门不许退化成「见涨就退」或「永远说 fits」 ─────────────────────

// 判据本体的负对照(真机 U_EN:164×727 → 175×296,**宽度涨了 11**、高度大降 431)。
func TestZfGateRegression_NegativeControl_UEn(t *testing.T) {
	_, _, dom := zfGateA4Domain(defaultPartitionOpts())
	if why := zfGateRegression(164, 727, 175, 296, dom); why != "" {
		t.Fatalf("负对照失效:U_EN 收敛(164×727 → 175×296)被误回退 —— %s", why)
	}
	// 反过来:同样是「宽涨」,但涨到本页没有落点时必须回退。
	if why := zfGateRegression(164, 727, 1200, 296, dom); why == "" {
		t.Fatal("宽度涨到超出可用域仍不回退 —— 门对宽这一维是瞎的")
	}
	// 原形与收敛**同档**(两个都没救)时不许拦:拦了只是把小框换成大框。
	if why := zfGateRegression(900, 800, 244, 767, dom); why != "" {
		t.Fatalf("同为 0 档时该放行收敛,却回退了:%s", why)
	}
	// 两维都不变大 → 三档各自单调,结构上不可能掉档。
	if why := zfGateRegression(433, 541, 400, 500, dom); why != "" {
		t.Fatalf("两维都收窄却回退了:%s", why)
	}
	// 掉档但仍在同一档之上(2 → 1)也要拦:被图签挡住 ≠ 有落点。
	if why := zfGateRegression(396, 700, 500, 700, dom); why == "" {
		t.Fatal("2 档掉到 1 档没拦 —— 阶梯只认最下面那一级就等于没有阶梯")
	}
}

// 端到端负对照①:一个真的靠收敛救回来的区(现状 72×834 连可用域都装不下,
// 收敛后 82×244 本页有落点)。门必须放行,retained 必须是 false。
func TestZfGate_ConvergenceStillAdoptedWhenItHelps(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	g, c := zfFixtureU3V3()

	rawW, rawH := zoneArrangeRawFrame(c.Box, opts, 0)
	if rawW != 72 || rawH != 834 {
		t.Fatalf("现状口径框该是真机 U_3V3 那个 72×834,得到 %.0f×%.0f", rawW, rawH)
	}
	conv, err := planZoneFollow("U_3V3", []zfGroup{g}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if conv.FrameW <= rawW {
		t.Fatalf("负对照失去意义:收敛后宽度没有变大(%.0f → %.0f)—— 换个 fixture", rawW, conv.FrameW)
	}
	if dom.fitRank(rawW, rawH) != 0 || dom.fitRank(conv.FrameW, conv.FrameH) != 2 {
		t.Fatalf("负对照失去意义:现状 %d 档 → 收敛 %d 档",
			dom.fitRank(rawW, rawH), dom.fitRank(conv.FrameW, conv.FrameH))
	}
	got, err := planZoneFollowGated("U_3V3", []zfGroup{g}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	if got.Retained {
		t.Fatalf("门误回退:%.0f×%.0f → %.0f×%.0f 是货真价实的收敛(%s)",
			rawW, rawH, conv.FrameW, conv.FrameH, got.RetainWhy)
	}
	if got.FrameW != conv.FrameW || got.FrameH != conv.FrameH {
		t.Errorf("采纳收敛时输出该与未加门逐字相同:%.0f×%.0f vs %.0f×%.0f",
			got.FrameW, got.FrameH, conv.FrameW, conv.FrameH)
	}
	t.Logf("负对照①:现状 %.0f×%.0f → 收敛 %.0f×%.0f(宽 +%.0f、高 %.0f)已采纳",
		rawW, rawH, conv.FrameW, conv.FrameH, conv.FrameW-rawW, conv.FrameH-rawH)
}

// 端到端负对照②:现状与收敛**都没救**(两个都是 0 档)。门不许拦(拦了就是把
// 369×781 换成更大的 348×1031,纯粹变差),phase B 照常 blocked,归因必须读得出
// 「纸面放不下」而不是一句「无处可放」。
func TestZfGate_BothShapesUnplaceableStaysBlockedWithReadableReason(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet, keepout, dom := zfGateA4Domain(opts)
	g, c := zfFixtureWideHeaderStaged()

	rawW, rawH := zoneArrangeRawFrame(c.Box, opts, 0)
	if rawW != 348 || rawH != 1031 {
		t.Fatalf("现状口径框该是 348×1031,得到 %.0f×%.0f", rawW, rawH)
	}
	got, err := planZoneFollowGated("J2_HEADER", []zfGroup{g}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	if got.Retained {
		t.Fatalf("两个形状都是 0 档,门不该拦(拦了只是把 %.0f×%.0f 换成更大的 %.0f×%.0f):%s",
			got.FrameW, got.FrameH, rawW, rawH, got.RetainWhy)
	}
	if dom.fitRank(rawW, rawH) != 0 || dom.fitRank(got.FrameW, got.FrameH) != 0 {
		t.Fatalf("fixture 失去判别力:现状 %d 档、输出 %d 档",
			dom.fitRank(rawW, rawH), dom.fitRank(got.FrameW, got.FrameH))
	}
	// phase B 照常报 blocked,且归因可读可执行。
	res := zonesArrange([]zaZone{
		{Name: "J2_HEADER", W: got.FrameW, H: got.FrameH, Home: [2]float64{350, 415}},
		{Name: "U_IO0", W: 181, H: 198, Home: [2]float64{800, 600}},
	}, sheet, keepout, opts)
	if res.OK {
		t.Fatal("0 档的框居然落位了 —— fitRank 与求解器分家了")
	}
	if res.Blocked != "J2_HEADER" {
		t.Errorf("blocked 的该是那个 0 档区,得到 %q", res.Blocked)
	}
	if !strings.Contains(res.Tried, "纸面放不下") {
		t.Errorf("归因该说得出「纸面放不下」而不是一句「无处可放」:%s", res.Tried)
	}
	t.Logf("负对照②:blocked=%s tried=%s", res.Blocked, res.Tried)
}

// ── ⑤ 判据本体 + 一把尺 + 确定性 ───────────────────────────────────────────

// fits 是「本页还存不存在落点」的精确判据(单个矩形障碍),fitsBare 是「忽略图签
// 装不装得进可用域」;两者都对 (w,h) **单调** —— 门就是靠单调性省掉「任一维度
// 变大」的显式判断,单调性破了门的推理就垮了。
func TestZfDomainFits(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	left, right, below, above := dom.strips()
	if left != 396 || above != 555 {
		t.Fatalf("A4+图签的通道该是左 396 / 上 555,得到 左%.0f 右%.0f 下%.0f 上%.0f", left, right, below, above)
	}
	for _, tc := range []struct {
		w, h float64
		rank int
		why  string
	}{
		{433, 541, 2, "图签上方那条(541 ≤ 555)"},
		{433, 556, 1, "上方装不下、又比左侧通道宽,但仍在可用域内"},
		{396, 765, 2, "图签左侧整条"},
		{397, 765, 1, "比左侧通道宽 1,上方又太高"},
		{244, 767, 0, "高 767 > 可用高 765"},
		{449, 737, 1, "真机第二轮的原形:只是被图签挡住"},
		{244, 863, 0, "真机第二轮的收敛:连可用域都装不下"},
		{1111, 100, 0, "比可用域还宽"},
		{100, 766, 0, "比可用域还高"},
	} {
		if got := dom.fitRank(tc.w, tc.h); got != tc.rank {
			t.Errorf("fitRank(%.0f,%.0f)=%d(%s)want %d(%s)", tc.w, tc.h,
				got, zfRankWhy(got), tc.rank, tc.why)
		}
	}
	// 单调性:框缩小之后档位绝不会降。
	for _, base := range [][2]float64{{433, 541}, {396, 765}, {180, 300}, {449, 737}, {600, 900}} {
		r0 := dom.fitRank(base[0], base[1])
		for _, d := range []float64{1, 17, 111} {
			if r := dom.fitRank(base[0]-d, base[1]-d); r < r0 {
				t.Errorf("单调性破了:%v 是 %d 档,缩小 %g 后掉到 %d 档", base, r0, d, r)
			}
		}
	}
	// 无图签的页:2 档与 1 档合并成可用域本身。
	bare := zfDomainFor(layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}, nil, opts)
	if bare.fitRank(1110, 765) != 2 || bare.fitRank(1111, 765) != 0 {
		t.Error("无图签时判据该退化成纯可用域")
	}
}

// 一把尺:门的可行域必须与 phase B 求解器的 zaFrame 逐字段同源。
// 两处各算各的域界 = 又造了一把尺(门会放行求解器排不下的框,或反过来)。
func TestZfDomainFor_SameRulerAsSolver(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet, keepout, dom := zfGateA4Domain(opts)
	s := newZaSearch(nil, sheet, keepout, opts)
	if dom.L != s.f.L || dom.R != s.f.R || dom.B != s.f.B || dom.T != s.f.T || dom.G != s.f.G {
		t.Fatalf("可行域与求解器分家:门 %+v vs zaFrame %+v", dom, s.f)
	}
	safe := inflatedTitleKeepout(keepout)
	if dom.Keep == nil || safe == nil || *dom.Keep != *safe {
		t.Fatalf("图签安全带口径分家:门 %+v vs inflatedTitleKeepout %+v", dom.Keep, safe)
	}
}

// 一把尺(行为级):fitRank 判的必须就是求解器真会做的事 ——
//
//	2 档  ⟺  这个框单独放在空页上,phase B 一定放得下
//	0 档  ⟹  phase B 逐边归因全是「纸面放不下」(Cands == 0)
//	1 档  ⟹  放不下,但归因是「被图签挡」而不是纸面装不下
//
// 域界字段相等(上一条)只保证输入同源,这一条才保证**结论**同源。
func TestZfFitRank_MatchesSolverOutcome(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet, keepout, dom := zfGateA4Domain(opts)
	for _, wh := range [][2]float64{
		{325, 556}, {449, 737}, {244, 863}, {396, 765}, {397, 765},
		{1111, 100}, {100, 766}, {230, 897}, {348, 311}, {369, 781}, {82, 244}, {72, 834},
	} {
		w, h := wh[0], wh[1]
		rank := dom.fitRank(w, h)
		res := zonesArrange([]zaZone{{Name: "solo", W: w, H: h, Home: [2]float64{585, 412}}},
			sheet, keepout, opts)
		if (rank == 2) != res.OK {
			t.Errorf("%.0f×%.0f:fitRank=%d 但求解器 OK=%v(%s)", w, h, rank, res.OK, res.Tried)
		}
		if res.OK {
			continue
		}
		allNoRoom := len(res.Edges) > 0
		for _, e := range res.Edges {
			if e.Cands != 0 {
				allNoRoom = false
			}
		}
		if rank == 0 && !allNoRoom {
			t.Errorf("%.0f×%.0f 是 0 档,求解器却说有候选:%s", w, h, res.Tried)
		}
		if rank == 1 && allNoRoom && !res.Exhausted {
			t.Errorf("%.0f×%.0f 是 1 档(该是被图签挡),求解器却报纸面放不下:%s", w, h, res.Tried)
		}
	}
}

// 逐区独立:一页里一个区回退,不影响另一个区照常收敛。
func TestZfGate_PerZoneIndependent(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	big, _ := zfFixtureWideHeader()
	small, _ := zfGateBuild("C9", layoutBBox{MinX: 100, MinY: 100, MaxX: 118, MaxY: 122}, 2,
		[]zfGateMarker{
			{"3V3", "netflag", "up", 109, 122, 109, 162},
			{"GND", "netflag", "down", 109, 100, 109, 60},
		}, nil)
	a, err := planZoneFollowGated("J1_HEADER", []zfGroup{big}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	b, err := planZoneFollowGated("U_3V3", []zfGroup{small}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Retained {
		t.Error("排针区该回退")
	}
	if b.Retained {
		t.Errorf("小区不该被隔壁的回退传染:%s", b.RetainWhy)
	}
}

// 确定性:同输入同输出;判定不依赖输入顺序,也不依赖 map 遍历序
// (端子按几何全序排;门只做算术)。
func TestZfGate_Deterministic(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	g, _ := zfFixtureWideHeader()
	base, err := planZoneFollowGated("J1_HEADER", []zfGroup{g}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := planZoneFollowGated("J1_HEADER", []zfGroup{g}, opts, dom)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(base, again) {
			t.Fatalf("同输入第 %d 次输出不同 —— 有 map 遍历序泄漏", i)
		}
	}
	// 多组时输入顺序无关(回退路径也要过这一关)。
	c1, _ := zfGateBuild("C1", layoutBBox{MinX: 600, MinY: 200, MaxX: 618, MaxY: 222}, 2,
		[]zfGateMarker{
			{"EN", "netport", "left", 600, 211, 580, 211},
			{"GND", "netflag", "down", 609, 200, 609, 160},
		}, nil)
	r1, _ := zfGateBuild("R1", layoutBBox{MinX: 600, MinY: 651, MaxX: 610, MaxY: 673}, 2,
		[]zfGateMarker{
			{"EN", "netport", "left", 600, 662, 580, 662},
			{"3V3", "netflag", "up", 605, 673, 605, 713},
		}, nil)
	fwd, err := planZoneFollowGated("U_EN", []zfGroup{c1, r1}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := planZoneFollowGated("U_EN", []zfGroup{r1, c1}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fwd, rev) {
		t.Fatal("输入顺序改变了输出 —— 有序依赖")
	}
	// 原形计划本身也必须与输入顺序无关。
	kf, _ := zfRetainPlan("U_EN", []zfGroup{c1, r1}, opts)
	kr, _ := zfRetainPlan("U_EN", []zfGroup{r1, c1}, opts)
	if !reflect.DeepEqual(kf, kr) {
		t.Fatal("原形计划有序依赖")
	}
}

// 没有实测几何(纯几何单测的输入)时门无从回退 —— 必须原样采纳收敛,不许崩。
func TestZfGate_NoMeasuredGeometryFallsThrough(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	got, err := planZoneFollowGated("U", zfFixtureU(), opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	if got.Retained {
		t.Fatal("没有实测几何却报了回退 —— 回退的是什么形?")
	}
	want, err := planZoneFollow("U", zfFixtureU(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("无实测时门必须完全透明")
	}
}

// 回退必须在**两个**输出面都看得见:人读(行首 ↩ + Mode 尾巴)与
// JSON(retained / retainWhy)。`retained` 恒定出现(无 omitempty)——
// 值为 false 就被抹掉的话,读的人分不清「没回退」与「这版没这个字段」。
func TestZoneArrange_RetainedVisibleInBothOutputs(t *testing.T) {
	out := &zoneArrangeOut{
		Sheet: layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825},
		Zones: []zoneArrangeZoneOut{
			{Name: "esp32s3_wroom1_module", Mode: "原形保留(不重排、不重生桩) · 收敛回退:高 737→863 后从「装得进可用域但被图签挡住」掉到「连可用域都装不下」",
				RawW: 449, RawH: 737, FrameW: 449, FrameH: 737, Retained: true,
				RetainWhy: "收敛回退:高 737→863 后从「装得进可用域但被图签挡住」掉到「连可用域都装不下」"},
			{Name: "U_EN", Mode: "无主导锚件 → 全员单列(位号序)", RawW: 164, RawH: 727, FrameW: 175, FrameH: 296},
		},
		Arrange: zaResult{OK: true}, Verdict: "pass",
	}
	var buf strings.Builder
	renderZoneArrange(out, &buf)
	txt := buf.String()
	if !strings.Contains(txt, "↩esp32s3_wroom1_module") {
		t.Errorf("人读输出该给回退区打 ↩:\n%s", txt)
	}
	if !strings.Contains(txt, "收敛回退:高 737→863") {
		t.Errorf("人读输出该带回退理由:\n%s", txt)
	}
	if strings.Contains(txt, "↩U_EN") {
		t.Errorf("没回退的区不该带 ↩:\n%s", txt)
	}
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	js := string(blob)
	if !strings.Contains(js, `"retained":true`) || !strings.Contains(js, `"retainWhy":"收敛回退`) {
		t.Errorf("JSON 该带 retained/retainWhy:%s", js)
	}
	if strings.Count(js, `"retained":`) != 2 {
		t.Errorf("retained 必须**恒定出现**(false 也要在),JSON:%s", js)
	}
}
