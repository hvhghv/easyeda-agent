package app

// pcb_score_geom.go — layout-score 的两个几何维：**可布性**(dimRoutable) 与
// **装配间距**(dimClearance)。
//
// 这两维不自己算几何。它们消费 ctx.layout（= analyzePcbLayout 的结果，layout-lint
// 的纯核），把它已经算好的 ratsnest / 交叉 / 间距翻译成「分数 + 是谁拉低的」。这
// 条纪律是有代价换来的：这个项目吃过两套引擎长期给矛盾答案的亏（netlist 引擎被坏
// 原语毒死返 0，check 几何引擎照常工作，两边矛盾了很久没人发现）。同一个量只准有
// 一个算法。
//
// ── 与 layout-lint 旧单标量分的三处本质区别 ─────────────────────────────────
//
//  1. **交叉用密度，不用绝对数。** 旧公式 `score -= 4×crossings` 对大板不公平：
//     166 个器件的板天然比 20 个器件的板飞线多、交叉多，同样的布局水准会被判成
//     "very-hard"。真正该被度量的是「每条信号网平均惹上几次跨网交叉」，那是布局
//     水准，与板子规模无关。所以这里除以 SignalNets。
//
//  2. **装配间距反过来用绝对计数。** 这不是不一致，是两种约束的性质不同：交叉数
//     是"布线会有多难"的连续代理量，越大越难；而 0.2mm 的 SMD-SMD 装配间距是
//     **绝对工艺约束**（规范 §3.4），板子大不等于允许挤得更近。密度化会让大板上
//     的违规被稀释掉，正好抹掉最该报的东西。
//
//  3. **分数与门必须同号。** 侦查发现的老矛盾：`evalLayoutGate` 里 tightPairs>0
//     直接判门挂，而旧 score 里一个 tight pair 只扣 1 分 —— 于是出现「score 95
//     分照样 FAIL」这种没法判读的报告（记忆里"计数与判定分离处必查一致性"那条教训
//     的同一个坑）。这里用一个**固定进入代价**修掉：只要越过了装配间距门限，这一
//     维就先扣 sgTightEntryPenalty，把分数直接压到 good 档以下，再按每对的缺口深浅
//     加扣。分数低 ⟺ 门会挂，两者不再打架。
//
// ── 不重复扣分 ─────────────────────────────────────────────────────────────
//
// Overlaps / Shorts / OutsideOutline 已经进了报告的 Blocking（一票否决），这两维
// **绝不再扣它们的分**。否则一处重叠既打 Blocking 又把可布性和装配间距双双打到 0，
// 综合分被同一个缺陷惩罚三次，失真到没法比较两个布局方案。layout-lint 纯核本身也
// 保证了这点：重叠的对走 `continue`，不会再进 TightPairs。
//
// 单位：mil，y-UP。

import (
	"fmt"
	"math"
	"sort"
)

func init() {
	registerDimScorer(sgRoutableScorer{})
	registerDimScorer(sgClearanceScorer{})
}

// ---------------------------------------------------------------------------
// 阈值（每个都写清出处；标"待校准"的等 #167 第五层拿真板标定）
// ---------------------------------------------------------------------------

