package app

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// componentsResult 造一份 `pcb.components.list --include-bbox --include-pads`
// 形状的响应（字段名照 extension/src/actions.ts serializePcbComponent/Pad）。
func componentsResult(comps ...map[string]any) map[string]any {
	items := make([]any, 0, len(comps))
	for _, c := range comps {
		items = append(items, c)
	}
	return map[string]any{"components": items, "count": float64(len(comps))}
}

func TestParseBoardComponents_FullPose(t *testing.T) {
	res := componentsResult(map[string]any{
		"primitiveId":    "p1",
		"designator":     "U1",
		"name":           "={Manufacturer Part}", // 未解析模板，必须让位给 manufacturerId
		"manufacturerId": "ESP32-S3-WROOM-1",
		"layer":          float64(1),
		"x":              float64(100.5),
		"y":              float64(200.25),
		"rotation":       float64(90),
		"locked":         true,
		"bbox":           map[string]any{"minX": 80.0, "minY": 180.0, "maxX": 120.0, "maxY": 220.0},
		"pads": []any{
			map[string]any{"primitiveId": "pad1", "padNumber": "1", "net": "GND", "layer": float64(1),
				"x": 85.0, "y": 185.0, "width": 20.0, "height": 12.0, "rotation": 0.0, "padType": float64(0)},
			map[string]any{"primitiveId": "pad2", "padNumber": "2", "net": "SDA", "layer": float64(12),
				"x": 115.0, "y": 215.0}, // polygon pad：连接器不给 width/height
		},
	})

	got := parseBoardComponents(res)
	if len(got) != 1 {
		t.Fatalf("want 1 component, got %d", len(got))
	}
	c := got[0]
	if c.ID != "p1" || c.Designator != "U1" {
		t.Errorf("identity: %+v", c)
	}
	// 关键回归：placed 件的 name 常是 "={…}" 模板，device 必须取 manufacturerId，
	// 否则所有按器件名的角色分类（RF/连接器/保护件）全部瞎。
	if c.Device != "ESP32-S3-WROOM-1" {
		t.Errorf("device = %q, want manufacturerId (not the ={…} template)", c.Device)
	}
	// #153 整齐度维度需要的姿态：过去 layout-lint 的解析把这三个全丢了。
	if c.Rotation != 90 {
		t.Errorf("rotation = %v, want 90", c.Rotation)
	}
	if !c.Locked {
		t.Error("locked should survive parsing")
	}
	if c.X != 100.5 || c.Y != 200.25 {
		t.Errorf("anchor = (%v,%v), want (100.5,200.25)", c.X, c.Y)
	}
	if cx, cy := c.center(); cx != 100 || cy != 200 {
		t.Errorf("center = (%v,%v), want bbox center (100,200) not the anchor", cx, cy)
	}
	if len(c.Pads) != 2 {
		t.Fatalf("want 2 pads, got %d", len(c.Pads))
	}
	// 测不到尺寸的焊盘必须保持 0（"未知"），绝不能猜一个值——猜出来的铜箔尺寸
	// 会让短路判定产生假阳/假阴。
	if c.Pads[1].W != 0 || c.Pads[1].H != 0 {
		t.Errorf("polygon pad extent should stay 0/unknown, got %v×%v", c.Pads[1].W, c.Pads[1].H)
	}
	if !c.Pads[1].isThroughHole() {
		t.Error("layer 12 pad should read as through-hole")
	}
}

func TestBoardComp_NetsSplitGlobal(t *testing.T) {
	c := boardComp{Designator: "U1", Pads: []boardPad{
		{Number: "1", Net: "GND"}, {Number: "2", Net: "3V3"},
		{Number: "3", Net: "SPI_CLK"}, {Number: "4", Net: "SPI_CLK"}, {Number: "5", Net: ""},
	}}
	if got := c.nets(); len(got) != 3 {
		t.Errorf("nets() = %v, want 3 distinct non-empty", got)
	}
	// 功能分区聚类只能看信号网：GND/3V3 连着半块板，拿它们聚会并成一坨。
	sig := c.signalNets()
	if len(sig) != 1 || sig[0] != "SPI_CLK" {
		t.Errorf("signalNets() = %v, want [SPI_CLK]", sig)
	}
}

