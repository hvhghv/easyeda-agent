package app

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// ── sch destagger 落地侧(issue #171)────────────────────────────────────────
//
// 规划在 cmd_sch_destagger.go(纯函数)。这里只负责三件事:拉数据 → 按计划做
// disconnect+connect 手术 → **用真实 `sch check` 复验,电气项一恶化就整批回滚**。
//
// 为什么复验必须用真机 check 而不是自己算:规划器的 bbox 是**预测**(平台不给
// "某方向下的 bbox"),而真正的判据是平台渲染出来的几何 + 连接器重建的网表。
// 2026-08-12 那次手动修复正是栽在没复验——改到第三轮才发现中途引入了一条
// multi-net-wire 短路(两支旗的旧桩线端点重叠,共用了一条线树)。

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

// destaggerAppliedMove 记一次已落地的搬迁,供回滚用。
type destaggerAppliedMove struct {
	Plan     destaggerMove
	NewFlag  string // connect_pin 回的 flagPrimitiveId(回滚时删它)
	NewWire  string
	Rollback bool // 回滚是否成功
}

// destaggerRunReport 是命令的 JSON 输出。
type destaggerRunReport struct {
	Applied        bool                 `json:"applied"`
	Rounds         int                  `json:"rounds"`
	Plan           destaggerPlan        `json:"plan"`
	Moved          []destaggerMove      `json:"moved,omitempty"`
	Before         destaggerElectrical  `json:"before"`
	After          *destaggerElectrical `json:"after,omitempty"`
	Regressions    []string             `json:"regressions,omitempty"`
	RolledBack     bool                 `json:"rolledBack"`
	OverlapsBefore int                  `json:"overlapsBefore"`
	OverlapsAfter  int                  `json:"overlapsAfter"`
}

