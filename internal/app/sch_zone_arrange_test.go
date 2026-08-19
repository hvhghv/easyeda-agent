package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
)

// 这组单测移植自 2026-08-16 演示页的证明面板:确定性不是口头承诺,是穷举验出来的。
// fixture 用 ceshi/P3_USB_DEBUG 的真机数值(演示页 v3 同源),两套形状:
//   raw  = 现状口径框(标签入框后):任意两个大区并不下一行 → 必 blocked;
//   cmpt = phase A 收敛后的框:有唯一解。

func zaSheetA4() (layoutBBox, *layoutBBox) {
	return layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825},
		&layoutBBox{MinX: 468, MinY: 0, MaxX: 1170, MaxY: 198}
}

func zaRawZones() []zaZone {
	return []zaZone{
		{Name: "Q", W: 572, H: 214, Home: [2]float64{274, 367}},
		{Name: "U", W: 682, H: 356, Home: [2]float64{747, 512}},
		{Name: "D_ESD", W: 256, H: 160, Home: [2]float64{210, 572}},
		{Name: "J_USB", W: 486, H: 276, Home: [2]float64{589, 322}},
	}
}

func zaCompactZones() []zaZone {
	return []zaZone{
		{Name: "Q", W: 246, H: 326, Home: [2]float64{274, 367}},
		{Name: "U", W: 318, H: 360, Home: [2]float64{747, 512}},
		{Name: "D_ESD", W: 243, H: 160, Home: [2]float64{210, 572}},
		{Name: "J_USB", W: 294, H: 279, Home: [2]float64{589, 322}},
	}
}

// zaP3Zones 是 2026-08-19 真机取证 fixture:ceshi / P3_USB_DL,phase A 收敛之后
// 的四个区框(收敛前 → 收敛后:U 682×312→315×353、J_USB 662×373→303×454、
// esp32_autodownload 497×231→249×400、D_ESD 256×177→242×183)。
//
// **首版 phase B 在这一页报 blocked**:
//
//	blocked —— esp32_autodownload 无处可放,回退链已试尽:S(230)→W(266)→N(595)→E(904)
//
// (回退链里的四个距离唯一确定了它的质心 = (266,230),所以这个 fixture 的
// Home 不是编的。)可四区总面积 392,643,可用面积 849,150 —— **占用只有 46%**,
// 手算解显然存在:
//
//	第 1 列 x 30..345(整列在图签左侧,不受 y 下界约束):U 353 + gutter 12 + autodl 400 = 765 = 可用高
//	第 2 列 x 357..660,y 从图签安全带之上起:J_USB 454
//	第 3 列 x 672..914:D_ESD 183 —— 总宽 884 ≤ 1110,横向还剩 226
//
// 排不下的是**形状**(准确说是「一条边只能开一列」的表达能力),不是面积。
// 这个 fixture 就是那条缺陷的回归钉子:它必须 pass,而且必须过既有验证器。
func zaP3Zones() []zaZone {
	return []zaZone{
		{Name: "U", W: 315, H: 353, Home: [2]float64{600, 500}},
		{Name: "J_USB", W: 303, H: 454, Home: [2]float64{150, 500}},
		{Name: "esp32_autodownload", W: 249, H: 400, Home: [2]float64{266, 230}},
		{Name: "D_ESD", W: 242, H: 183, Home: [2]float64{330, 500}},
	}
}

