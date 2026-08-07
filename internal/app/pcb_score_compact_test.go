package app

// pcb_score_compact_test.go — 紧凑度维的离线单测。
//
// 全部是纯结构体字面量喂纯函数：不连 daemon、不连编辑器，跑 `go test` 即可。
// 这一维的判据里有两条是**约定级**的（"没测不能给满分"、"硬碰撞不重复扣分"），
// 单测把它们钉死——它们坏掉时不会有编译错误，只会安静地把校准判据废掉。

import (
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 造板助手
// ---------------------------------------------------------------------------

// compactSqComp 造一个位于 (x,y)、边长 size 的方形器件（bbox 即本体）。
func compactSqComp(des string, layer int, x, y, size float64) boardComp {
	return boardComp{
		Designator: des, Layer: layer, X: x, Y: y,
		BBox: &layoutBBox{MinX: x, MinY: y, MaxX: x + size, MaxY: y + size},
	}
}

// compactRectOutline 造一块 w×h 的矩形板（带真实多边形点列 → Source=="polygon"）。
func compactRectOutline(w, h float64) *boardOutline {
	return &boardOutline{
		BBox:   layoutBBox{MinX: 0, MinY: 0, MaxX: w, MaxY: h},
		Points: [][2]float64{{0, 0}, {w, 0}, {w, h}, {0, h}},
		Source: "polygon",
	}
}

// runCompactScore 是被测入口的薄封装。
func runCompactScore(snap *boardSnapshot, layout *pcbLayoutReport) scoreDimension {
	if layout == nil {
		layout = &pcbLayoutReport{}
	}
	return compactScorer{}.score(&scoreCtx{snap: snap, layout: layout, opts: layoutScoreOpts{}})
}

// compactGridBoard 在 w×h 的板上铺 cols×rows 个边长 size 的方件，间距 pitch。
func compactGridBoard(w, h float64, cols, rows int, size, pitch float64) *boardSnapshot {
	snap := &boardSnapshot{Outline: compactRectOutline(w, h)}
	n := 0
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			n++
			snap.Components = append(snap.Components,
				compactSqComp(compactDesOf(n), pcbSideTop, 50+float64(c)*pitch, 50+float64(r)*pitch, size))
		}
	}
	return snap
}

func compactDesOf(n int) string {
	// R1、R2… 够用了；测试只关心位号唯一。
	return "R" + compactItoa(n)
}

func compactItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 评分曲线：双侧
// ---------------------------------------------------------------------------

// 曲线必须是**双侧**的：平台内满分，两侧各自下滑并停在各自地板分。
// 单调曲线（"越紧凑越好"或"越松越好"）会漏掉另一侧的缺陷，这是这一维存在的理由。
func TestCompactUtilScore_Trapezoid(t *testing.T) {
	cases := []struct {
		name string
		u    float64
		want float64
	}{
		{"平台下沿", compactPlateauLo, 100},
		{"平台中间", (compactPlateauLo + compactPlateauHi) / 2, 100},
		{"平台上沿", compactPlateauHi, 100},
		{"空到底", compactSparseFloor, compactSparseMinScore},
		{"比空到底还空", 0.001, compactSparseMinScore},
		{"挤到顶", compactDenseCeil, compactDenseMinScore},
		{"比挤到顶还挤", 0.99, compactDenseMinScore},
	}
	for _, c := range cases {
		if got := compactUtilScore(c.u); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: compactUtilScore(%.3f) = %.2f, want %.2f", c.name, c.u, got, c.want)
		}
	}

	// 两侧都必须**严格**扣分（不是平台的延伸），否则"太空"和"太挤"测不出来。
	sparse := compactUtilScore((compactSparseFloor + compactPlateauLo) / 2)
	if sparse >= 100 || sparse <= compactSparseMinScore {
		t.Errorf("稀疏侧中点 = %.2f，应严格落在 (%.0f, 100) 内", sparse, compactSparseMinScore)
	}
	dense := compactUtilScore((compactPlateauHi + compactDenseCeil) / 2)
	if dense >= 100 || dense <= compactDenseMinScore {
		t.Errorf("拥挤侧中点 = %.2f，应严格落在 (%.0f, 100) 内", dense, compactDenseMinScore)
	}

	// 拥挤侧罚得比稀疏侧重（太挤是工程后果，太空只是浪费）——这条不对称是刻意的。
	if compactDenseMinScore >= compactSparseMinScore {
		t.Errorf("拥挤侧地板分 %.0f 应低于稀疏侧 %.0f", compactDenseMinScore, compactSparseMinScore)
	}
}

