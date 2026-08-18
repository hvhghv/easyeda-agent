package app

import (
	"encoding/json"
	"fmt"
	"io"
	"math"

	"github.com/spf13/cobra"
)

// ── sch destagger 落地侧(issue #171 → ADR-0004 解禁)───────────────────────
//
// 规划在 cmd_sch_destagger.go(纯函数)。执行只准调 ADR-0004 单一安全 move
// 内核(schMoveKernel):宿主器件不动(HasTarget=false),内核把宿主的桩线/旗
// **整树删净(删证回读)**后按显式端子(新方向/桩长)+ 快照 autoconnect 重建,
// 再做网表逐 pin 对账 + bridge 增量检查,失败自动恢复。
//
// 它当年三次三败、被禁用的根因正是**没有安全执行层**:逐根 disconnect 删桩线
// 会让 EasyEDA 把相邻共线导线自动合并 → 串网,新桩线被邻居吞掉,连回滚都拆
// 不动(marker-move-breaks-on-wire-merge 定案)。整树删净 → 器件身上没有任何
// 导线 → 重建零合并风险,这正是内核的第 2/3 步;合并短路即便发生也会被内核
// 对账的 bridge 增量当场抓住。
//
// 复验仍用真实 `sch check` 电气快照(规划器的 bbox 是预测,真正判据是平台
// 渲染的几何+网表):电气项恶化 → 用内核按**原**方向/桩长再跑一遍还原,如实
// 上报;2026-08-12 那次手动修复正是栽在没复验。

// destaggerElectrical 是复验用的**电气项**快照。几何项(marker-overlap 等)故意
// 不在内:那正是本命令要改的东西,把它算进"恶化"会自锁。
type destaggerElectrical struct {
	FloatingPins        int `json:"floatingPins"`
	GeomNetMismatches   int `json:"geomNetMismatches"`
	NetMarkerMismatches int `json:"netMarkerMismatches"`
	MultiNetWires       int `json:"multiNetWires"`
	WireCrossings       int `json:"wireCrossings"`
	WireOverPins        int `json:"wireOverPins"`
	ZeroLengthWires     int `json:"zeroLengthWires"`
	DanglingWires       int `json:"danglingWires"`
	MarkerOverlaps      int `json:"markerOverlaps"` // 记录用(判"降没降"),不参与恶化判定
}

func electricalOf(s checkSummary) destaggerElectrical {
	return destaggerElectrical{
		FloatingPins:        s.FloatingPins,
		GeomNetMismatches:   s.GeomNetMismatches,
		NetMarkerMismatches: s.NetMarkerMismatches,
		MultiNetWires:       s.MultiNetWires,
		WireCrossings:       s.WireCrossings,
		WireOverPins:        s.WireOverPins,
		ZeroLengthWires:     s.ZeroLengthWires,
		DanglingWires:       s.DanglingWires,
		MarkerOverlaps:      s.MarkerOverlaps,
	}
}

// regressions 列出 after 相对 before **变差**的电气项。任何一项非空 = 回滚。
// 判据是"不许变差"而非"必须全 0":有些板进来时就带着已知 floating pin(未
// NC 标的 MCU IO 是正常的),要求全 0 会让本命令在真实板上永远不可用。
func (before destaggerElectrical) regressions(after destaggerElectrical) []string {
	var out []string
	cmp := func(name string, b, a int) {
		if a > b {
			out = append(out, fmt.Sprintf("%s %d→%d", name, b, a))
		}
	}
	cmp("floatingPins", before.FloatingPins, after.FloatingPins)
	cmp("geomNetMismatches", before.GeomNetMismatches, after.GeomNetMismatches)
	cmp("netMarkerMismatches", before.NetMarkerMismatches, after.NetMarkerMismatches)
	cmp("multiNetWires", before.MultiNetWires, after.MultiNetWires)
	cmp("wireCrossings", before.WireCrossings, after.WireCrossings)
	cmp("wireOverPins", before.WireOverPins, after.WireOverPins)
	cmp("zeroLengthWires", before.ZeroLengthWires, after.ZeroLengthWires)
	cmp("danglingWires", before.DanglingWires, after.DanglingWires)
	return out
}

