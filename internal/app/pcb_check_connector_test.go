package app

// #168 两条连接器规则的离线单测。全部用结构体字面量喂纯函数，不连 daemon。
//
// 板框统一用 4000×2400 mil（≈101×61mm）的矩形，坐标 y-UP：底边 y=0、顶边 y=2400。

import (
	"strconv"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

// mkBoardConn 造一个居中于 (cx,cy)、渲染 bbox 为 w×h 的已放置连接器。
// 焊盘按 nets 顺序编号 1..n，坐标不重要（规则只看 bbox/中心/网名/脚数）。
func mkBoardConn(des, device string, layer int, cx, cy, w, h float64, nets ...string) boardComp {
	c := boardComp{
		ID: "p_" + des, Designator: des, Device: device, Layer: layer,
		X: cx, Y: cy,
		BBox: &layoutBBox{MinX: cx - w/2, MinY: cy - h/2, MaxX: cx + w/2, MaxY: cy + h/2},
	}
	for i, n := range nets {
		c.Pads = append(c.Pads, boardPad{
			Number: strconv.Itoa(i + 1), Net: n, Layer: layer, X: cx, Y: cy, W: 20, H: 20,
		})
	}
	return c
}

func testOutline() *boardOutline {
	return &boardOutline{
		BBox:   layoutBBox{MinX: 0, MinY: 0, MaxX: 4000, MaxY: 2400},
		Source: "polygon",
		Points: [][2]float64{{0, 0}, {4000, 0}, {4000, 2400}, {0, 2400}},
	}
}

// ── internal-on-edge ────────────────────────────────────────────────────────

// box-v2 rev-a 的实测场景：J1 是 PH2.0-3P 备份锂电池座（VBATT/GND/TS_NTC 接箱内
// 电芯），却和 Type-C 一起挤在底边外沿。
//
// 这条测试同时钉住 #168 最关键的设计：**启发式只报 INFO，spec 显式标注才报 WARN**。
func TestFindInternalOnEdge_SourceDrivesSeverity(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("J1", "PH2.0-3P", 1, 1000, 120, 300, 200, "VBATT", "GND", "TS_NTC"),
			mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 2000, 140, 360, 280, "VBUS", "GND", "CC1"),
		},
	}

	// (1) 没有 spec → 靠「线对板类别 + 无对外网」推定，只能 INFO。
	got := findInternalOnEdge(collectBoardConnectors(snap, nil), snap.Outline)
	if len(got) != 1 {
		t.Fatalf("internal-on-edge = %d, want 1 (J1 only; the Type-C is a real external port): %+v", len(got), got)
	}
	if got[0].Designator != "J1" {
		t.Fatalf("wrong offender %q, want J1", got[0].Designator)
	}
	if got[0].Level != "INFO" {
		t.Errorf("heuristic finding must be INFO (it can misjudge an XH socket that feeds an outside sensor); got %s", got[0].Level)
	}
	if !strings.Contains(got[0].Message, "internal=heuristic") {
		t.Errorf("finding must name its source so a reader can weigh it: %s", got[0].Message)
	}

	// (2) spec 显式声明 → 板级决定，升级成 WARN。
	s := &spec.Spec{Interfaces: []spec.Interface{{Name: "backup battery", Ref: "J1", Internal: true}}}
	got = findInternalOnEdge(collectBoardConnectors(snap, s), snap.Outline)
	if len(got) != 1 || got[0].Level != "WARN" {
		t.Fatalf("spec-declared internal must be WARN; got %+v", got)
	}
	if !strings.Contains(got[0].Message, "internal=spec") {
		t.Errorf("finding must record that the verdict came from the spec: %s", got[0].Message)
	}
	if !strings.Contains(got[0].Message, "规范 §3.5") {
		t.Errorf("finding must cite the design-rules section: %s", got[0].Message)
	}
}

// 内部件摆在板中央不是问题 —— 这条规则治的是「占了外沿」，不是「存在」。
func TestFindInternalOnEdge_InboardIsFine(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("J1", "PH2.0-3P", 1, 2000, 1200, 300, 200, "VBATT", "GND", "TS_NTC"),
		},
	}
	if got := findInternalOnEdge(collectBoardConnectors(snap, nil), snap.Outline); len(got) != 0 {
		t.Fatalf("a connector parked mid-board must not be flagged: %+v", got)
	}
}

// 同样是 XH 线对板座，只要挂着对外语义网（VIN）就不再被推定成 internal ——
// 启发式的两个条件必须同时成立。
func TestFindInternalOnEdge_ExternalNetDefeatsHeuristic(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("J3", "XH2.54-2P", 1, 500, 120, 300, 200, "VIN", "GND"),
		},
	}
	if got := findInternalOnEdge(collectBoardConnectors(snap, nil), snap.Outline); len(got) != 0 {
		t.Fatalf("an XH socket carrying VIN feeds the outside world — must not be called internal: %+v", got)
	}
}

