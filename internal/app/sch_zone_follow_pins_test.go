package app

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// sch_zone_follow_pins_test.go — phase A **必须知道引脚在哪**(2026-08-20 真机定案)。
//
// ── 缺陷:断言③ 在每一页恒红,报文自称「计划未覆盖」──────────────────────────
//
// 真机 ceshi / 页 MCU_IO 连跑三轮 `zone-arrange --apply`,每轮断言③ 都红,区框
// 重叠只从 29 收敛到 19、不到 0。报文点名的「计划未覆盖」清单**全部是 GND 侧引脚**:
//
//	C4:2 C5:2 C6:2 LED1:2 SW1:2 SW2:2 U2:1 U2:40 U2:41   (MCU_IO,9 只)
//	C7:2 C8:2 D1:3 J1:8 J1:9 J1:10 J1:11 J1:A1B12 J1:B1A12 R6:2 R7:2 U3:1  (USB_DEBUG,12 只)
//
// ── 复现取证(2026-08-20 第四轮 --apply 全量日志)────────────────────────────
//
// 「GND 全军覆没」不是巧合,链条是这样的:
//
//	① 规划器**假定**两脚无源件的引脚在本体上下缘中线上,再按 R3 把 GND 派到下端;
//	② 真机 C4 / C6 是 rot 90 的电容,pin1(+3V3)在本体**下方**、pin2(GND)在
//	   本体**上方** —— 正好是反的。zaaMapTerms 按网名把 GND 端子映到物理上在上面
//	   的那只脚,却带着计划给的 direction=down;+3V3 端子映到下面那只脚,带着
//	   direction=up。两根桩线双双钻进本体、共线合并;
//	③ 一处合并 = **GND 整张网并进 +3V3**。页级对账当场红,内核恢复段把全页每一只
//	   地脚 replace 掉重连(日志逐条写着 `[replaced net "+3V3"]`),几何由 autoconnect
//	   自由评分挑 —— 于是「全部地脚」进了 FreeConnected,区框凭空胖一档;
//	④ 报文那句「计划未覆盖」是这条路径的**产物**,不是原因(计划里其实有那 9 支
//	   端子)。真正的「没覆盖」是另一条:cluster 的「专属 marker」规则不把共树
//	   marker 算给本组,逐 marker 折端子的话共树 pin 在计划里根本不存在。
//
// 两条都指向同一处:**规划器手里没有引脚坐标**。修法是让计划端子逐 pin 从活体折出
// 并带上引脚位置(zfGroupFromCluster + zfGenPassive/zfGenMultiPinMeasured)。
//
// 本文件的四组对照都用**真机 MCU_IO 的实测几何**(bodies / pins / nets 逐字来自
// 2026-08-20 `sch list --include-pins --include-bbox`):
//
//	正对照    计划端子集合 ⊇ 落地会重建的端子集合(「计划未覆盖」为空)+ 按执行
//	          指令复算的落地框不比规划框胖 + 区框零重叠(判据本体 zaaRecheckFindings)
//	针对性    电容的 GND 端子进入了计划(共树时首版逐 marker 折会整支丢掉)
//	负对照 A  计划覆盖齐全但落地就是胖了 → 断言③ 照样红(判据没有被改松)
//	负对照 B  同件两旗异向:C4/C6 的两支旗不许被排成同向(自短路防线)

// ── fixture:真机 MCU_IO 的实测几何 ─────────────────────────────────────────

// zfPinsPart 是一件的实测:本体盒 + 逐 pin 坐标与网名。
type zfPinsPart struct {
	desig string
	body  layoutBBox
	pins  []zfPinsPin
}

type zfPinsPin struct {
	num  string
	x, y float64
	net  string
}