// diff 列出两份电气快照**任何方向**的差异。regressions 只管"变差"(落地判据),
// 复原判据更严:回滚后必须与动手前**一模一样**,变好了同样说明页面被改动过。
func (before destaggerElectrical) diff(after destaggerElectrical) []string {
	var out []string
	cmp := func(name string, b, a int) {
		if a != b {
			out = append(out, fmt.Sprintf("%s %d→%d", name, b, a))
		}
	}
	cmp("floatingPins", before.FloatingPins, after.FloatingPins)
	cmp("geomNetMismatches", before.GeomNetMismatches, after.GeomNetMismatches)
	cmp("netMarkerMismatches", before.NetMarkerMismatches, after.NetMarkerMismatches)
	cmp("multiNetWires", before.MultiNetWires, after.MultiNetWires)
	cmp("wireCrossings", before.WireCrossings, after.WireCrossings)
	cmp("wireOverPins", before.WireOverPins, after.WireOverPins)
	cmp("zeroLengthWires", before.ZeroLengthWires, after.ZeroLengthWires)
	cmp("danglingWires", before.DanglingWires, after.DanglingWires)
	cmp("markerOverlaps", before.MarkerOverlaps, after.MarkerOverlaps)
	return out
}

// fetchDestaggerElectrical 跑一次连接器 schematic.check 取电气快照,并补上
// Go 侧的 marker-overlap 计数(几何项连接器看不见)。
func fetchDestaggerElectrical(cfg *appConfig, window string, comps []layoutComp, eps float64) (destaggerElectrical, error) {
	res, err := requestAction(cfg, "schematic.check", window, map[string]any{})
	if err != nil {
		return destaggerElectrical{}, err
	}
	rep, perr := parseCheckReport(res.Result)
	if perr != nil {
		return destaggerElectrical{}, perr
	}
	e := electricalOf(rep.Summary)
	e.MarkerOverlaps = len(markerOverlapFindings(comps, eps))
	return e, nil
}

// destaggerRunReport 是命令的 JSON 输出。
type destaggerRunReport struct {
	Applied     bool                 `json:"applied"`
	Rounds      int                  `json:"rounds"`
	Plan        destaggerPlan        `json:"plan"`
	Moved       []destaggerMove      `json:"moved,omitempty"`
	Before      destaggerElectrical  `json:"before"`
	After       *destaggerElectrical `json:"after,omitempty"`
	Regressions []string             `json:"regressions,omitempty"`
	RolledBack  bool                 `json:"rolledBack"`
	// RollbackSurvivors 非空 = PARTIAL STATE:回滚没能把页面还原,残留的新建旗 id
	// 与/或对不上的电气项都列在这里,必须人工收拾。
	RollbackSurvivors []string `json:"rollbackSurvivors,omitempty"`
	OverlapsBefore    int      `json:"overlapsBefore"`
	OverlapsAfter     int      `json:"overlapsAfter"`
}