const (
	// 交叉密度 = 跨网飞线交叉数 / 信号网数。
	//
	// 待校准初值，依据是旧公式的等效点：旧 `-4×crossings` 在 25 处交叉时把分打成
	// 0，而我们实测的板普遍在 15-25 条信号网量级，25/20 ≈ 1.25 —— 取 2.0 作为
	// "满扣"点比旧口径略宽松（旧口径对大板太狠，见文件头 §1）。下限 0.25 的含义
	// 是「四条网里有一条掺进一次交叉」：每处交叉大致等价于一个过孔或一段绕行，
	// 零星几个是任何真实板的常态，不该扣分。
	sgCrossPerNetClean = 0.25
	sgCrossPerNetBad   = 2.0

	// 飞线长度密度 = 总飞线长 / (信号网数 × 参考跨距)。参考跨距取板框对角线。
	//
	// 待校准初值。物理含义：一条信号网的 MST 平均横跨了板对角线的百分之几。摆得
	// 好的板上，一条网的两端通常在同一个功能块内，占板对角线 10-15%；如果平均每
	// 条网都要横跨半块板（0.6），说明相关器件根本没聚在一起。
	sgRatsPerSpanClean = 0.15
	sgRatsPerSpanBad   = 0.60

	// 可布性维 100 分的分配：交叉 60 / 飞线长度 40。
	// 交叉权重更高是因为它直接对应"这里必须打过孔或绕行"这个可布性事实；飞线长度
	// 是更软的聚集度信号（长而不交叉的板仍然布得通，只是走线长）。
	sgCrossBudget = 60.0
	sgRatsBudget  = 40.0

	// 装配间距：进入代价 + 每对代价。见文件头 §3 —— 进入代价存在的唯一目的就是让
	// 「分数」和「门」同号。25 分的取值使得**任何**一处违规都把这一维压到 75 以下
	// （good 档下沿），符合 layout-lint 门"有 tight pair 就 FAIL"的既有口径。
	// 待校准：如果真板上发现 25 分太重（例如高密板普遍有一两处贴装边缘对），调这
	// 个常量而不是回去改门。
	sgTightEntryPenalty = 25.0
	// 每对按缺口深浅加扣的上限（gap=0 时扣满）。待校准初值。
	sgTightPairPenalty = 10.0
	// 每个被四面围死、烙铁进不去的器件（#99）扣多少。比一对间距过近更重：间距近
	// 只是难焊，四面围死是**返修不可能**。待校准初值。
	sgAccessPenalty = 15.0

	// 规范 §3.4：SMD 与 SMD 最小间距 0.2mm ≈ 7.87mil。低于它的 --min-gap 说明调用
	// 方拿电气 clearance（默认 6mil）当装配间距用了，这一维只能抓到"几乎贴住"的对，
	// 必须降级说明而不是假装体检过。
	sgAssemblyGapFloorMil = 7.87

	// 归因列表的截断长度。精修环只需要"先动谁"的前几个；一块 166 器件的板全量列出
	// 上百条归因，人和 agent 都读不动。截断量本身写进 Reason，避免看起来像全集。
	sgMaxContributors = 12
	// findings 截断同理（明细在 layout-lint 里有全量）。
	sgMaxFindings = 10
)

// sgRamp 把一个原始量线性映射到 [0,1] 的扣分比例：≤lo 不扣，≥hi 扣满。
// 用斜坡而不是阶跃，是为了让精修环有梯度可跟 —— 阶跃函数下"改好了一点点"体现不
// 出来，迭代就会在平台期原地打转。
func sgRamp(v, lo, hi float64) float64 {
	if hi <= lo || math.IsNaN(v) {
		return 0
	}
	if v <= lo {
		return 0
	}
	if v >= hi {
		return 1
	}
	return (v - lo) / (hi - lo)
}

// ---------------------------------------------------------------------------
// 可布性维
// ---------------------------------------------------------------------------

type sgRoutableScorer struct{}

func (sgRoutableScorer) id() string { return dimRoutable }

