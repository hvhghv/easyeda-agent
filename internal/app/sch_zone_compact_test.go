package app

import (
	"strings"
	"testing"
)

// 收敛判据的风险是**乱动**:每翻一个区都要删桩重连,而删桩会触发导线自动合并
// (这一轮真机把两张网桥在一起过)。所以「什么时候不该动」比「怎么排」更重要。

func TestShouldCompactZone_OnlyWideFlatOnes(t *testing.T) {
	cases := []struct {
		name string
		w, h float64
		want bool
		hint string
	}{
		// 真机实测的四个区形状。
		{"Q 区 279×213(比 1.3)", 279, 213, false, "已经够方"},
		{"U 区 367×235(比 1.6)", 367, 235, false, "已经够方"},
		{"D_ESD 96×161(比 1.7 高瘦)", 96, 161, false, "已经够方"},
		{"J_USB 478×166(比 2.9 宽扁)", 478, 166, true, "宽扁条"},
		// 高瘦到超门槛也不动:竖排只会更高,A4 横放高度更金贵。
		{"高瘦 100×400(比 4.0)", 100, 400, false, "高瘦"},
		{"退化尺寸", 0, 0, false, "已经够方"},
	}
	for _, c := range cases {
		got, why := shouldCompactZone(c.w, c.h)
		if got != c.want {
			t.Errorf("%s: got %v want %v (%s)", c.name, got, c.want, why)
		}
		if !strings.Contains(why, c.hint) {
			t.Errorf("%s: 理由里缺 %q — %s", c.name, c.hint, why)
		}
	}
}

func TestPlanSignalColumn_StacksVerticallyAndPointsLabelsOutward(t *testing.T) {
	anchor := tidyAnchor{X: 800, Y: 400, IsIC: true, HalfWidth: 40}
	members := []tidySignalMemberIn{
		{Designator: "R4", CenterX: 560, Pins: []tidySignalPinIn{{Pin: "1", IsPort: true, Net: "U3_N6", X: 550}}},
		{Designator: "R3", CenterX: 430, Pins: []tidySignalPinIn{{Pin: "1", IsPort: true, Net: "U3_N7", X: 420}}},
	}
	plans, err := planSignalColumn(members, anchor, 50, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("want 2 plans, got %d", len(plans))
	}
	// 位号自然序:R3 在前。
	if plans[0].Designator != "R3" || plans[1].Designator != "R4" {
		t.Errorf("该按位号自然序:%s,%s", plans[0].Designator, plans[1].Designator)
	}
	// 同一列(X 相同)、竖向拉开(Y 不同)——这正是「宽度不再累加」的来源。
	if plans[0].X != plans[1].X {
		t.Errorf("竖排该同列:X=%v vs %v", plans[0].X, plans[1].X)
	}
	if plans[0].Y == plans[1].Y {
		t.Error("竖排该拉开 Y")
	}
	// 排在锚件左侧。
	if plans[0].X >= anchor.X {
		t.Errorf("side=-1 该排在锚件左侧:X=%v anchor.X=%v", plans[0].X, anchor.X)
	}
	// **必须带 HasPose** —— 否则执行侧只改标签方向、件根本不动,收敛无从谈起。
	for _, p := range plans {
		if !p.HasPose {
			t.Errorf("%s 缺 HasPose,件不会被移动", p.Designator)
		}
		// 标签一律朝远离锚件的一侧(左),不能按老规则各判各的。
		for _, pin := range p.Pins {
			if pin.Direction != "left" {
				t.Errorf("%s 的标签朝 %s,该朝远离锚件的 left —— 否则标签插回区内把区撑开",
					p.Designator, pin.Direction)
			}
		}
	}
}

func TestPlanSignalColumn_SideRight(t *testing.T) {
	anchor := tidyAnchor{X: 300, Y: 400, IsIC: true, HalfWidth: 40}
	plans, err := planSignalColumn([]tidySignalMemberIn{
		{Designator: "R1", Pins: []tidySignalPinIn{{Pin: "1", IsPort: true, Net: "N"}}},
	}, anchor, 50, +1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].X <= anchor.X {
		t.Fatalf("side=+1 该排在右侧:%+v", plans)
	}
	if plans[0].Pins[0].Direction != "right" {
		t.Errorf("右侧列的标签该朝 right,得到 %s", plans[0].Pins[0].Direction)
	}
}

func TestPlanSignalColumn_SkipsMembersWithoutPorts(t *testing.T) {
	// 没有 netport 的件不属于 signal-row,不该被这个规划器搬走 ——
	// 它的连接形态(电源旗/普通导线)由别的 pattern 负责,乱搬会扯断。
	plans, err := planSignalColumn([]tidySignalMemberIn{
		{Designator: "C1", Pins: []tidySignalPinIn{{Pin: "1", IsPort: false, Net: "GND"}}},
	}, tidyAnchor{X: 100, Y: 100}, 50, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 {
		t.Errorf("无 netport 的件不该出现在计划里:%+v", plans)
	}
}

func TestEstimateColumnBBox_WidthStopsAccumulating(t *testing.T) {
	// 竖排的全部意义:宽度只算「锚件 + 一件」,不随件数增长。
	w2, h2 := estimateColumnBBox(80, 90, 20, 50, 2)
	w5, h5 := estimateColumnBBox(80, 90, 20, 50, 5)
	if w2 != w5 {
		t.Errorf("宽度不该随件数变:%v(2件) vs %v(5件)", w2, w5)
	}
	if h5 <= h2 {
		t.Errorf("高度该随件数增长:%v(2件) vs %v(5件)", h2, h5)
	}
	// 空列退化成锚件自身。
	if w, h := estimateColumnBBox(80, 90, 20, 50, 0); w != 80 || h != 90 {
		t.Errorf("空列该给锚件尺寸,得到 %v×%v", w, h)
	}
}
