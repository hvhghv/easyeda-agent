package app

// cmd_sch_layoutscore.go — `easyeda sch layout-score`:原理图布局质量打分(用户立项)。
//
// 立项背景:原理图侧只有 `sch layout-lint`(重叠/间距硬门)和 `sch check`(电气 +
// marker 几何),**没有布局质量打分** —— "标签折叠"、"标签背向折返(反向)"、"外围件
// 离核心半张纸远"、"长链散乱"这些可读性缺陷零量化,规划器产出难看布局时无机制感知。
// PCB 侧九维 `pcb layout-score` 已验证「维度分 + 逐器件归因 + verdict」模式,这里是
// 它的原理图对应物,并强化一点:**每条归因都带可直接执行的修复命令**(fix 字段填好
// 真实位号/坐标,AI 照抄执行)。
//
// 与既有命令的分工(**不抢门**):
//
//	layout-lint  = 放置硬门(overlap / pin-coincidence / off-grid / zone)。
//	sch check    = 电气 + marker 几何缺陷(折叠在这里也报 warn,但不打分不归因)。
//	layout-score = 五维各 0-100 + 加权综合 + 逐条归因带 fix。诊断视角:无
//	               --min-score 时永远 exit 0;显式给了才在综合分不足时非零退出。
//
// 沿用 PCB 侧两条铁律:
//  1. 「没测」和「测了满分」必须可区分 —— 算不了的维 status=skipped、不参与加权、
//     写明原因,绝不默认满分(frame-fit 的分区框几何当前取不到,就是 skipped)。
//  2. verdict 单一产出点(schScoreVerdict),分数与判定不分家。
//
// 数据来源与 `sch check` 的 marker 规则相同:schematic.components.list
// {includeBBox,includePins} → parseLayoutComps;纯核 analyzeSchLayoutScore 无 I/O,
// 可用 ceshi 真实几何做表驱动测试。

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// 维度身份
// ---------------------------------------------------------------------------

// 维度 id:对外契约(JSON key),改名等于破坏用户脚本。
const (
	schDimFolded    = "folded-labels"   // netport 竖排折叠(复用 sch check 的判据)
	schDimReversed  = "reversed-labels" // netport 朝向背离宿主→核心方向(用户点名)
	schDimProximity = "proximity"       // 无源小件贴核心件
	schDimTidiness  = "stub-tidiness"   // 长链跨度 + 同排标签挤压
	schDimFrameFit  = "frame-fit"       // 说明文字不压电路;分区框内不外溢
)

var schScoreDimOrder = []string{schDimFolded, schDimReversed, schDimProximity, schDimTidiness, schDimFrameFit}

var schScoreDimTitles = map[string]string{
	schDimFolded:    "标签折叠",
	schDimReversed:  "标签反向",
	schDimProximity: "外围贴核心",
	schDimTidiness:  "长链/挤压",
	schDimFrameFit:  "版面整洁",
}

// schScoreDimWeights:可读性硬伤(折叠/反向)> 布局结构(贴核心)> 观感(长链/版面)。
var schScoreDimWeights = map[string]float64{
	schDimFolded:    1.2,
	schDimReversed:  1.2,
	schDimProximity: 1.0,
	schDimTidiness:  0.8,
	schDimFrameFit:  0.5,
}

// 维度状态(语义同 pcb layout-score)。
const (
	schDimScored  = "scored"
	schDimSkipped = "skipped"
)

// ---------------------------------------------------------------------------
// 判据常量(全部机械可算;来源见各注释)
// ---------------------------------------------------------------------------

const (
	// schScoreHostPinMaxDist:netport/netflag 通过 stub 挂在器件 pin 上,connect_pin
	// 默认 offset 24~40 → 60 覆盖常规 stub 长度而不会跨到邻件的脚。
	schScoreHostPinMaxDist = 60.0
	// proximity 线性衰减:边距 ≤150 满分,≥500 记 0。
	schScoreProximityFull = 150.0
	schScoreProximityZero = 500.0
	// stub-tidiness:小件 + 两端 flag 的链跨度上限;超过 = 长链。
	schScoreChainSpanMax = 250.0
	// 只有长轴 ≤ 此值的小件参与长链判定 —— 大 IC 本体就超 250,不是"链"。
	schScoreChainPartMaxAxis = 100.0
	// 同排两个相向水平 netport 标签所需最小间距(本项目实测值)。
	schScoreRowMinGap = 117.0
	// fix 命令里 connect 的建议 offset(与 skill 默认一致)。
	schScoreConnectOffset = 24
	// 核心件资格的最小 bbox 面积:核心只能是"大件"(IC/模块/连接器)。live 误报
	// 根因之一:U2 的核心兜底选到了电容 C5,把全板核心自己的引脚标签判了反向。
	schScoreCoreMinArea = 1500.0
	// 宿主导线匹配:pin 与 wire 端点都吸在 5-unit 格上,1.0 只容浮点残差。
	schScoreWireEndpointEps = 1.0
	// 宿主导线追踪的最大段深(connect_pin 短桩通常 1 段,留裕量给折线 stub)。
	schScoreWireTraceDepth = 4
	// 每条命中的扣分。
	schScoreFoldedPenalty   = 15.0
	schScoreReversedPenalty = 20.0
	schScoreChainPenalty    = 10.0
	schScoreCrowdPenalty    = 8.0
	schScoreTextOverPenalty = 12.0
	// reversed:核心与宿主的水平距离小于此值时方向不构成偏好,不判反向。
	schScoreCoreDXMin = 30.0
)

// ---------------------------------------------------------------------------
// 报告结构
// ---------------------------------------------------------------------------

