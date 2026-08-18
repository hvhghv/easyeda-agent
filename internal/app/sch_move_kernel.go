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
//	├ 4. 重连   显式计划端子(connect_pin,梯次桩长等参数由调用方闭包按实测
//	│           pins 计算)+ 其余按快照 autoconnect(评分避让;器件在原位也能连回)
//	└ 5. 对账   网表逐 pin 与快照比对(判据是电气不是坐标)+ bridge 增量检查
//	            (合并短路当场抓);红 → 恢复段补连一轮再复查
//
// 失败语义(#151 部分应用约定):任何一步失败 → 立即进入恢复段(对当前实际
// 位置的全部涉及 pin 按快照重连 —— autoconnect 按当前几何重新解析引脚坐标,
// 挪成没挪成的都能连回),然后返回结构化 moveReport;绝不留「桩线已清、器件
// 没挪、连接全断」的 PARTIAL 尸体。恢复本身失败 → 如实列出仍断 pin。
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

// moveKernelOpts 是内核的运行选项。
type moveKernelOpts struct {
	Label      string        // 日志/报错前缀(调用方命令名)
	RetryDelay time.Duration // connect_pin 单次重试前的等待(0 = 生产默认 2s)
	Stdout     io.Writer
	Stderr     io.Writer
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
	autoconnect(conns []acConnSpec) (succeeded, failed []string, err error)
	// bridgeSignatures 返回当前全部 wire-bridge 的排序签名(如 "[GND,5V]")。
	bridgeSignatures() ([]string, error)
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

	// 恢复段(共用):对当前实际位置的全部涉及 pin 按快照重连。autoconnect 按
	// **当前几何**重新解析引脚坐标,所以挪成的在新位置、没挪成的在原位都能连回;
	// 已连着的 pin 幂等跳过(State=already-connected),不会叠重复 marker。
	recoverConns := func(stage string, cause error) error {
		if len(conns) == 0 {
			return fmt.Errorf("%s %s:%w(涉及成员没有已连引脚,画布上无断点)", label, stage, cause)
		}
		fmt.Fprintf(stderr, "warn: %s %s失败 —— 立即按快照对全部 %d 个引脚重连(器件停在当前位置)\n", label, stage, len(conns))
		succ, failed, rerr := ops.autoconnect(conns)
		rep.Recovered = succ
		rep.StillBroken = failed
		if rerr != nil && len(succ)+len(failed) == 0 {
			refs := make([]string, 0, len(conns))
			for _, c := range conns {
				refs = append(refs, c.PinRef)
			}
			rep.StillBroken = refs
			return fmt.Errorf("%s %s:%w;恢复重连本身失败(%v),以下引脚仍断开,逐脚 `sch connect` 补:%s",
				label, stage, cause, rerr, strings.Join(refs, " "))
		}
		if len(failed) > 0 {
			return fmt.Errorf("%s %s:%w;恢复段已自动重连 %d 成 %d 败,仍断引脚(逐脚 `sch connect` 补):%s",
				label, stage, cause, len(succ), len(failed), strings.Join(failed, " "))
		}
		return fmt.Errorf("%s %s:%w;恢复段已按快照重连 %d/%d 个引脚(电气未断,器件停在当前位置)",
			label, stage, cause, len(succ), len(conns))
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
	if len(termFails) > 0 {
		// 端子失败不打断(zone-arrange 首跑教训:中途回滚对着卡死的连接器全数
		// 无效,好的没保住坏的没修好)—— 交给第 5 步对账 + 恢复段兜底。
		rep.Notes = append(rep.Notes, fmt.Sprintf("计划端子重连失败 %d 处(交对账兜底):%s", len(termFails), strings.Join(termFails, ";")))
		fmt.Fprintf(stderr, "  ⚠ 计划端子重连失败 %d 处 —— 交对账修复\n", len(termFails))
	}
	var rest []acConnSpec
	for _, c := range conns {
		if !covered[strings.ToUpper(c.PinRef)] {
			rest = append(rest, c)
		}
	}
	if len(rest) > 0 {
		succ, failed, aerr := ops.autoconnect(rest)
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
	reconcile := func() (diffs, newBridges []string, err error) {
		after, aerr := ops.liveNets()
		if aerr != nil {
			return nil, nil, fmt.Errorf("读移动后网表(没有证明不算过):%w", aerr)
		}
		diffs = groupRebuildNetDiff(before, groupRebuildSnapshotOf(after))
		bridgeAfter, berr := ops.bridgeSignatures()
		if berr != nil {
			return nil, nil, fmt.Errorf("bridge-check 无法运行(没有证明不算过):%w", berr)
		}
		for _, b := range bridgeAfter {
			if !baseline[b] {
				newBridges = append(newBridges, b)
			}
		}
		return diffs, newBridges, nil
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
	diffs, newBridges, rerr := reconcile()
	if rerr != nil {
		return rep, recoverConns("对账", rerr)
	}
	if len(diffs) == 0 && len(newBridges) == 0 {
		fmt.Fprintf(stdout, "✓ %s 对账:网表逐引脚一致、无新增 bridge(%d 移动 / %d no-op)\n", label, len(rep.Moved), len(rep.Skipped))
		return rep, nil
	}
	// 对账红 → 恢复段补连一轮(autoconnect 对已连 pin 幂等跳过,断点按当前几何
	// 连回)→ 复查一轮后如实上报。
	fmt.Fprintf(stderr, "  ⚠ %s 对账首轮红(%d 差异 / %d 新增 bridge)—— 恢复段按快照补连后复查\n", label, len(diffs), len(newBridges))
	succ, failed, aerr := ops.autoconnect(conns)
	rep.Recovered = succ
	rep.StillBroken = failed
	if aerr != nil && len(succ)+len(failed) == 0 {
		rep.NetDiffs, rep.NewBridges = diffs, newBridges
		return rep, fmt.Errorf("%s 对账红且恢复重连未跑起来(%v)—— %s", label, aerr, describeRed(diffs, newBridges))
	}
	diffs2, newBridges2, rerr2 := reconcile()
	if rerr2 != nil {
		rep.NetDiffs, rep.NewBridges = diffs, newBridges
		return rep, fmt.Errorf("%s 对账复查失败(%v)—— 首轮:%s", label, rerr2, describeRed(diffs, newBridges))
	}
	rep.NetDiffs, rep.NewBridges = diffs2, newBridges2
	if len(diffs2) == 0 && len(newBridges2) == 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("对账首轮红(%d 差异 / %d bridge),恢复段补连后达成一致", len(diffs), len(newBridges)))
		fmt.Fprintf(stdout, "✓ %s 对账:恢复段补连后网表逐引脚一致、无新增 bridge\n", label)
		return rep, nil
	}
	return rep, fmt.Errorf("%s 对账不过(判据是电气不是坐标):%s;`sch check` 复核后重试", label, describeRed(diffs2, newBridges2))
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

func (o *daemonMoveOps) autoconnect(conns []acConnSpec) ([]string, []string, error) {
	stderr := o.stderr
	if stderr == nil {
		stderr = io.Discard
	}
	rep, err := runAutoconnectOpts(o.cfg, o.win, conns, defaultAutoconnectRules(), acRunOpts{}, stderr, stderr)
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
