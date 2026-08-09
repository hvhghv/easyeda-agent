package app

// pcb_sync_designators.go — 修复 `pcb import-changes` 之后 PCB 器件位号变成占位符
// （`U?` / `C?` / `RF?`）的问题。
//
// ── 现象与实测 ─────────────────────────────────────────────────────────────
//
// 在一块 166 器件的真板上把 PCB 清空后重新 `import-changes`：器件、封装、
// manufacturerId、supplierId 全部正确落到 PCB 上，**唯独位号 166/166 全是
// 占位符**。原理图侧位号完好（0 个带 `?`），所以不是设计的问题，是导入这一步
// 没把位号带过来。
//
// 后果比看上去严重：位号是这套工具链几乎所有规则的输入 —— 模块归属（S0 spec 的
// modules[].parts 按位号写）、保护件识别（F*/D*/TVS* 前缀）、去耦判定、
// `pcb check` 的 finding 定位、BOM。位号一丢，这些规则要么全部失灵，要么更糟：
// 静默地按错误的分类算出一份看起来正常的报告。
//
// ── 为什么用 uniqueId 修 ───────────────────────────────────────────────────
//
// 原理图和 PCB 是两个文档，各自 mint 自己的 primitiveId，互相对不上。但平台给
// 每个器件分配的 `uniqueId`（`gge*`）**跨文档共用同一套命名空间** —— 实测在这块
// 166 器件的板上 166/166 完全匹配。它就是唯一可靠的 schematic↔PCB 连接键。
//
// 原理图侧的 `serializeComponent` 一直在返回 uniqueId；PCB 侧此前没有，本次补上。
//
// ── 修不了什么（诚实边界）─────────────────────────────────────────────────
//
// 这是**兜底修复**，不是根因修复：位号为什么没被 importChanges 带过来，属于平台
// 行为，我们看不到也改不了它的内部。所以这里做的是「导入后立刻把位号补回去」，
// 而不是「让导入不丢位号」。真正的根因需要平台侧确认（值得提 issue）。
//
// 只回填**占位符位号**（含 `?`）。已经有真实位号的器件一律不碰 —— 用户可能在 PCB
// 侧手工改过位号，那是他的决定，不该被原理图静默覆盖。

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// syncDesignatorsResult 是一次回填的结果。
type syncDesignatorsResult struct {
	PCBTotal      int      `json:"pcbTotal"`
	Placeholder   int      `json:"placeholderDesignators"`
	Matched       int      `json:"matched"`
	Repaired      int      `json:"repaired"`
	Unmatched     []string `json:"unmatched,omitempty"`
	Failed        []string `json:"failed,omitempty"`
	SchematicSeen int      `json:"schematicComponents"`
	DryRun        bool     `json:"dryRun,omitempty"`
	Summary       string   `json:"summary"`
}

// isPlaceholderDesignator 判断位号是不是平台的未分配占位符。
//
// EasyEDA 用 `<前缀>?` 表示「这个器件还没分配位号」——`U?` / `C?` / `RF?`。
// 判据刻意宽松（只看有没有 `?`）：占位符的前缀跟着封装类别走，穷举前缀既不可能
// 也没必要，而真实位号里出现 `?` 是不合法的。
func isPlaceholderDesignator(d string) bool {
	return strings.Contains(strings.TrimSpace(d), "?")
}

// pcbDesignatorRow 是一个 PCB 器件的位号身份。UID 可能为空（旧连接器不返回它）。
type pcbDesignatorRow struct {
	PID string
	UID string
	Des string
}