// schScoreAttribution 是一条归因:哪个对象、扣多少、怎么修。Fix 是**可直接执行**
// 的 CLI 命令(真实位号/坐标已填好)—— 这是本命令区别于 lint 的核心交付。
type schScoreAttribution struct {
	Dimension string      `json:"dimension"`
	Target    string      `json:"target"` // 位号或 net 名
	Penalty   float64     `json:"penalty"`
	Message   string      `json:"message"`
	Fix       string      `json:"fix,omitempty"`
	At        *checkPoint `json:"at,omitempty"`
}

type schScoreDimension struct {
	ID           string                `json:"id"`
	Title        string                `json:"title"`
	Status       string                `json:"status"` // scored | skipped
	Score        float64               `json:"score"`  // 0-100(skipped 时无意义)
	Weight       float64               `json:"weight"`
	Reason       string                `json:"reason,omitempty"` // skipped 的原因
	Attributions []schScoreAttribution `json:"attributions,omitempty"`
}

type schLayoutScoreReport struct {
	Overall float64 `json:"overall"`
	Verdict string  `json:"verdict"` // excellent | good | fair | poor | unscored
	// MinScore 只有显式传了 --min-score 才非零 —— 无它时本命令永远 exit 0。
	MinScore   float64             `json:"minScore,omitempty"`
	Dimensions []schScoreDimension `json:"dimensions"`
	// DimensionScores 是 id→分数扁平映射(可断言);skipped 的维不出现在这里。
	DimensionScores map[string]float64 `json:"dimensionScores,omitempty"`
	ComponentCount  int                `json:"componentCount"`
	ScoredDims      int                `json:"scoredDims"`
	SkippedDims     int                `json:"skippedDims"`
	Summary         string             `json:"summary"`
}

func (r *schLayoutScoreReport) dimension(id string) *schScoreDimension {
	for i := range r.Dimensions {
		if r.Dimensions[i].ID == id {
			return &r.Dimensions[i]
		}
	}
	return nil
}

// schScoreVerdict 是判定的唯一来源(分档同 pcb layout-score)。
func schScoreVerdict(rep *schLayoutScoreReport) string {
	if rep.ScoredDims == 0 {
		return "unscored"
	}
	switch {
	case rep.Overall >= 90:
		return "excellent"
	case rep.Overall >= 75:
		return "good"
	case rep.Overall >= 55:
		return "fair"
	default:
		return "poor"
	}
}

// ---------------------------------------------------------------------------
// 共享推导:宿主 / 核心 / 电源网
// ---------------------------------------------------------------------------

// schScoreInputs 是纯核除 comps 外的可选输入 —— 都是 best-effort:缺了降级到
// 几何/全页兜底,不缺时判定更准。
type schScoreInputs struct {
	// Wires 是页面导线段(components.list includeWires 的 buildWireSegments 产物)。
	// 有它时宿主 pin 走「netport anchor == wire 一端,wire 另一端 == pin」的电气
	// 匹配优先,几何最近兜底 —— live 抓到过几何最近判错宿主(LED_CTRL 的 fix 指到
	// U2:29,实际挂在 R3:1 的 stub 上)。
	Wires []wireSegment
	// ModuleOf 是 位号(大写)→模块名(来自 sch zones claims)。有它时核心在本模块
	// 内推导,防止跨模块误归因(POWER 模块的 C1/C2/C3 被建议搬到 U2 旁)。
	ModuleOf map[string]string
}

// schScoreScene 是五个维度共用的一次性推导结果。
type schScoreScene struct {
	comps   []layoutComp
	parts   []layoutComp // componentType==part 且有 bbox
	markers []layoutComp // isSchMarker 且 anchor 可用
	in      schScoreInputs
	// hostPart[markerIdx] = parts 下标(-1 = 没找到宿主);hostPin 是对应 pin。
	hostPart map[int]int
	hostPin  map[int]layoutPin
	// partNets[partIdx] = 该件通过 markers 挂到的**信号网**(非电源)集合。
	partNets map[int]map[string]bool
	// partPowerOnly[partIdx] = 挂了 ≥1 个标记且全是电源/地网(典型去耦电容)。
	// 无分区认领时这类件推不出可信的信号核心,proximity 豁免而不是硬绑最大件。
	partPowerOnly map[int]bool
	// largestEligible = 面积最大的**核心资格**件下标(-1 = 无);全页兜底参照。
	largestEligible int
}

// isPowerNetName:电源/地网不构成"信号伙伴"关系,核心推导要排除。
func isPowerNetName(net string) bool {
	n := strings.ToUpper(strings.TrimSpace(net))
	if n == "" {
		return true
	}
	for _, p := range []string{"GND", "AGND", "PGND", "DGND", "VCC", "VDD", "VSS", "VBUS", "VBAT", "VIN"} {
		if n == p || strings.HasPrefix(n, p+"_") {
			return true
		}
	}
	// 电压轨命名:5V / +5V / 3V3 / 3.3V / 12V0 …
	n = strings.TrimPrefix(n, "+")
	digits, seenV := 0, false
	for _, r := range n {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == 'V':
			seenV = true
		case r == '.':
		default:
			return false
		}
	}
	return digits > 0 && seenV
}

// bboxCenter(layoutBBox) 与 clampScore 复用 autolayout / pcb_layoutscore 的现成实现。

func bboxArea(b *layoutBBox) float64 {
	return (b.MaxX - b.MinX) * (b.MaxY - b.MinY)
}

// coreEligible:核心参照只能是"大件"—— 排除 R/C/L 前缀与小 bbox 件。live 回炉
// 教训:兜底不设资格线时,U2 的核心被推成了电容 C5。
func coreEligible(c layoutComp) bool {
	return !isPassivePart(c) && bboxArea(c.BBox) >= schScoreCoreMinArea
}