// 单调性：平台左侧随利用率递增，右侧随利用率递减。
func TestCompactUtilScore_Monotonic(t *testing.T) {
	prev := -1.0
	for u := 0.0; u <= compactPlateauLo; u += 0.005 {
		s := compactUtilScore(u)
		if s < prev-1e-9 {
			t.Fatalf("稀疏侧非单调递增：u=%.3f 得 %.2f，前一档 %.2f", u, s, prev)
		}
		prev = s
	}
	prev = 101.0
	for u := compactPlateauHi; u <= 1.0; u += 0.005 {
		s := compactUtilScore(u)
		if s > prev+1e-9 {
			t.Fatalf("拥挤侧非单调递减：u=%.3f 得 %.2f，前一档 %.2f", u, s, prev)
		}
		prev = s
	}
}

// ---------------------------------------------------------------------------
// 「没测 ≠ 测了满分」
// ---------------------------------------------------------------------------

// 板框读不到（PCB 不在前台时平台返 null）→ 必须 skipped，绝不能给 100。
// 这条一破，一块什么都没测的板就能在 #167 第五层的校准里拿满分。
func TestCompactScore_NoOutlineSkips(t *testing.T) {
	snap := &boardSnapshot{Components: []boardComp{compactSqComp("R1", pcbSideTop, 0, 0, 40)}}
	d := runCompactScore(snap, nil)
	if d.Status != dimSkipped {
		t.Fatalf("板框缺失 status = %s, want %s（分 %.1f）", d.Status, dimSkipped, d.Score)
	}
	if d.Score == 100 {
		t.Errorf("skipped 维不得给满分")
	}
	if d.Reason == "" {
		t.Errorf("skipped 必须写明原因")
	}
}

func TestCompactScore_NoComponentsSkips(t *testing.T) {
	d := runCompactScore(&boardSnapshot{Outline: compactRectOutline(2000, 1500)}, nil)
	if d.Status != dimSkipped || d.Reason == "" {
		t.Fatalf("空板 status=%s reason=%q，want skipped+原因", d.Status, d.Reason)
	}
}

// 器件全都没有 bbox → 量不到面积 → skipped（不是"面积 0 所以超级空"）。
func TestCompactScore_AllComponentsWithoutBBoxSkips(t *testing.T) {
	snap := &boardSnapshot{
		Outline:    compactRectOutline(2000, 1500),
		Components: []boardComp{{Designator: "R1", Layer: pcbSideTop}, {Designator: "R2", Layer: pcbSideTop}},
	}
	d := runCompactScore(snap, nil)
	if d.Status != dimSkipped {
		t.Fatalf("全员无 bbox status = %s, want skipped", d.Status)
	}
}

// ---------------------------------------------------------------------------
// 「输入是近似 → degraded」
// ---------------------------------------------------------------------------

// 本仓根本没有 courtyard，只有含丝印的渲染 bbox → 这一维今天恒为 degraded，
// 且 Reason 必须点破"渲染 bbox 不是 courtyard"，否则读报告的人会把 60% 当真值。
func TestCompactScore_AlwaysDegradedWithoutCourtyard(t *testing.T) {
	snap := compactGridBoard(2000, 1500, 4, 3, 100, 300) // 12 件 ×1e4 = 1.2e5 / 3e6 = 4%
	d := runCompactScore(snap, nil)
	if d.Status != dimDegraded {
		t.Fatalf("status = %s, want %s（无 courtyard 数据）", d.Status, dimDegraded)
	}
	if !strings.Contains(d.Reason, "courtyard") {
		t.Errorf("degraded 原因必须点破 courtyard 缺失，实际 %q", d.Reason)
	}
}

