package app

// sch_zone_follow.go — phase A 区内收敛:跟随规则 R1–R5(用户裁定 2026-08-16)。
//
//	R1 跟随:卫星无源件朝向 = 锚件朝向(原理图锚件恒 upright → 卫星一律竖放、
//	   互相平行);多脚件保持符号朝向,端子保持实测侧。
//	R2 排列轴 = 贴边切向:卫星贴锚件下方 → 横排一排竖立的件(顶边对齐);
//	   贴左/右 → 竖列堆叠。贴哪侧取 argmin max(宽,高),平局序 左<右<下。
//	R3 上下端推导(**不查「电源上/地下」固定表** —— 那条从规定降级为推论):
//	   有 rail 脚的件 GND 端朝下、电源端朝上;双信号件原左脚→上(+90° 旋转约定)。
//	R4 标签:旗顺引脚朝外(上端旗朝上、下端旗朝下);netport 恒水平(铁则4,
//	   竖放会折叠长条标),无源件的 port 统一朝右(阅读方向),多脚件保持实测侧。
//	R5 硬不变式:同件端子几何不得互相重叠(「同件两旗同向必自短路」的可执行形式,
//	   真机踩过的自短路防线)—— 规划器必须**单独校验**,不能假设 R3 蕴含它。
//
// 规划是纯函数:输入 L1 组的类型化描述(本体尺寸 + 端子实测宽高/网名/挂侧),
// 输出区内局部坐标的落位(本体 + 桩线 + 端子)与收敛后的框尺寸。**重生短桩**
// (zfStub=20)是收敛的核心:实测里横跨半页的长导线是跨组走线,不属于组内几何。
// 落地执行(转向/挪件/重连)走 ADR-0003 舞步,归 --apply 层。

import (
	"fmt"
	"sort"
)

const (
	zfStub      = 20.0 // 重生短桩长(引脚 → 旗/port 起点)
	zfPitch     = 12.0 // 多脚件同侧端子的纵向节距
	zfPortH     = 11.0 // netport 标签高(实测 10~12,取平台默认)
	zfFlagGap   = 6.0  // 本体/桩线与旗体的间隙
	zfGroupGap  = 10.0 // 卫星之间的间距
	zfAnchorGap = 12.0 // 锚件与卫星排/列的间距
)

// zfTerm 是一个端子的类型化描述(从 schCluster 的归属 marker 折出)。
type zfTerm struct {
	Kind string  // "netflag" | "netport"
	Net  string  //
	W, H float64 // 标签实测尺寸(netflag 用;netport 高一律 zfPortH)
	Side string  // 实测挂侧:"left"|"right"|"up"|"down"(相对器件本体)
}

// zfGroup 是 phase A 的一个输入 L1 组。
type zfGroup struct {
	Designator   string
	BodyW, BodyH float64
	// MultiPin:引脚数 > 2。R1 对它不转向(符号管脚定义锁死),端子保持实测侧。
	MultiPin bool
	Terms    []zfTerm
}

// zfPlacedTerm 是端子落位(区内局部坐标,y-UP)。
type zfPlacedTerm struct {
	Kind string     `json:"kind"`
	Net  string     `json:"net"`
	Dir  string     `json:"dir"` // 旗:up/down/left/right;port:left/right(恒水平)
	BBox layoutBBox `json:"bbox"`
}

// zfPlacedGroup 是一个组的落位。
type zfPlacedGroup struct {
	Designator string         `json:"designator"`
	Rotated    bool           `json:"rotated"` // 原横放的无源件转竖(执行侧 +90°)
	Body       layoutBBox     `json:"body"`
	Terms      []zfPlacedTerm `json:"terms"`
	Wires      []layoutBBox   `json:"wires"` // 桩线段(占位/渲染)
}

// zfZonePlan 是一个区的收敛结果。
type zfZonePlan struct {
	Zone   string          `json:"zone"`
	Mode   string          `json:"mode"`
	Groups []zfPlacedGroup `json:"groups"`
	// Content 是全图元并集(区内局部);FrameW/H = Content + 2·pad + 区名带 + 说明带
	// —— 与 zone-plan 的框口径同一算式(标签入框)。
	Content layoutBBox `json:"content"`
	FrameW  float64    `json:"frameW"`
	FrameH  float64    `json:"frameH"`
}

// zfBBoxUnion 并集(零值安全:base 为空时直接取 b)。
func zfGrow(dst *layoutBBox, has *bool, b layoutBBox) {
	if !*has {
		*dst = b
		*has = true
		return
	}
	dst.MinX = minF(dst.MinX, b.MinX)
	dst.MinY = minF(dst.MinY, b.MinY)
	dst.MaxX = maxF(dst.MaxX, b.MaxX)
	dst.MaxY = maxF(dst.MaxY, b.MaxY)
}

