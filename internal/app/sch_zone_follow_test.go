package app

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// fixture:ceshi/P3_USB_DEBUG 真机实测(2026-08-16),与演示页 v3 同源。
// 端子宽高取自 sch clusters 的逐图元 bbox;挂侧取自 marker 相对本体的位置。

func zfFixtureU() []zfGroup {
	return []zfGroup{
		{Designator: "U3", BodyW: 72, BodyH: 92, MultiPin: true, Terms: []zfTerm{
			{Kind: "netport", Net: "MCU_TX", W: 74, Side: "left"},
			{Kind: "netport", Net: "MCU_RX", W: 76, Side: "left"},
			{Kind: "netport", Net: "U3_N3", W: 70, Side: "left"},
			{Kind: "netport", Net: "U3_N4", W: 70, Side: "left"},
			{Kind: "netport", Net: "U3_N5", W: 70, Side: "left"},
			{Kind: "netport", Net: "USB_DTR", W: 82, Side: "right"},
			{Kind: "netport", Net: "USB_RTS", W: 80, Side: "right"},
			{Kind: "netflag", Net: "GND", W: 37, H: 23, Side: "down"},
			{Kind: "netflag", Net: "5V", W: 23, H: 17, Side: "down"},
		}},
		{Designator: "C7", BodyW: 18, BodyH: 22, Terms: []zfTerm{
			{Kind: "netport", Net: "U3_N3", W: 70, Side: "right"},
			{Kind: "netflag", Net: "GND", W: 22, H: 22, Side: "up"}, // 实测竟在上 —— R3 要纠正
		}},
		{Designator: "C8", BodyW: 16, BodyH: 20, Terms: []zfTerm{
			{Kind: "netflag", Net: "5V", W: 12, H: 18, Side: "up"},
			{Kind: "netflag", Net: "GND", W: 20, H: 22, Side: "down"},
		}},
	}
}

func zfFixtureQ() []zfGroup {
	return []zfGroup{
		{Designator: "Q1", BodyW: 12, BodyH: 22, MultiPin: true, Terms: []zfTerm{
			{Kind: "netport", Net: "Q_N3", W: 64, Side: "left"},
			{Kind: "netport", Net: "IO0", W: 58, Side: "right"},
			{Kind: "netport", Net: "USB_DTR", W: 82, Side: "right"},
		}},
		{Designator: "Q2", BodyW: 12, BodyH: 22, MultiPin: true, Terms: []zfTerm{
			{Kind: "netport", Net: "Q_N4", W: 64, Side: "left"},
			{Kind: "netport", Net: "EN", W: 52, Side: "right"},
			{Kind: "netport", Net: "USB_RTS", W: 82, Side: "right"},
		}},
		{Designator: "R5", BodyW: 22, BodyH: 10, Terms: []zfTerm{ // 实测横放 —— R1 要转竖
			{Kind: "netport", Net: "USB_DTR", W: 82, Side: "left"},
			{Kind: "netport", Net: "Q_N4", W: 64, Side: "right"},
		}},
		{Designator: "R6", BodyW: 22, BodyH: 10, Terms: []zfTerm{
			{Kind: "netport", Net: "USB_RTS", W: 82, Side: "left"},
			{Kind: "netport", Net: "Q_N3", W: 64, Side: "right"},
		}},
	}
}

func zfFixtureJ() []zfGroup {
	return []zfGroup{
		{Designator: "J1", BodyW: 70, BodyH: 72, MultiPin: true, Terms: []zfTerm{
			{Kind: "netport", Net: "U3_N4", W: 68, Side: "left"},
			{Kind: "netport", Net: "U3_N5", W: 68, Side: "left"},
			{Kind: "netport", Net: "U3_N6", W: 68, Side: "left"},
			{Kind: "netport", Net: "U3_N4", W: 68, Side: "right"},
			{Kind: "netport", Net: "U3_N5", W: 68, Side: "right"},
			{Kind: "netport", Net: "U3_N7", W: 68, Side: "right"},
		}},
		{Designator: "R3", BodyW: 8, BodyH: 22, Terms: []zfTerm{
			{Kind: "netport", Net: "U3_N6", W: 68, Side: "right"},
			{Kind: "netflag", Net: "GND", W: 38, H: 23, Side: "down"},
		}},
		{Designator: "R4", BodyW: 8, BodyH: 22, Terms: []zfTerm{
			{Kind: "netport", Net: "U3_N7", W: 70, Side: "left"}, // 实测朝左 —— R4 统一朝右
			{Kind: "netflag", Net: "GND", W: 20, H: 22, Side: "down"},
		}},
	}
}

