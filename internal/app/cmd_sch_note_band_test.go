package app

// 说明带预留 + note-outside-zone 回归。
//
// 第一批(REPORT-esp32mini-round2 新 1/新 2):noteBBox 高度曾写死 26(单行),
// 多行说明结构上塞不进带、被回退链踢到框外;交付判据只有存在性三条,框外说明
// 零告警 —— 补 note-outside-zone。
//
// 第二批(2026-08-19 真机复验,本文件下半部分):**只修了高度,没修宽度**。
//   - 435 宽的说明配 435 宽的框:带内唯一候选点算出来就探出框外,再加上带内
//     x[604,686] 被邻区 L1 的桩线占住 → 落点退到「区外走廊」,说明落在框外下方;
//   - note-outside-zone 的修法(prim-delete 后重跑 sch note)必然死循环:重跑
//     落在**完全相同**的坐标;
//   - POWER_IN 区只有一个 2 脚端子,框宽 68,任何可读说明都比 68 宽 —— 结构上
//     装不进,于是永远报警。
// 修法:预留是**二维**的(requiredNoteWidth + requiredNoteBand),带内占用参与
// 求解,装不下就把框**扩边/下探**而不是把说明踢出去;生成侧(planner 第二遍)
// 与落点侧共用同一个 reserveZoneNoteArea。

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

// 带尺寸只认内容+字号:同一批登记说明,不管落点坐标在哪(甚至在页角),推出的
// NoteWidth/NoteHeight 相同 —— 这是「不重新引入自增长反馈环」的机械保证。
func TestSetZoneNoteSizes_PositionIndependent(t *testing.T) {
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
	setZoneNoteSizes(zones, a, []zoneMoveText{{ID: "t1", X: 120, Y: 380, Content: "一\n二\n三", FontSize: 10}})
	b := mods()
	setZoneNoteSizes(zones, b, []zoneMoveText{{ID: "t1", X: 35, Y: 55, Content: "一\n二\n三", FontSize: 10}})
	if a[0].NoteHeight != threeLineNoteH || a[0].NoteHeight != b[0].NoteHeight {
		t.Errorf("NoteHeight 必须只由内容+字号决定:%v vs %v", a[0].NoteHeight, b[0].NoteHeight)
	}
	if a[0].NoteWidth != 10 || a[0].NoteWidth != b[0].NoteWidth {
		t.Errorf("NoteWidth 必须只由内容+字号决定(1 个全角 @10 = 10):%v vs %v", a[0].NoteWidth, b[0].NoteWidth)
	}
	// 无登记的区保持 0(用默认带);stale 登记(t-stale 不在 texts 里)静默跳过。
	if a[1].NoteHeight != 0 || a[1].NoteWidth != 0 {
		t.Errorf("未登记说明的区尺寸应为 0,got %v/%v", a[1].NoteWidth, a[1].NoteHeight)
	}
}

// ── 新 2:note-outside-zone 正负对照 ────────────────────────────────────────

