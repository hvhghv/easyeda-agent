package app

import (
	"fmt"
	"strings"
	"testing"
)

// sch_zone_follow_opposed_test.go — 「同件两旗异向」硬不变式的**收窄**对照组
// (2026-08-20 真机回归)。
//
// ── 缺陷 ─────────────────────────────────────────────────────────────────────
//
// 首版 zfCheckPassiveOpposed 拿「方向相等」当违规,把一整类物理上只能同向的合法
// 器件挡死了。真机 ceshi / 页 POWER,`sch zone-arrange --doc POWER --apply` 当场拒
// 绝执行:
//
//	phase A(POWER_IN(J2)): J2: 两支旗同向(+5V:left / GND)…
//
// J2 = conn.screw_terminal_2p(KF301-5.0-2P),实测几何:
//
//	J2 bbox: minX=59.5 maxX=80.5  minY=664.5 maxY=695.5   (x,y)=(70,680) rot=0
//	  pin(+5V) at (50, 685)
//	  pin(GND) at (50, 675)
//
// 两只脚**都在 x=50**(本体左缘外侧)、纵向差 10 —— 只能都朝 left 出桩,「异向」在
// 这个符号上物理不可能;而两根朝 left 的桩线一根躺在 y=685、一根躺在 y=675,
// **平行不共线,永远不会合并**。佐证:收窄之前这个端子在真机上一直是好的
// (`sch bridge-check` 报 0 real short,`sch nets` 里 +5V 与 GND 各自独立)。
//
// ── 收窄后的判据 ─────────────────────────────────────────────────────────────
//
// 真正的短路条件是**两根桩线共线**(共线 → 相接 → 平台自动合并成一根 → 并网)。
// 桩只能从 pin 沿 direction 直出、垂直坐标原样留在 pin 上(endpointFor),所以:
//
//	left/right  桩躺在 y = PinY  → 两脚 y 相等才共线
//	up/down     桩站在 x = PinX  → 两脚 x 相等才共线
//
// 四组对照(收窄 ≠ 取消):
//
//	正对照     J2 真实几何 → 放行,且 phase A 正常产出 POWER_IN 这个区
//	负对照 A   两脚 y 相同、都朝 left → 照样违规,报文说得出「共线」
//	负对照 B   两脚 x 相同、都朝 down/up → 违规;两脚 x 不同、都朝 up → 放行
//	判别力     TestZfGenPassive_TwoFlagsNeverSameDirection 的两条老断言仍成立

// zfJ2Part 是真机 J2(KF301-5.0-2P)的实测几何,逐字来自 2026-08-20 页 POWER。
func zfJ2Part() zfPinsPart {
	return zfPinsPart{"J2", layoutBBox{MinX: 59.5, MinY: 664.5, MaxX: 80.5, MaxY: 695.5},
		[]zfPinsPin{{"1", 50, 685, "+5V"}, {"2", 50, 675, "GND"}}}
}

// zfOpposedGroup 把一件实测折成 phase A 的输入(与 zfGroupFromCluster 同一口径:
// 引脚坐标折成本体局部坐标,挂侧走 zfPointSideOf)。
func zfOpposedGroup(p zfPinsPart) zfGroup {
	g := zfGroup{Designator: p.desig,
		BodyW: p.body.MaxX - p.body.MinX, BodyH: p.body.MaxY - p.body.MinY}
	for _, pin := range p.pins {
		g.Terms = append(g.Terms, zfTerm{Kind: zfPinsKindFor(pin.net), Net: pin.net,
			Side:   zfPointSideOf(p.body, pin.x, pin.y),
			PinX:   pin.x - p.body.MinX,
			PinY:   pin.y - p.body.MinY,
			HasPin: true})
	}
	return g
}

