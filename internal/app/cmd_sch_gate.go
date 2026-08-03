package app

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// ── sch gate: the S5 校验门, one command ──────────────────────────────────
//
// 收敛动机(docs/design-sch-surface-convergence.md):原理图有 4 个独立检查命令
// (layout-lint / check / bridge-check / drc),agent 每次都要自己决定跑哪几个、
// 什么顺序、哪个的非零退出算数 —— 这个决策没有数据判据,只有散文描述,**猜错就是
// 不稳定**。audit log 实测 agent 在检查器失败后分别改调过 components.list / save /
// check / pages.list 四种不同的下一步,没有一种是被规定的。
//
// gate 把顺序固定下来、把判据写进代码、出一张报告:agent 只需要「跑 gate,读
// blockers」。四个单命令原样保留给专家和局部复查,但不再是主干路径。

// gateStageSpec declares one stage's identity and how to run it. Order in
// gateStages IS the execution order.
type gateStageSpec struct {
	Name string
	// Why this position: cheap-and-explanatory first. Geometry (layout-lint) is
	// one components.list read and explains downstream noise — overlapping parts
	// produce phantom electrical findings, so reporting them first tells the
	// agent what to fix before it chases symptoms. DRC is last: it is the
	// slowest, needs the window in the foreground, and its aggregate output is
	// the least actionable of the four.
	Why string
}

var gateStages = []gateStageSpec{
	{Name: "layout-lint", Why: "几何真值,一次 components.list,解释后面的连锁误报"},
	{Name: "check", Why: "重建式逐条设计检查 + Go 侧几何 marker 规则"},
	{Name: "bridge-check", Why: "wire-tree 粒度的合并短路 / 孤儿桩"},
	{Name: "drc", Why: "官方 SDK DRC,最慢且需前台,放最后"},
}

// gateStage is one stage's outcome inside the aggregate report.
type gateStage struct {
	Name string `json:"name"`
	// Status separates the two failure kinds that agents kept conflating:
	//   pass    — ran, nothing blocking
	//   fail    — ran, found blocking problems (the BOARD has a problem)
	//   error   — could NOT run (connector down, page not open, bad shape).
	//             The board is not implicated; retrying other checkers is futile.
	//   skipped — excluded via --only/--skip, or not reached after a hard stop
	Status   string `json:"status"`
	Errors   int    `json:"errors"`
	Warnings int    `json:"warnings"`
	Summary  string `json:"summary"`
	Error    string `json:"error,omitempty"`
	// Detail carries the stage's own full report under --json, so `sch gate --json`
	// is a superset of the four single commands' JSON and nothing needs a re-run.
	Detail any `json:"detail,omitempty"`
}

// gateReport is the one report the S5 gate produces.
type gateReport struct {
	OK bool `json:"ok"`
	// Verdict is the three-state outcome:
	//   pass    — every executed stage passed
	//   fail    — the schematic has blocking problems (see blockers)
	//   blocked — a checker could not run; the schematic was never fully judged
	// "blocked" exists because treating infra failure as a board failure is what
	// sent agents chasing phantom fixes.
	Verdict  string      `json:"verdict"`
	Stages   []gateStage `json:"stages"`
	Blockers []string    `json:"blockers"`
	Warnings []string    `json:"warnings,omitempty"`
}

const (
	gateStatusPass    = "pass"
	gateStatusFail    = "fail"
	gateStatusError   = "error"
	gateStatusSkipped = "skipped"
)

// resolveGateStages applies --only/--skip and returns the ordered stage names to
// run plus the ones deliberately excluded. Unknown names are an error rather
// than a silent no-op — a typo'd --only must never look like a clean gate.
func resolveGateStages(only, skip string) (run []string, skipped []string, err error) {
	known := make(map[string]bool, len(gateStages))
	order := make([]string, 0, len(gateStages))
	for _, s := range gateStages {
		known[s.Name] = true
		order = append(order, s.Name)
	}
	parse := func(raw, flag string) (map[string]bool, error) {
		set := map[string]bool{}
		for _, part := range strings.Split(raw, ",") {
			name := strings.TrimSpace(strings.ToLower(part))
			if name == "" {
				continue
			}
			if !known[name] {
				return nil, fmt.Errorf("%s: unknown stage %q (known: %s)", flag, name, strings.Join(order, ", "))
			}
			set[name] = true
		}
		return set, nil
	}
	onlySet, err := parse(only, "--only")
	if err != nil {
		return nil, nil, err
	}
	skipSet, err := parse(skip, "--skip")
	if err != nil {
		return nil, nil, err
	}
	if len(onlySet) > 0 && len(skipSet) > 0 {
		return nil, nil, fmt.Errorf("--only and --skip are mutually exclusive")
	}
	for _, name := range order {
		switch {
		case len(onlySet) > 0 && !onlySet[name]:
			skipped = append(skipped, name)
		case skipSet[name]:
			skipped = append(skipped, name)
		default:
			run = append(run, name)
		}
	}
	if len(run) == 0 {
		return nil, nil, fmt.Errorf("no stages left to run")
	}
	return run, skipped, nil
}

