package app

// cmd_sch_zone_move_test.go — `sch zone move` 纯函数表驱动单测(契约 §2):
// 展开集组装(组去重/散件纳入/跨区组拒绝/全区一份展开:区内直连线随区搬、
// 跨区布线留原地/框图元排除/文本纳入判定)+ 目的地预检几何(出界/压图签/
// 压他区)+ 文本搬移 JS + 重画前 settle 的 (id+bbox) 指纹判定。

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// --zone 解析已统一走 resolveLayoutZone,用例移植到 sch_layout_objects_test.go。

// ── partitionZoneMoveUnits:组去重 / 散件 / 跨区组 ──────────────────────────

func TestPartitionZoneMoveUnits(t *testing.T) {
	claimOf := map[string]string{
		"U2": "POWER", "C1": "POWER", "C2": "POWER", "R9": "POWER",
		"J1": "USB", "R5": "USB",
	}
	cases := []struct {
		name       string
		claimed    []string
		groups     []*schGroup
		wantGroups []string // group ids,按序
		wantLoose  []string
		wantErr    string // 空 = 期望成功
	}{
		{
			name:       "全员在区的组 + 剩余散件",
			claimed:    []string{"U2", "C1", "C2", "R9"},
			groups:     []*schGroup{mkGroup("g1", "ldo", "U2", "C1")},
			wantGroups: []string{"g1"},
			wantLoose:  []string{"C2", "R9"},
		},
		{
			name:       "同组重复出现只算一次(组去重)",
			claimed:    []string{"U2", "C1"},
			groups:     []*schGroup{mkGroup("g1", "", "U2", "C1"), mkGroup("g1", "", "U2", "C1")},
			wantGroups: []string{"g1"},
			wantLoose:  nil,
		},
		{
			name:       "零成员在区的组被忽略",
			claimed:    []string{"C2"},
			groups:     []*schGroup{mkGroup("g1", "", "J1", "R5")},
			wantGroups: nil,
			wantLoose:  []string{"C2"},
		},
		{
			name:    "跨区组(区外成员属于他区)→ 拒绝并点名归属",
			claimed: []string{"U2", "C1"},
			groups:  []*schGroup{mkGroup("g2", "mix", "U2", "J1")},
			wantErr: "USB",
		},
		{
			name:    "跨区组(区外成员未被认领)→ 拒绝",
			claimed: []string{"U2"},
			groups:  []*schGroup{mkGroup("g3", "", "U2", "X7")},
			wantErr: "未被任何区认领",
		},
		{
			name:       "认领重复位号只产出一个散件",
			claimed:    []string{"C2", "c2", " C2 "},
			groups:     nil,
			wantGroups: nil,
			wantLoose:  []string{"C2"},
		},
		{
			name:       "多组按 ID 排序",
			claimed:    []string{"U2", "C1", "C2", "R9"},
			groups:     []*schGroup{mkGroup("g5", "", "C2", "R9"), mkGroup("g1", "", "U2", "C1")},
			wantGroups: []string{"g1", "g5"},
			wantLoose:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			units, err := partitionZoneMoveUnits("POWER", tc.claimed, tc.groups, claimOf)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var gotIDs []string
			for _, g := range units.Groups {
				gotIDs = append(gotIDs, g.ID)
			}
			if strings.Join(gotIDs, ",") != strings.Join(tc.wantGroups, ",") {
				t.Errorf("groups = %v, want %v", gotIDs, tc.wantGroups)
			}
			if strings.Join(units.Loose, ",") != strings.Join(tc.wantLoose, ",") {
				t.Errorf("loose = %v, want %v", units.Loose, tc.wantLoose)
			}
		})
	}
}

// ── buildZoneMoveExpandInput:全区一份的展开输入 ────────────────────────────