func zfFind(p zfZonePlan, d string) *zfPlacedGroup {
	for i := range p.Groups {
		if p.Groups[i].Designator == d {
			return &p.Groups[i]
		}
	}
	return nil
}

// U 区:锚件 U3 + 卫星排(下);C7/C8 竖放平行、GND 全部归下 —— 用户裁定的
// 「C7 C8 跟随主芯片的布局放置」正是这一幕。
func TestPlanZoneFollow_UZone(t *testing.T) {
	p, err := planZoneFollow("U", zfFixtureU(), defaultPartitionOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Mode, "U3") || !strings.Contains(p.Mode, "排(下,竖放平行)") {
		t.Fatalf("U 区该是锚件+卫星排(下),得到 %q", p.Mode)
	}
	u3, c7, c8 := zfFind(p, "U3"), zfFind(p, "C7"), zfFind(p, "C8")
	if u3 == nil || c7 == nil || c8 == nil {
		t.Fatal("三个组都该在输出里")
	}
	// R1:卫星竖放平行(本体高 ≥ 宽)。
	for _, g := range []*zfPlacedGroup{c7, c8} {
		if g.Body.MaxX-g.Body.MinX > g.Body.MaxY-g.Body.MinY {
			t.Errorf("%s 该竖放,得到 %.0f×%.0f", g.Designator, g.Body.MaxX-g.Body.MinX, g.Body.MaxY-g.Body.MinY)
		}
	}
	// R2:排在锚件下方,顶边对齐。
	if c7.Body.MaxY >= u3.Body.MinY || c8.Body.MaxY >= u3.Body.MinY {
		t.Error("卫星该在锚件下方")
	}
	// R3:C7 的 GND 实测在上,规划必须纠正到下(推论,不是查表)。
	for _, g := range []*zfPlacedGroup{c7, c8} {
		for _, tm := range g.Terms {
			if tidyNetClass(tm.Net) == "ground" && tm.Dir != "down" {
				t.Errorf("%s 的 GND 该朝下,得到 %s", g.Designator, tm.Dir)
			}
			if tidyNetClass(tm.Net) == "power" && tm.Dir != "up" {
				t.Errorf("%s 的 %s 该朝上,得到 %s", g.Designator, tm.Net, tm.Dir)
			}
		}
	}
	// 锚件的多脚端子保持实测侧(MCU_* 朝左,USB_* 朝右)。
	for _, tm := range u3.Terms {
		if strings.HasPrefix(tm.Net, "MCU_") && tm.Dir != "left" {
			t.Errorf("U3 %s 该保持朝左,得到 %s", tm.Net, tm.Dir)
		}
		if strings.HasPrefix(tm.Net, "USB_") && tm.Dir != "right" {
			t.Errorf("U3 %s 该保持朝右,得到 %s", tm.Net, tm.Dir)
		}
	}
}

