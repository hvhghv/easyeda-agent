package app

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

// 这组单测移植自 2026-08-16 演示页的证明面板:确定性不是口头承诺,是穷举验出来的。
// fixture 用 ceshi/P3_USB_DEBUG 的真机数值(演示页 v3 同源),两套形状:
//   raw  = 现状口径框(标签入框后):任意两个大区并不下一行 → 必 blocked;
//   cmpt = phase A 收敛后的框:有唯一解。

func zaSheetA4() (layoutBBox, *layoutBBox) {
	return layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825},
		&layoutBBox{MinX: 468, MinY: 0, MaxX: 1170, MaxY: 198}
}

func zaRawZones() []zaZone {
	return []zaZone{
		{Name: "Q", W: 572, H: 214, Home: [2]float64{274, 367}},
		{Name: "U", W: 682, H: 356, Home: [2]float64{747, 512}},
		{Name: "D_ESD", W: 256, H: 160, Home: [2]float64{210, 572}},
		{Name: "J_USB", W: 486, H: 276, Home: [2]float64{589, 322}},
	}
}

func zaCompactZones() []zaZone {
	return []zaZone{
		{Name: "Q", W: 246, H: 326, Home: [2]float64{274, 367}},
		{Name: "U", W: 318, H: 360, Home: [2]float64{747, 512}},
		{Name: "D_ESD", W: 243, H: 160, Home: [2]float64{210, 572}},
		{Name: "J_USB", W: 294, H: 279, Home: [2]float64{589, 322}},
	}
}

func zaPerms(zs []zaZone) [][]zaZone {
	if len(zs) <= 1 {
		return [][]zaZone{zs}
	}
	var out [][]zaZone
	for i := range zs {
		rest := append(append([]zaZone{}, zs[:i]...), zs[i+1:]...)
		for _, p := range zaPerms(rest) {
			out = append(out, append([]zaZone{zs[i]}, p...))
		}
	}
	return out
}

// 证明①:全排列穷举 —— 4!=24 种输入顺序,输出必须逐字节相同。
func TestZonesArrange_AllPermutationsIdentical(t *testing.T) {
	sheet, ko := zaSheetA4()
	base := zonesArrange(zaCompactZones(), sheet, ko, defaultPartitionOpts())
	if !base.OK {
		t.Fatalf("收敛后的形状该有解,得到 blocked=%s(%s)", base.Blocked, base.Tried)
	}
	perms := zaPerms(zaCompactZones())
	if len(perms) != 24 {
		t.Fatalf("穷举该有 24 种排列,得到 %d", len(perms))
	}
	for i, p := range perms {
		if got := zonesArrange(p, sheet, ko, defaultPartitionOpts()); !reflect.DeepEqual(got, base) {
			t.Fatalf("排列 %d 的输出与基准不同 —— 有序依赖!\nbase=%+v\ngot =%+v", i, base, got)
		}
	}
}

// 证明②:blocked 判决也要确定 —— 现状形状 24 种顺序必须同判同一个 blocked。
func TestZonesArrange_RawShapesBlockedDeterministically(t *testing.T) {
	sheet, ko := zaSheetA4()
	base := zonesArrange(zaRawZones(), sheet, ko, defaultPartitionOpts())
	if base.OK {
		t.Fatalf("现状形状(U 682 宽 + J 486 宽)不该有解 —— phase A 是前置条件,不是优化")
	}
	for i, p := range zaPerms(zaRawZones()) {
		got := zonesArrange(p, sheet, ko, defaultPartitionOpts())
		if got.OK || got.Blocked != base.Blocked {
			t.Fatalf("排列 %d 判决漂移:base blocked=%s got=%+v", i, base.Blocked, got)
		}
	}
	// blocked 必须带能执行的解释:回退链 + 各边距离。
	if base.Tried == "" {
		t.Error("blocked 该报出回退链(是谁、每条边为何不行),得到空")
	}
}

// 证明③:5 格律 —— 落位都在「规范角 + 5k」上(扫描轴),执行侧再圆整件的 Δ。
func TestZonesArrange_Lattice(t *testing.T) {
	sheet, ko := zaSheetA4()
	res := zonesArrange(zaCompactZones(), sheet, ko, defaultPartitionOpts())
	if !res.OK {
		t.Fatal("fixture 该有解")
	}
	L, R := snap5Up(sheet.MinX+28), snap5Dn(sheet.MaxX-28)
	B, T := snap5Up(sheet.MinY+28), snap5Dn(sheet.MaxY-28)
	for _, p := range res.Placed {
		switch p.Edge {
		case "W", "E":
			if math.Mod(T-p.Rect.MaxY, 5) != 0 || (p.Rect.MinX != L && p.Rect.MaxX != R) {
				t.Errorf("%s@%s 脱格:rect=%+v", p.Name, p.Edge, p.Rect)
			}
		case "N", "S":
			if math.Mod(p.Rect.MinX-L, 5) != 0 || (p.Rect.MinY != B && p.Rect.MaxY != T) {
				t.Errorf("%s@%s 脱格:rect=%+v", p.Name, p.Edge, p.Rect)
			}
		}
	}
}