// 板框只有 AABB（Source=="bbox"，异形板）→ 面积偏大，Reason 必须额外说明。
func TestCompactScore_AABBOutlineNoted(t *testing.T) {
	snap := compactGridBoard(2000, 1500, 4, 4, 200, 320)
	snap.Outline = &boardOutline{BBox: layoutBBox{MaxX: 2000, MaxY: 1500}, Source: "bbox"}
	d := runCompactScore(snap, nil)
	if d.Status != dimDegraded || !strings.Contains(d.Reason, "AABB") {
		t.Fatalf("AABB 板框应额外降级说明，status=%s reason=%q", d.Status, d.Reason)
	}
}

// 部分器件缺 bbox → 计入 degraded，并在 Metrics 里暴露 componentsWithoutBBox，
// 让判读的人知道利用率是偏低的。
func TestCompactScore_MissingBBoxExposed(t *testing.T) {
	snap := compactGridBoard(2000, 1500, 3, 3, 200, 400)
	snap.Components = append(snap.Components, boardComp{Designator: "U9", Layer: pcbSideTop})
	d := runCompactScore(snap, nil)
	if got := d.Metrics["componentsWithoutBBox"]; got != 1 {
		t.Fatalf("componentsWithoutBBox = %v, want 1", got)
	}
	if d.Status != dimDegraded || !strings.Contains(d.Reason, "bbox") {
		t.Errorf("缺 bbox 必须进降级说明：status=%s reason=%q", d.Status, d.Reason)
	}
}

// ---------------------------------------------------------------------------
// 双面板：两面分别算，取较大者
// ---------------------------------------------------------------------------

// 底面的件不占顶面的空间。顶面 ~50%、底面 ~8% 的板真实拥挤度是 50%（平台内，
// 满分）；若把两面相加会算成 58%（仍在平台内但已逼近拐点），加得再多就会被误判
// 成"太挤"。这条钉住"分面统计 + 取 max"的口径。
func TestCompactScore_PerSideNotSummed(t *testing.T) {
	board := compactRectOutline(2000, 2000) // 4e6 mil²
	snap := &boardSnapshot{Outline: board}
	// 顶面：8 件 ×500×500 = 2.0e6 → 50%
	for i := 0; i < 8; i++ {
		snap.Components = append(snap.Components,
			compactSqComp("U"+compactItoa(i+1), pcbSideTop, float64(i%4)*500, float64(i/4)*500, 500))
	}
	// 底面：2 件 ×300×300 = 1.8e5 → 4.5%
	for i := 0; i < 2; i++ {
		snap.Components = append(snap.Components,
			compactSqComp("C"+compactItoa(i+1), pcbSideBottom, float64(i)*400, 1600, 300))
	}
	d := runCompactScore(snap, nil)

	if got := d.Metrics["utilizationTop"]; math.Abs(got-0.5) > 0.01 {
		t.Errorf("utilizationTop = %.3f, want ~0.50", got)
	}
	if got := d.Metrics["utilizationBottom"]; math.Abs(got-0.045) > 0.01 {
		t.Errorf("utilizationBottom = %.3f, want ~0.045", got)
	}
	if got := d.Metrics["utilization"]; math.Abs(got-0.5) > 0.01 {
		t.Errorf("utilization = %.3f，应取两面较大者 0.50，而不是相加的 0.545", got)
	}
	if d.Score != 100 {
		t.Errorf("顶面 50%% 在平台内，应满分，实际 %.1f", d.Score)
	}
}

// ---------------------------------------------------------------------------
// 硬碰撞：只报数，不重复扣分
// ---------------------------------------------------------------------------