func (sgRoutableScorer) score(ctx *scoreCtx) scoreDimension {
	d := newDimension(dimRoutable, ctx.opts)
	l := ctx.layout
	if l == nil {
		return skipDimension(dimRoutable, ctx.opts,
			"layout core did not run — no ratsnest to measure")
	}
	// 没有信号网 = 没有要布的线。纯机械板 / 只剩电源地的板落在这里。**不能给 100**：
	// 一块什么都不用布的板"可布性满分"是句废话，会污染综合分。
	if l.SignalNets == 0 {
		return skipDimension(dimRoutable, ctx.opts,
			"board has no multi-pad signal net (power/GND are poured, not routed) — routability is not measurable")
	}

	nets := float64(l.SignalNets)
	crossPerNet := float64(l.CrossingCount) / nets
	crossPen := sgRamp(crossPerNet, sgCrossPerNetClean, sgCrossPerNetBad) * sgCrossBudget

	d.Metrics = map[string]float64{
		"signalNets":     nets,
		"crossings":      float64(l.CrossingCount),
		"crossPerNet":    round2(crossPerNet),
		"ratsnestLenMil": l.RatsnestLenMil,
	}

	// 飞线长度要归一化才能跨板比较：除以「信号网数 × 参考跨距」。参考跨距优先用板框
	// 对角线；板框读不到时（PCB 不在前台平台返 null）退回**器件摆放范围**的对角线，
	// 并降级说明 —— 摆放范围会随布局本身变化，用它当分母时"把所有件挤到一角"会同时
	// 缩小分子和分母，指标不再是绝对的板面利用率，只能当相对参考。
	span, spanSource := sgReferenceSpan(ctx)
	budget := sgCrossBudget
	var ratsPen, ratsPerSpan float64
	if span > 0 {
		ratsPerSpan = l.RatsnestLenMil / (nets * span)
		ratsPen = sgRamp(ratsPerSpan, sgRatsPerSpanClean, sgRatsPerSpanBad) * sgRatsBudget
		budget += sgRatsBudget
		d.Metrics["referenceSpanMil"] = round2(span)
		d.Metrics["ratsnestPerNetSpan"] = round2(ratsPerSpan)
	}
	if spanSource != "outline" {
		d.Status = dimDegraded
		if span > 0 {
			d.Reason = "board outline unavailable — ratsnest length normalized by the placement extent instead of the real board diagonal (relative signal only)"
		} else {
			// 连摆放范围都算不出（没有 bbox 也没有坐标）→ 飞线长度这半边彻底测不了，
			// 只按交叉打分，并把 60 分的预算拉伸回 100。这是"少测了一半"的诚实处理：
			// 缺失的半边既不算满分也不算 0，而是退出计分。
			d.Reason = "no board outline and no placement extent — ratsnest length not scored, routability judged on crossing density alone"
		}
	}

	// 把 [0,budget] 的扣分拉伸回 [0,100]。budget<100 时说明有子指标退出计分。
	scale := 100.0 / budget
	d.Score = clampScore(100 - (crossPen+ratsPen)*scale)
	d.Metrics["crossPenalty"] = round2(crossPen * scale)
	d.Metrics["ratsnestPenalty"] = round2(ratsPen * scale)

	// ── 归因 ────────────────────────────────────────────────────────────────
	pen := map[string]float64{}
	why := map[string]string{}
	sgAttributeCrossings(ctx, l, crossPen*scale, pen, why)
	sgAttributeRatsnest(ctx, ratsPen*scale, pen, why)
	d.Contributors = sgContributors(pen, why, &d)

	d.Findings = sgRoutableFindings(l, crossPen, ratsPen, ratsPerSpan)
	return d
}

