package app

// cmd_pcb_refine.go — `easyeda pcb refine`：打分驱动的精修环（#167 ACHIEVE 层）。
//
// 环的形状（#167 原文）：
//
//	layout-score → 哪维低就对症下确定性变换 → 重新打分 → 循环到每维过阈值
//
// **诚实的范围声明**：目前只有一个变换器（grid-snap → tidy 维）。其余八维的分数
// 算得出来、归因也给得出来，但没有"对症的确定性变换"可下 —— 环会照实说"这一维低，
// 但我没有能安全自动化的手段"，而不是硬凑一个动作去搬件。
//
// 为什么不硬凑：#153 用实测证明了乱凑的代价。那轮 cleanup 里 `silk-align --side`
// 看着"aligned: 69"，实际只动了 10 件、方位分布纹丝不动（它是碰撞规避语义，不是
// 一致性工具）；更糟的是它把 silk-over-pad 从 0 条推到 3 条，自检还报 clean:true。
// 一个会偶尔搞坏板子的自动美化工具，没人敢开。
//
// 所以这一版的价值不在"能修多少维"，而在**把安全框架立住**：不可动集合、位移预算、
// 逐步复核、按步回滚、回读证实。后续每加一个变换器，都直接继承这套护栏。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

func newPcbRefineCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var (
		specPath string
		apply    bool
		asJSON   bool
		maxShift float64
		maxRound int
		target   float64
		gridMil  float64
		withLock bool
	)
	c := &cobra.Command{
		Use:   "refine",
		Short: "Score-driven placement refinement with per-step rollback (dry-run by default)",
		Long: "读 `pcb layout-score` 的逐维归因，对最弱的维下**确定性**变换，每步之后复核，\n" +
			"任一步让 `pcb check` 新增 finding 或让综合分下降就**回滚该步**。\n\n" +
			"默认是 dry-run —— 只出计划不落笔，要 `--apply` 才真动。这与本仓多数命令的\n" +
			"`--dry-run` 惯例相反，是刻意的：这条命令会批量搬件，而 issue #153 的实测里\n" +
			"一个看似无害的对齐动作静默制造了 3 条压焊盘的丝印。\n\n" +
			"**当前只实现了一个变换器**：grid-snap → tidy 维（#153 实测「零副作用，纯收益」：\n" +
			"BBClaw 69 器件板上 37/39 落格、最大位移 1.998mil、check findings 25→25 不变）。\n" +
			"其余八维会照实报告「这一维低，但没有可安全自动化的对症手段」，不硬凑动作。\n\n" +
			"三条护栏（都来自 #153 的要求，全部默认开启）：\n" +
			"  · 不可动集合 —— 锁定件、已签字的 tier-1/2（孔与朝向经用户确认的边缘件）一律不碰\n" +
			"  · 位移预算 —— 单件位移超 --max-shift 的**剔除而不是截断**（截断会把件放到\n" +
			"    既不是原位也不是目标的第三个位置，比不动更糟）\n" +
			"  · 逐步回滚 —— 按**步**而非按命令回滚，好的步不会被坏的步连累",
		Example: "  # 先看会动什么（默认就是 dry-run）\n" +
			"  easyeda pcb refine --project ceshi\n\n" +
			"  # 确认后落笔\n" +
			"  easyeda pcb refine --project ceshi --apply\n\n" +
			"  # 收紧位移预算到 2mil（只清亚 mil 漂移）\n" +
			"  easyeda pcb refine --apply --max-shift 2",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := defaultRefineOpts()
			opts.DryRun = !apply
			opts.IncludeLocked = withLock
			if maxShift > 0 {
				opts.MaxShiftMil = maxShift
			}
			if maxRound > 0 {
				opts.MaxRounds = maxRound
			}
			if target > 0 {
				opts.TargetScore = target
			}

			var s0 *spec.Spec
			if specPath != "" {
				raw, err := os.ReadFile(specPath)
				if err != nil {
					return fmt.Errorf("read spec: %w", err)
				}
				if s0, err = spec.Parse(raw); err != nil {
					return err
				}
			}
			rep, err := runRefineLoop(cfg, *window, s0, opts, gridMil, stderr)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"refine": rep})
			}
			renderRefine(rep, stdout)
			return nil
		},
	}
	c.Flags().StringVar(&specPath, "spec", "", "S0 spec JSON — unlocks the intent dimensions when scoring")
	c.Flags().BoolVar(&apply, "apply", false, "actually move parts (without it this is a dry run — see Long)")
	c.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	c.Flags().Float64Var(&maxShift, "max-shift", 0, "per-part displacement budget in mil (default 5 — snapping, not rearranging)")
	c.Flags().IntVar(&maxRound, "max-rounds", 0, "convergence round cap (default 4)")
	c.Flags().Float64Var(&target, "target", 0, "per-dimension pass mark (default 85)")
	c.Flags().Float64Var(&gridMil, "grid", 0, "grid for the snap transform in mil (default 5 — catches sub-mil drift)")
	c.Flags().BoolVar(&withLock, "include-locked", false, "also move parts locked in the editor (off by default — a lock is a decision)")
	return c
}

