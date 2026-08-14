package app

import (
	"math"
	"testing"
)

// ── 第二层:组间关系可以算,不需要新声明(ADR-0003 §4)────────────────────────

// **电源/地必须排除在耦合之外**。它们连着页面上几乎每一个器件,计入耦合会让
// 任意两组都显得"强相关",排布退化成一团 —— 真正决定谁挨着谁的是信号。
func TestGroupCouplings_IgnoresPowerAndGround(t *testing.T) {
	groupOf := map[string]string{"U3": "g1", "J1": "g1", "Q1": "g2", "R9": "g2"}
	live := map[string]map[string]bool{
		"GND":      {"U3.1": true, "J1.8": true, "Q1.2": true, "R9.2": true}, // 连所有人
		"5V":       {"U3.5": true, "Q1.1": true},                             // 同上
		"UART_TXD": {"U3.2": true, "Q1.3": true},                             // 真正的跨组信号
		"UART_RTS": {"U3.14": true, "R9.1": true},                            // 同上
		"C7_N4":    {"U3.7": true, "J1.3": true},                             // 组内网,不算耦合
	}
	coup := groupCouplings(live, groupOf)
	if got := couplingOf(coup, "g1", "g2"); got != 2 {
		t.Errorf("只该数两条跨组信号(TXD/RTS), got %d —— 电源地或组内网被误算了", got)
	}
	if len(coup) != 1 {
		t.Errorf("只有一对组,不该产生别的耦合项: %+v", coup)
	}
}

// 耦合读取与顺序无关。
func TestCouplingOf_IsSymmetric(t *testing.T) {
	coup := map[string]int{"a|b": 3}
	if couplingOf(coup, "a", "b") != 3 || couplingOf(coup, "b", "a") != 3 {
		t.Error("耦合是无向的,两个方向必须读到同一个值")
	}
}

// 链式排序:耦合最强的相邻,且**确定性**(同输入同输出)。
// 真实形态:USB连接器 → 桥芯片 → 下载电路,链的两端是耦合度最低的。
func TestOrderGroupsByFlow_StrongestCouplingAdjacent(t *testing.T) {
	items := []bslGroupItem{{ID: "usb"}, {ID: "bridge"}, {ID: "download"}}
	coup := map[string]int{
		"bridge|usb":      4, // usb ↔ bridge 强
		"bridge|download": 2, // bridge ↔ download 中
		// usb ↔ download 无
	}
	for i := 0; i < 10; i++ { // 多跑几次:map 遍历顺序随机,结果必须稳定
		got := orderGroupsByFlow(items, coup)
		ids := []string{got[0].ID, got[1].ID, got[2].ID}
		// bridge 必须在中间 —— 它是唯一与两端都有耦合的
		if ids[1] != "bridge" {
			t.Fatalf("耦合最强的应相邻,bridge 该在中间: %v", ids)
		}
	}
}

// 单个组直接返回,不该崩。
func TestOrderGroupsByFlow_SingleGroup(t *testing.T) {
	items := []bslGroupItem{{ID: "only"}}
	if got := orderGroupsByFlow(items, nil); len(got) != 1 || got[0].ID != "only" {
		t.Errorf("单组应原样返回: %+v", got)
	}
}

// 铺排:行内从左到右,放不下换行,**绝不越界**(ADR §6:每层自己保证)。
func TestArrangeGroups_WrapsAndStaysInBounds(t *testing.T) {
	bounds := layoutBBox{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 800}
	items := []bslGroupItem{
		{ID: "a", Name: "A", BBox: layoutBBox{MinX: 100, MinY: 100, MaxX: 500, MaxY: 300}}, // 400×200
		{ID: "b", Name: "B", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 200}},     // 400×200
		{ID: "c", Name: "C", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 200}},     // 400×200
	}
	got, err := arrangeGroups(items, bounds, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("三个组都该有落位: %+v", got)
	}
	// 前两个同一行(400+40+400=840 ≤ 1000),第三个换行
	if got[0].Row != 0 || got[1].Row != 0 {
		t.Errorf("前两个该在同一行: %+v", got)
	}
	if got[2].Row != 1 {
		t.Errorf("第三个该换行: %+v", got)
	}
	// 逐个验证平移后仍在 bounds 内
	for i, p := range got {
		b := items[i].BBox
		moved := layoutBBox{MinX: b.MinX + p.DX, MinY: b.MinY + p.DY, MaxX: b.MaxX + p.DX, MaxY: b.MaxY + p.DY}
		if !boxInside(moved, bounds) {
			t.Errorf("组 %s 排到了界外: %+v (bounds %+v)", p.ID, moved, bounds)
		}
	}
}

// 比可用区还大的组:**明确说放不下**,而不是硬塞或溢出。
func TestArrangeGroups_RefusesOversizeInsteadOfOverflowing(t *testing.T) {
	bounds := layoutBBox{MinX: 0, MinY: 0, MaxX: 300, MaxY: 300}
	items := []bslGroupItem{{ID: "big", Name: "BIG", BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 900, MaxY: 200}}}
	if _, err := arrangeGroups(items, bounds, 40); err == nil {
		t.Error("放不下必须报错,不能硬塞到界外")
	}
}

// 装不下的总量:同样明确失败,并说清是第几行不够。
func TestArrangeGroups_RefusesWhenPageIsFull(t *testing.T) {
	bounds := layoutBBox{MinX: 0, MinY: 0, MaxX: 500, MaxY: 500}
	var items []bslGroupItem
	for i := 0; i < 6; i++ { // 6 × (450×200) 一页装不下
		items = append(items, bslGroupItem{
			ID: string(rune('a' + i)), Name: "X",
			BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 450, MaxY: 200},
		})
	}
	if _, err := arrangeGroups(items, bounds, 40); err == nil {
		t.Error("一页装不下这么多组时必须报错")
	}
}

