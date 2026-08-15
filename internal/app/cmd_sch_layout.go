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
	// Rotation is the stored primitive rotation when the connector reported one
	// (markers carry it; the reversed-net-flag rule reads it against the
	// orientation truth table). Nil when absent/non-numeric.
	Rotation       *float64
	BBox           *layoutBBox
	Pins           []layoutPin
	PinsAvailable  bool // true when the connector confirmed the pins read succeeded
	PinsProofKnown bool // true only for the explicit pinsAvailable connector contract
	GeometryErrors []string
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
	case "out-of-sheet":
		// OvX/OvY 复用为「超出量」(每轴超出图纸可用区多少),X/Y 是锚点。
		out["x"], out["y"], out["overlapX"], out["overlapY"] = f.X, f.Y, f.OvX, f.OvY
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
	// OutOfSheet 是 bbox 越出图纸可用区(边框内缩 sheetEdgeMinGap)的器件。
	// 此前**没有任何判据抓这个**:出图的件照样连线、照样 netlist 对账通过,
	// 只是印不出来(实测 block-apply 把件放到 x=-20 / y=880 而图纸 0..825)。
	// 与 zone-violation 同档:默认 advisory,--strict 才判失败。
	OutOfSheet       []layoutFinding `json:"outOfSheet,omitempty"`
	SheetCheckStatus string          `json:"sheetCheckStatus"`
	SheetCheckError  string          `json:"sheetCheckError,omitempty"`
	ZoneCheckError   string          `json:"zoneCheckError,omitempty"`
	UncheckedPins    []string        `json:"uncheckedPins,omitempty"`
	UnprovenPins     []string        `json:"unprovenPins,omitempty"`
	InvalidGeometry  []string        `json:"invalidGeometry,omitempty"`
	PinEps           float64         `json:"pinEps"`
	Summary          string          `json:"summary"`
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
		len(rep.OutOfSheet) > 0 ||
		rep.SheetCheckStatus == "unavailable" ||
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
	// out-of-sheet 复用 OvX/OvY 表示「超出量」,必须与 overlap 同口径换算 ——
	// 否则同一份 JSON 里同名键两种单位,而报告自报 measurementUnit:"mm"。
	for i := range rep.OutOfSheet {
		rep.OutOfSheet[i].OvX = schematicUnitsToMM(rep.OutOfSheet[i].OvX)
		rep.OutOfSheet[i].OvY = schematicUnitsToMM(rep.OutOfSheet[i].OvY)
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
	rep.Summary = fmt.Sprintf("strict=%t: %d components (%d with bbox): %d overlap, %d tight (<%.2fmm), %d pin-coincidence, %d off-grid, %d zone-violation, %d out-of-sheet, %d unchecked-bbox, %d unchecked-pins, %d unproven-pins, %d invalid-geometry; zoneCheck=%s sheetCheck=%s",
		rep.Strict, rep.Total, rep.WithBBox, len(rep.Overlaps), len(rep.TightPairs), rep.MinGap,
		len(rep.PinCoincidences), len(rep.GridViolations), len(rep.ZoneViolations), len(rep.OutOfSheet),
		len(rep.NoBBox), len(rep.UncheckedPins), len(rep.UnprovenPins),
		len(rep.InvalidGeometry), rep.ZoneCheckStatus, rep.SheetCheckStatus)
	return rep
}

func validateLayoutDistanceFlag(name string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return fmt.Errorf("%s must be a finite value >= 0mm", name)
	}
	return nil
}