// 渲染 bbox 会把"丝印框相擦"的紧凑好板判成 overlap，那已经进了 Blocking 一票否决。
// 这一维若再扣一遍，同一件事罚两次，紧凑板与乱撞板又被搅回一起。
func TestCompactScore_HardCollisionsNotDoubleCounted(t *testing.T) {
	snap := compactGridBoard(2000, 2000, 4, 4, 400, 480) // 16 件 ×1.6e5 = 2.56e6 / 4e6 = 64%
	clean := runCompactScore(snap, nil)

	withHits := runCompactScore(snap, &pcbLayoutReport{
		Overlaps: []pcbLFinding{{Type: "overlap", A: "R1", B: "R2"}},
		Shorts:   []pcbLShort{{A: "R1.1", NetA: "GND", B: "R2.1", NetB: "3V3"}},
	})

	if withHits.Score != clean.Score {
		t.Fatalf("硬碰撞改变了分数（%.1f → %.1f），本维不得重复扣分", clean.Score, withHits.Score)
	}
	if got := withHits.Metrics["hardCollisions"]; got != 2 {
		t.Errorf("hardCollisions = %v, want 2（只报数）", got)
	}
	if !strings.Contains(withHits.Reason, "blocking") {
		t.Errorf("Reason 应提示硬碰撞已单列 blocking，实际 %q", withHits.Reason)
	}
}

// ---------------------------------------------------------------------------
// 归因
// ---------------------------------------------------------------------------

// 稀疏板：一小簇件挤在角上，另有两件被扔到板子对角 —— 扣分必须归到"周围空得
// 离谱"的那两件头上（它们才是可收紧的对象），而不是归到那一簇正常的件。
func TestCompactScore_SparseAttribution(t *testing.T) {
	snap := &boardSnapshot{Outline: compactRectOutline(6000, 6000)} // 3.6e7 mil²
	// 一簇 8 件，彼此 20mil 间距 —— 中位邻距会落在 20 附近。
	for i := 0; i < 8; i++ {
		snap.Components = append(snap.Components,
			compactSqComp("R"+compactItoa(i+1), pcbSideTop, 100+float64(i)*120, 100, 100))
	}
	// 两个孤岛，离那一簇几千 mil。
	snap.Components = append(snap.Components,
		compactSqComp("U1", pcbSideTop, 5000, 5000, 200),
		compactSqComp("U2", pcbSideTop, 5600, 3000, 200))

	d := runCompactScore(snap, nil)
	if d.Score >= 100 {
		t.Fatalf("空板（利用率 %.3f）不该满分，实际 %.1f", d.Metrics["utilization"], d.Score)
	}
	if len(d.Contributors) == 0 {
		t.Fatalf("扣了分就必须给归因（reason=%q）", d.Reason)
	}
	top := d.Contributors[0].Designator
	if top != "U1" && top != "U2" {
		t.Errorf("首位归因 = %s，应是被扔到远处的 U1/U2", top)
	}
	for _, c := range d.Contributors {
		if strings.HasPrefix(c.Designator, "R") {
			t.Errorf("簇内正常件 %s 不该被归因为「空得离谱」", c.Designator)
		}
		if c.Detail == "" {
			t.Errorf("%s 归因缺 Detail", c.Designator)
		}
	}
	// Penalty 必须降序（精修环靠它排序决定先动谁）。
	for i := 1; i < len(d.Contributors); i++ {
		if d.Contributors[i-1].Penalty < d.Contributors[i].Penalty {
			t.Fatalf("Contributors 未按 Penalty 降序：%+v", d.Contributors)
		}
	}
	if len(d.Findings) == 0 || d.Findings[0].Type != "board-underused" {
		t.Errorf("稀疏侧应给 board-underused finding，实际 %+v", d.Findings)
	}
}

