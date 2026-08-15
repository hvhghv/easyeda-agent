package app

// pcb_sync_designators.go — 修复 PCB 器件位号变成占位符（`U?` / `C?` / `RF?`）的问题。
//
// ── 事故与根因（真机二分定位，2026-08-09）───────────────────────────────────
//
// 一块 166 器件的真板在 `pcb import-changes` 后位号 166/166 全变占位符。最初
// 归因于平台导入行为——**错了**。真机控制变量实验（ceshi 六件小板）钉死了根因：
//
//   - 裸 `eda.pcb_Document.importChanges()` + 手动点「应用修改」→ 位号全对。
//     平台的导入一直是对的，还顺带给两侧铸造 uniqueId（gge*）。
//   - 元凶是我们自己的 `pcb.component.attrs_backfill`（import-changes 会自动跑）：
//     器件库记录的 otherProperty 里带着 `Designator: "C?"`（库自己的占位位号），
//     merge「填空值」把它灌进实例，平台把 otherProperty.Designator 同步成图元
//     位号 → 一板位号当场全灭，每件变成各自库记录的占位前缀（U?/C?/RF?）。
//     安静板上单独跑一次 sync-attrs 即 100% 复现，与时序无关。
//
// 根因已在连接器侧根治（attrs_backfill 剔除身份键 + 同一 modify 显式回传位号）。
// 本文件是**殿后防线 + 存量修复**：老版本毁过的板用它修回来；未来任何整包
// otherProperty 写入若再毁位号，排在最后的它也能当场修回。
//
// 位号的分量：模块归属（S0 spec 的 modules[].parts）、保护件前缀（F*/D*/TVS*）、
// 去耦判定、`pcb check` 定位、BOM 全按位号索引。位号一丢，这些规则不是失灵，
// 而是静默按错误分类算出一份看起来正常的报告。
//
// ── 为什么用 uniqueId 修 ───────────────────────────────────────────────────
//
// 原理图和 PCB 是两个文档，各自 mint 自己的 primitiveId，互相对不上。但平台在
// 首次 sch→PCB 导入时给每个器件铸造的 `uniqueId`（`gge*`）**跨文档共用同一套
// 命名空间**——实测 166/166 完全匹配。它就是唯一可靠的 schematic↔PCB 连接键。
// （API 手放且从未导入过的原理图器件 uniqueId 为空——首次导入才铸造。）
//
// ── 约束 ──────────────────────────────────────────────────────────────────
//
// 只回填**占位符位号**（含 `?`）。已经有真实位号的器件一律不碰 —— 用户可能在 PCB
// 侧手工改过位号，那是他的决定，不该被原理图静默覆盖。
// 每笔写入都回读验证（平台的 modify 有静默 no-op 前科），只有读回一致才计 Repaired。

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
	PCBTotal       int      `json:"pcbTotal"`
	Placeholder    int      `json:"placeholderDesignators"`
	Matched        int      `json:"matched"`
	Repaired       int      `json:"repaired"`
	Unmatched      []string `json:"unmatched,omitempty"`
	SchUnannotated []string `json:"schematicUnannotated,omitempty"`
	Failed         []string `json:"failed,omitempty"`
	SchematicSeen  int      `json:"schematicComponents"`
	DryRun         bool     `json:"dryRun,omitempty"`
	Saved          bool     `json:"saved,omitempty"`
	Summary        string   `json:"summary"`
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

// schDesignators 是原理图侧按 uniqueId 索引的位号表。Real 只收真实位号；
// Placeholder 收「有 uniqueId 但位号本身还是占位符」的件 —— 这类件修不了，
// 但必须与「原理图里根本找不到」区分开，否则会把用户引向错误的排查方向
// （前者该去标注原理图，后者该查两份文档是否同源）。
type schDesignators struct {
	Real        map[string]string
	Placeholder map[string]string
	Seen        int
}

