package app

// sch_layout_objects_test.go — ADR-0004 Decision 3:一页一张「布局对象注册表」。
//
// 钉住四件事(issue #181 补充评论第 1 条的直接诉求):
//   1. **双源解析**:`sch zones set` 的模块认领与 `sch block-apply` 的虚拟组/子组
//      在同一张表里,同一个 --zone/--group 都查得到 —— 不再是「报错才泄露本命令
//      认哪套命名」;
//   2. **来源标注**:每个条目带 module claim / block / subgroup / group 标签;
//   3. **解析规则**:精确名 > 大小写折叠 > 唯一前缀,组 id 与子组末段是别名;
//      歧义报错列全部候选(带来源);
//   4. **报错规范**:解析失败必须列出本页全部可用名并标注来源。

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// registryFixture 是 #181 场景的缩影:一页上同时有模块认领(POWER/MCU)、
// 块整组(esp32s3_wroom1_module(C1))、块子组(ch340c_usb_serial(U3)/D_ESD)
// 和手工组(decaps)。
func registryFixture() []*layoutObject {
	claims := map[string]*schZoneClaim{
		"POWER": {Zone: "left-top", Parts: []string{"U3", "C5"}, NoteIDs: []string{"t9"}},
		"MCU":   {Zone: "center", Parts: []string{"U1"}},
	}
	groups := []*schGroup{
		{ID: "g1", Name: "esp32s3_wroom1_module(C1)", Members: []string{"U1", "C1"}, BlockID: "block.esp32s3_wroom1_module"},
		{ID: "g2", Name: "ch340c_usb_serial(U3)/D_ESD", Members: []string{"D1"}, BlockID: "block.ch340c_usb_serial", NoteIDs: []string{"t1"}},
		{ID: "g3", Name: "decaps", Members: []string{"C2", "C3"}},
	}
	return buildLayoutObjectTable(claims, groups)
}

func TestBuildLayoutObjectTable_DualSourceWithSourceLabels(t *testing.T) {
	table := registryFixture()
	if len(table) != 5 {
		t.Fatalf("双源表该有 5 条(2 认领 + 3 组),得到 %d", len(table))
	}
	want := map[string]layoutObjectSource{
		"POWER":                       layoutSourceClaim,
		"MCU":                         layoutSourceClaim,
		"esp32s3_wroom1_module(C1)":   layoutSourceBlock,
		"ch340c_usb_serial(U3)/D_ESD": layoutSourceSubgroup,
		"decaps":                      layoutSourceGroup,
	}
	got := map[string]layoutObjectSource{}
	for _, o := range table {
		got[o.Name] = o.Source
	}
	for name, src := range want {
		if got[name] != src {
			t.Errorf("%q 的来源该是 %q,得到 %q", name, src, got[name])
		}
	}
	// 无名组回落到组 id 当名字。
	table2 := buildLayoutObjectTable(nil, []*schGroup{{ID: "g7", Members: []string{"R1"}}})
	if len(table2) != 1 || table2[0].Name != "g7" {
		t.Fatalf("无名组该用组 id 当名,得到 %+v", table2)
	}
}

func TestResolveLayoutObject_ExactThenFoldThenPrefix(t *testing.T) {
	table := registryFixture()
	// 精确名。
	o, err := resolveLayoutObject(table, "POWER")
	if err != nil || o.Name != "POWER" || o.Source != layoutSourceClaim {
		t.Fatalf("精确名该命中认领 POWER:%+v %v", o, err)
	}
	// 大小写折叠。
	o, err = resolveLayoutObject(table, "power")
	if err != nil || o.Name != "POWER" {
		t.Fatalf("大小写折叠该命中 POWER:%+v %v", o, err)
	}
	// 唯一前缀。
	o, err = resolveLayoutObject(table, "esp32s3")
	if err != nil || o.Name != "esp32s3_wroom1_module(C1)" {
		t.Fatalf("唯一前缀该命中块整组:%+v %v", o, err)
	}
	// 精确名必须赢过折叠歧义(移植自旧 findZoneMoveClaim 用例)。
	ambi := buildLayoutObjectTable(map[string]*schZoneClaim{
		"power": {Parts: []string{"U9"}},
		"POWER": {Parts: []string{"U2"}},
	}, nil)
	if o, err := resolveLayoutObject(ambi, "POWER"); err != nil || o.Name != "POWER" {
		t.Fatalf("折叠歧义下精确名仍该命中:%+v %v", o, err)
	}
	if _, err := resolveLayoutObject(ambi, "Power"); err == nil || !strings.Contains(err.Error(), "power") || !strings.Contains(err.Error(), "POWER") {
		t.Fatalf("折叠歧义该报错并列候选:%v", err)
	}
	// 空 ref。
	if _, err := resolveLayoutObject(table, "  "); err == nil {
		t.Fatal("空 ref 该报错")
	}
}