// runRefineLoop 是环本体。
func runRefineLoop(cfg *appConfig, window string, s0 *spec.Spec, opts refineOpts, gridMil float64, stderr io.Writer) (refineReport, error) {
	rep := refineReport{DryRun: opts.DryRun}

	snap, err := fetchBoardSnapshot(cfg, window, boardSnapshotOpts{withSilk: true, withRules: true, withLayers: true})
	if err != nil {
		return rep, err
	}
	if len(snap.Components) == 0 {
		rep.Summary = "board has no components — nothing to refine"
		return rep, nil
	}

	// 不可动集合：锁定件 + 已签字的 tier-1/2。tier 数据读不到时（没跑过 stage）
	// 只保护锁定件，并在报告里说明——这是降级不是失败。
	tiers := map[int][]string{}
	if project, perr := resolveStageProject(cfg, window); perr == nil {
		if st, serr := loadPcbStageState(project); serr == nil {
			for n := 1; n <= workflowTierCount; n++ {
				if tc := st.Tier(n); tc != nil {
					tiers[n] = tc.Designators
				}
			}
		}
	}
	if len(tiers) == 0 {
		rep.Warnings = append(rep.Warnings,
			"no confirmed placement tiers found — only editor-locked parts are protected. Run `pcb stage confirm-tier` first so mounting holes and user-confirmed edge connectors are off-limits too")
	}
	immovable, immovableList := buildImmovableSet(snap, tiers, opts.IncludeLocked)
	rep.Immovable = len(immovableList)

	scoreOpts := layoutScoreOpts{gridMil: gridMil}
	before := analyzeLayoutScore(snap, s0, scoreOpts)
	rep.ScoreBefore = before.Overall
	cur := before

	for round := range opts.MaxRounds {
		weak := cur.weakest(0)
		if len(weak) == 0 {
			break
		}
		if weak[0].Score >= opts.TargetScore {
			rep.Converged = true
			break
		}
		// 按最弱维依次找变换器；找不到就往下一维走，全找不到就停。
		var step *refineStep
		for _, d := range weak {
			if d.Score >= opts.TargetScore {
				break
			}
			if s := planStepFor(d, snap, immovable, opts, gridMil); s != nil {
				step = s
				break
			}
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(
				"%s(%s) is at %.1f but has no deterministic transform yet — fix it by hand (`pcb layout-score --only %s --all` lists the parts)",
				dimensionTitles[d.ID], d.ID, d.Score, d.ID))
		}
		if step == nil {
			break
		}
		rep.Rounds = round + 1
		step.ScoreBefore = cur.Overall

		if opts.DryRun {
			step.Reason = "dry run — nothing was moved"
			rep.Steps = append(rep.Steps, *step)
			rep.MovedParts += len(step.Moves)
			break
		}

		// 复核基线：变换前的 check finding 数。
		beforeFindings := countGateableFindings(cfg, window, stderr)
		step.FindingsBefore = beforeFindings

		attempted, applied, aerr := applyRefineMoves(cfg, window, step.Moves, stderr)
		step.Applied = applied
		if aerr != nil {
			step.Reason = "apply failed: " + aerr.Error()
			step.RolledBack = true
			step.Restored, step.Errors = rollbackRefineMoves(cfg, window, attempted, stderr)
			rep.Steps = append(rep.Steps, *step)
			break
		}

		// 复核：重新拉快照 + 打分 + 数 finding。
		newSnap, ferr := fetchBoardSnapshot(cfg, window, boardSnapshotOpts{withSilk: true, withRules: true, withLayers: true})
		if ferr != nil {
			step.Reason = "post-step re-read failed: " + ferr.Error()
			step.RolledBack = true
			step.Restored, step.Errors = rollbackRefineMoves(cfg, window, attempted, stderr)
			rep.Steps = append(rep.Steps, *step)
			break
		}
		after := analyzeLayoutScore(newSnap, s0, scoreOpts)
		step.ScoreAfter = after.Overall
		step.FindingsAfter = countGateableFindings(cfg, window, stderr)

		// #153 的硬护栏：新增 finding 就回滚，哪怕分数涨了。
		// 分数是启发式，check finding 是会进 Gerber 的真问题。
		switch {
		case step.FindingsAfter > step.FindingsBefore:
			step.Reason = fmt.Sprintf("rolled back: pcb check findings rose %d → %d", step.FindingsBefore, step.FindingsAfter)
			step.RolledBack = true
		case after.Overall < cur.Overall-0.05:
			step.Reason = fmt.Sprintf("rolled back: overall score fell %.1f → %.1f", cur.Overall, after.Overall)
			step.RolledBack = true
		}
		if step.RolledBack {
			step.Restored, step.Errors = rollbackRefineMoves(cfg, window, attempted, stderr)
			rep.Steps = append(rep.Steps, *step)
			break
		}

		rep.MovedParts += applied
		snap, cur = newSnap, after
		rep.Steps = append(rep.Steps, *step)
	}

	rep.ScoreAfter = cur.Overall
	rep.OK = true
	for _, s := range rep.Steps {
		if s.RolledBack {
			rep.OK = false
		}
	}
	rep.Summary = refineSummary(&rep)
	return rep, nil
}

