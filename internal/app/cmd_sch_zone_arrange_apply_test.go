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
