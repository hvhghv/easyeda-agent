package app

// pcb_score_flow.go — layout-score 的「信号流向」维（dimFlowOrder）。
//
// 要测的东西（#167 原文）：**信号流向单调（电源 → 数字 → RF → 天线）**。审 box-v2
// 那块外包板时得到的判断是：它「好」在左→右 = 电源 → 数字 → RF → 天线，域不串。
// 反过来，把 RF 塞在电源和 MCU 中间的板子，就算每一维几何都干净，走线也一定要
// 来回穿域 —— 这是布局层面的错，不是布线层面的错，所以必须在布局阶段就测出来。
//
// 度量方式：把每个 flow 阶段的**面积加权质心**投影到流向轴上，得到一串实际序位，
// 与 spec 声明的顺序算 **Kendall tau-b** 相关系数。tau ∈ [-1,1]，1 = 完全同序，
// 0 = 毫无序关系，-1 = 完全反序。分数 = (tau+1)/2 × 100。
//
// 为什么是秩相关而不是「质心间距」之类的绝对量：布局的好坏在这一维上是**相对**
// 的——板子多大、模块隔多远都不该影响「顺序对不对」的判定。秩相关天然无量纲，
// 换一块两倍大的板同样布局得同样的分。tau 而不是 Spearman，是因为阶段数很少
// （典型 3-5 个），tau 在小样本上比 Spearman 稳，而且 tau-b 能正确处理并列
// （两个模块质心在流向轴上重合是真实会发生的事，不能当成任意顺序）。
//
// ── 三条关键设计判断 ─────────────────────────────────────────────────────────
//
// 1. **流向是相对的，反向不是错。** spec 写 POWER→ANT，板上从右往左排 —— 这是
//    一块好板，只是坐标系跟作者写 spec 时脑子里的方向相反。所以正向和反向 tau
//    都算，取绝对值大的那个，并在 Metrics 里记 reversed=1。不这么做的话，一块
//    完美的板会因为作者没写 flowAxis 的方向而拿 0 分，度量就废了。
//    （数学上 tau(reverse) ≡ -tau(forward)，取 max|·| 等价于取 |tau|；这里仍然
//    显式算两遍，是为了让「双向都可接受」这个判断在代码里看得见，也顺手当成
//    一次自洽校验。）
//
// 2. **板上没有的阶段跳过，不当成 0 坐标。** spec 声明 flow=[POWER,MCU,RF,ANT]
//    但 ANT 是板载走线天线没有器件 —— 把 ANT 的质心当 (0,0) 会让它排到最前面，
//    tau 直接变负，冤枉一块好板。这类阶段一律剔除并写进 Reason，剩余阶段 <2 时
//    整维 skipped（「没测」≠「测了满分」）。
//
// 3. **质心按器件面积加权。** 一颗 WROOM 模组和一颗 0402 电容不该同权：算术平均
//    下，MCU 阶段里三颗散落的去耦电容能把质心从主控身上拽走一大截，于是「MCU 在
//    哪」这个问题的答案由最不重要的器件决定。面积加权让阶段质心落在这个阶段真正
//    的重心上，也让归因指向的「代表器件」就是精修环该先动的那个。
//
// 阈值来源见各 const 注释；没有工业标准可引的一律标「待校准」，并把原始量
// （tau / 各阶段质心 / 序位偏移）暴露进 Metrics，等真板校准（#167 第五层）。

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

func init() { registerDimScorer(flowOrderScorer{}) }

// flowTieEpsMil 是「两个阶段质心沿流向轴算并列」的容差（mil）。
//
// 出处：**待校准初值**。取 1mil 的理由是量级隔离而非工艺依据 —— 真板上两个功能
// 模块的质心间距是几百 mil 量级（板子本身才几千 mil），而浮点投影的噪声在 1e-9
// 量级。1mil 落在两者中间几个数量级的空档里，既不会把真实的相邻模块误判成并列，
// 也不会让 0.0001mil 的浮点抖动产生一个假的严格序。若将来发现真板上确有「刻意
// 并排、无先后」的模块对被判成有序，把它调到一个格距（5-25mil）即可。
const flowTieEpsMil = 1.0

