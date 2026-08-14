package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// ── ADR-0003 第一步:归组即封刚体,清页即作废组表 ─────────────────────────────

// `sch clear` 删光页面图元后,虚拟组表存的位号引用就全成了孤儿 —— 不作废它,
// 下一次 block-apply 想登记同名位号会撞上「该位号已属于组 gN」而拒绝归组
// (ADR-0003 落地时真机踩到:clear 后重跑 apply,归组被自己上一轮的残留挡住)。
func TestDropSchGroupsForPage_ClearsOrphanedGroups(t *testing.T) {
	t.Setenv(workflow.EnvDir, t.TempDir())
	const project, doc = "ceshi", "docA"

	st, err := loadPcbStageState(project)
	if err != nil {
		t.Fatal(err)
	}
	groups, _, err := groupsCreate(nil, "ch340c(C7)", []string{"U3", "C7", "J1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveSchGroups(st, doc, groups); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	dropSchGroupsForPage(project, doc, &errb)

	reloaded, err := loadPcbStageState(project)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.GroupsForPage(doc); len(got) != 0 {
		t.Errorf("清页后组表必须作废, 仍有 %d 个: %+v", len(got), got)
	}
	if !strings.Contains(errb.String(), "作废") {
		t.Errorf("必须告诉用户组表被一并作废了, got %q", errb.String())
	}
}

// 只作废**这一页**的组:同项目其他页的组表不受影响(组是 page-scoped)。
func TestDropSchGroupsForPage_OtherPagesUntouched(t *testing.T) {
	t.Setenv(workflow.EnvDir, t.TempDir())
	const project = "ceshi"

	st, err := loadPcbStageState(project)
	if err != nil {
		t.Fatal(err)
	}
	a, _, _ := groupsCreate(nil, "onPageA", []string{"U1"})
	b, _, _ := groupsCreate(nil, "onPageB", []string{"U2"})
	if err := saveSchGroups(st, "docA", a); err != nil {
		t.Fatal(err)
	}
	st2, _ := loadPcbStageState(project)
	if err := saveSchGroups(st2, "docB", b); err != nil {
		t.Fatal(err)
	}

	dropSchGroupsForPage(project, "docA", &bytes.Buffer{})

	reloaded, _ := loadPcbStageState(project)
	if got := reloaded.GroupsForPage("docA"); len(got) != 0 {
		t.Errorf("docA 的组该没了, 仍有 %d 个", len(got))
	}
	if got := reloaded.GroupsForPage("docB"); len(got) != 1 {
		t.Errorf("docB 的组不该被动, got %d 个", len(got))
	}
}

// 没有组时静默返回,不打扰用户(clear 是高频操作)。
func TestDropSchGroupsForPage_QuietWhenNoGroups(t *testing.T) {
	t.Setenv(workflow.EnvDir, t.TempDir())
	var errb bytes.Buffer
	dropSchGroupsForPage("ceshi", "docA", &errb)
	if errb.Len() != 0 {
		t.Errorf("无组时不该有输出, got %q", errb.String())
	}
}

// 组名 = 块名(去 block. 前缀)+ 实例号 —— 上层看到的是「哪个电路」,不是块 id。
func TestBapGroupName(t *testing.T) {
	for _, c := range []struct{ id, inst, want string }{
		{"block.ch340c_usb_serial", "C7", "ch340c_usb_serial(C7)"},
		{"block.led_indicator_gpio", "", "led_indicator_gpio"},
	} {
		if got := bapGroupName(bapPlan{BlockID: c.id, Instance: c.inst}); got != c.want {
			t.Errorf("bapGroupName(%q,%q) = %q, want %q", c.id, c.inst, got, c.want)
		}
	}
}