// Q 区:无主导锚件 → 全员单列;R5/R6 转竖(Rotated),原左端(USB_*)在上。
func TestPlanZoneFollow_QZone(t *testing.T) {
	p, err := planZoneFollow("Q", zfFixtureQ(), defaultPartitionOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Mode, "单列") {
		t.Fatalf("Q 区无主导锚件该单列,得到 %q", p.Mode)
	}
	for _, d := range []string{"R5", "R6"} {
		g := zfFind(p, d)
		if g == nil || !g.Rotated {
			t.Fatalf("%s 实测横放,该标记 Rotated 转竖", d)
		}
		// 原左脚(USB_*)→ 上:上端子的 y 高于下端子。
		var upNet string
		bestY := -1e18
		for _, tm := range g.Terms {
			if c := (tm.BBox.MinY + tm.BBox.MaxY) / 2; c > bestY {
				bestY, upNet = c, tm.Net
			}
		}
		if !strings.HasPrefix(upNet, "USB_") {
			t.Errorf("%s 原左脚 USB_* 该在上,上端却是 %s", d, upNet)
		}
	}
	// 位号序:Q1 在最上,R6 在最下。
	q1, r6 := zfFind(p, "Q1"), zfFind(p, "R6")
	if q1.Body.MinY <= r6.Body.MinY {
		t.Error("单列该按位号序自上而下:Q1 高于 R6")
	}
}

// J 区:R4 的 netport 实测朝左,R4 是无源件 → R4 统一朝右。
func TestPlanZoneFollow_JZone_PortsUnifiedRight(t *testing.T) {
	p, err := planZoneFollow("J_USB", zfFixtureJ(), defaultPartitionOpts())
	if err != nil {
		t.Fatal(err)
	}
	r4 := zfFind(p, "R4")
	for _, tm := range r4.Terms {
		if tm.Kind == "netport" && tm.Dir != "right" {
			t.Errorf("无源件 port 该统一朝右,R4 %s 得到 %s", tm.Net, tm.Dir)
		}
	}
	// 锚件 J1 的双侧 port 保持双侧(USB 引脚天生两侧,不翻)。
	j1 := zfFind(p, "J1")
	left, right := 0, 0
	for _, tm := range j1.Terms {
		switch tm.Dir {
		case "left":
			left++
		case "right":
			right++
		}
	}
	if left != 3 || right != 3 {
		t.Errorf("J1 端子该保持 3左3右,得到 %d左%d右", left, right)
	}
}

// R5 硬不变式:端子重叠必须在规划期炸,单独校验。
func TestZfCheckTermOverlap(t *testing.T) {
	bad := zfPlacedGroup{Designator: "X", Terms: []zfPlacedTerm{
		{Net: "A", Dir: "down", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 20, MaxY: 20}},
		{Net: "B", Dir: "down", BBox: layoutBBox{MinX: 10, MinY: 10, MaxX: 30, MaxY: 30}},
	}}
	if err := zfCheckTermOverlap(bad); err == nil {
		t.Fatal("重叠端子该报 R5 违例")
	}
	// U3 两个下旗垂直梯次 —— 同侧但不重叠,合法。
	p, err := planZoneFollow("U", zfFixtureU(), defaultPartitionOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range p.Groups {
		if err := zfCheckTermOverlap(g); err != nil {
			t.Errorf("布置后端子不该重叠:%v", err)
		}
	}
}

// 确定性:输入顺序无关。
func TestPlanZoneFollow_OrderIndependent(t *testing.T) {
	base, err := planZoneFollow("U", zfFixtureU(), defaultPartitionOpts())
	if err != nil {
		t.Fatal(err)
	}
	rev := zfFixtureU()
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	got, err := planZoneFollow("U", rev, defaultPartitionOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base, got) {
		t.Fatal("输入顺序改变了输出 —— 有序依赖")
	}
}

// 收敛必须真的收敛:三个区收敛后的框都要窄于现状口径框(这是 phase B 有解的前提)。
func TestPlanZoneFollow_ShrinksP3Zones(t *testing.T) {
	rawW := map[string]float64{"U": 682, "Q": 572, "J_USB": 486}
	for zone, fix := range map[string][]zfGroup{"U": zfFixtureU(), "Q": zfFixtureQ(), "J_USB": zfFixtureJ()} {
		p, err := planZoneFollow(zone, fix, defaultPartitionOpts())
		if err != nil {
			t.Fatal(err)
		}
		if p.FrameW >= rawW[zone] {
			t.Errorf("%s 收敛后框宽 %.0f 未小于现状 %.0f", zone, p.FrameW, rawW[zone])
		}
		t.Logf("%s: %s → 框 %.0f×%.0f", zone, p.Mode, p.FrameW, p.FrameH)
	}
}