// zfMCUIOParts 逐字来自真机(页 MCU_IO,工程 ceshi,2026-08-20)。
// U2 只列**已连接**的 9 只脚(其余 32 只浮空,不参与端子计划)。
func zfMCUIOParts() []zfPinsPart {
	return []zfPinsPart{
		{"U2", layoutBBox{MinX: 164.5, MinY: 309.5, MaxX: 235.5, MaxY: 730.5}, []zfPinsPin{
			{"2", 155, 535, "+3V3"}, {"3", 155, 525, "MCU_EN"},
			{"36", 155, 515, "MCU_RX"}, {"37", 155, 505, "MCU_TX"},
			{"41", 245, 320, "GND"}, {"40", 245, 350, "GND"}, {"1", 245, 360, "GND"},
			{"38", 245, 700, "LED_CTRL"}, {"27", 245, 720, "MCU_IO0"},
		}},
		// C4 / C6:**rot 90 的电容,+3V3 脚在本体下方、GND 脚在上方** —— 缺陷的震中。
		{"C4", layoutBBox{MinX: 621.5, MinY: 674.5, MaxX: 638.5, MaxY: 695.5}, []zfPinsPin{
			{"1", 630, 665, "+3V3"}, {"2", 630, 705, "GND"},
		}},
		{"C6", layoutBBox{MinX: 801.5, MinY: 674.5, MaxX: 818.5, MaxY: 695.5}, []zfPinsPin{
			{"1", 810, 665, "+3V3"}, {"2", 810, 705, "GND"},
		}},
		{"C5", layoutBBox{MinX: 666.5, MinY: 709.5, MaxX: 683.5, MaxY: 730.5}, []zfPinsPin{
			{"1", 675, 700, "MCU_EN"}, {"2", 675, 740, "GND"},
		}},
		{"R3", layoutBBox{MinX: 625.5, MinY: 539.5, MaxX: 634.5, MaxY: 560.5}, []zfPinsPin{
			{"1", 630, 530, "MCU_EN"}, {"2", 630, 570, "+3V3"},
		}},
		{"R4", layoutBBox{MinX: 765.5, MinY: 539.5, MaxX: 774.5, MaxY: 560.5}, []zfPinsPin{
			{"1", 770, 530, "MCU_IO0"}, {"2", 770, 570, "+3V3"},
		}},
		{"SW1", layoutBBox{MinX: 997.5, MinY: 689.5, MaxX: 1007.5, MaxY: 730.5}, []zfPinsPin{
			{"1", 1000, 740, "MCU_IO0"}, {"2", 1000, 680, "GND"},
		}},
		{"SW2", layoutBBox{MinX: 997.5, MinY: 569.5, MaxX: 1007.5, MaxY: 610.5}, []zfPinsPin{
			{"1", 1000, 620, "MCU_EN"}, {"2", 1000, 560, "GND"},
		}},
		// LED1:**两脚是左右的**(本体却比宽高),首版模型会给它派 up/down。
		{"LED1", layoutBBox{MinX: 424.5, MinY: 706.5, MaxX: 445.5, MaxY: 732.5}, []zfPinsPin{
			{"1", 460, 715, "LED1_N2"}, {"2", 420, 715, "GND"},
		}},
		{"R5", layoutBBox{MinX: 425.5, MinY: 604.5, MaxX: 434.5, MaxY: 625.5}, []zfPinsPin{
			{"1", 430, 635, "LED_CTRL"}, {"2", 430, 595, "LED1_N2"},
		}},
	}
}

func zfMCUIOZoneClaims() map[string]*schZoneClaim {
	return map[string]*schZoneClaim{
		"led_indicator_gpio(LED1)": {Parts: []string{"LED1", "R5"}},
		"tactile_boot_reset(SW1)":  {Parts: []string{"SW1", "SW2"}},
		"wroom-core":               {Parts: []string{"U2"}},
		"wroom-passives":           {Parts: []string{"C4", "C5", "C6", "R3", "R4"}},
	}
}

// zfPinsKindFor 是 fixture 里「这只脚该挂什么标记」的口径:电源/地挂旗,其余挂
// netport —— 与 zaaPadTermsToPins 的合成口径同一条(tidyNetClass)。
func zfPinsKindFor(net string) string {
	if cls := tidyNetClass(net); cls == "ground" || cls == "power" {
		return "netflag"
	}
	return "netport"
}