// 拥挤板：扣分归到吃板面最多的大件（可动的手就是放大板框或换小封装）。
func TestCompactScore_DenseAttribution(t *testing.T) {
	board := compactRectOutline(2000, 2000) // 4e6
	snap := &boardSnapshot{Outline: board}
	// 一个 1400×1400 的巨件（1.96e6，占 49%）
	snap.Components = append(snap.Components, compactSqComp("U1", pcbSideTop, 20, 20, 1400))
	// 一堆小件补到 ~80%
	for i := 0; i < 12; i++ {
		snap.Components = append(snap.Components,
			compactSqComp("R"+compactItoa(i+1), pcbSideTop, 1450+float64(i%2)*270, 20+float64(i/2)*330, 260))
	}
	d := runCompactScore(snap, nil)
	if u := d.Metrics["utilization"]; u <= compactPlateauHi {
		t.Fatalf("这块板应落在拥挤侧，实际利用率 %.3f", u)
	}
	if d.Score >= 100 {
		t.Fatalf("拥挤板不该满分，实际 %.1f", d.Score)
	}
	if len(d.Contributors) == 0 {
		t.Fatalf("拥挤侧也必须给归因")
	}
	if d.Contributors[0].Designator != "U1" {
		t.Errorf("首位归因 = %s，应是吃掉半块板的 U1", d.Contributors[0].Designator)
	}
	if len(d.Findings) == 0 || d.Findings[0].Type != "board-overcrowded" {
		t.Errorf("拥挤侧应给 board-overcrowded finding，实际 %+v", d.Findings)
	}
}

// 平台内的板：满分、无扣分、也就没有归因（Penalty 的语义是"扣掉多少分"，
// 满分维挂着非零 Penalty 会让精修环去动一块没问题的板）。
func TestCompactScore_PlateauNoContributors(t *testing.T) {
	snap := compactGridBoard(2000, 2000, 4, 4, 350, 480) // 16×1.2e5 = 1.96e6 / 4e6 = 49%
	d := runCompactScore(snap, nil)
	if d.Score != 100 {
		t.Fatalf("利用率 %.3f 在平台内，应满分，实际 %.1f", d.Metrics["utilization"], d.Score)
	}
	if len(d.Contributors) != 0 {
		t.Errorf("满分维不该有归因：%+v", d.Contributors)
	}
}

// 归因条数封顶（报告可读性），但扣分是在全部 offender 上分摊的，所以列出来的
// Penalty 之和小于总扣分——这是有意的，钉住别让人"修"成不封顶。
func TestCompactScore_ContributorsCapped(t *testing.T) {
	snap := &boardSnapshot{Outline: compactRectOutline(20000, 20000)}
	// 一簇 30 件贴在一起（邻距 20mil）——**必须是多数**，否则中位邻距会被孤岛
	// 自己拉高到孤岛的量级，判据就抓不住它们了（中位数是相对基准，不是绝对值）。
	for i := 0; i < 30; i++ {
		snap.Components = append(snap.Components,
			compactSqComp("R"+compactItoa(i+1), pcbSideTop, 100+float64(i)*120, 100, 100))
	}
	// 10 个散落的孤岛，彼此也隔着 2850mil。
	for i := 0; i < 10; i++ {
		snap.Components = append(snap.Components,
			compactSqComp("U"+compactItoa(i+1), pcbSideTop, 6000+float64(i%5)*3000, 8000+float64(i/5)*3000, 150))
	}
	d := runCompactScore(snap, nil)
	if len(d.Contributors) > compactMaxContributors {
		t.Fatalf("归因 %d 条，应封顶 %d", len(d.Contributors), compactMaxContributors)
	}
	if len(d.Contributors) == 0 {
		t.Fatalf("这块超空板必须给归因")
	}
}

