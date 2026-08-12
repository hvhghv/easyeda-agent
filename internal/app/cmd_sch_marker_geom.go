package app

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// ── sch check: Go-side geometric marker rules (issues #146 / #147 / #148) ─────
//
// The connector's electrical schematic.check reconstructs floating pins, net-name
// mismatches, wire crossings, etc. — but it CANNOT see three classes of purely
// geometric defect that leave the netlist clean yet the drawing wrong/unreadable:
//
//	duplicate-net-marker  #146  two+ markers of the SAME kind+net at the SAME
//	                            anchor — the residue of a partial `sch autoconnect`
//	                            retry. The connector even collapses coincident
//	                            same-name flags to one net, so every electrical
//	                            rule (net-marker-mismatch, dangling-wire, bridge)
//	                            reports clean while the page carries a stacked pair.
//	titleblock-overlap    #147  a part/marker whose bbox intrudes the A4 title-block
//	                            keep-out (图签/明细表). autoconnect can drop a netport
//	                            there and layout-lint (part-only) + the electrical
//	                            check both pass.
//	marker-overlap        #148  a marker body positively overlaps a part or another
//	                            marker — electrically fine, visually unreadable.
//
// All three are pure bbox/anchor geometry over the SAME components.list layout-lint
// already pulls, so they live Go-side (no connector rebuild) and are table-testable
// against the issues' real coordinates.

// schMarkerTypes are the connector componentType values for net markers.
func isSchMarker(t string) bool {
	switch t {
	case "netflag", "netport", "netlabel", "short_symbol":
		return true
	}
	return false
}

const (
	// markerAnchorQuant quantizes a marker anchor so two coincident markers with
	// sub-unit float drift (e.g. 1384.9999999 vs 1385) hash to the same bucket,
	// while markers even one grid step apart (≥5) never collide. See issue #146.
	markerAnchorQuant = 0.5
)

// analyzeMarkerGeometry runs the three read-only marker-geometry rules over the
// live component list. Pure (no I/O) so the issues' real H2/H4 bboxes drive table
// tests directly. overlapEps is the minimum positive-area extent (smaller axis)
// the overlap rules report — below it, edge grazing and the ~1-unit float noise of
// parallel same-side ports are ignored.
func analyzeMarkerGeometry(comps []layoutComp, titleBlock *layoutBBox, overlapEps float64) []checkFinding {
	var findings []checkFinding
	findings = append(findings, duplicateNetMarkerFindings(comps)...)
	findings = append(findings, titleblockOverlapFindings(comps, titleBlock, overlapEps)...)
	findings = append(findings, markerOverlapFindings(comps, overlapEps)...)
	findings = append(findings, foldedNetLabelFindings(comps)...)
	return findings
}

// foldedNetLabelFindings flags netports standing VERTICAL (rotation 90/270): the
// long body (31×11 horizontal) rotates to 11×31 and its net name renders sideways
// — the "标签折起来" readability fail on dense pin columns (live 2026-08-11: the
// autoconnect scorer picked vertical to dodge fanout-channel penalties; the
// costFoldedPort scoring change fixes the planner, this rule is the delivery-gate
// backstop so a folded label can never reach a finished sheet unseen, whatever
// planner produced it). Pure bbox geometry: height > width on a netport ⇔
// rotation ∈ {90,270}; ground/power markers are near-square and exempt.
func foldedNetLabelFindings(comps []layoutComp) []checkFinding {
	var findings []checkFinding
	for _, c := range comps {
		if c.ComponentType != "netport" || c.BBox == nil {
			continue
		}
		w := c.BBox.MaxX - c.BBox.MinX
		h := c.BBox.MaxY - c.BBox.MinY
		if h <= w {
			continue
		}
		findings = append(findings, checkFinding{
			Type:          "folded-net-label",
			Level:         "warn",
			PrimitiveId:   c.ID,
			ComponentType: c.ComponentType,
			MarkerNet:     c.Net,
			BBox:          c.BBox,
			At:            &checkPoint{X: c.X, Y: c.Y},
			Message:       fmt.Sprintf("netport %q 竖排折叠(bbox %.0f×%.0f, 文字侧向)— 重连该脚:`sch autoconnect --replace` 加大 offset 水平错列,或显式 --direction left|right", c.Net, w, h),
		})
	}
	return findings
}

