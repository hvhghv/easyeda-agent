package app

import (
	"testing"
)

// findingByType returns the findings of a given type, in order.
func findingsOfType(fs []checkFinding, typ string) []checkFinding {
	var out []checkFinding
	for _, f := range fs {
		if f.Type == typ {
			out = append(out, f)
		}
	}
	return out
}

func TestShortSymbolIsConnectivityMarker(t *testing.T) {
	if !isSchMarker("short_symbol") {
		t.Fatal("short_symbol must participate in marker/title-block geometry checks")
	}
}

// ── duplicate-net-marker (issue #146) ───────────────────────────────────────

func TestDuplicateNetMarker_CoincidentAndFloatDrift(t *testing.T) {
	comps := []layoutComp{
		{ID: "g1", ComponentType: "netflag", Net: "GND", X: 1325, Y: 275},
		{ID: "g2", ComponentType: "netflag", Net: "GND", X: 1325, Y: 275},                 // exact duplicate
		{ID: "g3", ComponentType: "netflag", Net: "GND", X: 1324.9999999, Y: 275.0000001}, // float drift → same bucket
		{ID: "p1", ComponentType: "netport", Net: "MOTOR_G", X: 770, Y: 255},
		{ID: "p2", ComponentType: "netport", Net: "MOTOR_G", X: 770, Y: 255}, // duplicate
		{ID: "solo", ComponentType: "netflag", Net: "VCC", X: 500, Y: 500},   // no duplicate
		// NEGATIVE cases that MUST NOT merge with the GND stack at (1325,275):
		{ID: "diffnet", ComponentType: "netflag", Net: "5V", X: 1325, Y: 275},   // different net
		{ID: "difftype", ComponentType: "netport", Net: "GND", X: 1325, Y: 275}, // different marker kind
		{ID: "R1", ComponentType: "part", Designator: "R1", X: 1325, Y: 275},    // not a marker
	}
	got := duplicateNetMarkerFindings(comps)
	if len(got) != 2 {
		t.Fatalf("want 2 duplicate groups (GND, MOTOR_G), got %d: %+v", len(got), got)
	}
	// The GND group: keep the lexically-smallest id, delete the rest.
	var gnd *checkFinding
	for i := range got {
		if got[i].MarkerNet == "GND" {
			gnd = &got[i]
		}
	}
	if gnd == nil {
		t.Fatalf("no GND duplicate finding in %+v", got)
	}
	if gnd.Type != "duplicate-net-marker" || gnd.Level != "warn" {
		t.Errorf("GND finding type/level = %s/%s", gnd.Type, gnd.Level)
	}
	if len(gnd.PrimitiveIds) != 3 {
		t.Errorf("GND group should carry all 3 coincident ids (incl. float-drift g3), got %v", gnd.PrimitiveIds)
	}
	if gnd.SuggestKeepId != "g1" {
		t.Errorf("keep id should be lexically-smallest g1, got %q", gnd.SuggestKeepId)
	}
	if len(gnd.SuggestDeleteIds) != 2 {
		t.Errorf("should suggest deleting g2,g3, got %v", gnd.SuggestDeleteIds)
	}
}

// ── titleblock-overlap (issue #147) ─────────────────────────────────────────