// zfPinsScene 把 fixture 折成一份**页面场景快照**(comps + wires + 活体网表):
// 图框 + 每件本体 + 每只已连接 pin 一支「桩线 + marker」。
//
// shareGND:把 C4:2 与 C6:2 并到**同一棵导线树**上、树上只挂一支 GND 旗
// (真机 block-apply 之后很常见的形态)。cluster 的「专属 marker」规则会让这支旗
// 只归其中一件,于是首版逐 marker 折端子时另一件的 GND **整支消失** —— 针对性
// fixture 要的就是这个。
func zfPinsScene(shareGND bool) ([]layoutComp, []schGroupWire, map[string]map[string]bool) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	comps := []layoutComp{{ID: "sheet", ComponentType: "sheet", BBox: &sheet}}
	var wires []schGroupWire
	live := map[string]map[string]bool{}
	addNet := func(net, ref string) {
		if live[net] == nil {
			live[net] = map[string]bool{}
		}
		live[net][strings.ToUpper(ref)] = true
	}
	addMarker := func(id, kind, net string, x, y float64, dir string) {
		canon := zfCanonKind(kind, net)
		mb := predictedMarkerBody(x, y, canon, dir, net)
		rot, err := tidyLabelRotation(canon, dir)
		if err != nil {
			panic(err)
		}
		r := rot
		comps = append(comps, layoutComp{ID: id, ComponentType: kind, Net: net,
			X: x, Y: y, AnchorAvailable: true, BBox: &mb, Rotation: &r})
	}
	for _, p := range zfMCUIOParts() {
		bb := p.body
		rot := 0.0
		part := layoutComp{ID: "pid-" + p.desig, Designator: p.desig, ComponentType: "part",
			X: (bb.MinX + bb.MaxX) / 2, Y: (bb.MinY + bb.MaxY) / 2,
			AnchorAvailable: true, Rotation: &rot, BBox: &bb, PinsAvailable: true}
		for _, pin := range p.pins {
			part.Pins = append(part.Pins, layoutPin{Number: pin.num, X: pin.x, Y: pin.y})
			addNet(pin.net, p.desig+"."+pin.num)
			if shareGND && pin.net == "GND" && (p.desig == "C4" || p.desig == "C6") {
				continue // 共树:统一在下面接
			}
			dir := zfPointSideOf(p.body, pin.x, pin.y)
			kind := zfPinsKindFor(pin.net)
			if kind == "netport" && dir != "left" && dir != "right" {
				dir = "right"
			}
			ex, ey := endpointFor(pin.x, pin.y, zfStub, dir)
			addMarker(fmt.Sprintf("m-%s-%s", p.desig, pin.num), kind, pin.net, ex, ey, dir)
			wires = append(wires, schGroupWire{ID: fmt.Sprintf("w-%s-%s", p.desig, pin.num),
				Points: []float64{pin.x, pin.y, ex, ey}})
		}
		comps = append(comps, part)
	}
	if shareGND {
		// C4:2 (630,705) ─┐          ┌─ C6:2 (810,705)
		//                 └ (630,725) ┴ (810,725)   旗锚在这条横线中点
		wires = append(wires,
			schGroupWire{ID: "w-gnd-tree", Points: []float64{630, 705, 630, 725, 810, 725, 810, 705}})
		addMarker("m-gnd-shared", "netflag", "GND", 720, 725, "up")
		wires = append(wires, schGroupWire{ID: "w-gnd-flag", Points: []float64{720, 725, 720, 726}})
	}
	return comps, wires, live
}

// zfPinsGroups 走生产折算链(与 planZoneArrangeScene 逐字同源)拿到一个区的
// zfGroup;byMarker=true 时**强制走首版的逐 marker 口径**(变异对照)。
func zfPinsGroups(comps []layoutComp, wires []schGroupWire, parts []string, byMarker bool) []zfGroup {
	clusters, _ := buildSchClusters(comps, wires)
	byDesig := map[string]schCluster{}
	for _, c := range clusters {
		byDesig[strings.ToUpper(c.Designator)] = c
	}
	partOf := map[string]layoutComp{}
	pinCount := map[string]int{}
	var markers []layoutComp
	for _, c := range comps {
		if c.ComponentType == "part" {
			partOf[strings.ToUpper(label(c))] = c
			pinCount[strings.ToUpper(label(c))] = len(c.Pins)
		}
		if isSchMarker(c.ComponentType) {
			markers = append(markers, c)
		}
	}
	roots := tidyWireRoots(wires)
	var out []zfGroup
	for _, d := range parts {
		u := strings.ToUpper(d)
		c, ok := byDesig[u]
		if !ok {
			continue
		}
		measured := zfMeasureCluster(c, partOf[u], wires, roots, markers)
		if byMarker {
			out = append(out, zfGroupFromCluster(c, pinCount[u], nil))
			continue
		}
		g := zfGroupFromCluster(c, pinCount[u], measured)
		g.Measured = measured
		out = append(out, g)
	}
	return out
}

func zfTermNets(g zfGroup) []string {
	var out []string
	for _, t := range g.Terms {
		out = append(out, t.Net)
	}
	sort.Strings(out)
	return out
}

// ── 针对性:电容的 GND 端子必须进入计划 ─────────────────────────────────────