// 位移**必须落在 5 格上**:器件原本在网格上,带小数的平移会把引脚推到格外,
// connect_pin 直接拒绝(「Pin (612.5, 706.5) sits OFF the 5-unit schematic grid」),
// 重连全线失败 —— 真机踩过。判定坐标 = 落地坐标,这一层也不例外。
func TestArrangeGroups_DeltasSnapToGrid(t *testing.T) {
	bounds := layoutBBox{MinX: 12, MinY: 198, MaxX: 1158, MaxY: 813}
	items := []bslGroupItem{
		{ID: "g1", Name: "A", BBox: layoutBBox{MinX: 64, MinY: 266, MaxX: 700, MaxY: 528}},
		{ID: "g2", Name: "B", BBox: layoutBBox{MinX: 34, MinY: 289, MaxX: 395, MaxY: 400}},
	}
	got, err := arrangeGroups(items, bounds, 60)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if math.Mod(p.DX, float64(schAnchorGrid)) != 0 || math.Mod(p.DY, float64(schAnchorGrid)) != 0 {
			t.Errorf("组 %s 的位移不在 %d 格上: Δ=(%v,%v)", p.ID, schAnchorGrid, p.DX, p.DY)
		}
	}
	// 吸附之后仍不许越界
	for i, p := range got {
		b := items[i].BBox
		moved := layoutBBox{MinX: b.MinX + p.DX, MinY: b.MinY + p.DY, MaxX: b.MaxX + p.DX, MaxY: b.MaxY + p.DY}
		if !boxInside(moved, bounds) {
			t.Errorf("吸附把组 %s 推出了边界: %+v", p.ID, moved)
		}
	}
}

// ── P2:区框与说明必须**在排布时占地**,不是事后捡缝 ─────────────────────────

// 完整占地必须比器件包络大出「区框 + 标签带 + 说明带」——这正是"同级参与求解"的
// 实质:第二层排的是这个盒子,于是框和说明的地方在求解时就留出来了。
func TestGroupAnnotatedExtent_ReservesRoomForFrameAndNote(t *testing.T) {
	dev := layoutBBox{MinX: 100, MinY: 200, MaxX: 500, MaxY: 400}
	bare := groupAnnotatedExtent(dev, 0)
	if bare.MinX >= dev.MinX || bare.MaxX <= dev.MaxX || bare.MinY >= dev.MinY {
		t.Errorf("区框必须四周留白: %+v vs 器件 %+v", bare, dev)
	}
	if bare.MaxY <= dev.MaxY+groupFramePad {
		t.Errorf("顶上要额外留组名标签带: %+v", bare)
	}
	withNote := groupAnnotatedExtent(dev, 2)
	if withNote.MinY >= bare.MinY {
		t.Errorf("有说明时下方必须再让出说明带: 无说明 %v, 两行说明 %v", bare.MinY, withNote.MinY)
	}
	if got, want := bare.MinY-withNote.MinY, 2*groupNoteLine; got != want {
		t.Errorf("说明带高度应随行数增长: got %v want %v", got, want)
	}
}

// 反推的区框只包器件,不含标签带和说明带 —— 否则框会把说明圈进去。
func TestGroupFrameOf_ExcludesLabelAndNoteBands(t *testing.T) {
	dev := layoutBBox{MinX: 100, MinY: 200, MaxX: 500, MaxY: 400}
	full := groupAnnotatedExtent(dev, 2)
	frame := groupFrameOf(full, 2)
	if frame.MaxY != dev.MaxY+groupFramePad {
		t.Errorf("框上沿应贴器件留白处,不含标签带: got %v want %v", frame.MaxY, dev.MaxY+groupFramePad)
	}
	if frame.MinY != dev.MinY-groupFramePad {
		t.Errorf("框下沿不该含说明带: got %v want %v", frame.MinY, dev.MinY-groupFramePad)
	}
	// 框必须真的把器件包住
	if !boxInside(dev, frame) {
		t.Errorf("框没包住器件: 框 %+v 器件 %+v", frame, dev)
	}
}

// 排布用完整占地时,相邻两组之间的间距必须**同时容纳两边的框和说明**,
// 不能出现"框画出去压到隔壁"。
func TestArrangeGroups_FramesDoNotOverlapNeighbours(t *testing.T) {
	bounds := layoutBBox{MinX: 0, MinY: 0, MaxX: 2000, MaxY: 900}
	devA := layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 200}
	devB := layoutBBox{MinX: 0, MinY: 0, MaxX: 300, MaxY: 150}
	items := []bslGroupItem{
		{ID: "a", Name: "A", DeviceBox: devA, BBox: groupAnnotatedExtent(devA, 2), NoteLines: []string{"x", "y"}},
		{ID: "b", Name: "B", DeviceBox: devB, BBox: groupAnnotatedExtent(devB, 1), NoteLines: []string{"z"}},
	}
	got, err := arrangeGroups(items, bounds, 40)
	if err != nil {
		t.Fatal(err)
	}
	// 平移后各自的**完整占地**(含框与说明)不许相交
	boxes := make([]layoutBBox, len(got))
	for i, p := range got {
		b := items[i].BBox
		boxes[i] = layoutBBox{MinX: b.MinX + p.DX, MinY: b.MinY + p.DY, MaxX: b.MaxX + p.DX, MaxY: b.MaxY + p.DY}
	}
	if _, _, overlap := overlapExtent(boxes[0], boxes[1]); overlap {
		t.Errorf("两组的框/说明区重叠了: %+v vs %+v", boxes[0], boxes[1])
	}
}
