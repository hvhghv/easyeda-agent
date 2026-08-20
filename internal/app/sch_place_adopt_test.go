package app

import (
	"reflect"
	"strings"
	"testing"
)

func adoptComp(id, designator, typ string, x, y float64) layoutComp {
	return layoutComp{ID: id, Designator: designator, ComponentType: typ, X: x, Y: y, AnchorAvailable: true}
}

func TestSchAdoptOrphanPlacementAdoptsTheNewPartAtTheRequestedSpot(t *testing.T) {
	known := map[string]bool{"old-1": true}
	live := []layoutComp{
		adoptComp("old-1", "R9", "part", 400, 300),
		adoptComp("ghost-1", "U2", "part", 400, 300),
	}
	v := schAdoptOrphanPlacement(known, live, schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted == nil || v.Adopted.ID != "ghost-1" {
		t.Fatalf("adopted=%+v, want ghost-1", v.Adopted)
	}
	if !reflect.DeepEqual(v.CandidateIDs(), []string{"ghost-1"}) {
		t.Fatalf("candidates=%v", v.CandidateIDs())
	}
	if !strings.Contains(v.Reason, "ghost-1") {
		t.Fatalf("reason must name the adopted id: %q", v.Reason)
	}
}

// 负对照 A:place 真失败 —— 画布上没有新器件。不许收编出任何 id,也不许把
// 页面上原有的同型器件报成疑似残件。
func TestSchAdoptOrphanPlacementNeverInventsAnIDWhenNothingLanded(t *testing.T) {
	known := map[string]bool{"old-1": true}
	live := []layoutComp{adoptComp("old-1", "R9", "part", 400, 300)}
	v := schAdoptOrphanPlacement(known, live, schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted != nil || len(v.Candidates) != 0 {
		t.Fatalf("verdict=%+v, want nothing adopted and nothing named", v)
	}
	if !strings.Contains(v.Reason, "没有落地") {
		t.Fatalf("reason must state the place did not land: %q", v.Reason)
	}
}

// 负对照 B:同坐标、同类型、**页面上早就有**的实例 —— 快照里有它,所以它永远
// 不会被认成本次的产物。这是「不许误删已有同型器件」唯一的机械保证。
func TestSchAdoptOrphanPlacementNeverAdoptsAPreexistingTwin(t *testing.T) {
	twin := adoptComp("preexisting", "U2", "part", 400, 300)
	known := map[string]bool{"preexisting": true}
	v := schAdoptOrphanPlacement(known, []layoutComp{twin}, schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted != nil || len(v.Candidates) != 0 {
		t.Fatalf("verdict=%+v, want the pre-existing twin left alone", v)
	}
}

// 负对照 C:快照缺失时上层必须整个关掉收编。这里钉的是「known 为空集 ≠ 快照缺失」
// —— 空集是合法的空白页快照,缺失由调用方用 nil 表达(见 bapAdoptAfterPlaceFailure)。
func TestSchAdoptOrphanPlacementEmptySnapshotStillMeansEverythingIsNew(t *testing.T) {
	v := schAdoptOrphanPlacement(map[string]bool{},
		[]layoutComp{adoptComp("ghost-1", "U2", "part", 400, 300)},
		schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted == nil || v.Adopted.ID != "ghost-1" {
		t.Fatalf("verdict=%+v, want ghost-1 adopted on an empty page", v)
	}
}

func TestSchAdoptOrphanPlacementRejectsNonPartsAndDistantParts(t *testing.T) {
	live := []layoutComp{
		adoptComp("flag-1", "", "netflag", 400, 300), // 门②:标志不是放置件
		adoptComp("sheet-1", "", "sheet", 400, 300),  // 门②:图框绝不能被删
		adoptComp("far-1", "R7", "part", 460, 300),   // 门③:不在下发坐标附近
		{ID: "noxy-1", ComponentType: "part"},        // 无可信坐标 → 不猜
	}
	v := schAdoptOrphanPlacement(map[string]bool{}, live, schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted != nil || len(v.Candidates) != 0 {
		t.Fatalf("verdict=%+v, want no adoption", v)
	}
}

func TestSchAdoptOrphanPlacementNamesEveryCandidateWhenAmbiguous(t *testing.T) {
	live := []layoutComp{
		adoptComp("ghost-b", "U2", "part", 402, 301),
		adoptComp("ghost-a", "U2", "part", 400, 300),
	}
	v := schAdoptOrphanPlacement(map[string]bool{}, live, schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted != nil {
		t.Fatalf("ambiguous match must not be adopted: %+v", v.Adopted)
	}
	if got := v.CandidateIDs(); !reflect.DeepEqual(got, []string{"ghost-a", "ghost-b"}) {
		t.Fatalf("candidates=%v, want both ghosts named", got)
	}
}

func TestSchAdoptOrphanPlacementToleranceIsOneGridStep(t *testing.T) {
	inside := adoptComp("in", "U2", "part", 400+schAdoptTolerance, 300)
	outside := adoptComp("out", "U2", "part", 400+schAdoptTolerance+0.5, 300)
	if v := schAdoptOrphanPlacement(map[string]bool{}, []layoutComp{inside},
		schAdoptRequest{X: 400, Y: 300}); v.Adopted == nil {
		t.Fatal("a part exactly at the tolerance edge must be adopted")
	}
	if v := schAdoptOrphanPlacement(map[string]bool{}, []layoutComp{outside},
		schAdoptRequest{X: 400, Y: 300}); v.Adopted != nil {
		t.Fatal("a part past the tolerance must not be adopted")
	}
}

func TestSchPageComponentSnapshotKeepsEveryTypeAndSkipsBlankIDs(t *testing.T) {
	snap := schPageComponentSnapshot([]layoutComp{
		adoptComp("c1", "R1", "part", 0, 0),
		adoptComp("f1", "", "netflag", 0, 0),
		{ComponentType: "part"},
	})
	if len(snap) != 2 || !snap["c1"] || !snap["f1"] {
		t.Fatalf("snapshot=%v, want every id-bearing component of any type", snap)
	}
}

func TestSchAdoptResidueGuidanceIsRunnable(t *testing.T) {
	var b strings.Builder
	schAdoptResidueGuidance(&b, []string{"id-1", "id-2"})
	out := b.String()
	if !strings.Contains(out, "easyeda sch prim-delete --ids id-1,id-2") {
		t.Fatalf("guidance must carry a runnable command:\n%s", out)
	}
	if !strings.Contains(out, "wedge") {
		t.Fatalf("guidance must explain the wedge case:\n%s", out)
	}
	var empty strings.Builder
	schAdoptResidueGuidance(&empty, nil)
	if empty.Len() != 0 {
		t.Fatalf("no residue → no noise, got %q", empty.String())
	}
}
