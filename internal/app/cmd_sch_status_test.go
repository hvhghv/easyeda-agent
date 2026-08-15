package app

import (
	"strings"
	"testing"
)

// 判据的核心契约:**unknown 不是 todo 的委婉说法**。这几条测试盯的正是
// 「空板被打成实心圆」那类失败 —— 判不了要留 unknown,没做要报 todo,
// 两者都不能被折成 done。

func stageOf(vs []schStageVerdict, stage string) schStageVerdict {
	for _, v := range vs {
		if v.Stage == stage {
			return v
		}
	}
	return schStageVerdict{}
}

func TestSchStageVerdicts_EmptyBoardIsNeverDone(t *testing.T) {
	pages := []schPageFacts{{Name: "P1_POWER", Reachable: true, HasSheet: true, NamedWell: true}}
	vs := schStageVerdicts(pages, schGateSummary{})
	for _, stage := range []string{"S2", "S3", "S4"} {
		if got := stageOf(vs, stage).State; got != schStageTodo {
			t.Errorf("%s on an empty page = %q, want %q", stage, got, schStageTodo)
		}
	}
	if got := stageOf(vs, "S1").State; got != schStageDone {
		t.Errorf("S1 = %q, want done (sheet + 有语义页名)", got)
	}
}

func TestSchStageVerdicts_S5UnknownUntilGateRuns(t *testing.T) {
	pages := []schPageFacts{{Name: "P1_POWER", Reachable: true, HasSheet: true, NamedWell: true,
		Parts: 5, Wires: 12, Groups: 5, Frames: 1, Notes: 2}}
	vs := schStageVerdicts(pages, schGateSummary{})
	if got := stageOf(vs, "S5").State; got != schStageUnknown {
		t.Fatalf("S5 without --gate = %q, want unknown — status 不报质量,更不能默认打勾", got)
	}
	// 全绿的四步不该被 S5 的 unknown 拖下水。
	for _, stage := range []string{"S1", "S2", "S3", "S4"} {
		if got := stageOf(vs, stage).State; got != schStageDone {
			t.Errorf("%s = %q, want done", stage, got)
		}
	}
	// 跑过 gate 才允许 done。
	vs = schStageVerdicts(pages, schGateSummary{Ran: true, Passed: 1, Total: 1})
	if got := stageOf(vs, "S5").State; got != schStageDone {
		t.Errorf("S5 after a passing gate = %q, want done", got)
	}
	vs = schStageVerdicts(pages, schGateSummary{Ran: true, Passed: 0, Total: 1, Fails: []string{"P1_POWER: clusters: 1 处组间过近"}})
	if got := stageOf(vs, "S5").State; got != schStagePartial {
		t.Errorf("S5 after a failing gate = %q, want partial", got)
	}
}

func TestSchStageVerdicts_S6AlwaysUnknown(t *testing.T) {
	// 平台不暴露脏标记 —— 无论画布多完整,S6 都只能是 unknown。这一条是故意的:
	// 拿「跑过 save 命令」冒充「已存盘」正是 workflow status 撒谎的同一个病。
	full := []schPageFacts{{Name: "P1_POWER", Reachable: true, HasSheet: true, NamedWell: true,
		Parts: 9, Wires: 30, Groups: 9, Frames: 1, Notes: 3}}
	if got := stageOf(schStageVerdicts(full, schGateSummary{Ran: true, Passed: 1, Total: 1}), "S6").State; got != schStageUnknown {
		t.Fatalf("S6 = %q, want unknown even on a complete page", got)
	}
}