// gateLayoutStage runs layout-lint and grades it. Overlaps and pin-coincidences
// are blocking (a real geometric defect); tight spacing is advisory.
func gateLayoutStage(cfg *appConfig, window string, minGap, pinEps float64, allPages, strict bool) gateStage {
	st := gateStage{Name: "layout-lint"}
	rep, err := collectLayoutLint(cfg, window, minGap, pinEps, allPages, false, strict)
	if err != nil {
		st.Status, st.Error = gateStatusError, err.Error()
		st.Summary = "layout-lint 没能跑起来"
		return st
	}
	st.Detail = rep
	st.Errors = len(rep.Overlaps) + len(rep.PinCoincidences)
	st.Warnings = len(rep.TightPairs) + len(rep.GridViolations) + len(rep.ZoneViolations)
	st.Summary = fmt.Sprintf("%d overlap, %d pin-coincidence, %d tight, %d off-grid, %d zone-violation (zone-check=%s)",
		len(rep.Overlaps), len(rep.PinCoincidences), len(rep.TightPairs),
		len(rep.GridViolations), len(rep.ZoneViolations), rep.ZoneCheckStatus)
	// rep.OK already folds in the strict gate and the geometry-provenance checks
	// (unchecked/unproven pins, invalid values) — trust it rather than
	// re-deriving a second, subtly different verdict here.
	if rep.OK {
		st.Status = gateStatusPass
	} else {
		st.Status = gateStatusFail
	}
	return st
}

// gateCheckStage runs the reconstructed design check. fatal/error findings block;
// warn/info are advisory unless --strict.
func gateCheckStage(cfg *appConfig, window string, allPages, strict bool, overlapEps float64, stderr io.Writer) gateStage {
	st := gateStage{Name: "check"}
	payload := map[string]any{}
	if allPages {
		payload["allPages"] = true
	}
	res, err := requestAction(cfg, "schematic.check", window, payload)
	if err != nil {
		st.Status, st.Error = gateStatusError, err.Error()
		st.Summary = "sch check 没能跑起来"
		return st
	}
	rep, perr := parseCheckReport(res.Result)
	if perr != nil {
		st.Status, st.Error = gateStatusError, perr.Error()
		st.Summary = "sch check 返回了无法解析的结构"
		return st
	}
	mergeMarkerGeomFindings(cfg, window, allPages, overlapEps, &rep, stderr)
	st.Detail = rep
	for _, f := range rep.Findings {
		switch strings.ToLower(f.Level) {
		case "fatal", "error":
			st.Errors++
		default:
			st.Warnings++
		}
	}
	st.Summary = fmt.Sprintf("%d finding(s): %d error/fatal, %d warn/info", rep.Summary.Total, st.Errors, st.Warnings)
	switch {
	case st.Errors > 0:
		st.Status = gateStatusFail
	case strict && st.Warnings > 0:
		st.Status = gateStatusFail
	default:
		st.Status = gateStatusPass
	}
	return st
}