// zfGenGroup 生成一个组的局部几何(本体 min 角在原点)。
func zfGenGroup(g zfGroup) (zfPlacedGroup, error) {
	if g.MultiPin {
		return zfGenMultiPin(g), nil
	}
	return zfGenPassive(g)
}

// zfGenPassive:R1 竖放 + R3 上下端推导 + R4 端子几何。
func zfGenPassive(g zfGroup) (zfPlacedGroup, error) {
	bw, bh := g.BodyW, g.BodyH
	rotated := false
	if bw > bh { // R1:一律竖放(原横放 → 执行侧 +90°)
		bw, bh = bh, bw
		rotated = true
	}
	out := zfPlacedGroup{Designator: g.Designator, Rotated: rotated,
		Body: layoutBBox{MinX: 0, MinY: 0, MaxX: bw, MaxY: bh}}
	if len(g.Terms) > 2 {
		return out, fmt.Errorf("%s: 无源件端子数 %d > 2 —— MultiPin 标错了?", g.Designator, len(g.Terms))
	}
	top, bot := zfAssignEnds(g.Terms)
	cx := bw / 2
	place := func(t *zfTerm, up bool) {
		if t == nil {
			return
		}
		y0 := 0.0
		dir := "down"
		if up {
			y0, dir = bh, "up"
		}
		y1 := y0 - zfStub
		if up {
			y1 = y0 + zfStub
		}
		out.Wires = append(out.Wires, layoutBBox{MinX: cx, MinY: minF(y0, y1), MaxX: cx, MaxY: maxF(y0, y1)})
		if t.Kind == "netflag" { // R4:旗顺引脚朝外
			b := layoutBBox{MinX: cx - t.W/2, MaxX: cx + t.W/2}
			if up {
				b.MinY, b.MaxY = y1, y1+t.H
			} else {
				b.MinY, b.MaxY = y1-t.H, y1
			}
			out.Terms = append(out.Terms, zfPlacedTerm{Kind: t.Kind, Net: t.Net, Dir: dir, BBox: b})
			return
		}
		// R4:netport 恒水平,无源件统一朝右(阅读方向)
		out.Terms = append(out.Terms, zfPlacedTerm{Kind: t.Kind, Net: t.Net, Dir: "right",
			BBox: layoutBBox{MinX: cx, MinY: y1 - zfPortH/2, MaxX: cx + t.W, MaxY: y1 + zfPortH/2}})
	}
	place(top, true)
	place(bot, false)
	return out, zfCheckTermOverlap(out)
}

// zfAssignEnds 是 R3 本体:GND→下、电源→上、双信号原左→上。
// 「电源上/地下」在这里是**推论**:竖放 + 旗顺引脚朝外 + rail 归位。
func zfAssignEnds(terms []zfTerm) (top, bot *zfTerm) {
	if len(terms) == 0 {
		return nil, nil
	}
	if len(terms) == 1 {
		t := terms[0]
		if tidyNetClass(t.Net) == "ground" {
			return nil, &t
		}
		return &t, nil
	}
	a, b := terms[0], terms[1]
	ca, cb := tidyNetClass(a.Net), tidyNetClass(b.Net)
	switch {
	case ca == "ground" && cb != "ground":
		return &b, &a
	case cb == "ground" && ca != "ground":
		return &a, &b
	case ca == "power" && cb != "power":
		return &a, &b
	case cb == "power" && ca != "power":
		return &b, &a
	}
	// 双信号(或双同类):原左/原上 → 上(+90° 旋转约定)。
	if b.Side == "left" || b.Side == "up" {
		return &b, &a
	}
	return &a, &b
}

