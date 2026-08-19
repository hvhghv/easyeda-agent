package app

// sch_move_kernel.go — ADR-0004 Decision 1:单一安全 move 内核(五步管线)。
//
// **所有**改变「已连线器件位置」的执行路径必须走这里(Decision 2 的五个调用方:
// group-move / zone move / zone-arrange --apply / zone relayout(tidy apply)/
// destagger --apply)。规划各家自己做,执行只准调内核 —— 「挪动」的失败模式
// 从此收敛到一处。
//
//	┌ 1. 快照   电气快照(pin→net 全表,readLiveNets)+ 涉及图元整树清单
//	│           (tidyDeepSweepPlan,union-find 树判定)+ bridge 基线
//	├ 2. 删证   整树删除(分批 40)+ 回读证实;删除撒谎 → 计 partial,
//	│           **绝不带病进入移动步**(残留旧旗会挂上新桩线串网)
//	├ 3. 移动   逐件 modify,坐标先 snap 到 5 网格(schAnchorGrid —— off-grid
//	│           重连全拒的根因);modify 报错时**轻读复核**(负载停摆的假失败
//	│           大概率写已落地,盲重试造重复、盲回退丢移动)
//	├ 3.5 合并早检  删证是共线合并的触发时刻:此处读一次全页网表,把被合并
//	│           吞掉的**第三方** pin 在新桩线落地前就修回(为何不在第 2 步内:
//	│           netlist 族读紧贴 modify 会毒化写,而删证→移动之间零导线变更,
//	│           后置检测保真度相同 —— 见步内注释)
//	├ 4. 重连   显式计划端子(connect_pin,梯次桩长等参数由调用方闭包按实测
//	│           pins 计算)+ 其余按快照 autoconnect(评分避让;器件在原位也能连回)
//	└ 5. 对账   网表逐 pin 与快照比对(判据是电气不是坐标)+ bridge 增量检查
//	            (合并短路当场抓);红 → 恢复段**全页**补连(至多两轮)再复查
//
// 失败语义(#151 部分应用约定):任何一步失败 → 立即进入恢复段。恢复段的辖区
// 是**全页**而非移动集合 —— 快照是 pin→net 全表,对账逐 pin 比对,所以第三方
// 网被毁能检出;恢复段若只重连移动集合就是「抓到了但救不回」(esp32Mini P2:
// 共线合并吞掉 GND 树上 9 个第三方地脚灌进 +3V3,页面只能删页重建)。做法:
// 凡快照里有网名、现在断连或网名不符的 pin,一律按快照网名重连(灌错网的走
// replace:带回读验证的 disconnect 后重连);autoconnect 按当前几何重新解析
// 引脚坐标,挪成没挪成的都能连回。返回结构化 moveReport;绝不留「桩线已清、
// 器件没挪、连接全断」的 PARTIAL 尸体。恢复本身失败 → 仍偏离 pin 连同期望
// 网名结构化列全(REF→期望网,可直接喂 `sch connect`)。
//
// 教训出处(全部真机定案,见 ADR-0004 Context):
//   - marker-move-breaks-on-wire-merge:删单根桩线触发相邻共线导线合并 → 串网;
//     唯一安全形态 = 整树删净后重建(此刻器件身上没有任何导线,modify 零合并风险);
//   - platform-delete-lies:大批量 delete 返回 true 实际 no-op → 分批 + 回读证实;
//   - connector-wedge-fake-failure:停摆期报失败的写大概率已落地 → 轻读复核;
//   - 对账不过 = 失败,即使几何都成功了。

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	// moveKernelDeleteBatch:平台删除 API 的经验安全批量(超过这个量级开始
	// 静默丢弃 —— groupRebuildDeleteVerified 同源)。
	moveKernelDeleteBatch = 40
	// moveKernelPosEps:锚坐标比对容差(与 schGroupEps 同量级)。
	moveKernelPosEps = 0.5
)

// moveConnTerm 是一条显式重连指令(规划端子):内核用 connect_pin 按给定
// 方向/桩长/文字 rotation 落地,不走 autoconnect 评分 —— 规划算出的梯次桩长
// 等自由度必须原样执行(规划自由度 ⊆ 执行自由度)。
type moveConnTerm struct {
	Pin       string // pin number(属所在 item 的位号)
	Kind      string // connect_pin canonical kind:power / ground / net_port_bi …
	Net       string
	Direction string  // up | down | left | right
	Rotation  float64 // 文字 rotation(调用方从 tidyLabelRotation 校准表取)
	Offset    float64 // 桩长;0 = connect_pin 默认
}

// moveItem 是内核输入的一件刚体成员(primitiveId 由内核从快照解析,调用方
// 只报位号 —— 快照与执行用同一份场景,不会拿 stale id 去 mutate)。
type moveItem struct {
	Designator string
	// HasTarget=false = 器件本体不动,只重建它的桩线/旗(destagger 挪 marker)。
	HasTarget bool
	X, Y      float64 // 目标锚坐标(内核 snap 5 网格)
	// Rot 非 nil 时 modify 一并改 rotation(RotCandidates 为空时生效)。
	Rot *float64
	// RotCandidates 非空时逐候选实测消解(库件符号镜像二义):modify(候选) →
	// settle 实测 pins → VerifyPins;全部候选不符 = 本件失败 → 恢复段。
	RotCandidates []float64
	VerifyPins    func(pins []layoutPin) (bool, error)
	// CenterOnPins:旋转后 bbox 未知,由 pin 中点驱动落位(zone-arrange 转竖件):
	// 原地转 → 实测 pin 中点 → 平移使中点对 (X,Y)。
	CenterOnPins bool
	// Terms 显式重连指令(nil = 本件全部 pin 走快照 autoconnect)。回调收到
	// settle 后的实测 pins —— 依赖实测几何的参数(统一总高的桩长等)在此计算。
	// 被 Terms 覆盖的 pin 不再参与快照 autoconnect(不双连)。
	Terms func(pins []layoutPin) ([]moveConnTerm, error)
}

// moveReport 是内核的结构化结果(成功与失败都填)。
type moveReport struct {
	Moved       []string // 落位成功的位号(含假失败复核后判成的)
	Skipped     []string // 零位移 no-op(未发 mutation)
	FakeSuccess []string // modify 报错但轻读复核证实已落地的位号
	Partial     bool     // 平台病(删除撒谎等)致部分执行,未进入后续步骤
	Recovered   []string // 恢复段重连回来的 pin refs
	StillBroken []string // 恢复后仍断的 pin refs(可直接喂 `sch connect`)
	NewBridges  []string // 对账检出的新增 wire-bridge(真短路)
	NetDiffs    []string // 对账检出的网表差异(人类可读)
	Notes       []string
}

