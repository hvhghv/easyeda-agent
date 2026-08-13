package app

import (
	"math"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
)

// ── 真块 origin 回归(issue #180 P0 对抗审查抓到的 blocker)──────────────────
//
// 教训:第一版回归测试用的是 300 宽的**玩具块**,step 小、搜索还能动,于是全绿 ——
// 而真块(ch340c 680×310 → step 380)在 A4 上 96 个候选一个都落不进可用区,
// 20/37 个块从「apply 成功」退化成「压在别人身上 → wiring 前硬门 → 整单回滚」。
// 所以这里一律用**内嵌块库的真实数据**跑,不再造玩具。

// bapRealBlockOffsets 用生产路径把一个真块算成 role→offset(与 planBlockApply 同源)。
func bapRealBlockOffsets(t *testing.T, id string) (map[string]bapRoleOffset, func(string) float64, blocks.Block) {
	t.Helper()
	b, ok, err := blocks.Get(id)
	if err != nil || !ok {
		t.Fatalf("内嵌块库里没有 %s (err=%v)", id, err)
	}
	roles := make([]string, 0, len(b.Parts))
	for r := range b.Parts {
		roles = append(roles, r)
	}
	spacing := bapGridSpacing(roles, b)
	halfOf := make(map[string]float64, len(roles))
	for _, r := range roles {
		halfOf[r] = bapRoleHalfExtent(b.Parts[r].Part)
	}
	half := bapHalfExtentFn(spacing, halfOf)
	layout, lerr := b.SchematicLayout()
	if lerr != nil {
		t.Fatalf("%s 的 schematic_layout 解析失败: %v", id, lerr)
	}
	return bapRoleOffsets(roles, layout, spacing, 4, halfOf), half, b
}

// A4 图纸 + 一个压在默认原点上的既有器件 —— block-apply 最常见的真实场景
// (同一页连着放第二个块)。
func a4SheetAndObstacle() (*layoutBBox, []layoutBBox) {
	sheet := &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	return sheet, []layoutBBox{{MinX: 380, MinY: 280, MaxX: 420, MaxY: 320}}
}

// 核心回归:默认原点被占时,真块必须被**搬到图纸内的合法空位**,而不是回落原坐标。
// 回落 = 压在既有器件上 = wiring 前硬门 = 整单回滚。
func TestBapResolveOrigin_RealBlocksRelocateWithinSheet(t *testing.T) {
	for _, id := range []string{
		"block.ch340c_usb_serial",     // 680×310,step 380 —— 螺旋必然落空的典型
		"block.esp32s3_wroom1_module", // 含 WROOM(半宽 250)
		"block.usbc_ufp_power_or",     // E2E 用例的核心块
		"block.xl1509_buck_12v_5v",    // 同上
		"block.esp32_autodownload",    // 带模板的小块
	} {
		t.Run(id, func(t *testing.T) {
			offsets, half, _ := bapRealBlockOffsets(t, id)
			sheet, obstacles := a4SheetAndObstacle()
			in := bapInput{OriginX: 400, OriginY: 300, Sheet: sheet, Obstacles: obstacles}

			x, y, origin, warns := bapResolveOrigin(in, offsets, half)
			rect := bapBlockRect(x, y, offsets, half)
			usable := layoutBBox{
				MinX: sheet.MinX + sheetEdgeMinGap, MinY: sheet.MinY + sheetEdgeMinGap,
				MaxX: sheet.MaxX - sheetEdgeMinGap, MaxY: sheet.MaxY - sheetEdgeMinGap,
			}
			bw, bh := bboxSize(rect)
			uw, uh := bboxSize(usable)
			if bw > uw || bh > uh {
				t.Skipf("本块估算 %.0f×%.0f 大于 A4 可用区 %.0f×%.0f,属 oversize 分支(另有用例)", bw, bh, uw, uh)
			}

			if !origin.Relocated {
				t.Fatalf("默认原点被占,必须搬位置;实际 relocated=false warns=%v", warns)
			}
			if !boxInside(rect, usable) {
				t.Errorf("搬完仍出图纸可用区: rect=%+v usable=%+v", rect, usable)
			}
			for _, o := range obstacles {
				if boxesGapOverlap(rect, o, bapObstacleGap) {
					t.Errorf("搬完仍与既有器件冲突: rect=%+v obstacle=%+v", rect, o)
				}
			}
		})
	}
}

// oversize(块比图纸还大)时**不许连坐掉避让**:边界约束降级,但器件避让必须照旧,
// 且警告要说清楚为什么。此前它恒 false 让 96 候选全否决 → 回落原坐标 → 压件 → 回滚,
// 比不加边界约束还差。
func TestBapResolveOrigin_OversizeStillAvoidsObstacles(t *testing.T) {
	// 造一个必然大于可用区的块(不依赖具体块库内容,避免块数据变动导致测试脆弱)。
	offsets := map[string]bapRoleOffset{
		"A": {dx: 0, dy: 0},
		"B": {dx: 0, dy: 2000}, // 高度远超 A4
	}
	half := func(string) float64 { return 50 }
	sheet := &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	obstacle := layoutBBox{MinX: 380, MinY: 280, MaxX: 420, MaxY: 320}
	in := bapInput{OriginX: 400, OriginY: 300, Sheet: sheet, Obstacles: []layoutBBox{obstacle}}

	x, y, _, warns := bapResolveOrigin(in, offsets, half)
	rect := bapBlockRect(x, y, offsets, half)
	if boxesGapOverlap(rect, obstacle, bapObstacleGap) {
		t.Errorf("oversize 时仍必须避开器件,实际压上了: rect=%+v obstacle=%+v warns=%v", rect, obstacle, warns)
	}
}

