package app

import (
	"strings"
	"testing"
)

// sch_move_kernel_geometry_test.go — 内核的**几何保真度**判据(2026-08-20 真机)。
//
// 缺陷形态:`zone-arrange --apply` 电气全绿,断言③却逐区红,其中两个区的**宽度**
// 凭空多了 82 / 126 —— 那不是标定误差(标定偏小会让六个区一致偏胖,而实测里
// 三个区的宽度分毫不差),是某几支 marker 落到了规划根本没预测的一侧。
//
// 内核里有两条路会产生「不受任何计划约束」的几何:
//   ① 第 4 步 rest:网表认得、计划却没覆盖到的 pin(几何读不到它的桩,preserve
//      也复现不了)→ autoconnect 自由评分挑方向,桩长只被 OffsetCap 夹住;
//   ② 恢复段 / 合并早检:一律自由评分 —— 正确性拿回来了,几何换掉了。
//
// 修法:② 先按已知几何(计划端子 ∪ 全页桩线快照)复现,复现不了的才自由评分;
// ①② 剩下的一律点名进 moveReport.FreeConnected,让断言③ 能归因。
// 「偏差可以有,但必须可见」。

// kernelFixtureWithUncoveredPin 在标准场景上给 R1 加一只 pin3:网表认得它
// (接 IO0),但画布上没有任何桩线/marker —— 几何读不出来,preserve 无从复现。
// 这正是「计划没覆盖 → 自由落点」的最小复刻。
func kernelFixtureWithUncoveredPin() *fakeMoveOps {
	f := kernelFixture()
	for i := range f.comps {
		if f.comps[i].Designator == "R1" {
			f.comps[i].Pins = append(f.comps[i].Pins, layoutPin{Number: "3", X: 100, Y: 95})
		}
	}
	f.netSeq = []map[string]map[string]bool{{
		"5V":  {"R1.1": true},
		"GND": {"R1.2": true},
		"IO0": {"R1.3": true},
	}}
	return f
}

// 自由落点必须点名带回调用方:它是「规划 pass → 落地胖一档」的唯一结构性来源。
func TestMoveKernel_FreeLandedPinsAreNamed(t *testing.T) {
	f := kernelFixtureWithUncoveredPin()
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 300, Y: 400},
	}, kernelTestOpts())
	if err != nil {
		t.Fatalf("不该报错:%v", err)
	}
	if len(rep.FreeConnected) != 1 || rep.FreeConnected[0] != "R1:3" {
		t.Fatalf("几何不受计划约束落地的 pin 必须点名(want [R1:3]),got %v", rep.FreeConnected)
	}
	// 有桩可复现的两只 pin 不许被算进自由落点(否则报文一片红,没人再看)。
	if f.calls("connectPin 5V up") != 1 || f.calls("connectPin GND down") != 1 {
		t.Fatalf("能复现的桩仍该原样重建,log=%v", f.log)
	}

	// 负对照:全部 pin 都能复现时,FreeConnected 必须为空 —— 判据不能一律喊红。
	clean := kernelFixture()
	rep2, err := schMoveKernelWith(clean, []moveItem{
		{Designator: "R1", HasTarget: true, X: 300, Y: 400},
	}, kernelTestOpts())
	if err != nil {
		t.Fatalf("不该报错:%v", err)
	}
	if len(rep2.FreeConnected) != 0 {
		t.Fatalf("桩全部复现时不该报自由落点:%v", rep2.FreeConnected)
	}
}

// 恢复段必须先按**已知几何**把断连的 pin 连回来,再把复现不了的交给自由评分。
// 旧行为:恢复段一律 autoconnect —— 「救回来了」的同时把桩换成评分器挑的那根,
// phase A 的收敛被一次火警撤销。
func TestMoveKernel_RecoveryRebuildsKnownStubGeometry(t *testing.T) {
	f := kernelFixture()
	var got []moveConnTerm
	f.connectHook = func(px, py float64, tm moveConnTerm) { got = append(got, tm) }
	full := f.netSeq[0]
	degraded := map[string]map[string]bool{"5V": {"R1.1": true}} // GND 那只脚断了
	// 读序:快照(full)→ 合并早检(full,干净)→ 对账首轮(degraded,红)→
	// 恢复 → 对账复查(full,绿)。
	f.netSeq = []map[string]map[string]bool{full, full, degraded, full}
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 300, Y: 400},
	}, kernelTestOpts())
	if err != nil {
		t.Fatalf("恢复后复查绿应算成功:%v", err)
	}
	// 第 4 步一次(preserve)+ 恢复段一次(按已知几何重建)= 两次同几何的 connect。
	if n := f.calls("connectPin GND down"); n != 2 {
		t.Fatalf("恢复段该按原桩几何重建 GND(共 2 次 connectPin GND down),got %d,log=%v", n, f.log)
	}
	for _, tm := range got {
		if tm.Net == "GND" && (tm.Direction != "down" || tm.Offset != 30) {
			t.Errorf("恢复重建必须复现原几何(down/30),got %+v", tm)
		}
	}
	if !strings.Contains(strings.Join(rep.Notes, ";"), "原样重建") {
		t.Errorf("恢复段按已知几何重建必须留痕:%v", rep.Notes)
	}
	// 复现成功的 pin 不算自由落点。
	for _, r := range rep.FreeConnected {
		if r == "R1:2" {
			t.Errorf("按原几何重建成功的 pin 不该算自由落点:%v", rep.FreeConnected)
		}
	}
}

