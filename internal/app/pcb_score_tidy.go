package app

// pcb_score_tidy.go — 齐整度维（dimTidy），即 issue #153「一致性审计」的打分实现。
//
// 为什么这一维值得单独存在：商业开源板第一眼的「专业感」几乎全部来自**一致性**，
// 而不是电气质量。BBClaw（开源 AI 语音终端，69 器件 / 221 丝印）`pcb check` 干净、
// `layout-lint` 也不报，但拿 `pcb list --include-bbox` 的原始坐标自己算，五处都不齐：
//
//	① 落格   5mil 网格 21/69 落格（10mil 7/69、25mil 3/69）。坐标长这样
//	         C2(635.0015, 1109.998)、C6(455.0015, 839.998) —— auto-place / GUI 拖动
//	         留下的**亚 mil 漂移**，目视看不出，却让行列对齐永远差一点
//	② 朝向   U 组 n=10 出现 {0:6, 90:1, 180:1, 270:2} 四种朝向
//	③ 位号   方位四面开花：左 20 / 上 21 / 右 10 / 下 18
//	④ 字号   Designator {32:68, 45:1}（1 个漏网）；自由字符串 {30:13, 32:1}
//	⑤ 阵列   同类近邻中心距 85/85/85/120/130.8/…/199.1/200/205.2 ——「差一点」是丑源
//
// 五条子规则一一对应上面五条实测，全部 WARN/INFO：**纯 cosmetic，不该阻塞一块电气
// 正确的板**（#153 原文）。本维的 finding 因此永远不会进 layoutScoreReport.Blocking
// —— 那份只从 layout-lint 的硬错（短路/重叠/出框）来。
//
// ── 只报不修 ────────────────────────────────────────────────────────────────
// #153 的真机验证给了两条硬结论，直接决定本文件的定位：
//
//  1. `silk-align --side` 是 **soft hint**（它的语义是碰撞规避：已经不撞的标就不动）。
//     实测 69 件里只动了 10 件，方位分布纹丝不动（左20/上21/右10/下18 → 完全一样）。
//     所以「统一位号方位」现有 CLI **没有任何路径**，本维只负责**报**，不假设有
//     现成执行器能修。
//  2. 同一次 silk-align 把 `silk-over-pad` 从 0 条推到 3 条，而它自己对这几个还报
//     clean:true。所以任何给出修复建议的 finding 都必须提示「改完要复核 pcb check」。
//
// ── 「没测」不等于「满分」 ──────────────────────────────────────────────────
// 五条子规则各自可能在某块板上**没有可判样本**（没有丝印数据 / 没有 ≥3 件的同类组 /
// 板上根本没有阵列）。这种子规则**退出加权**（不是拿 0 分也不是拿 100 分），并把原因
// 写进 Reason。全部子规则都无样本时整维 skipped。否则一块只有 3 个器件、没有丝印的
// 板会靠「什么都没测到」拿满分，#167 第五层「好板必须得高分」的校准判据就废了。

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 阈值 —— 每个数字写清出处
// ---------------------------------------------------------------------------

const (
	// 默认落格网格 = 5mil。**这与 conventions §9.1 的 25mil（纯通孔板 50mil）不矛盾，
	// 两者目的不同**：§9.1 的 25mil 是「吸栅该吸到哪」的**目标栅**（给执行器
	// `pcb grid-snap` 用），5mil 是「有没有亚 mil 脏值」的**判据**（给审计用）。
	// 用目标栅当判据没有信息量 —— BBClaw 在 25mil 栅上只有 3/69 落格，整块板判全错；
	// 5mil 栅上 21/69 落格，剩下 48 个正是 635.0015 这类漂移，而且可修、修完零副作用
	// （#153 实测 grid-snap 最大位移 1.998mil，pcb check findings 25→25 不变）。
	pcbTidyDefaultGridMil = 5.0

	// 落格容差：EasyEDA 把坐标存到 0.0001mil 量级（635.0015），0.001mil 只用来吸收
	// 浮点噪声，**不**用来放过漂移 —— 放松到 0.005 就会把 635.0015 判成落格，这一维
	// 要抓的东西正好全被放过。
	pcbTidyGridTolMil = 0.001

	// 同类组的最小样本数。2 件谈不上「多数派」，统计噪声会压过信号。#153 实测的组
	// 是 C n=19 / R n=16 / U n=10 / SW n=3 / H n=4，3 能全覆盖。**待校准**。
	pcbTidyMinGroup = 3

	// 公制间距件判据的最小脚数。两脚 0402 本体 pad 间距也是公制（≈0.5mm），但 §9.1
	// 排除公制件的顾虑是「**成排** pad 整体偏离原生子栅」；两脚件吸栅位移 ≤ 半格
	// （#153 实测最大 1.998mil）没有这个问题，而 C/R/D 恰恰是最该吸栅的一批。
	// SOT-23（3 脚）同理保留。
	pcbTidyMetricMinPads = 4
	// 公制 pitch 恒为 0.05mm 的整倍数；0.005mm 容差 ≈ 0.2mil，够吸收坐标噪声又不至于
	// 把 1.11mm 这种非标间距误判成公制。**待校准**。
	pcbTidyMetricTolMM = 0.005
	// 英制 pitch（50/100mil）都是整 mil；0.05mm 与整 mil 的最小公倍数要到 250mil 才
	// 出现，常规封装够不着，所以「整 mil ⇒ 英制」这条先行判据不会误杀公制件。
	pcbTidyImperialTolMil = 0.05

	// 朝向：把 ±1° 内的值吸到 90° 步进，纯粹吸收浮点/取整噪声。真正的自由角
	// （§9.3 硬禁）会自成一类，被「>2 种朝向」抓住。
	pcbTidyRotSnapDeg = 1.0
	// 180/270 的附加扣分系数（相对「不在多数派两态里」的 1.0）。§9.3：默认 0/90 两态，
	// 180/270 仅在 pin-1/极性需要时 —— 平台不暴露「有没有极性理由」，所以只能给一个
	// 轻量提醒而不是等价违规。#153 的 ClawFlow 评估也点名这条无法机械判定
	// （H 的 90/270、SW 的 90/270 大概率是结构决定的合法朝向）。**待校准**。
	pcbTidyOddRotFactor = 0.3

	// 位号方位死区：丝印中心与本体中心距离小于它就判不出方位（丝印压在本体正中），
	// 这类不进多数派统计。1mil 只是「几乎重合」的下限。
	pcbTidySideDeadzoneMil = 1.0

	// 字号离群容差。fontSize 是设计值不是测量值（32/45/30 这种整数），0.5mil 只用来
	// 吸收序列化噪声。
	pcbTidyFontTolMil = 0.5

	// 阵列：同一条轴的判据 ±2mil、行内中心距恒定 ±5mil，两者都直接来自 conventions
	// §9.2「同类件成行对齐 + 等距」。
	pcbTidyArrayAxisTolMil = 2.0
	pcbTidyArrayStepTolMil = 5.0
	// 相邻步距超过它就不算同一个阵列（是两簇碰巧共线的件）。500mil ≈ 12.7mm，
	// #153 实测的 C 组近邻距最大 366.7mil 仍在同簇内。**待校准**。
	pcbTidyArrayMaxStepMil = 500.0

	// 归因列表上限。BBClaw 一块板就有 48 个不落格件，全量进 JSON 太吵；真实总数在
	// Metrics 里（offGridCount 等），截断不会丢信息。
	pcbTidyMaxContributors = 20
	// finding 里最多点名几个位号（后面用 "+N more" 收口）。
	pcbTidyMaxNamed = 8
)

