package app

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// ── layout-lint: mechanical placement check ─────────────────────────────────
//
// The overlap problem reported from real use ("元件覆盖在一起") is fundamentally a
// missing-feedback problem: the agent placed components but had no ground truth
// for whether they collided. `sch layout-lint` is that ground truth — it pulls
// every component's rendered bbox and runs cheap pairwise geometry in Go, so the
// place→verify→adjust loop has a quantified input instead of an eyeball.

// layoutBBox is a component's rendered extent in native EasyEDA schematic
// canvas units (0.01 inch, y-up).
type layoutBBox struct {
	MinX float64 `json:"minX"`
	MinY float64 `json:"minY"`
	MaxX float64 `json:"maxX"`
	MaxY float64 `json:"maxY"`
}

// layoutPin is one pin's number + coordinate in native EasyEDA schematic canvas
// units (0.01 inch, y-up). Used by the pin-coincidence check: two pins from
// DIFFERENT components landing on the same point is an implicit short (any
// wire/stub through that point ties the two nets) that bbox-only overlap
// detection cannot see. See issue #63.
type layoutPin struct {
	Number string
	X      float64
	Y      float64
}

// layoutComp is the minimal per-component shape layout-lint reasons about.
type layoutComp struct {
	ID            string
	Designator    string
	ComponentType string // "part" | "sheet" | "netflag" | "netport" | … (from the connector)
	// Net is the marker's net name (netflag/netport/netlabel carry it). Used by the
	// duplicate-net-marker rule to avoid merging two same-anchor markers of DIFFERENT
	// nets. See issue #146.
	Net string
	// X, Y is the component anchor (for a marker, its connection anchor). The
	// duplicate-net-marker rule quantizes this so coincident markers hash together
	// even with sub-unit float drift, without depending on a bbox (present on the
	// active page only). See issue #146.
	X, Y            float64
	AnchorAvailable bool // both x/y were present, numeric, and finite
	BBox            *layoutBBox
	Pins            []layoutPin
	PinsAvailable   bool // true when the connector confirmed the pins read succeeded
	PinsProofKnown  bool // true only for the explicit pinsAvailable connector contract
	GeometryErrors  []string
}

// schLayoutPartType is the componentType of a real placed device. Only these
// participate in placement geometry by default. The drawing sheet / title block
// (componentType "sheet") spans the whole page, so including it false-flags an
// overlap against nearly every component; the various flag/label/port primitives
// (netflag/netport/netlabel/…) are likewise not physical parts. See issue #13.
const schLayoutPartType = "part"

// filterLayoutComps keeps only real parts unless includeNonParts is set. It
// returns the kept slice plus the count of excluded non-part primitives (sheet,
// netflag, netport, …) so the report can disclose what was skipped. Components
// with an empty componentType are KEPT — an older connector build that doesn't
// emit the field must not have every component silently dropped.
func filterLayoutComps(comps []layoutComp, includeNonParts bool) (kept []layoutComp, skipped int) {
	if includeNonParts {
		return comps, 0
	}
	kept = make([]layoutComp, 0, len(comps))
	for _, c := range comps {
		if c.ComponentType == "" || c.ComponentType == schLayoutPartType {
			kept = append(kept, c)
			continue
		}
		skipped++
	}
	return kept, skipped
}

// layoutFinding is one pairwise issue (overlap, tight spacing, or pin coincidence).
type layoutFinding struct {
	Type string  `json:"type"` // "overlap" | "spacing" | "pin-coincidence"
	A    string  `json:"a"`    // designator (or id)
	B    string  `json:"b"`
	OvX  float64 `json:"overlapX,omitempty"` // overlap extent, overlap only
	OvY  float64 `json:"overlapY,omitempty"`
	Gap  float64 `json:"gap,omitempty"` // edge-to-edge gap, spacing only
	// pin-coincidence only: the two colliding pins and their shared point.
	APin string  `json:"aPin,omitempty"`
	BPin string  `json:"bPin,omitempty"`
	X    float64 `json:"x,omitempty"`
	Y    float64 `json:"y,omitempty"`
	Dist float64 `json:"dist,omitempty"` // pin-to-pin distance, pin-coincidence only
}

