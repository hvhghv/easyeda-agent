package app

// 说明带高度 + note-outside-zone 回归(REPORT-esp32mini-round2 新 1 / 新 2)。
//
// 新 1:noteBBox 高度曾写死 26(单行),SKILL 要求每模块 1~3 行说明(渲染高
// 36~50)——多行说明结构上塞不进带,被回退链踢到框外(7 条中 4 条框外)。
// 修法:带高按该区已登记说明的实际渲染高度预留(requiredNoteBand,同一把尺),
// 高度只从内容+字号推导(setZoneNoteHeights),绝不读落点 bbox —— 无自增长环。
// 新 2:交付判据只有存在性三条,框外说明零告警 —— 补 note-outside-zone。

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

const threeLineNoteH = 3 * schNoteDefaultFontSize * 1.3 // 39:3 行默认字号说明

// 新 1 判据主体:3 行说明的区,说明带按实际渲染高度预留,自动落点落在带内、
// 完整被分区框包含(旧 26 常数带下,同一说明落到框外下方 —— 报告的决定性对照)。
func TestPlanPartitions_NoteBandReservesRegisteredHeight(t *testing.T) {
	sheet := layoutBBox{0, 0, 1170, 825}
	mod := partitionModule{Name: "SY8089", BBox: layoutBBox{200, 400, 500, 700}, NoteHeight: threeLineNoteH}
	plan := planPartitions(sheet, nil, []partitionModule{mod}, defaultPartitionOpts())
	if len(plan.Partitions) != 1 {
		t.Fatalf("want 1 partition, got %+v", plan.Partitions)
	}
	p := plan.Partitions[0]
	band := p.NoteBBox.MaxY - p.NoteBBox.MinY
	if want := requiredNoteBand(threeLineNoteH); band != want {
		t.Fatalf("说明带高 %.1f,应按登记说明高度预留 %.1f", band, want)
	}
	// 带加高只向外扩:器件区下沿到框底的距离变大,content ± pad 一寸不挤。
	if p.BBox.MinY > mod.BBox.MinY-partitionContentPad-band+0.01 {
		t.Errorf("带加高必须向外扩(框底下探),got frame MinY %.1f", p.BBox.MinY)
	}
	// 自动落点:3 行说明(39 高)在带内候选就能命中,且整体在框内。
	w, h := 100.0, threeLineNoteH
	zr, nb := p.BBox, p.NoteBBox
	x, y, ok := planNoteAnchor(w, h, []layoutBBox{mod.BBox}, &zr, &nb, sheet, nil)
	if !ok {
		t.Fatal("3 行说明应能落点")
	}
	got := noteAnchorBBox(x, y, w, h)
	if !bboxContains(p.BBox, got) {
		t.Fatalf("3 行说明应落在分区框内:note %+v vs frame %+v", got, p.BBox)
	}
	if boxesGapOverlap(got, mod.BBox, 0) {
		t.Fatalf("说明不许压器件区:note %+v vs module %+v", got, mod.BBox)
	}
}

// 幂等收敛(报告判据):同样的登记状态,重复跑 zone-plan 必须得到相同几何 ——
// 带高来自「登记记录的文字尺寸」而非落点,结构上不存在自增长反馈环。
func TestPlanPartitions_NoteBandIdempotent(t *testing.T) {
	sheet := layoutBBox{0, 0, 1170, 825}
	mods := []partitionModule{
		{Name: "POWER", BBox: layoutBBox{100, 400, 400, 700}, NoteHeight: threeLineNoteH},
		{Name: "MCU", BBox: layoutBBox{600, 200, 1000, 700}},
	}
	first := planPartitions(sheet, nil, mods, defaultPartitionOpts())
	second := planPartitions(sheet, nil, mods, defaultPartitionOpts())
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("zone-plan 必须幂等:\nfirst  %+v\nsecond %+v", first, second)
	}
}

// 生成与预测同一把尺:placeSchNote 在登记**之前**用 extendZoneBandForNote 预扩
// 的框/带,必须与登记**之后** zone-plan 按 NoteHeight 重算出的框/带完全一致 ——
// 否则落点按 A 算、框按 B 画,说明又会飘到框外。
func TestExtendZoneBandForNote_MatchesReplan(t *testing.T) {
	sheet := layoutBBox{0, 0, 1170, 825}
	mod := partitionModule{Name: "LED", BBox: layoutBBox{300, 300, 600, 650}}
	before := planPartitions(sheet, nil, []partitionModule{mod}, defaultPartitionOpts()).Partitions[0]
	gotRect, gotBand := extendZoneBandForNote(before.BBox, before.NoteBBox, threeLineNoteH)

	mod.NoteHeight = threeLineNoteH
	after := planPartitions(sheet, nil, []partitionModule{mod}, defaultPartitionOpts()).Partitions[0]
	if gotRect != after.BBox {
		t.Errorf("预扩框 %+v ≠ 重算框 %+v", gotRect, after.BBox)
	}
	if gotBand != after.NoteBBox {
		t.Errorf("预扩带 %+v ≠ 重算带 %+v", gotBand, after.NoteBBox)
	}
	// 单行说明(13 高)装得进默认带 → 预扩必须是 no-op(带只增不缩)。
	r2, b2 := extendZoneBandForNote(before.BBox, before.NoteBBox, schNoteDefaultFontSize*1.3)
	if r2 != before.BBox || b2 != before.NoteBBox {
		t.Errorf("装得下的说明不该动框:rect %+v band %+v", r2, b2)
	}
}