func TestResolveLayoutObject_GroupIDAndSubgroupTailAliases(t *testing.T) {
	table := registryFixture()
	// 组 id 是别名(group-move --group g2 的既有用法不回归)。
	o, err := resolveLayoutObject(table, "g2")
	if err != nil || o.Group == nil || o.Group.ID != "g2" {
		t.Fatalf("组 id 该命中 g2:%+v %v", o, err)
	}
	// 子组末段是别名(zone tidy --zone D_ESD 的既有投影口径不回归)。
	o, err = resolveLayoutObject(table, "D_ESD")
	if err != nil || o.Group == nil || o.Group.ID != "g2" {
		t.Fatalf("子组末段该命中 g2:%+v %v", o, err)
	}
	// 全名照常命中。
	o, err = resolveLayoutObject(table, "ch340c_usb_serial(U3)/D_ESD")
	if err != nil || o.Group == nil || o.Group.ID != "g2" {
		t.Fatalf("子组全名该命中 g2:%+v %v", o, err)
	}
}

func TestResolveLayoutObject_NotFoundListsInventoryWithSources(t *testing.T) {
	table := registryFixture()
	_, err := resolveLayoutObject(table, "RF")
	if err == nil {
		t.Fatal("不存在的名字该报错")
	}
	msg := err.Error()
	for _, want := range []string{
		"POWER (module claim)",
		"esp32s3_wroom1_module(C1) (block)",
		"ch340c_usb_serial(U3)/D_ESD (subgroup)",
		"decaps (group)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("解析失败的报错该带来源地列出 %q,实际:%s", want, msg)
		}
	}
}

func TestResolveLayoutObject_EmptyTableGivesActionableError(t *testing.T) {
	_, err := resolveLayoutObject(nil, "POWER")
	if err == nil || !strings.Contains(err.Error(), "block-apply") ||
		!strings.Contains(err.Error(), "zones set") || !strings.Contains(err.Error(), "group create") {
		t.Fatalf("空表报错该给出三条建组/认领路径:%v", err)
	}
}

func TestResolveLayoutObject_AmbiguousTailListsCandidates(t *testing.T) {
	// 两个块实例各有一个 D_ESD 子组 —— 末段别名歧义,必须列出双方全名+来源。
	groups := []*schGroup{
		{ID: "g1", Name: "ch340c_usb_serial(U3)/D_ESD", Members: []string{"D1"}, BlockID: "b"},
		{ID: "g2", Name: "ch340c_usb_serial(U5)/D_ESD", Members: []string{"D2"}, BlockID: "b"},
	}
	table := buildLayoutObjectTable(nil, groups)
	_, err := resolveLayoutObject(table, "D_ESD")
	if err == nil {
		t.Fatal("跨实例同名子组该判歧义")
	}
	if !strings.Contains(err.Error(), "ch340c_usb_serial(U3)/D_ESD (subgroup)") ||
		!strings.Contains(err.Error(), "ch340c_usb_serial(U5)/D_ESD (subgroup)") {
		t.Fatalf("歧义报错该列出全部候选(带来源):%v", err)
	}
}