func TestParseBoardOutline_PolygonPreferredOverBBox(t *testing.T) {
	// 连接器给了真实多边形 → Source 必须是 polygon（异形板的边距才算得准）。
	o := parseBoardOutline(map[string]any{
		"bbox": map[string]any{"minX": 0.0, "minY": 0.0, "maxX": 100.0, "maxY": 100.0},
		"points": []any{
			[]any{0.0, 0.0}, []any{100.0, 0.0}, []any{100.0, 100.0}, []any{0.0, 100.0},
		},
	})
	if o == nil || o.Source != "polygon" {
		t.Fatalf("want polygon source, got %+v", o)
	}
	if got := o.area(); math.Abs(got-10000) > 0.01 {
		t.Errorf("shoelace area = %v, want 10000", got)
	}
}

func TestParseBoardOutline_DegradesToBBox(t *testing.T) {
	// 旧连接器只给 bbox → 必须标 "bbox"，让下游知道自己在算 AABB 近似。
	o := parseBoardOutline(map[string]any{
		"bbox": map[string]any{"minX": 0.0, "minY": 0.0, "maxX": 200.0, "maxY": 50.0},
	})
	if o == nil || o.Source != "bbox" {
		t.Fatalf("want bbox source, got %+v", o)
	}
	if o.longAxis() != "x" {
		t.Errorf("longAxis = %q, want x for a 200×50 board", o.longAxis())
	}
	// 退化点列（<3 点）不该被当几何用。
	o2 := parseBoardOutline(map[string]any{
		"bbox":   map[string]any{"minX": 0.0, "minY": 0.0, "maxX": 10.0, "maxY": 10.0},
		"points": []any{[]any{0.0, 0.0}, []any{10.0, 10.0}},
	})
	if o2.Points != nil {
		t.Errorf("a 2-point outline is not a polygon, should be dropped: %v", o2.Points)
	}
}

func TestParseBoardOutline_NilWhenUnreadable(t *testing.T) {
	// PCB 不在前台时平台返 null —— 必须返回 nil 让调用方降级，
	// 而不是造一个 0×0 的板框把所有件判成 off-board。
	if o := parseBoardOutline(map[string]any{}); o != nil {
		t.Errorf("want nil outline for an empty result, got %+v", o)
	}
	if o := parseBoardOutline(nil); o != nil {
		t.Errorf("want nil outline for a nil result, got %+v", o)
	}
}

func TestBoardOutline_DistToEdge(t *testing.T) {
	o := &boardOutline{BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 500}, Source: "bbox"}
	// 板中心 → 到最近边（上下各 250）
	if d := o.distToEdge(500, 250); math.Abs(d-250) > 0.01 {
		t.Errorf("center dist = %v, want 250", d)
	}
	// 贴左边 → 小距离，且 nearestEdge 说得出是哪条边
	if d := o.distToEdge(20, 250); math.Abs(d-20) > 0.01 {
		t.Errorf("near-left dist = %v, want 20", d)
	}
	e, d := o.nearestEdge(20, 250)
	if e != edgeLeft || math.Abs(d-20) > 0.01 {
		t.Errorf("nearestEdge = (%v,%v), want (left,20)", e, d)
	}
	// 板外为负 —— off-board 与 "贴边" 必须可区分
	if d := o.distToEdge(-30, 250); d >= 0 {
		t.Errorf("outside point dist = %v, want negative", d)
	}
}

func TestBoardOutline_PolygonDistOnNotchedBoard(t *testing.T) {
	// Type-C 突出的板：AABB 会把突出根部的件判成"离边很远"，多边形才算得对。
	// 板形：1000×500 主体，右侧中段伸出 100 长的凸台。
	o := &boardOutline{
		BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 1100, MaxY: 500},
		Points: [][2]float64{
			{0, 0}, {1000, 0}, {1000, 200}, {1100, 200}, {1100, 300}, {1000, 300}, {1000, 500}, {0, 500},
		},
		Source: "polygon",
	}
	// (990, 400) 在主体右上区，离真实右边界只有 10mil。
	if d := o.distToEdge(990, 400); math.Abs(d-10) > 0.01 {
		t.Errorf("polygon dist = %v, want 10 (AABB would have said 110)", d)
	}
	// 同一点用 AABB 算会得到 110 —— 这正是必须区分 Source 的原因。
	aabb := &boardOutline{BBox: o.BBox, Source: "bbox"}
	if d := aabb.distToEdge(990, 400); math.Abs(d-100) > 1 {
		t.Errorf("AABB dist = %v, want ~100 (the approximation we degrade to)", d)
	}
}

