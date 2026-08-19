package app

// sch_zone_arrange.go — 功能区区间布局求解器(phase B):边归属 + 回退链 + 多层货架装箱。
//
// 设计对齐 2026-08-16(演示页 v3,用户逐条裁定):
//   - **域界固定 A4 横放 1170×825**,不做纸张阶梯 —— 装不下的出路是区内收敛
//     (phase A)或拆页,永不建议换纸。
//   - **口径:区框 = 成员 L1 虚拟组全图元并集**(器件+桩线+netport+netflag)
//     + pad + 区名带/说明带 —— 标签必须在框内,是硬约束(老口径只算器件,
//     标签互相穿插时报 clean,是判据盲区)。
//   - **同一输入,唯一输出**:六个不确定性来源逐条消除(全序排序/固定平局序/
//     5 格律/无随机/规划不读活画布/判定与生成同一把尺)。稳定性推论(用户确认):
//     确定的元器件集合 → 每次同一解;小幅挪动某个元素,只要不改变质心平局,
//     输出不变 —— 位置只参与边归属与排序平局,不参与落位坐标。
//   - **三态输出**:pass / blocked(报出是谁、回退链每条边为何不行)/ 输入非法。
//     永不输出「大概摆了一下」。
//
// ## 2026-08-19 修复:单轴滑动 → 多层货架 + 回溯(真机 P3 有解却报 blocked)
//
// 首版 phase B 把每个区**绑死在一条边的一条轴**上滑动:W/E 只能贴着 x=L / x=R
// 自上而下滑,N/S 只能贴着 y=T / y=B 自左而右滑。于是
//   ①一条边只装得下一列/一行 —— 四个区各占一条边之后,第五个(以及尺寸不巧的
//     第四个)就「无处可放」;
//   ②先落位的区一旦占了坏位置,后面全盘皆输 —— **贪心不可回头**。
//
// 真机取证(ceshi / P3_USB_DL,phase A 收敛后 U 315×353、J_USB 303×454、
// esp32_autodownload 249×400、D_ESD 242×183):四区总面积只占可用面积 46%,
// 手算可行解显然存在(左列 U+autodl 竖叠 353+12+400=765 恰好等于可用高;
// 中列 J_USB;右列 D_ESD;总宽 884 ≤ 1110),首版却报
// `blocked —— esp32_autodownload 无处可放,回退链已试尽:S(230)→W(266)→N(595)→E(904)`。
// **排不下的不是面积,是「一条边只能开一列」这个表达能力缺陷。**
//
// 修法(仍是纯函数、仍然确定性):
//   - **同一条边可以开多层货架**:边的「深度」候选 = 贴边 ∪ 每个障碍朝内的那一面
//     + gutter(skyline 收缩点),按离边由浅到深排序 —— 深度 0 就是首版行为,
//     所以这是严格的能力超集。
//   - **沿边轴只取接触点**:候选 = 规范角 ∪ 每个障碍朝向扫描方向的那一面 ± gutter
//     ∪ 轴末端。首版逐 5 步扫出来的「首个空位」必定落在其中之一,所以贪心路径
//     一字不差地被保留(DFS 的第一条下潜路径 = 首版结果)。
//   - **回溯**:某个区放不下时,退回去换上一个区的下一个候选,而不是直接判死。
//     搜索序是全序的(区序 → 边序 → 深度 → 沿边序),第一个可行解即输出,
//     所以「唯一输出」不受影响。
//   - **预算**:候选评估次数上限 zaSearchBudget(确定性计数,不是时间),
//     超出即 blocked 并在输出里标 exhausted —— 绝不用随机/退火换解。
//
// 求解器是纯函数:输入区形状+质心(或声明边),输出每区落位框。落地执行(挪件+重连)
// 是另一层(ADR-0003 舞步),必须先补齐删除集=重建集断言 —— 见 sch_zone_compact.go
// 尾注的三问。

import (
	"fmt"
	"sort"
	"strings"
)

// zaEdge 常量:边的固定优先序(平局裁决用,W<E<N<S)。
var zaEdgeOrder = []string{"W", "E", "N", "S"}

// zaZone 是求解器的一个输入区。
type zaZone struct {
	Name string
	// W/H 是区框的目标尺寸(phase A 收敛后的框,含 pad 与区名/说明带)。
	W, H float64
	// Home 是区当前质心 —— **只用于边归属推断与排序平局,不参与落位坐标**。
	// 这就是稳定性的来源:区内小幅挪件不改变归属,输出就一个字都不变。
	Home [2]float64
	// Edge 是 S0 声明的归属边("W"/"E"/"N"/"S");空 = 按质心回退推断。
	// 归属一经决定应写回声明(声明式沉淀),重排不重新推断。
	Edge string
}