// 负对照:**被灌进别的网**的 pin 不许走「按原几何重建」那条路 —— 它得先拆
// (replace autoconnect),直接 connect_pin 会在旧连接上再叠一支。
// 这条同时保证上一测试钉住的不是「见到 deficit 就 connect」。
func TestMoveKernel_MiswiredPinStillGoesThroughReplace(t *testing.T) {
	f := kernelFixtureWithNeighbor()
	healthy := f.netSeq[0]
	corrupt := map[string]map[string]bool{"+3V3": {"U9.1": true}} // U9.1 被灌进 +3V3
	f.netSeq = []map[string]map[string]bool{healthy, corrupt, healthy}
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err != nil {
		t.Fatalf("早检修复后应成功:%v", err)
	}
	// U9 的桩几何**是**已知的(全页快照),但它的病是「连错网」不是「断连」——
	// 只能 replace,不许直接重建。
	if n := f.calls("connectPin GND down 495,500"); n != 0 {
		t.Fatalf("灌错网的 pin 不许直接 connect_pin 重建(会叠一支),got %d 次,log=%v", n, f.log)
	}
	replaced := false
	for _, l := range f.log {
		if strings.HasPrefix(l, "autoconnect U9:1") && strings.Contains(l, "replace=true") {
			replaced = true
		}
	}
	if !replaced {
		t.Fatalf("灌错网的 pin 必须走 replace autoconnect,log=%v", f.log)
	}
	// 走了自由评分 = 几何不可预测,必须点名。
	if len(rep.FreeConnected) != 1 || rep.FreeConnected[0] != "U9:1" {
		t.Fatalf("replace 重连的 pin 该点名进自由落点,got %v", rep.FreeConnected)
	}
}

// 第三方 pin 被共线合并**吞掉**(单纯断连)时:全页桩线快照让它也能按原几何
// 重建 —— 只快照移动集合等于「知道怎么修的偏偏不修」,邻区的框跟着变形。
func TestMoveKernel_ThirdPartyDisconnectRebuiltFromPageWideSnapshot(t *testing.T) {
	f := kernelFixtureWithNeighbor()
	healthy := f.netSeq[0]
	// 早检时 GND 整网消失(桩线被合并吞了):U9.1 断连,成员 pin 此刻本就该浮空。
	// 网表不能整张读空 —— 那会撞上「netlist 引擎疑似被毒死」的空表守卫。
	swallowed := map[string]map[string]bool{"5V": {"R1.1": true}}
	f.netSeq = []map[string]map[string]bool{healthy, swallowed, healthy}
	var got []moveConnTerm
	f.connectHook = func(px, py float64, tm moveConnTerm) { got = append(got, tm) }
	if _, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts()); err != nil {
		t.Fatalf("早检修复后应成功:%v", err)
	}
	rebuilt := false
	for _, tm := range got {
		if tm.Pin == "1" && tm.Net == "GND" && tm.Direction == "down" && tm.Offset == 30 {
			// U9.1 与 R1.2 都是 pin GND/down,靠坐标区分:U9 的 pin 在 (495,500)。
			rebuilt = true
		}
	}
	if !rebuilt || f.calls("connectPin GND down 495,500") != 1 {
		t.Fatalf("第三方断连 pin 该按全页快照里的原桩几何重建(GND/down/30 @495,500),log=%v", f.log)
	}
	// 全页快照本身:成员与非成员都在。
	snap := moveKernelStubSnapshot(nil, f.comps, f.wires)
	for _, ref := range []string{"R1:1", "R1:2", "U9:1"} {
		if _, ok := snap[ref]; !ok {
			t.Errorf("全页快照该含 %s,got %v", ref, snap)
		}
	}
	// 负对照:只快照移动集合时,第三方 pin 的几何就是不可知的。
	only := moveKernelStubSnapshot(map[string]bool{"R1": true}, f.comps, f.wires)
	if _, ok := only["U9:1"]; ok {
		t.Error("memberSet 过滤该把第三方 pin 挡在外面(负对照失效)")
	}
}
