package app

// cmd_sch_group.go — persistent virtual groups for the schematic (用户点名;
// sch side of issue #173).
//
// The platform wall (probed live, EasyEDA Pro 3.2.121): `eda.*` has NO generic
// grouping API (api search "group" hits only pcb_Drc-class long names), and a
// placed sch_PrimitiveComponent instance's 70 methods/properties carry ZERO
// group/parent fields — native UI groups are completely invisible to
// extensions. So easyeda-agent persists the relation itself:
//
//   - `sch group create/list/add/remove/ungroup` — CRUD over
//     workflow.State.GroupsByPage (~/.easyeda-agent/workflow/<project>.json,
//     keyed by documentUuid — the same page-scoped pattern as zones claims);
//   - `sch group-move --group <id>` — resolves members (designators) to live
//     primitiveIds, AUTO-DISCOVERS their attachments (stub wires + the
//     netflags/netports at the far end, same tree semantics as `sch
//     disconnect`), and hands the full set to the existing rigid move;
//   - `sch align` / `sch distribute` refuse a selection that PARTIALLY covers a
//     group (--break-group overrides); autolayout / autoplace-free warn.
//
// Members are DESIGNATORS, not primitiveIds: the designator is the netlist key
// and stays stable within a document, while primitiveIds churn on wire rebuilds
// / re-place / reloads. group-move re-resolves them at call time.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

type schGroup = workflow.Group

// ── pure CRUD core (table-tested, no I/O) ───────────────────────────────────

var groupIDRe = regexp.MustCompile(`^g(\d+)$`)

// nextGroupID allocates the next g<N> id (max existing numeric suffix + 1), so
// ids never collide even after deletions leave holes.
func nextGroupID(groups []*schGroup) string {
	max := 0
	for _, g := range groups {
		if g == nil {
			continue
		}
		if m := groupIDRe.FindStringSubmatch(g.ID); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > max {
				max = n
			}
		}
	}
	return fmt.Sprintf("g%d", max+1)
}

// findSchGroup resolves --group by id first, then by (unique) name.
func findSchGroup(groups []*schGroup, ref string) (*schGroup, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("--group is required (a group id like g1, or a group name)")
	}
	for _, g := range groups {
		if g != nil && g.ID == ref {
			return g, nil
		}
	}
	var byName []*schGroup
	for _, g := range groups {
		if g != nil && g.Name != "" && strings.EqualFold(g.Name, ref) {
			byName = append(byName, g)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return nil, fmt.Errorf("group %q not found on this page (`sch group list` to see groups)", ref)
	default:
		ids := make([]string, len(byName))
		for i, g := range byName {
			ids[i] = g.ID
		}
		return nil, fmt.Errorf("group name %q is ambiguous (%s) — use the group id", ref, strings.Join(ids, ", "))
	}
}

// groupOfMember returns the group that owns a designator (nil when free).
func groupOfMember(groups []*schGroup, designator string) *schGroup {
	u := strings.ToUpper(strings.TrimSpace(designator))
	for _, g := range groups {
		if g == nil {
			continue
		}
		for _, m := range g.Members {
			if m == u {
				return g
			}
		}
	}
	return nil
}

// describeSchGroup renders "g1" or `g1 "mcu-core"` for error/report text.
func describeSchGroup(g *schGroup) string {
	if g.Name != "" {
		return fmt.Sprintf("%s %q", g.ID, g.Name)
	}
	return g.ID
}

// groupsCreate adds a new group. Every member must be group-free (a designator
// belongs to at most one group per page); violations name the owning group.
func groupsCreate(groups []*schGroup, name string, members []string) ([]*schGroup, *schGroup, error) {
	norm := normalizeDesignators(members)
	if len(norm) == 0 {
		return nil, nil, fmt.Errorf("--members is required (CSV of designators, e.g. R1,C5,U2)")
	}
	for _, m := range norm {
		if owner := groupOfMember(groups, m); owner != nil {
			return nil, nil, fmt.Errorf("%s already belongs to group %s — remove it first (`sch group remove --group %s --members %s`) or pass different members",
				m, describeSchGroup(owner), owner.ID, m)
		}
	}
	g := &schGroup{
		ID: nextGroupID(groups), Name: strings.TrimSpace(name),
		Members: norm, At: time.Now().Format(time.RFC3339),
	}
	return append(append([]*schGroup(nil), groups...), g), g, nil
}

// groupsAddMembers adds members to an existing group (same one-group-per-part
// rule as create; adding a designator the group already has is a no-op).
func groupsAddMembers(groups []*schGroup, ref string, members []string) ([]*schGroup, *schGroup, error) {
	g, err := findSchGroup(groups, ref)
	if err != nil {
		return nil, nil, err
	}
	norm := normalizeDesignators(members)
	if len(norm) == 0 {
		return nil, nil, fmt.Errorf("--members is required (CSV of designators)")
	}
	merged := append([]string(nil), g.Members...)
	for _, m := range norm {
		owner := groupOfMember(groups, m)
		if owner != nil && owner.ID != g.ID {
			return nil, nil, fmt.Errorf("%s already belongs to group %s — a designator can only be in one group", m, describeSchGroup(owner))
		}
		if owner == nil {
			merged = append(merged, m)
		}
	}
	g.Members = normalizeDesignators(merged)
	return groups, g, nil
}