func TestRequireLayoutGroup_ClaimHitExplainsMismatchAndUsage(t *testing.T) {
	table := registryFixture()
	o, err := resolveLayoutObject(table, "POWER")
	if err != nil {
		t.Fatal(err)
	}
	// 命中了但类型不适配本命令:要说清来源并给正确用法。
	if _, gerr := requireLayoutGroup(o, "sch group-move"); gerr == nil ||
		!strings.Contains(gerr.Error(), "module claim") ||
		!strings.Contains(gerr.Error(), "zone move") {
		t.Fatalf("认领喂给组命令该报类型不适配并指路 zone move:%v", gerr)
	}
	// 组命中直接放行。
	o, _ = resolveLayoutObject(table, "decaps")
	if g, gerr := requireLayoutGroup(o, "sch group tidy"); gerr != nil || g == nil || g.ID != "g3" {
		t.Fatalf("组命中该放行:%+v %v", g, gerr)
	}
}

func TestLayoutObject_ZoneProjection(t *testing.T) {
	table := registryFixture()
	// 认领直通(指针原样,写回可落地)。
	o, _ := resolveLayoutObject(table, "POWER")
	if c := o.zoneClaim(); c == nil || len(c.Parts) != 2 || c.NoteIDs[0] != "t9" {
		t.Fatalf("认领该直通:%+v", c)
	}
	if o.zoneName() != "POWER" {
		t.Fatalf("认领的区名就是认领名,得到 %q", o.zoneName())
	}
	// 子组投影:区名取末段(与 schGroupModulesFromState / 分区框口径一致),
	// Parts/NoteIDs 跟着投影。
	o, _ = resolveLayoutObject(table, "g2")
	if o.zoneName() != "D_ESD" {
		t.Fatalf("子组区名该取末段 D_ESD,得到 %q", o.zoneName())
	}
	c := o.zoneClaim()
	if len(c.Parts) != 1 || c.Parts[0] != "D1" || len(c.NoteIDs) != 1 {
		t.Fatalf("子组该投影出成员与说明:%+v", c)
	}
	// 整组区名保持全名。
	o, _ = resolveLayoutObject(table, "g1")
	if o.zoneName() != "esp32s3_wroom1_module(C1)" {
		t.Fatalf("无 / 的组名原样当区名,得到 %q", o.zoneName())
	}
}

func TestLayoutObjectTableFromState_DualReadBothStores(t *testing.T) {
	// 双读:同一页同时有认领与虚拟组时,两套都可见 —— 这正是 loadSchZoneModules
	// 「组优先、认领整表隐身」的盲区(#181:zone move --zone POWER 在块页直接失联)。
	st := &pcbStageState{}
	st.SetSchZonesForPage("doc-1", map[string]*schZoneClaim{
		"POWER": {Zone: "left-top", Parts: []string{"U3"}},
	})
	st.SetGroupsForPage("doc-1", []*workflow.Group{
		{ID: "g1", Name: "esp32s3_wroom1_module(C1)", Members: []string{"U1"}, BlockID: "b"},
	})
	table := layoutObjectTableFromState(st, "doc-1")
	if _, err := resolveLayoutObject(table, "POWER"); err != nil {
		t.Fatalf("组存在时模块认领不该隐身:%v", err)
	}
	if _, err := resolveLayoutObject(table, "g1"); err != nil {
		t.Fatalf("虚拟组该照常命中:%v", err)
	}
	// 页隔离:别的页读不到。
	if got := layoutObjectTableFromState(st, "doc-2"); len(got) != 0 {
		t.Fatalf("doc-2 该是空表,得到 %d 条", len(got))
	}
	// legacy 工程级认领(SchZonesByPage 尚未启用)也要能读到。
	legacy := &pcbStageState{SchZones: map[string]*schZoneClaim{"MCU": {Parts: []string{"U1"}}}}
	if _, err := resolveLayoutObject(layoutObjectTableFromState(legacy, "doc-x"), "MCU"); err != nil {
		t.Fatalf("legacy 认领该可读:%v", err)
	}
}