// runSchDestagger 是命令主体。单页作用域:桩线只能从激活页读(--all-pages 系
// 列的已知边界),跨页整理请逐页切 `doc switch` 后各跑一次。
func runSchDestagger(cfg *appConfig, window string, apply bool, maxRounds, maxMoves int, eps float64, asJSON bool, stdout, stderr io.Writer) error {
	rep := destaggerRunReport{Applied: apply}

	comps, wires, err := fetchDestaggerGeometry(cfg, window)
	if err != nil {
		return err
	}
	plan := planDestagger(comps, wires, eps)
	rep.Plan = plan
	rep.OverlapsBefore = plan.OverlapsBefore

	if !apply || len(plan.Moves) == 0 {
		if !asJSON {
			fmt.Fprintf(stdout, "%s\n", destaggerPlanSummary(plan))
			renderDestaggerPlan(stdout, plan)
			if len(plan.Moves) > 0 {
				fmt.Fprintf(stdout, "\n只算不动 —— 加 --apply 落地(每轮自动 sch check 复验,电气项恶化即整批回滚)\n")
			}
		}
		return emitDestaggerJSON(stdout, asJSON, rep)
	}

	before, err := fetchDestaggerElectrical(cfg, window, comps, eps)
	if err != nil {
		return fmt.Errorf("电气基线读取失败(没有基线就没法判断改坏没改坏,拒绝落地): %w", err)
	}
	rep.Before = before

	for round := 1; round <= maxRounds; round++ {
		if round > 1 {
			comps, wires, err = fetchDestaggerGeometry(cfg, window)
			if err != nil {
				return err
			}
			plan = planDestagger(comps, wires, eps)
			if len(plan.Moves) == 0 {
				break
			}
		}
		rep.Rounds = round
		// **一轮只落地 maxMoves 个搬迁(默认 1)**:每轮都在最新几何上重新规划,
		// 同批后续搬迁不会拿过时快照算候选。要整批走,显式 --max-moves 0
		// (内核的删证/对账/恢复对整批同样生效)。
		batch := plan.Moves
		if maxMoves > 0 && len(batch) > maxMoves {
			batch = batch[:maxMoves]
		}
		fmt.Fprintf(stderr, "round %d: %s(本轮落地 %d 个)\n", round, destaggerPlanSummary(plan), len(batch))

		// 执行只准调内核(ADR-0004 解禁前提):宿主不动,整树删净后按新方向/
		// 桩长显式重建,宿主其余 pin 按快照 autoconnect 连回,对账兜底。
		if kerr := destaggerKernelRound(cfg, window, comps, batch, false, stdout, stderr); kerr != nil {
			rep.Regressions = append(rep.Regressions, kerr.Error())
			_ = emitDestaggerJSON(stdout, asJSON, rep)
			return fmt.Errorf("destagger 内核执行失败(内核已按快照自动恢复,详见错误):%w", kerr)
		}
		rep.Moved = append(rep.Moved, batch...)

		compsAfter, _, ferr := fetchDestaggerGeometry(cfg, window)
		if ferr != nil {
			return ferr
		}
		after, cerr := fetchDestaggerElectrical(cfg, window, compsAfter, eps)
		if cerr != nil {
			_ = emitDestaggerJSON(stdout, asJSON, rep)
			return fmt.Errorf("复验失败(无法确认电气未被改坏;内核对账已过,几何复验请手工 `sch check`): %w", cerr)
		}
		if regs := before.regressions(after); len(regs) > 0 {
			// 电气项恶化:用内核按**原**方向/桩长再跑一遍还原(同一条安全路径,
			// 不是当年拆不动的逐根 disconnect)。
			rep.Regressions = regs
			rep.After = &after
			rep.RolledBack = true
			if rerr := destaggerKernelRound(cfg, window, compsAfter, batch, true, stdout, stderr); rerr != nil {
				rep.RollbackSurvivors = append(rep.RollbackSurvivors, rerr.Error())
				_ = emitDestaggerJSON(stdout, asJSON, rep)
				return fmt.Errorf("电气项恶化(%v)且还原也失败 —— PARTIAL STATE,`sch check`/`sch bridge-check` 人工收拾:%w", regs, rerr)
			}
			_ = emitDestaggerJSON(stdout, asJSON, rep)
			return fmt.Errorf("电气项恶化(%v),已用内核按原方向/桩长还原本轮 %d 个搬迁(内核对账绿)", regs, len(batch))
		}
		rep.After = &after
		rep.OverlapsAfter = after.MarkerOverlaps
		before = after
		if after.MarkerOverlaps == 0 {
			break
		}
	}

	if !asJSON {
		fmt.Fprintf(stdout, "已搬迁 %d 个 marker(%d 轮);marker-overlap %d → %d,电气项无恶化\n",
			len(rep.Moved), rep.Rounds, rep.OverlapsBefore, rep.OverlapsAfter)
		if len(rep.Plan.Skips) > 0 {
			fmt.Fprintf(stdout, "跳过 %d 个(见 --json 的 skips:not-a-stub/stub-too-long/diagonal-stub/no-free-slot)\n",
				len(rep.Plan.Skips))
		}
	}
	return emitDestaggerJSON(stdout, asJSON, rep)
}