// buildSchScoreScene 做一次全部共享推导。
func buildSchScoreScene(comps []layoutComp, in schScoreInputs) *schScoreScene {
	s := &schScoreScene{
		comps:           comps,
		in:              in,
		hostPart:        map[int]int{},
		hostPin:         map[int]layoutPin{},
		partNets:        map[int]map[string]bool{},
		partPowerOnly:   map[int]bool{},
		largestEligible: -1,
	}
	for _, c := range comps {
		if c.ComponentType == schLayoutPartType && c.BBox != nil {
			s.parts = append(s.parts, c)
		}
	}
	var maxArea float64
	for i := range s.parts {
		if !coreEligible(s.parts[i]) {
			continue
		}
		if a := bboxArea(s.parts[i].BBox); s.largestEligible < 0 || a > maxArea {
			s.largestEligible, maxArea = i, a
		}
	}
	for _, c := range comps {
		if isSchMarker(c.ComponentType) && c.AnchorAvailable {
			s.markers = append(s.markers, c)
		}
	}
	powerHosted := map[int]bool{}
	for mi, m := range s.markers {
		// 第一优先:导线端点匹配(anchor → stub wire → pin,电气事实)。
		best, bestPin, ok := s.hostByWireTrace(m)
		if !ok {
			// 兜底:几何最近 pin ≤ 60。
			best, bestPin, ok = s.hostByNearestPin(m)
		}
		if !ok {
			s.hostPart[mi] = -1
			continue
		}
		s.hostPart[mi] = best
		s.hostPin[mi] = bestPin
		if isPowerNetName(m.Net) {
			powerHosted[best] = true
			continue
		}
		if s.partNets[best] == nil {
			s.partNets[best] = map[string]bool{}
		}
		s.partNets[best][m.Net] = true
	}
	for pi, hasPower := range powerHosted {
		if hasPower && len(s.partNets[pi]) == 0 {
			s.partPowerOnly[pi] = true
		}
	}
	return s
}

// hostByNearestPin:marker anchor(= stub 远端)最近的 part pin,距离 ≤ 60。
func (s *schScoreScene) hostByNearestPin(m layoutComp) (int, layoutPin, bool) {
	best, bestDist := -1, math.Inf(1)
	var bestPin layoutPin
	for pi, p := range s.parts {
		for _, pin := range p.Pins {
			if d := math.Hypot(pin.X-m.X, pin.Y-m.Y); d < bestDist {
				best, bestDist, bestPin = pi, d, pin
			}
		}
	}
	if best >= 0 && bestDist <= schScoreHostPinMaxDist {
		return best, bestPin, true
	}
	return -1, layoutPin{}, false
}

// hostByWireTrace 从 marker anchor 沿导线段追到 part pin:anchor 落在某 wire 段
// 一端,顺着共享端点 BFS(限深),任一到达端点与某 pin 重合(≤1 unit)即为宿主。
// 这是电气事实优先于几何最近 —— live 抓到几何最近把 LED_CTRL 的宿主判成 U2:29,
// 实际 stub 挂在 R3:1(密脚区里别件的 pin 可能比真宿主更近)。
func (s *schScoreScene) hostByWireTrace(m layoutComp) (int, layoutPin, bool) {
	if len(s.in.Wires) == 0 {
		return -1, layoutPin{}, false
	}
	near := func(x0, y0, x1, y1 float64) bool {
		return math.Hypot(x0-x1, y0-y1) <= schScoreWireEndpointEps
	}
	type pt struct{ x, y float64 }
	frontier := []pt{{m.X, m.Y}}
	visited := []pt{{m.X, m.Y}}
	seen := func(p pt) bool {
		for _, v := range visited {
			if near(p.x, p.y, v.x, v.y) {
				return true
			}
		}
		return false
	}
	for depth := 0; depth < schScoreWireTraceDepth && len(frontier) > 0; depth++ {
		var next []pt
		for _, f := range frontier {
			for _, w := range s.in.Wires {
				var other pt
				switch {
				case near(w.X0, w.Y0, f.x, f.y):
					other = pt{w.X1, w.Y1}
				case near(w.X1, w.Y1, f.x, f.y):
					other = pt{w.X0, w.Y0}
				default:
					continue
				}
				// 端点是否落在某个 part pin 上?
				for pi, p := range s.parts {
					for _, pin := range p.Pins {
						if near(pin.X, pin.Y, other.x, other.y) {
							return pi, pin, true
						}
					}
				}
				if !seen(other) {
					visited = append(visited, other)
					next = append(next, other)
				}
			}
		}
		frontier = next
	}
	return -1, layoutPin{}, false
}

// moduleFor 返回 partIdx 的分区认领模块名(无认领 = false)。
func (s *schScoreScene) moduleFor(partIdx int) (string, bool) {
	if len(s.in.ModuleOf) == 0 {
		return "", false
	}
	mod, ok := s.in.ModuleOf[strings.ToUpper(s.parts[partIdx].Designator)]
	return mod, ok
}

// largestEligibleIn 返回模块内面积最大的核心资格件(-1 = 模块内无大件)。
func (s *schScoreScene) largestEligibleIn(module string) int {
	best, bestArea := -1, 0.0
	for pi, p := range s.parts {
		if mod, ok := s.moduleFor(pi); !ok || mod != module {
			continue
		}
		if !coreEligible(p) {
			continue
		}
		if a := bboxArea(p.BBox); best < 0 || a > bestArea {
			best, bestArea = pi, a
		}
	}
	return best
}

