package app

import (
	"strings"
	"testing"
)

// ── sch note 自动落点(2026-08-13 用户纠偏)────────────────────────────────
//
// 「每个编组对象还有 title、注释,属于同级别的;计算摆放位置的时候可以计算现有
// 虚拟组的 xy 和长宽碰撞,自动算出对齐和层叠方式」——这些用例把"说明必须和器件
// 同级参与碰撞"钉死:此前 --x/--y 必填,三条说明齐刷刷压在器件和网标上。

func TestNoteSizeOf_CJKAndASCIIWidth(t *testing.T) {
	// CJK 全宽、ASCII 半宽:同样字数的中文条目必须更宽。
	wCN, hCN := noteSizeOf("电源说明", 10)
	wEN, hEN := noteSizeOf("abcd", 10)
	if !(wCN > wEN) {
		t.Errorf("CJK 应比 ASCII 宽: cn=%v en=%v", wCN, wEN)
	}
	if hCN != hEN {
		t.Errorf("同行数应同高: %v vs %v", hCN, hEN)
	}
	// 行数决定高度。
	_, h2 := noteSizeOf("a\nb", 10)
	if !(h2 > hEN) {
		t.Errorf("两行应高于一行: %v vs %v", h2, hEN)
	}
}

// 尺寸口径必须与 zone-plan 折进画框用的 schNoteBBoxEstimate 完全一致 —— 两套
// 估算一旦分家,就会"求解时说不撞、画框时说撞"。
func TestNoteSizeSharedWithPartitionEstimate(t *testing.T) {
	const content = "SERIAL_IC — CH340C USB 转串口\nV3 脚必须挂 100nF 对地"
	w, h := noteSizeOf(content, 9)
	got := schNoteBBoxEstimate(zoneMoveText{X: 100, Y: 500, Content: content, FontSize: 9})
	want := noteAnchorBBox(100, 500, w, h)
	if got != want {
		t.Fatalf("两处估算漂移了:\n plan=%+v\n note=%+v", got, want)
	}
}

// 核心行为:器件占着的位置绝不落说明,且与任何图元至少留 noteGap。
func TestPlanNoteAnchor_AvoidsPartsAndTexts(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	// 一块占住页面中部的电路 + 一条已有说明。
	obstacles := []layoutBBox{
		{MinX: 300, MinY: 300, MaxX: 900, MaxY: 600}, // 器件群
		{MinX: 60, MinY: 200, MaxX: 400, MaxY: 240},  // 已有说明
	}
	w, h := noteSizeOf("测试说明一行", 10)
	x, y, ok := planNoteAnchor(w, h, obstacles, nil, nil, sheet, nil)
	if !ok {
		t.Fatal("整页有大片空白,不该求解失败")
	}
	box := noteAnchorBBox(x, y, w, h)
	for i, ob := range obstacles {
		if boxesGapOverlap(box, ob, noteGap) {
			t.Errorf("落点 %+v 与障碍 %d %+v 的间隙不足 %v", box, i, ob, noteGap)
		}
	}
	if box.MinX < sheet.MinX || box.MaxX > sheet.MaxX || box.MinY < sheet.MinY || box.MaxY > sheet.MaxY {
		t.Errorf("落点越出图纸: %+v", box)
	}
}

// 给了区就优先待在自己区里(读图习惯:说明贴着它描述的那块电路)。
func TestPlanNoteAnchor_PrefersInsideItsZone(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	zone := layoutBBox{MinX: 300, MinY: 300, MaxX: 900, MaxY: 700}
	// 区内上半被器件占住,下半留白 —— 说明应该落在区内下半。
	obstacles := []layoutBBox{{MinX: 320, MinY: 480, MaxX: 880, MaxY: 690}}
	w, h := noteSizeOf("区内说明\n第二行", 9)
	x, y, ok := planNoteAnchor(w, h, obstacles, &zone, nil, sheet, nil)
	if !ok {
		t.Fatal("区内下半有空位,不该失败")
	}
	box := noteAnchorBBox(x, y, w, h)
	if box.MinX < zone.MinX || box.MaxX > zone.MaxX || box.MinY < zone.MinY || box.MaxY > zone.MaxY {
		t.Errorf("说明应落在自己区内 %+v,实际 %+v", zone, box)
	}
	if boxesGapOverlap(box, obstacles[0], noteGap) {
		t.Errorf("落点压住了区内器件: %+v vs %+v", box, obstacles[0])
	}
}

