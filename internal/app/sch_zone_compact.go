package app

// sch_zone_compact.go — 区内收敛:把宽扁的功能区压成近似方块。
//
// 用户实测提出(2026-08-16,对着 P3_USB_DEBUG 的画布):四个功能区互相穿插、
// 各自横跨大半页,「起码要往自己所在的边靠近」。查下去发现两件事:
//
//  1. **面积根本不是瓶颈** —— 四个区面积和只占可用区的 28~34%,`group-arrange`
//     却报「装不下」。排不下的是**形状**:J_USB 478×166(长宽比 2.9)这样的宽扁条,
//     放哪都横占半页,行排必然溢出。
//  2. **现有两个排布器都只排不收**(`group-arrange` / `sheet tidy` 把区当刚体铺),
//     而 `group tidy` 只收**双旗无源件**(power-updown 横排),对挂 netport 的
//     signal-row 件**只改标签方向、不动位置** —— J_USB 的 R3/R4/J1 就这么横着排开。
//
// 所以缺的是这一块:**signal-row 件的位置重排**。
//
// 判据是**长宽比自适应**,不是无脑竖排:横排让宽度累加、高度不变,竖排反过来。
// A4 横放(1170×825,扣图签后可用高 571)高度更金贵,所以只有**已经宽扁**的区才该
// 转竖排 —— 对已经接近方形的区(实测 Q 区 1.3)动手只会把它推向另一个极端。
//
// 落地机制早就在:`tidySignalPlan` 带 HasPose+X/Y,执行侧会先 modify 件再重连
// (zone relayout 用的,组内 tidy 只是没填过)。所以这里只出规划,复用既有的
// 深度清扫 → 重建 → 自检 → 回滚。

import (
	"fmt"
	"sort"
)

// zoneCompactAspectTrigger 是「该转竖排」的长宽比门槛。
//
// 2.0 的来历:实测四个区 1.3 / 1.6 / 1.7 / 2.9 —— 前三个是正常的略扁,最后一个
// (J_USB)才是真正排不进去的宽扁条。门槛压到 1.7 会把本来好好的区也翻一遍,
// 而每翻一次都要删桩重连(有串网风险),不值得。
const zoneCompactAspectTrigger = 2.0

// zoneCompactPlan 是一个区的收敛计划。
type zoneCompactPlan struct {
	Zone string `json:"zone"`
	// BeforeW/H 与 AfterW/H 是收敛前后的**预估** bbox(只算器件本体 + 排布跨度,
	// 不含 marker —— marker 的实际伸展要落地后重测)。
	BeforeW float64          `json:"beforeW"`
	BeforeH float64          `json:"beforeH"`
	AfterW  float64          `json:"afterW"`
	AfterH  float64          `json:"afterH"`
	Aspect  float64          `json:"aspectBefore"`
	Moves   []tidySignalPlan `json:"-"`
	Reason  string           `json:"reason"`
}

// planSignalColumn 把 signal-row 件**竖向堆叠**在锚件的一侧。
//
// 与 planSignalRow 的分工:那个只管「netport 一律水平」(铁则4,竖放会折叠长条标),
// 不动件的位置;这个在保持 netport 水平的前提下，把件本身从横排改成竖排。
// 两者的 netport 方向规则**必须一致** —— 标签朝远离锚件的一侧,否则标签会插回
// 区内、把区从内部撑开。
//
// side: -1 排在锚件左侧,+1 右侧。选空间大的那侧由调用方决定。
func planSignalColumn(members []tidySignalMemberIn, anchor tidyAnchor, spacing, side float64) ([]tidySignalPlan, error) {
	if len(members) == 0 {
		return nil, nil
	}
	if spacing <= 0 {
		spacing = tidyDefaultSpacing
	}
	if side == 0 {
		side = -1
	}
	ordered := append([]tidySignalMemberIn(nil), members...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return tidyDesignatorLess(ordered[i].Designator, ordered[j].Designator)
	})
	// 列心对齐锚件中心:竖排在锚件正侧方,不上下漂。
	colX := snap5(anchor.X + side*(anchor.HalfWidth+spacing))
	startY := snap5(anchor.Y - spacing*float64(len(ordered)-1)/2)

	out := make([]tidySignalPlan, 0, len(ordered))
	for i, m := range ordered {
		y := snap5(startY + spacing*float64(i))
		var pins []tidyPinTarget
		for _, p := range m.Pins {
			if !p.IsPort {
				continue
			}
			// **标签一律朝远离锚件的那一侧**。planSignalRow 是按 pin 相对件心
			// 判左右(件不动时那是对的),这里件要挪到锚件侧方,再按老规则判就会
			// 让一半标签指回锚件、从内部把区撑开 —— 收敛的收益当场抵消。
			dir := "left"
			if side > 0 {
				dir = "right"
			}
			rot, err := tidyLabelRotation("netport", dir)
			if err != nil {
				return nil, err
			}
			pins = append(pins, tidyPinTarget{Pin: p.Pin, Direction: dir, Kind: "net_port_bi", Net: p.Net, LabelRotation: rot})
		}
		if len(pins) == 0 {
			continue
		}
		out = append(out, tidySignalPlan{
			Designator: m.Designator, Pins: pins,
			HasPose: true, X: colX, Y: y,
		})
	}
	return out, nil
}