// moveStubPolicy 是「重连时桩线伸多长」的策略 —— **挪动侧的那把尺**。
//
// 背景(2026-08-20 收敛性缺陷):内核第 4 步对「没有计划端子」的 pin 一律走
// autoconnect 自由评分,而评分器的档位里常驻 min+k·laneStepFor(netport 一档
// ~89)。于是**刚体平移**这种「几何不变地搬走」的操作,会把原本 30 的短桩换成
// 107 的长桩:真机实测 group-move --dx 40 把 U 组框从 315×389 撑到 523×406
// (+208 ≈ 两档),一次「挪一下让开」反而把 phase A 的区内收敛撤销了大半。
// 用户/agent 的直觉操作不该破坏收敛成果。
type moveStubPolicy string

const (
	// moveStubPreserve(默认):按移动前实测的桩线方向/长度原样重建。刚体平移的
	// 语义就是几何不变,重连不该重新评分。
	moveStubPreserve moveStubPolicy = "preserve"
	// moveStubFree:退回 autoconnect 自由评分(旧行为)。**负对照与显式请求专用** ——
	// 只有当调用方确实想让落点重新优化(而非复现)时才用。
	moveStubFree moveStubPolicy = "free"
)

// moveKernelOpts 是内核的运行选项。
type moveKernelOpts struct {
	Label      string        // 日志/报错前缀(调用方命令名)
	RetryDelay time.Duration // connect_pin 单次重试前的等待(0 = 生产默认 2s)
	Stdout     io.Writer
	Stderr     io.Writer
	// StubPolicy 空值 = moveStubPreserve。
	StubPolicy moveStubPolicy
	// MaxStub 是**常规重连步**里 autoconnect 的桩长硬上限(0 = 按下面的兜底推导)。
	// zone-arrange 传规划里的最大桩长 —— 落地框就不会越过规划框。
	//
	// **恢复段有意不夹**:那是火警现场,把连接接回来比把框收窄重要;夹太死会把
	// 「能自动救回」变成「N 个 pin 待手工恢复」。恢复段带来的伸展由 --apply 的
	// 落地复判如实报出来(而不是打绿勾),这是「正确性 > 收敛性,但偏差必须可见」。
	MaxStub float64
}

// stubRules 是常规重连步给 autoconnect 的规则(带硬上限)。
func (o moveKernelOpts) stubRules(observedMax float64) autoconnectRules {
	r := defaultAutoconnectRules()
	cap := o.MaxStub
	if cap <= 0 {
		// 兜底上限:细档全留(OffsetMax=80),但砍掉 laneStepFor 的标准档位
		// (netport 一档 ~89、三档 ~285)与无上界的 extendedOffsets —— 那才是把
		// 组框撑成本体几倍的那一段。页面本来就有更长的桩时按页面来,不倒逼收紧。
		cap = maxF(r.OffsetMax, observedMax)
	}
	r.OffsetCap = cap
	return r
}

// moveKernelStubSnapshot 读出成员 pin **移动前**的桩线几何(方向 + 长度 + 标记
// 类型/网名),供 preserve 策略原样重建。key = "DESIG:PIN"(全大写,与
// acConnSpec.PinRef / covered 同口径)。
//
// 纯函数:只吃场景快照,不碰平台 —— 判据与执行同一份数据(判定坐标 = 落地坐标)。
// 只认「pin 在导线树上且树上有可重建标记(netflag/netport)」的桩;普通导线直连、
// netlabel 等重建不了的,留给 autoconnect,不硬塞。
func moveKernelStubSnapshot(memberSet map[string]bool, comps []layoutComp, wires []schGroupWire) map[string]moveConnTerm {
	roots := tidyWireRoots(wires)
	var markers []layoutComp
	for _, c := range comps {
		if isSchMarker(c.ComponentType) {
			markers = append(markers, c)
		}
	}
	out := map[string]moveConnTerm{}
	for _, c := range comps {
		if c.ComponentType != "" && c.ComponentType != schLayoutPartType && c.ComponentType != "part" {
			continue
		}
		if !memberSet[strings.ToUpper(strings.TrimSpace(c.Designator))] {
			continue
		}
		for _, p := range c.Pins {
			m, hasM, onWire := tidyPinAttachment(p.X, p.Y, wires, roots, markers)
			if !onWire || !hasM || m.Net == "" {
				continue
			}
			kind := tidyRestoreKind(m.ComponentType, m.Net)
			if kind == "" {
				continue // netlabel 等:connect_pin 重建不了
			}
			dir, off := tidyStubDirection(p.X, p.Y, m.X, m.Y)
			if dir == "" || off <= 0 {
				continue // 标记压在 pin 上(零长桩):没有可复现的几何
			}
			rot, rerr := tidyLabelRotation(kind, dir)
			if rerr != nil {
				continue
			}
			out[strings.ToUpper(strings.TrimSpace(c.Designator))+":"+p.Number] = moveConnTerm{
				Pin: p.Number, Kind: kind, Net: m.Net, Direction: dir, Rotation: rot, Offset: off,
			}
		}
	}
	return out
}

// moveKernelOps 是内核对平台的全部依赖面 —— 生产走 daemonMoveOps(连接器),
// 测试用 fake 注入三个平台病(删除撒谎 / 超时假失败 / 合并短路)。
type moveKernelOps interface {
	// resolveDoc 验证目标页在当前窗口仍可解析(doc ls / ensureActiveDoc 同源
	// 判定)。目标页被删除/工程被重建时,不做这一步会一路走到重连步才报
	// 「no document named or with uuid」,恢复段又因同一错误失败,最后输出
	// 一份「N pin 断开」的虚假警告(页面根本不存在,无实际损伤,但报告
	// 严重误导)—— 真机 smoke 实录。不可解析 → fail-closed 零 mutation。
	resolveDoc() error
	// scene 读一次场景:components(带 bbox+pins)+ stable 导线快照。
	scene() ([]layoutComp, []schGroupWire, error)
	// liveNets 读实时网表(net → pin 集合;唯一可信的连接判据)。
	liveNets() (map[string]map[string]bool, error)
	deleteBatch(ids []string) error
	// present 回读 ids 中仍然存在的那些(删除 API 会撒谎,不能信返回值)。
	present(ids []string) ([]string, error)
	modify(primitiveID string, x, y float64, rot *float64) error
	settledPins(desig string) ([]layoutPin, error)
	// anchorOf 轻读一件的当前锚坐标(假失败复核用;ok=false = 读到了场景但
	// 该件无锚坐标)。
	anchorOf(desig string) (x, y float64, ok bool, err error)
	connectPin(pinX, pinY float64, t moveConnTerm) error
	// autoconnect 按快照网名连回;replace=true 时对「已连在别的网」的 pin 先走
	// 带回读验证的 schematic.pin.disconnect 再重连(autoconnect --replace 语义)
	// —— 恢复段治「灌错网」(9 个地脚被灌进 +3V3)必需,普通重连保持 false。
	// rules 带桩长硬上限(OffsetCap):常规重连步夹住,恢复段传默认(见
	// moveKernelOpts.MaxStub 的注释)。
	autoconnect(conns []acConnSpec, replace bool, rules autoconnectRules) (succeeded, failed []string, err error)
	// bridgeSignatures 返回当前全部 wire-bridge 的排序签名(如 "[GND,5V]")。
	bridgeSignatures() ([]string, error)
}