// 带高只认内容+字号:同一批登记说明,不管落点坐标在哪(甚至在页角),推出的
// NoteHeight 相同 —— 这是「不重新引入自增长反馈环」的机械保证。
func TestSetZoneNoteHeights_PositionIndependent(t *testing.T) {
	zones := map[string]*workflow.SchZoneClaim{
		"POWER": {Parts: []string{"U1"}, NoteIDs: []string{"t1", "t-stale"}},
		"MCU":   {Parts: []string{"U2"}},
	}
	mods := func() []partitionModule {
		return []partitionModule{
			{Name: "POWER", BBox: layoutBBox{100, 400, 400, 700}},
			{Name: "MCU", BBox: layoutBBox{600, 200, 1000, 700}},
		}
	}
	a := mods()
	setZoneNoteHeights(zones, a, []zoneMoveText{{ID: "t1", X: 120, Y: 380, Content: "一\n二\n三", FontSize: 10}})
	b := mods()
	setZoneNoteHeights(zones, b, []zoneMoveText{{ID: "t1", X: 35, Y: 55, Content: "一\n二\n三", FontSize: 10}})
	if a[0].NoteHeight != threeLineNoteH || a[0].NoteHeight != b[0].NoteHeight {
		t.Errorf("NoteHeight 必须只由内容+字号决定:%v vs %v", a[0].NoteHeight, b[0].NoteHeight)
	}
	// 无登记的区保持 0(用默认带);stale 登记(t-stale 不在 texts 里)静默跳过。
	if a[1].NoteHeight != 0 {
		t.Errorf("未登记说明的区 NoteHeight 应为 0,got %v", a[1].NoteHeight)
	}
}

// ── 新 2:note-outside-zone 正负对照 ────────────────────────────────────────

func TestNoteOutsideZoneFindings_PositiveAndNegative(t *testing.T) {
	parts := []partitionRect{
		{Modules: []string{"POWER"}, BBox: layoutBBox{236, 502, 671, 754}},
		{Modules: []string{"MCU"}, BBox: layoutBBox{32, 180, 364, 760}},
	}
	zones := map[string]*workflow.SchZoneClaim{
		"POWER":  {Parts: []string{"U1"}, NoteIDs: []string{"t-out", "t-stale"}},
		"MCU":    {Parts: []string{"U2"}, NoteIDs: []string{"t-in"}},
		"NOPLAN": {Parts: []string{"J9"}, NoteIDs: []string{"t-noplan"}}, // 不在分区计划里
	}
	texts := []zoneMoveText{
		// 报告新 1 的真机取证坐标:SY8089 的说明 (250,445),框 {236,502}–{671,754} → 框外。
		{ID: "t-out", X: 250, Y: 445, Content: "SY8089: 5V→3V3\n1.5MHz\n2A", FontSize: 10},
		// 框内说明(锚点=左上,y-UP 向下排行;整个 bbox 在 MCU 框里)。
		{ID: "t-in", X: 50, Y: 300, Content: "WROOM 模组", FontSize: 10},
		// 未登记 zone 的自由文本:哪怕在所有框外,也绝不误伤。
		{ID: "t-free", X: 900, Y: 60, Content: "免责声明", FontSize: 10},
		{ID: "t-noplan", X: 900, Y: 800, Content: "无框区说明", FontSize: 10},
	}
	got := noteOutsideZoneFindingsFor(parts, zones, texts)
	if len(got) != 1 {
		t.Fatalf("恰应报 1 条(t-out),got %+v", got)
	}
	f := got[0]
	if f.Type != "note-outside-zone" || f.Level != "warn" || f.PrimitiveId != "t-out" {
		t.Errorf("finding 形态不对:%+v", f)
	}
	if f.At == nil || f.At.X != 250 || f.At.Y != 445 {
		t.Errorf("必须带说明坐标:%+v", f.At)
	}
	for _, want := range []string{`区 "POWER"`, "236", "754", "sch note --zone POWER", "prim-delete"} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("Message 缺 %q:%s", want, f.Message)
		}
	}
}

func TestNoteOutsideZoneFindings_NoRegistrationsNoFindings(t *testing.T) {
	parts := []partitionRect{{Modules: []string{"POWER"}, BBox: layoutBBox{0, 0, 100, 100}}}
	zones := map[string]*workflow.SchZoneClaim{"POWER": {Parts: []string{"U1"}}}
	texts := []zoneMoveText{{ID: "t9", X: 900, Y: 800, Content: "游离文本", FontSize: 10}}
	if got := noteOutsideZoneFindingsFor(parts, zones, texts); len(got) != 0 {
		t.Fatalf("未登记 zone 的文本不许误伤,got %+v", got)
	}
}