func TestNoteOutsideZoneFindings_PositiveAndNegative(t *testing.T) {
	parts := []partitionRect{
		{Modules: []string{"POWER"}, BBox: layoutBBox{236, 502, 671, 754}, NoteBBox: layoutBBox{236, 502, 671, 557}},
		{Modules: []string{"MCU"}, BBox: layoutBBox{32, 180, 364, 760}, NoteBBox: layoutBBox{32, 180, 364, 235}},
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
	for _, want := range []string{`区 "POWER"`, "236", "754", "sch note --zone POWER"} {
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

// 告警文案两档必须**都能执行**。旧文案只有一句「prim-delete 后重跑 sch note」,
// 在「说明比带宽」和「框只有 68 宽」两种情形下都是死循环(重跑落回同一坐标)。
func TestNoteOutsideZoneMessage_ActionableBothWays(t *testing.T) {
	frame := layoutBBox{236, 373, 671, 754}
	band := layoutBBox{236, 373, 671, 528}
	t.Run("带装得下→给算好的落点坐标", func(t *testing.T) {
		msg := noteOutsideZoneMessage("POWER", zoneMoveText{ID: "t1", X: 250, Y: 435,
			Content: "SY8089 5V→3V3\n2A 1.5MHz\n输入22uF 输出22uF", FontSize: 10}, frame, band)
		for _, want := range []string{"--x ", "--y ", "sch note --zone POWER", "prim-delete"} {
			if !strings.Contains(msg, want) {
				t.Errorf("缺可执行修法 %q:%s", want, msg)
			}
		}
	})
	t.Run("带装不下→绝不建议原样重跑", func(t *testing.T) {
		tiny := layoutBBox{116, 434, 184, 476} // 68 宽的窄带
		msg := noteOutsideZoneMessage("POWER_IN", zoneMoveText{ID: "t2", X: 50, Y: 400,
			Content: strings.Repeat("宽", 30), FontSize: 10}, layoutBBox{116, 434, 184, 614}, tiny)
		if !strings.Contains(msg, "别再原样重跑") {
			t.Errorf("装不下时必须明说别原样重跑(否则就是死循环):%s", msg)
		}
		if !strings.Contains(msg, "缩短文字") || !strings.Contains(msg, "group-move") {
			t.Errorf("装不下时必须给真正能改变结果的下一步:%s", msg)
		}
	})
}

// ── 第二批:宽度 + 带内占用 + 窄框扩边 ──────────────────────────────────────

// simulateNotePlacement 复刻 placeSchNote 的**纯几何**部分(折行 → 预留 → 落点),
// 让「生成侧 planner」与「落点侧 sch note」能在离线单测里对齐比对。
func simulateNotePlacement(plan partitionPlan, zone, content string, fontSize float64,
	obstacles []layoutBBox, sheet layoutBBox, keepout *layoutBBox, opts partitionOpts) (
	wrapped string, rect, band layoutBBox, x, y float64, ok bool) {

	zr, nb, _, matched := matchNotePartition(plan.Partitions, zone)
	if !matched {
		return "", layoutBBox{}, layoutBBox{}, 0, 0, false
	}
	wrapped = wrapNoteContent(content, fontSize, noteWrapWidth(zr.MaxX-zr.MinX))
	w, h := noteSizeOf(wrapped, fontSize)
	rect, band, x, y, ok = reserveZoneNoteArea(*zr, nb.MaxY, w, h, obstacles, sheet, keepout,
		partitionBaseRects(plan.Partitions, zone), opts.Gutter)
	return wrapped, rect, band, x, y, ok
}

// 情形 1(真机取证):宽说明 + 带内被邻区桩线占住。
//
// 负对照:planner 看不见占用时(noteObstacles=nil),带就是 [473,528] 那一条,
// 带内**没有**任何可用落点 —— 旧行为正是在这里跌进「区外走廊」,把说明甩到
// 框外下方 (250,435)。修复后:planner 认占用,把框底下探到占用之下,说明落回
// 框内。
func TestNoteBand_WideNoteWithOccupiedBand(t *testing.T) {
	sheet := layoutBBox{0, 0, 1170, 825}
	opts := defaultPartitionOpts()
	// 真机 P1_POWER 的量级:器件区 (260,552)..(647,700) → 框 (236,473)..(671,754)。
	mod := partitionModule{Name: "SY8089", BBox: layoutBBox{260, 552, 647, 700}}
	// 邻区 L1 的桩线/marker 伸进说明带(y 460..520 与带 [473,528] 相交)。
	intruder := layoutBBox{604, 460, 686, 520}
	obstacles := []layoutBBox{mod.BBox, intruder}
	content := strings.Repeat("宽", 40) + "\n" + strings.Repeat("宽", 40) + "\n" + strings.Repeat("宽", 40)

	// ① 落点侧:从「还没登记这条说明」的计划出发。
	plan0 := planPartitionsWithNotes(sheet, nil, []partitionModule{mod}, opts, obstacles)
	wrapped, rect, band, x, y, ok := simulateNotePlacement(plan0, "SY8089", content, 10, obstacles, sheet, nil, opts)
	if !ok {
		t.Fatalf("宽说明 + 带内占用必须仍能落进框里(扩边/下探),got 求解失败;band=%+v", band)
	}
	w, h := noteSizeOf(wrapped, 10)
	got := noteAnchorBBox(x, y, w, h)
	if !bboxContains(rect, got) {
		t.Fatalf("落点必须在框内:note %+v vs frame %+v", got, rect)
	}
	if boxesGapOverlap(got, intruder, noteGap) {
		t.Fatalf("落点仍压住带内占用:note %+v vs intruder %+v", got, intruder)
	}
	if rect.MinY >= plan0.Partitions[0].BBox.MinY {
		t.Errorf("带内有占用时框底必须下探:before %.0f after %.0f", plan0.Partitions[0].BBox.MinY, rect.MinY)
	}

	// ② 负对照:planner 对占用视而不见(旧行为),带里根本没有可用落点 ——
	//    说明只能被回退链踢出框,这正是真机症状的机械复现。
	blind := planPartitionsWithNotes(sheet, nil,
		[]partitionModule{{Name: "SY8089", BBox: mod.BBox, NoteWidth: w, NoteHeight: h}}, opts, nil)
	bp := blind.Partitions[0]
	if _, _, hit := scanNoteBand(bp.NoteBBox, bp.BBox, w, h, obstacles, sheet, nil); hit {
		t.Fatalf("负对照失效:不认占用的带 %+v 里本不该有可用落点", bp.NoteBBox)
	}
	if bx, by, fell := planNoteAnchor(w, h, obstacles, &bp.BBox, &bp.NoteBBox, sheet, nil); fell &&
		bboxContains(bp.BBox, noteAnchorBBox(bx, by, w, h)) {
		t.Fatalf("负对照失效:旧行为应把说明甩出框外,got (%g,%g) 仍在框内", bx, by)
	}

	// ③ 生成与判定同一把尺:登记之后 planner 重算出的框/带,必须与落点侧
	//    预留出来的逐字段相同,且包含落点 —— note-outside-zone 结构上不会响。
	after := planPartitionsWithNotes(sheet, nil,
		[]partitionModule{{Name: "SY8089", BBox: mod.BBox, NoteWidth: w, NoteHeight: h}}, opts, obstacles)
	ap := after.Partitions[0]
	if ap.BBox != rect || ap.NoteBBox != band {
		t.Fatalf("planner 重算与落点预留分家了:\n plan  %+v / %+v\n place %+v / %+v",
			ap.BBox, ap.NoteBBox, rect, band)
	}
	if !bboxContains(ap.BBox, got) {
		t.Fatalf("重算后的框必须包住落点:frame %+v note %+v", ap.BBox, got)
	}
	zones := map[string]*workflow.SchZoneClaim{"SY8089": {Parts: []string{"U1"}, NoteIDs: []string{"n1"}}}
	texts := []zoneMoveText{{ID: "n1", X: x, Y: y, Content: wrapped, FontSize: 10}}
	if f := noteOutsideZoneFindingsFor(after.Partitions, zones, texts); len(f) != 0 {
		t.Fatalf("修复后不该再报 note-outside-zone:%+v", f)
	}
}

// 情形 2(真机取证):窄框(68 宽,区里只有一个 2 脚接线端子)。
// 任何可读说明都比 68 宽 —— 策略是**框为说明横向扩边**,而不是「既装不进又
// 永远报警」。扩边不许越过纸边/图签/邻框,所以不会自己造出 partitionOverlap。
func TestNoteBand_NarrowZoneWidensForReadableNote(t *testing.T) {
	sheet := layoutBBox{0, 0, 1170, 825}
	opts := defaultPartitionOpts()
	mod := partitionModule{Name: "POWER_IN", BBox: layoutBBox{140, 500, 160, 560}}
	obstacles := []layoutBBox{mod.BBox}
	content := "5V 输入端子 共地 丝印标极性" // 任何可读说明都宽于 68

	plan0 := planPartitionsWithNotes(sheet, nil, []partitionModule{mod}, opts, obstacles)
	if w := plan0.Partitions[0].BBox.MaxX - plan0.Partitions[0].BBox.MinX; w > 100 {
		t.Fatalf("fixture 失效:这一区本该是窄框,got 宽 %.0f", w)
	}
	wrapped, rect, band, x, y, ok := simulateNotePlacement(plan0, "POWER_IN", content, 10, obstacles, sheet, nil, opts)
	if !ok {
		t.Fatal("窄框必须为说明扩边并落点成功,而不是装不进")
	}
	w, h := noteSizeOf(wrapped, 10)
	if w < noteMinReadableWidth-noteGap {
		t.Errorf("说明不该被切成竖排单字:折完宽 %.0f", w)
	}
	if rect.MaxX-rect.MinX < requiredNoteWidth(w)-0.01 {
		t.Errorf("框应横向扩到 %.0f,实际 %.0f", requiredNoteWidth(w), rect.MaxX-rect.MinX)
	}
	got := noteAnchorBBox(x, y, w, h)
	if !bboxContains(rect, got) {
		t.Fatalf("扩边后落点必须在框内:note %+v frame %+v", got, rect)
	}
	// 器件区一寸不挤:扩边只向外长,原框仍被包住。
	if !bboxContains(rect, plan0.Partitions[0].BBox) {
		t.Errorf("扩边只许向外长:new %+v 应包住 old %+v", rect, plan0.Partitions[0].BBox)
	}

	// 登记之后 planner 重算 = 落点侧预留;note-outside-zone 归零(不再是那条
	// 「既装不进又永远报警」的噪音)。
	after := planPartitionsWithNotes(sheet, nil,
		[]partitionModule{{Name: "POWER_IN", BBox: mod.BBox, NoteWidth: w, NoteHeight: h}}, opts, obstacles)
	ap := after.Partitions[0]
	if ap.BBox != rect || ap.NoteBBox != band {
		t.Fatalf("窄框扩边两侧分家:\n plan  %+v / %+v\n place %+v / %+v", ap.BBox, ap.NoteBBox, rect, band)
	}
	zones := map[string]*workflow.SchZoneClaim{"POWER_IN": {Parts: []string{"J1"}, NoteIDs: []string{"n1"}}}
	texts := []zoneMoveText{{ID: "n1", X: x, Y: y, Content: wrapped, FontSize: 10}}
	if f := noteOutsideZoneFindingsFor(after.Partitions, zones, texts); len(f) != 0 {
		t.Fatalf("窄框扩边后不该再报 note-outside-zone:%+v", f)
	}
	if v := after.Validation; !v.clean() {
		t.Fatalf("为说明扩边不许自己撑出违规:%+v", v)
	}
}

// 扩边不许越过邻区的**基础框**:否则 zone-draw 会因为我们自己撑出来的
// partitionOverlap 拒绝画框,把「永远报警」换成「永远画不出」。
func TestNoteBand_WideningStopsAtNeighborFrame(t *testing.T) {
	sheet := layoutBBox{0, 0, 1170, 825}
	opts := defaultPartitionOpts()
	mods := []partitionModule{
		{Name: "POWER_IN", BBox: layoutBBox{140, 500, 160, 560}, NoteWidth: 200, NoteHeight: 13},
		{Name: "MCU", BBox: layoutBBox{300, 400, 700, 700}},
	}
	plan := planPartitionsWithNotes(sheet, nil, mods, opts, nil)
	if len(plan.Partitions) != 2 {
		t.Fatalf("want 2 partitions, got %d", len(plan.Partitions))
	}
	var narrow, mcu partitionRect
	for _, p := range plan.Partitions {
		if strInSlice(p.Modules, "POWER_IN") {
			narrow = p
		} else {
			mcu = p
		}
	}
	if narrow.BBox.MaxX > mcu.BaseBBox.MinX-opts.Gutter+0.01 {
		t.Errorf("扩边越过了邻框:narrow %+v vs neighbor base %+v", narrow.BBox, mcu.BaseBBox)
	}
	if v := plan.Validation; v.PartitionOverlap != 0 {
		t.Errorf("扩边不许造出 partitionOverlap:%+v", v)
	}
}

// 幂等/收敛:同一条说明反复走 zone-plan 与落点,结果一步不漂 ——
// 防的是「放一条 note → 带变 → 框动 → note 又不在带里」的自增长环回归。
func TestNoteBand_PlacementConverges(t *testing.T) {
	sheet := layoutBBox{0, 0, 1170, 825}
	opts := defaultPartitionOpts()
	mod := partitionModule{Name: "SY8089", BBox: layoutBBox{260, 552, 647, 700}}
	obstacles := []layoutBBox{mod.BBox, {604, 460, 686, 520}}
	content := strings.Repeat("宽", 40) + "\n" + strings.Repeat("宽", 40) + "\n" + strings.Repeat("宽", 40)

	plan0 := planPartitionsWithNotes(sheet, nil, []partitionModule{mod}, opts, obstacles)
	wrapped, rect, band, x, y, ok := simulateNotePlacement(plan0, "SY8089", content, 10, obstacles, sheet, nil, opts)
	if !ok {
		t.Fatal("首次落点应成功")
	}
	w, h := noteSizeOf(wrapped, 10)
	registered := []partitionModule{{Name: "SY8089", BBox: mod.BBox, NoteWidth: w, NoteHeight: h}}

	plan1 := planPartitionsWithNotes(sheet, nil, registered, opts, obstacles)
	plan2 := planPartitionsWithNotes(sheet, nil, registered, opts, obstacles)
	if !reflect.DeepEqual(plan1, plan2) {
		t.Fatalf("zone-plan 必须幂等:\n1 %+v\n2 %+v", plan1, plan2)
	}
	// 说明已登记后再走一次落点(等价于「重跑 sch note」):同样的框、同样的带、
	// 同样的锚点 —— 不再有「重跑落到别处」或「框又长一截」。
	w2, r2, b2, x2, y2, ok2 := simulateNotePlacement(plan1, "SY8089", wrapped, 10, obstacles, sheet, nil, opts)
	if !ok2 || w2 != wrapped || r2 != rect || b2 != band || x2 != x || y2 != y {
		t.Fatalf("重跑落点必须收敛:\n first  %+v/%+v @(%g,%g)\n second %+v/%+v @(%g,%g) ok=%v",
			rect, band, x, y, r2, b2, x2, y2, ok2)
	}
}
