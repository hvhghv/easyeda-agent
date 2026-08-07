package app

// pcb_score_partition.go — 功能分区维（#167 dimPartition）。
//
// 这一维回答的问题：**各功能域是不是各自成团、域与域不互相穿插。**
//
// 人类工程师摆板的第一性动作就是分区——电源一块、主控一块、RF 一块、对外口贴边，
// 域之间留出走线通道。差板的典型长相恰好相反：降压模块的电感/续流件散落在 MCU
// 的去耦阵里、RF 匹配件飘到板子另一头。这两种板在既有 `layout-lint` 眼里可以拿
// 完全一样的分数（都没重叠、都没短路），差异被单标量彻底抹平——这正是 #167 说
// 「太粗」的地方。
//
// ── 怎么算 ──────────────────────────────────────────────────────────────────
//
//	1. 模块归属：spec.modules[].parts 优先（设计者的**声明意图**）；没有 spec 就
//	   拿信号网连通度并查集兜底（**推断**，因此整维标 degraded）。
//	2. 交错度（配额 60）：每个模块取「成员 center 的包络」当领地，两两算重叠面积
//	   占较小领地的比例。用 center 而不是成员 bbox 的并集，是因为后者会让相邻模块
//	   的边界天然互相咬合，每块真板都报一堆假交错——我们要测的是「A 的件跑进 B 的
//	   腹地」，不是「A 和 B 挨着」。
//	3. 紧凑度（配额 40）：成员到模块质心的平均距离 / 等面积圆半径。scale-free，
//	   大模块不会因为件多就被判散。
//	4. 归因：闯进别人领地的件 + 离本模块质心最远的件，各自摊到它拉低的分数。
//
// ── 三条硬约定在这里长什么样 ────────────────────────────────────────────────
//
//   - 聚不出模块（板上没有 2..8 成员的信号网 / spec 声明的件一个都没放）→
//     skipDimension，**不是 100**。一块「什么都没测出来」的板绝不能拿满分。
//   - 只有一个模块 / 模块由网聚类推断 / 成员没有渲染 bbox → degraded + Reason，
//     仍参与加权。
//   - 交错和紧凑任何一半测不了时，分数按**可测配额**归一化（见 score() 里的
//     scale），测不了的那半不白送分。

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 阈值 —— 每个都写清出处；没把握的标「待校准」并把原始量丢进 Metrics
// ---------------------------------------------------------------------------

const (
	// partScoreOverlapTolerance —— 两模块领地重叠面积占较小领地的比例，低于它算
	// 正常排布余量，不扣分。
	//
	// 出处：**待校准初值**。人类板的相邻功能域边界本来就会互相探入一点（共用的
	// 去耦、串在两域之间的限流电阻、跨域的滤波磁珠），一刀切「零重叠才满分」会
	// 把公认的好板一起判成差板，那校准判据就废了。0.10 是保守起点，真板校准读
	// Metrics 里的 worstOverlapRatio。
	partScoreOverlapTolerance = 0.10

	// partScoreSpreadTolerance / partScoreSpreadSaturation —— 模块离散度
	// spreadRatio（= 成员到质心的平均距离 ÷ 等面积圆半径）的容忍带。
	//
	// 基准是几何推导的：n 个件肩并肩塞进一个圆盘时，均匀分布的点到质心的平均
	// 距离是 2R/3，算上 ~90% 的圆内填充效率，spreadRatio ≈ 0.7（推导见
	// buildPartScoreModule）。所以 1.5 ≈「模块占了它零件面积 4.6 倍的地方」，
	// 3.0 ≈「18 倍，件已经甩得满板都是」。
	//
	// 基准是推导的，**容忍带是待校准的**：分母用的是渲染 bbox 面积（含丝印，
	// 实测比封装本体大 40%+），所以真实填充率比这里算出来的更松，绝不能照搬
	// KiCad 那套按 IPC courtyard 定的经验值。
	partScoreSpreadTolerance  = 1.5
	partScoreSpreadSaturation = 3.0

	// 分数配额：交错 60 / 紧凑 40。
	//
	// 定这个比例的理由：交错是**分区本身坏了**（电源件插在 MCU 中间，回路和噪声
	// 都跟着串），紧凑只是同一个域摊得开一点，后果轻一个量级。
	partScoreInterleaveBudget = 60.0
	partScoreSpreadBudget     = 40.0

	// 网聚类的扇出窗口 —— 与连接器侧 clusterByLocalNets 同口径
	// （extension/src/actions.ts）：只有 2..8 个成员的信号网才 union。
	//
	// 下界 2：单成员网连不起任何东西。上界 8：总线 / 片选 / 复位这类高扇出网会
	// 把半块板并成一坨，聚出来的「模块」等于整板，判不出任何分区结构。两边必须
	// 同口径，否则 `pcb arrange` 按 A 口径摆完、layout-score 按 B 口径打分，工具
	// 会自己跟自己打架。
	partScoreMinNetFanout = 2
	partScoreMaxNetFanout = 8

	// 归因裁剪：低于 0.05 分的贡献者是浮点噪声，进报告只会把真正的头部淹掉；
	// 精修环也只取前几个，留 12 个足够排序。
	partScoreMinContributor  = 0.05
	partScoreMaxContributors = 12
)