// 图签 keep-out 是硬禁区(标题栏/明细表上不许压说明)。
func TestPlanNoteAnchor_RespectsTitleBlockKeepout(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	keepout := layoutBBox{MinX: 780, MinY: 0, MaxX: 1170, MaxY: 240}
	// 除了图签那块,其余全被占 —— 只剩图签区可放,必须求解失败而不是压上去。
	obstacles := []layoutBBox{
		{MinX: 0, MinY: 240, MaxX: 1170, MaxY: 825},
		{MinX: 0, MinY: 0, MaxX: 780, MaxY: 240},
	}
	w, h := noteSizeOf("不该落在图签上", 10)
	_, _, ok := planNoteAnchor(w, h, obstacles, nil, nil, sheet, &keepout)
	if ok {
		t.Error("唯一空位是图签 keep-out,应当拒绝落点而不是压上去")
	}
}

// 放不下就明确失败 —— 宁可不画,也不把说明糊在电路上。
func TestPlanNoteAnchor_FailsWhenNoRoom(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 300}
	obstacles := []layoutBBox{{MinX: 0, MinY: 0, MaxX: 400, MaxY: 300}}
	w, h := noteSizeOf("满页无处可放", 10)
	if _, _, ok := planNoteAnchor(w, h, obstacles, nil, nil, sheet, nil); ok {
		t.Error("整页被占满时必须求解失败")
	}
}

// 障碍表必须把 marker 的文字带算进去(与 sch check 的 marker-overlap 同口径),
// 否则说明会压在网标文字上——正是实测看到的症状之一。
func TestCollectNoteObstacles_IncludesMarkerTextBand(t *testing.T) {
	rot := flagBodyRotation["ground"]["down"]
	comps := []layoutComp{
		{ID: "sheet1", ComponentType: "sheet", BBox: bb(0, 0, 1170, 825)},
		{ID: "R1", ComponentType: "part", Designator: "R1", BBox: bb(100, 100, 140, 120)},
		{ID: "g1", ComponentType: "netflag", Net: "GND", X: 300, Y: 300,
			AnchorAvailable: true, Rotation: &rot, BBox: bb(295, 279, 305, 300)},
	}
	obs := collectNoteObstacles(comps, []zoneMoveText{{ID: "t1", X: 500, Y: 500, Content: "已有说明", FontSize: 10}})
	if len(obs) != 3 { // sheet 不算障碍;R1 + 旗 + 已有文字
		t.Fatalf("障碍数 = %d, want 3 (sheet 必须排除): %+v", len(obs), obs)
	}
	// 旗的障碍框必须比它的裸 bbox 宽 —— 文字带被算进去了。
	var flagBox layoutBBox
	for _, o := range obs {
		if o.MinX < 310 && o.MaxX > 290 && o.MaxY <= 300 {
			flagBox = o
		}
	}
	if flagBox.MaxX-flagBox.MinX <= 10 {
		t.Errorf("旗的障碍框应含文字带(宽 > 10 的裸 bbox),实际 %+v", flagBox)
	}
	// 文字带朝下(ground/down 真值表),障碍框应向下扩出裸 bbox 的 279。
	if flagBox.MinY >= 279 {
		t.Errorf("障碍框应含向下的文字带(MinY < 279),实际 %+v", flagBox)
	}
}