// isLocalTop:partIdx 自己就是参照系里最大的核心资格件(有认领 = 本模块内,无
// 认领 = 全页)。核心自己的引脚标签朝外扇出是正常拓扑,reversed 判定豁免它。
func (s *schScoreScene) isLocalTop(partIdx int) bool {
	if mod, ok := s.moduleFor(partIdx); ok {
		return s.largestEligibleIn(mod) == partIdx
	}
	return s.largestEligible == partIdx
}

// coreOf 推导 partIdx 的「核心件」。候选一律要过 coreEligible(核心只能是大件)。
//
//   - 有分区认领且本件被认领:核心只在**本模块内**找 —— 先共信号网最大件,退模块
//     内最大件;模块内没大件就判"无核心"(绝不跨模块兜底,那正是把 POWER 电容
//     建议搬去 U2 旁的 live 误报)。
//   - 无认领(或本件未认领):先共**信号**网(电源网已在 partNets 录入时排除)最大
//     件;退全页最大大件,并把 degraded=true 交给归因文案声明"未分区,按全页最大
//     件推核心"。
//
// 返回 (parts 下标或 -1, 是否全页兜底降级)。
func (s *schScoreScene) coreOf(partIdx int) (int, bool) {
	mod, claimed := s.moduleFor(partIdx)
	sameScope := func(other int) bool {
		if !claimed {
			return true
		}
		om, ok := s.moduleFor(other)
		return ok && om == mod
	}
	nets := s.partNets[partIdx]
	best, bestArea := -1, 0.0
	for other, otherNets := range s.partNets {
		if other == partIdx || !coreEligible(s.parts[other]) || !sameScope(other) {
			continue
		}
		shared := false
		for n := range nets {
			if otherNets[n] {
				shared = true
				break
			}
		}
		if !shared {
			continue
		}
		if a := bboxArea(s.parts[other].BBox); best < 0 || a > bestArea {
			best, bestArea = other, a
		}
	}
	if best >= 0 {
		return best, false
	}
	if claimed {
		if top := s.largestEligibleIn(mod); top >= 0 && top != partIdx {
			return top, false
		}
		return -1, false // 模块内无核心:不跨模块兜底
	}
	if s.largestEligible >= 0 && s.largestEligible != partIdx {
		return s.largestEligible, true
	}
	return -1, false
}

// designatorPrefix 取位号的前导字母段("R12"→"R","LED1"→"LED")。
func designatorPrefix(d string) string {
	for i, r := range d {
		if r >= '0' && r <= '9' {
			return d[:i]
		}
	}
	return d
}

// isPassivePart:R/C/L 前缀的无源小件 —— proximity 维的评价对象。
func isPassivePart(c layoutComp) bool {
	switch designatorPrefix(c.Designator) {
	case "R", "C", "L":
		return true
	}
	return false
}

func snap5(v float64) float64 { return math.Round(v/5) * 5 }

// ---------------------------------------------------------------------------
// 纯核:五维打分
// ---------------------------------------------------------------------------

// analyzeSchLayoutScore 是纯核(无 I/O),表驱动测试直接喂 layoutComp;
// wires/claims 是可选输入,缺了降级(见 schScoreInputs)。
func analyzeSchLayoutScore(comps []layoutComp, in schScoreInputs) schLayoutScoreReport {
	s := buildSchScoreScene(comps, in)
	rep := schLayoutScoreReport{ComponentCount: len(comps)}

	if len(s.parts) == 0 {
		for _, id := range schScoreDimOrder {
			rep.Dimensions = append(rep.Dimensions, schScoreDimension{
				ID: id, Title: schScoreDimTitles[id], Status: schDimSkipped,
				Weight: schScoreDimWeights[id], Reason: "页面无带 bbox 的 part 几何,五维全部未测",
			})
		}
		rep.SkippedDims = len(rep.Dimensions)
		rep.Verdict = schScoreVerdict(&rep)
		rep.Summary = "sch layout-score: 页面无 part,未打分"
		return rep
	}

	rep.Dimensions = append(rep.Dimensions,
		scoreFoldedLabels(s),
		scoreReversedLabels(s),
		scoreProximity(s),
		scoreStubTidiness(s),
		scoreFrameFit(s),
	)

	// 加权综合:只算 scored 维;每维归因按扣分降序稳定排序。
	var sum, wsum float64
	rep.DimensionScores = map[string]float64{}
	for i := range rep.Dimensions {
		d := &rep.Dimensions[i]
		sort.SliceStable(d.Attributions, func(a, b int) bool {
			if d.Attributions[a].Penalty != d.Attributions[b].Penalty {
				return d.Attributions[a].Penalty > d.Attributions[b].Penalty
			}
			return d.Attributions[a].Target < d.Attributions[b].Target
		})
		if d.Status == schDimSkipped {
			rep.SkippedDims++
			continue
		}
		rep.ScoredDims++
		rep.DimensionScores[d.ID] = d.Score
		sum += d.Score * d.Weight
		wsum += d.Weight
	}
	if wsum > 0 {
		rep.Overall = math.Round(sum/wsum*10) / 10
	}
	rep.Verdict = schScoreVerdict(&rep)
	rep.Summary = fmt.Sprintf("sch layout-score: 综合 %.1f [%s] — %d 维已测 / %d 维未测(skipped≠满分),%d 图元",
		rep.Overall, rep.Verdict, rep.ScoredDims, rep.SkippedDims, rep.ComponentCount)
	return rep
}

// markerIndexByID 从 marker 列表按 primitiveId 找下标(-1 = 没有)。
func (s *schScoreScene) markerIndexByID(id string) int {
	for i, m := range s.markers {
		if m.ID == id {
			return i
		}
	}
	return -1
}