func zmComp(id, desig, ctype string, pins ...[2]float64) layoutComp {
	c := layoutComp{ID: id, Designator: desig, ComponentType: ctype}
	for i, p := range pins {
		c.Pins = append(c.Pins, layoutPin{Number: string(rune('1' + i)), X: p[0], Y: p[1]})
	}
	if len(pins) > 0 {
		c.X, c.Y, c.AnchorAvailable = pins[0][0], pins[0][1], true
	}
	return c
}

func TestBuildZoneMoveExpandInput(t *testing.T) {
	comps := []layoutComp{
		zmComp("idR1", "R1", "part", [2]float64{100, 200}, [2]float64{100, 160}),
		zmComp("idU1", "U1", "part", [2]float64{300, 200}),
		zmComp("idF1", "F1", "netflag", [2]float64{100, 260}),
		zmComp("idSheet", "", "sheet"), // sheet 不入 pin 池
	}
	wires := []schGroupWire{{ID: "w1", Points: []float64{100, 200, 100, 260}}}
	in, missing := buildZoneMoveExpandInput([]string{"R1", "r1", " R9 "}, comps, wires)
	if strings.Join(missing, ",") != "R9" {
		t.Fatalf("missing = %v, want [R9]", missing)
	}
	if len(in.MemberPins) != 2 {
		t.Errorf("MemberPins = %v, want R1's 2 pins(位号大小写/空白归一,不重复计)", in.MemberPins)
	}
	if len(in.OtherPins) != 1 || in.OtherPins[0] != [2]float64{300, 200} {
		t.Errorf("OtherPins = %v, want only U1's pin(移动集之外的件)", in.OtherPins)
	}
	if len(in.Flags) != 1 || in.Flags[0].ID != "idF1" {
		t.Errorf("Flags = %v, want the netflag", in.Flags)
	}
	if len(in.Wires) != 1 || in.Wires[0].ID != "w1" {
		t.Errorf("Wires = %v, want the full-page wire list", in.Wires)
	}
}

// 全区展开语义走真展开:桩线+远端旗随区;终止在区外件 pin 的树 = SharedTrees
// 留原地。注意 stub 与跨区线必须从不同 pin 出发 —— 同点起步会被 union-find 并成
// 一棵树(平台把共点导线合并成一个电气树,正是 expandGroupAttachments 建模的语义)。
func TestZoneMoveExpansionStubAndFlag(t *testing.T) {
	comps := []layoutComp{
		zmComp("idR1", "R1", "part", [2]float64{100, 200}, [2]float64{100, 160}),
		zmComp("idU1", "U1", "part", [2]float64{300, 160}),
		zmComp("idF1", "F1", "netflag", [2]float64{100, 260}),
	}
	// stub:R1 pin1 → 旗;inter:R1 pin2 → 区外件 U1 pin(终止在区外 pin = 跨区布线)
	wires := []schGroupWire{
		{ID: "wStub", Points: []float64{100, 200, 100, 260}},
		{ID: "wInter", Points: []float64{100, 160, 300, 160}},
	}
	in, missing := buildZoneMoveExpandInput([]string{"R1"}, comps, wires)
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	exp := expandGroupAttachments(in)
	if strings.Join(exp.WireIDs, ",") != "wStub" {
		t.Errorf("WireIDs = %v, want [wStub] (cross-zone tree stays)", exp.WireIDs)
	}
	if strings.Join(exp.FlagIDs, ",") != "idF1" {
		t.Errorf("FlagIDs = %v, want [idF1]", exp.FlagIDs)
	}
	if exp.SharedTrees != 1 {
		t.Errorf("SharedTrees = %d, want 1 (the R1→U1 wire)", exp.SharedTrees)
	}
	if len(exp.Suspects) != 0 {
		t.Errorf("Suspects = %v, want none", exp.Suspects)
	}
}