// 上/下侧多旗:垂直梯次。connect_pin 的桩只能从 pin 沿 direction 直出,pin 的 x
// 由符号锁死 —— 「横向散开」执行侧表达不了,落地全退默认桩长,pitch 10 的相邻
// 引脚上旗当场竖叠(P1 U1 打地鼠真因,人肉梯次 20/50/85 顶了算法的班)。钉死:
// 同侧旗 Offset 严格递增,且后旗桩长越过前旗旗体;左右侧端子恒 zfStub。
func TestZfGenMultiPin_TopBottomFlagsVerticalLadder(t *testing.T) {
	g := zfGenMultiPin(zfGroup{Designator: "U1", BodyW: 60, BodyH: 40, MultiPin: true,
		Terms: []zfTerm{
			{Kind: "netflag", Net: "3V3", W: 24, H: 18, Side: "up"},
			{Kind: "netflag", Net: "VBUS", W: 30, H: 18, Side: "up"},
			{Kind: "netflag", Net: "5V", W: 20, H: 18, Side: "up"},
			{Kind: "netport", Net: "IO0", W: 40, Side: "left"},
		}})
	var ups []zfPlacedTerm
	for _, tm := range g.Terms {
		switch tm.Dir {
		case "up":
			ups = append(ups, tm)
		case "left", "right":
			if tm.Offset != zfStub {
				t.Errorf("左右端子桩长该恒 %g,%s 得到 %g", zfStub, tm.Net, tm.Offset)
			}
		}
	}
	if len(ups) != 3 {
		t.Fatalf("该有 3 只上旗,得到 %d", len(ups))
	}
	for i, tm := range ups {
		if tm.Offset <= 0 {
			t.Errorf("旗 %s 缺 Offset —— apply 不带 offset 就退回默认桩长(竖叠复发)", tm.Net)
		}
		if i == 0 {
			continue
		}
		prev := ups[i-1]
		if tm.Offset <= prev.Offset {
			t.Errorf("梯次不增:%s %g ≤ %s %g", tm.Net, tm.Offset, prev.Net, prev.Offset)
		}
		if want := prev.Offset + (prev.BBox.MaxY - prev.BBox.MinY); tm.Offset < want {
			t.Errorf("%s 桩长 %g 没越过前旗旗体(需 ≥ %g)—— 落地仍会叠", tm.Net, tm.Offset, want)
		}
	}
	if err := zfCheckTermOverlap(g); err != nil {
		t.Errorf("梯次布置不该触发 R5:%v", err)
	}
}

// 左/右侧连续旗:水平梯次。执行侧旗的 y 跟 pin 锁死(真 pin pitch 10 < 旗高 12+),
// 相邻同向旗纵向必叠 —— P3 真机 J2 左侧 5V/GND 相邻脚旗深叠,与 U1 三旗竖叠
// 同病转 90°。port 恒水平高 11 < pitch,保持短桩不参与梯次。
func TestZfGenMultiPin_SideFlagsHorizontalLadder(t *testing.T) {
	g := zfGenMultiPin(zfGroup{Designator: "J2", BodyW: 70, BodyH: 72, MultiPin: true,
		Terms: []zfTerm{
			{Kind: "netport", Net: "U3_N4", W: 68, Side: "left"},
			{Kind: "netflag", Net: "5V", W: 23, H: 17, Side: "left"},
			{Kind: "netflag", Net: "GND", W: 37, H: 23, Side: "left"},
			{Kind: "netport", Net: "U3_N7", W: 68, Side: "left"},
		}})
	var flags []zfPlacedTerm
	for _, tm := range g.Terms {
		if tm.Kind == "netport" {
			if tm.Offset != zfStub {
				t.Errorf("port %s 该保持短桩 %g,得到 %g", tm.Net, zfStub, tm.Offset)
			}
			continue
		}
		flags = append(flags, tm)
	}
	if len(flags) != 2 {
		t.Fatalf("该有 2 只旗,得到 %d", len(flags))
	}
	if flags[0].Offset != zfStub {
		t.Errorf("首旗桩长该 %g,得到 %g", zfStub, flags[0].Offset)
	}
	w0 := flags[0].BBox.MaxX - flags[0].BBox.MinX
	if want := zfStub + w0 + zfFlagGap; flags[1].Offset != want {
		t.Errorf("次旗桩长该 %g(首旗宽 %g + gap 递增),得到 %g", want, w0, flags[1].Offset)
	}
}

