package app

// pcb_check_connector.go —— #168 的两条连接器规则，纯函数实现（`pcb check` 与
// layout-score 的 edge-io 维共用同一份判读，不各算各的）：
//
//	internal-on-edge         内部件占用板外沿 —— 外沿是稀缺资源，该留给真出线的口
//	connector-plug-clearance 相邻对外口的**插头护套**打架 —— footprint 不重叠不代表插得进去
//
// 为什么这两条现有工具一条都抓不到：
//   - layout-lint 的 overlap / pcb check 的 clearance 只看**铜箔与渲染 bbox**。插头护套
//     是板子上根本不存在的三维实体，几何里没有它，再精确的 bbox 判定也永远看不见。
//   - 「这个连接器是对内还是对外」是**设计意图**，不是几何属性。同一个 PH2.0-3P 座，
//     接箱内电芯就是 internal，接箱外传感器就是 user-facing —— 只能由 S0 spec 声明，
//     几何再准也推不出来。所以这里刻意分了两级置信度：spec 显式标注报 WARN，
//     启发式推定只报 INFO（见 boardConnector.facingSrc）。
//
// 连接器识别刻意**复用**既有判据（cpReEdgeConn / cpReAnyEdge / edgeRoleOf 的
// `J* 且非 JP*` 口径），不另起第三套 —— 这个仓库吃过「两套引擎长期给矛盾答案」的亏。

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

const (
	// pcbConnEdgeBandMil 是「板外沿带」的宽度：器件到板框边界小于它就算占着外沿。
	// 300mil ≈ 7.6mm 是 #168 给的建议值，**待校准初值**。
	//
	// 一个常数同时被两处用：internal-on-edge 的触发线、edge-io 维判「对外口到底贴没
	// 贴边」的线。刻意共用 —— 这两件事问的是同一个物理问题（你在不在外沿带里），
	// 拆成两个魔数只会让校准时忘掉其中一个。
	pcbConnEdgeBandMil = 300.0

	// pcbPlugFallbackMarginMil 是查不到插头包络表时的兜底余量：渲染 bbox 宽 + 2mm。
	// 2mm 来自「护套通常比母座每侧宽 1mm 左右」的粗经验，**待校准初值**；用到它的
	// finding 一律带 `plug-width=fallback` 标记，让人知道这条是估出来的。
	pcbPlugFallbackMarginMil = 2.0 * mmToMil

	// pcbMatingCorridorDepthMil 是插拔通道（mating corridor）的纵深：卧贴插口的
	// 开口面前方这么深的一条走廊里不能有器件，否则插头进不来。
	//
	// v1 用固定 250mil ≈ 6.35mm，**待校准初值**——它是「插头本体插入段 + 手指/
	// 护套余量」的下限估计，不是从包络表推的：包络表（_plug_envelope.json）记的是
	// 插头**宽**（沿板边方向），不是插入**深**，拿宽换算深是张冠李戴。等包络表
	// 补上 depth 字段后再按件取值。
	pcbMatingCorridorDepthMil = 250.0
)

// ---------------------------------------------------------------------------
// 连接器判读 —— 一次解析，两条规则 + edge-io 维共用
// ---------------------------------------------------------------------------

// boardConnector 是一个已放置连接器的完整判读结果。
type boardConnector struct {
	comp boardComp

	// facing 是「朝向谁」：user-facing | internal | any | ""（判不出来）。
	// facingSrc 是这个结论的**来源**，决定下游报 WARN 还是 INFO：
	//   "spec"      —— S0 spec 的 interfaces[].facing/internal 显式声明（板级决定）
	//   "heuristic" —— 器件类别 + 网名推定（类别经验，可能错）
	//   ""          —— 没结论
	facing    string
	facingSrc string

	// role 是 edgeRoleOf 的口径（"user-facing" | "any" | ""）。"any" 指 RF 天线座 /
	// 无线模组这类**必须在某条边但哪条都行**的件，聚边判定要把它们排除，否则会因为
	// 天线单独占一条边就扣掉一堆分。
	role string

	edge       apEdge  // 最近的板边（hasEdge 为 false 时无意义）
	hasEdge    bool    // 有板框才有边
	edgeGapMil float64 // 封装 bbox 到板框边界的最短距离，板外为负；无板框时 +Inf

	pins int // 去重后的焊盘编号数（排式连接器的包络宽随它变）

	plugMil float64 // 插拔包络宽（mil）；0 = 算不出来
	plugSrc string  // "spec" | "table:<match>" | "fallback" | "unknown"
}