// zoneCompactAspect 算长宽比(恒 ≥1;退化尺寸给 1 表示「无从判断,不动」)。
func zoneCompactAspect(w, h float64) float64 {
	if w <= 0 || h <= 0 {
		return 1
	}
	if w > h {
		return w / h
	}
	return h / w
}

// shouldCompactZone 判一个区该不该转竖排。**只压宽扁的**:高瘦的区转横排是另一
// 回事(A4 横放高度更金贵,把区摊宽通常反而更好排),这里不做。
func shouldCompactZone(w, h float64) (bool, string) {
	a := zoneCompactAspect(w, h)
	switch {
	case a < zoneCompactAspectTrigger:
		return false, fmt.Sprintf("长宽比 %.1f < %.1f —— 已经够方,重排的收益抵不过删桩重连的风险", a, zoneCompactAspectTrigger)
	case h > w:
		return false, fmt.Sprintf("长宽比 %.1f 但**高瘦**(%.0f×%.0f)—— 竖排只会更高;A4 横放高度更金贵,留给区间布局处理", a, w, h)
	default:
		return true, fmt.Sprintf("长宽比 %.1f 的宽扁条(%.0f×%.0f)—— 卫星件转竖排,宽度不再累加", a, w, h)
	}
}

// estimateColumnBBox 估算竖排后的区尺寸:宽 = 锚件宽 + 一列件的横向占位,
// 高 = max(锚件高, 列高)。这是**预估**,用于 dry-run 报告;真实 bbox 由落地后
// 的 `sch clusters` 实测 —— 两者不该混为一谈(marker 的实际伸展只有落地才知道)。
func estimateColumnBBox(anchorW, anchorH, colItemW, spacing float64, n int) (w, h float64) {
	if n <= 0 {
		return anchorW, anchorH
	}
	w = anchorW + spacing + colItemW
	h = anchorH
	if colH := spacing * float64(n-1); colH+colItemW > h {
		h = colH + colItemW
	}
	return w, h
}

// tidyPatternSignalColumn 是 `group tidy --pattern` 的新档:signal-row 件竖排。
const tidyPatternSignalColumn = "signal-column"

// tidySignalStaged 是收集阶段的暂存:件的 live 状态、规划输入、以及要竖直化的
// 电源/地旗。分两遍是因为**列内 Y 分布要知道全部成员**才算得出来。
type tidySignalStaged struct {
	Live  tidyLiveMember
	In    tidySignalMemberIn
	Rails []tidyRailPin
}

// tidyColumnSide 选把列排在锚件的哪一侧:**跟随这些件当前的重心**。
//
// 不选「页面空间大的那侧」是有意的:那要把纸面尺寸传进这个纯函数,而且会让同一
// 组在不同页得出不同结果;跟随重心则是就近收拢 —— 件原本在锚件左边就排左边,
// 视觉跳变最小,跨区连线也不会平白绕到另一侧。
func tidyColumnSide(ins []tidySignalMemberIn, anchor tidyAnchor) float64 {
	if len(ins) == 0 {
		return -1
	}
	var sum float64
	var n int
	for _, in := range ins {
		if in.CenterX != 0 {
			sum += in.CenterX
			n++
		}
	}
	if n == 0 || sum/float64(n) <= anchor.X {
		return -1
	}
	return 1
}

// ── 接线状态:**未接入**(2026-08-16 真机回退)────────────────────────────────
//
// 试过把 planSignalColumn 接进 `group tidy --pattern signal-column`,在 J_USB
// (g2: J1/R3/R4)上真机跑,**把 R3/R4 的连接搞断了** —— 回退了那部分改动,
// 规划器与单测留下。现象与线索记在这里,免得下次从零查:
//
//  1. dry-run 正确:三件都出了「竖排落位 @ (600,240/290/340)」,同列不同 Y。
//  2. `--apply` 只落地 1 件(J1)。而 **deep sweep 是按整组删的**(输出
//     「删除 12 个旧桩/旗/残段(整树)」),R3/R4 的桩线跟着被删,重连却没轮到
//     它们 —— 落地后 `sch clusters` 显示两件 marker 0 / 桩线 0,
//     `sch nets` 报 U3_N7 变成单引脚网。
//  3. 抢修:`sch autoconnect` 逐脚补回 4 个连接,18 张网恢复。
//
// 下次接线前要先答清楚的三个问题:
//   • 为什么 plan.Signal 到执行侧只剩一件?(dry-run 与 apply 走的是同一个
//     buildTidyPlan,差别只在 forceAll —— 先把两条路径的 plan 打出来对比)
//   • deep sweep 的删除范围与 plan.Signal 的重建范围**必须同集**,现在没有任何
//     判据在保证这件事 —— 少一件就是静默断线。这条不变式该做成执行前的断言。
//   • 断线发生在 tidyApply 内部,而它的自检(layout-lint + bridge-check)**没报**:
//     两件孤立器件既不重叠也不短路,判据结构上看不见 —— 自检该加一条
//     「sweep 前有连接的 pin,重建后必须仍有连接」。
