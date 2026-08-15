package app

// cmd_sch_nets.go — `easyeda sch nets`:**跨页**网名审计。
//
// 立项现场(2026-08-16 esp32Mini E2E #2):电源块落地出 `+3V3`/`+5V`,而 MCU 块与
// CH340 块要的是 `3V3`/`5V` —— 四页板子上,主控和它的稳压器**根本没连在一起**。
//
// 这个缺陷此前**没有任何判据会报**:
//   • `sch check` / `gate` 是逐页的,而每页各自只有一个变体,同页看不见;
//   • 两个网各自都完全合法(`+3V3` 和 `3V3` 都是有效网名、都有 ≥2 个引脚、
//     都不悬空),没有一条既有规则会觉得不对;
//   • `bridge-check` 找的是「本该分开却连上了」,这里正好相反 —— 本该连上却分开了。
//
// 所以它只能靠人眼在 block-apply 的输出里看出来。我就是这么发现的,而下一次
// 未必有人在看。
//
// 数据源是**全工程网表导出**(一次调用):`sch netlist` 的 .enet 里
// components[].pinInfoMap[].net 覆盖所有页 —— 这正是逐页命令拿不到的那个视角。
//
// 判据只有两条,都刻意保守:
//  1. **网名变体** —— 归一化后同名、原名不同。归一化只做电子领域的**书写惯例**
//     等价(极性号、分隔符、`3V3`↔`3.3V`),绝不碰字母前缀:`GND`/`AGND`、
//     `VCC`/`VDD`、`VCC`/`VCC_IO` 语义不同,合并它们会制造假警报,而一条假警报
//     会让人开始忽略这条规则。
//  2. **单引脚网** —— 只挂着一个引脚的网,那个引脚实际上什么也没接上。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// schNetInfo 是一条网的全工程画像。
type schNetInfo struct {
	Name  string   `json:"net"`
	Pins  int      `json:"pins"`
	Parts []string `json:"parts"` // 去重后的位号,升序
}

// schNetVariant 是一组「本该是同一张网」的变体。
type schNetVariant struct {
	Normalized string       `json:"normalized"`
	Nets       []schNetInfo `json:"nets"`
}

type schNetsReport struct {
	OK       bool            `json:"ok"`
	Total    int             `json:"totalNets"`
	Variants []schNetVariant `json:"variants,omitempty"`
	Lonely   []schNetInfo    `json:"singlePinNets,omitempty"`
	Nets     []schNetInfo    `json:"nets,omitempty"`
}

// schNetVoltagePattern 匹配 `3V3` / `1V8` 这类以 V 代小数点的写法。
var schNetVoltagePattern = regexp.MustCompile(`^(\d+)V(\d+)$`)

// normalizeSchNetName 把书写惯例上等价的网名折成同一个 key。
//
// **只做惯例等价,不做语义猜测**:
//   - 去前导极性号:`+3V3` → `3V3`(`+5V` 与 `5V` 是同一条轨的两种写法)
//   - 去分隔符与空白:`VBUS_5V` → `VBUS5V`
//   - `3V3` ↔ `3.3V`:V 代小数点是电子行业的标准写法,同一个电压两种写法
//   - 统一大小写
//
// 刻意**不做**的:去掉字母前缀。`AGND` 不是 `GND`、`VDD` 不是 `VCC`、
// `VCC_IO` 不是 `VCC` —— 那些前缀带着电源域语义,合并它们会产生假警报,
// 而一条假警报足以让人开始忽略整条规则。
func normalizeSchNetName(name string) string {
	n := strings.ToUpper(strings.TrimSpace(name))
	n = strings.TrimLeft(n, "+")
	n = strings.NewReplacer("_", "", "-", "", " ", "", ".", "").Replace(n)
	// 3V3 → 33V,3.3V(已去点)→ 33V:两种写法落到同一个 key。
	if m := schNetVoltagePattern.FindStringSubmatch(n); m != nil {
		n = m[1] + m[2] + "V"
	}
	return n
}