// zaPlaced 是一个区的落位结果。
type zaPlaced struct {
	Name  string     `json:"name"`
	Rect  layoutBBox `json:"rect"`
	Edge  string     `json:"edge"`  // 实际落到的边
	Chain []string   `json:"chain"` // 完整回退链(首项=首选边)
	// Shelf 是落在本边的第几层货架(0 = 贴边那一列/行)。多层货架是 2026-08-19
	// 修复引入的表达能力:>0 说明这个区退到了本边的第二列/第二行。
	Shelf int `json:"shelf"`
	Steps int `json:"steps"` // 命中的候选序号(可回放性)
}

// zaEdgeProbe 是 blocked 时「每条边各卡在哪」的结构化归因。
type zaEdgeProbe struct {
	Edge  string  `json:"edge"`
	Dist  float64 `json:"dist"`  // 质心到该边的距离(回退链排序依据)
	Cands int     `json:"cands"` // 该边生成了多少个候选位(0 = 纸面根本放不下)
	// Blocker 是把该边全部候选顶掉的主要障碍(区名 / "图签");
	// Cands=0 时为空,表示这条边连一个合法候选都排不出来(纸面不够)。
	Blocker string `json:"blocker,omitempty"`
}

// zaResult 是三态输出。
type zaResult struct {
	OK      bool       `json:"ok"`
	Placed  []zaPlaced `json:"placed,omitempty"`
	Blocked string     `json:"blocked,omitempty"`
	// Tried 记录 blocked 区的回退链:每条边的质心距离 + 卡在谁身上,
	// 给人一句能执行的解释。
	Tried string `json:"tried,omitempty"`
	// Edges 是 Tried 的结构化版本(--json 用)。
	Edges []zaEdgeProbe `json:"edges,omitempty"`
	// Exhausted:搜索预算用尽才判的 blocked(不等于数学上无解)。
	// 出路与真无解一样(收敛/拆页),但必须如实标出来,不许伪装成「证明了无解」。
	Exhausted bool `json:"exhausted,omitempty"`
}

// zaEdgeChain 算一个区的归属链:声明边优先,其余按质心距离升序,平局按 W<E<N<S。
func zaEdgeChain(home [2]float64, declared string, sheet layoutBBox) (chain []string, dist map[string]float64) {
	dist = map[string]float64{
		"W": home[0] - sheet.MinX, "E": sheet.MaxX - home[0],
		"S": home[1] - sheet.MinY, "N": sheet.MaxY - home[1],
	}
	rest := make([]string, 0, 4)
	for _, e := range zaEdgeOrder {
		if e != declared {
			rest = append(rest, e)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if dist[rest[i]] != dist[rest[j]] {
			return dist[rest[i]] < dist[rest[j]]
		}
		return zaEdgeIdx(rest[i]) < zaEdgeIdx(rest[j])
	})
	if declared != "" {
		return append([]string{declared}, rest...), dist
	}
	return rest, dist
}

func zaEdgeIdx(e string) int {
	for i, x := range zaEdgeOrder {
		if x == e {
			return i
		}
	}
	return len(zaEdgeOrder)
}

// zaHit 判两框是否相交(pad 为膨胀量;判定与 JS 演示同一严格性:开区间)。
func zaHit(a, b layoutBBox, pad float64) bool {
	return a.MinX < b.MaxX+pad && b.MinX-pad < a.MaxX &&
		a.MinY < b.MaxY+pad && b.MinY-pad < a.MaxY
}

// zaScanStep:扫描格律 = EasyEDA 连接格。所有落位锚坐标都是 5 的整数倍
// (L/R/B/T 本身由 snap5Up/snap5Dn 得到,也是 5 的整数倍);框的另一侧 = 锚 ± 尺寸。
// 执行侧再把每个件的平移量圆整到 5(件是格点公民,框角跟着件走)。
const zaScanStep = 5.0

