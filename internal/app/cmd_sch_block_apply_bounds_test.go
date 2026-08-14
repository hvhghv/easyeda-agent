package app

import (
	"strings"
	"testing"
)

// ── issue #180 P0:origin 三修 + 出图纸判据 ──────────────────────────────────
//
// 2026-08-13 实测:block-apply 把 J_USB 放到 x=-20、把 R6 放到 y=880(图纸 0..825),
// 而 layout-lint 全绿。当时把原因归给「螺旋搜索不把负偏移计进 footprint」——
// **归因是错的**,bapBlockRect 一直用 min/max 覆盖负值。真机制是 bapResolveOrigin
// 给 findSlot 的 inBounds/hitsTitle 传了 nil,边界谓词从未参与搜索。
// 这些用例把订正后的行为钉死。

// 负偏移一直被正确计入 footprint —— 防止后人照旧 issue 正文再去"修" min/max。
func TestBapBlockRect_CountsNegativeOffsets(t *testing.T) {
	offsets := map[string]bapRoleOffset{
		"U":      {dx: 0, dy: 0},
		"J_USB":  {dx: -420, dy: 0},
		"C_HIGH": {dx: 0, dy: 230},
	}
	half := func(string) float64 { return 0 } // 只看偏移本身,不掺半宽
	r := bapBlockRect(1000, 500, offsets, half)
	if r.MinX > 1000-420 {
		t.Errorf("负 dx 必须计入 footprint 左缘: MinX=%v, 期望 ≤ %v", r.MinX, 1000-420)
	}
	if r.MaxY < 500+230 {
		t.Errorf("正 dy 必须计入上缘: MaxY=%v, 期望 ≥ %v", r.MaxY, 500+230)
	}
}

// Fix A:footprint 必须用**真实半宽**,不能一刀切 bapPartMargin —— 含模组/MCU 的
// 块每边曾低估 200,导致"螺旋说没撞、放完实测撞、硬门整单回滚"。
func TestBapBlockRect_UsesRealHalfExtent(t *testing.T) {
	offsets := map[string]bapRoleOffset{"U": {dx: 0, dy: 0}}
	narrow := bapBlockRect(0, 0, offsets, func(string) float64 { return 0 })
	wide := bapBlockRect(0, 0, offsets, func(string) float64 { return 250 }) // 模组半宽
	nw, _ := bboxSize(narrow)
	ww, _ := bboxSize(wide)
	// 半宽 250 的模组:footprint 宽度必须真的反映 2×250,而不是一刀切的
	// 2×bapPartMargin(=100)——正是这个低估让含 WROOM 的块每边少算 200。
	if ww != 2*250 {
		t.Errorf("footprint 宽度应 = 2×半宽 = 500, got %v (narrow=%v)", ww, nw)
	}
	// half 小于 bapPartMargin 时不得缩水(margin 是下限)。
	tiny := bapBlockRect(0, 0, offsets, func(string) float64 { return 1 })
	tw, _ := bboxSize(tiny)
	if tw < 2*bapPartMargin {
		t.Errorf("半宽小于 bapPartMargin 时应按 margin 兜底: got %v, want ≥ %v", tw, 2*bapPartMargin)
	}
}

// Fix B:图纸边界必须**参与搜索**。种子原点贴着图纸右缘时,搜出来的原点不许让
// 块探出可用区。
func TestBapResolveOrigin_KeepsBlockInsideSheet(t *testing.T) {
	sheet := &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	offsets := map[string]bapRoleOffset{
		"A": {dx: 0, dy: 0},
		"B": {dx: 300, dy: 0}, // 块宽 300 + 两侧 margin
	}
	half := func(string) float64 { return 0 }
	in := bapInput{
		OriginX: 1100, OriginY: 400, // 贴右缘:B 会落到 1400,远超 1170
		Sheet: sheet,
		// 放一个障碍,逼它进入搜索分支(无障碍且在界内会直接早退)
		Obstacles: []layoutBBox{{MinX: 1090, MinY: 390, MaxX: 1110, MaxY: 410}},
	}
	x, y, origin, _ := bapResolveOrigin(in, offsets, half)
	rect := bapBlockRect(x, y, offsets, half)
	usable := layoutBBox{
		MinX: sheet.MinX + sheetEdgeMinGap, MinY: sheet.MinY + sheetEdgeMinGap,
		MaxX: sheet.MaxX - sheetEdgeMinGap, MaxY: sheet.MaxY - sheetEdgeMinGap,
	}
	if !boxInside(rect, usable) {
		t.Fatalf("搜出来的原点让块出了图纸可用区: rect=%+v usable=%+v (relocated=%v)",
			rect, usable, origin.Relocated)
	}
}

