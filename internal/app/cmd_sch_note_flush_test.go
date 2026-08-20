package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// cmd_sch_note_flush_test.go — 「说明贴着分区框底边」的正负对照。
//
// 用户 2026-08-20:「note 的位置还是不行,能直接排到框选 zone 的置底吗?」
// 真机量到的症状是**离框底的距离一条一个样**(41…68),而且每条下面都白空一截:
//
//	esp32s3_wroom1_module  y=290  框底 248  距底 42   (2 行 @10)
//	U_EN                   y=510  框底 456  距底 54   (3 行 @10)
//	U_IO0                  y=635  框底 568  距底 67   (4 行 @10)
//	U_3V3                  y=425  框底 372  距底 53   (3 行 @9)
//	tactile_boot_reset     y=80   框底 28   距底 52
//
// 根因不是"求解器随机挑高度",是**锚点语义搞反了**:落点侧按
// `y = 带底 + 块高 + noteGap` 写坐标,以为把块的**顶**放在那儿;而
// `eda.sch_Primitive.getPrimitivesBBox` 实测 5/5 例 `bbox.minY == 锚点 y` ——
// 锚点是块的**底**,块向上长。于是"块高"整个变成了离框底的距离:2 行 42、
// 3 行 55、4 行 68,行数越多飘得越高。
//
// 修完之后离框底恒为 noteBottomInset,与行数/字号无关。本文件钉住这条,以及
// 三条不许被"贴底"这个位置约束顺手破坏的性质(不压图元 / 不覆盖显式坐标 /
// 装不下要如实说)。

// flushCase 是一条真实说明的形状:它那个区的框 y 区间 + 说明的行数/字号。
// 框 y 区间照抄真机(2026-08-20,工程 ceshi,A4 三页);行数与字号取自同一次
// getPrimitivesBBox 实测(能对上的几条)与按 `距底 = 块高 + noteGap` 反推
// (其余几条)—— 用例验的是"行数/字号不许影响离框底的距离",所以取值只需要
// **覆盖真机出现过的档位**(2/3/4 行 × 字号 9/10/11)。
type flushCase struct {
	zone      string
	frameMinY float64
	frameMaxY float64
	lines     int
	fontSize  float64
	// realDistWas 是**修复前真机量到的**"锚点 y − 框底"。它就是本次的症状本体:
	// 12 条各不相同(41…68),而且恰好 = 块高 + noteGap。
	realDistWas float64
}

func realFlushCases() []flushCase {
	return []flushCase{
		{"POWER_IN", 612, 791, 4, 10, 68},
		{"sy8089_buck_3v3", 236, 802, 3, 11, 59},
		{"esp32s3_wroom1_module", 248, 790, 2, 10, 42},
		{"U_EN", 456, 790, 3, 10, 54},
		{"U_IO0", 568, 802, 4, 10, 67},
		{"led_indicator_gpio", 566, 792, 3, 10, 54},
		{"U_3V3", 372, 791, 3, 9, 53},
		{"tactile_boot_reset", 28, 380, 2, 10, 52},
		{"U", 394, 813, 3, 10, 56},
		{"J_USB", 330, 790, 3, 10, 55},
		{"D_ESD", 120, 298, 2, 11, 45},
		{"esp32_autodownload", 324, 790, 2, 10, 41},
	}
}

// noteContentOf 造一条 lines 行、每行窄到肯定装得进框的说明。
func noteContentOf(lines int) string {
	ls := make([]string, lines)
	for i := range ls {
		ls[i] = fmt.Sprintf("说明第%d行", i+1)
	}
	return strings.Join(ls, "\n")
}

// moduleForFrame 反推一个器件区 bbox,让**还没登记说明**的分区框恰好落在
// [frameMinY, frameMaxY] —— 这样 fixture 用的就是真机那批框本身。
func moduleForFrame(name string, x0, x1, frameMinY, frameMaxY float64, opts partitionOpts) partitionModule {
	b := layoutBBox{
		MinX: x0 + partitionContentPad,
		MinY: frameMinY + partitionContentPad + opts.NoteBand,
		MaxX: x1 - partitionContentPad,
		MaxY: frameMaxY - partitionContentPad - opts.TitleBand,
	}
	return partitionModule{Name: name, BBox: b, CoreBBox: b}
}

