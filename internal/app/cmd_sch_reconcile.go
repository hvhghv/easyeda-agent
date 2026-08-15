package app

// cmd_sch_reconcile.go — S5 的「设计意图门」的机械化。
//
// design-flow S5 有两道门:机械门(`sch gate`)判「接得合不合法」,设计意图门判
// 「接对了没有」。后者此前只有一句「逐项对照 spec / 黄金表」——**唯一没有机械判据
// 的一步**,靠人肉核对,而人肉核对在 30 条网上必然漏。
//
// 但「本该怎么连」其实是有正本的:凡是 `sch block-apply` 落地的模块,块库里就写着它的
// internal_nets,而虚拟组记着这一实例的 role→位号(workflow.Group.BlockID/Roles)。
// 于是任何时候都能**从块库重新推导应有连接**,与活体网表逐条对账 —— 不依赖 apply 当时
// 的日志,不依赖网名。
//
// 判据故意做成**连通性**而不是网名:块内网的实例名是 `<INSTANCE>_N<i>`、边界网被
// `--bind` 重绑成宿主网名,名字随时会变,而「这几个引脚必须在同一张网上」不会变。

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
)

// schReconcileDiff 是一条对账差异。
type schReconcileDiff struct {
	Group   string   `json:"group"`
	BlockID string   `json:"blockId"`
	Net     string   `json:"net"`               // 块配方里的网(用它的成员列表标识)
	Kind    string   `json:"kind"`              // split | missing | unresolved
	Members []string `json:"members"`           // 该内部网期望的全部成员
	LiveIn  []string `json:"liveIn,omitempty"`  // split:这些成员分别落在哪些活体网上
	Missing []string `json:"missing,omitempty"` // missing/unresolved:哪些成员没连上
}

// schReconcileGroup 是一个组的对账结果。
type schReconcileGroup struct {
	Group   string             `json:"group"`
	BlockID string             `json:"blockId"`
	Nets    int                `json:"netsChecked"`
	Diffs   []schReconcileDiff `json:"diffs,omitempty"`
}

// schReconcileReport 是命令输出。
type schReconcileReport struct {
	OK          bool                `json:"ok"`
	Groups      []schReconcileGroup `json:"groups"`
	Unprovenant []string            `json:"withoutProvenance,omitempty"` // 没有拓扑来源、对不了账的组
	Note        string              `json:"note,omitempty"`
}

// reconcileGroupNets 是纯函数核心:给定块的内部网(role.pin 引用)、role→位号映射,
// 与活体网表,判每条内部网的成员是否**都落在同一张活体网上**。
//
// 三类差异:
//   - unresolved — 引脚引用解析不出真实引脚(块数据与实际器件对不上);
//   - missing    — 成员在活体网表里根本找不到(没连);
//   - split      — 成员分散在多张活体网上(**连错了/断了**,这是真正的电气缺陷)。
func reconcileGroupNets(nets [][]string, roles map[string]string,
	liveNets map[string]map[string]bool, pinNumbers map[string]map[string][]string) []schReconcileDiff {

	// 反向索引:引脚 → 它所在的活体网。
	netOfPin := map[string]string{}
	for net, members := range liveNets {
		for m := range members {
			netOfPin[strings.ToUpper(m)] = net
		}
	}
	var diffs []schReconcileDiff
	for _, net := range nets {
		var members []string
		var missing, unresolved []string
		found := map[string][]string{} // 活体网 → 该网上的成员
		for _, ref := range net {
			if strings.HasPrefix(ref, "PORT:") {
				continue // 边界端口由宿主网名承载,连通性对账不看它
			}
			role, pin, ok := splitBlockPinRef(strings.TrimSuffix(ref, "*"))
			if !ok {
				continue
			}
			desig, known := roles[role]
			if !known {
				unresolved = append(unresolved, ref)
				continue
			}
			member := desig + ":" + pin
			if strings.HasSuffix(ref, "*") {
				member += "*" // 同名脚全并联,交给 bapPinKeys 展开
			}
			members = append(members, member)
			keys, ok := bapPinKeys(member, pinNumbers)
			if !ok || len(keys) == 0 {
				unresolved = append(unresolved, member)
				continue
			}
			hit := false
			for _, k := range keys {
				if n, in := netOfPin[strings.ToUpper(k)]; in {
					found[n] = append(found[n], k)
					hit = true
				}
			}
			if !hit {
				missing = append(missing, member)
			}
		}
		label := strings.Join(append(append([]string{}, members...), unresolved...), " ")
		// unresolved 先报:它意味着**这条网根本没被判过**,不能被「成员不够两个」
		// 的短路规则吞掉 —— 那正好是解析失败时会发生的事。
		if len(unresolved) > 0 {
			diffs = append(diffs, schReconcileDiff{Net: label, Kind: "unresolved",
				Members: members, Missing: unresolved})
		}
		if len(members) < 2 {
			continue // 单成员网没有「连通性」可言
		}
		if len(missing) > 0 {
			diffs = append(diffs, schReconcileDiff{Net: label, Kind: "missing",
				Members: members, Missing: missing})
		}
		if len(found) > 1 {
			live := make([]string, 0, len(found))
			for n, ms := range found {
				sort.Strings(ms)
				live = append(live, fmt.Sprintf("%s{%s}", n, strings.Join(ms, ",")))
			}
			sort.Strings(live)
			diffs = append(diffs, schReconcileDiff{Net: label, Kind: "split",
				Members: members, LiveIn: live})
		}
	}
	return diffs
}