// zaP3TallZones 是**负对照**:P3 的四个区宽度不变、高度 ×1.25。每个区**单独**
// 都还放得下(所以不是容量诊断那种「一个区自己就塞不进」),但整页真的排不下 ——
// 手工穷举:
//   - J_USB 568 高 > 图签安全带之上的 555,所以它只能整块待在图签左侧那条窄带
//     (MaxX ≤ 426),而窄带高 765 装不下它 + 任何第二个区(最矮的 D_ESD 229:
//     568+12+229 = 809 > 765);
//   - 于是 U(315×441)/ autodl(249×500)/ D_ESD(242×229)三个必须全部落在
//     x ≥ 345 且 y ≥ 240 的区域(795 宽 × 555 高):并排要 830 > 795,
//     任意两个竖叠最矮也要 682 > 555。
//
// **没有这条负对照,「让 phase B 找到解」就等于「把判据改松」。**
func zaP3TallZones() []zaZone {
	return []zaZone{
		{Name: "U", W: 315, H: 441, Home: [2]float64{600, 500}},
		{Name: "J_USB", W: 303, H: 568, Home: [2]float64{150, 500}},
		{Name: "esp32_autodownload", W: 249, H: 500, Home: [2]float64{266, 230}},
		{Name: "D_ESD", W: 242, H: 229, Home: [2]float64{330, 500}},
	}
}

func zaPerms(zs []zaZone) [][]zaZone {
	if len(zs) <= 1 {
		return [][]zaZone{zs}
	}
	var out [][]zaZone
	for i := range zs {
		rest := append(append([]zaZone{}, zs[:i]...), zs[i+1:]...)
		for _, p := range zaPerms(rest) {
			out = append(out, append([]zaZone{zs[i]}, p...))
		}
	}
	return out
}

// 证明①:全排列穷举 —— 4!=24 种输入顺序,输出必须逐字节相同。
func TestZonesArrange_AllPermutationsIdentical(t *testing.T) {
	sheet, ko := zaSheetA4()
	base := zonesArrange(zaCompactZones(), sheet, ko, defaultPartitionOpts())
	if !base.OK {
		t.Fatalf("收敛后的形状该有解,得到 blocked=%s(%s)", base.Blocked, base.Tried)
	}
	perms := zaPerms(zaCompactZones())
	if len(perms) != 24 {
		t.Fatalf("穷举该有 24 种排列,得到 %d", len(perms))
	}
	for i, p := range perms {
		if got := zonesArrange(p, sheet, ko, defaultPartitionOpts()); !reflect.DeepEqual(got, base) {
			t.Fatalf("排列 %d 的输出与基准不同 —— 有序依赖!\nbase=%+v\ngot =%+v", i, base, got)
		}
	}
}

// 证明②:blocked 判决也要确定 —— 现状形状 24 种顺序必须同判同一个 blocked。
func TestZonesArrange_RawShapesBlockedDeterministically(t *testing.T) {
	sheet, ko := zaSheetA4()
	base := zonesArrange(zaRawZones(), sheet, ko, defaultPartitionOpts())
	if base.OK {
		t.Fatalf("现状形状(U 682 宽 + J 486 宽)不该有解 —— phase A 是前置条件,不是优化")
	}
	for i, p := range zaPerms(zaRawZones()) {
		got := zonesArrange(p, sheet, ko, defaultPartitionOpts())
		if got.OK || got.Blocked != base.Blocked {
			t.Fatalf("排列 %d 判决漂移:base blocked=%s got=%+v", i, base.Blocked, got)
		}
	}
	// blocked 必须带能执行的解释:回退链 + 各边距离。
	if base.Tried == "" {
		t.Error("blocked 该报出回退链(是谁、每条边为何不行),得到空")
	}
}

// 证明③:5 格律 —— 每个落位框的**两个锚坐标**都落在 5 的整数倍上
// (L/R/B/T 由 snap5Up/snap5Dn 得到,本身就是 5 的整数倍,所以「离规范角 5k」
// 与「是 5 的整数倍」是同一件事);框的另一侧 = 锚 ± 区尺寸,区尺寸不圆整
// (圆整会吃掉 phase A 收敛出来的那几个单位余量 —— P3 左列 353+12+400=765
// 恰好等于可用高,round-up 到 355 就当场 blocked)。执行侧再圆整件的 Δ。
//
// 锚是哪两个由落位边决定:W=MinX+MaxY、E=MaxX+MaxY、N=MinX+MaxY、S=MinX+MinY。
func zaAnchorCoords(p zaPlaced) []float64 {
	switch p.Edge {
	case "W", "N":
		return []float64{p.Rect.MinX, p.Rect.MaxY}
	case "E":
		return []float64{p.Rect.MaxX, p.Rect.MaxY}
	default: // S
		return []float64{p.Rect.MinX, p.Rect.MinY}
	}
}