// groupsRemoveMembers removes members from a group; an emptied group is deleted
// (reported via removedGroup so the CLI can say so).
func groupsRemoveMembers(groups []*schGroup, ref string, members []string) (out []*schGroup, g *schGroup, removedGroup bool, err error) {
	g, err = findSchGroup(groups, ref)
	if err != nil {
		return nil, nil, false, err
	}
	norm := normalizeDesignators(members)
	if len(norm) == 0 {
		return nil, nil, false, fmt.Errorf("--members is required (CSV of designators)")
	}
	drop := map[string]bool{}
	for _, m := range norm {
		found := false
		for _, cur := range g.Members {
			if cur == m {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, false, fmt.Errorf("%s is not a member of group %s (members: %s)", m, describeSchGroup(g), strings.Join(g.Members, ","))
		}
		drop[m] = true
	}
	var kept []string
	for _, cur := range g.Members {
		if !drop[cur] {
			kept = append(kept, cur)
		}
	}
	g.Members = kept
	if len(kept) > 0 {
		return groups, g, false, nil
	}
	for _, other := range groups {
		if other != nil && other.ID != g.ID {
			out = append(out, other)
		}
	}
	return out, g, true, nil
}

// groupsUngroup deletes the whole group relation (primitives untouched).
func groupsUngroup(groups []*schGroup, ref string) ([]*schGroup, *schGroup, error) {
	g, err := findSchGroup(groups, ref)
	if err != nil {
		return nil, nil, err
	}
	var out []*schGroup
	for _, other := range groups {
		if other != nil && other.ID != g.ID {
			out = append(out, other)
		}
	}
	return out, g, nil
}

// findPartialGroups reports every group the selection PARTIALLY covers: it
// contains at least one member but not all of them. align/distribute treat a
// group as a rigid body and refuse such a selection unless --break-group.
func findPartialGroups(groups []*schGroup, selected []string) []*schGroup {
	sel := map[string]bool{}
	for _, d := range selected {
		sel[strings.ToUpper(strings.TrimSpace(d))] = true
	}
	var out []*schGroup
	for _, g := range groups {
		if g == nil || len(g.Members) == 0 {
			continue
		}
		in, missing := 0, 0
		for _, m := range g.Members {
			if sel[m] {
				in++
			} else {
				missing++
			}
		}
		if in > 0 && missing > 0 {
			out = append(out, g)
		}
	}
	return out
}

// ── attachment expansion (pure geometry, fixture-tested) ────────────────────

// schGroupEps is the coincidence tolerance in native canvas units (0.01 inch).
// Wire endpoints / pins / flag anchors sit on the 5-unit grid; 0.5 absorbs
// float tails without ever bridging distinct grid points.
const schGroupEps = 0.5

// schGroupNearTol is the ALONG-LINE gap tolerance of the expansion-completeness
// precheck. Live 2026-08-12: a stub wire that an earlier HALF-MOVE had stranded
// sat 10 units off its pin ALONG ITS OWN LINE (line start 820 vs pin at 810,
// same y, folded-back vertex list) — it electrically touches nothing, so the
// eps-based tree classes (member/foreign) both miss it, the expansion leaves it
// behind, and the next move half-carries the group AGAIN, compounding the
// damage. A wire whose LINE passes through the pin (perpendicular distance ≤
// schGroupPerpTol) but whose span stops short of it by a gap in
// (schGroupEps, schGroupNearTol] is treated as such residue and hard-rejects
// the move. 12 covers the observed 10-unit displacement with margin.
const schGroupNearTol = 12.0

// schGroupPerpTol bounds the PERPENDICULAR distance for the residue test.
// Half-move residue is COLLINEAR with its pin — displaced only along the line,
// perpendicular offset ~0. A legitimate NEIGHBORING stub runs PARALLEL one
// half-pitch away and must never match (live 终验 2026-08-12: U2:EN's healthy
// stub at y=485 passed 10 radial units from R1's real pin at y=475 — the pin
// half-pitch is ±20, putting parallel neighbors inside a radial 12 — and a
// radius-only test wrongly rejected the move). Perpendicular offset above this
// means a DIFFERENT line entirely → never residue.
const schGroupPerpTol = 1.0

// schGroupWire is one wire polyline with its live primitiveId (pulled via the
// debug.exec_js hatch — the components.list `wires` payload carries segments
// but drops the primitiveId, and group-move must name the wire to move it).
type schGroupWire struct {
	ID     string
	Points []float64 // flattened vertices x0,y0,x1,y1,…
}

// schGroupFlag is one netflag/netport/netlabel anchor.
type schGroupFlag struct {
	ID   string
	X, Y float64
}

type groupExpandInput struct {
	MemberPins [][2]float64 // pins of the group's member components
	OtherPins  [][2]float64 // pins of every OTHER part on the page
	Wires      []schGroupWire
	Flags      []schGroupFlag
}

// groupSuspect is one wire flagged by the completeness precheck: its carrier
// LINE passes through a member pin (perp ≤ schGroupPerpTol) but its span stops
// short of it by an along-line gap in (schGroupEps, schGroupNearTol] — the
// collinear "same line, broken contact" signature of half-move residue.
// Endpoints + the grazed pin go into the rejection report so the cleanup is
// actionable.
type groupSuspect struct {
	WireID         string
	X0, Y0, X1, Y1 float64 // wire's first/last polyline vertex
	PinX, PinY     float64 // the member pin it grazes
}

// groupExpansion is the discovered attachment set. SharedTrees counts wire
// trees that touch BOTH a member pin and a non-member pin — real inter-part
// wiring, not a stub; moving it rigidly would tear the far connection, so it is
// skipped and reported. Suspects (see groupSuspect) mean the expansion is
// INCOMPLETE and the move must be refused: a partial move is exactly the
// failure mode groups exist to prevent.
type groupExpansion struct {
	WireIDs     []string
	FlagIDs     []string
	SharedTrees int
	Suspects    []groupSuspect
}

func pointsClose(ax, ay, bx, by, eps float64) bool {
	dx, dy := ax-bx, ay-by
	return dx*dx+dy*dy <= eps*eps
}

// pointOnSegment reports whether (px,py) lies within eps of segment (x0,y0)-(x1,y1)
// — endpoints AND interior. EasyEDA connects at endpoint-on-wire junctions and
// leaves merged flags mid-span (the `sch disconnect` lesson), so interior hits count.
func pointOnSegment(px, py, x0, y0, x1, y1, eps float64) bool {
	vx, vy := x1-x0, y1-y0
	wx, wy := px-x0, py-y0
	segLen2 := vx*vx + vy*vy
	if segLen2 == 0 {
		return pointsClose(px, py, x0, y0, eps)
	}
	t := (wx*vx + wy*vy) / segLen2
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	cx, cy := x0+t*vx, y0+t*vy
	return pointsClose(px, py, cx, cy, eps)
}

// pointOnPolyline reports whether the point lies on any edge of the polyline.
func pointOnPolyline(px, py float64, pts []float64, eps float64) bool {
	if len(pts) == 2 {
		return pointsClose(px, py, pts[0], pts[1], eps)
	}
	for i := 0; i+3 < len(pts); i += 2 {
		if pointOnSegment(px, py, pts[i], pts[i+1], pts[i+2], pts[i+3], eps) {
			return true
		}
	}
	return false
}

// segmentGrazesPoint reports whether (px,py) lies ON the segment's carrier LINE
// (perpendicular distance ≤ schGroupPerpTol) while the segment's span stops
// short of it by an along-line gap in (schGroupEps, schGroupNearTol] — the
// "same line, broken contact" signature of half-move residue. A PARALLEL
// neighboring stub (perp offset > schGroupPerpTol) never matches, no matter how
// radially close (终验 false-reject: y=485 stub vs y=475 pin, radial 10). The
// projection-outside-span formula covers slanted segments unchanged. A
// degenerate zero-length segment falls back to radial distance as the gap.
func segmentGrazesPoint(px, py, x0, y0, x1, y1 float64) bool {
	vx, vy := x1-x0, y1-y0
	wx, wy := px-x0, py-y0
	segLen2 := vx*vx + vy*vy
	if segLen2 == 0 {
		d := math.Hypot(wx, wy)
		return d > schGroupEps && d <= schGroupNearTol
	}
	segLen := math.Sqrt(segLen2)
	// Perpendicular distance from the pin to the carrier (infinite) line.
	if math.Abs(vx*wy-vy*wx)/segLen > schGroupPerpTol {
		return false
	}
	// Along-line gap: how far the pin's projection falls OUTSIDE the span.
	t := (wx*vx + wy*vy) / segLen2
	var gap float64
	switch {
	case t < 0:
		gap = -t * segLen
	case t > 1:
		gap = (t - 1) * segLen
	default:
		gap = 0 // projection inside the span (attached is handled by eps matching)
	}
	return gap > schGroupEps && gap <= schGroupNearTol
}

// polylineGrazesPoint reports whether any edge of the polyline collinearly
// grazes the point (see segmentGrazesPoint). A single-point polyline degrades
// to the radial-gap test.
func polylineGrazesPoint(px, py float64, pts []float64) bool {
	if len(pts) == 2 {
		d := math.Hypot(px-pts[0], py-pts[1])
		return d > schGroupEps && d <= schGroupNearTol
	}
	for i := 0; i+3 < len(pts); i += 2 {
		if segmentGrazesPoint(px, py, pts[i], pts[i+1], pts[i+2], pts[i+3]) {
			return true
		}
	}
	return false
}

// wiresTouch reports whether two polylines connect: any vertex of one lies on
// an edge of the other (covers shared endpoints AND T-junctions — EasyEDA
// merges endpoint-on-wire contact into one electrical tree).
func wiresTouch(a, b schGroupWire, eps float64) bool {
	for i := 0; i+1 < len(a.Points); i += 2 {
		if pointOnPolyline(a.Points[i], a.Points[i+1], b.Points, eps) {
			return true
		}
	}
	for i := 0; i+1 < len(b.Points); i += 2 {
		if pointOnPolyline(b.Points[i], b.Points[i+1], a.Points, eps) {
			return true
		}
	}
	return false
}

// treeTerminatesAt reports whether the wire tree ENDS at the point: the point
// touches the tree only at segment endpoints, and every incident segment leaves
// in ONE direction — the signature of a wire deliberately drawn TO that point.
// It returns false when the tree PASSES THROUGH the point: interior span
// contact, or endpoint contact with segments leaving in different directions.
//
// Why direction-based and not "is the point a terminal polyline vertex": the
// live folded stub [820,475 835,475 845,475 835,475] has its terminal VERTEX at
// 835 — exactly on U2:3 — yet geometrically the wire runs 820→845 and passes
// THROUGH 835 (incident directions west AND east). A vertex-based test would
// call that a termination and misclassify the tree as deliberately wired to
// U2:3; the direction test correctly sees a pass-over. The true open ends (820,
// 845) have all incident directions equal.
func treeTerminatesAt(wires []schGroupWire, px, py float64) bool {
	var dirX, dirY float64
	found := false
	// addDir folds one outgoing direction into the running "single direction"
	// check; a second DISTINCT direction means pass-through/junction, not an end.
	addDir := func(ox, oy float64) bool {
		dx, dy := ox-px, oy-py
		l := math.Hypot(dx, dy)
		if l <= schGroupEps {
			return true // degenerate (zero-length) — no direction information
		}
		dx, dy = dx/l, dy/l
		if !found {
			dirX, dirY, found = dx, dy, true
			return true
		}
		return dirX*dx+dirY*dy > 0.999 // same outgoing direction (folded tail re-tracing)
	}
	for _, w := range wires {
		pts := w.Points
		for i := 0; i+3 < len(pts); i += 2 {
			x0, y0, x1, y1 := pts[i], pts[i+1], pts[i+2], pts[i+3]
			if !pointOnSegment(px, py, x0, y0, x1, y1, schGroupEps) {
				continue
			}
			c0 := pointsClose(px, py, x0, y0, schGroupEps)
			c1 := pointsClose(px, py, x1, y1, schGroupEps)
			if !c0 && !c1 {
				return false // interior span contact — the tree passes over the point
			}
			if c0 && !addDir(x1, y1) {
				return false
			}
			if c1 && !addDir(x0, y0) {
				return false
			}
		}
	}
	return found
}

// expandGroupAttachments discovers which wires + flags ride along with the
// group members, at wire-TREE granularity (union-find over touching wires —
// the same semantics `sch disconnect` uses: collinear merged stubs span several
// wire primitives, and flags can sit on mid-vertices or mid-span).
//
// A tree is included iff it touches ≥1 member pin and is not DELIBERATELY
// wired to a non-member pin. Member matching is whole-span generous (endpoint,
// vertex, or mid-span — never leave member attachments behind); foreign
// matching demands that the tree TERMINATE at the foreign pin
// (treeTerminatesAt): a deliberate connection is a wire drawn TO the pin,
// while a span merely PASSING OVER a foreign pin is incidental wire-over-pin
// contact. The distinction is load-bearing (live 2026-08-12 悬案): a folded
// stub moved +100 came to rest ON TOP of U2:3 — a radial whole-span foreign
// test then classified its tree as "real inter-part wiring" and refused to
// carry it back, half-moving the group on every return leg. Deliberately
// wired trees (foreign pin at the tree's open end) still count as
// SharedTrees: rigidly moving them would tear the far end, so they are
// skipped and reported.
func expandGroupAttachments(in groupExpandInput) groupExpansion {
	n := len(in.Wires)
	parent := make([]int, n)
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
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if wiresTouch(in.Wires[i], in.Wires[j], schGroupEps) {
				union(i, j)
			}
		}
	}

	trees := map[int][]int{}
	for i := 0; i < n; i++ {
		root := find(i)
		trees[root] = append(trees[root], i)
	}

	var out groupExpansion
	for _, idxs := range trees {
		tw := make([]schGroupWire, len(idxs))
		for k, wi := range idxs {
			tw[k] = in.Wires[wi]
		}
		member := false
		for _, w := range tw {
			for _, p := range in.MemberPins {
				if pointOnPolyline(p[0], p[1], w.Points, schGroupEps) {
					member = true
					break
				}
			}
			if member {
				break
			}
		}
		if !member {
			// Completeness precheck: a tree that ATTACHES to no member pin but
			// COLLINEARLY GRAZES one (same carrier line, along-line gap ≤
			// schGroupNearTol) is residue of an earlier half-move — silently
			// leaving it behind is how the damage compounds. Parallel neighbor
			// stubs (perp offset > schGroupPerpTol) are legitimate and ignored.
			for _, w := range tw {
				for _, p := range in.MemberPins {
					if polylineGrazesPoint(p[0], p[1], w.Points) {
						last := len(w.Points)
						out.Suspects = append(out.Suspects, groupSuspect{
							WireID: w.ID,
							X0:     w.Points[0], Y0: w.Points[1],
							X1: w.Points[last-2], Y1: w.Points[last-1],
							PinX: p[0], PinY: p[1],
						})
						break
					}
				}
			}
			continue
		}
		// Foreign = the tree is deliberately WIRED to a non-member pin: it must
		// TERMINATE there, not merely pass over it (see treeTerminatesAt — the
		// pass-over case is a stub parked on a foreign pin by an earlier move,
		// which MUST ride along or the group half-moves forever).
		foreign := false
		for _, p := range in.OtherPins {
			touches := false
			for _, w := range tw {
				if pointOnPolyline(p[0], p[1], w.Points, schGroupEps) {
					touches = true
					break
				}
			}
			if touches && treeTerminatesAt(tw, p[0], p[1]) {
				foreign = true
				break
			}
		}
		if foreign {
			out.SharedTrees++
			continue
		}
		for _, w := range tw {
			out.WireIDs = append(out.WireIDs, w.ID)
		}
		for _, f := range in.Flags {
			for _, w := range tw {
				if pointOnPolyline(f.X, f.Y, w.Points, schGroupEps) {
					out.FlagIDs = append(out.FlagIDs, f.ID)
					break
				}
			}
		}
	}
	sort.Strings(out.WireIDs)
	sort.Strings(out.FlagIDs)
	sort.Slice(out.Suspects, func(i, j int) bool { return out.Suspects[i].WireID < out.Suspects[j].WireID })
	return out
}

