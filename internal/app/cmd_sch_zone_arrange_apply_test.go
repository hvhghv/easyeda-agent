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
		{Kind: "netport", Net: "U3_N4", Dir: "left"},
		{Kind: "netport", Net: "U3_N4", Dir: "right"},
	}
	side := map[string]string{"A5": "right", "B5": "left"}
	out, err := zaaMapTerms(pre, terms, side, func(zfPlacedTerm) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Pin != "B5" || out[1].Pin != "A5" {
		t.Errorf("该按现侧配对(left→B5, right→A5),得到 %s/%s", out[0].Pin, out[1].Pin)
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