// MarshalJSON uses the finding type to emit every applicable numeric field even
// when its value is zero. Schema v1's blanket omitempty made gap=0, dist=0, or a
// coordinate on an axis indistinguishable from "not applicable".
func (f layoutFinding) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": f.Type, "a": f.A}
	switch f.Type {
	case "overlap":
		out["b"], out["overlapX"], out["overlapY"] = f.B, f.OvX, f.OvY
	case "spacing":
		out["b"], out["gap"] = f.B, f.Gap
	case "pin-coincidence":
		out["b"], out["aPin"], out["bPin"] = f.B, f.APin, f.BPin
		out["x"], out["y"], out["dist"] = f.X, f.Y, f.Dist
	case "off-grid":
		out["x"], out["y"] = f.X, f.Y
	case "zone-violation":
		out["b"], out["x"], out["y"] = f.B, f.X, f.Y
	default:
		if f.B != "" {
			out["b"] = f.B
		}
		if f.OvX != 0 {
			out["overlapX"] = f.OvX
		}
		if f.OvY != 0 {
			out["overlapY"] = f.OvY
		}
		if f.Gap != 0 {
			out["gap"] = f.Gap
		}
		if f.APin != "" {
			out["aPin"] = f.APin
		}
		if f.BPin != "" {
			out["bPin"] = f.BPin
		}
		if f.X != 0 {
			out["x"] = f.X
		}
		if f.Y != 0 {
			out["y"] = f.Y
		}
		if f.Dist != 0 {
			out["dist"] = f.Dist
		}
	}
	return json.Marshal(out)
}

// layoutReport is the full normalized result of a layout-lint run.
type layoutReport struct {
	SchemaVersion   int             `json:"schemaVersion,omitempty"`
	OK              bool            `json:"ok"`
	Strict          bool            `json:"strict,omitempty"`
	MinGap          float64         `json:"minGap"`
	Total           int             `json:"componentCount"`
	WithBBox        int             `json:"withBBox"`
	SkippedNonParts int             `json:"skippedNonParts,omitempty"`
	NoBBox          []string        `json:"noBBox,omitempty"`
	Overlaps        []layoutFinding `json:"overlaps"`
	TightPairs      []layoutFinding `json:"tightSpacing"`
	PinCoincidences []layoutFinding `json:"pinCoincidences"`
	// MeasurementUnit applies to MinGap/OvX/OvY/Gap/Dist. X/Y remain native
	// schematic coordinates so findings can be fed back into placement tools;
	// CoordinateUnit also applies to AnchorGridRaw.
	MeasurementUnit string          `json:"measurementUnit,omitempty"`
	CoordinateUnit  string          `json:"coordinateUnit,omitempty"`
	AnchorGridRaw   float64         `json:"anchorGridRaw"`
	AnchorGridUnit  string          `json:"anchorGridUnit,omitempty"`
	GridViolations  []layoutFinding `json:"gridViolations,omitempty"`
	// ZoneViolations are claimed parts sitting outside their `sch zones` claim.
	// They are advisory in default mode and fail the report under --strict.
	ZoneViolations  []layoutFinding `json:"zoneViolations,omitempty"`
	ZoneCheckStatus string          `json:"zoneCheckStatus"`
	ZoneCheckError  string          `json:"zoneCheckError,omitempty"`
	UncheckedPins   []string        `json:"uncheckedPins,omitempty"`
	UnprovenPins    []string        `json:"unprovenPins,omitempty"`
	InvalidGeometry []string        `json:"invalidGeometry,omitempty"`
	PinEps          float64         `json:"pinEps"`
	Summary         string          `json:"summary"`
}

