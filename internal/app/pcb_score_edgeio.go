package app

// pcb_score_edgeio.go —— layout-score 的「对外接口与板沿」维（dimEdgeIO，#167）。
//
// 这一维回答三个问题，全部围绕**板外沿这块稀缺资源被谁占了、占得对不对**：
//
//	① 对外口是不是聚在一条边、开口朝外？   —— 散在四条边的板子，外壳要开四面孔，
//	                                        线束在箱内绕一圈，装配成本直接翻倍。
//	② 内部件有没有占着外沿？（#168 规则①）—— 占了就是把可达边让给了不出线的件。
//	③ 相邻对外口的插头会不会打架？（#168 规则②）—— footprint 不重叠 ≠ 插头插得进去。
//
// 三件事共用 collectBoardConnectors 的一次判读，规则本体在 pcb_check_connector.go
// （纯函数，`pcb check` 也消费同一份），这里只负责**把违规翻译成分数和归因梯度**。
//
// 三条硬约定在这一维的落法：
//   - 板框读不到 / 板上根本没有连接器 → skipDimension，绝不返回 100。一块没有任何
//     对外口的板拿满分会让「好板必须得高分」的校准判据失效。
//   - 板框只有 AABB（Source != "polygon"）→ 照算，但标 degraded：异形板上「到板边
//     距离」会系统性算错（Type-C 突出部位的件明明贴边，AABB 看却离边很远）。
//     插头包络走 bbox 兜底时同理。
//   - Contributors 必须填满：精修环靠「先动谁」的梯度派活，只有分数没有归因等于没做。

import (
	"fmt"
	"strings"
)

// 扣分表（0-100 尺度）。全部是**待校准初值**，定序原则是「工艺/装配后果 > 意图违背 >
// 观感」：插不进去和插头打架是板子做出来直接报废的量级，摆错边只是抬高装配成本。
//
// #167 第五层要求拿一批公认的好板跑分来校准这些数：好板在这一维掉分，就是这张表或
// 判据错了，回来改这里而不是改板子。
const (
	// 对外口压根不贴板边（停在板中央）—— 最重的意图违背：外壳根本没法给它开孔。
	edgeIOOffEdgePenalty = 20.0
	// 插头护套打架 —— 有真实工艺后果（装配时插不进去），仅次于完全没贴边。
	edgeIOPlugPenalty = 18.0
	// spec 显式声明的内部件占外沿 —— 板级决定被违背，可信度高。
	edgeIOInternalSpecPenalty = 15.0
	// 连接器开口朝板内 —— 线只能从板内绕出来，块库声明了开口才判得了。
	edgeIOOpeningInwardPenalty = 15.0
	// 对外口散在非主边 —— 抬高外壳/线束成本，但板子仍然能用。
	edgeIOStrayEdgePenalty = 12.0
	// 启发式推定的内部件占外沿 —— 判据可能错（接箱外传感器的 XH 座也长这样），
	// 扣分刻意压到 spec 档的 40%，与它只报 INFO 的严重度一致。
	edgeIOInternalHeurPenalty = 6.0
	// 开口平行板边（既没朝外也没朝内，多半是转错了 90°）—— 半档。
	edgeIOOpeningSidewaysPenalty = 7.5
)

type edgeIOScorer struct{}

func init() { registerDimScorer(edgeIOScorer{}) }

func (edgeIOScorer) id() string { return dimEdgeIO }