// 没板框就没有「外沿」这个概念，规则必须闭嘴（由上层 skip 整维，而不是在这里瞎猜）。
func TestFindInternalOnEdge_NoOutline(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{
		mkBoardConn("J1", "PH2.0-3P", 1, 1000, 120, 300, 200, "VBATT", "GND"),
	}}
	if got := findInternalOnEdge(collectBoardConnectors(snap, nil), nil); got != nil {
		t.Fatalf("no outline → no verdict; got %+v", got)
	}
}

// ── connector-plug-clearance ────────────────────────────────────────────────

// box-v2 rev-a 底边：Type-C ↔ 车辆端子中心距 13mm。母座本体 ~9mm+~15mm 判不出问题，
// 插头护套（13mm）+ 端子本体（15mm）要求 14mm，13mm 就打架了。
func TestFindConnectorPlugClearance_PlugsCollideWhereFootprintsDont(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			// 512mil ≈ 13.0mm 中心距。
			mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 1000, 140, 360, 280, "VBUS", "GND", "CC1"),
			mkBoardConn("J_VEH", "KF301-5.0-3P", 1, 1512, 160, 600, 320, "VEH_12V", "GND", "ACC"),
		},
	}
	got := findConnectorPlugClearance(collectBoardConnectors(snap, nil), snap.Outline)
	if len(got) != 1 {
		t.Fatalf("plug-clearance = %d, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Level != "WARN" || f.Type != "connector-plug-clearance" {
		t.Errorf("unexpected shape: %+v", f)
	}
	// 两侧都必须查到表，否则这条 WARN 的说服力就只是 bbox 估算。
	if !strings.Contains(f.Message, "[table:type-c]") || !strings.Contains(f.Message, "[table:kf301]") {
		t.Errorf("message must attribute each width to its source: %s", f.Message)
	}
	if strings.Contains(f.Message, "plug-width=fallback") {
		t.Errorf("both widths came from the table — must not be tagged fallback: %s", f.Message)
	}
	if len(f.Primitives) != 2 {
		t.Errorf("both components should be addressable from the finding: %+v", f.Primitives)
	}

	// 拉开到 40mm 就没问题了 —— 阈值必须真的随距离生效，不是恒报。
	snap.Components[1] = mkBoardConn("J_VEH", "KF301-5.0-3P", 1, 2575, 160, 600, 320, "VEH_12V", "GND", "ACC")
	if got := findConnectorPlugClearance(collectBoardConnectors(snap, nil), snap.Outline); len(got) != 0 {
		t.Fatalf("40mm apart must be clean: %+v", got)
	}
}

// 分处两条对边的口，插头朝相反方向，中心距再近也插得进去 —— 不能报。
func TestFindConnectorPlugClearance_OppositeEdgesDontFight(t *testing.T) {
	// 一块窄板：上下边相距只有 500mil。
	o := &boardOutline{BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 4000, MaxY: 500}, Source: "bbox"}
	snap := &boardSnapshot{
		Outline: o,
		Components: []boardComp{
			mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 1000, 60, 360, 120, "VBUS", "GND"),
			mkBoardConn("J2", "KF301-5.0-2P", 1, 1000, 440, 400, 120, "VOUT", "GND"),
		},
	}
	if got := findConnectorPlugClearance(collectBoardConnectors(snap, nil), o); len(got) != 0 {
		t.Fatalf("connectors on opposite edges face away from each other: %+v", got)
	}
}

// 异面（顶层/底层）连接器的插头在 Z 向被板厚错开，侧向重叠不算干涉。
func TestFindConnectorPlugClearance_DifferentAssemblySides(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 1000, 140, 360, 280, "VBUS", "GND"),
			mkBoardConn("J2", "KF301-5.0-3P", 2, 1100, 160, 600, 320, "VOUT", "GND"),
		},
	}
	if got := findConnectorPlugClearance(collectBoardConnectors(snap, nil), snap.Outline); len(got) != 0 {
		t.Fatalf("top-side vs bottom-side plugs are offset in Z: %+v", got)
	}
}

// 查不到包络表时兜底走 bbox+2mm，finding 必须自曝 fallback —— 否则读的人无从判断
// 这条该不该信。
func TestFindConnectorPlugClearance_FallbackWidthIsTagged(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 1000, 140, 360, 280, "VBUS", "GND"),
			mkBoardConn("J9", "WEIRD-CONN-4P", 1, 1300, 160, 200, 200, "SIG1", "GND"),
		},
	}
	got := findConnectorPlugClearance(collectBoardConnectors(snap, nil), snap.Outline)
	if len(got) != 1 {
		t.Fatalf("plug-clearance = %d, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "plug-width=fallback") {
		t.Errorf("an estimated width must be labelled: %s", got[0].Message)
	}
}