// analyzeLayout is the pure core: given components and thresholds in native
// schematic canvas units, return every overlapping and too-close pair.
// Deterministic ordering keeps output and tests stable. Kept free of I/O for
// unit-testing.
func analyzeLayout(comps []layoutComp, minGap, pinEps float64) layoutReport {
	rep := layoutReport{MinGap: minGap, PinEps: pinEps, Total: len(comps)}

	withBBox := make([]layoutComp, 0, len(comps))
	for _, c := range comps {
		if c.BBox != nil {
			withBBox = append(withBBox, c)
		} else {
			rep.NoBBox = append(rep.NoBBox, label(c))
		}
	}
	rep.WithBBox = len(withBBox)
	sort.Strings(rep.NoBBox)

	for i := 0; i < len(withBBox); i++ {
		for j := i + 1; j < len(withBBox); j++ {
			a, b := withBBox[i], withBBox[j]
			ox, oy, overlap := overlapExtent(*a.BBox, *b.BBox)
			la, lb := label(a), label(b)
			// Order the pair labels for a stable, readable A↔B.
			if lb < la {
				la, lb = lb, la
			}
			if overlap {
				rep.Overlaps = append(rep.Overlaps, layoutFinding{Type: "overlap", A: la, B: lb, OvX: round2(ox), OvY: round2(oy)})
				continue
			}
			if gap := rectGap(*a.BBox, *b.BBox); gap < minGap {
				rep.TightPairs = append(rep.TightPairs, layoutFinding{Type: "spacing", A: la, B: lb, Gap: round2(gap)})
			}
		}
	}

	rep.PinCoincidences = detectPinCoincidence(comps, pinEps)
	rep.UncheckedPins = uncheckedPinGeometry(comps)

	sortFindings(rep.Overlaps)
	sortFindings(rep.TightPairs)
	sortFindings(rep.PinCoincidences)
	sort.Strings(rep.UncheckedPins)

	// Both bbox overlaps and pin coincidences are hard errors: a coincident pin
	// pair is an implicit short even when the bboxes never touch.
	rep.OK = len(rep.Overlaps) == 0 && len(rep.PinCoincidences) == 0
	rep.Summary = fmt.Sprintf("%d components (%d with bbox): %d overlap, %d tight (<%.2f schematic units), %d pin-coincidence",
		rep.Total, rep.WithBBox, len(rep.Overlaps), len(rep.TightPairs), minGap, len(rep.PinCoincidences))
	return rep
}

func uncheckedPinGeometry(comps []layoutComp) []string {
	var out []string
	for _, c := range comps {
		if c.PinsAvailable {
			continue
		}
		out = append(out, label(c))
	}
	sort.Strings(out)
	return out
}

// unprovenPinGeometry identifies legacy connector responses that returned a
// pins array but did not explicitly say whether the SDK read succeeded. Older
// builds used `(pins ?? [])` and swallowed exceptions, so an empty array could
// mean either "zero pins" or "pin read failed". Strict mode must not call that
// ambiguity a completed electrical-geometry proof.
func unprovenPinGeometry(comps []layoutComp) []string {
	var out []string
	for _, c := range comps {
		if c.PinsAvailable && !c.PinsProofKnown {
			out = append(out, label(c))
		}
	}
	sort.Strings(out)
	return out
}

func invalidLayoutGeometry(comps []layoutComp) []string {
	var out []string
	for _, c := range comps {
		for _, issue := range c.GeometryErrors {
			out = append(out, fmt.Sprintf("%s: %s", label(c), issue))
		}
	}
	sort.Strings(out)
	return out
}

// detectOffGridAnchors reports parts whose placement anchor is not on the
// schematic connection grid. Pins are fixed offsets from that anchor, so an
// off-grid anchor makes downstream orthogonal connect_pin stubs unreliable even
// when the rendered bboxes do not overlap.
func detectOffGridAnchors(comps []layoutComp, grid, eps float64) []layoutFinding {
	if grid <= 0 {
		return nil
	}
	var out []layoutFinding
	for _, c := range comps {
		if !c.AnchorAvailable {
			continue
		}
		dx := math.Abs(c.X - math.Round(c.X/grid)*grid)
		dy := math.Abs(c.Y - math.Round(c.Y/grid)*grid)
		if dx <= eps && dy <= eps {
			continue
		}
		out = append(out, layoutFinding{
			Type: "off-grid",
			A:    label(c),
			X:    round2(c.X),
			Y:    round2(c.Y),
		})
	}
	return out
}