func TestZfGroupFromCluster_CapacitorGroundTerminalEntersThePlan(t *testing.T) {
	comps, wires, _ := zfPinsScene(true) // C4:2 与 C6:2 共树,一支旗
	parts := []string{"C4", "C6"}

	// 变异对照:首版逐 marker 折 —— 共树的那支旗只归一件,另一件的 GND 整支消失。
	legacy := zfPinsGroups(comps, wires, parts, true)
	lost := 0
	for _, g := range legacy {
		if !strings.Contains(strings.Join(zfTermNets(g), ","), "GND") {
			lost++
		}
	}
	if lost == 0 {
		t.Fatal("变异对照失效:逐 marker 折居然一支 GND 都没丢 —— 这份 fixture 不再复现覆盖缺陷")
	}
	t.Logf("变异对照:逐 marker 折时 %d/%d 个电容的 GND 端子不在计划里", lost, len(legacy))

	// 修复后:逐 pin 折,两件都必须有 GND,而且引脚坐标要带进计划。
	got := zfPinsGroups(comps, wires, parts, false)
	if len(got) != 2 {
		t.Fatalf("该有 2 个组,得到 %d", len(got))
	}
	for _, g := range got {
		nets := zfTermNets(g)
		if len(nets) != 2 || nets[0] != "+3V3" || nets[1] != "GND" {
			t.Errorf("%s 的计划端子该是 [+3V3 GND],得到 %v", g.Designator, nets)
		}
		for _, tm := range g.Terms {
			if !tm.HasPin {
				t.Errorf("%s 的 %s 端子没有引脚坐标 —— 规划器又只能靠假定了", g.Designator, tm.Net)
			}
		}
	}
}

// 真机 C4:pin1(+3V3)在本体**下方**、pin2(GND)在**上方**。规划必须照这个事实
// 出桩(GND 朝上、+3V3 朝下),而不是照「电源上 / 地下」的假定把两根桩都塞进本体。
func TestZfGenPassive_FollowsRealPinEndsNotTheAssumedOnes(t *testing.T) {
	comps, wires, _ := zfPinsScene(false)
	for _, tc := range []struct {
		desig string
		want  map[string]string // 网名 → 期望的桩方向
	}{
		{"C4", map[string]string{"+3V3": "down", "GND": "up"}},
		{"C6", map[string]string{"+3V3": "down", "GND": "up"}},
		{"C5", map[string]string{"MCU_EN": "right", "GND": "up"}},
		{"R3", map[string]string{"MCU_EN": "right", "+3V3": "up"}},
		{"SW1", map[string]string{"MCU_IO0": "right", "GND": "down"}},
		// LED1 两脚是左右的:首版模型会派 up/down,真实朝外方向是 left/right。
		{"LED1", map[string]string{"LED1_N2": "right", "GND": "left"}},
	} {
		gs := zfPinsGroups(comps, wires, []string{tc.desig}, false)
		if len(gs) != 1 {
			t.Fatalf("%s:折不出组", tc.desig)
		}
		pg, err := zfGenGroup(gs[0])
		if err != nil {
			t.Fatalf("%s: %v", tc.desig, err)
		}
		got := map[string]string{}
		for _, tm := range pg.Terms {
			got[tm.Net] = tm.Dir
		}
		for net, want := range tc.want {
			if got[net] != want {
				t.Errorf("%s 的 %s 桩该朝 %s(引脚朝外方向),得到 %q —— 全部方向 %v",
					tc.desig, net, want, got[net], got)
			}
		}
		// 桩线不许钻进本体:两支桩的包络与本体盒不相交(合并短路的几何前提)。
		body := pg.Body
		for i, w := range pg.Wires {
			if boxesOverlap(w, body) {
				t.Errorf("%s 第 %d 支桩线钻进了本体 %v(桩 %v)—— 同件两桩会共线合并",
					tc.desig, i, body, w)
			}
		}
	}
}

// ── 负对照 B:同件两旗异向(自短路防线)───────────────────────────────────────