// zaSearchBudget 是候选评估次数的上限 —— **确定性计数**(不是超时、不是随机重启,
// 所以同一输入在任何机器上耗不耗尽预算的结论都一样)。
//
// 实测(defaultPartitionOpts,A4 + 图签):真机 P3 有解 191 次;raw 形状证明无解
// 2,523 次;负对照 P3×1.25 证明无解 6,796 次 —— 四区规模离预算差三个数量级。
// 二维装箱是 NP-hard,区数上去之后「证明无解」会指数爆炸(随机 8 区的最坏样本
// 跑满 2,000 万次、1.7 秒仍没搜完),所以预算存在的意义不是防死循环,而是给
// **最坏情况一个确定的墙钟上界**(~12M 次/秒 → 200ms 量级)。撞墙时判决仍是
// blocked(出路一样:收敛或拆页),但 Exhausted=true 会如实说明「没有证明无解」。
const zaSearchBudget = 2000000

func snap5Up(v float64) float64 {
	s := snap5(v)
	if s < v {
		return s + zaScanStep
	}
	return s
}

func snap5Dn(v float64) float64 {
	s := snap5(v)
	if s > v {
		return s - zaScanStep
	}
	return s
}

// zaFrame 是可用域(页边距之内)与区间距。
type zaFrame struct{ L, R, B, T, G float64 }

// zaOverlapArea 是两框交集的面积(不相交时 0)。
func zaOverlapArea(a, b layoutBBox) float64 {
	w := minF(a.MaxX, b.MaxX) - maxF(a.MinX, b.MinX)
	h := minF(a.MaxY, b.MaxY) - maxF(a.MinY, b.MinY)
	if w <= 0 || h <= 0 {
		return 0
	}
	return w * h
}

// zaObstacle 是落位要避开的一个障碍(图签安全带 / 已落位区),带名字供归因。
type zaObstacle struct {
	box  layoutBBox
	name string
}

// zaCand 是一个候选落位。
type zaCand struct {
	rect  layoutBBox
	edge  string
	shelf int // 本边第几层货架(0 = 贴边)
}

// zaZoneCtx 是排好序、算好链的一个区。
type zaZoneCtx struct {
	zaZone
	chain []string
	dist  map[string]float64
	// landedStep 是「本区落位时命中的是第几个候选」。挂在 ctx 上(而不是 zaCand 上)
	// 是因为回溯会反复覆盖它 —— 成功路径上残留的那个值才是最终值。
	landedStep int
}

