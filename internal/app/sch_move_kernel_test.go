package app

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ── 内核测试夹具:一件双旗电阻 R1(5V/GND 各一桩一旗)+ 远处无关件 U9 ─────────
//
// 三个平台病(ADR-0004 Consequences 点名)各一组失败注入:
//   1. 删除撒谎(delete 返回 ok 但回读存活)→ partial,绝不带病进入移动步;
//   2. 超时假失败(modify 报错但写已落地)→ 轻读复核,按成功继续;
//   3. 合并短路(重连后出现新 bridge)→ 对账失败 + 恢复段。
// 另加常规:成功路径 / 恢复路径 / snap 网格 / 共享树零 mutation / 端子覆盖。

type fakeMoveOps struct {
	mu    sync.Mutex
	comps []layoutComp
	wires []schGroupWire

	// liveNets 按队列出快照,末项粘住(snapshot → reconcile → 复查)。
	netSeq []map[string]map[string]bool
	netIdx int

	// bridgeSignatures 同样按队列出。
	bridgeSeq [][]string
	bridgeIdx int

	alive  map[string]bool // 桩线/旗等图元存活集(deleteBatch 从中移除)
	lieIDs map[string]bool // 删除撒谎:这些 id 无论删几轮都存活

	pos map[string][2]float64 // 位号 → 当前锚坐标(modify 更新;anchorOf 读)

	modifyErrOn       map[string]error // primitiveID → 注入的 modify 错误
	modifyLandsAnyway bool             // 注入错误时写仍落地(超时假失败)

	autoconnectFn func(conns []acConnSpec, replace bool) ([]string, []string, error)
	connectErr    error
	docErr        error // resolveDoc 注入:目标页不可解析(工程被重建)

	log []string
}

func (f *fakeMoveOps) resolveDoc() error {
	f.record("resolveDoc")
	return f.docErr
}

func (f *fakeMoveOps) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, fmt.Sprintf(format, args...))
}

