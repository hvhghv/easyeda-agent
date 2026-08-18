package app

// sch_layout_objects.go — 一页一张「布局对象注册表」(ADR-0004 Decision 3)。
//
// 此前 `--zone`/`--group` 是**三套命名空间并存**(issue #181 补充评论第 1 条,
// 用户探了 5+ 轮才搞清):`sch zones set` 记模块名(POWER/MCU),`sch block-apply`
// 记块虚拟组名(esp32s3_wroom1_module(C1))与子组名(…/D_ESD),不同命令各认一套,
// 报错才泄露本命令认哪套。loadSchZoneModules 收敛过一半 —— 但它是「组优先、
// 认领整表隐身」的**或**,不是**并**:块页上 `zone move --zone POWER` 直接失联。
//
// 本文件是统一解析层:**不迁移底层存储**(SchZonesByPage 与 GroupsByPage 双读),
// 把两套存储投影成一张带来源标签的表,所有吃 --zone/--group 的命令走同一个
// 解析器。解析规则:精确名 > 大小写折叠 > 唯一前缀;组 id(g1)与子组末段
// (D_ESD)是别名;歧义报错列全部候选;解析失败必须列出本页全部可用名并标注
// 来源(`POWER (module claim)` / `esp32s3_wroom1_module(C1) (block)` /
// `…/D_ESD (subgroup)`)。

import (
	"fmt"
	"sort"
	"strings"
)

// layoutObjectSource 是布局对象的来源标签(报错/status 里原样展示)。
type layoutObjectSource string

const (
	// layoutSourceClaim:`sch zones set` 的模块认领(SchZonesByPage)。
	layoutSourceClaim layoutObjectSource = "module claim"
	// layoutSourceBlock:`sch block-apply` 登记的块整组(组名不含 "/")。
	layoutSourceBlock layoutObjectSource = "block"
	// layoutSourceSubgroup:块的功能子组(组名带块实例前缀,如 `…(U3)/D_ESD`)。
	layoutSourceSubgroup layoutObjectSource = "subgroup"
	// layoutSourceGroup:手工 `sch group create` 的虚拟组。
	layoutSourceGroup layoutObjectSource = "group"
)

// layoutObject 是注册表里的一个条目:恰好一个来源(Claim 与 Group 二选一非空)。
type layoutObject struct {
	Name    string             // 规范名(认领名 / 组名;无名组回落到组 id)
	Aliases []string           // 别名:组 id;子组的末段名
	Source  layoutObjectSource // 来源标签
	Claim   *schZoneClaim      // 来源=module claim 时非空(指向存储,写回可落地)
	Group   *schGroup          // 来源=block/subgroup/group 时非空(指向存储)
}

// keys 返回参与解析匹配的全部名字(规范名 + 别名)。
func (o *layoutObject) keys() []string {
	return append([]string{o.Name}, o.Aliases...)
}

// describe 渲染 `name (source)`,报错与 status 共用同一口径。
func (o *layoutObject) describe() string {
	return fmt.Sprintf("%s (%s)", o.Name, o.Source)
}

// zoneName 是 zone 语义消费者(zone move/tidy/relayout、分区框、note 登记)用的
// **区名投影**:子组取末段(与 schGroupModulesFromState / 分区计划的取名口径
// 一致,否则 band 查不到、写回落不下),其余原样。
func (o *layoutObject) zoneName() string {
	if o.Group == nil {
		return o.Name
	}
	name := o.Group.Name
	if i := strings.LastIndex(name, "/"); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	if name == "" {
		name = o.Group.ID
	}
	return name
}

// zoneClaim 把条目投影成 zone 消费者要的 claim:认领**直通存储指针**(写回可
// 落地);组投影出成员与说明的副本(往投影上写会蒸发 —— 写组数据请走 o.Group,
// 见 registerSchZoneNote)。
func (o *layoutObject) zoneClaim() *schZoneClaim {
	if o.Claim != nil {
		return o.Claim
	}
	return &schZoneClaim{
		Parts:   append([]string(nil), o.Group.Members...),
		NoteIDs: append([]string(nil), o.Group.NoteIDs...),
	}
}

// buildLayoutObjectTable 把两套存储投影成一张表(纯函数):认领按名排序在前,
// 组按登记顺序在后。不做任何去重/合并 —— 同名冲突留给解析器按歧义报错。
func buildLayoutObjectTable(claims map[string]*schZoneClaim, groups []*schGroup) []*layoutObject {
	var out []*layoutObject
	names := make([]string, 0, len(claims))
	for n, zc := range claims {
		if zc != nil && strings.TrimSpace(n) != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, &layoutObject{Name: n, Source: layoutSourceClaim, Claim: claims[n]})
	}
	for _, g := range groups {
		if g == nil {
			continue
		}
		name := strings.TrimSpace(g.Name)
		src := layoutSourceGroup
		var aliases []string
		switch {
		case strings.Contains(name, "/"):
			src = layoutSourceSubgroup
			if i := strings.LastIndex(name, "/"); i >= 0 && i+1 < len(name) {
				aliases = append(aliases, name[i+1:])
			}
		case g.BlockID != "" || g.Instance != "":
			src = layoutSourceBlock
		}
		if name == "" {
			name = g.ID
		} else if g.ID != "" && g.ID != name {
			aliases = append(aliases, g.ID)
		}
		out = append(out, &layoutObject{Name: name, Aliases: aliases, Source: src, Group: g})
	}
	return out
}

