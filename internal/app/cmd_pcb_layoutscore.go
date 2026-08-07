package app

// cmd_pcb_layoutscore.go — `easyeda pcb layout-score` 的 CLI 层：取数、组装、渲染。
//
// 纯核在 pcb_layoutscore.go（analyzeLayoutScore），各维实现在 pcb_score_*.go。
// 这一层只做三件事：把板拉成快照（或从文件读回）、把 S0 spec 读进来、把报告渲染成
// 人读表格或 JSON。
//
// `--from <snapshot.json>` 是刻意留的离线入口：金标准好板回归(#167 第五层)要能在
// 没有编辑器的环境里重放同一块板，CI 里也是。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

func newPcbLayoutScoreCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var (
		specPath string
		fromPath string
		asJSON   bool
		minScore float64
		only     []string
		skip     []string
		weights  []string
		gridMil  float64
		minGap   float64
		showAll  bool
	)
	c := &cobra.Command{
		Use:   "layout-score",
		Short: "Score placement quality across 9 dimensions, with per-component attribution",
		Long: "把「好布局」拆成多维可计算分数(#167)。与 `pcb layout-lint` 的分工：\n\n" +
			"  layout-lint   单标量可布性分 + 布线前硬门。一处重叠就把分数打成 0，\n" +
			"                其它维度的差异全被抹平 —— 看不出布局到底好在哪差在哪。\n" +
			"  layout-score  九维各自 0-100 + 每维「是哪几个器件拉低了它」。硬错不再\n" +
			"                抹平分数，而是单列 blocking 一票否决。\n\n" +
			"维度：partition(功能分区) flow-order(信号流向) edge-io(对外接口与板沿)\n" +
			"      protection(保护件/去耦就近) tidy(齐整度) compact(紧凑度) rf(射频)\n" +
			"      routable(可布性) clearance(装配间距)\n\n" +
			"意图类维度（flow-order / edge-io 的 internal 判定）需要 --spec；没给就标\n" +
			"skipped 而不是给满分 —— 「没测」和「测了满分」在报告里必须可区分。",
		Example: "  # 给当前板打分\n" +
			"  easyeda pcb layout-score --project ceshi\n\n" +
			"  # 带 S0 意图（解锁 flow-order 与 internal 连接器判定）\n" +
			"  easyeda pcb layout-score --spec .easyeda/s0-ceshi.json\n\n" +
			"  # 只看最弱的两维，带完整归因\n" +
			"  easyeda pcb layout-score --only tidy,compact --all\n\n" +
			"  # 离线重放一块已 dump 的板（CI / 金标准回归）\n" +
			"  easyeda pcb dump --out board.json\n" +
			"  easyeda pcb layout-score --from board.json --json\n\n" +
			"  # 当门用（综合分不达标则非零退出）\n" +
			"  easyeda pcb layout-score --min-score 75",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := layoutScoreOpts{
				minScore: minScore,
				only:     csvSet(only),
				skip:     csvSet(skip),
				minGap:   minGap,
				gridMil:  gridMil,
			}
			var err error
			if opts.weights, err = parseWeightOverrides(weights); err != nil {
				return err
			}
			if err := validateDimSelectors(opts.only, opts.skip); err != nil {
				return err
			}

			// 板数据：文件优先（离线重放），否则拉实时快照。
			var snap *boardSnapshot
			if fromPath != "" {
				f, oerr := os.Open(fromPath)
				if oerr != nil {
					return fmt.Errorf("open snapshot: %w", oerr)
				}
				defer f.Close()
				if snap, err = loadBoardSnapshotFile(f); err != nil {
					return err
				}
			} else {
				// 齐整度要丝印、间距要活规则、射频要铜层数 —— 一次拉齐，九维共用。
				if snap, err = fetchBoardSnapshot(cfg, *window, boardSnapshotOpts{
					withSilk: true, withRules: true, withLayers: true,
				}); err != nil {
					return err
				}
			}

			// S0 意图：给了就读，读不动就报错（用户明确要求用它，静默忽略是最差的）。
			var s0 *spec.Spec
			if specPath != "" {
				raw, rerr := os.ReadFile(specPath)
				if rerr != nil {
					return fmt.Errorf("read spec: %w", rerr)
				}
				if s0, err = spec.Parse(raw); err != nil {
					return err
				}
				issues := spec.Validate(s0)
				if spec.HasErrors(issues) {
					for _, i := range issues {
						if i.Level == "ERROR" {
							fmt.Fprintf(stderr, "❌ spec %s: %s\n", i.Field, i.Message)
						}
					}
					return fmt.Errorf("S0 spec has errors; fix it or run `easyeda spec validate %s`", specPath)
				}
				for _, i := range issues {
					if i.Level == "WARN" {
						fmt.Fprintf(stderr, "⚠️  spec %s: %s\n", i.Field, i.Message)
					}
				}
			}

			rep := analyzeLayoutScore(snap, s0, opts)

			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(map[string]any{"report": rep}); err != nil {
					return err
				}
			} else {
				renderLayoutScore(rep, showAll, stdout)
			}

			// 退出码语义：只有明确设了 --min-score 才把「分数不够」当失败；
			// 硬错（blocking）永远非零 —— 那不是分数问题，是板子错了。
			if len(rep.Blocking) > 0 {
				return fmt.Errorf("layout has %d blocking issue(s)", len(rep.Blocking))
			}
			if minScore > 0 && rep.Overall < minScore {
				return fmt.Errorf("layout score %.1f below --min-score %.1f", rep.Overall, minScore)
			}
			return nil
		},
	}
	c.Flags().StringVar(&specPath, "spec", "", "S0 spec JSON — unlocks the intent dimensions (flow-order, internal connectors)")
	c.Flags().StringVar(&fromPath, "from", "", "score a snapshot file from `pcb dump` instead of the live board (offline)")
	c.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	c.Flags().Float64Var(&minScore, "min-score", 0, "fail (non-zero exit) when the weighted overall falls below this")
	c.Flags().StringSliceVar(&only, "only", nil, "score only these dimensions (comma-separated)")
	c.Flags().StringSliceVar(&skip, "skip", nil, "skip these dimensions (comma-separated)")
	c.Flags().StringArrayVar(&weights, "weight", nil, "override a dimension weight, e.g. --weight tidy=1.5 (repeatable)")
	c.Flags().Float64Var(&gridMil, "grid", 0, "tidiness grid in mil (default 5 — catches the sub-mil drift auto-place leaves)")
	c.Flags().Float64Var(&minGap, "min-gap", 0, "assembly spacing in mil (default: the board's live clearance rule)")
	c.Flags().BoolVar(&showAll, "all", false, "list every contributor and finding instead of the top few")
	return c
}