// ── I/O plumbing ────────────────────────────────────────────────────────────

// loadSchGroupsContext pins the active schematic page and loads its group
// table plus the surrounding workflow state (zones-claims pattern).
func loadSchGroupsContext(cfg *appConfig, window string) (pinned *appConfig, win, docUUID, project string, st *pcbStageState, groups []*schGroup, err error) {
	pinned, win, docUUID, err = pinZonePage(cfg, window)
	if err != nil {
		return nil, "", "", "", nil, nil, err
	}
	project, err = resolveStageProject(pinned, win)
	if err != nil {
		return nil, "", "", "", nil, nil, err
	}
	st, err = loadPcbStageState(project)
	if err != nil {
		return nil, "", "", "", nil, nil, err
	}
	return pinned, win, docUUID, project, st, st.GroupsForPage(docUUID), nil
}

// schGroupFlagTypes are the marker component types that ride along with a stub.
var schGroupFlagTypes = map[string]bool{"netflag": true, "netport": true, "netlabel": true}

// fetchSchWirePolylines pulls every wire's {primitiveId, polyline} via the
// debug.exec_js hatch: the typed components.list `wires` payload flattens to
// segments WITHOUT primitiveIds, and group-move must name the wire primitives
// it moves. Read-only; same hatch precedent as autolayout-official/zone-draw.
func fetchSchWirePolylines(cfg *appConfig, window, docUUID string) ([]schGroupWire, error) {
	const code = `
const wires = await eda.sch_PrimitiveWire.getAll() ?? [];
const out = [];
for (const w of wires) {
	let id = '', line = null;
	try { id = String(w.getState_PrimitiveId?.() ?? ''); } catch {}
	try { const l = w.getState_Line?.(); if (Array.isArray(l)) line = l; } catch {}
	if (id && Array.isArray(line) && line.length >= 4) out.push({ id, line });
}
return { wires: out };`
	res, err := requestAutolayoutActionTimed(cfg, "debug.exec_js", window,
		map[string]any{"code": code}, 30*time.Second, docUUID, "read wire polylines")
	if err != nil {
		return nil, err
	}
	value, _ := res.Result["value"].(map[string]any)
	if value == nil {
		return nil, fmt.Errorf("wire read returned no value (result: %v)", res.Result)
	}
	raw, _ := value["wires"].([]any)
	out := make([]schGroupWire, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := asString(m["id"])
		lineRaw, _ := m["line"].([]any)
		if id == "" || len(lineRaw) < 4 {
			continue
		}
		pts := make([]float64, 0, len(lineRaw))
		valid := true
		for _, v := range lineRaw {
			f, ok := finiteFloat(v)
			if !ok {
				valid = false
				break
			}
			pts = append(pts, f)
		}
		if valid {
			out = append(out, schGroupWire{ID: id, Points: pts})
		}
	}
	return out, nil
}

