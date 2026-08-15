package app

import (
	"strings"
	"testing"
)

// 这条诊断的风险是**把「摆得不好」误判成「装不下」** —— 那会让人白换一张大纸,
// 而真正的毛病(两个组顶在一起)原封不动。所以判据只问单个模块自己塞不塞得进,
// 完全不管模块之间怎么排。

func a4() (layoutBBox, *layoutBBox) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	ko := &layoutBBox{MinX: 468, MinY: 0, MaxX: 1170, MaxY: 198}
	return sheet, ko
}

func TestDiagnoseZoneCapacity_WroomOnA4DoesNotFit(t *testing.T) {
	// 真实现场:ESP32-S3-WROOM-1 的虚拟组(本体 421 + 自己的 marker)高 462。
	sheet, ko := a4()
	mods := []partitionModule{{Name: "U2", BBox: layoutBBox{MinX: 430, MinY: 214, MaxX: 882, MaxY: 676}}}
	cap := diagnoseZoneCapacity(sheet, ko, mods, defaultPartitionOpts())
	if cap.Fits {
		t.Fatalf("A4 上一颗 WROOM-1 该判装不下,得到 fits=true(need %.0f×%.0f have %.0f×%.0f)",
			cap.NeedW, cap.NeedH, cap.HaveW, cap.HaveH)
	}
	if cap.Blocking != "U2" {
		t.Errorf("该点名是谁装不下,得到 %q", cap.Blocking)
	}
	if cap.Suggest != "A3" {
		t.Errorf("该建议换 A3,得到 %q", cap.Suggest)
	}
	adv := capacityAdvice(cap)
	for _, want := range []string{"装不下", "U2", "A3", "调 margin/gutter 无解"} {
		if !strings.Contains(adv, want) {
			t.Errorf("建议里缺 %q:%s", want, adv)
		}
	}
}

func TestDiagnoseZoneCapacity_SmallModulesFit(t *testing.T) {
	// 几个小模块:装得下 —— 这时哪怕 plan 有 violation,那也是**摆放**问题,
	// 绝不能建议换纸。
	sheet, ko := a4()
	mods := []partitionModule{
		{Name: "PWR", BBox: layoutBBox{MinX: 100, MinY: 300, MaxX: 400, MaxY: 500}},
		{Name: "LED", BBox: layoutBBox{MinX: 500, MinY: 300, MaxX: 700, MaxY: 450}},
	}
	cap := diagnoseZoneCapacity(sheet, ko, mods, defaultPartitionOpts())
	if !cap.Fits {
		t.Fatalf("小模块该判装得下,得到 need %.0f×%.0f have %.0f×%.0f", cap.NeedW, cap.NeedH, cap.HaveW, cap.HaveH)
	}
	if capacityAdvice(cap) != "" {
		t.Error("装得下时不该给换纸建议 —— 那会掩盖真正的摆放问题")
	}
}

func TestDiagnoseZoneCapacity_NoSheetFitsAtAll(t *testing.T) {
	// 比 A0 还大:必须说实话(拆页),而不是推荐一张装不下它的纸。
	sheet, ko := a4()
	mods := []partitionModule{{Name: "HUGE", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 6000, MaxY: 6000}}}
	cap := diagnoseZoneCapacity(sheet, ko, mods, defaultPartitionOpts())
	if cap.Fits || cap.Suggest != "" {
		t.Fatalf("超出全部标准纸时不该推荐纸张,得到 suggest=%q", cap.Suggest)
	}
	if !strings.Contains(capacityAdvice(cap), "拆开到多页") {
		t.Errorf("该建议拆页:%s", capacityAdvice(cap))
	}
}

func TestDiagnoseZoneCapacity_KeepoutCountsAgainstHeight(t *testing.T) {
	// 图签吃掉的高度必须计入,否则「刚好差一点」的页会被判成装得下,
	// 然后在 titleBlockHits 上无限循环。
	sheet, ko := a4()
	mods := []partitionModule{{Name: "M", BBox: layoutBBox{MinX: 100, MinY: 300, MaxX: 400, MaxY: 830}}}
	withKO := diagnoseZoneCapacity(sheet, ko, mods, defaultPartitionOpts())
	noKO := diagnoseZoneCapacity(sheet, nil, mods, defaultPartitionOpts())
	if withKO.HaveH >= noKO.HaveH {
		t.Errorf("有图签时可用高度该更小:%.0f vs %.0f", withKO.HaveH, noKO.HaveH)
	}
}
