package app

// pcb_score_compact.go — 紧凑度维（dimCompact）：板面利用率的**双侧**评分。
//
// #167 原话：「紧凑不真撞 —— 板内面积 / courtyard 总面积（>3=太空）+ 真实焊盘
// 重叠=0（DRC 清）」。人类工程师摆的板紧凑但不真撞：courtyard 相擦而已。
//
// ── 这一维最大的坑：这个项目根本没有 courtyard ────────────────────────────────
//
// 全仓唯一的器件包络是 `getPrimitivesBBox` 的**渲染包围盒**——它把丝印外框、位号
// 文字一起算进去，skill 文档自述「常比封装本体大 40%+」。它不是 IPC-7351 的
// courtyard（courtyard = 本体 + 装配余量，是个工艺量；渲染 bbox 是个绘图量）。
// 两个后果，都必须写在脸上而不是埋在代码里：
//
//  1. **分母用 bbox 面积的密度指标系统性偏大。** 同一块板，courtyard 口径算出 45%
//     的利用率，bbox 口径能算到 60%+。所以 #167 那个「板面/courtyard > 3 = 太空」
//     （等价于利用率 < 33%）的阈值**不能照搬**——照搬会把一堆正常板判成"太挤"。
//     本文件的拐点是拿 courtyard 口径的经验区间 × 实测膨胀系数**推**出来的初值
//     （见 compactPlateauLo/Hi 的注释），全部标记为待校准，原始量进 Metrics，
//     等 #167 第五层拿真板校准。
//
//  2. **人类眼里「courtyard 相擦而已」的好板，用渲染 bbox 判会被 layout-lint 直接
//     判成 overlap。** 那正是紧凑好板的常态：两个 0402 的丝印框互相压一点点。
//     所以这一维**绝不重复惩罚**已经进 Blocking 的重叠/短路——真碰撞由
//     `layout-lint` 纯核算完塞进 rep.Blocking 一票否决，这里只在 Reason 里提一句
//     "有 N 处硬碰撞，见 blocking"。若这里也扣一遍，紧凑板会被同一件事罚两次，
//     刚好把 #167 想区分的「紧凑」和「乱撞」又搅回一起。
//
// 因为 courtyard 数据今天压根不存在，这一维的 Status **恒为 degraded**（不是
// skipped——它算得出有意义的相对量，只是绝对刻度不可信）。哪天连接器能给出真
// courtyard，把 compAreaOf 换掉、把 degraded 的第一条理由删掉即可。
//
// 单位：面积 mil²，距离 mil，y-UP。

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

func init() { registerDimScorer(compactScorer{}) }

// ---------------------------------------------------------------------------
// 阈值（全部是**待校准初值**）
// ---------------------------------------------------------------------------

// 利用率评分是个梯形：中间一段平台给满分，两侧各自线性下滑到地板分。
//
// 拐点怎么来的（这是"可解释"的全部内容，别当成实测值）：
//
//	courtyard 口径的经验区间 —— 布线通道够、又不至于浪费板面 —— 大约是
//	15% ~ 45%（业内手册与商业板目测的粗略共识，本仓无实测）。
//	本仓只有渲染 bbox，实测面积膨胀 ≈1.4×（"大 40%+"）。
//	  0.15 × 1.4 ≈ 0.20  → compactPlateauLo
//	  0.45 × 1.4 ≈ 0.60  → compactPlateauHi   （与 #167 提的 60% 口径也对得上）
//
// 两端的"扣到底"点取平台外再走一倍距离左右，纯经验，同样待校准。
const (
	compactSparseFloor = 0.06 // 利用率 ≤6%：板子基本是空的，稀疏侧扣到底（待校准）
	compactPlateauLo   = 0.20 // 稀疏侧拐点：低于此开始扣分（待校准，见上文推导）
	compactPlateauHi   = 0.60 // 拥挤侧拐点：高于此开始扣分（待校准，见上文推导）
	compactDenseCeil   = 0.85 // 利用率 ≥85%：几乎没有布线通道，拥挤侧扣到底（待校准）
)