func TestTitleblockOverlap_RealMotorG(t *testing.T) {
	keepout := bb(912.6, 0, 1170, 115.5) // issue #147 A4 title-block keep-out
	comps := []layoutComp{
		// MOTOR_G netport bbox from the issue — fully inside the keep-out.
		{ID: "motorG", ComponentType: "netport", Net: "MOTOR_G", BBox: bb(1019.5, 79.5, 1030.5, 110.5)},
		{ID: "safe", ComponentType: "netport", Net: "SAFE", BBox: bb(100, 200, 120, 220)}, // clear
		{ID: "sheet", ComponentType: "sheet", BBox: bb(0, 0, 1170, 825)},                  // spans page → must NOT report
	}
	got := titleblockOverlapFindings(comps, keepout, sheetSourceKnownTemplate, 0.5)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 titleblock-overlap (MOTOR_G), got %d: %+v", len(got), got)
	}
	if got[0].PrimitiveId != "motorG" {
		t.Errorf("expected MOTOR_G, got %q", got[0].PrimitiveId)
	}
	// A CONFIRMED (A4-calibrated) keep-out reports at warn.
	if got[0].Level != "warn" {
		t.Errorf("confirmed keep-out hit must be warn, got %q", got[0].Level)
	}
	// overlap = 11 (x) × 31 (y).
	if got[0].OverlapX != 11 || got[0].OverlapY != 31 {
		t.Errorf("overlap = %.2f×%.2f, want 11×31", got[0].OverlapX, got[0].OverlapY)
	}
}

func TestTitleblockOverlap_NoKeepoutIsNoop(t *testing.T) {
	comps := []layoutComp{{ID: "x", ComponentType: "netport", BBox: bb(1000, 50, 1010, 60)}}
	if got := titleblockOverlapFindings(comps, nil, sheetSourceNone, 0.5); len(got) != 0 {
		t.Errorf("nil keep-out must yield no findings, got %+v", got)
	}
}

// Issue #172: a hit against an ESTIMATED keep-out (source=fallback-ratio — non-A4
// sheet or unmatched aspect) downgrades to info and says the geometry is a guess;
// only the confirmed A4-calibrated source keeps warn.
func TestTitleblockOverlap_EstimatedSourceIsInfo(t *testing.T) {
	keepout := bb(953, 0, 1655, 198)
	comps := []layoutComp{
		{ID: "hit", Designator: "R23", ComponentType: "part", BBox: bb(1400, 20, 1440, 60)},
	}
	got := titleblockOverlapFindings(comps, keepout, sheetSourceFallback, 0.5)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(got), got)
	}
	if got[0].Level != "info" {
		t.Errorf("estimated keep-out hit must be info, got %q", got[0].Level)
	}
	if !containsStr(got[0].Message, "建议人工确认") || !containsStr(got[0].Message, sheetSourceFallback) {
		t.Errorf("info message must say the keep-out is estimated + suggest human confirmation, got %q", got[0].Message)
	}
}

// Issue #172 end-to-end reproduction: on the 1655×1170 sheet the old A4-fraction
// keep-out ([662,0 → 1655,234]) false-flagged parts at x≈700–770 mid-sheet. With
// the fixed-size estimate they must not be reported at all, and a part genuinely
// inside the bottom-right table region reports at info (not warn).
func TestTitleblockOverlap_Issue172NonA4NoFalsePositives(t *testing.T) {
	sheet := bb(0, 0, 1655, 1170)
	keepout, source := titleBlockKeepoutWithSource(sheet)
	if keepout == nil || source != sheetSourceFallback {
		t.Fatalf("non-A4 sheet must yield an estimated keep-out, got %+v source=%q", keepout, source)
	}
	comps := []layoutComp{
		// The issue's real false positives (x ≈ 43–46% of the sheet width).
		{ID: "L1", Designator: "L1", ComponentType: "part", BBox: bb(750, 100, 790, 160)},
		{ID: "C6", Designator: "C6", ComponentType: "part", BBox: bb(690, 90, 730, 150)},
		{ID: "R5", Designator: "R5", ComponentType: "part", BBox: bb(690, 160, 730, 220)},
		{ID: "gnd1", ComponentType: "netflag", Net: "GND", BBox: bb(700, 80, 710, 101)},
		// A part truly inside the bottom-right table area.
		{ID: "deep", Designator: "R99", ComponentType: "part", BBox: bb(1400, 20, 1450, 80)},
	}
	got := titleblockOverlapFindings(comps, keepout, source, 0.5)
	if len(got) != 1 {
		t.Fatalf("only the truly bottom-right part may be reported, got %d: %+v", len(got), got)
	}
	if got[0].PrimitiveId != "deep" || got[0].Level != "info" {
		t.Errorf("want deep@info, got %s@%s", got[0].PrimitiveId, got[0].Level)
	}
}

