package app

import (
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
	// U3 两个下旗横向散开 —— 同侧但不重叠,合法。
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