// movePinDeficit 是「与电气快照不符」的一只 pin —— **全页范围,不限移动集合**。
// esp32Mini P2 实锤:删桩线触发相邻共线导线合并,吞掉的是 GND 树上**第三方**
// (非移动件)的脚(9 个地脚被灌进 +3V3、GND 整网消失);对账逐 pin 比对能检出,
// 但恢复段若只重连移动集合就是「抓到了但救不回」。deficit 把 diff 解析成可
// 重连的三元组,喂给 replace-autoconnect。
type movePinDeficit struct {
	Ref     string // DESIG:PIN(`sch connect --pin` 格式)
	WantNet string // 快照网名;"" = 快照里本是浮空(现在却有网 → 只能拆不能连)
	GotNet  string // 当前网名;"" = 当前断连
}

// String 输出结构化清单项:可重连的 = 「REF→期望网」(可直接喂 `sch connect
// --pin REF --net 期望网`);快照浮空却被灌进网的 = 只能手工拆
// (`sch disconnect --pin REF`),自动拆共享树风险太高(拆树会连累树上无辜 pin)。
func (d movePinDeficit) String() string {
	if d.WantNet != "" {
		return d.Ref + "→" + d.WantNet
	}
	return fmt.Sprintf("%s(快照浮空,现被灌进 %s,需 `sch disconnect --pin %s`)", d.Ref, d.GotNet, d.Ref)
}

// moveKernelPinDeficits 逐 pin 比对快照与当前网表,返回全部偏离 pin。
// ref 形如 "R1.2"(netlist 口径),输出转成 "R1:2"(connect/autoconnect 口径)。
func moveKernelPinDeficits(before, after map[string]map[string]bool) []movePinDeficit {
	pinNetOf := func(live map[string]map[string]bool) map[string]string {
		out := map[string]string{}
		for net, pins := range live {
			for ref := range pins {
				out[strings.ToUpper(ref)] = net
			}
		}
		return out
	}
	wantBy, gotBy := pinNetOf(before), pinNetOf(after)
	seen := map[string]bool{}
	var defs []movePinDeficit
	add := func(key, want, got string) {
		if seen[key] || want == got {
			return
		}
		seen[key] = true
		defs = append(defs, movePinDeficit{Ref: strings.Replace(key, ".", ":", 1), WantNet: want, GotNet: got})
	}
	for key, want := range wantBy {
		add(key, want, gotBy[key])
	}
	for key, got := range gotBy {
		add(key, wantBy[key], got)
	}
	// 定序与 groupRebuildConnSpecs 同理由:电源 → 地 → 信号(先落满方向最固定的
	// marker,信号才有得绕);拆不了的(WantNet 空)排最后,只进清单不进重连。
	kindRank := map[string]int{"power": 0, "gnd": 1, "agnd": 1, "pgnd": 1}
	rank := func(d movePinDeficit) int {
		if d.WantNet == "" {
			return 3
		}
		if r, ok := kindRank[bapFlagKind(d.WantNet)]; ok {
			return r
		}
		return 2
	}
	sort.Slice(defs, func(i, j int) bool {
		if ri, rj := rank(defs[i]), rank(defs[j]); ri != rj {
			return ri < rj
		}
		if defs[i].WantNet != defs[j].WantNet {
			return defs[i].WantNet < defs[j].WantNet
		}
		return defs[i].Ref < defs[j].Ref
	})
	return defs
}

// moveKernelDeficitSpecs 把 deficits 拆成「可自动重连的 autoconnect 规格」和
// 「只能手工拆的」两份。
func moveKernelDeficitSpecs(defs []movePinDeficit) (specs []acConnSpec, manual []movePinDeficit) {
	for _, d := range defs {
		if d.WantNet == "" {
			manual = append(manual, d)
			continue
		}
		specs = append(specs, acConnSpec{PinRef: d.Ref, Kind: bapFlagKind(d.WantNet), Net: d.WantNet})
	}
	return specs, manual
}

// moveKernelFormatDeficits 输出结构化待手工清单(报告从「页面已毁」降级为
// 「N 个 pin 待手工恢复,清单如下」的载体)。
func moveKernelFormatDeficits(defs []movePinDeficit) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.String())
	}
	return out
}