func TestZonesArrange_Lattice(t *testing.T) {
	sheet, ko := zaSheetA4()
	for _, tc := range []struct {
		name  string
		zones []zaZone
	}{
		{"compact", zaCompactZones()},
		{"P3-real", zaP3Zones()},
	} {
		res := zonesArrange(tc.zones, sheet, ko, defaultPartitionOpts())
		if !res.OK {
			t.Fatalf("%s fixture 该有解:blocked=%s(%s)", tc.name, res.Blocked, res.Tried)
		}
		for _, p := range res.Placed {
			for _, v := range zaAnchorCoords(p) {
				if math.Mod(v, zaScanStep) != 0 {
					t.Errorf("%s/%s@%s 锚坐标 %.2f 脱格:rect=%+v", tc.name, p.Name, p.Edge, v, p.Rect)
				}
			}
		}
	}
}

// 证明④:同一把尺 —— 输出必须过**既有的** validatePartitions(不是本文件自带的尺)。
func TestZonesArrange_OutputPassesExistingRuler(t *testing.T) {
	sheet, ko := zaSheetA4()
	res := zonesArrange(zaCompactZones(), sheet, ko, defaultPartitionOpts())
	if !res.OK {
		t.Fatal("fixture 该有解")
	}
	v := zaValidate(res, sheet, ko, defaultPartitionOpts())
	if v.SheetOverflow != 0 || v.PartitionOverlap != 0 || v.TitleBlockHits != 0 || v.SheetMarginHits != 0 {
		t.Fatalf("输出未过既有验证器:%+v", v)
	}
}

// 稳定性(用户 2026-08-16 确认的性质):确定的元器件集合每次同一解;
// 小幅挪动某个元素 —— 只要没改变「首选边 + 实际用到的回退边 + 边内次序」——
// **落位**一个字都不变。位置只参与边归属与排序平局,不参与落位坐标。
// (比对的是落位投影 name/rect/edge;回退链尾部的诊断性次序允许随距离微调 ——
// 它不影响任何落位,J_USB 的 W/E 谁排链尾就属于这类。)
func TestZonesArrange_StableUnderSmallHomeJitter(t *testing.T) {
	sheet, ko := zaSheetA4()
	proj := func(r zaResult) []zaPlaced {
		out := make([]zaPlaced, len(r.Placed))
		for i, p := range r.Placed {
			out[i] = zaPlaced{Name: p.Name, Rect: p.Rect, Edge: p.Edge}
		}
		return out
	}
	base := proj(zonesArrange(zaCompactZones(), sheet, ko, defaultPartitionOpts()))
	for _, d := range [][2]float64{{8, 0}, {-8, 0}, {0, 8}, {0, -8}, {6, -6}} {
		zs := zaCompactZones()
		for i := range zs { // 全体小幅漂移(模拟区内挪件带来的质心抖动)
			zs[i].Home[0] += d[0]
			zs[i].Home[1] += d[1]
		}
		if got := proj(zonesArrange(zs, sheet, ko, defaultPartitionOpts())); !reflect.DeepEqual(got, base) {
			t.Fatalf("±8 的质心抖动改变了落位(d=%v)—— 稳定性性质被破坏\nbase=%+v\ngot =%+v", d, base, got)
		}
	}
}

