package app

import (
	"testing"
)

func schZoneComps() []layoutComp {
	box := func(cx, cy float64) *layoutBBox {
		return &layoutBBox{MinX: cx - 20, MinY: cy - 10, MaxX: cx + 20, MaxY: cy + 10}
	}
	// Sheet 0..900 × 0..600 (y-UP canvas: top rows are y > 300).
	return []layoutComp{
		{Designator: "U1", ComponentType: "part", BBox: box(450, 300)}, // center
		{Designator: "U3", ComponentType: "part", BBox: box(100, 500)}, // left-top (large y = visually up)
		{Designator: "C5", ComponentType: "part", BBox: box(800, 100)}, // right-bottom — NOT left-top
		{Designator: "J1", ComponentType: "part", BBox: nil},           // no bbox → skipped
	}
}

// TestFindSchZoneViolations: only claimed parts outside their zone rect are
// flagged; absent/bbox-less designators are skipped, order is deterministic.

// TestFindSchZoneViolationsYUp pins the row semantics: the canvas is y-UP
// (probe-proven 2026-07-19), so "top" is the LARGER-y half. A part at large y
// must satisfy a -top claim and violate a -bottom claim.

func TestParseSchZoneSpec(t *testing.T) {
	raw := []byte(`{"modules":[
		{"name":"MCU","page":"P1","zone":"center","parts":["U1"]},
		{"name":"unzoned","parts":["X1"]},
		{"name":"POWER","zone":"Left-Top","parts":["u3","C5","u3"]}
	]}`)
	claims, err := parseSchZoneSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 {
		t.Fatalf("claims = %v, want MCU+POWER (unzoned skipped)", claims)
	}
	if claims["MCU"].Page != "P1" || claims["MCU"].Zone != "center" {
		t.Errorf("MCU claim = %+v", claims["MCU"])
	}
	p := claims["POWER"]
	if p.Zone != "left-top" || len(p.Parts) != 2 || p.Parts[0] != "C5" || p.Parts[1] != "U3" {
		t.Errorf("POWER claim = %+v, want normalized deduped sorted parts", p)
	}
	if _, err := parseSchZoneSpec([]byte(`{"modules":[{"name":"A","zone":"middle","parts":["U1"]}]}`)); err == nil {
		t.Error("unknown zone name accepted")
	}
	if _, err := parseSchZoneSpec([]byte(`{"modules":[{"name":"A","parts":["U1"]}]}`)); err == nil {
		t.Error("spec with no zoned modules accepted")
	}
	if _, err := parseSchZoneSpec([]byte(`{"modules":[
		{"name":"POWER","page":"P1","zone":"left","parts":["U1"]},
		{"name":"POWER","page":"P2","zone":"right","parts":["U2"]}
	]}`)); err == nil {
		t.Error("duplicate cross-page module names must fail instead of silently overwriting")
	}
}

func TestParseSchZoneModuleFlags(t *testing.T) {
	claims, err := parseSchZoneModuleFlags([]string{"POWER=left-top:U3,C5"})
	if err != nil {
		t.Fatal(err)
	}
	if claims["POWER"].Zone != "left-top" || len(claims["POWER"].Parts) != 2 {
		t.Errorf("claims = %+v", claims["POWER"])
	}
	for _, bad := range []string{"noequals", "A=nocolon", "A=badzone:U1"} {
		if _, err := parseSchZoneModuleFlags([]string{bad}); err == nil {
			t.Errorf("malformed --module %q accepted", bad)
		}
	}
}

func TestGroupSchZoneClaimsByPage(t *testing.T) {
	claims := map[string]*schZoneClaim{
		"MCU":        {Page: "P1", Zone: "left-top", Parts: []string{"U1"}},
		"POWER":      {Page: "P2", Zone: "left", Parts: []string{"U2"}},
		"PERIPHERAL": {Page: "P3", Zone: "right", Parts: []string{"J1"}},
		"LOCAL":      {Zone: "center", Parts: []string{"R1"}},
	}
	ids := map[string]string{"P1": "page-mcu", "P2": "page-power", "P3": "page-peripheral"}
	got, err := groupSchZoneClaimsByPage(claims, "page-mcu", func(selector string) (string, error) {
		return ids[selector], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("grouped into %d pages, want 3: %v", len(got), got)
	}
	if got["page-mcu"]["MCU"] == nil || got["page-mcu"]["LOCAL"] == nil {
		t.Fatalf("explicit and active-page claims not grouped together: %v", got["page-mcu"])
	}
	if got["page-power"]["POWER"] == nil || got["page-peripheral"]["PERIPHERAL"] == nil {
		t.Fatalf("page selectors did not resolve: %v", got)
	}
}

// **判所见**:partition 模式的框是从活体模块 bbox 反推的,与固定九宫格无关 ——
// 有画出来的几何就按它判,否则一张画得好好的图会被九宫格判成违规(真机实测:
// 单模块页铺满整纸、认领 center、画的是 partition 框,lint 报 2 处 zone-violation)。