// flowStageMaxReps 是每个错位阶段最多列几个代表器件进 Contributors。
//
// 出处：**产品判断**，不是工艺阈值。精修环拿到归因后是要「先动谁」，列满整个
// 阶段（可能十几个件）等于没有排序信息；列前 3 个最大的件，覆盖了阶段质心的
// 绝大部分权重，也正是人工调整时会去拖动的那几个。
const flowStageMaxReps = 3

// flowSeriousDisplacement 是「序位偏移到什么程度算真错」的分界（阶段数）。
//
// 出处：**待校准初值**，理由是噪声隔离。偏移 1 = 与相邻阶段互换，在质心相近时
// 可能只是几十 mil 的抖动造成的，报 INFO；偏移 ≥2 = 这个阶段跨过了至少一个别的
// 域，那是真的站错了位（RF 跑到了 POWER 前面这类），报 WARN。
const flowSeriousDisplacement = 2

type flowOrderScorer struct{}

func (flowOrderScorer) id() string { return dimFlowOrder }

// flowStage 是一个「在板上真实存在」的 flow 阶段。
type flowStage struct {
	kind    string      // 功能域（spec.ModuleKinds 之一）
	comps   []boardComp // 该阶段在板上的器件，按面积降序（代表件在前）
	weights []float64   // 与 comps 一一对应的加权重量（面积，缺 bbox 的用替身）
	cx, cy  float64     // 面积加权质心
	proj    float64     // 沿流向轴的投影
	declIdx int         // 声明序位（只数有效阶段，0..n-1）
	actIdx  int         // 实际序位（按 proj 升序，0..n-1）
	noBBox  int         // 该阶段里读不到渲染 bbox 的器件数
	tieMate int         // 沿轴与之并列的其它阶段数
}