// schMoveKernel 是生产入口:cfg 必须是已 pin 的配置,window 是解析后的窗口,
// docUUID 是目标页。
func schMoveKernel(cfg *appConfig, window, docUUID string, items []moveItem, opts moveKernelOpts) (*moveReport, error) {
	if opts.RetryDelay == 0 {
		opts.RetryDelay = 2 * time.Second // zaaRetry 同源:平台随机吃掉一个连接,歇口气再试一次
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	return schMoveKernelWith(&daemonMoveOps{cfg: cfg, win: window, docUUID: docUUID, stderr: stderr}, items, opts)
}

// schMoveKernelWith 是内核本体(ops 可注入,失败注入测试的接缝)。
func schMoveKernelWith(ops moveKernelOps, items []moveItem, opts moveKernelOpts) (*moveReport, error) {
	stdout, stderr := opts.Stdout, opts.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	label := opts.Label
	if label == "" {
		label = "move-kernel"
	}
	rep := &moveReport{}
	if len(items) == 0 {
		return rep, fmt.Errorf("%s:内核无输入(空刚体集合)", label)
	}

	// ── 1. 快照 ────────────────────────────────────────────────────────────
	// 先验目标页:不可解析(被删除/工程被重建)→ fail-closed 零 mutation。
	if derr := ops.resolveDoc(); derr != nil {
		return rep, fmt.Errorf("%s:目标页不存在/已被重建,拒绝操作(画布零改动):%w", label, derr)
	}
	comps, wires, err := ops.scene()
	if err != nil {
		return rep, fmt.Errorf("%s 快照读场景:%w", label, err)
	}
	memberSet := map[string]bool{}
	for _, it := range items {
		d := strings.ToUpper(strings.TrimSpace(it.Designator))
		if d == "" {
			return rep, fmt.Errorf("%s:成员位号为空(内部不一致)", label)
		}
		memberSet[d] = true
	}
	live, err := ops.liveNets()
	if err != nil {
		return rep, fmt.Errorf("%s 快照读网表(重连的唯一依据):%w", label, err)
	}
	// 空画布矛盾判定:items 非空但页面器件数为 0 且网表为空 —— 操作对象根本
	// 不在画布上(目标页多半已被重建成空页),继续走会输出虚假的断连报告。
	partCount := 0
	for _, c := range comps {
		if c.ComponentType == "" || c.ComponentType == schLayoutPartType {
			partCount++
		}
	}
	if partCount == 0 && len(live) == 0 {
		return rep, fmt.Errorf("%s:页面器件数为 0 且网表为空,与 %d 个待移动成员矛盾 —— 目标页可能已被重建,拒绝操作(画布零改动);`easyeda doc ls` 核对后重跑",
			label, len(items))
	}
	// 桩线快照:成员 pin 移动前的桩几何(preserve 策略原样重建的原料)。必须在
	// **删证之前**从同一份场景快照取 —— 删完就没得量了。
	policy := opts.StubPolicy
	if policy == "" {
		policy = moveStubPreserve
	}
	stubSnap := map[string]moveConnTerm{}
	rotBefore := map[string]float64{}
	if policy == moveStubPreserve {
		stubSnap = moveKernelStubSnapshot(memberSet, comps, wires)
		for _, c := range comps {
			if c.Rotation != nil && memberSet[strings.ToUpper(strings.TrimSpace(c.Designator))] {
				rotBefore[strings.ToUpper(strings.TrimSpace(c.Designator))] = *c.Rotation
			}
		}
	}
	conns, movable := groupRebuildConnSpecs(comps, memberSet, live)
	byDesig := map[string]groupRebuildMember{}
	for _, m := range movable {
		byDesig[strings.ToUpper(m.Designator)] = m
	}
	var missing []string
	for d := range memberSet {
		if _, ok := byDesig[d]; !ok {
			missing = append(missing, d)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return rep, fmt.Errorf("%s:成员不在当前页:%s —— 拒绝执行,画布零改动", label, strings.Join(missing, ","))
	}
	before := groupRebuildSnapshotOf(live)
	// bridge 基线:没有基线就无法把「新增短路」归因给本次移动(fail-closed,
	// 读不到直接拒 —— 没有证明不算过)。
	bridgeBefore, err := ops.bridgeSignatures()
	if err != nil {
		return rep, fmt.Errorf("%s 快照 bridge 基线(没有基线无法归因新增短路,拒绝执行):%w", label, err)
	}
	deleteIDs, err := tidyDeepSweepPlan(memberSet, comps, wires)
	if err != nil {
		// 共享树(触到非成员 pin):删掉它会切断组外电路 —— fail-closed,零 mutation。
		return rep, fmt.Errorf("%s:%w", label, err)
	}
	deleteIDs = dropSheetIDs(uniqueIDs(deleteIDs), comps)

	// netOfSpec:快照里该 pin 的期望网(标注仍断 pin 用,喂 `sch connect`)。
	netOfSpec := map[string]string{}
	for _, c := range conns {
		netOfSpec[strings.ToUpper(c.PinRef)] = c.Net
	}
	annotateRefs := func(refs []string) []string {
		out := make([]string, 0, len(refs))
		for _, r := range refs {
			if net := netOfSpec[strings.ToUpper(r)]; net != "" && !strings.Contains(r, "→") {
				out = append(out, r+"→"+net)
				continue
			}
			out = append(out, r)
		}
		return out
	}
	// pageDeficits:读当前网表,算**全页**偏离 pin(不限移动集合)。空表守卫:
	// netlist 引擎会被坏原语毒死静默返 0 —— 快照非空而当前读回全空时不可信,
	// 按「读失败」处理,绝不据此生成「全页皆断」的假 deficit。
	pageDeficits := func() ([]movePinDeficit, error) {
		cur, lerr := ops.liveNets()
		if lerr != nil {
			return nil, lerr
		}
		if len(cur) == 0 && len(live) > 0 {
			return nil, fmt.Errorf("当前网表读回为空而快照有 %d 张网 —— netlist 引擎疑似被毒死/读到坏帧,本轮不采信", len(live))
		}
		return moveKernelPinDeficits(live, cur), nil
	}

	// 恢复段(共用):按快照把**全页**偏离 pin 拉回,不限移动集合 —— 删桩线触发
	// 的相邻共线导线合并吞的是**第三方** pin(esp32Mini P2:GND 树上 9 个别人的
	// 地脚被灌进 +3V3),只救移动集合 = 「抓到了但救不回」。做法:移动集合快照
	// conns ∪ 全页 deficit 规格,replace=true(灌错网的 pin 先走带回读验证的
	// disconnect 再重连);autoconnect 按**当前几何**重新解析引脚坐标,挪成的在
	// 新位置、没挪成的在原位都能连回;已连对的 pin 幂等跳过,不叠重复 marker。
	// 收尾复读一轮:仍偏离的 pin 连同期望网名结构化列全(REF→期望网,可直接喂
	// `sch connect`),报告从「页面已毁」降级为「N 个 pin 待手工恢复」。
	recoverConns := func(stage string, cause error) error {
		specs := append([]acConnSpec(nil), conns...)
		haveSpec := map[string]bool{}
		for _, c := range specs {
			haveSpec[strings.ToUpper(c.PinRef)] = true
		}
		var manual []movePinDeficit
		if defs, derr := pageDeficits(); derr != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("恢复段读全页网表失败(%v)—— 降级为只按移动集合快照重连", derr))
		} else {
			dspecs, dmanual := moveKernelDeficitSpecs(defs)
			manual = dmanual
			for _, s := range dspecs {
				if !haveSpec[strings.ToUpper(s.PinRef)] {
					haveSpec[strings.ToUpper(s.PinRef)] = true
					specs = append(specs, s)
					netOfSpec[strings.ToUpper(s.PinRef)] = s.Net
				}
			}
		}
		if len(specs) == 0 && len(manual) == 0 {
			return fmt.Errorf("%s %s:%w(涉及成员没有已连引脚且全页网表与快照一致,画布上无断点)", label, stage, cause)
		}
		fmt.Fprintf(stderr, "warn: %s %s失败 —— 立即按快照对全页 %d 个引脚重连(含第三方偏离 pin;器件停在当前位置)\n", label, stage, len(specs))
		succ, failed, rerr := ops.autoconnect(specs, true, defaultAutoconnectRules())
		rep.Recovered = succ
		if rerr != nil && len(succ)+len(failed) == 0 {
			refs := make([]string, 0, len(specs))
			for _, c := range specs {
				refs = append(refs, c.PinRef+"→"+c.Net)
			}
			rep.StillBroken = append(refs, moveKernelFormatDeficits(manual)...)
			return fmt.Errorf("%s %s:%w;恢复重连本身失败(%v),以下引脚待手工恢复(REF→期望网,逐脚 `sch connect`):%s",
				label, stage, cause, rerr, strings.Join(rep.StillBroken, " "))
		}
		// 复读验证:autoconnect 的成败自报不是证明(负载停摆期报失败的写大概率已
		// 落地)。以「autoconnect 自报失败 ∪ 复读仍偏离」为准 —— 复读只能加名单
		// 不能销名单,单次读不足以证明「没断」。
		still := annotateRefs(failed)
		haveStill := map[string]bool{}
		for _, s := range still {
			haveStill[strings.ToUpper(s)] = true
		}
		if defs, derr := pageDeficits(); derr == nil {
			for _, item := range moveKernelFormatDeficits(defs) {
				if !haveStill[strings.ToUpper(item)] {
					haveStill[strings.ToUpper(item)] = true
					still = append(still, item)
				}
			}
		} else {
			rep.Notes = append(rep.Notes, fmt.Sprintf("恢复段复读验证失败(%v)—— 仍断名单仅含 autoconnect 自报失败", derr))
		}
		for _, m := range moveKernelFormatDeficits(manual) {
			if !haveStill[strings.ToUpper(m)] {
				haveStill[strings.ToUpper(m)] = true
				still = append(still, m)
			}
		}
		rep.StillBroken = still
		if len(still) > 0 {
			return fmt.Errorf("%s %s:%w;恢复段已自动重连 %d 成,%d 个 pin 待手工恢复(REF→期望网,逐脚 `sch connect`;标注 disconnect 的先拆):%s",
				label, stage, cause, len(succ), len(still), strings.Join(still, " "))
		}
		return fmt.Errorf("%s %s:%w;恢复段已按快照重连全页 %d/%d 个引脚,复读与快照一致(电气未断,器件停在当前位置)",
			label, stage, cause, len(succ), len(specs))
	}

	// ── 2. 删证 ────────────────────────────────────────────────────────────
	if len(deleteIDs) > 0 {
		deleteRound := func(ids []string) error {
			for i := 0; i < len(ids); i += moveKernelDeleteBatch {
				end := min(i+moveKernelDeleteBatch, len(ids))
				if derr := ops.deleteBatch(ids[i:end]); derr != nil {
					return derr
				}
			}
			return nil
		}
		if derr := deleteRound(deleteIDs); derr != nil {
			// 请求本身失败:前面批次可能已落地,pins 已有断点 → 恢复。
			return rep, recoverConns("删证(清扫旧桩/旗)", derr)
		}
		// 回读证实:删除 API 返回成功不代表真删了(大批量静默 no-op 仍返 true)。
		left, perr := ops.present(deleteIDs)
		if perr != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("清扫回读失败(%v)—— 依赖第 5 步对账兜底", perr))
			fmt.Fprintf(stderr, "  ⚠ 清扫回读失败(%v)—— 若后续对账红,先跑 `sch bridge-check`\n", perr)
		} else if len(left) > 0 {
			// 补删一轮:剩下的通常是首轮被静默丢弃的。
			if derr := deleteRound(left); derr != nil {
				return rep, recoverConns("删证(补删残留)", derr)
			}
			still, serr := ops.present(deleteIDs)
			if serr == nil && len(still) > 0 {
				// 删除撒谎坐实:残留旧旗会挂到新桩线上串网,**绝不带病进入移动步**。
				rep.Partial = true
				return rep, recoverConns("删证",
					fmt.Errorf("清扫后仍残留 %d 个旧桩线/旗(平台静默丢弃删除请求,删除撒谎)—— 不进入移动步,器件未动", len(still)))
			}
			rep.Notes = append(rep.Notes, fmt.Sprintf("补删 %d 个平台首轮静默丢弃的图元", len(left)))
		}
		fmt.Fprintf(stdout, "  清扫:删除 %d 个旧桩线/旗/残段(整树,回读证实)\n", len(deleteIDs))
	}

	// ── 3. 移动 ────────────────────────────────────────────────────────────
	for _, it := range items {
		if !it.HasTarget {
			continue
		}
		m := byDesig[strings.ToUpper(it.Designator)]
		tx, ty := snap5(it.X), snap5(it.Y)
		if len(it.RotCandidates) > 0 {
			resolved := false
			for _, cand := range it.RotCandidates {
				cand := cand
				var pins []layoutPin
				var merr error
				if it.CenterOnPins {
					// 先原地转(旋转后 bbox 未知,pin 中点驱动),再平移对中。
					if merr = ops.modify(m.ID, m.X, m.Y, &cand); merr != nil {
						return rep, recoverConns("移动(转竖)", fmt.Errorf("%s rot %g:%w", it.Designator, cand, merr))
					}
					if pins, merr = ops.settledPins(it.Designator); merr != nil {
						return rep, recoverConns("移动(转竖实测)", merr)
					}
					mx, my := zaaPinMidpoint(pins)
					ddx, ddy := snap5(tx-mx), snap5(ty-my)
					if merr = ops.modify(m.ID, snap5(m.X+ddx), snap5(m.Y+ddy), &cand); merr != nil {
						return rep, recoverConns("移动(转竖平移)", fmt.Errorf("%s:%w", it.Designator, merr))
					}
				} else {
					if merr = ops.modify(m.ID, tx, ty, &cand); merr != nil {
						return rep, recoverConns("移动", fmt.Errorf("%s rot %g:%w", it.Designator, cand, merr))
					}
				}
				if pins, merr = ops.settledPins(it.Designator); merr != nil {
					return rep, recoverConns("移动(settle 实测)", merr)
				}
				okc := true
				if it.VerifyPins != nil {
					if okc, merr = it.VerifyPins(pins); merr != nil {
						return rep, recoverConns("移动(候选消解)", fmt.Errorf("%s:%w", it.Designator, merr))
					}
				}
				if okc {
					resolved = true
					break
				}
			}
			if !resolved {
				return rep, recoverConns("移动", fmt.Errorf("%s 全部 rotation 候选实测不符(符号基向异常)", it.Designator))
			}
			rep.Moved = append(rep.Moved, it.Designator)
			continue
		}
		// 零位移 no-op:不发 mutation(判定坐标 = 落地坐标,都已 snap)。
		if it.Rot == nil && math.Abs(tx-m.X) <= moveKernelPosEps && math.Abs(ty-m.Y) <= moveKernelPosEps {
			rep.Skipped = append(rep.Skipped, it.Designator)
			continue
		}
		if merr := ops.modify(m.ID, tx, ty, it.Rot); merr != nil {
			// 轻读复核:负载停摆期「报失败的写」大概率已落地 —— 盲重试造重复,
			// 盲回退丢移动。轻读当前锚坐标,已在目标 = 假失败,按成功继续。
			ax, ay, okp, lerr := ops.anchorOf(it.Designator)
			if lerr == nil && okp && math.Abs(ax-tx) <= moveKernelPosEps && math.Abs(ay-ty) <= moveKernelPosEps {
				rep.FakeSuccess = append(rep.FakeSuccess, it.Designator)
				rep.Moved = append(rep.Moved, it.Designator)
				rep.Notes = append(rep.Notes, fmt.Sprintf("%s modify 报错(%v)但轻读复核证实已落地(假失败),按成功继续", it.Designator, merr))
				fmt.Fprintf(stderr, "  ⚠ %s modify 报错但轻读复核已在目标位(平台假失败)—— 按成功继续\n", it.Designator)
				continue
			}
			return rep, recoverConns("移动", fmt.Errorf("平移 %s → (%g,%g) 失败:%w", it.Designator, tx, ty, merr))
		}
		rep.Moved = append(rep.Moved, it.Designator)
	}
	if len(rep.Moved) > 0 {
		fmt.Fprintf(stdout, "  移动:%d 件落位(snap 5 网格)\n", len(rep.Moved))
	}

	// ── 3.5 合并早检(轻量增量 spot-check)───────────────────────────────────
	// 删证(第 2 步)删桩线正是触发「相邻共线导线自动合并」的时刻(marker-move
	// 三败定案)—— 合并吞掉的是第三方 pin,不等第 5 步对账,这里就查一次全页网表、
	// 把偏离的**第三方** pin 当场修回(成员 pin 此刻本来就该浮空,第 4 步才重连,
	// 不参与判定)。为什么不严格放在第 2 步里:netlist 族读操作紧贴 modify 会
	// 毒化下一条写(pins-readback-poisons-modify,唯一解是排序);而删证→移动
	// 之间没有任何导线变更(modify 的对象此刻身上零导线),检测保真度完全相同,
	// 所以后置到移动步之后、重连步之前 —— 既避开毒化,又赶在新桩线落地前修复。
	if len(deleteIDs) > 0 {
		if defs, derr := pageDeficits(); derr != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("合并早检读网表失败(%v)—— 交第 5 步对账兜底", derr))
		} else {
			var third []movePinDeficit
			for _, d := range defs {
				desig := strings.ToUpper(strings.SplitN(d.Ref, ":", 2)[0])
				if !memberSet[desig] {
					third = append(third, d)
				}
			}
			if len(third) > 0 {
				items := moveKernelFormatDeficits(third)
				rep.Notes = append(rep.Notes, fmt.Sprintf("合并早检:删证已触发相邻共线导线合并,波及 %d 个第三方 pin(%s)—— 在新桩线落地前修复",
					len(third), strings.Join(items, " ")))
				fmt.Fprintf(stderr, "  ⚠ 合并早检:删证波及 %d 个第三方 pin(%s)—— 按快照 replace 重连\n", len(third), strings.Join(items, " "))
				specs, manual := moveKernelDeficitSpecs(third)
				if len(specs) > 0 {
					if succ, failed, aerr := ops.autoconnect(specs, true, defaultAutoconnectRules()); aerr != nil && len(succ)+len(failed) == 0 {
						rep.Notes = append(rep.Notes, fmt.Sprintf("合并早检修复未跑起来(%v)—— 交第 5 步对账+恢复段兜底", aerr))
					} else if len(failed) > 0 {
						rep.Notes = append(rep.Notes, fmt.Sprintf("合并早检修复 %d 成 %d 败(%s)—— 交第 5 步对账+恢复段兜底",
							len(succ), len(failed), strings.Join(failed, " ")))
					}
				}
				if len(manual) > 0 {
					rep.Notes = append(rep.Notes, fmt.Sprintf("合并早检:%d 个 pin 快照浮空却被灌进网,只能手工拆:%s",
						len(manual), strings.Join(moveKernelFormatDeficits(manual), " ")))
				}
			}
		}
	}

	// ── 4. 重连 ────────────────────────────────────────────────────────────
	covered := map[string]bool{}
	var termFails []string
	for _, it := range items {
		if it.Terms == nil {
			continue
		}
		pins, perr := ops.settledPins(it.Designator)
		if perr != nil {
			return rep, recoverConns("重连(实测引脚)", fmt.Errorf("%s:%w", it.Designator, perr))
		}
		terms, terr := it.Terms(pins)
		if terr != nil {
			return rep, recoverConns("重连(展开计划端子)", fmt.Errorf("%s:%w", it.Designator, terr))
		}
		for _, t := range terms {
			covered[strings.ToUpper(it.Designator+":"+t.Pin)] = true
			px, py, okp := tidyPinCoord(pins, t.Pin)
			if !okp {
				termFails = append(termFails, fmt.Sprintf("%s:%s(实测无此 pin)", it.Designator, t.Pin))
				continue
			}
			cerr := ops.connectPin(px, py, t)
			if cerr != nil {
				// 平台会随机吃掉一个连接:歇口气重试一次,再失败交对账/恢复兜底。
				time.Sleep(opts.RetryDelay)
				cerr = ops.connectPin(px, py, t)
			}
			if cerr != nil {
				termFails = append(termFails, fmt.Sprintf("%s:%s(%v)", it.Designator, t.Pin, cerr))
			}
		}
	}
	// 计划端子没覆盖到的 pin:preserve 策略下**先按移动前的桩几何原样重建**,
	// 剩下真的复现不了的才交 autoconnect 评分。这一步是「刚体平移不撑胖区框」的
	// 本体 —— 挪一件不该顺手把它的短桩换成评分器挑的长桩。
	// 转姿态的件排除在外:桩方向跟着符号转,原方向已经不成立(那类件本来就该由
	// 调用方给显式 Terms)。
	reoriented := map[string]bool{}
	for _, it := range items {
		d := strings.ToUpper(it.Designator)
		switch {
		case len(it.RotCandidates) > 0:
			reoriented[d] = true
		case it.Rot != nil:
			// 「传了 rot」不等于「转了」:zone relayout 会把**现值**原样传下来占位。
			// 真的与现姿态不同才排除 —— 否则整条 placement-first 路径白白丢掉复现。
			cur, known := rotBefore[d]
			if !known || math.Abs(*it.Rot-cur) > 1e-6 {
				reoriented[d] = true
			}
		}
	}
	var rest []acConnSpec
	preservedBy := map[string][]moveConnTerm{} // 位号 → 待原样重建的桩
	preservedN, observedMaxStub := 0, 0.0
	for _, c := range conns {
		ref := strings.ToUpper(c.PinRef)
		if covered[ref] {
			continue
		}
		desig := strings.ToUpper(strings.SplitN(c.PinRef, ":", 2)[0])
		t, hasStub := stubSnap[ref]
		// 调用方给了硬上限(zone-arrange 传规划最长桩)时,**原样重建也要服从它**:
		// 「复现旧几何」在收敛场景下会把老页面横跨半页的长桩搬进新框里,那正是
		// 收敛要消灭的东西。超限的退回 autoconnect(它也被同一个上限夹着)。
		overCap := opts.MaxStub > 0 && t.Offset > opts.MaxStub
		// 网名必须与快照一致才敢原样重建:不一致说明这只 pin 的连接语义已经变了,
		// 复现旧几何等于把它接回旧网(判定与落地必须同一件事)。
		if policy == moveStubPreserve && hasStub && !overCap && !reoriented[desig] && strings.EqualFold(t.Net, c.Net) {
			preservedBy[desig] = append(preservedBy[desig], t)
			preservedN++
			observedMaxStub = maxF(observedMaxStub, t.Offset)
			continue
		}
		rest = append(rest, c)
	}
	if preservedN > 0 {
		desigs := make([]string, 0, len(preservedBy))
		for d := range preservedBy {
			desigs = append(desigs, d)
		}
		sort.Strings(desigs) // 确定性:重建顺序与 map 遍历序无关
		for _, d := range desigs {
			pins, perr := ops.settledPins(d)
			if perr != nil {
				// 读不到实测引脚就复现不了 —— 退回 autoconnect,不硬猜坐标。
				for _, t := range preservedBy[d] {
					rest = append(rest, acConnSpec{PinRef: d + ":" + t.Pin, Kind: bapFlagKind(t.Net), Net: t.Net})
				}
				rep.Notes = append(rep.Notes, fmt.Sprintf("%s 原样重建读引脚失败(%v)—— 该件退回 autoconnect 评分", d, perr))
				continue
			}
			for _, t := range preservedBy[d] {
				px, py, okp := tidyPinCoord(pins, t.Pin)
				if !okp {
					rest = append(rest, acConnSpec{PinRef: d + ":" + t.Pin, Kind: bapFlagKind(t.Net), Net: t.Net})
					continue
				}
				cerr := ops.connectPin(px, py, t)
				if cerr != nil {
					time.Sleep(opts.RetryDelay)
					cerr = ops.connectPin(px, py, t)
				}
				if cerr != nil {
					termFails = append(termFails, fmt.Sprintf("%s:%s(原样重建 %v)", d, t.Pin, cerr))
				}
			}
		}
		fmt.Fprintf(stdout, "  重连:%d 只 pin 按移动前桩线几何原样重建(刚体平移不改桩长)\n", preservedN)
	}
	if len(termFails) > 0 {
		// 端子失败不打断(zone-arrange 首跑教训:中途回滚对着卡死的连接器全数
		// 无效,好的没保住坏的没修好)—— 交给第 5 步对账 + 恢复段兜底。
		rep.Notes = append(rep.Notes, fmt.Sprintf("计划端子重连失败 %d 处(交对账兜底):%s", len(termFails), strings.Join(termFails, ";")))
		fmt.Fprintf(stderr, "  ⚠ 计划端子重连失败 %d 处 —— 交对账修复\n", len(termFails))
	}
	if len(rest) > 0 {
		succ, failed, aerr := ops.autoconnect(rest, false, opts.stubRules(observedMaxStub))
		if aerr != nil && len(succ)+len(failed) == 0 {
			return rep, recoverConns("重连", aerr)
		}
		if len(failed) > 0 {
			rep.Notes = append(rep.Notes, fmt.Sprintf("快照重连失败 %d 处(交对账兜底):%s", len(failed), strings.Join(failed, " ")))
		}
	}

	// ── 5. 对账 ────────────────────────────────────────────────────────────
	// 判据是电气不是坐标:网表逐 pin 与快照一致 + 无新增 bridge(合并短路当场抓)。
	baseline := map[string]bool{}
	for _, b := range bridgeBefore {
		baseline[b] = true
	}
	reconcile := func() (diffs, newBridges []string, after map[string]map[string]bool, err error) {
		after, aerr := ops.liveNets()
		if aerr != nil {
			return nil, nil, nil, fmt.Errorf("读移动后网表(没有证明不算过):%w", aerr)
		}
		diffs = groupRebuildNetDiff(before, groupRebuildSnapshotOf(after))
		bridgeAfter, berr := ops.bridgeSignatures()
		if berr != nil {
			return nil, nil, nil, fmt.Errorf("bridge-check 无法运行(没有证明不算过):%w", berr)
		}
		for _, b := range bridgeAfter {
			if !baseline[b] {
				newBridges = append(newBridges, b)
			}
		}
		return diffs, newBridges, after, nil
	}
	describeRed := func(diffs, newBridges []string) string {
		var parts []string
		if len(newBridges) > 0 {
			parts = append(parts, fmt.Sprintf("%d 个新增 wire-bridge(真短路)%s —— `sch bridge-check` 定位后 `sch prim-delete` 拆桥",
				len(newBridges), strings.Join(newBridges, " ")))
		}
		if len(diffs) > 0 {
			parts = append(parts, fmt.Sprintf("%d 处网表差异:%s —— 按清单逐脚 `sch connect` 补齐", len(diffs), strings.Join(diffs, ";")))
		}
		return strings.Join(parts, ";")
	}
	diffs, newBridges, afterCur, rerr := reconcile()
	if rerr != nil {
		return rep, recoverConns("对账", rerr)
	}
	if len(diffs) == 0 && len(newBridges) == 0 {
		fmt.Fprintf(stdout, "✓ %s 对账:网表逐引脚一致、无新增 bridge(%d 移动 / %d no-op)\n", label, len(rep.Moved), len(rep.Skipped))
		return rep, nil
	}
	// 对账红 → 恢复段(**全页扩权**):把 diffs 解析成逐 pin deficit,凡快照里有
	// 网名、现在断连或网名不符的 pin —— **无论属不属于移动集合** —— 都按快照
	// 网名重连;灌错网的(9 个地脚进 +3V3)走 replace(带回读验证的 disconnect
	// 后重连)。最多两轮:replace 拆合并树可能连累树上无辜 pin(拆树二次波及),
	// 第二轮按复查后的新 deficit 把它们连回。复查(reconcile)绿才算恢复成功;
	// 仍红则把偏离 pin 连同期望网名结构化列全,如实进 StillBroken。
	fmt.Fprintf(stderr, "  ⚠ %s 对账首轮红(%d 差异 / %d 新增 bridge)—— 恢复段按快照全页补连后复查\n", label, len(diffs), len(newBridges))
	var manualLeft []movePinDeficit
	recovered := map[string]bool{}
	for round := 0; round < 2; round++ {
		var defs []movePinDeficit
		if len(afterCur) == 0 && len(live) > 0 {
			// 空表守卫:当前读全空而快照非空 = netlist 引擎疑似被毒死,不据此
			// 生成「全页皆断」的假 deficit;首轮退回移动集合快照,次轮直接停。
			rep.Notes = append(rep.Notes, "恢复段:网表读回为空而快照非空,不采信(仅按移动集合快照重连)")
		} else {
			defs = moveKernelPinDeficits(live, afterCur)
		}
		specs, manual := moveKernelDeficitSpecs(defs)
		manualLeft = manual
		if round == 0 {
			// 首轮并上移动集合快照(幂等跳过已连对的):对账红的成因不止 diff
			// 可见的 pin(bridge-only 红时 deficits 为空,也要补连兜一轮)。
			haveSpec := map[string]bool{}
			for _, s := range specs {
				haveSpec[strings.ToUpper(s.PinRef)] = true
			}
			for _, c := range conns {
				if !haveSpec[strings.ToUpper(c.PinRef)] {
					specs = append(specs, c)
				}
			}
		}
		for _, s := range specs {
			netOfSpec[strings.ToUpper(s.PinRef)] = s.Net
		}
		if len(specs) == 0 {
			break
		}
		succ, _, aerr := ops.autoconnect(specs, true, defaultAutoconnectRules())
		for _, s := range succ {
			recovered[s] = true
		}
		if aerr != nil && len(succ) == 0 && round == 0 {
			rep.NetDiffs, rep.NewBridges = diffs, newBridges
			rep.StillBroken = append(moveKernelFormatDeficits(defs), moveKernelFormatDeficits(manual)...)
			return rep, fmt.Errorf("%s 对账红且恢复重连未跑起来(%v)—— %s", label, aerr, describeRed(diffs, newBridges))
		}
		diffs2, newBridges2, after2, rerr2 := reconcile()
		if rerr2 != nil {
			rep.NetDiffs, rep.NewBridges = diffs, newBridges
			return rep, fmt.Errorf("%s 对账复查失败(%v)—— 首轮:%s", label, rerr2, describeRed(diffs, newBridges))
		}
		diffs, newBridges, afterCur = diffs2, newBridges2, after2
		if len(diffs) == 0 && len(newBridges) == 0 {
			break
		}
	}
	for r := range recovered {
		rep.Recovered = append(rep.Recovered, r)
	}
	sort.Strings(rep.Recovered)
	rep.NetDiffs, rep.NewBridges = diffs, newBridges
	if len(diffs) == 0 && len(newBridges) == 0 {
		rep.Notes = append(rep.Notes, "对账首轮红,恢复段全页补连后达成一致")
		fmt.Fprintf(stdout, "✓ %s 对账:恢复段全页补连后网表逐引脚一致、无新增 bridge\n", label)
		return rep, nil
	}
	// 恢复失败:结构化列全仍偏离的 pin(REF→期望网,可直接喂 `sch connect`),
	// 报告从「页面已毁」降级为「N 个 pin 待手工恢复」。
	var still []movePinDeficit
	if len(afterCur) > 0 || len(live) == 0 {
		still = moveKernelPinDeficits(live, afterCur)
	} else {
		still = manualLeft
	}
	rep.StillBroken = moveKernelFormatDeficits(still)
	msg := describeRed(diffs, newBridges)
	if len(rep.StillBroken) > 0 {
		msg += fmt.Sprintf(";%d 个 pin 待手工恢复(REF→期望网,逐脚 `sch connect`;标注 disconnect 的先拆):%s",
			len(rep.StillBroken), strings.Join(rep.StillBroken, " "))
	}
	return rep, fmt.Errorf("%s 对账不过(判据是电气不是坐标):%s;`sch check` 复核后重试", label, msg)
}