func TestSchStageVerdicts_UnreachablePageBlocksTheVerdict(t *testing.T) {
	// **回归**:首个真机跑法里 4 页有 3 页切不过去,命令拿剩下那 1 页宣布
	// 「S1–S4 已就绪,下一步进 PCB」。把读不到的页排除出分母,等于让环境故障
	// 自动伪装成全绿 —— 页越读不到,结论越乐观。语义同 gate 的 blocked:
	// 检查器没跑完 ≠ 板子没问题。
	pages := []schPageFacts{
		{Name: "P1_POWER", Reachable: true, HasSheet: true, NamedWell: true, Parts: 5, Wires: 12, Groups: 5, Frames: 1, Notes: 1},
		{Name: "P2_MCU", Err: "切不过去"},
	}
	vs := schStageVerdicts(pages, schGateSummary{})
	for _, stage := range []string{"S1", "S2", "S3", "S4"} {
		v := stageOf(vs, stage)
		if v.State == schStageDone {
			t.Errorf("%s = done,但有页读不到 —— 判定不完整时不许打勾", stage)
		}
		if v.State == schStageUnknown && !strings.Contains(v.Detail, "判定不完整") {
			t.Errorf("%s 降级成 unknown 却没说明原因:%q", stage, v.Detail)
		}
	}
	// next 必须指向修环境,而不是往下走。
	next, why := schStatusNext(vs, pages)
	if !strings.Contains(next, "health") {
		t.Errorf("next = %q, want 修环境", next)
	}
	if !strings.Contains(why, "P2_MCU") {
		t.Errorf("why = %q, 该点名读不到的页", why)
	}
}

func TestSchStageVerdicts_PartialPages(t *testing.T) {
	pages := []schPageFacts{
		{Name: "P1_POWER", Reachable: true, HasSheet: true, NamedWell: true, Parts: 5, Wires: 12, Groups: 5, Frames: 1, Notes: 1},
		{Name: "P2_MCU", Reachable: true, HasSheet: true, NamedWell: true, Parts: 6, Wires: 0, Groups: 6, Frames: 0},
	}
	vs := schStageVerdicts(pages, schGateSummary{})
	if got := stageOf(vs, "S2").State; got != schStagePartial {
		t.Errorf("S2 (一页有框一页没有) = %q, want partial", got)
	}
	if got := stageOf(vs, "S4").State; got != schStagePartial {
		t.Errorf("S4 (一页没连线) = %q, want partial", got)
	}
}

func TestSchPlaceholderPageName(t *testing.T) {
	for _, name := range []string{"P1", "p2", "Page1", "page", "Schematic1", "sheet2", "", "  ", "3"} {
		if !schPlaceholderPageName(name) {
			t.Errorf("%q 应判为占位页名", name)
		}
	}
	for _, name := range []string{"P1_POWER", "P2_MCU", "usb_debug", "MCU", "P3_USB_DEBUG", "电源"} {
		if schPlaceholderPageName(name) {
			t.Errorf("%q 不该判为占位页名", name)
		}
	}
}

func TestSchStatusNext_PointsAtFirstUnfinishedStage(t *testing.T) {
	// unknown 不阻断:S5/S6 判不了,不该把「下一步」永远钉死在那里。
	done := []schPageFacts{{Name: "P1_POWER", DocUUID: "u1", Reachable: true, HasSheet: true,
		NamedWell: true, Parts: 5, Wires: 12, Groups: 5, Frames: 1, Notes: 1}}
	next, _ := schStatusNext(schStageVerdicts(done, schGateSummary{}), done)
	if next != "easyeda pcb import-changes" {
		t.Errorf("S1–S4 全绿时 next = %q, want the PCB handoff", next)
	}
	// 页名占位 → 指向改名,并带上真实 uuid(照抄即可执行)。
	bad := []schPageFacts{{Name: "P1", DocUUID: "abc123", Reachable: true, HasSheet: true, Parts: 4, Wires: 8, Groups: 4, Frames: 1, Notes: 1}}
	next, _ = schStatusNext(schStageVerdicts(bad, schGateSummary{}), bad)
	if !strings.Contains(next, "abc123") {
		t.Errorf("next = %q, want a page-rename carrying the real uuid", next)
	}
}