// gateBridgeStage runs bridge-check. A BRIDGE is a real short and blocks; an
// orphan stub/flag is advisory (it is cosmetic until it is wired).
func gateBridgeStage(cfg *appConfig, window string, allPages, strict bool) gateStage {
	st := gateStage{Name: "bridge-check"}
	payload := map[string]any{}
	if allPages {
		payload["allPages"] = true
	}
	res, err := requestAction(cfg, "schematic.bridgeCheck", window, payload)
	if err != nil {
		st.Status, st.Error = gateStatusError, err.Error()
		st.Summary = "sch bridge-check 没能跑起来"
		return st
	}
	rep, perr := parseBridgeReport(res.Result)
	if perr != nil {
		st.Status, st.Error = gateStatusError, perr.Error()
		st.Summary = "sch bridge-check 返回了无法解析的结构"
		return st
	}
	st.Detail = rep
	st.Errors = rep.Summary.Bridges
	st.Warnings = rep.Summary.Orphans + rep.Summary.OrphanFlags
	st.Summary = fmt.Sprintf("%d bridge(short), %d orphan-stub, %d orphan-flag (%d wire tree(s))",
		rep.Summary.Bridges, rep.Summary.Orphans, rep.Summary.OrphanFlags, rep.Summary.WireTreesTotal)
	switch {
	case st.Errors > 0:
		st.Status = gateStatusFail
	case strict && st.Warnings > 0:
		st.Status = gateStatusFail
	default:
		st.Status = gateStatusPass
	}
	return st
}

// gateDrcStage runs the official SDK DRC. Only fatal blocks — matching the
// standalone `sch drc` contract, which real boards are calibrated against.
func gateDrcStage(cfg *appConfig, window string, strict bool) gateStage {
	st := gateStage{Name: "drc"}
	payload := map[string]any{"includeVerboseError": true}
	if strict {
		payload["strict"] = true
	}
	res, err := requestAction(cfg, "schematic.drc.check", window, payload)
	if err != nil {
		st.Status, st.Error = gateStatusError, err.Error()
		st.Summary = "sch drc 没能跑起来(DRC 需要窗口在前台)"
		return st
	}
	rep, perr := parseDrcReport(res.Result)
	if perr != nil {
		st.Status, st.Error = gateStatusError, perr.Error()
		st.Summary = "sch drc 返回了无法解析的结构"
		return st
	}
	st.Detail = rep
	st.Errors = rep.Fatal
	st.Warnings = rep.Summary.Error + rep.Summary.Warn
	st.Summary = fmt.Sprintf("%d fatal, %d error, %d warn, %d info (total %d)",
		rep.Summary.Fatal, rep.Summary.Error, rep.Summary.Warn, rep.Summary.Info, rep.Summary.Total)
	if st.Errors > 0 {
		st.Status = gateStatusFail
	} else {
		st.Status = gateStatusPass
	}
	return st
}

// gateAdviceFor maps a stage name to the one next action worth taking. Kept
// here (not in the skill prose) so the fix path travels with the failure —
// the audit log showed agents inventing four different next steps for the
// same failure because nothing prescribed one.
func gateAdviceFor(name string) string {
	switch name {
	case "layout-lint":
		return "先治几何:overlap/pin-coincidence 用 `sch autolayout` 重排或 `sch modify` 单件挪位,别急着改电气"
	case "check":
		return "看 finding 的 type:duplicate-net-marker 喂 `sch prim-delete`,floating-pin 用 `sch no-connect`,wire-* 用 `sch disconnect` 后重连"
	case "bridge-check":
		return "bridge = 真短路,按 tree 的 primitiveIds 定位后 `sch prim-delete` 拆掉压线,再 `sch connect` 重连"
	case "drc":
		return "跑 `sch drc --verbose` 看逐条明细(gate 只汇总);DRC 需要 EasyEDA 窗口在前台"
	}
	return ""
}