// 样本太少时中位邻距是噪声 → 放弃稀疏归因，并在 Reason 里说明，而不是拿假梯度
// 让精修环去挪无辜的件。
func TestCompactScore_TooFewNeighborsNoAttribution(t *testing.T) {
	snap := &boardSnapshot{Outline: compactRectOutline(8000, 8000)}
	snap.Components = append(snap.Components,
		compactSqComp("R1", pcbSideTop, 100, 100, 100),
		compactSqComp("R2", pcbSideTop, 300, 100, 100),
		compactSqComp("U1", pcbSideTop, 7000, 7000, 200))
	d := runCompactScore(snap, nil)
	if len(d.Contributors) != 0 {
		t.Fatalf("样本 %d < %d 时不该给稀疏归因：%+v",
			len(compactNeighborGaps(snap.Components)), compactMinGapPopulation, d.Contributors)
	}
	if !strings.Contains(d.Reason, "中位邻距不可信") {
		t.Errorf("放弃归因必须写明原因，实际 %q", d.Reason)
	}
}

// ---------------------------------------------------------------------------
// 邻距原语
// ---------------------------------------------------------------------------

// 邻距只比**同侧**：双面板上顶/底件在 XY 上重叠是完全正常的，跨面当邻居会把
// 邻距统计压成一片 0，稀疏归因随之全部失灵。
func TestCompactNeighborGaps_SameSideOnly(t *testing.T) {
	comps := []boardComp{
		compactSqComp("U1", pcbSideTop, 0, 0, 100),
		compactSqComp("U2", pcbSideTop, 500, 0, 100), // 同侧邻居，间距 400
		compactSqComp("C1", pcbSideBottom, 10, 10, 50),
	}
	gaps := compactNeighborGaps(comps)
	byDes := map[string]compactGap{}
	for _, g := range gaps {
		byDes[g.designator] = g
	}
	if g, ok := byDes["U1"]; !ok || math.Abs(g.gapMil-400) > 1e-9 || g.neighbor != "U2" {
		t.Fatalf("U1 邻距 = %+v，want 400mil@U2（底面 C1 不算邻居）", g)
	}
	// 底面独苗没有邻距，不该进统计（塞 0 或 +∞ 都会毒死中位数）。
	if _, ok := byDes["C1"]; ok {
		t.Errorf("该侧独苗不该有邻距记录：%+v", byDes["C1"])
	}
}

func TestCompactMedian(t *testing.T) {
	if got := compactMedian(nil); got != 0 {
		t.Errorf("空集中位数 = %v, want 0", got)
	}
	if got := compactMedian([]float64{5, 1, 3}); got != 3 {
		t.Errorf("奇数中位数 = %v, want 3", got)
	}
	if got := compactMedian([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Errorf("偶数中位数 = %v, want 2.5", got)
	}
	// 中位数而非均值：一个极端孤岛不该把基准拉高（拉高了就抓不住它自己）。
	if got := compactMedian([]float64{10, 12, 11, 10, 9, 100000}); got > 20 {
		t.Errorf("离群值污染了中位数：%v", got)
	}
}

// ---------------------------------------------------------------------------
// 契约面
// ---------------------------------------------------------------------------

// 维度必须自注册进打分表，否则 analyzeLayoutScore 会把它填成 "not implemented"。
func TestCompactScorer_Registered(t *testing.T) {
	if s := scorerFor(dimCompact); s == nil {
		t.Fatalf("dimCompact 未注册进 allDimScorers")
	}
	if got := (compactScorer{}).id(); got != dimCompact {
		t.Errorf("id() = %q, want %q", got, dimCompact)
	}
}

// #167 要求的六个原始量必须都在 Metrics 里 —— 只有分数没有原始量时，
// "这维为什么是 62 分"无法回答，也没法拿真板校准拐点。
func TestCompactScore_MetricsContract(t *testing.T) {
	d := runCompactScore(compactGridBoard(2000, 1500, 4, 3, 200, 400), nil)
	for _, k := range []string{
		"boardAreaMil2", "compAreaMil2", "utilization",
		"utilizationTop", "utilizationBottom", "medianNeighborGapMil",
	} {
		if _, ok := d.Metrics[k]; !ok {
			t.Errorf("Metrics 缺 %s：%+v", k, d.Metrics)
		}
	}
	if got := d.Metrics["boardAreaMil2"]; math.Abs(got-3e6) > 1 {
		t.Errorf("boardAreaMil2 = %v, want 3e6", got)
	}
}
