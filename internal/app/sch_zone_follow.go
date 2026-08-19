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
//
// ── 一把尺:端子几何只许问 zfTermGeom 要(2026-08-20 收敛性缺陷定案)──────────
//
// 真机连跑 4 轮取证:每轮 dry-run 都 `verdict: pass`、validation 四项全 0,落地
// 后 `zone-plan` 实测**必然**重叠(2 / 1 / — / 2 处)。规划尺寸 vs 落地实测:
// U 区 315×351 → 353×382(宽 +38、高 +31),而排布器的 gutter 只有 12 —— 误差
// 系统性大于间距,「规划无重叠」落地必然可能重叠。这不是抖动,多跑几遍不收敛
// (第 3 轮落位整体重排,J_USB 从 E 边跳到 N 边,那是追尾不是收敛)。
//
// 根因是**同一件事有三套算法**:
//
//	① 规划侧:phase A 首版自己拼端子盒 —— 用实测 marker 宽高、把盒子贴在桩端点上、
//	   无源件的 netport 还画成「桩线朝下、标签朝右」。三条都与落地不符:
//	     - 落地的 marker 本体从端点起**空出 Near**(netport/gnd 9.5、power 4.5)
//	       才开始画,规划贴着端点画 → 每支端子少算 Near;
//	     - 实测宽高是**旧朝向**下量的,规划换了朝向却不转置 → ±11 的错位;
//	     - connect_pin 的桩只能沿 direction 直出,「桩朝下、标签朝右」执行侧不存在。
//	② 落地侧:--apply 重连时未被计划端子覆盖的 pin 走 autoconnect 自由评分
//	   (offset 18~80,外加 laneStepFor 的标准档位 min+k·lane,netport 上一档 ~89);
//	③ 挪动侧:group-move 的重连同样走自由 autoconnect(真机:U 组 315×389 →
//	   523×406,一次「微调」把 phase A 的收敛撤销了大半)。
//
// 修法是让三处**共用落地侧那条真实函数链**:
//
//	connect_pin(direction, offset) → endpointFor(桩端点,5 网格吸附)
//	                               → predictedMarkerBBox(本体 ∪ 网名带)
//
// 规划期(zfGenPassive / zfGenMultiPin)与复算期(zfLandedGroupBBox)都只经
// zfTermGeom 取几何;落地期由 --apply 的显式端子(zaaTermExec.Offset)与 move
// 内核的 preserve 桩线策略保证 offset 原样执行。于是「规划框」成为「落地框」的
// 可靠预测,zfLandedFrame + 负对照 zfStubFreeAutoconnect 把这条性质钉成机械判据。

import (
	"fmt"
	"sort"
)