// planStepFor 给一维找对症的确定性变换。没有就返回 nil —— 这是**正常**结果，
// 不是失败：八维目前都没有，报告会照实说。
func planStepFor(d scoreDimension, snap *boardSnapshot, immovable map[string]string, opts refineOpts, gridMil float64) *refineStep {
	switch d.ID {
	case dimTidy:
		moves := planGridSnap(snap, gridMil, immovable)
		kept, rejects := budgetMoves(moves, immovable, opts.MaxShiftMil)
		if len(kept) == 0 {
			return nil
		}
		return &refineStep{
			Name: "grid-snap", Dimension: dimTidy,
			Moves: kept, Skipped: len(rejects),
		}
	default:
		// partition / flow-order / edge-io / protection / compact / rf /
		// routable / clearance：分数与归因都有，但没有能安全自动化的变换。
		// 它们要的是"把这个模块整体搬到那条带里""把连接器换一条边"这类**重排**，
		// 而重排一旦做错，代价远大于收益 —— 交给人，或交给 place-constrained
		// 的分档流程（它有 zone 约束和螺旋合法化）。
		return nil
	}
}

// countGateableFindings 数一次 pcb check 的可门控 finding。
//
// 口径与 workflow advance 的 post-route 门一致（Errors + power-not-poured +
// width-under-spec），而不是 --strict 那套「Warnings>0 全灭」—— 精修只该对
// **它自己可能制造的**问题负责，不该被板上早已存在的告警绑架。
// 读失败返回 -1，调用方视作"无法复核"并保守回滚。
func countGateableFindings(cfg *appConfig, window string, stderr io.Writer) int {
	rep, err := gatherPcbCheckReport(cfg, window, 0, stderr)
	if err != nil || rep == nil {
		return -1
	}
	return rep.Summary.Errors + rep.Summary.PowerNotPoured + rep.Summary.WidthUnderSpec + rep.Summary.SilkOverPad
}

func refineSummary(rep *refineReport) string {
	var b strings.Builder
	if rep.DryRun {
		b.WriteString("dry run — ")
	}
	fmt.Fprintf(&b, "%.1f → %.1f over %d round(s), %d part(s) moved",
		rep.ScoreBefore, rep.ScoreAfter, rep.Rounds, rep.MovedParts)
	if rep.Converged {
		b.WriteString(", converged")
	}
	rolled := 0
	for _, s := range rep.Steps {
		if s.RolledBack {
			rolled++
		}
	}
	if rolled > 0 {
		fmt.Fprintf(&b, ", %d step(s) rolled back", rolled)
	}
	return b.String()
}

func renderRefine(rep refineReport, w io.Writer) {
	head := "精修环"
	if rep.DryRun {
		head += "（dry run — 未落笔，加 --apply 才真动）"
	}
	fmt.Fprintf(w, "\n%s\n%s\n", head, strings.Repeat("─", 72))
	fmt.Fprintf(w, "综合分 %.1f → %.1f · %d 件不可动\n", rep.ScoreBefore, rep.ScoreAfter, rep.Immovable)

	for _, s := range rep.Steps {
		status := "✓"
		if s.RolledBack {
			status = "↩"
		}
		fmt.Fprintf(w, "\n%s %s → %s维  移动 %d 件", status, s.Name, dimensionTitles[s.Dimension], len(s.Moves))
		if s.Skipped > 0 {
			fmt.Fprintf(w, "（%d 件被护栏拦下）", s.Skipped)
		}
		fmt.Fprintln(w)
		if s.Reason != "" {
			fmt.Fprintf(w, "   %s\n", s.Reason)
		}
		if s.RolledBack {
			fmt.Fprintf(w, "   回读证实还原 %d/%d 件\n", s.Restored, len(s.Moves))
		}
		shown := 0
		for _, m := range s.Moves {
			if shown >= 5 {
				fmt.Fprintf(w, "   … 另有 %d 件\n", len(s.Moves)-shown)
				break
			}
			fmt.Fprintf(w, "   %-8s (%.4f,%.4f) → (%.1f,%.1f)  %s\n", m.Designator, m.FromX, m.FromY, m.ToX, m.ToY, m.Why)
			shown++
		}
		for _, e := range s.Errors {
			fmt.Fprintf(w, "   ⚠️  %s\n", e)
		}
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintf(w, "\n⚠️  %s\n", warn)
	}
	fmt.Fprintf(w, "\n%s\n\n", rep.Summary)
}