// ── F1 回归:区内跨单元直连线必须随区刚移 ──────────────────────────────────
//
// 场景:区 POWER = 组{U2} + 散件 J1。wDirect 直连 U2↔J1(区内跨单元直连线),
// wCross 从 U2 拉到区外件 X9 的 pin(真实跨区布线)。契约:wDirect 随区搬
// (∈ WireIDs),只有 wCross 算 SharedTrees(=1)留在原地。
//
// 旧实现逐单元展开(组走 expandSchGroupForMove、散件走临时单件组输入),同区
// 另一单元的 pin 也算 foreign → wDirect 两边都判 shared 留在原地(刚体被撕开、
// 两端悬空断网),且同一棵树被两单元各计一次(实测旧路径:WireIDs=[]、
// SharedTrees=2/3,本测试红);全区一份展开(buildZoneMoveExpandInput,
// MemberPins=U2+J1 全部 pin)修复后绿。
func TestZoneMoveExpandIntraZoneDirectWire(t *testing.T) {
	comps := []layoutComp{
		zmComp("idU2", "U2", "part", [2]float64{100, 200}, [2]float64{140, 200}, [2]float64{100, 160}),
		zmComp("idJ1", "J1", "part", [2]float64{300, 200}),
		zmComp("idX9", "X9", "part", [2]float64{100, 40}),
	}
	wDirect := schGroupWire{ID: "wDirect", Points: []float64{140, 200, 300, 200}}
	wCross := schGroupWire{ID: "wCross", Points: []float64{100, 160, 100, 40}}
	cases := []struct {
		name        string
		wires       []schGroupWire
		wantWireIDs string
		wantShared  int
	}{
		{"区内跨单元直连线随区搬", []schGroupWire{wDirect}, "wDirect", 0},
		{"真跨区布线留原地", []schGroupWire{wCross}, "", 1},
		{"混合:直连随搬+跨区留守", []schGroupWire{wDirect, wCross}, "wDirect", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 生产路径:移动集 = 组{U2} 全部成员 + 散件 J1,全区一份展开输入,
			// expandGroupAttachments 只调一次(runSchZoneMove 的同款口径)。
			in, missing := buildZoneMoveExpandInput([]string{"U2", "J1"}, comps, tc.wires)
			if len(missing) != 0 {
				t.Fatalf("missing = %v", missing)
			}
			exp := expandGroupAttachments(in)
			if got := strings.Join(exp.WireIDs, ","); got != tc.wantWireIDs {
				t.Errorf("WireIDs = %v, want %q(区内直连线必须随区刚移)", exp.WireIDs, tc.wantWireIDs)
			}
			if exp.SharedTrees != tc.wantShared {
				t.Errorf("SharedTrees = %d, want %d(只有终止于区外件 pin 的树才算跨区,且一棵树只计一次)", exp.SharedTrees, tc.wantShared)
			}
			if len(exp.Suspects) != 0 {
				t.Errorf("Suspects = %v, want none", exp.Suspects)
			}
		})
	}
}

// 半移残骸(同线断触)在全区展开里同样必须被抓住 —— 与组预检同一判据。
func TestZoneMoveExpansionCatchesResidue(t *testing.T) {
	comps := []layoutComp{
		zmComp("idR1", "R1", "part", [2]float64{810, 475}),
	}
	// 残骸:同 y、同线,起点距 pin 10 单位(> eps 0.5,≤ nearTol 12),不接触。
	wires := []schGroupWire{{ID: "wGhost", Points: []float64{820, 475, 845, 475}}}
	in, _ := buildZoneMoveExpandInput([]string{"R1"}, comps, wires)
	exp := expandGroupAttachments(in)
	if len(exp.Suspects) != 1 || exp.Suspects[0].WireID != "wGhost" {
		t.Fatalf("Suspects = %+v, want the collinear-graze residue wGhost", exp.Suspects)
	}
}

// ── F2:move 后重画前的几何指纹(settle 的一致性判定)───────────────────────

