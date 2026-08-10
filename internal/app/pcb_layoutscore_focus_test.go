package app

import (
	"strings"
	"testing"
)

// 聚焦卡的三条核心契约：直接扣分/关联提及分列、位号词边界不误配、blocking 收录。
func TestPartFocus_DirectRelatedAndBoundary(t *testing.T) {
	rep := &layoutScoreReport{
		Blocking: []pcbCheckFinding{
			{Type: "component-overlap", Designator: "C1", Message: "footprint overlap: C1 ↔ U2 by 3×4 mil"},
			{Type: "off-board", Designator: "C10", Message: "C10 sits outside the board outline"},
		},
		Dimensions: []scoreDimension{
			{ID: dimProtection, Score: 60, Status: dimScored, Contributors: []scoreContributor{
				{Designator: "TVS1", Penalty: 9.6, Detail: "protection: 323mil from port pad J2.B4A9 on net VBUS_IN"},
			}},
			{ID: dimTidy, Score: 80, Status: dimScored, Contributors: []scoreContributor{
				{Designator: "C1", Penalty: 1.1, Detail: "anchor off grid"},
				{Designator: "C10", Penalty: 2.2, Detail: "anchor off grid"},
			}},
			{ID: dimRF, Score: 0, Status: dimSkipped, Reason: "no antenna"},
		},
	}
	snap := &boardSnapshot{Components: []boardComp{
		{Designator: "J2", X: 100, Y: -200, Layer: 1},
		{Designator: "C1", X: 50, Y: -50, Layer: 1},
	}}

	// J2:自己零扣分,但 TVS1 的归因提及它(词边界:J2.B4A9 里的 J2 命中)。
	j2 := buildPartFocus(rep, snap, "j2")
	if !j2.Found {
		t.Fatal("J2 is on the board")
	}
	if len(j2.Entries) != 1 || !j2.Entries[0].Related || j2.Entries[0].Dimension != dimProtection {
		t.Fatalf("J2 must surface exactly the TVS1 mention as related, got %+v", j2.Entries)
	}
	if !strings.Contains(j2.Entries[0].Detail, "TVS1") {
		t.Errorf("related entry must name whose attribution mentioned it, got %q", j2.Entries[0].Detail)
	}

	// C1:直接扣分(tidy)+blocking;绝不能把 C10 的条目错配进来(词边界)。
	c1 := buildPartFocus(rep, snap, "C1")
	if len(c1.Blocking) != 1 || !strings.Contains(c1.Blocking[0], "C1 ↔ U2") {
		t.Fatalf("C1 must carry its own blocking only, got %v", c1.Blocking)
	}
	if len(c1.Entries) != 1 || c1.Entries[0].Related || c1.Entries[0].Penalty != 1.1 {
		t.Fatalf("C1 must carry exactly its own tidy penalty, got %+v", c1.Entries)
	}

	// 未知位号如实说。
	if buildPartFocus(rep, snap, "U99").Found {
		t.Error("U99 is not on the board")
	}
}