// applyLayoutStrictGate upgrades every finding that prevents a complete proof
// of placement readiness from warning to command failure. Default mode remains
// backward compatible and only hard-fails physical overlap/pin coincidence.
func applyLayoutStrictGate(rep *layoutReport, strict bool) {
	rep.Strict = strict
	if !strict {
		return
	}
	if len(rep.TightPairs) > 0 ||
		len(rep.GridViolations) > 0 ||
		len(rep.ZoneViolations) > 0 ||
		len(rep.NoBBox) > 0 ||
		len(rep.UncheckedPins) > 0 ||
		len(rep.UnprovenPins) > 0 ||
		len(rep.InvalidGeometry) > 0 ||
		rep.ZoneCheckStatus == "unavailable" {
		rep.OK = false
	}
}

// layoutReportInMM converts only physical distances to millimetres for the CLI.
// Finding X/Y values intentionally remain in native schematic coordinates: they
// identify an actionable point on the canvas rather than a distance.
func layoutReportInMM(rep layoutReport) layoutReport {
	// layoutReport contains slices, so a struct copy alone would mutate the raw
	// report's backing arrays and make a second conversion multiply values again.
	rep.Overlaps = append([]layoutFinding(nil), rep.Overlaps...)
	rep.TightPairs = append([]layoutFinding(nil), rep.TightPairs...)
	rep.PinCoincidences = append([]layoutFinding(nil), rep.PinCoincidences...)
	rep.MinGap = schematicUnitsToMM(rep.MinGap)
	if rep.PinEps >= 0 {
		rep.PinEps = schematicUnitsToMM(rep.PinEps)
	}
	for i := range rep.Overlaps {
		rep.Overlaps[i].OvX = schematicUnitsToMM(rep.Overlaps[i].OvX)
		rep.Overlaps[i].OvY = schematicUnitsToMM(rep.Overlaps[i].OvY)
	}
	for i := range rep.TightPairs {
		rep.TightPairs[i].Gap = schematicUnitsToMM(rep.TightPairs[i].Gap)
	}
	for i := range rep.PinCoincidences {
		rep.PinCoincidences[i].Dist = schematicUnitsToMM(rep.PinCoincidences[i].Dist)
	}
	rep.SchemaVersion = 2
	rep.MeasurementUnit = "mm"
	rep.CoordinateUnit = "0.01inch"
	rep.AnchorGridUnit = "0.01inch"
	rep.Summary = fmt.Sprintf("strict=%t: %d components (%d with bbox): %d overlap, %d tight (<%.2fmm), %d pin-coincidence, %d off-grid, %d zone-violation, %d unchecked-bbox, %d unchecked-pins, %d unproven-pins, %d invalid-geometry; zoneCheck=%s",
		rep.Strict, rep.Total, rep.WithBBox, len(rep.Overlaps), len(rep.TightPairs), rep.MinGap,
		len(rep.PinCoincidences), len(rep.GridViolations), len(rep.ZoneViolations),
		len(rep.NoBBox), len(rep.UncheckedPins), len(rep.UnprovenPins),
		len(rep.InvalidGeometry), rep.ZoneCheckStatus)
	return rep
}

func validateLayoutDistanceFlag(name string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return fmt.Errorf("%s must be a finite value >= 0mm", name)
	}
	return nil
}

