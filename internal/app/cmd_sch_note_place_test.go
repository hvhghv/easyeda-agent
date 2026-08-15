package app

import "testing"

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