func TestZfGenPassive_TwoFlagsNeverSameDirection(t *testing.T) {
	comps, wires, _ := zfPinsScene(false)
	for _, p := range zfMCUIOParts() {
		gs := zfPinsGroups(comps, wires, []string{p.desig}, false)
		if len(gs) != 1 || gs[0].MultiPin {
			continue
		}
		pg, err := zfGenGroup(gs[0])
		if err != nil {
			t.Fatalf("%s: %v", p.desig, err)
		}
		if err := zfCheckPassiveOpposed(pg); err != nil {
			t.Errorf("%s 违反同件两旗异向:%v", p.desig, err)
		}
		if err := zfCheckTermOverlap(pg); err != nil {
			t.Errorf("%s 端子几何重叠:%v", p.desig, err)
		}
	}
	// 判据本身必须有判别力:构造一件两脚都从下缘探出的电容,规划必须 fail-closed。
	bad := zfGroup{Designator: "C99", BodyW: 17, BodyH: 21, Terms: []zfTerm{
		{Kind: "netflag", Net: "+3V3", Side: "down", PinX: 8.5, PinY: -9.5, HasPin: true},
		{Kind: "netflag", Net: "GND", Side: "down", PinX: 8.5, PinY: -9.5, HasPin: true},
	}}
	if _, err := zfGenPassive(bad); err == nil {
		t.Fatal("两支旗同向却放行了 —— 自短路防线是摆设")
	} else if !strings.Contains(err.Error(), "同件两旗异向") {
		t.Errorf("报错该点名硬不变式:%v", err)
	}
	// 两支 **netport** 同朝右是既有的正常形态(R4 阅读方向),不许误判成短路。
	ok := zfGroup{Designator: "R99", BodyW: 9, BodyH: 21, Terms: []zfTerm{
		{Kind: "netport", Net: "A", Side: "down", PinX: 4.5, PinY: -9.5, HasPin: true},
		{Kind: "netport", Net: "B", Side: "up", PinX: 4.5, PinY: 30.5, HasPin: true},
	}}
	if _, err := zfGenPassive(ok); err != nil {
		t.Fatalf("两支 netport 被误判:%v", err)
	}
}

// ── 正对照:整页计划覆盖每一只会被重建的 pin,落地框不胖于规划框 ───────────────

// zfPinsPlanPage 把 fixture 场景过一遍 zone-arrange 的**纯规划核心**。
func zfPinsPlanPage(t *testing.T, comps []layoutComp, wires []schGroupWire, opts partitionOpts) *zoneArrangeOut {
	t.Helper()
	out, err := planZoneArrangeScene(zfMCUIOZoneClaims(), comps, wires, map[string]zoneNoteSize{}, opts)
	if err != nil {
		t.Fatalf("planZoneArrangeScene: %v", err)
	}
	return out
}

// zfPinsUncovered 复刻内核算「自由落点」的那条算术:活体网表逐 pin 的重建规格
// (groupRebuildConnSpecs)减去计划端子覆盖到的 pin。空 = 报文里那句
// 「N 只 pin 走了 autoconnect 自由落点(计划未覆盖:…)」不会出现。
func zfPinsUncovered(comps []layoutComp, live map[string]map[string]bool,
	execs []zaaMemberExec, sweepSet map[string]bool) []string {

	covered := map[string]bool{}
	for _, m := range execs {
		for _, tm := range m.Terms {
			covered[strings.ToUpper(m.Desig+":"+tm.Pin)] = true
		}
	}
	conns, _ := groupRebuildConnSpecs(comps, sweepSet, live)
	var out []string
	for _, c := range conns {
		if !covered[strings.ToUpper(c.PinRef)] {
			out = append(out, c.PinRef)
		}
	}
	sort.Strings(out)
	return out
}

// zfPinsLandedZones 按**执行指令 + 真实引脚**复算落地几何,折成断言③ 的输入。
// 走的是落地那条链本身(zaaRetainEnvelope → zfTermGeomCanon → predictedMarkerBBox),
// 与规划器是两条独立代码路径 —— 规划器的引脚模型一旦不对,两者当场分家。
func zfPinsLandedZones(out *zoneArrangeOut, comps []layoutComp, execs []zaaMemberExec,
	opts partitionOpts, twist func(desig string, t *zaaTermExec)) []zaaLandedZone {

	live := map[string]layoutComp{}
	for _, c := range comps {
		if c.ComponentType == "part" {
			live[strings.ToUpper(label(c))] = c
		}
	}
	byDesig := map[string]zaaMemberExec{}
	for _, m := range execs {
		byDesig[strings.ToUpper(m.Desig)] = m
	}
	var zones []zaaLandedZone
	for _, z := range out.Zones {
		lz := zaaLandedZone{Name: z.Name, PlanW: z.FrameW, PlanH: z.FrameH}
		has := false
		for _, g := range z.Groups {
			d := strings.ToUpper(g.Designator)
			m, ok := byDesig[d]
			lc := live[d]
			if !ok || lc.BBox == nil {
				lz.Missing = append(lz.Missing, g.Designator)
				continue
			}
			snaps := make([]zaaPinSnap, 0, len(m.Terms))
			for _, tm := range m.Terms {
				tm := tm
				if twist != nil {
					twist(m.Desig, &tm)
				}
				px, py, okp := tidyPinCoord(lc.Pins, tm.Pin)
				if !okp {
					continue
				}
				snaps = append(snaps, zaaPinSnap{Pin: tm.Pin, Net: tm.Net, Kind: tm.Kind,
					Dir: tm.Dir, Offset: tm.Offset, PinX: px, PinY: py})
			}
			zfGrow(&lz.Content, &has, zaaRetainEnvelope(*lc.BBox, snaps, m.DX, m.DY))
		}
		if !has {
			lz.Missing = append(lz.Missing, "(全区无实测几何)")
			zones = append(zones, lz)
			continue
		}
		lz.Rect = partitionFrameRect(lz.Content, opts.TitleBand, zaaZoneNoteBand(z, opts.TitleBand))
		lz.FrameW, lz.FrameH = lz.Rect.MaxX-lz.Rect.MinX, lz.Rect.MaxY-lz.Rect.MinY
		zones = append(zones, lz)
	}
	return zones
}

