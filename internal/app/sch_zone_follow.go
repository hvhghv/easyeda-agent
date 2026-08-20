package app

// sch_zone_follow.go — phase A 区内收敛:跟随规则 R1–R5(用户裁定 2026-08-16)。
//
//	R1 跟随:卫星无源件朝向 = 锚件朝向(原理图锚件恒 upright → 卫星一律竖放、
//	   互相平行);多脚件保持符号朝向,端子保持实测侧。
//	R2 排列轴 = 贴边切向:卫星贴锚件下方 → 横排一排竖立的件(顶边对齐);
//	   贴左/右 → 竖列堆叠。贴哪侧、每行摆几件由**域感知选形**定(zfPickShape:
//	   先比可排布档位与「装得下的通道条数」,平局才回到 argmin max(宽,高)、
//	   平局序 左<右<下)。
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
	// 与其让这一格去撞 gutter,不如把它算进框里。它是框自己的属性(哪个区端子多,
	// 哪个区的余量就真的用得上),所以放在这里,不写进全局 gutter。
	//
	// **上界的适用范围**(2026-08-20 真机订正,别再写成无条件的「规划框=落地框上界」):
	// 这一格只封住**同一份 pin 坐标 + 同一份桩长**下的网格相位差 —— 它是
	// `zfLandedFrame(plan, opts, zfStubPlanned) ≤ 规划框` 这条**模型内**性质的余量
	// (TestZfLandedFrame_PlannedStubIsUpperBound 钉住)。它不承诺、也承诺不了真机:
	//   ① 规划把无源件的 pin 假定在本体 bbox 的上下缘中线上(zfGenPassive),真符号
	//      的 pin 未必在那里;转竖后的实测 bbox 也不是简单转置;
	//   ② markerBBoxProfile 是 2026-06 的实测标定,不是平台契约;
	//   ③ 计划没覆盖到的 pin 会走 autoconnect **自由方向**落点(内核 rest / 恢复段),
	//      那已经不是「同一份桩长」了 —— moveReport.FreeConnected 会点名它们。
	// 真机 MCU_IO 六区实测偏差 +141/+126/+82/+56/+26/+10,上界在真机上**不成立**;
	// 断言③(zaaRecheckFindings)存在的理由正是把不成立的那几次如实报出来。
	zfLandSlack = acSchGrid
)

// zfTerm 是一个端子的类型化描述(逐 pin 从活体折出,见 zfGroupFromCluster)。
type zfTerm struct {
	Kind string  // "netflag" | "netport"
	Net  string  //
	W, H float64 // 标签实测尺寸(只在无实测的老路径上填;规划几何一律走 zfTermGeom)
	Side string  // 挂侧:"left"|"right"|"up"|"down"(引脚相对器件本体的边界语义)
	// PinX/PinY 是这只引脚在**本体局部坐标**(相对 body 的 min 角)里的位置,
	// HasPin=false 表示调用方没喂引脚几何(纯几何单测 / 老调用点)。
	//
	// **为什么规划非要知道引脚在哪**:connect_pin 的桩只能从 pin 沿 direction 直出,
	// 所以「端子落在哪」= f(pin 坐标, direction, offset)。规划器过去假定两脚在本体
	// 上下缘中线上,真符号只要不是那样(C4/C6 的 rot 90 电容:+3V3 在下、GND 在上;
	// LED1 的两脚干脆是左右),预测的盒子就在错误的位置,而且派下去的 direction
	// 会让桩线钻进本体 —— 同件两支桩当场合并,GND 整张网并进 +3V3。
	PinX, PinY float64
	HasPin     bool
}

// zfGroup 是 phase A 的一个输入 L1 组。
type zfGroup struct {
	Designator   string
	BodyW, BodyH float64
	// MultiPin:引脚数 > 2。R1 对它不转向(符号管脚定义锁死),端子保持实测侧。
	MultiPin bool
	Terms    []zfTerm
	// Measured 是这个组的**现状实测几何**(页面绝对坐标)—— 「不得变差」门
	// (zfGateRegression)回退原形时的原料。nil = 调用方没喂实测(纯几何单测),
	// 此时门无从回退,收敛照原样采纳。
	Measured *zfMeasured
}