// describeLayoutObjects 渲染 `name (source)` 列表(逗号相接),用于报错与 status。
func describeLayoutObjects(table []*layoutObject) string {
	parts := make([]string, 0, len(table))
	for _, o := range table {
		parts = append(parts, o.describe())
	}
	return strings.Join(parts, ", ")
}

// layoutObjectsEmptyHint 是空表时的可执行修复路径(三种来源各给一条)。
const layoutObjectsEmptyHint = "本页没有任何布局对象 —— 块驱动的页用 `sch block-apply` 自动归组;" +
	"手工页 `sch group create`(虚拟组)或 `sch zones set`(模块认领)"

// resolveLayoutObject 按统一规则解析一个 --zone/--group 引用(纯函数):
// 精确名 > 大小写折叠 > 唯一前缀(均含别名);任一档命中多个即歧义,报错列
// 全部候选;三档全空报「未找到」并列出本页全部可用名(带来源)。
func resolveLayoutObject(table []*layoutObject, ref string) (*layoutObject, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		if len(table) == 0 {
			return nil, fmt.Errorf("--zone/--group 不能为空;%s", layoutObjectsEmptyHint)
		}
		return nil, fmt.Errorf("--zone/--group 不能为空 —— 本页可用布局对象:%s", describeLayoutObjects(table))
	}
	match := func(pred func(key string) bool) []*layoutObject {
		var hits []*layoutObject
		for _, o := range table {
			for _, k := range o.keys() {
				if pred(k) {
					hits = append(hits, o)
					break
				}
			}
		}
		return hits
	}
	lowerRef := strings.ToLower(ref)
	levels := []struct {
		label string
		hits  []*layoutObject
	}{
		{"精确名", match(func(k string) bool { return k == ref })},
		{"大小写折叠", match(func(k string) bool { return strings.EqualFold(k, ref) })},
		{"前缀", match(func(k string) bool { return strings.HasPrefix(strings.ToLower(k), lowerRef) })},
	}
	for _, lv := range levels {
		switch len(lv.hits) {
		case 0:
			continue
		case 1:
			return lv.hits[0], nil
		default:
			return nil, fmt.Errorf("%q 按%s匹配到 %d 个候选:%s —— 用精确全名(或组 id)",
				ref, lv.label, len(lv.hits), describeLayoutObjects(lv.hits))
		}
	}
	if len(table) == 0 {
		return nil, fmt.Errorf("未找到 %q —— %s", ref, layoutObjectsEmptyHint)
	}
	return nil, fmt.Errorf("未找到 %q —— 本页可用布局对象:%s", ref, describeLayoutObjects(table))
}

// requireLayoutGroup 是组语义消费者(group-move / group tidy)的类型门:命中了
// 但对象是模块认领时,说清来源并给出正确用法(#181:「命中但类型不适配也要
// 说清」),而不是让用户猜本命令认哪套命名。
func requireLayoutGroup(o *layoutObject, cmdName string) (*schGroup, error) {
	if o.Group != nil {
		return o.Group, nil
	}
	return nil, fmt.Errorf("%q 是 %s(`sch zones set` 的模块认领),`%s` 只作用于虚拟组 —— "+
		"整个模块请用 `sch zone move --zone %s` / `sch zone tidy --zone %s`;组的创建/成员见 `sch group --help`",
		o.Name, o.Source, cmdName, o.Name, o.Name)
}

// layoutObjectTableFromState 从已加载的 workflow 状态双读一页的两套存储
// (认领含 legacy 工程级表的回落,组按 documentUuid)。
func layoutObjectTableFromState(st *pcbStageState, docUUID string) []*layoutObject {
	if st == nil {
		return nil
	}
	return buildLayoutObjectTable(st.SchZonesForPage(docUUID), st.GroupsForPage(docUUID))
}

// loadLayoutObjectTable 解析工程、加载 workflow 状态并双读一页的注册表。
func loadLayoutObjectTable(cfg *appConfig, window, docUUID string) ([]*layoutObject, string, error) {
	project, err := resolveStageProject(cfg, window)
	if err != nil {
		return nil, "", err
	}
	st, err := loadPcbStageState(project)
	if err != nil {
		return nil, project, err
	}
	return layoutObjectTableFromState(st, docUUID), project, nil
}

// resolveLayoutZone 是消费命令的公共入口:一次查两套存储,返回命中条目(带
// 来源)、全表与工程名。解析失败的报错已带全量可用名 + 来源。
func resolveLayoutZone(cfg *appConfig, window, docUUID, ref string) (*layoutObject, []*layoutObject, string, error) {
	table, project, err := loadLayoutObjectTable(cfg, window, docUUID)
	if err != nil {
		return nil, nil, project, err
	}
	obj, rerr := resolveLayoutObject(table, ref)
	return obj, table, project, rerr
}