// detectPinCoincidence finds pins from DIFFERENT components that land on the same
// point (distance <= eps). It buckets pins by quantized coordinate so only pins in
// the same/adjacent cell are compared — avoiding a full O(n²) scan over every pin
// pair on the page. Same-component pins never collide with each other (a symbol's
// own pins are expected to sit at fixed offsets). See issue #63.
func detectPinCoincidence(comps []layoutComp, eps float64) []layoutFinding {
	if eps < 0 {
		return nil // negative eps disables the check (internal bbox-only callers)
	}
	// Cell size: eps>0 buckets by eps so neighbors fall within ±1 cell; eps==0
	// (strict equality) buckets on an exact grid so identical points collide.
	cell := eps
	if cell <= 0 {
		cell = 1e-6
	}
	type keyed struct {
		comp int
		pin  layoutPin
	}
	buckets := make(map[[2]int64][]keyed)
	key := func(x, y float64) [2]int64 {
		return [2]int64{int64(math.Floor(x / cell)), int64(math.Floor(y / cell))}
	}
	var out []layoutFinding
	seen := make(map[[2]int]bool) // dedupe pair by (compA,compB) — one finding per part pair
	for ci := range comps {
		for _, p := range comps[ci].Pins {
			k := key(p.X, p.Y)
			// Compare against pins already placed in this and neighbouring cells.
			for dx := int64(-1); dx <= 1; dx++ {
				for dy := int64(-1); dy <= 1; dy++ {
					for _, other := range buckets[[2]int64{k[0] + dx, k[1] + dy}] {
						if other.comp == ci {
							continue // same component: skip
						}
						if math.Hypot(p.X-other.pin.X, p.Y-other.pin.Y) > eps {
							continue
						}
						lo, hi := other.comp, ci
						if lo > hi {
							lo, hi = hi, lo
						}
						if seen[[2]int{lo, hi}] {
							continue
						}
						seen[[2]int{lo, hi}] = true
						la, lb := label(comps[other.comp]), label(comps[ci])
						pa, pb := other.pin, p
						if lb < la {
							la, lb = lb, la
							pa, pb = p, other.pin
						}
						out = append(out, layoutFinding{
							Type: "pin-coincidence", A: la, B: lb,
							APin: pa.Number, BPin: pb.Number,
							X: round2(pa.X), Y: round2(pa.Y),
							Dist: round2(math.Hypot(p.X-other.pin.X, p.Y-other.pin.Y)),
						})
					}
				}
			}
			buckets[k] = append(buckets[k], keyed{comp: ci, pin: p})
		}
	}
	return out
}

// overlapExtent reports the intersection extent of two bboxes and whether they
// actually overlap (positive area on both axes). Touching edges do NOT count.
func overlapExtent(a, b layoutBBox) (ox, oy float64, overlap bool) {
	ox = math.Min(a.MaxX, b.MaxX) - math.Max(a.MinX, b.MinX)
	oy = math.Min(a.MaxY, b.MaxY) - math.Max(a.MinY, b.MinY)
	return ox, oy, ox > 0 && oy > 0
}

// rectGap is the edge-to-edge separation between two non-overlapping bboxes.
func rectGap(a, b layoutBBox) float64 {
	dx := math.Max(0, math.Max(a.MinX-b.MaxX, b.MinX-a.MaxX))
	dy := math.Max(0, math.Max(a.MinY-b.MaxY, b.MinY-a.MaxY))
	return math.Hypot(dx, dy)
}

// label picks the most identifying name. A freshly placed part carries an
// UNASSIGNED designator ("C?", "R?", or empty) — useless for telling two apart —
// so fall back to the primitiveId in that case.
func label(c layoutComp) string {
	d := c.Designator
	if d != "" && !strings.HasSuffix(d, "?") {
		return d
	}
	if c.ID != "" {
		if d != "" {
			return d + "@" + c.ID // e.g. "C?@129274a01919b064"
		}
		return c.ID
	}
	return d
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func sortFindings(f []layoutFinding) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].A != f[j].A {
			return f[i].A < f[j].A
		}
		return f[i].B < f[j].B
	})
}

