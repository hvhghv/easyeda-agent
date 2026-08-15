package app

import (
	"encoding/json"
	"io"
	"math"
	"strings"
	"testing"
)

func bb(minX, minY, maxX, maxY float64) *layoutBBox {
	return &layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
}

func TestSchematicUnitConversion(t *testing.T) {
	if got := mmToSchematicUnits(2.54); got != 10 {
		t.Fatalf("2.54mm must equal 10 schematic units, got %.6f", got)
	}
	if got := schematicUnitsToMM(10); got != 2.54 {
		t.Fatalf("10 schematic units must equal 2.54mm, got %.6f", got)
	}
}

func TestLayoutReportInMM(t *testing.T) {
	comps := []layoutComp{
		{Designator: "U1", BBox: bb(0, 0, 10, 10)},
		{Designator: "C1", BBox: bb(15, 0, 25, 10)}, // 5 raw = 1.27mm
	}
	raw := analyzeLayout(comps, mmToSchematicUnits(2.54), 0)
	rep := layoutReportInMM(raw)
	if rep.MinGap != 2.54 {
		t.Fatalf("reported min-gap must stay user-facing mm, got %.2f", rep.MinGap)
	}
	if len(rep.TightPairs) != 1 || rep.TightPairs[0].Gap != 1.27 {
		t.Fatalf("expected 1.27mm reported gap, got %+v", rep.TightPairs)
	}
	if rep.MeasurementUnit != "mm" || rep.CoordinateUnit != "0.01inch" || rep.AnchorGridUnit != "0.01inch" {
		t.Fatalf("unexpected unit metadata: measurement=%q coordinate=%q anchorGrid=%q",
			rep.MeasurementUnit, rep.CoordinateUnit, rep.AnchorGridUnit)
	}
	if rep.SchemaVersion != 2 {
		t.Fatalf("schemaVersion=%d, want 2 for corrected measurement semantics", rep.SchemaVersion)
	}
	if raw.TightPairs[0].Gap != 5 {
		t.Fatalf("conversion mutated raw input report: gap=%v, want 5 raw units", raw.TightPairs[0].Gap)
	}
	again := layoutReportInMM(raw)
	if again.TightPairs[0].Gap != rep.TightPairs[0].Gap {
		t.Fatalf("conversion is not repeatable: first=%v second=%v", rep.TightPairs[0].Gap, again.TightPairs[0].Gap)
	}
}

func TestValidateLayoutDistanceFlagRejectsInvalidNumbers(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    float64
	}{
		{"negative", -1},
		{"nan", math.NaN()},
		{"positive-inf", math.Inf(1)},
		{"negative-inf", math.Inf(-1)},
	} {
		if err := validateLayoutDistanceFlag("--min-gap", tc.v); err == nil {
			t.Errorf("%s value %v unexpectedly accepted", tc.name, tc.v)
		}
	}
	for _, v := range []float64{0, 0.004, 2.54} {
		if err := validateLayoutDistanceFlag("--min-gap", v); err != nil {
			t.Errorf("finite value %v rejected: %v", v, err)
		}
	}
}

func TestDetectOffGridAnchors(t *testing.T) {
	comps := []layoutComp{
		{Designator: "U1", X: 100, Y: 205, AnchorAvailable: true, BBox: bb(90, 195, 110, 215)},
		{Designator: "C1", X: 102, Y: 200, AnchorAvailable: true, BBox: bb(120, 190, 130, 200)},
	}
	findings := detectOffGridAnchors(comps, schAnchorGrid, acCoordEps)
	if len(findings) != 1 {
		t.Fatalf("expected one off-grid anchor, got %+v", findings)
	}
	if got := findings[0]; got.A != "C1" || got.X != 102 || got.Y != 200 {
		t.Fatalf("unexpected off-grid finding: %+v", got)
	}
	rep := layoutReport{OK: true, GridViolations: findings}
	if !rep.OK {
		t.Fatal("off-grid is advisory in default mode")
	}
	applyLayoutStrictGate(&rep, true)
	if rep.OK {
		t.Fatal("strict mode must fail an off-grid anchor")
	}
}