// ── 根因 A(2026-08-19 真机 E2E):--zone 传注册表全名静默落空 ───────────────
//
// 注册表里块子群叫 `ch340c_usb_serial(C4)/U`,分区计划的 Modules 里只有末段短名
// `U`;旧实现拿原始引用与短名做精确串匹配 → 传全名匹配不到 → 静默跌进整页兜底,
// 命令还照样报 "registered to zone" 成功。修法:统一解析器(resolveLayoutObject)
// 先解析,再用 zoneName() 投影出的短名对分区计划;未命中必须显式可见。
func TestNoteZoneRef_FullNameAndShortNameHitSamePartition(t *testing.T) {
	groups := []*schGroup{
		{ID: "g1", Name: "ch340c_usb_serial(C4)/U", Members: []string{"U2"}},
		{ID: "g2", Name: "ch340c_usb_serial(C4)/D_ESD", Members: []string{"D1"}},
	}
	table := buildLayoutObjectTable(nil, groups)
	parts := []partitionRect{
		{Modules: []string{"U"}, BBox: *bb(100, 500, 400, 790), NoteBBox: *bb(100, 500, 400, 526)},
		{Modules: []string{"D_ESD"}, BBox: *bb(500, 500, 900, 790), NoteBBox: *bb(500, 500, 900, 526)},
	}
	// 全名与短名必须命中**同一个**分区(报告 §三.A 的对照实验:全名落页角、短名
	// 落说明带 —— 修复后两者等价)。
	for _, ref := range []string{"ch340c_usb_serial(C4)/U", "U"} {
		obj, err := resolveLayoutObject(table, ref)
		if err != nil {
			t.Fatalf("统一解析器应命中 %q: %v", ref, err)
		}
		zr, nb, others, matched := matchNotePartition(parts, obj.zoneName())
		if !matched {
			t.Fatalf("%q(zoneName=%q)应命中分区计划", ref, obj.zoneName())
		}
		if *zr != parts[0].BBox || *nb != parts[0].NoteBBox {
			t.Errorf("%q 命中的分区不对: rect=%+v band=%+v", ref, *zr, *nb)
		}
		// 根因 B 的输入:命中时其余分区的矩形全部成为障碍。
		if len(others) != 1 || others[0] != parts[1].BBox {
			t.Errorf("%q 的邻区障碍表不对: %+v", ref, others)
		}
	}
	// 未命中(解析出的区不在本页分区计划里):matched=false 且**所有**分区都成
	// 障碍 —— 兜底整页落点绝不许落进任何分区框;调用方必须据此发 stderr 警告。
	if zr, _, others, matched := matchNotePartition(parts, "NOPE"); matched || zr != nil || len(others) != 2 {
		t.Errorf("未命中时应 matched=false 且全部分区入障碍表: matched=%v others=%+v", matched, others)
	}
}

// ── 根因 B(2026-08-19 真机 E2E):自动落点落进邻区框内 ─────────────────────
//
// 短名 J_USB 自动落点落到 (600,595) —— 那是邻区 D_ESD/U 的框内;区 bbox 被拉炸
// → partitionOverlap=1 → zone-draw 拒绝重画的死锁。修法:邻区分区矩形进障碍表。
func TestPlanNoteAnchor_AvoidsNeighborPartitionRect(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	zone := layoutBBox{MinX: 300, MinY: 300, MaxX: 900, MaxY: 700}
	// 盖满本区并向四周多探 40:框内候选与四周单点候选全被挡,逼进走廊回退链。
	occupied := layoutBBox{MinX: 260, MinY: 260, MaxX: 940, MaxY: 740}
	// 邻区分区框在本区正下方 —— 旧行为的走廊/整页候选会落进它的"空白"里。
	neighbor := layoutBBox{MinX: 200, MinY: 100, MaxX: 1000, MaxY: 290}
	w, h := noteSizeOf("邻区测试", 10)

	// 负对照(证明场景咬人):不把邻区当障碍,落点就落进邻区框内 —— 正是真机死锁。
	x0, y0, ok := planNoteAnchor(w, h, []layoutBBox{occupied}, &zone, nil, sheet, nil)
	if !ok {
		t.Fatal("负对照不该求解失败")
	}
	if !boxesOverlap(noteAnchorBBox(x0, y0, w, h), neighbor) {
		t.Fatalf("负对照失效:旧行为应落进邻区框内, got (%g,%g)", x0, y0)
	}

	// 修复后:邻区矩形进障碍表,落点绝不与邻区框相交。
	x1, y1, ok := planNoteAnchor(w, h, []layoutBBox{occupied, neighbor}, &zone, nil, sheet, nil)
	if !ok {
		t.Fatal("区上方走廊有空位,不该求解失败")
	}
	box := noteAnchorBBox(x1, y1, w, h)
	if boxesOverlap(box, neighbor) {
		t.Errorf("落点落进了邻区分区框: %+v vs %+v", box, neighbor)
	}
	if boxesGapOverlap(box, occupied, noteGap) {
		t.Errorf("落点与本区占用图元间隙不足: %+v", box)
	}
}