// fetchPcbDesignators 读 PCB 全部器件的 (primitiveId, uniqueId, 位号)。
//
// **不**在这里因为缺 uniqueId 就丢弃器件：占位符的判定只需要位号，而「这块板到底
// 有没有活要干」应该先于「连接器够不够新」回答 —— 否则一块位号本来就正常的板，
// 在旧连接器上会被误报成错误。
func fetchPcbDesignators(cfg *appConfig, window string) ([]pcbDesignatorRow, error) {
	res, err := requestAction(cfg, "pcb.components.list", window, nil)
	if err != nil {
		return nil, fmt.Errorf("list PCB components: %w", err)
	}
	raw, _ := mnav(res.Result, "components").([]any)
	out := make([]pcbDesignatorRow, 0, len(raw))
	for _, ri := range raw {
		cm, ok := ri.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, pcbDesignatorRow{
			PID: asString(cm["primitiveId"]),
			UID: strings.TrimSpace(asString(cm["uniqueId"])),
			Des: asString(cm["designator"]),
		})
	}
	return out, nil
}

// fetchSchematicUniqueIDs 读**全部页**的 uniqueId → 位号。
//
// allPages + tagPages 一起给：allPages 让 getAll 跨页取，tagPages 让连接器先把
// 每一页都激活一遍。后者不是可有可无的 —— 平台的页是懒加载的，没被本会话打开过
// 的页在 getAll(allPages) 里根本不出现（这个坑在多页板上吃过一次）。
func fetchSchematicUniqueIDs(cfg *appConfig, window string) (map[string]string, int, error) {
	res, err := requestAction(cfg, "schematic.components.list", window,
		map[string]any{"allPages": true, "tagPages": true})
	if err != nil {
		return nil, 0, fmt.Errorf("list schematic components: %w", err)
	}
	raw, _ := mnav(res.Result, "components").([]any)
	out := make(map[string]string, len(raw))
	for _, ri := range raw {
		cm, ok := ri.(map[string]any)
		if !ok {
			continue
		}
		uid := strings.TrimSpace(asString(cm["uniqueId"]))
		des := strings.TrimSpace(asString(cm["designator"]))
		if uid != "" && des != "" && !isPlaceholderDesignator(des) {
			out[uid] = des
		}
	}
	return out, len(raw), nil
}

// runSyncDesignators 执行回填。
func runSyncDesignators(cfg *appConfig, window string, dryRun bool, stderr io.Writer) (syncDesignatorsResult, error) {
	var rep syncDesignatorsResult
	rep.DryRun = dryRun

	rows, err := fetchPcbDesignators(cfg, window)
	if err != nil {
		return rep, err
	}
	rep.PCBTotal = len(rows)
	if len(rows) == 0 {
		rep.Summary = "no components on the PCB"
		return rep, nil
	}

	// 先问「有没有活要干」，再问「工具够不够」：全是真实位号就直接收工，
	// 既不必去读原理图（读全页会 cycle 文档，有代价），也不必要求新连接器。
	var broken []pcbDesignatorRow
	for _, r := range rows {
		if isPlaceholderDesignator(r.Des) {
			broken = append(broken, r)
		}
	}
	rep.Placeholder = len(broken)
	if len(broken) == 0 {
		rep.Summary = fmt.Sprintf("all %d designator(s) already real — nothing to repair", len(rows))
		return rep, nil
	}

	// 到这里确实有占位符要修，才需要 uniqueId 这个连接键。
	haveUID := false
	for _, r := range broken {
		if r.UID != "" {
			haveUID = true
			break
		}
	}
	if !haveUID {
		rep.Summary = fmt.Sprintf("%d placeholder designator(s) need repair, but the connector reports no uniqueId — re-import the .eext (this repair matches on uniqueId)", len(broken))
		return rep, fmt.Errorf("%s", rep.Summary)
	}

	schByUID, schSeen, err := fetchSchematicUniqueIDs(cfg, window)
	if err != nil {
		return rep, err
	}
	rep.SchematicSeen = schSeen

	type fix struct{ pid, des, uid string }
	var fixes []fix
	for _, r := range broken {
		des, ok := schByUID[r.UID]
		if !ok || r.UID == "" {
			label := r.UID
			if label == "" {
				label = r.Des + "(no uniqueId)"
			}
			rep.Unmatched = append(rep.Unmatched, label)
			continue
		}
		fixes = append(fixes, fix{pid: r.PID, des: des, uid: r.UID})
	}
	sort.Slice(fixes, func(i, j int) bool { return fixes[i].des < fixes[j].des })
	sort.Strings(rep.Unmatched)
	rep.Matched = len(fixes)

	if dryRun {
		rep.Summary = fmt.Sprintf("dry run — would repair %d/%d placeholder designator(s)", rep.Matched, rep.Placeholder)
		return rep, nil
	}

	for _, f := range fixes {
		if f.pid == "" {
			rep.Failed = append(rep.Failed, f.des+" (no primitiveId)")
			continue
		}
		_, aerr := requestAction(cfg, "pcb.component.modify", window, map[string]any{
			"primitiveId": f.pid,
			"patch":       map[string]any{"designator": f.des},
		})
		if aerr != nil {
			rep.Failed = append(rep.Failed, fmt.Sprintf("%s: %v", f.des, aerr))
			continue
		}
		rep.Repaired++
	}

	rep.Summary = fmt.Sprintf("repaired %d/%d placeholder designator(s) from %d schematic component(s)",
		rep.Repaired, rep.Placeholder, schSeen)
	if len(rep.Unmatched) > 0 {
		rep.Summary += fmt.Sprintf("; %d unmatched", len(rep.Unmatched))
	}
	return rep, nil
}

func newPcbSyncDesignatorsCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var dryRun, asJSON bool
	c := &cobra.Command{
		Use:   "sync-designators",
		Short: "Repair placeholder PCB designators (U? / C?) from the schematic, matched by uniqueId",
		Long: "`pcb import-changes` lands components, footprints and supplier ids correctly but\n" +
			"leaves every designator as a placeholder (`U?` / `C?` / `RF?`) — measured 166/166 on a\n" +
			"real board whose schematic had zero placeholder designators.\n\n" +
			"That matters more than it looks: designators feed almost every rule in this\n" +
			"toolchain — S0 spec module membership, protection-part prefixes (F*/D*/TVS*),\n" +
			"decoupling detection, `pcb check` finding locations, the BOM. Losing them doesn't\n" +
			"just disable those rules, it can make them classify silently wrong.\n\n" +
			"This repairs them by matching on `uniqueId`, which the platform keeps in ONE\n" +
			"namespace across both documents (primitiveId does not — each document mints its\n" +
			"own). Only PLACEHOLDER designators are touched: a real designator you set by hand\n" +
			"on the PCB is a decision, and is never overwritten by the schematic.\n\n" +
			"`pcb import-changes` runs this automatically; use this command standalone to repair\n" +
			"a board that was imported before the fix.",
		Example: "  easyeda pcb sync-designators --project ceshi\n" +
			"  easyeda pcb sync-designators --dry-run    # 先看会改多少\n" +
			"  easyeda pcb sync-designators --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, err := runSyncDesignators(cfg, *window, dryRun, stderr)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"syncDesignators": rep})
			}
			renderSyncDesignators(rep, stdout)
			if len(rep.Failed) > 0 {
				return fmt.Errorf("%d designator write(s) failed", len(rep.Failed))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be repaired without writing")
	c.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return c
}

func renderSyncDesignators(rep syncDesignatorsResult, w io.Writer) {
	fmt.Fprintf(w, "PCB 器件 %d · 占位位号 %d · 匹配 %d · 已修 %d\n",
		rep.PCBTotal, rep.Placeholder, rep.Matched, rep.Repaired)
	if len(rep.Unmatched) > 0 {
		n := len(rep.Unmatched)
		show := rep.Unmatched
		if n > 6 {
			show = show[:6]
		}
		fmt.Fprintf(w, "⚠️  %d 个 PCB 器件在原理图里找不到同 uniqueId 的对应件: %s",
			n, strings.Join(show, " "))
		if n > 6 {
			fmt.Fprintf(w, " …+%d", n-6)
		}
		fmt.Fprintln(w)
	}
	for _, f := range rep.Failed {
		fmt.Fprintf(w, "❌ %s\n", f)
	}
	fmt.Fprintf(w, "%s\n", rep.Summary)
}