// spec 里人工写的 plugWidthMm 压过查找表 —— 板级决定 > 类别经验。
func TestConnPlugWidthPrefersSpecOverride(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 1000, 140, 360, 280, "VBUS", "GND"),
		},
	}
	s := &spec.Spec{Interfaces: []spec.Interface{{Ref: "USB1", PlugWidthMM: 25}}}
	conns := collectBoardConnectors(snap, s)
	if len(conns) != 1 {
		t.Fatalf("want 1 connector, got %d", len(conns))
	}
	if conns[0].plugSrc != "spec" {
		t.Errorf("plug width source = %q, want spec", conns[0].plugSrc)
	}
	if got, want := conns[0].plugMil, 25*mmToMil; got != want {
		t.Errorf("plug width = %v mil, want %v", got, want)
	}
}

// ── 连接器识别 ──────────────────────────────────────────────────────────────

// 识别必须与 classifyCP/edgeRoleOf 同口径，尤其是 `J_VEH` 这种不带数字的位号 ——
// connectorDesRe(^J\d) 覆盖不到它，而它正是 #168 的主要目标器件。
func TestIsBoardConnector(t *testing.T) {
	cases := []struct {
		comp boardComp
		want bool
		why  string
	}{
		{boardComp{Designator: "J_VEH", Device: "KF301-5.0-3P"}, true, "digit-less J designator"},
		{boardComp{Designator: "J1", Device: "PH2.0-3P"}, true, "plain J"},
		{boardComp{Designator: "JP701", Device: "HDR-2.54-1x2P"}, false, "JP is a jumper, not an external port"},
		{boardComp{Designator: "USB1", Device: "TYPE-C-31-M-12"}, true, "USB prefix"},
		{boardComp{Designator: "U3", Device: "MICRO-SD-PUSHPUSH"}, true, "connector footprint under a U designator"},
		{boardComp{Designator: "C12", Device: "0402 100nF"}, false, "passive"},
		{boardComp{Designator: "ANT1", Device: "2.4G CERAMIC ANTENNA"}, false, "a chip antenna is not a connector"},
	}
	for _, c := range cases {
		if got := isBoardConnector(c.comp); got != c.want {
			t.Errorf("isBoardConnector(%s/%s) = %v, want %v (%s)", c.comp.Designator, c.comp.Device, got, c.want, c.why)
		}
	}
}

// ── connector-mating-blocked（插拔通道禁布）─────────────────────────────────

// 走廊几何：块库声明了开口方向（kf301 局部 -y）才算得出走廊；旋转参与换算。
func TestConnectorMatingCorridor_BlockDeclaredDirection(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			// 板中的 KF301：bbox x[1800,2200] y[1100,1300]，rot 0 → 开口 -y。
			mkBoardConn("J1", "KF301-5.0-2P", 1, 2000, 1200, 400, 200, "VOUT", "GND"),
		},
	}
	conns := collectBoardConnectors(snap, nil)
	if len(conns) != 1 {
		t.Fatalf("want 1 connector, got %d", len(conns))
	}
	cor, ok := connectorMatingCorridor(conns[0], snap.Outline)
	if !ok {
		t.Fatal("a block-declared opening must yield a corridor")
	}
	want := layoutBBox{MinX: 1800, MinY: 1100 - pcbMatingCorridorDepthMil, MaxX: 2200, MaxY: 1100}
	if cor != want {
		t.Fatalf("corridor = %+v, want %+v (in front of the -y opening face, bbox-wide)", cor, want)
	}

	// 转 90°（CCW）后局部 -y 指向世界 +x → 走廊在 bbox 右侧。
	snap.Components[0].Rotation = 90
	conns = collectBoardConnectors(snap, nil)
	cor, ok = connectorMatingCorridor(conns[0], snap.Outline)
	if !ok {
		t.Fatal("rotated connector must still yield a corridor")
	}
	want = layoutBBox{MinX: 2200, MinY: 1100, MaxX: 2200 + pcbMatingCorridorDepthMil, MaxY: 1300}
	if cor != want {
		t.Fatalf("rot-90 corridor = %+v, want %+v (opening now faces +x)", cor, want)
	}
}

// 贴边件没有块声明 → 按「朝板外」推定,通道在板外 —— 不产出板上走廊。
// 这正是绝大多数摆对了的边缘连接器:它们永远不会因此被误报。
func TestConnectorMatingCorridor_EdgePresumedOutwardNoCorridor(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			// Type-C 贴底边(edgeGap 0),块库没有它的开口声明。
			mkBoardConn("USB1", "TYPE-C-31-M-12", 1, 2000, 140, 360, 280, "VBUS", "GND"),
		},
	}
	conns := collectBoardConnectors(snap, nil)
	if _, ok := connectorMatingCorridor(conns[0], snap.Outline); ok {
		t.Fatal("an edge connector without a declared opening is presumed to face off-board — no on-board corridor")
	}
}