// ── 根因 C(2026-08-19 真机 E2E):说明带自增长反馈环 ───────────────────────
//
// 说明带由区内容 bbox 推出(框底 26 单位);落进带里的 note 曾被 fold 回内容
// bbox,框每重画一次向下长一截 ≈ pad+带高(实测 D_ESD 框 minY 554→501),带随框
// 下移,原来带内的说明又"不在带里"。修法:分区框推导**排除已登记的说明**
// (computePartitionPlan 不再 fold)—— 放 note 后重算,框与带逐字段不动。
func TestPartitionPlanStableAfterNotePlacedInBand(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	modules := []partitionModule{{Name: "D_ESD", BBox: *bb(600, 560, 700, 760), CoreBBox: *bb(600, 560, 700, 760)}}
	opts := defaultPartitionOpts()

	plan1 := planPartitions(sheet, nil, modules, opts)
	if len(plan1.Partitions) != 1 {
		t.Fatalf("want 1 partition, got %d", len(plan1.Partitions))
	}
	p := plan1.Partitions[0]
	// 说明按求解器的带内锚点落进说明带(锚点=左上角,y-UP 向下排行)。
	_, h := noteSizeOf("带内说明", 10)
	note := zoneMoveText{ID: "n1", X: p.NoteBBox.MinX + noteGap, Y: p.NoteBBox.MinY + h + noteGap, Content: "带内说明", FontSize: 10}
	noteBB := schNoteBBoxEstimate(note)
	if !bboxContains(p.BBox, noteBB) {
		t.Fatalf("带内说明应在框内: frame=%+v note=%+v", p.BBox, noteBB)
	}

	// 反馈环已断:已登记的 note 不参与内容 bbox,重算后框与说明带逐字段不变,
	// 带内说明仍然在带的位置上(不会被下移的带甩出去)。
	plan2 := planPartitions(sheet, nil, modules, opts)
	if plan2.Partitions[0].BBox != p.BBox || plan2.Partitions[0].NoteBBox != p.NoteBBox {
		t.Errorf("放 note 后重算分区框不该位移:\n before=%+v/%+v\n after =%+v/%+v",
			p.BBox, p.NoteBBox, plan2.Partitions[0].BBox, plan2.Partitions[0].NoteBBox)
	}

	// 负对照(记录旧病):把 note bbox fold 进模块再画框 —— 框必然向下再长一截,
	// 新带随框下移,原来带内的说明落到新带之外。这正是被移除的行为。
	grown := []partitionModule{{Name: "D_ESD",
		BBox:     *bb(minF(600, noteBB.MinX), minF(560, noteBB.MinY), maxF(700, noteBB.MaxX), maxF(760, noteBB.MaxY)),
		CoreBBox: *bb(600, 560, 700, 760)}}
	plan3 := planPartitions(sheet, nil, grown, opts)
	if !(plan3.Partitions[0].BBox.MinY < p.BBox.MinY-20) {
		t.Fatalf("负对照失效:旧 fold 行为应让框向下生长: before=%v after=%v", p.BBox.MinY, plan3.Partitions[0].BBox.MinY)
	}
	if noteBB.MinY <= plan3.Partitions[0].NoteBBox.MaxY {
		t.Fatalf("负对照失效:旧行为应把原带内说明甩出新说明带: note=%+v newBand=%+v", noteBB, plan3.Partitions[0].NoteBBox)
	}
}

// 已有换行的说明,逐行折行时宽度必须按行清零 —— 此前整段当一行累计宽度,
// 首行吃掉大半预算后,第二行开头 3~4 个字就被误折("丝印标正/负极性",
// 2026-08-18 P2 LED 说明真机定案)。
func TestWrapNoteContentRespectsExistingNewlines(t *testing.T) {
	// 首行 ~10 全角(90u @font9 口径按 wrapNoteLines 的 groupNoteFontSize 计),
	// 第二行 8 全角;maxWidth 给 160:两行各自都装得下,谁都不该被折。
	content := "IO2高=亮 R3=1k限流\n丝印标正负极性(PCB)"
	got := wrapNoteContent(content, 160)
	if got != content {
		t.Fatalf("both lines fit; wrap must be a no-op\nwant: %q\ngot:  %q", content, got)
	}
	// 真超宽的单行仍要折。
	long := strings.Repeat("宽", 40)
	if wrapped := wrapNoteContent(long, 160); !strings.Contains(wrapped, "\n") {
		t.Fatalf("a genuinely overwide line must still wrap, got %q", wrapped)
	}
}