// ── marker-overlap (issue #148) ─────────────────────────────────────────────

// realH2H4Comps uses the exact H2/H4 part + netport bboxes from issue #148.
func realH2H4Comps() []layoutComp {
	return []layoutComp{
		{ID: "H2", Designator: "H2", ComponentType: "part", BBox: bb(184.5, 579.5, 210.5, 660.5)},
		{ID: "H4", Designator: "H4", ComponentType: "part", BBox: bb(104.5, 599.5, 125.5, 640.5)},
		{ID: "mMICSD", ComponentType: "netport", Net: "MICSD", BBox: bb(194.5, 624.5, 225.5, 635.5)},
		{ID: "mBCLK", ComponentType: "netport", Net: "BCLK", BBox: bb(114.5, 634.5, 145.5, 645.5)},
		{ID: "mDIN", ComponentType: "netport", Net: "DIN", BBox: bb(114.5, 624.5, 145.5, 635.5)},
		{ID: "mSD", ComponentType: "netport", Net: "SD", BBox: bb(114.5, 604.5, 145.5, 615.5)},
	}
}

func TestMarkerOverlap_RealH2H4(t *testing.T) {
	got := markerOverlapFindings(realH2H4Comps(), 0.5)
	// Expect the 4 marker×part overlaps + BCLK×DIN (31×1) marker×marker = 5.
	if len(got) != 5 {
		t.Fatalf("want 5 marker-overlaps, got %d: %+v", len(got), summarizeOverlaps(got))
	}
	// Spot-check the issue's exact overlap extents.
	want := map[[2]string][2]float64{
		{"H2", "mMICSD"}:  {16, 11},
		{"H4", "mBCLK"}:   {11, 6},
		{"H4", "mDIN"}:    {11, 11},
		{"H4", "mSD"}:     {11, 11},
		{"mBCLK", "mDIN"}: {31, 1}, // the boundary case: min axis 1 > eps 0.5 → reported
	}
	for _, f := range got {
		key := [2]string{f.PrimitiveId, f.Other.PrimitiveId}
		w, ok := want[key]
		if !ok {
			t.Errorf("unexpected overlap pair %v", key)
			continue
		}
		if f.OverlapX != w[0] || f.OverlapY != w[1] {
			t.Errorf("pair %v overlap = %.2f×%.2f, want %.2f×%.2f", key, f.OverlapX, f.OverlapY, w[0], w[1])
		}
		if len(f.PrimitiveIds) != 2 {
			t.Errorf("marker-overlap must carry BOTH primitive ids, got %v", f.PrimitiveIds)
		}
	}
}

func TestMarkerOverlap_ExcludesPartPairAndSubEps(t *testing.T) {
	comps := []layoutComp{
		// Two overlapping PARTS — layout-lint's job, NOT marker-overlap.
		{ID: "P1", Designator: "P1", ComponentType: "part", BBox: bb(0, 0, 20, 20)},
		{ID: "P2", Designator: "P2", ComponentType: "part", BBox: bb(10, 10, 30, 30)},
		// A marker grazing part P3 by only 0.3 on the min axis (x) — below eps 0.5.
		// (Placed at y 40..60 so it does NOT touch P1/P2.)
		{ID: "mA", ComponentType: "netport", Net: "A", BBox: bb(29.7, 40, 50, 60)},
		{ID: "P3", Designator: "P3", ComponentType: "part", BBox: bb(0, 40, 30, 60)},
	}
	got := markerOverlapFindings(comps, 0.5)
	if len(got) != 0 {
		t.Errorf("part×part excluded and sub-eps graze ignored → want 0, got %+v", summarizeOverlaps(got))
	}
}