func TestZoneArrange_MCUIOPlanCoversEveryRebuiltPin(t *testing.T) {
	opts := defaultPartitionOpts()
	comps, wires, live := zfPinsScene(true) // 带共树 GND —— 覆盖面最难的那一版
	out := zfPinsPlanPage(t, comps, wires, opts)
	if out.Verdict != "pass" {
		t.Fatalf("规划该 pass,得到 %s(%s)", out.Verdict, out.Arrange.Tried)
	}
	execs, sweepSet, err := zaaBuildExec(out, &zaScene{comps: comps, wires: wires}, opts)
	if err != nil {
		t.Fatalf("zaaBuildExec(断言①):%v", err)
	}
	if un := zfPinsUncovered(comps, live, execs, sweepSet); len(un) > 0 {
		t.Fatalf("计划未覆盖 %d 只 pin:%v —— 它们落地走 autoconnect 自由落点,区框必然凭空胖一档",
			len(un), un)
	}
	// 落地复判(判据本体,未改一行):落地框不胖于规划框 + 区框零重叠。
	if findings := zaaRecheckFindings(zfPinsLandedZones(out, comps, execs, opts, nil), opts.Gutter); len(findings) > 0 {
		t.Fatalf("断言③ 仍红:%s", strings.Join(findings, ";"))
	}
	for _, z := range out.Zones {
		t.Logf("  %-26s 规划框 %.0f×%.0f | %s", z.Name, z.FrameW, z.FrameH, z.Mode)
	}
}

// ── 负对照 A:判据不许被改松 ────────────────────────────────────────────────
//
// 计划覆盖齐全,但**落地方向与计划不符**(人为把每支 GND 旗掰到另一侧)——
// 断言③ 必须照样红。这一条防的是「把容差调大 / 把未覆盖端子排除出复判」那种
// 假修复:覆盖面修好了,判据的门槛一个单位都不许动。
func TestZoneArrange_RecheckStillRedWhenLandingDivergesFromPlan(t *testing.T) {
	opts := defaultPartitionOpts()
	comps, wires, live := zfPinsScene(true)
	out := zfPinsPlanPage(t, comps, wires, opts)
	execs, sweepSet, err := zaaBuildExec(out, &zaScene{comps: comps, wires: wires}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if un := zfPinsUncovered(comps, live, execs, sweepSet); len(un) > 0 {
		t.Fatalf("前提没成立:仍有 %d 只 pin 没被计划覆盖 %v", len(un), un)
	}
	// 落地时 GND 旗被评分器挑到了左边、桩长翻到 autoconnect 的下一档。
	twist := func(desig string, tm *zaaTermExec) {
		if tm.Net == "GND" {
			tm.Dir, tm.Offset = "left", 89
			if rot, err := tidyLabelRotation(tm.Kind, tm.Dir); err == nil {
				tm.LabelRot = rot
			}
		}
	}
	findings := zaaRecheckFindings(zfPinsLandedZones(out, comps, execs, opts, twist), opts.Gutter)
	if len(findings) == 0 {
		t.Fatal("负对照失效:落地方向与计划完全不符,断言③ 却绿了 —— 判据被改松了")
	}
	t.Logf("负对照 A 如期报红:%s", strings.Join(findings, ";"))
}