// detectOutOfSheet flags parts whose rendered bbox leaves the printed sheet's
// usable area (the frame inset by `inset`).
//
// 为什么需要它:出图纸的件**照样连线、照样 netlist 对账通过**,下游没有任何判据
// 会响 —— 它只是印不出来。实测 block-apply 把 J_USB 放到 x=-20、把 R6 放到
// y=880(图纸 0..825),layout-lint 当时全绿(issue #180)。
//
// 判据是 bbox 而不是锚点:锚点在框内、body 探出框外一样是出图(块 apply 事后那条
// warning 比的就是锚点,所以漏报)。没有 bbox 的件跳过 —— 那是 NoBBox 的职责。
// 纯函数。
func detectOutOfSheet(comps []layoutComp, sheet layoutBBox, inset float64) []layoutFinding {
	usable := layoutBBox{
		MinX: sheet.MinX + inset, MinY: sheet.MinY + inset,
		MaxX: sheet.MaxX - inset, MaxY: sheet.MaxY - inset,
	}
	var out []layoutFinding
	for _, c := range comps {
		if c.BBox == nil {
			continue
		}
		b := *c.BBox
		// 每轴的超出量:两侧都可能出,取更大的那侧(0 = 该轴没出)。
		ovX := math.Max(usable.MinX-b.MinX, b.MaxX-usable.MaxX)
		ovY := math.Max(usable.MinY-b.MinY, b.MaxY-usable.MaxY)
		if ovX <= 0 && ovY <= 0 {
			continue
		}
		out = append(out, layoutFinding{
			Type: "out-of-sheet",
			A:    label(c),
			X:    round2(c.X), Y: round2(c.Y),
			OvX: round2(math.Max(0, ovX)), OvY: round2(math.Max(0, ovY)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].A < out[j].A })
	return out
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
	rep, err := collectLayoutLint(cfg, window, minGap, pinEps, allPages, includeNonParts, strict)
	if err != nil {
		return err
	}

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
		// 每一个能让 OK=false 的判据都必须出现在这句里 —— 少一个就会出现
		// 「所有计数都是 0 却非零退出」的不可归因失败(记忆:真机验的是报告读起来对不对)。
		return fmt.Errorf("layout-lint: %d overlap(s), %d pin-coincidence(s), %d tight pair(s), %d off-grid anchor(s), %d zone violation(s), %d out-of-sheet, %d unchecked bbox(s), %d unchecked pin-set(s), %d unproven pin-set(s), %d invalid geometry value(s), zone-check=%s sheet-check=%s",
			len(rep.Overlaps), len(rep.PinCoincidences), len(rep.TightPairs),
			len(rep.GridViolations), len(rep.ZoneViolations), len(rep.OutOfSheet), len(rep.NoBBox),
			len(rep.UncheckedPins), len(rep.UnprovenPins), len(rep.InvalidGeometry),
			rep.ZoneCheckStatus, rep.SheetCheckStatus)
	}
	return nil
}