func TestLayoutStrictGateFailsWarningsAndUnprovenGeometry(t *testing.T) {
	rep := layoutReport{
		OK:            true,
		TightPairs:    []layoutFinding{{Type: "spacing", A: "C1", B: "U1"}},
		NoBBox:        []string{"R1"},
		UncheckedPins: []string{"C1"},
	}
	applyLayoutStrictGate(&rep, false)
	if !rep.OK {
		t.Fatal("default mode must preserve warning-only compatibility")
	}
	applyLayoutStrictGate(&rep, true)
	if rep.OK || !rep.Strict {
		t.Fatalf("strict mode must fail warning/unproven findings: %+v", rep)
	}
}

func TestParseLayoutCompsTracksPinAvailability(t *testing.T) {
	result := map[string]any{"components": []any{
		map[string]any{"designator": "U1", "componentType": "part", "pins": []any{}},
		map[string]any{"designator": "U2", "componentType": "part"},
	}}
	comps, err := parseLayoutComps(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(comps) != 2 || !comps[0].PinsAvailable || comps[1].PinsAvailable {
		t.Fatalf("pin availability not preserved: %+v", comps)
	}
	rep := analyzeLayout(comps, 0, 0)
	if len(rep.UncheckedPins) != 1 || rep.UncheckedPins[0] != "U2" {
		t.Fatalf("unchecked pins = %v, want [U2]", rep.UncheckedPins)
	}
	rep.ZoneCheckStatus = "not-configured"
	applyLayoutStrictGate(&rep, true)
	if rep.OK {
		t.Fatal("strict gate must fail when pin geometry was omitted")
	}
}

func TestParseLayoutCompsHonorsExplicitPinReadStatus(t *testing.T) {
	result := map[string]any{"components": []any{
		map[string]any{
			"primitiveId": "id-U1", "designator": "U1", "componentType": "part",
			"x": 100, "y": 200,
			"bbox":          map[string]any{"minX": 90, "minY": 190, "maxX": 110, "maxY": 210},
			"pinsAvailable": true,
			"pins":          []any{},
		},
		map[string]any{
			"primitiveId": "id-U2", "designator": "U2", "componentType": "part",
			"x": 300, "y": 400,
			"bbox":          map[string]any{"minX": 290, "minY": 390, "maxX": 310, "maxY": 410},
			"pinsAvailable": false,
			"pinsError":     "SDK timeout",
		},
	}, "count": 2}
	comps, err := parseLayoutComps(result)
	if err != nil {
		t.Fatal(err)
	}
	if !comps[0].PinsAvailable || !comps[0].PinsProofKnown {
		t.Fatalf("explicit successful pin proof lost: %+v", comps[0])
	}
	if comps[1].PinsAvailable || !comps[1].PinsProofKnown {
		t.Fatalf("explicit failed pin proof misread as success: %+v", comps[1])
	}
	if len(comps[1].GeometryErrors) == 0 {
		t.Fatalf("pinsError was not surfaced: %+v", comps[1])
	}
}

func TestParseLayoutCompsRejectsMalformedOrNonFiniteGeometry(t *testing.T) {
	result := map[string]any{"components": []any{
		map[string]any{
			"primitiveId": "id-U1", "designator": "U1", "componentType": "part",
			"x": math.NaN(), "y": "200",
			"bbox":          map[string]any{"minX": 10, "minY": 10, "maxX": 5, "maxY": math.Inf(1)},
			"pinsAvailable": true,
			"pins": []any{
				map[string]any{"pinNumber": "1", "x": 0, "y": "bad"},
			},
		},
	}, "count": 1}
	comps, err := parseLayoutComps(result)
	if err != nil {
		t.Fatal(err)
	}
	if comps[0].AnchorAvailable || comps[0].BBox != nil || comps[0].PinsAvailable {
		t.Fatalf("malformed geometry was converted into usable zeroes: %+v", comps[0])
	}
	if got := invalidLayoutGeometry(comps); len(got) < 3 {
		t.Fatalf("expected anchor+bbox+pin diagnostics, got %v", got)
	}
}

func TestParseLayoutCompsRejectsCountMismatch(t *testing.T) {
	_, err := parseLayoutComps(map[string]any{
		"components": []any{map[string]any{}},
		"count":      2,
	})
	if err == nil || !strings.Contains(err.Error(), "does not prove") {
		t.Fatalf("count mismatch error=%v", err)
	}
}

func TestLayoutStrictRejectsFlattenedOrNonPartGeometry(t *testing.T) {
	cfg := &appConfig{}
	if err := runLayoutLint(cfg, "", 2.54, 0, true, false, false, true, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--all-pages") {
		t.Fatalf("strict all-pages error=%v", err)
	}
	if err := runLayoutLint(cfg, "", 2.54, 0, false, false, true, true, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--include-non-parts") {
		t.Fatalf("strict include-non-parts error=%v", err)
	}
}

func TestLayoutSummaryIncludesStrictFailures(t *testing.T) {
	rep := layoutReport{
		OK:              false,
		Strict:          true,
		Total:           2,
		WithBBox:        2,
		MinGap:          10,
		GridViolations:  []layoutFinding{{Type: "off-grid", A: "U1"}},
		UncheckedPins:   []string{"U2"},
		ZoneCheckStatus: "unavailable",
	}
	got := layoutReportInMM(rep).Summary
	// zone-violation 已随固定九宫格一并废弃(分区框数据驱动后该判据是同义反复)。
	for _, want := range []string{"strict=true", "1 off-grid", "1 unchecked-pins", "zoneCheck=unavailable"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
}

func TestLayoutFindingJSONPreservesApplicableZeroValues(t *testing.T) {
	raw, err := json.Marshal(layoutFinding{
		Type: "pin-coincidence", A: "U1", B: "U2",
		APin: "1", BPin: "2", X: 0, Y: 0, Dist: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{`"x":0`, `"y":0`, `"dist":0`} {
		if !strings.Contains(got, want) {
			t.Errorf("pin finding JSON %s missing %s", got, want)
		}
	}

	raw, err = json.Marshal(layoutFinding{Type: "spacing", A: "C1", B: "U1", Gap: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"gap":0`) {
		t.Errorf("touching spacing JSON %s omitted zero gap", raw)
	}
}

func TestAnalyzeLayout_Overlap(t *testing.T) {
	comps := []layoutComp{
		{Designator: "R1", BBox: bb(0, 0, 10, 10)},
		{Designator: "C2", BBox: bb(5, 5, 15, 15)}, // overlaps R1 by 5×5
	}
	rep := analyzeLayout(comps, 2.54, -1)
	if rep.OK {
		t.Fatal("expected OK=false when components overlap")
	}
	if len(rep.Overlaps) != 1 {
		t.Fatalf("expected 1 overlap, got %d", len(rep.Overlaps))
	}
	f := rep.Overlaps[0]
	if f.A != "C2" || f.B != "R1" { // labels sorted
		t.Errorf("expected pair C2↔R1, got %s↔%s", f.A, f.B)
	}
	if f.OvX != 5 || f.OvY != 5 {
		t.Errorf("expected overlap 5×5, got %.2f×%.2f", f.OvX, f.OvY)
	}
}

func TestAnalyzeLayout_TightSpacing(t *testing.T) {
	comps := []layoutComp{
		{Designator: "U1", BBox: bb(0, 0, 10, 10)},
		{Designator: "C5", BBox: bb(11, 0, 21, 10)}, // 1 native schematic-unit gap
	}
	rep := analyzeLayout(comps, 2.54, -1)
	if !rep.OK {
		t.Fatal("tight spacing alone should not fail OK (only overlaps do)")
	}
	if len(rep.TightPairs) != 1 {
		t.Fatalf("expected 1 tight pair, got %d", len(rep.TightPairs))
	}
	if g := rep.TightPairs[0].Gap; g != 1 {
		t.Errorf("expected gap 1.0 schematic unit, got %.2f", g)
	}
}

func TestAnalyzeLayout_Clear(t *testing.T) {
	comps := []layoutComp{
		{Designator: "U1", BBox: bb(0, 0, 10, 10)},
		{Designator: "C5", BBox: bb(20, 0, 30, 10)}, // 10 native units, well clear
	}
	rep := analyzeLayout(comps, 2.54, -1)
	if !rep.OK || len(rep.Overlaps) != 0 || len(rep.TightPairs) != 0 {
		t.Fatalf("expected clean report, got %+v", rep)
	}
}

func TestAnalyzeLayout_TouchingEdgesNotOverlap(t *testing.T) {
	comps := []layoutComp{
		{Designator: "A", BBox: bb(0, 0, 10, 10)},
		{Designator: "B", BBox: bb(10, 0, 20, 10)}, // shares an edge, gap 0
	}
	rep := analyzeLayout(comps, 2.54, -1)
	if len(rep.Overlaps) != 0 {
		t.Fatalf("touching edges must not count as overlap, got %d", len(rep.Overlaps))
	}
	if len(rep.TightPairs) != 1 || rep.TightPairs[0].Gap != 0 {
		t.Fatalf("expected one tight pair at gap 0, got %+v", rep.TightPairs)
	}
}

func TestAnalyzeLayout_UnassignedDesignatorFallsBackToID(t *testing.T) {
	comps := []layoutComp{
		{ID: "aaa111", Designator: "C?", BBox: bb(0, 0, 10, 10)},
		{ID: "bbb222", Designator: "C?", BBox: bb(5, 5, 15, 15)}, // overlap
	}
	rep := analyzeLayout(comps, 2.54, -1)
	if len(rep.Overlaps) != 1 {
		t.Fatalf("expected 1 overlap, got %d", len(rep.Overlaps))
	}
	f := rep.Overlaps[0]
	// Both designators are unassigned ("C?") → labels disambiguate via id.
	if f.A == f.B {
		t.Fatalf("unassigned designators must disambiguate, got %q ↔ %q", f.A, f.B)
	}
	if f.A != "C?@aaa111" || f.B != "C?@bbb222" {
		t.Errorf("expected id-suffixed labels, got %q ↔ %q", f.A, f.B)
	}
}

func TestFilterLayoutComps_ExcludesNonPartsByDefault(t *testing.T) {
	comps := []layoutComp{
		{Designator: "R1", ComponentType: "part", BBox: bb(0, 0, 10, 10)},
		{Designator: "SHEET", ComponentType: "sheet", BBox: bb(-100, -100, 400, 300)}, // full-page frame
		{ID: "nf1", ComponentType: "netflag", BBox: bb(0, 0, 2, 2)},
		{Designator: "C2", ComponentType: "part", BBox: bb(20, 0, 30, 10)},
	}
	kept, skipped := filterLayoutComps(comps, false)
	if len(kept) != 2 {
		t.Fatalf("expected 2 parts kept, got %d", len(kept))
	}
	if skipped != 2 {
		t.Fatalf("expected 2 non-parts skipped, got %d", skipped)
	}
	for _, c := range kept {
		if c.ComponentType != "part" {
			t.Errorf("non-part leaked through: %+v", c)
		}
	}
}

func TestFilterLayoutComps_IncludeNonPartsKeepsAll(t *testing.T) {
	comps := []layoutComp{
		{Designator: "R1", ComponentType: "part", BBox: bb(0, 0, 10, 10)},
		{Designator: "SHEET", ComponentType: "sheet", BBox: bb(-100, -100, 400, 300)},
	}
	kept, skipped := filterLayoutComps(comps, true)
	if len(kept) != 2 || skipped != 0 {
		t.Fatalf("include-non-parts must keep all, got kept=%d skipped=%d", len(kept), skipped)
	}
}

func TestFilterLayoutComps_EmptyTypeKept(t *testing.T) {
	// An older connector that doesn't emit componentType must not have every
	// component silently dropped.
	comps := []layoutComp{
		{Designator: "R1", BBox: bb(0, 0, 10, 10)},
		{Designator: "C2", BBox: bb(20, 0, 30, 10)},
	}
	kept, skipped := filterLayoutComps(comps, false)
	if len(kept) != 2 || skipped != 0 {
		t.Fatalf("empty componentType must be kept, got kept=%d skipped=%d", len(kept), skipped)
	}
}

func TestFilterLayoutComps_SheetNoLongerFalseOverlaps(t *testing.T) {
	// Regression for issue #13: a full-page sheet bbox overlaps every real part.
	// After filtering, the analysis must report a clean layout.
	comps := []layoutComp{
		{Designator: "SHEET", ComponentType: "sheet", BBox: bb(-100, -100, 400, 300)},
		{Designator: "R1", ComponentType: "part", BBox: bb(0, 0, 10, 10)},
		{Designator: "C2", ComponentType: "part", BBox: bb(20, 0, 30, 10)},
	}
	parts, skipped := filterLayoutComps(comps, false)
	rep := analyzeLayout(parts, 2.54, -1)
	rep.SkippedNonParts = skipped
	if !rep.OK {
		t.Fatalf("expected clean report after excluding sheet, got %+v", rep.Overlaps)
	}
	if rep.SkippedNonParts != 1 {
		t.Errorf("expected SkippedNonParts=1, got %d", rep.SkippedNonParts)
	}
}

func TestAnalyzeLayout_NoBBoxSkipped(t *testing.T) {
	comps := []layoutComp{
		{Designator: "R1", BBox: bb(0, 0, 10, 10)},
		{Designator: "R2"}, // no bbox → skipped, recorded
	}
	rep := analyzeLayout(comps, 2.54, -1)
	if rep.WithBBox != 1 {
		t.Errorf("expected WithBBox=1, got %d", rep.WithBBox)
	}
	if len(rep.NoBBox) != 1 || rep.NoBBox[0] != "R2" {
		t.Errorf("expected R2 recorded as no-bbox, got %v", rep.NoBBox)
	}
}

func pin(num string, x, y float64) layoutPin { return layoutPin{Number: num, X: x, Y: y} }

func TestAnalyzeLayout_PinCoincidenceError(t *testing.T) {
	// Issue #63: a 1210 cap and a 0402 resistor whose pins land on the same
	// point — bboxes never touch, but the shared pin is an implicit short.
	comps := []layoutComp{
		{Designator: "C1", BBox: bb(255, 200, 265, 210), Pins: []layoutPin{pin("1", 255, 205), pin("2", 260, 205)}},
		{Designator: "R_Q3G", BBox: bb(260, 205, 270, 215), Pins: []layoutPin{pin("1", 260, 205), pin("2", 265, 205)}},
	}
	rep := analyzeLayout(comps, 2.54, 0)
	if rep.OK {
		t.Fatal("expected OK=false when pins of different parts coincide")
	}
	if len(rep.PinCoincidences) != 1 {
		t.Fatalf("expected 1 pin-coincidence, got %d: %+v", len(rep.PinCoincidences), rep.PinCoincidences)
	}
	f := rep.PinCoincidences[0]
	if f.Type != "pin-coincidence" {
		t.Errorf("expected type pin-coincidence, got %q", f.Type)
	}
	if f.A != "C1" || f.B != "R_Q3G" {
		t.Errorf("expected pair C1↔R_Q3G, got %s↔%s", f.A, f.B)
	}
	if f.APin != "2" || f.BPin != "1" {
		t.Errorf("expected pins C1:2 ↔ R_Q3G:1, got %s ↔ %s", f.APin, f.BPin)
	}
	if f.X != 260 || f.Y != 205 {
		t.Errorf("expected shared point (260,205), got (%.2f,%.2f)", f.X, f.Y)
	}
}

func TestAnalyzeLayout_SamePartPinsNoFalsePositive(t *testing.T) {
	// A single symbol's own pins sit at fixed offsets; they must never collide
	// with each other even if numerically close.
	comps := []layoutComp{
		{Designator: "U1", BBox: bb(0, 0, 20, 20), Pins: []layoutPin{pin("1", 5, 5), pin("2", 5, 5)}},
	}
	rep := analyzeLayout(comps, 2.54, 0)
	if len(rep.PinCoincidences) != 0 {
		t.Fatalf("same-component pins must not flag, got %+v", rep.PinCoincidences)
	}
	if !rep.OK {
		t.Fatal("single component must report OK")
	}
}

func TestAnalyzeLayout_PinEpsBoundary(t *testing.T) {
	// Two pins 0.5 native unit apart: clear under eps=0, flagged under eps=0.5.
	comps := []layoutComp{
		{Designator: "R1", BBox: bb(0, 0, 10, 10), Pins: []layoutPin{pin("1", 5, 5)}},
		{Designator: "R2", BBox: bb(20, 0, 30, 10), Pins: []layoutPin{pin("1", 5.5, 5)}},
	}
	if rep := analyzeLayout(comps, 2.54, 0); len(rep.PinCoincidences) != 0 {
		t.Fatalf("eps=0 must not flag pins 0.5 schematic unit apart, got %+v", rep.PinCoincidences)
	}
	rep := analyzeLayout(comps, 2.54, 0.5)
	if len(rep.PinCoincidences) != 1 {
		t.Fatalf("eps=0.5 must flag pins exactly 0.5 schematic unit apart, got %d", len(rep.PinCoincidences))
	}
}

func TestAnalyzeLayout_PinCheckDisabled(t *testing.T) {
	// Negative eps disables the pin check (internal bbox-only callers).
	comps := []layoutComp{
		{Designator: "C1", BBox: bb(0, 0, 10, 10), Pins: []layoutPin{pin("1", 5, 5)}},
		{Designator: "R1", BBox: bb(20, 0, 30, 10), Pins: []layoutPin{pin("1", 5, 5)}},
	}
	rep := analyzeLayout(comps, 2.54, -1)
	if len(rep.PinCoincidences) != 0 || !rep.OK {
		t.Fatalf("negative eps must disable pin check, got %+v OK=%v", rep.PinCoincidences, rep.OK)
	}
}

func TestParseLayoutComps_ExtractsPins(t *testing.T) {
	result := map[string]any{
		"components": []any{
			map[string]any{
				"primitiveId":   "aaa",
				"designator":    "C1",
				"componentType": "part",
				"bbox":          map[string]any{"minX": 255.0, "minY": 200.0, "maxX": 265.0, "maxY": 210.0},
				"pins": []any{
					map[string]any{"pinNumber": "1", "x": 255.0, "y": 205.0},
					map[string]any{"pinNumber": "2", "x": 260.0, "y": 205.0},
				},
			},
		},
	}
	comps, err := parseLayoutComps(result)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(comps) != 1 || len(comps[0].Pins) != 2 {
		t.Fatalf("expected 1 comp with 2 pins, got %+v", comps)
	}
	if comps[0].Pins[1].Number != "2" || comps[0].Pins[1].X != 260 || comps[0].Pins[1].Y != 205 {
		t.Errorf("unexpected pin[1]: %+v", comps[0].Pins[1])
	}
}
