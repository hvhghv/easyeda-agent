package app

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// ── CRUD core ───────────────────────────────────────────────────────────────

func mkGroup(id, name string, members ...string) *schGroup {
	return &workflow.Group{ID: id, Name: name, Members: members}
}

func TestNextGroupID(t *testing.T) {
	cases := []struct {
		name   string
		groups []*schGroup
		want   string
	}{
		{"empty", nil, "g1"},
		{"sequential", []*schGroup{mkGroup("g1", ""), mkGroup("g2", "")}, "g3"},
		{"hole never reuses", []*schGroup{mkGroup("g3", "")}, "g4"},
		{"ignores non-matching ids", []*schGroup{mkGroup("weird", ""), mkGroup("g2", "")}, "g3"},
	}
	for _, tc := range cases {
		if got := nextGroupID(tc.groups); got != tc.want {
			t.Errorf("%s: nextGroupID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGroupsCreate(t *testing.T) {
	// fresh create: id allocated, members normalized (upper-case, sorted, deduped)
	out, g, err := groupsCreate(nil, "mcu-core", []string{"r1", "C5", "U2", "r1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if g.ID != "g1" || g.Name != "mcu-core" {
		t.Fatalf("got id=%q name=%q", g.ID, g.Name)
	}
	if strings.Join(g.Members, ",") != "C5,R1,U2" {
		t.Fatalf("members = %v", g.Members)
	}
	if len(out) != 1 {
		t.Fatalf("out len = %d", len(out))
	}

	// duplicate member across groups is refused and names the owning group
	_, _, err = groupsCreate(out, "", []string{"C9", "c5"})
	if err == nil || !strings.Contains(err.Error(), "g1") || !strings.Contains(err.Error(), "C5") {
		t.Fatalf("dup-member error should name g1 and C5, got: %v", err)
	}

	// empty members is refused
	if _, _, err := groupsCreate(out, "", []string{" ", ""}); err == nil {
		t.Fatal("empty members should error")
	}

	// second group gets g2
	_, g2, err := groupsCreate(out, "", []string{"C9"})
	if err != nil || g2.ID != "g2" {
		t.Fatalf("second create: g=%v err=%v", g2, err)
	}
}

func TestGroupsAddMembers(t *testing.T) {
	groups := []*schGroup{mkGroup("g1", "", "C5", "R1"), mkGroup("g2", "", "U9")}

	// normal add merges + sorts
	_, g, err := groupsAddMembers(groups, "g1", []string{"c6"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if strings.Join(g.Members, ",") != "C5,C6,R1" {
		t.Fatalf("members = %v", g.Members)
	}

	// re-adding an existing member is a no-op, not an error
	if _, g, err = groupsAddMembers(groups, "g1", []string{"C5"}); err != nil || len(g.Members) != 3 {
		t.Fatalf("re-add: g=%v err=%v", g, err)
	}

	// member of ANOTHER group is refused
	if _, _, err = groupsAddMembers(groups, "g1", []string{"U9"}); err == nil || !strings.Contains(err.Error(), "g2") {
		t.Fatalf("cross-group add should name g2, got: %v", err)
	}

	// unknown group
	if _, _, err = groupsAddMembers(groups, "g99", []string{"C7"}); err == nil {
		t.Fatal("unknown group should error")
	}
}

func TestGroupsRemoveMembersAndAutoDelete(t *testing.T) {
	groups := []*schGroup{mkGroup("g1", "", "C5", "R1"), mkGroup("g2", "", "U9")}

	// partial removal keeps the group
	out, g, removed, err := groupsRemoveMembers(groups, "g1", []string{"c5"})
	if err != nil || removed {
		t.Fatalf("remove: removed=%v err=%v", removed, err)
	}
	if strings.Join(g.Members, ",") != "R1" || len(out) != 2 {
		t.Fatalf("members=%v out=%d", g.Members, len(out))
	}

	// removing the last member auto-deletes the group
	out, _, removed, err = groupsRemoveMembers(out, "g1", []string{"R1"})
	if err != nil || !removed {
		t.Fatalf("last remove: removed=%v err=%v", removed, err)
	}
	if len(out) != 1 || out[0].ID != "g2" {
		t.Fatalf("out = %v", out)
	}

	// removing a non-member errors
	if _, _, _, err = groupsRemoveMembers(out, "g2", []string{"C5"}); err == nil {
		t.Fatal("non-member removal should error")
	}
}

func TestGroupsUngroup(t *testing.T) {
	groups := []*schGroup{mkGroup("g1", "", "C5"), mkGroup("g2", "power", "U9")}
	out, g, err := groupsUngroup(groups, "power") // by name
	if err != nil || g.ID != "g2" {
		t.Fatalf("ungroup: g=%v err=%v", g, err)
	}
	if len(out) != 1 || out[0].ID != "g1" {
		t.Fatalf("out = %v", out)
	}
	if _, _, err = groupsUngroup(out, "g2"); err == nil {
		t.Fatal("ungroup of a missing group should error")
	}
}

func TestFindSchGroupNameAmbiguity(t *testing.T) {
	groups := []*schGroup{mkGroup("g1", "pwr", "C5"), mkGroup("g2", "pwr", "U9")}
	if _, err := findSchGroup(groups, "pwr"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous name should error, got: %v", err)
	}
	// id lookup still works
	if g, err := findSchGroup(groups, "g2"); err != nil || g.ID != "g2" {
		t.Fatalf("id lookup: g=%v err=%v", g, err)
	}
}

func TestFindPartialGroups(t *testing.T) {
	groups := []*schGroup{
		mkGroup("g1", "", "C5", "R1", "U2"),
		mkGroup("g2", "", "U9", "C9"),
		mkGroup("g3", "", "D1"),
	}
	cases := []struct {
		name     string
		selected []string
		wantIDs  []string
	}{
		{"no overlap", []string{"R8", "C8"}, nil},
		{"full coverage ok", []string{"c5", "r1", "u2"}, nil},
		{"partial one group", []string{"C5", "R8"}, []string{"g1"}},
		{"partial two groups", []string{"C5", "U9"}, []string{"g1", "g2"}},
		{"single-member group fully covered", []string{"D1"}, nil},
	}
	for _, tc := range cases {
		got := findPartialGroups(groups, tc.selected)
		var ids []string
		for _, g := range got {
			ids = append(ids, g.ID)
		}
		if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
			t.Errorf("%s: partial = %v, want %v", tc.name, ids, tc.wantIDs)
		}
	}
}

// ── attachment expansion geometry ───────────────────────────────────────────

func TestExpandGroupAttachmentsSimpleStub(t *testing.T) {
	// member pin at (100,100); stub wire to (140,100); flag at (140,100)
	in := groupExpandInput{
		MemberPins: [][2]float64{{100, 100}},
		Wires:      []schGroupWire{{ID: "w1", Points: []float64{100, 100, 140, 100}}},
		Flags:      []schGroupFlag{{ID: "f1", X: 140, Y: 100}},
	}
	got := expandGroupAttachments(in)
	if strings.Join(got.WireIDs, ",") != "w1" || strings.Join(got.FlagIDs, ",") != "f1" || got.SharedTrees != 0 {
		t.Fatalf("expansion = %+v", got)
	}
}

func TestExpandGroupAttachmentsMergedCollinearTree(t *testing.T) {
	// EasyEDA merges collinear stubs: two wire primitives share vertex (120,100);
	// the flag hangs off the FAR wire's end. Both wires + flag must ride along.
	in := groupExpandInput{
		MemberPins: [][2]float64{{100, 100}},
		Wires: []schGroupWire{
			{ID: "w1", Points: []float64{100, 100, 120, 100}},
			{ID: "w2", Points: []float64{120, 100, 160, 100}},
		},
		Flags: []schGroupFlag{{ID: "f1", X: 160, Y: 100}},
	}
	got := expandGroupAttachments(in)
	if strings.Join(got.WireIDs, ",") != "w1,w2" || strings.Join(got.FlagIDs, ",") != "f1" {
		t.Fatalf("expansion = %+v", got)
	}
}

func TestExpandGroupAttachmentsFlagMidSpan(t *testing.T) {
	// A merged flag can sit mid-span (the `sch disconnect` lesson): flag at
	// (130,100) on the interior of a (100,100)-(160,100) wire still rides along.
	in := groupExpandInput{
		MemberPins: [][2]float64{{100, 100}},
		Wires:      []schGroupWire{{ID: "w1", Points: []float64{100, 100, 160, 100}}},
		Flags:      []schGroupFlag{{ID: "f1", X: 130, Y: 100}},
	}
	got := expandGroupAttachments(in)
	if strings.Join(got.FlagIDs, ",") != "f1" {
		t.Fatalf("mid-span flag not picked up: %+v", got)
	}
}

func TestExpandGroupAttachmentsSharedTreeSkipped(t *testing.T) {
	// Wire from a member pin to a NON-member pin is real inter-part wiring:
	// moving it rigidly would tear the far connection — skip + count.
	in := groupExpandInput{
		MemberPins: [][2]float64{{100, 100}},
		OtherPins:  [][2]float64{{200, 100}},
		Wires:      []schGroupWire{{ID: "w1", Points: []float64{100, 100, 200, 100}}},
		Flags:      []schGroupFlag{{ID: "f1", X: 200, Y: 100}},
	}
	got := expandGroupAttachments(in)
	if len(got.WireIDs) != 0 || len(got.FlagIDs) != 0 || got.SharedTrees != 1 {
		t.Fatalf("shared tree must be skipped and counted: %+v", got)
	}
}

func TestExpandGroupAttachmentsTJunctionForeign(t *testing.T) {
	// T-junction: the stub itself is member-only, but a second wire T-taps its
	// midpoint and runs to a foreign pin. EasyEDA merges endpoint-on-wire contact
	// into one electrical tree, so the WHOLE tree is shared → skip.
	in := groupExpandInput{
		MemberPins: [][2]float64{{100, 100}},
		OtherPins:  [][2]float64{{120, 160}},
		Wires: []schGroupWire{
			{ID: "w1", Points: []float64{100, 100, 140, 100}},
			{ID: "w2", Points: []float64{120, 100, 120, 160}}, // taps w1 mid-span
		},
	}
	got := expandGroupAttachments(in)
	if len(got.WireIDs) != 0 || got.SharedTrees != 1 {
		t.Fatalf("T-junction shared tree must be skipped: %+v", got)
	}
}

func TestExpandGroupAttachmentsUnrelatedWireIgnored(t *testing.T) {
	in := groupExpandInput{
		MemberPins: [][2]float64{{100, 100}},
		Wires: []schGroupWire{
			{ID: "w1", Points: []float64{100, 100, 140, 100}},
			{ID: "w-far", Points: []float64{500, 500, 540, 500}},
		},
		Flags: []schGroupFlag{{ID: "f-far", X: 540, Y: 500}},
	}
	got := expandGroupAttachments(in)
	if strings.Join(got.WireIDs, ",") != "w1" || len(got.FlagIDs) != 0 {
		t.Fatalf("unrelated wire/flag must not ride along: %+v", got)
	}
}

func TestExpandGroupAttachmentsMultiPinFanout(t *testing.T) {
	// Two pins of the same member, each with its own stub+flag; plus a polyline
	// stub with a bend (L-shape) — all included, ids sorted.
	in := groupExpandInput{
		MemberPins: [][2]float64{{100, 100}, {100, 80}},
		Wires: []schGroupWire{
			{ID: "wb", Points: []float64{100, 80, 120, 80, 120, 60}}, // L-shaped stub
			{ID: "wa", Points: []float64{100, 100, 140, 100}},
		},
		Flags: []schGroupFlag{
			{ID: "fa", X: 140, Y: 100},
			{ID: "fb", X: 120, Y: 60},
		},
	}
	got := expandGroupAttachments(in)
	if strings.Join(got.WireIDs, ",") != "wa,wb" || strings.Join(got.FlagIDs, ",") != "fa,fb" {
		t.Fatalf("fanout expansion = %+v", got)
	}
}

// ── completeness precheck: half-move residue detection ──────────────────────

func TestExpandGroupAttachmentsPathologicalResidueRejected(t *testing.T) {
	// EXACT live geometry (2026-08-12 半搬事故残骸): member pin R1:1 at (810,475);
	// the stranded stub's line is [820,475 835,475 845,475 835,475] — a
	// folded-back 4-vertex polyline whose start sits 10 units OFF the pin. It
	// attaches to nothing (eps=0.5 misses it), so the old expansion silently
	// left it behind → half-move. The completeness precheck must flag it.
	in := groupExpandInput{
		MemberPins: [][2]float64{{810, 475}},
		Wires: []schGroupWire{
			{ID: "w-sick", Points: []float64{820, 475, 835, 475, 845, 475, 835, 475}},
		},
		Flags: []schGroupFlag{{ID: "f-sick", X: 845, Y: 475}},
	}
	got := expandGroupAttachments(in)
	if len(got.WireIDs) != 0 || len(got.FlagIDs) != 0 {
		t.Fatalf("residue must not be silently included: %+v", got)
	}
	if len(got.Suspects) != 1 || got.Suspects[0].WireID != "w-sick" {
		t.Fatalf("residue must be flagged as a suspect: %+v", got.Suspects)
	}
	s := got.Suspects[0]
	if s.PinX != 810 || s.PinY != 475 || s.X0 != 820 || s.Y0 != 475 {
		t.Fatalf("suspect must carry the grazed pin + wire endpoints: %+v", s)
	}
}

func TestExpandGroupAttachmentsResidueBesideHealthyStub(t *testing.T) {
	// A healthy stub AND a residue wire on the same member: the healthy one
	// expands normally, the residue still flags — one suspect per wire.
	in := groupExpandInput{
		MemberPins: [][2]float64{{810, 475}, {810, 455}},
		Wires: []schGroupWire{
			{ID: "w-ok", Points: []float64{810, 455, 850, 455}},
			{ID: "w-sick", Points: []float64{820, 475, 835, 475}},
		},
		Flags: []schGroupFlag{{ID: "f-ok", X: 850, Y: 455}},
	}
	got := expandGroupAttachments(in)
	if strings.Join(got.WireIDs, ",") != "w-ok" || strings.Join(got.FlagIDs, ",") != "f-ok" {
		t.Fatalf("healthy stub must still expand: %+v", got)
	}
	if len(got.Suspects) != 1 || got.Suspects[0].WireID != "w-sick" {
		t.Fatalf("residue must be flagged exactly once: %+v", got.Suspects)
	}
}

func TestExpandGroupAttachmentsParallelNeighborStubNotSuspect(t *testing.T) {
	// EXACT live geometry (终验 2026-08-12 误拒): R1's REAL pin at (820,475)
	// (pin half-pitch is ±20, not the ±10 of the earlier hand-built scene);
	// U2:EN's healthy stub runs PARALLEL at y=485 — [815,485 → 835,485] passes
	// 10 RADIAL units from the pin, inside a naive radius-12 test, but its
	// perpendicular offset is 10 ≫ 1: a different line entirely, never residue.
	in := groupExpandInput{
		MemberPins: [][2]float64{{820, 475}},
		Wires: []schGroupWire{
			{ID: "w-r1", Points: []float64{820, 475, 850, 475}},   // R1's own stub (attaches)
			{ID: "w-u2en", Points: []float64{815, 485, 835, 485}}, // parallel neighbor
		},
		Flags: []schGroupFlag{{ID: "f-r1", X: 850, Y: 475}},
	}
	got := expandGroupAttachments(in)
	if len(got.Suspects) != 0 {
		t.Fatalf("parallel neighbor stub must NOT be a suspect: %+v", got.Suspects)
	}
	if strings.Join(got.WireIDs, ",") != "w-r1" || strings.Join(got.FlagIDs, ",") != "f-r1" {
		t.Fatalf("member's own stub must expand normally: %+v", got)
	}
}

func TestSegmentGrazesPointCollinearBand(t *testing.T) {
	// The graze criterion is COLLINEAR: perp ≤1 AND along-line gap ∈ (0.5, 12].
	cases := []struct {
		name           string
		px, py         float64
		x0, y0, x1, y1 float64
		want           bool
	}{
		{"collinear 10-unit gap (live residue)", 810, 475, 820, 475, 835, 475, true},
		{"collinear gap beyond far end", 845, 475, 820, 475, 835, 475, true},
		{"collinear but gap over tolerance", 800, 475, 820, 475, 835, 475, false},
		{"collinear attached (gap under eps)", 820, 475, 820, 475, 835, 475, false},
		{"parallel neighbor perp 10 (live false-reject)", 820, 475, 815, 485, 835, 485, false},
		{"perp just over the 1-unit bound", 810, 476.5, 820, 475, 835, 475, false},
		{"slanted carrier, on-line gap ~9.9", 813, 468, 820, 475, 830, 485, true},
		{"slanted carrier, on-line gap over tolerance", 810, 465, 820, 475, 830, 485, false},
	}
	for _, tc := range cases {
		if got := segmentGrazesPoint(tc.px, tc.py, tc.x0, tc.y0, tc.x1, tc.y1); got != tc.want {
			t.Errorf("%s: segmentGrazesPoint = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestExpandGroupAttachmentsCleanScenesHaveNoSuspects(t *testing.T) {
	// Attached stubs (exact pin contact) and genuinely-far wires must never
	// trip the precheck — only the collinear graze band does.
	cases := []struct {
		name string
		in   groupExpandInput
	}{
		{"attached stub", groupExpandInput{
			MemberPins: [][2]float64{{100, 100}},
			Wires:      []schGroupWire{{ID: "w1", Points: []float64{100, 100, 140, 100}}},
		}},
		{"far unrelated wire", groupExpandInput{
			MemberPins: [][2]float64{{100, 100}},
			Wires:      []schGroupWire{{ID: "w-far", Points: []float64{500, 500, 540, 500}}},
		}},
		{"shared tree stays a warning, not a suspect", groupExpandInput{
			MemberPins: [][2]float64{{100, 100}},
			OtherPins:  [][2]float64{{200, 100}},
			Wires:      []schGroupWire{{ID: "w1", Points: []float64{100, 100, 200, 100}}},
		}},
	}
	for _, tc := range cases {
		if got := expandGroupAttachments(tc.in); len(got.Suspects) != 0 {
			t.Errorf("%s: unexpected suspects %+v", tc.name, got.Suspects)
		}
	}
}

// ── return-leg dump replay: folded stub parked over a foreign pin ───────────

func TestExpandGroupAttachmentsFoldedStubOverForeignPin(t *testing.T) {
	// Minimal form of the live 悬案: the folded stub [820→835→845→835] sits at
	// +100 with U2:3 directly UNDER its 835 vertex (also its terminal polyline
	// vertex). The wire geometrically PASSES THROUGH 835 (runs 820→845), so
	// this is incidental wire-over-pin contact, NOT deliberate wiring — the
	// tree must ride along with the member, or the group half-moves forever.
	in := groupExpandInput{
		MemberPins: [][2]float64{{820, 475}},
		OtherPins:  [][2]float64{{835, 475}}, // U2:3 under the fold
		Wires: []schGroupWire{
			{ID: "w-fold", Points: []float64{820, 475, 835, 475, 845, 475, 835, 475}},
		},
		Flags: []schGroupFlag{{ID: "f-port", X: 845, Y: 475}},
	}
	got := expandGroupAttachments(in)
	if strings.Join(got.WireIDs, ",") != "w-fold" || strings.Join(got.FlagIDs, ",") != "f-port" {
		t.Fatalf("pass-over foreign contact must not strand the stub: %+v", got)
	}
	if got.SharedTrees != 0 || len(got.Suspects) != 0 {
		t.Fatalf("no shared/suspect expected: %+v", got)
	}

	// Counter-case: the foreign pin at 845 — the tree's true OPEN END (all
	// incident directions point back west). That IS deliberate wiring → shared.
	in.OtherPins = [][2]float64{{845, 475}}
	got = expandGroupAttachments(in)
	if len(got.WireIDs) != 0 || got.SharedTrees != 1 {
		t.Fatalf("open-end foreign contact must stay a shared tree: %+v", got)
	}
}

func TestTreeTerminatesAt(t *testing.T) {
	fold := []schGroupWire{{ID: "w", Points: []float64{820, 475, 835, 475, 845, 475, 835, 475}}}
	cases := []struct {
		name   string
		wires  []schGroupWire
		px, py float64
		want   bool
	}{
		{"folded stub: 820 is an open end", fold, 820, 475, true},
		{"folded stub: 845 is an open end (re-traced tail, same direction twice)", fold, 845, 475, true},
		{"folded stub: 835 is pass-through despite being the terminal VERTEX", fold, 835, 475, false},
		{"plain wire: interior span contact", []schGroupWire{{ID: "w", Points: []float64{100, 100, 200, 100}}}, 150, 100, false},
		{"plain wire: endpoint", []schGroupWire{{ID: "w", Points: []float64{100, 100, 200, 100}}}, 200, 100, true},
		{"T-junction point: three directions", []schGroupWire{
			{ID: "a", Points: []float64{100, 100, 200, 100}},
			{ID: "b", Points: []float64{150, 100, 150, 160}},
		}, 150, 100, false},
		{"no contact at all", fold, 500, 500, false},
	}
	for _, tc := range cases {
		if got := treeTerminatesAt(tc.wires, tc.px, tc.py); got != tc.want {
			t.Errorf("%s: treeTerminatesAt = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestExpandGroupAttachmentsReturnLegDump(t *testing.T) {
	// FULL live dump replay (scratchpad dump-wires/dump-comps, 2026-08-12): the
	// return leg of a +100/-100 pair. Members R1/C5 are at +100 (pins shifted
	// +100 in x from the dump's rest positions), the two folded stragglers
	// 6d5de030/992f1930 sit at +100 where their spans PASS OVER U2:3 (835,475)
	// and C6:1 (890,710). Live this expanded 2+2+2 and stranded them; the
	// terminate-at-foreign rule must include both wires + both their flags.
	dumpWires := []schGroupWire{
		{ID: "ca3f7ab6eaf8d6db", Points: []float64{175, 610, 195, 610}},
		{ID: "a2c541eec38e02e6", Points: []float64{165, 620, 195, 620}},
		{ID: "05f1913f4f534755", Points: []float64{305, 620, 285, 620}},
		{ID: "e7091e49102b44c4", Points: []float64{160, 630, 195, 630}},
		{ID: "0631bb5fa9ccc36b", Points: []float64{815, 485, 835, 485}},
		{ID: "dc79db38db08f8db", Points: []float64{945, 310, 925, 310}},
		{ID: "03d645e612a633ed", Points: []float64{955, 300, 925, 300}},
		{ID: "cf58af16050d691b", Points: []float64{925, 240, 925, 270}},
		{ID: "2f636bbf11f51ac6", Points: []float64{1000, 460, 980, 460}},
		{ID: "e175d8489822eeae", Points: []float64{1120, 460, 1100, 460}},
		{ID: "de552f2d7bedff6b", Points: []float64{1010, 460, 1060, 460}},
		{ID: "a8a5696e57e45fda", Points: []float64{945, 650, 925, 650}},
		{ID: "3d0d9250afc3b1e4", Points: []float64{945, 670, 925, 670}},
		{ID: "7283f81ad3a8232d", Points: []float64{940, 440, 940, 460}},
		{ID: "15fcf26cacf3165d", Points: []float64{120, 670, 140, 670}},
		{ID: "f10636960ad683c6", Points: []float64{180, 640, 180, 670}},
		{ID: "73b5b4d38c34010c", Points: []float64{200, 690, 200, 670}},
		{ID: "9097fc8ca332024f", Points: []float64{240, 700, 240, 670}},
		{ID: "91d8bea283bcb5e1", Points: []float64{260, 690, 260, 670}},
		{ID: "ffb5f5542600133e", Points: []float64{320, 670, 300, 670}},
		{ID: "7146c215674924ce", Points: []float64{580, 710, 600, 710}},
		{ID: "ea610ac561d747ce", Points: []float64{660, 710, 640, 710}},
		{ID: "48fa397237afface", Points: []float64{950, 710, 930, 710}},
		{ID: "5af32079e0d4d0c4", Points: []float64{850, 240, 870, 240}},
		{ID: "69ebf0a39c2744f2", Points: []float64{910, 220, 910, 240}},
		{ID: "6d5de030c74d42ae", Points: []float64{820, 475, 835, 475, 845, 475, 835, 475}},
		{ID: "992f19309a01bae4", Points: []float64{885, 710, 890, 710, 905, 710, 890, 710}},
		{ID: "172ff0339f55d7de", Points: []float64{660, 475, 680, 475}},
		{ID: "66b353cd212cb01d", Points: []float64{725, 710, 745, 710}},
	}
	otherPins := [][2]float64{
		{240, 670}, {200, 670}, {930, 710}, {890, 710}, {140, 670},
		{180, 670}, {600, 710}, {640, 710}, {260, 670}, {300, 670},
		{195, 630}, {195, 620}, {195, 610}, {285, 620}, {910, 240},
		{870, 240}, {835, 485}, {835, 475}, {835, 465}, {835, 455},
		{925, 270}, {925, 300}, {925, 310}, {925, 340}, {925, 350},
		{925, 360}, {925, 370}, {925, 380}, {925, 390}, {925, 400},
		{925, 410}, {925, 420}, {925, 430}, {925, 440}, {925, 450},
		{925, 460}, {925, 470}, {925, 480}, {925, 490}, {925, 500},
		{925, 510}, {925, 520}, {925, 530}, {925, 540}, {925, 550},
		{925, 560}, {925, 570}, {925, 580}, {925, 590}, {925, 600},
		{925, 610}, {925, 620}, {925, 630}, {925, 640}, {925, 650},
		{925, 660}, {925, 670}, {1060, 460}, {1100, 460}, {980, 460},
		{940, 460},
	}
	dumpFlags := []schGroupFlag{
		{ID: "d6ea4a990dac8743", X: 175, Y: 610},
		{ID: "efd9ac4991092d3a", X: 165, Y: 620},
		{ID: "bcf974688534e9e0", X: 305, Y: 620},
		{ID: "28a8576f85f5eafa", X: 815, Y: 485},
		{ID: "78ba73565e481a3c", X: 120, Y: 670},
		{ID: "7b46c83c07a3d804", X: 200.00000000000003, Y: 690},
		{ID: "5df56dabb44a70a7", X: 259.99999999999994, Y: 690},
		{ID: "16a7eafd30a1bc9c", X: 580, Y: 710},
		{ID: "82d8c0448e22e145", X: 910, Y: 220},
		{ID: "b7464e597f031005", X: 160, Y: 630},
		{ID: "cb9d163cf88ade6a", X: 945, Y: 310},
		{ID: "40e4347fd16d59f7", X: 955, Y: 300},
		{ID: "a711dfe2a0cbf64e", X: 925, Y: 240},
		{ID: "046a811e71630703", X: 1010, Y: 459.9999999999999},
		{ID: "0f32f367ed25ef94", X: 180.00000000000003, Y: 640},
		{ID: "583ea7ad96c0be2e", X: 240, Y: 700},
		{ID: "b75cab5e8b176658", X: 320, Y: 670},
		{ID: "65db43d6e91e139f", X: 660, Y: 710},
		{ID: "575da495c7fb20fb", X: 950.0000000000001, Y: 710},
		{ID: "36503a859f4e4a8c", X: 1000, Y: 460},
		{ID: "3f012deae3152d7b", X: 1120, Y: 460},
		{ID: "685a32b3670dcb5a", X: 945, Y: 650},
		{ID: "c2a78d5741f13a97", X: 945, Y: 670},
		{ID: "26cc53caf3d2acf0", X: 940, Y: 440},
		{ID: "4b3db16c9edf4831", X: 850, Y: 240},
		{ID: "b8e4549ee4132dbb", X: 845, Y: 475},
		{ID: "d4fad03d208ac84c", X: 905, Y: 710},
		{ID: "5c01a4ca252866a4", X: 660, Y: 475},
		{ID: "d9ad205729976df0", X: 725, Y: 710},
	}
	memberPins := [][2]float64{{780, 475}, {820, 475}, {845, 710}, {885, 710}}
	got := expandGroupAttachments(groupExpandInput{
		MemberPins: memberPins,
		OtherPins:  otherPins,
		Wires:      dumpWires,
		Flags:      dumpFlags,
	})
	if strings.Join(got.WireIDs, ",") != "6d5de030c74d42ae,992f19309a01bae4" {
		t.Fatalf("both stragglers must be carried: %v", got.WireIDs)
	}
	if strings.Join(got.FlagIDs, ",") != "b8e4549ee4132dbb,d4fad03d208ac84c" {
		t.Fatalf("both stranded flags must be carried: %v", got.FlagIDs)
	}
	if got.SharedTrees != 0 || len(got.Suspects) != 0 {
		t.Fatalf("no shared/suspect expected on the dump scene: %+v", got)
	}
}

// ── move-set flattening ─────────────────────────────────────────────────────

func TestSchGroupMoveSetAllIDs(t *testing.T) {
	set := &schGroupMoveSet{
		ComponentIDs: []string{"c1", "c2"},
		Expansion:    groupExpansion{WireIDs: []string{"w1"}, FlagIDs: []string{"f1"}},
	}
	if got := strings.Join(set.AllIDs(), ","); got != "c1,c2,w1,f1" {
		t.Fatalf("AllIDs = %q", got)
	}
}
