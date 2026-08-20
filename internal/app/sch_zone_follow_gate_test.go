package app

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// sch_zone_follow_gate_test.go — phase A「不得变差」门的机械判据。
//
// 缺陷形态(真机 ceshi / 页 MCU_IO,2026-08-20):区 esp32s3_wroom1_module 只有
// 一件 U2(ESP32-S3-WROOM-1,41 脚,本体 71×421),实测 L1 体积 385×421 ——
// marker 横向铺开(各自贴在自己那一行引脚旁边),宽度大而高度不超过本体。
//
//	收敛前 433×541 → 收敛后 244×767;可用高只有 765 → 差 2 个单位排不下
//
// 「不收敛能排,收敛了排不下」—— phase A 违背了它自己存在的理由。
// 下面四组判据把「回退」钉成机械可检的:①真机 case 必须保留原形且理由可见;
// ②接真链 phase B 加门前 blocked、加门后 OK;③负对照(收敛确实变好的区)必须
// 仍然采纳收敛,不许做成「见涨就退」;④确定性。

// ── fixture:真机 case 的可复算重建 ─────────────────────────────────────────

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
func zfGateBuild(desig string, body layoutBBox, pinCount int, mks []zfGateMarker, box *layoutBBox) (zfGroup, layoutBBox) {
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
		part.Pins = append(part.Pins, layoutPin{Number: string(rune('1' + i)), X: m.pinX, Y: m.pinY})
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
	return g, c.Box
}

// zfFixtureESP32Module 是真机那一区:一件 41 脚大符号,8 支标签横向铺开。
// 本体 71×421;L1 体积按真机实测值 385×421 给(平台量的 bbox 比我们预测的包络
// 381×421 略大 —— 体积是观测,不许拿预测替换)。
// 四支 netport 走中段行(TXD0/RXD0/USB_DP/USB_DM),3V3/GND/IO0 在下端行、
// EN 在上端行 —— 这几支物理上照样在左右两侧,但**挂侧判定按 marker 中心相对
// 本体中心的主轴**(zfGroupFromCluster),本体 421 高而横向触达只有百来个单位,
// 于是上下两端那几支的 |dy| 反而更大,被判成 up/down。这就是收敛把它们塞进
// 垂直梯次、把高度顶到 767 的直接机理。
func zfFixtureESP32Module() (zfGroup, layoutBBox) {
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

// zfGateA4Domain 是真机那一页的可行域:A4 1170×825 + 标准图签角 + 默认 opts。
func zfGateA4Domain(opts partitionOpts) (layoutBBox, *layoutBBox, zfDomain) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	keepout, _ := titleBlockKeepout(&sheet)
	return sheet, keepout, zfDomainFor(sheet, keepout, opts)
}

// ── ① 真机 case:加门之后必须保留原形 433×541,理由可见 ──────────────────────