// ── 生产 ops:连接器实现 ─────────────────────────────────────────────────────

type daemonMoveOps struct {
	cfg     *appConfig
	win     string
	docUUID string
	stderr  io.Writer
}

// resolveDoc:与 `doc ls` / ensureActiveDoc 同源(discoverDocs + resolveDoc)
// 判定目标页仍可解析。docUUID 为空(如 destagger 直接操作激活页)时验证窗口
// 仍有激活文档。
func (o *daemonMoveOps) resolveDoc() error {
	docs, activeUUID, _, err := discoverDocs(o.cfg, o.win)
	if err != nil {
		return fmt.Errorf("枚举窗口文档:%w", err)
	}
	if o.docUUID == "" {
		if activeUUID == "" {
			return fmt.Errorf("窗口没有激活文档(`easyeda doc ls` 查看)")
		}
		return nil
	}
	if _, rerr := resolveDoc(docs, o.docUUID); rerr != nil {
		return rerr
	}
	return nil
}

func (o *daemonMoveOps) scene() ([]layoutComp, []schGroupWire, error) {
	res, err := requestAutolayoutAction(o.cfg, "schematic.components.list", o.win,
		map[string]any{"includeBBox": true, "includePins": true}, o.docUUID, "move-kernel 快照")
	if err != nil {
		return nil, nil, err
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return nil, nil, err
	}
	wires, err := fetchSchWirePolylinesStable(o.cfg, o.win, o.docUUID)
	if err != nil {
		return nil, nil, fmt.Errorf("读导线:%w", err)
	}
	return comps, wires, nil
}