// duplicateNetMarkerFindings groups net markers by (kind, net, quantized anchor)
// and reports every group of 2+ as one finding carrying ALL coincident primitive
// IDs plus a keep/delete suggestion. Different kinds, nets, or anchors never merge,
// so a legitimately distinct marker at another point is not a false positive.
func duplicateNetMarkerFindings(comps []layoutComp) []checkFinding {
	type gkey struct {
		kind   string
		net    string
		qx, qy int64
	}
	q := func(v float64) int64 { return int64(math.Round(v / markerAnchorQuant)) }
	groups := map[gkey][]layoutComp{}
	var order []gkey
	for _, c := range comps {
		if !isSchMarker(c.ComponentType) {
			continue
		}
		k := gkey{c.ComponentType, c.Net, q(c.X), q(c.Y)}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], c)
	}
	var out []checkFinding
	for _, k := range order {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		// Deterministic keep: the lexically-smallest primitiveId.
		sort.Slice(g, func(i, j int) bool { return g[i].ID < g[j].ID })
		ids := make([]string, len(g))
		for i, c := range g {
			ids[i] = c.ID
		}
		keepID := ids[0]
		delIDs := append([]string(nil), ids[1:]...)
		netTxt := g[0].Net
		if netTxt == "" {
			netTxt = "(unnamed)"
		}
		out = append(out, checkFinding{
			Type:             "duplicate-net-marker",
			Level:            "warn",
			ComponentType:    g[0].ComponentType,
			MarkerNet:        g[0].Net,
			PrimitiveId:      keepID,
			PrimitiveIds:     ids,
			SuggestKeepId:    keepID,
			SuggestDeleteIds: delIDs,
			At:               &checkPoint{X: round2(g[0].X), Y: round2(g[0].Y)},
			Message: fmt.Sprintf("重复 %s(%s) @(%.2f,%.2f) ×%d — 建议保留 %s，删除 %s (sch prim-delete)",
				g[0].ComponentType, netTxt, round2(g[0].X), round2(g[0].Y), len(g), keepID, strings.Join(delIDs, "、")),
		})
	}
	return out
}

