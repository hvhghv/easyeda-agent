package app

import (
	"strings"
	"testing"
)

// 这组单测是 J_USB 事故(2026-08-16)的直接回放:sweep 按 3 件删、重建只轮到
// 1 件,而 layout-lint + bridge-check 双绿 —— 两条断言必须在纯函数层就把这种
// 局面拦下来,不能等真机。

// 断言①名单形式 —— 事故重放:删除集 {J1,R3,R4},重建名单只有 [J1]。
func TestZaaGateSetEquality_JUSBIncidentReplay(t *testing.T) {
	sweep := map[string]bool{"J1": true, "R3": true, "R4": true}
	err := zaaGateSetEquality(sweep, []string{"J1"})
	if err == nil {
		t.Fatal("删除集 ≠ 重建集必须拒绝 —— 这正是 J_USB 断线的成因")
	}
	for _, want := range []string{"R3", "R4", "画布零改动"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报文里缺 %q:%v", want, err)
		}
	}
	if zaaGateSetEquality(sweep, []string{"J1", "R4", "R3"}) != nil {
		t.Error("集合相等(顺序无关)该放行")
	}
}

// 断言① pin 级覆盖:少一条端子 = 删了不重建;导线直连/netlabel 盖不住 → 拒绝。
func TestZaaGatePinCoverage(t *testing.T) {
	pre := []zaaPinSnap{
		{Desig: "R3", Pin: "1", Net: "U3_N6", Kind: "net_port_bi"},
		{Desig: "R3", Pin: "2", Net: "GND", Kind: "ground"},
	}
	full := []zfPlacedTerm{{Kind: "netport", Net: "U3_N6"}, {Kind: "netflag", Net: "GND"}}
	if err := zaaGatePinCoverage("R3", pre, full); err != nil {
		t.Fatalf("覆盖相等该放行:%v", err)
	}
	if err := zaaGatePinCoverage("R3", pre, full[:1]); err == nil {
		t.Fatal("计划少了 GND 端子必须拒绝(删了不重建 = 静默断线)")
	}
	wired := []zaaPinSnap{{Desig: "C8", Pin: "1", Wired: true}}
	if err := zaaGatePinCoverage("C8", wired, nil); err == nil ||
		!strings.Contains(err.Error(), "普通导线直连") {
		t.Fatalf("导线直连 pin 该拒绝并说明原因:%v", err)
	}
	nl := []zaaPinSnap{{Desig: "X1", Pin: "1", Net: "SIG", Kind: ""}}
	if err := zaaGatePinCoverage("X1", nl, []zfPlacedTerm{{Kind: "netflag", Net: "SIG"}}); err == nil {
		t.Fatal("netlabel 类不可重建的连接该拒绝")
	}
}

