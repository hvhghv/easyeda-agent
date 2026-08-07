package app

import (
	"math"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

// fpBoard 造一块矩形板的快照。
func fpBoard(w, h float64, comps ...boardComp) *boardSnapshot {
	return &boardSnapshot{
		Components: comps,
		Outline:    &boardOutline{BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: w, MaxY: h}, Source: "bbox"},
	}
}

// fpComp 造一个带 bbox 的器件（中心 cx,cy，边长 size）。
func fpComp(des string, cx, cy, size float64) boardComp {
	h := size / 2
	return boardComp{
		Designator: des, Layer: 1, X: cx, Y: cy,
		BBox: bb(cx-h, cy-h, cx+h, cy+h),
	}
}

func fpSpec(t *testing.T, raw string) *spec.Spec {
	t.Helper()
	s, err := spec.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("bad test spec: %v", err)
	}
	return s
}

func TestPlanFloorplan_BandsFollowFlowOrder(t *testing.T) {
	s := fpSpec(t, `{
	  "flow": ["POWER","MCU","RF"],
	  "flowAxis": "x",
	  "modules": [
	    {"name":"PWR","kind":"POWER","parts":["U1"]},
	    {"name":"BRAIN","kind":"MCU","parts":["U2"]},
	    {"name":"RADIO","kind":"RF","parts":["U3"]}
	  ]
	}`)
	// 板上器件已经是左→右 POWER/MCU/RF，与声明同向。
	snap := fpBoard(6000, 2000,
		fpComp("U1", 800, 1000, 200),
		fpComp("U2", 3000, 1000, 200),
		fpComp("U3", 5200, 1000, 200),
	)
	rep := planFloorplan(s, snap, defaultFloorplanOpts())
	if !rep.OK || len(rep.Bands) != 3 {
		t.Fatalf("want 3 bands, got %+v", rep)
	}
	if rep.Axis != "x" {
		t.Errorf("axis = %q, want x", rep.Axis)
	}
	if rep.Reversed {
		t.Error("board already runs left→right; must NOT be flagged reversed")
	}
	// 带必须沿轴依次排列且不重叠
	for i := 1; i < len(rep.Bands); i++ {
		prev, cur := rep.Bands[i-1], rep.Bands[i]
		if cur.MinX < prev.MaxX-0.01 {
			t.Errorf("band %s overlaps %s: %.1f < %.1f", cur.Kind, prev.Kind, cur.MinX, prev.MaxX)
		}
	}
	if rep.Bands[0].Kind != "POWER" || rep.Bands[2].Kind != "RF" {
		t.Errorf("band order = %s..%s, want POWER..RF", rep.Bands[0].Kind, rep.Bands[2].Kind)
	}
}

func TestPlanFloorplan_DetectsReversedBoard(t *testing.T) {
	// 同一份 spec，但板上是右→左摆的。流向是相对的：这块板一样好，floorplan
	// 必须顺着它切带，而不是把整块板翻过来重排（那会让精修环每轮大幅搬件却不涨分）。
	s := fpSpec(t, `{
	  "flow": ["POWER","MCU","RF"], "flowAxis": "x",
	  "modules": [
	    {"name":"PWR","kind":"POWER","parts":["U1"]},
	    {"name":"BRAIN","kind":"MCU","parts":["U2"]},
	    {"name":"RADIO","kind":"RF","parts":["U3"]}
	  ]
	}`)
	snap := fpBoard(6000, 2000,
		fpComp("U1", 5200, 1000, 200), // POWER 在右
		fpComp("U2", 3000, 1000, 200),
		fpComp("U3", 800, 1000, 200), // RF 在左
	)
	rep := planFloorplan(s, snap, defaultFloorplanOpts())
	if !rep.Reversed {
		t.Fatal("a board laid out right→left must be detected as reversed")
	}
	// 反向时 POWER 带应该落在板子右侧
	var power, rf floorplanBand
	for _, b := range rep.Bands {
		switch b.Kind {
		case "POWER":
			power = b
		case "RF":
			rf = b
		}
	}
	if power.MinX <= rf.MinX {
		t.Errorf("reversed flow should put POWER right of RF, got POWER@%.0f RF@%.0f", power.MinX, rf.MinX)
	}
}

