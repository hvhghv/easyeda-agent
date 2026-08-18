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

	autoconnectFn func(conns []acConnSpec) ([]string, []string, error)
	connectErr    error

	log []string
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

func (f *fakeMoveOps) autoconnect(conns []acConnSpec) ([]string, []string, error) {
	refs := make([]string, 0, len(conns))
	for _, c := range conns {
		refs = append(refs, c.PinRef)
	}
	f.record("autoconnect %s", strings.Join(refs, ","))
	if f.autoconnectFn != nil {
		return f.autoconnectFn(conns)
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
	f.autoconnectFn = func(conns []acConnSpec) ([]string, []string, error) {
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
	if len(rep.StillBroken) != 1 || rep.StillBroken[0] != "R1:2" {
		t.Fatalf("StillBroken 应为 [R1:2],got %v", rep.StillBroken)
	}
	if !strings.Contains(err.Error(), "R1:2") {
		t.Fatalf("错误必须点名仍断的 pin:%v", err)
	}
}

// ── 对账首轮红、恢复段补连后复查绿 = 成功(带 note)─────────────────────────

func TestMoveKernel_ReconcileHealsAfterRecovery(t *testing.T) {
	f := kernelFixture()
	degraded := map[string]map[string]bool{"5V": {"R1.1": true}} // GND 网丢了
	full := f.netSeq[0]
	f.netSeq = []map[string]map[string]bool{full, degraded, full}
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