// csvSet 把重复/逗号分隔的 flag 值折成集合。
func csvSet(vals []string) map[string]bool {
	if len(vals) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out[part] = true
			}
		}
	}
	return out
}

// parseWeightOverrides 解析 --weight dim=val。
func parseWeightOverrides(vals []string) (map[string]float64, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := map[string]float64{}
	for _, v := range vals {
		name, num, ok := strings.Cut(v, "=")
		if !ok {
			return nil, fmt.Errorf("--weight wants dim=value, got %q", v)
		}
		name = strings.TrimSpace(name)
		if _, known := defaultDimensionWeights[name]; !known {
			return nil, fmt.Errorf("--weight: unknown dimension %q (want one of %s)", name, strings.Join(dimensionOrder, ", "))
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("--weight %s: want a non-negative number, got %q", name, num)
		}
		out[name] = f
	}
	return out, nil
}

// validateDimSelectors 拒绝拼错的维度名。
//
// 为什么不静默忽略：`--only tidyness` 打错一个字母就会变成"一维都不算"，报告显示
// 全部 skipped，用户会以为是数据问题而不是自己拼错了。
func validateDimSelectors(sets ...map[string]bool) error {
	known := map[string]bool{}
	for _, d := range dimensionOrder {
		known[d] = true
	}
	for _, set := range sets {
		for name := range set {
			if !known[name] {
				return fmt.Errorf("unknown dimension %q (want one of %s)", name, strings.Join(dimensionOrder, ", "))
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

const scoreTopN = 3 // 默认每维只列前几个归因；--all 全列

// renderLayoutScore 输出人读报告。
//
// 排版上刻意把 skipped 的维也列出来（灰着但在），因为「这维没测」本身就是结论 ——
// 隐藏它会让 7 维 90 分读起来像全面体检，实际有两维压根没跑。
func renderLayoutScore(rep layoutScoreReport, showAll bool, w io.Writer) {
	fmt.Fprintf(w, "\n布局评分  %.1f/100  [%s]  —— %d 器件\n", rep.Overall, rep.Verdict, rep.ComponentCount)
	fmt.Fprintf(w, "%s\n", strings.Repeat("─", 72))

	if len(rep.Blocking) > 0 {
		fmt.Fprintf(w, "\n⛔ 阻塞项 %d 条（不计入加权，一票否决）\n", len(rep.Blocking))
		n := len(rep.Blocking)
		if !showAll && n > scoreTopN {
			n = scoreTopN
		}
		for _, f := range rep.Blocking[:n] {
			fmt.Fprintf(w, "   %-18s %s\n", f.Type, f.Message)
		}
		if n < len(rep.Blocking) {
			fmt.Fprintf(w, "   … 另有 %d 条（--all 全列）\n", len(rep.Blocking)-n)
		}
	}

	fmt.Fprintf(w, "\n%-22s %-6s %-7s %s\n", "维度", "分数", "权重", "说明")
	for _, d := range rep.Dimensions {
		switch d.Status {
		case dimSkipped:
			fmt.Fprintf(w, "%-22s %-6s %-7s %s\n", dimLabel(d), "—", "—", "未测："+d.Reason)
		default:
			mark := ""
			if d.Status == dimDegraded {
				mark = " ~" // 近似输入
			}
			note := d.Reason
			if note == "" {
				note = scoreBar(d.Score)
			}
			fmt.Fprintf(w, "%-22s %-6.1f %-7.1f %s%s\n", dimLabel(d), d.Score, d.Weight, note, mark)
		}
	}

	// 归因：只展开拖后腿的维，因为「哪几个器件拉低了哪一维」才是行动依据。
	for _, d := range rep.Dimensions {
		if d.Status == dimSkipped || len(d.Contributors) == 0 {
			continue
		}
		if !showAll && d.Score >= 90 {
			continue
		}
		fmt.Fprintf(w, "\n  ↓ %s（%.1f）拉低它的是：\n", dimLabel(d), d.Score)
		n := len(d.Contributors)
		if !showAll && n > scoreTopN {
			n = scoreTopN
		}
		for _, c := range d.Contributors[:n] {
			detail := c.Detail
			if detail != "" {
				detail = " — " + detail
			}
			fmt.Fprintf(w, "     %-10s −%.1f%s\n", c.Designator, c.Penalty, detail)
		}
		if n < len(d.Contributors) {
			fmt.Fprintf(w, "     … 另有 %d 个（--all 全列）\n", len(d.Contributors)-n)
		}
	}

	if len(rep.Partial) > 0 {
		fmt.Fprintf(w, "\n数据降级：\n")
		for _, p := range rep.Partial {
			fmt.Fprintf(w, "   ⚠️  %s\n", p)
		}
	}
	fmt.Fprintf(w, "\n%s\n\n", rep.Summary)
}

// dimLabel 是「中文标题(id)」的展示名 —— id 要露出来，因为 --only/--skip 和
// playbook assert 用的是 id 而不是标题。
func dimLabel(d scoreDimension) string {
	title := d.Title
	if title == "" {
		title = d.ID
		return title
	}
	return title + "(" + d.ID + ")"
}

// scoreBar 是一个粗略的视觉条，让一屏里的强弱维一眼可辨。
func scoreBar(v float64) string {
	filled := int(v / 10)
	if filled < 0 {
		filled = 0
	}
	if filled > 10 {
		filled = 10
	}
	return strings.Repeat("█", filled) + strings.Repeat("·", 10-filled)
}
