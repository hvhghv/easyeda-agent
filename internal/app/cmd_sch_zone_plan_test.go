package app

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// The issue #149 real 6-module A4 page: the planner must carve it into
// non-overlapping, in-sheet partitions that each fully contain their module and
// leave the bottom-right title block a gap — validation all zero.
func TestPlanPartitions_RealA4SixModules(t *testing.T) {
	sheet := layoutBBox{0, 0, 1170, 825}
	keepout := &layoutBBox{912.6, 0, 1170, 115.5}
	mods := []partitionModule{
		{Name: "音频接口", BBox: layoutBBox{104.5, 579.5, 285.5, 660.5}},
		{Name: "调试接口", BBox: layoutBBox{574.5, 584.5, 600.5, 655.5}},
		{Name: "RGB与编码器", BBox: layoutBBox{954.45, 509.5, 995.5, 694.5}},
		{Name: "用户输入", BBox: layoutBBox{104.5, 184.5, 255.5, 262}},
		{Name: "显示接口", BBox: layoutBBox{499.5, 369.5, 600.5, 460.5}},
		{Name: "马达驱动", BBox: layoutBBox{909.5, 149.5, 1035.5, 273}},
	}
	plan := planPartitions(sheet, keepout, mods, defaultPartitionOpts())
	if len(plan.Partitions) == 0 {
		t.Fatal("no partitions produced")
	}
	if !plan.Validation.clean() {
		t.Fatalf("validation not clean: %+v\npartitions: %+v", plan.Validation, plan.Partitions)
	}
	// Every module must be assigned to exactly one partition and fully inside it.
	assigned := map[string]int{}
	for _, p := range plan.Partitions {
		for _, name := range p.Modules {
			assigned[name]++
		}
	}
	for _, m := range mods {
		if assigned[m.Name] != 1 {
			t.Errorf("module %q assigned %d times (want 1)", m.Name, assigned[m.Name])
		}
	}
	// The partition containing 马达驱动 (bottom-right) must clear the title block.
	for _, p := range plan.Partitions {
		if strInSlice(p.Modules, "马达驱动") {
			if p.BBox.MinY <= keepout.MaxY {
				t.Errorf("马达驱动 partition bottom %.1f not lifted above title block %.1f", p.BBox.MinY, keepout.MaxY)
			}
		}
	}
}

func TestPlanPartitions_TwoModules(t *testing.T) {
	sheet := layoutBBox{0, 0, 1170, 825}
	mods := []partitionModule{
		{Name: "主MCU", BBox: layoutBBox{456.5, 279.5, 713.5, 663.5}},
		{Name: "复位", BBox: layoutBBox{173.5, 146.5, 255.5, 270.5}},
	}
	plan := planPartitions(sheet, nil, mods, defaultPartitionOpts())
	if len(plan.Partitions) < 1 {
		t.Fatal("no partitions")
	}
	if !plan.Validation.clean() {
		t.Fatalf("2-module plan not clean: %+v", plan.Validation)
	}
}

func TestPlanPartitions_EmptyIsNoop(t *testing.T) {
	plan := planPartitions(layoutBBox{0, 0, 1170, 825}, nil, nil, defaultPartitionOpts())
	if len(plan.Partitions) != 0 || !plan.Validation.clean() {
		t.Errorf("empty input → empty clean plan, got %+v", plan)
	}
}

func TestComputePartitionPlanRejectsGeometryFromAnotherPage(t *testing.T) {
	t.Setenv(workflow.EnvDir, t.TempDir())
	st := &workflow.State{Project: "zone-project"}
	st.SetSchZonesForPage("page-a", map[string]*workflow.SchZoneClaim{
		"MCU": {Zone: "center", Parts: []string{"U1"}},
	})
	if err := workflow.Save(st); err != nil {
		t.Fatal(err)
	}

	cfg, _, cleanup := newAutolayoutTestDaemon(t, func(_ int, call autolayoutTestCall) string {
		switch call.Action {
		case "document.current":
			return autolayoutOK("page-a", `{"uuid":"page-a"}`)
		case "schematic.pages.list":
			return autolayoutOK("page-a", `{"pages":[{"uuid":"page-a","name":"Page A"}]}`)
		case "pcb.documents.list":
			return autolayoutOK("page-a", `{"pcbs":[]}`)
		case "project.current":
			return autolayoutOK("page-a", `{"friendlyName":"zone-project"}`)
		case "schematic.components.list":
			return autolayoutOK("page-b", `{"components":[],"count":0}`)
		default:
			return autolayoutOK("page-a", `{}`)
		}
	})
	defer cleanup()
	cfg.doc = "page-a"

	_, _, err := computePartitionPlan(cfg, "", "page-a", defaultPartitionOpts())
	if err == nil || !strings.Contains(err.Error(), "page drift") {
		t.Fatalf("cross-page geometry was not rejected: %v", err)
	}
}

