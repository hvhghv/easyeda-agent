package app

// cmd_pcb_floorplan.go — `easyeda pcb floorplan`：从 S0 的 flow 推出布局骨架。
//
// 范围声明（诚实优先于好看）：这一版是**只读规划器**。它产出有序功能带 + 该钉边的
// 连接器目标点，供人审阅和后续消费；它**不搬器件**。真正落笔仍走 place-constrained
// 的四档流程。之所以先只做规划：概念上 floorplan 决定的是"板子怎么分区"，这件事
// 错了后面搬多少次件都是白搬，值得先让人看一眼再执行。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

func newPcbFloorplanCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var (
		specPath string
		fromPath string
		asJSON   bool
		margin   float64
		minBand  float64
	)
	c := &cobra.Command{
		Use:   "floorplan",
		Short: "Plan ordered functional bands from the S0 signal flow (read-only)",
		Long: "把 S0 spec 的 `flow`（如 [\"POWER\",\"MCU\",\"RF\",\"ANT\"]）沿板子的流向轴切成\n" +
			"**有序**功能带，带宽按各段器件面积分配，并把 spec 明确声明了 edge 的连接器\n" +
			"钉到目标边。\n\n" +
			"为什么不用现有的 `pcb zones`：那套词汇是固定的 3×2 九宫格，能表达「MCU 在\n" +
			"中间」这种位置意图，但表达不了顺序（谁在谁之后）、比例（166 器件的域不该和\n" +
			"3 器件的域等宽）和段数（flow 可能有 2 段也可能 6 段）。两者并存，不互相取代。\n\n" +
			"**只读**：本命令不搬器件。它给出的是布局骨架，落笔仍走 `pcb place-constrained`。\n\n" +
			"方向不强制：板子从右到左走 电源→天线 与从左到右一样好。已有器件分布更接近\n" +
			"反向时按反向切带（输出 reversed=true），不会把一块本来就摆对的板翻过来重排。",
		Example: "  easyeda pcb floorplan --spec .easyeda/s0-ceshi.json\n" +
			"  easyeda pcb floorplan --spec s0.json --json\n" +
			"  easyeda pcb floorplan --spec s0.json --from board.json   # 离线\n" +
			"  easyeda pcb floorplan --spec s0.json --margin 200        # 小板收窄留白",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if specPath == "" {
				return fmt.Errorf("--spec is required: the floorplan comes from the S0 flow, there is nothing to infer it from")
			}
			raw, err := os.ReadFile(specPath)
			if err != nil {
				return fmt.Errorf("read spec: %w", err)
			}
			s, err := spec.Parse(raw)
			if err != nil {
				return err
			}
			issues := spec.Validate(s)
			if spec.HasErrors(issues) {
				for _, i := range issues {
					if i.Level == "ERROR" {
						fmt.Fprintf(stderr, "❌ spec %s: %s\n", i.Field, i.Message)
					}
				}
				return fmt.Errorf("S0 spec has errors; run `easyeda spec validate %s`", specPath)
			}

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
			} else if snap, err = fetchBoardSnapshot(cfg, *window, boardSnapshotOpts{}); err != nil {
				return err
			}

			opts := defaultFloorplanOpts()
			if margin > 0 {
				opts.MarginMil = margin
			}
			if minBand > 0 {
				opts.MinBandMil = minBand
			}
			rep := planFloorplan(s, snap, opts)

			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(map[string]any{"floorplan": rep}); err != nil {
					return err
				}
			} else {
				renderFloorplan(rep, stdout)
			}
			if !rep.OK {
				return fmt.Errorf("floorplan not produced: %s", rep.Summary)
			}
			return nil
		},
	}
	c.Flags().StringVar(&specPath, "spec", "", "S0 spec JSON with a `flow` of at least 2 stages (required)")
	c.Flags().StringVar(&fromPath, "from", "", "plan against a `pcb dump` snapshot instead of the live board")
	c.Flags().BoolVar(&asJSON, "json", false, "emit the plan as JSON")
	c.Flags().Float64Var(&margin, "margin", 0, "board-edge margin in mil (default 300 — leaves room for connectors and a routing channel)")
	c.Flags().Float64Var(&minBand, "min-band", 0, "minimum band width in mil (default 400 — a tiny stage must not collapse to zero)")
	return c
}

func renderFloorplan(rep floorplanReport, w io.Writer) {
	dir := "正向"
	if rep.Reversed {
		dir = "反向（板上已是这个方向，顺着它切）"
	}
	fmt.Fprintf(w, "\n布局骨架  轴=%s  %s\n%s\n", rep.Axis, dir, strings.Repeat("─", 72))
	if len(rep.Bands) == 0 {
		fmt.Fprintf(w, "%s\n\n", rep.Summary)
		return
	}
	fmt.Fprintf(w, "\n%-12s %-10s %-24s %s\n", "功能域", "面积mil²", "矩形(minX,minY)-(maxX,maxY)", "器件")
	for _, b := range rep.Bands {
		parts := strings.Join(b.Parts, ",")
		if len(parts) > 34 {
			parts = parts[:31] + "…"
		}
		if parts == "" {
			parts = "(空)"
		}
		fmt.Fprintf(w, "%-12s %-10.0f (%.0f,%.0f)-(%.0f,%.0f)%s %s\n",
			b.Kind, b.AreaMil2, b.MinX, b.MinY, b.MaxX, b.MaxY,
			strings.Repeat(" ", maxInt(0, 24-len(fmt.Sprintf("(%.0f,%.0f)-(%.0f,%.0f)", b.MinX, b.MinY, b.MaxX, b.MaxY)))),
			parts)
	}
	if len(rep.Pins) > 0 {
		fmt.Fprintf(w, "\n钉边连接器（仅 spec 显式声明 ref+edge 的；边序是装配体验，工具不猜）:\n")
		for _, p := range rep.Pins {
			facing := p.Facing
			if facing == "" {
				facing = "—"
			}
			fmt.Fprintf(w, "   %-8s → %-7s facing=%-12s @(%.0f,%.0f)\n", p.Designator, p.Edge, facing, p.TargetX, p.TargetY)
		}
	}
	if len(rep.Unzoned) > 0 {
		show := rep.Unzoned
		more := 0
		if len(show) > 12 {
			more = len(show) - 12
			show = show[:12]
		}
		fmt.Fprintf(w, "\n未归属任何功能域的器件 %d 个: %s", len(rep.Unzoned), strings.Join(show, " "))
		if more > 0 {
			fmt.Fprintf(w, " …+%d", more)
		}
		fmt.Fprintln(w)
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintf(w, "\n⚠️  %s\n", warn)
	}
	fmt.Fprintf(w, "\n%s\n只读规划：器件不会被搬动，落笔走 `pcb place-constrained`。\n\n", rep.Summary)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