// score 是这一维的入口。
func (s flowOrderScorer) score(ctx *scoreCtx) scoreDimension {
	opts := ctx.opts

	// ── 门 1：有没有意图可比 ───────────────────────────────────────────────
	if !ctx.hasSpec() {
		return skipDimension(dimFlowOrder, opts,
			"no S0 spec supplied (--spec) — without a declared flow there is no intent to score the layout against")
	}
	flow := normalizeFlowStages(ctx.spec.Flow)
	if len(flow) < 2 {
		return skipDimension(dimFlowOrder, opts,
			"spec declares %d distinct flow stage(s); an order needs at least 2", len(flow))
	}
	if ctx.snap == nil || len(ctx.snap.Components) == 0 {
		return skipDimension(dimFlowOrder, opts, "no components on the board")
	}

	// ── 门 2：意图落到板上还剩几个阶段 ────────────────────────────────────
	set := collectFlowStages(ctx.snap, flow, ctx.spec.ModuleByKind())
	stages := set.stages
	if len(stages) < 2 {
		return skipDimension(dimFlowOrder, opts,
			"only %d of %d declared flow stage(s) map to on-board components (%s) — an order needs at least 2",
			len(stages), len(flow), joinOrNone(set.drops))
	}

	d := newDimension(dimFlowOrder, opts)
	var degraded []string

	// ── 轴 ─────────────────────────────────────────────────────────────────
	axis, axisReason := flowAxisFor(ctx)
	if axisReason != "" {
		degraded = append(degraded, axisReason)
	}
	for i := range stages {
		stages[i].declIdx = i // collectFlowStages 已按声明顺序产出
		if axis == "y" {
			stages[i].proj = stages[i].cy
		} else {
			stages[i].proj = stages[i].cx
		}
	}

	// 实际序位：按投影升序，SliceStable 让完全相等的投影保持声明先后（输出确定）。
	// 这里**故意不**用 flowTieEpsMil 当比较容差 —— 带容差的比较不是严格弱序
	// （a≈b、b≈c 却 a≉c），排序结果会不稳定。并列的正确处理在 tau-b 那边（它会
	// 把并列对从分子分母里一起剔掉），这里只需要一个确定的展示序位。
	order := make([]int, len(stages))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return stages[order[a]].proj < stages[order[b]].proj })
	for pos, idx := range order {
		stages[idx].actIdx = pos
	}
	for i := range stages {
		for j := range stages {
			if i != j && math.Abs(stages[i].proj-stages[j].proj) <= flowTieEpsMil {
				stages[i].tieMate++
			}
		}
	}

	// ── Kendall tau-b ─────────────────────────────────────────────────────
	decl := make([]float64, len(stages))
	proj := make([]float64, len(stages))
	rev := make([]float64, len(stages))
	for i, st := range stages {
		decl[i] = float64(st.declIdx)
		rev[i] = float64(len(stages) - 1 - st.declIdx)
		proj[i] = st.proj
	}
	// 声明序位天生互不相同，epsA 给 0；投影是连续量，用 flowTieEpsMil 判并列。
	tau := kendallTauB(decl, proj, 0, flowTieEpsMil)
	tauRev := kendallTauB(rev, proj, 0, flowTieEpsMil)
	// 取拟合更好的那个朝向。因为 tauRev ≡ -tau，取较大者就等于取 |tau| —— 于是
	// tauEff 恒 ≥0，反向的板不再被扣分，只在 Metrics 里记一笔 reversed=1。
	// （注意这里必须比 signed 值而不是 |·|：两者绝对值恒相等，比绝对值永远分不出
	// 胜负，反向就会被漏标。）
	tauEff, reversed := tau, false
	if tauRev > tauEff {
		tauEff, reversed = tauRev, true
	}

	// 分数映射照 #167 原文：(tau+1)/2×100。
	//
	// 注意由此产生的一个**已知刻度性质**：因为反向被判为等价（判断 1），tauEff
	// 恒 ≥0，于是这一维的实际下限是 50 分而不是 0 —— 一块完全打乱的板得 50。
	// 这是「毫无序关系 = 一半信用」的读法，不是 bug；但它压缩了动态范围，
	// **待校准**：若真板跑分显示 50-100 区分度不够，改成 tauEff×100 即可，
	// 原始 tau 已在 Metrics 里，换刻度不需要重跑板子。
	score := clampScore((tauEff + 1) / 2 * 100)
	d.Score = score
	penalty := 100 - score

	// ── 归因 ───────────────────────────────────────────────────────────────
	// 用**生效**的声明序位（反向时取镜像），否则一块反向的完美板会被算成
	// 每个阶段都错位。
	dispOf := make([]int, len(stages))
	for i, st := range stages {
		want := st.declIdx
		if reversed {
			want = len(stages) - 1 - st.declIdx
		}
		dispOf[i] = st.actIdx - want
		if dispOf[i] < 0 {
			dispOf[i] = -dispOf[i]
		}
	}
	d.Contributors, d.Findings = flowAttribute(stages, dispOf, penalty, axis, reversed)

	if reversed {
		d.Findings = append(d.Findings, pcbCheckFinding{
			Type: "flow-reversed", Level: "INFO",
			Message: fmt.Sprintf(
				"board flow runs %s along the %s axis, i.e. opposite to the declared order (%s) — scored as equivalent because a flow direction is relative to the coordinate frame, not an error",
				flowDirectionWord(axis), axis, strings.Join(flowKinds(stages), " → ")),
		})
	}
	if len(set.dupes) > 0 {
		// 一个位号同时属于两个阶段，两个阶段的质心就不再独立，tau 的可信度下降。
		// 这是 spec 的数据问题，报出来比默默算一个可疑的分数强。
		d.Findings = append(d.Findings, pcbCheckFinding{
			Type: "flow-stage-overlap", Level: "WARN",
			Message: fmt.Sprintf("%s belong to more than one flow stage — those stage centroids are not independent, the flow score is less trustworthy", strings.Join(set.dupes, ", ")),
		})
	}

	// ── 降级说明 ───────────────────────────────────────────────────────────
	if len(set.drops) > 0 {
		degraded = append(degraded, "flow stage(s) not scored: "+joinOrNone(set.drops))
	}
	degraded = append(degraded, set.notes...)
	noBBox := 0
	for _, st := range stages {
		noBBox += st.noBBox
	}
	if noBBox > 0 {
		degraded = append(degraded, fmt.Sprintf(
			"%d component(s) in the flow have no rendered bbox — their area weight fell back to the board mean", noBBox))
	}
	if len(degraded) > 0 {
		d.Status = dimDegraded
		d.Reason = strings.Join(degraded, "; ")
	}

	// ── 原始量 ─────────────────────────────────────────────────────────────
	axisCode := 0.0
	if axis == "y" {
		axisCode = 1
	}
	revCode := 0.0
	if reversed {
		revCode = 1
	}
	maxDisp := 0
	for _, v := range dispOf {
		if v > maxDisp {
			maxDisp = v
		}
	}
	d.Metrics = map[string]float64{
		"tau":             round2(tau),    // 有符号的正向 tau（负 = 板上反着排）
		"tauEffective":    round2(tauEff), // 取绝对值后的单调度，分数由它算
		"reversed":        revCode,
		"axis":            axisCode, // 0=x 1=y
		"stageCount":      float64(len(stages)),
		"declaredStages":  float64(len(flow)),
		"maxDisplacement": float64(maxDisp),
	}
	for _, st := range stages {
		d.Metrics["centroid."+st.kind+".x"] = round2(st.cx)
		d.Metrics["centroid."+st.kind+".y"] = round2(st.cy)
		d.Metrics["parts."+st.kind] = float64(len(st.comps))
	}
	return d
}