// sgReferenceSpan 返回归一化飞线长度用的参考跨距（mil）和它的来源。
//
// 优先板框对角线（绝对量，跨板可比）；板框缺失时退回全体器件中心的 AABB 对角线。
func sgReferenceSpan(ctx *scoreCtx) (float64, string) {
	if o := ctx.outline(); o != nil {
		if d := math.Hypot(o.width(), o.height()); d > 0 {
			return d, "outline"
		}
	}
	if ctx.snap == nil {
		return 0, "none"
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	n := 0
	for _, c := range ctx.snap.Components {
		x, y := c.center()
		minX, minY = math.Min(minX, x), math.Min(minY, y)
		maxX, maxY = math.Max(maxX, x), math.Max(maxY, y)
		n++
	}
	if n < 2 {
		return 0, "none"
	}
	if d := math.Hypot(maxX-minX, maxY-minY); d > 0 {
		return d, "placement"
	}
	return 0, "none"
}

// sgAttributeCrossings 把交叉扣分摊回**器件**。
//
// 这是 #167 侦查点名的缺口：crossFinding 只有 netA/netB + 交点坐标，**没有位号**。
// 精修环拿到"NET_SDA 和 NET_SCL 在 (1200,800) 交叉了"没法行动 —— 它要知道的是
// "挪哪个器件能消掉它"。
//
// 反查链路：netMembers() 把网名 → 参与该网的器件位号，再按「离交点多近」在成员里
// 挑嫌疑人。挑几个：交叉的两条飞线各是一条 MST 边，一条边有两个端点，所以每条网
// 取**最近的两个**成员，均分该网的那一半扣分。
//
// 已知局限（写在这里免得下次有人当 bug 修）：真正的 MST 边端点没有被 layout-lint
// 导出，这里是几何最近邻的**重建**，不是真值。密集区里最近邻可能不是真正的边端点。
// 要根治得让 analyzePcbLayout 在 crossFinding 上带出两条边的端点位号——那是纯核的
// 改动，属于共享文件，留给后续单独一版做。
func sgAttributeCrossings(ctx *scoreCtx, l *pcbLayoutReport, total float64, pen map[string]float64, why map[string]string) {
	if total <= 0 || len(l.Crossings) == 0 || ctx.snap == nil {
		return
	}
	members := ctx.snap.netMembers()
	byDes := ctx.snap.byDesignator()
	per := total / float64(len(l.Crossings))
	hits := map[string]int{}
	example := map[string]string{}

	for _, cf := range l.Crossings {
		for _, net := range [2]string{cf.NetA, cf.NetB} {
			cands := sgNearestMembers(members[net], byDes, cf.X, cf.Y, 2)
			if len(cands) == 0 {
				continue // 网上没有可定位的器件（无 bbox 无坐标）——扣分留在总分里，
				// 只是这一份摊不下去，不硬凑一个假归因。
			}
			share := per / 2 / float64(len(cands))
			for _, des := range cands {
				pen[des] += share
				hits[des]++
				if _, seen := example[des]; !seen {
					example[des] = fmt.Sprintf("%s↔%s @(%.0f,%.0f)", cf.NetA, cf.NetB, cf.X, cf.Y)
				}
			}
		}
	}
	for des, n := range hits {
		why[des] = fmt.Sprintf("靠近 %d 处跨网飞线交叉（如 %s）", n, example[des])
	}
}

// sgNearestMembers 在一条网的成员里挑出离 (x,y) 最近的 n 个器件位号。
// 距离用器件 bbox 到点的距离（rectPtDist），没有 bbox 时退回 anchor 点距 —— 交点
// 常常就落在器件轮廓内部，用中心距会把一个大 IC 判得比它旁边的小电阻还远。
func sgNearestMembers(des []string, byDes map[string]boardComp, x, y float64, n int) []string {
	type cand struct {
		des string
		d   float64
	}
	cands := make([]cand, 0, len(des))
	for _, name := range des {
		c, ok := byDes[name]
		if !ok {
			continue
		}
		var dist float64
		if c.BBox != nil {
			dist = rectPtDist(c.BBox.MinX, c.BBox.MinY, c.BBox.MaxX, c.BBox.MaxY, x, y)
		} else {
			cx, cy := c.center()
			dist = math.Hypot(cx-x, cy-y)
		}
		cands = append(cands, cand{name, dist})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].d != cands[j].d {
			return cands[i].d < cands[j].d
		}
		return cands[i].des < cands[j].des // 并列按位号，保证输出确定
	})
	if len(cands) > n {
		cands = cands[:n]
	}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.des)
	}
	return out
}

// sgAttributeRatsnest 把飞线长度扣分摊回器件：按「该器件离它所在各条信号网的焊盘
// 质心有多远」的比例分配。
//
// 为什么是这个量：飞线总长是一个标量，layout-lint 不按网拆分，所以没法直接说"哪条
// 网太长"。但"某个件离它自己那几条网的重心很远"是可直接执行的指令 —— 把它拉回去，
// 飞线就短了。这也正是精修环需要的形式。
func sgAttributeRatsnest(ctx *scoreCtx, total float64, pen map[string]float64, why map[string]string) {
	if total <= 0 || ctx.snap == nil {
		return
	}
	// 信号网的焊盘质心（全局网排除：GND 连着所有东西，它的质心没有布局含义）。
	centroid := map[string][2]float64{}
	for net, pads := range ctx.snap.netPads() {
		if isGlobalNet(net) || len(pads) < 2 {
			continue
		}
		var sx, sy float64
		for _, p := range pads {
			sx, sy = sx+p.X, sy+p.Y
		}
		centroid[net] = [2]float64{sx / float64(len(pads)), sy / float64(len(pads))}
	}
	if len(centroid) == 0 {
		return
	}

	pull := map[string]float64{}
	worstNet := map[string]string{}
	worstDist := map[string]float64{}
	var sum float64
	for _, c := range ctx.snap.Components {
		cx, cy := c.center()
		for _, net := range c.signalNets() {
			ct, ok := centroid[net]
			if !ok {
				continue
			}
			dist := math.Hypot(cx-ct[0], cy-ct[1])
			pull[c.Designator] += dist
			sum += dist
			if dist > worstDist[c.Designator] {
				worstDist[c.Designator], worstNet[c.Designator] = dist, net
			}
		}
	}
	if sum <= 0 {
		return
	}
	for des, p := range pull {
		if p <= 0 {
			continue
		}
		pen[des] += total * p / sum
		note := fmt.Sprintf("离所属信号网重心共 %.0fmil（最远 %s %.0fmil）", p, worstNet[des], worstDist[des])
		if prev, ok := why[des]; ok {
			why[des] = prev + "；" + note
		} else {
			why[des] = note
		}
	}
}

