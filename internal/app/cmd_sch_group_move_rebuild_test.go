package app

import (
	"math"
	"strings"
	"testing"
)

// ── 组平移的刚体判据(ADR-0003)────────────────────────────────────────────

// 刚体平移的**定义**就是网表逐引脚不变 —— 这条比对是 group-move 存在的理由:
// 旧实现(带线一起搬,delete+recreate)静默丢过 3 个 GND 引脚而无人察觉。
func TestGroupRebuildNetDiff_CatchesEveryKindOfDamage(t *testing.T) {
	before := map[string][]string{
		"GND":   {"C7.2", "J1.8", "J1.9", "U3.1"},
		"C7_N6": {"J1.A5", "R4.1"},
		"5V":    {"J1.A4", "U3.5"},
	}
	t.Run("完全一致时无差异", func(t *testing.T) {
		same := map[string][]string{
			"GND": {"C7.2", "J1.8", "J1.9", "U3.1"}, "C7_N6": {"J1.A5", "R4.1"}, "5V": {"J1.A4", "U3.5"},
		}
		if d := groupRebuildNetDiff(before, same); len(d) != 0 {
			t.Errorf("一致的快照不该报差异: %v", d)
		}
	})
	t.Run("丢引脚", func(t *testing.T) { // 旧 group-move 的真实故障
		after := map[string][]string{
			"GND": {"J1.8", "J1.9", "U3.1"}, "C7_N6": {"J1.A5", "R4.1"}, "5V": {"J1.A4", "U3.5"},
		}
		d := groupRebuildNetDiff(before, after)
		if len(d) != 1 || !strings.Contains(d[0], "C7.2") {
			t.Errorf("必须点名丢失的引脚: %v", d)
		}
	})
	t.Run("整条网消失并入别的网(串网)", func(t *testing.T) {
		after := map[string][]string{
			"GND": {"C7.2", "J1.8", "J1.9", "J1.A5", "R4.1", "U3.1"}, "5V": {"J1.A4", "U3.5"},
		}
		d := groupRebuildNetDiff(before, after)
		if len(d) != 2 {
			t.Fatalf("消失 + 并入应报两条: %v", d)
		}
		joined := strings.Join(d, " | ")
		if !strings.Contains(joined, "C7_N6") || !strings.Contains(joined, "新增") {
			t.Errorf("必须同时点出哪条网没了、并到了哪里: %v", d)
		}
	})
}

// 重连顺序**决定落点质量**:评分器的 scene 随放随长,每落一个 marker 就注册回去
// 当障碍。电源/地数量最多、方向最固定(电上地下),必须先落满,信号才能绕开。
// 实测:按引脚字母序打散 → markerOverlaps 3→13;把 GND 排到最后 → 当场串网。
func TestGroupRebuildConnSpecs_OrdersPowerThenGndThenSignal(t *testing.T) {
	comps := []layoutComp{{
		ID: "p1", Designator: "R4", ComponentType: "part",
		Pins: []layoutPin{{Number: "1"}, {Number: "2"}},
	}, {
		ID: "p2", Designator: "U3", ComponentType: "part",
		Pins: []layoutPin{{Number: "5"}, {Number: "1"}},
	}}
	live := map[string]map[string]bool{
		"C7_N6": {"R4.1": true},
		"GND":   {"R4.2": true, "U3.1": true},
		"5V":    {"U3.5": true},
	}
	conns, movable := groupRebuildConnSpecs(comps, map[string]bool{"R4": true, "U3": true}, live)
	if len(movable) != 2 {
		t.Fatalf("两个成员都该可移动: %+v", movable)
	}
	if len(conns) != 4 {
		t.Fatalf("四个已连引脚都该重连: %+v", conns)
	}
	if conns[0].Net != "5V" {
		t.Errorf("电源必须最先落: got %q", conns[0].Net)
	}
	if conns[1].Net != "GND" || conns[2].Net != "GND" {
		t.Errorf("地紧随其后(成片落满): got %q,%q", conns[1].Net, conns[2].Net)
	}
	if conns[3].Net != "C7_N6" {
		t.Errorf("信号最后: got %q", conns[3].Net)
	}
}