// placeOneFlushCase 跑一条说明的完整落点链(与 `sch note --zone` 的纯几何部分
// 同一条路):建计划 → 折行 → reserveZoneNoteArea 预留 → 贴底落点。
// 返回**最终会被 zone-draw 画出来的那个框**与落点。
func placeOneFlushCase(t *testing.T, c flushCase, sheet layoutBBox) (rect, band layoutBBox, x, y, h float64) {
	t.Helper()
	opts := defaultPartitionOpts()
	mod := moduleForFrame(c.zone, 120, 420, c.frameMinY, c.frameMaxY, opts)
	obstacles := []layoutBBox{mod.BBox}
	plan := planPartitionsWithNotes(sheet, nil, []partitionModule{mod}, opts, obstacles)
	if len(plan.Partitions) != 1 {
		t.Fatalf("%s: want 1 partition, got %d", c.zone, len(plan.Partitions))
	}
	if got := plan.Partitions[0].BBox.MinY; math.Abs(got-c.frameMinY) > 0.01 {
		t.Fatalf("%s: fixture 失效 —— 未登记说明时的框底应为 %.0f,实际 %.0f", c.zone, c.frameMinY, got)
	}
	wrapped, rect, band, x, y, ok := simulateNotePlacement(plan, c.zone,
		noteContentOf(c.lines), c.fontSize, obstacles, sheet, nil, opts)
	if !ok {
		t.Fatalf("%s: 这条说明(%d 行 @%.0f)应当能贴底落进带里,却求解失败;band=%+v", c.zone, c.lines, c.fontSize, band)
	}
	_, h = noteSizeOf(wrapped, c.fontSize)
	return rect, band, x, y, h
}

// ── 正对照:12 条真实说明,底边到自己框底的距离必须是**同一个常量** ──────────
func TestNoteFlush_AllRealNotesShareOneBottomInset(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	type got struct {
		c      flushCase
		inset  float64
		oldWay float64
	}
	var results []got
	for _, c := range realFlushCases() {
		rect, band, x, y, h := placeOneFlushCase(t, c, sheet)
		// ① 底边内缩:锚点即底边(实测),所以这就是 y - 框底。
		inset := y - rect.MinY
		if math.Abs(inset-noteBottomInset) > 1e-9 {
			t.Errorf("%s(%d 行 @%.0f):离框底 %.4f,应恒为 %.0f", c.zone, c.lines, c.fontSize, inset, noteBottomInset)
		}
		// ② 说明必须整个待在**带内**(带顶就是器件区下沿,探出去就是压器件)。
		full := noteAnchorBBox(x, y, 1, h)
		if !bboxContains(band, full) || !bboxContains(rect, full) {
			t.Errorf("%s:落点 %+v 不在带 %+v / 框 %+v 内", c.zone, full, band, rect)
		}
		// 旧式(把锚点当左上角)会给出的距离 —— 留作用例的自证:它必须随行数变。
		results = append(results, got{c, inset, h + noteGap})
	}
	// ③ 同一个常量:12 条逐条相等,行数/字号一个字都改不动它。
	for _, r := range results[1:] {
		if r.inset != results[0].inset {
			t.Fatalf("%s 的内缩 %.4f ≠ %s 的 %.4f —— 底边没有齐平",
				r.c.zone, r.inset, results[0].c.zone, results[0].inset)
		}
	}
	// ④ 关键项:3 行的 U_3V3 与 2 行的那几条**底边齐平**。
	var threeLine, twoLine []got
	for _, r := range results {
		switch r.c.lines {
		case 2:
			twoLine = append(twoLine, r)
		case 3:
			threeLine = append(threeLine, r)
		}
	}
	if len(twoLine) == 0 || len(threeLine) == 0 {
		t.Fatal("fixture 失效:必须同时含 2 行与 3 行说明")
	}
	for _, a := range threeLine {
		for _, b := range twoLine {
			if a.inset != b.inset {
				t.Errorf("行数影响了底边:%s(3 行)%.4f vs %s(2 行)%.4f", a.c.zone, a.inset, b.c.zone, b.inset)
			}
		}
	}
	// ⑤ 自证 fixture 有鉴别力。两条:
	//    (a) 真机修复前这 12 条的"距底"本来就是散的(41…68,≥5 个不同值);
	//    (b) 旧式换算(锚点当左上角:距底 = 块高 + noteGap)在本 fixture 上同样
	//        散得开 —— 也就是说,把贴底逻辑改回旧式,①/③/④ 必然转红。
	//    这两条若变绿,说明 fixture 退化成了"怎么改都过"。
	measured := map[float64]bool{}
	oldWays := map[float64]bool{}
	for _, r := range results {
		measured[r.c.realDistWas] = true
		oldWays[r.oldWay] = true
		if r.oldWay < 30 {
			t.Fatalf("fixture 失效:%s 旧式距底 %.1f 太小,复现不出真机的 41…68", r.c.zone, r.oldWay)
		}
	}
	if len(measured) < 5 {
		t.Fatalf("fixture 失效:真机 12 条的距底应当是散的(实测 41…68),只见到 %d 个不同值", len(measured))
	}
	if len(oldWays) < 3 {
		t.Fatalf("fixture 失去鉴别力:旧式换算只产生 %d 种距底,应 ≥3 种(2/3/4 行 × 字号)", len(oldWays))
	}
	// ⑥ 用户的另一半诉求:内缩要小到"下面不再白空 40+"。
	if noteBottomInset >= 40 {
		t.Fatalf("noteBottomInset=%.0f 又把 40+ 的空隙留回来了", noteBottomInset)
	}
}