// 没有图纸信息时不许假装检查过 —— 行为回退到"只避障碍",且不因边界而拒绝。
func TestBapResolveOrigin_NoSheetIsUnconstrained(t *testing.T) {
	offsets := map[string]bapRoleOffset{"A": {dx: 0, dy: 0}}
	in := bapInput{OriginX: -5000, OriginY: -5000} // 远在图纸外
	x, y, _, warns := bapResolveOrigin(in, offsets, func(string) float64 { return 0 })
	if x != -5000 || y != -5000 {
		t.Errorf("无障碍无图纸时应原样返回: got (%v,%v)", x, y)
	}
	if len(warns) != 0 {
		t.Errorf("不该凭空报边界警告: %v", warns)
	}
}

// Fix C:出图纸的件必须被 lint 抓到 —— 判据是 **bbox** 不是锚点(锚点在框内、
// body 探出框外一样印不出来;block-apply 事后那条 warning 比锚点,所以漏报)。
func TestDetectOutOfSheet_JudgesBBoxNotAnchor(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	comps := []layoutComp{
		{ID: "in", Designator: "R1", ComponentType: "part", X: 500, Y: 400, BBox: bb(480, 390, 520, 410)},
		// 锚点在框内(1160 < 1170),但 body 探出右缘 —— 必须被抓到
		{ID: "edge", Designator: "J1", ComponentType: "part", X: 1160, Y: 400, BBox: bb(1130, 380, 1200, 420)},
		// 实测那两个:x 负、y 超上界
		{ID: "left", Designator: "J_USB", ComponentType: "part", X: -20, Y: 400, BBox: bb(-55, 365, 15, 435)},
		{ID: "top", Designator: "R6", ComponentType: "part", X: 510, Y: 880, BBox: bb(500, 870, 520, 890)},
		{ID: "nobbox", Designator: "C9", ComponentType: "part", X: 9999, Y: 9999}, // 无 bbox → 跳过
	}
	got := detectOutOfSheet(comps, sheet, sheetEdgeMinGap)
	names := map[string]layoutFinding{}
	for _, f := range got {
		names[f.A] = f
		if f.Type != "out-of-sheet" {
			t.Errorf("type = %q", f.Type)
		}
	}
	for _, want := range []string{"J1", "J_USB", "R6"} {
		if _, ok := names[want]; !ok {
			t.Errorf("%s 出图纸却没被抓到; got=%+v", want, got)
		}
	}
	if _, ok := names["R1"]; ok {
		t.Error("框内的件不该被报")
	}
	if _, ok := names["C9"]; ok {
		t.Error("没有 bbox 的件应跳过(那是 NoBBox 的职责)")
	}
	// 超出量必须有值,且指向正确的轴。
	if f := names["R6"]; f.OvY <= 0 {
		t.Errorf("R6 是上界超出,OvY 应 > 0: %+v", f)
	}
	if f := names["J_USB"]; f.OvX <= 0 {
		t.Errorf("J_USB 是左界超出,OvX 应 > 0: %+v", f)
	}
}

// 档位:默认 advisory(与 zone-violation 同档),--strict 才判失败。否则一夜之间
// 所有既有板子的 lint 变红。
func TestOutOfSheetIsAdvisoryUntilStrict(t *testing.T) {
	base := layoutReport{OK: true, OutOfSheet: []layoutFinding{{Type: "out-of-sheet", A: "R6"}}}

	lenient := base
	applyLayoutStrictGate(&lenient, false)
	if !lenient.OK {
		t.Error("默认档不该因 out-of-sheet 失败")
	}

	strict := base
	applyLayoutStrictGate(&strict, true)
	if strict.OK {
		t.Error("--strict 必须因 out-of-sheet 失败")
	}
}

// 每一个能让 OK=false 的判据,都必须出现在**人读**通道里 —— 否则会出现
// 「所有计数都是 0 却非零退出」的不可归因失败(记忆:真机验的是报告读起来对不对;
// audit 里实测过 agent 对同一个失败拼出四种不同下一步)。
func TestOutOfSheetIsVisibleInEveryChannel(t *testing.T) {
	rep := layoutReport{
		OK: true, Total: 1, WithBBox: 1,
		SheetCheckStatus: "checked",
		ZoneCheckStatus:  "checked",
		OutOfSheet: []layoutFinding{
			{Type: "out-of-sheet", A: "R6", X: 510, Y: 880, OvY: 67},
		},
	}
	applyLayoutStrictGate(&rep, true)
	if rep.OK {
		t.Fatal("--strict 下 out-of-sheet 必须让报告失败")
	}
	var sb strings.Builder
	renderLayoutReport(rep, &sb)
	out := sb.String()
	if !strings.Contains(out, "out-of-sheet") || !strings.Contains(out, "R6") {
		t.Errorf("文本通道必须逐条打印出图件,实际:\n%s", out)
	}
	if !strings.Contains(out, "sheet-check=") {
		t.Errorf("结论行必须带 sheet-check 状态,实际:\n%s", out)
	}
	if !strings.Contains(out, "1 out-of-sheet") {
		t.Errorf("结论行的计数必须包含 out-of-sheet,实际:\n%s", out)
	}
}