func summarizeOverlaps(fs []checkFinding) []string {
	var s []string
	for _, f := range fs {
		other := ""
		if f.Other != nil {
			other = f.Other.PrimitiveId
		}
		s = append(s, f.PrimitiveId+"×"+other)
	}
	return s
}

// ── partial-run bookkeeping (issue #146) ────────────────────────────────────

func TestSplitConnResults_Partial(t *testing.T) {
	conns := []acConnResult{
		{Pin: "U1:1", Net: "GND"},                        // ok
		{Pin: "U1:2", Net: "GND", Error: "connect drop"}, // failed
		{Pin: "U1:3", Net: "VCC"},                        // ok
	}
	ok, failed, partial := splitConnResults(conns, false)
	if len(ok) != 2 || len(failed) != 1 {
		t.Fatalf("want 2 ok / 1 failed, got %v / %v", ok, failed)
	}
	if failed[0] != "U1:2" {
		t.Errorf("failed pin should be U1:2, got %v", failed)
	}
	if !partial {
		t.Error("a real batch with both successes and failures is partial")
	}
	// Dry-run is never 'partial' (nothing mutated), even with a resolve error.
	if _, _, p := splitConnResults(conns, true); p {
		t.Error("dry-run must not be reported as partial")
	}
	// All-success is not partial.
	if _, _, p := splitConnResults([]acConnResult{{Pin: "A:1"}}, false); p {
		t.Error("all-success run must not be partial")
	}
}

// TestPartitionFindingFor covers the 铁律#15 backstop: a multi-module page
// (parts ≥ schPartitionMinParts) with zero free text (no zone frames / notes) is
// flagged missing-partition; a framed/noted page or a trivially small one is not.
func TestPartitionFindingFor(t *testing.T) {
	// **框和说明分开判**:第一版只看「自由文本 > 0」,于是画了区框(区名也是文本)
	// 或随手写一行注释,判据就闭嘴了 —— 交付三件套里有两件可以蒙混过去。
	cases := []struct {
		name                        string
		parts, rects, labels, texts int
		wantFrame, wantNote         bool
	}{
		{"什么都没有的 12 件页:框和说明都报", 12, 0, 0, 0, true, true},
		{"画了框但一条说明都没有:只报说明", 12, 3, 3, 3, false, true},
		{"框 + 说明齐全:干净", 12, 3, 3, 6, false, false},
		{"只有说明没有框:只报框", 12, 0, 0, 2, true, false},
		{"低于阈值:一条都不报", schPartitionMinParts - 1, 0, 0, 0, false, false},
		{"恰好到阈值:照报", schPartitionMinParts, 0, 0, 0, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := partitionFindingFor(tc.parts, tc.rects, tc.labels, tc.texts)
			var frame, note bool
			for _, f := range got {
				switch f.Type {
				case "missing-partition":
					frame = true
				case "missing-note":
					note = true
				}
			}
			if frame != tc.wantFrame || note != tc.wantNote {
				t.Fatalf("frame=%v note=%v, want frame=%v note=%v (findings=%+v)",
					frame, note, tc.wantFrame, tc.wantNote, got)
			}
		})
	}
}

// TestFoldedNetLabelFindings: vertical netports (bbox taller than wide = rotation
// 90/270, label sideways) are flagged; horizontal netports and near-square
// ground/power markers are not. Real ceshi geometry: folded 11×31, normal 31×11.
func TestFoldedNetLabelFindings(t *testing.T) {
	comps := []layoutComp{
		{ID: "folded", ComponentType: "netport", Net: "LED_CTRL", BBox: bb(940, 440, 951, 471)}, // 11×31 vertical
		{ID: "normal", ComponentType: "netport", Net: "LED_CTRL", BBox: bb(945, 650, 976, 661)}, // 31×11 horizontal
		{ID: "gndv", ComponentType: "netflag", Net: "GND", BBox: bb(0, 0, 10, 21)},              // ground is exempt
		{ID: "nobox", ComponentType: "netport", Net: "EN"},                                      // no bbox → skip
	}
	// 2026-08-12 用户拍板「netport 顺着方向摆布即可」:竖放合法,判据恒零
	// (拥挤由 marker-overlap 文字带管)。此测试翻转为语义变更的回归钉。
	fs := foldedNetLabelFindings(comps)
	if len(fs) != 0 {
		t.Fatalf("vertical netport is legal now — folded must report nothing, got %d: %+v", len(fs), fs)
	}
}