func (edgeIOScorer) score(ctx *scoreCtx) scoreDimension {
	o := ctx.outline()
	if o == nil {
		// 「到板边多远」是这一维每条判据的分母。没有板框就一条都算不了 —— 报 skipped
		// 并说清怎么补（PCB 切前台再拉一次快照），而不是给个满分糊弄过去。
		return skipDimension(dimEdgeIO, ctx.opts,
			"board outline unavailable (pcb.outline.get returns null when the PCB is not the foreground document) — every edge-I/O judgement is a distance to the board rim, so none of them can be computed")
	}
	conns := collectBoardConnectors(ctx.snap, ctx.spec)
	if len(conns) == 0 {
		return skipDimension(dimEdgeIO, ctx.opts,
			"no connector on the board — there is no external interface to place, so this dimension has nothing to measure (a 100 here would be indistinguishable from a well-arranged I/O edge)")
	}

	// 参评「聚一条边」的集合：对外口，且排除 edge="any" 的 RF/天线座 —— 它们必须在
	// *某*条边但哪条都行，把它们算进来会因为天线单独占一条边就误扣一大截。
	var facing []boardConnector
	internalCount, extCount := 0, 0
	for _, b := range conns {
		if b.isInternal() {
			internalCount++
			continue
		}
		extCount++
		if b.role != "any" {
			facing = append(facing, b)
		}
	}
	// 三个子判据一个都没有主体时才算真的测不了（例：板上只有一个 IPEX 天线座）。
	if len(facing) == 0 && internalCount == 0 && extCount < 2 {
		return skipDimension(dimEdgeIO, ctx.opts,
			"the board's only connector is edge-agnostic (an RF/antenna socket, block-declared edge=\"any\") — no I/O grouping to judge, no internal port on the rim, and no adjacent pair to check")
	}

	d := newDimension(dimEdgeIO, ctx.opts)
	penalty := map[string]float64{}
	details := map[string][]string{}
	charge := func(des string, p float64, format string, args ...any) {
		penalty[des] += p
		details[des] = append(details[des], fmt.Sprintf(format, args...))
	}

	// ── ① 对外口聚一条边 + 开口朝外 ──────────────────────────────────────────
	onEdge := map[apEdge][]boardConnector{}
	var offEdge []boardConnector
	for _, b := range facing {
		if b.hasEdge && b.edgeGapMil < pcbConnEdgeBandMil {
			onEdge[b.edge] = append(onEdge[b.edge], b)
		} else {
			offEdge = append(offEdge, b)
		}
	}
	// 主边 = 贴边对外口最多的那条。并列时按 left→right→top→bottom 的固定顺序取第一条：
	// 谁当主边在并列时本来就没有物理依据，固定顺序至少保证同一块板每次跑出同一个答案
	// （打分要能进 golden 回归）。
	dominant, dominantN := edgeLeft, 0
	for _, e := range []apEdge{edgeLeft, edgeRight, edgeTop, edgeBottom} {
		if n := len(onEdge[e]); n > dominantN {
			dominant, dominantN = e, n
		}
	}
	strayN, inwardN, sidewaysN, openingUnknown := 0, 0, 0, 0
	for _, b := range offEdge {
		charge(b.comp.Designator, edgeIOOffEdgePenalty,
			"external port sits %.0fmil from the nearest board edge (rim band is %.0fmil)", round2(b.edgeGapMil), pcbConnEdgeBandMil)
		d.Findings = append(d.Findings, edgeIOFinding("external-port-off-edge", "WARN", b,
			fmt.Sprintf("%s is an external port but sits %.0fmil (%.2fmm) inboard of the %s edge — an enclosure cannot open a hole to it there",
				b.comp.Designator, round2(b.edgeGapMil), round2(b.edgeGapMil/mmToMil), b.edge.String())))
	}
	for _, e := range []apEdge{edgeLeft, edgeRight, edgeTop, edgeBottom} {
		for _, b := range onEdge[e] {
			if e != dominant {
				strayN++
				charge(b.comp.Designator, edgeIOStrayEdgePenalty,
					"external port on the %s edge while %d other port(s) group on the %s edge", e.String(), dominantN, dominant.String())
				d.Findings = append(d.Findings, edgeIOFinding("external-port-scattered", "INFO", b,
					fmt.Sprintf("%s is on the %s edge while %d external port(s) group on the %s edge — one I/O edge means one enclosure cutout and one harness run",
						b.comp.Designator, e.String(), dominantN, dominant.String())))
			}
			// 开口朝向：块库声明了这个封装的**局部**开口方向才判得了（对称的 2P 端子
			// 从铜箔几何里根本看不出开口朝哪，这正是 blocks 的 openings 存在的理由）。
			lox, loy, ok := connOpeningFor(b.comp.Device)
			if !ok {
				openingUnknown++
				continue
			}
			wx, wy := rotate2d(lox, loy, b.comp.Rotation)
			ix, iy := edgeInteriorDir(e)
			dot := wx*(-ix) + wy*(-iy) // 与「离板方向」的一致度：+1 朝外，-1 朝内
			switch {
			case dot < -0.1:
				inwardN++
				charge(b.comp.Designator, edgeIOOpeningInwardPenalty, "connector opening faces INTO the board on the %s edge", e.String())
				d.Findings = append(d.Findings, edgeIOFinding("connector-opening-inward", "WARN", b,
					fmt.Sprintf("%s sits on the %s edge with its opening facing into the board (rotation %.0f°) — the mating cable has to come from inside the enclosure",
						b.comp.Designator, e.String(), b.comp.Rotation)))
			case dot < 0.1:
				sidewaysN++
				charge(b.comp.Designator, edgeIOOpeningSidewaysPenalty, "connector opening runs parallel to the %s edge", e.String())
				// 单列一个 type 而不是复用 inward：下游（精修环 / playbook assert）
				// 要能只挑"转错 90°"这一类来批量纠正，混在一个 type 里就筛不出来。
				d.Findings = append(d.Findings, edgeIOFinding("connector-opening-sideways", "INFO", b,
					fmt.Sprintf("%s sits on the %s edge with its opening parallel to that edge (rotation %.0f°) — most likely rotated 90° off",
						b.comp.Designator, e.String(), b.comp.Rotation)))
			}
		}
	}

	// ── ② internal-on-edge（#168 规则①）──────────────────────────────────────
	internalFindings := findInternalOnEdge(conns, o)
	internalSpecN := 0
	for _, f := range internalFindings {
		p := edgeIOInternalHeurPenalty
		if f.Level == "WARN" { // WARN ⇔ spec 显式声明（见 findInternalOnEdge）
			p = edgeIOInternalSpecPenalty
			internalSpecN++
		}
		charge(f.Designator, p, "internal connector occupies the board rim")
	}
	d.Findings = append(d.Findings, internalFindings...)

	// ── ③ connector-plug-clearance（#168 规则②）──────────────────────────────
	conflicts := connectorPlugConflicts(conns, o)
	for _, c := range conflicts {
		// 一对冲突两边**各担一半**：这样 Σ(contributor.Penalty) 恰好等于本维扣掉的
		// 总分，精修环拿归因排序时不会因为重复计数把某个器件排到不该有的位置。
		charge(c.a.comp.Designator, edgeIOPlugPenalty/2,
			"plug envelope collides with %s (%.2fmm apart, needs %.2fmm)", c.b.comp.Designator, round2(c.distMil/mmToMil), round2(c.needMil/mmToMil))
		charge(c.b.comp.Designator, edgeIOPlugPenalty/2,
			"plug envelope collides with %s (%.2fmm apart, needs %.2fmm)", c.a.comp.Designator, round2(c.distMil/mmToMil), round2(c.needMil/mmToMil))
	}
	d.Findings = append(d.Findings, findConnectorPlugClearance(conns, o)...)

	// ── 汇总 ────────────────────────────────────────────────────────────────
	total := 0.0
	var contribs []scoreContributor
	for des, p := range penalty {
		total += p
		contribs = append(contribs, scoreContributor{
			Designator: des, Penalty: round2(p), Detail: strings.Join(details[des], "; "),
		})
	}
	d.Contributors = sortContributors(contribs)
	d.Score = clampScore(100 - total)

	plugFallback, plugUnknown := 0, 0
	for _, b := range conns {
		switch b.plugSrc {
		case "fallback":
			plugFallback++
		case "unknown":
			plugUnknown++
		}
	}
	concentration := 0.0
	if n := len(facing); n > 0 {
		concentration = float64(dominantN) / float64(n)
	}
	d.Metrics = map[string]float64{
		"connectors":         float64(len(conns)),
		"external":           float64(extCount),
		"internal":           float64(internalCount),
		"groupingCandidates": float64(len(facing)),
		"onDominantEdge":     float64(dominantN),
		"strayEdge":          float64(strayN),
		"offEdge":            float64(len(offEdge)),
		"edgeConcentration":  round2(concentration),
		"openingInward":      float64(inwardN),
		"openingSideways":    float64(sidewaysN),
		"openingUnknown":     float64(openingUnknown),
		"internalOnEdge":     float64(len(internalFindings)),
		"internalOnEdgeSpec": float64(internalSpecN),
		"plugConflicts":      float64(len(conflicts)),
		"plugWidthFallback":  float64(plugFallback),
		"plugWidthUnknown":   float64(plugUnknown),
		"edgeBandMil":        pcbConnEdgeBandMil,
		"outlinePolygon":     boolMetric(o.Source == "polygon"),
	}

	// degraded：算出来了，但输入是近似 —— 必须说出来，否则读的人会把近似当实测。
	var deg []string
	if o.Source != "polygon" {
		deg = append(deg, "board outline is an AABB approximation (no polygon from the connector) — on a non-rectangular board the distance-to-rim is wrong exactly where it matters (a part on a Type-C notch reads as far from the edge)")
	}
	if plugFallback > 0 {
		deg = append(deg, fmt.Sprintf("%d connector(s) have no plug-envelope table entry — their envelope is the rendered bbox + %.0fmil, an estimate (add a row to internal/blocks/data/_plug_envelope.json to make it real)", plugFallback, pcbPlugFallbackMarginMil))
	}
	if plugUnknown > 0 {
		deg = append(deg, fmt.Sprintf("%d connector(s) have neither a table entry nor a rendered bbox — they were left out of the plug-clearance check entirely", plugUnknown))
	}
	if openingUnknown > 0 {
		deg = append(deg, fmt.Sprintf("%d on-edge connector(s) have no block-declared opening direction — \"opening faces out\" could not be checked for them (a symmetric 2P terminal's opening is not in its copper)", openingUnknown))
	}
	if len(deg) > 0 {
		d.Status = dimDegraded
		d.Reason = strings.Join(deg, "; ")
	}
	return d
}

// edgeIOFinding 是本维自产 finding 的构造助手（规则①②的 finding 由规则本体产出）。
func edgeIOFinding(kind, level string, b boardConnector, msg string) pcbCheckFinding {
	cx, cy := b.comp.center()
	f := pcbCheckFinding{
		Type: kind, Level: level,
		Designator: b.comp.Designator,
		Message:    msg + docRule("3.5", "对外接口与板沿"),
		At:         &pcbXY{X: round2(cx), Y: round2(cy)},
	}
	if b.comp.ID != "" {
		f.Primitives = []string{b.comp.ID}
	}
	return f
}

// boolMetric 把布尔量塞进 Metrics（map[string]float64 装不下 bool，但「板框是不是
// 真多边形」正是判读这一维可信度的第一个问题，必须出现在原始量里）。
func boolMetric(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