func (o *daemonMoveOps) liveNets() (map[string]map[string]bool, error) {
	live, _, err := readLiveNets(o.cfg, o.win)
	return live, err
}

func (o *daemonMoveOps) deleteBatch(ids []string) error {
	_, err := requestAutolayoutAction(o.cfg, "schematic.primitives.delete", o.win,
		map[string]any{"primitiveIds": ids}, o.docUUID, "move-kernel 清扫")
	return err
}

func (o *daemonMoveOps) present(ids []string) ([]string, error) {
	return groupRebuildStillPresent(o.cfg, o.win, o.docUUID, ids)
}

func (o *daemonMoveOps) modify(primitiveID string, x, y float64, rot *float64) error {
	patch := map[string]any{"x": x, "y": y}
	if rot != nil {
		patch["rotation"] = *rot
	}
	_, err := requestAutolayoutAction(o.cfg, "schematic.component.modify", o.win,
		map[string]any{"primitiveId": primitiveID, "patch": patch}, o.docUUID, "move-kernel 落位")
	return err
}

func (o *daemonMoveOps) settledPins(desig string) ([]layoutPin, error) {
	return tidySettledPins(o.cfg, o.win, o.docUUID, desig)
}

func (o *daemonMoveOps) anchorOf(desig string) (float64, float64, bool, error) {
	res, err := requestAutolayoutAction(o.cfg, "schematic.components.list", o.win,
		map[string]any{"includeBBox": false, "includePins": false}, o.docUUID, "move-kernel 轻读复核")
	if err != nil {
		return 0, 0, false, err
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return 0, 0, false, err
	}
	for _, c := range comps {
		if (c.ComponentType == "" || c.ComponentType == schLayoutPartType) &&
			strings.EqualFold(strings.TrimSpace(c.Designator), strings.TrimSpace(desig)) {
			return c.X, c.Y, c.AnchorAvailable, nil
		}
	}
	return 0, 0, false, nil
}