// zaAnchors 把「基准锚 + 一组**已按安全方向取整**的候选锚」折成去重、定序、
// 落在 [lo,hi] 内的锚序列。asc=true 表示离边由浅到深/沿轴由小到大。
//
// 取整方向在调用点决定,原则只有一条:**朝着仍然满足约束的那一侧取整**
// (要求 anchor ≥ x 就 snap5Up,要求 anchor ≤ x 就 snap5Dn)。这样格律
// (锚永远是 5 的整数倍)与约束(gutter 不被取整吃掉)同时成立。
func zaAnchors(base float64, vals []float64, lo, hi float64, asc bool) []float64 {
	out := make([]float64, 0, len(vals)+1)
	seen := make(map[float64]bool, len(vals)+1)
	add := func(v float64) {
		if v < lo-1e-9 || v > hi+1e-9 || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(base)
	for _, v := range vals {
		add(v)
	}
	sort.Float64s(out)
	if !asc {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// zaSearch 是回溯求解器的状态。
type zaSearch struct {
	f      zaFrame
	zs     []zaZoneCtx
	obs    []zaObstacle // 图签安全带 + 已落位区(索引 0..baseObs-1 是固定障碍)
	sol    []zaCand     // 与 zs 同序的当前解
	budget int
	// free 是当前还剩多少净面积(可用域 − 图签安全带 − 已落位框);need[i] 是
	// 第 i 个区起往后所有区的面积和。free < need[i] 时这一支必然无解 —— 剪掉。
	// **必须是「可采纳」剪枝**(绝不剪掉可行支):障碍与未来落位互不重叠,
	// 所以面积和是货真价实的下界,不含任何启发式猜测,确定性不受影响。
	free float64
	need []float64
	// deepest/probe 记录**最深的一次失败**:那一层的区就是「无处可放」的那个,
	// 它的逐边归因就是给人的下一步。只在第一次到达该深度时记录(确定性)。
	deepest int
	probe   []zaEdgeProbe
	overrun bool
}

// zaEdgePlan 是一个区在一条边上的全部候选,按货架层(离边由浅到深)分组。
type zaEdgePlan struct {
	edge    string
	dist    float64
	shelves [][]zaCand
}

// candidates 生成区 z 在边 edge 上的全部候选,按货架层分组、层内沿边按扫描序。
func (s *zaSearch) candidates(z zaZone, edge string) [][]zaCand {
	f := s.f
	depthRaw := make([]float64, 0, len(s.obs))
	alongRaw := make([]float64, 0, len(s.obs)+1)
	for _, o := range s.obs {
		switch edge {
		case "W":
			depthRaw = append(depthRaw, snap5Up(o.box.MaxX+f.G))
		case "E":
			depthRaw = append(depthRaw, snap5Dn(o.box.MinX-f.G))
		case "N":
			depthRaw = append(depthRaw, snap5Dn(o.box.MinY-f.G))
		case "S":
			depthRaw = append(depthRaw, snap5Up(o.box.MaxY+f.G))
		}
		switch edge {
		case "W", "E": // 沿边轴 = y,自上而下
			alongRaw = append(alongRaw, snap5Dn(o.box.MinY-f.G))
		default: // 沿边轴 = x,自左而右
			alongRaw = append(alongRaw, snap5Up(o.box.MaxX+f.G))
		}
	}
	var depths, alongs []float64
	switch edge {
	case "W":
		depths = zaAnchors(f.L, depthRaw, f.L, f.R-z.W, true) // 锚 = MinX
		alongs = zaAnchors(f.T, append(alongRaw, snap5Up(f.B+z.H)), f.B+z.H, f.T, false)
	case "E":
		depths = zaAnchors(f.R, depthRaw, f.L+z.W, f.R, false) // 锚 = MaxX
		alongs = zaAnchors(f.T, append(alongRaw, snap5Up(f.B+z.H)), f.B+z.H, f.T, false)
	case "N":
		depths = zaAnchors(f.T, depthRaw, f.B+z.H, f.T, false) // 锚 = MaxY
		alongs = zaAnchors(f.L, append(alongRaw, snap5Dn(f.R-z.W)), f.L, f.R-z.W, true)
	case "S":
		depths = zaAnchors(f.B, depthRaw, f.B, f.T-z.H, true) // 锚 = MinY
		alongs = zaAnchors(f.L, append(alongRaw, snap5Dn(f.R-z.W)), f.L, f.R-z.W, true)
	}
	out := make([][]zaCand, 0, len(depths))
	for si, d := range depths {
		shelf := make([]zaCand, 0, len(alongs))
		for _, a := range alongs {
			var r layoutBBox
			switch edge {
			case "W":
				r = layoutBBox{MinX: d, MaxX: d + z.W, MaxY: a, MinY: a - z.H}
			case "E":
				r = layoutBBox{MaxX: d, MinX: d - z.W, MaxY: a, MinY: a - z.H}
			case "N":
				r = layoutBBox{MinX: a, MaxX: a + z.W, MaxY: d, MinY: d - z.H}
			case "S":
				r = layoutBBox{MinX: a, MaxX: a + z.W, MinY: d, MaxY: d + z.H}
			}
			shelf = append(shelf, zaCand{rect: r, edge: edge, shelf: si})
		}
		out = append(out, shelf)
	}
	return out
}

// fits 判候选是否可落:纸面之内 + 与全部障碍留够 gutter。返回顶掉它的障碍名。
func (s *zaSearch) fits(r layoutBBox) (bool, string) {
	if r.MinX < s.f.L-1e-9 || r.MaxX > s.f.R+1e-9 || r.MinY < s.f.B-1e-9 || r.MaxY > s.f.T+1e-9 {
		return false, ""
	}
	for _, o := range s.obs {
		if zaHit(r, o.box, s.f.G) {
			return false, o.name
		}
	}
	return true, ""
}

// solve 是深度优先回溯:区按全序逐个落位,某区排不下就回退换上一个区的下一个候选。
//
// **候选序是「货架层优先」**:先让回退链上四条边都试一遍贴边那一层(= 首版的
// 全部行为,一步不差),都不行才整体往里开第二层货架,再不行开第三层……
// 这样两件事同时成立:
//   - 边归属/回退链的语义不被稀释(不会为了一个区把它塞进 W 边第 5 层 = 页面正中,
//     而 E 边贴边明明空着);
//   - DFS 的第一条下潜路径逐字等于首版贪心 —— 原本就有解的页,输出不变。
func (s *zaSearch) solve(i int) bool {
	if i == len(s.zs) {
		return true
	}
	if s.free < s.need[i]-1e-9 {
		return false // 面积下界剪枝(可采纳):剩下的区加起来就已经比空地大
	}
	z := s.zs[i]
	plans := make([]zaEdgePlan, 0, len(z.chain))
	levels := 0
	for _, e := range z.chain {
		sh := s.candidates(z.zaZone, e)
		if len(sh) > levels {
			levels = len(sh)
		}
		plans = append(plans, zaEdgePlan{edge: e, dist: z.dist[e], shelves: sh})
	}
	// blockers 按首次出现序累计,取次数最多者(平局取先出现者)—— 无 map 迭代。
	bn := make([][]string, len(plans))
	bc := make([][]int, len(plans))
	cands := make([]int, len(plans))
	step := 0
	for lv := 0; lv < levels; lv++ {
		for pi := range plans {
			if lv >= len(plans[pi].shelves) {
				continue
			}
			for _, c := range plans[pi].shelves[lv] {
				if s.budget <= 0 {
					s.overrun = true
					return false
				}
				s.budget--
				step++
				cands[pi]++
				ok, who := s.fits(c.rect)
				if !ok {
					if who != "" {
						idx := -1
						for t, n := range bn[pi] {
							if n == who {
								idx = t
								break
							}
						}
						if idx < 0 {
							bn[pi], bc[pi] = append(bn[pi], who), append(bc[pi], 1)
						} else {
							bc[pi][idx]++
						}
					}
					continue
				}
				s.sol[i] = c
				s.zs[i].landedStep = step
				s.obs = append(s.obs, zaObstacle{box: c.rect, name: z.Name})
				s.free -= z.W * z.H
				if s.solve(i + 1) {
					return true
				}
				s.free += z.W * z.H
				s.obs = s.obs[:len(s.obs)-1]
				if s.overrun {
					return false
				}
			}
		}
	}
	if i > s.deepest {
		probe := make([]zaEdgeProbe, 0, len(plans))
		for pi, p := range plans {
			pr := zaEdgeProbe{Edge: p.edge, Dist: p.dist, Cands: cands[pi]}
			best := -1
			for t := range bn[pi] {
				if best < 0 || bc[pi][t] > bc[pi][best] {
					best = t
				}
			}
			if best >= 0 {
				pr.Blocker = bn[pi][best]
			}
			probe = append(probe, pr)
		}
		s.deepest, s.probe = i, probe
	}
	return false
}

// newZaSearch 备好求解器状态:可用域、全序区表、固定障碍(图签安全带)、
// 面积剪枝的两个量。**唯一的构造入口** —— 单测里的负基线重放也走它,
// 免得两处初始化漂移(那正是「两把尺」的经典发生方式)。
func newZaSearch(zones []zaZone, sheet layoutBBox, keepout *layoutBBox, opts partitionOpts) *zaSearch {
	f := zaFrame{
		L: snap5Up(sheet.MinX + opts.Margin), R: snap5Dn(sheet.MaxX - opts.Margin),
		B: snap5Up(sheet.MinY + opts.Margin), T: snap5Dn(sheet.MaxY - opts.Margin),
		G: opts.Gutter,
	}
	zs := zaOrderZones(zones, sheet)
	s := &zaSearch{f: f, zs: zs, sol: make([]zaCand, len(zs)), budget: zaSearchBudget, deepest: -1}
	s.free = (f.R - f.L) * (f.T - f.B)
	if safe := inflatedTitleKeepout(keepout); safe != nil {
		s.obs = append(s.obs, zaObstacle{box: *safe, name: "图签"})
		s.free -= zaOverlapArea(*safe, layoutBBox{MinX: f.L, MinY: f.B, MaxX: f.R, MaxY: f.T})
	}
	s.need = make([]float64, len(zs)+1)
	for i := len(zs) - 1; i >= 0; i-- {
		s.need[i] = s.need[i+1] + zs[i].W*zs[i].H
	}
	return s
}

// zaOrderZones 算每个区的归属链并把区排成**显式全序**:首选边序(W<E<N<S)→
// 沿边坐标(W/E 自上而下 = 大 y 在前;N/S 自左而右)→ 区名自然序。
// 输入顺序被彻底抹掉 —— 这是「同一输入唯一输出」的第一道闸。
func zaOrderZones(zones []zaZone, sheet layoutBBox) []zaZoneCtx {
	zs := make([]zaZoneCtx, 0, len(zones))
	for _, z := range zones {
		chain, dist := zaEdgeChain(z.Home, z.Edge, sheet)
		zs = append(zs, zaZoneCtx{zaZone: z, chain: chain, dist: dist})
	}
	along := func(z zaZoneCtx) float64 {
		switch z.chain[0] {
		case "W", "E":
			return -z.Home[1]
		default:
			return z.Home[0]
		}
	}
	sort.SliceStable(zs, func(i, j int) bool {
		if a, b := zaEdgeIdx(zs[i].chain[0]), zaEdgeIdx(zs[j].chain[0]); a != b {
			return a < b
		}
		if a, b := along(zs[i]), along(zs[j]); a != b {
			return a < b
		}
		return tidyDesignatorLess(zs[i].Name, zs[j].Name)
	})
	return zs
}

// zonesArrange 是求解器本体。纯函数;输入顺序与输出无关(内部全序排序)。
//
// 落位规则:每区沿归属链逐边尝试;每条边可开**多层货架**(深度候选 = 贴边 ∪
// 各障碍朝内面 + gutter),每层货架沿边轴按接触点候选滑动(W/E 自上而下,
// N/S 自左而右),与全部障碍(图签 keep-out 按 titleBlockSafety 膨胀 —— 与
// validatePartitions 同一口径,同一把尺 —— ∪ 已落位框,均再按 gutter 膨胀)无交
// 即落位;链尽 → 回退到上一个区换候选;全部回退用尽(或预算耗尽)→ blocked。
func zonesArrange(zones []zaZone, sheet layoutBBox, keepout *layoutBBox, opts partitionOpts) zaResult {
	s := newZaSearch(zones, sheet, keepout, opts)
	zs := s.zs
	if !s.solve(0) {
		who := ""
		if s.deepest >= 0 && s.deepest < len(zs) {
			who = zs[s.deepest].Name
		} else if len(zs) > 0 {
			who = zs[len(zs)-1].Name // 预算耗尽且从未记录到失败层:点最后一个区
		}
		return zaResult{OK: false, Blocked: who, Tried: zaTriedText(s.probe),
			Edges: s.probe, Exhausted: s.overrun}
	}
	out := make([]zaPlaced, 0, len(zs))
	for i, c := range s.sol {
		out = append(out, zaPlaced{Name: zs[i].Name, Rect: c.rect, Edge: c.edge,
			Chain: zs[i].chain, Shelf: c.shelf, Steps: zs[i].landedStep})
	}
	// 输出按区名自然序 —— 序列化后可直接做确定性哈希比对。
	sort.SliceStable(out, func(i, j int) bool { return tidyDesignatorLess(out[i].Name, out[j].Name) })
	return zaResult{OK: true, Placed: out}
}

// zaTriedText 把逐边归因折成一句能执行的解释:每条边的质心距离 + 卡在谁身上。
func zaTriedText(probe []zaEdgeProbe) string {
	if len(probe) == 0 {
		return "搜索预算耗尽,未能证明无解"
	}
	var b strings.Builder
	for i, p := range probe {
		if i > 0 {
			b.WriteString("→")
		}
		fmt.Fprintf(&b, "%s(%.0f)", p.Edge, p.Dist)
		switch {
		case p.Cands == 0:
			b.WriteString("纸面放不下")
		case p.Blocker != "":
			b.WriteString("被" + p.Blocker + "挡")
		default:
			b.WriteString("无空位")
		}
	}
	return b.String()
}

// zaValidate 用**既有的 validatePartitions**(同一把尺)验证落位框:
// 把每个落位框折成 partitionRect(区名带在顶、说明带在底,与 zone-plan 同版式)。
// modules 传 nil 时只验框级四项(overflow/overlap/titleHits/marginHits)。
func zaValidate(res zaResult, sheet layoutBBox, keepout *layoutBBox, opts partitionOpts) partitionValidation {
	plan := partitionPlan{Sheet: sheet, Keepout: keepout}
	for _, p := range res.Placed {
		plan.Partitions = append(plan.Partitions, partitionRect{
			Modules: []string{p.Name},
			BBox:    p.Rect,
			TitleBBox: layoutBBox{MinX: p.Rect.MinX, MinY: p.Rect.MaxY - opts.TitleBand,
				MaxX: p.Rect.MaxX, MaxY: p.Rect.MaxY},
			NoteBBox: layoutBBox{MinX: p.Rect.MinX, MinY: p.Rect.MinY,
				MaxX: p.Rect.MaxX, MaxY: p.Rect.MinY + opts.NoteBand},
		})
	}
	return validatePartitions(plan, nil, keepout)
}