// schGroupMoveSet is the fully-expanded id set group-move dispatches.
type schGroupMoveSet struct {
	Group        *schGroup
	ComponentIDs []string
	Expansion    groupExpansion
}

// AllIDs is the flattened primitiveId list for schematic.group.move.
func (s *schGroupMoveSet) AllIDs() []string {
	out := append([]string(nil), s.ComponentIDs...)
	out = append(out, s.Expansion.WireIDs...)
	return append(out, s.Expansion.FlagIDs...)
}

// expandSchGroupForMove resolves a persisted group into the full rigid-move id
// set: member designators → live primitiveIds, plus auto-discovered stub wires
// and far-end flags. Every member must be present on the active page — moving a
// silently-partial group is exactly the failure mode groups exist to prevent.
func expandSchGroupForMove(cfg *appConfig, window, groupRef string) (*schGroupMoveSet, error) {
	pinned, win, docUUID, _, _, groups, err := loadSchGroupsContext(cfg, window)
	if err != nil {
		return nil, err
	}
	g, err := findSchGroup(groups, groupRef)
	if err != nil {
		return nil, err
	}
	res, err := requestAutolayoutAction(pinned, "schematic.components.list", win,
		map[string]any{"includePins": true}, docUUID, "resolve group members")
	if err != nil {
		return nil, err
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return nil, err
	}

	member := map[string]bool{}
	for _, m := range g.Members {
		member[m] = true
	}
	in := groupExpandInput{}
	set := &schGroupMoveSet{Group: g}
	found := map[string]bool{}
	for _, c := range comps {
		desig := strings.ToUpper(c.Designator)
		switch {
		case member[desig]:
			found[desig] = true
			set.ComponentIDs = append(set.ComponentIDs, c.ID)
			for _, p := range c.Pins {
				in.MemberPins = append(in.MemberPins, [2]float64{p.X, p.Y})
			}
		case schGroupFlagTypes[c.ComponentType]:
			if c.AnchorAvailable && c.ID != "" {
				in.Flags = append(in.Flags, schGroupFlag{ID: c.ID, X: c.X, Y: c.Y})
			}
		case c.ComponentType == "" || c.ComponentType == schLayoutPartType:
			for _, p := range c.Pins {
				in.OtherPins = append(in.OtherPins, [2]float64{p.X, p.Y})
			}
		}
	}
	var missing []string
	for _, m := range g.Members {
		if !found[m] {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("group %s has stale member(s) not on the active page: %s — `sch group remove --group %s --members %s` (or `sch group list` to inspect)",
			describeSchGroup(g), strings.Join(missing, ","), g.ID, strings.Join(missing, ","))
	}
	in.Wires, err = fetchSchWirePolylinesStable(pinned, win, docUUID)
	if err != nil {
		return nil, fmt.Errorf("discover member attachments: %w", err)
	}
	set.Expansion = expandGroupAttachments(in)
	// Completeness precheck — refuse over half-move. A wire that grazes a member
	// pin without attaching is residue of an earlier half-move (live 2026-08-12:
	// a stranded stub 10 units off R1:1 was left behind by the expansion on every
	// pass, so each move stranded it further and parked its flag on other parts).
	// Moving anyway would half-carry the group again; the ONLY safe answer is to
	// stop and have the residue cleaned first.
	if len(set.Expansion.Suspects) > 0 {
		var lines, ids []string
		for _, s := range set.Expansion.Suspects {
			ids = append(ids, s.WireID)
			lines = append(lines, fmt.Sprintf("  wire %s [%g,%g → %g,%g] collinearly grazes member pin (%g,%g) without attaching",
				s.WireID, s.X0, s.Y0, s.X1, s.Y1, s.PinX, s.PinY))
		}
		return nil, fmt.Errorf(`group %s expansion is INCOMPLETE — %d wire(s) lie on a member pin's own line but stop short of it by ≤%g units without attaching (the signature of residue from an earlier half-move):
%s
refusing to move — a half-moved group is exactly what groups exist to prevent. Clean up first:
  easyeda sch prim-delete --ids %s   # remove the stray stub(s)
  easyeda sch check                  # audit dangling wires / stray flags
then re-connect the affected pin(s) (`+"`sch connect`"+`) and retry`,
			describeSchGroup(g), len(set.Expansion.Suspects), schGroupNearTol,
			strings.Join(lines, "\n"), strings.Join(ids, ","))
	}
	return set, nil
}

// fetchSchWirePolylinesStable reads the wire list until two consecutive reads
// agree on the id set. A group-move recreates every stub wire/flag (new
// primitiveIds); an IMMEDIATELY following expansion can catch the platform's
// snapshot mid-churn and see only PART of the new wires — live 2026-08-12: the
// return leg of a +100/-100 pair expanded 2+2+2 instead of 2+4+4, leaving two
// stubs+flags stranded at the old offset (dangling wires + a marker parked on
// another part). Two agreeing reads prove the snapshot has settled; still
// unstable after the retry budget = refuse to expand (a half-moved group is the
// exact failure mode groups exist to prevent).
func fetchSchWirePolylinesStable(cfg *appConfig, window, docUUID string) ([]schGroupWire, error) {
	const attempts = 4
	var prevKey string
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(350 * time.Millisecond)
		}
		cur, err := fetchSchWirePolylines(cfg, window, docUUID)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(cur))
		for _, w := range cur {
			ids = append(ids, w.ID)
		}
		sort.Strings(ids)
		key := strings.Join(ids, ",")
		if prevKey != "" && key == prevKey {
			return cur, nil
		}
		prevKey = key
	}
	return nil, fmt.Errorf("wire list is still churning after %d reads (platform snapshot settling after a recent mutation) — wait a moment and rerun", attempts)
}