// 非贴边、又没有块声明(板中 FPC 座/卡座) → 方向判不了,绝不猜。
func TestConnectorMatingCorridor_UnknownDirectionRefusesToGuess(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("J9", "WEIRD-FPC-CONN-8P", 1, 2000, 1200, 300, 100, "SIG", "GND"),
		},
	}
	conns := collectBoardConnectors(snap, nil)
	if _, ok := connectorMatingCorridor(conns[0], snap.Outline); ok {
		t.Fatal("no block declaration and not on the rim — direction is unknowable, must not guess")
	}
}

// 走廊裁剪到板框:开口贴着板边朝外时走廊全在板外,裁完退化 → ok=false。
func TestConnectorMatingCorridor_ClippedAwayOffBoard(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			// KF301 齐贴底边:bbox y[0,200],开口 -y → 走廊 y[-250,0] 全在板外。
			mkBoardConn("J1", "KF301-5.0-2P", 1, 2000, 100, 400, 200, "VOUT", "GND"),
		},
	}
	conns := collectBoardConnectors(snap, nil)
	if cor, ok := connectorMatingCorridor(conns[0], snap.Outline); ok {
		t.Fatalf("a flush outward-facing opening has its whole corridor off-board — must clip to nothing, got %+v", cor)
	}
	// 因此也不可能有遮挡 finding。
	if got := findConnectorMatingBlocked(conns, snap.Components, snap.Outline); len(got) != 0 {
		t.Fatalf("no corridor → no blockers: %+v", got)
	}
}

// 遮挡判定:走廊里的器件每件一条 WARN,归因落在**遮挡件**(动它才能解决)。
func TestFindConnectorMatingBlocked(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			// 板中 KF301,走廊 {1800, 850, 2200, 1100}。
			mkBoardConn("J1", "KF301-5.0-2P", 1, 2000, 1200, 400, 200, "VOUT", "GND"),
			// C1 的焊盘并集(20×20 @ (2000,1000))正落在走廊里。
			mkBoardConn("C1", "0402 100nF", 1, 2000, 1000, 40, 20, "VOUT", "GND"),
			// C2 在走廊外 —— 不报。
			mkBoardConn("C2", "0402 100nF", 1, 3000, 1000, 40, 20, "3V3", "GND"),
		},
	}
	conns := collectBoardConnectors(snap, nil)
	got := findConnectorMatingBlocked(conns, snap.Components, snap.Outline)
	if len(got) != 1 {
		t.Fatalf("mating-blocked = %d, want 1 (C1 only): %+v", len(got), got)
	}
	f := got[0]
	if f.Type != "connector-mating-blocked" || f.Level != "WARN" {
		t.Errorf("unexpected shape: %+v", f)
	}
	if f.Designator != "C1" {
		t.Errorf("attribution must land on the BLOCKER (moving it is the fix); got %q", f.Designator)
	}
	if !strings.Contains(f.Message, "J1") {
		t.Errorf("message must name the connector whose corridor is blocked: %s", f.Message)
	}
	if len(f.Primitives) != 2 {
		t.Errorf("both the blocker and the connector should be addressable: %+v", f.Primitives)
	}
	if !strings.Contains(f.Message, "规范 §3.5") {
		t.Errorf("finding must cite the design-rules section: %s", f.Message)
	}
}

// 异面器件不遮挡:插头路径在 Z 向被板厚隔开(与 plug-clearance 的收窄同理)。
func TestFindConnectorMatingBlocked_DifferentSideDoesNotBlock(t *testing.T) {
	snap := &boardSnapshot{
		Outline: testOutline(),
		Components: []boardComp{
			mkBoardConn("J1", "KF301-5.0-2P", 1, 2000, 1200, 400, 200, "VOUT", "GND"),
			mkBoardConn("C1", "0402 100nF", 2, 2000, 1000, 40, 20, "VOUT", "GND"), // bottom side
		},
	}
	conns := collectBoardConnectors(snap, nil)
	if got := findConnectorMatingBlocked(conns, snap.Components, snap.Outline); len(got) != 0 {
		t.Fatalf("a bottom-side part cannot block a top-side plug path: %+v", got)
	}
}

// 同号并联焊盘（USB-C 双取向）只算一个引脚位 —— 排式连接器的胶壳宽跟位数走。
func TestConnectorPinsDedupes(t *testing.T) {
	c := boardComp{Designator: "J1", Pads: []boardPad{
		{Number: "1"}, {Number: "1"}, {Number: "2"}, {Number: "3"}, {Number: ""},
	}}
	if got := connectorPins(c); got != 3 {
		t.Errorf("pins = %d, want 3", got)
	}
}