func TestZoneMoveGeomFingerprint(t *testing.T) {
	b1 := &layoutBBox{MinX: 100, MinY: 200, MaxX: 140, MaxY: 260}
	b2 := &layoutBBox{MinX: 300, MinY: 200, MaxX: 340, MaxY: 260}
	a := []layoutComp{{ID: "idA", BBox: b1}, {ID: "idB", BBox: b2}, {ID: "idFlag"}} // idFlag 无 bbox
	// 读取顺序无关:同一份几何换序,指纹必须一致(double-read 判定的根基)。
	shuffled := []layoutComp{{ID: "idFlag"}, {ID: "idB", BBox: b2}, {ID: "idA", BBox: b1}}
	if zoneMoveGeomFingerprint(a) != zoneMoveGeomFingerprint(shuffled) {
		t.Fatal("同一份几何换序后指纹必须一致")
	}
	// bbox 变化(半更新快照:器件还在旧位)→ 指纹必须变。
	movedB := []layoutComp{{ID: "idA", BBox: b1},
		{ID: "idB", BBox: &layoutBBox{MinX: 400, MinY: 200, MaxX: 440, MaxY: 260}}, {ID: "idFlag"}}
	if zoneMoveGeomFingerprint(a) == zoneMoveGeomFingerprint(movedB) {
		t.Fatal("bbox 平移后指纹必须变化")
	}
	// id 集变化(文本/旗 recreate 出新 id)→ 指纹必须变。
	if zoneMoveGeomFingerprint(a) == zoneMoveGeomFingerprint(a[:2]) {
		t.Fatal("id 集变化后指纹必须变化")
	}
	// 有无 bbox 是不同状态(bbox 从缺到有 = 渲染信息刚就绪)。
	if zoneMoveGeomFingerprint([]layoutComp{{ID: "idA"}}) ==
		zoneMoveGeomFingerprint([]layoutComp{{ID: "idA", BBox: b1}}) {
		t.Fatal("无 bbox 与有 bbox 指纹必须不同")
	}
	if zoneMoveGeomFingerprint(nil) != "" {
		t.Fatal("空快照指纹应为空串")
	}
}

// ── 文本纳入判定 + 框图元排除 ───────────────────────────────────────────────

func TestZoneMoveExcludedTextIDs(t *testing.T) {
	page := &workflow.SchZoneFrames{Rects: []string{"r1"}, Texts: []string{"t1", "t2"}}
	legacy := &workflow.SchZoneFrames{Texts: []string{"t9"}}
	got := zoneMoveExcludedTextIDs(page, legacy, nil)
	for _, id := range []string{"r1", "t1", "t2", "t9"} {
		if !got[id] {
			t.Errorf("id %q should be excluded", id)
		}
	}
	if len(got) != 4 {
		t.Errorf("len = %d, want 4", len(got))
	}
	if n := len(zoneMoveExcludedTextIDs(nil, nil)); n != 0 {
		t.Errorf("all-nil: len = %d, want 0", n)
	}
}

func TestSelectZoneMoveTexts(t *testing.T) {
	zone := layoutBBox{MinX: 100, MinY: 100, MaxX: 300, MaxY: 300}
	texts := []zoneMoveText{
		{ID: "tIn", X: 150, Y: 250, Content: "LDO 5V→3V3"},
		{ID: "tEdge", X: 300, Y: 100, Content: "边界含"},
		{ID: "tOut", X: 400, Y: 250, Content: "他区说明"},
		{ID: "tFrame", X: 200, Y: 200, Content: "POWER (left-top)"}, // 框标题
		{ID: "", X: 150, Y: 150},                                    // 无 id 跳过
	}
	got := selectZoneMoveTexts(texts, zone, map[string]bool{"tFrame": true})
	var ids []string
	for _, t2 := range got {
		ids = append(ids, t2.ID)
	}
	if strings.Join(ids, ",") != "tEdge,tIn" {
		t.Fatalf("selected = %v, want [tEdge tIn] (sorted by id; frame + out-of-bbox + empty-id excluded)", ids)
	}
}