// ---------------------------------------------------------------------------
// 阶段采集
// ---------------------------------------------------------------------------

// normalizeFlowStages 把 spec.Flow 归一成大写、去空、保序去重的阶段列表。
// spec.Validate 已经把重复报成 ERROR，但打分不该因为 spec 写重了就崩 —— 保留
// 首次出现的位置，后面的重复丢掉。
func normalizeFlowStages(flow []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(flow))
	for _, f := range flow {
		k := strings.ToUpper(strings.TrimSpace(f))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// flowStageSet 是采集结果。drops 和 notes 必须分开：drops 是**整段没打分**的阶段
// （报告里要说"这一维只测了 3/4 段"），notes 是打了分但输入有缺（阶段里有件没放
// 到板上）。混成一个列表会让读报告的人分不清哪一段是真没测。
type flowStageSet struct {
	stages []flowStage // 能打分的阶段，按声明顺序
	drops  []string    // 被整段剔除的阶段 + 原因
	notes  []string    // 仍打分但降级的说明
	dupes  []string    // 跨阶段重复出现的位号
}

// collectFlowStages 把「声明的 flow 阶段」落到「板上真实器件」上，算出每个阶段的
// 面积加权质心。
//
// 剔除原因必须能区分三种情况 —— spec 里压根没这个域的模块 / 模块只写了 block 没写
// parts / 写了 parts 但板上没有 —— 因为这三种的修法完全不同（补 spec / 展开块 /
// 检查是否漏放件）。
func collectFlowStages(snap *boardSnapshot, flow []string, byKind map[string][]spec.Module) flowStageSet {
	index := map[string]boardComp{}
	for _, c := range snap.Components {
		key := strings.ToUpper(strings.TrimSpace(c.Designator))
		if key == "" {
			continue
		}
		if _, dup := index[key]; !dup {
			index[key] = c
		}
	}
	fallback := flowAreaFallback(snap)

	var set flowStageSet
	owner := map[string]string{} // 位号 → 首次认领它的阶段
	dupeSet := map[string]bool{}

	for _, kind := range flow {
		mods := byKind[kind]
		if len(mods) == 0 {
			set.drops = append(set.drops, fmt.Sprintf("%s (no module of that kind in the spec)", kind))
			continue
		}
		// 阶段内按位号去重：同一个件被两个同域模块都列到，只算一次。
		seen := map[string]bool{}
		var declared, missing []string
		st := flowStage{kind: kind}
		for _, m := range mods {
			for _, p := range m.PartsOf() {
				key := strings.ToUpper(p)
				if seen[key] {
					continue
				}
				seen[key] = true
				declared = append(declared, p)
				c, ok := index[key]
				if !ok {
					missing = append(missing, p)
					continue
				}
				if prev, taken := owner[key]; taken && prev != kind {
					dupeSet[c.Designator] = true
				} else if !taken {
					owner[key] = kind
				}
				w := c.area()
				if w <= 0 {
					// 没有渲染 bbox 的件（连接器没给 bbox）。给 0 权重等于把它从
					// 阶段里删掉，给 1 等于在 mil² 量级里把它抹平 —— 两种都是静默
					// 丢数据。用全板平均面积当替身，至少让它以「一个普通件」的
					// 分量参与质心，并在 Reason 里说明这次降级。
					w = fallback
					st.noBBox++
				}
				cx, cy := c.center()
				st.cx += cx * w
				st.cy += cy * w
				st.weights = append(st.weights, w)
				st.comps = append(st.comps, c)
			}
		}
		switch {
		case len(declared) == 0:
			set.drops = append(set.drops, fmt.Sprintf("%s (its module(s) declare no parts — only a block id)", kind))
			continue
		case len(st.comps) == 0:
			set.drops = append(set.drops, fmt.Sprintf("%s (declared %s but none is on the board)", kind, joinCapped(missing, 4)))
			continue
		}
		var sumW float64
		for _, w := range st.weights {
			sumW += w
		}
		st.cx /= sumW
		st.cy /= sumW
		if len(missing) > 0 {
			set.notes = append(set.notes, fmt.Sprintf("flow stage %s scored from %d of %d declared part(s) — %s not on the board",
				kind, len(st.comps), len(declared), joinCapped(missing, 4)))
		}
		// 代表件排前面：面积大的先，同面积按位号，输出确定。
		sortStageComps(&st)
		set.stages = append(set.stages, st)
	}

	set.dupes = make([]string, 0, len(dupeSet))
	for d := range dupeSet {
		set.dupes = append(set.dupes, d)
	}
	sort.Strings(set.dupes)
	return set
}

// sortStageComps 按面积降序重排阶段内的器件（weights 跟着走）。
func sortStageComps(st *flowStage) {
	idx := make([]int, len(st.comps))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		if st.weights[ia] != st.weights[ib] {
			return st.weights[ia] > st.weights[ib]
		}
		return st.comps[ia].Designator < st.comps[ib].Designator
	})
	comps := make([]boardComp, len(idx))
	ws := make([]float64, len(idx))
	for i, j := range idx {
		comps[i], ws[i] = st.comps[j], st.weights[j]
	}
	st.comps, st.weights = comps, ws
}