// runSchReconcile 读组表 + 活体网表,逐组对账。只读,不改画布。
func runSchReconcile(cfg *appConfig, window string, asJSON bool, stdout, stderr io.Writer) error {
	_, win, docUUID, _, _, groups, err := loadSchGroupsContext(cfg, window)
	if err != nil {
		return fmt.Errorf("读不到页面分组表: %w", err)
	}
	_ = docUUID
	liveNets, pinNumbers, nerr := readLiveNets(cfg, win)
	if nerr != nil {
		return fmt.Errorf("读不到活体网表: %w", nerr)
	}

	rep := schReconcileReport{OK: true}
	// **同一个块实例可能登记成多个功能子群**(USB 口 / ESD / 桥芯片…),对账前先按
	// (blockId, instance) 合并 role→位号:块的 internal_nets 是跨子群的,拿单个子群
	// 的部分映射去解析,另一头会被判成 unresolved —— 那是合并没做,不是电路有问题。
	type inst struct {
		blockID, instance string
	}
	merged := map[inst]map[string]string{}
	order := []inst{}
	labels := map[inst][]string{}
	for _, g := range groups {
		if g == nil {
			continue
		}
		if g.BlockID == "" || len(g.Roles) == 0 {
			rep.Unprovenant = append(rep.Unprovenant, describeSchGroup(g))
			continue
		}
		k := inst{g.BlockID, g.Instance}
		if merged[k] == nil {
			merged[k] = map[string]string{}
			order = append(order, k)
		}
		for r, d := range g.Roles {
			merged[k][r] = d
		}
		labels[k] = append(labels[k], describeSchGroup(g))
	}
	for _, k := range order {
		blk, ok, berr := blocks.Get(k.blockID)
		label := strings.Join(labels[k], " + ")
		if berr != nil || !ok {
			rep.Unprovenant = append(rep.Unprovenant, label+"(块库里没有 "+k.blockID+")")
			continue
		}
		nets := bslBlockNets(blk)
		diffs := reconcileGroupNets(nets, merged[k], liveNets, pinNumbers)
		for i := range diffs {
			diffs[i].Group, diffs[i].BlockID = label, k.blockID
		}
		rep.Groups = append(rep.Groups, schReconcileGroup{
			Group: label, BlockID: k.blockID, Nets: len(nets), Diffs: diffs})
		if len(diffs) > 0 {
			rep.OK = false
		}
	}
	if len(rep.Groups) == 0 {
		rep.Note = "没有带拓扑来源的组 —— 只有 `sch block-apply` 落地的模块记着「本该怎么连」;" +
			"手工搭的电路仍需 `sch read` 人工对照 spec"
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		for _, g := range rep.Groups {
			if len(g.Diffs) == 0 {
				fmt.Fprintf(stdout, "✓ %s (%s):%d 条内部网逐条对上活体网表\n", g.Group, g.BlockID, g.Nets)
				continue
			}
			fmt.Fprintf(stdout, "✗ %s (%s):%d 条内部网,%d 处对不上\n", g.Group, g.BlockID, g.Nets, len(g.Diffs))
			for _, d := range g.Diffs {
				switch d.Kind {
				case "split":
					fmt.Fprintf(stdout, "   split      本该同网的 [%s] 分散在:%s\n", d.Net, strings.Join(d.LiveIn, " | "))
				case "missing":
					fmt.Fprintf(stdout, "   missing    [%s] 里 %s 没连上任何网\n", d.Net, strings.Join(d.Missing, ","))
				case "unresolved":
					fmt.Fprintf(stdout, "   unresolved [%s] 里 %s 解析不出真实引脚(块数据与器件对不上)\n",
						d.Net, strings.Join(d.Missing, ","))
				}
			}
		}
		for _, u := range rep.Unprovenant {
			fmt.Fprintf(stdout, "—  %s:没有拓扑来源,对不了账(手工搭的组)\n", u)
		}
		if rep.Note != "" {
			fmt.Fprintf(stdout, "note: %s\n", rep.Note)
		}
	}
	if !rep.OK {
		return fmt.Errorf("reconcile: 有内部网与块配方对不上 —— 先修连接再进 S6")
	}
	return nil
}

// newSchReconcileCmd 注册 `sch reconcile`。
func newSchReconcileCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "reconcile",
		Short: "把「接对了没有」变成机械判据:从块库重推应有连接,与活体网表逐条对账",
		Long: `逐组对账:凡是 ` + "`sch block-apply`" + ` 落地的模块,块库里写着它的 internal_nets,
虚拟组记着这一实例的 role→位号 —— 于是任何时候都能**从块库重新推导本该怎么连**,
和活体网表比对。这就是 design-flow S5「设计意图门」的机械化:此前那一步只有一句
「逐项对照 spec」,是整条流程里唯一没有机械判据的门。

判据是**连通性**不是网名:块内网的实例名(` + "`<INSTANCE>_N<i>`" + `)、被 --bind 重绑的
边界网名都会变,而「这几个引脚必须在同一张网上」不会变。

三类差异:
  • split      本该同网的引脚分散在多张活体网上 —— **真正的电气缺陷**
  • missing    成员在活体网表里找不到 —— 没连
  • unresolved 引脚引用解析不出真实引脚 —— 块数据与实际器件对不上

有差异时非零退出,可直接当门禁。手工搭的组没有拓扑来源,会如实列出「对不了账」,
不会假装通过。`,
		Args:    cobra.NoArgs,
		Example: `  easyeda sch reconcile\n  easyeda sch reconcile --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchReconcile(cfg, *window, asJSON, stdout, stderr)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出")
	return c
}
