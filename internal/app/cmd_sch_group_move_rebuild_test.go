package app

import (
	"encoding/json"
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

	// 组**当前就已越界**是常态(marker 探出图纸上沿)。此时旧实现会把「往下挪 30」
	// 算成「往上挪 40」—— 方向反了,比不动还糟(2026-08-15 esp32Mini E2E 实测)。
	t.Run("已越界时收拢不许反号", func(t *testing.T) {
		over := layoutBBox{MinX: 100, MinY: 150, MaxX: 400, MaxY: 850} // MaxY 已超 bounds
		_, dy := clampDeltaToBounds(over, 0, -30, bounds)
		if dy > 0 {
			t.Errorf("请求往下(-30),收拢结果不许为正: %v", dy)
		}
		if dy < -30 {
			t.Errorf("收拢只许减小位移量: %v", dy)
		}
	})
}

// 图签只占右下角一个矩形,不是整条底边 —— 组落在它左边时,页面下部照常可用。
// 把下界整条抬到图签上沿会凭空少一条地,而 MCU 这类高组正需要它。
func TestClampDeltaAvoidingKeepout(t *testing.T) {
	bounds := layoutBBox{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 800}
	keepout := &layoutBBox{MinX: 500, MinY: 0, MaxX: 1000, MaxY: 200}

	t.Run("图签左侧可以下探", func(t *testing.T) {
		box := layoutBBox{MinX: 20, MinY: 250, MaxX: 400, MaxY: 700}
		_, dy := clampDeltaAvoidingKeepout(box, 0, -200, bounds, keepout)
		if dy != -200 {
			t.Errorf("组在图签左边,下探不该被拦: %v", dy)
		}
	})
	t.Run("压到图签上收回其上沿", func(t *testing.T) {
		box := layoutBBox{MinX: 600, MinY: 250, MaxX: 900, MaxY: 700}
		_, dy := clampDeltaAvoidingKeepout(box, 0, -200, bounds, keepout)
		if box.MinY+dy < keepout.MaxY {
			t.Errorf("组与图签同列,不该落进 keepout: MinY=%v < %v", box.MinY+dy, keepout.MaxY)
		}
		if dy > 0 {
			t.Errorf("仍不许反号: %v", dy)
		}
	})
	t.Run("本来就压着图签时允许往好的方向挪", func(t *testing.T) {
		// marker 伸进图签区是常态。要求"一步到位挪干净"做不到,于是每次 y 移动
		// 都被收成 0 —— 连"挪一点点变好"都做不了。判据是**别变糟**,不是**必须干净**。
		box := layoutBBox{MinX: 600, MinY: -22, MaxX: 900, MaxY: 660}
		_, dy := clampDeltaAvoidingKeepout(box, 0, 40, bounds, keepout)
		if dy != 40 {
			t.Errorf("上移减少了对图签的侵入,不该被拦: %v", dy)
		}
	})
	t.Run("没有图签几何时退化成纯边界收拢", func(t *testing.T) {
		box := layoutBBox{MinX: 600, MinY: 250, MaxX: 900, MaxY: 700}
		_, dy := clampDeltaAvoidingKeepout(box, 0, -200, bounds, nil)
		if dy != -200 {
			t.Errorf("无 keepout 时应原样通过: %v", dy)
		}
	})
}