// zfMeasured 是一个组的现状实测几何。**它是观测,不是预测** —— Box 由
// buildSchClusters 从真实图元量出来,是「现状口径框」(zoneArrangeRawFrame)的内容。
type zfMeasured struct {
	// Body 是器件本体实测盒(--apply 按它算平移量,必须与 components.list 的
	// bbox 同源)。
	Body layoutBBox
	// Box 是 L1 体积:本体 ∪ 归属 marker ∪ 归属桩线 —— 现状口径框的内容并集。
	Box layoutBBox
	// Terms 是现状端子:逐 pin 从实测连接折出(Kind/Net/Dir/PinX/PinY/Offset)。
	// **原料只有这六个量**;BBox 一律由 zfTermGeom 导出,回退路径不许自造第二把尺。
	Terms []zfPlacedTerm
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
	// Retained:本区**没有采纳收敛**,原形保留(「不得变差」门拦下)。
	// **没有 omitempty 是有意的**:值为 false 就被抹掉的话,读的人无法区分
	// 「没回退」与「这版没有这个字段」(zone-plan 的 labelScopeDegraded 踩过)。
	Retained bool `json:"retained"`
	// RetainWhy 是回退理由的结构化版本(同一句话也挂在 Mode 尾巴上给人读)。
	RetainWhy string `json:"retainWhy,omitempty"`
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
// zfPlacedTerm.SpreadX。kind 是**规划口径**(netflag/netport);已经是 connect_pin
// canonical kind 的调用方走 zfTermGeomCanon(别再过一遍 zfCanonKind —— 它会把
// `net_port_bi` 当成 netflag 折成 power 盒,预测的就是另一个家族的几何)。
func zfTermGeom(pinX, pinY, offset float64, dir, kind, net string, spreadX float64) (wire, marker layoutBBox) {
	return zfTermGeomCanon(pinX, pinY, offset, dir, zfCanonKind(kind, net), net, spreadX)
}

// zfTermGeomCanon 是同一把尺的 canonical-kind 入口:落地侧(connect_pin 的
// kind / zaaTermExec.Kind / moveConnTerm.Kind / tidyRestoreKind)手里拿的都是
// canonical kind,再折一次就错了。
func zfTermGeomCanon(pinX, pinY, offset float64, dir, canonKind, net string, spreadX float64) (wire, marker layoutBBox) {
	ex, ey := endpointFor(pinX, pinY, offset, dir)
	wire = layoutBBox{
		MinX: minF(pinX, ex), MinY: minF(pinY, ey),
		MaxX: maxF(pinX, ex), MaxY: maxF(pinY, ey),
	}
	marker = predictedMarkerBBox(ex, ey, canonKind, dir, net)
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
//
// ── 两条支路(2026-08-20 真机定案)──────────────────────────────────────────────
//
//	转竖(rotated)  件会被执行侧转 ±90°,转完两脚必然落在本体上下缘中线上 ——
//	                实测引脚坐标此刻已经过期,按 R3 派上下端才是对的(执行侧
//	                zaaVerticalOrderOK 用 ExpectUpper 从两个候选里挑出兑现 R3
//	                的那一个,所以「电源上 / GND 下」在这条支路上是**可执行的**)。
//	不转(已竖立)  件原样不动,两脚**就在它们现在所在的地方**。此时 R3 的
//	                「GND 派到下端」不是选择而是幻觉:真机 C4/C6(rot 90)的
//	                +3V3 脚在本体下方、GND 脚在本体上方,派下去就是让 GND 的桩
//	                朝下、+3V3 的桩朝上 —— 两根桩线双双钻进本体并合并,GND 整张
//	                网并进 +3V3,页级对账当场红。
//
// 所以不转的件一律**按引脚自己的朝外方向**出桩(zfPointSideOf,与挂侧同一把尺)。
// 这不是放弃 R3:用户 2026-08-16 的裁定里,「电源上 / GND 下」本来就是**推论**
// (竖放 + 旗顺引脚朝外 + rail 归位),硬不变式是「同件两旗异向」——
// 后者由 zfCheckTermOverlap + zfCheckPassiveOpposed 两条一起守住。
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
	if !rotated && zfAllHavePins(g.Terms) {
		body := layoutBBox{MinX: 0, MinY: 0, MaxX: bw, MaxY: bh}
		for _, t := range g.Terms {
			dir := zfPointSideOf(body, t.PinX, t.PinY)
			// R4:netport 恒水平(竖放会折叠长条标)。引脚朝外已经是左右就照它,
			// 否则按阅读方向朝右 —— 与首版口径一致。
			if t.Kind == "netport" && dir != "left" && dir != "right" {
				dir = "right"
			}
			zfAppendTerm(&out, zfPlacedTerm{Kind: t.Kind, Net: t.Net, Dir: dir,
				PinX: t.PinX, PinY: t.PinY, Offset: zfStub})
		}
		if err := zfCheckPassiveOpposed(out); err != nil {
			return out, err
		}
		return out, zfCheckTermOverlap(out)
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
	if err := zfCheckPassiveOpposed(out); err != nil {
		return out, err
	}
	return out, zfCheckTermOverlap(out)
}

// zfAllHavePins:这一组的端子是不是全都带实测引脚坐标。**全有才用** ——
// 半份引脚几何比没有更危险(一支按真坐标、一支按假定,两把尺当场分家)。
func zfAllHavePins(ts []zfTerm) bool {
	if len(ts) == 0 {
		return false
	}
	for _, t := range ts {
		if !t.HasPin {
			return false
		}
	}
	return true
}

// zfCheckPassiveOpposed 是「**同件两旗**异向」硬不变式的可执行形式(用户裁定;
// 真机「同件双脚同向必自短路」的防线)。
//
// 口径严格按字面:**两支都是旗(netflag)**的两脚无源件。为什么不扩到 netport ——
// R4 让无源件的 port 一律朝右(阅读方向),同件两支 port 同向是**既有的正常形态**
// (真机 R5:LED1_N2 与 LED_CTRL 都朝右,靠两只脚 y 不同错开),把它们也判红
// 就是拿一条不存在的规则去毁掉正常版面。旗不一样:旗是电源/地,同向的两支旗
// 桩线共线、当场合并成一根,GND 与 +3V3 并成一张网。
//
// 判定与生成分离:生成侧按引脚朝外派方向,理应天然异向;真出现同向就是输入
// 几何异常(两脚从本体同一条边探出),此时 fail-closed 好过画一张会自短路的图。
func zfCheckPassiveOpposed(g zfPlacedGroup) error {
	if len(g.Terms) != 2 {
		return nil
	}
	a, b := g.Terms[0], g.Terms[1]
	if a.Kind != "netflag" || b.Kind != "netflag" {
		return nil
	}
	if a.Dir == b.Dir {
		return fmt.Errorf("%s: 两支旗同向(%s:%s / %s)—— 违反「同件两旗异向」硬不变式(自短路防线);两脚从本体同一条边探出,先核对符号引脚几何",
			g.Designator, a.Net, a.Dir, b.Net)
	}
	return nil
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
	if zfAllHavePins(g.Terms) {
		return zfGenMultiPinMeasured(g, bw, bh)
	}
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

// zfGenMultiPinMeasured 是**有实测引脚坐标**时的多脚件布置:桩线从引脚真实所在的
// 地方出,方向就是引脚的朝外方向(zfPointSideOf 已在 zfGroupFromCluster 里算好并
// 存进 Side)。与首版(zfGenMultiPin 的兜底支路)的三处区别都来自「知道 pin 在哪」:
//
//	① 落点真实   首版把左右侧端子按 zfPitch 从本体顶端往下**排**、上下侧端子一律
//	   画在本体中线,那是「pin 在哪不知道,先摆个样子」。真机 U2 的三只地脚在本体
//	   下段(y=320/350/360,本体高 421),首版预测的盒子却在本体顶端 —— 规划框与
//	   落地框谈不上任何对应关系,断言③ 只能红。
//	② 不确定带消失 SpreadX 是「pin 可能落在本体任意 x」的展宽(bw/2),知道 x 之后
//	   它就是纯虚胖,直接归零。
//	③ 梯次按需  首版对同侧每一支旗**无条件**递增桩长;真实 pin 间距够开(U2 的
//	   320 与 350 差 30 > 旗高)时那是白白把区框推宽。改成「只在预测盒真的与已放
//	   的同侧盒相撞时才让开一档」,而「算不算相撞」用的是 `sch check` 的
//	   marker-overlap 那把尺(zfMarkerCollides / schMarkerOverlapEps)——
//	   引脚节距 10 而标签高 11,同侧相邻两支标签**必然**竖向擦过 1 个单位,那是字体
//	   现实、换 lane 也消不掉,却要付 65 个单位的宽度。让位只对判据真会报的重叠出手。
//
// 确定性:端子按调用方给的全序(zfGroupFromCluster 逐 pin 折时已按 y 降序、x 升序、
// 网名定序)逐支放,不依赖 map 遍历序。
func zfGenMultiPinMeasured(g zfGroup, bw, bh float64) zfPlacedGroup {
	out := zfPlacedGroup{Designator: g.Designator,
		Body: layoutBBox{MinX: 0, MinY: 0, MaxX: bw, MaxY: bh}}
	// 谁参与让位、跟谁比 —— **首版的参与规则逐字保留**,变的只是「按需触发」:
	//
	//	左/右侧  只有旗(netflag)让位,而且只跟同侧**已放的旗**比。首版明写着
	//	         「port 恒水平、高 11 < 最小 pin pitch,保持短桩不参与梯次」——
	//	         把 port 也拉进来会当场毁掉版面:真机 U3(SOP-16,pin pitch 10)
	//	         左侧一支 GND 旗(含网名带 22 高)与下一只脚的 MCU_RX port 实压
	//	         6 个单位,让 port 出让就得躲开 75 宽的相邻标签,区框从 353 撑到
	//	         443,phase B 当场 blocked。那 6 个单位是 `sch check` 的账
	//	         (marker-overlap WARN / `sch destagger` 的辖区),不是布局器的。
	//	上/下侧  所有 kind 都参与、跟同侧已放的任何盒比(首版对上下侧就是无条件
	//	         递增所有 kind —— 那一侧的标签是竖起来的,谁压谁都是真压)。
	placedBySide := map[string][]layoutBBox{}
	for _, t := range g.Terms {
		dir := t.Side
		lateral := dir == "left" || dir == "right"
		yields := !lateral || t.Kind == "netflag"
		off := zfStub
		var marker layoutBBox
		// 让位循环:撞上同侧已放的盒就沿桩线方向让开「自己这一支的占地 + gap」。
		// 上限 32 档是防呆(每档至少让开一个盒宽,正常一两档就散开);到顶按当前
		// 档收下,不静默循环 —— 组间重叠断言(planZoneFollow 尾)仍会兜住画布。
		for i := 0; i < 32; i++ {
			_, marker = zfTermGeom(t.PinX, t.PinY, off, dir, t.Kind, t.Net, 0)
			if !yields || !zfOverlapsAny(marker, placedBySide[dir]) {
				break
			}
			if lateral {
				off += (marker.MaxX - marker.MinX) + zfFlagGap
			} else {
				off += (marker.MaxY - marker.MinY) + zfFlagGap
			}
		}
		zfAppendTerm(&out, zfPlacedTerm{Kind: t.Kind, Net: t.Net, Dir: dir,
			PinX: t.PinX, PinY: t.PinY, Offset: off})
		if !lateral || t.Kind == "netflag" {
			placedBySide[dir] = append(placedBySide[dir], marker)
		}
	}
	return out
}

// zfMarkerCollides 是「两支标签算不算撞上了」的判据 —— **与 `sch check` 的
// marker-overlap 逐字同源**(overlapExtent + schMarkerOverlapEps)。规划器让位
// 的门槛必须就是判据会报的那条线:低于它,让位换不来任何一条 finding 消失,
// 只换来一条 lane 的宽度;高于它,判据会报而规划器装作没看见。
func zfMarkerCollides(a, b layoutBBox) bool {
	ox, oy, overlap := overlapExtent(a, b)
	return overlap && minF(ox, oy) > schMarkerOverlapEps
}

func zfOverlapsAny(b layoutBBox, others []layoutBBox) bool {
	for _, o := range others {
		if zfMarkerCollides(b, o) {
			return true
		}
	}
	return false
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

// zfLandedFrame 用给定桩线策略重算整个区的框尺寸。**口径必须是落地复判那一侧**
// (zaaLandedRecheck):实测内容并集 → partitionFrameSize,**不加落地余量**。
//
// 首版在这里也 `zfInflate(content, plan.Slack)` —— 于是余量在两边同时出现、当场
// 抵消,「规划框是落地框的上界」这句话在模型里都成立不了(实测:三个 fixture
// 全部 +1~+3 越界)。而真机侧的 zaaLandedRecheck 是不加余量的:模型与它对不上,
// 就等于拿一把不存在的尺子去证明一条不存在的性质。余量只属于**规划框**,它存在
// 的全部意义就是让规划框比落地框大出那一格。
func zfLandedFrame(plan zfZonePlan, opts partitionOpts, stub zfStubPolicy) (w, h float64) {
	content, has := layoutBBox{}, false
	for _, g := range plan.Groups {
		zfGrow(&content, &has, zfLandedGroupBBox(g, stub))
	}
	if !has {
		return 0, 0
	}
	return partitionFrameSize(content, opts.TitleBand, opts.NoteBand)
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

// ── 形状候选 + 域感知选形(2026-08-20 用户取证:收敛目标此前是「域盲」的)────
//
// 缺陷:phase A 给一个区选形状时**根本不看空地长什么样**。两条支路各有各的病:
//
//	① 「无主导锚件」支路**连候选都没有** —— 「全员单列」是硬编码的(首版这里
//	   没有任何取舍,谈不上目标函数);
//	② 锚件支路有三个候选,但目标函数是 argmin max(w,h)(求方),同样域盲。
//
// 真机取证(ceshi / 页 MCU_IO / A4:可用域 1110×765,图签把它切成
// 「左通道 396 宽 × 765 高」+「上通道 1110 宽 × 555 高」):
//
//	wroom-passives  5 个 0402/0805 小无源件 → **单列** 152×696
//	                  696 > 555 → 只进得了左通道
//	wroom-core      单件 WROOM 模组         → 325×556.5
//	                  556.5 > 555 → **也**只进得了左通道
//	→ 两个区抢同一条 396 宽的通道:并排 325+152+12 > 396、上下叠 556+696+12 > 765
//	   → phase B blocked(报「wroom-core 被 wroom-passives 挡」)。
//
// **注意两个区各自的 fitRank 都是 2**(都有落点)—— 「不得变差」门的掉档判据
// 结构上看不见这种病:病在**落点自由度**,不在「有没有落点」。所以选形要用两把
// 钥匙,都从 zfDomain 的同一份 strips() 出来(fits 已重写成 stripFits>0 的投影,
// 与 phase B 的 zaFrame 同源 —— 不许另写一份通道算术):
//
//	① fitRank    三档可排布性(2 有落点 / 1 只被图签挡 / 0 连可用域都装不下)
//	② stripFits  **本页有几条通道装得下它**(落点自由度的离散度量)
//
// 选择律(三条性质缺一不可,机械可验):
//
//	① 候选里存在装得进通道的形状时绝不选装不进的 —— (fitRank, stripFits)
//	   字典序取最大;
//	② 不许退化成「永远选最扁」—— 候选先按**原有紧凑性偏好**定序(首版会选中的
//	   那个形态排最前,其余按 argmin max(w,h)),两把钥匙**平局时取序最靠前者**。
//	   都装得下时钥匙全平,选出来的就是原有偏好那一个,版面一个单位都不动;
//	③ 域未知(zfDomain 零值/退化)→ 整套退回原有偏好,老调用点与纯几何单测的
//	   输出逐字不变。

// zfGenned 是一个已生成局部几何的组(带包络与 MultiPin 标记)。
type zfGenned struct {
	g        zfPlacedGroup
	bb       layoutBBox
	multiPin bool
}

func zfGennedArea(x zfGenned) float64 {
	return (x.bb.MaxX - x.bb.MinX) * (x.bb.MaxY - x.bb.MinY)
}

// zfShapeCand 是一个**形状候选**:同一批组的一种摆法 + 它的框尺寸。
type zfShapeCand struct {
	mode    string
	groups  []zfPlacedGroup
	content layoutBBox
	w, h    float64
	// orig:首版(域盲)会选中的那个形态。平局时它赢 —— 「加了候选」本身不许
	// 改变任何已经排得下的版面。
	orig bool
	// pref 是**原有紧凑性偏好**的代价:orig 候选用首版那把尺(锚件支路的
	// sideCost,逐字保留),其余候选用 max(框宽,框高)。两种量纲从不互相比较
	// (orig 永远排在前面),所以不构成第三把尺。
	pref float64
}

// zfShelfPack 把一串组摆成「每行 perRow 件」的货架块:行内左对齐、顶边对齐,
// 行与行自上而下;块的左上角在原点(y-UP,所以往下是负 y)。
// perRow=1 就是「单列」(首版形态),perRow=len(items) 就是「单排」。
// 返回逐件的平移量(把组包络的左上角钉到货架格上)与块的净宽高。
//
// reach:相邻两件任一是 MultiPin 时,间距补 zfPinReach —— MultiPin 的无端子裸
// 引脚伸出本体 bbox(SOT-23 实测 9~15),规划器没有 pin 几何,单列 gap 20 曾让
// Q1-E 与 Q2-C 端点在组间走廊物理同点(隐式短路,真机两次复现)。**只有「无主导
// 锚件」支路开它**:锚件支路的卫星间距首版就是裸 zfGroupGap,候选化不许顺手改
// 已有形态的几何(那是另一笔账)。
func zfShelfPack(items []zfGenned, perRow int, reach bool) (offs [][2]float64, w, h float64) {
	if perRow < 1 {
		perRow = 1
	}
	gap := func(a, b bool) float64 {
		if reach && (a || b) {
			return zfGroupGap + zfPinReach
		}
		return zfGroupGap
	}
	offs = make([][2]float64, len(items))
	y, maxW, prevRowMulti := 0.0, 0.0, false
	for start := 0; start < len(items); start += perRow {
		end := start + perRow
		if end > len(items) {
			end = len(items)
		}
		row := items[start:end]
		rowMulti := false
		for _, it := range row {
			rowMulti = rowMulti || it.multiPin
		}
		if start > 0 {
			y -= gap(prevRowMulti, rowMulti)
		}
		x, rowH := 0.0, 0.0
		for i, it := range row {
			if i > 0 {
				x += gap(row[i-1].multiPin, it.multiPin)
			}
			offs[start+i] = [2]float64{x - it.bb.MinX, y - it.bb.MaxY}
			x += it.bb.MaxX - it.bb.MinX
			rowH = maxF(rowH, it.bb.MaxY-it.bb.MinY)
		}
		maxW = maxF(maxW, x)
		y -= rowH
		prevRowMulti = rowMulti
	}
	return offs, maxW, -y
}

// zfShelfMode / zfSideCN 是候选的人读名(进 Mode,也进 JSON 的 zones[].mode)。
func zfShelfMode(perRow, n int) string {
	switch {
	case perRow <= 1:
		return "无主导锚件 → 全员单列(位号序)"
	case perRow >= n:
		return "无主导锚件 → 全员单排(位号序)"
	}
	return fmt.Sprintf("无主导锚件 → 货架每行 %d 件(位号序)", perRow)
}

func zfSideCN(side string) string {
	switch side {
	case "left":
		return "左"
	case "right":
		return "右"
	}
	return "下"
}

// zfShelfCands:无主导锚件 → 货架族(k = 1..n)。k=1 是首版的「全员单列」。
func zfShelfCands(gen []zfGenned) []zfShapeCand {
	out := make([]zfShapeCand, 0, len(gen))
	for k := 1; k <= len(gen); k++ {
		offs, _, _ := zfShelfPack(gen, k, true)
		gs := make([]zfPlacedGroup, len(gen))
		for i := range gen {
			gs[i] = zfTranslate(gen[i].g, offs[i][0], offs[i][1])
		}
		out = append(out, zfShapeCand{mode: zfShelfMode(k, len(gen)), groups: gs, orig: k == 1})
	}
	return out
}

// zfAnchorCands:锚件 + 卫星族 —— 三条贴边 × 货架每行 k 件。
// 首版的三个候选(左/右各一列、下面一排)仍在其中,orig 标着,pref 用首版那把
// sideCost 尺 —— 平局时选出来的还是首版那一个。
func zfAnchorCands(gen []zfGenned, anchor zfGenned) []zfShapeCand {
	sats := make([]zfGenned, 0, len(gen)-1)
	for _, g := range gen {
		if g.g.Designator != anchor.g.Designator {
			sats = append(sats, g)
		}
	}
	if len(sats) == 0 {
		return []zfShapeCand{{mode: "单组:重生短桩,不再动", orig: true,
			groups: []zfPlacedGroup{zfTranslate(anchor.g, -anchor.bb.MinX, -anchor.bb.MinY)}}}
	}
	aw := anchor.bb.MaxX - anchor.bb.MinX
	ah := anchor.bb.MaxY - anchor.bb.MinY
	// MultiPin 锚件的裸引脚伸出本体 bbox(见 zfPinReach)——卫星别贴进触达带。
	aGap := float64(zfAnchorGap)
	if anchor.multiPin {
		aGap += zfPinReach
	}
	base := zfTranslate(anchor.g, -anchor.bb.MinX, -anchor.bb.MinY)
	label := map[string]string{"left": "列(左)", "right": "列(右)", "below": "排(下,竖放平行)"}
	// 首版代价(R2 的 argmin max(w,h),平局序 左<右<下)——**逐字保留**:它现在
	// 是平局时的紧凑性偏好,不再是唯一判据。
	_, colW, colH := zfShelfPack(sats, 1, false)
	_, rowW, rowH := zfShelfPack(sats, len(sats), false)
	legacy := map[string]float64{
		"left":  maxF(aw+zfAnchorGap+colW, maxF(ah, colH)),
		"right": maxF(aw+zfAnchorGap+colW, maxF(ah, colH)),
		"below": maxF(maxF(aw, rowW), ah+zfAnchorGap+rowH),
	}
	origK := map[string]int{"left": 1, "right": 1, "below": len(sats)}
	out := make([]zfShapeCand, 0, 3*len(sats))
	for _, side := range []string{"left", "right", "below"} {
		for k := 1; k <= len(sats); k++ {
			offs, bw, _ := zfShelfPack(sats, k, false)
			var bx, by float64
			switch side {
			case "left":
				bx, by = -aGap-bw, ah
			case "right":
				bx, by = aw+aGap, ah
			default: // below:横排一排竖立的件,顶边对齐在锚件下缘 - gap
				bx, by = 0, -aGap
			}
			gs := make([]zfPlacedGroup, 0, len(gen))
			gs = append(gs, base)
			for i, s := range sats {
				gs = append(gs, zfTranslate(s.g, bx+offs[i][0], by+offs[i][1]))
			}
			c := zfShapeCand{groups: gs, orig: k == origK[side]}
			if c.orig {
				c.mode = fmt.Sprintf("锚件 %s + 卫星%s", anchor.g.Designator, label[side])
				c.pref = legacy[side]
			} else {
				c.mode = fmt.Sprintf("锚件 %s + 卫星货架(%s,每行 %d 件)",
					anchor.g.Designator, zfSideCN(side), k)
			}
			out = append(out, c)
		}
	}
	return out
}

// zfShapeCands 生成本区的全部形状候选(至少一个)。分支判据(单组 / 有没有
// 主导锚件)与首版逐字相同 —— 变的只是「每条分支给出几个候选」。
func zfShapeCands(gen []zfGenned) []zfShapeCand {
	switch {
	case len(gen) == 0:
		return []zfShapeCand{{mode: "空区(无成员)", orig: true}}
	case len(gen) == 1:
		return []zfShapeCand{{mode: "单组:重生短桩,不再动", orig: true,
			groups: []zfPlacedGroup{zfTranslate(gen[0].g, -gen[0].bb.MinX, -gen[0].bb.MinY)}}}
	}
	byArea := append([]zfGenned(nil), gen...)
	sort.SliceStable(byArea, func(i, j int) bool {
		if a, b := zfGennedArea(byArea[i]), zfGennedArea(byArea[j]); a != b {
			return a > b
		}
		return tidyDesignatorLess(byArea[i].g.Designator, byArea[j].g.Designator)
	})
	if zfGennedArea(byArea[0]) < 2*zfGennedArea(byArea[1]) {
		return zfShelfCands(gen)
	}
	return zfAnchorCands(gen, byArea[0])
}

// zfFinishCand 给候选算框:内容并集 → 落地余量 → **外框的唯一函数**
// (partitionFrameSize,区名带 + 说明带在账里)。判定与生成分离:选形比的就是
// 这个框,不是候选自己报的尺寸。
func zfFinishCand(c *zfShapeCand, opts partitionOpts) {
	content, has := layoutBBox{}, false
	for _, g := range c.groups {
		zfGrow(&content, &has, zfGroupBBox(g))
	}
	if has {
		content = zfInflate(content, zfLandSlack)
	}
	c.content = content
	c.w, c.h = partitionFrameSize(content, opts.TitleBand, opts.NoteBand)
}

// zfCandOrder 是**原有紧凑性偏好序**:orig 在前,其次 pref 小者在前,最后按
// 生成序(确定性,不依赖 map 遍历)。域感知只在这个序上做「严格更优才换」。
func zfCandOrder(cands []zfShapeCand) []int {
	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := cands[order[a]], cands[order[b]]
		if x.orig != y.orig {
			return x.orig
		}
		if x.pref != y.pref {
			return x.pref < y.pref
		}
		return order[a] < order[b]
	})
	return order
}

// zfPickShape 选形:返回选中候选的下标 + 挂到 Mode 尾巴上的域感知说明。
// **决策必须可见** —— 「为什么是这个形状」不许只活在代码里。
func zfPickShape(cands []zfShapeCand, dom zfDomain) (int, string) {
	order := zfCandOrder(cands)
	first := order[0]
	if !dom.known() {
		return first, " · 域未知 → 按紧凑序 argmin max(w,h) 选形"
	}
	best := first
	bestR, bestS := dom.shapeKeys(cands[best].w, cands[best].h)
	for _, i := range order[1:] {
		r, s := dom.shapeKeys(cands[i].w, cands[i].h)
		if r > bestR || (r == bestR && s > bestS) {
			best, bestR, bestS = i, r, s
		}
	}
	tail := ""
	if bestS == 0 {
		// 「确实无解」也必须是**看过域之后的结论**,而且要读得出下一步。
		tail = fmt.Sprintf(" —— %d 个候选没有一个装得进任何通道(%s),phase B 必然 blocked:拆区或 `sch page-new` 拆页",
			len(cands), zfRankWhy(bestR))
	}
	if best == first {
		return best, fmt.Sprintf(" · 域感知选形(%d 候选):原有偏好即最优 — %d 档 / %d 通道%s",
			len(cands), bestR, bestS, tail)
	}
	fr, fs := dom.shapeKeys(cands[first].w, cands[first].h)
	return best, fmt.Sprintf(" · 域感知选形(%d 候选):改选本形态 — 档 %d→%d、通道 %d→%d(原偏好「%s」%.0f×%.0f)%s",
		len(cands), fr, bestR, fs, bestS, cands[first].mode, cands[first].w, cands[first].h, tail)
}

// planZoneFollow 是 phase A 主入口:一个区的全部 L1 组 → 收敛布置。
// 纯函数;输入顺序无关(内部按位号自然序全序排序);dom 是本页可行域
// (zfDomainFor,与 phase B 同源)—— **零值 = 域未知**,此时退回原有偏好。
func planZoneFollow(zone string, groups []zfGroup, opts partitionOpts, dom zfDomain) (zfZonePlan, error) {
	gs := append([]zfGroup(nil), groups...)
	sort.SliceStable(gs, func(i, j int) bool { return tidyDesignatorLess(gs[i].Designator, gs[j].Designator) })

	gen := make([]zfGenned, 0, len(gs))
	for _, g := range gs {
		pg, err := zfGenGroup(g)
		if err != nil {
			return zfZonePlan{}, err
		}
		gen = append(gen, zfGenned{pg, zfGroupBBox(pg), g.MultiPin})
	}
	cands := zfShapeCands(gen)
	for i := range cands {
		zfFinishCand(&cands[i], opts)
		if !cands[i].orig {
			cands[i].pref = maxF(cands[i].w, cands[i].h)
		}
	}
	pick, why := zfPickShape(cands, dom)
	c := cands[pick]

	plan := zfZonePlan{Zone: zone, Mode: c.mode, Groups: c.groups, Content: c.content}
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
	// 落地余量:规划框要当**上界**用(见 zfLandSlack)。**决策必须可见** ——
	// 「框比内容大了一圈」如果只藏在常量里,下一个人量出来的框对不上就会去改
	// 别的地方。Mode 是人读输出与 JSON(zones[].mode)都带的字段。
	plan.Slack = zfLandSlack
	plan.Mode += fmt.Sprintf(" · 落地余量 %g(桩端点 5 网格吸附;上界只在同一份 pin 坐标+桩长的模型内成立,见 zfLandSlack)", float64(zfLandSlack))
	plan.Mode += why
	// 收敛后的框走**外框的唯一函数**(partitionFrameSize):收紧时区名带 + 说明带
	// 就在账里,收紧完再画框 —— 而不是「按常量带收紧 → 画框 → 再放 note 装不下」。
	// opts.NoteBand 由调用方按本区已登记说明的渲染高度预置(schZoneNoteBandHeight)。
	plan.FrameW, plan.FrameH = c.w, c.h
	return plan, nil
}

// ── 「不得变差」门:收敛只有在确实变好时才采纳(2026-08-20 真机取证)──────────
//
// 真机 ceshi / 页 MCU_IO:区 esp32s3_wroom1_module 只有一件 U2
// (ESP32-S3-WROOM-1,41 脚,本体 71×421),实测 L1 体积 385×421 —— **marker 是
// 横向铺开的**,各自贴在自己那一行引脚旁边,所以宽度大而高度不超过本体。
//
//	收敛前 433×541   收敛后 244×767   可用高只有 765
//
// 宽度收了 189,高度涨了 226 —— **差 2 个单位排不下**,phase B 当场
// `N(405)纸面放不下→S(420)→W(533)→E(637)` 判 blocked;而收敛前的 433×541 是
// 排得下的(图签上方可用 555 ≥ 541)。「不收敛能排,收敛了排不下」——
// phase A 违背了它自己存在的理由(它不是优化,是 phase B 的前置条件)。
//
// 直接机理(复核确认):zfGroupFromCluster 按 marker 中心相对**本体中心**的主轴
// 判挂侧(|dx| ≥ |dy| 才算左右)。本体 421 高、marker 横向触达约 ±100 —— 于是靠近
// 上下两端那几行的 marker,|dy| 反而大于 |dx|,被判成 up/down,进了 zfGenMultiPin
// 的**垂直梯次**:一支朝下的 netport 光本体+网名带就 57 高,梯次步长 57+6,四支
// 摞下来就是两百多个单位的纵向增生。R1–R5 是给「小锚件 + 一圈分立卫星」设计的,
// 用在「本体已经很高、marker 天生横向」的大符号上就把高度顶爆。
//
// 门的口径必须是**可排布性**,不是面积/周长:这一例宽度实实在在收窄了 189,
// 面积也小了,但高度越过域界 → 不可排。判据是
//
//	收敛后的可排布档位 < 原形的档位  →  回退原形
//
// ── 订正(2026-08-20 第二轮真机):档位必须是阶梯,不能是一个布尔 ──────────────
//
// 首版判据写成「原形 fits ∧ 收敛后 !fits」,漏掉了真机上真正发生的那一种变差。
// 同一页同一区,第二轮实测 449×737 → 244×863:
//
//	原形 449×737   高 737 ≤ 可用高 765,但 449 > 图签左侧通道 396 → **不 fits**
//	收敛 244×863   高 863 > 可用高 765                          → 也不 fits
//
// 首版门走到第二条分支(`原形本就排不下 → 收敛是唯一出路,不许拦`)当场放行,
// 输出 retained=false —— 而这两个「都排不下」根本不是一回事:原形只是被图签挡住
// (页面重排、拆页、挪图签都还有救),收敛后的框却连**可用域本身**都装不下,
// 结构上没救。phase A 把一个 1 档的框做成了 0 档,门却看不见。
//
// 所以 fits 升格成 zfDomain.fitRank 的三档阶梯(2 有落点 / 1 只被图签挡 / 0 连
// 可用域都装不下),门比档位。「任一维度变大」仍不必单独写进条件:三档各自对
// (w,h) 单调 —— 两维都不变大就不可能掉档。回退**逐区独立**
// (planZoneFollowGated 每区各判各的),且必须在输出里可见(Mode 尾巴 +
// Retained/RetainWhy 字段),绝不静默。

// zfDomain 是 phase B 的可行域,与 newZaSearch 同一把尺:页边距之内的可用矩形
// (锚按 5 格律取整)+ 图签安全带(inflatedTitleKeepout)+ gutter。
// 配对由 ruler 一致性测试钉住 —— 两处各算各的域界就是又造了一把尺。
type zfDomain struct {
	L, R, B, T float64
	// Keep 是**已按 titleBlockSafety 膨胀**的图签安全带(nil = 本页不设防)。
	Keep *layoutBBox
	G    float64 // gutter(区框与障碍之间的间距)
}

// zfDomainFor 由页面几何构造可行域(与 newZaSearch 的 zaFrame 逐字段同源)。
func zfDomainFor(sheet layoutBBox, keepout *layoutBBox, opts partitionOpts) zfDomain {
	return zfDomain{
		L: snap5Up(sheet.MinX + opts.Margin), R: snap5Dn(sheet.MaxX - opts.Margin),
		B: snap5Up(sheet.MinY + opts.Margin), T: snap5Dn(sheet.MaxY - opts.Margin),
		Keep: inflatedTitleKeepout(keepout), G: opts.Gutter,
	}
}

// strips 是图签安全带把可用域切出来的四条整条通道的净尺寸(左/右宽、下/上高)。
// 负值 = 那条通道不存在。**归因用**:blocked 的人要知道「上方只剩多少高」。
func (d zfDomain) strips() (left, right, below, above float64) {
	if d.Keep == nil {
		return d.R - d.L, d.R - d.L, d.T - d.B, d.T - d.B
	}
	return (d.Keep.MinX - d.G) - d.L, d.R - (d.Keep.MaxX + d.G),
		(d.Keep.MinY - d.G) - d.B, d.T - (d.Keep.MaxY + d.G)
}

// stripFits 数「本页**有几条通道**装得下这个 w×h 的框」—— 落点自由度的离散度量。
//
// 它与 fits 是**同一把尺的两个投影**(fits = stripFits > 0,下面就是这么写的):
// 通道算术只有这一份,与 phase B 的 zaFrame 同源。两个判据一旦各算各的,就会
// 再出一次「判定与生成两把尺」的老账。
//
// 为什么选形需要「几条」而不只是「有没有」:两个区可以各自 fitRank=2(都有
// 落点)却都只进得了**同一条**通道,于是必然互相挤掉一个 —— 真机 MCU_IO 的
// wroom-core(325×556.5)与 wroom-passives(152×696)就是这一对。掉档判据在
// 那一局面上结构性失明。
//
// 对 (w,h) 单调:w、h 变小绝不会让某条通道从装得下变成装不下。
func (d zfDomain) stripFits(w, h float64) int {
	const eps = 1e-9
	if !d.fitsBare(w, h) {
		return 0
	}
	if d.Keep == nil {
		return 1 // 本页不设防 → 整个可用域就是一条通道
	}
	left, right, below, above := d.strips()
	n := 0
	for _, s := range [4]struct{ avail, need float64 }{
		{left, w}, {right, w}, {below, h}, {above, h},
	} {
		if s.avail > 0 && s.need <= s.avail+eps {
			n++
		}
	}
	return n
}

// fits 判「一个 w×h 的区框在本页上**还存不存在任何落点**」——
// 即 phase B 归因里那句「纸面放不下」(zaEdgeProbe.Cands == 0)的纯几何形式。
//
// 单个矩形障碍下这是**精确判据**(不是启发式):框要么整个落在图签左侧那条
// 通道、要么右侧、要么下方、要么上方,四条都塞不下就真的没有落点。
// 对 (w,h) 单调:w,h 变小绝不会从可排变成不可排 —— 门靠这条性质省掉
// 「任一维度变大」的显式判断。
func (d zfDomain) fits(w, h float64) bool { return d.stripFits(w, h) > 0 }

// known 判「调用方到底给没给域」。零值 zfDomain(纯几何单测、老调用点)必须
// 退化成「不做域感知」,而不是被当成一张零面积的纸把所有候选都判成 0 档。
func (d zfDomain) known() bool { return d.R-d.L > 0 && d.T-d.B > 0 }

// shapeKeys 是选形的两把钥匙(字典序比较):可排布档位 + 装得下的通道条数。
func (d zfDomain) shapeKeys(w, h float64) (rank, strips int) {
	return d.fitRank(w, h), d.stripFits(w, h)
}

// fitsBare 是**忽略图签**的可行域判据:框放得进页边距之内的那个矩形。
// 它比 fits 弱一档,但弱得有意义 —— 越过它就意味着「本页无论怎么挪、无论
// 图签在不在,都装不下」,而只是 fits 不成立还只是「被图签挡住了」。
func (d zfDomain) fitsBare(w, h float64) bool {
	const eps = 1e-9
	return w <= (d.R-d.L)+eps && h <= (d.T-d.B)+eps
}

// zfFitRank 是可排布性的**三档阶梯**(2 最好):
//
//	2  fits      本页存在落点(图签也让开了)
//	1  fitsBare  装得进可用域,但被图签挡住 —— 页面重排/拆页还有救
//	0  连可用域都装不下 —— 结构上没救
//
// 门比的是这个档位,不是 fits 这一个布尔。首版只有 `fits` 一档,于是
// 「原形 1 档 → 收敛 0 档」这种**实打实的变差**从判据里漏了出去:
// 真机 MCU_IO 的 esp32s3_wroom1_module,原形 449×737(高 737 ≤ 765,只是
// 449 > 图签左侧通道 396 → 1 档)、收敛后 244×863(高 863 > 可用高 765 → 0 档)。
// 首版门在第二条分支上直接放行(`原形本就排不下 → 收敛是唯一出路`),retained=false,
// 结果 phase B 拿着一个**更没救**的框去撞墙。阶梯化之后这一例回退。
//
// 三档都对 (w,h) 单调(fits / fitsBare 各自单调),所以「两维都没变大 → 不可能
// 掉档」这条短路仍然成立。
func (d zfDomain) fitRank(w, h float64) int {
	switch {
	case d.fits(w, h):
		return 2
	case d.fitsBare(w, h):
		return 1
	}
	return 0
}

// zfRankWhy 把档位折成一句人话(归因用)。
func zfRankWhy(r int) string {
	switch r {
	case 2:
		return "本页有落点"
	case 1:
		return "装得进可用域但被图签挡住"
	}
	return "连可用域都装不下"
}

// zfGateRegression 是「不得变差」门的判据本体(纯函数,便于单测与负对照)。
// 返回回退理由(空 = 采纳收敛)。
func zfGateRegression(rawW, rawH, convW, convH float64, d zfDomain) string {
	const eps = 1e-9
	grewW, grewH := convW > rawW+eps, convH > rawH+eps
	if !grewW && !grewH {
		return "" // 两维都没变大 → 三档各自单调,不可能掉档
	}
	rawRank, convRank := d.fitRank(rawW, rawH), d.fitRank(convW, convH)
	if convRank >= rawRank {
		return "" // 没掉档 → 采纳收敛(负对照:宽涨一点但高大降,必须仍收敛)
	}
	var grew []string
	if grewW {
		grew = append(grew, fmt.Sprintf("宽 %.0f→%.0f", rawW, convW))
	}
	if grewH {
		grew = append(grew, fmt.Sprintf("高 %.0f→%.0f", rawH, convH))
	}
	left, _, _, above := d.strips()
	return fmt.Sprintf("收敛回退:%s 后从「%s」掉到「%s」(可用 %.0f×%.0f;图签上方高 %.0f、左侧宽 %.0f)—— 保留原形 %.0f×%.0f",
		joinCN(grew), zfRankWhy(rawRank), zfRankWhy(convRank),
		d.R-d.L, d.T-d.B, above, left, rawW, rawH)
}

// joinCN 用顿号连接归因短语(确定性:输入已定序)。
func joinCN(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "、"
		}
		out += p
	}
	return out
}

// zfRetainPlan 是**原形计划**:不重排、不重生桩,把现状实测几何原样折成一份
// zfZonePlan(区内局部坐标)。回退时输出它 —— 「保留原形」必须是一份下游能用的
// 完整计划,不是「跳过这个区」:--apply 的断言①(计划端子网名多重集 = 已连接
// pin 网名多重集)要求每件都有端子,漏了就整页拒绝执行。
//
// 三条与收敛路径不同的地方,都是**有意**的:
//   - 端子几何仍走 zfTermGeom(一把尺),但桩长用**实测**桩长(Offset),不是 zfStub
//     —— 原形保留的意思就是「一个单位都不动」;
//   - Content 取「实测 L1 体积 ∪ 预测落地包络」:体积是观测(现状口径框的内容,
//     与 zoneArrangeRawFrame 同源),并集只是防止重建后的标签探出观测框;
//   - **不加 zfLandSlack**:那一格余量是给「规划坐标 → 落地坐标换网格相位」留的,
//     而原形保留是刚体平移(Δ 已 snap5),落地几何与观测逐字相同,不需要余量。
//     加了它,原形框反而比现状口径框大一圈,「不得变差」自己就变差了。
//
// 组间重叠断言在这里**不做**:实测 marker 是四面星形展开的,两组的包络矩形
// 天然互相穿插(cmd_sch_clusters.go 的开篇就是这条),拿包络判会误炸。
func zfRetainPlan(zone string, groups []zfGroup, opts partitionOpts) (zfZonePlan, bool) {
	gs := append([]zfGroup(nil), groups...)
	sort.SliceStable(gs, func(i, j int) bool { return tidyDesignatorLess(gs[i].Designator, gs[j].Designator) })

	plan := zfZonePlan{Zone: zone, Mode: "原形保留(不重排、不重生桩)"}
	content, has := layoutBBox{}, false
	for _, g := range gs {
		if g.Measured == nil {
			return zfZonePlan{}, false // 没有实测几何 → 无从回退
		}
		out := zfPlacedGroup{Designator: g.Designator, Body: g.Measured.Body}
		for _, t := range g.Measured.Terms {
			zfAppendTerm(&out, t)
		}
		plan.Groups = append(plan.Groups, out)
		zfGrow(&content, &has, g.Measured.Box)
		zfGrow(&content, &has, zfGroupBBox(out))
	}
	if !has {
		return zfZonePlan{}, false
	}
	// 平移到区内局部坐标(内容 min 角归零)——与收敛路径的坐标约定一致,
	// --apply 的 abs = rect.Min + (pad, noteBand+pad) + (local − Content.Min) 才成立。
	dx, dy := -content.MinX, -content.MinY
	for i, g := range plan.Groups {
		plan.Groups[i] = zfTranslate(g, dx, dy)
	}
	plan.Content = layoutBBox{MinX: 0, MinY: 0,
		MaxX: content.MaxX - content.MinX, MaxY: content.MaxY - content.MinY}
	plan.FrameW, plan.FrameH = partitionFrameSize(plan.Content, opts.TitleBand, opts.NoteBand)
	return plan, true
}

// planZoneFollowGated = planZoneFollow + 「不得变差」门。**phase A 的生产入口**。
// 纯函数;逐区独立判定(一个区回退不影响其它区照常收敛);判定只用算术,
// 不依赖 map 遍历序。
func planZoneFollowGated(zone string, groups []zfGroup, opts partitionOpts, dom zfDomain) (zfZonePlan, error) {
	conv, err := planZoneFollow(zone, groups, opts, dom)
	if err != nil {
		return conv, err // 收敛本身出错仍 fail-closed(R5 违例是缺陷,不是尺寸问题)
	}
	keep, ok := zfRetainPlan(zone, groups, opts)
	if !ok {
		return conv, nil
	}
	why := zfGateRegression(keep.FrameW, keep.FrameH, conv.FrameW, conv.FrameH, dom)
	if why == "" {
		return conv, nil
	}
	keep.Retained, keep.RetainWhy = true, why
	keep.Mode += " · " + why
	return keep, nil
}