// ── 收敛性:预测 = 落地(2026-08-20 缺陷的机械判据)──────────────────────────
//
// 缺陷形态:真机 4 轮 `zone-arrange --apply`,每轮 dry-run 都 pass、validation
// 四项全 0,落地实测必然重叠(2/1/—/2 处);规划 315×351 → 落地 353×382,
// 而 gutter 只有 12 —— 误差系统性大于间距。根因是「桩线伸展」有三套算法。
//
// 这条测试钉住其中的规划 ↔ 落地那一对:
//
//	① 生成期累加出来的组包络 == 复算期按 zfTermGeom 重走一遍的包络
//	   (两条独立代码路径;有人绕过 zfTermGeom 手改盒子就会不等);
//	② 用**落地侧桩线策略**重算的框尺寸,与规划框的偏差 ≤ gutter;
//	③ **负对照**:把落地侧换回旧的自由 offset 策略(autoconnect 评分:首支
//	   OffsetMin、同侧后续按 laneStepFor 让开一整个占地),②必须失败。
//	   没有③,②只是在自证。
func TestZfLandedFrame_PredictionEqualsLanding(t *testing.T) {
	opts := defaultPartitionOpts()
	fixtures := map[string][]zfGroup{"U": zfFixtureU(), "Q": zfFixtureQ(), "J_USB": zfFixtureJ()}
	names := []string{"J_USB", "Q", "U"} // 定序,失败信息可复现
	for _, zone := range names {
		// ① 两条路径同一几何 —— 在**未平移**的生成坐标系上判,必须逐字相等。
		// (平移后两边的 5 网格相位不同,那一格是结构性余量,见下面的 ①'。)
		for _, in := range fixtures[zone] {
			g, err := zfGenGroup(in)
			if err != nil {
				t.Fatalf("%s/%s: %v", zone, in.Designator, err)
			}
			got, want := zfLandedGroupBBox(g, zfStubPlanned), zfGroupBBox(g)
			if got != want {
				t.Errorf("%s/%s 生成期包络 %+v ≠ 复算期包络 %+v —— 端子几何又分家了(必须只经 zfTermGeom)",
					zone, in.Designator, want, got)
			}
		}
		p, err := planZoneFollow(zone, fixtures[zone], opts)
		if err != nil {
			t.Fatalf("%s: %v", zone, err)
		}
		// ①' 平移后仍必须在**一格之内**(endpointFor 的 5 网格吸附相位差),
		// 而且这一格已经由 zfLandSlack 算进框里。
		for _, g := range p.Groups {
			got, want := zfLandedGroupBBox(g, zfStubPlanned), zfGroupBBox(g)
			for _, d := range []float64{got.MinX - want.MinX, got.MinY - want.MinY,
				got.MaxX - want.MaxX, got.MaxY - want.MaxY} {
				if math.Abs(d) > zfLandSlack {
					t.Errorf("%s/%s 平移后包络偏差 %.1f > 落地余量 %.0f:生成 %+v 复算 %+v",
						zone, g.Designator, d, float64(zfLandSlack), want, got)
				}
			}
		}
		// ② 规划框 = 落地框(偏差 ≤ gutter)。
		lw, lh := zfLandedFrame(p, opts, zfStubPlanned)
		if dw, dh := math.Abs(lw-p.FrameW), math.Abs(lh-p.FrameH); dw > opts.Gutter || dh > opts.Gutter {
			t.Errorf("%s 规划框 %.0f×%.0f 与落地框 %.0f×%.0f 偏差 %.0f/%.0f > gutter %.0f",
				zone, p.FrameW, p.FrameH, lw, lh, dw, dh, opts.Gutter)
		}
		// ③ 负对照:旧的自由 offset 策略必须把断言②打穿。
		fw, fh := zfLandedFrame(p, opts, zfStubFreeAutoconnect)
		if math.Abs(fw-p.FrameW) <= opts.Gutter && math.Abs(fh-p.FrameH) <= opts.Gutter {
			t.Errorf("%s 负对照失效:自由 offset 策略下落地框 %.0f×%.0f 仍在 gutter 内(规划 %.0f×%.0f)—— 断言②钉不住任何东西",
				zone, fw, fh, p.FrameW, p.FrameH)
		}
		t.Logf("%s 规划 %.0f×%.0f / 落地(计划桩)%.0f×%.0f / 落地(自由 offset)%.0f×%.0f",
			zone, p.FrameW, p.FrameH, lw, lh, fw, fh)
	}
}