// flowAreaFallback 是「读不到渲染 bbox 的器件」在面积加权里的替身重量：全板有
// bbox 器件的平均面积。全板都没 bbox 时退回 1（此时所有权重相等，等价于算术
// 平均 —— 降级但仍可比）。
func flowAreaFallback(snap *boardSnapshot) float64 {
	var sum float64
	n := 0
	for _, c := range snap.Components {
		if a := c.area(); a > 0 {
			sum += a
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return sum / float64(n)
}

// ---------------------------------------------------------------------------
// 轴
// ---------------------------------------------------------------------------

// flowAxisFor 决定信号流向轴，并在需要降级时返回原因。
//
// 优先级：spec 显式 flowAxis > 板框长边 > 器件质心分布的主轴。最后一档是板框读
// 不到时的兜底（PCB 不在前台时平台返 null，这是既有坑），必须标 degraded ——
// 用器件分布反推的轴在「器件恰好摆成一团」时会翻车，判读的人得知道这一点。
func flowAxisFor(ctx *scoreCtx) (string, string) {
	switch ctx.spec.Axis() {
	case "x":
		return "x", ""
	case "y":
		return "y", ""
	}
	if o := ctx.outline(); o != nil {
		// 板框只有 AABB（Source=="bbox"）在这里**不算降级**：长轴判定只比长宽，
		// AABB 的长宽和真实多边形的长宽是同一个量。真正需要多边形的是「到板边
		// 距离」，那是 edge-io 维的事。
		return o.longAxis(), ""
	}
	ax := principalSpreadAxis(ctx.snap.Components)
	return ax, fmt.Sprintf("board outline unavailable — flow axis inferred from the component spread (%s)", ax)
}

// principalSpreadAxis 返回器件质心分布方差较大的轴。方差相等（含只有一个器件的
// 退化情形）时取 "x"，与 boardOutline.longAxis 的同分取向保持一致。
func principalSpreadAxis(comps []boardComp) string {
	n := 0
	var sx, sy float64
	xs := make([]float64, 0, len(comps))
	ys := make([]float64, 0, len(comps))
	for _, c := range comps {
		x, y := c.center()
		xs = append(xs, x)
		ys = append(ys, y)
		sx += x
		sy += y
		n++
	}
	if n < 2 {
		return "x"
	}
	mx, my := sx/float64(n), sy/float64(n)
	var vx, vy float64
	for i := range xs {
		vx += (xs[i] - mx) * (xs[i] - mx)
		vy += (ys[i] - my) * (ys[i] - my)
	}
	if vy > vx {
		return "y"
	}
	return "x"
}

// ---------------------------------------------------------------------------
// Kendall tau-b
// ---------------------------------------------------------------------------

// kendallTauB 是带并列修正的 Kendall 秩相关系数。
//
//	tau_b = (C - D) / sqrt((n0 - n1) * (n0 - n2))
//	n0 = n(n-1)/2，n1 = a 里的并列对数，n2 = b 里的并列对数
//
// 为什么用 tau-b 而不是简单的 tau-a：两个模块的质心可能在流向轴上重合（并排摆
// 的电源和保护电路就是这样），tau-a 会把这种「本来就没有先后」的对子当成一次
// 失序来扣分。tau-b 把它从分子分母里一起剔掉，得到的才是「有序信息里有多少是
// 对的」。
//
// epsA/epsB 是各自的并列容差 —— 连续量（坐标投影）必须给一个容差，否则
// 1e-12 的浮点差就会被当成一个严格序。全部并列（分母为 0）时返回 0：没有任何
// 序信息可比，相关性无定义，返回 0 = 「毫无序关系」是唯一诚实的取值。
func kendallTauB(a, b []float64, epsA, epsB float64) float64 {
	n := len(a)
	if n != len(b) || n < 2 {
		return 0
	}
	var conc, disc, tiesA, tiesB float64
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			da, db := a[i]-a[j], b[i]-b[j]
			ta, tb := math.Abs(da) <= epsA, math.Abs(db) <= epsB
			// 一对同时在两边并列的，n1 和 n2 都要记 —— 这正是 tau-b 定义里
			// 「按各自维度分别统计并列」的含义。
			if ta {
				tiesA++
			}
			if tb {
				tiesB++
			}
			if ta || tb {
				continue
			}
			if da*db > 0 {
				conc++
			} else {
				disc++
			}
		}
	}
	n0 := float64(n) * float64(n-1) / 2
	den := math.Sqrt((n0 - tiesA) * (n0 - tiesB))
	if den <= 0 {
		return 0
	}
	return (conc - disc) / den
}