// 子规则 id（对外契约：Metrics key 和 finding.Type 都用它）。
const (
	tidyRuleOffGrid   = "off-grid"
	tidyRuleRotation  = "rotation-inconsistent"
	tidyRuleSilkSide  = "silk-side-inconsistent"
	tidyRuleSilkStyle = "silk-style-inconsistent"
	tidyRuleArray     = "array-irregular"
)

// pcbTidySubWeights 是五条子规则在本维内部的权重（和为 1）。
//
// 定权理由（**待校准初值**，#167 第五层拿真板校准后回来改）：
//   - off-grid 0.30：#153 自己说这条「最刺眼」，而且落格是行列对齐的地基 —— 不落格
//     的板做 align/distribute 结果继续带尾数，其它四条都建在它上面。
//   - rotation 0.25：同类件朝向散乱是第二眼就看见的，且与电气无关、纯观感损失。
//   - silk-side 0.20：位号四面开花影响的是**读板**（找件、对照 BOM），比字号严重。
//   - array 0.15：步进「差一点」只在成排件上出现，覆盖面比前三条窄。
//   - silk-style 0.10：字号离群通常只有一两个漏网件（BBClaw 各 1 个），影响面最小，
//     而且 `silk-set --font-size` 一条命令就能修。
var pcbTidySubWeights = map[string]float64{
	tidyRuleOffGrid:   0.30,
	tidyRuleRotation:  0.25,
	tidyRuleSilkSide:  0.20,
	tidyRuleArray:     0.15,
	tidyRuleSilkStyle: 0.10,
}

// pcbTidyMechRe 是机械/锚定件的位号前缀。conventions §9.1 明令把「机械/外壳锚定」
// 排除在吸栅之外：安装孔的坐标来自结构图（3.5mm 距角 = 137.795mil），吸到 5mil 栅
// 上等于把孔挪出结构件的位置。用户手工 lock 的件由 boardComp.Locked 覆盖，这里只
// 兜住「没 lock 但本质是机械件」的那批。
var pcbTidyMechRe = regexp.MustCompile(`(?i)^(H|MH|MK|MARK|FID|LOGO)\d*$`)

// tidyConvRule 是 docRule 的姊妹。齐整度这五条规则的正本在**布局约定**
// (pcb-layout-conventions.md §9)，不在工艺规范手册 (pcb-design-rules.md) —— 用
// docRule 会把读者指到一个根本没有这些章节的文件去。
func tidyConvRule(section, title string) string {
	return fmt.Sprintf(" [约定 §%s %s — pcb-layout-conventions.md]", section, title)
}

// ---------------------------------------------------------------------------
// 子规则的记账容器
// ---------------------------------------------------------------------------

// tidyRule 是一条子规则的产物。
//
// ok=false 表示「这块板上这条规则没有可判样本」——它**退出加权**，既不是 0 分也不是
// 100 分。这是本维守住「没测≠满分」的地方。
type tidyRule struct {
	id     string
	weight float64
	ok     bool
	reason string  // ok=false 时的原因（会汇进维度 Reason）
	score  float64 // 0-100 子分（settle 后有效）

	// lost 是「这个器件在本子规则里扣掉多少分」，order 保序输出。
	// 不变式：Σ lost == 100 - score（settle 负责维持）。
	lost   map[string]float64
	detail map[string]string
	order  []string

	findings []pcbCheckFinding
	metrics  map[string]float64
}

func newTidyRule(id string) *tidyRule {
	return &tidyRule{
		id: id, weight: pcbTidySubWeights[id], score: 100,
		lost: map[string]float64{}, detail: map[string]string{}, metrics: map[string]float64{},
	}
}

// skip 把子规则标成「无样本」。
func (r *tidyRule) skip(format string, args ...any) *tidyRule {
	r.ok = false
	r.reason = fmt.Sprintf(format, args...)
	return r
}

// charge 记一笔扣分。同一个器件可能被同一条子规则扣两次（例如既不在多数派朝向、
// 又是 180/270），累加。
func (r *tidyRule) charge(des string, pts float64, detail string) {
	if des == "" || pts <= 0 {
		return
	}
	if _, seen := r.lost[des]; !seen {
		r.order = append(r.order, des)
	}
	r.lost[des] += pts
	if detail != "" {
		if old := r.detail[des]; old != "" {
			r.detail[des] = old + "；" + detail
		} else {
			r.detail[des] = detail
		}
	}
}

// scale 把「每个违规件记 1 笔」换算成分数。子规则先按件计数、最后统一乘
// 100/population —— 这样每件的扣分正好是它的**边际贡献**（修好一个，子分升
// 100/population），精修环拿到的是一个可比的量而不是布尔标记。
func (r *tidyRule) scale(unit float64) {
	for d := range r.lost {
		r.lost[d] *= unit
	}
}

// settle 把扣分总额收敛成子分。
//
// 总额可能因为「一个器件被扣两笔」超过 100，这时按比例缩放全部扣分 —— 保住
// 「Σ contributor.Penalty == 100 − Score」这条不变式（单测钉住）。否则归因加起来
// 对不上分数，判读的人会以为报告算错了；这个项目在聚合报告上踩过一次
// 「0 个阻塞项却 FAIL」的坑，计数与判定必须同源。
func (r *tidyRule) settle() *tidyRule {
	total := 0.0
	for _, v := range r.lost {
		total += v
	}
	if total > 100 {
		k := 100 / total
		for d := range r.lost {
			r.lost[d] *= k
		}
		total = 100
	}
	r.score = math.Max(0, 100-total)
	r.ok = true
	return r
}