func (f *fakeMoveOps) calls(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, l := range f.log {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeMoveOps) scene() ([]layoutComp, []schGroupWire, error) {
	f.record("scene")
	return f.comps, f.wires, nil
}

func (f *fakeMoveOps) liveNets() (map[string]map[string]bool, error) {
	f.record("liveNets")
	if len(f.netSeq) == 0 {
		return nil, errors.New("no nets configured")
	}
	i := f.netIdx
	if i >= len(f.netSeq) {
		i = len(f.netSeq) - 1
	}
	f.netIdx++
	return f.netSeq[i], nil
}

func (f *fakeMoveOps) deleteBatch(ids []string) error {
	f.record("delete %s", strings.Join(ids, ","))
	for _, id := range ids {
		if f.lieIDs[id] {
			continue // 撒谎:返回成功但没删
		}
		delete(f.alive, id)
	}
	return nil
}

func (f *fakeMoveOps) present(ids []string) ([]string, error) {
	var left []string
	for _, id := range ids {
		if f.alive[id] {
			left = append(left, id)
		}
	}
	f.record("present left=%d", len(left))
	return left, nil
}

func (f *fakeMoveOps) modify(primitiveID string, x, y float64, rot *float64) error {
	f.record("modify %s %g,%g", primitiveID, x, y)
	err := f.modifyErrOn[primitiveID]
	if err == nil || f.modifyLandsAnyway {
		for _, c := range f.comps {
			if c.ID == primitiveID {
				f.pos[strings.ToUpper(c.Designator)] = [2]float64{x, y}
			}
		}
	}
	return err
}

func (f *fakeMoveOps) settledPins(desig string) ([]layoutPin, error) {
	f.record("settledPins %s", desig)
	for _, c := range f.comps {
		if strings.EqualFold(c.Designator, desig) {
			return c.Pins, nil
		}
	}
	return nil, fmt.Errorf("no such part %s", desig)
}

func (f *fakeMoveOps) anchorOf(desig string) (float64, float64, bool, error) {
	f.record("anchorOf %s", desig)
	p, ok := f.pos[strings.ToUpper(desig)]
	if !ok {
		return 0, 0, false, nil
	}
	return p[0], p[1], true, nil
}

func (f *fakeMoveOps) connectPin(pinX, pinY float64, t moveConnTerm) error {
	f.record("connectPin %s %s %g,%g", t.Net, t.Direction, pinX, pinY)
	return f.connectErr
}

func (f *fakeMoveOps) autoconnect(conns []acConnSpec, replace bool) ([]string, []string, error) {
	refs := make([]string, 0, len(conns))
	for _, c := range conns {
		refs = append(refs, c.PinRef)
	}
	// 末尾追加 replace 标记:既能用前缀断言引脚集合,也能断言恢复段走 replace。
	f.record("autoconnect %s replace=%v", strings.Join(refs, ","), replace)
	if f.autoconnectFn != nil {
		return f.autoconnectFn(conns, replace)
	}
	return refs, nil, nil
}

func (f *fakeMoveOps) bridgeSignatures() ([]string, error) {
	if len(f.bridgeSeq) == 0 {
		f.record("bridges 0")
		return nil, nil
	}
	i := f.bridgeIdx
	if i >= len(f.bridgeSeq) {
		i = len(f.bridgeSeq) - 1
	}
	f.bridgeIdx++
	f.record("bridges %d", len(f.bridgeSeq[i]))
	return f.bridgeSeq[i], nil
}

// kernelFixture 造标准场景:R1(pid-r1)在 (100,100),pin1 (95,100)→5V 旗,
// pin2 (105,100)→GND 旗;两根桩线 w1/w2、两面旗 f1/f2 构成 R1 的独占树。
func kernelFixture() *fakeMoveOps {
	rot := 0.0
	comps := []layoutComp{
		{ID: "pid-r1", Designator: "R1", ComponentType: "part", X: 100, Y: 100,
			AnchorAvailable: true, Rotation: &rot,
			BBox: &layoutBBox{MinX: 90, MinY: 95, MaxX: 110, MaxY: 105},
			Pins: []layoutPin{{Number: "1", X: 95, Y: 100}, {Number: "2", X: 105, Y: 100}}},
		{ID: "pid-u9", Designator: "U9", ComponentType: "part", X: 500, Y: 500,
			AnchorAvailable: true,
			BBox:            &layoutBBox{MinX: 490, MinY: 490, MaxX: 510, MaxY: 510},
			Pins:            []layoutPin{{Number: "1", X: 495, Y: 500}}},
		{ID: "f1", ComponentType: "netflag", Net: "5V", X: 95, Y: 130, AnchorAvailable: true},
		{ID: "f2", ComponentType: "netflag", Net: "GND", X: 105, Y: 70, AnchorAvailable: true},
	}
	wires := []schGroupWire{
		{ID: "w1", Points: []float64{95, 100, 95, 130}},
		{ID: "w2", Points: []float64{105, 100, 105, 70}},
	}
	nets := map[string]map[string]bool{
		"5V":  {"R1.1": true},
		"GND": {"R1.2": true},
	}
	return &fakeMoveOps{
		comps:       comps,
		wires:       wires,
		netSeq:      []map[string]map[string]bool{nets},
		alive:       map[string]bool{"w1": true, "w2": true, "f1": true, "f2": true},
		pos:         map[string][2]float64{"R1": {100, 100}, "U9": {500, 500}},
		lieIDs:      map[string]bool{},
		modifyErrOn: map[string]error{},
	}
}

func kernelTestOpts() moveKernelOpts {
	return moveKernelOpts{Label: "test", RetryDelay: 1}
}

// kernelFixtureWithNeighbor 在标准场景上给第三方件 U9 接一根自己的 GND 树
// (w9+f9,几何上远离 R1 的树 —— 不构成共享树,深度清扫不会碰它)。
// GND 网横跨两棵树:{R1.2, U9.1}。这正是 esp32Mini P2 的最小复刻:移动 R1
// 删桩线时,共线合并吞掉的是 GND 树上**别人**的脚。
func kernelFixtureWithNeighbor() *fakeMoveOps {
	f := kernelFixture()
	f.comps = append(f.comps, layoutComp{ID: "f9", ComponentType: "netflag", Net: "GND", X: 495, Y: 470, AnchorAvailable: true})
	f.wires = append(f.wires, schGroupWire{ID: "w9", Points: []float64{495, 500, 495, 470}})
	f.alive["w9"], f.alive["f9"] = true, true
	f.netSeq = []map[string]map[string]bool{{
		"5V":  {"R1.1": true},
		"GND": {"R1.2": true, "U9.1": true},
	}}
	return f
}

// ── 成功路径 + snap 网格 ────────────────────────────────────────────────────

func TestMoveKernel_SuccessPathSnapsToGrid(t *testing.T) {
	f := kernelFixture()
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 163, Y: 97}, // 非格点目标
	}, kernelTestOpts())
	if err != nil {
		t.Fatalf("成功路径不该报错:%v", err)
	}
	if len(rep.Moved) != 1 || rep.Moved[0] != "R1" {
		t.Fatalf("Moved 应为 [R1],got %v", rep.Moved)
	}
	// snap 5 网格:163,97 → 165,95(off-grid 是重连全拒的根因)。
	want := "modify pid-r1 165,95"
	found := false
	for _, l := range f.log {
		if l == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("modify 坐标必须 snap 到 5 网格(want %q),log=%v", want, f.log)
	}
	// 整树(桩+旗)必须先删净。
	if f.calls("delete ") == 0 {
		t.Fatal("移动前必须整树删净旧桩/旗")
	}
	for _, id := range []string{"w1", "w2", "f1", "f2"} {
		if f.alive[id] {
			t.Errorf("旧图元 %s 未被清扫", id)
		}
	}
	// 快照重连必须覆盖全部已连 pin。
	if f.calls("autoconnect R1:1,R1:2") != 1 {
		t.Fatalf("快照重连应对 R1:1,R1:2 各连一次,log=%v", f.log)
	}
}