// 两侧的地板分**故意不对称**：
//   - 太空只是浪费板费、走线绕远，是经济与效率问题，板子照样能造 → 地板 25 分。
//   - 太挤是工程后果：没有布线通道、没有钢网间距、没有返修空间，往往做不出来
//     → 地板 10 分，罚得更重。
//
// 都不给 0：0 分应该留给"真的错了"，而利用率再极端也只是"不该这么摆"。
const (
	compactSparseMinScore = 25.0
	compactDenseMinScore  = 10.0
)

// 稀疏归因（"周围空得离谱"）的判据：到最近邻的间距同时超过
//   - 中位间距的 compactSparseGapFactor 倍（相对判据：这块板自己的尺度）；
//   - compactSparseGapFloorMil 绝对下限（防止超密板上把 60mil 判成"空得离谱"——
//     中位数 20mil 时 3× 才 60mil，那不叫空）。
//
// 两个数都是待校准初值。250mil ≈ 6.3mm，是"肉眼一看就是块空地"的量级。
const (
	compactSparseGapFactor   = 3.0
	compactSparseGapFloorMil = 250.0
)

// compactMinGapPopulation 是算中位间距所需的最小样本数。样本太少时中位数就是
// 噪声（3 个件里挑中位数毫无统计意义），此时放弃稀疏归因并在 Reason 里说明——
// 宁可不给梯度，也不给一个假梯度让精修环去挪无辜的件。
const compactMinGapPopulation = 6

// compactMaxContributors 限制归因条数：精修环一轮只会动前几个，列 50 条既没人
// 读也拖慢报告。注意扣分是在**全部**offender 上按比例分的，只是报告里截断，
// 所以列出来的 Penalty 之和会小于本维总扣分——这是有意的。
const compactMaxContributors = 8

// ---------------------------------------------------------------------------
// 打分
// ---------------------------------------------------------------------------

type compactScorer struct{}

func (compactScorer) id() string { return dimCompact }