// ---------------------------------------------------------------------------
// 维度入口
// ---------------------------------------------------------------------------

type tidyScorer struct{}

func (tidyScorer) id() string { return dimTidy }

func (tidyScorer) score(ctx *scoreCtx) scoreDimension { return scoreTidy(ctx) }

func init() { registerDimScorer(tidyScorer{}) }

// scoreTidy 是齐整度维的纯核：吃 scoreCtx，出一份带归因的 scoreDimension。
func scoreTidy(ctx *scoreCtx) scoreDimension {
	if ctx == nil || ctx.snap == nil {
		return skipDimension(dimTidy, layoutScoreOpts{}, "没有板级快照")
	}
	opts := ctx.opts
	snap := ctx.snap
	if len(snap.Components) == 0 {
		return skipDimension(dimTidy, opts, "板上没有已放置器件 —— 齐整度无从测起")
	}

	grid := opts.gridMil
	if grid <= 0 {
		grid = pcbTidyDefaultGridMil
	}

	groups := tidyGroups(snap.Components)
	rules := []*tidyRule{
		tidyScoreOffGrid(snap.Components, grid),
		tidyScoreRotation(groups),
		tidyScoreSilkSide(snap, groups),
		tidyScoreSilkStyle(snap),
		tidyScoreArray(groups),
	}

	// 加权：只有 ok 的子规则进分母。
	var sumW, sumWS float64
	for _, r := range rules {
		if r.ok {
			sumW += r.weight
			sumWS += r.weight * r.score
		}
	}

	d := newDimension(dimTidy, opts)
	if sumW <= 0 {
		// 五条全无样本 —— 明确 skipped，绝不返回 100。
		var why []string
		for _, r := range rules {
			why = append(why, r.id+"："+r.reason)
		}
		return skipDimension(dimTidy, opts, "五条子规则都没有可判样本（%s）", strings.Join(why, "；"))
	}
	d.Score = clampScore(sumWS / sumW)

	// 归因：按归一化后的子权重把每条子规则的扣分折算到本维 0-100 尺度上，
	// 同一器件跨子规则累加 —— 精修环靠这个梯度决定先动谁。
	agg := map[string]float64{}
	det := map[string][]string{}
	var order []string
	for _, r := range rules {
		if !r.ok {
			continue
		}
		f := r.weight / sumW
		for _, des := range r.order {
			if _, seen := agg[des]; !seen {
				order = append(order, des)
			}
			agg[des] += f * r.lost[des]
			if s := r.detail[des]; s != "" {
				det[des] = append(det[des], s)
			}
		}
	}
	var contribs []scoreContributor
	for _, des := range order {
		contribs = append(contribs, scoreContributor{
			Designator: des,
			Penalty:    math.Round(agg[des]*100) / 100,
			Detail:     strings.Join(det[des], "；"),
		})
	}
	contribs = sortContributors(contribs)
	if len(contribs) > pcbTidyMaxContributors {
		contribs = contribs[:pcbTidyMaxContributors]
	}
	d.Contributors = contribs

	// findings + metrics 汇总。
	d.Metrics = map[string]float64{"gridMil": grid}
	for _, r := range rules {
		d.Findings = append(d.Findings, r.findings...)
		if !r.ok {
			continue
		}
		for k, v := range r.metrics {
			d.Metrics[k] = v
		}
		d.Metrics[tidyScoreKey(r.id)] = math.Round(r.score*10) / 10
	}

	// 状态与原因。**数据缺失**（丝印读不到 / 器件没有渲染 bbox）算 degraded：分数
	// 仍然可信但输入不全，报告必须说明。**板上就是没这类样本**（没有 ≥3 件的同类组、
	// 没有阵列）不是数据降级，但同样要写进 Reason —— 否则「齐整度 92 分」读起来像
	// 全面体检，实际只测了一条半。
	var missing []string
	for _, r := range rules {
		if !r.ok {
			missing = append(missing, r.id+"（"+r.reason+"）")
		}
	}
	degraded := len(snap.Silk) == 0
	noBBox := 0
	for _, c := range snap.Components {
		if c.BBox == nil {
			noBBox++
		}
	}
	var reasons []string
	if len(missing) > 0 {
		reasons = append(reasons, "退出加权的子规则："+strings.Join(missing, "、"))
	}
	if degraded {
		reasons = append(reasons, "丝印数据为空（PCB 非前台 / 连接器未返回）—— 位号方位与字号两条无从判起")
	}
	if noBBox > 0 {
		degraded = true
		reasons = append(reasons, fmt.Sprintf(
			"%d/%d 个器件没有渲染 bbox，位置退回 anchor 近似（anchor≠中心，方位/阵列判定因此是近似值）",
			noBBox, len(snap.Components)))
	}
	if degraded {
		d.Status = dimDegraded
	}
	d.Reason = strings.Join(reasons, "；")
	return d
}

// tidyScoreKey 是子分在 Metrics 里的 key（scoreOffGrid / scoreRotationInconsistent…）。
func tidyScoreKey(ruleID string) string {
	parts := strings.Split(ruleID, "-")
	out := "score"
	for _, p := range parts {
		if p == "" {
			continue
		}
		out += strings.ToUpper(p[:1]) + p[1:]
	}
	return out
}

// ---------------------------------------------------------------------------
// 同类分组
// ---------------------------------------------------------------------------

// tidyGroup 是一组「同类件」：同位号字母前缀 + 同装配面。
type tidyGroup struct {
	prefix  string
	layer   int
	members []boardComp
}

// key 是分组的稳定输出名（R@top / U@bottom）。
func (g tidyGroup) key() string {
	switch g.layer {
	case pcbSideTop:
		return g.prefix + "@top"
	case pcbSideBottom:
		return g.prefix + "@bottom"
	}
	return g.prefix + "@unknown"
}

