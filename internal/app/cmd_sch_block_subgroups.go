package app

// cmd_sch_block_subgroups.go — 把一个块拆成**功能子群**(可复用,不靠人手认领)。
//
// 起因:用户要求分区粒度更细,而此前只能手工
// `sch zones set --module USB_C=left:J1,R4,R5 --module ESD=center:D1 …` —— 一次性、
// 不复用、换个块就得重写一遍。但「哪几件构成一个功能子群」**块自己就知道**:
// 关系模板里的 `flow` 是信号流上的各级(USB口 → ESD → 桥芯片),`attach` 是贴在某级
// 电源脚上的去耦,`pair` 是挂在某级上的并列组。按这三条就能确定地拆出子群。
//
// 拆出来的子群有两个用处,都不需要人再看图:
//   1. **归组**:block-apply 按子群登记虚拟组,于是「把 USB 口那一簇整体挪开」有了抓手
//      (`sch group-move --group <id>`),而不是整块 7 件一起动;
//   2. **分区**:同一份拆分直接当 zone 认领,`zone-plan` 的框就按功能走,不用手认领。

import (
	"sort"
	"strings"

	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
)

// bslSubgroup 是一个功能子群:一个名字 + 属于它的 role 列表。
type bslSubgroup struct {
	Name  string   // 子群名(取该级的 role 名,块可在数据里覆盖)
	Roles []string // 成员 role,已排序
}