func TestZfGate_BigSymbolRetainsRawShape(t *testing.T) {
	opts := defaultPartitionOpts()
	g, box := zfFixtureESP32Module()

	// fixture 自证:L1 体积 385×421,本体 71×421 —— 与真机实测一致。
	if w, h := box.MaxX-box.MinX, box.MaxY-box.MinY; w != 385 || h != 421 {
		t.Fatalf("fixture L1 体积该是 385×421,得到 %.0f×%.0f", w, h)
	}
	// 根因钉死:上下两端行的标记被判成 up/down(它们物理上在左右两侧)。
	side := map[string]string{}
	for _, tm := range g.Terms {
		side[tm.Net] = tm.Side
	}
	for net, want := range map[string]string{"3V3": "down", "GND": "down", "IO0": "down", "EN": "up",
		"TXD0": "left", "RXD0": "left", "USB_DP": "left", "USB_DM": "left"} {
		if side[net] != want {
			t.Errorf("挂侧判定漂了:%s 该是 %s,得到 %q —— 根因(端行标记被判 up/down)与 fixture 脱钩了", net, want, side[net])
		}
	}

	// 收敛(未加门)= 真机那个负优化:宽收 189、高涨 226。
	conv, err := planZoneFollow("esp32s3_wroom1_module", []zfGroup{g}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if conv.FrameW != 244 || conv.FrameH != 767 {
		t.Fatalf("未加门的收敛框该是真机那个 244×767,得到 %.0f×%.0f", conv.FrameW, conv.FrameH)
	}
	rawW, rawH := zoneArrangeRawFrame(box, opts, 0)
	if rawW != 433 || rawH != 541 {
		t.Fatalf("现状口径框该是 433×541,得到 %.0f×%.0f", rawW, rawH)
	}

	// 加门:必须保留原形。
	_, _, dom := zfGateA4Domain(opts)
	got, err := planZoneFollowGated("esp32s3_wroom1_module", []zfGroup{g}, opts, dom)
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
	// 从多少涨到多少、可用多少」——否则下一个人只会看到框莫名其妙没收敛。
	for _, s := range []string{"收敛回退", "541", "767", "765"} {
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
	g, _ := zfFixtureESP32Module()
	plan, ok := zfRetainPlan("esp32s3_wroom1_module", []zfGroup{g}, defaultPartitionOpts())
	if !ok || len(plan.Groups) != 1 {
		t.Fatalf("原形计划该有 1 个组,ok=%v groups=%d", ok, len(plan.Groups))
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
		t.Errorf("原形计划端子网名多重集 %v ≠ 实测已连接 pin %v —— --apply 断言①会拒整页", got, want)
	}
	// 端子几何仍走唯一那把尺(zfTermGeom):生成期包络 == 复算期包络。
	// 与收敛路径同样的容差口径(sch_zone_follow_test.go 的 ①'):区内局部坐标与
	// 页面绝对坐标的 5 网格相位不同,平移后最多差一格 —— 落地时的平移量是 snap5
	// 的(zaaBuildExec 的 DX/DY),相位不变,所以这一格只是局部坐标系的记账差。
	pg := plan.Groups[0]
	got2, want2 := zfLandedGroupBBox(pg, zfStubPlanned), zfGroupBBox(pg)
	for _, d := range []float64{got2.MinX - want2.MinX, got2.MinY - want2.MinY,
		got2.MaxX - want2.MaxX, got2.MaxY - want2.MaxY} {
		if d > acSchGrid || d < -acSchGrid {
			t.Errorf("原形计划也必须只经 zfTermGeom:生成 %+v 复算 %+v 偏差 %.1f", want2, got2, d)
		}
	}
}

// ── ② 接真链:phase B 加门前 blocked、加门后 OK ─────────────────────────────

// zfGateMCUIOZones 是真机 MCU_IO 那一页的六个区(其余五个是 phase A 的实测输出值),
// 第一个区的尺寸由参数给(加门前 = 收敛 244×767,加门后 = 原形 433×541)。
func zfGateMCUIOZones(w, h float64) []zaZone {
	return []zaZone{
		{Name: "esp32s3_wroom1_module", W: w, H: h, Home: [2]float64{532, 410}},
		{Name: "U_3V3", W: 82, H: 378, Home: [2]float64{200, 600}},
		{Name: "U_EN", W: 175, H: 296, Home: [2]float64{250, 300}},
		{Name: "U_IO0", W: 181, H: 198, Home: [2]float64{800, 600}},
		{Name: "led_indicator_gpio", W: 180, H: 265, Home: [2]float64{900, 400}},
		{Name: "tactile_boot_reset", W: 179, H: 346, Home: [2]float64{850, 250}},
	}
}

func TestZfGate_PhaseBBlockedBeforeOKAfter(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet, keepout, dom := zfGateA4Domain(opts)

	// 门的输入端:两个尺寸就是真机那两个数,判据必须一个可排一个不可排。
	if !dom.fits(433, 541) {
		t.Fatal("原形 433×541 在 A4+图签下该是可排的(图签上方 555 ≥ 541)")
	}
	if dom.fits(244, 767) {
		t.Fatal("收敛后 244×767 该是不可排的(可用高只有 765)")
	}

	before := zonesArrange(zfGateMCUIOZones(244, 767), sheet, keepout, opts)
	if before.OK {
		t.Fatal("加门前该是 blocked —— 复现不了缺陷,后面那半条判据就是自证")
	}
	if before.Blocked != "esp32s3_wroom1_module" {
		t.Errorf("blocked 的该是那个大符号区,得到 %q", before.Blocked)
	}
	t.Logf("加门前:blocked=%s tried=%s", before.Blocked, before.Tried)

	after := zonesArrange(zfGateMCUIOZones(433, 541), sheet, keepout, opts)
	if !after.OK {
		t.Fatalf("加门后 phase B 必须 OK,仍然 blocked=%s(%s)—— 如实报告:门不够,还需要别的手段",
			after.Blocked, after.Tried)
	}
	if len(after.Placed) != 6 {
		t.Fatalf("六个区都该落位,得到 %d", len(after.Placed))
	}
	v := zaValidate(after, sheet, keepout, opts)
	if v.SheetOverflow != 0 || v.PartitionOverlap != 0 || v.TitleBlockHits != 0 || v.SheetMarginHits != 0 {
		t.Errorf("加门后落位该验证全绿,得到 %+v", v)
	}
	for _, p := range after.Placed {
		t.Logf("  %-24s %s shelf=%d [%.0f,%.0f → %.0f,%.0f]", p.Name, p.Edge, p.Shelf,
			p.Rect.MinX, p.Rect.MinY, p.Rect.MaxX, p.Rect.MaxY)
	}
}

// ── ③ 负对照:收敛确实变好的区,加门后必须仍然采纳收敛 ─────────────────────

// 真机 U_EN:164×727 → 175×296。**宽度涨了 11**,高度大降 431。门要是做成
// 「见涨就退」,这一区就会被误回退成一个 727 高的柱子,把页面重新撑爆。
func TestZfGateRegression_NegativeControl_UEn(t *testing.T) {
	_, _, dom := zfGateA4Domain(defaultPartitionOpts())
	if why := zfGateRegression(164, 727, 175, 296, dom); why != "" {
		t.Fatalf("负对照失效:U_EN 收敛(164×727 → 175×296)被误回退 —— %s", why)
	}
	// 反过来:同样是「宽涨」,但涨到本页没有落点时必须回退。
	if why := zfGateRegression(164, 727, 1200, 296, dom); why == "" {
		t.Fatal("宽度涨到超出可用域仍不回退 —— 门对宽这一维是瞎的")
	}
	// 原形本来就排不下时不许拦:收敛是唯一出路。
	if why := zfGateRegression(900, 800, 244, 767, dom); why != "" {
		t.Fatalf("原形本就不可排时该放行收敛,却回退了:%s", why)
	}
	// 两维都不变大 → fits 单调,结构上不可能变差。
	if why := zfGateRegression(433, 541, 400, 500, dom); why != "" {
		t.Fatalf("两维都收窄却回退了:%s", why)
	}
}

// 端到端负对照:一个真的靠收敛救回来的区(实测两件竖向摊了 607,收敛成一列)。
// 宽度同样是**涨的**(148 → 150),高度大降(727 → 297)—— 门必须放行。
func TestZfGate_ConvergenceStillAdoptedWhenItHelps(t *testing.T) {
	opts := defaultPartitionOpts()
	c1, b1 := zfGateBuild("C1", layoutBBox{MinX: 600, MinY: 200, MaxX: 618, MaxY: 222}, 2,
		[]zfGateMarker{
			{"EN", "netport", "left", 600, 211, 580, 211},
			{"GND", "netflag", "down", 609, 200, 609, 160},
		}, nil)
	r1, b2 := zfGateBuild("R1", layoutBBox{MinX: 600, MinY: 651, MaxX: 610, MaxY: 673}, 2,
		[]zfGateMarker{
			{"EN", "netport", "left", 600, 662, 580, 662},
			{"3V3", "netflag", "up", 605, 673, 605, 713},
		}, nil)
	raw, has := b1, true
	zfGrow(&raw, &has, b2)
	rawW, rawH := zoneArrangeRawFrame(raw, opts, 0)

	groups := []zfGroup{c1, r1}
	conv, err := planZoneFollow("U_EN", groups, opts)
	if err != nil {
		t.Fatal(err)
	}
	if conv.FrameW <= rawW {
		t.Fatalf("负对照失去意义:收敛后宽度没有变大(%.0f → %.0f)—— 换个 fixture", rawW, conv.FrameW)
	}
	if conv.FrameH >= rawH {
		t.Fatalf("负对照失去意义:收敛没让高度下降(%.0f → %.0f)", rawH, conv.FrameH)
	}
	_, _, dom := zfGateA4Domain(opts)
	got, err := planZoneFollowGated("U_EN", groups, opts, dom)
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
	t.Logf("负对照:现状 %.0f×%.0f → 收敛 %.0f×%.0f(宽 +%.0f、高 %.0f)已采纳",
		rawW, rawH, conv.FrameW, conv.FrameH, conv.FrameW-rawW, conv.FrameH-rawH)
}

// 逐区独立:一页里一个区回退,不影响另一个区照常收敛。
func TestZfGate_PerZoneIndependent(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	big, _ := zfFixtureESP32Module()
	small, _ := zfGateBuild("C9", layoutBBox{MinX: 100, MinY: 100, MaxX: 118, MaxY: 122}, 2,
		[]zfGateMarker{
			{"3V3", "netflag", "up", 109, 122, 109, 162},
			{"GND", "netflag", "down", 109, 100, 109, 60},
		}, nil)
	a, err := planZoneFollowGated("esp32s3_wroom1_module", []zfGroup{big}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	b, err := planZoneFollowGated("U_3V3", []zfGroup{small}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Retained {
		t.Error("大符号区该回退")
	}
	if b.Retained {
		t.Errorf("小区不该被隔壁的回退传染:%s", b.RetainWhy)
	}
}

// ── ④ 判据本体 + 确定性 ────────────────────────────────────────────────────

// fits 是「本页还存不存在落点」的精确判据(单个矩形障碍),而且对 (w,h) **单调**
// —— 门就是靠单调性省掉「任一维度变大」的显式判断,单调性破了门的推理就垮了。
func TestZfDomainFits(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	left, right, below, above := dom.strips()
	if left != 396 || above != 555 {
		t.Fatalf("A4+图签的通道该是左 396 / 上 555,得到 左%.0f 右%.0f 下%.0f 上%.0f", left, right, below, above)
	}
	for _, tc := range []struct {
		w, h float64
		want bool
		why  string
	}{
		{433, 541, true, "图签上方那条(541 ≤ 555)"},
		{433, 556, false, "上方装不下、又比左侧通道宽"},
		{396, 765, true, "图签左侧整条"},
		{397, 765, false, "比左侧通道宽 1,上方又太高"},
		{244, 767, true, "??"}, // 占位,下面单独判
		{1111, 100, false, "比可用域还宽"},
		{100, 766, false, "比可用域还高"},
	} {
		if tc.w == 244 && tc.h == 767 {
			if dom.fits(244, 767) {
				t.Error("244×767 该判不可排(高 767 > 可用 765)")
			}
			continue
		}
		if got := dom.fits(tc.w, tc.h); got != tc.want {
			t.Errorf("fits(%.0f,%.0f)=%v want %v(%s)", tc.w, tc.h, got, tc.want, tc.why)
		}
	}
	// 单调性:可排的框缩小后仍必须可排。
	for _, base := range [][2]float64{{433, 541}, {396, 765}, {180, 300}} {
		if !dom.fits(base[0], base[1]) {
			t.Fatalf("基准 %v 该可排", base)
		}
		for _, d := range []float64{1, 17, 111} {
			if !dom.fits(base[0]-d, base[1]-d) {
				t.Errorf("单调性破了:%v 可排,但缩小 %g 后不可排", base, d)
			}
		}
	}
	// 无图签的页:只剩可用域约束。
	bare := zfDomainFor(layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}, nil, opts)
	if !bare.fits(1110, 765) || bare.fits(1111, 765) {
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

// 确定性:同输入同输出;判定不依赖输入顺序,也不依赖 map 遍历序
// (端子按几何全序排;门只做算术)。
func TestZfGate_Deterministic(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	g, _ := zfFixtureESP32Module()
	base, err := planZoneFollowGated("esp32s3_wroom1_module", []zfGroup{g}, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := planZoneFollowGated("esp32s3_wroom1_module", []zfGroup{g}, opts, dom)
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
			{Name: "esp32s3_wroom1_module", Mode: "原形保留(不重排、不重生桩) · 收敛回退:高 541→767 后本页无落点",
				RawW: 433, RawH: 541, FrameW: 433, FrameH: 541, Retained: true,
				RetainWhy: "收敛回退:高 541→767 后本页无落点"},
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
	if !strings.Contains(txt, "收敛回退:高 541→767") {
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
