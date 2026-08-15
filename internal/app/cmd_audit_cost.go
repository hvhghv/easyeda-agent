package app

// cmd_audit_cost.go — `easyeda audit cost`:一次设计跑了多久、花了多少次调用、
// 其中多少是白花的。用户立项(2026-08-16):「耗时和 token 以后都要记录,用以改善」。
//
// 为什么值得单独做一条命令:首次统计 esp32Mini 的原理图 E2E 就翻出一个谁都没注意
// 的事实 —— 5466 次调用里 **3527 次(65%)是上下文探测**(document.current /
// schematic.pages.list / pcb.documents.list),它们不改变任何东西,只是每个 CLI 进程
// 启动时都要重新 resolve 一遍窗口和工程。这种浪费在单条命令的视角下完全不可见,
// 只有把一整场跑聚合起来才会显形。
//
// 三个指标是分开的,因为**改法不同**:
//   - 墙钟          —— 用户实际等了多久;
//   - daemon 侧耗时  —— 机器真在算的时间(优化调用次数/批量化);
//   - 两者之差      —— agent 在想/在改代码(优化流程与判据,不是优化 API)。
//
// token 不在审计日志里(那是 agent 侧的账),所以由调用方 `--tokens` 自报,和上面
// 三个机器指标一起落进台账。缺了就记 0,**绝不估算冒充实测**。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

// auditProbeActions 是**零信息上下文探测** —— CLI 每次启动 resolve 窗口/工程时打的
// 那几发。它们既不读设计数据也不改画布,却能占掉总调用量的大半;单独计一栏,
// 是为了让「真正的工作」和「进程启动开销」不再混在一个总数里。
var auditProbeActions = map[string]bool{
	"document.current":         true,
	"document.list":            true,
	"documents.list":           true,
	"schematic.pages.list":     true,
	"pcb.documents.list":       true,
	"schematic.documents.list": true,
}

// auditActionStat 是一个动作的聚合。
type auditActionStat struct {
	Action   string  `json:"action"`
	Calls    int     `json:"calls"`
	Failures int     `json:"failures"`
	Seconds  float64 `json:"seconds"`
}

// auditCostReport 是一场跑的成本画像。
type auditCostReport struct {
	Label   string `json:"label,omitempty"`
	Day     string `json:"day"`
	From    string `json:"from"`
	To      string `json:"to"`
	Project string `json:"project,omitempty"`

	WallMinutes   float64 `json:"wallMinutes"`
	DaemonMinutes float64 `json:"daemonMinutes"`
	// ThinkMinutes 是墙钟减去 daemon 侧 —— agent 思考 + 编译 + 人工介入的时间。
	ThinkMinutes float64 `json:"thinkMinutes"`

	Calls    int     `json:"calls"`
	Failures int     `json:"failures"`
	FailRate float64 `json:"failRate"`
	// Probes / ProbeShare 是上下文探测的次数与占比(见 auditProbeActions)。
	Probes     int     `json:"probes"`
	ProbeShare float64 `json:"probeShare"`
	// Mutations 是写动作次数 —— 「产出」的粗略度量,用来跟成本对比。
	Mutations int `json:"mutations"`

	// Tokens 由调用方自报(审计日志里没有);0 = 未记录,不是「零消耗」。
	Tokens int `json:"tokens"`

	Top      []auditActionStat `json:"topActions"`
	TopFails []auditActionStat `json:"topFailures"`
	Note     string            `json:"note,omitempty"`
}

// summarizeAuditCost 是纯核:把一段区间内的审计行折成成本画像。无 I/O,可单测。
func summarizeAuditCost(rows []auditRow, mutating map[string]bool) auditCostReport {
	rep := auditCostReport{}
	if len(rows) == 0 {
		return rep
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Ts.Before(rows[j].Ts) })
	first, last := rows[0].Ts, rows[len(rows)-1].Ts
	rep.Day = first.UTC().Format("2006-01-02")
	rep.From = first.UTC().Format("15:04:05")
	rep.To = last.UTC().Format("15:04:05")
	rep.WallMinutes = last.Sub(first).Minutes()

	type agg struct {
		calls, fails int
		ms           float64
	}
	byAction := map[string]*agg{}
	var totalMs float64
	for _, r := range rows {
		rep.Calls++
		if !r.OK {
			rep.Failures++
		}
		if auditProbeActions[r.Action] {
			rep.Probes++
		}
		if mutating[r.Action] {
			rep.Mutations++
		}
		totalMs += r.DurationMs
		a := byAction[r.Action]
		if a == nil {
			a = &agg{}
			byAction[r.Action] = a
		}
		a.calls++
		a.ms += r.DurationMs
		if !r.OK {
			a.fails++
		}
	}
	rep.DaemonMinutes = totalMs / 1000 / 60
	rep.ThinkMinutes = rep.WallMinutes - rep.DaemonMinutes
	if rep.ThinkMinutes < 0 {
		rep.ThinkMinutes = 0 // 并发调用可能让 daemon 侧总和超过墙钟
	}
	if rep.Calls > 0 {
		rep.FailRate = float64(rep.Failures) / float64(rep.Calls)
		rep.ProbeShare = float64(rep.Probes) / float64(rep.Calls)
	}

	stats := make([]auditActionStat, 0, len(byAction))
	for name, a := range byAction {
		stats = append(stats, auditActionStat{Action: name, Calls: a.calls, Failures: a.fails, Seconds: a.ms / 1000})
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Calls != stats[j].Calls {
			return stats[i].Calls > stats[j].Calls
		}
		return stats[i].Action < stats[j].Action
	})
	rep.Top = stats[:minInt(len(stats), 12)]

	fails := make([]auditActionStat, 0, len(stats))
	for _, s := range stats {
		if s.Failures > 0 {
			fails = append(fails, s)
		}
	}
	sort.Slice(fails, func(i, j int) bool { return fails[i].Failures > fails[j].Failures })
	rep.TopFails = fails[:minInt(len(fails), 8)]
	return rep
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// auditLedgerPath 是累积台账:每次 --record 追加一行,用来跨批次对比「有没有变好」。
func auditLedgerPath() string {
	return filepath.Join(filepath.Dir(defaultAuditDir()), "cost-ledger.jsonl")
}