// ── layout-action guards ────────────────────────────────────────────────────

// guardSchGroupIntegrity is the align/distribute rigid-body gate: a selection
// that partially covers a group is refused with the full member list, unless
// --break-group. Load failures degrade to a stderr warning (the protection is
// best-effort; the primary command must not die on a stale workflow file).
func guardSchGroupIntegrity(cfg *appConfig, window string, selected []string, breakGroup bool, stderr io.Writer) error {
	if breakGroup {
		return nil
	}
	_, _, _, _, _, groups, err := loadSchGroupsContext(cfg, window)
	if err != nil {
		fmt.Fprintf(stderr, "note: group integrity check unavailable (%v)\n", err)
		return nil
	}
	partial := findPartialGroups(groups, selected)
	if len(partial) == 0 {
		return nil
	}
	var lines []string
	for _, g := range partial {
		lines = append(lines, fmt.Sprintf("  %s: %s", describeSchGroup(g), strings.Join(g.Members, ",")))
	}
	return fmt.Errorf("selection partially covers %d group(s) — a group moves as a rigid body:\n%s\npass the WHOLE group, or --break-group to override (`sch group ungroup --group <id>` to dissolve it)",
		len(partial), strings.Join(lines, "\n"))
}

// warnSchGroupsPresent tells autolayout/autoplace-free callers that persistent
// groups exist on the page: those planners place parts individually and do NOT
// preserve intra-group relative geometry (v1 scope: warn, don't re-plan).
// Best-effort: any failure is silent (the planner itself is about to talk to
// the same window and will surface real connectivity errors).
func warnSchGroupsPresent(cfg *appConfig, window, verb string, stderr io.Writer) {
	_, _, _, _, _, groups, err := loadSchGroupsContext(cfg, window)
	if err != nil || len(groups) == 0 {
		return
	}
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		if g != nil {
			ids = append(ids, describeSchGroup(g))
		}
	}
	fmt.Fprintf(stderr, "⚠ %d persistent group(s) on this page (%s) — %s does NOT preserve intra-group relative geometry; use `sch group-move --group <id>` to move a group rigidly, or `sch group ungroup` first\n",
		len(groups), strings.Join(ids, ", "), verb)
}

