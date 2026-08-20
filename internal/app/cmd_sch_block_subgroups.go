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
	"strconv"
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
// 没有 flow 的块走**按 attach 目标引脚**分子群(bslPinSubgroups)—— 见那里的
// 真机取证:一律返回单群会让 WROOM 那种「锚件 + 五件贴脚」的块糊成一个
// 507×712 的区,独占一整页也放不进 A4 的图签左侧。拆不动(件太少/全贴同一个脚)
// 时才退回单群 —— 小块本来就是一个功能单元,硬拆只会把去耦和它的芯片分到两个框里。
func bslFunctionalGroups(blk blocks.Block, rel bslRelations, nets [][]string, anchorRole string) []bslSubgroup {
	roles := make([]string, 0, len(blk.Parts))
	for r := range blk.Parts {
		roles = append(roles, r)
	}
	sort.Strings(roles) // 同输入同输出
	if len(rel.Flow) < 2 {
		if subs, ok := bslPinSubgroups(blk, rel, nets, anchorRole, roles); ok {
			return subs
		}
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

	return bslFoldSubgroups(owner)
}

// bslFoldSubgroups 把 role→子群名 折成排好序的子群表(两条路共用,输出形状必须同一)。
func bslFoldSubgroups(owner map[string]string) []bslSubgroup {
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

// ── 无 flow 的块:按 attach 目标引脚拆(2026-08-20 真机取证)────────────────
//
// **缺陷**:`esp32s3_wroom1_module` 没有 flow,于是整块 6 件(U + C_BULK + C_VDD +
// R_EN + C_EN + R_IO0)登记成一个组。phase A 收敛之后它仍是 **507×712** ——
// A4(1170×825,页边距 28)带图签 keep-out [468,0,1170,198] 的约束是「待在图签
// 左侧 ⇒ 宽 ≤ 438」或「绕到图签上方 ⇒ 高 ≤ 597」,两条都不满足,`zone-arrange`
// 逐边报 blocked:**这个区独占一整页也放不下**。
//
// **根因**:功能划分的信息本来就在块数据里,是这段代码把它丢了 —— attach 归属
// 只取 `role.pin` 的 role 那一半(`U.3V3` / `U.EN` / `U.IO0` 一律归约成 `U`),
// 而「哪几件是一个功能单元」恰恰写在被丢掉的引脚那一半:
//
//	U.3V3 → C_VDD + C_BULK   供电去耦
//	U.EN  → R_EN  + C_EN     EN 上电复位 RC
//	U.IO0 → R_IO0            IO0 boot strap
//
// 所以规则是「**贴同一个脚的件构成一个功能单元**」—— 不需要块作者补任何声明,
// 对所有没写 flow 的块都成立。(给 WROOM 补 flow 解决不了:补了之后这些件照样
// 全归到 `U` 那一级,因为丢引脚的是代码。)
//
// **flow 路径一步不动**:CH340C 的 attach 也指着两个不同的脚(U.VCC / U.V3),
// 按脚细分会把它现有的 `/U` 组(U + C_VCC + C_V3)拆成三份 —— 那是行为回归,
// 不是修复。信号流上的一级本来就是比引脚更粗的功能粒度,两者不该混。
const (
	// 至少两个不同的目标引脚:全贴同一个脚时「按脚分」没有区分度,拆出来的
	// 是「锚件」+「其余全部」,白得两个框。
	bslPinSplitMinPins = 2
	// 至少四件贴脚。拆分的真正判据是几何(区框装不装得进图纸),而拆分器手里
	// 没有页面尺寸,只能拿「贴脚件数」当代理 —— 两端都用真块钉死:
	//   ams1117_ldo_3v3(锚 + 3 件贴 2 个脚)整块也就锚件旁一列,拆开只是多两个
	//   带标题的框;esp32s3_wroom1_module(锚 + 5 件贴 3 个脚)整块 507×712,
	//   独占一页也放不下。4 是把这两块分开的标定值,不是从理论推的。
	bslPinSplitMinAttach = 4
)

// bslPinSubgroups 按 attach 的**目标引脚**把无 flow 的块拆成功能子群。
//
// 规则:
//   - 锚件自成一群,群名 = **块短名**(拆之前这块就叫这个名,拆完锚件那群继承它);
//   - 其余 attach 件按目标引脚的完整键(`ROLE.PIN`)分群,贴同一个脚的归一群,
//     群名 `ROLE_PIN`(如 `U_3V3`);attach 链(件贴在件上)顺着解析到根引脚,
//     链上的件与根件同群;
//   - pair 组整组不拆散,跟「组里已归属的多数」走;都没归属时跟跨接网最多的那群;
//   - 其余件归跨接网最多的那群,一条都没有时归锚件那群(**不留孤儿**)。
//
// **锚件那群为什么不叫 role 名**:组名末段就是区名(schGroupModulesFromState),
// 而区名在一页里必须唯一。真机那一页(ceshi/MCU_IO)同时有 CH340C —— 它的 flow
// 路径已经登记了一个叫 `U` 的子群,WROOM 的锚件再叫 `U` 就是两区同名、后写的把
// 先写的顶掉。块短名既避开这次碰撞,又与拆分前的组名一致(升级不改名)。
//
// 拆不动时返回 ok=false,由调用方退回「整块一个子群」。
func bslPinSubgroups(blk blocks.Block, rel bslRelations, nets [][]string, anchorRole string, roles []string) ([]bslSubgroup, bool) {
	if anchorRole == "" {
		return nil, false
	}
	if _, ok := blk.Parts[anchorRole]; !ok {
		return nil, false
	}
	hostPin := bslResolveAttachPins(blk, rel, anchorRole)
	if len(hostPin) < bslPinSplitMinAttach {
		return nil, false
	}
	keys := make([]string, 0, len(hostPin))
	seen := map[string]bool{}
	for _, k := range hostPin {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if len(keys) < bslPinSplitMinPins {
		return nil, false
	}
	sort.Strings(keys) // 命名与去重都按同一个序,同输入同输出
	anchorGroup := blockShortName(blk)
	if anchorGroup == "" {
		anchorGroup = anchorRole
	}
	names := bslPinGroupNames(keys, anchorGroup)

	owner := map[string]string{anchorRole: anchorGroup}
	for role, key := range hostPin {
		if name, ok := names[key]; ok {
			owner[role] = name
		}
	}
	// 子群名全序:pair / 兜底归属挑不出赢家时按它做 tie-break。
	groupNames := []string{anchorGroup}
	for _, k := range keys {
		groupNames = append(groupNames, names[k])
	}
	sort.Strings(groupNames)

	// seed = 按 attach 已经确定归属的成员表。pair 与兜底都拿它算跨接网,
	// 不吃自己刚写进去的结果 —— 归属不许依赖遍历顺序。
	seed := map[string][]string{}
	for role, name := range owner {
		seed[name] = append(seed[name], role)
	}
	for name := range seed {
		sort.Strings(seed[name])
	}

	// pair 组整组跟随:先看组里已有归属的多数(等距并列是电路语义,不许拆散),
	// 没有已归属成员时按跨接网选。
	for _, group := range rel.Pair {
		votes := map[string]int{}
		for _, m := range group {
			if o, ok := owner[m]; ok {
				votes[o]++
			}
		}
		best := bslTopVote(votes, groupNames)
		if best == "" {
			best = bslBestByCrossNets(seed, groupNames, nets, group)
		}
		if best == "" {
			best = anchorGroup
		}
		for _, m := range group {
			if _, ok := blk.Parts[m]; !ok {
				continue
			}
			if _, taken := owner[m]; !taken {
				owner[m] = best
			}
		}
	}
	// 其余件:跨接网最多的那群;一条都没有就归锚件那群(不留孤儿)。
	for _, r := range roles {
		if _, taken := owner[r]; taken {
			continue
		}
		best := bslBestByCrossNets(seed, groupNames, nets, []string{r})
		if best == "" {
			best = anchorGroup
		}
		owner[r] = best
	}
	return bslFoldSubgroups(owner), true
}

// bslResolveAttachPins 把 attach 表解析成 role → **根引脚键**(`ROLE.PIN`)。
//
// 三件事在这里做掉:锚件自己排除在外(它自成一群)、指向不存在 role 的声明丢弃、
// attach 链(件贴在件上)顺着解析到根 —— C 贴 R.2、R 贴 U.VIN 时两件同属 `U.VIN`,
// 否则 R 与吊在它身上的 C 会被分进两个框。
func bslResolveAttachPins(blk blocks.Block, rel bslRelations, anchorRole string) map[string]string {
	out := map[string]string{}
	for role := range rel.Attach {
		if role == anchorRole {
			continue
		}
		if _, ok := blk.Parts[role]; !ok {
			continue
		}
		if key, ok := bslResolveAttachKey(blk, rel, role, anchorRole, len(rel.Attach)); ok {
			out[role] = key
		}
	}
	return out
}

// bslResolveAttachKey 顺着 attach 链找根引脚键。budget 是防环的步数上限
// (成环时放弃这一条,绝不死循环);链走到**锚件就停**(锚件自己也写了 attach 时,
// 贴在锚件脚上的件属于那只脚,不该顺着锚件再飘到别处去)。
func bslResolveAttachKey(blk blocks.Block, rel bslRelations, role, anchorRole string, budget int) (string, bool) {
	if budget < 0 {
		return "", false
	}
	target, ok := rel.Attach[role]
	if !ok {
		return "", false
	}
	tRole, pin, ok := splitBlockPinRef(target)
	if !ok || tRole == role {
		return "", false
	}
	if _, ok := blk.Parts[tRole]; !ok {
		return "", false
	}
	if _, chained := rel.Attach[tRole]; chained && tRole != anchorRole {
		return bslResolveAttachKey(blk, rel, tRole, anchorRole, budget-1)
	}
	return tRole + "." + strings.TrimSuffix(strings.TrimSpace(pin), "*"), true
}

// bslPinGroupNames 给每个引脚键起子群名:`ROLE_PIN`(如 `U_3V3`)。
//
// 为什么带上宿主 role 而不是光用引脚名:同一块里两只芯片可能都有 `VCC` 脚,
// 而组名要能直接当区名用(`sch note --zone <区名>` 写得回去)。非法字符
// (含 `/` —— 组名按 `/` 分段取区名)一律折成 `_`,重名按序号消歧,保证唯一且确定。
func bslPinGroupNames(keys []string, anchorGroup string) map[string]string {
	taken := map[string]bool{anchorGroup: true}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		role, pin, ok := splitBlockPinRef(k)
		if !ok {
			role, pin = k, ""
		}
		base := bslSanitizeName(role + "_" + pin)
		if base == "" {
			base = "PIN"
		}
		name := base
		for i := 2; taken[name]; i++ {
			name = base + "_" + strconv.Itoa(i)
		}
		taken[name] = true
		out[k] = name
	}
	return out
}

// bslSanitizeName 把任意引脚名折成可当区名用的标识(字母/数字/`_`/`+`/`-` 保留,
// 其余折成 `_`,首尾 `_` 去掉)。
func bslSanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '+', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// bslTopVote 取票数最多的子群名;平票按 order(子群名全序)取最小,零票返回空。
func bslTopVote(votes map[string]int, order []string) string {
	best, bestN := "", 0
	for _, name := range order {
		if n := votes[name]; n > bestN {
			best, bestN = name, n
		}
	}
	return best
}

// bslBestByCrossNets 挑「与这批 role 跨接网最多」的子群(members 是各子群的
// 种子成员表)。全零返回空 —— 由调用方决定兜底去哪。
func bslBestByCrossNets(members map[string][]string, order []string, nets [][]string, roles []string) string {
	best, bestN := "", 0
	for _, name := range order {
		n := 0
		for _, m := range members[name] {
			for _, r := range roles {
				n += bslCrossNets(nets, m, r)
			}
		}
		if n > bestN {
			best, bestN = name, n
		}
	}
	return best
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