// zfGenMultiPin:多脚件保持符号朝向,端子保持实测侧;同侧端子按 zfPitch 纵向
// 排布(左右侧)或沿本体宽度横向散开(上下侧的旗 —— **不许竖叠**,那正是
// 「同件两旗同向自短路」的几何)。
func zfGenMultiPin(g zfGroup) zfPlacedGroup {
	bw, bh := g.BodyW, g.BodyH
	out := zfPlacedGroup{Designator: g.Designator,
		Body: layoutBBox{MinX: 0, MinY: 0, MaxX: bw, MaxY: bh}}
	bySide := map[string][]zfTerm{}
	for _, t := range g.Terms {
		bySide[t.Side] = append(bySide[t.Side], t)
	}
	// 左/右:自上而下 zfPitch 节距;port 水平指向实测侧,旗亦同侧。
	for _, side := range []string{"left", "right"} {
		y := bh - zfPortH
		for _, t := range bySide[side] {
			x0, x1 := -zfStub, 0.0
			if side == "right" {
				x0, x1 = bw, bw+zfStub
			}
			cy := y + zfPortH/2
			out.Wires = append(out.Wires, layoutBBox{MinX: x0, MinY: cy, MaxX: x1, MaxY: cy})
			var b layoutBBox
			h := t.H
			if t.Kind == "netport" {
				h = zfPortH
			}
			if side == "left" {
				b = layoutBBox{MinX: x0 - t.W, MinY: cy - h/2, MaxX: x0, MaxY: cy + h/2}
			} else {
				b = layoutBBox{MinX: x1, MinY: cy - h/2, MaxX: x1 + t.W, MaxY: cy + h/2}
			}
			out.Terms = append(out.Terms, zfPlacedTerm{Kind: t.Kind, Net: t.Net, Dir: side, BBox: b})
			y -= zfPitch
		}
	}
	// 上/下:按**实际旗宽**排成横向序列,居中于本体 —— 首版按 (i+1)/(n+1) 均匀
	// 散开,没算旗宽,U3 的 GND(37宽)与 5V(23宽)在 72 宽的本体下重叠 6 个单位,
	// 被 R5 校验器当场抓住 —— 判定与生成分离的价值。旗顺引脚朝外。
	for _, side := range []string{"down", "up"} {
		total := 0.0
		for i, t := range bySide[side] {
			total += t.W
			if i > 0 {
				total += zfFlagGap
			}
		}
		x := bw/2 - total/2
		for _, t := range bySide[side] {
			cx := x + t.W/2
			x += t.W + zfFlagGap
			y0, dir := 0.0, "down"
			if side == "up" {
				y0, dir = bh, "up"
			}
			y1 := y0 - zfStub
			if side == "up" {
				y1 = y0 + zfStub
			}
			out.Wires = append(out.Wires, layoutBBox{MinX: cx, MinY: minF(y0, y1), MaxX: cx, MaxY: maxF(y0, y1)})
			b := layoutBBox{MinX: cx - t.W/2, MaxX: cx + t.W/2}
			if side == "up" {
				b.MinY, b.MaxY = y1, y1+t.H
			} else {
				b.MinY, b.MaxY = y1-t.H, y1
			}
			out.Terms = append(out.Terms, zfPlacedTerm{Kind: t.Kind, Net: t.Net, Dir: dir, BBox: b})
		}
	}
	return out
}

// zfCheckTermOverlap 是 R5 的可执行形式:同件端子几何互不重叠。
// 单独校验,不假设 R3 蕴含它 —— 判定与生成分离。
func zfCheckTermOverlap(g zfPlacedGroup) error {
	for i := 0; i < len(g.Terms); i++ {
		for j := i + 1; j < len(g.Terms); j++ {
			if boxesOverlap(g.Terms[i].BBox, g.Terms[j].BBox) {
				return fmt.Errorf("%s: 端子重叠 %s(%s) × %s(%s) —— R5 硬不变式(自短路防线)",
					g.Designator, g.Terms[i].Net, g.Terms[i].Dir, g.Terms[j].Net, g.Terms[j].Dir)
			}
		}
	}
	return nil
}

// zfGroupBBox 组的全图元并集。
func zfGroupBBox(g zfPlacedGroup) layoutBBox {
	b, has := layoutBBox{}, false
	zfGrow(&b, &has, g.Body)
	for _, t := range g.Terms {
		zfGrow(&b, &has, t.BBox)
	}
	for _, w := range g.Wires {
		zfGrow(&b, &has, w)
	}
	return b
}

// zfTranslate 平移一个组(局部 → 区内布置)。
func zfTranslate(g zfPlacedGroup, dx, dy float64) zfPlacedGroup {
	sh := func(b layoutBBox) layoutBBox {
		return layoutBBox{MinX: b.MinX + dx, MinY: b.MinY + dy, MaxX: b.MaxX + dx, MaxY: b.MaxY + dy}
	}
	out := g
	out.Body = sh(g.Body)
	out.Terms = append([]zfPlacedTerm(nil), g.Terms...)
	for i := range out.Terms {
		out.Terms[i].BBox = sh(out.Terms[i].BBox)
	}
	out.Wires = make([]layoutBBox, len(g.Wires))
	for i, w := range g.Wires {
		out.Wires[i] = sh(w)
	}
	return out
}