// warnSchGroupMemberDeletion warns when prim-delete is about to remove a group
// member (the relation goes stale; `sch group list` marks it). Best-effort and
// read-only — a failure must never block the delete.
func warnSchGroupMemberDeletion(cfg *appConfig, window string, ids []string, stderr io.Writer) {
	if len(ids) == 0 {
		return
	}
	pinned, win, docUUID, _, _, groups, err := loadSchGroupsContext(cfg, window)
	if err != nil || len(groups) == 0 {
		return
	}
	res, err := requestAutolayoutAction(pinned, "schematic.components.list", win, nil, docUUID, "check group membership")
	if err != nil {
		return
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return
	}
	desigByID := map[string]string{}
	for _, c := range comps {
		if c.ID != "" && c.Designator != "" {
			desigByID[c.ID] = strings.ToUpper(c.Designator)
		}
	}
	for _, id := range ids {
		desig := desigByID[id]
		if desig == "" {
			continue
		}
		if g := groupOfMember(groups, desig); g != nil {
			fmt.Fprintf(stderr, "⚠ %s belongs to group %s — deleting it leaves the group stale (`sch group remove --group %s --members %s`, or `sch group list` to audit)\n",
				desig, describeSchGroup(g), g.ID, desig)
		}
	}
}

// ── cobra surface ───────────────────────────────────────────────────────────

