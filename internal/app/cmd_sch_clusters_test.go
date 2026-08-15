package app

import "testing"

// ── L1 虚拟组(器件 + 它自己的 marker/桩线)────────────────────────────────

func clPart(desig string, minX, minY, maxX, maxY float64, pins ...[2]float64) layoutComp {
	c := layoutComp{ID: "p-" + desig, Designator: desig, ComponentType: "part",
		BBox: &layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}}
	for _, p := range pins {
		c.Pins = append(c.Pins, layoutPin{Number: "1", X: p[0], Y: p[1]})
	}
	return c
}

func clMarker(id string, x, y float64) layoutComp {
	return layoutComp{ID: id, ComponentType: "netport", X: x, Y: y,
		BBox: &layoutBBox{MinX: x - 30, MinY: y - 5, MaxX: x, MaxY: y + 5}}
}

// 归属走导线:marker 顺着自己的桩线找到宿主,**不看距离** —— 第一版按「最近引脚 +
// 90 半径」判,lane 错开把 marker 推到 248 远时直接判成无主,体积算小、判据失明。
func TestBuildSchClusters_FollowsTheStubWireNotTheDistance(t *testing.T) {
	comps := []layoutComp{
		clPart("U1", 400, 400, 500, 500, [2]float64{400, 450}),
		clMarker("m-far", 150, 450), // 离 U1 的脚 250 远,但桩线连着它
	}
	wires := []schGroupWire{{ID: "w1", Points: []float64{400, 450, 150, 450}}}

	cs, unowned := buildSchClusters(comps, wires)
	if unowned != 0 {
		t.Fatalf("沾着导线的 marker 不该算无主: %d", unowned)
	}
	if len(cs) != 1 || cs[0].Markers != 1 || cs[0].Wires != 1 {
		t.Fatalf("marker 与桩线都该归 U1: %+v", cs)
	}
	if cs[0].Box.MinX != 120 { // marker 的判定 bbox 左沿 = 150-30
		t.Errorf("组的体积必须含 marker: MinX=%v want 120", cs[0].Box.MinX)
	}
}

// 跨器件的连线是两组之间的走线通道,**不计入任何一组的体积**。
func TestBuildSchClusters_InterPartWireBelongsToNobody(t *testing.T) {
	comps := []layoutComp{
		clPart("U1", 400, 400, 500, 500, [2]float64{400, 450}),
		clPart("U2", 100, 400, 200, 500, [2]float64{200, 450}),
	}
	wires := []schGroupWire{{ID: "w1", Points: []float64{400, 450, 200, 450}}}

	cs, _ := buildSchClusters(comps, wires)
	for _, c := range cs {
		if c.Wires != 0 {
			t.Errorf("%s 不该把跨组走线算进自己的体积: %+v", c.Designator, c)
		}
		if c.Box != c.Body {
			t.Errorf("%s 的体积不该被跨组走线撑大: box=%v body=%v", c.Designator, c.Box, c.Body)
		}
	}
}

// 判定:体积相交 = ERROR;探出可用区 = ERROR;够不着 min-gap = WARN。
func TestJudgeSchClusters(t *testing.T) {
	cs := []schCluster{
		{Designator: "A", Box: layoutBBox{MinX: 0, MinY: 0, MaxX: 100, MaxY: 100}},
		{Designator: "B", Box: layoutBBox{MinX: 90, MinY: 0, MaxX: 200, MaxY: 100}},
		{Designator: "C", Box: layoutBBox{MinX: 205, MinY: 0, MaxX: 300, MaxY: 100}},
	}
	usable := &layoutBBox{MinX: 10, MinY: 0, MaxX: 400, MaxY: 400}
	got := judgeSchClusters(cs, usable, 20)

	var overlap, tight, off int
	for _, f := range got {
		switch f.Type {
		case "overlap":
			overlap++
			if f.Level != "ERROR" {
				t.Errorf("重叠必须是 ERROR: %+v", f)
			}
		case "tight":
			tight++ // B↔C 间隙 5 < 20
		case "out-of-sheet":
			off++ // A 左沿 0 < 10
			if f.A != "A" {
				t.Errorf("出图纸的该是 A: %+v", f)
			}
		}
	}
	if overlap != 1 || tight != 1 || off != 1 {
		t.Fatalf("overlap=%d tight=%d out-of-sheet=%d,期望各 1: %+v", overlap, tight, off, got)
	}
	// A↔C 分得很开,不该报任何东西。
	for _, f := range got {
		if (f.A == "A" && f.B == "C") || (f.A == "C" && f.B == "A") {
			t.Errorf("分开的两组不该出判定: %+v", f)
		}
	}
}