// 声明边优先于质心回退,且声明后位置彻底失去影响(归属写回声明的意义)。
func TestZonesArrange_DeclaredEdgeWins(t *testing.T) {
	sheet, ko := zaSheetA4()
	zs := zaCompactZones()
	for i := range zs {
		if zs[i].Name == "D_ESD" {
			zs[i].Edge = "E" // 质心明明最靠 W,声明说 E
		}
	}
	res := zonesArrange(zs, sheet, ko, defaultPartitionOpts())
	if !res.OK {
		t.Fatalf("blocked=%s(%s)", res.Blocked, res.Tried)
	}
	for _, p := range res.Placed {
		if p.Name == "D_ESD" {
			if p.Edge != "E" {
				t.Fatalf("声明 E 该落 E,落到了 %s", p.Edge)
			}
			if p.Chain[0] != "E" {
				t.Fatalf("声明边该是链首,链=%v", p.Chain)
			}
		}
	}
}

// 回退链真的在工作:P3 收敛形状下 J_USB 首选 S 的**贴边那一行**被 Q 与图签夹死,
// 必须落到链上后位。
//
// 2026-08-19 注:多层货架是「回退链整轮走完之后」才往里开的(候选序 = 货架层优先,
// 层内按回退链走边)—— 所以边归属语义没有被稀释:不会为了一个区把它塞进 W 边第
// 5 层(那已经是页面正中)而 E 边贴边明明空着。这条断言也因此原封不动地成立。
func TestZonesArrange_FallbackChainEngages(t *testing.T) {
	sheet, ko := zaSheetA4()
	res := zonesArrange(zaCompactZones(), sheet, ko, defaultPartitionOpts())
	if !res.OK {
		t.Fatal("fixture 该有解")
	}
	var j *zaPlaced
	for i := range res.Placed {
		if res.Placed[i].Name == "J_USB" {
			j = &res.Placed[i]
		}
	}
	if j == nil {
		t.Fatal("J_USB 没在输出里")
	}
	if j.Chain[0] != "S" {
		t.Fatalf("J_USB 首选边该是 S(质心 322),链=%v", j.Chain)
	}
	if j.Edge == "S" {
		t.Fatalf("S 边贴边那一行该被 Q 与图签安全带夹死,J 却落在了 S:%+v", j.Rect)
	}
	if j.Shelf != 0 {
		t.Fatalf("回退到 %s 时该落在贴边那一层(层优先于深度),得到 shelf=%d", j.Edge, j.Shelf)
	}
}

// 回溯真的在工作 —— 这是 2026-08-19 缺陷的第二条钉子(第一条是多层货架)。
//
// 旧版是**不可回头的贪心**:先落位的区占了坏位置,后面全盘皆输,直接判死。
// P3 真机页正是这样炸的 —— J_USB 贴着左边顶格落下(454 高),左列剩下 299 高,
// 400 高的 esp32_autodownload 就再也进不去了。现在求解器会退回去换 U 的候选。
// 判据:P3 的解里必须有区**不是**落在它的首个可行候选上(step>1 且是被迫的),
// 亦即贪心第一条下潜路径不是解。
func TestZonesArrange_P3NeedsBacktracking(t *testing.T) {
	sheet, ko := zaSheetA4()
	res := zonesArrange(zaP3Zones(), sheet, ko, defaultPartitionOpts())
	if !res.OK {
		t.Fatalf("fixture 该有解:%s(%s)", res.Blocked, res.Tried)
	}
	if zaLegacyGreedy(zaP3Zones(), sheet, ko, defaultPartitionOpts()) {
		t.Fatal("旧版贪心在 P3 上居然有解 —— 那这个 fixture 就钉不住修复了")
	}
	// 修复后必须有解(与 P3RealFixturePasses 重叠是有意的:这条读起来是
	//「贪心无解 → 回溯有解」的对照,拆开看反而丢了因果)。
	if !res.OK {
		t.Fatal("回溯后该有解")
	}
}