// bslFunctionalGroups 按关系把块拆成功能子群。
//
// 规则(全部来自块数据,没有一条是猜的):
//   - **flow 的每一级各自成群** —— 信号流上的一级就是一个功能单元;
//   - **attach 件跟宿主** —— 去耦的全部意义就是贴着那只脚,它不可能属于别的功能;
//   - **pair 组跟它连的那一级** —— CC 下拉挂在 USB 口上,就属于 USB 口那群;
//   - **其余件**归到与它**跨接网最多**的那一级(连得最紧的就是最相关的);一条跨接网
//     都没有(纯电源/地)时归锚件那群,保证没有孤儿。
//
// 块没有 flow(只有 attach/pair 的小块)时返回单个子群 —— 那种块本来就是一个功能单元,
// 硬拆只会把去耦和它的芯片分到两个框里。
func bslFunctionalGroups(blk blocks.Block, rel bslRelations, nets [][]string, anchorRole string) []bslSubgroup {
	roles := make([]string, 0, len(blk.Parts))
	for r := range blk.Parts {
		roles = append(roles, r)
	}
	sort.Strings(roles) // 同输入同输出
	if len(rel.Flow) < 2 {
		return []bslSubgroup{{Name: blockShortName(blk), Roles: roles}}
	}

	owner := map[string]string{} // role → 子群名(= flow 那一级的 role)
	stages := map[string]bool{}
	for _, s := range rel.Flow {
		if _, ok := blk.Parts[s]; !ok {
			continue
		}
		owner[s] = s
		stages[s] = true
	}
	// attach 件跟宿主:目标是哪一级就归哪一级;宿主自己不是 flow 级时顺着再找一层。
	for role, target := range rel.Attach {
		tRole, _, ok := splitBlockPinRef(target)
		if !ok {
			continue
		}
		if o, has := owner[tRole]; has {
			owner[role] = o
		}
	}
	// pair 组:整组跟随「与它跨接网最多的那一级」,组内不拆散(等距并列是电路语义)。
	for _, group := range rel.Pair {
		best, bestN := "", -1
		for _, s := range rel.Flow {
			if !stages[s] {
				continue
			}
			n := 0
			for _, m := range group {
				n += bslCrossNets(nets, s, m)
			}
			if n > bestN {
				best, bestN = s, n
			}
		}
		if best == "" {
			continue
		}
		for _, m := range group {
			if _, taken := owner[m]; !taken {
				owner[m] = best
			}
		}
	}
	// 剩下的:跨接网最多的那一级;都没有就归锚件那群(不留孤儿)。
	fallback := owner[anchorRole]
	if fallback == "" && len(rel.Flow) > 0 {
		fallback = rel.Flow[len(rel.Flow)-1]
	}
	for _, r := range roles {
		if _, taken := owner[r]; taken {
			continue
		}
		best, bestN := "", 0
		for _, s := range rel.Flow {
			if !stages[s] {
				continue
			}
			if n := bslCrossNets(nets, s, r); n > bestN {
				best, bestN = s, n
			}
		}
		if best == "" {
			best = fallback
		}
		if best != "" {
			owner[r] = best
		}
	}

	byName := map[string][]string{}
	for role, name := range owner {
		byName[name] = append(byName[name], role)
	}
	out := make([]bslSubgroup, 0, len(byName))
	for name, members := range byName {
		sort.Strings(members)
		out = append(out, bslSubgroup{Name: name, Roles: members})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// blockShortName 是块 id 去掉 `block.` 前缀后的短名,用作单子群时的组名。
func blockShortName(blk blocks.Block) string {
	return strings.TrimPrefix(strings.TrimSpace(blk.ID), "block.")
}

// bapSubgroupsOf 从 plan 反查块并拆功能子群;取不到块时退回「整块一个子群」,
// 归组绝不因为拆分失败而整个跳过(器件与连线此刻都已落地)。
func bapSubgroupsOf(plan bapPlan) []bslSubgroup {
	all := make([]string, 0, len(plan.Placements))
	for _, p := range plan.Placements {
		if r := strings.TrimSpace(p.Role); r != "" {
			all = append(all, r)
		}
	}
	sort.Strings(all)
	fallback := []bslSubgroup{{Name: strings.TrimPrefix(plan.BlockID, "block."), Roles: all}}
	blk, ok, err := blocks.Get(plan.BlockID)
	if err != nil || !ok {
		return fallback
	}
	layout, lerr := blk.SchematicLayout()
	if lerr != nil {
		return fallback
	}
	rel, isRel := bslRelationsFrom(layout)
	if !isRel {
		return fallback
	}
	if subs := bslFunctionalGroups(blk, rel, bslBlockNets(blk), plan.AnchorRole); len(subs) > 0 {
		return subs
	}
	return fallback
}

// schGroupModules 把这一页的持久虚拟组当成分区模块 —— 组名去掉块实例前缀当区名。
//
// 为什么是它而不是 `sch zones set` 的认领:归组时已经确定了「哪几件是一个功能单元」,
// 认领再抄一份就是**两个事实来源**,而副本不会跟着 group-move / 删件更新。zone 认领
// 保留给它真正独有的职责:布局**之前**给 autolayout 指定模块该落在纸面的哪一格
// (那时件还没放,谈不上虚拟组)。
func schGroupModules(cfg *appConfig, window, docUUID string) map[string]*schZoneClaim {
	_, _, ctxDoc, _, st, _, err := loadSchGroupsContext(cfg, window)
	if err != nil || st == nil {
		return nil
	}
	if docUUID == "" {
		docUUID = ctxDoc
	}
	return schGroupModulesFromState(st, docUUID)
}

// schGroupModulesFromState 是纯核:把一页的持久虚拟组投影成模块表。无 I/O,可单测。
//
// 区名取组名**末段**(`ch340c_usb_serial(U3)/J_USB` → `J_USB`),与 `sch note --zone`
// 的写回口径必须一致 —— 读得到的区名要写得回去(见 findSchGroupByZoneName)。
func schGroupModulesFromState(st *pcbStageState, docUUID string) map[string]*schZoneClaim {
	groups := st.GroupsForPage(docUUID)
	if len(groups) == 0 {
		return nil
	}
	out := map[string]*schZoneClaim{}
	for _, g := range groups {
		if g == nil || len(g.Members) == 0 {
			continue
		}
		name := g.Name
		if i := strings.LastIndex(name, "/"); i >= 0 && i+1 < len(name) {
			name = name[i+1:]
		}
		if name == "" {
			name = g.ID
		}
		out[name] = &schZoneClaim{
			Parts: append([]string(nil), g.Members...),
			// 说明的归属也在组上 —— 画框要把登记的说明 fold 进去,zone move 要带着它走,
			// 两者读的都是 claim.NoteIDs,所以在这里把组的说明投影过来,读路径就只有一条。
			NoteIDs: append([]string(nil), g.NoteIDs...),
		}
	}
	return out
}

// loadSchZoneModules 是「这一页有哪些功能模块、各由哪些件组成」的**唯一读入口**:
// 虚拟组优先,没有组才回落到 `sch zones set` 的认领。
//
// 为什么必须收敛成一个函数:`computePartitionPlan` 改成组优先之后,另有六处
// (note 登记 / layout-score 模块围栏 / sheet-tidy / zone-tidy / zone-move /
// zone-relayout)仍直接读认领表 —— 而 `block-apply` 按设计**不写**认领(那正是被砍掉的
// 第二份副本),于是块驱动的页在这六条命令里全都报「没有 zone 认领」。同一个问题
// (归属从哪来)有两份答案,就一定会有一半走错。
//
// 例外只有一个:`sch autolayout --engine template` 要的是**格位**(left-top…),
// 那是布局前的落位目标,那时件还没放下去、谈不上虚拟组,只能读认领。
func loadSchZoneModules(cfg *appConfig, window, docUUID string) (map[string]*schZoneClaim, string, error) {
	if zones := schGroupModules(cfg, window, docUUID); len(zones) > 0 {
		project := ""
		if _, _, _, p, _, _, err := loadSchGroupsContext(cfg, window); err == nil {
			project = p
		}
		return zones, project, nil
	}
	return loadSchZoneClaimsForPage(cfg, window, docUUID)
}