// fixReconnect 生成「拆掉重连」的可执行修复命令:disconnect 宿主 pin,再在同一
// pin 上按建议方向重新 connect。宿主找不到时退化为只给 connect 的坐标建议。
func (s *schScoreScene) fixReconnect(markerIdx int, kind, direction string) string {
	m := s.markers[markerIdx]
	host := s.hostPart[markerIdx]
	if host < 0 {
		return ""
	}
	pin := s.hostPin[markerIdx]
	return fmt.Sprintf("easyeda sch disconnect --pin %s:%s && easyeda sch connect --x %g --y %g --kind %s --net %s --direction %s --offset %d",
		s.parts[host].Designator, pin.Number, pin.X, pin.Y, kind, m.Net, direction, schScoreConnectOffset)
}

// suggestDirection:给 marker 的宿主件挑水平朝向 —— 有核心时面向核心(与
// reversed 维同一判据,修完不会被它再抓),没有核心时朝宿主 pin 的外侧。
func (s *schScoreScene) suggestDirection(markerIdx int) string {
	host := s.hostPart[markerIdx]
	if host < 0 {
		return "right"
	}
	hx, _ := bboxCenter(*s.parts[host].BBox)
	if core, _ := s.coreOf(host); core >= 0 {
		cx, _ := bboxCenter(*s.parts[core].BBox)
		if math.Abs(cx-hx) >= schScoreCoreDXMin {
			if cx > hx {
				return "right"
			}
			return "left"
		}
	}
	// 无核心/核心正上下:朝 pin 相对器件中心的外侧,stub 不横穿器件本体。
	if s.hostPin[markerIdx].X < hx {
		return "left"
	}
	return "right"
}

// ── 维度 1:folded-labels ───────────────────────────────────────────────────
// 判据**复用** sch check 的 foldedNetLabelFindings(netport bbox 高>宽 ⇔ 竖排),
// 不重写 —— 两条命令对"什么算折叠"永远同一答案。这里的增量是归因+fix。
func scoreFoldedLabels(s *schScoreScene) schScoreDimension {
	d := schScoreDimension{ID: schDimFolded, Title: schScoreDimTitles[schDimFolded],
		Status: schDimScored, Weight: schScoreDimWeights[schDimFolded]}
	findings := foldedNetLabelFindings(s.comps)
	for _, f := range findings {
		att := schScoreAttribution{
			Dimension: schDimFolded,
			Target:    f.MarkerNet,
			Penalty:   schScoreFoldedPenalty,
			At:        f.At,
			Message: fmt.Sprintf("netport %q @(%.0f,%.0f) 竖排折叠(bbox %.0f×%.0f,文字侧向)",
				f.MarkerNet, f.At.X, f.At.Y, f.BBox.MaxX-f.BBox.MinX, f.BBox.MaxY-f.BBox.MinY),
		}
		if mi := s.markerIndexByID(f.PrimitiveId); mi >= 0 {
			att.Fix = s.fixReconnect(mi, "netport", s.suggestDirection(mi))
		}
		d.Attributions = append(d.Attributions, att)
	}
	d.Score = clampScore(100 - schScoreFoldedPenalty*float64(len(findings)))
	return d
}

// ── 维度 2:reversed-labels(用户点名)──────────────────────────────────────
// netport 的伸出方向(bbox 体相对 anchor 的水平侧)背离「宿主件→核心件」方向 =
// 反向:读图时信号被标注引向左,实物却接到右边的核心 —— 视觉折返。只算水平分量
// (竖排已被维度 1 抓);核心与宿主几乎同 x 时无方向偏好,不判。
// 实际案例校准:R1@(700,475) rot180 服务 U2@(880,470)(共 EN 网):R1 的 EN
// netport 朝左(背离 U2)= 反向;朝右(面向 U2)= 正确。
func scoreReversedLabels(s *schScoreScene) schScoreDimension {
	d := schScoreDimension{ID: schDimReversed, Title: schScoreDimTitles[schDimReversed],
		Status: schDimScored, Weight: schScoreDimWeights[schDimReversed]}
	hits := 0
	for mi, m := range s.markers {
		if m.ComponentType != "netport" || m.BBox == nil {
			continue
		}
		w, h := m.BBox.MaxX-m.BBox.MinX, m.BBox.MaxY-m.BBox.MinY
		if h > w {
			continue // 竖排:维度 1 的对象
		}
		host := s.hostPart[mi]
		if host < 0 {
			continue
		}
		if s.isLocalTop(host) {
			// 宿主自己就是全页/本模块最大件(核心本尊):它的引脚标签朝外扇出
			// 是正常拓扑,不拿去和外围小件比方向 —— live 误报回炉修(U2 的
			// IO0 netport 被判"背向核心 C5")。
			continue
		}
		core, _ := s.coreOf(host)
		if core < 0 {
			continue
		}
		bx, _ := bboxCenter(*m.BBox)
		faceDX := bx - m.X // bbox 体在 anchor 左侧 = 朝左伸
		hx, _ := bboxCenter(*s.parts[host].BBox)
		cx, _ := bboxCenter(*s.parts[core].BBox)
		coreDX := cx - hx
		if math.Abs(coreDX) < schScoreCoreDXMin || math.Abs(faceDX) < 1 {
			continue // 核心正上下 / 朝向不可判:无水平偏好
		}
		if faceDX*coreDX >= 0 {
			continue // 面向核心,正确
		}
		hits++
		face, want := "左", "right"
		if faceDX > 0 {
			face, want = "右", "left"
		}
		coreSide := "右"
		if coreDX < 0 {
			coreSide = "左"
		}
		pin := s.hostPin[mi]
		d.Attributions = append(d.Attributions, schScoreAttribution{
			Dimension: schDimReversed,
			Target:    fmt.Sprintf("%s:%s", s.parts[host].Designator, pin.Number),
			Penalty:   schScoreReversedPenalty,
			At:        &checkPoint{X: round2(m.X), Y: round2(m.Y)},
			Message: fmt.Sprintf("%s 的 %s netport 背向核心 %s(朝%s,核心在%s)",
				s.parts[host].Designator, m.Net, s.parts[core].Designator, face, coreSide),
			Fix: s.fixReconnect(mi, "netport", want),
		})
	}
	d.Score = clampScore(100 - schScoreReversedPenalty*float64(hits))
	return d
}