// schAutoNetName 判一个网名是不是平台自动生成的(`$1N2`)或块实例内部网
// (`<INSTANCE>_N<i>`)。它们**本来就该各不相同**,不参与变体判定,否则
// `Q_N3`/`Q_N4` 这种正常的块内网会被当成变体刷屏。
var schAutoNetPattern = regexp.MustCompile(`^\$?\w*_?N\d+$`)

func schAutoNetName(name string) bool {
	return strings.HasPrefix(name, "$") || schAutoNetPattern.MatchString(strings.ToUpper(name))
}

// auditSchNets 是纯核:给定全工程网名→画像,找出变体与单引脚网。无 I/O,可单测。
func auditSchNets(nets []schNetInfo) schNetsReport {
	rep := schNetsReport{OK: true, Total: len(nets), Nets: nets}
	byNorm := map[string][]schNetInfo{}
	for _, n := range nets {
		if schAutoNetName(n.Name) {
			continue
		}
		k := normalizeSchNetName(n.Name)
		byNorm[k] = append(byNorm[k], n)
	}
	keys := make([]string, 0, len(byNorm))
	for k := range byNorm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		group := byNorm[k]
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].Name < group[j].Name })
		rep.Variants = append(rep.Variants, schNetVariant{Normalized: k, Nets: group})
		rep.OK = false
	}
	for _, n := range nets {
		if n.Pins == 1 {
			rep.Lonely = append(rep.Lonely, n)
		}
	}
	sort.Slice(rep.Lonely, func(i, j int) bool { return rep.Lonely[i].Name < rep.Lonely[j].Name })
	return rep
}