// clusterSplits must split in the empty band between module bboxes, not through a
// straddling module.
func TestClusterSplits_NaturalGap(t *testing.T) {
	// Two clusters: [80,120] and [880,920] → one split in the 120↔880 band (~500).
	two := []axisInterval{{80, 120, 100}, {880, 920, 900}}
	got := clusterSplits(two, 12, 3)
	if len(got) != 1 {
		t.Fatalf("want 1 split for two clusters, got %v", got)
	}
	if got[0] < 400 || got[0] > 600 {
		t.Errorf("split %.0f should sit in the 120↔880 band (~500)", got[0])
	}
	// Intervals that OVERLAP on this axis (a tall module straddling) → no split.
	straddle := []axisInterval{{100, 700, 400}, {150, 260, 205}}
	if s := clusterSplits(straddle, 12, 3); len(s) != 0 {
		t.Errorf("overlapping intervals → no split, got %v", s)
	}
	// A band narrower than the gutter → no split (no room for two partitions).
	tight := []axisInterval{{100, 200, 150}, {205, 300, 252}}
	if s := clusterSplits(tight, 12, 3); len(s) != 0 {
		t.Errorf("5-unit band < 12 gutter → no split, got %v", s)
	}
}

// TestSchNoteBBoxEstimate + fold:登记的说明(zone 对象模型的内置对象)按内容
// 估算 bbox 并入模块画框口径;CoreBBox 不动(说明不参与图签/区名硬校验)。
func TestZoneNoteFoldEstimate(t *testing.T) {
	nb := schNoteBBoxEstimate(zoneMoveText{X: 330, Y: 640, Content: "AMS1117: 5V→3V3\nC2入/C3出/C1旁路", FontSize: 10})
	if nb.MinX != 330 || nb.MaxY != 640 {
		t.Fatalf("anchor must be top-left: %+v", nb)
	}
	if nb.MaxX <= 330+80 { // 两行 CJK 混排,最长行应显著宽于 80
		t.Fatalf("width estimate too small: %+v", nb)
	}
	if h := nb.MaxY - nb.MinY; h < 20 || h > 40 { // 2 行 × 10pt × 1.3
		t.Fatalf("height estimate off: %+v", nb)
	}
}

// ── 生成器与校验器同一把尺(P2_MCU 真机复现,2026-08-18)──────────────────
//
// 真机:esp32Mini P2 页,MCU 组 content 顶 770.5(离纸边 54.5,本体完全装得下),
// 但框 = content+pad24+titleBand30 = 824.5,距纸边 0.5 < sheetEdgeMinGap(12)
// → 自己产生的框被自己的 SheetMarginHits 拒绝,zone-draw 永远画不出来。
// 预留带(pad/title/note)是我们加的,不是内容 —— 撞纸边就该缩,内容一寸不让;
// 只有内容本体自己贴边时才有资格报 marginHit。图签方向已有此逻辑(说明带撞
// 图签就缩),纸边四周此前没有 —— 判定与生成两把尺。
func TestPlanPartitions_ReservedBandsYieldToSheetEdge(t *testing.T) {
	sheet := layoutBBox{0, 0, 1170, 825}
	keepout := &layoutBBox{468, 0, 1170, 198}
	mods := []partitionModule{
		// content 顶 770.5:+24+30=824.5 会贴到纸边(825-12=813 才合规)
		{Name: "MCU", BBox: layoutBBox{444.5, 265.5, 1053.5, 770.5}},
	}
	plan := planPartitions(sheet, keepout, mods, defaultPartitionOpts())
	if len(plan.Partitions) != 1 {
		t.Fatalf("want 1 partition, got %+v", plan.Partitions)
	}
	p := plan.Partitions[0]
	if plan.Validation.SheetMarginHits != 0 {
		t.Errorf("reserved bands must shrink at the sheet edge, got marginHits=%d bbox=%+v",
			plan.Validation.SheetMarginHits, p.BBox)
	}
	if p.BBox.MaxY > sheet.MaxY-sheetEdgeMinGap+0.01 {
		t.Errorf("frame top %.1f exceeds sheet edge budget %.1f", p.BBox.MaxY, sheet.MaxY-sheetEdgeMinGap)
	}
	// 内容一寸不让:框仍须包住模块。
	if !bboxContains(p.BBox, mods[0].BBox) {
		t.Errorf("clamped frame no longer contains its module: %+v vs %+v", p.BBox, mods[0].BBox)
	}
	if plan.Validation.TitleBlockHits != 0 {
		t.Errorf("note band must yield to the inflated title keepout, got titleBlockHits=%d bbox=%+v",
			plan.Validation.TitleBlockHits, p.BBox)
	}
}
