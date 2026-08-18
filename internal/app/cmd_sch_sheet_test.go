package app

import "testing"

func boolPtr(b bool) *bool { return &b }

func hasWarningContaining(warnings []string, substr string) bool {
	for _, w := range warnings {
		if len(w) >= len(substr) && containsStr(w, substr) {
			return true
		}
	}
	return false
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Known template: an A-series landscape sheet (aspect ≈ √2 ≈ 1.414). The
// title-block keep-out must be the bottom-right corner, tagged known-template.
func TestDeriveSheetGeometry_KnownTemplateA4Landscape(t *testing.T) {
	// 1160 / 820 ≈ 1.4146 → matches a-series-landscape.
	sheet := bb(0, 0, 1160, 820)
	g := deriveSheetGeometry(sheet, boolPtr(true))

	if g.Sheet.Template != "a-series-landscape" {
		t.Fatalf("expected a-series-landscape, got %q", g.Sheet.Template)
	}
	if g.TitleBlock.Source != sheetSourceKnownTemplate {
		t.Fatalf("expected source %q, got %q", sheetSourceKnownTemplate, g.TitleBlock.Source)
	}
	if g.TitleBlock.BBox == nil {
		t.Fatal("expected a title-block bbox")
	}
	tb := g.TitleBlock.BBox
	// 60% width × 24% height (calibrated against the real 3.2.148 A4 title block —
	// see defaultTitleBlockRatio; height re-calibrated 0.2→0.24 on 2026-08-11 when
	// the rendered table top measured ≈ y190 on A4 vs the 0.2 estimate's 165),
	// anchored to the visual bottom-right corner — on the y-UP canvas that is the
	// high-x/LOW-y corner (probe-proven 2026-07-19; the old high-y form protected
	// the visual top-right, the wrong corner).
	if tb.MaxX != 1160 || tb.MinY != 0 {
		t.Errorf("keep-out must anchor to the high-x/low-y (visual bottom-right) corner, got maxX %.2f minY %.2f", tb.MaxX, tb.MinY)
	}
	if tb.MinX != round2(1160-0.6*1160) || tb.MaxY != round2(0+0.24*820) {
		t.Errorf("unexpected keep-out extent: minX %.2f maxY %.2f", tb.MinX, tb.MaxY)
	}
	if len(g.Keepouts) != 1 || g.Keepouts[0].Name != "titleBlock" || !g.Keepouts[0].Hard {
		t.Fatalf("expected one hard titleBlock keepout, got %+v", g.Keepouts)
	}
	if hasWarningContaining(g.Warnings, "fallback") {
		t.Errorf("known template must not warn about fallback: %v", g.Warnings)
	}
}

// A non-A4 A-series landscape (A3: A4 × √2) still matches the aspect, but the
// title-block ratio is calibrated for A4 only — so it emits a best-effort keep-out
// with DOWNGRADED provenance + a calibration warning. Issue #172: the real title
// block is a FIXED-SIZE table, so the estimate must be the fixed A4-calibrated
// size anchored bottom-right — NOT the A4 fraction scaled up with the sheet
// (which over-reserved ~60% of an A3's width and false-flagged mid-sheet parts).
func TestDeriveSheetGeometry_NonA4LandscapeWarns(t *testing.T) {
	sheet := bb(0, 0, 1654, 1167) // A3 landscape ≈ A4 × √2, aspect ≈ 1.417
	g := deriveSheetGeometry(sheet, boolPtr(true))
	if g.Sheet.Template != "a-series-landscape" {
		t.Fatalf("A3 should still match a-series-landscape aspect, got %q", g.Sheet.Template)
	}
	if g.TitleBlock.BBox == nil {
		t.Fatal("expected a best-effort title-block bbox on a non-A4 sheet")
	}
	if g.TitleBlock.Source != sheetSourceFallback {
		t.Errorf("non-A4 landscape must downgrade provenance to fallback, got %q", g.TitleBlock.Source)
	}
	if !hasWarningContaining(g.Warnings, "calibrated for A4 landscape only") {
		t.Errorf("expected an A4-only calibration warning, got %v", g.Warnings)
	}
	tb := g.TitleBlock.BBox
	// Fixed-size estimate: A4-calibrated table (0.6×1170 wide, 0.24×825 high),
	// anchored to the visual bottom-right (high-x/low-y) corner.
	wantMinX := round2(1654 - titleBlockFixedW)
	wantMaxY := round2(0 + titleBlockFixedH)
	if tb.MaxX != 1654 || tb.MinY != 0 || tb.MinX != wantMinX || tb.MaxY != wantMaxY {
		t.Errorf("non-A4 keep-out must be the FIXED A4-calibrated size anchored bottom-right; got [%.2f,%.2f → %.2f,%.2f], want [%.2f,0 → 1654,%.2f]",
			tb.MinX, tb.MinY, tb.MaxX, tb.MaxY, wantMinX, wantMaxY)
	}
}

// Issue #172's exact reproduction sheet (1655×1170): the A4-fraction estimate put
// the keep-out's left edge at x=662 (~40% of the page) and false-flagged parts at
// x≈700–770 mid-sheet. The fixed-size estimate must leave those clear.
func TestDeriveSheetGeometry_Issue172FixedSizeKeepout(t *testing.T) {
	sheet := bb(0, 0, 1655, 1170)
	g := deriveSheetGeometry(sheet, boolPtr(true))
	if g.TitleBlock.Source != sheetSourceFallback {
		t.Fatalf("1655×1170 is non-A4 → fallback provenance, got %q", g.TitleBlock.Source)
	}
	tb := g.TitleBlock.BBox
	if tb == nil {
		t.Fatal("expected a keep-out")
	}
	if tb.MinX != round2(1655-titleBlockFixedW) {
		t.Errorf("keep-out left edge = %.2f, want %.2f (fixed-size, bottom-right anchored)", tb.MinX, 1655-titleBlockFixedW)
	}
	// The issue's false-positive parts sit at x≈700–770 — they must be OUTSIDE.
	if tb.MinX <= 780 {
		t.Errorf("keep-out still swallows mid-sheet parts (minX %.2f ≤ 780) — the issue #172 over-estimate", tb.MinX)
	}
}

// A sheet smaller than the fixed table clamps the estimate to the sheet instead of
// producing a keep-out that pokes past the left/top edge.
func TestDeriveSheetGeometry_FixedSizeClampsToSmallSheet(t *testing.T) {
	sheet := bb(0, 0, 500, 150) // unmatched aspect, smaller than 702×198
	g := deriveSheetGeometry(sheet, boolPtr(true))
	tb := g.TitleBlock.BBox
	if tb == nil {
		t.Fatal("expected a keep-out")
	}
	if tb.MinX < 0 || tb.MaxY > 150 {
		t.Errorf("fixed-size estimate must clamp to the sheet, got [%.2f,%.2f → %.2f,%.2f]", tb.MinX, tb.MinY, tb.MaxX, tb.MaxY)
	}
}

// Unknown template: a square-ish sheet matches no known aspect → fallback ratio,
// keep-out still emitted, but provenance downgraded and a warning surfaced.
func TestDeriveSheetGeometry_UnknownTemplate(t *testing.T) {
	sheet := bb(0, 0, 1000, 1000) // aspect 1.0 → no match
	g := deriveSheetGeometry(sheet, boolPtr(true))

	if g.TitleBlock.Source != sheetSourceFallback {
		t.Fatalf("expected source %q, got %q", sheetSourceFallback, g.TitleBlock.Source)
	}
	if g.TitleBlock.BBox == nil {
		t.Fatal("fallback must still emit a keep-out (generic ratio)")
	}
	if len(g.Keepouts) != 1 {
		t.Fatalf("expected one keepout in fallback, got %d", len(g.Keepouts))
	}
	if !hasWarningContaining(g.Warnings, "did not match a known template") {
		t.Errorf("expected an unrecognized-template warning, got %v", g.Warnings)
	}
}

// A hidden title block must emit NO keep-out and say so.
func TestDeriveSheetGeometry_HiddenTitleBlock(t *testing.T) {
	sheet := bb(0, 0, 1160, 820)
	g := deriveSheetGeometry(sheet, boolPtr(false))

	if g.TitleBlock.BBox != nil {
		t.Errorf("hidden title block must not produce a bbox, got %+v", g.TitleBlock.BBox)
	}
	if len(g.Keepouts) != 0 {
		t.Errorf("hidden title block must emit no keepouts, got %d", len(g.Keepouts))
	}
	if g.TitleBlock.Source != sheetSourceNone {
		t.Errorf("expected source %q, got %q", sheetSourceNone, g.TitleBlock.Source)
	}
	if !hasWarningContaining(g.Warnings, "hidden") {
		t.Errorf("expected a hidden-title-block warning, got %v", g.Warnings)
	}
}

// No sheet primitive → no geometry, a warning, no false precision.
func TestDeriveSheetGeometry_NoSheet(t *testing.T) {
	g := deriveSheetGeometry(nil, nil)

	if g.Sheet.BBox != nil {
		t.Errorf("no sheet must yield a nil sheet bbox, got %+v", g.Sheet.BBox)
	}
	if g.TitleBlock.BBox != nil || len(g.Keepouts) != 0 {
		t.Errorf("no sheet must yield no keep-out, got tb=%+v keepouts=%d", g.TitleBlock.BBox, len(g.Keepouts))
	}
	if !hasWarningContaining(g.Warnings, "no sheet primitive found") {
		t.Errorf("expected a no-sheet warning, got %v", g.Warnings)
	}
}

// Unknown visibility (showTitleBlock not reported) assumes visible and warns.
func TestDeriveSheetGeometry_UnknownVisibility(t *testing.T) {
	sheet := bb(0, 0, 1160, 820)
	g := deriveSheetGeometry(sheet, nil)

	if g.TitleBlock.BBox == nil {
		t.Fatal("unknown visibility must assume visible and emit a keep-out")
	}
	if !hasWarningContaining(g.Warnings, "visibility unknown") {
		t.Errorf("expected a visibility-unknown warning, got %v", g.Warnings)
	}
}

// Degenerate sheet bbox (non-positive dimensions) → no keep-out, warning.
func TestDeriveSheetGeometry_DegenerateBBox(t *testing.T) {
	sheet := bb(100, 100, 100, 100) // zero width/height
	g := deriveSheetGeometry(sheet, boolPtr(true))

	if g.TitleBlock.BBox != nil || len(g.Keepouts) != 0 {
		t.Errorf("degenerate bbox must not derive a keep-out, got %+v", g.TitleBlock.BBox)
	}
	if !hasWarningContaining(g.Warnings, "non-positive dimensions") {
		t.Errorf("expected a degenerate-bbox warning, got %v", g.Warnings)
	}
}

// The autoconnect keep-out must stay consistent with the shared derivation:
// same corner rect for a known sheet, provisional when no sheet.
func TestTitleBlockKeepout_DelegatesToDerive(t *testing.T) {
	sheet := bb(0, 0, 1160, 820)
	box, provisional := titleBlockKeepout(sheet)
	if provisional {
		t.Fatal("a known sheet must not be provisional")
	}
	g := deriveSheetGeometry(sheet, nil)
	if box == nil || g.TitleBlock.BBox == nil || *box != *g.TitleBlock.BBox {
		t.Errorf("titleBlockKeepout must match deriveSheetGeometry; got %+v vs %+v", box, g.TitleBlock.BBox)
	}

	if nilBox, prov := titleBlockKeepout(nil); nilBox != nil || !prov {
		t.Errorf("no sheet → nil keep-out + provisional, got box=%+v prov=%v", nilBox, prov)
	}
}

// titleBlockKeepoutWithSource must return the same bbox as the plain helper plus
// the derivation provenance, so check rules can grade confidence (issue #172).
func TestTitleBlockKeepoutWithSource(t *testing.T) {
	a4 := bb(0, 0, 1160, 820)
	box, src := titleBlockKeepoutWithSource(a4)
	plain, _ := titleBlockKeepout(a4)
	if box == nil || plain == nil || *box != *plain {
		t.Errorf("WithSource bbox must match titleBlockKeepout: %+v vs %+v", box, plain)
	}
	if src != sheetSourceKnownTemplate {
		t.Errorf("A4 source = %q, want %q", src, sheetSourceKnownTemplate)
	}

	nonA4 := bb(0, 0, 1655, 1170)
	if _, src := titleBlockKeepoutWithSource(nonA4); src != sheetSourceFallback {
		t.Errorf("non-A4 source = %q, want %q", src, sheetSourceFallback)
	}

	if box, src := titleBlockKeepoutWithSource(nil); box != nil || src != sheetSourceNone {
		t.Errorf("no sheet → nil + %q, got %+v %q", sheetSourceNone, box, src)
	}
}