// 端子→pin 映射:J1 的双 U3_N4(左右各一)必须按现侧区分,不许交叉。
func TestZaaMapTerms_DoubleNetBySide(t *testing.T) {
	pre := []zaaPinSnap{
		{Desig: "J1", Pin: "A5", Net: "U3_N4", Kind: "net_port_bi"},
		{Desig: "J1", Pin: "B5", Net: "U3_N4", Kind: "net_port_bi"},
	}
	terms := []zfPlacedTerm{
		{Kind: "netport", Net: "U3_N4", Dir: "left", Offset: 20},
		{Kind: "netport", Net: "U3_N4", Dir: "right", Offset: 46},
	}
	side := map[string]string{"A5": "right", "B5": "left"}
	out, err := zaaMapTerms(pre, terms, side, func(zfPlacedTerm) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Pin != "B5" || out[1].Pin != "A5" {
		t.Errorf("该按现侧配对(left→B5, right→A5),得到 %s/%s", out[0].Pin, out[1].Pin)
	}
	// 计划桩长必须透传 —— 丢掉它,connect_pin 落默认桩长,多旗梯次白算(竖叠复发)。
	if out[0].Offset != 20 || out[1].Offset != 46 {
		t.Errorf("Offset 该透传 20/46,得到 %g/%g", out[0].Offset, out[1].Offset)
	}
	// 断言①已过却映射不上 = 内部不一致,必须报错而不是静默丢。
	if _, err := zaaMapTerms(pre[:1], terms, side, func(zfPlacedTerm) bool { return false }); err == nil {
		t.Fatal("端子多于可用 pin 必须报错")
	}
}

// 转竖消解:计划在上的端子,其 pin 实测必须更高。
func TestZaaVerticalOrderOK(t *testing.T) {
	pins := []layoutPin{{Number: "1", X: 0, Y: 100}, {Number: "2", X: 0, Y: 60}}
	terms := []zaaTermExec{
		{Pin: "1", ExpectUpper: true},
		{Pin: "2", ExpectUpper: false},
	}
	if !zaaVerticalOrderOK(pins, terms) {
		t.Error("上端子 pin 在上,该判 OK")
	}
	terms[0].Pin, terms[1].Pin = "2", "1" // 交叉:上端子映射到了下 pin
	if zaaVerticalOrderOK(pins, terms) {
		t.Error("上端子 pin 在下,该判不符(换旋转候选)")
	}
}

// 同网冗余 pin 的扩容:J2 真机 —— USB-C 的 GND 焊盘组 6 pin 全接地,块计划 5 只,
// 断言①曾按「集合不等」拒掉整页 apply。扩容后 sweep 删几只就重建几只;
// 计划完全没有的网(真正的意外连接)仍保持不等,交给 gate 拒。
func TestZaaPadTermsToPins(t *testing.T) {
	terms := []zfPlacedTerm{
		{Net: "GND", Dir: "left", Offset: 20},
		{Net: "5V", Dir: "right", Offset: 20},
	}
	pre := []zaaPinSnap{
		{Pin: "A1", Net: "GND"}, {Pin: "B1", Net: "GND"}, {Pin: "EP", Net: "GND"},
		{Pin: "A4", Net: "5V"},
	}
	got := zaaPadTermsToPins(terms, pre, map[string]bool{"GND": true, "5V": true, "IO0": true})
	gnd := 0
	for _, tm := range got {
		if tm.Net == "GND" {
			gnd++
			if tm.Dir != "left" || tm.Offset != 20 {
				t.Errorf("克隆端子该继承模板的 Dir/Offset,得到 %s/%g", tm.Dir, tm.Offset)
			}
		}
	}
	if gnd != 3 {
		t.Fatalf("GND 端子该扩容到 3(实际 pin 数),得到 %d", gnd)
	}
	// 计划没有的网不扩容(真意外连接留给 gate)。
	pre2 := append(pre, zaaPinSnap{Pin: "X1", Net: "MYSTERY"})
	if got2 := zaaPadTermsToPins(terms, pre2, map[string]bool{"GND": true, "5V": true}); len(got2) != len(got) {
		t.Fatalf("计划外的网不该被扩容进端子:%d → %d", len(got), len(got2))
	}
	// 共树 pin:计划外但页内有人认领的网 → 按实测侧合成端子(Q1-E 与 R3 共树案)。
	pre3 := append(pre[:4:4], zaaPinSnap{Pin: "2", Net: "USB_DTR", Dir: "down", Kind: "net_port_bi"})
	got4 := zaaPadTermsToPins(terms, pre3, map[string]bool{"GND": true, "5V": true, "USB_DTR": true})
	found := false
	for _, tm := range got4 {
		if tm.Net == "USB_DTR" {
			found = true
			if tm.Dir != "down" || tm.Kind != "netport" {
				t.Errorf("合成端子该用实测侧/信号口径,得到 %s/%s", tm.Dir, tm.Kind)
			}
		}
	}
	if !found {
		t.Fatal("页内认领的共树网该被合成端子")
	}
	// 实际比计划少:不收缩(「删了不重建」仍要红)。
	if got3 := zaaPadTermsToPins(terms, pre[:1], map[string]bool{"GND": true, "5V": true}); len(got3) != 2 {
		t.Fatalf("不收缩:%d", len(got3))
	}
}

// ── 断言③ 落地复判:绿勾必须与事实相符 ──────────────────────────────────────
//
// 缺陷形态(真机 4 轮取证):`--apply` 每轮都打「断言①② + 内核对账 + layout-lint
// 全绿,已保存」,而落地后 `zone-plan` 实测分区框重叠 2 / 1 / 2 处。三条既有判据
// 分别看电气 / 网表 / 器件两两重叠,没有一条看得见「区框胖了撞邻区」。

// 落地后确实重叠的场景:必须报出来,不许算绿。
func TestZaaRecheckFindings_ReportsLandedOverlap(t *testing.T) {
	zones := []zaaLandedZone{
		{Name: "U", PlanW: 315, PlanH: 351, FrameW: 353, FrameH: 382,
			Rect: layoutBBox{MinX: 0, MinY: 0, MaxX: 353, MaxY: 382}},
		{Name: "J_USB", PlanW: 300, PlanH: 300, FrameW: 300, FrameH: 300,
			Rect: layoutBBox{MinX: 331, MinY: 40, MaxX: 631, MaxY: 340}},
	}
	got := zaaRecheckFindings(zones, 12)
	if len(got) == 0 {
		t.Fatal("落地框比规划框胖 38/31、且与邻区实测重叠 —— 必须报,不能打绿勾")
	}
	joined := strings.Join(got, ";")
	if !strings.Contains(joined, "区 U 落地框") || !strings.Contains(joined, "gutter") {
		t.Errorf("尺寸偏差条目要指名区、实测框、规划框、gutter:%v", got)
	}
	if !strings.Contains(joined, "区框实测重叠 U ↔ J_USB") {
		t.Errorf("区框重叠必须单独成条(那正是用户实测到的 partitionOverlap):%v", got)
	}
}

// 偏差在 gutter 之内、区框不相交 = 复判绿(不许一有偏差就红,否则判据没人信)。
func TestZaaRecheckFindings_GreenWithinGutter(t *testing.T) {
	zones := []zaaLandedZone{
		{Name: "U", PlanW: 315, PlanH: 351, FrameW: 320, FrameH: 345,
			Rect: layoutBBox{MinX: 0, MinY: 0, MaxX: 320, MaxY: 345}},
		{Name: "Q", PlanW: 200, PlanH: 200, FrameW: 200, FrameH: 200,
			Rect: layoutBBox{MinX: 332, MinY: 0, MaxX: 532, MaxY: 200}},
	}
	if got := zaaRecheckFindings(zones, 12); len(got) != 0 {
		t.Fatalf("gutter 之内 + 零重叠该算绿:%v", got)
	}
}

// 读不到的成员是一等公民:绝不排除出分母、绝不合成 0 —— 一次读故障不许伪装成
// 「完美收敛」(progress-derived-not-recorded 同一条纪律)。
func TestZaaRecheckFindings_UnknownIsNotGreen(t *testing.T) {
	zones := []zaaLandedZone{{Name: "U", PlanW: 315, PlanH: 351, Missing: []string{"C7"}}}
	got := zaaRecheckFindings(zones, 12)
	if len(got) != 1 || !strings.Contains(got[0], "不算过") {
		t.Fatalf("成员读不到必须如实报、不算过:%v", got)
	}
}

// 复判用的说明带高必须与规划**逐区一致**:从规划框反推(框是唯一函数,带高是它
// 的可逆量),不许再读一遍 note —— 读第二遍就是第二把尺。
func TestZaaZoneNoteBand_RoundTripsPlanFrame(t *testing.T) {
	content := layoutBBox{MinX: 10, MinY: 20, MaxX: 210, MaxY: 120}
	const titleBand, noteBand = 30.0, 55.0
	w, h := partitionFrameSize(content, titleBand, noteBand)
	z := zoneArrangeZoneOut{Name: "U", Content: content, FrameW: w, FrameH: h}
	if got := zaaZoneNoteBand(z, titleBand); got != noteBand {
		t.Fatalf("说明带高该反推回 %.0f,got %.0f", noteBand, got)
	}
	// 反推出来的带高必须让实测框与规划框在无变化时逐字相等。
	r := partitionFrameRect(content, titleBand, zaaZoneNoteBand(z, titleBand))
	if r.MaxX-r.MinX != w || r.MaxY-r.MinY != h {
		t.Fatalf("反推带高后重算的框 %.0f×%.0f ≠ 规划框 %.0f×%.0f", r.MaxX-r.MinX, r.MaxY-r.MinY, w, h)
	}
}

// 桩长硬上限 = 计划里最长的桩(落地桩不越过规划桩 → 落地框不越过规划框)。
func TestZaaMaxPlannedStub(t *testing.T) {
	if got := zaaMaxPlannedStub(nil); got != zfStub {
		t.Fatalf("无计划端子时该退到短桩 %g,got %g", zfStub, got)
	}
	execs := []zaaMemberExec{
		{Terms: []zaaTermExec{{Offset: 20}, {Offset: 48}}},
		{Terms: []zaaTermExec{{Offset: 0}}}, // 0 = connect_pin 默认,不该拉低上限
	}
	if got := zaaMaxPlannedStub(execs); got != 48 {
		t.Fatalf("上限该取计划最长桩 48,got %g", got)
	}
}

// 单边判据:规划框是落地框的**上界**(已含落地余量),落地更瘦不是缺陷 ——
// 双边判会让结构性余量先吃掉一半 gutter 预算,判据就不再可信。
func TestZaaRecheckFindings_ThinnerThanPlanIsNotRed(t *testing.T) {
	zones := []zaaLandedZone{{Name: "U", PlanW: 343, PlanH: 444, FrameW: 320, FrameH: 410,
		Rect: layoutBBox{MinX: 0, MinY: 0, MaxX: 320, MaxY: 410}}}
	if got := zaaRecheckFindings(zones, 12); len(got) != 0 {
		t.Fatalf("落地比规划瘦不该报红:%v", got)
	}
	// 反向:胖出 gutter 必须红。
	zones[0].FrameW, zones[0].FrameH = 343+13, 444
	if got := zaaRecheckFindings(zones, 12); len(got) != 1 {
		t.Fatalf("胖出 gutter 必须红:%v", got)
	}
}