// ── 平台病 1:删除撒谎 → partial,不进入移动步 ──────────────────────────────

func TestMoveKernel_DeleteLiesGoesPartialAndNeverMoves(t *testing.T) {
	f := kernelFixture()
	f.lieIDs["f2"] = true // f2 删除永远静默 no-op(平台撒谎)
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err == nil {
		t.Fatal("删除撒谎必须报错(残留旧旗会挂上新桩线串网)")
	}
	if !rep.Partial {
		t.Fatal("删除撒谎必须计入 partial")
	}
	if f.calls("modify ") != 0 {
		t.Fatalf("绝不带病进入移动步:log=%v", f.log)
	}
	// 恢复段必须对全部快照 conns 重连(部分删除已落地,pins 有断点)。
	if f.calls("autoconnect R1:1,R1:2") != 1 {
		t.Fatalf("恢复段应按快照重连全部引脚,log=%v", f.log)
	}
}

// ── 平台病 2:超时假失败(报错但写落地)→ 轻读复核,按成功继续 ────────────────

func TestMoveKernel_FakeFailureRecheckedByLightRead(t *testing.T) {
	f := kernelFixture()
	f.modifyErrOn["pid-r1"] = errors.New("connector wedged: timeout")
	f.modifyLandsAnyway = true // 写其实已落地
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err != nil {
		t.Fatalf("假失败经轻读复核证实落地后必须按成功继续:%v", err)
	}
	if len(rep.FakeSuccess) != 1 || rep.FakeSuccess[0] != "R1" {
		t.Fatalf("FakeSuccess 应记下 R1,got %v", rep.FakeSuccess)
	}
	if f.calls("anchorOf R1") == 0 {
		t.Fatalf("必须走轻读复核(anchorOf),log=%v", f.log)
	}
	// 复核判成后管线继续:重连 + 对账都要跑。
	if f.calls("autoconnect ") == 0 {
		t.Fatal("复核判成后必须继续重连步")
	}
}