// parseSchNetlistNets 从 .enet(全工程网表导出)里提取每张网的引脚数与器件。
func parseSchNetlistNets(raw []byte) ([]schNetInfo, error) {
	var doc struct {
		Components map[string]struct {
			Props      map[string]any `json:"props"`
			PinInfoMap map[string]struct {
				Net string `json:"net"`
			} `json:"pinInfoMap"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析网表导出: %w", err)
	}
	pins := map[string]int{}
	parts := map[string]map[string]bool{}
	for _, c := range doc.Components {
		desig, _ := c.Props["Designator"].(string)
		for _, pi := range c.PinInfoMap {
			net := strings.TrimSpace(pi.Net)
			if net == "" {
				continue
			}
			pins[net]++
			if parts[net] == nil {
				parts[net] = map[string]bool{}
			}
			if desig != "" {
				parts[net][desig] = true
			}
		}
	}
	out := make([]schNetInfo, 0, len(pins))
	for net, n := range pins {
		ds := make([]string, 0, len(parts[net]))
		for d := range parts[net] {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		out = append(out, schNetInfo{Name: net, Pins: n, Parts: ds})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func runSchNets(cfg *appConfig, window string, strict, asJSON, listAll bool, stdout, stderr io.Writer) error {
	res, err := requestActionTimed(cfg, "schematic.export.netlist", window, map[string]any{}, schNetsTimeout)
	if err != nil {
		return fmt.Errorf("导出全工程网表: %w", err)
	}
	if len(res.Artifacts) == 0 || res.Artifacts[0].Path == "" {
		return fmt.Errorf("网表导出没有落盘产物 —— 无法审计网名")
	}
	raw, rerr := os.ReadFile(res.Artifacts[0].Path)
	if rerr != nil {
		return fmt.Errorf("读网表导出 %s: %w", res.Artifacts[0].Path, rerr)
	}
	nets, perr := parseSchNetlistNets(raw)
	if perr != nil {
		return perr
	}
	rep := auditSchNets(nets)
	if !listAll {
		rep.Nets = nil
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(stdout, "sch nets — %d 张网(**全工程,跨页**)\n\n", rep.Total)
		for _, v := range rep.Variants {
			fmt.Fprintf(stdout, "  ✗ 网名变体:同一条轨被写成了 %d 种名字\n", len(v.Nets))
			for _, n := range v.Nets {
				fmt.Fprintf(stdout, "      %-12s %2d 脚  %s\n", fmt.Sprintf("%q", n.Name), n.Pins, strings.Join(n.Parts, ","))
			}
			fmt.Fprintf(stdout, "      它们**不会自动合并** —— 板子上这是几张互不相连的网\n\n")
		}
		for _, n := range rep.Lonely {
			fmt.Fprintf(stdout, "  ⚠ 单引脚网 %q —— 只挂着 %s,那个引脚什么也没接上\n", n.Name, strings.Join(n.Parts, ","))
		}
		if listAll {
			fmt.Fprintln(stdout)
			for _, n := range rep.Nets {
				fmt.Fprintf(stdout, "  %-14s %2d 脚  %s\n", n.Name, n.Pins, strings.Join(n.Parts, ","))
			}
		}
		if rep.OK && len(rep.Lonely) == 0 {
			fmt.Fprintf(stdout, "✓ %d 张网,无同轨异名,无单引脚网\n", rep.Total)
		}
	}
	if strict && (!rep.OK || len(rep.Lonely) > 0) {
		return fmt.Errorf("sch nets: %d 组网名变体 / %d 张单引脚网 —— 先统一网名(block-apply 用 --bind)再进 S6",
			len(rep.Variants), len(rep.Lonely))
	}
	if !rep.OK {
		return fmt.Errorf("sch nets: %d 组网名变体 —— 同一条轨有多个名字,它们不会自动合并", len(rep.Variants))
	}
	return nil
}

// schNetsTimeout:网表导出要遍历全工程,比普通读贵。
const schNetsTimeout = 60 * time.Second

func newSchNetsCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var strict, asJSON, listAll bool
	c := &cobra.Command{
		Use:   "nets",
		Short: "跨页网名审计:同一条轨被写成多个名字 / 单引脚网(逐页命令看不见的那半)",
		Long: `从**全工程网表导出**审计网名 —— 这是逐页命令拿不到的视角。

**为什么需要它**:esp32Mini E2E 实测,电源块落地出 ` + "`+3V3`/`+5V`" + `,而 MCU 块与
CH340 块要的是 ` + "`3V3`/`5V`" + ` —— 主控和它的稳压器根本没连在一起,而**没有任何既有
判据会报**:` + "`sch check`/`gate`" + ` 是逐页的(每页各自只有一个变体,同页看不见),
两个网各自又都完全合法(有 ≥2 个引脚、不悬空、名字有效),` + "`bridge-check`" + ` 找的
则是相反的毛病(本该分开却连上了)。它只能靠人眼在 block-apply 的输出里看出来。

两条判据都刻意保守:
  • **网名变体** —— 归一化后同名、原名不同。归一化只做书写惯例等价(极性号、
    分隔符、` + "`3V3`↔`3.3V`" + `),**绝不碰字母前缀**:` + "`AGND`≠`GND`、`VDD`≠`VCC`、" + `
    ` + "`VCC_IO`≠`VCC`" + ` —— 那些前缀带着电源域语义,合并会制造假警报,而一条假警报
    足以让人开始忽略整条规则。块实例内部网(` + "`U3_N3`" + `)与平台自动名(` + "`$1N2`" + `)
    本来就该各不相同,不参与判定。
  • **单引脚网** —— 只挂着一个引脚的网,那个引脚实际上什么也没接上。

修法:` + "`sch block-apply … --bind <端口>=<统一网名>`" + `;S0 阶段就该定下全工程网名表。`,
		Args: cobra.NoArgs,
		Example: `  easyeda sch nets --project ceshi
  easyeda sch nets --project ceshi --strict
  easyeda sch nets --project ceshi --all --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchNets(cfg, *window, strict, asJSON, listAll, stdout, stderr)
		},
	}
	c.Flags().BoolVar(&strict, "strict", false, "单引脚网也阻塞(非零退出)")
	c.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出")
	c.Flags().BoolVar(&listAll, "all", false, "把全部网也列出来")
	return c
}