// ---------------------------------------------------------------------------
// 模块模型
// ---------------------------------------------------------------------------

// partScoreModule 是一个待判定的功能模块（成员都是板上真实存在的件）。
type partScoreModule struct {
	Name    string
	Members []boardComp
	Box     layoutBBox // 成员 center 的包络 = 这个模块的「领地」
	CX, CY  float64    // 质心（成员 center 的算术平均）
	Spread  float64    // 成员 center 到质心的平均距离，mil
	RIdeal  float64    // 等面积圆半径；0 = 成员全无 bbox，紧凑度测不了
	known   int        // 有渲染 bbox 的成员数
}

// buildPartScoreModule 把一组成员折成模块的几何摘要。
//
// RIdeal 的来历：把模块全部成员的面积摊平成一个圆，它的半径就是「这些件最紧能
// 占多大地方」的下界。均匀分布在半径 R 圆盘上的点，到圆心的平均距离是 2R/3；
// 再算上圆内 ~90% 的填充效率（R ≈ 1.05·RIdeal），一个塞满的模块 spreadRatio
// ≈ 0.7。拿它当分母，度量就与模块大小无关——20 件的主控域和 3 件的电源域用同一
// 把尺子，不会「件多就判散」。
//
// 部分成员没 bbox 时按有 bbox 的那部分**等比外推**总面积，而不是当 0 处理：当 0
// 会让 RIdeal 偏小、spreadRatio 偏大，凭空冤枉一个其实很紧凑的模块。
func buildPartScoreModule(name string, members []boardComp) partScoreModule {
	m := partScoreModule{Name: name, Members: members}
	n := float64(len(members))
	if n == 0 {
		return m
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	var sumX, sumY, sumArea float64
	for _, c := range members {
		x, y := c.center()
		minX, minY = math.Min(minX, x), math.Min(minY, y)
		maxX, maxY = math.Max(maxX, x), math.Max(maxY, y)
		sumX, sumY = sumX+x, sumY+y
		if c.BBox != nil {
			sumArea += c.area()
			m.known++
		}
	}
	m.Box = layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
	m.CX, m.CY = sumX/n, sumY/n

	var sumDist float64
	for _, c := range members {
		x, y := c.center()
		sumDist += math.Hypot(x-m.CX, y-m.CY)
	}
	m.Spread = sumDist / n

	if m.known > 0 && sumArea > 0 {
		total := sumArea * n / float64(m.known)
		m.RIdeal = math.Sqrt(total / math.Pi)
	}
	return m
}

// partScoreGrouping 是一次模块归属的结果 + 判读它所需的全部降级信息。
type partScoreGrouping struct {
	modules     []partScoreModule // 只含 ≥2 成员的模块，按名字排序（输出确定）
	singletons  int               // 只有 1 件的模块数（无领地可言，不参与判定）
	judged      int               // 进入 ≥2 成员模块的器件数
	specDriven  bool              // true = 来自 spec 声明；false = 网连通度推断
	specMissing int               // spec 声明了但板上没放的位号数
	noBBox      int               // 参与判定的成员里没有渲染 bbox 的件数
}

// partScoreModules 按 spec 优先、网聚类兜底的顺序做模块归属。
func partScoreModules(ctx *scoreCtx) partScoreGrouping {
	if g, ok := partScoreFromSpec(ctx); ok {
		return g
	}
	return partScoreFromNets(ctx.snap)
}

// partScoreFromSpec 用 S0 声明的 modules[].parts 归属。
//
// 返回 false = 「spec 没法用」（没给 spec / 没声明 parts / 声明的件一个都没放到
// 板上），调用方据此退回网聚类，而不是让这一维直接死掉。
func partScoreFromSpec(ctx *scoreCtx) (partScoreGrouping, bool) {
	if !ctx.hasSpec() {
		return partScoreGrouping{}, false
	}
	pm := ctx.spec.PartModule() // key 已经是大写位号
	if len(pm) == 0 {
		return partScoreGrouping{}, false
	}
	buckets := map[string][]boardComp{}
	placed := map[string]bool{}
	for _, c := range ctx.snap.Components {
		key := strings.ToUpper(strings.TrimSpace(c.Designator))
		mod, ok := pm[key]
		if !ok {
			continue // spec 没管这个件：不猜，直接不判（会进 looseParts）
		}
		placed[key] = true
		buckets[partScoreSpecModuleName(mod.Name, mod.KindOf())] = append(buckets[partScoreSpecModuleName(mod.Name, mod.KindOf())], c)
	}
	if len(buckets) == 0 {
		return partScoreGrouping{}, false
	}
	g := finishPartScoreGrouping(buckets)
	g.specDriven = true
	for key := range pm {
		if !placed[key] {
			g.specMissing++
		}
	}
	return g, true
}

// partScoreSpecModuleName 给 spec 模块取一个稳定可读的名字：name 优先，退 kind，
// 最后兜底 "module"（Validate 应该拦掉无名模块，但打分不该因此 panic）。
func partScoreSpecModuleName(name, kind string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	if k := strings.TrimSpace(kind); k != "" {
		return k
	}
	return "module"
}

// partScoreFromNets 用信号网连通度并查集兜底聚类。
//
// **只用 signalNets**：GND / 电源连着半块板，拿它们聚类会把整板并成一坨，聚出来
// 的唯一「模块」等于整板，什么分区结构都判不出来。
func partScoreFromNets(snap *boardSnapshot) partScoreGrouping {
	comps := snap.Components
	parent := make([]int, len(comps))
	for i := range parent {
		parent[i] = i
	}
	find := func(a int) int {
		for parent[a] != a {
			parent[a] = parent[parent[a]] // 路径折半
			a = parent[a]
		}
		return a
	}
	union := func(a, b int) { parent[find(a)] = find(b) }

	netToIdx := map[string][]int{}
	for i, c := range comps {
		for _, n := range c.signalNets() { // signalNets 已去重，一件一网只登记一次
			netToIdx[n] = append(netToIdx[n], i)
		}
	}
	// 按网名排序再 union：并查集的结果与顺序无关，但簇的**代表元**（进而名字和
	// 报告顺序）会随 map 迭代顺序抖动，golden 测试会假失败。
	nets := make([]string, 0, len(netToIdx))
	for n := range netToIdx {
		nets = append(nets, n)
	}
	sort.Strings(nets)
	for _, n := range nets {
		idxs := netToIdx[n]
		if len(idxs) < partScoreMinNetFanout || len(idxs) > partScoreMaxNetFanout {
			continue // 单成员网连不动人；高扇出总线会把整板并成一坨
		}
		for k := 1; k < len(idxs); k++ {
			union(idxs[0], idxs[k])
		}
	}

	roots := map[int][]int{}
	order := make([]int, 0, len(comps))
	for i := range comps {
		r := find(i)
		if _, seen := roots[r]; !seen {
			order = append(order, r)
		}
		roots[r] = append(roots[r], i)
	}
	buckets := map[string][]boardComp{}
	for _, r := range order {
		idxs := roots[r]
		name := partScoreClusterName(comps, idxs)
		for _, i := range idxs {
			buckets[name] = append(buckets[name], comps[i])
		}
	}
	return finishPartScoreGrouping(buckets)
}

// partScoreClusterName 给推断出的簇取名：成员里字典序最小的位号，带 "net:" 前缀
// 明示这是**推断**的模块而不是 spec 声明的（报告里两者不能混为一谈）。
func partScoreClusterName(comps []boardComp, idxs []int) string {
	best := ""
	for _, i := range idxs {
		d := strings.TrimSpace(comps[i].Designator)
		if d == "" {
			d = comps[i].ID
		}
		if d == "" {
			continue
		}
		if best == "" || d < best {
			best = d
		}
	}
	if best == "" {
		best = fmt.Sprintf("#%d", idxs[0])
	}
	return "net:" + best
}

// finishPartScoreGrouping 把桶折成排序好的模块列表，并统计降级所需的计数。
func finishPartScoreGrouping(buckets map[string][]boardComp) partScoreGrouping {
	var g partScoreGrouping
	names := make([]string, 0, len(buckets))
	for n := range buckets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		members := buckets[n]
		if len(members) < 2 {
			// 一个点没有领地，也没有「内部离散度」——不是它好，是它测不了。
			g.singletons++
			continue
		}
		m := buildPartScoreModule(n, members)
		g.judged += len(members)
		g.noBBox += len(members) - m.known
		g.modules = append(g.modules, m)
	}
	return g
}