// 端子几何必须走落地侧那条链:marker 本体从端点起空出 Near 才开始画(首版把盒子
// 贴在端点上,每支端子少算 9.5 —— 两侧就是 19,已经超过 gutter 12)。
func TestZfTermGeom_MatchesLandingChain(t *testing.T) {
	// 朝上的 GND 旗:pin (100,100),桩 20 → 端点 (100,120)。
	wire, marker := zfTermGeom(100, 100, 20, "up", "netflag", "GND", 0)
	if wire != (layoutBBox{MinX: 100, MinY: 100, MaxX: 100, MaxY: 120}) {
		t.Errorf("桩线段该是 pin→端点的竖直段,got %+v", wire)
	}
	if want := predictedMarkerBBox(100, 120, "ground", "up", "GND"); marker != want {
		t.Errorf("marker 包络必须 == predictedMarkerBBox(落点评分/`sch check` 同一把尺):got %+v want %+v", marker, want)
	}
	if marker.MinY <= 120 {
		t.Errorf("旗体该从端点上方 Near 处才开始,got MinY=%.1f", marker.MinY)
	}
	// SpreadX 只加在 x 上(梯次要用的纵向占地不能被它污染)。
	_, wide := zfTermGeom(100, 100, 20, "up", "netflag", "GND", 30)
	if wide.MinX != marker.MinX-30 || wide.MaxX != marker.MaxX+30 ||
		wide.MinY != marker.MinY || wide.MaxY != marker.MaxY {
		t.Errorf("SpreadX 该只展宽 x:base %+v spread %+v", marker, wide)
	}
}

// 无源件的 netport 必须是**水平**桩(R4 恒水平朝右):首版画成「桩朝下、标签朝右」,
// 那个形态 connect_pin 造不出来(桩只能沿 direction 直出),于是规划的高虚高、宽虚低。
func TestZfGenPassive_PortStubIsHorizontal(t *testing.T) {
	g, err := zfGenPassive(zfGroup{Designator: "R9", BodyW: 8, BodyH: 22, Terms: []zfTerm{
		{Kind: "netport", Net: "U3_N6", W: 68, Side: "right"},
		{Kind: "netflag", Net: "GND", W: 20, H: 22, Side: "down"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tm := range g.Terms {
		if tm.Kind != "netport" {
			continue
		}
		if tm.Dir != "right" {
			t.Fatalf("无源件 port 该朝右,got %s", tm.Dir)
		}
		// 桩沿 direction 直出 → 端点与 pin 同 y。
		if tm.BBox.MinY > tm.PinY || tm.BBox.MaxY < tm.PinY {
			t.Errorf("port 标签该落在 pin 的水平线上(桩只能沿 direction 直出),pinY=%.1f bbox=%+v", tm.PinY, tm.BBox)
		}
		if tm.BBox.MinX <= tm.PinX {
			t.Errorf("port 该在 pin 右侧,pinX=%.1f bbox=%+v", tm.PinX, tm.BBox)
		}
	}
}