// ── 负对照 A:贴底不是免检 —— 带底被占住时绝不压上去 ───────────────────────
//
// 两半都要验:
//
//	A1 还能下探时:框底下探到占用之下,说明**仍然贴底**(位置约束被保住,
//	   代价由框承担);
//	A2 下探被纸边/邻框顶死时:求解**失败**(ok=false),绝不为了贴底压上去。
func TestNoteFlush_NeverOverlapsToStayFlush(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	opts := defaultPartitionOpts()

	t.Run("A1 带底被占→框底下探,说明仍贴底", func(t *testing.T) {
		mod := partitionModule{Name: "SY8089", BBox: layoutBBox{260, 552, 647, 700}}
		// 邻区桩线伸进说明带,横跨整条带宽 —— 带底那一行没有任何可用 x。
		intruder := layoutBBox{236, 470, 671, 520}
		obstacles := []layoutBBox{mod.BBox, intruder}
		plan := planPartitionsWithNotes(sheet, nil, []partitionModule{mod}, opts, obstacles)
		_, rect, band, x, y, ok := simulateNotePlacement(plan, "SY8089", noteContentOf(3), 10, obstacles, sheet, nil, opts)
		if !ok {
			t.Fatalf("下探空间充足,应当仍能贴底落点;band=%+v", band)
		}
		if inset := y - rect.MinY; math.Abs(inset-noteBottomInset) > 1e-9 {
			t.Errorf("下探之后仍必须贴底:内缩 %.4f,want %.0f", inset, noteBottomInset)
		}
		_, h := noteSizeOf(noteContentOf(3), 10)
		box := noteAnchorBBox(x, y, 100, h)
		if boxesGapOverlap(box, intruder, noteGap) {
			t.Errorf("为了贴底压上了带内占用:%+v vs %+v", box, intruder)
		}
		if boxesGapOverlap(box, mod.BBox, noteGap) {
			t.Errorf("为了贴底压上了器件区:%+v vs %+v", box, mod.BBox)
		}
		if rect.MinY >= plan.Partitions[0].BBox.MinY {
			t.Errorf("带底被占时框底应下探:before %.0f after %.0f", plan.Partitions[0].BBox.MinY, rect.MinY)
		}
	})

	t.Run("A2 下探被顶死→如实失败,不压上去", func(t *testing.T) {
		// 器件区几乎贴着纸边下沿:说明带只有 floor..bandTop 这么点,而且被一条
		// 横跨整条带的占用堵死 —— 再往下就穿出纸边,没有任何合法落点。
		mod := partitionModule{Name: "POWER_IN", BBox: layoutBBox{200, 100, 500, 300}}
		blocker := layoutBBox{100, 0, 600, 92} // 盖住整条带
		obstacles := []layoutBBox{mod.BBox, blocker}
		plan := planPartitionsWithNotes(sheet, nil, []partitionModule{mod}, opts, obstacles)
		_, rect, band, x, y, ok := simulateNotePlacement(plan, "POWER_IN", noteContentOf(3), 10, obstacles, sheet, nil, opts)
		if ok {
			_, h := noteSizeOf(noteContentOf(3), 10)
			box := noteAnchorBBox(x, y, 100, h)
			if boxesGapOverlap(box, blocker, noteGap) {
				t.Fatalf("为了贴底压到了占用上:%+v vs %+v", box, blocker)
			}
			t.Fatalf("带底被顶死时应求解失败,却给出了 (%g,%g);rect=%+v band=%+v", x, y, rect, band)
		}
		// 失败必须给得出"哪一维不够"的可执行下一步(报文本体见负对照 C)。
		if band.MaxY-band.MinY <= 0 {
			t.Fatalf("失败时仍要交出可解释的带几何,got %+v", band)
		}
	})
}