// tidyGroups 按「前缀 + 装配面」分组并按 key 排序。
//
// 为什么必须带装配面：底面件是镜像放置的，同一个**视觉**朝向在底面存的是另一个角度
// 值；把两面混进一组统计多数派会凭空造出「朝向不一致」和「位号不同侧」。代价是小板
// 上的组更容易低于 pcbTidyMinGroup 而退出统计 —— 宁可少测也不要假报。
func tidyGroups(comps []boardComp) []tidyGroup {
	byKey := map[string]*tidyGroup{}
	for _, c := range comps {
		p := refPrefixCP(c.Designator)
		if p == "" {
			continue // 位号不以字母开头：无法归类，不参与同类统计
		}
		k := fmt.Sprintf("%s|%d", p, c.Layer)
		g := byKey[k]
		if g == nil {
			g = &tidyGroup{prefix: p, layer: c.Layer}
			byKey[k] = g
		}
		g.members = append(g.members, c)
	}
	out := make([]tidyGroup, 0, len(byKey))
	for _, g := range byKey {
		sort.SliceStable(g.members, func(i, j int) bool {
			return g.members[i].Designator < g.members[j].Designator
		})
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// ---------------------------------------------------------------------------
// ① off-grid
// ---------------------------------------------------------------------------

// tidyScoreOffGrid 判器件 anchor 是否落在网格上。
//
// 判的是 **anchor**（boardComp.X/Y）而不是 bbox 中心：`pcb grid-snap` 写的就是
// anchor，用中心判会得出一个执行器无法直接消费的判据（中心含丝印，随旋转变）。
//
// 子分 = 落格率。每个不落格件的扣分是它的**边际贡献**（修好一个，落格率升
// 1/eligible）—— 这个量精确可解释，所以所有不落格件扣分相同；「谁漂得最远」写在
// Detail 与 finding 里，不靠扣分体现（落格本来就是二值属性）。
func tidyScoreOffGrid(comps []boardComp, grid float64) *tidyRule {
	r := newTidyRule(tidyRuleOffGrid)
	type off struct {
		des  string
		dev  float64
		x, y float64
	}
	var offs []off
	eligible, onGrid := 0, 0
	for _, c := range comps {
		if !tidyGridEligible(c) {
			continue
		}
		eligible++
		// 两轴取最大偏差：一个轴落格另一个轴漂移，整体仍然是「行列对不齐」。
		dev := math.Max(tidyGridDev(c.X, grid), tidyGridDev(c.Y, grid))
		if dev <= pcbTidyGridTolMil {
			onGrid++
			continue
		}
		offs = append(offs, off{des: c.Designator, dev: dev, x: c.X, y: c.Y})
	}
	if eligible == 0 {
		return r.skip("没有可判落格的器件（全部 locked / 公制间距件 / 机械锚定件）")
	}
	// 最远的排前面 —— 归因列表被截断时留下的是最脏的那批。
	sort.SliceStable(offs, func(i, j int) bool {
		if offs[i].dev != offs[j].dev {
			return offs[i].dev > offs[j].dev
		}
		return offs[i].des < offs[j].des
	})
	for _, o := range offs {
		r.charge(o.des, 1, fmt.Sprintf("anchor(%.4f, %.4f) 离最近 %gmil 格点 %.4fmil", o.x, o.y, grid, o.dev))
	}
	r.scale(100 / float64(eligible))
	r.metrics["onGridRatio"] = math.Round(float64(onGrid)/float64(eligible)*1000) / 1000
	r.metrics["offGridCount"] = float64(len(offs))
	r.metrics["gridEligible"] = float64(eligible)
	if len(offs) > 0 {
		r.metrics["worstOffGridMil"] = math.Round(offs[0].dev*10000) / 10000
		named := make([]string, 0, len(offs))
		for _, o := range offs {
			named = append(named, fmt.Sprintf("%s(%.4fmil)", o.des, o.dev))
		}
		r.findings = append(r.findings, pcbCheckFinding{
			Type: tidyRuleOffGrid, Level: "WARN",
			Designator: offs[0].des,
			At:         &pcbXY{round2(offs[0].x), round2(offs[0].y)},
			Message: fmt.Sprintf("%d/%d free component(s) are off the %gmil grid (worst first: %s) — sub-mil drift left by auto-place/GUI dragging; `pcb grid-snap --grid %g` fixes it (#153 measured max shift 1.998mil with no new `pcb check` findings). Locked / metric-pitch / mechanical-anchor parts are excluded%s",
				len(offs), eligible, grid, tidyNames(named), grid, tidyConvRule("9.1", "吸栅——排除公制/锚定件")),
		})
	}
	return r.settle()
}

// tidyGridDev 是坐标到最近格点的距离。
func tidyGridDev(v, grid float64) float64 {
	if grid <= 0 {
		return 0
	}
	return math.Abs(v - math.Round(v/grid)*grid)
}

// tidyGridEligible 报告一个器件该不该参与落格统计。
//
// 三条排除，全部来自 conventions §9.1（「原 agentCheck 把任何不在栅上的件判违规会
// 大量误报且有害」的修订）：
//   - locked：用户明示的意图，不是布局缺陷，且执行器本来就不该动它。
//   - 机械/锚定件：坐标来自结构图，吸栅等于把孔挪出位置。
//   - 公制间距件：吸本体会把成排 pad 推离其原生子栅，反而更糟。
func tidyGridEligible(c boardComp) bool {
	if c.Locked {
		return false
	}
	if pcbTidyMechRe.MatchString(strings.TrimSpace(c.Designator)) {
		return false
	}
	return !tidyMetricPitchPart(c)
}

// tidyMetricPitchPart 判器件是否为公制间距件（0.5/0.65/0.8mm pitch IC、JST /
// 2.0 / 2.5mm 连接器）。
//
// 判据是**机械的**而不是型号白名单：取最小相邻焊盘中心距 p，
//   - p 落在整 mil 上 ⇒ 英制（50/100mil 都是整 mil）——先行短路，防止 100mil=2.54mm
//     恰好逼近 0.05mm 倍数被误判；
//   - 否则 p 换算成 mm 后落在 0.05mm 的整倍数上 ⇒ 公制（公制封装 pitch 恒是 0.05mm
//     的倍数：0.5 / 0.65 / 0.8 / 1.0 / 2.0 / 2.54… 中除 2.54 外全部命中）。
//
// 白名单方案在这里行不通：placed 件的 name 常是 "={Manufacturer Part}" 模板，
// footprint 名各家各写，只有坐标是可信的。
func tidyMetricPitchPart(c boardComp) bool {
	if len(c.Pads) < pcbTidyMetricMinPads {
		return false
	}
	p := tidyMinPadPitch(c.Pads)
	if p <= 0 {
		return false
	}
	if math.Abs(p-math.Round(p)) <= pcbTidyImperialTolMil {
		return false
	}
	mm := p * 0.0254
	steps := mm / 0.05
	return math.Abs(steps-math.Round(steps))*0.05 <= pcbTidyMetricTolMM
}

// tidyMinPadPitch 是焊盘两两中心距的最小值（0 = 算不出）。
func tidyMinPadPitch(pads []boardPad) float64 {
	best := math.Inf(1)
	for i := 0; i < len(pads); i++ {
		for j := i + 1; j < len(pads); j++ {
			if d := math.Hypot(pads[i].X-pads[j].X, pads[i].Y-pads[j].Y); d > 0 && d < best {
				best = d
			}
		}
	}
	if math.IsInf(best, 1) {
		return 0
	}
	return best
}

// ---------------------------------------------------------------------------
// ② rotation-inconsistent
// ---------------------------------------------------------------------------

// tidyScoreRotation 判同类件的朝向是否收敛到两态。
//
// 两条判据（#153 原文）：
//   - 组内出现 **>2 种**朝向 → 不在出现次数最多的两态里的件算异类（§9.3「默认 0/90
//     两态」，所以两种朝向本身合法：C 组 {0:7, 90:12} 不该报）。
//   - 出现 **180°/270°** → 轻量提醒（系数 0.3）。§9.3 说这两态「仅在 pin-1/极性需要
//     时」，而平台不暴露「有没有极性理由」，所以只能提醒不能等价违规。
//
// 两条排除：
//   - **焊盘数 ≤1 的件不参与**。安装孔 / Mark / 测试点是旋转对称的，rotation 既不可
//     观测也无观感后果 —— #153 实测的 H 组 {90:2, 270:2} 正是这种假阳性。
//   - **locked 件不扣分**，但仍参与多数派投票。锁定的连接器/结构件恰恰是「该跟谁对齐」
//     的锚点，把它们踢出投票会让多数派被少数散件绑架；但它们本身不可动，扣分没有意义
//     （分数只反映**可修**的散乱，与 off-grid 的口径一致）。
func tidyScoreRotation(groups []tidyGroup) *tidyRule {
	r := newTidyRule(tidyRuleRotation)
	population, outliers, odd := 0, 0, 0
	maxKinds := 0
	for _, g := range groups {
		var members []boardComp
		free := 0
		for _, c := range g.members {
			if len(c.Pads) >= 2 {
				members = append(members, c)
				if !c.Locked {
					free++
				}
			}
		}
		if len(members) < pcbTidyMinGroup || free == 0 {
			continue
		}
		population += free
		counts := map[float64]int{}
		for _, c := range members {
			counts[tidyNormalizeRot(c.Rotation)]++
		}
		if len(counts) > maxKinds {
			maxKinds = len(counts)
		}
		allowed := tidyTopRotations(counts, 2)
		var offNames, oddNames []string
		for _, c := range members {
			rot := tidyNormalizeRot(c.Rotation)
			if !allowed[rot] {
				outliers++
				if !c.Locked {
					r.charge(c.Designator, 1, fmt.Sprintf("%s 组朝向 %g°，不在多数派 %s 内", g.key(), rot, tidyRotSet(allowed)))
				}
				offNames = append(offNames, fmt.Sprintf("%s(%g°)", c.Designator, rot))
			}
			if rot == 180 || rot == 270 {
				odd++
				if !c.Locked {
					r.charge(c.Designator, pcbTidyOddRotFactor, fmt.Sprintf("朝向 %g°（§9.3 仅在 pin-1/极性需要时才该用）", rot))
				}
				oddNames = append(oddNames, fmt.Sprintf("%s(%g°)", c.Designator, rot))
			}
		}
		if len(counts) > 2 {
			r.findings = append(r.findings, pcbCheckFinding{
				Type: tidyRuleRotation, Level: "WARN",
				Message: fmt.Sprintf("%s group (n=%d) uses %d distinct rotations %s — same-type parts should settle on at most two (0°/90°); off-majority: %s%s",
					g.key(), len(members), len(counts), tidyRotHistogram(counts), tidyNames(offNames),
					tidyConvRule("9.3", "旋转只取 90° 步进")),
			})
		}
		if len(oddNames) > 0 {
			r.findings = append(r.findings, pcbCheckFinding{
				Type: tidyRuleRotation, Level: "INFO",
				Message: fmt.Sprintf("%s group has %d part(s) at 180°/270° (%s) — legitimate only when pin-1/polarity demands it; the API exposes no such intent, so verify by hand before flipping%s",
					g.key(), len(oddNames), tidyNames(oddNames), tidyConvRule("9.3", "旋转只取 90° 步进")),
			})
		}
	}
	if population == 0 {
		return r.skip("没有 ≥%d 件、焊盘数 ≥2、且含未锁定件的同类组（旋转对 ≤1 焊盘的件不可观测）", pcbTidyMinGroup)
	}
	r.scale(100 / float64(population))
	r.metrics["rotationKinds"] = float64(maxKinds)
	r.metrics["rotationOutliers"] = float64(outliers)
	r.metrics["rotationOddCount"] = float64(odd)
	r.metrics["rotationPopulation"] = float64(population)
	return r.settle()
}

// tidyNormalizeRot 把角度归一到 [0,360) 并把 ±1° 内的值吸到 90° 步进。真正的自由角
// （§9.3 硬禁）不会被吸走，会自成一类被「>2 种朝向」抓住。
func tidyNormalizeRot(deg float64) float64 {
	v := math.Mod(deg, 360)
	if v < 0 {
		v += 360
	}
	for _, step := range []float64{0, 90, 180, 270, 360} {
		if math.Abs(v-step) <= pcbTidyRotSnapDeg {
			return math.Mod(step, 360)
		}
	}
	return math.Round(v*10) / 10
}

// tidyTopRotations 取出现次数最多的 n 种朝向（并列时取角度小的，保证输出确定）。
func tidyTopRotations(counts map[float64]int, n int) map[float64]bool {
	type kv struct {
		rot float64
		n   int
	}
	list := make([]kv, 0, len(counts))
	for rot, c := range counts {
		list = append(list, kv{rot, c})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].rot < list[j].rot
	})
	out := map[float64]bool{}
	for i, kv := range list {
		if i >= n {
			break
		}
		out[kv.rot] = true
	}
	return out
}

