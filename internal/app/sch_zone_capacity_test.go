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
	// 真机实测(2026-08-16 ceshi/P2_MCU):ESP32-S3-WROOM-1 的虚拟组含自己的 marker
	// 后约 607×501,加 ±24 pad + 30 区名带 + 26 说明带 → 框要 655×605。
	// 图签上方可用高只有 569,左侧净宽只有 410 —— 两条路都走不通,是真装不下。
	//
	// 早先这个 fixture 用的是 452×462(need 500×566),在**修正后的**口径下它其实
	// 刚好装得进图签上方(566 ≤ 569)—— 旧口径多扣了一次页边距(28)才把它算成装不下。
	// 换成真机数值,这个用例才真的在测「装不下」。
	sheet, ko := a4()
	mods := []partitionModule{{Name: "U2", BBox: layoutBBox{MinX: 275, MinY: 175, MaxX: 882, MaxY: 676}}}
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
	for _, want := range []string{"当前摆法", "U2", "A3", "先试重排", "调 margin/gutter 无解"} {
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
	if !strings.Contains(capacityAdvice(cap), "拆到多页") {
		t.Errorf("该建议拆页:%s", capacityAdvice(cap))
	}
}

func TestDiagnoseZoneCapacity_KeepoutIsACornerNotABand(t *testing.T) {
	// 图签是**角落障碍**,不是整条底带。首版无脑扣掉整条高度,把「窄而高、能塞在
	// 图签左边」的模块误判成装不下 —— 那会让人白换一张大纸,而真正的毛病
	// (摆得不对)原封不动。
	sheet, ko := a4()

	// ① 窄长条:宽 300 < 图签左侧净宽(468-28=440),高度可以用满整幅 → 装得下。
	tall := []partitionModule{{Name: "TALL", BBox: layoutBBox{MinX: 60, MinY: 60, MaxX: 360, MaxY: 700}}}
	if c := diagnoseZoneCapacity(sheet, ko, tall, defaultPartitionOpts()); !c.Fits {
		t.Errorf("窄而高、可塞在图签左侧的模块该判装得下(need %.0f×%.0f)", c.NeedW, c.NeedH)
	}

	// ② 宽而矮:跨过图签横向,高度必须让开图签 → 仍装得下。
	wide := []partitionModule{{Name: "WIDE", BBox: layoutBBox{MinX: 60, MinY: 400, MaxX: 1000, MaxY: 700}}}
	if c := diagnoseZoneCapacity(sheet, ko, wide, defaultPartitionOpts()); !c.Fits {
		t.Errorf("宽而矮、可落在图签上方的模块该判装得下(need %.0f×%.0f)", c.NeedW, c.NeedH)
	}

	// ③ 又宽又高:左侧塞不进、上方也放不下 → 这才是真装不下。
	both := []partitionModule{{Name: "BOTH", BBox: layoutBBox{MinX: 60, MinY: 60, MaxX: 1000, MaxY: 700}}}
	c := diagnoseZoneCapacity(sheet, ko, both, defaultPartitionOpts())
	if c.Fits {
		t.Errorf("又宽又高该判装不下(need %.0f×%.0f, have %.0f×%.0f)", c.NeedW, c.NeedH, c.HaveW, c.HaveH)
	}
	if c.Blocking != "BOTH" {
		t.Errorf("该点名 BOTH,得到 %q", c.Blocking)
	}
}

func TestFitsAroundCorner_LShape(t *testing.T) {
	usable := layoutBBox{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 800}
	ko := &layoutBBox{MinX: 600, MinY: 0, MaxX: 1000, MaxY: 200} // 右下角
	cases := []struct {
		name string
		w, h float64
		want bool
	}{
		{"窄高:塞进障碍左侧(宽600 高800)", 600, 800, true},
		{"宽矮:落在障碍上方(高600)", 1000, 600, true},
		{"又宽又高", 1000, 800, false},
		{"稍微超过左侧宽,但高度让得开", 700, 600, true},
		{"超过左侧宽,高度也让不开", 700, 700, false},
	}
	for _, c := range cases {
		if got := fitsAroundCorner(c.w, c.h, usable, ko); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	// 无障碍时退化成整幅比较。
	if !fitsAroundCorner(1000, 800, usable, nil) {
		t.Error("无障碍时整幅该装得下")
	}
	if fitsAroundCorner(1001, 800, usable, nil) {
		t.Error("超宽该装不下")
	}
}