// runSchGate executes the fixed S5 gate pipeline and renders one report.
func runSchGate(cfg *appConfig, window string, allPages, strict, asJSON, failFast bool,
	only, skip string, minGap, pinEps, overlapEps float64, stdout, stderr io.Writer) error {
	if strict && allPages {
		return fmt.Errorf("sch gate: --strict cannot be combined with --all-pages: inactive pages expose shallow geometry (see layout-lint), so gate each page after `easyeda doc switch <page>`")
	}
	run, skippedNames, err := resolveGateStages(only, skip)
	if err != nil {
		return err
	}

	rep := gateReport{Stages: make([]gateStage, 0, len(gateStages))}
	byName := map[string]gateStage{}
	hardStopped := false
	for _, name := range run {
		if hardStopped {
			byName[name] = gateStage{Name: name, Status: gateStatusSkipped, Summary: "前一个 stage 没能跑起来,后续未执行"}
			continue
		}
		var st gateStage
		switch name {
		case "layout-lint":
			st = gateLayoutStage(cfg, window, minGap, pinEps, allPages, strict)
		case "check":
			st = gateCheckStage(cfg, window, allPages, strict, overlapEps, stderr)
		case "bridge-check":
			st = gateBridgeStage(cfg, window, allPages, strict)
		case "drc":
			st = gateDrcStage(cfg, window, strict)
		}
		byName[name] = st
		// A stage that could not RUN stops the pipeline: the remaining checkers
		// talk to the same connector/page and would fail the same way, and their
		// noise would bury the real cause. This is exactly the pattern the audit
		// log caught — 146 blind retries after a NO_CONNECTOR.
		if st.Status == gateStatusError {
			hardStopped = true
		}
		if failFast && st.Status == gateStatusFail {
			hardStopped = true
		}
	}
	for _, name := range skippedNames {
		byName[name] = gateStage{Name: name, Status: gateStatusSkipped, Summary: "被 --only/--skip 排除"}
	}
	// Emit in declared pipeline order regardless of --only/--skip, so the report
	// always reads as the same fixed pipeline.
	for _, spec := range gateStages {
		if st, ok := byName[spec.Name]; ok {
			rep.Stages = append(rep.Stages, st)
		}
	}

	gradeGateReport(&rep)

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		renderGateReport(rep, stdout)
	}

	switch rep.Verdict {
	case "blocked":
		return fmt.Errorf("sch gate: BLOCKED — 检查器没能跑完,原理图未被完整判定(不是板子的问题):%s",
			strings.Join(rep.Blockers, "; "))
	case "fail":
		return fmt.Errorf("sch gate: FAIL — %s", strings.Join(rep.Blockers, "; "))
	}
	return nil
}

// gradeGateReport derives blockers, warnings, verdict and ok from the stage
// outcomes. Pure so the verdict contract is unit-testable without a live
// connector — the gate's whole value is that this grading is fixed in code
// rather than re-decided per run.
//
// `blocked` outranks `fail`: if any checker could not run, the schematic was
// never fully judged, and reporting "fail" would send the agent fixing a board
// that may be fine (or, worse, declaring it fixed after the real checker never
// ran).
func gradeGateReport(rep *gateReport) {
	rep.Blockers = nil
	rep.Warnings = nil
	anyError, anyFail := false, false
	for _, st := range rep.Stages {
		switch st.Status {
		case gateStatusError:
			anyError = true
			rep.Blockers = append(rep.Blockers, fmt.Sprintf("%s 没能跑起来: %s", st.Name, st.Error))
		case gateStatusFail:
			anyFail = true
			rep.Blockers = append(rep.Blockers,
				fmt.Sprintf("%s: %d 个阻塞项 — %s", st.Name, st.Errors, st.Summary))
		}
		if st.Status != gateStatusError && st.Warnings > 0 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %d 条告警", st.Name, st.Warnings))
		}
	}
	switch {
	case anyError:
		rep.Verdict, rep.OK = "blocked", false
	case anyFail:
		rep.Verdict, rep.OK = "fail", false
	default:
		rep.Verdict, rep.OK = "pass", true
	}
}

func gateStatusTag(status string) string {
	switch status {
	case gateStatusPass:
		return "PASS "
	case gateStatusFail:
		return "FAIL "
	case gateStatusError:
		return "ERROR"
	case gateStatusSkipped:
		return "SKIP "
	}
	return "?????"
}

func renderGateReport(rep gateReport, w io.Writer) {
	verdict := strings.ToUpper(rep.Verdict)
	fmt.Fprintf(w, "sch gate: %s\n", verdict)
	for _, st := range rep.Stages {
		fmt.Fprintf(w, "  %s  %-13s  %s\n", gateStatusTag(st.Status), st.Name, st.Summary)
		if st.Error != "" {
			fmt.Fprintf(w, "         └─ %s\n", st.Error)
		}
	}
	if len(rep.Blockers) > 0 {
		fmt.Fprintln(w, "\n阻塞项:")
		for _, b := range rep.Blockers {
			fmt.Fprintf(w, "  • %s\n", b)
		}
		// One prescribed next action per failing stage — see gateAdviceFor.
		advice := make([]string, 0, len(rep.Stages))
		for _, st := range rep.Stages {
			if st.Status != gateStatusFail {
				continue
			}
			if a := gateAdviceFor(st.Name); a != "" {
				advice = append(advice, fmt.Sprintf("  → %s", a))
			}
		}
		if len(advice) > 0 {
			sort.Strings(advice)
			fmt.Fprintln(w, "\n下一步:")
			for _, a := range advice {
				fmt.Fprintln(w, a)
			}
		}
		if rep.Verdict == "blocked" {
			fmt.Fprintln(w, "\n  注意:这是「检查器没跑成」,不是「板子有问题」——先 `easyeda health` 确认连接器,")
			fmt.Fprintln(w, "  再 `easyeda doc ls` / `doc switch <page>` 确认目标页已打开,然后重跑 gate。")
		}
	}
	if len(rep.Warnings) > 0 {
		fmt.Fprintf(w, "\n告警(不阻塞): %s\n", strings.Join(rep.Warnings, ", "))
	}
}