// planZoneFollow 是 phase A 主入口:一个区的全部 L1 组 → 收敛布置。
// 纯函数;输入顺序无关(内部按位号自然序全序排序)。
func planZoneFollow(zone string, groups []zfGroup, opts partitionOpts) (zfZonePlan, error) {
	gs := append([]zfGroup(nil), groups...)
	sort.SliceStable(gs, func(i, j int) bool { return tidyDesignatorLess(gs[i].Designator, gs[j].Designator) })

	type genned struct {
		g  zfPlacedGroup
		bb layoutBBox
	}
	gen := make([]genned, 0, len(gs))
	for _, g := range gs {
		pg, err := zfGenGroup(g)
		if err != nil {
			return zfZonePlan{}, err
		}
		gen = append(gen, genned{pg, zfGroupBBox(pg)})
	}
	plan := zfZonePlan{Zone: zone}
	area := func(x genned) float64 { return (x.bb.MaxX - x.bb.MinX) * (x.bb.MaxY - x.bb.MinY) }
	byArea := append([]genned(nil), gen...)
	sort.SliceStable(byArea, func(i, j int) bool {
		if a, b := area(byArea[i]), area(byArea[j]); a != b {
			return a > b
		}
		return tidyDesignatorLess(byArea[i].g.Designator, byArea[j].g.Designator)
	})

	putAt := func(g genned, dx, dy float64) {
		plan.Groups = append(plan.Groups, zfTranslate(g.g, dx, dy))
	}
	switch {
	case len(gen) == 1:
		plan.Mode = "单组:重生短桩,不再动"
		putAt(gen[0], -gen[0].bb.MinX, -gen[0].bb.MinY)
	case area(byArea[0]) < 2*area(byArea[1]):
		// 无主导锚件 → 全员单列,位号序,左缘对齐,自上而下。
		plan.Mode = "无主导锚件 → 全员单列(位号序)"
		y := 0.0
		for _, g := range gen {
			putAt(g, -g.bb.MinX, y-g.bb.MaxY)
			y -= (g.bb.MaxY - g.bb.MinY) + zfGroupGap
		}
	default:
		anchor := byArea[0]
		sats := make([]genned, 0, len(gen)-1)
		for _, g := range gen {
			if g.g.Designator != anchor.g.Designator {
				sats = append(sats, g)
			}
		}
		// R2:贴侧 = argmin max(w,h),平局序 左<右<下。
		colW, colH, rowW, rowH := 0.0, 0.0, 0.0, 0.0
		for i, s := range sats {
			w, h := s.bb.MaxX-s.bb.MinX, s.bb.MaxY-s.bb.MinY
			colW = maxF(colW, w)
			colH += h
			rowW += w
			rowH = maxF(rowH, h)
			if i > 0 {
				colH += zfGroupGap
				rowW += zfGroupGap
			}
		}
		aw := anchor.bb.MaxX - anchor.bb.MinX
		ah := anchor.bb.MaxY - anchor.bb.MinY
		sideCost := map[string]float64{
			"left":  maxF(aw+zfAnchorGap+colW, maxF(ah, colH)),
			"right": maxF(aw+zfAnchorGap+colW, maxF(ah, colH)),
			"below": maxF(maxF(aw, rowW), ah+zfAnchorGap+rowH),
		}
		best := "left"
		for _, k := range []string{"left", "right", "below"} {
			if sideCost[k] < sideCost[best] {
				best = k
			}
		}
		label := map[string]string{"left": "列(左)", "right": "列(右)", "below": "排(下,竖放平行)"}
		plan.Mode = fmt.Sprintf("锚件 %s + 卫星%s · argmin max(w,h)", anchor.g.Designator, label[best])
		putAt(anchor, -anchor.bb.MinX, -anchor.bb.MinY)
		if best == "below" {
			// 横排一排竖立的件,顶边对齐在锚件下缘 - gap。
			x := 0.0
			for _, s := range sats {
				putAt(s, x-s.bb.MinX, -zfAnchorGap-s.bb.MaxY)
				x += (s.bb.MaxX - s.bb.MinX) + zfGroupGap
			}
		} else {
			dx := aw + zfAnchorGap
			if best == "left" {
				dx = -zfAnchorGap - colW
			}
			y := ah
			for _, s := range sats {
				putAt(s, dx-s.bb.MinX, y-s.bb.MaxY)
				y -= (s.bb.MaxY - s.bb.MinY) + zfGroupGap
			}
		}
	}
	// 组间不重叠断言(间距由构造保证,但判定与生成分离 —— 出错要在规划期炸,
	// 不能等落地把画布弄脏)。
	for i := 0; i < len(plan.Groups); i++ {
		for j := i + 1; j < len(plan.Groups); j++ {
			if boxesOverlap(zfGroupBBox(plan.Groups[i]), zfGroupBBox(plan.Groups[j])) {
				return zfZonePlan{}, fmt.Errorf("%s: 组 %s 与 %s 布置后重叠 —— 规划器缺陷",
					zone, plan.Groups[i].Designator, plan.Groups[j].Designator)
			}
		}
	}
	has := false
	for _, g := range plan.Groups {
		zfGrow(&plan.Content, &has, zfGroupBBox(g))
	}
	plan.FrameW = (plan.Content.MaxX - plan.Content.MinX) + 2*partitionContentPad
	plan.FrameH = (plan.Content.MaxY - plan.Content.MinY) + 2*partitionContentPad + opts.TitleBand + opts.NoteBand
	return plan, nil
}