func TestZoneMoveInflate(t *testing.T) {
	got := zoneMoveInflate(layoutBBox{MinX: 10, MinY: 20, MaxX: 30, MaxY: 40}, 5)
	want := layoutBBox{MinX: 5, MinY: 15, MaxX: 35, MaxY: 45}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// ── union bbox ──────────────────────────────────────────────────────────────

func TestZoneMoveUnionBBox(t *testing.T) {
	if _, ok := zoneMoveUnionBBox(nil, nil); ok {
		t.Fatal("empty input must report ok=false")
	}
	u, ok := zoneMoveUnionBBox(
		[][2]float64{{50, 400}, {260, 90}},
		[]layoutBBox{{MinX: 100, MinY: 100, MaxX: 200, MaxY: 200}, {MinX: 150, MinY: 150, MaxX: 250, MaxY: 300}},
	)
	if !ok {
		t.Fatal("ok=false")
	}
	want := layoutBBox{MinX: 50, MinY: 90, MaxX: 260, MaxY: 400}
	if u != want {
		t.Fatalf("union = %+v, want %+v", u, want)
	}
}

// ── 目的地预检几何:出界 / 压图签 / 压他区 ──────────────────────────────────

func TestCheckZoneMoveDestination(t *testing.T) {
	sheet := &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	keepout := &layoutBBox{MinX: 912, MinY: 0, MaxX: 1170, MaxY: 115}
	current := layoutBBox{MinX: 100, MinY: 500, MaxX: 300, MaxY: 700}
	others := []zoneNamedBBox{
		{Name: "USB", BBox: layoutBBox{MinX: 500, MinY: 500, MaxX: 700, MaxY: 700}},
		{Name: "MCU", BBox: layoutBBox{MinX: 350, MinY: 100, MaxX: 800, MaxY: 400}},
	}
	cases := []struct {
		name         string
		dx, dy       float64
		offSheet     bool
		titleBlock   bool
		zoneOverlaps string
	}{
		{"原地小移动干净", 20, -20, false, false, ""},
		{"右移出 sheet", 900, 0, true, false, ""},
		{"下移压图签", 850, -450, false, true, ""},
		{"移进他区 → 警告项", 400, 0, false, false, "USB"},
		{"斜移同时压两区", 300, -150, false, false, "MCU,USB"},
		{"上移出 sheet 顶", 0, 200, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := checkZoneMoveDestination(current, tc.dx, tc.dy, sheet, keepout, others)
			if rep.OffSheet != tc.offSheet {
				t.Errorf("OffSheet = %v, want %v (moved %+v)", rep.OffSheet, tc.offSheet, rep.Moved)
			}
			if rep.TitleBlock != tc.titleBlock {
				t.Errorf("TitleBlock = %v, want %v (moved %+v)", rep.TitleBlock, tc.titleBlock, rep.Moved)
			}
			if got := strings.Join(rep.ZoneOverlaps, ","); got != tc.zoneOverlaps {
				t.Errorf("ZoneOverlaps = %q, want %q", got, tc.zoneOverlaps)
			}
		})
	}
	// sheet/keepout 缺失时对应检查跳过(不瞎猜硬拒)
	rep := checkZoneMoveDestination(current, 5000, 5000, nil, nil, nil)
	if rep.OffSheet || rep.TitleBlock {
		t.Fatalf("nil sheet/keepout must skip checks, got %+v", rep)
	}
	// 平移量本身要作用在 Moved 上
	rep = checkZoneMoveDestination(current, 10, -20, sheet, keepout, nil)
	want := layoutBBox{MinX: 110, MinY: 480, MaxX: 310, MaxY: 680}
	if rep.Moved != want {
		t.Fatalf("Moved = %+v, want %+v", rep.Moved, want)
	}
}