// isBoardConnector 判定一个已放置器件是不是连接器。
//
// 判据完全镜像 classifyCP/edgeRoleOf 的既有口径：
//   - 位号 J* 且非 JP*（JP 是跳线/短接排，按网就近摆，不是对外口）——这条覆盖了
//     `J_VEH` 这种**不带数字**的位号，而 connectorDesRe 的 `^J\d` 覆盖不到它，
//     偏偏 #168 的主要目标器件就长这样。
//   - CN/CON/USB/SIM/BAT + 数字（connectorDesRe，pcb_autoplace.go）。
//   - device 名命中连接器封装正则（cpReEdgeConn，pcb_place_constrained.go）。
//
// 注意 cpReAnyEdge 里的 wroom/antenna 不在此列：无线模组和贴片天线不是连接器，
// 它们不参与插头打架，也不该被算进「对外口聚一条边」。
//
// **位号优先于器件名**（真板实测修正，车机V2 166 器件板）：位号是设计者写下的分类，
// 器件名子串只是猜。放任 cpReEdgeConn 直接判，真板上出了两个假连接器：
//
//	ESD1     = USBLC6-2SC6  → 名字含 "usb" → 被判成 USB 连接器（它是 ESD 二极管）
//	TVS_VBUS = SMAJ5.0A     → 名字含 "sma" → 被判成 SMA 射频接头（它是 TVS，SMA 是**封装**名）
//
// 后一个尤其危险：它不但进了连接器集合，还从插头护套表里查到了 SMA 射频接头的
// 14mm 包络，于是报出「ESD1 ↔ TVS_VBUS 插头打架」这种纯属虚构的 WARN。
// 所以先按位号前缀把无源/半导体件排除掉，再谈器件名。
func isBoardConnector(c boardComp) bool {
	des := strings.ToUpper(strings.TrimSpace(c.Designator))
	if strings.HasPrefix(des, "JP") {
		return false
	}
	if strings.HasPrefix(des, "J") {
		return true
	}
	if connectorDesRe.MatchString(des) {
		return true
	}
	// 位号已经声明了它是别的东西 —— 器件名撞关键词不算数。
	if nonConnectorDesRe.MatchString(des) {
		return false
	}
	return c.Device != "" && cpReEdgeConn.MatchString(c.Device)
}

// nonConnectorDesRe 是「位号已表明它不是连接器」的前缀集：无源件、分立半导体、
// 保护件、晶体、测试点、安装孔。后面必须跟数字或下划线，所以 `TVS_VBUS`、`ESD1`、
// `D_ESD_ANT`、`F1`、`L2` 命中，而 `USB1`（U 后面是 S）不会被误伤。
//
// 刻意**不**包含的几个，各有理由：
//   - `U` —— 真板上 microSD 卡座、SIM 座、无线模组常标成 U*（单测里的
//     `U3 = MICRO-SD-PUSHPUSH` 就是），排掉它会把真卡座漏判成非连接器。
//     U* 继续走器件名判据，代价是 IC 若撞上 cpReEdgeConn 的关键词仍会误判 ——
//     但那个代价比漏判一个真卡座小。
//   - `P` / `CN` / `CON` —— 本来就是连接器位号。
//   - `SW` / `K` —— 开关、继电器虽不是连接器，但它们是用户可触的 user-facing 件，
//     edge-io 维要看它们，这里不做一刀切。
var nonConnectorDesRe = regexp.MustCompile(`(?i)^(?:C|R|L|D|Q|FB|F|Y|X|TP|MH|H|RV|TVS|ESD|VR)[\d_]`)

// connectorPins 是去重后的焊盘编号数。同号多焊盘（USB-C 的 A/B 双取向、大电流端子的
// 并联焊盘）只算一个引脚位 —— 排式连接器的胶壳宽度跟**位**数走，不跟焊盘数走。
func connectorPins(c boardComp) int {
	seen := map[string]bool{}
	for _, p := range c.Pads {
		if n := strings.TrimSpace(p.Number); n != "" {
			seen[n] = true
		}
	}
	return len(seen)
}