// 「没检查」不许显得像「查了干净」:sheet-check unavailable 在 --strict 下本身即
// 阻塞(与 zone-check 同态),且必须带得出原因。
func TestSheetCheckUnavailableBlocksUnderStrict(t *testing.T) {
	rep := layoutReport{OK: true, SheetCheckStatus: "unavailable", SheetCheckError: "本页读不到图纸边框"}
	applyLayoutStrictGate(&rep, true)
	if rep.OK {
		t.Error("--strict 下 sheet-check unavailable 必须阻塞(zone-check 同态)")
	}
	lenient := layoutReport{OK: true, SheetCheckStatus: "unavailable"}
	applyLayoutStrictGate(&lenient, false)
	if !lenient.OK {
		t.Error("默认档不该因 unavailable 失败")
	}
}

// out-of-sheet 复用 OvX/OvY 表示超出量,必须与 overlap 走同一套 mm 换算 ——
// 否则同一份 JSON 里同名键两种单位,而报告自报 measurementUnit:"mm"。
func TestOutOfSheetOverExtentConvertsToMM(t *testing.T) {
	raw := layoutReport{
		Overlaps:   []layoutFinding{{Type: "overlap", A: "a", B: "b", OvX: 10, OvY: 10}},
		OutOfSheet: []layoutFinding{{Type: "out-of-sheet", A: "R6", OvX: 10, OvY: 10}},
	}
	mm := layoutReportInMM(raw)
	if mm.OutOfSheet[0].OvX != mm.Overlaps[0].OvX {
		t.Errorf("同名键必须同单位: outOfSheet.OvX=%v overlap.OvX=%v",
			mm.OutOfSheet[0].OvX, mm.Overlaps[0].OvX)
	}
	if mm.OutOfSheet[0].OvX == 10 {
		t.Error("超出量没有被换算成 mm")
	}
}

// **显式 --at 不能凌驾于「不出界」之上**。坐标是偏好,出界是硬约束:落在图纸外
// 的器件在导出/打印/评审里根本不存在,那是废图,而重叠还能人工挪。
// 旧行为是「按你的坐标照常放置(显式 --at 优先)」+ 一条 warning —— 于是每次给一个
// 越界的 --at,块就真的被放到纸外面去。
func TestBapResolveOrigin_ExplicitAtStillCannotLeaveTheSheet(t *testing.T) {
	sheet := &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	offsets := map[string]bapRoleOffset{
		"A": {dx: 0, dy: 0},
		"B": {dx: 300, dy: 0},
	}
	half := func(string) float64 { return 0 }
	in := bapInput{
		OriginX: 1100, OriginY: 400, // B 会落到 1400,远超 1170
		Sheet:      sheet,
		AtExplicit: true, // ← 关键:显式指定
	}
	x, y, origin, warns := bapResolveOrigin(in, offsets, half)
	rect := bapBlockRect(x, y, offsets, half)
	usable := layoutBBox{
		MinX: sheet.MinX + sheetEdgeMinGap, MinY: sheet.MinY + sheetEdgeMinGap,
		MaxX: sheet.MaxX - sheetEdgeMinGap, MaxY: sheet.MaxY - sheetEdgeMinGap,
	}
	if !boxInside(rect, usable) {
		t.Fatalf("显式 --at 也不许把块放到图纸外: rect=%+v usable=%+v", rect, usable)
	}
	if !origin.Relocated {
		t.Error("动了用户给的坐标就必须标 Relocated,否则调用方无从得知")
	}
	if len(warns) == 0 {
		t.Error("必须告诉用户坐标被改了以及为什么")
	}
}

// 只是**重叠**(没出界)时,仍然尊重「显式 --at 优先」—— 两种失败不同等对待。
func TestBapResolveOrigin_ExplicitAtStillWinsOverMereOverlap(t *testing.T) {
	sheet := &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	offsets := map[string]bapRoleOffset{"A": {dx: 0, dy: 0}}
	half := func(string) float64 { return 0 }
	in := bapInput{
		OriginX: 500, OriginY: 400, // 图纸正中,不会出界
		Sheet:      sheet,
		AtExplicit: true,
		Obstacles:  []layoutBBox{{MinX: 450, MinY: 350, MaxX: 550, MaxY: 450}}, // 压在原点上
	}
	x, y, origin, warns := bapResolveOrigin(in, offsets, half)
	if x != 500 || y != 400 {
		t.Errorf("仅重叠时应尊重显式坐标: got (%v,%v)", x, y)
	}
	if origin.Relocated {
		t.Error("没动坐标就不该标 Relocated")
	}
	if len(warns) == 0 {
		t.Error("重叠仍要警告")
	}
}