func TestZoneMoveOtherZoneBBoxes(t *testing.T) {
	zones := map[string]*schZoneClaim{
		"POWER": {Parts: []string{"U2"}},
		"USB":   {Parts: []string{"J1", "R5"}},
		"GHOST": {Parts: []string{"X9"}}, // 不在页上 → 无 bbox → 不参与
	}
	byDesig := map[string]layoutComp{
		"U2": {Designator: "U2", BBox: &layoutBBox{MinX: 0, MinY: 0, MaxX: 10, MaxY: 10}},
		"J1": {Designator: "J1", BBox: &layoutBBox{MinX: 100, MinY: 100, MaxX: 120, MaxY: 140}},
		"R5": {Designator: "R5", BBox: &layoutBBox{MinX: 200, MinY: 90, MaxX: 220, MaxY: 110}},
	}
	got := zoneMoveOtherZoneBBoxes(zones, "POWER", byDesig)
	if len(got) != 1 || got[0].Name != "USB" {
		t.Fatalf("others = %+v, want only USB", got)
	}
	want := layoutBBox{MinX: 100, MinY: 90, MaxX: 220, MaxY: 140}
	if got[0].BBox != want {
		t.Fatalf("USB union = %+v, want %+v", got[0].BBox, want)
	}
}

// ── 文本 list 解析 + 搬移 JS ────────────────────────────────────────────────

func TestParseZoneMoveTexts(t *testing.T) {
	result := map[string]any{"texts": []any{
		map[string]any{"primitiveId": "t1", "content": "LDO", "x": 100.0, "y": 200.0,
			"rotation": 90.0, "fontSize": 12.0, "color": "#5A5A5A"},
		map[string]any{"primitiveId": "", "x": 1.0, "y": 2.0},      // 无 id 跳过
		map[string]any{"primitiveId": "t3", "x": "oops", "y": 2.0}, // 坐标坏 → 跳过
		"not-a-map",
	}}
	got := parseZoneMoveTexts(result)
	if len(got) != 1 {
		t.Fatalf("parsed %d texts, want 1: %+v", len(got), got)
	}
	tx := got[0]
	if tx.ID != "t1" || tx.Content != "LDO" || tx.X != 100 || tx.Y != 200 ||
		tx.Rotation != 90 || tx.FontSize != 12 || tx.Color != "#5A5A5A" {
		t.Fatalf("parsed = %+v", tx)
	}
	if parseZoneMoveTexts(nil) != nil {
		t.Fatal("nil result must parse to nil")
	}
}

func TestBuildZoneMoveTextJS(t *testing.T) {
	js := buildZoneMoveTextJS(zoneMoveText{
		ID: "tOld", X: 100, Y: 200, Content: "LDO \"5V→3V3\"\n1A",
		Rotation: 90, FontSize: 12, Color: "#5A5A5A",
	}, 30, -50)
	for _, want := range []string{
		`const oldId = "tOld";`,
		"eda.sch_PrimitiveText.create(130, 150, ", // 坐标已平移
		`"LDO \"5V→3V3\"\n1A"`,                    // 内容 JSON 转义
		", 90, ",                                  // 旋转保留
		`"#5A5A5A"`,                               // 颜色保留
		", 12);",                                  // 字号保留
		"generic.delete([oldId])",                 // 旧 id 走 generic 删除(#164)
		"oldDeleted: !alive.has(oldId)",           // 删除复核
	} {
		if !strings.Contains(js, want) {
			t.Errorf("js missing %q\n---\n%s", want, js)
		}
	}
	// 空颜色/空字体 → null;fontSize<=0 → 默认 note 字号
	js = buildZoneMoveTextJS(zoneMoveText{ID: "t2", X: 0, Y: 0, Content: "x"}, 1, 1)
	if !strings.Contains(js, ", 0, null, null, 10);") {
		t.Errorf("empty color/font should emit null + default size 10:\n%s", js)
	}
}