const (
	zfStub    = 20.0 // 重生短桩长(引脚 → 旗/port 起点)
	zfPitch   = 12.0 // 多脚件同侧端子的纵向节距
	zfPortH   = 11.0 // netport 标签高(实测 10~12,取平台默认)
	zfFlagGap = 6.0  // 本体/桩线与旗体的间隙
	// 组间/锚卫间距与块布局求解器同一把尺(bslPartGap=20,见 ruler_consistency_test):
	// 首版各立 10/12,P3 真机三处浅擦全是它 —— 规划按裸 bbox 排,check 按**文字
	// 渲染宽度**判(netport 的平台 bbox 只有裸六边形,网名画在外面),渲染外延
	// 吃掉了 6~9 个单位,10 的 gap 当场穿帮。20 是仓库既有的间距基准,不另立数。
	zfGroupGap  = bslPartGap // 卫星之间的间距
	zfAnchorGap = bslPartGap // 锚件与卫星排/列的间距
	// MultiPin 组的裸引脚(无端子的 pin)伸出本体 bbox 之外的最大触达(SOT-23
	// 实测 9~15)。规划器没有 pin 几何,排列时对 MultiPin 邻接的 gap 补这个量,
	// 防两组 pin 端点在走廊里物理同点(隐式短路)。
	zfPinReach = 15.0
	// zfLandSlack 是「规划框 → 落地框」的余量,四周各留一格。
	//
	// 规划在**区内局部坐标**上算,落地在**页面绝对坐标**上算,而 connect_pin 的桩
	// 端点按 5 网格吸附(endpointFor/acSchGrid)—— 两边网格相位不同,单边最多差一格。
	// 判据要求「偏差超过 gutter 就如实报告」,而这一格是**结构性的、可上界的**:
	// 与其让它去撞 gutter,不如把它算进框里,让规划框成为落地框的**上界**而不是估计。
	// 它是框自己的属性(哪个区端子多,哪个区的余量就真的用得上),所以放在这里,
	// 不写进全局 gutter。
	zfLandSlack = acSchGrid
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
//
// BBox 是**导出量**:由 (PinX,PinY,Offset,Dir,Kind,Net,SpreadX) 经 zfTermGeom
// 唯一确定。任何地方手改 BBox 而不动这几个参数,就是又造了一把尺 —— 配对测试
// (TestZfLandedFrame_PredictionEqualsLanding)会当场炸。
type zfPlacedTerm struct {
	Kind string     `json:"kind"`
	Net  string     `json:"net"`
	Dir  string     `json:"dir"` // 旗:up/down/left/right;port:left/right(恒水平)
	BBox layoutBBox `json:"bbox"`
	// Offset 是 pin → 标签锚点的桩长,--apply 原样喂给 connect_pin。规划的几何
	// 必须是执行模型能表达的:connect_pin 的桩只能从 pin 沿 direction 直出
	// offset,别无自由度 —— 所以「怎么错开」只能编码在这里,不能编码在 BBox 的
	// 横向位置里(执行侧没有那个旋钮)。
	Offset float64 `json:"offset"`
	// PinX/PinY 是桩线起点(区内局部)。存下来复算才可能:落地复判要按另一套
	// 桩长重算这支端子的占地,而 BBox 本身已经把桩长烘进去了。
	PinX float64 `json:"pinX"`
	PinY float64 `json:"pinY"`
	// SpreadX 是「规划期不知道 pin 的横向位置」时的不确定带半宽(MultiPin 的
	// 上/下侧:规划器没有符号 pin 几何,旗可能落在本体任意 x)。只加在 x 上,
	// 不参与梯次要用的纵向占地。
	SpreadX float64 `json:"spreadX,omitempty"`
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
	// Slack 是已经算进 Content 的落地余量(zfLandSlack,四周各一格)。输出里
	// 可见 —— 「gutter 按实测偏差上界自适应放大」这件事必须让人看得见,不许
	// 悄悄塞在常数里。
	Slack float64 `json:"slack"`
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

// ── 一把尺:端子几何 ────────────────────────────────────────────────────────

// zfCanonKind 把规划端子折成 connect_pin 的 canonical kind。落地侧
// (zaaConnectKind)与预测侧(predictedMarkerBBox / laneStepFor)共用这一个映射,
// 不许各自 switch —— kind 分家会让「预测的是 power 盒、落地的是 ground 盒」。
func zfCanonKind(kind, net string) string {
	if kind == "netport" {
		return "net_port_bi"
	}
	if tidyNetClass(net) == "ground" {
		return "ground"
	}
	return "power"
}

// zfTermGeom 是「一个端子落地后占多大」的**唯一函数**:走落地侧那条真实链
// connect_pin(direction, offset) → endpointFor(5 网格吸附)→ predictedMarkerBBox
// (marker 本体 ∪ 网名带,与 `sch check` 的 flagTextBand 严格对称)。
//
// 返回桩线段与 marker 包络两个盒子(都在传入坐标系里)。spreadX 见
// zfPlacedTerm.SpreadX。
func zfTermGeom(pinX, pinY, offset float64, dir, kind, net string, spreadX float64) (wire, marker layoutBBox) {
	ex, ey := endpointFor(pinX, pinY, offset, dir)
	wire = layoutBBox{
		MinX: minF(pinX, ex), MinY: minF(pinY, ey),
		MaxX: maxF(pinX, ex), MaxY: maxF(pinY, ey),
	}
	marker = predictedMarkerBBox(ex, ey, zfCanonKind(kind, net), dir, net)
	marker.MinX -= spreadX
	marker.MaxX += spreadX
	return wire, marker
}

// zfAppendTerm 落一个端子:几何一律由 zfTermGeom 导出,桩线与 marker 一并入账。
// 返回带 BBox 的完整端子(梯次要读它的占地)。
func zfAppendTerm(out *zfPlacedGroup, t zfPlacedTerm) zfPlacedTerm {
	wire, marker := zfTermGeom(t.PinX, t.PinY, t.Offset, t.Dir, t.Kind, t.Net, t.SpreadX)
	t.BBox = marker
	out.Wires = append(out.Wires, wire)
	out.Terms = append(out.Terms, t)
	return t
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
		pinY, dir := 0.0, "down"
		if up {
			pinY, dir = bh, "up"
		}
		// R4:旗顺引脚朝外(up/down);netport 恒水平、无源件统一朝右(阅读方向)。
		// **朝向就是 connect_pin 的 direction**:首版把 port 的桩画成竖的、盒子摆
		// 到右边,那形态执行侧根本造不出来(桩只能沿 direction 直出),于是规划的
		// 高度虚高、宽度虚低 —— 落地必然对不上。
		if t.Kind == "netport" {
			dir = "right"
		}
		zfAppendTerm(&out, zfPlacedTerm{Kind: t.Kind, Net: t.Net, Dir: dir,
			PinX: cx, PinY: pinY, Offset: zfStub})
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
	// 旗(netflag)走**水平梯次**:执行侧旗的 y 跟 pin 锁死(zfPitch 只是规划愿望,
	// 真 pin pitch 常是 10 < 旗高 12+),相邻同向两旗纵向必然交叠 —— 唯一可控的
	// 自由度还是桩长,连续旗按前旗宽 + gap 递增错开(P3 真机:J2 左侧 5V/GND
	// 相邻脚旗深叠 22×12,与 U1 三旗竖叠同病,只是转了 90°)。port 恒水平、
	// 高 11 < 最小 pin pitch,保持短桩不参与梯次。
	for _, side := range []string{"left", "right"} {
		y := bh - zfPortH
		off := zfStub
		for _, t := range bySide[side] {
			stub := zfStub
			if t.Kind == "netflag" {
				stub = off
			}
			pinX := 0.0
			if side == "right" {
				pinX = bw
			}
			cy := y + zfPortH/2
			placed := zfAppendTerm(&out, zfPlacedTerm{Kind: t.Kind, Net: t.Net, Dir: side,
				PinX: pinX, PinY: cy, Offset: stub})
			if t.Kind == "netflag" {
				// 梯次步长按**落地占地**(zfTermGeom 出的包络,含网名带)递增,
				// 不再按实测宽 —— 实测宽是旧朝向下量的,换朝向就是错的尺。
				off += (placed.BBox.MaxX - placed.BBox.MinX) + zfFlagGap
			}
			y -= zfPitch
		}
	}
	// 上/下:**垂直梯次**(桩长递增)。二版 —— 首版按实际旗宽排横向序列,几何上
	// 成立,但执行模型表达不了:connect_pin 的桩只能从 pin 沿 direction 直出,
	// pin 的 x 由符号锁死,「旗中心横向挪开」没有对应的旋钮。落地时全部旗退回
	// 默认桩长,pitch 10 的相邻引脚上三旗当场竖叠(P1 U1 打地鼠真因,人肉梯次
	// 20/50/85 顶了算法的班)。梯次把错开量放进唯一可控的自由度 —— 桩长:
	// Offset_i = zfStub + Σ_{j<i}(H_j + gap),y 向分离与 pin 密度无关。
	for _, side := range []string{"down", "up"} {
		off := zfStub
		for _, t := range bySide[side] {
			pinY := 0.0
			if side == "up" {
				pinY = bh
			}
			// 规划期不知道 pin 的 x(符号细节)——桩画在本体中线;marker 盒横向按
			// 「pin 可落本体任意 x」的不确定带展宽(SpreadX = bw/2),框尺寸不低估。
			placed := zfAppendTerm(&out, zfPlacedTerm{Kind: t.Kind, Net: t.Net, Dir: side,
				PinX: bw / 2, PinY: pinY, Offset: off, SpreadX: bw / 2})
			off += (placed.BBox.MaxY - placed.BBox.MinY) + zfFlagGap
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

// zfInflate 四周等量外扩一个盒子(负值即收缩)。
func zfInflate(b layoutBBox, d float64) layoutBBox {
	return layoutBBox{MinX: b.MinX - d, MinY: b.MinY - d, MaxX: b.MaxX + d, MaxY: b.MaxY + d}
}

// ── 收敛性的机械判据:预测 = 落地 ───────────────────────────────────────────

// zfStubPolicy 是「落地侧怎么定桩长」的可替换策略:输入一个已布置组,返回与
// Terms 逐位对应的桩长。**这是三处桩线伸展的那把尺的可插拔形式** —— 换掉它就是
// 换掉落地策略,配对测试的负对照正是靠它成立。
type zfStubPolicy func(g zfPlacedGroup) []float64

// zfStubPlanned 是**现行**落地策略:规划桩长原样执行。
// 它由两条保证共同兑现:
//   - zone-arrange --apply 把每个计划端子的 Offset 显式喂给 connect_pin
//     (zaaTermExec.Offset → moveConnTerm.Offset);
//   - move 内核对未被计划端子覆盖的 pin 走 preserve 策略(原样复现移动前的桩),
//     并把恢复段的 autoconnect 用 OffsetCap 夹住。
func zfStubPlanned(g zfPlacedGroup) []float64 {
	out := make([]float64, len(g.Terms))
	for i, t := range g.Terms {
		out[i] = t.Offset
	}
	return out
}

// zfStubFreeAutoconnect 是**旧的自由 offset 落地策略**的模型 —— 负对照专用,
// 不许在生产路径上用。
//
// 它复刻 autoconnect 的两条实际行为:首支落 rules.OffsetMin(18),同侧第二支起
// 按 laneStepFor 让开前一支的**整个占地**(candidateOffsets 常驻的标准档位
// min+k·lane + applyLaneStagger 的「至少让开一个完整步长」)。netport 的一档是
// ~89 —— 这就是 group-move 把 U 组从 315 宽撑到 523 宽(+208 ≈ 两档)的算术。
func zfStubFreeAutoconnect(g zfPlacedGroup) []float64 {
	rules := defaultAutoconnectRules()
	lane := map[string]float64{}
	out := make([]float64, len(g.Terms))
	for i, t := range g.Terms {
		kind := zfCanonKind(t.Kind, t.Net)
		off := rules.OffsetMin
		if used, ok := lane[t.Dir]; ok {
			off = used + laneStepFor(kind, t.Net)
		}
		lane[t.Dir] = off
		out[i] = off
	}
	return out
}

// zfLandedGroupBBox 按给定桩线策略**重新走一遍落地侧的函数链**,算出这个组落地
// 后的包络。与生成期(zfGenPassive/zfGenMultiPin 累加出来的 zfGroupBBox)是两条
// 独立代码路径:策略 = zfStubPlanned 时两者必须逐字相等,不等就说明有人绕过
// zfTermGeom 手改了盒子(又造了一把尺)。
func zfLandedGroupBBox(g zfPlacedGroup, stub zfStubPolicy) layoutBBox {
	offs := stub(g)
	b, has := layoutBBox{}, false
	zfGrow(&b, &has, g.Body)
	for i, t := range g.Terms {
		off := t.Offset
		if i < len(offs) {
			off = offs[i]
		}
		wire, marker := zfTermGeom(t.PinX, t.PinY, off, t.Dir, t.Kind, t.Net, t.SpreadX)
		zfGrow(&b, &has, wire)
		zfGrow(&b, &has, marker)
	}
	return b
}

// zfLandedFrame 用给定桩线策略重算整个区的框尺寸(口径与 planZoneFollow 完全
// 一致:内容并集 + 落地余量 → partitionFrameSize)。
func zfLandedFrame(plan zfZonePlan, opts partitionOpts, stub zfStubPolicy) (w, h float64) {
	content, has := layoutBBox{}, false
	for _, g := range plan.Groups {
		zfGrow(&content, &has, zfLandedGroupBBox(g, stub))
	}
	if !has {
		return 0, 0
	}
	return partitionFrameSize(zfInflate(content, plan.Slack), opts.TitleBand, opts.NoteBand)
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
		// PinX/PinY 是复算的原料,必须跟着平移 —— 漏掉它,落地复判会拿区内局部
		// 坐标去和绝对坐标比,结论毫无意义(而且看起来很像"规划错了")。
		out.Terms[i].PinX += dx
		out.Terms[i].PinY += dy
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
		g        zfPlacedGroup
		bb       layoutBBox
		multiPin bool
	}
	gen := make([]genned, 0, len(gs))
	for _, g := range gs {
		pg, err := zfGenGroup(g)
		if err != nil {
			return zfZonePlan{}, err
		}
		gen = append(gen, genned{pg, zfGroupBBox(pg), g.MultiPin})
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
		// 相邻组任一是 MultiPin 时加 zfPinReach:MultiPin 的无端子裸引脚伸出
		// 本体 bbox 之外(SOT-23 实测 9~15),规划器没有 pin 几何,单列 gap 20
		// 曾让 Q1-E 与 Q2-C 端点在组间走廊物理同点(隐式短路,pin-coincidence
		// ERROR 真机两次复现)—— 保守按最大触达补余量。
		plan.Mode = "无主导锚件 → 全员单列(位号序)"
		y := 0.0
		for i, g := range gen {
			putAt(g, -g.bb.MinX, y-g.bb.MaxY)
			gap := float64(zfGroupGap)
			if g.multiPin || (i+1 < len(gen) && gen[i+1].multiPin) {
				gap += zfPinReach
			}
			y -= (g.bb.MaxY - g.bb.MinY) + gap
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
		// MultiPin 锚件的裸引脚伸出本体 bbox(见 zfPinReach)——卫星别贴进触达带。
		aGap := float64(zfAnchorGap)
		if anchor.multiPin {
			aGap += zfPinReach
		}
		if best == "below" {
			// 横排一排竖立的件,顶边对齐在锚件下缘 - gap。
			x := 0.0
			for _, s := range sats {
				putAt(s, x-s.bb.MinX, -aGap-s.bb.MaxY)
				x += (s.bb.MaxX - s.bb.MinX) + zfGroupGap
			}
		} else {
			dx := aw + aGap
			if best == "left" {
				dx = -aGap - colW
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
	// 落地余量:规划框要当**上界**用(见 zfLandSlack)。**决策必须可见** ——
	// 「框比内容大了一圈」如果只藏在常量里,下一个人量出来的框对不上就会去改
	// 别的地方。Mode 是人读输出与 JSON(zones[].mode)都带的字段。
	plan.Content = zfInflate(plan.Content, zfLandSlack)
	plan.Slack = zfLandSlack
	plan.Mode += fmt.Sprintf(" · 落地余量 %g(桩端点 5 网格吸附,规划框=落地框上界)", float64(zfLandSlack))
	// 收敛后的框走**外框的唯一函数**(partitionFrameSize):收紧时区名带 + 说明带
	// 就在账里,收紧完再画框 —— 而不是「按常量带收紧 → 画框 → 再放 note 装不下」。
	// opts.NoteBand 由调用方按本区已登记说明的渲染高度预置(schZoneNoteBandHeight)。
	plan.FrameW, plan.FrameH = partitionFrameSize(plan.Content, opts.TitleBand, opts.NoteBand)
	return plan, nil
}
