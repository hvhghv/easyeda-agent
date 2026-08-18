package app

import (
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// 模块归属只有**一个读入口**(loadSchZoneModules:虚拟组优先、回落 zone 认领)。
// 这几条钉住两件事:组名怎么折成区名、写回该落到谁身上。
//
// 背景:`computePartitionPlan` 改成组优先后,另有六处仍直接读认领表,而
// `block-apply` 按设计**不写**认领 —— 于是块驱动的页在 sheet-tidy / zone-tidy /
// zone-move / zone-relayout / layout-score / note --zone 上全报「没有 zone 认领」。
// 真机实测:P3 页想用 `sch sheet tidy` 重排,直接被这句话挡住。

func TestLayoutRegistry_ReportedZoneNamesResolveBack(t *testing.T) {
	// 「读得到的区名要写得回去」:schGroupModules 展示的末段区名、报告里的全名与
	// 组 id,三种写法都必须能被统一解析器解析回同一个组(note --zone 的写回口径)。
	groups := []*schGroup{
		{ID: "g1", Name: "ch340c_usb_serial(U3)/J_USB", Members: []string{"J1", "R3"}},
		{ID: "g2", Name: "esp32_autodownload(Q)", Members: []string{"Q1", "Q2"}},
	}
	table := buildLayoutObjectTable(nil, groups)
	for _, ref := range []string{"J_USB", "ch340c_usb_serial(U3)/J_USB", "g1"} {
		if o, err := resolveLayoutObject(table, ref); err != nil || o.Group == nil || o.Group.ID != "g1" {
			t.Errorf("写法 %q 该命中 g1:%+v %v", ref, o, err)
		}
	}
	// 无前缀的组名按原样匹配。
	if o, err := resolveLayoutObject(table, "esp32_autodownload(Q)"); err != nil || o.Group == nil || o.Group.ID != "g2" {
		t.Errorf("无 / 前缀的组名该原样命中:%v", err)
	}
	if _, err := resolveLayoutObject(table, "NOPE"); err == nil {
		t.Error("不存在的区名该报错")
	}
}

func TestSchGroupModules_ProjectsMembersAndNotes(t *testing.T) {
	// schGroupModules 把组投影成 claim 供**读**路径统一;NoteIDs 必须跟着投影,
	// 否则画框时 fold 不进说明、zone move 也带不走它。
	st := &pcbStageState{}
	st.SetGroupsForPage("doc-1", []*workflow.Group{
		{ID: "g1", Name: "blk(U3)/J_USB", Members: []string{"J1", "R3"}, NoteIDs: []string{"t1", "t2"}},
		{ID: "g2", Name: "", Members: []string{"Q1"}},
	})
	got := schGroupModulesFromState(st, "doc-1")
	if len(got) != 2 {
		t.Fatalf("该投影出 2 个模块,得到 %d", len(got))
	}
	j := got["J_USB"]
	if j == nil {
		t.Fatalf("区名该取组名末段 J_USB,实际键:%v", keysOf(got))
	}
	if len(j.Parts) != 2 || j.Parts[0] != "J1" {
		t.Errorf("成员没投影对:%v", j.Parts)
	}
	if len(j.NoteIDs) != 2 {
		t.Errorf("NoteIDs 该跟着投影(画框要 fold 说明、zone move 要带走它),得到 %v", j.NoteIDs)
	}
	// 无名组回落到组 id,不能丢。
	if got["g2"] == nil {
		t.Errorf("无名组该用组 id 当区名,实际键:%v", keysOf(got))
	}
}

func keysOf(m map[string]*schZoneClaim) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