// ── 平台病 3:合并短路(新增 bridge)→ 对账失败 + 恢复段 ─────────────────────

func TestMoveKernel_NewBridgeFailsReconcileAndRecovers(t *testing.T) {
	f := kernelFixture()
	// 基线无 bridge;重连后出现 [5V,GND] 合并短路,恢复段也修不掉。
	f.bridgeSeq = [][]string{nil, {"[5V,GND]"}, {"[5V,GND]"}}
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err == nil {
		t.Fatal("新增 bridge = 真短路,对账必须失败(即使几何都成功了)")
	}
	if len(rep.NewBridges) != 1 || rep.NewBridges[0] != "[5V,GND]" {
		t.Fatalf("NewBridges 应报 [5V,GND],got %v", rep.NewBridges)
	}
	if !strings.Contains(err.Error(), "bridge") && !strings.Contains(err.Error(), "短路") {
		t.Fatalf("错误必须点名短路:%v", err)
	}
	// 恢复段必须被调用(第一次 autoconnect 是重连步,之后至少一次是恢复段)。
	if f.calls("autoconnect ") < 2 {
		t.Fatalf("对账红必须走恢复段,log=%v", f.log)
	}
}

// ── 恢复路径:modify 真失败(写没落地)→ 恢复段重连 + 如实报仍断 pin ──────────

func TestMoveKernel_HardMoveFailureRunsRecovery(t *testing.T) {
	f := kernelFixture()
	f.modifyErrOn["pid-r1"] = errors.New("connector rejected")
	f.autoconnectFn = func(conns []acConnSpec, replace bool) ([]string, []string, error) {
		return []string{"R1:1"}, []string{"R1:2"}, fmt.Errorf("1 connection(s) failed")
	}
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err == nil {
		t.Fatal("移动真失败必须报错(恢复不是吞错)")
	}
	if len(rep.Recovered) != 1 || rep.Recovered[0] != "R1:1" {
		t.Fatalf("Recovered 应为 [R1:1],got %v", rep.Recovered)
	}
	// 仍断 pin 必须带期望网名(REF→期望网,可直接喂 `sch connect`)。
	if len(rep.StillBroken) != 1 || rep.StillBroken[0] != "R1:2→GND" {
		t.Fatalf("StillBroken 应为 [R1:2→GND],got %v", rep.StillBroken)
	}
	if !strings.Contains(err.Error(), "R1:2→GND") {
		t.Fatalf("错误必须点名仍断的 pin 及期望网:%v", err)
	}
}

// ── 对账首轮红、恢复段补连后复查绿 = 成功(带 note)─────────────────────────

func TestMoveKernel_ReconcileHealsAfterRecovery(t *testing.T) {
	f := kernelFixture()
	degraded := map[string]map[string]bool{"5V": {"R1.1": true}} // GND 网丢了
	full := f.netSeq[0]
	// 读序:快照(full)→ 合并早检(full,干净)→ 对账首轮(degraded,红)→
	// 恢复段补连 → 对账复查(full,绿)。
	f.netSeq = []map[string]map[string]bool{full, full, degraded, full}
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err != nil {
		t.Fatalf("恢复段补连后复查绿应算成功:%v", err)
	}
	if len(rep.Notes) == 0 {
		t.Fatal("首轮红必须留痕(note)")
	}
	if f.calls("autoconnect ") < 2 {
		t.Fatalf("必须走过恢复段补连,log=%v", f.log)
	}
}

// ── 共享树 fail-closed:零 mutation ─────────────────────────────────────────