// destaggerKernelRound 把一批 marker 搬迁折成**一次内核调用**(ADR-0004):
// 按宿主件归组,宿主 HasTarget=false(器件在原位也能连回是内核契约),被搬
// marker 用显式端子重建(restore=false 用新方向/桩长,true 用原方向/桩长 ——
// 还原走同一条安全路径,不是当年拆不动的逐根 disconnect),宿主其余被清扫的
// pin 由内核按快照 autoconnect 连回,网表逐 pin 对账 + bridge 增量兜底。
func destaggerKernelRound(cfg *appConfig, window string, comps []layoutComp, batch []destaggerMove, restore bool, stdout, stderr io.Writer) error {
	type hostTerm struct {
		pin string
		m   destaggerMove
	}
	byHost := map[string][]hostTerm{}
	var order []string
	for _, m := range batch {
		desig, pin, ok := destaggerHostPin(comps, m.HostX, m.HostY)
		if !ok {
			return fmt.Errorf("找不到 (%g,%g) 上的宿主 pin(%s %s)—— 场景已变,重跑规划", m.HostX, m.HostY, m.Net, m.ComponentType)
		}
		if _, seen := byHost[desig]; !seen {
			order = append(order, desig)
		}
		byHost[desig] = append(byHost[desig], hostTerm{pin: pin, m: m})
	}
	items := make([]moveItem, 0, len(order))
	for _, d := range order {
		terms := byHost[d]
		items = append(items, moveItem{
			Designator: d, // HasTarget=false:宿主一动不动,只重排它的 marker
			Terms: func(pins []layoutPin) ([]moveConnTerm, error) {
				out := make([]moveConnTerm, 0, len(terms))
				for _, ht := range terms {
					dir, off := ht.m.ToDir, ht.m.ToOffset
					if restore {
						dir, off = ht.m.FromDir, ht.m.FromOffset
					}
					rot, rerr := tidyLabelRotation(ht.m.Kind, dir)
					if rerr != nil {
						return nil, fmt.Errorf("%s(%s)方向 %s 无 rotation 校准:%w", ht.m.Net, ht.m.Kind, dir, rerr)
					}
					out = append(out, moveConnTerm{Pin: ht.pin, Kind: ht.m.Kind, Net: ht.m.Net,
						Direction: dir, Rotation: rot, Offset: off})
				}
				return out, nil
			},
		})
	}
	label := "destagger"
	if restore {
		label = "destagger-restore"
	}
	_, err := schMoveKernel(cfg, window, "", items, moveKernelOpts{Label: label, Stdout: stdout, Stderr: stderr})
	return err
}

// destaggerHostPin 按坐标找 (x,y) 上的宿主器件 pin(位号 + pin 号)。
func destaggerHostPin(comps []layoutComp, x, y float64) (desig, pin string, ok bool) {
	for _, c := range comps {
		if c.ComponentType != "" && c.ComponentType != schLayoutPartType {
			continue
		}
		for _, p := range c.Pins {
			if math.Abs(p.X-x) <= schGroupEps && math.Abs(p.Y-y) <= schGroupEps {
				return c.Designator, p.Number, true
			}
		}
	}
	return "", "", false
}

// fetchDestaggerGeometry 拉一次判定所需的全部几何:带 bbox+pins 的图元表 + 线
// (pins 供宿主 pin 反查,内核端子重建要按位号:pin 号点名)。
func fetchDestaggerGeometry(cfg *appConfig, window string) ([]layoutComp, []schGroupWire, error) {
	res, err := requestAction(cfg, "schematic.components.list", window, map[string]any{"includeBBox": true, "includePins": true})
	if err != nil {
		return nil, nil, fmt.Errorf("components.list 失败: %w", err)
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return nil, nil, perr
	}
	wires, werr := fetchSchWirePolylinesStable(cfg, window, "")
	if werr != nil {
		return nil, nil, fmt.Errorf("导线读取失败(没有桩线几何就无法安全搬迁): %w", werr)
	}
	return comps, wires, nil
}

func renderDestaggerPlan(w io.Writer, p destaggerPlan) {
	for _, m := range p.Moves {
		fmt.Fprintf(w, "  %-10s %-8s %s/%.0f → %s/%.0f  (解开与 %v 的重叠)\n",
			m.Net, m.ComponentType, m.FromDir, m.FromOffset, m.ToDir, m.ToOffset, m.ClearedWith)
	}
	for _, s := range p.Skips {
		fmt.Fprintf(w, "  skip %-10s %-8s %s\n", s.Net, s.ComponentType, s.Reason)
	}
}