// runLayoutLint fetches components with bbox, analyzes, renders, and returns a
// non-nil error when overlaps exist so the command exits non-zero (gate-able).
func runLayoutLint(cfg *appConfig, window string, minGap, pinEps float64, allPages, asJSON, includeNonParts, strict bool, stdout, stderr io.Writer) error {
	if err := validateLayoutDistanceFlag("--min-gap", minGap); err != nil {
		return err
	}
	if err := validateLayoutDistanceFlag("--pin-eps", pinEps); err != nil {
		return err
	}
	if strict && allPages {
		return fmt.Errorf("layout-lint: --strict cannot be combined with --all-pages: inactive pages expose shallow/cross-page geometry, so lint each page after `easyeda doc switch <page>`")
	}
	if strict && includeNonParts {
		return fmt.Errorf("layout-lint: --strict cannot be combined with --include-non-parts: sheet frames and net markers are not placement bodies and would create false geometry failures")
	}
	payload := map[string]any{"includeBBox": true, "includePins": true}
	if allPages {
		payload["allPages"] = true
	}
	res, err := requestAction(cfg, "schematic.components.list", window, payload)
	if err != nil {
		return err
	}

	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return perr
	}
	realParts, _ := filterLayoutComps(comps, false)
	parts, skipped := filterLayoutComps(comps, includeNonParts)
	rep := analyzeLayout(parts, mmToSchematicUnits(minGap), mmToSchematicUnits(pinEps))
	if includeNonParts {
		// --include-non-parts expands bbox/spacing inspection only. Sheet/text/
		// markers do not have device pins and must not make the strict pin proof
		// fail or create meaningless pin-coincidence comparisons.
		rep.PinCoincidences = detectPinCoincidence(realParts, mmToSchematicUnits(pinEps))
		sortFindings(rep.PinCoincidences)
		rep.UncheckedPins = uncheckedPinGeometry(realParts)
		rep.OK = len(rep.Overlaps) == 0 && len(rep.PinCoincidences) == 0
	}
	rep.SkippedNonParts = skipped
	rep.AnchorGridRaw = schAnchorGrid
	rep.GridViolations = detectOffGridAnchors(realParts, schAnchorGrid, acCoordEps)
	rep.UnprovenPins = unprovenPinGeometry(realParts)
	rep.InvalidGeometry = invalidLayoutGeometry(realParts)
	sortFindings(rep.GridViolations)

	// Zone checks are explicit in schema v2. No configured claims is a valid
	// "not-configured" state; an unreadable state or configured claims without a
	// live sheet is "unavailable" and fails --strict instead of silently passing.
	zones, _, zerr := loadSchZoneClaims(cfg, window)
	sheet := sheetBBoxOf(comps)
	switch {
	case zerr != nil:
		rep.ZoneCheckStatus = "unavailable"
		rep.ZoneCheckError = zerr.Error()
	case len(zones) == 0:
		rep.ZoneCheckStatus = "not-configured"
	case sheet == nil:
		rep.ZoneCheckStatus = "unavailable"
		rep.ZoneCheckError = "schematic zone claims are configured but the active page has no readable sheet bbox"
	default:
		rep.ZoneCheckStatus = "checked"
		rep.ZoneViolations = findSchZoneViolations(zones, *sheet, realParts)
	}
	applyLayoutStrictGate(&rep, strict)
	rep = layoutReportInMM(rep)

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		renderLayoutReport(rep, stdout)
	}

	if !rep.OK {
		return fmt.Errorf("layout-lint: %d overlap(s), %d pin-coincidence(s), %d tight pair(s), %d off-grid anchor(s), %d zone violation(s), %d unchecked bbox(s), %d unchecked pin-set(s), %d unproven pin-set(s), %d invalid geometry value(s), zone-check=%s",
			len(rep.Overlaps), len(rep.PinCoincidences), len(rep.TightPairs),
			len(rep.GridViolations), len(rep.ZoneViolations), len(rep.NoBBox),
			len(rep.UncheckedPins), len(rep.UnprovenPins), len(rep.InvalidGeometry),
			rep.ZoneCheckStatus)
	}
	return nil
}

