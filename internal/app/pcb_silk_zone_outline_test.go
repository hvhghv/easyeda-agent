package app

import "testing"

func TestOrthogonalEnvelopeMakesStairStep(t *testing.T) {
	rects := []cpRect{
		{x0: 0, y0: 0, x1: 100, y1: 100},
		{x0: 80, y0: 100, x1: 220, y1: 180},
	}
	hull, bb, err := orthogonalEnvelope(rects, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hull) < 7 {
		t.Fatalf("expected non-rectangular stair-step hull, got %v", hull)
	}
	if bb.x0 != -10 || bb.y0 != -10 || bb.x1 != 230 || bb.y1 != 190 {
		t.Fatalf("unexpected bbox: %+v", bb)
	}
	if got := outlinePolygons(hull, bb, 6); len(got) == 0 {
		t.Fatal("expected grouped outline polygons")
	}
}

func TestParseSilkZoneFlags(t *testing.T) {
	got, err := parseSilkZoneFlags([]string{"POWER=U1, c1,C1", "RF=U7"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["POWER"]) != 2 || got["POWER"][0] != "C1" || got["POWER"][1] != "U1" {
		t.Fatalf("unexpected normalized refs: %#v", got)
	}
}

func TestAvoidZoneObstaclesBendsAroundCrossedPart(t *testing.T) {
	members := []cpRect{{x0: 0, y0: 0, x1: 100, y1: 100}}
	all := map[string]silkZonePart{
		"U1": {Designator: "U1", Rect: members[0]},
		"J1": {Designator: "J1", Rect: cpRect{x0: 90, y0: 30, x1: 140, y1: 70}},
	}
	hull, bb, avoided, blocked, err := avoidZoneObstacles(members, map[string]bool{"U1": true}, all, 10, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 0 || len(avoided) != 1 || avoided[0] != "J1" {
		t.Fatalf("unexpected avoidance: avoided=%v blocked=%v", avoided, blocked)
	}
	if bb.x1 < 150 || len(hull) < 5 {
		t.Fatalf("outline did not move outside obstacle: bbox=%+v hull=%v", bb, hull)
	}
}

func TestOrthogonalPerimeterStaysOnOuterBBox(t *testing.T) {
	rects := []cpRect{
		{x0: 0, y0: 0, x1: 100, y1: 100},
		{x0: 200, y0: 200, x1: 300, y1: 300},
	}
	hull, bb, err := orthogonalPerimeter(rects, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(hull) != 21 || bb.x0 != 0 || bb.y0 != 0 || bb.x1 != 300 || bb.y1 != 300 {
		t.Fatalf("unexpected perimeter: bbox=%+v hull=%v", bb, hull)
	}
	// No point may sit in the central member-to-member gap on both axes.
	for _, p := range hull {
		if p[0] > 30 && p[0] < 270 && p[1] > 30 && p[1] < 270 {
			t.Fatalf("perimeter entered group interior: %v", p)
		}
	}
}

func TestAvoidPerimeterMovesOuterEdgePastObstacle(t *testing.T) {
	members := []cpRect{{x0: 0, y0: 0, x1: 100, y1: 100}}
	all := map[string]silkZonePart{
		"U1": {Designator: "U1", Rect: members[0]},
		"J1": {Designator: "J1", Rect: cpRect{x0: 20, y0: 90, x1: 80, y1: 140}},
	}
	_, bb, avoided, blocked, err := avoidPerimeterObstacles(members, map[string]bool{"U1": true}, all, 10, 20, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 0 || len(avoided) != 1 || bb.y1 < 150 {
		t.Fatalf("perimeter did not move past obstacle: bbox=%+v avoided=%v blocked=%v", bb, avoided, blocked)
	}
}

func TestChooseZoneLabelAvoidsCenteredObstacle(t *testing.T) {
	bb := cpRect{x0: 0, y0: 0, x1: 400, y1: 200}
	board := cpRect{x0: -100, y0: -100, x1: 500, y1: 500}
	parts := map[string]silkZonePart{
		"R1": {Designator: "R1", Rect: cpRect{x0: 120, y0: 200, x1: 280, y1: 270}},
	}
	label := chooseZoneLabel("USB_DATA", bb, board, parts, 32, 5, 10)
	if !label.Clear || label.Placement == "fallback" {
		t.Fatalf("expected a clear scanned label position, got %+v", label)
	}
	lr := cpRect{x0: label.X, y0: label.Y, x1: label.X + float64(len([]rune(label.Text)))*32*0.58, y1: label.Y + 32*1.4}
	if rectsOverlap(lr, parts["R1"].Rect) {
		t.Fatalf("label still overlaps obstacle: %+v", label)
	}
}