// 浮空引脚不出现在网表里 → 不产生重连规格。重连后不该凭空多一根桩线。
func TestGroupRebuildConnSpecs_SkipsFloatingPins(t *testing.T) {
	comps := []layoutComp{{
		ID: "p1", Designator: "U3", ComponentType: "part",
		Pins: []layoutPin{{Number: "1"}, {Number: "2"}, {Number: "3"}},
	}}
	live := map[string]map[string]bool{"GND": {"U3.1": true}}
	conns, _ := groupRebuildConnSpecs(comps, map[string]bool{"U3": true}, live)
	if len(conns) != 1 || conns[0].PinRef != "U3:1" {
		t.Errorf("只有已连的引脚该重连: %+v", conns)
	}
}

// 非成员器件既不移动也不重连 —— 组平移不得触碰组外任何东西。
func TestGroupRebuildConnSpecs_IgnoresNonMembers(t *testing.T) {
	comps := []layoutComp{
		{ID: "p1", Designator: "U3", ComponentType: "part", Pins: []layoutPin{{Number: "1"}}},
		{ID: "p2", Designator: "Q9", ComponentType: "part", Pins: []layoutPin{{Number: "1"}}},
	}
	live := map[string]map[string]bool{"GND": {"U3.1": true, "Q9.1": true}}
	conns, movable := groupRebuildConnSpecs(comps, map[string]bool{"U3": true}, live)
	if len(movable) != 1 || movable[0].Designator != "U3" {
		t.Errorf("只该移动成员: %+v", movable)
	}
	for _, c := range conns {
		if strings.HasPrefix(c.PinRef, "Q9") {
			t.Errorf("不该重连组外器件: %+v", conns)
		}
	}
}

// **每一层都要自己保证不出界**(ADR-0003 §6)。group-arrange 有边界排布器,而手工
// group-move 过去完全不查 —— 实测 Δ=(40,60) 就把组推出图纸,layout-lint 报
// 5 out-of-sheet 而命令一声不吭。
func TestClampDeltaToBounds(t *testing.T) {
	bounds := layoutBBox{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 800}
	box := layoutBBox{MinX: 100, MinY: 100, MaxX: 400, MaxY: 300}

	t.Run("界内不动", func(t *testing.T) {
		dx, dy := clampDeltaToBounds(box, 50, 50, bounds)
		if dx != 50 || dy != 50 {
			t.Errorf("不越界就不该改: (%v,%v)", dx, dy)
		}
	})
	t.Run("右越界收回", func(t *testing.T) {
		dx, _ := clampDeltaToBounds(box, 900, 0, bounds)
		if box.MaxX+dx > bounds.MaxX {
			t.Errorf("收拢后仍越界: MaxX=%v > %v", box.MaxX+dx, bounds.MaxX)
		}
	})
	t.Run("上越界收回", func(t *testing.T) {
		_, dy := clampDeltaToBounds(box, 0, 900, bounds)
		if box.MaxY+dy > bounds.MaxY {
			t.Errorf("收拢后仍越界: MaxY=%v > %v", box.MaxY+dy, bounds.MaxY)
		}
	})
	t.Run("负向越界收回", func(t *testing.T) {
		dx, dy := clampDeltaToBounds(box, -900, -900, bounds)
		if box.MinX+dx < bounds.MinX || box.MinY+dy < bounds.MinY {
			t.Errorf("收拢后仍越界: (%v,%v)", box.MinX+dx, box.MinY+dy)
		}
	})
	t.Run("结果吸附在网格上", func(t *testing.T) {
		dx, dy := clampDeltaToBounds(box, 903, 707, bounds)
		if math.Mod(dx, float64(schAnchorGrid)) != 0 || math.Mod(dy, float64(schAnchorGrid)) != 0 {
			t.Errorf("位移必须在 %d 格上: (%v,%v)", schAnchorGrid, dx, dy)
		}
	})
}