// runSchDestagger 是命令主体。单页作用域:桩线只能从激活页读(--all-pages 系
// 列的已知边界),跨页整理请逐页切 `doc switch` 后各跑一次。
func runSchDestagger(cfg *appConfig, window string, apply bool, maxRounds int, eps float64, asJSON bool, stdout, stderr io.Writer) error {
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
		fmt.Fprintf(stderr, "round %d: %s\n", round, destaggerPlanSummary(plan))

		applied, aerr := applyDestaggerMoves(cfg, window, plan.Moves, stderr)
		rep.Moved = append(rep.Moved, movesOf(applied)...)
		if aerr != nil {
			rollbackDestagger(cfg, window, applied, stderr)
			rep.RolledBack = true
			_ = emitDestaggerJSON(stdout, asJSON, rep)
			return fmt.Errorf("搬迁中断,已回滚本轮: %w", aerr)
		}

		compsAfter, _, ferr := fetchDestaggerGeometry(cfg, window)
		if ferr != nil {
			return ferr
		}
		after, cerr := fetchDestaggerElectrical(cfg, window, compsAfter, eps)
		if cerr != nil {
			rollbackDestagger(cfg, window, applied, stderr)
			rep.RolledBack = true
			_ = emitDestaggerJSON(stdout, asJSON, rep)
			return fmt.Errorf("复验失败(无法确认电气未被改坏),已回滚本轮: %w", cerr)
		}
		if regs := before.regressions(after); len(regs) > 0 {
			rollbackDestagger(cfg, window, applied, stderr)
			rep.RolledBack = true
			rep.Regressions = regs
			rep.After = &after
			_ = emitDestaggerJSON(stdout, asJSON, rep)
			return fmt.Errorf("电气项恶化(%v),已回滚本轮 %d 个搬迁 —— 页面回到动手前状态", regs, len(applied))
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

// fetchDestaggerGeometry 拉一次判定所需的全部几何:带 bbox 的图元表 + 线。
func fetchDestaggerGeometry(cfg *appConfig, window string) ([]layoutComp, []schGroupWire, error) {
	res, err := requestAction(cfg, "schematic.components.list", window, map[string]any{"includeBBox": true})
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

// applyDestaggerMoves 逐个执行 disconnect → connect_pin。任何一步失败即返回
// 已完成的部分,由调用方回滚(部分应用绝不静默留在画布上)。
func applyDestaggerMoves(cfg *appConfig, window string, moves []destaggerMove, stderr io.Writer) ([]destaggerAppliedMove, error) {
	var applied []destaggerAppliedMove
	for _, m := range moves {
		if _, err := requestAction(cfg, "schematic.pin.disconnect", window, map[string]any{
			"flagPrimitiveId": m.FlagID,
		}); err != nil {
			return applied, fmt.Errorf("拆除 %s(%s)失败: %w", m.FlagID, m.Net, err)
		}
		res, err := requestAction(cfg, "schematic.power.connect_pin", window, map[string]any{
			"pinX":      m.HostX,
			"pinY":      m.HostY,
			"kind":      m.Kind,
			"net":       m.Net,
			"direction": m.ToDir,
			"offset":    m.ToOffset,
		})
		if err != nil {
			// 旧的已删、新的没建 —— 这个 marker 现在是缺的。交给回滚补回原位。
			applied = append(applied, destaggerAppliedMove{Plan: m})
			return applied, fmt.Errorf("在 %s 新位置重连 %s 失败: %w", m.Net, m.ToDir, err)
		}
		a := destaggerAppliedMove{Plan: m}
		a.NewFlag, _ = res.Result["flagPrimitiveId"].(string)
		a.NewWire, _ = res.Result["wirePrimitiveId"].(string)
		applied = append(applied, a)
		fmt.Fprintf(stderr, "  %s(%s) %s→%s offset %.0f→%.0f\n",
			m.Net, m.ComponentType, m.FromDir, m.ToDir, m.FromOffset, m.ToOffset)
	}
	return applied, nil
}

// rollbackDestagger 逆序把每个搬迁还原:删掉新旗+新桩,再按**原**方向/桩长
// 重连。宿主端从未动过,所以还原即回到动手前的拓扑与几何。
func rollbackDestagger(cfg *appConfig, window string, applied []destaggerAppliedMove, stderr io.Writer) {
	for i := len(applied) - 1; i >= 0; i-- {
		a := applied[i]
		if a.NewFlag != "" {
			if _, err := requestAction(cfg, "schematic.pin.disconnect", window, map[string]any{
				"flagPrimitiveId": a.NewFlag,
			}); err != nil {
				fmt.Fprintf(stderr, "  回滚:拆除新建 %s 失败(需人工检查): %v\n", a.NewFlag, err)
			}
		}
		if _, err := requestAction(cfg, "schematic.power.connect_pin", window, map[string]any{
			"pinX":      a.Plan.HostX,
			"pinY":      a.Plan.HostY,
			"kind":      a.Plan.Kind,
			"net":       a.Plan.Net,
			"direction": a.Plan.FromDir,
			"offset":    a.Plan.FromOffset,
		}); err != nil {
			fmt.Fprintf(stderr, "  回滚:还原 %s 到 %s/%.0f 失败(需人工检查): %v\n",
				a.Plan.Net, a.Plan.FromDir, a.Plan.FromOffset, err)
			continue
		}
		fmt.Fprintf(stderr, "  回滚:%s 还原到 %s/%.0f\n", a.Plan.Net, a.Plan.FromDir, a.Plan.FromOffset)
	}
}

func movesOf(applied []destaggerAppliedMove) []destaggerMove {
	out := make([]destaggerMove, 0, len(applied))
	for _, a := range applied {
		if a.NewFlag != "" {
			out = append(out, a.Plan)
		}
	}
	return out
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
	var maxRounds int
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
  2. **带桩线一起挪**:disconnect(旗+桩一起删)→ connect_pin(按新方向/桩长重
     拉),宿主端(pin 侧)坐标一字不动,电气拓扑天然不变;
  3. 桩长候选是**量出来的**(跟着该旗文字带尺寸递增)并吸附 5 单位连接网格,
     不是拍脑袋常量;方向按「电上地下」偏好序分配,rotation 走与
     reversed-net-flag 判据同一张真值表;
  4. --apply 每轮落地后自动跑真实 ` + "`sch check`" + ` 复验:floating pin /
     dangling wire / net-marker mismatch / multi-net-wire 等电气项**任何一项
     变差就整批回滚**,页面回到动手前状态并非零退出。

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
			return runSchDestagger(cfg, *window, apply, maxRounds, eps, asJSON, stdout, stderr)
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "落地搬迁(默认只算不动);每轮自动 sch check 复验,电气项恶化即整批回滚")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只算不动(默认行为,显式写出便于脚本自述)")
	c.Flags().BoolVar(&asJSON, "json", false, "输出结构化计划/结果(含每个 skip 的原因)")
	c.Flags().IntVar(&maxRounds, "max-rounds", 1, "最多迭代几轮(每轮重新拉几何重新规划;marker-overlap 归零即提前收敛)")
	c.Flags().Float64Var(&eps, "overlap-eps", schMarkerOverlapEps, "重叠判定阈值,与 sch check 同义(小于它的边缘擦碰不算)")
	return c
}