// ── 维度 3:proximity(外围贴核心)──────────────────────────────────────────
// 每个 R/C/L 无源小件到其核心件(见 coreOf:认领内 > 共信号网 > 全页兜底)的
// bbox 边距:≤150 满分,≥500 记 0,线性衰减;维度分 = 平均 ×100。归因给不满分
// 的件,建议目标区取核心件上/下方(y 按当前相对位置就近取上或下)。
//
// 两类豁免(live 误报回炉修,POWER 模块的 C1/C2/C3 被建议搬到 U2 旁):
//   - 只挂电源/地网的件(去耦电容):无认领时推不出可信信号核心,豁免不硬绑;
//   - 认领模块内无核心大件、或全页无大件:无参照,豁免。
//
// 全页兜底(degraded)算分照旧,但归因文案声明"未分区,按全页最大件推核心"。
func scoreProximity(s *schScoreScene) schScoreDimension {
	d := schScoreDimension{ID: schDimProximity, Title: schScoreDimTitles[schDimProximity],
		Status: schDimScored, Weight: schScoreDimWeights[schDimProximity]}
	type miss struct {
		partIdx  int
		core     int
		dist     float64
		ratio    float64
		degraded bool
	}
	var total float64
	var n, exemptPowerOnly int
	var misses []miss
	for pi, p := range s.parts {
		if !isPassivePart(p) {
			continue
		}
		if _, claimed := s.moduleFor(pi); !claimed && s.partPowerOnly[pi] {
			exemptPowerOnly++
			continue // 只共电源网:无信号核心可信推导,豁免
		}
		core, degraded := s.coreOf(pi)
		if core < 0 {
			continue
		}
		dist := rectGap(*p.BBox, *s.parts[core].BBox)
		ratio := 1.0
		switch {
		case dist <= schScoreProximityFull:
		case dist >= schScoreProximityZero:
			ratio = 0
		default:
			ratio = (schScoreProximityZero - dist) / (schScoreProximityZero - schScoreProximityFull)
		}
		total += ratio
		n++
		if ratio < 1 {
			misses = append(misses, miss{pi, core, dist, ratio, degraded})
		}
	}
	if n == 0 {
		reason := "页面无可评的 R/C/L 无源件(或无核心大件可参照)"
		if exemptPowerOnly > 0 {
			reason = fmt.Sprintf("%d 个无源件只挂电源/地网(去耦类)且页面未分区认领 — 无信号核心可信推导,全部豁免;要评它们先 `sch zones set` 落模块认领", exemptPowerOnly)
		}
		d.Status = schDimSkipped
		d.Reason = reason
		return d
	}
	d.Score = clampScore(total / float64(n) * 100)
	for _, m := range misses {
		p, core := s.parts[m.partIdx], s.parts[m.core]
		cx, cy := bboxCenter(*core.BBox)
		_, py := bboxCenter(*p.BBox)
		ty := snap5(core.BBox.MaxY + 40) // 核心上方
		side := "上"
		if py < cy {
			ty = snap5(core.BBox.MinY - 40) // 已在下侧就往核心下方靠
			side = "下"
		}
		tx := snap5(cx)
		caveat := ""
		if m.degraded {
			caveat = "(未分区,按全页最大件推核心,建议先 sch zones set 落认领再复核)"
		}
		d.Attributions = append(d.Attributions, schScoreAttribution{
			Dimension: schDimProximity,
			Target:    p.Designator,
			Penalty:   math.Round((1-m.ratio)*100/float64(n)*10) / 10,
			At:        &checkPoint{X: round2(p.X), Y: round2(p.Y)},
			Message: fmt.Sprintf("%s 距核心 %s 边距 %.0f(>%.0f 开始扣分)— 建议移到 %s %s方 (%g,%g) 附近后重连%s",
				p.Designator, core.Designator, m.dist, schScoreProximityFull, core.Designator, side, tx, ty, caveat),
			Fix: fmt.Sprintf("easyeda sch modify --id %s --patch '{\"x\":%g,\"y\":%g}'", p.ID, tx, ty),
		})
	}
	return d
}