// zfOpposedScene 把若干实测件折成一份页面场景快照(图框 + 本体 + 每只脚一支
// 「桩线 + marker」)—— 与 zfPinsScene 同一造法,只是不带共树分支。
func zfOpposedScene(parts []zfPinsPart) ([]layoutComp, []schGroupWire) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	comps := []layoutComp{{ID: "sheet", ComponentType: "sheet", BBox: &sheet}}
	var wires []schGroupWire
	for _, p := range parts {
		bb := p.body
		rot := 0.0
		part := layoutComp{ID: "pid-" + p.desig, Designator: p.desig, ComponentType: "part",
			X: (bb.MinX + bb.MaxX) / 2, Y: (bb.MinY + bb.MaxY) / 2,
			AnchorAvailable: true, Rotation: &rot, BBox: &bb, PinsAvailable: true}
		for _, pin := range p.pins {
			part.Pins = append(part.Pins, layoutPin{Number: pin.num, X: pin.x, Y: pin.y})
			dir := zfPointSideOf(p.body, pin.x, pin.y)
			kind := zfPinsKindFor(pin.net)
			if kind == "netport" && dir != "left" && dir != "right" {
				dir = "right"
			}
			ex, ey := endpointFor(pin.x, pin.y, zfStub, dir)
			canon := zfCanonKind(kind, pin.net)
			mb := predictedMarkerBody(ex, ey, canon, dir, pin.net)
			lrot, err := tidyLabelRotation(canon, dir)
			if err != nil {
				panic(err)
			}
			r := lrot
			comps = append(comps, layoutComp{ID: fmt.Sprintf("m-%s-%s", p.desig, pin.num),
				ComponentType: kind, Net: pin.net, X: ex, Y: ey,
				AnchorAvailable: true, BBox: &mb, Rotation: &r})
			wires = append(wires, schGroupWire{ID: fmt.Sprintf("w-%s-%s", p.desig, pin.num),
				Points: []float64{pin.x, pin.y, ex, ey}})
		}
		comps = append(comps, part)
	}
	return comps, wires
}

// ── 正对照:J2 的真实几何必须放行,phase A 必须产出 POWER_IN 这个区 ─────────────

func TestZfCheckPassiveOpposed_J2SameSideParallelStubsAreLegal(t *testing.T) {
	in := zfOpposedGroup(zfJ2Part())
	pg, err := zfGenPassive(in)
	if err != nil {
		t.Fatalf("J2 的真实几何被判红 —— 两脚同在左缘、y 差 10,桩线平行不共线,这是合法拓扑:%v", err)
	}
	if len(pg.Terms) != 2 {
		t.Fatalf("该折出 2 支端子,得到 %d", len(pg.Terms))
	}
	for _, tm := range pg.Terms {
		if tm.Dir != "left" {
			t.Errorf("J2 %s 的桩该朝 left(两脚都在本体左缘外侧),得到 %q", tm.Net, tm.Dir)
		}
	}
	// 「同向」这件事必须真的发生过 —— 否则正对照什么也没证明(判据根本没被触发)。
	if pg.Terms[0].Dir != pg.Terms[1].Dir {
		t.Fatal("正对照失效:两支旗并不同向,收窄后的判据在这条用例上从未被求值")
	}
	// 而且必须真的是**平行不共线**(判据放行的理由,不是别的理由)。
	if pg.Terms[0].PinY == pg.Terms[1].PinY {
		t.Fatal("fixture 走样:两脚 y 相同就该是负对照 A 了")
	}
	if err := zfCheckPassiveOpposed(pg); err != nil {
		t.Errorf("zfCheckPassiveOpposed 直接判也该放行:%v", err)
	}
	if err := zfCheckTermOverlap(pg); err != nil {
		t.Errorf("J2 两支端子不该重叠:%v", err)
	}

	// phase A 整条链:POWER_IN 这个区要能正常产出。
	comps, wires := zfOpposedScene([]zfPinsPart{zfJ2Part()})
	opts := defaultPartitionOpts()
	out, err := planZoneArrangeScene(map[string]*schZoneClaim{"POWER_IN(J2)": {Parts: []string{"J2"}}},
		comps, wires, map[string]zoneNoteSize{}, opts)
	if err != nil {
		t.Fatalf("phase A 该产出 POWER_IN,却整条拒绝执行:%v", err)
	}
	var zone *zoneArrangeZoneOut
	for i := range out.Zones {
		if out.Zones[i].Name == "POWER_IN(J2)" {
			zone = &out.Zones[i]
		}
	}
	if zone == nil {
		t.Fatalf("输出里没有 POWER_IN(J2) 区:%+v", out.Zones)
	}
	if len(zone.Groups) != 1 || zone.Groups[0].Designator != "J2" {
		t.Fatalf("POWER_IN 区该只含 J2,得到 %+v", zone.Groups)
	}
	if n := len(zone.Groups[0].Terms); n != 2 {
		t.Fatalf("J2 该带 2 支端子(+5V / GND),得到 %d", n)
	}
	t.Logf("POWER_IN(J2) 框 %.0f×%.0f | %s", zone.FrameW, zone.FrameH, zone.Mode)
}

