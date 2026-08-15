package app

// cmd_sch_group_tidy_test.go — `sch group tidy` 纯核的表驱动测试(契约
// docs/schematic-layout-hierarchy.md §1):分类表 / planPowerUpdown 几何 /
// rot 二义消解 / 文字朝外校准表全查 / 几何发现 / extractor。

import (
	"reflect"
	"strings"
	"testing"
)

// ── classifyTidyMember:分类表 ───────────────────────────────────────────────

func TestClassifyTidyMember(t *testing.T) {
	cap2 := []tidyPinConn{
		{Pin: "1", Net: "3V3", Flag: "netflag"},
		{Pin: "2", Net: "GND", Flag: "netflag"},
	}
	cases := []struct {
		name  string
		desig string
		pins  []tidyPinConn
		want  tidyRole
	}{
		{"双电源旗电容", "C1", cap2, tidyRolePowerUpdown},
		{"VCC 命名也算电源族", "C4", []tidyPinConn{
			{Pin: "1", Net: "VCC_IO", Flag: "netflag"},
			{Pin: "2", Net: "PGND", Flag: "netflag"},
		}, tidyRolePowerUpdown},
		{"带 netport 件", "R3", []tidyPinConn{
			{Pin: "1", Net: "LED_CTRL", Flag: "netport"},
			{Pin: "2", Net: "GND", Flag: "netflag"},
		}, tidyRoleSignalRow},
		{"netport 优先于双旗(竖放折叠长条标)", "C3", []tidyPinConn{
			{Pin: "1", Net: "VCC", Flag: "netflag"},
			{Pin: "2", Net: "GND", Flag: "netflag"},
			{Pin: "3", Net: "EN", Flag: "netport"},
		}, tidyRoleSignalRow},
		{"IC 为锚", "U2", cap2, tidyRoleAnchorIC},
		{"小写 u 也是 IC", "u5", nil, tidyRoleAnchorIC},
		{"已整理件幂等:连接不变分类不变", "C1", cap2, tidyRolePowerUpdown},
		{"单地旗不够双旗", "C9", []tidyPinConn{
			{Pin: "1"},
			{Pin: "2", Net: "GND", Flag: "netflag"},
		}, tidyRoleSkip},
		{"双地旗不算 power-updown", "C7", []tidyPinConn{
			{Pin: "1", Net: "GND", Flag: "netflag"},
			{Pin: "2", Net: "AGND", Flag: "netflag"},
		}, tidyRoleSkip},
		{"信号 netflag 不构成双旗", "R1", []tidyPinConn{
			{Pin: "1", Net: "SIG", Flag: "netflag"},
			{Pin: "2", Net: "GND", Flag: "netflag"},
		}, tidyRoleSkip},
		{"netlabel 不算旗", "R2", []tidyPinConn{
			{Pin: "1", Net: "3V3", Flag: "netlabel"},
			{Pin: "2", Net: "GND", Flag: "netlabel"},
		}, tidyRoleSkip},
		{"未连接件", "C2", []tidyPinConn{{Pin: "1"}, {Pin: "2"}}, tidyRoleSkip},
		// ── F1/铁则5:双旗 + 未建模第三连接 → skip(不动比扯断好)──
		{"3-pin 馈通:第三 pin 普通导线 → skip", "C11", []tidyPinConn{
			{Pin: "1", Net: "5V", Flag: "netflag", OnWire: true},
			{Pin: "2", Net: "GND", Flag: "netflag", OnWire: true},
			{Pin: "3", Net: "SIG", OnWire: true}, // 无标记但在线上 = 真连接
		}, tidyRoleSkip},
		{"3-pin 馈通:第三 pin 信号 netflag → skip", "C12", []tidyPinConn{
			{Pin: "1", Net: "5V", Flag: "netflag", OnWire: true},
			{Pin: "2", Net: "GND", Flag: "netflag", OnWire: true},
			{Pin: "3", Net: "SIG", Flag: "netflag", OnWire: true},
		}, tidyRoleSkip},
		{"3-pin 馈通:第三 pin netlabel → skip", "C13", []tidyPinConn{
			{Pin: "1", Net: "5V", Flag: "netflag", OnWire: true},
			{Pin: "2", Net: "GND", Flag: "netflag", OnWire: true},
			{Pin: "3", Net: "SIG", Flag: "netlabel", OnWire: true},
		}, tidyRoleSkip},
		{"3-pin:第三 pin 悬空(不在线上)→ 仍 power-updown", "C14", []tidyPinConn{
			{Pin: "1", Net: "5V", Flag: "netflag", OnWire: true},
			{Pin: "2", Net: "GND", Flag: "netflag", OnWire: true},
			{Pin: "3"},
		}, tidyRolePowerUpdown},
		{"旗 pin 本身 OnWire(旗经导线连,常态)不影响判型", "C15", []tidyPinConn{
			{Pin: "1", Net: "3V3", Flag: "netflag", OnWire: true},
			{Pin: "2", Net: "GND", Flag: "netflag", OnWire: true},
		}, tidyRolePowerUpdown},
	}
	for _, tc := range cases {
		if got := classifyTidyMember(tc.desig, tc.pins); got != tc.want {
			t.Errorf("%s: classifyTidyMember(%s) = %s, want %s", tc.name, tc.desig, got, tc.want)
		}
	}
}

