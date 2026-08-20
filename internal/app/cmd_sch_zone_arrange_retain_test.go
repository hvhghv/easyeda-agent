package app

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// cmd_sch_zone_arrange_retain_test.go — 「↩ 原形保留」必须兑现 + 落地复判的两条
// 新判据(出图纸 / retain 刚体)。
//
// 缺陷形态(真机 ceshi / 页 MCU_IO,2026-08-20):`zone-arrange --apply` 电气全绿,
// 断言③却逐区红,其中**最刺眼的一条**是标着「↩ 原形保留(不重排、不重生桩)」的
// esp32s3_wroom1_module:落地前 L1 组 391×421,落地后 391×562 —— 宽度分毫不差
// (横向几何复现是准的),高度凭空多了 141。
//
// 「不动的东西真的没动」不依赖任何预测模型,是本命令最强的可验证不变式,所以它
// 该在**执行前**用算术判掉(zaaGateRetainRigid),而不是事后靠 `sch clusters` 发现。

// ── fixture:把「器件 + 每 pin 一支 marker + 一根桩线」造成一份真实场景快照 ────
//
// 走的是生产路径本身:marker 的 bbox 由 predictedMarkerBody 生成、rotation 由
// tidyLabelRotation 生成,于是 buildSchClusters(经 markerJudgeBBox → flagTextBand)
// 量出来的 L1 体积与 zaaRetainEnvelope(经 zfTermGeomCanon → predictedMarkerBBox)
// 预测出来的包络是**两条独立代码路径**,可以互相验证。

type zaaTestPin struct {
	pin, net, kind, dir string  // kind:netflag | netport
	pinX, pinY, offset  float64 // pin 坐标 + 桩长(方向由 dir 给)
}

// zaaTestScene 造一件带 marker 的器件,返回场景 comps/wires。
func zaaTestScene(desig string, body layoutBBox, anchorX, anchorY float64, pins []zaaTestPin) ([]layoutComp, []schGroupWire) {
	bb := body
	rot := 0.0
	part := layoutComp{ID: "pid-" + desig, Designator: desig, ComponentType: "part",
		X: anchorX, Y: anchorY, AnchorAvailable: true, Rotation: &rot, BBox: &bb, PinsAvailable: true}
	comps := []layoutComp{}
	var wires []schGroupWire
	for i, p := range pins {
		part.Pins = append(part.Pins, layoutPin{Number: p.pin, X: p.pinX, Y: p.pinY})
		canon := zfCanonKind(p.kind, p.net)
		ex, ey := endpointFor(p.pinX, p.pinY, p.offset, p.dir)
		mb := predictedMarkerBody(ex, ey, canon, p.dir, p.net)
		mrot, err := tidyLabelRotation(canon, p.dir)
		if err != nil {
			panic(err)
		}
		mr := mrot
		comps = append(comps, layoutComp{ID: fmt.Sprintf("m%d-%s", i, desig), ComponentType: p.kind,
			Net: p.net, X: ex, Y: ey, AnchorAvailable: true, BBox: &mb, Rotation: &mr})
		wires = append(wires, schGroupWire{ID: fmt.Sprintf("w%d-%s", i, desig),
			Points: []float64{p.pinX, p.pinY, ex, ey}})
	}
	return append([]layoutComp{part}, comps...), wires
}

// zaaTestGroups 走 computeZoneArrange 的那条折算链:场景 → clusters → zfGroup
// (含 Measured)。**不许在测试里手搓 zfGroup** —— 手搓等于绕过挂侧判定与实测
// 桩长这两处生产逻辑,测出来的东西与真机无关。
func zaaTestGroups(comps []layoutComp, wires []schGroupWire) ([]zfGroup, map[string]schCluster) {
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
	for _, c := range clusters {
		d := strings.ToUpper(c.Designator)
		g := zfGroupFromCluster(c, pinCount[d])
		g.Measured = zfMeasureCluster(c, partOf[d], wires, roots, markers)
		out = append(out, g)
	}
	return out, byDesig
}