// sgContributors 把扣分表变成排好序、截断过的归因列表。截断时把这件事写进 Reason，
// 否则读者会以为看到的是全集。
func sgContributors(pen map[string]float64, why map[string]string, d *scoreDimension) []scoreContributor {
	out := make([]scoreContributor, 0, len(pen))
	for des, p := range pen {
		// 0.05 以下是分摊出来的浮点尘埃，不是"拉低了这一维"的器件。
		if p < 0.05 || des == "" {
			continue
		}
		out = append(out, scoreContributor{Designator: des, Penalty: round2(p), Detail: why[des]})
	}
	out = sortContributors(out)
	if len(out) > sgMaxContributors {
		hidden := len(out) - sgMaxContributors
		out = out[:sgMaxContributors]
		d.Reason = sgAppendReason(d.Reason,
			fmt.Sprintf("contributors truncated to the worst %d (%d more not shown)", sgMaxContributors, hidden))
	}
	return out
}

// sgAppendReason 拼接多条降级/说明原因（用 "; " 分隔，空的那条直接返回另一条）。
func sgAppendReason(existing, add string) string {
	if existing == "" {
		return add
	}
	if add == "" {
		return existing
	}
	return existing + "; " + add
}

// sgRoutableFindings 出可布性的明细。只在真的扣分时才出 —— 干净的板不该被 INFO
// 噪声淹没（这一维的全量明细 layout-lint 里本来就有）。
func sgRoutableFindings(l *pcbLayoutReport, crossPen, ratsPen, ratsPerSpan float64) []pcbCheckFinding {
	var out []pcbCheckFinding
	if crossPen > 0 && len(l.Crossings) > 0 {
		// 按网对聚合：同一对网反复交叉是一个"热点"，比逐条列 40 个交点有用得多。
		type key struct{ a, b string }
		count := map[key]int{}
		at := map[key]crossFinding{}
		for _, cf := range l.Crossings {
			k := key{cf.NetA, cf.NetB}
			count[k]++
			if _, ok := at[k]; !ok {
				at[k] = cf
			}
		}
		keys := make([]key, 0, len(count))
		for k := range count {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if count[keys[i]] != count[keys[j]] {
				return count[keys[i]] > count[keys[j]]
			}
			if keys[i].a != keys[j].a {
				return keys[i].a < keys[j].a
			}
			return keys[i].b < keys[j].b
		})
		if len(keys) > sgMaxFindings {
			keys = keys[:sgMaxFindings]
		}
		for _, k := range keys {
			cf := at[k]
			out = append(out, pcbCheckFinding{
				Type: "ratline-crossing", Level: "WARN",
				Nets: []string{k.a, k.b},
				At:   &pcbXY{X: cf.X, Y: cf.Y},
				Message: fmt.Sprintf("%s ↔ %s ratlines cross %d× — each crossing costs a via or a detour to route",
					k.a, k.b, count[k]),
			})
		}
	}
	if ratsPen > 0 {
		out = append(out, pcbCheckFinding{
			Type: "ratsnest-sprawl", Level: "WARN",
			Message: fmt.Sprintf("ratsnest averages %.0f%% of the board span per signal net (%.0f%% is tidy) — related parts are not grouped",
				ratsPerSpan*100, sgRatsPerSpanClean*100),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// 装配间距维
// ---------------------------------------------------------------------------

type sgClearanceScorer struct{}

func (sgClearanceScorer) id() string { return dimClearance }

func (sgClearanceScorer) score(ctx *scoreCtx) scoreDimension {
	d := newDimension(dimClearance, ctx.opts)
	l := ctx.layout
	if l == nil {
		return skipDimension(dimClearance, ctx.opts,
			"layout core did not run — no spacing data")
	}
	// 间距是**成对**的量，得有两个能量出轮廓的器件才谈得上。少于两个不是"间距完美"，
	// 是没得测。
	withBBox := sgComponentsWithBBox(ctx)
	if withBBox < 2 {
		return skipDimension(dimClearance, ctx.opts,
			"fewer than 2 components with a rendered bbox (%d) — pairwise assembly spacing is not measurable", withBBox)
	}
	if l.MinGapMil <= 0 {
		return skipDimension(dimClearance, ctx.opts,
			"assembly gap threshold is 0 — the tight-pair check was disabled, nothing was measured")
	}

	d.Metrics = map[string]float64{
		"tightPairs":    float64(len(l.TightPairs)),
		"minGapMil":     l.MinGapMil,
		"accessBlocked": float64(len(l.AccessBlocked)),
		"accessMil":     l.AccessMil,
		"worstGapMil":   round2(sgWorstGap(ctx)),
	}

	// ── 扣分 ────────────────────────────────────────────────────────────────
	// 只吃 TightPairs 和 AccessBlocked。Overlaps/Shorts/OutsideOutline 是 Blocking
	// 的活，这里碰它们就成了同一个缺陷罚三遍。
	pairCost := make([]float64, len(l.TightPairs))
	var pairSum float64
	for i, p := range l.TightPairs {
		// 缺口深浅：gap=0（贴住）扣满，gap 接近门限则接近 0。
		sev := math.Max(0, math.Min(1, (l.MinGapMil-p.Gap)/l.MinGapMil))
		pairCost[i] = sgTightPairPenalty * sev
		pairSum += pairCost[i]
	}
	tightPen := 0.0
	if len(l.TightPairs) > 0 {
		// 固定进入代价：见文件头 §3 —— 让「这一维的分」和「layout-lint 的门」同号。
		tightPen = sgTightEntryPenalty + pairSum
	}
	accessPen := sgAccessPenalty * float64(len(l.AccessBlocked))
	d.Score = clampScore(100 - tightPen - accessPen)
	d.Metrics["tightPenalty"] = round2(tightPen)
	d.Metrics["accessPenalty"] = round2(accessPen)

	// ── 状态与降级 ──────────────────────────────────────────────────────────
	// (a) 手焊通道：AccessBlocked 只在 hand-solder profile 下才有数据，reflow 板上
	//     这个数组恒空。空数组**不等于**通过 —— 必须说明它压根没检查，否则报告读
	//     起来像"手焊也没问题"。
	if l.AccessMil <= 0 {
		d.Status = dimDegraded
		d.Reason = sgAppendReason(d.Reason,
			"hand-solder iron-access corridor NOT checked (needs a hand-solder assembly profile) — this score covers pairwise spacing only")
	}
	// (b) 门限本身太松：默认 minGap 来自电气 clearance（6mil），远低于规范 §3.4 的
	//     SMD-SMD 0.2mm(≈7.9mil) 装配下限。这时"零 tight pair"只证明没有几乎贴住的
	//     对，不证明能装配。
	if l.MinGapMil < sgAssemblyGapFloorMil {
		d.Status = dimDegraded
		d.Reason = sgAppendReason(d.Reason, fmt.Sprintf(
			"gap threshold %.1fmil is below the %.1fmil SMD-to-SMD assembly minimum — only near-touching pairs were caught; pass --min-gap or set a hand-solder profile%s",
			l.MinGapMil, sgAssemblyGapFloorMil, docRule("3.4", "元件间距")))
	}
	// (c) 渲染 bbox 当 courtyard 用：bbox 含丝印，实测比封装本体大 40%+，所以量出来
	//     的间距**偏小**。这个偏差是单向的，于是判读也是单向的：
	//       没测出 tight pair ⇒ 结论可信（保守方向正确，真间距只会更大）；
	//       测出了 tight pair ⇒ 可能是 bbox 撑大造成的误报，必须降级说明。
	//     所以只在有 tight pair 时降级，而不是无条件挂着一个永远为真的 degraded。
	if len(l.TightPairs) > 0 {
		d.Status = dimDegraded
		d.Reason = sgAppendReason(d.Reason,
			"gaps measured on rendered bboxes (silk included), not IPC courtyards — reported gaps are conservative (too small), so a tight pair may be a false positive; verify the worst ones by hand")
	}

	// ── 归因 ────────────────────────────────────────────────────────────────
	pen := map[string]float64{}
	why := map[string]string{}
	// 每对的扣分（含均摊到该对头上的那份进入代价）在两端器件之间对半分：挪走任何
	// 一个都能解决这对，所以两个都是同等嫌疑人。
	for i, p := range l.TightPairs {
		var shareOfEntry float64
		switch {
		case pairSum > 0:
			shareOfEntry = sgTightEntryPenalty * pairCost[i] / pairSum
		default: // 所有对都刚好卡在门限上（sev=0）：进入代价平摊
			shareOfEntry = sgTightEntryPenalty / float64(len(l.TightPairs))
		}
		half := (pairCost[i] + shareOfEntry) / 2
		detail := func(other string) string {
			return fmt.Sprintf("与 %s 间距 %.1fmil < %.1fmil（%s 面）", other, p.Gap, l.MinGapMil, p.Side)
		}
		pen[p.A] += half
		why[p.A] = sgAppendReason(why[p.A], detail(p.B))
		if p.B != "" {
			pen[p.B] += half
			why[p.B] = sgAppendReason(why[p.B], detail(p.A))
		}
	}
	for _, a := range l.AccessBlocked {
		pen[a.Designator] += sgAccessPenalty
		why[a.Designator] = sgAppendReason(why[a.Designator],
			fmt.Sprintf("四面被围，最宽一侧仅 %.1fmil < %.1fmil 烙铁通道", a.BestGap, l.AccessMil))
	}
	d.Contributors = sgContributors(pen, why, &d)

	d.Findings = sgClearanceFindings(l)
	return d
}

// sgComponentsWithBBox 数有渲染 bbox 的器件 —— 没有 bbox 的件根本没进 layout 纯核
// 的成对比较，把它们算进"体检过"是自欺。
func sgComponentsWithBBox(ctx *scoreCtx) int {
	if ctx.snap == nil {
		return 0
	}
	n := 0
	for _, c := range ctx.snap.Components {
		if c.BBox != nil {
			n++
		}
	}
	return n
}

// sgWorstGap 是全板同面器件间的**真实最小间距**（mil）。
//
// 为什么要自己算：layout-lint 只报低于门限的对，门限之上一律不报，所以报告里读不到
// "这块板最紧的地方还剩多少余量"。这个标量对精修环有用（余量 3mil 和余量 30mil 是
// 两种局面），而且它让 worstGapMil 这个指标在零 tight pair 时依然有意义。
// 复用 rectGap + sameAssemblySide，不新造几何。重叠的对 rectGap 返 0（重叠本身由
// Blocking 处理，这里只是让指标如实反映"最紧处为 0"）。
func sgWorstGap(ctx *scoreCtx) float64 {
	if ctx.snap == nil {
		return 0
	}
	type item struct {
		bb    layoutBBox
		layer int
	}
	items := make([]item, 0, len(ctx.snap.Components))
	for _, c := range ctx.snap.Components {
		if c.BBox != nil {
			items = append(items, item{*c.BBox, c.Layer})
		}
	}
	worst := math.Inf(1)
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if !sameAssemblySide(items[i].layer, items[j].layer) {
				continue
			}
			if g := rectGap(items[i].bb, items[j].bb); g < worst {
				worst = g
			}
		}
	}
	if math.IsInf(worst, 1) {
		return 0
	}
	return worst
}

// sgClearanceFindings 出装配间距明细：最紧的几对 + 被围死的器件。
func sgClearanceFindings(l *pcbLayoutReport) []pcbCheckFinding {
	pairs := append([]pcbLFinding(nil), l.TightPairs...)
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].Gap != pairs[j].Gap {
			return pairs[i].Gap < pairs[j].Gap // 最紧的排前面
		}
		return pairs[i].A < pairs[j].A
	})
	if len(pairs) > sgMaxFindings {
		pairs = pairs[:sgMaxFindings]
	}
	var out []pcbCheckFinding
	for _, p := range pairs {
		out = append(out, pcbCheckFinding{
			Type: "assembly-gap", Level: "WARN", Designator: p.A,
			Message: fmt.Sprintf("%s ↔ %s gap %.1fmil < %.1fmil assembly minimum (%s side)%s",
				p.A, p.B, p.Gap, l.MinGapMil, p.Side, docRule("3.4", "元件间距")),
		})
	}
	for _, a := range l.AccessBlocked {
		out = append(out, pcbCheckFinding{
			Type: "solder-access-blocked", Level: "WARN", Designator: a.Designator,
			Message: fmt.Sprintf("%s is boxed in on all four sides (widest %.1fmil < %.1fmil iron corridor) — no way to bring an iron or rework tool in",
				a.Designator, a.BestGap, l.AccessMil),
		})
	}
	return out
}