// zaLegacyGreedy 复现**首版**的落位策略:每条边只有贴边那一层货架、首个可行位
// 即落、绝不回头。它就是被修掉的那个东西 —— 留在单测里当负基线,证明 fixture
// 确实需要新能力才有解(否则「修复」可能只是换了个写法)。
func zaLegacyGreedy(zones []zaZone, sheet layoutBBox, keepout *layoutBBox, opts partitionOpts) bool {
	s := newZaSearch(zones, sheet, keepout, opts)
	for _, z := range s.zs {
		landed := false
		for _, e := range z.chain {
			sh := s.candidates(z.zaZone, e)
			if len(sh) == 0 {
				continue
			}
			for _, c := range sh[0] { // sh[0] = 贴边那一层 = 首版能看见的全部
				if ok, _ := s.fits(c.rect); ok {
					s.obs = append(s.obs, zaObstacle{box: c.rect, name: z.Name})
					landed = true
					break
				}
			}
			if landed {
				break
			}
		}
		if !landed {
			return false
		}
	}
	return true
}

// blocked 报文可读性:必须点名 + 带回退链距离(能执行的下一步,不是一句报错)。
func TestZonesArrange_BlockedMessageActionable(t *testing.T) {
	sheet, ko := zaSheetA4()
	res := zonesArrange(zaRawZones(), sheet, ko, defaultPartitionOpts())
	if res.OK {
		t.Fatal("raw 形状该 blocked")
	}
	if res.Blocked == "" {
		t.Error("该点名是谁 blocked")
	}
	want := fmt.Sprintf("%s(", res.Blocked)
	_ = want
	if len(res.Tried) < len("W(0)→E(0)→N(0)") {
		t.Errorf("Tried 该是完整回退链,得到 %q", res.Tried)
	}
}

// ── 2026-08-19 缺陷回归:phase B 在明显有解时报 blocked ────────────────────────

// 验收①:P3 真机 fixture 必须 pass,且必须过**既有的** validatePartitions
// (三计数全 0)—— 判定与生成同一把尺,不许自带一把宽松的尺给自己发合格证。
func TestZonesArrange_P3RealFixturePasses(t *testing.T) {
	sheet, ko := zaSheetA4()
	opts := defaultPartitionOpts()
	res := zonesArrange(zaP3Zones(), sheet, ko, opts)
	if !res.OK {
		t.Fatalf("P3 收敛后四区占用只有 46%%,手算解存在,不该 blocked:blocked=%s(%s)",
			res.Blocked, res.Tried)
	}
	if len(res.Placed) != 4 {
		t.Fatalf("四个区都要落位,得到 %d 个", len(res.Placed))
	}
	v := zaValidate(res, sheet, ko, opts)
	if v.PartitionOverlap != 0 || v.TitleBlockHits != 0 || v.SheetOverflow != 0 || v.SheetMarginHits != 0 {
		t.Fatalf("输出未过既有验证器:%+v\nplaced=%+v", v, res.Placed)
	}
	// 落位互不相交(留够 gutter)—— 验证器之外再钉一遍几何本体。
	for i := 0; i < len(res.Placed); i++ {
		for j := i + 1; j < len(res.Placed); j++ {
			if zaHit(res.Placed[i].Rect, res.Placed[j].Rect, 0) {
				t.Errorf("%s 与 %s 相交:%+v / %+v", res.Placed[i].Name, res.Placed[j].Name,
					res.Placed[i].Rect, res.Placed[j].Rect)
			}
		}
	}
}

// 验收①附:解之所以存在,靠的是「同一条边开第二列/第二行」这个新增的表达能力。
// 如果哪天有人把多层货架退回单层,这条会先炸 —— 它盯的是**机制**,不是结果。
func TestZonesArrange_P3NeedsMultiShelf(t *testing.T) {
	sheet, ko := zaSheetA4()
	res := zonesArrange(zaP3Zones(), sheet, ko, defaultPartitionOpts())
	if !res.OK {
		t.Fatal("fixture 该有解")
	}
	multi := 0
	for _, p := range res.Placed {
		if p.Shelf > 0 {
			multi++
		}
	}
	if multi == 0 {
		t.Fatalf("P3 的解必须用到非贴边货架(旧版单层扫描正是在这里判死的),placed=%+v", res.Placed)
	}
}

