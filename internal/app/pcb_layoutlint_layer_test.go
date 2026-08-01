package app

import "testing"

// Layer + net awareness for `pcb layout-lint` overlap (docs/ecosystem-survey.md
// §9.3). The overlap check used to compare unlayered rendered bboxes, so a top
// part and a bottom part sharing an XY — a perfectly legal top/bottom
// pass-through — counted as a collision. box-v2 rev-a (166 parts, double-sided)
// reported 100+ "overlaps" whose real same-side count was 0.
//
// bb() and layerComp() build the fixtures; bb is shared from cmd_sch_layout_test.go.

func layerComp(d string, layer int, minX, minY, maxX, maxY float64) pcbLComp {
	return pcbLComp{Designator: d, Layer: layer, BBox: bb(minX, minY, maxX, maxY)}
}

// The headline fix: identical XY, opposite sides ⇒ 0 overlap; identical XY, same
// side ⇒ still an ERROR. Mirrors the KiCad control experiment (two C_0805 at the
// same XY on F.Cu / B.Cu → 0 courtyard violations).
func TestAnalyzePcbLayout_OverlapIsLayerAware(t *testing.T) {
	crossSide := []pcbLComp{
		layerComp("C_TOP", pcbSideTop, 0, 0, 50, 30),
		layerComp("C_BOT", pcbSideBottom, 0, 0, 50, 30),
	}
	rep := analyzePcbLayout(crossSide, nil, nil, 6)
	if len(rep.Overlaps) != 0 {
		t.Errorf("same XY on opposite sides is a legal pass-through: overlaps=%d, want 0 (%+v)", len(rep.Overlaps), rep.Overlaps)
	}
	if len(rep.TightPairs) != 0 {
		t.Errorf("opposite sides must not produce a tight-spacing pair either, got %+v", rep.TightPairs)
	}
	if !rep.OK || rep.Score != 100 {
		t.Errorf("cross-side pair: ok=%v score=%d, want true/100", rep.OK, rep.Score)
	}
	if rep.Sides["top"] != 1 || rep.Sides["bottom"] != 1 {
		t.Errorf("sides=%v, want top:1 bottom:1", rep.Sides)
	}

	sameSide := []pcbLComp{
		layerComp("C_A", pcbSideTop, 0, 0, 50, 30),
		layerComp("C_B", pcbSideTop, 0, 0, 50, 30),
	}
	rep2 := analyzePcbLayout(sameSide, nil, nil, 6)
	if len(rep2.Overlaps) != 1 || rep2.OK || rep2.Verdict != "overlap" {
		t.Errorf("same XY on the SAME side must stay an ERROR: overlaps=%d ok=%v verdict=%q",
			len(rep2.Overlaps), rep2.OK, rep2.Verdict)
	}
	if rep2.Overlaps[0].Side != "top" {
		t.Errorf("overlap side=%q, want top", rep2.Overlaps[0].Side)
	}
}

// Tight spacing is per side too: a bottom part 1mil away from a top part's XY is
// not "too close to hand-solder", it's on the other side of the laminate.
func TestAnalyzePcbLayout_TightSpacingIsLayerAware(t *testing.T) {
	comps := []pcbLComp{
		layerComp("U1", pcbSideTop, 0, 0, 100, 100),
		layerComp("R1", pcbSideBottom, 101, 0, 200, 100), // 1mil away, other side
		layerComp("R2", pcbSideTop, 101, 200, 200, 300),  // far on the same side
	}
	rep := analyzePcbLayout(comps, nil, nil, 40)
	for _, tp := range rep.TightPairs {
		if tp.A == "R1" || tp.B == "R1" {
			t.Errorf("a bottom-side neighbour must not count as tight spacing: %+v", tp)
		}
	}
}

// A component with an unknown side (layer 0 — older connector) must still be
// compared against everything: a missing field may never HIDE a real overlap.
func TestAnalyzePcbLayout_UnknownSideStillCompared(t *testing.T) {
	comps := []pcbLComp{
		layerComp("U1", pcbSideTop, 0, 0, 50, 50),
		layerComp("U2", 0, 25, 25, 75, 75), // side unknown
	}
	rep := analyzePcbLayout(comps, nil, nil, 6)
	if len(rep.Overlaps) != 1 {
		t.Fatalf("unknown side must be compared conservatively: overlaps=%d, want 1", len(rep.Overlaps))
	}
	if rep.Overlaps[0].Side != "top" {
		t.Errorf("side label should fall back to the known side, got %q", rep.Overlaps[0].Side)
	}
}

// Iron access is per side as well — a part on the opposite side of the board
// never blocks the soldering iron.
func TestAnalyzeSolderAccess_LayerAware(t *testing.T) {
	// U1 walled in on all four sides, but every wall is on the BOTTOM side.
	comps := []pcbLComp{
		layerComp("U1", pcbSideTop, 0, 0, 100, 100),
		layerComp("R1", pcbSideBottom, 120, 0, 220, 100),
		layerComp("R2", pcbSideBottom, -220, 0, -20, 100),
		layerComp("C1", pcbSideBottom, 0, 120, 100, 220),
		layerComp("C2", pcbSideBottom, 0, -220, 100, -20),
	}
	for _, b := range analyzeSolderAccess(comps, 60) {
		if b.Designator == "U1" {
			t.Fatalf("bottom-side neighbours cannot block a top-side iron: %+v", b)
		}
	}
	// Same geometry, walls moved to the top side → blocked again.
	for i := 1; i < len(comps); i++ {
		comps[i].Layer = pcbSideTop
	}
	blocked := false
	for _, b := range analyzeSolderAccess(comps, 60) {
		if b.Designator == "U1" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("same-side walls must still box U1 in")
	}
}