func TestPlanFloorplan_BandWidthFollowsArea(t *testing.T) {
	// #167 的动机之一：166 器件的 MCU 域和 3 器件的 ANT 域不该各占三分之一。
	s := fpSpec(t, `{
	  "flow": ["MCU","ANT"], "flowAxis": "x",
	  "modules": [
	    {"name":"BRAIN","kind":"MCU","parts":["U1","U2","U3"]},
	    {"name":"AERIAL","kind":"ANT","parts":["ANT1"]}
	  ]
	}`)
	snap := fpBoard(8000, 2000,
		fpComp("U1", 1000, 1000, 600),
		fpComp("U2", 2000, 1000, 600),
		fpComp("U3", 3000, 1000, 600),
		fpComp("ANT1", 7000, 1000, 100),
	)
	rep := planFloorplan(s, snap, defaultFloorplanOpts())
	var mcu, ant floorplanBand
	for _, b := range rep.Bands {
		if b.Kind == "MCU" {
			mcu = b
		} else {
			ant = b
		}
	}
	mw, aw := mcu.MaxX-mcu.MinX, ant.MaxX-ant.MinX
	if mw <= aw {
		t.Errorf("MCU band (%.0f) should be wider than ANT (%.0f) — width follows component area", mw, aw)
	}
	// 但小域不能被压没：MinBandMil 是地板
	if aw < defaultFloorplanOpts().MinBandMil-0.01 {
		t.Errorf("ANT band %.0f collapsed below the %.0f mil floor", aw, defaultFloorplanOpts().MinBandMil)
	}
}

func TestPlanFloorplan_TinyBoardWarnsInsteadOfNegativeBands(t *testing.T) {
	s := fpSpec(t, `{
	  "flow": ["POWER","MCU","RF","ANT"],
	  "modules": [
	    {"name":"A","kind":"POWER","parts":["U1"]},
	    {"name":"B","kind":"MCU","parts":["U2"]},
	    {"name":"C","kind":"RF","parts":["U3"]},
	    {"name":"D","kind":"ANT","parts":["U4"]}
	  ]
	}`)
	// 1200mil 宽的板，扣掉 2×300 边距只剩 600 —— 放不下 4 条 400mil 的带。
	snap := fpBoard(1200, 1200, fpComp("U1", 400, 400, 50))
	rep := planFloorplan(s, snap, defaultFloorplanOpts())
	if len(rep.Warnings) == 0 {
		t.Fatal("a board too small for the flow must warn, not silently produce cramped bands")
	}
	for _, b := range rep.Bands {
		if b.MaxX <= b.MinX || b.MaxY <= b.MinY {
			t.Errorf("degenerate band %+v — allocation must never emit zero/negative width", b)
		}
	}
}

func TestPlanFloorplan_NeedsFlowAndOutline(t *testing.T) {
	snap := fpBoard(4000, 2000, fpComp("U1", 500, 500, 100))
	// 没有 flow
	if rep := planFloorplan(fpSpec(t, `{"modules":[{"name":"A","kind":"MCU","parts":["U1"]}]}`), snap, defaultFloorplanOpts()); rep.OK {
		t.Error("planning without a flow must not report OK")
	}
	// 只有一段 flow（表达不了顺序）
	if rep := planFloorplan(fpSpec(t, `{"flow":["MCU"]}`), snap, defaultFloorplanOpts()); rep.OK {
		t.Error("a single-stage flow cannot express an order")
	}
	// 没有板框
	noOutline := &boardSnapshot{Components: snap.Components}
	rep := planFloorplan(fpSpec(t, `{"flow":["POWER","MCU"]}`), noOutline, defaultFloorplanOpts())
	if rep.OK {
		t.Error("planning without a board outline must not report OK")
	}
	if rep.Summary == "" {
		t.Error("a refusal must explain itself")
	}
}