// collectLayoutLint gathers the geometry and produces the normalized report
// WITHOUT rendering or gating. Split out of runLayoutLint so `sch gate` can run
// layout-lint as one stage of an aggregate report (the flag-validation and the
// exit-code contract stay in runLayoutLint — a stage decides its own verdict).
func collectLayoutLint(cfg *appConfig, window string, minGap, pinEps float64, allPages, includeNonParts, strict bool) (layoutReport, error) {
	var zero layoutReport
	readCfg, readWindow, docUUID := cfg, window, ""
	if !allPages {
		pinnedCfg, win, pinnedUUID, err := pinZonePage(cfg, window)
		if err != nil {
			return zero, err
		}
		readCfg, readWindow, docUUID = pinnedCfg, win, pinnedUUID
	}
	payload := map[string]any{"includeBBox": true, "includePins": true}
	if allPages {
		payload["allPages"] = true
	}
	var res *actionResult
	var err error
	if allPages {
		res, err = requestAction(readCfg, "schematic.components.list", readWindow, payload)
	} else {
		res, err = requestAutolayoutAction(readCfg, "schematic.components.list", readWindow,
			payload, docUUID, "read layout-lint geometry")
	}
	if err != nil {
		return zero, err
	}

	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return zero, perr
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
	var zones map[string]*schZoneClaim
	var zerr error
	if !allPages {
		zones, _, zerr = loadSchZoneClaimsForPage(readCfg, readWindow, docUUID)
	}
	sheet := sheetBBoxOf(comps)
	switch {
	case allPages:
		rep.ZoneCheckStatus = "unavailable"
		rep.ZoneCheckError = "all-pages geometry cannot be matched authoritatively to page-scoped schematic zone claims; lint each page separately"
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
		rep.ZoneViolations = findSchZoneViolations(zones, *sheet, realParts,
			loadDrawnZoneRects(readCfg, readWindow, docUUID))
	}
	// 出图纸判据(issue #180 Fix C):与 zone 同档诚实披露 —— 读不到图纸 bbox 就
	// 说 unavailable,绝不因为"没检查"而显得干净。allPages 下逐页图纸无法对应,
	// 与 zone-check 同理不判。sheet 取自**未过滤**的 comps(filterLayoutComps 会
	// 把 componentType=="sheet" 滤掉)。
	switch {
	case allPages:
		rep.SheetCheckStatus = "unavailable"
		rep.SheetCheckError = "--all-pages 下各页图纸边框无法与器件一一对应;逐页 lint(`doc switch` 后单页跑)才能判出图"
	case sheet == nil:
		rep.SheetCheckStatus = "unavailable"
		rep.SheetCheckError = "本页读不到图纸边框(sheet)bbox —— 无法判断器件是否越出图纸;`easyeda doc switch` 到该原理图页后重跑"
	default:
		rep.SheetCheckStatus = "checked"
		rep.OutOfSheet = detectOutOfSheet(realParts, *sheet, sheetEdgeMinGap)
	}
	applyLayoutStrictGate(&rep, strict)
	return layoutReportInMM(rep), nil
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
		if rot, rotOK := finiteFloat(m["rotation"]); rotOK {
			c.Rotation = &rot
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
	for _, f := range rep.OutOfSheet {
		fmt.Fprintf(w, "  %s  out-of-sheet  %s at %.0f,%.0f 越出图纸可用区(图框内缩 %.0f 单位)%s — 该件照样连线、netlist 也对得上,但印不出来\n",
			softSeverity, f.A, f.X, f.Y, sheetEdgeMinGap, layoutOverExtent(f, unit))
	}
	if rep.SheetCheckStatus == "unavailable" {
		fmt.Fprintf(w, "  %s  sheet-check unavailable: %s\n", softSeverity, rep.SheetCheckError)
	}
	if rep.SkippedNonParts > 0 {
		// **这条不能只是 note**:排除掉的正是 netflag/netport,而「标签压标签 / 标签压
		// 器件」恰恰只发生在它们身上。此前这里一路打印 `✓ placement gate passed`,
		// 而同一张画布上有 11 处标签重叠 —— 用户一眼看见,工具全绿。判据看不见的东西,
		// 必须在判据自己的输出里说清楚它没看,并指出谁看得见。
		fmt.Fprintf(w, "  %s  未判定 %d 个非 part 图元(图框 / netflag / netport / 文字)——\n"+
			"        本命令只判器件本体,**标签之间、标签压器件的重叠不在其中**;\n"+
			"        跑 `easyeda sch clusters --strict` 才看得到(--include-non-parts 是粗筛,\n"+
			"        它会把旗和它自己的桩线也算成一处重叠)\n", softSeverity, rep.SkippedNonParts)
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
		fmt.Fprintf(w, "✓ placement gate passed; %d tight pair(s), %d off-grid anchor(s), %d zone violation(s), %d out-of-sheet, zone-check=%s sheet-check=%s%s\n",
			len(rep.TightPairs), len(rep.GridViolations), len(rep.ZoneViolations), len(rep.OutOfSheet),
			rep.ZoneCheckStatus, rep.SheetCheckStatus, skipCaveat)
	} else {
		fmt.Fprintf(w, "✗ %d overlap(s), %d pin-coincidence(s), %d tight pair(s), %d off-grid anchor(s), %d zone violation(s), %d out-of-sheet, %d unchecked pin-set(s), %d unproven pin-set(s), %d invalid geometry value(s), zone-check=%s sheet-check=%s%s\n",
			len(rep.Overlaps), len(rep.PinCoincidences), len(rep.TightPairs),
			len(rep.GridViolations), len(rep.ZoneViolations), len(rep.OutOfSheet), len(rep.UncheckedPins),
			len(rep.UnprovenPins), len(rep.InvalidGeometry), rep.ZoneCheckStatus, rep.SheetCheckStatus, skipCaveat)
	}
}

// layoutOverExtent 把 out-of-sheet 的超出量渲染成「(右侧超出 3.2mm)」这类人话。
// 只打非零轴 —— 一个件通常只在一两个方向越线,四轴全打是噪声。
func layoutOverExtent(f layoutFinding, unit string) string {
	switch {
	case f.OvX > 0 && f.OvY > 0:
		return fmt.Sprintf(" 横向超出 %.2f %s、纵向超出 %.2f %s", f.OvX, unit, f.OvY, unit)
	case f.OvX > 0:
		return fmt.Sprintf(" 横向超出 %.2f %s", f.OvX, unit)
	case f.OvY > 0:
		return fmt.Sprintf(" 纵向超出 %.2f %s", f.OvY, unit)
	}
	return ""
}