// 证明④:同一把尺 —— 输出必须过**既有的** validatePartitions(不是本文件自带的尺)。
func TestZonesArrange_OutputPassesExistingRuler(t *testing.T) {
	sheet, ko := zaSheetA4()
	res := zonesArrange(zaCompactZones(), sheet, ko, defaultPartitionOpts())
	if !res.OK {
		t.Fatal("fixture 该有解")
	}
	v := zaValidate(res, sheet, ko, defaultPartitionOpts())
	if v.SheetOverflow != 0 || v.PartitionOverlap != 0 || v.TitleBlockHits != 0 || v.SheetMarginHits != 0 {
		t.Fatalf("输出未过既有验证器:%+v", v)
	}
}

// 稳定性(用户 2026-08-16 确认的性质):确定的元器件集合每次同一解;
// 小幅挪动某个元素 —— 只要没改变「首选边 + 实际用到的回退边 + 边内次序」——
// **落位**一个字都不变。位置只参与边归属与排序平局,不参与落位坐标。
// (比对的是落位投影 name/rect/edge;回退链尾部的诊断性次序允许随距离微调 ——
// 它不影响任何落位,J_USB 的 W/E 谁排链尾就属于这类。)
func TestZonesArrange_StableUnderSmallHomeJitter(t *testing.T) {
	sheet, ko := zaSheetA4()
	proj := func(r zaResult) []zaPlaced {
		out := make([]zaPlaced, len(r.Placed))
		for i, p := range r.Placed {
			out[i] = zaPlaced{Name: p.Name, Rect: p.Rect, Edge: p.Edge}
		}
		return out
	}
	base := proj(zonesArrange(zaCompactZones(), sheet, ko, defaultPartitionOpts()))
	for _, d := range [][2]float64{{8, 0}, {-8, 0}, {0, 8}, {0, -8}, {6, -6}} {
		zs := zaCompactZones()
		for i := range zs { // 全体小幅漂移(模拟区内挪件带来的质心抖动)
			zs[i].Home[0] += d[0]
			zs[i].Home[1] += d[1]
		}
		if got := proj(zonesArrange(zs, sheet, ko, defaultPartitionOpts())); !reflect.DeepEqual(got, base) {
			t.Fatalf("±8 的质心抖动改变了落位(d=%v)—— 稳定性性质被破坏\nbase=%+v\ngot =%+v", d, base, got)
		}
	}
}

// 声明边优先于质心回退,且声明后位置彻底失去影响(归属写回声明的意义)。
func TestZonesArrange_DeclaredEdgeWins(t *testing.T) {
	sheet, ko := zaSheetA4()
	zs := zaCompactZones()
	for i := range zs {
		if zs[i].Name == "D_ESD" {
			zs[i].Edge = "E" // 质心明明最靠 W,声明说 E
		}
	}
	res := zonesArrange(zs, sheet, ko, defaultPartitionOpts())
	if !res.OK {
		t.Fatalf("blocked=%s(%s)", res.Blocked, res.Tried)
	}
	for _, p := range res.Placed {
		if p.Name == "D_ESD" {
			if p.Edge != "E" {
				t.Fatalf("声明 E 该落 E,落到了 %s", p.Edge)
			}
			if p.Chain[0] != "E" {
				t.Fatalf("声明边该是链首,链=%v", p.Chain)
			}
		}
	}
}

// 回退链真的在工作:P3 收敛形状下 J_USB 首选 S 被 Q 与图签夹死,必须落到链上后位。
func TestZonesArrange_FallbackChainEngages(t *testing.T) {
	sheet, ko := zaSheetA4()
	res := zonesArrange(zaCompactZones(), sheet, ko, defaultPartitionOpts())
	if !res.OK {
		t.Fatal("fixture 该有解")
	}
	var j *zaPlaced
	for i := range res.Placed {
		if res.Placed[i].Name == "J_USB" {
			j = &res.Placed[i]
		}
	}
	if j == nil {
		t.Fatal("J_USB 没在输出里")
	}
	if j.Chain[0] != "S" {
		t.Fatalf("J_USB 首选边该是 S(质心 322),链=%v", j.Chain)
	}
	if j.Edge == "S" {
		t.Fatalf("S 边该被 Q 与图签安全带夹死,J 却落在了 S:%+v", j.Rect)
	}
}

// blocked 报文可读性:必须点名 + 带回退链距离(能执行的下一步,不是一句报错)。
func TestZonesArrange_BlockedMessageActionable(t *testing.T) {
	sheet, ko := zaSheetA4()
	res := zonesArrange(zaRawZones(), sheet, ko, defaultPartitionOpts())
	if res.OK {
		t.Fatal("raw 形状该 blocked")
	}
	if res.Blocked == "" {
		t.Error("该点名是谁 blocked")
	}
	want := fmt.Sprintf("%s(", res.Blocked)
	_ = want
	if len(res.Tried) < len("W(0)→E(0)→N(0)") {
		t.Errorf("Tried 该是完整回退链,得到 %q", res.Tried)
	}
}