// fetchSchematicUniqueIDs 读**全部页**的 uniqueId → 位号。
//
// allPages + tagPages 一起给：allPages 让 getAll 跨页取，tagPages 让连接器先把
// 每一页都激活一遍。后者不是可有可无的 —— 平台的页是懒加载的，没被本会话打开过
// 的页在 getAll(allPages) 里根本不出现（这个坑在多页板上吃过一次）。
func fetchSchematicUniqueIDs(cfg *appConfig, window string) (schDesignators, error) {
	sd := schDesignators{Real: map[string]string{}, Placeholder: map[string]string{}}
	res, err := requestAction(cfg, "schematic.components.list", window,
		map[string]any{"allPages": true, "tagPages": true})
	if err != nil {
		return sd, fmt.Errorf("list schematic components: %w", err)
	}
	raw, _ := mnav(res.Result, "components").([]any)
	sd.Seen = len(raw)
	for _, ri := range raw {
		cm, ok := ri.(map[string]any)
		if !ok {
			continue
		}
		uid := strings.TrimSpace(asString(cm["uniqueId"]))
		des := strings.TrimSpace(asString(cm["designator"]))
		if uid == "" || des == "" {
			continue
		}
		if isPlaceholderDesignator(des) {
			sd.Placeholder[uid] = des
		} else {
			sd.Real[uid] = des
		}
	}
	return sd, nil
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

	sch, err := fetchSchematicUniqueIDs(cfg, window)
	if err != nil {
		return rep, err
	}
	rep.SchematicSeen = sch.Seen

	type fix struct{ pid, des, uid string }
	var fixes []fix
	for _, r := range broken {
		if des, ok := sch.Real[r.UID]; ok && r.UID != "" {
			fixes = append(fixes, fix{pid: r.PID, des: des, uid: r.UID})
			continue
		}
		if schDes, ok := sch.Placeholder[r.UID]; ok && r.UID != "" {
			// 原理图侧同 uniqueId 的件位号也还是占位符——不是匹配问题，是原理图
			// 本身没标注。修不了，但要指对方向。
			rep.SchUnannotated = append(rep.SchUnannotated, fmt.Sprintf("%s (schematic: %s)", r.UID, schDes))
			continue
		}
		label := r.UID
		if label == "" {
			label = r.Des + " (no uniqueId)"
		}
		rep.Unmatched = append(rep.Unmatched, label)
	}
	sort.Slice(fixes, func(i, j int) bool { return fixes[i].des < fixes[j].des })
	sort.Strings(rep.Unmatched)
	sort.Strings(rep.SchUnannotated)
	rep.Matched = len(fixes)

	if dryRun {
		rep.Summary = fmt.Sprintf("dry run — would repair %d/%d placeholder designator(s)", rep.Matched, rep.Placeholder)
		return rep, nil
	}

	written := make(map[string]string, len(fixes)) // pid → 期望位号
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
		written[f.pid] = f.des
	}

	// 回读验证：平台的写 API 有「返回成功但没落」的前科（delete 静默 no-op、
	// 位号唯一性避让都可能改写结果）。只有读回一致的才算修好。
	if len(written) > 0 {
		verify, verr := fetchPcbDesignators(cfg, window)
		if verr != nil {
			// 读失败不равно写失败：如实报出去，别把 Repaired 归零冤枉写入。
			fmt.Fprintf(stderr, "⚠ post-write verification read failed: %v — repairs were issued but are unverified\n", verr)
			rep.Repaired = len(written)
		} else {
			byPID := make(map[string]string, len(verify))
			for _, r := range verify {
				byPID[r.PID] = r.Des
			}
			for pid, want := range written {
				if got := byPID[pid]; got == want {
					rep.Repaired++
				} else {
					rep.Failed = append(rep.Failed, fmt.Sprintf("%s: write reported ok but read back %q", want, got))
				}
			}
			sort.Strings(rep.Failed)
		}
	}

	// 修好了就立刻落一个已知良好检查点——回填结果不该只活在 autosave 的
	// debounce 窗口里（窗口期内崩溃/重载会整批回退）。best-effort。
	if rep.Repaired > 0 {
		if _, serr := requestAction(cfg, "pcb.save", window, nil); serr == nil {
			rep.Saved = true
		} else {
			fmt.Fprintf(stderr, "⚠ pcb.save checkpoint after repair failed: %v — repairs live in memory until the next save\n", serr)
		}
	}

	rep.Summary = fmt.Sprintf("repaired %d/%d placeholder designator(s) from %d schematic component(s)",
		rep.Repaired, rep.Placeholder, sch.Seen)
	if len(rep.Unmatched) > 0 {
		rep.Summary += fmt.Sprintf("; %d unmatched", len(rep.Unmatched))
	}
	if len(rep.SchUnannotated) > 0 {
		rep.Summary += fmt.Sprintf("; %d unannotated in the schematic", len(rep.SchUnannotated))
	}
	if len(rep.Failed) > 0 {
		rep.Summary += fmt.Sprintf("; %d failed", len(rep.Failed))
	}
	return rep, nil
}

func newPcbSyncDesignatorsCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var dryRun, asJSON bool
	c := &cobra.Command{
		Use:   "sync-designators",
		Short: "Repair placeholder PCB designators (U? / C?) from the schematic, matched by uniqueId",
		Long: "Repairs PCB designators that were wiped to placeholders (`U?` / `C?` / `RF?`).\n\n" +
			"Root cause (found by controlled real-machine bisection): the OLD attrs backfill\n" +
			"merged the device library's own placeholder `Designator` key (\"C?\") into each\n" +
			"instance's otherProperty, and the platform syncs that key into the primitive's\n" +
			"designator — wiping 166/166 on a real board. The leak is fixed in the connector;\n" +
			"this command repairs boards damaged by older versions and stands rear-guard\n" +
			"after every `pcb import-changes`.\n\n" +
			"Designators feed almost every rule in this toolchain — S0 spec module membership,\n" +
			"protection-part prefixes (F*/D*/TVS*), decoupling detection, `pcb check` finding\n" +
			"locations, the BOM. Losing them doesn't just disable those rules, it can make\n" +
			"them classify silently wrong.\n\n" +
			"Matching key: `uniqueId` — the platform keeps it in ONE namespace across both\n" +
			"documents (primitiveId does not; each document mints its own). Only PLACEHOLDER\n" +
			"designators are touched: a real designator you set by hand on the PCB is a\n" +
			"decision, and is never overwritten by the schematic. Every write is verified by\n" +
			"read-back; repaired boards get an immediate `pcb.save` checkpoint.",
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
				if eerr := enc.Encode(map[string]any{"syncDesignators": rep}); eerr != nil {
					return eerr
				}
			} else {
				renderSyncDesignators(rep, stdout)
			}
			// 失败必须以非零退出码暴露——JSON 模式也一样（gate 脚本靠退出码）。
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
	if len(rep.SchUnannotated) > 0 {
		n := len(rep.SchUnannotated)
		show := rep.SchUnannotated
		if n > 6 {
			show = show[:6]
		}
		fmt.Fprintf(w, "⚠️  %d 个器件在原理图里也还是占位位号（先标注原理图再重跑）: %s",
			n, strings.Join(show, " "))
		if n > 6 {
			fmt.Fprintf(w, " …+%d", n-6)
		}
		fmt.Fprintln(w)
	}
	for _, f := range rep.Failed {
		fmt.Fprintf(w, "❌ %s\n", f)
	}
	if rep.Saved {
		fmt.Fprintln(w, "✓ pcb.save checkpoint written")
	}
	fmt.Fprintf(w, "%s\n", rep.Summary)
}