// zaaTestESP32Scene 是真机那一区的场景形态:一件 41 脚大符号(本体 71×421),
// 8 支标签横向铺开(桩长 39~84,与真机同量级)。
func zaaTestESP32Scene() ([]layoutComp, []schGroupWire) {
	return zaaTestScene("U2", layoutBBox{MinX: 500, MinY: 200, MaxX: 571, MaxY: 621}, 535, 410,
		[]zaaTestPin{
			{"1", "3V3", "netflag", "left", 500, 240, 40},
			{"2", "GND", "netflag", "left", 500, 220, 40},
			{"3", "EN", "netport", "right", 571, 600, 39},
			{"4", "IO0", "netport", "right", 571, 230, 84},
			{"5", "TXD0", "netport", "left", 500, 440, 75},
			{"6", "RXD0", "netport", "left", 500, 420, 75},
			{"7", "USB_DP", "netport", "left", 500, 560, 75},
			{"8", "USB_DM", "netport", "left", 500, 540, 75},
		})
}

// zaaTestOut 把一份 phase A 计划包成 --apply 吃的 zoneArrangeOut(落位框直接给,
// 不跑 phase B —— 本测试判的是「计划 → 执行指令」这一段)。
func zaaTestOut(sheet layoutBBox, zone string, plan zfZonePlan, rect layoutBBox) *zoneArrangeOut {
	return &zoneArrangeOut{
		Sheet: sheet,
		Zones: []zoneArrangeZoneOut{{
			Name: zone, Mode: plan.Mode, FrameW: plan.FrameW, FrameH: plan.FrameH,
			Groups: plan.Groups, Content: plan.Content,
			Retained: plan.Retained, RetainWhy: plan.RetainWhy,
		}},
		Arrange: zaResult{OK: true, Placed: []zaPlaced{{Name: zone, Rect: rect}}},
		Verdict: "pass",
	}
}

// ── ① retain 不变式:落地前后 L1 组几何逐字相同(仅差刚体平移)────────────────