func (compactScorer) score(ctx *scoreCtx) scoreDimension {
	if ctx == nil {
		return skipDimension(dimCompact, layoutScoreOpts{}, "没有打分上下文")
	}
	d := newDimension(dimCompact, ctx.opts)

	if ctx.snap == nil || len(ctx.snap.Components) == 0 {
		return skipDimension(dimCompact, ctx.opts, "板上没有器件，无法计算板面利用率")
	}
	out := ctx.outline()
	if out == nil {
		// 板框读不到（PCB 不在前台时平台返 null）。没有分母就没有利用率——
		// 这里返 100 等于说"一块看不见的板紧凑度满分"，正是约定禁止的事。
		return skipDimension(dimCompact, ctx.opts,
			"板框不可用（PCB 是否为前台文档？），没有分母算不了板面利用率")
	}
	boardArea := out.area()
	if boardArea <= 0 {
		return skipDimension(dimCompact, ctx.opts, "板框面积为 0，无法计算板面利用率")
	}

	// ── 面积统计：按装配面分开 ────────────────────────────────────────────
	//
	// 为什么不把两面的件面积简单相加：底面的件**不占顶面的空间**，两面各自拥有
	// 一整块板面。相加会让一块顶面 55%、底面 10% 的板算出 65%（判成"太挤"），
	// 而它顶面其实还有通道、底面几乎全空。
	//
	// 取两面的**较大者**而不是加权平均：布线通道是逐面的，最挤的那一面决定了这
	// 块板好不好布；平均会让空荡的底面把顶面的拥挤稀释掉，正好抹掉我们要测的东西。
	var areaTop, areaBottom float64
	var noBBox, unknownSide int
	sized := make([]boardComp, 0, len(ctx.snap.Components))
	for _, c := range ctx.snap.Components {
		if c.BBox == nil {
			// 没有渲染 bbox 就没有面积可算。不猜一个默认封装尺寸——猜出来的面积
			// 会直接污染利用率这个唯一的输出量。
			noBBox++
			continue
		}
		sized = append(sized, c)
		if c.Layer == pcbSideBottom {
			areaBottom += c.area()
			continue
		}
		if c.Layer != pcbSideTop {
			// 层未知（连接器没给 layer）。顶面是默认装配面，归顶面比丢弃更接近
			// 事实，但要计数暴露出来。
			unknownSide++
		}
		areaTop += c.area()
	}
	if len(sized) == 0 {
		return skipDimension(dimCompact, ctx.opts,
			"%d 个器件全都没有渲染 bbox，量不到占用面积", len(ctx.snap.Components))
	}

	utilTop := areaTop / boardArea
	utilBottom := areaBottom / boardArea
	util := math.Max(utilTop, utilBottom)

	d.Score = clampScore(compactUtilScore(util))

	// ── 邻距统计（稀疏归因的原料，同侧才算） ─────────────────────────────
	gaps := compactNeighborGaps(sized)
	medianGap := compactMedian(compactGapValues(gaps))

	d.Metrics = map[string]float64{
		"boardAreaMil2":         round2(boardArea),
		"compAreaMil2":          round2(areaTop + areaBottom),
		"utilization":           round2(util),
		"utilizationTop":        round2(utilTop),
		"utilizationBottom":     round2(utilBottom),
		"medianNeighborGapMil":  round2(medianGap),
		"componentsWithoutBBox": float64(noBBox),
		"componentsUnknownSide": float64(unknownSide),
		// 真碰撞只**报数**不扣分，见文件头第 2 条。放进 Metrics 是为了让判读的人
		// 一眼看到"这维 92 分"和"板上有 3 处重叠"并不矛盾。
		"hardCollisions": float64(len(compactHardCollisions(ctx))),
	}

	// ── 降级说明（这一维今天恒为 degraded） ───────────────────────────────
	var reasons []string
	reasons = append(reasons,
		"无 courtyard 数据：用渲染 bbox（含丝印，实测比本体大 40%+）当包络，"+
			"利用率系统性偏高，拐点为待校准初值")
	if out.Source != "polygon" {
		// 异形板（Type-C 突出、切角、挖槽）的 AABB 面积明显大于真板面积，
		// 分母偏大 → 利用率偏低 → 会被误判成"太空"。
		reasons = append(reasons, "板框只有 AABB 近似（无多边形点列），异形板的板面积偏大、利用率偏低")
	}
	if noBBox > 0 {
		reasons = append(reasons,
			fmt.Sprintf("%d/%d 个器件没有渲染 bbox，其占用面积未计入（利用率偏低）",
				noBBox, len(ctx.snap.Components)))
	}
	if unknownSide > 0 {
		reasons = append(reasons, fmt.Sprintf("%d 个器件层未知，按顶面统计", unknownSide))
	}
	if n := len(compactHardCollisions(ctx)); n > 0 {
		reasons = append(reasons,
			fmt.Sprintf("存在 %d 处硬碰撞（重叠/短路），已单列 blocking，本维不重复扣分", n))
	}
	d.Status = dimDegraded
	d.Reason = strings.Join(reasons, "；")

	// ── 归因 ──────────────────────────────────────────────────────────────
	penalty := 100 - d.Score
	if penalty > 0 {
		if util < compactPlateauLo {
			d.Contributors, d.Findings = compactSparseAttribution(gaps, medianGap, penalty, util)
			if len(d.Contributors) == 0 && len(gaps) < compactMinGapPopulation {
				d.Reason += fmt.Sprintf("；有邻居的器件只有 %d 个（<%d），中位邻距不可信，本维不给稀疏归因",
					len(gaps), compactMinGapPopulation)
			}
		} else {
			d.Contributors, d.Findings = compactDenseAttribution(sized, boardArea, penalty, util)
		}
	}
	return d
}