// ---------------------------------------------------------------------------
// 归因
// ---------------------------------------------------------------------------

// flowAttribute 把这一维扣掉的分摊到具体器件上，并为每个错位阶段产出一条 finding。
//
// 摊派口径：先按「序位偏移」把总扣分分给各阶段（偏移大的多担），再按面积份额分
// 给该阶段的前几个代表件。这样 Contributors 的 Penalty 之和 == 100−Score，
// 精修环按这个梯度从上往下动件，动的就是对分数影响最大的那个。
//
// 一个边界：偏移全是 0 却仍有扣分 —— 这只可能来自并列（两个阶段质心在轴上重合，
// tau-b 把那对剔掉，分子拿不满）。此时按「谁参与了并列」摊派，因为要修的正是
// 这几个重合的阶段，而不是没人。
func flowAttribute(stages []flowStage, disp []int, penalty float64, axis string, reversed bool) ([]scoreContributor, []pcbCheckFinding) {
	if penalty <= 0 || len(stages) == 0 {
		return nil, nil
	}
	blame := make([]float64, len(stages))
	var sum float64
	for i := range stages {
		blame[i] = float64(disp[i])
		sum += blame[i]
	}
	tieBased := false
	if sum <= 0 {
		tieBased = true
		for i := range stages {
			blame[i] = float64(stages[i].tieMate)
			sum += blame[i]
		}
	}
	if sum <= 0 {
		return nil, nil // 既没错位也没并列却有扣分：算术上到不了这里，防御性返回
	}

	var cs []scoreContributor
	var fs []pcbCheckFinding
	for i, st := range stages {
		if blame[i] <= 0 {
			continue
		}
		stagePenalty := penalty * blame[i] / sum
		reps := st.comps
		repW := st.weights
		if len(reps) > flowStageMaxReps {
			reps, repW = reps[:flowStageMaxReps], repW[:flowStageMaxReps]
		}
		var repSum float64
		for _, w := range repW {
			repSum += w
		}
		want := st.declIdx
		if reversed {
			want = len(stages) - 1 - st.declIdx
		}
		var detail string
		if tieBased {
			detail = fmt.Sprintf("flow stage %s shares its %s-position with %d other stage(s) — the two domains overlap along the flow axis",
				st.kind, axis, st.tieMate)
		} else {
			detail = fmt.Sprintf("flow stage %s sits at position %d/%d along %s, expected %d/%d",
				st.kind, st.actIdx+1, len(stages), axis, want+1, len(stages))
		}
		for j, c := range reps {
			share := 1.0 / float64(len(reps))
			if repSum > 0 {
				share = repW[j] / repSum
			}
			cs = append(cs, scoreContributor{
				Designator: c.Designator,
				Penalty:    round2(stagePenalty * share),
				Detail:     detail,
			})
		}

		level := "INFO"
		if disp[i] >= flowSeriousDisplacement {
			level = "WARN"
		}
		lead := ""
		if len(reps) > 0 {
			lead = reps[0].Designator
		}
		fs = append(fs, pcbCheckFinding{
			Type: "flow-order-stage", Level: level,
			Designator: lead,
			Message: fmt.Sprintf("%s (centroid %.0f,%.0f mil; %d part(s), lead %s)%s",
				detail, st.cx, st.cy, len(st.comps), lead,
				docRule("3.3", "模拟 / 数字分区")),
			At: &pcbXY{X: round2(st.cx), Y: round2(st.cy)},
		})
	}
	// fs 天然按声明顺序产出（stages 就是声明序），读起来跟 spec 里的 flow 对得上，
	// 不再二次排序。
	return sortContributors(cs), fs
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func flowKinds(stages []flowStage) []string {
	out := make([]string, 0, len(stages))
	for _, st := range stages {
		out = append(out, st.kind)
	}
	return out
}

// flowDirectionWord 把「反向」翻译成人能对着板子核对的方位词（y-UP：+y 向上）。
func flowDirectionWord(axis string) string {
	if axis == "y" {
		return "top → bottom"
	}
	return "right → left"
}

// joinCapped 拼接一个列表，超过 n 个只列前 n 个 + 省略计数（报错信息不该刷屏）。
func joinCapped(items []string, n int) string {
	if len(items) == 0 {
		return "none"
	}
	if len(items) <= n {
		return strings.Join(items, ",")
	}
	return fmt.Sprintf("%s,…+%d", strings.Join(items[:n], ","), len(items)-n)
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, "; ")
}