// ── 负对照 B:显式 --x/--y 一字不改(贴底只管自动落点)────────────────────
func TestNoteFlush_ExplicitCoordsAreVerbatim(t *testing.T) {
	cfg, cleanup := newFakeNotePageDaemon(t)
	defer cleanup()

	content := noteContentOf(3)
	// 故意给一个**离贴底行很远**的坐标,而且压在器件上:坐标必须原样返回,
	// 只补一句警告。
	x, y := 733.0, 611.0
	warns, _, err := placeSchNote(cfg, "", "", "", &content, 10, false, &x, &y)
	if err != nil {
		t.Fatalf("显式坐标不该报错:%v", err)
	}
	if x != 733 || y != 611 {
		t.Fatalf("显式坐标被改写成 (%g,%g)", x, y)
	}
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, "\n"), "压住了已有图元") {
		t.Errorf("压到图元必须明确警告(不静默):%v", warns)
	}

	// 不压任何东西的显式坐标同样一字不改,且不该被"贴底"顺手挪走。
	x2, y2 := 60.0, 700.0
	if _, _, err := placeSchNote(cfg, "", "", "", &content, 10, false, &x2, &y2); err != nil {
		t.Fatalf("显式坐标不该报错:%v", err)
	}
	if x2 != 60 || y2 != 700 {
		t.Fatalf("显式坐标被贴底覆盖成 (%g,%g)", x2, y2)
	}
}

