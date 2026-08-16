package app

import (
	"strings"
	"testing"
)

// sheetTidyDiag 的价值全在「分清是哪一种装不下」——两种失败的修法是相反的:
// 单区超带要换纸/拆页(重排没用),总量超要拆页或收紧区内。给一句通用建议
// 等于让人两条都试一遍,而 needW=0 那版连差多少都不说。

func band1000x600() layoutBBox {
	return layoutBBox{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 600}
}

func TestSheetTidyDiag_SingleZoneTooBig(t *testing.T) {
	groups := []zonePackGroup{
		{ID: "MCU", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 700}}, // 高 700 > 带高 600
		{ID: "PWR", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 200, MaxY: 100}},
	}
	d := sheetTidyDiag("base", groups, band1000x600(), nil)
	if d.NeedH != 700 || d.NeedW != 400 {
		t.Errorf("need 该是最大区的尺寸,得到 %.0f×%.0f", d.NeedW, d.NeedH)
	}
	for _, want := range []string{"MCU", "自己就放不进", "重排没用"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("缺 %q:%s", want, d.Reason)
		}
	}
}

func TestSheetTidyDiag_TotalAreaTooBig(t *testing.T) {
	// 每个都装得下,合起来超面积 —— 修法是拆页,不是换纸。
	var groups []zonePackGroup
	for _, id := range []string{"A", "B", "C", "D"} {
		groups = append(groups, zonePackGroup{ID: id,
			BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 500, MaxY: 400}}) // 4×200000 > 600000
	}
	d := sheetTidyDiag("base", groups, band1000x600(), nil)
	if strings.Contains(d.Reason, "自己就放不进") {
		t.Errorf("不该判成单区超带:%s", d.Reason)
	}
	for _, want := range []string{"总面积", "拆页"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("缺 %q:%s", want, d.Reason)
		}
	}
}

func TestSheetTidyDiag_PurelyPackingFailure(t *testing.T) {
	// 单区不超、总面积也够 —— 那就是行排/图签避让排不下,该调 gap 或拆最大的区。
	groups := []zonePackGroup{
		{ID: "A", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 400}},
		{ID: "B", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 100}},
	}
	d := sheetTidyDiag("base", groups, band1000x600(), nil)
	if !strings.Contains(d.Reason, "排布") {
		t.Errorf("该判成排布问题:%s", d.Reason)
	}
	// 三种情形下 need 都必须有值 —— needW=0 needH=0 正是这条判据此前的症状。
	if d.NeedW == 0 || d.NeedH == 0 {
		t.Errorf("need 不该是 0:%.0f×%.0f", d.NeedW, d.NeedH)
	}
}

func TestSheetTidyDiag_AlwaysCarriesBandAndNeed(t *testing.T) {
	d := sheetTidyDiag("base", nil, band1000x600(), nil)
	if d.BandW != 1000 || d.BandH != 600 {
		t.Errorf("band 该照实带出:%.0f×%.0f", d.BandW, d.BandH)
	}
}