func TestMoveKernel_SharedTreeRefusesWithZeroMutation(t *testing.T) {
	f := kernelFixture()
	// w3 连接 R1.1 与 U9.1(共享树:触到非成员 pin)。
	f.wires = append(f.wires, schGroupWire{ID: "w3", Points: []float64{95, 100, 495, 500}})
	_, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err == nil {
		t.Fatal("共享树必须拒绝(删掉它会切断组外电路)")
	}
	if f.calls("delete ") != 0 || f.calls("modify ") != 0 || f.calls("autoconnect ") != 0 {
		t.Fatalf("拒绝必须零 mutation,log=%v", f.log)
	}
}

// ── 目标页不可解析(工程被重建)→ fail-closed:零 mutation、零恢复输出 ────────
//
// 真机 smoke 实录:ceshi 工程被重建后,旧内核一路走到重连步才报 `--doc no
// document named or with uuid`,恢复段因同一错误再失败,最后输出一份虚假的
// 「31 pin 断开」警告 —— 页面根本不存在,无实际损伤,但报告严重误导。

func TestMoveKernel_DocUnresolvableFailsClosed(t *testing.T) {
	f := kernelFixture()
	f.docErr = errors.New(`no document named or with uuid "doc-gone" (run ` + "`easyeda doc ls`" + ` to see options)`)
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err == nil {
		t.Fatal("目标页不可解析必须报错拒绝操作")
	}
	if !strings.Contains(err.Error(), "目标页不存在/已被重建") {
		t.Fatalf("错误必须明说目标页不存在/已被重建:%v", err)
	}
	// 零 mutation:快照之后的任何一步都不许发生。
	if f.calls("delete ") != 0 || f.calls("modify ") != 0 || f.calls("connectPin ") != 0 {
		t.Fatalf("fail-closed 必须零 mutation,log=%v", f.log)
	}
	// 零恢复输出:不许再对不存在的页面跑恢复重连、更不许报虚假的断开清单。
	if f.calls("autoconnect ") != 0 {
		t.Fatalf("不存在的页面不许跑恢复重连,log=%v", f.log)
	}
	if len(rep.Recovered) != 0 || len(rep.StillBroken) != 0 {
		t.Fatalf("不许输出虚假的恢复/断开清单:recovered=%v stillBroken=%v", rep.Recovered, rep.StillBroken)
	}
}

// ── 空画布矛盾:items 非空但页面 0 器件 + 空网表 → fail-closed 零 mutation ────

func TestMoveKernel_EmptyPageContradictionFailsClosed(t *testing.T) {
	f := kernelFixture()
	f.comps = nil // 页面被重建成空页:没有任何器件
	f.wires = nil
	f.netSeq = []map[string]map[string]bool{{}} // 网表为空
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err == nil {
		t.Fatal("空画布与非空 items 矛盾必须报错拒绝操作")
	}
	if !strings.Contains(err.Error(), "已被重建") && !strings.Contains(err.Error(), "器件数为 0") {
		t.Fatalf("错误必须点明矛盾:%v", err)
	}
	if f.calls("delete ") != 0 || f.calls("modify ") != 0 || f.calls("autoconnect ") != 0 {
		t.Fatalf("fail-closed 必须零 mutation 零恢复,log=%v", f.log)
	}
	if len(rep.Recovered) != 0 || len(rep.StillBroken) != 0 {
		t.Fatalf("不许输出虚假的恢复/断开清单:%+v", rep)
	}
}

// ── 平台病 4:删桩线触发共线合并吞第三方网(esp32Mini P2 P0 缺陷注入)─────────
//
// 真机实录:`zone-arrange --apply` 在 P2 页删证后,相邻共线导线自动合并把
// GND 树上 9 个**非移动件**的地脚灌进 +3V3、GND 整网消失;第 5 步对账(断言②)
// 抓到了,但旧恢复段只重连「被移动件」的涉及 pin —— 第三方 pin 无人来救,
// 页面只能删页重建。以下三个注入分别钉住:合并早检(3.5 步就发现并修)、
// 恢复段全页扩权(对账红时第三方 pin 也按快照重连)、修不动时结构化列全
// (报告从「页面已毁」降级为「N 个 pin 待手工恢复」)。