// ---------------------------------------------------------------------------
// 交错度
// ---------------------------------------------------------------------------

// partScorePair 是一对交错的模块（excess 已过容忍带）。
type partScorePair struct {
	a, b      int
	ratio     float64  // 原始重叠比例（进 Metrics / finding 文案）
	excess    float64  // 过容忍带后归一到 [0,1] 的实际扣分权重
	intruders []string // 闯进对方领地的位号（归因对象）
	cx, cy    float64  // 交叠区中心（finding 的定位点）
}

// partScoreInterleave 算一对模块的交错程度 [0,1]，顺带给出归因用的闯入者。
func partScoreInterleave(a, b partScoreModule) (ratio float64, intruders []string) {
	areaA := (a.Box.MaxX - a.Box.MinX) * (a.Box.MaxY - a.Box.MinY)
	areaB := (b.Box.MaxX - b.Box.MinX) * (b.Box.MaxY - b.Box.MinY)
	ox, oy, ov := overlapExtent(a.Box, b.Box)

	// 闯入者 = 中心落在**对方**领地里的件。它既是归因对象，也是退化模块唯一还
	// 能用的判据（见下面的 fallback）。
	for _, c := range a.Members {
		if x, y := c.center(); partScorePointInBox(x, y, b.Box) {
			intruders = append(intruders, c.Designator)
		}
	}
	for _, c := range b.Members {
		if x, y := c.center(); partScorePointInBox(x, y, a.Box) {
			intruders = append(intruders, c.Designator)
		}
	}

	switch {
	case ov:
		// ov 为真意味着两个方向都有正重叠，因此两块领地面积都 > 0，除法安全。
		ratio = ox * oy / math.Min(areaA, areaB)
	case areaA <= 0 || areaB <= 0:
		// 退化模块（成员共线，领地有一边宽度为 0——两颗电容排成一行就是这样，
		// 真板上极常见）。此时 overlapExtent 对「这条线整个躺在对方腹地里」恒
		// 判不重叠（oy==0），面积比是死路。改用闯入比例兜底，否则最刺眼的那种
		// 交错反而检不出来。
		n := math.Min(float64(len(a.Members)), float64(len(b.Members)))
		if n > 0 {
			ratio = float64(len(intruders)) / n
		}
	}
	ratio = partScoreClamp01(ratio)
	if ratio <= 0 {
		intruders = nil
	}
	return ratio, partScoreUniqSorted(intruders)
}