func TestPlanFloorplan_ReportsUnzonedParts(t *testing.T) {
	// spec 只认领了一半的板 —— 必须报出来，因为 partition/flow-order 两维会跟着失真。
	s := fpSpec(t, `{
	  "flow": ["POWER","MCU"],
	  "modules": [{"name":"P","kind":"POWER","parts":["U1"]},{"name":"M","kind":"MCU","parts":["U2"]}]
	}`)
	snap := fpBoard(6000, 2000,
		fpComp("U1", 500, 1000, 100), fpComp("U2", 2000, 1000, 100),
		fpComp("C1", 3000, 1000, 50), fpComp("C2", 3500, 1000, 50),
		fpComp("C3", 4000, 1000, 50), fpComp("C4", 4500, 1000, 50),
	)
	rep := planFloorplan(s, snap, defaultFloorplanOpts())
	if len(rep.Unzoned) != 4 {
		t.Errorf("unzoned = %v, want the 4 unclaimed caps", rep.Unzoned)
	}
	found := false
	for _, w := range rep.Warnings {
		if len(w) > 0 && (w[0] == '4' || w[0] == '6') {
			found = true
		}
	}
	if !found {
		t.Errorf("more than half the board unzoned must warn, got %v", rep.Warnings)
	}
}

func TestPlanEdgePins_OnlyPinsExplicitSpecEdges(t *testing.T) {
	// 边序是装配体验，agent 猜不了（SKILL 停点表把它列为必须用户确认）。
	// 所以只钉 spec 明确给了 ref+edge 的接口，其余留给 place-constrained。
	s := fpSpec(t, `{
	  "flow": ["POWER","MCU"],
	  "interfaces": [
	    {"name":"USB","ref":"J2","edge":"bottom","facing":"user-facing"},
	    {"name":"BATT","ref":"J1","facing":"internal"},
	    {"name":"ANT","ref":"ANT1","edge":"any"},
	    {"name":"GHOST","ref":"J9","edge":"top"}
	  ]
	}`)
	snap := fpBoard(4000, 2000,
		fpComp("J1", 500, 500, 100), fpComp("J2", 2000, 200, 100), fpComp("ANT1", 3500, 1500, 100),
	)
	board := cpRect{x0: 300, y0: 300, x1: 3700, y1: 1700}
	pins := planEdgePins(s, snap, board)
	if len(pins) != 1 {
		t.Fatalf("want only J2 pinned (J1 has no edge, ANT1 is \"any\", J9 is not on the board), got %+v", pins)
	}
	if pins[0].Designator != "J2" || pins[0].Edge != "bottom" {
		t.Errorf("pin = %+v", pins[0])
	}
	if math.Abs(pins[0].TargetY-board.y0) > 0.01 {
		t.Errorf("bottom edge target Y = %.1f, want the board's low-Y margin %.1f (y-UP)", pins[0].TargetY, board.y0)
	}
	if pins[0].Source != "spec" {
		t.Errorf("source = %q, want spec — heuristic pins must be distinguishable", pins[0].Source)
	}
}

func TestFlowRunsReversed_NeedsTwoPopulatedStages(t *testing.T) {
	// 新板（只有一个阶段有器件）无从判断方向 —— 必须按正向切，不能瞎猜。
	stages := []*fpStage{
		{kind: "POWER", centroid: 100, area: 1},
		{kind: "MCU", centroid: math.NaN()},
		{kind: "RF", centroid: math.NaN()},
	}
	if flowRunsReversed(stages) {
		t.Error("with a single populated stage there is no evidence of direction")
	}
}