// 3.5 合并早检:删证吞掉 U9.1(灌进 +3V3),必须在新桩线落地(第 4 步)之前
// 用 replace 重连修回,而不是拖到第 5 步对账。
func TestMoveKernel_MergeSwallowsThirdPartyPin_EarlyDetectAndRepair(t *testing.T) {
	f := kernelFixtureWithNeighbor()
	healthy := f.netSeq[0]
	corrupt := map[string]map[string]bool{"+3V3": {"U9.1": true}} // GND 整网消失,U9.1 被灌进 +3V3
	// 读序:快照(healthy)→ 合并早检(corrupt)→ 对账(healthy:早检修复后一致)。
	f.netSeq = []map[string]map[string]bool{healthy, corrupt, healthy}
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err != nil {
		t.Fatalf("早检修复后应成功:%v", err)
	}
	idxRepair, idxRest := -1, -1
	for i, l := range f.log {
		if strings.HasPrefix(l, "autoconnect U9:1") {
			idxRepair = i
			if !strings.Contains(l, "replace=true") {
				t.Fatalf("早检修复必须走 replace(灌错网要先拆再连):%s", l)
			}
		}
		if strings.HasPrefix(l, "autoconnect R1:1,R1:2") {
			idxRest = i
		}
	}
	if idxRepair < 0 {
		t.Fatalf("合并早检必须按快照重连第三方 pin U9:1,log=%v", f.log)
	}
	if idxRest < 0 || idxRepair > idxRest {
		t.Fatalf("早检修复必须发生在重连步(新桩线落地)之前:repair@%d rest@%d,log=%v", idxRepair, idxRest, f.log)
	}
	joined := strings.Join(rep.Notes, ";")
	if !strings.Contains(joined, "合并早检") {
		t.Fatalf("必须留痕归因(合并早检),notes=%v", rep.Notes)
	}
}

// 恢复段全页扩权:合并的后果到第 5 步对账才暴露时,恢复段必须把**第三方**
// 偏离 pin(U9:1)也按快照网名重连(replace),复查绿才算恢复成功。
func TestMoveKernel_ThirdPartyDamageRecoveredAtReconcile(t *testing.T) {
	f := kernelFixtureWithNeighbor()
	healthy := f.netSeq[0]
	corrupt := map[string]map[string]bool{"+3V3": {"U9.1": true}}
	// 读序:快照 → 早检(干净)→ 对账首轮(corrupt,红)→ 恢复 → 复查(healthy,绿)。
	f.netSeq = []map[string]map[string]bool{healthy, healthy, corrupt, healthy}
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err != nil {
		t.Fatalf("恢复段全页补连后复查绿应算成功:%v", err)
	}
	repaired := false
	for _, l := range f.log {
		if strings.HasPrefix(l, "autoconnect ") && strings.Contains(l, "U9:1") && strings.Contains(l, "replace=true") {
			repaired = true
		}
	}
	if !repaired {
		t.Fatalf("恢复段必须对第三方 pin U9:1 走 replace 重连(只救移动集合=抓到了但救不回),log=%v", f.log)
	}
	found := false
	for _, r := range rep.Recovered {
		if r == "U9:1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Recovered 必须含第三方 pin U9:1,got %v", rep.Recovered)
	}
	if len(rep.Notes) == 0 {
		t.Fatal("对账首轮红必须留痕(note)")
	}
}