// partScorePointInBox 是闭区间的点在矩形内判定（边界算在内：正好压在领地边线上
// 的件确实已经踩进对方地盘了）。
func partScorePointInBox(x, y float64, b layoutBBox) bool {
	return x >= b.MinX && x <= b.MaxX && y >= b.MinY && y <= b.MaxY
}

// partScoreNearest 返回模块里离给定点最近的成员位号。归因兜底用：两块领地只在
// 角上擦碰、没有任何成员中心落进对方腹地时，总得有人为这次交错负责。
func partScoreNearest(m partScoreModule, x, y float64) string {
	best, bestD := "", math.Inf(1)
	for _, c := range m.Members {
		cx, cy := c.center()
		if d := math.Hypot(cx-x, cy-y); d < bestD {
			best, bestD = c.Designator, d
		}
	}
	return best
}

func partScoreClamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

func partScoreUniqSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 打分器
// ---------------------------------------------------------------------------

type partitionScorer struct{}

func init() { registerDimScorer(partitionScorer{}) }

func (partitionScorer) id() string { return dimPartition }

func (partitionScorer) score(ctx *scoreCtx) scoreDimension {
	opts := ctx.opts
	if ctx.snap == nil || len(ctx.snap.Components) < 2 {
		return skipDimension(dimPartition, opts,
			"fewer than 2 components on the board — there is nothing to partition")
	}

	g := partScoreModules(ctx)
	if len(g.modules) == 0 {
		// 聚不出任何 ≥2 件的模块。**这是「没测」不是「满分」**：一块摆得一团糟
		// 但恰好没有 2..8 成员信号网的板，绝不能因为测不了就拿 100 分。
		if g.specDriven {
			return skipDimension(dimPartition, opts,
				"spec declares modules but none has ≥2 parts placed on this board (%d declared designator(s) missing)",
				g.specMissing)
		}
		return skipDimension(dimPartition, opts,
			"no functional module could be inferred: no signal net links %d..%d components "+
				"(GND/power nets are excluded on purpose — they would merge the whole board into one blob); "+
				"pass --spec with modules[].parts to score this dimension",
			partScoreMinNetFanout, partScoreMaxNetFanout)
	}

	d := newDimension(dimPartition, opts)
	var degraded []string
	if !g.specDriven {
		degraded = append(degraded, fmt.Sprintf(
			"modules inferred from signal-net connectivity (no spec modules[].parts) — cluster boundaries are a heuristic, not the designer's declared intent; %d cluster(s) formed",
			len(g.modules)))
	}
	if g.noBBox > 0 {
		degraded = append(degraded, fmt.Sprintf(
			"%d module member(s) have no rendered bbox — their position falls back to the placement anchor and their area is extrapolated",
			g.noBBox))
	}

	// ── 交错度 ──────────────────────────────────────────────────────────────
	interleavable := len(g.modules) >= 2
	var pairs []partScorePair
	var sumExcess, worstRatio float64
	if interleavable {
		for i := 0; i < len(g.modules); i++ {
			for j := i + 1; j < len(g.modules); j++ {
				ratio, intruders := partScoreInterleave(g.modules[i], g.modules[j])
				worstRatio = math.Max(worstRatio, ratio)
				e := partScoreClamp01((ratio - partScoreOverlapTolerance) / (1 - partScoreOverlapTolerance))
				if e <= 0 {
					continue
				}
				cx, cy := partScoreOverlapCenter(g.modules[i].Box, g.modules[j].Box)
				pairs = append(pairs, partScorePair{
					a: i, b: j, ratio: ratio, excess: e, intruders: intruders, cx: cx, cy: cy,
				})
				sumExcess += e
			}
		}
	} else {
		degraded = append(degraded,
			"only 1 module was identified — there is no cross-module interleaving to judge; this score reflects intra-module compactness only")
	}
	// 一对彻底套在一起的模块就把交错配额吃满：分区已经不成立了，再叠更多对也不
	// 会更坏，min(1,·) 保证分数不被负溢出。
	interleavePenalty := partScoreInterleaveBudget * math.Min(1, sumExcess)

	// ── 紧凑度 ──────────────────────────────────────────────────────────────
	type modSpread struct {
		idx    int
		ratio  float64
		excess float64
	}
	var spreads []modSpread
	var wSum, wExcess, spreadMilSum, spreadRatioSum float64
	for i, m := range g.modules {
		spreadMilSum += m.Spread
		if m.RIdeal <= 0 {
			continue // 成员全无 bbox：没有面积就没有参照尺度，测不了
		}
		r := m.Spread / m.RIdeal
		e := partScoreClamp01((r - partScoreSpreadTolerance) / (partScoreSpreadSaturation - partScoreSpreadTolerance))
		// 按成员数加权：20 件的主控域摊开，比 2 件的分压电阻摊开严重得多。
		w := float64(len(m.Members))
		wSum += w
		wExcess += w * e
		spreadRatioSum += r
		spreads = append(spreads, modSpread{idx: i, ratio: r, excess: e})
	}
	compactable := wSum > 0
	var spreadPenalty float64
	if compactable {
		spreadPenalty = partScoreSpreadBudget * wExcess / wSum
	} else {
		degraded = append(degraded,
			"no module member has a rendered bbox — intra-module compactness needs component areas and could not be computed")
	}

	// ── 归一化：测不了的那半不白送分 ────────────────────────────────────────
	budget := 0.0
	if interleavable {
		budget += partScoreInterleaveBudget
	}
	if compactable {
		budget += partScoreSpreadBudget
	}
	if budget <= 0 {
		return skipDimension(dimPartition, opts,
			"a single module whose members have no rendered bbox — neither interleaving nor compactness is measurable")
	}
	scale := 100 / budget // 把「可测配额」拉回 0-100 尺度
	d.Score = clampScore(100 - (interleavePenalty+spreadPenalty)*scale)

	// ── 归因 ────────────────────────────────────────────────────────────────
	acc := map[string]*scoreContributor{}
	blame := func(des string, penalty float64, detail string) {
		if des == "" || penalty <= 0 {
			return
		}
		c := acc[des]
		if c == nil {
			c = &scoreContributor{Designator: des}
			acc[des] = c
		}
		c.Penalty += penalty
		if detail != "" && !strings.Contains(c.Detail, detail) {
			if c.Detail == "" {
				c.Detail = detail
			} else {
				c.Detail += "; " + detail
			}
		}
	}

	for _, p := range pairs {
		ma, mb := g.modules[p.a], g.modules[p.b]
		// 这一对在总交错扣分里占的份额（sumExcess 可能被 min(1,·) 截断，按比例摊）。
		share := interleavePenalty * (p.excess / sumExcess) * scale
		culprits := p.intruders
		if len(culprits) == 0 {
			// 角上擦碰：没人真的闯进腹地，抓两边各自离交叠区最近的件顶上——
			// 交错是**成对**的性质，只怪一边会让精修环把好的那边也挪了。
			culprits = partScoreUniqSorted([]string{
				partScoreNearest(ma, p.cx, p.cy),
				partScoreNearest(mb, p.cx, p.cy),
			})
		}
		if len(culprits) == 0 {
			continue
		}
		per := share / float64(len(culprits))
		for _, des := range culprits {
			blame(des, per, fmt.Sprintf("straddles the %s ↔ %s module boundary", ma.Name, mb.Name))
		}
		d.Findings = append(d.Findings, pcbCheckFinding{
			Type:       "module-interleave",
			Level:      "WARN",
			Designator: culprits[0],
			Primitives: culprits,
			At:         &pcbXY{X: round2(p.cx), Y: round2(p.cy)},
			Message: fmt.Sprintf(
				"modules %s and %s interleave: their member envelopes overlap by %.0f%% of the smaller one (offenders: %s) — keep each functional block in its own territory%s",
				ma.Name, mb.Name, p.ratio*100, strings.Join(culprits, ", "),
				docRule("3.3", "模拟 / 数字分区")),
		})
	}

	for _, s := range spreads {
		if s.excess <= 0 {
			continue
		}
		m := g.modules[s.idx]
		share := partScoreSpreadBudget * float64(len(m.Members)) * s.excess / wSum * scale
		// 只怪离质心比平均还远的那些件——把扣分平摊到整个模块，会让精修环去动
		// 那些本来就摆在窝里的件。
		var far []boardComp
		var farSum float64
		for _, c := range m.Members {
			x, y := c.center()
			if dist := math.Hypot(x-m.CX, y-m.CY); dist > m.Spread {
				far = append(far, c)
				farSum += dist
			}
		}
		if len(far) == 0 { // 成员到质心等距（正多边形排布）：只能平摊
			far = m.Members
			farSum = 0
		}
		for _, c := range far {
			x, y := c.center()
			dist := math.Hypot(x-m.CX, y-m.CY)
			w := 1 / float64(len(far))
			if farSum > 0 {
				w = dist / farSum
			}
			blame(c.Designator, share*w, fmt.Sprintf("%.0fmil from the %s centroid", dist, m.Name))
		}
		d.Findings = append(d.Findings, pcbCheckFinding{
			Type:       "module-spread",
			Level:      "WARN",
			Designator: m.Name,
			At:         &pcbXY{X: round2(m.CX), Y: round2(m.CY)},
			Message: fmt.Sprintf(
				"module %s is smeared: its %d members sit %.0fmil from the centroid on average, %.1f× the radius their own footprints need — pull the block together%s",
				m.Name, len(m.Members), m.Spread, s.ratio,
				docRule("3.3", "模拟 / 数字分区")),
		})
	}

	cs := make([]scoreContributor, 0, len(acc))
	for _, c := range acc {
		if c.Penalty < partScoreMinContributor {
			continue // 浮点噪声，进报告只会淹掉真正的头部
		}
		c.Penalty = round2(c.Penalty)
		cs = append(cs, *c)
	}
	cs = sortContributors(cs)
	if len(cs) > partScoreMaxContributors {
		cs = cs[:partScoreMaxContributors]
	}
	d.Contributors = cs

	// ── 原始量 —— 没有它们，「这维为什么 62 分」无法回答，也没法拿真板校准阈值
	meanSpreadRatio := 0.0
	if len(spreads) > 0 {
		meanSpreadRatio = spreadRatioSum / float64(len(spreads))
	}
	d.Metrics = map[string]float64{
		"moduleCount":       float64(len(g.modules)), // 参与判定的（≥2 成员）
		"singletonModules":  float64(g.singletons),
		"interleavedPairs":  float64(len(pairs)),
		"worstOverlapRatio": round2(worstRatio),
		"meanSpreadMil":     round2(spreadMilSum / float64(len(g.modules))),
		"meanSpreadRatio":   round2(meanSpreadRatio),
		"judgedParts":       float64(g.judged),
		"looseParts":        float64(len(ctx.snap.Components) - g.judged),
		"specDriven":        partScoreBool(g.specDriven),
		"specMissingParts":  float64(g.specMissing),
		"interleavePenalty": round2(interleavePenalty * scale),
		"spreadPenalty":     round2(spreadPenalty * scale),
	}

	// 模块粒度过粗的自曝（真板校准发现，车机V2 166 器件 / 5 页原理图）：
	// 拿**原理图分页**当模块喂进来时，5 个模块的 10 对领地全部交错、worstOverlapRatio
	// 打满 1.0，本维掉到 33 分 —— 但那块板的 flow-order 是满分，大格局明明是对的。
	//
	// 原因是粒度错配：原理图页是**画图的组织单位**，一页 20-50 个器件在一块 85×45mm
	// 的密板上必然交织；#167 说的「功能分区」是设计者在**板面上**有意划出的域。
	// 这一维测不出「该分几个区」，只能测「你声明的区互不互相穿插」——所以当所有模块对
	// 都交错时，更可能是模块粒度给粗了，而不是板子摆坏了。把这句话说出来，
	// 免得用户对着一个 33 分去重排一块布局本来合理的板。
	if interleavable && len(pairs) == len(g.modules)*(len(g.modules)-1)/2 && worstRatio >= 0.99 {
		degraded = append(degraded, fmt.Sprintf(
			"every one of the %d module pairs interleaves at ~100%% — that usually means the module granularity is too coarse (e.g. schematic PAGES fed in as modules) rather than a badly placed board; declare finer functional domains via `pcb zones set` or spec modules[] and re-score",
			len(pairs)))
	}

	if len(degraded) > 0 {
		d.Status = dimDegraded
		d.Reason = strings.Join(degraded, "; ")
	}
	return d
}

// partScoreOverlapCenter 是两块领地交叠区的中心（不重叠时退化成两框中心的中点，
// finding 的定位点总要有个值）。
func partScoreOverlapCenter(a, b layoutBBox) (float64, float64) {
	loX, hiX := math.Max(a.MinX, b.MinX), math.Min(a.MaxX, b.MaxX)
	loY, hiY := math.Max(a.MinY, b.MinY), math.Min(a.MaxY, b.MaxY)
	if loX <= hiX && loY <= hiY {
		return (loX + hiX) / 2, (loY + hiY) / 2
	}
	return (a.MinX + a.MaxX + b.MinX + b.MaxX) / 4, (a.MinY + a.MaxY + b.MinY + b.MaxY) / 4
}

func partScoreBool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