// ── 负对照 A:真共线(水平桩)仍要 fail-closed ─────────────────────────────────
//
// 两脚 y 相同、都从左缘探出 → 两根桩躺在同一条横线上 → 平台合并 → 并网。
// 这一条证明收窄不是「把判据删了」。
func TestZfCheckPassiveOpposed_CollinearHorizontalStubsStillFail(t *testing.T) {
	bad := zfOpposedGroup(zfPinsPart{"J99", layoutBBox{MinX: 59.5, MinY: 664.5, MaxX: 80.5, MaxY: 695.5},
		[]zfPinsPin{{"1", 50, 680, "+5V"}, {"2", 50, 680, "GND"}}})
	_, err := zfGenPassive(bad)
	if err == nil {
		t.Fatal("两脚 y 相同、都朝 left —— 两根桩共线必被平台合并,却放行了:自短路防线是摆设")
	}
	for _, want := range []string{"共线", "同件两旗异向", "y 相差 0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报文该点名 %q(真实违规原因 + 硬不变式 + 是哪条轴):%v", want, err)
		}
	}
	// 报文必须给能执行的下一步(本仓铁律)。
	if !strings.Contains(err.Error(), "--include-pins") {
		t.Errorf("报文该给出核对引脚几何的那条命令:%v", err)
	}
	t.Logf("负对照 A 如期报红:%v", err)
}

// ── 负对照 B:竖直轴同理 ──────────────────────────────────────────────────────
//
// 两脚 x 相同、都从下缘(或上缘)探出 → 两根桩站在同一条竖线上 → 违规;
// 两脚 x 不同、都朝上 → 平行不共线 → 放行。
func TestZfCheckPassiveOpposed_VerticalAxisSameRule(t *testing.T) {
	// 本体必须已竖立(bw < bh),否则 R1 会把它转竖、走的就不是实测引脚那条支路了。
	body := layoutBBox{MinX: 100, MinY: 100, MaxX: 121, MaxY: 140}
	// 违规:同 x、都朝 down(两脚都在本体下缘外侧)。
	badDown := zfOpposedGroup(zfPinsPart{"C98", body,
		[]zfPinsPin{{"1", 110.5, 90, "+3V3"}, {"2", 110.5, 90, "GND"}}})
	err := zfMustFailOpposed(t, badDown, "同 x 都朝 down")
	if !strings.Contains(err.Error(), "x 相差 0.0") {
		t.Errorf("竖直桩该报 x 轴共线(而不是 y 轴):%v", err)
	}
	// 违规:同 x、都朝 up。
	badUp := zfOpposedGroup(zfPinsPart{"C97", body,
		[]zfPinsPin{{"1", 110.5, 150, "+3V3"}, {"2", 110.5, 150, "GND"}}})
	zfMustFailOpposed(t, badUp, "同 x 都朝 up")

	// 放行:两脚 x 不同、都在本体上缘外侧 → 都朝 up,但桩线平行。
	ok := zfOpposedGroup(zfPinsPart{"C96", body,
		[]zfPinsPin{{"1", 105, 150, "+3V3"}, {"2", 116, 150, "GND"}}})
	pg, err2 := zfGenPassive(ok)
	if err2 != nil {
		t.Fatalf("两脚 x 差 11、都朝 up —— 桩线平行不共线,该放行:%v", err2)
	}
	if pg.Terms[0].Dir != "up" || pg.Terms[1].Dir != "up" {
		t.Fatalf("正对照失效:两支旗并不同向(%s / %s),判据没被求值",
			pg.Terms[0].Dir, pg.Terms[1].Dir)
	}
}

func zfMustFailOpposed(t *testing.T, g zfGroup, what string) error {
	t.Helper()
	_, err := zfGenPassive(g)
	if err == nil {
		t.Fatalf("%s(%s):两根桩共线必被合并,却放行了", g.Designator, what)
	}
	if !strings.Contains(err.Error(), "共线") || !strings.Contains(err.Error(), "同件两旗异向") {
		t.Errorf("%s 的报文该点名共线 + 硬不变式:%v", g.Designator, err)
	}
	return err
}