// newFakeNotePageDaemon:一页 = 图框 + 一个器件 + 一条已有说明。
func newFakeNotePageDaemon(t *testing.T) (*appConfig, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"service":"easyeda-agent","windows":[]}`))
			return
		}
		if r.URL.Path != "/action" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		_ = json.Unmarshal(buf.Bytes(), &req)
		result := map[string]any{}
		switch req.Action {
		case "schematic.components.list":
			result = map[string]any{"components": []any{
				map[string]any{"componentType": "sheet", "primitiveId": "sheet1",
					"bbox": map[string]any{"minX": 0.0, "minY": 0.0, "maxX": 1170.0, "maxY": 825.0}},
				map[string]any{"componentType": "part", "designator": "U1", "primitiveId": "u1",
					"bbox": map[string]any{"minX": 700.0, "minY": 600.0, "maxX": 800.0, "maxY": 700.0}},
			}}
		case "schematic.text.list":
			result = map[string]any{"texts": []any{
				map[string]any{"primitiveId": "t1", "x": 100.0, "y": 100.0, "content": "已有说明", "fontSize": 10.0},
			}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, _ := strings.Cut(hostPort, ":")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return &appConfig{host: host, ports: fmt.Sprintf("%d-%d", port, port)}, srv.Close
}

// ── 负对照 C:比说明带还高的说明 —— 报文要说得出原因并给下一步 ─────────────
func TestNoteFlush_TooTallReportsWhyAndWhatNext(t *testing.T) {
	// 68 宽 / 42 高的窄带,配一条 30 全角 × 3 行的说明:哪一维都不够。
	frame := layoutBBox{116, 434, 184, 614}
	band := layoutBBox{116, 434, 184, 476}
	msg := noteOutsideZoneMessage("POWER_IN", zoneMoveText{ID: "t2", X: 50, Y: 400,
		Content:  strings.Repeat("宽", 30) + "\n" + strings.Repeat("宽", 30) + "\n" + strings.Repeat("宽", 30),
		FontSize: 10}, frame, band)
	for _, want := range []string{"装不下", "说明带只有", "缩短文字", "--font-size", "group-move", "别再原样重跑"} {
		if !strings.Contains(msg, want) {
			t.Errorf("装不下的报文缺 %q:%s", want, msg)
		}
	}
	if strings.Contains(msg, "--x ") {
		t.Errorf("装不下时不许开一张自己都装不下的方子(不该给 --x/--y):%s", msg)
	}
}

// ── 一把尺:带的定义 / note-outside-zone 的处方 / 落点求解三者同源 ──────────
//
// 真机取证(2026-08-20):某个框 (36,12)..(204,380) 的说明带是 (36,12)..(204,70),
// 而 check 给出的修法坐标却是 `--x 50 --y 80` —— 80 > 带顶 70,处方自己就把说明
// 放到了带外。处方与落点求解各写了一遍换算式,就是两把尺。
var noteFixHintRe = regexp.MustCompile(`--x ([-+0-9.eE]+) --y ([-+0-9.eE]+)`)

func TestRuler_NoteBandDefinitionSolverAndCheckAgree(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	opts := defaultPartitionOpts()
	content := noteContentOf(3)
	w, h := noteSizeOf(content, 10)
	mod := partitionModule{Name: "MCU", BBox: layoutBBox{260, 400, 560, 700}, NoteWidth: w, NoteHeight: h}
	plan := planPartitionsWithNotes(sheet, nil, []partitionModule{mod}, opts, []layoutBBox{mod.BBox})
	p := plan.Partitions[0]

	// ① 带的定义只有 zoneNoteBand 一个函数:planner 填进 NoteBBox 的那条带,
	//    必须逐字段等于用它自己的框 + 带顶重算出来的带。
	if got := zoneNoteBand(p.BBox, p.NoteBBox.MaxY); got != p.NoteBBox {
		t.Fatalf("planner 的说明带与 zoneNoteBand 分家了:%+v vs %+v", p.NoteBBox, got)
	}

	// ② 落点求解给出的锚点。
	sx, sy, ok := scanNoteBand(p.NoteBBox, p.BBox, w, h, []layoutBBox{mod.BBox}, sheet, nil)
	if !ok {
		t.Fatalf("带里应当有贴底落点:band=%+v", p.NoteBBox)
	}

	// ③ check 的处方必须逐字等于②,而且处方落点必须在**带内**。
	msg := noteOutsideZoneMessage("MCU", zoneMoveText{ID: "t1", X: 900, Y: 800, Content: content, FontSize: 10},
		p.BBox, p.NoteBBox)
	m := noteFixHintRe.FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("带装得下时必须给出算好的落点坐标:%s", msg)
	}
	hx, _ := strconv.ParseFloat(m[1], 64)
	hy, _ := strconv.ParseFloat(m[2], 64)
	if hx != sx || hy != sy {
		t.Fatalf("处方 (%g,%g) ≠ 求解器 (%g,%g) —— 两把尺", hx, hy, sx, sy)
	}
	if box := noteAnchorBBox(hx, hy, w, h); !bboxContains(p.NoteBBox, box) {
		t.Fatalf("处方把说明放到了带外:box=%+v band=%+v", box, p.NoteBBox)
	}
	if hy-p.BBox.MinY != noteBottomInset {
		t.Fatalf("处方没有贴底:离框底 %.4f,want %.0f", hy-p.BBox.MinY, noteBottomInset)
	}
}

// 锚点语义的换算与它的逆必须闭环:改了 noteAnchorBBox 就必须改
// noteAnchorYForBottom,否则"贴底"会算到别处去。
func TestRuler_NoteAnchorBottomRoundTrip(t *testing.T) {
	for _, h := range []float64{13, 26, 39, 52, 27} {
		for _, bottom := range []float64{0, 28, 372.5, 612} {
			y := noteAnchorYForBottom(bottom, h)
			if got := noteAnchorBBox(100, y, 40, h); math.Abs(got.MinY-bottom) > 1e-9 {
				t.Fatalf("h=%v bottom=%v:换算不闭环,box=%+v", h, bottom, got)
			}
		}
	}
	// 实测锚定(getPrimitivesBBox,2026-08-20):锚点 y 就是 bbox 的 minY。
	for _, tc := range []struct{ y, h, wantMinY, wantMaxY float64 }{
		{290, 20, 290, 310}, {510, 30, 510, 540}, {635, 40, 635, 675},
		{80, 20, 80, 100}, {425, 27, 425, 452},
	} {
		b := noteAnchorBBox(0, tc.y, 10, tc.h)
		if b.MinY != tc.wantMinY || b.MaxY != tc.wantMaxY {
			t.Fatalf("锚点语义与真机实测不符:y=%v h=%v → %+v", tc.y, tc.h, b)
		}
	}
}

// 带高与底边内缩是配对的:带高 = 内缩 + 块高。一分家,带要么装不下贴底的
// 说明、要么在说明上方白留一条。
func TestRuler_NoteBandExactlyFitsFlushNote(t *testing.T) {
	if noteBottomInset != noteGap {
		t.Fatalf("noteBottomInset=%v ≠ noteGap=%v —— 说明带高 requiredNoteBand 是按 noteGap 算的",
			noteBottomInset, noteGap)
	}
	for _, h := range []float64{13, 26, 39, 52} {
		band := layoutBBox{MinX: 0, MinY: 100, MaxX: 300, MaxY: 100 + requiredNoteBand(h)}
		y := noteFlushAnchorY(band, h)
		box := noteAnchorBBox(10, y, 40, h)
		if !bboxContains(band, box) {
			t.Fatalf("h=%v:按 requiredNoteBand 预留的带装不下贴底的说明 box=%+v band=%+v", h, box, band)
		}
		if math.Abs(box.MaxY-band.MaxY) > 1e-9 {
			t.Fatalf("h=%v:贴底之后带顶应当正好被顶满,box=%+v band=%+v", h, box, band)
		}
	}
}
