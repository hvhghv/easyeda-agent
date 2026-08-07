package app

// cmd_pcb_dump.go — `easyeda pcb dump`：把整块板的只读几何拉成一份自包含 JSON。
//
// 存在理由是**金标准好板回归**(#167 第五层 LEARNING)：拿一块人类公认的好板跑
// layout-score，它就该得高分；某一维在好板上得低分 → 是度量错了，回去校准。
// 要让这条闭环成立，好板必须能变成**离线 fixture**——否则每次改权重都得开着
// EasyEDA 手动重跑，回归就形同虚设。
//
// 注意与 `pcb stage-snapshot` 的区别：那个抓的是 PNG 截图（给人看的把关帧），
// 这个抓的是结构化几何（给 CLI/单测吃的数据）。两者名字接近但用途不同。
//
// dump 出来的文件可以直接喂回：`easyeda pcb layout-score --from board.json`
// ——不需要连编辑器，CI 里也能跑。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func newPcbDumpCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var (
		outPath  string
		noSilk   bool
		noRules  bool
		noLayers bool
		label    string
	)
	c := &cobra.Command{
		Use:   "dump",
		Short: "Dump the board's read-only geometry to a self-contained JSON snapshot",
		Long: "Capture components (anchor/rotation/locked/bbox/pads), board outline, silkscreen,\n" +
			"live DRC rules and copper layer count in ONE file.\n\n" +
			"The snapshot is what `" + "pcb layout-score --from <file>" + "` replays offline, so a\n" +
			"reference board can become a regression fixture that needs no live editor.\n\n" +
			"Board outline requires the PCB to be the FOREGROUND document (the platform\n" +
			"returns null otherwise) — the snapshot records that degradation under\n" +
			"`partial[]` rather than silently pretending the board has no edges.",
		Example: "  # 抓当前板为 fixture\n" +
			"  easyeda pcb dump --project ceshi --out /tmp/ceshi-board.json\n\n" +
			"  # 只要器件+板框（省两次往返）\n" +
			"  easyeda pcb dump --no-silk --no-rules --no-layers\n\n" +
			"  # 离线重放打分\n" +
			"  easyeda pcb layout-score --from /tmp/ceshi-board.json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			snap, err := fetchBoardSnapshot(cfg, *window, boardSnapshotOpts{
				withSilk:   !noSilk,
				withRules:  !noRules,
				withLayers: !noLayers,
			})
			if err != nil {
				return err
			}
			if label != "" {
				snap.Project = label
			} else if cfg.project != "" {
				snap.Project = cfg.project
			}

			blob, err := json.MarshalIndent(snap, "", "  ")
			if err != nil {
				return err
			}
			blob = append(blob, '\n')
			if outPath == "" {
				_, werr := stdout.Write(blob)
				return werr
			}
			if err := os.WriteFile(outPath, blob, 0o644); err != nil {
				return fmt.Errorf("write snapshot: %w", err)
			}
			fmt.Fprintf(stderr, "✅ %d component(s), outline=%s → %s\n",
				len(snap.Components), outlineSourceLabel(snap), outPath)
			for _, p := range snap.Partial {
				fmt.Fprintf(stderr, "⚠️  %s\n", p)
			}
			return nil
		},
	}
	c.Flags().StringVar(&outPath, "out", "", "write the snapshot to this file (default: stdout)")
	c.Flags().StringVar(&label, "label", "", "record this name as the snapshot's project label")
	c.Flags().BoolVar(&noSilk, "no-silk", false, "skip silkscreen (drops the silk-consistency dimensions)")
	c.Flags().BoolVar(&noRules, "no-rules", false, "skip live DRC rules (thresholds fall back to the JLCPCB baseline)")
	c.Flags().BoolVar(&noLayers, "no-layers", false, "skip copper layer count")
	return c
}

// outlineSourceLabel 是 dump 摘要里那一格的人读标签：说清板框是真多边形还是
// AABB 近似，还是压根没读到——异形板上这三者的打分含义完全不同。
func outlineSourceLabel(s *boardSnapshot) string {
	if s.Outline == nil {
		return "unavailable"
	}
	return s.Outline.Source
}