// connWireToBoardRe 是「线对板连接器」的器件名判据 —— internal 启发式的必要条件之一。
// 只收 JST 家族与其国产兼容件：这类座子天生是接一束线到另一块板/电芯，接到箱外的
// 概率远低于 Type-C/DC 座/螺钉端子。
var connWireToBoardRe = regexp.MustCompile(`(?i)ph-?2\.0|xh-?2\.54|zh-?1\.5|sh-?1\.0|\bjst\b|molex|picoblade|wire-?to-?board|线对板`)

// connExternalNetRes 是「对外语义网」：一个连接器只要碰到其中之一，就说明它真的要跟
// 箱外世界打交道，启发式便不再把它推定成 internal。
//
// 刻意**不**收 VBAT/VBATT/BAT+ 这类电池网：box-v2 的 J1 正是 VBATT/GND/TS_NTC 接箱内
// 电芯，把电池网算成对外会让这条启发式对最典型的目标器件失效。
var connExternalNetRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(usb[_-]?)?(dp|dm|d[+-]|d_?[pn])$`),               // USB 差分对
	regexp.MustCompile(`(?i)usb`),                                              // USB_DP / VBUS_USB / USB_5V …
	regexp.MustCompile(`(?i)^v?bus$`),                                          // VBUS
	regexp.MustCompile(`(?i)^(cc[12]|sbu[12])$`),                               // Type-C CC / SBU
	regexp.MustCompile(`(?i)^(vin|v_?in|dc_?in|pwr_?in|v_?ext|vext)$`),         // 对外电源入口
	regexp.MustCompile(`(?i)^(can_?[hl]|rs485_?[ab]|eth_?.+|rj45_?.+)$`),       // 对外总线
	regexp.MustCompile(`(?i)^(acc|ign|ignition|veh(icle)?[_-]?.*|kl15|kl30)$`), // 车规对外
}

// hasExternalNet 报告器件是否挂着任何一个对外语义网。
func hasExternalNet(c boardComp) bool {
	for _, n := range c.nets() {
		for _, re := range connExternalNetRes {
			if re.MatchString(n) {
				return true
			}
		}
	}
	return false
}

// connectorFacing 解析一个连接器的朝向语义 + 结论来源。
//
// 优先级：S0 spec 显式声明 > 启发式推定。spec 赢是因为「这块板的 J1 接箱内电芯」是
// **板级决定**，比「PH2.0 通常接箱内」的类别经验更具体（spec.Interface 的注释里立的
// 就是这条规矩）。
func connectorFacing(c boardComp, ifaces map[string]spec.Interface) (facing, source string) {
	des := strings.ToUpper(strings.TrimSpace(c.Designator))
	if in, ok := ifaces[des]; ok {
		if f := strings.ToLower(strings.TrimSpace(in.FacingOf())); f != "" {
			return f, "spec"
		}
	}
	// 启发式：**线对板类别** 且 **一个对外语义网都没有** → 推定 internal。
	// 两个条件缺一不可：只看类别会把接箱外传感器的 XH 座误判成 internal；只看网名会把
	// 一个纯 GND/未连的 Type-C 误判成 internal。
	if connWireToBoardRe.MatchString(c.Device) && !hasExternalNet(c) {
		return "internal", "heuristic"
	}
	if r := connectorRole(c); r != "" {
		return r, "heuristic"
	}
	return "", ""
}

// connectorRole 是 edgeRoleOf 对 boardComp 的转写（那个函数吃 cpComp，构造它需要
// apComp 全套字段，这里只有快照）。判据逐字相同：先 any（RF/天线座），再 user-facing。
func connectorRole(c boardComp) string {
	des := strings.ToUpper(strings.TrimSpace(c.Designator))
	if h, ok := placementIndex().ByRefPrefix[refPrefixCP(des)]; ok {
		switch strings.ToLower(strings.TrimSpace(h.Edge)) {
		case "user-facing":
			return "user-facing"
		case "any":
			return "any"
		}
	}
	if c.Device != "" && cpReAnyEdge.MatchString(c.Device) {
		return "any"
	}
	if (c.Device != "" && cpReEdgeConn.MatchString(c.Device)) ||
		(strings.HasPrefix(des, "J") && !strings.HasPrefix(des, "JP")) {
		return "user-facing"
	}
	return ""
}

// connEdgeGap 是封装到板框边界的最短距离（板外为负，无板框为 +Inf）。
//
// 取 bbox 四角的最小值而不是中心距：中心距会把一个 8mm 深的 Type-C 判成「离边 4mm」，
// 而它其实正压在边上。代价是渲染 bbox 含丝印、实测比本体大 40%+，所以这个距离系统性
// 偏小 —— 对「有没有占外沿」这个问题偏保守（宁可多报），方向是对的。
func connEdgeGap(c boardComp, o *boardOutline) float64 {
	if o == nil {
		return math.Inf(1)
	}
	if c.BBox == nil {
		cx, cy := c.center()
		return o.distToEdge(cx, cy)
	}
	d := math.Inf(1)
	for _, p := range [][2]float64{
		{c.BBox.MinX, c.BBox.MinY}, {c.BBox.MinX, c.BBox.MaxY},
		{c.BBox.MaxX, c.BBox.MinY}, {c.BBox.MaxX, c.BBox.MaxY},
	} {
		d = math.Min(d, o.distToEdge(p[0], p[1]))
	}
	return d
}

// connPlugWidth 解析一个连接器的插拔包络宽（mil）及其来源。
//
// 三级：spec 人工覆盖 > 块库查找表 > bbox 兜底。兜底取**沿板边方向**的 bbox 跨度
// （左右边看 y 向、上下边看 x 向）—— 打架发生在沿边排布的方向上，拿另一个方向的
// 尺寸比只会张冠李戴。
func connPlugWidth(c boardComp, in spec.Interface, hasIface bool, edge apEdge, hasEdge bool, pins int) (float64, string) {
	if hasIface && in.PlugWidthMM > 0 {
		return in.PlugWidthMM * mmToMil, "spec"
	}
	if env, ok := blocks.MatchPlugEnvelope(c.Device); ok {
		if w := env.WidthMM(pins); w > 0 {
			return w * mmToMil, "table:" + env.Match
		}
	}
	span := math.Max(c.width(), c.height())
	if hasEdge {
		if edge.vertical() { // 左/右边：沿边方向是 y
			span = c.height()
		} else {
			span = c.width()
		}
	}
	if span <= 0 {
		return 0, "unknown" // 没 bbox 又没查到表 —— 老实承认测不了，别拿 0 去比距离
	}
	return span + pcbPlugFallbackMarginMil, "fallback"
}

// collectBoardConnectors 把快照里的连接器全部判读一遍，按位号排序（输出确定，
// 才能进 golden 回归）。
func collectBoardConnectors(snap *boardSnapshot, s *spec.Spec) []boardConnector {
	if snap == nil {
		return nil
	}
	ifaces := s.InterfaceByRef()
	var o *boardOutline
	if snap.Outline != nil {
		o = snap.Outline
	}
	var out []boardConnector
	for _, c := range snap.Components {
		if !isBoardConnector(c) {
			continue
		}
		bc := boardConnector{comp: c, role: connectorRole(c), pins: connectorPins(c)}
		bc.facing, bc.facingSrc = connectorFacing(c, ifaces)
		bc.edgeGapMil = connEdgeGap(c, o)
		if o != nil {
			cx, cy := c.center()
			bc.edge, _ = o.nearestEdge(cx, cy)
			bc.hasEdge = true
		}
		in, hasIface := ifaces[strings.ToUpper(strings.TrimSpace(c.Designator))]
		bc.plugMil, bc.plugSrc = connPlugWidth(c, in, hasIface, bc.edge, bc.hasEdge, bc.pins)
		out = append(out, bc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].comp.Designator < out[j].comp.Designator })
	return out
}

// isInternal / isExternal 是两条规则的取件口径。facing 判不出来（""）时按**对外**
// 处理：位号是 J* 的件默认是对外口，这是 EDA 通行约定，也是保守的那一侧
// （internal-on-edge 宁可漏报也别乱扣一个备份电池座的分）。
func (b boardConnector) isInternal() bool { return b.facing == "internal" }
func (b boardConnector) isExternal() bool { return b.facing != "internal" }

// ---------------------------------------------------------------------------
// 规则① internal-on-edge
// ---------------------------------------------------------------------------

// findInternalOnEdge 报告「被标为 internal 的连接器却占着板外沿」。
//
// 为什么这是问题：板外沿是稀缺资源，应留给真正要出线的对外口。内部件（备份电池座、
// 板间排针）占了外沿会挤掉对外口的可达边，而且电芯辫线还得从板边绕回箱内 —— 实测
// 证据是 box-v2 rev-a 的 J1（PH2.0-3P 备份锂电池座，VBATT/GND/TS_NTC 接**箱内**电芯）
// 和车辆端子、Type-C 一起挤在底边外沿。
//
// 严重度分两档，这是这条规则的关键设计：spec 显式标注 → WARN（板级决定，可信）；
// 启发式推定 → INFO（类别经验，可能把接箱外传感器的 XH 座误判成内部件，不该阻塞）。
func findInternalOnEdge(conns []boardConnector, o *boardOutline) []pcbCheckFinding {
	if o == nil {
		return nil // 没板框就没有「外沿」这个概念，交由调用方 skip 掉整维
	}
	var out []pcbCheckFinding
	for _, b := range conns {
		if !b.isInternal() || !b.hasEdge || b.edgeGapMil >= pcbConnEdgeBandMil {
			continue
		}
		level := "INFO"
		src := "heuristic"
		if b.facingSrc == "spec" {
			level, src = "WARN", "spec"
		}
		cx, cy := b.comp.center()
		msg := fmt.Sprintf(
			"%s is internal (per %s) yet sits %.0fmil (%.2fmm) from the %s board edge — inside the %.0fmil rim band. "+
				"The rim is scarce: reserve it for connectors that actually leave the enclosure, "+
				"and an internal harness routed off the rim has to come back inside anyway [internal=%s]",
			b.comp.Designator, src, round2(b.edgeGapMil), round2(b.edgeGapMil/mmToMil),
			b.edge.String(), pcbConnEdgeBandMil, src,
		)
		f := pcbCheckFinding{
			Type: "internal-on-edge", Level: level,
			Designator: b.comp.Designator,
			Message:    msg + docRule("3.5", "对外接口与板沿 — 外沿是稀缺资源"),
			At:         &pcbXY{X: round2(cx), Y: round2(cy)},
		}
		if b.comp.ID != "" {
			f.Primitives = []string{b.comp.ID}
		}
		if o.Source != "polygon" {
			f.Message += " (board outline is an AABB approximation — the edge distance is approximate on a non-rectangular board)"
		}
		out = append(out, f)
	}
	return out
}

// ---------------------------------------------------------------------------
// 规则② connector-plug-clearance
// ---------------------------------------------------------------------------

// plugConflict 是一对插头打架的连接器（结构化）。findings 是它的人读投影 ——
// 归因（谁拉低了 edge-io 维）必须拿到**两个**位号，而 pcbCheckFinding.Designator
// 只装得下一个，从 message 里正则抠第二个是自找的脆弱。
type plugConflict struct {
	a, b     boardConnector
	distMil  float64 // 中心距
	needMil  float64 // 两者包络宽的均值 = 最小中心距
	fallback bool    // 任一侧的包络宽是 bbox 估出来的
	noEdge   bool    // 没有板框，配对没能按板边过滤
}

// connectorPlugConflicts 找出所有插头会打架的对外连接器配对。
//
// 判据：中心距 < 两者插拔包络宽的均值。包络宽是**插头/护套**宽而不是母座本体宽 ——
// 这正是既有工具全都漏掉的那一层：USB-C 母座本体 ~9mm，粗线插头护套 12-13mm，
// box-v2 rev-a 底边三口中心距 12-13mm，按 footprint 判全过，按插头判才暴露。
//
// 三条收窄，都是为了少报假警：
//  1. 只比**对外**口。internal 件的线束当然也占空间，但那属于规则①的治理范围
//     （它压根就不该在外沿），在这里再报一遍等于同一个毛病扣两次分。
//  2. 只比**同一装配面**。异面连接器的插头在 Z 向被板厚错开，侧向重叠不构成干涉。
//     任一侧 Layer 未知（0）时不做这个过滤 —— 未知不等于不同面。
//  3. 有板框时只比**同一条板边**。两个口分别在对边，插头朝相反方向，中心距再近也
//     插得进去。没板框时退化成比所有配对，并把这层降级记进 noEdge。
func connectorPlugConflicts(conns []boardConnector, o *boardOutline) []plugConflict {
	var ext []boardConnector
	for _, b := range conns {
		if b.isExternal() && b.plugMil > 0 {
			ext = append(ext, b)
		}
	}
	var out []plugConflict
	for i := 0; i < len(ext); i++ {
		for j := i + 1; j < len(ext); j++ {
			a, b := ext[i], ext[j]
			if a.comp.Layer != 0 && b.comp.Layer != 0 && a.comp.Layer != b.comp.Layer {
				continue
			}
			if o != nil && (!a.hasEdge || !b.hasEdge || a.edge != b.edge) {
				continue
			}
			ax, ay := a.comp.center()
			bx, by := b.comp.center()
			dist := math.Hypot(ax-bx, ay-by)
			need := (a.plugMil + b.plugMil) / 2
			if dist >= need {
				continue
			}
			out = append(out, plugConflict{
				a: a, b: b, distMil: dist, needMil: need,
				fallback: a.plugSrc == "fallback" || b.plugSrc == "fallback",
				noEdge:   o == nil,
			})
		}
	}
	return out
}

// findConnectorPlugClearance 是规则②的 finding 投影（`pcb check` 消费的形状）。
func findConnectorPlugClearance(conns []boardConnector, o *boardOutline) []pcbCheckFinding {
	var out []pcbCheckFinding
	for _, c := range connectorPlugConflicts(conns, o) {
		a, b := c.a, c.b
		msg := fmt.Sprintf(
			"%s ↔ %s center distance %.2fmm (%.0fmil) < %.2fmm required by their plug envelopes "+
				"(%s %.2fmm[%s], %s %.2fmm[%s]) — the receptacle footprints may clear, but the plugs/overmolds will collide",
			a.comp.Designator, b.comp.Designator, round2(c.distMil/mmToMil), round2(c.distMil), round2(c.needMil/mmToMil),
			a.comp.Designator, round2(a.plugMil/mmToMil), a.plugSrc,
			b.comp.Designator, round2(b.plugMil/mmToMil), b.plugSrc,
		)
		if c.fallback {
			// 兜底宽度是 bbox+2mm 估出来的，必须标出来，否则读的人无从判断这条
			// 该不该信（#168 明确要求）。
			msg += " [plug-width=fallback — no envelope table entry, estimated from the rendered bbox]"
		}
		if c.noEdge {
			msg += " (no board outline — pairs could not be filtered to a single edge, so this may be a pair that faces opposite ways)"
		}
		ax, ay := a.comp.center()
		bx, by := b.comp.center()
		f := pcbCheckFinding{
			Type: "connector-plug-clearance", Level: "WARN",
			Designator: a.comp.Designator,
			Message:    msg + docRule("3.5", "插头护套包络 ≠ 母座本体宽"),
			At:         &pcbXY{X: round2((ax + bx) / 2), Y: round2((ay + by) / 2)},
		}
		for _, id := range []string{a.comp.ID, b.comp.ID} {
			if id != "" {
				f.Primitives = append(f.Primitives, id)
			}
		}
		out = append(out, f)
	}
	return out
}

// ---------------------------------------------------------------------------
// 规则③ connector-mating-blocked —— 插拔通道禁布（mating corridor）
// ---------------------------------------------------------------------------

// matingAxisDir 把世界系开口向量吸附到主轴（±1,0 / 0,±1）。
//
// 旋转几乎总是 90° 的倍数，向量本来就在轴上；万一遇到斜置件（45° 旋转），取分量
// 更大的轴——走廊是轴对齐矩形，斜向走廊 v1 不做。两分量近乎相等（|差|<0.1）时
// 判不出主轴，老实返回 false，别猜。
func matingAxisDir(wx, wy float64) (dx, dy float64, ok bool) {
	ax, ay := math.Abs(wx), math.Abs(wy)
	if ax < 0.1 && ay < 0.1 {
		return 0, 0, false // 零向量：块声明解析失败
	}
	if math.Abs(ax-ay) < 0.1 {
		return 0, 0, false // 45° 斜置：主轴不明，不猜
	}
	if ax > ay {
		if wx > 0 {
			return 1, 0, true
		}
		return -1, 0, true
	}
	if wy > 0 {
		return 0, 1, true
	}
	return 0, -1, true
}

// connectorMatingCorridor 计算一个连接器的**板上**插拔通道矩形。
//
// 卧贴（水平插拔）插口的器件特性：插头从开口面沿开口方向水平进入，开口前方
// depth（pcbMatingCorridorDepthMil）纵深、连接器 bbox 宽（垂直开口轴）的一条
// 走廊里放了器件，插头就物理进不来——footprint 不重叠 ≠ 插得进去，这与规则②
// 的插头护套是同一类「板上几何里不存在的三维实体」。
//
// 开口方向的取法分三档，**判不了绝不猜**：
//  1. 块库声明（connOpeningFor，局部向量 → rotate2d 换算世界向量）——唯一可信源。
//  2. 没有声明但**贴边**（外沿带内）：按「朝板外」推定——此时通道在板外，
//     板上没有走廊可言，直接 ok=false（这正是绝大多数正确摆放的边缘连接器：
//     它们的通道被板框天然裁掉，永远不会产生误报）。
//  3. 非贴边又没有声明（板中的 FPC 座 / 内部 Type-C / 卡座）：铜箔几何里根本
//     没有开口方向这个信息，ok=false——宁可漏报，不拿猜的方向去骂人。
//
// 走廊裁剪到板框 AABB 内（多边形裁剪 v1 不做：AABB 只会把走廊裁**小**，方向是
// 保守的）；裁完退化（任一边 <1mil）= 通道全在板外，ok=false。
func connectorMatingCorridor(b boardConnector, o *boardOutline) (layoutBBox, bool) {
	if b.comp.BBox == nil {
		return layoutBBox{}, false // 没 bbox 连开口面在哪都定不了
	}
	lox, loy, declared := connOpeningFor(b.comp.Device)
	if !declared {
		// 贴边推定朝外 → 通道在板外，不产出板上走廊；非贴边 → 判不了方向。
		// 两条路殊途同归都是 false，但语义不同，分开写免得后人合并成一句后
		// 顺手给非贴边件加个「朝最近边」的猜测——那正是这条规则明令禁止的。
		return layoutBBox{}, false
	}
	wx, wy := rotate2d(lox, loy, b.comp.Rotation)
	dx, dy, ok := matingAxisDir(wx, wy)
	if !ok {
		return layoutBBox{}, false
	}
	bb := *b.comp.BBox
	var cor layoutBBox
	switch {
	case dx > 0:
		cor = layoutBBox{MinX: bb.MaxX, MinY: bb.MinY, MaxX: bb.MaxX + pcbMatingCorridorDepthMil, MaxY: bb.MaxY}
	case dx < 0:
		cor = layoutBBox{MinX: bb.MinX - pcbMatingCorridorDepthMil, MinY: bb.MinY, MaxX: bb.MinX, MaxY: bb.MaxY}
	case dy > 0:
		cor = layoutBBox{MinX: bb.MinX, MinY: bb.MaxY, MaxX: bb.MaxX, MaxY: bb.MaxY + pcbMatingCorridorDepthMil}
	default: // dy < 0
		cor = layoutBBox{MinX: bb.MinX, MinY: bb.MinY - pcbMatingCorridorDepthMil, MaxX: bb.MaxX, MaxY: bb.MinY}
	}
	if o != nil {
		cor.MinX = math.Max(cor.MinX, o.BBox.MinX)
		cor.MinY = math.Max(cor.MinY, o.BBox.MinY)
		cor.MaxX = math.Min(cor.MaxX, o.BBox.MaxX)
		cor.MaxY = math.Min(cor.MaxY, o.BBox.MaxY)
	}
	if cor.MaxX-cor.MinX < 1 || cor.MaxY-cor.MinY < 1 {
		return layoutBBox{}, false // 走廊全在板外（贴边朝外的正常姿态）
	}
	return cor, true
}

// compBodyBBox 是遮挡判定用的器件本体代理：**焊盘并集**优先，退化才用渲染 bbox。
//
// 选焊盘并集而不是渲染 bbox 的理由与 layout-lint 的 bodyOf 完全相同：渲染 bbox
// 含丝印和位号文字，实测比本体大 40%+，拿它判「三维实体挡道」会把丝印级贴边的
// 邻件全骂成遮挡（好板校准时 lint 吃过 104 处误报的亏）。焊盘并集从下方逼近本体
// ——宁可漏报一个壳体略宽于焊盘的件，不误报一排摆对了的去耦电容。
// 焊盘矩形按轴对齐 W×H 取（与 padCopperRect 同口径，忽略 pad 自身旋转——
// 旋转焊盘的 AABB 介于两者之间，误差远小于丝印那 40%）。
func compBodyBBox(c boardComp) (layoutBBox, bool) {
	bb := layoutBBox{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	any := false
	for _, p := range c.Pads {
		if p.W <= 0 || p.H <= 0 {
			continue
		}
		any = true
		bb.MinX = math.Min(bb.MinX, p.X-p.W/2)
		bb.MinY = math.Min(bb.MinY, p.Y-p.H/2)
		bb.MaxX = math.Max(bb.MaxX, p.X+p.W/2)
		bb.MaxY = math.Max(bb.MaxY, p.Y+p.H/2)
	}
	if any {
		return bb, true
	}
	if c.BBox != nil {
		return *c.BBox, true
	}
	return layoutBBox{}, false
}

// bboxIntersects 是 layoutBBox 的开区间相交判定（贴边相切不算相交——走廊边界
// 上恰好压线的件按「刚好让开」处理，与 cpRect.overlaps 的排他边界同口径）。
func bboxIntersects(a, b layoutBBox) bool {
	return a.MinX < b.MaxX && b.MinX < a.MaxX && a.MinY < b.MaxY && b.MinY < a.MaxY
}

// findConnectorMatingBlocked 报告「器件挡在卧贴插口的插拔通道里」。
//
// 每个遮挡件一条 finding，级别恒 WARN——走廊方向只可能来自块库声明（见
// connectorMatingCorridor），是三档置信度里的可信档，不需要像规则①那样按来源
// 分级。**归因落在遮挡件**（Designator = 遮挡件）：连接器是贴边定死的，动它
// 代价大；动遮挡件才是解法，精修环按 Designator 派活必须拿到该动的那个。
//
// 只比同装配面：异面器件的插头路径在 Z 向被板厚隔开（与规则②的收窄③同理）；
// 任一侧 Layer 未知（0）时不过滤——未知不等于不同面。
func findConnectorMatingBlocked(conns []boardConnector, comps []boardComp, o *boardOutline) []pcbCheckFinding {
	var out []pcbCheckFinding
	for _, b := range conns {
		cor, ok := connectorMatingCorridor(b, o)
		if !ok {
			continue
		}
		for _, c := range comps {
			if c.Designator == b.comp.Designator || (c.ID != "" && c.ID == b.comp.ID) {
				continue // 自己不算挡自己
			}
			if c.Layer != 0 && b.comp.Layer != 0 && c.Layer != b.comp.Layer {
				continue
			}
			body, hasBody := compBodyBBox(c)
			if !hasBody || !bboxIntersects(body, cor) {
				continue
			}
			cx, cy := c.center()
			f := pcbCheckFinding{
				Type: "connector-mating-blocked", Level: "WARN",
				Designator: c.Designator, // 归因给遮挡件——动它才能解决
				Message: fmt.Sprintf(
					"%s sits in the mating corridor of %s (the %.0fmil-deep zone in front of its block-declared opening) — "+
						"a horizontally-mated plug cannot enter with the part in the way; move %s clear of the corridor",
					c.Designator, b.comp.Designator, pcbMatingCorridorDepthMil, c.Designator,
				) + docRule("3.5", "插拔通道禁布 — 开口前方是插头的必经之路"),
				At: &pcbXY{X: round2(cx), Y: round2(cy)},
			}
			for _, id := range []string{c.ID, b.comp.ID} {
				if id != "" {
					f.Primitives = append(f.Primitives, id)
				}
			}
			out = append(out, f)
		}
	}
	return out
}