func emitDestaggerJSON(stdout io.Writer, asJSON bool, rep destaggerRunReport) error {
	if !asJSON {
		return nil
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	_, _ = stdout.Write(b)
	fmt.Fprintln(stdout)
	return nil
}

// newSchDestaggerCommand builds `sch destagger` — issue #171 的修复侧。
func newSchDestaggerCommand(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var apply, dryRun, asJSON bool
	var maxRounds, maxMoves int
	var eps float64
	c := &cobra.Command{
		Use:   "destagger",
		Short: "批量消 marker-overlap:换方向/桩长并带桩线一起挪(默认 dry-run;--apply 执行)",
		Long: `安全批量 de-stagger(issue #171):把 ` + "`sch check`" + ` 报的 marker-overlap
(netflag/netport 之间、与器件之间的纯视觉重叠)一次性收拾掉。

` + "`sch check`" + ` 早有检测、一直没有修:直接 ` + "`sch modify`" + ` 挪标识坐标会把它
从导线端点上挪脱 → 断网。本命令的安全性来自四条:

  1. 只搬**两点直线短桩**上的 marker;挂在多段折线/网络主干上的一律跳过
     (not-a-stub / stub-too-long / diagonal-stub,每个跳过都带原因);
  2. **执行走 ADR-0004 单一安全 move 内核**:宿主器件一动不动,内核把它的
     桩线/旗整树删净(分批+回读证实,防平台删除撒谎)后按新方向/桩长重建,
     宿主其余 pin 按快照重连 —— 逐根删桩触发的相邻导线自动合并串网(当年
     三次三败禁用本命令的根因)在整树语义下不存在;
  3. 桩长候选是**量出来的**(跟着该旗文字带尺寸递增)并吸附 5 单位连接网格,
     不是拍脑袋常量;方向按「电上地下」偏好序分配,rotation 走与
     reversed-net-flag 判据同一张真值表;
  4. 内核每轮对账(网表逐 pin 与快照一致 + bridge 增量为零),外加真实
     ` + "`sch check`" + ` 复验:电气项**任何一项变差**就用内核按原方向/桩长
     还原本轮,非零退出;还原结果如实上报,绝不无条件宣称"页面已复原"。

挤不下时**宁可不动**(记 no-free-slot),不硬塞一个还撞的位置。
单页作用域(桩线只能从激活页读)——跨页请逐页 ` + "`doc switch`" + ` 后各跑一次。`,
		Args: cobra.NoArgs,
		Example: `  easyeda sch destagger                    # 只算不动(dry-run)
  easyeda sch destagger --json
  easyeda sch destagger --apply            # 落地 + 复验 + 恶化则回滚
  easyeda sch destagger --apply --max-rounds 3`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if apply && dryRun {
				return fmt.Errorf("--dry-run and --apply are mutually exclusive")
			}
			if maxRounds < 1 {
				return fmt.Errorf("--max-rounds must be ≥ 1")
			}
			return runSchDestagger(cfg, *window, apply, maxRounds, maxMoves, eps, asJSON, stdout, stderr)
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "落地搬迁(默认只算不动);每轮自动 sch check 复验,电气项恶化即整批回滚")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只算不动(默认行为,显式写出便于脚本自述)")
	c.Flags().BoolVar(&asJSON, "json", false, "输出结构化计划/结果(含每个 skip 的原因)")
	c.Flags().IntVar(&maxMoves, "max-moves", 1, "每轮最多落地几个搬迁(默认 1 —— 逐个落地+逐个复验最安全;EasyEDA 的导线自动合并会让同批后续搬迁的规划过时,实测整批落地会撞出 multi-net-wire 且回滚不干净)。0 = 不限,冒险整批走")
	c.Flags().IntVar(&maxRounds, "max-rounds", 1, "最多迭代几轮(每轮重新拉几何重新规划;marker-overlap 归零即提前收敛)")
	c.Flags().Float64Var(&eps, "overlap-eps", schMarkerOverlapEps, "重叠判定阈值,与 sch check 同义(小于它的边缘擦碰不算)")
	return c
}