func TestZaaRetain_ExecutionIsARigidTranslation(t *testing.T) {
	opts := defaultPartitionOpts()
	comps, wires := zaaTestESP32Scene()
	groups, byDesig := zaaTestGroups(comps, wires)
	sheet, keepout, dom := zfGateA4Domain(opts)
	_ = keepout

	plan, err := planZoneFollowGated("esp32s3_wroom1_module", groups, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Retained {
		t.Fatalf("fixture 失效:这一区该走 retain 路径(得到 %s)", plan.Mode)
	}

	rect := layoutBBox{MinX: 40, MinY: 60, MaxX: 40 + plan.FrameW, MaxY: 60 + plan.FrameH}
	out := zaaTestOut(sheet, "esp32s3_wroom1_module", plan, rect)
	execs, _, err := zaaBuildExec(out, &zaScene{comps: comps, wires: wires}, opts)
	if err != nil {
		t.Fatalf("retain 计划必须能构造执行指令(断言①含刚体门):%v", err)
	}
	if len(execs) != 1 {
		t.Fatalf("该有 1 件执行指令,得到 %d", len(execs))
	}
	me := execs[0]

	// (a) 逐 pin:执行指令 == 移动前实测几何。刚体平移的定义就是这一条。
	if err := zaaGateRetainRigid(me.Desig, me.Snaps, me.Terms); err != nil {
		t.Fatalf("retain 区的执行指令与移动前几何不符:%v", err)
	}
	if me.Rotate {
		t.Error("原形保留不许转姿态")
	}

	// (b) 包络:落地后应有的 L1 体积 == 实测 L1 体积**平移后**,逐字相等。
	if me.RetainBox == nil {
		t.Fatal("retain 成员必须带 RetainBox(落地复判要拿它跟真机量出来的 box 比)")
	}
	measured := byDesig["U2"].Box
	wantW, wantH := measured.MaxX-measured.MinX, measured.MaxY-measured.MinY
	gotW, gotH := me.RetainBox.MaxX-me.RetainBox.MinX, me.RetainBox.MaxY-me.RetainBox.MinY
	if math.Abs(gotW-wantW) > 1e-6 || math.Abs(gotH-wantH) > 1e-6 {
		t.Errorf("原形包络尺寸变了:实测 %.0f×%.0f → 落地应有 %.0f×%.0f(真机那一笔是 391×421 → 391×562)",
			wantW, wantH, gotW, gotH)
	}
	// 位移逐边一致(刚体):四条边的位移量必须是同一个 (DX,DY)。
	for _, d := range [][3]float64{
		{me.RetainBox.MinX - measured.MinX, me.DX, 0}, {me.RetainBox.MaxX - measured.MaxX, me.DX, 0},
		{me.RetainBox.MinY - measured.MinY, me.DY, 0}, {me.RetainBox.MaxY - measured.MaxY, me.DY, 0},
	} {
		if math.Abs(d[0]-d[1]) > 1e-6 {
			t.Errorf("不是刚体平移:某条边位移 %.1f ≠ Δ %.1f", d[0], d[1])
		}
	}
	t.Logf("retain 落地:%.0f×%.0f 原样平移 (%+.0f,%+.0f)", gotW, gotH, me.DX, me.DY)

	// ── 负对照:同一份场景走**收敛**路径(不 retain),刚体断言必须失败 ──────
	// 没有这一条,上面的断言只是在自证(收敛本来就要改几何,改不动才是缺陷)。
	conv, err := planZoneFollow("esp32s3_wroom1_module", groups, opts)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Retained {
		t.Fatal("负对照失效:planZoneFollow 不该标 retained")
	}
	convRect := layoutBBox{MinX: 40, MinY: 60, MaxX: 40 + conv.FrameW, MaxY: 60 + conv.FrameH}
	convOut := zaaTestOut(sheet, "esp32s3_wroom1_module", conv, convRect)
	convExecs, _, err := zaaBuildExec(convOut, &zaScene{comps: comps, wires: wires}, opts)
	if err != nil {
		t.Fatalf("收敛计划该能构造执行指令:%v", err)
	}
	if err := zaaGateRetainRigid(convExecs[0].Desig, convExecs[0].Snaps, convExecs[0].Terms); err == nil {
		t.Fatal("负对照失效:收敛计划把桩线全重生了,刚体断言却判它没动 —— 这条断言钉不住任何东西")
	} else {
		t.Logf("负对照(收敛计划)按预期判红:%v", err)
	}
	if convExecs[0].RetainBox != nil {
		t.Error("非 retain 区不该带 RetainBox")
	}
}

// retain 区的执行指令一旦与原形不符,必须在**执行前**拒绝(画布零改动),
// 而不是先改画布再靠事后复判说「哎呀胖了」。
func TestZaaBuildExec_RetainMismatchFailsClosed(t *testing.T) {
	opts := defaultPartitionOpts()
	comps, wires := zaaTestESP32Scene()
	groups, _ := zaaTestGroups(comps, wires)
	sheet, _, dom := zfGateA4Domain(opts)
	plan, err := planZoneFollowGated("esp32s3_wroom1_module", groups, opts, dom)
	if err != nil || !plan.Retained {
		t.Fatalf("fixture 失效:%v retained=%v", err, plan.Retained)
	}
	// 把原形计划里 IO0 那支端子的桩长改短(模拟「合成端子退回默认短桩」这类
	// 静默改几何的缺陷)——门必须拦住整页。
	touched := false
	for gi := range plan.Groups {
		for ti := range plan.Groups[gi].Terms {
			if plan.Groups[gi].Terms[ti].Net == "IO0" {
				plan.Groups[gi].Terms[ti].Offset = zfStub
				touched = true
			}
		}
	}
	if !touched {
		t.Fatal("fixture 失效:没找到 IO0 端子")
	}
	rect := layoutBBox{MinX: 40, MinY: 60, MaxX: 40 + plan.FrameW, MaxY: 60 + plan.FrameH}
	out := zaaTestOut(sheet, "esp32s3_wroom1_module", plan, rect)
	_, _, err = zaaBuildExec(out, &zaScene{comps: comps, wires: wires}, opts)
	if err == nil {
		t.Fatal("retain 计划被改了桩长却放行 —— 「原形保留」成了空话")
	}
	for _, want := range []string{"retain 刚体不变式", "画布零改动", "桩长"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报文里缺 %q:%v", want, err)
		}
	}
}