// titleblockOverlapFindings reports any part or net marker whose bbox positively
// intrudes the title-block keep-out. The sheet itself (spans the page) and
// anything without a bbox are skipped.
func titleblockOverlapFindings(comps []layoutComp, titleBlock *layoutBBox, eps float64) []checkFinding {
	if titleBlock == nil {
		return nil
	}
	var out []checkFinding
	for _, c := range comps {
		if c.BBox == nil {
			continue
		}
		if c.ComponentType != schLayoutPartType && c.ComponentType != "" && !isSchMarker(c.ComponentType) {
			continue // skip the sheet and any non-part/non-marker primitive
		}
		ox, oy, overlap := overlapExtent(*c.BBox, *titleBlock)
		if !overlap || math.Min(ox, oy) <= eps {
			continue
		}
		out = append(out, checkFinding{
			Type:          "titleblock-overlap",
			Level:         "warn",
			PrimitiveId:   c.ID,
			ComponentType: c.ComponentType,
			Designator:    c.Designator,
			MarkerNet:     c.Net,
			BBox:          c.BBox,
			Keepout:       titleBlock,
			OverlapX:      round2(ox),
			OverlapY:      round2(oy),
			Message: fmt.Sprintf("%s(%s) 侵入标题栏 keep-out（重叠 %.2f×%.2f）— 移出图签区或换连线方向",
				markerLabel(c), c.ComponentType, round2(ox), round2(oy)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PrimitiveId < out[j].PrimitiveId })
	return out
}

// markerOverlapFindings reports pairwise positive-area intersections where at
// least one side is a net marker (marker×part or marker×marker); part×part is
// already `sch layout-lint`'s overlap rule. Only overlaps whose smaller axis
// exceeds eps are reported — edge grazing and parallel-port float noise are below.
func markerOverlapFindings(comps []layoutComp, eps float64) []checkFinding {
	withBox := make([]layoutComp, 0, len(comps))
	for _, c := range comps {
		if c.BBox == nil || c.ComponentType == "sheet" {
			continue
		}
		withBox = append(withBox, c)
	}
	var out []checkFinding
	for i := 0; i < len(withBox); i++ {
		for j := i + 1; j < len(withBox); j++ {
			a, b := withBox[i], withBox[j]
			if !isSchMarker(a.ComponentType) && !isSchMarker(b.ComponentType) {
				continue // part×part is layout-lint's job
			}
			if isCoincidentDuplicate(a, b) {
				continue // already reported (with a keep/delete fix) by duplicate-net-marker
			}
			ox, oy, overlap := overlapExtent(*a.BBox, *b.BBox)
			if !overlap || math.Min(ox, oy) <= eps {
				continue
			}
			// Order the pair by id for stable output.
			pa, pb := a, b
			if pb.ID < pa.ID {
				pa, pb = pb, pa
			}
			out = append(out, checkFinding{
				Type:          "marker-overlap",
				Level:         "warn",
				PrimitiveId:   pa.ID,
				ComponentType: pa.ComponentType,
				Designator:    pa.Designator,
				MarkerNet:     pa.Net,
				BBox:          pa.BBox,
				Other: &checkOverlapSide{
					PrimitiveId:   pb.ID,
					ComponentType: pb.ComponentType,
					Designator:    pb.Designator,
					Net:           pb.Net,
					BBox:          pb.BBox,
				},
				PrimitiveIds: []string{pa.ID, pb.ID},
				OverlapX:     round2(ox),
				OverlapY:     round2(oy),
				Message: fmt.Sprintf("%s(%s) 与 %s(%s) 视觉重叠 %.2f×%.2f — 换方向/offset 或 stagger",
					markerLabel(pa), pa.ComponentType, markerLabel(pb), pb.ComponentType, round2(ox), round2(oy)),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PrimitiveId != out[j].PrimitiveId {
			return out[i].PrimitiveId < out[j].PrimitiveId
		}
		return out[i].Other.PrimitiveId < out[j].Other.PrimitiveId
	})
	return out
}

// isCoincidentDuplicate reports whether two markers are the SAME kind + net at the
// SAME quantized anchor — i.e. a pair the duplicate-net-marker rule already reports
// with a keep/delete suggestion. marker-overlap skips them to avoid double-flagging
// one defect as both "duplicate" and "visual overlap".
func isCoincidentDuplicate(a, b layoutComp) bool {
	if !isSchMarker(a.ComponentType) || a.ComponentType != b.ComponentType || a.Net != b.Net {
		return false
	}
	q := func(v float64) int64 { return int64(math.Round(v / markerAnchorQuant)) }
	return q(a.X) == q(b.X) && q(a.Y) == q(b.Y)
}

// markerLabel picks the most identifying name for a marker/part finding: a real
// designator, else the net name, else the primitive id.
func markerLabel(c layoutComp) string {
	if c.Designator != "" && !strings.HasSuffix(c.Designator, "?") {
		return c.Designator
	}
	if c.Net != "" {
		return c.Net
	}
	if c.ID != "" {
		return c.ID
	}
	return c.ComponentType
}

// mergeMarkerGeomFindings fetches the component bboxes/anchors and folds the three
// geometric rules into an existing electrical check report, updating its summary
// counts and passed/total. Best-effort: a components.list failure is logged to
// stderr and leaves the report untouched.
func mergeMarkerGeomFindings(cfg *appConfig, window string, allPages bool, overlapEps float64, rep *checkReport, stderr io.Writer) {
	payload := map[string]any{"includeBBox": true}
	if allPages {
		payload["allPages"] = true
	}
	res, err := requestAction(cfg, "schematic.components.list", window, payload)
	if err != nil {
		fmt.Fprintf(stderr, "sch check: marker-geometry skipped — components.list failed: %v\n", err)
		return
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		fmt.Fprintf(stderr, "sch check: marker-geometry skipped — %v\n", perr)
		return
	}
	var titleBlock *layoutBBox
	if sheet := sheetBBoxOf(comps); sheet != nil {
		titleBlock, _ = titleBlockKeepout(sheet)
	}
	geo := analyzeMarkerGeometry(comps, titleBlock, overlapEps)

	// redundant-net-marker needs the wire trees (exec_js read, stable). Best-effort:
	// a wire-read failure skips this rule only. Single-page only (wires are read
	// from the active page).
	if !allPages {
		if wires, werr := fetchSchWirePolylinesStable(cfg, window, ""); werr != nil {
			fmt.Fprintf(stderr, "sch check: redundant-marker skipped — wire read failed: %v\n", werr)
		} else {
			geo = append(geo, redundantNetMarkerFindings(comps, wires)...)
		}
	}

	// Layout-organization rule (铁律 #15): a multi-module page with no functional
	// zone frames / circuit notes. Mechanical backstop — the rule was skipped twice
	// in one session when it lived only in docs. Scope to the single page under
	// check (allPages inflates the part count while text.list is active-page only).
	if !allPages {
		if pf := partitionFinding(cfg, window, comps, stderr); pf != nil {
			geo = append(geo, *pf)
		}
	}

	if len(geo) == 0 {
		return
	}
	rep.Findings = append(rep.Findings, geo...)
	for _, f := range geo {
		switch f.Type {
		case "duplicate-net-marker":
			rep.Summary.DuplicateNetMarkers++
		case "titleblock-overlap":
			rep.Summary.TitleblockOverlaps++
		case "marker-overlap":
			rep.Summary.MarkerOverlaps++
		case "missing-partition":
			rep.Summary.MissingPartitions++
		case "redundant-net-marker":
			rep.Summary.RedundantNetMarkers++
		case "folded-net-label":
			rep.Summary.FoldedNetLabels++
		}
	}
	rep.Summary.Total = len(rep.Findings)
	rep.Passed = len(rep.Findings) == 0
}

// ── redundant-net-marker(同树冗余标志)────────────────────────────────────
//
// duplicate-net-marker only catches flags whose anchors COINCIDE (quantized).
// Live 2026-08-12: two 3V3 flags 10 units apart on the SAME stub tree (a repair
// stacked a second flag on an already-flagged pin) slipped every rule — anchors
// differ so no duplicate, bbox graze was under the overlap eps, electrically the
// tree is fine so bridge-check is silent. Visually it renders as stacked text.
// Rule: within ONE wire tree, ≥2 markers of the same (net, componentType) are
// redundant — one names the net, the rest are debris. WARN + suggestDeleteIds
// (keep the first by stable order).
func redundantNetMarkerFindings(comps []layoutComp, wires []schGroupWire) []checkFinding {
	if len(wires) == 0 {
		return nil
	}
	// Union-find over wires: two wires share a tree when any vertex of one lies on
	// the other (same touch semantics as the group expansion / disconnect family).
	parent := make([]int, len(wires))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) { parent[find(a)] = find(b) }
	const eps = 0.5
	for i := 0; i < len(wires); i++ {
		for j := i + 1; j < len(wires); j++ {
			touched := false
			for k := 0; k+1 < len(wires[i].Points) && !touched; k += 2 {
				if pointOnPolyline(wires[i].Points[k], wires[i].Points[k+1], wires[j].Points, eps) {
					touched = true
				}
			}
			for k := 0; k+1 < len(wires[j].Points) && !touched; k += 2 {
				if pointOnPolyline(wires[j].Points[k], wires[j].Points[k+1], wires[i].Points, eps) {
					touched = true
				}
			}
			if touched {
				union(i, j)
			}
		}
	}
	// Assign each marker (netflag/netport/netlabel with a usable anchor) to the
	// tree its anchor sits on; group per tree by (net, componentType).
	type key struct {
		tree int
		net  string
		typ  string
	}
	groups := map[key][]layoutComp{}
	for _, c := range comps {
		if !isSchMarker(c.ComponentType) || !c.AnchorAvailable || c.Net == "" {
			continue
		}
		for wi, w := range wires {
			if pointOnPolyline(c.X, c.Y, w.Points, eps) {
				k := key{find(wi), c.Net, c.ComponentType}
				groups[k] = append(groups[k], c)
				break
			}
		}
	}
	var findings []checkFinding
	for k, ms := range groups {
		if len(ms) < 2 {
			continue
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].ID < ms[j].ID })
		ids := make([]string, len(ms))
		var del []string
		for i, m := range ms {
			ids[i] = m.ID
			if i > 0 {
				del = append(del, m.ID)
			}
		}
		findings = append(findings, checkFinding{
			Type:             "redundant-net-marker",
			Level:            "warn",
			MarkerNet:        k.net,
			ComponentType:    k.typ,
			PrimitiveIds:     ids,
			SuggestKeepId:    ms[0].ID,
			SuggestDeleteIds: del,
			At:               &checkPoint{X: ms[0].X, Y: ms[0].Y},
			Count:            len(ms),
			Message: fmt.Sprintf("同一根线树上有 %d 个 %s(%s)标志 — 一个命名即可,其余是修补残留(视觉堆叠);建议保留 %s,删除 %s",
				len(ms), k.net, k.typ, ms[0].ID, strings.Join(del, ",")),
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].SuggestKeepId < findings[j].SuggestKeepId })
	return findings
}

// schPartitionMinParts is the part-count above which a page is expected to be
// organized into functional zones (铁律 #15). Below it, a page is a single trivial
// module and framing would be noise; our fixed ESP32-blink regression board has 12.
const schPartitionMinParts = 6

// partitionFinding flags a multi-part page that carries ZERO free text primitives
// (zone titles + circuit notes). `sch zone-draw` always writes a title text next to
// every frame it draws, and `sch note` writes the per-module descriptions, so
// text.list==0 on a ≥schPartitionMinParts-part page means neither ran — the page was
// left un-partitioned (exactly the lapse this backstops). Title-block fields are NOT
// free text (they live on the sheet), so a bare, un-annotated page reads as 0.
// Best-effort: a text.list failure returns nil (never masks the electrical findings).
func partitionFinding(cfg *appConfig, window string, comps []layoutComp, stderr io.Writer) *checkFinding {
	parts := 0
	for _, c := range comps {
		if c.ComponentType == schLayoutPartType {
			parts++
		}
	}
	if parts < schPartitionMinParts {
		return nil
	}
	res, err := requestAction(cfg, "schematic.text.list", window, map[string]any{})
	if err != nil {
		fmt.Fprintf(stderr, "sch check: partition-check skipped — text.list failed: %v\n", err)
		return nil
	}
	return partitionFindingFor(parts, schTextCount(res.Result))
}

// partitionFindingFor is the pure decision (split out for testing): a page with
// enough parts to be multi-module but zero free text = un-partitioned → WARN.
func partitionFindingFor(parts, textCount int) *checkFinding {
	if parts < schPartitionMinParts || textCount > 0 {
		return nil
	}
	return &checkFinding{
		Type:    "missing-partition",
		Level:   "warn",
		Count:   parts,
		Message: fmt.Sprintf("%d parts on this page but 0 functional zone frames / circuit notes — 铁律#15 要求分区框(sch zone-draw)+ 每模块电路说明(sch note);交付前补上", parts),
	}
}

// schTextCount extracts the number of free text primitives from a
// schematic.text.list result, tolerating either {texts:[…]} or a bare […].
func schTextCount(result map[string]any) int {
	if result == nil {
		return 0
	}
	if raw, ok := result["texts"]; ok {
		if arr, ok := raw.([]any); ok {
			return len(arr)
		}
	}
	return 0
}
