package app

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// TestBuildZoneDrawJS pins the generated script: deterministic module order,
// inset dashed rects, y-UP rectangle anchor, and label inside the top-left.
func TestBuildZoneDrawJS(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 900, MaxY: 600}
	zones := map[string]*schZoneClaim{
		"POWER": {Zone: "left-top", Parts: []string{"U3"}},
		"MCU":   {Zone: "center", Parts: []string{"U1"}},
		"BAD":   {Zone: "nope", Parts: []string{"X1"}}, // unknown zone → skipped
	}
	js := buildZoneDrawJS(zones, sheet, "#AA00AA")
	if !strings.Contains(js, `"MCU (center)"`) || !strings.Contains(js, `"POWER (left-top)"`) {
		t.Errorf("labels missing:\n%s", js)
	}
	if strings.Contains(js, "BAD") {
		t.Error("unknown zone was not skipped")
	}
	// MCU (center, full height) target bbox is [304,4 → 596,596].
	// Rectangle.create takes its TOP-LEFT at (MinX, MaxY), then extends toward -y.
	if !strings.Contains(js, "create(304, 596, 292, 592, 0, 0, \"#AA00AA\", null, 1, 1)") {
		t.Errorf("MCU rect geometry wrong:\n%s", js)
	}
	// Deterministic order: MCU before POWER (sorted).
	if strings.Index(js, "MCU") > strings.Index(js, "POWER") {
		t.Error("modules not emitted in sorted order")
	}
	if !strings.Contains(js, "return {rects, texts};") {
		t.Error("script must return the created ids")
	}
}

var zoneRectangleCreateRE = regexp.MustCompile(
	`sch_PrimitiveRectangle\.create\(([-+0-9.eE]+), ([-+0-9.eE]+), ([-+0-9.eE]+), ([-+0-9.eE]+),`,
)

// renderedZoneRectangleBBoxes reconstructs SDK readback geometry from generated
// create(topLeftX, topLeftY, width, height) calls. On the y-UP canvas the rendered
// bbox is [x, topY-height → x+width, topY].
func renderedZoneRectangleBBoxes(t *testing.T, js string) []layoutBBox {
	t.Helper()
	matches := zoneRectangleCreateRE.FindAllStringSubmatch(js, -1)
	out := make([]layoutBBox, 0, len(matches))
	for _, m := range matches {
		var values [4]float64
		for i := range values {
			v, err := strconv.ParseFloat(m[i+1], 64)
			if err != nil {
				t.Fatalf("parse rectangle arg %q: %v\njs:\n%s", m[i+1], err, js)
			}
			values[i] = v
		}
		x, topY, w, h := values[0], values[1], values[2], values[3]
		out = append(out, layoutBBox{MinX: x, MinY: topY - h, MaxX: x + w, MaxY: topY})
	}
	return out
}

func requireZoneBBoxEqual(t *testing.T, got, want layoutBBox) {
	t.Helper()
	const eps = 1e-9
	if math.Abs(got.MinX-want.MinX) > eps ||
		math.Abs(got.MinY-want.MinY) > eps ||
		math.Abs(got.MaxX-want.MaxX) > eps ||
		math.Abs(got.MaxY-want.MaxY) > eps {
		t.Fatalf("rendered bbox = %+v, want %+v", got, want)
	}
}

// Both fixed-grid and partition modes must go through the same SDK rectangle
// semantics. This catches the old fixed-mode bug where MinY was passed as the
// top-left y and the rendered frame dropped one full height below its target.
func TestZoneDrawRectangleSemanticsSharedByFixedAndPartition(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 900, MaxY: 600}
	fixedJS := buildZoneDrawJS(map[string]*schZoneClaim{
		"IO": {Zone: "right-bottom", Parts: []string{"J1"}},
	}, sheet, "#AA00AA")
	fixedBoxes := renderedZoneRectangleBBoxes(t, fixedJS)
	if len(fixedBoxes) != 1 {
		t.Fatalf("fixed mode emitted %d rectangles, want 1\n%s", len(fixedBoxes), fixedJS)
	}
	target := layoutBBox{MinX: 604, MinY: 4, MaxX: 896, MaxY: 296}
	requireZoneBBoxEqual(t, fixedBoxes[0], target)

	partitionJS := buildPartitionDrawJS(partitionPlan{Partitions: []partitionRect{{
		Modules:   []string{"IO"},
		BBox:      target,
		TitleBBox: layoutBBox{MinX: 604, MinY: 266, MaxX: 896, MaxY: 296},
	}}}, 22, "#AA00AA")
	partitionBoxes := renderedZoneRectangleBBoxes(t, partitionJS)
	if len(partitionBoxes) != 1 {
		t.Fatalf("partition mode emitted %d rectangles, want 1\n%s", len(partitionBoxes), partitionJS)
	}
	requireZoneBBoxEqual(t, partitionBoxes[0], target)
	requireZoneBBoxEqual(t, fixedBoxes[0], partitionBoxes[0])
}

// A bottom-right partition is lifted above the title-block keep-out by the
// planner. The generated SDK call must render that exact bbox; using MinY as the
// top-left y would drop it across the title-block and below the sheet.
func TestPartitionDrawRenderedBBoxStaysClearOfTitleBlock(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	keepout := layoutBBox{MinX: 468, MinY: 0, MaxX: 1170, MaxY: 165}
	modules := []partitionModule{{
		Name: "IO",
		BBox: layoutBBox{MinX: 900, MinY: 200, MaxX: 1000, MaxY: 260},
	}}
	plan := planPartitions(sheet, &keepout, modules, defaultPartitionOpts())
	if len(plan.Partitions) != 1 {
		t.Fatalf("planner produced %d partitions, want 1: %+v", len(plan.Partitions), plan)
	}
	if !plan.Validation.clean() {
		t.Fatalf("planner validation not clean: %+v", plan.Validation)
	}

	js := buildPartitionDrawJS(plan, 22, "#AA00AA")
	rendered := renderedZoneRectangleBBoxes(t, js)
	if len(rendered) != 1 {
		t.Fatalf("draw emitted %d rectangles, want 1\n%s", len(rendered), js)
	}
	requireZoneBBoxEqual(t, rendered[0], plan.Partitions[0].BBox)
	if !bboxContains(sheet, rendered[0]) {
		t.Errorf("rendered partition escaped sheet: sheet=%+v rendered=%+v", sheet, rendered[0])
	}
	if boxesOverlap(rendered[0], keepout) {
		t.Errorf("rendered partition crossed title-block keep-out: partition=%+v keepout=%+v", rendered[0], keepout)
	}
}

func TestBuildZoneClearJS(t *testing.T) {
	js := buildZoneClearJS(&workflow.SchZoneFrames{Rects: []string{"r1"}, Texts: []string{"t1", "t2"}})
	if !strings.Contains(js, `["r1"]`) || !strings.Contains(js, `["t1","t2"]`) {
		t.Errorf("ids not embedded:\n%s", js)
	}
}