// 落位偏移必须用**本区**的说明带高(从规划框反推),不是全局默认带:
// 规划给登记过说明的区留了更高的带,执行却按默认 42 落位 → 内容整体下沉进说明带,
// 框跟着往下长,严重时直接探出图纸下沿。复判侧早就用反推带高了,执行侧再用一次
// 全局常量就是两把尺。
func TestZaaBuildExec_UsesPerZoneNoteBand(t *testing.T) {
	opts := defaultPartitionOpts()
	comps, wires := zaaTestESP32Scene()
	groups, _ := zaaTestGroups(comps, wires)
	sheet, _, dom := zfGateA4Domain(opts)
	plan, err := planZoneFollowGated("esp32s3_wroom1_module", groups, opts, dom)
	if err != nil {
		t.Fatal(err)
	}
	rect := layoutBBox{MinX: 40, MinY: 60, MaxX: 40 + plan.FrameW, MaxY: 60 + plan.FrameH}
	base := zaaTestOut(sheet, "esp32s3_wroom1_module", plan, rect)
	baseExec, _, err := zaaBuildExec(base, &zaScene{comps: comps, wires: wires}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := zaaZoneNoteBand(base.Zones[0], opts.TitleBand); got != opts.NoteBand {
		t.Fatalf("fixture 该是默认说明带 %g,反推得到 %g", opts.NoteBand, got)
	}
	// 同一份计划,但这一区登记了更高的说明(框高多 20 = 带高多 20)。
	tall := zaaTestOut(sheet, "esp32s3_wroom1_module", plan, rect)
	tall.Zones[0].FrameH += 20
	tall.Arrange.Placed[0].Rect.MaxY += 20
	tallExec, _, err := zaaBuildExec(tall, &zaScene{comps: comps, wires: wires}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if d := tallExec[0].DY - baseExec[0].DY; d != 20 {
		t.Errorf("说明带高 +20 时内容该整体上抬 20(避开更高的带),ΔDY 得到 %g —— 执行侧还在用全局默认带", d)
	}
}

// ── ② 合成端子必须继承实测桩长(方向与长度是同一次观测的两半)──────────────
//
// 旧代码:`s.Dir, _ = tidyStubDirection(...)` 丢掉了长度,合成端子一律 zfStub。
// 于是「共树 pin」在**原形保留**区里被换成 20 的短桩 —— 计划说一个单位都不动。
func TestZaaPadTermsToPins_SynthesizedTermKeepsMeasuredStub(t *testing.T) {
	terms := []zfPlacedTerm{{Net: "GND", Dir: "left", Offset: 40}}
	pre := []zaaPinSnap{
		{Pin: "1", Net: "GND", Dir: "left", Offset: 40, Kind: "ground"},
		{Pin: "2", Net: "USB_DTR", Dir: "down", Offset: 62, Kind: "net_port_bi"},
	}
	got := zaaPadTermsToPins(terms, pre, map[string]bool{"GND": true, "USB_DTR": true})
	var syn *zfPlacedTerm
	for i := range got {
		if got[i].Net == "USB_DTR" {
			syn = &got[i]
		}
	}
	if syn == nil {
		t.Fatal("页内认领的共树网该被合成端子")
	}
	if syn.Dir != "down" {
		t.Errorf("合成端子方向该取实测 down,得到 %s", syn.Dir)
	}
	if syn.Offset != 62 {
		t.Errorf("合成端子桩长该取实测 62,得到 %g —— 只取方向不取长度就是在偷偷改几何", syn.Offset)
	}
	// 负对照:实测没有可复现的桩(Offset=0)时才退默认短桩。
	pre2 := []zaaPinSnap{pre[0], {Pin: "2", Net: "USB_DTR", Dir: "down"}}
	got2 := zaaPadTermsToPins(terms, pre2, map[string]bool{"GND": true, "USB_DTR": true})
	for _, tm := range got2 {
		if tm.Net == "USB_DTR" && tm.Offset != zfStub {
			t.Errorf("没有实测桩长时该退默认 %g,得到 %g", zfStub, tm.Offset)
		}
	}
}

// ── ③ 端子→pin 映射:优先按实测桩几何配对,不靠「现侧」 ──────────────────────
//
// 现侧(pin 相对本体中心的主轴)与桩方向在高瘦符号上系统性不等:真机 U2
// (71×421)上下两端行的 pin,|dy| > |dx| → 现侧判 up/down,而桩实际是 left/right。
// 只按现侧匹配时这些端子第一轮全落空,退到「只按 net」按 pin 号乱配 ——
// 同网多脚的桩几何当场互换,原形保留的区照样变形。
func TestZaaMapTerms_PrefersMeasuredStubGeometry(t *testing.T) {
	// 同一张 GND 网上的两只脚,桩几何不同(一只左 40、一只左 90)。
	pre := []zaaPinSnap{
		{Pin: "1", Net: "GND", Dir: "left", Offset: 40, Kind: "ground"},
		{Pin: "2", Net: "GND", Dir: "left", Offset: 90, Kind: "ground"},
	}
	// 现侧全判成 down(高瘦本体的实况)——它对这两只脚毫无区分力。
	side := map[string]string{"1": "down", "2": "down"}
	terms := []zfPlacedTerm{
		{Kind: "netflag", Net: "GND", Dir: "left", Offset: 90},
		{Kind: "netflag", Net: "GND", Dir: "left", Offset: 40},
	}
	out, err := zaaMapTerms(pre, terms, side, func(zfPlacedTerm) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Pin != "2" || out[1].Pin != "1" {
		t.Errorf("该按 (net, 方向, 桩长) 全等配对(90→pin2, 40→pin1),得到 %s/%s", out[0].Pin, out[1].Pin)
	}
	// 现侧仍要在「桩几何区分不了」时起作用(J1 双 U3_N4 左右各一的老场景)。
	pre2 := []zaaPinSnap{{Pin: "A5", Net: "U3_N4"}, {Pin: "B5", Net: "U3_N4"}}
	terms2 := []zfPlacedTerm{
		{Kind: "netport", Net: "U3_N4", Dir: "left", Offset: 20},
		{Kind: "netport", Net: "U3_N4", Dir: "right", Offset: 46},
	}
	out2, err := zaaMapTerms(pre2, terms2, map[string]string{"A5": "right", "B5": "left"},
		func(zfPlacedTerm) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if out2[0].Pin != "B5" || out2[1].Pin != "A5" {
		t.Errorf("桩几何缺席时该退回现侧配对,得到 %s/%s", out2[0].Pin, out2[1].Pin)
	}
}

// ── ④ 出图纸必须由 --apply 自己报出来 ───────────────────────────────────────
//
// 真机:`--apply` 打完绿勾,事后 `sch clusters --strict` 才报
// `C5 左沿 -34 < 12`、`U2 上沿 840 > 813`。判据用同一个常量(sheetEdgeMinGap),
// 不另立边距。
func TestZaaOutOfSheetWhy_MatchesClustersRuler(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	if why := zaaOutOfSheetWhy(layoutBBox{MinX: -34, MinY: 300, MaxX: 60, MaxY: 340}, sheet); why != "左沿 -34 < 12" {
		t.Errorf("C5 那条该逐字复现 `sch clusters` 的报文,得到 %q", why)
	}
	if why := zaaOutOfSheetWhy(layoutBBox{MinX: 400, MinY: 419, MaxX: 500, MaxY: 840}, sheet); why != "上沿 840 > 813" {
		t.Errorf("U2 那条该逐字复现,得到 %q", why)
	}
	if why := zaaOutOfSheetWhy(layoutBBox{MinX: 20, MinY: 20, MaxX: 1100, MaxY: 800}, sheet); why != "" {
		t.Errorf("可用区之内不该报:%q", why)
	}
	// 零尺寸「图纸」(读不到图框)不许把整页判成越界 —— 没有尺子就别量。
	if why := zaaOutOfSheetWhy(layoutBBox{MinX: 100, MinY: 100, MaxX: 200, MaxY: 200}, layoutBBox{}); why != "" {
		t.Errorf("没有图框时不该报越界:%q", why)
	}
}

func TestZaaRecheckFindings_ReportsOutOfSheetAndRetainDrift(t *testing.T) {
	zones := []zaaLandedZone{{
		Name: "esp32s3_wroom1_module", PlanW: 439, PlanH: 541, FrameW: 439, FrameH: 682,
		Rect:    layoutBBox{MinX: 0, MinY: 0, MaxX: 439, MaxY: 682},
		Rigid:   []string{"U2 实测 391×562 与原形平移后应有的 391×421 差 141(四边偏差 0/-141/0/0)"},
		Outside: []string{"U2 上沿 840 > 813"},
	}}
	got := zaaRecheckFindings(zones, 12)
	joined := strings.Join(got, ";")
	if !strings.Contains(joined, "原形保留") || !strings.Contains(joined, "没有兑现") {
		t.Errorf("retain 漂移必须单独成条并点名「原形保留没兑现」:%v", got)
	}
	if !strings.Contains(joined, "探出图纸可用区") || !strings.Contains(joined, "上沿 840 > 813") {
		t.Errorf("出图纸必须由 --apply 自己报出来:%v", got)
	}
	if !strings.Contains(joined, "落地框") {
		t.Errorf("尺寸偏差条目仍要在:%v", got)
	}
	// 三条判据互不吞并:retain 绿 + 出图纸绿 + 尺寸在 gutter 内 = 复判绿。
	clean := []zaaLandedZone{{Name: "U", PlanW: 300, PlanH: 300, FrameW: 295, FrameH: 300,
		Rect: layoutBBox{MinX: 0, MinY: 0, MaxX: 295, MaxY: 300}}}
	if got := zaaRecheckFindings(clean, 12); len(got) != 0 {
		t.Fatalf("干净场景该算绿:%v", got)
	}
}

// ── ⑤ 真机六区 fixture:断言③ 必须逐区报出来,且报文带得出归因数字 ────────────
//
// 这六组 (规划框, 实测框) 是 2026-08-20 ceshi/MCU_IO 的实测配对。**它们是观测,
// 不是纯函数的输出** —— 修复能改变的是「下一次落地会不会再这样」,不可能把这
// 六个已经量出来的数字变小。所以这条 fixture 钉的是判据本身:每一区都必须被报
// 出来(一个都不许漏),报文必须带得出偏差与 gutter,人才可能据此下一步。
//
// 各区偏差(实测 − 规划,gutter=12;负值 = 落地比规划瘦):
//
//	esp32s3_wroom1_module  +0 / +141   ← retain 区,最强的那条不变式被破坏
//	led_indicator_gpio     +125 / -2   ← 宽度多出约一支 netport 的整长
//	U_EN                   +82 / +24
//	tactile_boot_reset     -10 / +56
//	U_3V3                  -10 / +26
//	U_IO0                  -11 / +10   ← 唯一两维都在 gutter 内的区
func zaaMCUIOSixZones() []zaaLandedZone {
	mk := func(name string, pw, ph, fw, fh, x float64) zaaLandedZone {
		return zaaLandedZone{Name: name, PlanW: pw, PlanH: ph, FrameW: fw, FrameH: fh,
			Rect: layoutBBox{MinX: x, MinY: 0, MaxX: x + fw, MaxY: fh}}
	}
	// Rect 沿 x 摊开且互不相交 —— 本 fixture 判的是**尺寸偏差**那一条,
	// 不让区框重叠那条判据混进来。
	x := 0.0
	var out []zaaLandedZone
	for _, z := range [][5]float64{
		{439, 541, 439, 682, 0}, {180, 265, 305, 263, 0}, {175, 296, 257, 320, 0},
		{179, 346, 169, 402, 0}, {82, 378, 72, 404, 0}, {181, 198, 170, 208, 0},
	} {
		names := []string{"esp32s3_wroom1_module", "led_indicator_gpio", "U_EN",
			"tactile_boot_reset", "U_3V3", "U_IO0"}
		out = append(out, mk(names[len(out)], z[0], z[1], z[2], z[3], x))
		x += z[2] + 50
	}
	return out
}

func TestZaaRecheckFindings_MCUIORealMachineSixZones(t *testing.T) {
	const gutter = 12.0
	zones := zaaMCUIOSixZones()
	got := zaaRecheckFindings(zones, gutter)
	joined := strings.Join(got, "\n")

	// 逐区结论:偏差超 gutter 的必须被点名,没超的不许被点名(判据不能只会喊红)。
	for _, z := range zones {
		dw, dh := z.FrameW-z.PlanW, z.FrameH-z.PlanH
		over := dw > gutter || dh > gutter
		named := strings.Contains(joined, "区 "+z.Name+" 落地框")
		if over && !named {
			t.Errorf("区 %s 偏差 %+.0f/%+.0f 超 gutter %g 却没被报出来", z.Name, dw, dh, gutter)
		}
		if !over && named {
			t.Errorf("区 %s 偏差 %+.0f/%+.0f 在 gutter 内却被误报", z.Name, dw, dh)
		}
		t.Logf("  %-24s 规划 %3.0f×%3.0f → 实测 %3.0f×%3.0f  偏差 %+4.0f/%+4.0f  %s",
			z.Name, z.PlanW, z.PlanH, z.FrameW, z.FrameH, dw, dh,
			map[bool]string{true: "✗ 超 gutter", false: "✓ 在 gutter 内"}[over])
	}
	// U_IO0 是六区里唯一两维都在 gutter 内的 —— 它是这条 fixture 的负对照:
	// 判据要是退化成「一律报红」,这一区会被误伤,fixture 当场发现。
	if strings.Contains(joined, "区 U_IO0 落地框") {
		t.Errorf("U_IO0 偏差 -11/+10 都在 gutter 内,不该报:%v", got)
	}
	if len(got) != 5 {
		t.Errorf("六区里该有 5 区超 gutter,得到 %d 条:%v", len(got), got)
	}
	for _, want := range []string{"gutter 12", "超出", "规划框"} {
		if !strings.Contains(joined, want) {
			t.Errorf("报文缺归因要素 %q:%v", want, got)
		}
	}
}
