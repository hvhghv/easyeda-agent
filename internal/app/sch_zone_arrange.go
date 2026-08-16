package app

// sch_zone_arrange.go — 功能区区间布局求解器(phase B):边归属 + 回退链 + 货架扫描。
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
	Steps int        `json:"steps"` // 扫描步数(可回放性)
}

// zaResult 是三态输出。
type zaResult struct {
	OK      bool       `json:"ok"`
	Placed  []zaPlaced `json:"placed,omitempty"`
	Blocked string     `json:"blocked,omitempty"`
	// Tried 记录 blocked 区的回退链与各边质心距离,给人一句能执行的解释。
	Tried string `json:"tried,omitempty"`
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

// zonesArrange 是求解器本体。纯函数;输入顺序与输出无关(内部全序排序)。
//
// 扫描规则(与演示页§六一致):每区沿归属链逐边尝试;每条边从规范角出发,
// **只沿本边轴**以 5 为步长滑动(W/E 自上而下,N/S 自左而右),直至与全部障碍
// (图签 keep-out 按 titleBlockSafety 膨胀 —— 与 validatePartitions 同一口径,
// 同一把尺 —— ∪ 已落位框,均再按 gutter 膨胀)无交;本边扫尽 → 链上下一条;
// 链尽 → blocked。
func zonesArrange(zones []zaZone, sheet layoutBBox, keepout *layoutBBox, opts partitionOpts) zaResult {
	L := snap5Up(sheet.MinX + opts.Margin)
	R := snap5Dn(sheet.MaxX - opts.Margin)
	B := snap5Up(sheet.MinY + opts.Margin)
	T := snap5Dn(sheet.MaxY - opts.Margin)
	safe := inflatedTitleKeepout(keepout) // 与验证器同一膨胀基准,防两把尺

	type zc struct {
		zaZone
		chain []string
		dist  map[string]float64
	}
	zs := make([]zc, 0, len(zones))
	for _, z := range zones {
		chain, dist := zaEdgeChain(z.Home, z.Edge, sheet)
		zs = append(zs, zc{z, chain, dist})
	}
	// 全序:首选边序(W<E<N<S)→ 沿边坐标(W/E 自上而下 = 大 y 在前;N/S 自左而右)
	// → 区名自然序。输入顺序被彻底抹掉。
	along := func(z zc) float64 {
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

	var placed []layoutBBox
	var out []zaPlaced
	for _, z := range zs {
		var rect *layoutBBox
		landed, steps := "", 0
		for _, e := range z.chain {
			for t := 0.0; ; t += zaScanStep {
				steps++
				var r layoutBBox
				switch e {
				case "W":
					r = layoutBBox{MinX: L, MinY: T - t - z.H, MaxX: L + z.W, MaxY: T - t}
				case "E":
					r = layoutBBox{MinX: R - z.W, MinY: T - t - z.H, MaxX: R, MaxY: T - t}
				case "N":
					r = layoutBBox{MinX: L + t, MinY: T - z.H, MaxX: L + t + z.W, MaxY: T}
				case "S":
					r = layoutBBox{MinX: L + t, MinY: B, MaxX: L + t + z.W, MaxY: B + z.H}
				}
				if (e == "W" || e == "E") && r.MinY < B-1e-9 {
					break // 本边纵向扫尽
				}
				if (e == "N" || e == "S") && r.MaxX > R+1e-9 {
					break // 本边横向扫尽
				}
				clear := safe == nil || !zaHit(r, *safe, opts.Gutter)
				if clear {
					for _, p := range placed {
						if zaHit(r, p, opts.Gutter) {
							clear = false
							break
						}
					}
				}
				if clear {
					rr := r
					rect = &rr
					landed = e
					break
				}
			}
			if rect != nil {
				break
			}
		}
		if rect == nil {
			var tried strings.Builder
			for i, e := range z.chain {
				if i > 0 {
					tried.WriteString("→")
				}
				fmt.Fprintf(&tried, "%s(%.0f)", e, z.dist[e])
			}
			return zaResult{OK: false, Blocked: z.Name, Tried: tried.String()}
		}
		placed = append(placed, *rect)
		out = append(out, zaPlaced{Name: z.Name, Rect: *rect, Edge: landed, Chain: z.chain, Steps: steps})
	}
	// 输出按区名自然序 —— 序列化后可直接做确定性哈希比对。
	sort.SliceStable(out, func(i, j int) bool { return tidyDesignatorLess(out[i].Name, out[j].Name) })
	return zaResult{OK: true, Placed: out}
}

// zaScanStep:扫描步长 = EasyEDA 连接格。落位框都在「规范角 + 5k」格律上;
// 执行侧再把每个件的平移量圆整到 5(件是格点公民,框角跟着件走)。
const zaScanStep = 5.0

func snap5Up(v float64) float64 {
	s := snap5(v)
	if s < v {
		return s + 5
	}
	return s
}

func snap5Dn(v float64) float64 {
	s := snap5(v)
	if s > v {
		return s - 5
	}
	return s
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