func tidyRotSet(set map[float64]bool) string {
	var rots []float64
	for r := range set {
		rots = append(rots, r)
	}
	sort.Float64s(rots)
	parts := make([]string, 0, len(rots))
	for _, r := range rots {
		parts = append(parts, fmt.Sprintf("%g°", r))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func tidyRotHistogram(counts map[float64]int) string {
	var rots []float64
	for r := range counts {
		rots = append(rots, r)
	}
	sort.Float64s(rots)
	parts := make([]string, 0, len(rots))
	for _, r := range rots {
		parts = append(parts, fmt.Sprintf("%g°:%d", r, counts[r]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// ---------------------------------------------------------------------------
// ③ silk-side-inconsistent
// ---------------------------------------------------------------------------

// tidyScoreSilkSide 判位号相对本体的方位在同类里是否统一。
//
// 方位 = 丝印中心减本体中心，取偏移绝对值大的那个轴（y-UP：+y 是 top）。丝印中心
// 优先用连接器给的真实 bbox；没有就从**左下 anchor** + fontSize 估（#155：X/Y 不是
// 中心，把估算框中心压在 anchor 上会整体偏半个字，制造假阳性）。
//
// 只报不修：`silk-align --side` 是碰撞规避工具，--side 是 soft hint，实测 69 件里
// 只动了 10 件、方位分布纹丝不动（#153）。所以 finding 里不承诺「跑一下就好」。
//
// 注意 locked 在这一条**不豁免**：锁的是器件位置，位号文本是独立图元，`pcb silk-set`
// 照样能挪 —— 与 off-grid/rotation/array（锁了就动不了）不是一回事。
func tidyScoreSilkSide(snap *boardSnapshot, groups []tidyGroup) *tidyRule {
	r := newTidyRule(tidyRuleSilkSide)
	if len(snap.Silk) == 0 {
		return r.skip("快照里没有丝印数据（PCB 非前台或连接器未返回 pcb.silk.list）")
	}
	byComp := snap.silkByComponent()
	sideCount := map[apEdge]int{}
	resolved, agree := 0, 0
	for _, g := range groups {
		type sided struct {
			c    boardComp
			side apEdge
		}
		var members []sided
		for _, c := range g.members {
			t, ok := tidyDesignatorSilk(c, byComp[c.ID])
			if !ok {
				continue
			}
			sx, sy, ok := tidySilkCenter(t)
			if !ok {
				continue
			}
			cx, cy := c.center()
			side, ok := tidySilkSideOf(cx, cy, sx, sy)
			if !ok {
				continue // 丝印几乎压在本体正中：判不出方位，不参与多数派
			}
			members = append(members, sided{c, side})
			sideCount[side]++
		}
		if len(members) < pcbTidyMinGroup {
			continue
		}
		counts := map[apEdge]int{}
		for _, m := range members {
			counts[m.side]++
		}
		major := tidyMajoritySide(counts)
		resolved += len(members)
		var offNames []string
		for _, m := range members {
			if m.side == major {
				agree++
				continue
			}
			offNames = append(offNames, fmt.Sprintf("%s(%s)", m.c.Designator, m.side))
			r.charge(m.c.Designator, 1, fmt.Sprintf("位号在本体 %s 侧，%s 组多数派是 %s", m.side, g.key(), major))
		}
		if len(offNames) > 0 {
			r.findings = append(r.findings, pcbCheckFinding{
				Type: tidyRuleSilkSide, Level: "WARN", Layer: silkTopLayer,
				Message: fmt.Sprintf("%s group (n=%d): %d designator(s) sit on a different side than the majority %q — %s. NOTE: `pcb silk-align --side` is a COLLISION-AVOIDANCE tool and its --side is a soft hint (#153 measured 69 parts in, only 10 moved, side distribution unchanged), so this needs `pcb silk-set` per part; re-run `pcb check` afterwards — silk-align has been observed creating fresh silk-over-pad findings%s",
					g.key(), len(members), len(offNames), major, tidyNames(offNames),
					tidyConvRule("9.4", "丝印——同类件位号放同一相对侧")),
			})
		}
	}
	if resolved == 0 {
		return r.skip("没有 ≥%d 件、且位号丝印方位可判的同类组", pcbTidyMinGroup)
	}
	r.scale(100 / float64(resolved))
	r.metrics["silkSideMajorityRatio"] = math.Round(float64(agree)/float64(resolved)*1000) / 1000
	r.metrics["silkSideResolved"] = float64(resolved)
	// 四向分布是 #153 表格的原样复刻，给人一眼判读「四面开花」的程度。
	r.metrics["silkSideLeft"] = float64(sideCount[edgeLeft])
	r.metrics["silkSideRight"] = float64(sideCount[edgeRight])
	r.metrics["silkSideTop"] = float64(sideCount[edgeTop])
	r.metrics["silkSideBottom"] = float64(sideCount[edgeBottom])
	return r.settle()
}

// tidyDesignatorSilk 找出一个器件的位号丝印：优先 Designator 属性，退回「文本等于
// 位号」的自由串（老连接器不给 key 时的兜底）。
func tidyDesignatorSilk(c boardComp, texts []pcbSilkText) (pcbSilkText, bool) {
	for _, t := range texts {
		if t.Kind == "attribute" && strings.EqualFold(t.Key, "Designator") {
			return t, true
		}
	}
	for _, t := range texts {
		if strings.EqualFold(strings.TrimSpace(t.Text), strings.TrimSpace(c.Designator)) {
			return t, true
		}
	}
	return pcbSilkText{}, false
}

// tidySilkCenter 是丝印文本的中心。有真实 bbox 就用它；没有就照 findSilkOverPad 的
// 估算法从左下 anchor 推（半宽 = 字数×字高×0.6/2，半高 = 字高/2，90/270 交换）。
func tidySilkCenter(t pcbSilkText) (float64, float64, bool) {
	if t.BBox != nil {
		return (t.BBox.MinX + t.BBox.MaxX) / 2, (t.BBox.MinY + t.BBox.MaxY) / 2, true
	}
	txt := strings.TrimSpace(t.Text)
	if txt == "" {
		return 0, 0, false
	}
	fh := t.FontSize
	if fh <= 0 {
		fh = pcbSilkEstH
	}
	hw := float64(len([]rune(txt))) * fh * pcbSilkCharAsp / 2
	hh := fh / 2
	if rot := math.Mod(math.Abs(t.Rotation), 180); rot > 45 && rot < 135 {
		hw, hh = hh, hw
	}
	return t.X + hw, t.Y + hh, true
}

// tidySilkSideOf 把「丝印中心相对本体中心」的偏移归到四个方位（y-UP：+y = top）。
// 复用 apEdge 而不是自造 left/right 串：auto-place / place-constrained / silk-align
// 已经在用这套词汇，String() 出来的正是 `pcb silk-align --side` 收的 token。
func tidySilkSideOf(cx, cy, sx, sy float64) (apEdge, bool) {
	dx, dy := sx-cx, sy-cy
	if math.Hypot(dx, dy) < pcbTidySideDeadzoneMil {
		return edgeLeft, false
	}
	if math.Abs(dx) > math.Abs(dy) {
		if dx < 0 {
			return edgeLeft, true
		}
		return edgeRight, true
	}
	if dy > 0 {
		return edgeTop, true
	}
	return edgeBottom, true
}

// tidyMajoritySide 取多数派方位（并列时按 apEdge 序，保证输出确定）。
func tidyMajoritySide(counts map[apEdge]int) apEdge {
	best, bestN := edgeLeft, -1
	for _, e := range []apEdge{edgeLeft, edgeRight, edgeTop, edgeBottom} {
		if counts[e] > bestN {
			best, bestN = e, counts[e]
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// ④ silk-style-inconsistent
// ---------------------------------------------------------------------------

// tidyScoreSilkStyle 判同 Key 的丝印字号是否统一。
//
// #153 的实测就是这条最好的例子：Designator {32:68, **45:1**}、自由字符串
// {30:13, **32:1**} —— 一两个漏网件，`silk-set --font-size` 一条命令就能修，
// **纯粹缺一个规则去发现它**。
//
// lineWidth 这一版不做：`pcb.silk.list` 目前不返回它（只在 silk-add/silk-set 的入参
// 里），要覆盖得改连接器。fontSize 版本零连接器改动。
func tidyScoreSilkStyle(snap *boardSnapshot) *tidyRule {
	r := newTidyRule(tidyRuleSilkStyle)
	if len(snap.Silk) == 0 {
		return r.skip("快照里没有丝印数据（PCB 非前台或连接器未返回 pcb.silk.list）")
	}
	// CompID → 位号，用于把离群文本归因到器件；归不到就用 silk:<id> 当伪位号，
	// 保住「Σ 归因 == 扣分」的不变式，也让精修环知道要动哪个图元。
	desOf := map[string]string{}
	for _, c := range snap.Components {
		desOf[c.ID] = c.Designator
	}
	buckets := map[string][]pcbSilkText{}
	var keys []string
	for _, t := range snap.Silk {
		if t.FontSize <= 0 {
			continue // 老连接器不给 fontSize：无从判起，不能当成 0mil
		}
		k := "string"
		if t.Kind == "attribute" && t.Key != "" {
			k = "attr:" + t.Key
		}
		if _, seen := buckets[k]; !seen {
			keys = append(keys, k)
		}
		buckets[k] = append(buckets[k], t)
	}
	sort.Strings(keys)
	population, outliers := 0, 0
	for _, k := range keys {
		texts := buckets[k]
		if len(texts) < pcbTidyMinGroup {
			continue
		}
		population += len(texts)
		mode := tidyModeFont(texts)
		var offIDs []string
		for _, t := range texts {
			if math.Abs(t.FontSize-mode) <= pcbTidyFontTolMil {
				continue
			}
			outliers++
			offIDs = append(offIDs, fmt.Sprintf("%s(%gmil)", t.ID, t.FontSize))
			des := desOf[t.CompID]
			if des == "" {
				des = "silk:" + t.ID
			}
			r.charge(des, 1, fmt.Sprintf("丝印 %s 字号 %gmil，%s 组多数派 %gmil", t.ID, t.FontSize, k, mode))
		}
		if len(offIDs) > 0 {
			r.findings = append(r.findings, pcbCheckFinding{
				Type: tidyRuleSilkStyle, Level: "WARN",
				Primitives: tidyPrimitiveIDs(texts, mode),
				Message: fmt.Sprintf("silk group %q (n=%d) has %d font-size outlier(s) against the %gmil majority: %s — fix with `pcb silk-set --font-size %g`, then re-run `pcb check` (silk edits have been observed creating fresh silk-over-pad findings)%s",
					k, len(texts), len(offIDs), mode, tidyNames(offIDs), mode,
					tidyConvRule("9.4", "丝印——字高与归属")),
			})
		}
	}
	if population == 0 {
		return r.skip("没有 ≥%d 条、且带 fontSize 的同 key 丝印（连接器可能未返回字号）", pcbTidyMinGroup)
	}
	r.scale(100 / float64(population))
	r.metrics["fontSizeOutliers"] = float64(outliers)
	r.metrics["silkTextsStyled"] = float64(population)
	return r.settle()
}

// tidyModeFont 取一组丝印里出现次数最多的字号（并列取小的，保证输出确定）。
func tidyModeFont(texts []pcbSilkText) float64 {
	counts := map[float64]int{}
	for _, t := range texts {
		counts[math.Round(t.FontSize*10)/10]++
	}
	best, bestN := 0.0, -1
	var sizes []float64
	for s := range counts {
		sizes = append(sizes, s)
	}
	sort.Float64s(sizes)
	for _, s := range sizes {
		if counts[s] > bestN {
			best, bestN = s, counts[s]
		}
	}
	return best
}

// tidyPrimitiveIDs 收集离群文本的 primitiveId（finding 里要「报离群的那几个 id」）。
func tidyPrimitiveIDs(texts []pcbSilkText, mode float64) []string {
	var ids []string
	for _, t := range texts {
		if math.Abs(t.FontSize-mode) > pcbTidyFontTolMil && t.ID != "" {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// ---------------------------------------------------------------------------
// ⑤ array-irregular
// ---------------------------------------------------------------------------

// tidyRun 是一串成行/成列的同类件（沿 axis 排序）。
type tidyRun struct {
	axis    string // "x" = 横行（共用一条 Y）；"y" = 竖列（共用一条 X）
	members []boardComp
	pos     []float64 // 沿 axis 的中心坐标，与 members 同序
}

// tidyScoreArray 判成排同类件的步进是否统一。
//
// 「阵列」用 conventions §9.2 的定义抽取而不是拿两两近邻距硬聚类：同前缀同面的件，
// 共用一条轴（另一轴 ±2mil 内）、且相邻步距 ≤500mil 的连续段，长度 ≥3。这样
// §9.2 的「行内中心距恒定 ±5mil」才是一条可引用的阈值，而不是自己拍的容差。
//
// 离群判定用「中位步距 + 中位截距」的残差，而不是逐段比较：单个错位件用逐段比较会
// 连累它后面所有件（步距一错错两段），用拟合线只有它自己残差超标。#153 的
// 199.1 / 200 / 205.2 正是这种「一件差一点」。
func tidyScoreArray(groups []tidyGroup) *tidyRule {
	r := newTidyRule(tidyRuleArray)
	member := map[string]bool{}
	offenders := map[string]bool{}
	runs := 0
	for _, g := range groups {
		if len(g.members) < pcbTidyMinGroup {
			continue
		}
		for _, run := range append(tidyRunsAlong(g.members, "x"), tidyRunsAlong(g.members, "y")...) {
			runs++
			for _, c := range run.members {
				// 分母只算未锁定件：锁定件定义了阵列（它们参与步距统计），但动不了，
				// 扣它的分等于把一个不可修的事实算进「可修的散乱」。
				if !c.Locked {
					member[c.Designator] = true
				}
			}
			bad, step := tidyRunOutliers(run)
			if len(bad) == 0 {
				continue
			}
			var names []string
			for _, i := range bad {
				c := run.members[i]
				offenders[c.Designator] = true
				names = append(names, c.Designator)
				if !c.Locked {
					r.charge(c.Designator, 1, fmt.Sprintf("%s 组沿 %s 的阵列步距应为 %.1fmil，此件偏离 >%.0fmil",
						g.key(), run.axis, step, pcbTidyArrayStepTolMil))
				}
			}
			r.findings = append(r.findings, pcbCheckFinding{
				Type: tidyRuleArray, Level: "WARN",
				Designator: run.members[bad[0]].Designator,
				Message: fmt.Sprintf("%s group: a %d-part run along %s has an irregular step (median %.1fmil, %s off by >%.0fmil) — a row/column must keep a constant centre pitch; `pcb align` + `pcb distribute` on the run fixes it%s",
					g.key(), len(run.members), run.axis, step, tidyNames(names), pcbTidyArrayStepTolMil,
					tidyConvRule("9.2", "同类件成行对齐 + 等距")),
			})
		}
	}
	if len(member) == 0 {
		return r.skip("板上没有 ≥%d 件成行/成列（同轴 ±%gmil）、且含未锁定件的同类阵列", pcbTidyMinGroup, pcbTidyArrayAxisTolMil)
	}
	r.scale(100 / float64(len(member)))
	r.metrics["arrayIrregularCount"] = float64(len(offenders))
	r.metrics["arrayRuns"] = float64(runs)
	r.metrics["arrayMembers"] = float64(len(member))
	return r.settle()
}

// tidyRunsAlong 沿给定轴抽取「共轴且连续」的段。axis=="x" 表示成横行（Y 相同）。
func tidyRunsAlong(members []boardComp, axis string) []tidyRun {
	type pt struct {
		c             boardComp
		along, across float64
	}
	pts := make([]pt, 0, len(members))
	for _, c := range members {
		x, y := c.center()
		if axis == "x" {
			pts = append(pts, pt{c, x, y})
		} else {
			pts = append(pts, pt{c, y, x})
		}
	}
	sort.SliceStable(pts, func(i, j int) bool {
		if pts[i].across != pts[j].across {
			return pts[i].across < pts[j].across
		}
		return pts[i].along < pts[j].along
	})
	var out []tidyRun
	for i := 0; i < len(pts); {
		j := i + 1
		for j < len(pts) && math.Abs(pts[j].across-pts[i].across) <= pcbTidyArrayAxisTolMil {
			j++
		}
		band := pts[i:j]
		i = j
		if len(band) < pcbTidyMinGroup {
			continue
		}
		sort.SliceStable(band, func(a, b int) bool { return band[a].along < band[b].along })
		// 大间隙切段：超过 pcbTidyArrayMaxStepMil 的相邻距离说明这是两簇碰巧共线的
		// 件，不是一个阵列，硬当成一列会把「两簇之间的巨大间距」算进步距统计。
		start := 0
		for k := 1; k <= len(band); k++ {
			if k == len(band) || band[k].along-band[k-1].along > pcbTidyArrayMaxStepMil {
				if seg := band[start:k]; len(seg) >= pcbTidyMinGroup {
					run := tidyRun{axis: axis}
					for _, p := range seg {
						run.members = append(run.members, p.c)
						run.pos = append(run.pos, p.along)
					}
					out = append(out, run)
				}
				start = k
			}
		}
	}
	return out
}

// tidyRunOutliers 返回步进离群的成员下标 + 中位步距。
func tidyRunOutliers(run tidyRun) ([]int, float64) {
	if len(run.pos) < pcbTidyMinGroup {
		return nil, 0
	}
	steps := make([]float64, 0, len(run.pos)-1)
	for i := 1; i < len(run.pos); i++ {
		steps = append(steps, run.pos[i]-run.pos[i-1])
	}
	step := tidyMedian(steps)
	if step <= 0 {
		return nil, 0
	}
	// 截距取 median(pos_i − i·step)：单个错位件只有它自己残差大，不会把后面所有件
	// 一起带偏（用 pos_0 当截距就会）。
	offs := make([]float64, len(run.pos))
	for i, p := range run.pos {
		offs[i] = p - float64(i)*step
	}
	base := tidyMedian(offs)
	var bad []int
	for i, p := range run.pos {
		if math.Abs(p-(base+float64(i)*step)) > pcbTidyArrayStepTolMil {
			bad = append(bad, i)
		}
	}
	return bad, step
}

// tidyMedian 是中位数（空切片返回 0）。
func tidyMedian(vs []float64) float64 {
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
// 杂项
// ---------------------------------------------------------------------------

// tidyNames 把点名列表收口成 "A, B, C +N more"，避免 finding 变成一屏位号。
func tidyNames(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	if len(names) <= pcbTidyMaxNamed {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:pcbTidyMaxNamed], ", ") + fmt.Sprintf(" +%d more", len(names)-pcbTidyMaxNamed)
}