// TestRedundantNetMarkerFindings: two same-net flags on ONE wire tree with
// DIFFERENT anchors (live 2026-08-12: 3V3 flags 10 apart on C3's stub — slipped
// duplicate [anchors differ], marker-overlap [graze under eps] and bridge-check
// [electrically fine]). Same-tree same-net ≥2 = redundant; distinct trees or
// distinct nets stay clean.
func TestRedundantNetMarkerFindings(t *testing.T) {
	wires := []schGroupWire{
		{ID: "w1", Points: []float64{270, 555, 270, 605}}, // C3:1 stub, both flags sit on it
		{ID: "w2", Points: []float64{500, 100, 540, 100}}, // unrelated tree
	}
	comps := []layoutComp{
		{ID: "fA", ComponentType: "netflag", Net: "3V3", X: 270, Y: 595, AnchorAvailable: true},
		{ID: "fB", ComponentType: "netflag", Net: "3V3", X: 270, Y: 605, AnchorAvailable: true},
		{ID: "fC", ComponentType: "netflag", Net: "GND", X: 270, Y: 555, AnchorAvailable: true}, // same tree, different net → clean
		{ID: "fD", ComponentType: "netflag", Net: "3V3", X: 500, Y: 100, AnchorAvailable: true}, // different tree → clean
	}
	fs := redundantNetMarkerFindings(comps, wires)
	if len(fs) != 1 {
		t.Fatalf("expected exactly one redundant group, got %d: %+v", len(fs), fs)
	}
	f := fs[0]
	if f.Type != "redundant-net-marker" || f.MarkerNet != "3V3" || f.SuggestKeepId != "fA" ||
		len(f.SuggestDeleteIds) != 1 || f.SuggestDeleteIds[0] != "fB" {
		t.Fatalf("unexpected finding: %+v", f)
	}
	// Clean board: single flag per (tree, net).
	if got := redundantNetMarkerFindings(comps[1:], wires); len(got) != 0 {
		t.Fatalf("clean scene must have no findings, got %+v", got)
	}
}

// eps 1.0 的容差契约(2026-08-17):引脚节距 10 的 IC 列上相邻 netport 标签必然
// 恰好竖叠 1 单位(标签高 11),任何 offset/stagger 都消不掉 —— 判据给不出能
// 执行的下一步就该容忍;≥2 单位的真实叠必须照报。
func TestMarkerOverlap_PitchFontGrazeTolerated(t *testing.T) {
	mk := func(id string, minY, maxY float64) layoutComp {
		return layoutComp{ID: id, ComponentType: "netlabel", Net: "N" + id,
			BBox: &layoutBBox{MinX: 0, MinY: minY, MaxX: 70, MaxY: maxY}}
	}
	// 竖向恰叠 1(节距 10、高 11 的字体现实)→ 容忍。
	graze := []layoutComp{mk("a", 0, 11), mk("b", 10, 21)}
	if got := markerOverlapFindings(graze, schMarkerOverlapEps); len(got) != 0 {
		t.Fatalf("1 单位竖叠该容忍,报了 %d 条", len(got))
	}
	// 竖叠 2 → 真实叠,必须报(容差不许吞掉真问题)。
	real := []layoutComp{mk("a", 0, 11), mk("b", 9, 20)}
	if got := markerOverlapFindings(real, schMarkerOverlapEps); len(got) != 1 {
		t.Fatalf("2 单位竖叠该报 1 条,报了 %d 条", len(got))
	}
}