func appendCostLedger(rep auditCostReport) error {
	path := auditLedgerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line := map[string]any{
		"recordedAt": time.Now().UTC().Format(time.RFC3339),
		"label":      rep.Label, "day": rep.Day, "from": rep.From, "to": rep.To,
		"project":       rep.Project,
		"wallMinutes":   costRound1(rep.WallMinutes),
		"daemonMinutes": costRound1(rep.DaemonMinutes),
		"thinkMinutes":  costRound1(rep.ThinkMinutes),
		"calls":         rep.Calls, "failures": rep.Failures,
		"failRate": costRound3(rep.FailRate), "probes": rep.Probes, "probeShare": costRound3(rep.ProbeShare),
		"mutations": rep.Mutations, "tokens": rep.Tokens, "note": rep.Note,
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func costRound1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
func costRound3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

// mutatingActionSet 从 **动作目录本身**取 Mutates 标志 —— 不在这里另抄一份写动作
// 清单。目录已经是 daemon 防抖 autosave 的判据来源,再抄一份就会有两套「什么算写」。
func mutatingActionSet() map[string]bool {
	out := map[string]bool{}
	for _, a := range protocol.AllActions() {
		if a.Mutates {
			out[a.Name] = true
		}
	}
	return out
}

// readCostLedger 读回台账(缺文件 = 空,不是错)。
func readCostLedger() ([]map[string]any, error) {
	b, err := os.ReadFile(auditLedgerPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

func renderCostReport(rep auditCostReport, stdout io.Writer) {
	title := rep.Label
	if title == "" {
		title = "(未命名)"
	}
	fmt.Fprintf(stdout, "audit cost — %s · %s %s→%s UTC\n\n", title, rep.Day, rep.From, rep.To)
	fmt.Fprintf(stdout, "  墙钟            %.1f 分钟\n", rep.WallMinutes)
	fmt.Fprintf(stdout, "  ├ daemon 侧     %.1f 分钟(%.0f%%)—— 机器真在算\n",
		rep.DaemonMinutes, pct(rep.DaemonMinutes, rep.WallMinutes))
	fmt.Fprintf(stdout, "  └ 其余          %.1f 分钟(%.0f%%)—— agent 思考/编译/人工介入\n\n",
		rep.ThinkMinutes, pct(rep.ThinkMinutes, rep.WallMinutes))
	fmt.Fprintf(stdout, "  调用            %d 次,失败 %d(%.1f%%)\n", rep.Calls, rep.Failures, rep.FailRate*100)
	fmt.Fprintf(stdout, "  ├ 上下文探测     %d 次(%.0f%%)—— 零信息,每个 CLI 进程启动都要重来\n",
		rep.Probes, rep.ProbeShare*100)
	fmt.Fprintf(stdout, "  └ 写动作        %d 次 —— 产出\n", rep.Mutations)
	if rep.Tokens > 0 {
		fmt.Fprintf(stdout, "  token           %d(自报)\n", rep.Tokens)
	} else {
		fmt.Fprintf(stdout, "  token           未记录 —— 审计日志里没有,用 --tokens N 自报\n")
	}
	fmt.Fprintf(stdout, "\n  动作 top:\n")
	for _, s := range rep.Top {
		mark := ""
		if auditProbeActions[s.Action] {
			mark = "  ← 探测"
		}
		fmt.Fprintf(stdout, "    %-34s %5d 次  失败 %3d  累计 %7.1fs%s\n",
			s.Action, s.Calls, s.Failures, s.Seconds, mark)
	}
	if len(rep.TopFails) > 0 {
		fmt.Fprintf(stdout, "\n  失败 top(失败率高 = 那条路可能根本没在工作):\n")
		for _, s := range rep.TopFails {
			fmt.Fprintf(stdout, "    %-34s %3d/%d(%.0f%%)\n",
				s.Action, s.Failures, s.Calls, float64(s.Failures)/float64(s.Calls)*100)
		}
	}
}

func pct(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return part / whole * 100
}

// newAuditCostCmd 注册 `audit cost`。
func newAuditCostCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		dir, day, since, until string
		label, project, note   string
		tokens                 int
		asJSON, record, ledger bool
	)
	c := &cobra.Command{
		Use:   "cost",
		Short: "一场设计跑了多久、多少次调用、多少是白花的(可 --record 落台账)",
		Long: `把一段时间的审计日志聚合成**成本画像**,并可追加进累积台账做跨批次对比。

三个耗时指标是分开的,因为改法不同:
  • 墙钟        —— 用户实际等了多久
  • daemon 侧   —— 机器真在算(优化调用次数 / 批量化)
  • 两者之差    —— agent 思考 / 编译 / 人工介入(优化流程与判据,不是优化 API)

**上下文探测**单独计一栏(document.current / pages.list / documents.list):它们不读
设计数据也不改画布,只是每个 CLI 进程启动都要重新 resolve 一遍窗口和工程。首测
esp32Mini 原理图 E2E:5466 次调用里 3527 次(65%)是它们 —— 这种浪费在单条命令的
视角下完全看不见。

token 不在审计日志里(那是 agent 侧的账),用 --tokens 自报;不给就记「未记录」,
**不估算冒充实测**。`,
		Args: cobra.NoArgs,
		Example: `  easyeda audit cost --day 2026-08-15 --since 14:12 --until 15:50
  easyeda audit cost --since 14:12 --until 15:50 --label "esp32Mini 原理图 E2E" --tokens 1200000 --record
  easyeda audit cost --ledger`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if ledger {
				entries, err := readCostLedger()
				if err != nil {
					return err
				}
				if len(entries) == 0 {
					fmt.Fprintf(stdout, "台账还是空的(%s)—— 跑完一场用 `audit cost … --record` 记第一笔\n", auditLedgerPath())
					return nil
				}
				if asJSON {
					enc := json.NewEncoder(stdout)
					enc.SetIndent("", "  ")
					return enc.Encode(entries)
				}
				fmt.Fprintf(stdout, "cost ledger — %d 笔(%s)\n\n", len(entries), auditLedgerPath())
				fmt.Fprintf(stdout, "  %-28s %-11s %7s %7s %7s %7s %8s %8s\n",
					"label", "day", "墙钟m", "机器m", "调用", "探测%", "失败%", "token")
				for _, e := range entries {
					fmt.Fprintf(stdout, "  %-28s %-11s %7.1f %7.1f %7.0f %6.0f%% %7.1f%% %8.0f\n",
						truncPageName(asString(e["label"])), asString(e["day"]),
						asFloat(e["wallMinutes"]), asFloat(e["daemonMinutes"]), asFloat(e["calls"]),
						asFloat(e["probeShare"])*100, asFloat(e["failRate"])*100, asFloat(e["tokens"]))
				}
				return nil
			}

			if dir == "" {
				dir = defaultAuditDir()
			}
			if day == "" {
				day = time.Now().UTC().Format("2006-01-02")
			}
			rows, err := readAuditRows(filepath.Join(dir, day+".jsonl"))
			if err != nil {
				return err
			}
			fromTs, toTs, err := parseAuditRange(day, since, until)
			if err != nil {
				return err
			}
			var in []auditRow
			for _, r := range rows {
				if r.Ts.Before(fromTs) || r.Ts.After(toTs) {
					continue
				}
				in = append(in, r)
			}
			if len(in) == 0 {
				return fmt.Errorf("%s 的 %s–%s 区间内没有审计记录", day, since, until)
			}
			rep := summarizeAuditCost(in, mutatingActionSet())
			rep.Label, rep.Project, rep.Tokens, rep.Note = label, project, tokens, note

			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else {
				renderCostReport(rep, stdout)
			}
			if record {
				if err := appendCostLedger(rep); err != nil {
					return fmt.Errorf("写台账失败: %w", err)
				}
				fmt.Fprintf(stderr, "✓ 已记入台账 %s\n", auditLedgerPath())
			}
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "audit log directory (default ~/.easyeda-agent/audit)")
	c.Flags().StringVar(&day, "day", "", "day to read, YYYY-MM-DD (default today, UTC)")
	c.Flags().StringVar(&since, "since", "", "start time HH:MM or HH:MM:SS (UTC)")
	c.Flags().StringVar(&until, "until", "", "end time HH:MM or HH:MM:SS (UTC)")
	c.Flags().StringVar(&label, "label", "", "这一场叫什么(台账里的名字)")
	c.Flags().StringVar(&project, "project-name", "", "工程名(仅记录用)")
	c.Flags().StringVar(&note, "note", "", "备注(踩了什么坑、跑到哪一步)")
	c.Flags().IntVar(&tokens, "tokens", 0, "agent 侧 token 消耗(自报;审计日志里没有)")
	c.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出")
	c.Flags().BoolVar(&record, "record", false, "把这一场追加进累积台账")
	c.Flags().BoolVar(&ledger, "ledger", false, "只列台账历史(不分析日志)")
	return c
}