// parseLayoutComps extracts the minimal layoutComp slice from a components.list
// result map (components: [{primitiveId, designator, bbox:{minX..}}...]).
func parseLayoutComps(result map[string]any) ([]layoutComp, error) {
	raw, ok := result["components"].([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected components.list result: missing components array")
	}
	if countRaw, present := result["count"]; present {
		count, countOK := finiteFloat(countRaw)
		if !countOK || count != math.Trunc(count) || int(count) != len(raw) {
			return nil, fmt.Errorf("unexpected components.list result: count=%v does not prove the %d returned component record(s)", countRaw, len(raw))
		}
	}
	out := make([]layoutComp, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unexpected components.list result: components[%d] is not an object", i)
		}
		c := layoutComp{
			ID:            asString(m["primitiveId"]),
			Designator:    asString(m["designator"]),
			ComponentType: asString(m["componentType"]),
			Net:           asString(m["net"]),
		}
		x, xOK := finiteFloat(m["x"])
		y, yOK := finiteFloat(m["y"])
		if xOK && yOK {
			c.X, c.Y, c.AnchorAvailable = x, y, true
		} else {
			c.GeometryErrors = append(c.GeometryErrors, "anchor x/y missing, non-numeric, or non-finite")
		}
		if c.ID == "" {
			c.GeometryErrors = append(c.GeometryErrors, "primitiveId is empty")
		}

		if bboxRaw, present := m["bbox"]; present {
			bm, bboxObject := bboxRaw.(map[string]any)
			if !bboxObject {
				c.GeometryErrors = append(c.GeometryErrors, "bbox is not an object")
			} else {
				minX, okMinX := finiteFloat(bm["minX"])
				minY, okMinY := finiteFloat(bm["minY"])
				maxX, okMaxX := finiteFloat(bm["maxX"])
				maxY, okMaxY := finiteFloat(bm["maxY"])
				if !okMinX || !okMinY || !okMaxX || !okMaxY ||
					maxX <= minX || maxY <= minY {
					c.GeometryErrors = append(c.GeometryErrors, "bbox edges are missing, non-finite, or non-positive")
				} else {
					c.BBox = &layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
				}
			}
		}

		pinsRaw, pinsPresent := m["pins"]
		pins, pinsArray := pinsRaw.([]any)
		explicitAvailable, proofPresent := m["pinsAvailable"].(bool)
		c.PinsProofKnown = proofPresent
		if pinErr := strings.TrimSpace(asString(m["pinsError"])); pinErr != "" {
			c.GeometryErrors = append(c.GeometryErrors, "pin read failed: "+pinErr)
			explicitAvailable = false
			proofPresent = true
			c.PinsProofKnown = true
		}
		switch {
		case proofPresent && !explicitAvailable:
			c.PinsAvailable = false
			if pinsPresent && pinsArray {
				c.GeometryErrors = append(c.GeometryErrors, "pinsAvailable=false conflicts with a pins array")
			}
		case proofPresent && explicitAvailable && !pinsArray:
			c.GeometryErrors = append(c.GeometryErrors, "pinsAvailable=true but pins is not an array")
		case proofPresent && explicitAvailable:
			c.PinsAvailable = true
		case !proofPresent && pinsArray:
			// Backward-compatible parsing only. Strict mode records this in
			// UnprovenPins because legacy connectors swallowed pin-read failures.
			c.PinsAvailable = true
		}
		if c.PinsAvailable {
			pinsValid := true
			for pi, pp := range pins {
				pm, ok := pp.(map[string]any)
				if !ok {
					c.GeometryErrors = append(c.GeometryErrors, fmt.Sprintf("pins[%d] is not an object", pi))
					pinsValid = false
					continue
				}
				px, pxOK := finiteFloat(pm["x"])
				py, pyOK := finiteFloat(pm["y"])
				if !pxOK || !pyOK {
					c.GeometryErrors = append(c.GeometryErrors, fmt.Sprintf("pins[%d] x/y missing, non-numeric, or non-finite", pi))
					pinsValid = false
					continue
				}
				c.Pins = append(c.Pins, layoutPin{
					Number: asString(pm["pinNumber"]),
					X:      px,
					Y:      py,
				})
			}
			if !pinsValid {
				c.PinsAvailable = false
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asFloat(v any) float64 {
	if n, ok := finiteFloat(v); ok {
		return n
	}
	return 0
}

func finiteFloat(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int8:
		f = float64(n)
	case int16:
		f = float64(n)
	case int32:
		f = float64(n)
	case int64:
		f = float64(n)
	case uint:
		f = float64(n)
	case uint8:
		f = float64(n)
	case uint16:
		f = float64(n)
	case uint32:
		f = float64(n)
	case uint64:
		f = float64(n)
	case json.Number:
		var err error
		f, err = n.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}

// renderLayoutReport prints a compact human summary.
func renderLayoutReport(rep layoutReport, w io.Writer) {
	unit := rep.MeasurementUnit
	if unit == "" {
		unit = "schematic units"
	}
	fmt.Fprintf(w, "layout-lint: %d components (%d with bbox), min-gap %.2f %s\n", rep.Total, rep.WithBBox, rep.MinGap, unit)
	for _, f := range rep.Overlaps {
		fmt.Fprintf(w, "  ERROR  overlap  %s ↔ %s   (overlap %.2f × %.2f %s)\n", f.A, f.B, f.OvX, f.OvY, unit)
	}
	for _, f := range rep.PinCoincidences {
		fmt.Fprintf(w, "  ERROR  pin-coincidence  %s:%s ↔ %s:%s   (both at %.2f,%.2f schematic units — implicit short)\n",
			f.A, f.APin, f.B, f.BPin, f.X, f.Y)
	}
	softSeverity := "WARN"
	if rep.Strict {
		softSeverity = "ERROR"
	}
	for _, f := range rep.TightPairs {
		fmt.Fprintf(w, "  %s  spacing  %s ↔ %s   (gap %.2f %s < %.2f %s)\n", softSeverity, f.A, f.B, f.Gap, unit, rep.MinGap, unit)
	}
	for _, f := range rep.GridViolations {
		fmt.Fprintf(w, "  %s  off-grid  %s at %.2f,%.2f (anchor must land on %.0f-unit grid)\n",
			softSeverity, f.A, f.X, f.Y, rep.AnchorGridRaw)
	}
	for _, f := range rep.ZoneViolations {
		fmt.Fprintf(w, "  %s  zone-violation  %s at %.0f,%.0f outside its claimed zone %s — S0 拍板的分区没有落实(`sch zones status` 看认领)\n",
			softSeverity, f.A, f.X, f.Y, f.B)
	}
	if rep.SkippedNonParts > 0 {
		fmt.Fprintf(w, "  note: %d non-part primitive(s) excluded (sheet/title-frame, netflag/netport/…); pass --include-non-parts to include\n", rep.SkippedNonParts)
	}
	if len(rep.NoBBox) > 0 {
		fmt.Fprintf(w, "  %s  no-bbox  %d component(s) NOT CHECKED (no bbox — likely non-active-page shallow data under --all-pages; `doc switch` to that page to lint it): %v\n",
			softSeverity, len(rep.NoBBox), rep.NoBBox)
	}
	if len(rep.UncheckedPins) > 0 {
		fmt.Fprintf(w, "  %s  no-pins  %d component(s) NOT CHECKED for pin coincidence (connector omitted the pins array): %v\n",
			softSeverity, len(rep.UncheckedPins), rep.UncheckedPins)
	}
	if len(rep.UnprovenPins) > 0 {
		fmt.Fprintf(w, "  %s  unproven-pins  %d component(s) came from a legacy connector that did not distinguish an empty pin set from a failed SDK read: %v\n",
			softSeverity, len(rep.UnprovenPins), rep.UnprovenPins)
	}
	if len(rep.InvalidGeometry) > 0 {
		fmt.Fprintf(w, "  %s  invalid-geometry  %d malformed component geometry value(s): %v\n",
			softSeverity, len(rep.InvalidGeometry), rep.InvalidGeometry)
	}
	if rep.ZoneCheckStatus == "unavailable" {
		fmt.Fprintf(w, "  %s  zone-check unavailable: %s\n", softSeverity, rep.ZoneCheckError)
	}
	skipCaveat := ""
	if len(rep.NoBBox) > 0 {
		skipCaveat = fmt.Sprintf("; %d component(s) NOT checked (skipped ≠ confirmed clear)", len(rep.NoBBox))
	}
	if rep.OK {
		fmt.Fprintf(w, "✓ placement gate passed; %d tight pair(s), %d off-grid anchor(s), %d zone violation(s), zone-check=%s%s\n",
			len(rep.TightPairs), len(rep.GridViolations), len(rep.ZoneViolations), rep.ZoneCheckStatus, skipCaveat)
	} else {
		fmt.Fprintf(w, "✗ %d overlap(s), %d pin-coincidence(s), %d tight pair(s), %d off-grid anchor(s), %d zone violation(s), %d unchecked pin-set(s), %d unproven pin-set(s), %d invalid geometry value(s), zone-check=%s%s\n",
			len(rep.Overlaps), len(rep.PinCoincidences), len(rep.TightPairs),
			len(rep.GridViolations), len(rep.ZoneViolations), len(rep.UncheckedPins),
			len(rep.UnprovenPins), len(rep.InvalidGeometry), rep.ZoneCheckStatus, skipCaveat)
	}
}