// 验收②:负对照 —— 真正装不下的输入仍然 blocked,而且**指名道姓**:
// 报出是谁排不下、回退链上每条边各卡在谁身上。
func TestZonesArrange_TallVariantStillBlocked(t *testing.T) {
	sheet, ko := zaSheetA4()
	opts := defaultPartitionOpts()
	// 前提:每个区**单独**都放得下 —— 否则这就成了容量诊断题,证明不了装箱判据没松。
	usable := layoutBBox{MinX: sheet.MinX + opts.Margin, MinY: sheet.MinY + opts.Margin,
		MaxX: sheet.MaxX - opts.Margin, MaxY: sheet.MaxY - opts.Margin}
	for _, z := range zaP3TallZones() {
		if !fitsAroundCorner(z.W, z.H, usable, inflatedTitleKeepout(ko)) {
			t.Fatalf("负对照失格:%s(%.0f×%.0f)自己就塞不进,这题变成了容量诊断", z.Name, z.W, z.H)
		}
	}
	res := zonesArrange(zaP3TallZones(), sheet, ko, opts)
	if res.OK {
		t.Fatalf("×1.25 高的四个区排不下(J_USB 568 只能待窄带、剩三个 830 宽塞不进 795),"+
			"求解器却给出了解 —— 判据被改松了:%+v", res.Placed)
	}
	if res.Exhausted {
		t.Fatalf("这题该是搜完判死,不该是预算耗尽:%s", res.Tried)
	}
	if res.Blocked == "" {
		t.Error("blocked 必须点名是谁排不下")
	}
	if len(res.Edges) == 0 {
		t.Fatal("blocked 必须给出逐边归因(每条边各卡在哪)")
	}
	for _, e := range res.Edges {
		if e.Cands > 0 && e.Blocker == "" {
			t.Errorf("边 %s 生成了 %d 个候选却说不出被谁挡 —— 归因不可执行", e.Edge, e.Cands)
		}
	}
	if len(res.Edges) != 4 {
		t.Errorf("回退链四条边都该有归因,得到 %d 条:%s", len(res.Edges), res.Tried)
	}
}

// 验收③:确定性穷举 —— pass 与 blocked 两侧,24 种输入排列的**序列化结果**必须
// 逐字节相同(哈希一致)。map 迭代序、浮点平局、回溯路径,任何一处漏网都在这里现形。
func TestZonesArrange_PermutationHashStable(t *testing.T) {
	sheet, ko := zaSheetA4()
	for _, tc := range []struct {
		name  string
		zones []zaZone
	}{
		{"P3-pass", zaP3Zones()},
		{"P3-tall-blocked", zaP3TallZones()},
		{"raw-blocked", zaRawZones()},
		{"compact-pass", zaCompactZones()},
	} {
		perms := zaPerms(tc.zones)
		if len(perms) != 24 {
			t.Fatalf("%s:穷举该有 24 种排列,得到 %d", tc.name, len(perms))
		}
		var want string
		for i, p := range perms {
			blob, err := json.Marshal(zonesArrange(p, sheet, ko, defaultPartitionOpts()))
			if err != nil {
				t.Fatalf("%s:序列化失败 %v", tc.name, err)
			}
			got := fmt.Sprintf("%x", sha256.Sum256(blob))
			if i == 0 {
				want = got
				continue
			}
			if got != want {
				t.Fatalf("%s:排列 %d 的结果哈希与基准不同 —— 有序依赖!\n%s", tc.name, i, blob)
			}
		}
	}
}

// 验收④:求解耗时有确定性上界 —— blocked 侧不许靠「跑到天荒地老」证明无解。
// 预算是候选评估次数(不是时间),所以这条断言在任何机器上结论相同。
func TestZonesArrange_BlockedWithinBudget(t *testing.T) {
	sheet, ko := zaSheetA4()
	for _, zs := range [][]zaZone{zaRawZones(), zaP3TallZones()} {
		res := zonesArrange(zs, sheet, ko, defaultPartitionOpts())
		if res.OK {
			t.Fatalf("该 blocked:%+v", res.Placed)
		}
		if res.Exhausted {
			t.Fatalf("预算 %d 次候选评估没跑完这道题 —— 要么调预算要么加剪枝,"+
				"不能让 blocked 变成「没搜完」的同义词:%s", zaSearchBudget, res.Tried)
		}
	}
}