// 兜底扫描本身:给一个螺旋够不着但确实存在的空位,必须找到,且落点在 5 格网格上。
func TestBapScanOrigin_FindsSlotSpiralMisses(t *testing.T) {
	offsets := map[string]bapRoleOffset{"A": {dx: 0, dy: 0}, "B": {dx: 600, dy: 0}}
	half := func(string) float64 { return 50 }
	usable := layoutBBox{MinX: 12, MinY: 12, MaxX: 1158, MaxY: 813}
	// 把页面下半部占满,只在上方留出空间。
	block := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 500}
	collides := func(b layoutBBox) bool { return boxesGapOverlap(b, block, bapObstacleGap) }

	x, y, ok := bapScanOrigin(400, 300, offsets, half, usable, collides)
	if !ok {
		t.Fatal("上方有整片空白,扫描必须找得到")
	}
	r := bapBlockRect(x, y, offsets, half)
	if !boxInside(r, usable) || collides(r) {
		t.Errorf("扫描给出的落点不合法: rect=%+v", r)
	}
	if math.Mod(x, schAnchorGrid) != 0 || math.Mod(y, schAnchorGrid) != 0 {
		t.Errorf("落点必须在 %v 格网格上: (%v,%v)", schAnchorGrid, x, y)
	}
}

// 真的没空位时才允许失败 —— 兜底扫描不能把「装不下」也说成能放。
func TestBapScanOrigin_FailsWhenTrulyFull(t *testing.T) {
	offsets := map[string]bapRoleOffset{"A": {dx: 0, dy: 0}}
	half := func(string) float64 { return 50 }
	usable := layoutBBox{MinX: 12, MinY: 12, MaxX: 1158, MaxY: 813}
	collides := func(layoutBBox) bool { return true } // 处处是障碍
	if _, _, ok := bapScanOrigin(400, 300, offsets, half, usable, collides); ok {
		t.Error("处处有障碍时必须返回 false")
	}
}

// 半宽表说的是「rendered WIDTH」——不许拿它同时外扩纵向,否则一个 MCU 顶出 500 高,
// 使含模组的块在 A4 上「怎么摆都不可能 inBounds」,把边界约束变成死锁。
func TestBapBlockRect_HalfExtentIsWidthOnly(t *testing.T) {
	offsets := map[string]bapRoleOffset{"U": {dx: 0, dy: 0}}
	r := bapBlockRect(0, 0, offsets, func(string) float64 { return 250 })
	w, h := bboxSize(r)
	if w != 500 {
		t.Errorf("横向必须用真实半宽 250: w=%v", w)
	}
	if h != 2*bapPartMargin {
		t.Errorf("纵向没有实测半高表,应沿用 bapPartMargin: h=%v want %v", h, 2*bapPartMargin)
	}
}

// footprint 估算与网格间距是两件事:显式 --spacing 该左右**件间距**,不该缩小
// 「这块实际多大」的判断。此前共用一把尺,`--spacing 220` 让模组 footprint 每边
// 少算 140,Fix A 在这条路径上原样复发。
func TestBlockFootprintIgnoresExplicitSpacing(t *testing.T) {
	b, ok, err := blocks.Get("block.esp32s3_wroom1_module")
	if err != nil || !ok {
		t.Fatalf("块库缺 wroom1: %v", err)
	}
	roles := make([]string, 0, len(b.Parts))
	for r := range b.Parts {
		roles = append(roles, r)
	}
	// planBlockApply 里显式 --spacing 的分支:halfOf 为 nil。
	explicit := bapHalfExtentFn(220, nil)
	if got := explicit("U"); got != 110 {
		t.Fatalf("前提变了:显式 spacing 下网格半宽应为 spacing/2=110, got %v", got)
	}
	// footprint 用的那把尺必须仍然看得见模组的真实半宽。
	real := func(role string) float64 {
		if p, ok := b.Parts[role]; ok {
			return math.Max(bapRoleHalfExtent(p.Part), bapPartMargin)
		}
		return float64(bapPartMargin)
	}
	if real("U") != 250 {
		t.Errorf("footprint 尺必须用真实半宽 250,不受 --spacing 影响: got %v", real("U"))
	}
	offsets := map[string]bapRoleOffset{"U": {dx: 0, dy: 0}}
	thin, _ := bboxSize(bapBlockRect(0, 0, offsets, explicit))
	fat, _ := bboxSize(bapBlockRect(0, 0, offsets, real))
	if !(fat > thin) {
		t.Errorf("真实半宽算出的 footprint 必须更大: spacing 尺=%v 真实尺=%v", thin, fat)
	}
	_ = roles
}