func (o *daemonMoveOps) connectPin(pinX, pinY float64, t moveConnTerm) error {
	payload := map[string]any{
		"pinX": pinX, "pinY": pinY, "kind": t.Kind, "net": t.Net,
		"direction": t.Direction, "rotation": t.Rotation,
	}
	if t.Offset > 0 {
		payload["offset"] = t.Offset
	}
	_, err := requestAutolayoutAction(o.cfg, "schematic.power.connect_pin", o.win, payload, o.docUUID, "move-kernel 重连")
	return err
}

func (o *daemonMoveOps) autoconnect(conns []acConnSpec, replace bool, rules autoconnectRules) ([]string, []string, error) {
	stderr := o.stderr
	if stderr == nil {
		stderr = io.Discard
	}
	// replace=true(恢复段/合并早检):对「已连在别的网」的 pin 走 autoconnect
	// --replace 语义 —— 先 schematic.pin.disconnect(连接器侧带回读验证 +
	// alsoDisconnectedPins 上报)再按快照网名重连;已连对的 pin 幂等跳过。
	rep, err := runAutoconnectOpts(o.cfg, o.win, conns, rules, acRunOpts{Replace: replace}, stderr, stderr)
	return rep.Succeeded, rep.Failed, err
}

func (o *daemonMoveOps) bridgeSignatures() ([]string, error) {
	res, err := requestAutolayoutAction(o.cfg, "schematic.bridgeCheck", o.win, nil, o.docUUID, "move-kernel bridge 基线")
	if err != nil {
		return nil, err
	}
	brep, err := parseBridgeReport(res.Result)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range brep.Trees {
		if !strings.EqualFold(t.Kind, "BRIDGE") {
			continue
		}
		nets := append([]string(nil), t.Nets...)
		sort.Strings(nets)
		out = append(out, "["+strings.Join(nets, ",")+"]")
	}
	sort.Strings(out)
	return out, nil
}
