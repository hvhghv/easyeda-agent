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

func TestExpandGroupAttachmentsCleanScenesHaveNoSuspects(t *testing.T) {
	// Attached stubs (exact pin contact) and genuinely-far wires must never
	// trip the precheck — only the graze-without-attach band (0.5, 12] does.
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