// 恢复不动(网表持续腐坏)→ 不许谎报成功,仍偏离 pin 连同期望网名结构化列全
// (REF→期望网,可直接喂 `sch connect`)—— 报告从「页面已毁」降级为
// 「N 个 pin 待手工恢复,清单如下」。
func TestMoveKernel_ThirdPartyUnrecoverableStructuredList(t *testing.T) {
	f := kernelFixtureWithNeighbor()
	healthy := f.netSeq[0]
	corrupt := map[string]map[string]bool{"+3V3": {"U9.1": true}}
	// 对账起持续腐坏(粘住):恢复两轮都修不动。
	f.netSeq = []map[string]map[string]bool{healthy, healthy, corrupt}
	rep, err := schMoveKernelWith(f, []moveItem{
		{Designator: "R1", HasTarget: true, X: 200, Y: 100},
	}, kernelTestOpts())
	if err == nil {
		t.Fatal("网表持续与快照不符必须失败(判据是电气不是坐标)")
	}
	wantBroken := map[string]bool{"R1:1→5V": true, "R1:2→GND": true, "U9:1→GND": true}
	got := map[string]bool{}
	for _, s := range rep.StillBroken {
		got[s] = true
	}
	for w := range wantBroken {
		if !got[w] {
			t.Fatalf("StillBroken 必须结构化列全(含第三方,REF→期望网),缺 %s,got %v", w, rep.StillBroken)
		}
	}
	if !strings.Contains(err.Error(), "U9:1→GND") || !strings.Contains(err.Error(), "待手工恢复") {
		t.Fatalf("错误必须给出可执行清单(REF→期望网 + 待手工恢复):%v", err)
	}
	if !strings.Contains(err.Error(), "sch connect") {
		t.Fatalf("错误必须指路 `sch connect`:%v", err)
	}
}

// deficit 解析:快照浮空却被灌进网的 pin 只能手工拆(不自动 disconnect ——
// 拆共享树会连累树上无辜 pin),必须落进 manual 清单并指路 `sch disconnect`。
func TestMovePinDeficits_SpuriousGainListedManualOnly(t *testing.T) {
	before := map[string]map[string]bool{"GND": {"R1.2": true}}
	after := map[string]map[string]bool{"GND": {"R1.2": true}, "+3V3": {"U9.1": true}}
	defs := moveKernelPinDeficits(before, after)
	if len(defs) != 1 || defs[0].Ref != "U9:1" || defs[0].WantNet != "" || defs[0].GotNet != "+3V3" {
		t.Fatalf("应只有 U9:1 的 spurious-gain deficit,got %+v", defs)
	}
	specs, manual := moveKernelDeficitSpecs(defs)
	if len(specs) != 0 || len(manual) != 1 {
		t.Fatalf("快照浮空的 pin 不许自动重连,只进 manual:specs=%v manual=%v", specs, manual)
	}
	if s := manual[0].String(); !strings.Contains(s, "sch disconnect") || !strings.Contains(s, "U9:1") {
		t.Fatalf("manual 清单必须指路 sch disconnect:%s", s)
	}
}

// ── 显式端子:覆盖的 pin 不再走快照 autoconnect;器件可原位不动 ───────────────

func TestMoveKernel_ExplicitTermsExcludeSnapshotReconnect(t *testing.T) {
	f := kernelFixture()
	items := []moveItem{{
		Designator: "R1", // HasTarget=false:destagger 形态,器件不动只重排 marker
		Terms: func(pins []layoutPin) ([]moveConnTerm, error) {
			return []moveConnTerm{{Pin: "1", Kind: "power", Net: "5V", Direction: "left", Rotation: 90, Offset: 40}}, nil
		},
	}}
	rep, err := schMoveKernelWith(f, items, kernelTestOpts())
	if err != nil {
		t.Fatalf("端子重排不该报错:%v", err)
	}
	if len(rep.Moved) != 0 {
		t.Fatalf("器件未动,Moved 应为空,got %v", rep.Moved)
	}
	if f.calls("connectPin 5V left") != 1 {
		t.Fatalf("显式端子必须走 connect_pin,log=%v", f.log)
	}
	// R1:1 被端子覆盖 → 快照 autoconnect 只连 R1:2。
	if f.calls("autoconnect R1:2") != 1 || f.calls("autoconnect R1:1,R1:2") != 0 {
		t.Fatalf("被显式端子覆盖的 pin 不得重复走快照重连,log=%v", f.log)
	}
	if f.calls("modify ") != 0 {
		t.Fatalf("HasTarget=false 不得 modify,log=%v", f.log)
	}
}