// ── 钳位可见性(#151:esp32Mini round2 新 4)──────────────────────────────────
//
// 真机实录:--dy -110 撞图纸下沿被钳成 -2,命令仍打「✓ 平移 0 件 Δ=(0,-2)」退出 0。
// 判据:钳位必须结构化可见(requested vs applied),钳到接近 0(任一被请求轴
// |applied| < |requested|·10% 且 |applied| ≤ 5)= 位移意图丢失 → 拒绝执行。
func TestEvalGroupMoveClamp(t *testing.T) {
	cases := []struct {
		name             string
		reqDX, reqDY     float64
		appDX, appDY     float64
		clamped, refused bool
		axisMustContain  string // 任一 Axes 行须含此子串("" = 不检查)
	}{
		{"足额位移零钳位", 100, -80, 100, -80, false, false, ""},
		{"零请求零钳位", 0, 0, 0, 0, false, false, ""},
		{"部分钳位仍执行:钳掉一半", 0, -80, 0, -40, true, false, "图纸下沿"},
		{"真机案例:-110 钳成 -2 拒绝", 0, -110, 0, -2, true, true, "图纸下沿"},
		{"钳成全 0 拒绝", 40, 60, 0, 0, true, true, ""},
		{"边界:恰 10% 不算接近 0", 0, -50, 0, -5, true, false, ""}, // 5 == 50*0.1,不满足 <
		{"边界:|applied|=5 且 <10% 拒绝", 0, -110, 0, -5, true, true, ""},
		{"边界:<10% 但 |applied|>5 仍执行", 0, -110, 0, -10, true, false, ""},
		{"小位移大比例钳位不触发绝对档", -8, 0, -2, 0, true, false, "图纸左沿"},
		{"正向 x 撞右沿钳到近 0 拒绝", 300, 0, 4, 0, true, true, "图纸右沿"},
		{"正向 x 大幅钳位但仍 >5 执行", 300, 0, 20, 0, true, false, "图纸右沿"},
		{"正向 y 撞上沿", 0, 200, 0, 190, true, false, "图纸上沿"},
		{"一轴足额一轴钳到 0 整体拒绝", 100, -110, 100, 0, true, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := evalGroupMoveClamp(tc.reqDX, tc.reqDY, tc.appDX, tc.appDY)
			if rep.Clamped != tc.clamped || rep.Refused != tc.refused {
				t.Fatalf("clamped=%v refused=%v, want %v/%v (axes: %v)",
					rep.Clamped, rep.Refused, tc.clamped, tc.refused, rep.Axes)
			}
			if rep.RequestedDX != tc.reqDX || rep.RequestedDY != tc.reqDY ||
				rep.AppliedDX != tc.appDX || rep.AppliedDY != tc.appDY {
				t.Fatalf("requested/applied 字段没有原样带出: %+v", rep)
			}
			if tc.clamped && len(rep.Axes) == 0 {
				t.Fatal("钳位发生却没有逐轴描述")
			}
			if tc.axisMustContain != "" {
				found := false
				for _, a := range rep.Axes {
					if strings.Contains(a, tc.axisMustContain) {
						found = true
					}
				}
				if !found {
					t.Errorf("Axes %v 缺撞边归因 %q", rep.Axes, tc.axisMustContain)
				}
			}
		})
	}
}

// 收尾输出:足额位移与历史输出逐字节一致(别惊扰现有调用方);钳位时
// requested/applied 两个都印且给机器可读 partial 行;0 件被移动不许打绿勾。
func TestGroupMoveResultLines(t *testing.T) {
	t.Run("足额位移输出与历史一致", func(t *testing.T) {
		lines := groupMoveResultLines("g1", 5, groupMoveClampReport{
			RequestedDX: 100, RequestedDY: -80, AppliedDX: 100, AppliedDY: -80})
		if len(lines) != 1 {
			t.Fatalf("足额位移只该有一行: %v", lines)
		}
		want := "✓ 组 g1 平移 5 件 Δ=(100,-80);内核对账绿(网表逐引脚一致,无新增 bridge)"
		if lines[0] != want {
			t.Errorf("历史输出被改动:\n got %q\nwant %q", lines[0], want)
		}
	})
	t.Run("钳位时两个 Δ 都印且有 partial JSON 行", func(t *testing.T) {
		lines := groupMoveResultLines("g1", 5, groupMoveClampReport{
			RequestedDX: 0, RequestedDY: -110, AppliedDX: 0, AppliedDY: -40, Clamped: true})
		if len(lines) != 2 {
			t.Fatalf("钳位时该有 partial 行 + 绿勾行: %v", lines)
		}
		var payload struct {
			Requested struct{ Dx, Dy float64 } `json:"requestedDelta"`
			Applied   struct{ Dx, Dy float64 } `json:"appliedDelta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], "partial: ")), &payload); err != nil {
			t.Fatalf("partial 行不是合法 JSON: %q (%v)", lines[0], err)
		}
		if payload.Requested.Dy != -110 || payload.Applied.Dy != -40 {
			t.Errorf("requestedDelta/appliedDelta 数值不对: %+v", payload)
		}
		if !strings.Contains(lines[1], "requestedΔ=(0,-110)") || !strings.Contains(lines[1], "appliedΔ=(0,-40)") {
			t.Errorf("文本行没同时印两个 Δ: %q", lines[1])
		}
	})
	t.Run("0 件被移动是明确 no-op 不是绿勾", func(t *testing.T) {
		lines := groupMoveResultLines("g1", 0, groupMoveClampReport{
			RequestedDX: 0, RequestedDY: -4, AppliedDX: 0, AppliedDY: -2, Clamped: true})
		joined := strings.Join(lines, "\n")
		if strings.Contains(joined, "✓") {
			t.Errorf("0 件被移动不该打绿勾: %q", joined)
		}
		if !strings.Contains(joined, "no-op") || !strings.Contains(joined, "0 件") {
			t.Errorf("缺明确 no-op 提示: %q", joined)
		}
		if !strings.Contains(joined, `"requestedDelta"`) {
			t.Errorf("钳位过的 no-op 也要带 partial 结构化行: %q", joined)
		}
	})
}