// newSchGateCmd builds the `easyeda sch gate` command.
func newSchGateCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var (
		allPages, strict, asJSON, failFast bool
		only, skip                         string
		minGap, pinEps, overlapEps         float64
	)
	c := &cobra.Command{
		Use:   "gate",
		Short: "S5 校验门:按固定顺序跑 layout-lint → check → bridge-check → drc,出一张报告",
		Long: `Run the schematic verification gate: every checker, in one fixed order, as one report.

Pipeline (order is fixed on purpose — cheap and explanatory first):

  1. layout-lint   几何真值,一次 components.list,解释后面的连锁误报
  2. check         重建式逐条设计检查 + Go 侧几何 marker 规则
  3. bridge-check  wire-tree 粒度的合并短路 / 孤儿桩
  4. drc           官方 SDK DRC,最慢且需前台,放最后

Why one command: the four checkers exist as separate commands, so every run
started with an undecidable question — which ones, in what order, whose exit
code counts. The audit log shows agents answering it four different ways for
the same failure. Here the order is fixed, the blocking rules live in code, and
the output is one verdict.

Verdict is three-state, because "the checker could not run" is NOT "the board
is broken":

  pass     every executed stage passed
  fail     the schematic has blocking problems — see 阻塞项 / blockers
  blocked  a checker could not run (connector down, page not open, bad shape);
           the schematic was never fully judged. Remaining stages are skipped
           rather than run into the same wall.

Blocking rules: layout-lint overlap/pin-coincidence · check fatal+error ·
bridge-check bridge(real short) · drc fatal. Tight spacing, orphan stubs, and
non-fatal DRC items are advisory — --strict promotes them to blocking.

--json emits every stage's full native report under stages[].detail, so it is a
superset of the four single commands' JSON: nothing needs a second run.`,
		Args: cobra.NoArgs,
		Example: `  easyeda sch gate
  easyeda sch gate --json
  easyeda sch gate --strict                 # 告警也阻塞
  easyeda sch gate --only layout-lint,check # 只跑便宜的两关
  easyeda sch gate --skip drc               # 窗口不在前台时跳过 DRC
  easyeda sch gate --fail-fast              # 第一个阻塞失败就停`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchGate(cfg, *window, allPages, strict, asJSON, failFast,
				only, skip, minGap, pinEps, overlapEps, stdout, stderr)
		},
	}
	c.Flags().BoolVar(&allPages, "all-pages", false, "gate every page (geometry on inactive pages is shallow — cannot combine with --strict)")
	c.Flags().BoolVar(&strict, "strict", false, "promote advisory findings (tight spacing, warn-level, orphan stubs) to blocking")
	c.Flags().BoolVar(&asJSON, "json", false, "emit the aggregate report as JSON, with each stage's full report under stages[].detail")
	c.Flags().BoolVar(&failFast, "fail-fast", false, "stop at the first blocking failure instead of collecting every stage")
	c.Flags().StringVar(&only, "only", "", "run only these stages (comma-separated: layout-lint,check,bridge-check,drc)")
	c.Flags().StringVar(&skip, "skip", "", "skip these stages (comma-separated)")
	// Defaults mirror `sch layout-lint` / `sch check` exactly — gating a page must
	// never disagree with linting it directly.
	c.Flags().Float64Var(&minGap, "min-gap", 2.54, "layout-lint stage: min edge-to-edge gap (mm) before a pair is flagged as tight")
	c.Flags().Float64Var(&pinEps, "pin-eps", 0, "layout-lint stage: max distance (mm) at which two pins of DIFFERENT components count as coincident; 0 = strict equality")
	c.Flags().Float64Var(&overlapEps, "overlap-eps", 0.5, "check stage: min positive-area extent (mm) for the marker-overlap/titleblock-overlap rules")
	return c
}