// ── 维度 4:stub-tidiness(长链/挤压)───────────────────────────────────────
// (a) 长链:小件(长轴 ≤100)与其挂载 marker 的联合 bbox 长轴 > 250 —— 件加两端
//
//	flag 拖成一条横贯的链。fix 对最远的 netport 拆重连、收 offset。
//
// (b) 挤压:同排(y 区间相交)相邻件 bbox 水平净距 < 117(两个相向水平 netport
//
//	标签所需最小距,本项目实测值)且至少一侧挂着水平 netport —— 标签挤压风险。
func scoreStubTidiness(s *schScoreScene) schScoreDimension {
	d := schScoreDimension{ID: schDimTidiness, Title: schScoreDimTitles[schDimTidiness],
		Status: schDimScored, Weight: schScoreDimWeights[schDimTidiness]}

	// 每个 part 的挂载 marker 下标。
	hosted := map[int][]int{}
	for mi := range s.markers {
		if pi := s.hostPart[mi]; pi >= 0 {
			hosted[pi] = append(hosted[pi], mi)
		}
	}
	penalty := 0.0

	// (a) 长链
	for pi, p := range s.parts {
		if math.Max(p.BBox.MaxX-p.BBox.MinX, p.BBox.MaxY-p.BBox.MinY) > schScoreChainPartMaxAxis {
			continue
		}
		mis := hosted[pi]
		if len(mis) == 0 {
			continue
		}
		u := *p.BBox
		farIdx, farDist := -1, 0.0
		for _, mi := range mis {
			m := s.markers[mi]
			if m.BBox == nil {
				continue
			}
			u.MinX = math.Min(u.MinX, m.BBox.MinX)
			u.MinY = math.Min(u.MinY, m.BBox.MinY)
			u.MaxX = math.Max(u.MaxX, m.BBox.MaxX)
			u.MaxY = math.Max(u.MaxY, m.BBox.MaxY)
			if dist := math.Hypot(m.X-p.X, m.Y-p.Y); m.ComponentType == "netport" && dist > farDist {
				farIdx, farDist = mi, dist
			}
		}
		span := math.Max(u.MaxX-u.MinX, u.MaxY-u.MinY)
		if span <= schScoreChainSpanMax {
			continue
		}
		penalty += schScoreChainPenalty
		att := schScoreAttribution{
			Dimension: schDimTidiness,
			Target:    p.Designator,
			Penalty:   schScoreChainPenalty,
			At:        &checkPoint{X: round2(p.X), Y: round2(p.Y)},
			Message: fmt.Sprintf("%s 与其 %d 个标记连成跨度 %.0f 的长链(>%.0f)— 收短最远标记的 offset",
				p.Designator, len(mis), span, schScoreChainSpanMax),
		}
		if farIdx >= 0 {
			att.Fix = s.fixReconnect(farIdx, "netport", s.suggestDirection(farIdx))
		}
		d.Attributions = append(d.Attributions, att)
	}

	// (b) 同排挤压
	hasHorizPort := func(pi int) bool {
		for _, mi := range hosted[pi] {
			m := s.markers[mi]
			if m.ComponentType == "netport" && m.BBox != nil &&
				m.BBox.MaxX-m.BBox.MinX >= m.BBox.MaxY-m.BBox.MinY {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(s.parts); i++ {
		for j := i + 1; j < len(s.parts); j++ {
			a, b := s.parts[i], s.parts[j]
			if a.BBox.MaxX > b.BBox.MinX && b.BBox.MaxX > a.BBox.MinX {
				continue // 水平方向重叠:是 layout-lint 的 overlap,不重复报
			}
			if math.Min(a.BBox.MaxY, b.BBox.MaxY)-math.Max(a.BBox.MinY, b.BBox.MinY) <= 0 {
				continue // 不同排
			}
			gap := math.Max(a.BBox.MinX-b.BBox.MaxX, b.BBox.MinX-a.BBox.MaxX)
			if gap >= schScoreRowMinGap || (!hasHorizPort(i) && !hasHorizPort(j)) {
				continue
			}
			penalty += schScoreCrowdPenalty
			d.Attributions = append(d.Attributions, schScoreAttribution{
				Dimension: schDimTidiness,
				Target:    fmt.Sprintf("%s↔%s", a.Designator, b.Designator),
				Penalty:   schScoreCrowdPenalty,
				Message: fmt.Sprintf("%s 与 %s 同排净距 %.0f < %.0f(相向水平 netport 标签最小距)— 拉开间距(sch distribute --axis x)或把一侧标签换 direction",
					a.Designator, b.Designator, gap, schScoreRowMinGap),
			})
		}
	}

	d.Score = clampScore(100 - penalty)
	return d
}

// ── 维度 5:frame-fit(版面整洁)────────────────────────────────────────────
// 子项 1(可测,当 components.list 暴露 text 图元 bbox 时):自由说明文字与 part
// bbox 正面积重叠 = 说明压电路。子项 2(分区框内不外溢)需要框几何,而 workflow
// state 只存 frame ids 不存矩形 —— 取不到就整维 skipped 并写明原因,**绝不当满分**。
func scoreFrameFit(s *schScoreScene) schScoreDimension {
	d := schScoreDimension{ID: schDimFrameFit, Title: schScoreDimTitles[schDimFrameFit],
		Status: schDimScored, Weight: schScoreDimWeights[schDimFrameFit]}
	var texts []layoutComp
	for _, c := range s.comps {
		if c.ComponentType == "text" && c.BBox != nil {
			texts = append(texts, c)
		}
	}
	if len(texts) == 0 {
		d.Status = schDimSkipped
		d.Reason = "components.list 未暴露 text 图元 bbox,且分区框几何不在 workflow state — 未测(≠满分)"
		return d
	}
	penalty := 0.0
	for _, t := range texts {
		for _, p := range s.parts {
			ox, oy, overlap := overlapExtent(*t.BBox, *p.BBox)
			if !overlap {
				continue
			}
			penalty += schScoreTextOverPenalty
			tx, ty := bboxCenter(*t.BBox)
			d.Attributions = append(d.Attributions, schScoreAttribution{
				Dimension: schDimFrameFit,
				Target:    p.Designator,
				Penalty:   schScoreTextOverPenalty,
				At:        &checkPoint{X: round2(tx), Y: round2(ty)},
				Message: fmt.Sprintf("说明文字 @(%.0f,%.0f) 压在 %s 上(重叠 %.0f×%.0f)— 移到模块框下/旁空白处(sch text-list 找 id 后 sch prim-delete + sch note 重放)",
					tx, ty, p.Designator, ox, oy),
			})
		}
	}
	d.Score = clampScore(100 - penalty)
	d.Reason = "分区框几何不可得,框内外溢子项未测"
	return d
}

// ---------------------------------------------------------------------------
// CLI 层
// ---------------------------------------------------------------------------

func newSchLayoutScoreCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var (
		asJSON   bool
		minScore float64
		showAll  bool
	)
	c := &cobra.Command{
		Use:   "layout-score",
		Short: "Score schematic layout readability (folded/reversed labels, proximity, chains) with executable fixes",
		Long: "原理图布局质量五维打分(诊断视角,不是门 —— 门仍是 layout-lint + check):\n\n" +
			"  folded-labels    netport 竖排折叠(判据与 sch check 的 folded-net-label 同源)\n" +
			"  reversed-labels  netport 朝向背离宿主件→核心件方向(视觉折返;核心自身的标签豁免)\n" +
			"  proximity        R/C/L 无源件到核心件的边距(≤150 满分,≥500 记 0)。核心只认大件,\n" +
			"                   有 sch zones 认领时在本模块内推导;未分区且只挂电源网的去耦件豁免\n" +
			"  stub-tidiness    小件+两端标记的长链跨度 >250;同排净距 <117 的标签挤压\n" +
			"  frame-fit        说明文字压电路;分区框几何不可得时 skipped(≠满分)\n\n" +
			"每条归因带 fix 字段:已填好真实位号/坐标的可执行命令,照抄运行即可修复。\n" +
			"无 --min-score 时永远 exit 0;显式给了才在综合分低于它时非零退出。",
		Example: "  easyeda sch layout-score\n" +
			"  easyeda sch layout-score --json\n" +
			"  easyeda sch layout-score --min-score 75   # 当门用(不建议;门是 layout-lint)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// includeWires:宿主 pin 判定的电气匹配输入(anchor→stub→pin),
			// 缺了自动退几何最近 —— 与 autoconnect 同一 wires payload/解析。
			res, err := requestAction(cfg, "schematic.components.list", *window,
				map[string]any{"includeBBox": true, "includePins": true, "includeWires": true})
			if err != nil {
				return err
			}
			comps, perr := parseLayoutComps(res.Result)
			if perr != nil {
				return perr
			}
			in := schScoreInputs{Wires: buildWireSegments(res.Result)}
			// 分区认领(best-effort):有 claims 时核心在本模块内推导。读不到只
			// 降级为全页兜底(归因文案会自带"未分区"声明),不阻断诊断。
			if moduleOf, cerr := loadSchModuleClaims(cfg, *window); cerr != nil {
				fmt.Fprintf(stderr, "sch layout-score: zone claims unavailable (%v) — 核心按全页最大件兜底\n", cerr)
			} else {
				in.ModuleOf = moduleOf
			}
			rep := analyzeSchLayoutScore(comps, in)
			rep.MinScore = minScore

			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else {
				renderSchLayoutScore(rep, showAll, stdout)
			}
			if minScore > 0 && rep.Overall < minScore {
				return fmt.Errorf("sch layout-score %.1f below --min-score %.1f", rep.Overall, minScore)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	c.Flags().Float64Var(&minScore, "min-score", 0, "fail (non-zero exit) when the weighted overall falls below this; unset = always exit 0")
	c.Flags().BoolVar(&showAll, "all", false, "list every attribution instead of the top few per dimension")
	return c
}

// loadSchModuleClaims 把当前页的 sch zones claims(workflow state 按 documentUuid
// 存的 模块→位号 认领)摊平成 位号(大写)→模块名,给核心推导做模块围栏。
// 空表返回 nil(= 未分区,coreOf 走全页兜底 + degraded 声明)。
func loadSchModuleClaims(cfg *appConfig, window string) (map[string]string, error) {
	pinnedCfg, win, docUUID, err := pinZonePage(cfg, window)
	if err != nil {
		return nil, err
	}
	zones, _, err := loadSchZoneClaimsForPage(pinnedCfg, win, docUUID)
	if err != nil {
		return nil, err
	}
	if len(zones) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for module, claim := range zones {
		if claim == nil {
			continue
		}
		for _, desig := range claim.Parts {
			out[strings.ToUpper(desig)] = module
		}
	}
	return out, nil
}

const schScoreTopN = 3

// renderSchLayoutScore 输出人读报告:一行综合 + 每条归因带可执行 fix。
// skipped 的维照列(「这维没测」本身是结论,隐藏它会让报告读起来像全面体检)。
func renderSchLayoutScore(rep schLayoutScoreReport, showAll bool, w io.Writer) {
	var dims []string
	for _, d := range rep.Dimensions {
		if d.Status == schDimSkipped {
			dims = append(dims, d.ID+" skipped")
			continue
		}
		dims = append(dims, fmt.Sprintf("%s %.0f", d.ID, d.Score))
	}
	fmt.Fprintf(w, "sch layout-score: 综合 %.1f [%s]  (%d 维:%s)\n",
		rep.Overall, rep.Verdict, len(rep.Dimensions), strings.Join(dims, " / "))
	for _, d := range rep.Dimensions {
		if d.Status == schDimSkipped {
			fmt.Fprintf(w, "  %-16s 未测:%s\n", d.ID, d.Reason)
			continue
		}
		n := len(d.Attributions)
		if !showAll && n > schScoreTopN {
			n = schScoreTopN
		}
		for _, a := range d.Attributions[:n] {
			fmt.Fprintf(w, "  %-16s -%-5.1f %s\n", d.ID, a.Penalty, a.Message)
			if a.Fix != "" {
				fmt.Fprintf(w, "      fix: %s\n", a.Fix)
			}
		}
		if n < len(d.Attributions) {
			fmt.Fprintf(w, "  %-16s … 另有 %d 条(--all 全列)\n", d.ID, len(d.Attributions)-n)
		}
	}
	fmt.Fprintf(w, "%s\n", rep.Summary)
}