// Net awareness: overlapping pads from two different nets is a SHORT, not merely
// a geometric overlap. This is the D2.2[SW1_NODE] × C2.1[VBAT_RAW] case KiCad
// reported as `shorting_items` while we only saw "bboxes intersect".
func TestAnalyzePcbLayout_CrossNetPadShort(t *testing.T) {
	comps := []pcbLComp{
		layerComp("D2", pcbSideTop, 0, 0, 60, 40),
		layerComp("C2", pcbSideTop, 30, 0, 90, 40),
	}
	pads := []pcbLPad{
		{Designator: "D2", Number: "2", Net: "SW1_NODE", Layer: pcbSideTop, X: 40, Y: 20, W: 20, H: 20},
		{Designator: "C2", Number: "1", Net: "VBAT_RAW", Layer: pcbSideTop, X: 45, Y: 20, W: 20, H: 20},
	}
	rep := analyzePcbLayout(comps, pads, nil, 6)
	if len(rep.Shorts) != 1 {
		t.Fatalf("cross-net pad copper contact must be reported as a short: %+v", rep.Shorts)
	}
	s := rep.Shorts[0]
	if s.A != "C2.1" || s.NetA != "VBAT_RAW" || s.B != "D2.2" || s.NetB != "SW1_NODE" {
		t.Errorf("short endpoints=%+v, want C2.1[VBAT_RAW] ↔ D2.2[SW1_NODE]", s)
	}
	if s.Layer != "top" {
		t.Errorf("short layer=%q, want top", s.Layer)
	}
	if rep.OK || rep.Verdict != "short" || rep.Score != 0 {
		t.Errorf("a short is fatal: ok=%v verdict=%q score=%d", rep.OK, rep.Verdict, rep.Score)
	}
	if v := evalLayoutGate(rep, pcbLayoutGateOpts{gate: true, minScore: 60, maxCrossings: 8}); v.Pass {
		t.Error("a cross-net short must fail the routability gate")
	}

	// Same geometry, SAME net → touching copper is intentional, no short (the
	// bodies still overlap, which the overlap finding covers).
	same := []pcbLPad{
		{Designator: "D2", Number: "2", Net: "SW1_NODE", Layer: pcbSideTop, X: 40, Y: 20, W: 20, H: 20},
		{Designator: "C2", Number: "1", Net: "SW1_NODE", Layer: pcbSideTop, X: 45, Y: 20, W: 20, H: 20},
	}
	if rep2 := analyzePcbLayout(comps, same, nil, 6); len(rep2.Shorts) != 0 {
		t.Errorf("same-net pads touching is not a short: %+v", rep2.Shorts)
	}
}

// Copper contact is judged per PAD layer, not per assembly side — the one place
// the layer-aware overlap rule must NOT be applied:
//   - two SMD pads at the same XY on opposite sides never touch;
//   - a through-hole barrel (layer 12 = multi) conducts on every layer, so it
//     genuinely shorts against a pad on the far side.
func TestAnalyzePcbLayout_ShortAcrossSides(t *testing.T) {
	comps := []pcbLComp{
		layerComp("U1", pcbSideTop, 0, 0, 60, 40),
		layerComp("J1", pcbSideBottom, 30, 0, 90, 40),
	}
	smd := []pcbLPad{
		{Designator: "U1", Number: "1", Net: "A", Layer: pcbSideTop, X: 45, Y: 20, W: 20, H: 20},
		{Designator: "J1", Number: "1", Net: "B", Layer: pcbSideBottom, X: 45, Y: 20, W: 20, H: 20},
	}
	if rep := analyzePcbLayout(comps, smd, nil, 6); len(rep.Shorts) != 0 {
		t.Errorf("SMD pads on opposite sides cannot short: %+v", rep.Shorts)
	}

	tht := []pcbLPad{
		{Designator: "U1", Number: "1", Net: "A", Layer: pcbSideTop, X: 45, Y: 20, W: 20, H: 20},
		{Designator: "J1", Number: "1", Net: "B", Layer: pcbLayerMulti, X: 45, Y: 20, W: 20, H: 20},
	}
	rep := analyzePcbLayout(comps, tht, nil, 6)
	if len(rep.Shorts) != 1 || rep.Shorts[0].Layer != "multi" {
		t.Fatalf("a through-hole barrel shorts against the far side: %+v", rep.Shorts)
	}
	if len(rep.Overlaps) != 0 {
		t.Errorf("the bodies themselves are on opposite sides — no overlap finding: %+v", rep.Overlaps)
	}
}

// Pads the connector could not size (polygon shapes → no width/height) and pads
// with no net are skipped rather than guessed at: the short check must never
// invent contact it cannot measure.
func TestAnalyzePcbLayout_ShortSkipsUnmeasurablePads(t *testing.T) {
	comps := []pcbLComp{
		layerComp("U1", pcbSideTop, 0, 0, 60, 40),
		layerComp("U2", pcbSideTop, 30, 0, 90, 40),
	}
	pads := []pcbLPad{
		{Designator: "U1", Number: "1", Net: "A", Layer: pcbSideTop, X: 45, Y: 20}, // no extent
		{Designator: "U2", Number: "1", Net: "B", Layer: pcbSideTop, X: 45, Y: 20, W: 20, H: 20},
		{Designator: "U1", Number: "2", Net: "", Layer: pcbSideTop, X: 45, Y: 20, W: 20, H: 20}, // no net
	}
	if rep := analyzePcbLayout(comps, pads, nil, 6); len(rep.Shorts) != 0 {
		t.Errorf("unmeasurable / netless pads must not produce shorts: %+v", rep.Shorts)
	}
}
