package app

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// silkZonePart is the rendered component rectangle used by the outline builder.
type silkZonePart struct {
	Designator string
	Rect       cpRect
}

type silkZonePlan struct {
	Name     string        `json:"name"`
	Parts    []string      `json:"parts"`
	Avoided  []string      `json:"avoidedComponents,omitempty"`
	Blocked  []string      `json:"unresolvedObstacles,omitempty"`
	BBox     silkZoneBBox  `json:"bbox"`
	Label    silkZoneLabel `json:"label"`
	Hull     [][2]float64  `json:"hull"`
	Polygons [][]any       `json:"-"`
}

type silkZoneLabel struct {
	Text      string  `json:"text"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Rotation  float64 `json:"rotation"`
	Placement string  `json:"placement"`
	Clear     bool    `json:"clear"`
}

type silkZoneBBox struct {
	MinX float64 `json:"minX"`
	MinY float64 `json:"minY"`
	MaxX float64 `json:"maxX"`
	MaxY float64 `json:"maxY"`
}

// orthogonalEnvelope builds a deterministic stair-step hull. It scans the
// expanded member rectangles in horizontal bands, records the left/right
// envelope in every occupied band, and walks those envelopes into one simple
// orthogonal polygon. Gaps are intentionally bridged: a functional group should
// remain one selectable silk primitive even when its members are separated.
func orthogonalEnvelope(rects []cpRect, margin float64) ([][2]float64, cpRect, error) {
	if len(rects) == 0 {
		return nil, cpRect{}, fmt.Errorf("zone has no placed members")
	}
	ys := make([]float64, 0, len(rects)*2)
	ex := make([]cpRect, 0, len(rects))
	for _, r := range rects {
		r = cpRect{x0: r.x0 - margin, y0: r.y0 - margin, x1: r.x1 + margin, y1: r.y1 + margin}
		ex = append(ex, r)
		ys = append(ys, r.y0, r.y1)
	}
	sort.Float64s(ys)
	ys = uniqueFloats(ys)
	type band struct{ y0, y1, left, right float64 }
	var bands []band
	for i := 0; i+1 < len(ys); i++ {
		y0, y1 := ys[i], ys[i+1]
		if y1-y0 < 1e-6 {
			continue
		}
		mid := (y0 + y1) / 2
		left, right := math.Inf(1), math.Inf(-1)
		for _, r := range ex {
			if mid >= r.y0 && mid <= r.y1 {
				left, right = math.Min(left, r.x0), math.Max(right, r.x1)
			}
		}
		if math.IsInf(left, 1) {
			continue
		}
		if len(bands) > 0 {
			p := &bands[len(bands)-1]
			if almostEqual(p.y1, y0) && almostEqual(p.left, left) && almostEqual(p.right, right) {
				p.y1 = y1
				continue
			}
		}
		bands = append(bands, band{y0, y1, left, right})
	}
	if len(bands) == 0 {
		return nil, cpRect{}, fmt.Errorf("zone hull is degenerate")
	}
	// Bridge empty vertical gaps using the union envelope of adjacent bands.
	for i := 0; i+1 < len(bands); i++ {
		if bands[i].y1 < bands[i+1].y0 {
			bands[i].y1 = bands[i+1].y0
			bands[i].left = math.Min(bands[i].left, bands[i+1].left)
			bands[i].right = math.Max(bands[i].right, bands[i+1].right)
		}
	}
	var pts [][2]float64
	pts = append(pts, [2]float64{bands[0].left, bands[0].y0})
	for _, b := range bands {
		pts = appendStep(pts, b.left, b.y0)
		pts = appendStep(pts, b.left, b.y1)
	}
	last := bands[len(bands)-1]
	pts = appendStep(pts, last.right, last.y1)
	for i := len(bands) - 1; i >= 0; i-- {
		b := bands[i]
		pts = appendStep(pts, b.right, b.y1)
		pts = appendStep(pts, b.right, b.y0)
	}
	pts = appendStep(pts, bands[0].left, bands[0].y0)
	pts = simplifyOrthogonal(pts)
	bb := cpRect{x0: math.Inf(1), y0: math.Inf(1), x1: math.Inf(-1), y1: math.Inf(-1)}
	for _, p := range pts {
		bb.x0, bb.y0 = math.Min(bb.x0, p[0]), math.Min(bb.y0, p[1])
		bb.x1, bb.y1 = math.Max(bb.x1, p[0]), math.Max(bb.y1, p[1])
	}
	return pts, bb, nil
}

// orthogonalPerimeter is the presentation-safe outline: every edge stays on
// the group's outermost bbox, with small two-step corner cuts for a visibly
// non-rectangular shape. Unlike the compact envelope it never enters a gap
// between members, so the frame always reads as an external functional border.
func orthogonalPerimeter(rects []cpRect, corner float64) ([][2]float64, cpRect, error) {
	if len(rects) == 0 {
		return nil, cpRect{}, fmt.Errorf("zone has no placed members")
	}
	bb := cpRect{x0: math.Inf(1), y0: math.Inf(1), x1: math.Inf(-1), y1: math.Inf(-1)}
	for _, r := range rects {
		bb.x0, bb.y0 = math.Min(bb.x0, r.x0), math.Min(bb.y0, r.y0)
		bb.x1, bb.y1 = math.Max(bb.x1, r.x1), math.Max(bb.y1, r.y1)
	}
	c := math.Min(corner, math.Min((bb.x1-bb.x0)/6, (bb.y1-bb.y0)/6))
	if c <= 0 {
		return nil, cpRect{}, fmt.Errorf("zone perimeter is degenerate")
	}
	h := c / 2
	pts := [][2]float64{
		{bb.x0 + c, bb.y0}, {bb.x1 - c, bb.y0},
		{bb.x1 - c, bb.y0 + h}, {bb.x1 - h, bb.y0 + h},
		{bb.x1 - h, bb.y0 + c}, {bb.x1, bb.y0 + c},
		{bb.x1, bb.y1 - c}, {bb.x1 - h, bb.y1 - c},
		{bb.x1 - h, bb.y1 - h}, {bb.x1 - c, bb.y1 - h},
		{bb.x1 - c, bb.y1}, {bb.x0 + c, bb.y1},
		{bb.x0 + c, bb.y1 - h}, {bb.x0 + h, bb.y1 - h},
		{bb.x0 + h, bb.y1 - c}, {bb.x0, bb.y1 - c},
		{bb.x0, bb.y0 + c}, {bb.x0 + h, bb.y0 + c},
		{bb.x0 + h, bb.y0 + h}, {bb.x0 + c, bb.y0 + h},
		{bb.x0 + c, bb.y0},
	}
	return pts, bb, nil
}

func uniqueFloats(in []float64) []float64 {
	var out []float64
	for _, v := range in {
		if len(out) == 0 || !almostEqual(out[len(out)-1], v) {
			out = append(out, v)
		}
	}
	return out
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func appendStep(pts [][2]float64, x, y float64) [][2]float64 {
	if len(pts) == 0 || !almostEqual(pts[len(pts)-1][0], x) || !almostEqual(pts[len(pts)-1][1], y) {
		return append(pts, [2]float64{x, y})
	}
	return pts
}

func simplifyOrthogonal(in [][2]float64) [][2]float64 {
	if len(in) < 4 {
		return in
	}
	out := make([][2]float64, 0, len(in))
	for _, p := range in {
		out = append(out, p)
		for len(out) >= 3 {
			n := len(out)
			a, b, c := out[n-3], out[n-2], out[n-1]
			if (almostEqual(a[0], b[0]) && almostEqual(b[0], c[0])) ||
				(almostEqual(a[1], b[1]) && almostEqual(b[1], c[1])) {
				out = append(out[:n-2], c)
			} else {
				break
			}
		}
	}
	return out
}

// outlinePolygons converts a y-up hull into filled rectangles (one per edge)
// plus corner squares. They are sent together as one PrimitiveImage, so the
// whole functional zone is selectable and deletable as a single object.
func outlinePolygons(hull [][2]float64, bb cpRect, width float64) [][]any {
	half := width / 2
	var out [][]any
	addRect := func(x0, y0, x1, y1 float64) {
		// PrimitiveImage local coordinates: x right, y down from bbox top.
		lx0, lx1 := x0-bb.x0, x1-bb.x0
		ly0, ly1 := bb.y1-y1, bb.y1-y0
		out = append(out, []any{lx0, ly0, "L", lx1, ly0, lx1, ly1, lx0, ly1, lx0, ly0})
	}
	for i := 0; i+1 < len(hull); i++ {
		a, b := hull[i], hull[i+1]
		if almostEqual(a[0], b[0]) {
			addRect(a[0]-half, math.Min(a[1], b[1]), a[0]+half, math.Max(a[1], b[1]))
		} else {
			addRect(math.Min(a[0], b[0]), a[1]-half, math.Max(a[0], b[0]), a[1]+half)
		}
	}
	for _, p := range hull[:len(hull)-1] {
		addRect(p[0]-half, p[1]-half, p[0]+half, p[1]+half)
	}
	return out
}

func parseSilkZoneFlags(items []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, item := range items {
		name, refs, ok := strings.Cut(item, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(refs) == "" {
			return nil, fmt.Errorf("--zone %q: expected NAME=U1,C1,C2", item)
		}
		out[strings.TrimSpace(name)] = normalizeDesignators(strings.Split(refs, ","))
	}
	return out, nil
}

func expandRect(r cpRect, d float64) cpRect {
	return cpRect{x0: r.x0 - d, y0: r.y0 - d, x1: r.x1 + d, y1: r.y1 + d}
}

func segmentHitsRect(a, b [2]float64, r cpRect) bool {
	if almostEqual(a[0], b[0]) {
		x := a[0]
		return x >= r.x0 && x <= r.x1 &&
			math.Max(math.Min(a[1], b[1]), r.y0) <= math.Min(math.Max(a[1], b[1]), r.y1)
	}
	y := a[1]
	return y >= r.y0 && y <= r.y1 &&
		math.Max(math.Min(a[0], b[0]), r.x0) <= math.Min(math.Max(a[0], b[0]), r.x1)
}

func hullHitsRect(hull [][2]float64, r cpRect) bool {
	for i := 0; i+1 < len(hull); i++ {
		if segmentHitsRect(hull[i], hull[i+1], r) {
			return true
		}
	}
	return false
}

func rectsOverlap(a, b cpRect) bool {
	return a.x0 < b.x1 && a.x1 > b.x0 && a.y0 < b.y1 && a.y1 > b.y0
}

func chooseZoneLabel(name string, bb, board cpRect, parts map[string]silkZonePart, fontSize, lineWidth, edgeMargin float64) silkZoneLabel {
	text := strings.ReplaceAll(strings.TrimSpace(name), "_", " / ")
	w := math.Max(fontSize, float64(len([]rune(text)))*fontSize*0.58)
	h := fontSize * 1.4
	type row struct {
		name string
		y    float64
	}
	rows := []row{
		{"top-outside", bb.y1 + lineWidth},
		{"top-inside", bb.y1 - h - lineWidth},
		{"bottom-inside", bb.y0 + lineWidth},
		{"bottom-outside", bb.y0 - h - lineWidth},
	}
	var xs []float64
	left, right := bb.x0+lineWidth, bb.x1-lineWidth-w
	if right >= left {
		xs = append(xs, (left+right)/2, left, right)
		for x := left; x <= right; x += 20 {
			xs = append(xs, x)
		}
	}
	for _, r := range rows {
		for _, x := range xs {
			lr := cpRect{x0: x, y0: r.y, x1: x + w, y1: r.y + h}
			if lr.x0 < board.x0+edgeMargin || lr.x1 > board.x1-edgeMargin ||
				lr.y0 < board.y0+edgeMargin || lr.y1 > board.y1-edgeMargin {
				continue
			}
			clear := true
			for _, p := range parts {
				if rectsOverlap(lr, expandRect(p.Rect, lineWidth)) {
					clear = false
					break
				}
			}
			if clear {
				return silkZoneLabel{Text: text, X: x, Y: r.y, Rotation: 0, Placement: r.name, Clear: true}
			}
		}
	}
	type verticalCandidate struct {
		name, side string
		x, y       float64
		rotation   float64
		rect       cpRect
	}
	midY := (bb.y0 + bb.y1) / 2
	verticals := []verticalCandidate{
		{"right-inside", "right", bb.x1 - lineWidth, midY - w/2, 90, cpRect{x0: bb.x1 - lineWidth - h, y0: midY - w/2, x1: bb.x1 - lineWidth, y1: midY + w/2}},
		{"left-inside", "left", bb.x0 + lineWidth, midY + w/2, -90, cpRect{x0: bb.x0 + lineWidth, y0: midY - w/2, x1: bb.x0 + lineWidth + h, y1: midY + w/2}},
		{"right-outside", "right", bb.x1 + lineWidth, midY + w/2, -90, cpRect{x0: bb.x1 + lineWidth, y0: midY - w/2, x1: bb.x1 + lineWidth + h, y1: midY + w/2}},
		{"left-outside", "left", bb.x0 - lineWidth, midY - w/2, 90, cpRect{x0: bb.x0 - lineWidth - h, y0: midY - w/2, x1: bb.x0 - lineWidth, y1: midY + w/2}},
	}
	for _, c := range verticals {
		if c.rect.x0 < board.x0+edgeMargin || c.rect.x1 > board.x1-edgeMargin ||
			c.rect.y0 < board.y0+edgeMargin || c.rect.y1 > board.y1-edgeMargin {
			continue
		}
		clear := true
		for _, p := range parts {
			if rectsOverlap(c.rect, expandRect(p.Rect, lineWidth)) {
				clear = false
				break
			}
		}
		if clear {
			return silkZoneLabel{Text: text, X: c.x, Y: c.y, Rotation: c.rotation, Placement: c.name, Clear: true}
		}
	}
	// Deterministic fallback is reported as non-clear in dry-run/output.
	return silkZoneLabel{Text: text, X: math.Max(board.x0+edgeMargin, (bb.x0+bb.x1-w)/2), Y: bb.y1 + lineWidth, Rotation: 0, Placement: "fallback", Clear: false}
}

// avoidZoneObstacles grows the envelope around any non-member component that a
// candidate boundary would cross. Absorbed obstacles are NOT added to the
// functional membership; they merely bend the outside path around themselves.
func avoidZoneObstacles(memberRects []cpRect, memberSet map[string]bool, all map[string]silkZonePart, clearance float64, limit int) ([][2]float64, cpRect, []string, []string, error) {
	work := append([]cpRect(nil), memberRects...)
	absorbed := map[string]bool{}
	var hull [][2]float64
	var bb cpRect
	var err error
	for iter := 0; iter <= limit; iter++ {
		hull, bb, err = orthogonalEnvelope(work, 0)
		if err != nil {
			return nil, cpRect{}, nil, nil, err
		}
		var hits []string
		for d, p := range all {
			if memberSet[d] || absorbed[d] {
				continue
			}
			if hullHitsRect(hull, expandRect(p.Rect, clearance)) {
				hits = append(hits, d)
			}
		}
		sort.Strings(hits)
		if len(hits) == 0 {
			break
		}
		if iter == limit {
			return hull, bb, sortedBoolKeys(absorbed), hits, nil
		}
		for _, d := range hits {
			absorbed[d] = true
			work = append(work, expandRect(all[d].Rect, clearance))
		}
	}
	return hull, bb, sortedBoolKeys(absorbed), nil, nil
}

func avoidPerimeterObstacles(memberRects []cpRect, memberSet map[string]bool, all map[string]silkZonePart, clearance, corner float64, limit int) ([][2]float64, cpRect, []string, []string, error) {
	work := append([]cpRect(nil), memberRects...)
	absorbed := map[string]bool{}
	var hull [][2]float64
	var bb cpRect
	var err error
	for iter := 0; iter <= limit; iter++ {
		hull, bb, err = orthogonalPerimeter(work, corner)
		if err != nil {
			return nil, cpRect{}, nil, nil, err
		}
		var hits []string
		for d, p := range all {
			if memberSet[d] || absorbed[d] {
				continue
			}
			if hullHitsRect(hull, expandRect(p.Rect, clearance)) {
				hits = append(hits, d)
			}
		}
		sort.Strings(hits)
		if len(hits) == 0 {
			break
		}
		if iter == limit {
			return hull, bb, sortedBoolKeys(absorbed), hits, nil
		}
		for _, d := range hits {
			absorbed[d] = true
			work = append(work, expandRect(all[d].Rect, clearance))
		}
	}
	return hull, bb, sortedBoolKeys(absorbed), nil, nil
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fetchSilkZoneParts(cfg *appConfig, window string) (map[string]silkZonePart, error) {
	res, err := requestAction(cfg, "pcb.components.list", window, map[string]any{"includeBBox": true})
	if err != nil {
		return nil, err
	}
	raw, _ := res.Result["components"].([]any)
	out := map[string]silkZonePart{}
	for _, ri := range raw {
		cm, ok := ri.(map[string]any)
		if !ok {
			continue
		}
		bb, ok := cm["bbox"].(map[string]any)
		if !ok {
			continue
		}
		x0, ok0 := asFloatOK(bb["minX"])
		y0, ok1 := asFloatOK(bb["minY"])
		x1, ok2 := asFloatOK(bb["maxX"])
		y1, ok3 := asFloatOK(bb["maxY"])
		if !ok0 || !ok1 || !ok2 || !ok3 {
			continue
		}
		d := strings.ToUpper(asString(cm["designator"]))
		out[d] = silkZonePart{Designator: d, Rect: cpRect{x0: x0, y0: y0, x1: x1, y1: y1}}
	}
	return out, nil
}

func newPcbSilkZoneOutlineCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var zoneFlags []string
	var fromClaims, dryRun, avoidComponents bool
	var margin, edgeMargin, obstacleClearance, lineWidth float64
	var layer, maxAvoid int
	var shape string
	var addLabel bool
	var labelSize, labelLineWidth float64
	c := &cobra.Command{
		Use:   "silk-zone-outline",
		Short: "Auto-draw grouped orthogonal functional-zone outlines on silkscreen",
		Args:  cobra.NoArgs,
		Example: `  easyeda pcb silk-zone-outline --zone "POWER=U1,U64,C2,C6,C7,C8" --margin 40 --dry-run
  easyeda pcb silk-zone-outline --from-claims --margin 40`,
		RunE: func(cmd *cobra.Command, args []string) error {
			groups, err := parseSilkZoneFlags(zoneFlags)
			if err != nil {
				return err
			}
			if fromClaims {
				claims, _, err := loadZoneClaims(cfg, *window)
				if err != nil {
					return err
				}
				for name, claim := range claims {
					if claim != nil {
						groups[name] = append([]string(nil), claim.Parts...)
					}
				}
			}
			if len(groups) == 0 {
				return fmt.Errorf("pass --zone NAME=U1,C1,... or --from-claims")
			}
			parts, err := fetchSilkZoneParts(cfg, *window)
			if err != nil {
				return err
			}
			board, err := fetchBoardRect(cfg, *window)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(groups))
			allClaimed := map[string]bool{}
			for name := range groups {
				names = append(names, name)
				for _, d := range groups[name] {
					allClaimed[d] = true
				}
			}
			sort.Strings(names)
			var plans []silkZonePlan
			for _, name := range names {
				var rects []cpRect
				var missing []string
				// When several zones are planned together, components assigned to
				// another zone must not be absorbed as obstacles. Otherwise one
				// boundary expands into the next zone and cascades across the board.
				// Unclaimed components are still avoided normally.
				memberSet := map[string]bool{}
				for d := range allClaimed {
					memberSet[d] = true
				}
				for _, d := range groups[name] {
					p, ok := parts[d]
					if !ok {
						missing = append(missing, d)
						continue
					}
					memberSet[d] = true
					rects = append(rects, expandRect(p.Rect, margin))
				}
				if len(missing) > 0 {
					return fmt.Errorf("zone %q: component(s) not placed: %s", name, strings.Join(missing, ","))
				}
				var hull [][2]float64
				var bb cpRect
				var avoided, blocked []string
				switch shape {
				case "perimeter":
					if avoidComponents {
						hull, bb, avoided, blocked, err = avoidPerimeterObstacles(rects, memberSet, parts, obstacleClearance+lineWidth/2, margin, maxAvoid)
					} else {
						hull, bb, err = orthogonalPerimeter(rects, margin)
					}
				case "envelope":
					if avoidComponents {
						hull, bb, avoided, blocked, err = avoidZoneObstacles(rects, memberSet, parts, obstacleClearance+lineWidth/2, maxAvoid)
					} else {
						hull, bb, err = orthogonalEnvelope(rects, 0)
					}
				default:
					return fmt.Errorf("--shape must be perimeter or envelope")
				}
				if err != nil {
					return fmt.Errorf("zone %q: %w", name, err)
				}
				for i := range hull {
					hull[i][0] = math.Max(board.x0+edgeMargin, math.Min(board.x1-edgeMargin, hull[i][0]))
					hull[i][1] = math.Max(board.y0+edgeMargin, math.Min(board.y1-edgeMargin, hull[i][1]))
				}
				hull = simplifyOrthogonal(hull)
				bb = hullBBox(hull)
				label := chooseZoneLabel(name, bb, board, parts, labelSize, labelLineWidth, edgeMargin)
				plans = append(plans, silkZonePlan{
					Name: name, Parts: groups[name],
					Avoided: avoided, Blocked: blocked,
					BBox:     silkZoneBBox{MinX: bb.x0, MinY: bb.y0, MaxX: bb.x1, MaxY: bb.y1},
					Label:    label,
					Hull:     hull,
					Polygons: outlinePolygons(hull, bb, lineWidth),
				})
			}
			if dryRun {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"dryRun": true, "margin": margin, "lineWidth": lineWidth, "zones": plans})
			}
			var created []map[string]any
			for _, p := range plans {
				res, err := requestAction(cfg, "pcb.silk.import_svg", *window, map[string]any{
					"polygons": p.Polygons, "x": p.BBox.MinX, "y": p.BBox.MaxY,
					"width": p.BBox.MaxX - p.BBox.MinX, "height": p.BBox.MaxY - p.BBox.MinY,
					"layer": layer,
				})
				if err != nil {
					return fmt.Errorf("create zone %q: %w", p.Name, err)
				}
				item := map[string]any{"name": p.Name, "parts": p.Parts, "bbox": p.BBox, "outline": res.Result}
				if addLabel {
					if !p.Label.Clear {
						return fmt.Errorf("zone %q: no collision-free label position (use --label=false or adjust the layout/margin)", p.Name)
					}
					labelRes, err := requestAction(cfg, "pcb.silk.add", *window, map[string]any{
						"text": p.Label.Text, "x": p.Label.X, "y": p.Label.Y,
						"layer": layer, "fontSize": labelSize, "lineWidth": labelLineWidth, "rotation": p.Label.Rotation,
					})
					if err != nil {
						return fmt.Errorf("create zone %q label: %w", p.Name, err)
					}
					item["label"] = labelRes.Result
				}
				created = append(created, item)
				fmt.Fprintf(stderr, "✓ %s: %d part(s), grouped silk outline%s created\n", p.Name, len(p.Parts), map[bool]string{true: " + label", false: ""}[addLabel])
			}
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{"created": created})
		},
	}
	c.Flags().StringArrayVar(&zoneFlags, "zone", nil, `functional group: NAME=U1,C1,C2 (repeatable)`)
	c.Flags().BoolVar(&fromClaims, "from-claims", false, "read groups from persisted `pcb zones` claims")
	c.Flags().StringVar(&shape, "shape", "perimeter", "outline shape: perimeter (outer-only) or envelope (compact stair-step)")
	c.Flags().Float64Var(&margin, "margin", 40, "outline expansion around member bboxes (mil)")
	c.Flags().Float64Var(&edgeMargin, "edge-margin", 10, "minimum outline inset from board edge (mil)")
	c.Flags().BoolVar(&avoidComponents, "avoid-components", true, "bend the perimeter around non-member component bodies")
	c.Flags().Float64Var(&obstacleClearance, "obstacle-clearance", 12, "clearance from non-member component bboxes (mil)")
	c.Flags().IntVar(&maxAvoid, "max-avoid", 8, "maximum obstacle-expansion passes per zone")
	c.Flags().Float64Var(&lineWidth, "line-width", 6, "filled silkscreen outline width (mil)")
	c.Flags().IntVar(&layer, "layer", 3, "silk layer: 3=top, 4=bottom")
	c.Flags().BoolVar(&addLabel, "label", true, "add a functional-zone name beside the outline")
	c.Flags().Float64Var(&labelSize, "label-size", 32, "zone-label font height (mil)")
	c.Flags().Float64Var(&labelLineWidth, "label-line-width", 5, "zone-label stroke width (mil)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "compute hulls without modifying the PCB")
	return c
}

func hullBBox(hull [][2]float64) cpRect {
	bb := cpRect{x0: math.Inf(1), y0: math.Inf(1), x1: math.Inf(-1), y1: math.Inf(-1)}
	for _, p := range hull {
		bb.x0, bb.y0 = math.Min(bb.x0, p[0]), math.Min(bb.y0, p[1])
		bb.x1, bb.y1 = math.Max(bb.x1, p[0]), math.Max(bb.y1, p[1])
	}
	return bb
}