func TestBoardSnapshot_ProjectsToExistingCores(t *testing.T) {
	// 打分复用 layout-lint / check 的纯核，靠的就是这两个投影；投影错了会让两套
	// 引擎对同一块板给出矛盾答案（这个项目踩过：netlist 引擎与 check 几何引擎长期打架）。
	snap := &boardSnapshot{Components: []boardComp{
		{Designator: "U1", Layer: 1, BBox: bb(0, 0, 40, 40), Pads: []boardPad{
			{Number: "1", Net: "A", Layer: 1, X: 5, Y: 5, W: 10, H: 10},
		}},
		{Designator: "C1", Layer: 2, BBox: bb(100, 100, 120, 120), Pads: []boardPad{
			{Number: "1", Net: "A", Layer: 2, X: 105, Y: 105},
		}},
	}}
	comps, pads := snap.toLayoutComps()
	if len(comps) != 2 || len(pads) != 2 {
		t.Fatalf("projection = %d comps / %d pads, want 2/2", len(comps), len(pads))
	}
	if comps[1].Layer != 2 {
		t.Errorf("assembly side must survive projection: %+v", comps[1])
	}
	if pads[0].Designator != "U1" || pads[0].W != 10 {
		t.Errorf("pad projection lost data: %+v", pads[0])
	}
	if cp := snap.toCheckPads(); len(cp) != 2 || cp[0].Net != "A" {
		t.Errorf("check-pad projection = %+v", cp)
	}
	// 同网器件索引：功能分区聚类的原料
	if m := snap.netMembers(); len(m["A"]) != 2 {
		t.Errorf("netMembers[A] = %v, want both U1 and C1", m["A"])
	}
	if np := snap.netPads(); len(np["A"]) != 2 || !strings.HasPrefix(np["A"][0].Number, "U1.") {
		t.Errorf("netPads should carry a designator-qualified pad id, got %+v", np["A"])
	}
}

func TestBoardSnapshot_RoundTripsThroughJSON(t *testing.T) {
	// fixture 承载能力的直接断言：dump → 文件 → load 必须无损，否则金标准回归
	// 拿到的板和真板不是同一块。
	orig := &boardSnapshot{
		Components: []boardComp{{
			ID: "p1", Designator: "J1", Device: "PH-3AW", Layer: 1,
			X: 10, Y: 20, Rotation: 270, Locked: true, BBox: bb(0, 0, 50, 30),
			Pads: []boardPad{{Number: "1", Net: "VBATT", Layer: 12, X: 5, Y: 5, W: 25, H: 25}},
		}},
		Outline:      &boardOutline{BBox: layoutBBox{MaxX: 1000, MaxY: 500}, Source: "bbox"},
		CopperLayers: 4,
		Rules:        &boardRules{ClearanceMil: 6, TrackWidthMil: 10, Source: "live"},
		Partial:      []string{"board outline is an AABB approximation"},
	}
	blob, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	back, err := loadBoardSnapshotFile(strings.NewReader(string(blob)))
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Components) != 1 {
		t.Fatalf("lost components: %+v", back)
	}
	c := back.Components[0]
	if c.Rotation != 270 || !c.Locked || c.Device != "PH-3AW" {
		t.Errorf("pose lost in round-trip: %+v", c)
	}
	if c.BBox == nil || c.BBox.MaxX != 50 {
		t.Errorf("bbox lost in round-trip: %+v", c.BBox)
	}
	if back.CopperLayers != 4 || back.Rules == nil || back.Rules.Source != "live" {
		t.Errorf("board context lost: layers=%d rules=%+v", back.CopperLayers, back.Rules)
	}
	if len(back.Partial) != 1 {
		t.Errorf("degradation notes must survive — they explain why a dimension scored what it did")
	}
}

func TestBoardSnapshot_NoteDedups(t *testing.T) {
	s := &boardSnapshot{}
	s.note("outline unavailable")
	s.note("outline unavailable")
	s.note("silk unreadable (%v)", "boom")
	if len(s.Partial) != 2 {
		t.Errorf("Partial = %v, want deduped to 2", s.Partial)
	}
}

func TestBoardRules_FallbackWhenAbsent(t *testing.T) {
	var nilRules *boardRules
	if got := nilRules.toPcbRules(); got.clearanceMil != defaultPcbRules().clearanceMil {
		t.Errorf("nil rules must degrade to the JLCPCB baseline, got %+v", got)
	}
}