// saveSchGroups persists one page's table and stamps the state.
func saveSchGroups(st *pcbStageState, docUUID string, groups []*schGroup) error {
	st.SetGroupsForPage(docUUID, groups)
	return savePcbStageState(st)
}

func newSchGroupCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	group := &cobra.Command{
		Use:   "group",
		Short: "Persistent virtual groups (create/list/add/remove/ungroup) — the platform has NO grouping API",
		Long: `Persistent virtual groups over schematic parts.

The platform wall (probed live on EasyEDA Pro 3.2.121): ` + "`eda.*`" + ` exposes NO
grouping API, and a placed component's 70 methods/properties carry zero
group/parent fields — even native UI groups are invisible to extensions. So
easyeda-agent persists the relation itself, page-scoped by documentUuid in the
project workflow state (~/.easyeda-agent/workflow/<project>.json — same store
as zones claims), and layout actions consume it:

  - ` + "`sch group-move --group <id>`" + ` moves the whole group rigidly, with the
    members' stub wires + far-end netflags/netports discovered automatically;
  - ` + "`sch align` / `sch distribute`" + ` REFUSE a selection that partially covers a
    group (--break-group overrides);
  - ` + "`sch autolayout` / `autoplace-free`" + ` warn when groups exist (they do not
    preserve intra-group geometry).

Members are DESIGNATORS (stable within a document; primitiveIds churn on wire
rebuilds), resolved to live primitiveIds at move time. A designator belongs to
at most one group per page.`,
	}

	// ── create ──
	{
		var membersRaw, name string
		c := &cobra.Command{
			Use:   "create",
			Short: "Create a group from member designators (CSV); id is auto-assigned (g1, g2, …)",
			Args:  cobra.NoArgs,
			Example: `  easyeda sch group create --members R1,C5,U2
  easyeda sch group create --members U1,C1,C2 --name mcu-core`,
			RunE: func(cmd *cobra.Command, args []string) error {
				_, _, docUUID, project, st, groups, err := loadSchGroupsContext(cfg, *window)
				if err != nil {
					return err
				}
				next, g, err := groupsCreate(groups, name, splitDesignators(membersRaw))
				if err != nil {
					return err
				}
				if err := saveSchGroups(st, docUUID, next); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "✓ created group %s — %d member(s): %s (project %q, page %s)\n",
					describeSchGroup(g), len(g.Members), strings.Join(g.Members, ","), project, docUUID)
				return nil
			},
		}
		c.Flags().StringVar(&membersRaw, "members", "", "member designators — CSV: R1,C5,U2 (required)")
		c.Flags().StringVar(&name, "name", "", "optional human-readable group name")
		_ = c.MarkFlagRequired("members")
		group.AddCommand(c)
	}

	// ── list ──
	{
		var asJSON, allPages bool
		c := &cobra.Command{
			Use:   "list",
			Short: "List groups on the active page (--all-pages for every page); absent members are marked stale",
			Args:  cobra.NoArgs,
			Example: `  easyeda sch group list
  easyeda sch group list --all-pages
  easyeda sch group list --json`,
			RunE: func(cmd *cobra.Command, args []string) error {
				pinned, win, docUUID, project, st, groups, err := loadSchGroupsContext(cfg, *window)
				if err != nil {
					return err
				}
				pages := map[string][]*schGroup{}
				if allPages {
					for uuid, gs := range st.GroupsByPage {
						pages[uuid] = gs
					}
				} else if len(groups) > 0 {
					pages[docUUID] = groups
				}
				// Live presence check (best-effort): mark members whose designator
				// is no longer on the page as stale. --all-pages uses the shallow
				// cross-page list (designators are present even in shallow data).
				present := map[string]bool{}
				presenceKnown := false
				{
					payload := map[string]any{}
					if allPages {
						payload["allPages"] = true
					}
					if res, lerr := requestAutolayoutAction(pinned, "schematic.components.list", win, payload, docUUID, "check group member presence"); lerr == nil {
						if comps, perr := parseLayoutComps(res.Result); perr == nil {
							presenceKnown = true
							for _, c := range comps {
								if c.Designator != "" {
									present[strings.ToUpper(c.Designator)] = true
								}
							}
						}
					}
				}
				if asJSON {
					type memberOut struct {
						Designator string `json:"designator"`
						Stale      bool   `json:"stale,omitempty"`
					}
					type groupOut struct {
						ID      string      `json:"id"`
						Name    string      `json:"name,omitempty"`
						Members []memberOut `json:"members"`
						At      string      `json:"at,omitempty"`
					}
					out := map[string]any{"project": project, "documentUuid": docUUID, "presenceChecked": presenceKnown}
					pagesOut := map[string][]groupOut{}
					for uuid, gs := range pages {
						for _, g := range gs {
							if g == nil {
								continue
							}
							go_ := groupOut{ID: g.ID, Name: g.Name, At: g.At}
							for _, m := range g.Members {
								go_.Members = append(go_.Members, memberOut{Designator: m, Stale: presenceKnown && !present[m]})
							}
							pagesOut[uuid] = append(pagesOut[uuid], go_)
						}
					}
					out["groupsByPage"] = pagesOut
					enc := json.NewEncoder(stdout)
					enc.SetIndent("", "  ")
					return enc.Encode(out)
				}
				if len(pages) == 0 {
					scope := "page " + docUUID
					if allPages {
						scope = "any page"
					}
					fmt.Fprintf(stdout, "no groups for %q on %s — `sch group create --members R1,C5,U2`\n", project, scope)
					return nil
				}
				var uuids []string
				for uuid := range pages {
					uuids = append(uuids, uuid)
				}
				sort.Strings(uuids)
				for _, uuid := range uuids {
					fmt.Fprintf(stdout, "groups — project %q, page %s\n", project, uuid)
					for _, g := range pages[uuid] {
						if g == nil {
							continue
						}
						rendered := make([]string, 0, len(g.Members))
						stale := 0
						for _, m := range g.Members {
							if presenceKnown && !present[m] {
								rendered = append(rendered, m+"(stale)")
								stale++
							} else {
								rendered = append(rendered, m)
							}
						}
						name := ""
						if g.Name != "" {
							name = fmt.Sprintf(" %q", g.Name)
						}
						line := fmt.Sprintf("  %-4s%s  %d member(s): %s", g.ID, name, len(g.Members), strings.Join(rendered, ","))
						if stale > 0 {
							line += fmt.Sprintf("  ⚠ %d stale (designator not found — `sch group remove`)", stale)
						}
						fmt.Fprintln(stdout, line)
					}
				}
				if !presenceKnown {
					fmt.Fprintln(stderr, "note: live presence check unavailable — stale members not marked")
				}
				return nil
			},
		}
		c.Flags().BoolVar(&asJSON, "json", false, "emit groups as JSON")
		c.Flags().BoolVar(&allPages, "all-pages", false, "list every page's groups, not just the active page")
		group.AddCommand(c)
	}

	// ── add ──
	{
		var groupRef, membersRaw string
		c := &cobra.Command{
			Use:     "add",
			Short:   "Add members to an existing group",
			Args:    cobra.NoArgs,
			Example: `  easyeda sch group add --group g1 --members C6,C7`,
			RunE: func(cmd *cobra.Command, args []string) error {
				_, _, docUUID, _, st, groups, err := loadSchGroupsContext(cfg, *window)
				if err != nil {
					return err
				}
				next, g, err := groupsAddMembers(groups, groupRef, splitDesignators(membersRaw))
				if err != nil {
					return err
				}
				if err := saveSchGroups(st, docUUID, next); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "✓ group %s now has %d member(s): %s\n", describeSchGroup(g), len(g.Members), strings.Join(g.Members, ","))
				return nil
			},
		}
		c.Flags().StringVar(&groupRef, "group", "", "group id (g1) or name (required)")
		c.Flags().StringVar(&membersRaw, "members", "", "designators to add — CSV (required)")
		_ = c.MarkFlagRequired("group")
		_ = c.MarkFlagRequired("members")
		group.AddCommand(c)
	}

	// ── remove ──
	{
		var groupRef, membersRaw string
		c := &cobra.Command{
			Use:     "remove",
			Short:   "Remove members from a group (an emptied group is deleted)",
			Args:    cobra.NoArgs,
			Example: `  easyeda sch group remove --group g1 --members C6`,
			RunE: func(cmd *cobra.Command, args []string) error {
				_, _, docUUID, _, st, groups, err := loadSchGroupsContext(cfg, *window)
				if err != nil {
					return err
				}
				next, g, removedGroup, err := groupsRemoveMembers(groups, groupRef, splitDesignators(membersRaw))
				if err != nil {
					return err
				}
				if err := saveSchGroups(st, docUUID, next); err != nil {
					return err
				}
				if removedGroup {
					fmt.Fprintf(stdout, "✓ group %s is now empty — group deleted\n", describeSchGroup(g))
				} else {
					fmt.Fprintf(stdout, "✓ group %s now has %d member(s): %s\n", describeSchGroup(g), len(g.Members), strings.Join(g.Members, ","))
				}
				return nil
			},
		}
		c.Flags().StringVar(&groupRef, "group", "", "group id (g1) or name (required)")
		c.Flags().StringVar(&membersRaw, "members", "", "designators to remove — CSV (required)")
		_ = c.MarkFlagRequired("group")
		_ = c.MarkFlagRequired("members")
		group.AddCommand(c)
	}

	// ── ungroup ──
	{
		var groupRef string
		c := &cobra.Command{
			Use:     "ungroup",
			Short:   "Dissolve a group (removes the relation only — no primitive is touched)",
			Args:    cobra.NoArgs,
			Example: `  easyeda sch group ungroup --group g1`,
			RunE: func(cmd *cobra.Command, args []string) error {
				_, _, docUUID, _, st, groups, err := loadSchGroupsContext(cfg, *window)
				if err != nil {
					return err
				}
				next, g, err := groupsUngroup(groups, groupRef)
				if err != nil {
					return err
				}
				if err := saveSchGroups(st, docUUID, next); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "✓ dissolved group %s (%d member(s) released: %s) — primitives untouched\n",
					describeSchGroup(g), len(g.Members), strings.Join(g.Members, ","))
				return nil
			},
		}
		c.Flags().StringVar(&groupRef, "group", "", "group id (g1) or name (required)")
		_ = c.MarkFlagRequired("group")
		group.AddCommand(c)
	}

	return group
}