// compactUtilScore 是利用率 → 分数的梯形曲线（拐点见上方常量）。
//
// 双侧的理由：太空和太挤都是布局缺陷，但方向相反——
//
//	太空：板费白花，网络绕远路，EMI 回路变大；
//	太挤：没有布线通道、没有返修余量，往往根本布不通。
//
// 单调递减的"越紧凑越好"曲线测不出前者，单调递增的"越松越好"测不出后者。
func compactUtilScore(u float64) float64 {
	switch {
	case u >= compactPlateauLo && u <= compactPlateauHi:
		return 100
	case u < compactPlateauLo:
		if u <= compactSparseFloor {
			return compactSparseMinScore
		}
		t := (u - compactSparseFloor) / (compactPlateauLo - compactSparseFloor)
		return compactSparseMinScore + t*(100-compactSparseMinScore)
	default:
		if u >= compactDenseCeil {
			return compactDenseMinScore
		}
		t := (u - compactPlateauHi) / (compactDenseCeil - compactPlateauHi)
		return 100 - t*(100-compactDenseMinScore)
	}
}

// ---------------------------------------------------------------------------
// 邻距
// ---------------------------------------------------------------------------

// compactGap 是一个器件到它最近同侧邻居的边到边间距。
type compactGap struct {
	designator string
	neighbor   string
	gapMil     float64
}

func compactGapValues(gs []compactGap) []float64 {
	out := make([]float64, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.gapMil)
	}
	return out
}

// compactNeighborGaps 给每个有 bbox 的器件算「到最近同侧邻居的 bbox 边距」。
//
// 只比同侧：底面的件和顶面的件在 XY 上重叠是完全正常的（板子有两面），拿它们
// 互相当邻居会把双面板的邻距统计压成一片 0。层未知的件按顶面处理，与面积统计
// 保持同一口径。
//
// 孤件（该侧只有它一个）不进统计：它没有"邻距"这个量，塞个 +∞ 或 0 都会毒死中位数。
func compactNeighborGaps(comps []boardComp) []compactGap {
	side := func(c boardComp) int {
		if c.Layer == pcbSideBottom {
			return pcbSideBottom
		}
		return pcbSideTop
	}
	out := make([]compactGap, 0, len(comps))
	for i, c := range comps {
		best := math.Inf(1)
		bestName := ""
		for j, o := range comps {
			if i == j || side(c) != side(o) {
				continue
			}
			g := rectGap(*c.BBox, *o.BBox)
			if g < best {
				best, bestName = g, o.Designator
			}
		}
		if math.IsInf(best, 1) {
			continue // 该侧独苗，没有邻距
		}
		out = append(out, compactGap{designator: c.Designator, neighbor: bestName, gapMil: best})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].designator < out[j].designator })
	return out
}