func TestTidyNetClass(t *testing.T) {
	cases := []struct{ net, want string }{
		{"GND", "ground"}, {"AGND", "ground"}, {"PGND", "ground"},
		{"DGND", "ground"}, {"VSS", "ground"}, {"GND_PWR", "ground"},
		{"gnd", "ground"},
		{"VCC", "power"}, {"VDD", "power"}, {"VBUS", "power"},
		{"VBAT", "power"}, {"VIN", "power"}, {"VCC_IO", "power"},
		{"3V3", "power"}, {"3.3V", "power"}, {"5V", "power"},
		{"+5V", "power"}, {"-5V", "power"}, {"12V0", "power"},
		{"LED_CTRL", "signal"}, {"UART_TX", "signal"}, {"V", "signal"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := tidyNetClass(tc.net); got != tc.want {
			t.Errorf("tidyNetClass(%q) = %q, want %q", tc.net, got, tc.want)
		}
	}
}

// ── tidyUnmodeledConn:power-updown 未建模连接判据(F1/铁则5)─────────────────

func TestTidyUnmodeledConn(t *testing.T) {
	cases := []struct {
		name string
		pin  tidyPinConn
		want bool
	}{
		{"power netflag = 已建模", tidyPinConn{Pin: "1", Net: "3V3", Flag: "netflag", OnWire: true}, false},
		{"gnd netflag = 已建模", tidyPinConn{Pin: "2", Net: "GND", Flag: "netflag", OnWire: true}, false},
		{"netport = signal-row 建模(planPowerUpdown 另行显式拒绝)", tidyPinConn{Pin: "1", Net: "EN", Flag: "netport", OnWire: true}, false},
		{"信号 netflag = 未建模", tidyPinConn{Pin: "3", Net: "SIG", Flag: "netflag", OnWire: true}, true},
		{"未知网 netflag(net 空)= 未建模(fail-closed)", tidyPinConn{Pin: "3", Flag: "netflag", OnWire: true}, true},
		{"netlabel = 未建模(connect_pin 建不回)", tidyPinConn{Pin: "3", Net: "3V3", Flag: "netlabel", OnWire: true}, true},
		{"无标记但在导线上 = 未建模(普通线连接)", tidyPinConn{Pin: "3", Net: "SIG", OnWire: true}, true},
		{"悬空 pin = 无连接", tidyPinConn{Pin: "3"}, false},
	}
	for _, tc := range cases {
		if got := tidyUnmodeledConn(tc.pin); got != tc.want {
			t.Errorf("%s: tidyUnmodeledConn(%+v) = %v, want %v", tc.name, tc.pin, got, tc.want)
		}
	}
}

// ── tidyLabelRotation:契约校准表全查(铁则3/4) ─────────────────────────────

func TestTidyLabelRotationFullTable(t *testing.T) {
	cases := []struct {
		kind, dir string
		want      float64
		wantErr   bool
	}{
		// 契约校准表(真机 2026-08-12):power up=0 / power down=180 /
		// gnd down=0 / gnd up=180。
		{"power", "up", 0, false},
		{"power", "down", 180, false},
		{"ground", "down", 0, false},
		{"ground", "up", 180, false},
		// ground 族别名走同一行。
		{"gnd", "down", 0, false},
		{"agnd", "up", 180, false},
		{"pgnd", "down", 0, false},
		{"analog_ground", "up", 180, false},
		{"protective_ground", "down", 0, false},
		// netport 只允许 left/right(铁则4)。
		{"netport", "left", 180, false},
		{"netport", "right", 0, false},
		{"net_port_in", "right", 0, false},
		{"net_port_out", "left", 180, false},
		{"net_port_bi", "right", 0, false},
		// 2026-08-12 用户拍板「顺着方向摆布」:netport 竖直合法(port 真值表)。
		{"netport", "up", 90, false},
		{"netport", "down", 270, false},
		{"net_port_bi", "up", 90, false},
		// 表外组合一律拒绝,不猜。
		{"power", "left", 0, true},
		{"power", "right", 0, true},
		{"ground", "left", 0, true},
		{"ground", "right", 0, true},
		{"power", "sideways", 0, true},
		{"mystery", "up", 0, true},
		{"", "up", 0, true},
	}
	for _, tc := range cases {
		got, err := tidyLabelRotation(tc.kind, tc.dir)
		if tc.wantErr {
			if err == nil {
				t.Errorf("tidyLabelRotation(%q,%q) = %g, want error", tc.kind, tc.dir, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("tidyLabelRotation(%q,%q): unexpected error %v", tc.kind, tc.dir, err)
			continue
		}
		if got != tc.want {
			t.Errorf("tidyLabelRotation(%q,%q) = %g, want %g", tc.kind, tc.dir, got, tc.want)
		}
	}
}

// ── planPowerUpdown:几何(间距 / 上电下地 / 文字 rotation / 排序 / snap) ────

func tidyCapIn(desig, powerNet string) tidyMemberIn {
	return tidyMemberIn{Designator: desig, Pins: []tidyPinConn{
		{Pin: "1", Net: powerNet, Flag: "netflag"},
		{Pin: "2", Net: "GND", Flag: "netflag"},
	}}
}

func TestPlanPowerUpdownCentered(t *testing.T) {
	members := []tidyMemberIn{tidyCapIn("C1", "3V3"), tidyCapIn("C2", "3V3"), tidyCapIn("C3", "5V")}
	anchor := tidyAnchor{X: 400, Y: 300}              // 无 IC:bbox 中心锚,横排居中
	plans, err := planPowerUpdown(members, anchor, 0) // spacing 0 → 默认 50
	if err != nil {
		t.Fatalf("planPowerUpdown: %v", err)
	}
	if len(plans) != 3 {
		t.Fatalf("got %d plans, want 3", len(plans))
	}
	wantX := []float64{350, 400, 450}
	for i, p := range plans {
		if p.X != wantX[i] || p.Y != 300 {
			t.Errorf("plan[%d] %s at (%g,%g), want (%g,300)", i, p.Designator, p.X, p.Y, wantX[i])
		}
		if p.RotationCandidates != [2]float64{90, 270} {
			t.Errorf("plan[%d] candidates %v, want {90,270}", i, p.RotationCandidates)
		}
		if len(p.Pins) != 2 {
			t.Fatalf("plan[%d] has %d pin targets, want 2", i, len(p.Pins))
		}
		up, down := p.Pins[0], p.Pins[1]
		if up.Pin != p.PowerPin || up.Direction != "up" || up.Kind != "power" || up.LabelRotation != 0 {
			t.Errorf("plan[%d] power target %+v — want pin %s up power label 0", i, up, p.PowerPin)
		}
		if down.Pin != p.GndPin || down.Direction != "down" || down.Kind != "ground" || down.LabelRotation != 0 {
			t.Errorf("plan[%d] gnd target %+v — want pin %s down ground label 0", i, down, p.GndPin)
		}
		if down.Net != "GND" {
			t.Errorf("plan[%d] gnd net %q, want GND", i, down.Net)
		}
	}
	if plans[2].Pins[0].Net != "5V" {
		t.Errorf("C3 power net %q, want 5V", plans[2].Pins[0].Net)
	}
}

func TestPlanPowerUpdownICAnchor(t *testing.T) {
	members := []tidyMemberIn{tidyCapIn("C1", "VCC"), tidyCapIn("C2", "VCC")}
	anchor := tidyAnchor{X: 500, Y: 400, IsIC: true, HalfWidth: 60}
	plans, err := planPowerUpdown(members, anchor, 50)
	if err != nil {
		t.Fatalf("planPowerUpdown: %v", err)
	}
	// IC 锚:从 IC bbox 右侧 + spacing 起排 → startX = 500+60+50 = 610。
	wantX := []float64{610, 660}
	for i, p := range plans {
		if p.X != wantX[i] || p.Y != 400 {
			t.Errorf("plan[%d] %s at (%g,%g), want (%g,400)", i, p.Designator, p.X, p.Y, wantX[i])
		}
	}
}

func TestPlanPowerUpdownNaturalOrderAndSnap(t *testing.T) {
	// 输入乱序 C10,C2 → 自然序 C2 在前;锚坐标非格点 → snap 到 5。
	members := []tidyMemberIn{tidyCapIn("C10", "3V3"), tidyCapIn("C2", "3V3")}
	plans, err := planPowerUpdown(members, tidyAnchor{X: 402, Y: 301}, 50)
	if err != nil {
		t.Fatalf("planPowerUpdown: %v", err)
	}
	if plans[0].Designator != "C2" || plans[1].Designator != "C10" {
		t.Errorf("order = %s,%s — want C2,C10 (自然序)", plans[0].Designator, plans[1].Designator)
	}
	for i, p := range plans {
		if p.X != snap5(p.X) || p.Y != snap5(p.Y) {
			t.Errorf("plan[%d] (%g,%g) 未 snap 到 5 格", i, p.X, p.Y)
		}
	}
	if plans[0].Y != 300 {
		t.Errorf("rowY = %g, want 300 (snap5(301))", plans[0].Y)
	}
}

func TestPlanPowerUpdownIdempotent(t *testing.T) {
	members := []tidyMemberIn{tidyCapIn("C1", "3V3"), tidyCapIn("C2", "3V3")}
	anchor := tidyAnchor{X: 400, Y: 300}
	a, err := planPowerUpdown(members, anchor, 50)
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	b, err := planPowerUpdown(members, anchor, 50)
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("plan 不幂等:\n first=%+v\nsecond=%+v", a, b)
	}
}

func TestPlanPowerUpdownErrors(t *testing.T) {
	anchor := tidyAnchor{X: 0, Y: 0}
	cases := []struct {
		name   string
		member tidyMemberIn
	}{
		{"netport 件拒绝(属 signal-row)", tidyMemberIn{Designator: "R3", Pins: []tidyPinConn{
			{Pin: "1", Net: "EN", Flag: "netport"},
			{Pin: "2", Net: "GND", Flag: "netflag"},
		}}},
		{"缺地旗", tidyMemberIn{Designator: "C9", Pins: []tidyPinConn{
			{Pin: "1", Net: "3V3", Flag: "netflag"},
			{Pin: "2"},
		}}},
		{"缺电源旗", tidyMemberIn{Designator: "C8", Pins: []tidyPinConn{
			{Pin: "1", Net: "GND", Flag: "netflag"},
			{Pin: "2"},
		}}},
		{"双电源 pin", tidyMemberIn{Designator: "U0X", Pins: []tidyPinConn{
			{Pin: "1", Net: "3V3", Flag: "netflag"},
			{Pin: "2", Net: "5V", Flag: "netflag"},
			{Pin: "3", Net: "GND", Flag: "netflag"},
		}}},
		{"双地 pin", tidyMemberIn{Designator: "C6", Pins: []tidyPinConn{
			{Pin: "1", Net: "3V3", Flag: "netflag"},
			{Pin: "2", Net: "GND", Flag: "netflag"},
			{Pin: "3", Net: "AGND", Flag: "netflag"},
		}}},
	}
	for _, tc := range cases {
		if _, err := planPowerUpdown([]tidyMemberIn{tc.member}, anchor, 50); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}

// F1/铁则5:3-pin 馈通电容(pin1=5V 旗、pin2=GND 旗、pin3=信号/普通线)——
// 显式 --pattern power-updown 必须报错(不许静默扯断第三 pin),错误信息点名
// 违规 pin;auto 判型对同一件降级 skip(见 TestClassifyTidyMember)。
func TestPlanPowerUpdownFeedthrough3Pin(t *testing.T) {
	feedthrough := func(third tidyPinConn) tidyMemberIn {
		return tidyMemberIn{Designator: "C20", Pins: []tidyPinConn{
			{Pin: "1", Net: "5V", Flag: "netflag", OnWire: true},
			{Pin: "2", Net: "GND", Flag: "netflag", OnWire: true},
			third,
		}}
	}
	cases := []struct {
		name         string
		third        tidyPinConn
		wantContains []string
	}{
		{"第三 pin 普通导线(树上无标记)", tidyPinConn{Pin: "3", Net: "SIG", OnWire: true},
			[]string{"pin 3", "未建模", "普通导线"}},
		{"第三 pin 信号 netflag", tidyPinConn{Pin: "3", Net: "SIG", Flag: "netflag", OnWire: true},
			[]string{"pin 3", "未建模", "netflag SIG"}},
		{"第三 pin netlabel", tidyPinConn{Pin: "3", Net: "SIG", Flag: "netlabel", OnWire: true},
			[]string{"pin 3", "未建模", "netlabel SIG"}},
	}
	for _, tc := range cases {
		_, err := planPowerUpdown([]tidyMemberIn{feedthrough(tc.third)}, tidyAnchor{X: 100, Y: 100}, 50)
		if err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
			continue
		}
		for _, sub := range tc.wantContains {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("%s: 错误信息 %q 缺少 %q", tc.name, err.Error(), sub)
			}
		}
	}
	// 第三 pin 真悬空(不在任何导线上)→ 无连接可扯断,照常出计划。
	plans, err := planPowerUpdown([]tidyMemberIn{feedthrough(tidyPinConn{Pin: "3"})}, tidyAnchor{X: 100, Y: 100}, 50)
	if err != nil || len(plans) != 1 {
		t.Errorf("悬空第三 pin: got (%d plans, %v), want 1 plan no error", len(plans), err)
	}
}

func TestPlanPowerUpdownEmpty(t *testing.T) {
	plans, err := planPowerUpdown(nil, tidyAnchor{}, 50)
	if err != nil || plans != nil {
		t.Errorf("empty members: got (%v,%v), want (nil,nil)", plans, err)
	}
}

// ── rot 二义消解:mock 两种镜像(铁则1) ────────────────────────────────────

func TestTidyPowerPinOnTop(t *testing.T) {
	cases := []struct {
		name    string
		pins    []layoutPin
		power   string
		gnd     string
		want    bool
		wantErr bool
	}{
		{"镜像A:候选1 即电源 pin 在上(y-UP)", []layoutPin{
			{Number: "1", X: 100, Y: 450}, {Number: "2", X: 100, Y: 400},
		}, "1", "2", true, false},
		{"镜像B:候选1 电源 pin 在下 → 需换候选2", []layoutPin{
			{Number: "1", X: 100, Y: 400}, {Number: "2", X: 100, Y: 450},
		}, "1", "2", false, false},
		{"同高 = 没立起来 → 错误拒连", []layoutPin{
			{Number: "1", X: 80, Y: 400}, {Number: "2", X: 120, Y: 400},
		}, "1", "2", false, true},
		{"缺 pin → 错误", []layoutPin{
			{Number: "1", X: 100, Y: 450},
		}, "1", "2", false, true},
		{"实测空集 → 错误", nil, "1", "2", false, true},
	}
	for _, tc := range cases {
		got, err := tidyPowerPinOnTop(tc.pins, tc.power, tc.gnd)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: want error, got %v", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: onTop = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ── planSignalRow:左入右出 + netport 水平(铁则4) ──────────────────────────

func TestPlanSignalRow(t *testing.T) {
	members := []tidySignalMemberIn{
		{Designator: "R3", CenterX: 500, Pins: []tidySignalPinIn{
			{Pin: "1", X: 470, Net: "LED_IN", IsPort: true},
			{Pin: "2", X: 530, Net: "LED_OUT", IsPort: true},
			{Pin: "3", X: 500, Net: "GND", IsPort: false}, // 非 port 不出目标
		}},
		{Designator: "R4", CenterX: 600, Pins: []tidySignalPinIn{
			{Pin: "1", X: 600, Net: "MID", IsPort: true}, // 恰在中线 → left
		}},
		{Designator: "R5", CenterX: 700, Pins: []tidySignalPinIn{
			{Pin: "1", X: 690, Net: "X", IsPort: false}, // 无 port → 整件不出计划
		}},
	}
	plans, err := planSignalRow(members)
	if err != nil {
		t.Fatalf("planSignalRow: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("got %d plans, want 2 (R5 无 port 应省略)", len(plans))
	}
	r3 := plans[0]
	if r3.Designator != "R3" || len(r3.Pins) != 2 {
		t.Fatalf("r3 plan %+v", r3)
	}
	if r3.Pins[0].Direction != "left" || r3.Pins[0].LabelRotation != 180 || r3.Pins[0].Kind != "net_port_bi" {
		t.Errorf("R3 pin1 %+v — want left / label 180 / net_port_bi", r3.Pins[0])
	}
	if r3.Pins[1].Direction != "right" || r3.Pins[1].LabelRotation != 0 {
		t.Errorf("R3 pin2 %+v — want right / label 0", r3.Pins[1])
	}
	if plans[1].Pins[0].Direction != "left" {
		t.Errorf("R4 pin1 恰在中线应判 left, got %s", plans[1].Pins[0].Direction)
	}
}

// ── 几何发现:pin↔标记归属 / stub 方向 ──────────────────────────────────────

func tidyMarker(id, ctype, net string, x, y float64) layoutComp {
	return layoutComp{ID: id, ComponentType: ctype, Net: net, X: x, Y: y, AnchorAvailable: true}
}

func TestTidyPinAttachment(t *testing.T) {
	wires := []schGroupWire{
		{ID: "w1", Points: []float64{100, 100, 120, 100}},
		{ID: "w2", Points: []float64{120, 100, 140, 100}}, // 与 w1 共点成树
		{ID: "w3", Points: []float64{300, 300, 340, 300}}, // 独立树
	}
	roots := tidyWireRoots(wires)
	flagGND := tidyMarker("f1", "netflag", "GND", 140, 100)
	portEN := tidyMarker("f2", "netport", "EN", 140, 100)
	flagFar := tidyMarker("f3", "netflag", "3V3", 340, 300)

	// 直连桩:pin → w1;标记锚在 w2 远端,经树归属找到。
	m, ok, onWire := tidyPinAttachment(100, 100, wires, roots, []layoutComp{flagGND, flagFar})
	if !ok || m.ID != "f1" || !onWire {
		t.Errorf("merged-tree attachment = (%v,%v,onWire=%v), want f1 onWire", m.ID, ok, onWire)
	}
	// 同树 netflag+netport 并存 → netport 优先(决定 signal-row 分类)。
	m, ok, _ = tidyPinAttachment(100, 100, wires, roots, []layoutComp{flagGND, portEN})
	if !ok || m.ID != "f2" {
		t.Errorf("netport priority = (%v,%v), want f2", m.ID, ok)
	}
	// 别的树上的标记不算。
	m, ok, _ = tidyPinAttachment(300, 300, wires, roots, []layoutComp{flagGND, flagFar})
	if !ok || m.ID != "f3" {
		t.Errorf("far tree = (%v,%v), want f3", m.ID, ok)
	}
	// pin 不在任何 wire 上 → 无归属且不在线上(压坐标不算连接)。
	if _, ok, onWire = tidyPinAttachment(999, 999, wires, roots, []layoutComp{flagGND}); ok || onWire {
		t.Errorf("isolated pin = (ok=%v,onWire=%v), want no attachment / not on wire", ok, onWire)
	}
	// F1:pin 在导线上但树上无任何标记 → 无归属但 onWire=true(普通导线连接,
	// 不能折叠成「未连接」—— 否则 tidy 搬移会静默扯断它,铁则5)。
	m, ok, onWire = tidyPinAttachment(300, 300, wires, roots, []layoutComp{flagGND})
	if ok || !onWire {
		t.Errorf("bare-wire pin = (%v,ok=%v,onWire=%v), want no marker but onWire", m.ID, ok, onWire)
	}
	// 标记锚在 wire 中段(EasyEDA 合并遗留)也算。
	mid := tidyMarker("f4", "netflag", "GND", 110, 100)
	m, ok, _ = tidyPinAttachment(100, 100, wires, roots, []layoutComp{mid})
	if !ok || m.ID != "f4" {
		t.Errorf("mid-span marker = (%v,%v), want f4", m.ID, ok)
	}
}

func TestTidyStubDirection(t *testing.T) {
	cases := []struct {
		name           string
		px, py, ax, ay float64
		wantDir        string
		wantOff        float64
	}{
		{"right", 100, 100, 140, 100, "right", 40},
		{"left", 100, 100, 60, 100, "left", 40},
		{"up (y-UP:dy>0)", 100, 100, 100, 140, "up", 40},
		{"down", 100, 100, 100, 60, "down", 40},
		{"斜线取主轴", 100, 100, 150, 110, "right", 50},
	}
	for _, tc := range cases {
		dir, off := tidyStubDirection(tc.px, tc.py, tc.ax, tc.ay)
		if dir != tc.wantDir || off != tc.wantOff {
			t.Errorf("%s: (%s,%g), want (%s,%g)", tc.name, dir, off, tc.wantDir, tc.wantOff)
		}
	}
}

// ── extractor / settle 判据 / 自然序 ────────────────────────────────────────

func TestTidyExtractExtras(t *testing.T) {
	result := map[string]any{"components": []any{
		map[string]any{
			"designator": "C1", "componentType": "part", "rotation": 90.0,
			"pins": []any{
				map[string]any{"pinNumber": "1", "net": "3V3", "x": 1.0, "y": 2.0},
				map[string]any{"pinNumber": "2", "net": "GND"},
				map[string]any{"pinNumber": "3"}, // 无 net → 不入表
			},
		},
		map[string]any{"componentType": "netflag", "net": "GND"},    // 标记不入表
		map[string]any{"designator": "R1", "componentType": "part"}, // 无 rotation/pins
	}}
	ex := tidyExtractExtras(result)
	c1, ok := ex["C1"]
	if !ok {
		t.Fatal("C1 missing from extras")
	}
	if c1.Rotation != 90 {
		t.Errorf("C1 rotation = %g, want 90", c1.Rotation)
	}
	if c1.PinNets["1"] != "3V3" || c1.PinNets["2"] != "GND" {
		t.Errorf("C1 pin nets = %v", c1.PinNets)
	}
	if _, has := c1.PinNets["3"]; has {
		t.Error("pin 3 has no net and must not be recorded")
	}
	if _, ok := ex["R1"]; !ok {
		t.Error("R1 (no pins) should still be present with zero rotation")
	}
	if len(ex) != 2 {
		t.Errorf("extras has %d entries, want 2 (markers excluded)", len(ex))
	}
}

func TestTidyPinsAgree(t *testing.T) {
	a := []layoutPin{{Number: "1", X: 100, Y: 200}, {Number: "2", X: 100, Y: 160}}
	same := []layoutPin{{Number: "2", X: 100, Y: 160}, {Number: "1", X: 100.2, Y: 200}} // 顺序无关,eps 内
	moved := []layoutPin{{Number: "1", X: 100, Y: 300}, {Number: "2", X: 100, Y: 160}}
	fewer := []layoutPin{{Number: "1", X: 100, Y: 200}}
	if !tidyPinsAgree(a, same) {
		t.Error("eps 内应判一致")
	}
	if tidyPinsAgree(a, moved) {
		t.Error("坐标漂移应判不一致")
	}
	if tidyPinsAgree(a, fewer) {
		t.Error("数量不同应判不一致")
	}
}

func TestTidyDesignatorLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"C2", "C10", true}, // 数字自然序,非字典序
		{"C10", "C2", false},
		{"C1", "R1", true}, // 前缀字典序
		{"C1", "C1", false},
	}
	for _, tc := range cases {
		if got := tidyDesignatorLess(tc.a, tc.b); got != tc.want {
			t.Errorf("tidyDesignatorLess(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// ── anchor / 计划编排 ───────────────────────────────────────────────────────

func tidyLivePart(desig string, x, y float64, bbox *layoutBBox, role tidyRole) tidyLiveMember {
	return tidyLiveMember{
		Comp: layoutComp{ID: "id-" + desig, Designator: desig, ComponentType: schLayoutPartType,
			X: x, Y: y, AnchorAvailable: true, BBox: bbox},
		Role: role,
	}
}

func TestTidyResolveAnchor(t *testing.T) {
	ic := tidyLivePart("U1", 500, 400, &layoutBBox{MinX: 440, MinY: 340, MaxX: 560, MaxY: 460}, tidyRoleAnchorIC)
	cap1 := tidyLivePart("C1", 100, 100, &layoutBBox{MinX: 90, MinY: 80, MaxX: 110, MaxY: 120}, tidyRolePowerUpdown)
	cap2 := tidyLivePart("C2", 300, 100, &layoutBBox{MinX: 290, MinY: 80, MaxX: 310, MaxY: 120}, tidyRolePowerUpdown)

	a, desig, ok := tidyResolveAnchor([]tidyLiveMember{cap1, ic, cap2})
	if !ok || !a.IsIC || desig != "U1" {
		t.Fatalf("anchor = %+v (%s, ok=%v), want IC U1", a, desig, ok)
	}
	if a.X != 500 || a.Y != 400 || a.HalfWidth != 60 {
		t.Errorf("IC anchor geometry = %+v, want (500,400) halfWidth 60", a)
	}

	a, desig, ok = tidyResolveAnchor([]tidyLiveMember{cap1, cap2})
	if !ok || a.IsIC || desig != "" {
		t.Fatalf("no-IC anchor = %+v (%s, ok=%v), want bbox-center", a, desig, ok)
	}
	if a.X != 200 || a.Y != 100 {
		t.Errorf("bbox-center anchor = (%g,%g), want (200,100)", a.X, a.Y)
	}

	// F4:全员既无 bbox 又无锚坐标 → ok=false(不许拿零值锚继续,否则整排
	// 错排到 (0,0))。
	noGeo1 := tidyLivePart("C1", 0, 0, nil, tidyRolePowerUpdown)
	noGeo1.Comp.AnchorAvailable = false
	noGeo2 := tidyLivePart("C2", 0, 0, nil, tidyRolePowerUpdown)
	noGeo2.Comp.AnchorAvailable = false
	if a, desig, ok = tidyResolveAnchor([]tidyLiveMember{noGeo1, noGeo2}); ok {
		t.Errorf("no-geometry anchor = %+v (%s, ok=%v), want ok=false", a, desig, ok)
	}

	// F4:无几何的 IC 当不了锚 —— 跳过,让位给有几何成员的 bbox 中心。
	blindIC := tidyLivePart("U9", 0, 0, nil, tidyRoleAnchorIC)
	blindIC.Comp.AnchorAvailable = false
	a, desig, ok = tidyResolveAnchor([]tidyLiveMember{blindIC, cap1, cap2})
	if !ok || a.IsIC || desig != "" {
		t.Fatalf("blind-IC anchor = %+v (%s, ok=%v), want bbox-center fallback", a, desig, ok)
	}
	if a.X != 200 || a.Y != 100 {
		t.Errorf("blind-IC fallback anchor = (%g,%g), want (200,100)", a.X, a.Y)
	}
}

func TestTidyVerticalPortPins(t *testing.T) {
	m := tidyLiveMember{
		Comp: layoutComp{Designator: "R3"},
		Pins: []tidyLivePin{
			{Conn: tidyPinConn{Pin: "1", Net: "EN", Flag: "netport"}, X: 100, Y: 100,
				Marker: tidyMarker("f1", "netport", "EN", 100, 140), HasMarker: true}, // 竖桩 → 违例
			{Conn: tidyPinConn{Pin: "2", Net: "OUT", Flag: "netport"}, X: 140, Y: 100,
				Marker: tidyMarker("f2", "netport", "OUT", 180, 100), HasMarker: true}, // 水平 → 合规
			{Conn: tidyPinConn{Pin: "3", Net: "GND", Flag: "netflag"}, X: 120, Y: 80,
				Marker: tidyMarker("f3", "netflag", "GND", 120, 40), HasMarker: true}, // netflag 不属本规则
		},
	}
	got := tidyVerticalPortPins(m)
	if !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("vertical port pins = %v, want [1]", got)
	}
}

func TestBuildTidyPlanAuto(t *testing.T) {
	ic := tidyLivePart("U1", 500, 400, &layoutBBox{MinX: 440, MinY: 340, MaxX: 560, MaxY: 460}, tidyRoleAnchorIC)
	capLive := tidyLivePart("C1", 100, 100, nil, tidyRolePowerUpdown)
	capLive.Pins = []tidyLivePin{
		{Conn: tidyPinConn{Pin: "1", Net: "3V3", Flag: "netflag"}, X: 90, Y: 100,
			Marker: tidyMarker("f1", "netflag", "3V3", 50, 100), HasMarker: true},
		{Conn: tidyPinConn{Pin: "2", Net: "GND", Flag: "netflag"}, X: 110, Y: 100,
			Marker: tidyMarker("f2", "netflag", "GND", 150, 100), HasMarker: true},
	}
	sig := tidyLivePart("R3", 700, 100, nil, tidyRoleSignalRow)
	sig.Pins = []tidyLivePin{
		{Conn: tidyPinConn{Pin: "1", Net: "EN", Flag: "netport"}, X: 690, Y: 100,
			Marker: tidyMarker("f4", "netport", "EN", 690, 140), HasMarker: true}, // 竖放 → 要修
	}
	skip := tidyLivePart("R9", 900, 100, nil, tidyRoleSkip)

	members := map[string]tidyLiveMember{"U1": ic, "C1": capLive, "R3": sig, "R9": skip}
	order := []string{"U1", "C1", "R3", "R9"}
	plan, err := buildTidyPlan(members, order, "auto", 50, false)
	if err != nil {
		t.Fatalf("buildTidyPlan: %v", err)
	}
	if plan.AnchorDesig != "U1" || !plan.Anchor.IsIC {
		t.Errorf("anchor = %+v (%s), want IC U1", plan.Anchor, plan.AnchorDesig)
	}
	if len(plan.Power) != 1 || plan.Power[0].Designator != "C1" {
		t.Fatalf("power plans = %+v, want [C1]", plan.Power)
	}
	// IC 锚 (500,400) 半宽 60 → C1 排到 (610,400)。
	if plan.Power[0].X != 610 || plan.Power[0].Y != 400 {
		t.Errorf("C1 target = (%g,%g), want (610,400)", plan.Power[0].X, plan.Power[0].Y)
	}
	if len(plan.Signal) != 1 || plan.Signal[0].Designator != "R3" || len(plan.Signal[0].Pins) != 1 {
		t.Fatalf("signal plans = %+v, want R3 一个竖放 pin", plan.Signal)
	}
	if !reflect.DeepEqual(plan.Skipped, []string{"R9"}) {
		t.Errorf("skipped = %v, want [R9]", plan.Skipped)
	}
	// 已水平 netport 的件 → no-op(幂等)。
	sigOK := tidyLivePart("R4", 800, 100, nil, tidyRoleSignalRow)
	sigOK.Pins = []tidyLivePin{
		{Conn: tidyPinConn{Pin: "1", Net: "X", Flag: "netport"}, X: 790, Y: 100,
			Marker: tidyMarker("f5", "netport", "X", 750, 100), HasMarker: true},
	}
	members["R4"] = sigOK
	plan, err = buildTidyPlan(members, append(order, "R4"), "auto", 50, false)
	if err != nil {
		t.Fatalf("buildTidyPlan (R4): %v", err)
	}
	if !reflect.DeepEqual(plan.SignalNoop, []string{"R4"}) {
		t.Errorf("signal noop = %v, want [R4]", plan.SignalNoop)
	}
}

// F4:组内有 power-updown 件但全员无 bbox/锚坐标 → 锚不可得必须报错,
// 不许拿零值锚把整排排到 (0,0)。
func TestBuildTidyPlanAnchorUnresolvable(t *testing.T) {
	blind := tidyLivePart("C1", 0, 0, nil, tidyRolePowerUpdown)
	blind.Comp.AnchorAvailable = false
	blind.Pins = []tidyLivePin{
		{Conn: tidyPinConn{Pin: "1", Net: "3V3", Flag: "netflag", OnWire: true},
			Marker: tidyMarker("f1", "netflag", "3V3", 0, 40), HasMarker: true},
		{Conn: tidyPinConn{Pin: "2", Net: "GND", Flag: "netflag", OnWire: true},
			Marker: tidyMarker("f2", "netflag", "GND", 0, -40), HasMarker: true},
	}
	members := map[string]tidyLiveMember{"C1": blind}
	_, err := buildTidyPlan(members, []string{"C1"}, "auto", 50, false)
	if err == nil {
		t.Fatal("锚不可得应报错,got nil")
	}
	if !strings.Contains(err.Error(), "锚不可得") {
		t.Errorf("错误信息 %q 应点名锚不可得", err.Error())
	}
	// 同组只有 skip 件(无人按锚排)→ 锚不可得不阻断(signal-row/skip 不用锚)。
	skipOnly := tidyLivePart("R9", 0, 0, nil, tidyRoleSkip)
	skipOnly.Comp.AnchorAvailable = false
	plan, err := buildTidyPlan(map[string]tidyLiveMember{"R9": skipOnly}, []string{"R9"}, "auto", 50, false)
	if err != nil {
		t.Fatalf("纯 skip 组不应因锚报错:%v", err)
	}
	if !reflect.DeepEqual(plan.Skipped, []string{"R9"}) {
		t.Errorf("skipped = %v, want [R9]", plan.Skipped)
	}
}

// ── buildTidyMembers:OnWire 穿线(F1 几何发现 → 分类)────────────────────────

func TestBuildTidyMembersOnWireThreading(t *testing.T) {
	// 3-pin 馈通电容:pin1 经导线挂 5V 旗、pin2 挂 GND 旗、pin3 经普通导线
	// 连出去(树上无任何标记)。buildTidyMembers 必须把 pin3 标成 OnWire,
	// classify 因此判 skip —— 修复前 pin3 被折叠成「未连接」→ power-updown
	// 搬走器件把线扯断成开路。
	g := &schGroup{ID: "g1", Members: []string{"C1"}}
	part := layoutComp{
		ID: "id-C1", Designator: "C1", ComponentType: schLayoutPartType,
		X: 100, Y: 80, AnchorAvailable: true,
		Pins: []layoutPin{
			{Number: "1", X: 100, Y: 100},
			{Number: "2", X: 100, Y: 60},
			{Number: "3", X: 140, Y: 80},
		},
	}
	flag5V := tidyMarker("f1", "netflag", "5V", 100, 140)
	flagGND := tidyMarker("f2", "netflag", "GND", 100, 20)
	wires := []schGroupWire{
		{ID: "w1", Points: []float64{100, 100, 100, 140}}, // pin1 → 5V 旗
		{ID: "w2", Points: []float64{100, 60, 100, 20}},   // pin2 → GND 旗
		{ID: "w3", Points: []float64{140, 80, 200, 80}},   // pin3 → 普通导线,无标记
	}
	members, order, err := buildTidyMembers(g, []layoutComp{part, flag5V, flagGND}, nil, wires)
	if err != nil {
		t.Fatalf("buildTidyMembers: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"C1"}) {
		t.Fatalf("order = %v", order)
	}
	m := members["C1"]
	p3 := m.pin("3")
	if p3 == nil {
		t.Fatal("pin 3 missing")
	}
	if !p3.Conn.OnWire || p3.HasMarker || p3.Conn.Flag != "" {
		t.Errorf("pin3 = %+v — want OnWire=true 无标记(普通导线连接不许折叠成未连接)", p3.Conn)
	}
	if p1 := m.pin("1"); p1 == nil || !p1.Conn.OnWire || p1.Conn.Flag != "netflag" {
		t.Errorf("pin1 = %+v — want netflag + OnWire", p1)
	}
	if m.Role != tidyRoleSkip {
		t.Errorf("3-pin 馈通 auto 判型 = %s, want skip(铁则5:第三连接未建模)", m.Role)
	}
}

// ── tidyGuardDisconnect:共享导线连带断开 fail-fast(F2/铁则5)───────────────

func TestTidyGuardDisconnect(t *testing.T) {
	cases := []struct {
		name         string
		result       map[string]any
		wantErr      bool
		wantContains []string
	}{
		{"nil result(旧连接器无该字段)→ 放行", nil, false, nil},
		{"无 alsoDisconnectedPins 字段 → 放行", map[string]any{"disconnected": true}, false, nil},
		{"空列表 → 放行", map[string]any{"alsoDisconnectedPins": []any{}}, false, nil},
		{"连带断开邻件 pin → 错误且列出受影响 pin",
			map[string]any{"alsoDisconnectedPins": []any{"C2:1", "C2:2"}}, true,
			[]string{"C1:2", "C2:1", "C2:2", "铁则5"}},
		{"[]string 形态同样识别",
			map[string]any{"alsoDisconnectedPins": []string{"R7:1"}}, true,
			[]string{"R7:1"}},
	}
	for _, tc := range cases {
		err := tidyGuardDisconnect("C1", "2", tc.result)
		if !tc.wantErr {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
			continue
		}
		for _, sub := range tc.wantContains {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("%s: 错误信息 %q 缺少 %q", tc.name, err.Error(), sub)
			}
		}
	}
}

func TestTidyDisconnectCollateral(t *testing.T) {
	if got := tidyDisconnectCollateral(nil); got != nil {
		t.Errorf("nil result → %v, want nil", got)
	}
	got := tidyDisconnectCollateral(map[string]any{"alsoDisconnectedPins": []any{"C2:1", "", 42, "C2:2"}})
	if !reflect.DeepEqual(got, []string{"C2:1", "C2:2"}) {
		t.Errorf("collateral = %v, want [C2:1 C2:2](空串/非串剔除)", got)
	}
}

// ── 回滚记录 ────────────────────────────────────────────────────────────────

func TestTidyBuildRecord(t *testing.T) {
	live := tidyLivePart("C1", 200, 100, nil, tidyRolePowerUpdown)
	live.Rotation = 270
	live.Pins = []tidyLivePin{
		{Conn: tidyPinConn{Pin: "1", Net: "3V3", Flag: "netflag"}, X: 190, Y: 100,
			Marker: tidyMarker("f1", "netflag", "3V3", 150, 100), HasMarker: true},
		{Conn: tidyPinConn{Pin: "2", Net: "GND", Flag: "netflag"}, X: 210, Y: 100,
			Marker: tidyMarker("f2", "netflag", "GND", 210, 60), HasMarker: true},
	}
	rec := tidyBuildRecord(live, []string{"1", "2"})
	if rec.PrimitiveID != "id-C1" || rec.OrigX != 200 || rec.OrigY != 100 || rec.OrigRot != 270 {
		t.Errorf("record pose = %+v", rec)
	}
	if len(rec.Restores) != 2 {
		t.Fatalf("restores = %d, want 2", len(rec.Restores))
	}
	r1, r2 := rec.Restores[0], rec.Restores[1]
	if r1.Kind != "power" || r1.Direction != "left" || r1.Offset != 40 || !r1.HasFlag {
		t.Errorf("restore pin1 = %+v — want power left 40", r1)
	}
	if r2.Kind != "ground" || r2.Direction != "down" || r2.Offset != 40 {
		t.Errorf("restore pin2 = %+v — want ground down 40", r2)
	}
}

func TestTidyRestoreKind(t *testing.T) {
	cases := []struct{ mtype, net, want string }{
		{"netflag", "GND", "ground"},
		{"netflag", "3V3", "power"},
		{"netflag", "SIG", "power"}, // best-effort:非地网 netflag 按 power 重建
		{"netport", "EN", "net_port_bi"},
		{"netlabel", "X", ""}, // connect_pin 无法重建 → 跳过
	}
	for _, tc := range cases {
		if got := tidyRestoreKind(tc.mtype, tc.net); got != tc.want {
			t.Errorf("tidyRestoreKind(%q,%q) = %q, want %q", tc.mtype, tc.net, got, tc.want)
		}
	}
}

// ── cobra 构造函数 ──────────────────────────────────────────────────────────

func TestNewSchGroupTidyCommand(t *testing.T) {
	var window string
	c := newSchGroupTidyCommand(&appConfig{}, &window, nil, nil)
	if c.Use != "tidy" {
		t.Errorf("Use = %q, want tidy", c.Use)
	}
	for _, f := range []string{"group", "pattern", "spacing", "dry-run", "apply"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("flag --%s missing", f)
		}
	}
	if got := c.Flags().Lookup("pattern").DefValue; got != "auto" {
		t.Errorf("--pattern default = %q, want auto", got)
	}
	if got := c.Flags().Lookup("spacing").DefValue; got != "50" {
		t.Errorf("--spacing default = %q, want 50", got)
	}
}

// TestTidyDeepSweepPlan: the sweep must collect the member's whole trees —
// including the dangling remnant that touches NO pin (the grey half-segment,
// live 2026-08-12) and every marker on those trees — while refusing a tree
// shared with a non-member pin (F2 semantics), and leaving unrelated trees alone.
func TestTidyDeepSweepPlan(t *testing.T) {
	comps := []layoutComp{
		{ID: "c3", Designator: "C3", ComponentType: "part", AnchorAvailable: true,
			BBox: &layoutBBox{MinX: 262, MinY: 425, MaxX: 279, MaxY: 445},
			Pins: []layoutPin{{Number: "1", X: 270, Y: 455}, {Number: "2", X: 270, Y: 415}}},
		{ID: "u9", Designator: "U9", ComponentType: "part", AnchorAvailable: true,
			Pins: []layoutPin{{Number: "1", X: 600, Y: 455}}},
		{ID: "fOld", ComponentType: "netflag", Net: "3V3", X: 270, Y: 505, AnchorAvailable: true}, // on C3's stub
		{ID: "fFar", ComponentType: "netflag", Net: "3V3", X: 800, Y: 300, AnchorAvailable: true}, // unrelated
	}
	wires := []schGroupWire{
		{ID: "wStub", Points: []float64{270, 455, 270, 505}}, // C3:1 stub (tree A)
		{ID: "wFrag", Points: []float64{270, 443, 270, 448}}, // dangling remnant grazing C3 bbox, no pin (tree B)
		{ID: "wElse", Points: []float64{800, 300, 840, 300}}, // unrelated tree
	}
	ids, err := tidyDeepSweepPlan(map[string]bool{"C3": true}, comps, wires)
	if err != nil {
		t.Fatalf("clean sweep errored: %v", err)
	}
	want := []string{"fOld", "wFrag", "wStub"}
	if len(ids) != len(want) {
		t.Fatalf("sweep ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("sweep ids = %v, want %v", ids, want)
		}
	}
	// Shared rail: extend the stub tree to also touch U9's pin → refuse.
	shared := append(wires, schGroupWire{ID: "wRail", Points: []float64{270, 505, 600, 505, 600, 455}})
	if _, err := tidyDeepSweepPlan(map[string]bool{"C3": true}, comps, shared); err == nil {
		t.Fatal("shared tree must be refused")
	}
}

// TestTidySkipHint: a two-flag cap with ONE flag present and the other pin bare
// (live 2026-08-12: C4:1/C6:2 disconnections were silently skipped as
// "already tidy") must surface an actionable reconnect hint; fully-flagged or
// fully-bare members stay hint-free.
func TestTidySkipHint(t *testing.T) {
	gnd := tidyPinConn{Pin: "2", Flag: "netflag", Net: "GND"}
	bare := tidyPinConn{Pin: "1"}
	if h := tidySkipHint("C4", []tidyPinConn{bare, gnd}); h == "" ||
		!strings.Contains(h, "C4:1") || !strings.Contains(h, "--kind power") {
		t.Fatalf("one-flag+bare must hint reconnect, got %q", h)
	}
	pwr := tidyPinConn{Pin: "1", Flag: "netflag", Net: "3V3"}
	if h := tidySkipHint("C4", []tidyPinConn{pwr, gnd}); h != "" {
		t.Fatalf("fully flagged must be hint-free, got %q", h)
	}
	if h := tidySkipHint("C4", []tidyPinConn{{Pin: "1"}, {Pin: "2"}}); h != "" {
		t.Fatalf("fully bare (never wired) must be hint-free, got %q", h)
	}
}