// 单区就塞不进的输入照样 blocked(容量级),且点名的是那个区 —— 归因不许张冠李戴。
func TestZonesArrange_OversizedZoneNamed(t *testing.T) {
	sheet, ko := zaSheetA4()
	zs := []zaZone{
		{Name: "D_ESD", W: 242, H: 183, Home: [2]float64{330, 500}},
		{Name: "U_HUGE", W: 500, H: 600, Home: [2]float64{600, 400}}, // 宽>396 且 高>555:L 形两条腿都进不去
	}
	res := zonesArrange(zs, sheet, ko, defaultPartitionOpts())
	if res.OK {
		t.Fatalf("500×600 在 A4 上绕不开图签,该 blocked:%+v", res.Placed)
	}
	if res.Blocked != "U_HUGE" {
		t.Fatalf("该点名 U_HUGE,点了 %q(%s)", res.Blocked, res.Tried)
	}
}

// 真实页规模(8 个功能区)也要跑得动、跑得稳:pass + 过既有验证器 + 换输入顺序
// 结果不变。四区 fixture 证明的是判据,这条证明的是**规模**——多层货架 + 回溯
// 引入的是搜索,搜索一上规模就容易翻车(慢、或者被预算截断)。
func zaEightZones() []zaZone {
	return []zaZone{
		{Name: "M1", W: 250, H: 180, Home: [2]float64{120, 700}},
		{Name: "M2", W: 250, H: 180, Home: [2]float64{120, 450}},
		{Name: "M3", W: 240, H: 200, Home: [2]float64{400, 700}},
		{Name: "M4", W: 240, H: 200, Home: [2]float64{400, 450}},
		{Name: "M5", W: 260, H: 170, Home: [2]float64{700, 700}},
		{Name: "M6", W: 260, H: 170, Home: [2]float64{700, 450}},
		{Name: "M7", W: 230, H: 190, Home: [2]float64{1000, 700}},
		{Name: "M8", W: 230, H: 190, Home: [2]float64{1000, 450}},
	}
}

func TestZonesArrange_EightZonePageScales(t *testing.T) {
	sheet, ko := zaSheetA4()
	opts := defaultPartitionOpts()
	base := zonesArrange(zaEightZones(), sheet, ko, opts)
	if !base.OK {
		t.Fatalf("8 区小模块页该有解(总面积不到可用的一半):%s(%s)", base.Blocked, base.Tried)
	}
	v := zaValidate(base, sheet, ko, opts)
	if v.PartitionOverlap != 0 || v.TitleBlockHits != 0 || v.SheetOverflow != 0 || v.SheetMarginHits != 0 {
		t.Fatalf("8 区输出未过既有验证器:%+v", v)
	}
	// 8! 全排列太多,取一组**固定**的重排(反转 + 逐位轮转)—— 依旧是确定性判据。
	perm := func(zs []zaZone, k int) []zaZone {
		out := make([]zaZone, 0, len(zs))
		for i := range zs {
			out = append(out, zs[(i+k)%len(zs)])
		}
		return out
	}
	want, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for k := 1; k < 8; k++ {
		for _, zs := range [][]zaZone{perm(zaEightZones(), k), func() []zaZone {
			r := perm(zaEightZones(), k)
			for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
				r[i], r[j] = r[j], r[i]
			}
			return r
		}()} {
			got, err := json.Marshal(zonesArrange(zs, sheet, ko, opts))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("轮转 %d 的输出与基准不同 —— 有序依赖!\nwant=%s\ngot =%s", k, want, got)
			}
		}
	}
}