// compactMedian 是中位数（空集返回 0）。用中位数而不是均值：一两个被扔到角落的
// 孤岛件会把均值拉高，于是"远超均值"的判据反而抓不住它们自己。
func compactMedian(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// ---------------------------------------------------------------------------
// 归因
// ---------------------------------------------------------------------------

// compactSparseAttribution 找「周围空得离谱」的器件——它们是可以往板内收紧的对象。
//
// 扣分按「超出阈值的量」按比例分摊：离得越离谱，分摊越多，精修环排序后先动它。
func compactSparseAttribution(gaps []compactGap, medianGap, penalty, util float64) ([]scoreContributor, []pcbCheckFinding) {
	if len(gaps) < compactMinGapPopulation || medianGap <= 0 {
		return nil, nil
	}
	threshold := math.Max(medianGap*compactSparseGapFactor, compactSparseGapFloorMil)

	type offender struct {
		g      compactGap
		excess float64
	}
	var offs []offender
	var total float64
	for _, g := range gaps {
		if g.gapMil <= threshold {
			continue
		}
		e := g.gapMil - threshold
		offs = append(offs, offender{g: g, excess: e})
		total += e
	}
	if len(offs) == 0 || total <= 0 {
		return nil, nil
	}
	sort.SliceStable(offs, func(i, j int) bool {
		if offs[i].excess != offs[j].excess {
			return offs[i].excess > offs[j].excess
		}
		return offs[i].g.designator < offs[j].g.designator
	})

	cs := make([]scoreContributor, 0, len(offs))
	for _, o := range offs {
		cs = append(cs, scoreContributor{
			Designator: o.g.designator,
			Penalty:    round2(penalty * o.excess / total),
			Detail: fmt.Sprintf("最近邻 %s 在 %.0f mil 外（中位邻距 %.0f mil，%.1f×）——周围是空地，可向板内收紧",
				o.g.neighbor, o.g.gapMil, medianGap, o.g.gapMil/medianGap),
		})
	}
	cs = sortContributors(cs)
	if len(cs) > compactMaxContributors {
		cs = cs[:compactMaxContributors]
	}

	f := []pcbCheckFinding{{
		Type:  "board-underused",
		Level: "WARN",
		Message: fmt.Sprintf("板面利用率仅 %.0f%%（渲染 bbox 口径）——%d 个器件的最近邻超过 %.0f mil，板框可收小或器件可收拢",
			util*100, len(offs), threshold) + docRule("3", "布局原则"),
	}}
	return cs, f
}

// compactDenseAttribution 回答「板面被谁吃掉了」——板子太挤时，可动的手是放大板框
// 或换小封装，而这两件事都从最大的那几个器件下手。
//
// 注意这里**不是**在报间距违规（那是 clearance 维和 layout-lint 的 tight pair 的
// 活）；这里报的是面积账。
func compactDenseAttribution(comps []boardComp, boardArea, penalty, util float64) ([]scoreContributor, []pcbCheckFinding) {
	if len(comps) == 0 || boardArea <= 0 {
		return nil, nil
	}
	avg := 0.0
	for _, c := range comps {
		avg += c.area()
	}
	avg /= float64(len(comps))

	type hog struct {
		c    boardComp
		area float64
	}
	var hogs []hog
	var total float64
	for _, c := range comps {
		if a := c.area(); a > avg {
			hogs = append(hogs, hog{c: c, area: a})
			total += a
		}
	}
	if len(hogs) == 0 || total <= 0 {
		return nil, nil
	}
	sort.SliceStable(hogs, func(i, j int) bool {
		if hogs[i].area != hogs[j].area {
			return hogs[i].area > hogs[j].area
		}
		return hogs[i].c.Designator < hogs[j].c.Designator
	})

	cs := make([]scoreContributor, 0, len(hogs))
	for _, h := range hogs {
		cs = append(cs, scoreContributor{
			Designator: h.c.Designator,
			Penalty:    round2(penalty * h.area / total),
			Detail: fmt.Sprintf("占板面 %.1f%%（%.0f mil²，%.0f×%.0f mil）——板面利用率已达 %.0f%%，先考虑放大板框或换小封装",
				h.area/boardArea*100, h.area, h.c.width(), h.c.height(), util*100),
		})
	}
	cs = sortContributors(cs)
	if len(cs) > compactMaxContributors {
		cs = cs[:compactMaxContributors]
	}

	f := []pcbCheckFinding{{
		Type:  "board-overcrowded",
		Level: "WARN",
		Message: fmt.Sprintf("板面利用率 %.0f%%（渲染 bbox 口径，含丝印偏大）——超过 %.0f%% 后布线通道紧张，建议放大板框或改用更小封装",
			util*100, compactPlateauHi*100) + docRule("3", "布局原则"),
	}}
	return cs, f
}

// ---------------------------------------------------------------------------
// 只读取用
// ---------------------------------------------------------------------------

// compactHardCollisions 返回 layout-lint 已经认定的硬碰撞（器件重叠 + 跨网短路）
// 的配对标签。这一维**只读它报数**，不据此扣分——扣分在 rep.Blocking 那条一票
// 否决路径上（文件头第 2 条）。单独抽成函数，是为了让"这里只读不扣"这件事在
// 调用点一眼可见。
func compactHardCollisions(ctx *scoreCtx) []string {
	if ctx == nil || ctx.layout == nil {
		return nil
	}
	out := make([]string, 0, len(ctx.layout.Overlaps)+len(ctx.layout.Shorts))
	for _, o := range ctx.layout.Overlaps {
		out = append(out, o.A+"↔"+o.B)
	}
	for _, s := range ctx.layout.Shorts {
		out = append(out, s.A+"↔"+s.B)
	}
	return out
}
