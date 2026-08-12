package app

// cmd_sch_group_tidy.go — `sch group tidy`:组内布局计算(设计契约
// docs/schematic-layout-hierarchy.md §1;三层体系 Sheet→Zone→Group→Primitive
// 的 Group 层 tidy 能力)。
//
// v1 patterns:
//   - power-updown:双{power,gnd}旗无源件 —— 器件竖放、上电源旗/下 GND 旗、
//     文字朝外(契约校准表 tidyLabelRotation)、横排等距(默认 50,--spacing);
//     IC(若组内有)为锚不动,无 IC 时以组 bbox 中心为锚;
//   - signal-row:带信号 netport 的件 —— 保持横放,netport 一律水平(铁则4:
//     长条标竖排=折叠),pin 在器件中线左侧=left / 右侧=right;
//   - auto(默认):逐件判型 —— 每 pin 的目标旗从「现有连接」读(net + 旗类型):
//     IC → 锚;含 netport → signal-row(优先于双旗,竖放会折叠长条标);
//     双{power,gnd}旗且无未建模第三连接 → power-updown;其余 skip —— 信号
//     netflag / netlabel / 普通导线连接的 pin(如 3-pin 馈通电容的信号脚)
//     搬走器件会被静默扯断(铁则5),auto 降级 skip,显式 power-updown 报错。
//
// 执行铁则(契约,全部实战校准,违反必返工):
//   1. pin 实测:任何 rot 之后 fresh 重读 pin 实位再连 —— 同规格不同库件符号
//      存在镜像(cl05b104 需 rot90、grm21/cl21 需 rot270 才 pin1 朝上),rot90/
//      rot270 二义按「哪个 pin 在上」实测消解(tidyPowerPinOnTop),不按位号猜;
//   2. stale 防线:mutation 后 settle(double-read 一致 + ≥350ms 间隔)才读
//      (tidySettledPins);connect 全部用已实测的显式 pinX/pinY;
//   3. 文字朝外 rotation 只走 tidyLabelRotation 校准表,勿散写;
//   4. netport 永不竖放;
//   5. 收尾 layout-lint + bridge-check 自检,红则按记录的每步前几何逐步回滚
//      (tidyStepRecord / tidyRollback),不许半成品落地。
//
// 共享依赖只读复用(不改共享文件):cmd_sch_group.go(loadSchGroupsContext /
// findSchGroup / describeSchGroup / fetchSchWirePolylinesStable / schGroupWire /
// pointOnPolyline / schGroupEps)、cmd_sch_layout.go(parseLayoutComps /
// layoutComp / layoutPin / collectLayoutLint)、cmd_sch_bridge_check.go
// (parseBridgeReport)、cmd_sch_layoutscore.go(designatorPrefix / snap5)。
// 共享解析器不给的字段(component rotation / 每 pin 的 net)由本文件私有
// extractor(tidyExtractExtras)补齐。

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	// tidyDefaultSpacing:power-updown 横排等距的默认间距(契约 §1,单位 =
	// 原理图 native 坐标 0.01 inch)。
	tidyDefaultSpacing = 50.0
	// tidySettleDelay / tidySettleAttempts:铁则2 的 settle 参数 —— 每次读之间
	// ≥350ms,连续两读一致才算 settle,预算内不稳定则拒绝继续。
	tidySettleDelay    = 350 * time.Millisecond
	tidySettleAttempts = 4
)

// ── 纯核:分类 ───────────────────────────────────────────────────────────────

// tidyRole 是 auto 判型的结果(契约 §1)。
type tidyRole string

const (
	tidyRoleAnchorIC    tidyRole = "anchor-ic"
	tidyRolePowerUpdown tidyRole = "power-updown"
	tidyRoleSignalRow   tidyRole = "signal-row"
	tidyRoleSkip        tidyRole = "skip"
)

// tidyPinConn 是一个 pin 的「现有连接」事实:net + 旗类型(从画布几何读回,
// 不是目标)。Flag 取连接器的 componentType 值:netflag / netport / netlabel,
// "" = 该 pin 没挂标记。OnWire = pin 落在真实导线上(即使树上没有任何标记):
// 普通导线连接也是连接,搬器件前必须看见它(铁则5 —— 否则 3-pin 馈通电容的
// 第三 pin 会被静默扯断成开路)。
type tidyPinConn struct {
	Pin    string // pin number
	Net    string // 现有 net("" = 未连接)
	Flag   string // "netflag" | "netport" | "netlabel" | ""
	OnWire bool   // pin 在导线上(Flag != "" 时必为 true;Flag=="" 且 true = 普通线连接)
}

// tidyUnmodeledConn 判一个 pin 是否带着 power-updown 未建模的连接(铁则5:
// 器件被搬走时这些连接会被静默扯断成开路+孤旗,自检测不出):信号网/未知网的
// netflag、netlabel、无标记的普通导线。power/gnd netflag 与 netport 是已建模
// 形态(前者是 power-updown 的输入,后者由 signal-row 处理、planPowerUpdown
// 显式拒绝)。
func tidyUnmodeledConn(p tidyPinConn) bool {
	switch p.Flag {
	case "netport":
		return false
	case "netflag":
		c := tidyNetClass(p.Net)
		return c != "power" && c != "ground"
	case "":
		return p.OnWire
	}
	return true // netlabel 及其它标记类型:connect_pin 建不回来,一律未建模
}

// tidyConnDescribe 描述一个 pin 的连接形态(错误信息用)。
func tidyConnDescribe(p tidyPinConn) string {
	switch {
	case p.Flag != "" && p.Net != "":
		return fmt.Sprintf("%s %s", p.Flag, p.Net)
	case p.Flag != "":
		return p.Flag
	case p.OnWire:
		return "普通导线"
	}
	return "无连接"
}

// tidyNetClass 把网名分为 ground / power / signal("" = 未连接)。地族在前:
// GND/AGND/PGND/DGND/VSS;电源族 = VCC/VDD/VBUS/VBAT/VIN 或电压轨命名
// (5V / +5V / -5V / 3V3 / 3.3V / 12V0 …)。
func tidyNetClass(net string) string {
	n := strings.ToUpper(strings.TrimSpace(net))
	if n == "" {
		return ""
	}
	for _, g := range []string{"GND", "AGND", "PGND", "DGND", "VSS"} {
		if n == g || strings.HasPrefix(n, g+"_") {
			return "ground"
		}
	}
	for _, p := range []string{"VCC", "VDD", "VBUS", "VBAT", "VIN"} {
		if n == p || strings.HasPrefix(n, p+"_") {
			return "power"
		}
	}
	n = strings.TrimPrefix(n, "+")
	n = strings.TrimPrefix(n, "-")
	digits, seenV := 0, false
	for _, r := range n {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == 'V':
			seenV = true
		case r == '.':
		default:
			return "signal"
		}
	}
	if digits > 0 && seenV {
		return "power"
	}
	return "signal"
}

// classifyTidyMember 按「现有连接」逐件判型(契约 auto 判型)。优先级:
//  1. IC(位号 U 前缀)→ anchor-ic:锚不动,哪怕它也挂着 netport;
//  2. 任一 pin 挂 netport → signal-row:优先于双旗 —— 竖放会折叠长条标(铁则4);
//  3. 同时有 power 旗 pin 和 gnd 旗 pin,**且没有任何未建模的第三连接**(信号
//     netflag / netlabel / 普通导线,tidyUnmodeledConn)→ power-updown;有第三
//     连接的(如 3-pin 馈通电容)搬走器件会把那根线静默扯断(铁则5)→ skip;
//  4. 其余 → skip(信息不足,不动比动错好)。
//
// 分类只依赖连接(net + 旗类型 + 是否在线上),不依赖当前姿态 —— 已整理过的件
// 连接不变,分类不变,tidy 幂等。
func classifyTidyMember(designator string, pins []tidyPinConn) tidyRole {
	if strings.EqualFold(designatorPrefix(strings.TrimSpace(designator)), "U") {
		return tidyRoleAnchorIC
	}
	hasPower, hasGnd, hasUnmodeled := false, false, false
	for _, p := range pins {
		switch p.Flag {
		case "netport":
			return tidyRoleSignalRow
		case "netflag":
			switch tidyNetClass(p.Net) {
			case "power":
				hasPower = true
				continue
			case "ground":
				hasGnd = true
				continue
			}
		}
		if tidyUnmodeledConn(p) {
			hasUnmodeled = true
		}
	}
	if hasPower && hasGnd && !hasUnmodeled {
		return tidyRolePowerUpdown
	}
	return tidyRoleSkip
}

// ── 纯核:文字朝外校准表(铁则3) ────────────────────────────────────────────

// tidyKindFamily 归并 kind 别名:power / ground / netport("" = 未知)。
func tidyKindFamily(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "power":
		return "power"
	case "ground", "gnd", "agnd", "pgnd", "dgnd",
		"analog_ground", "protective_ground", "protect_ground":
		return "ground"
	case "netport", "port", "net_port_in", "net_port_out", "net_port_bi":
		return "netport"
	}
	return ""
}

// tidyLabelRotation 是契约的「文字朝外」rotation 校准表(真机校准 2026-08-12,
// 铁则3:connect 显式传 --rotation,值只从这张表出,勿散写):
//
//	power  up   → 0(文字上)    power  down → 180
//	ground down → 0(文字下)    ground up   → 180
//	netport left → 180  right → 0(orientation.json frozenTable port 行)
//
// netport 的 up/down 直接返回错误(铁则4:长条标竖排=折叠);power/ground 的
// left/right 契约未校准,同样拒绝 —— 表外组合宁可报错也不猜。
func tidyLabelRotation(kind, direction string) (float64, error) {
	dir := strings.ToLower(strings.TrimSpace(direction))
	switch tidyKindFamily(kind) {
	case "power":
		switch dir {
		case "up":
			return 0, nil
		case "down":
			return 180, nil
		}
		return 0, fmt.Errorf("tidy 校准表没有 power flag direction %q 的文字 rotation(契约只校准了 up/down)", direction)
	case "ground":
		switch dir {
		case "down":
			return 0, nil
		case "up":
			return 180, nil
		}
		return 0, fmt.Errorf("tidy 校准表没有 ground flag direction %q 的文字 rotation(契约只校准了 up/down)", direction)
	case "netport":
		switch dir {
		case "left":
			return 180, nil
		case "right":
			return 0, nil
		case "up", "down":
			return 0, fmt.Errorf("netport 永不竖放(铁则4:长条标竖排=折叠)— direction %q 拒绝", direction)
		}
		return 0, fmt.Errorf("netport direction %q 无效(只允许 left/right)", direction)
	}
	return 0, fmt.Errorf("未知 flag kind %q(power/ground 族或 netport 族)", kind)
}

// ── 纯核:power-updown 布局计划 ─────────────────────────────────────────────

// tidyMemberIn 是 planPowerUpdown 的输入:一件的位号 + 各 pin 现有连接。
type tidyMemberIn struct {
	Designator string
	Pins       []tidyPinConn
}

// tidyAnchor 是组内锚:IC 的几何(IsIC=true,锚不动,排在其 bbox 右侧),或
// 组 bbox 中心(IsIC=false,横排以它居中)。
type tidyAnchor struct {
	X, Y      float64
	IsIC      bool
	HalfWidth float64 // 锚件 bbox 半宽(无 bbox 时 0)
}

// tidyPinTarget 是一个 pin 的目标连接:方向 / 旗种 / 网名 / 文字 rotation
// (LabelRotation 只从 tidyLabelRotation 出)。
type tidyPinTarget struct {
	Pin           string
	Direction     string // up | down | left | right
	Kind          string // connect_pin 的 canonical kind:power / ground / net_port_bi
	Net           string
	LabelRotation float64
}

// tidyMemberPlan 是一件的 power-updown 目标:位置 + rotation 两候选 + pin 目标。
// RotationCandidates 的二义(库件符号镜像)由执行期实测消解:先 rot 到 [0],
// fresh 读 pin,电源 pin 不在上则改 [1] 再读(铁则1/2)。
type tidyMemberPlan struct {
	Designator         string
	X, Y               float64
	RotationCandidates [2]float64
	PowerPin, GndPin   string // 消解判据:PowerPin 实测必须在上(y-UP)
	Pins               []tidyPinTarget
}

// tidyDesignatorLess 位号自然序:前缀字母段字典序,同前缀按数字。
func tidyDesignatorLess(a, b string) bool {
	pa, pb := designatorPrefix(a), designatorPrefix(b)
	if !strings.EqualFold(pa, pb) {
		return strings.ToUpper(pa) < strings.ToUpper(pb)
	}
	na, ea := strconv.Atoi(a[len(pa):])
	nb, eb := strconv.Atoi(b[len(pb):])
	if ea == nil && eb == nil {
		return na < nb
	}
	return strings.ToUpper(a) < strings.ToUpper(b)
}

// planPowerUpdown 计算 power-updown 组内布局(纯函数):器件竖放、上电下地、
// 文字朝外、横排等距。spacing<=0 用默认 50。锚有 IC 时从 IC bbox 右侧起排,
// 无 IC 时以锚(组 bbox 中心)水平居中。全部坐标 snap 到 5 单位连线网格。
func planPowerUpdown(members []tidyMemberIn, anchor tidyAnchor, spacing float64) ([]tidyMemberPlan, error) {
	if len(members) == 0 {
		return nil, nil
	}
	if spacing <= 0 {
		spacing = tidyDefaultSpacing
	}
	ordered := append([]tidyMemberIn(nil), members...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return tidyDesignatorLess(ordered[i].Designator, ordered[j].Designator)
	})
	rowY := snap5(anchor.Y)
	var startX float64
	if anchor.IsIC {
		startX = snap5(anchor.X + anchor.HalfWidth + spacing)
	} else {
		startX = snap5(anchor.X - spacing*float64(len(ordered)-1)/2)
	}
	out := make([]tidyMemberPlan, 0, len(ordered))
	for i, m := range ordered {
		var powerPin, gndPin *tidyPinConn
		for k := range m.Pins {
			p := &m.Pins[k]
			switch p.Flag {
			case "netport":
				return nil, fmt.Errorf("%s 的 pin %s 挂着 netport(%s)— 属 signal-row,竖放会折叠长条标(铁则4),不能按 power-updown 排",
					m.Designator, p.Pin, p.Net)
			case "netflag":
				switch tidyNetClass(p.Net) {
				case "power":
					if powerPin != nil {
						return nil, fmt.Errorf("%s 有多个电源旗 pin(%s/%s)— power-updown 需要恰好一电源一地", m.Designator, powerPin.Pin, p.Pin)
					}
					powerPin = p
					continue
				case "ground":
					if gndPin != nil {
						return nil, fmt.Errorf("%s 有多个地旗 pin(%s/%s)— power-updown 需要恰好一电源一地", m.Designator, gndPin.Pin, p.Pin)
					}
					gndPin = p
					continue
				}
			}
			// 铁则5:power/gnd 旗之外任何有连接的 pin(信号 netflag / netlabel /
			// 普通导线)都是 power-updown 未建模的第三连接 —— 器件搬走会把它
			// 静默扯断成开路+孤旗(3-pin 馈通电容场景),报错而不是静默忽略。
			if tidyUnmodeledConn(*p) {
				return nil, fmt.Errorf("%s 的 pin %s 有 power-updown 未建模的连接(%s)— 器件搬走会把这根连接静默扯断成开路(铁则5),不能按 power-updown 排(auto 判型会 skip 它)",
					m.Designator, p.Pin, tidyConnDescribe(*p))
			}
		}
		if powerPin == nil || gndPin == nil {
			return nil, fmt.Errorf("%s 不是双{power,gnd}旗件(电源旗=%t 地旗=%t)— 不能按 power-updown 排(auto 判型会 skip 它)",
				m.Designator, powerPin != nil, gndPin != nil)
		}
		upRot, err := tidyLabelRotation("power", "up")
		if err != nil {
			return nil, err
		}
		downRot, err := tidyLabelRotation("ground", "down")
		if err != nil {
			return nil, err
		}
		out = append(out, tidyMemberPlan{
			Designator:         m.Designator,
			X:                  snap5(startX + float64(i)*spacing),
			Y:                  rowY,
			RotationCandidates: [2]float64{90, 270},
			PowerPin:           powerPin.Pin,
			GndPin:             gndPin.Pin,
			Pins: []tidyPinTarget{
				{Pin: powerPin.Pin, Direction: "up", Kind: "power", Net: powerPin.Net, LabelRotation: upRot},
				{Pin: gndPin.Pin, Direction: "down", Kind: "ground", Net: gndPin.Net, LabelRotation: downRot},
			},
		})
	}
	return out, nil
}

// tidyPowerPinOnTop 是 rot 二义的消解判据(铁则1):对 rot 之后 fresh 实测的
// pins,「电源 pin 在上」(y-UP:y 更大)= 候选正确。两 pin 实测同高说明旋转
// 候选没把器件立起来(符号基向异常)→ 错误,拒绝硬连。
func tidyPowerPinOnTop(measured []layoutPin, powerPin, gndPin string) (bool, error) {
	var pw, gd *layoutPin
	for i := range measured {
		switch measured[i].Number {
		case powerPin:
			pw = &measured[i]
		case gndPin:
			gd = &measured[i]
		}
	}
	if pw == nil || gd == nil {
		return false, fmt.Errorf("实测 pins 里找不到 power pin %q / gnd pin %q(fresh 读回 %d 个 pin)", powerPin, gndPin, len(measured))
	}
	dy := pw.Y - gd.Y
	if math.Abs(dy) <= schGroupEps {
		return false, fmt.Errorf("power pin 与 gnd pin 实测同高(y=%g)— 旋转候选没把器件立起来(符号基向异常),拒绝硬连", pw.Y)
	}
	return dy > 0, nil
}

// ── 纯核:signal-row 计划 ───────────────────────────────────────────────────

// tidySignalPinIn 是 signal-row 输入的一个 pin:实测 x + 是否挂 netport。
type tidySignalPinIn struct {
	Pin    string
	X      float64
	Net    string
	IsPort bool
}

// tidySignalMemberIn 是 signal-row 的一件:位号 + 器件中线 x + pins。
type tidySignalMemberIn struct {
	Designator string
	CenterX    float64
	Pins       []tidySignalPinIn
}

// tidySignalPlan 是一件的 netport 水平化目标(只含 netport pin;位置/器件
// rotation 不动 —— 保持横放)。
type tidySignalPlan struct {
	Designator string
	Pins       []tidyPinTarget
}

// planSignalRow 计算 signal-row 目标(纯函数):器件不动、netport 一律水平
// (铁则4),pin 在器件中线左侧(含线上)= left(信号入)、右侧 = right(信号
// 出),文字 rotation 走校准表。无 netport 的件不出计划。
func planSignalRow(members []tidySignalMemberIn) ([]tidySignalPlan, error) {
	var out []tidySignalPlan
	for _, m := range members {
		var pins []tidyPinTarget
		for _, p := range m.Pins {
			if !p.IsPort {
				continue
			}
			dir := "right"
			if p.X <= m.CenterX {
				dir = "left"
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
		out = append(out, tidySignalPlan{Designator: m.Designator, Pins: pins})
	}
	return out, nil
}

// ── 纯核:现有连接的几何发现 ────────────────────────────────────────────────

// tidyWireRoots 对全页 wires 做一次共点 union-find(与 expandGroupAttachments
// 同一 eps 语义),返回每根 wire 的树根编号 —— pin↔标记的归属都按树判。
func tidyWireRoots(wires []schGroupWire) []int {
	n := len(wires)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if wiresTouch(wires[i], wires[j], schGroupEps) {
				parent[find(i)] = find(j)
			}
		}
	}
	roots := make([]int, n)
	for i := range roots {
		roots[i] = find(i)
	}
	return roots
}

// tidyPinAttachment 找一个 pin 的现有旗连接:pin → 所在 wire 树 → 树上锚着的
// net 标记。netflag 必须经真实导线相连(压坐标不算连接),所以只认树上的锚。
// 同树多标记时 netport 优先(它决定 signal-row 分类),再 netflag,再 netlabel。
// 第三返回值 onWire:pin 落在某根导线上(即使树上无任何标记)—— 普通导线连接
// 也是连接,不返回它就会被折叠成「未连接」而在 tidy 搬移时被静默扯断(铁则5)。
func tidyPinAttachment(pinX, pinY float64, wires []schGroupWire, roots []int, markers []layoutComp) (layoutComp, bool, bool) {
	touched := map[int]bool{}
	for i, w := range wires {
		if pointOnPolyline(pinX, pinY, w.Points, schGroupEps) {
			touched[roots[i]] = true
		}
	}
	if len(touched) == 0 {
		return layoutComp{}, false, false
	}
	rank := func(t string) int {
		switch t {
		case "netport":
			return 0
		case "netflag":
			return 1
		case "netlabel":
			return 2
		}
		return 3
	}
	best := -1
	for mi := range markers {
		m := &markers[mi]
		if !m.AnchorAvailable {
			continue
		}
		for i, w := range wires {
			if !touched[roots[i]] {
				continue
			}
			if pointOnPolyline(m.X, m.Y, w.Points, schGroupEps) {
				if best < 0 || rank(m.ComponentType) < rank(markers[best].ComponentType) {
					best = mi
				}
				break
			}
		}
	}
	if best < 0 {
		return layoutComp{}, false, true // 在线上但树上无标记 = 普通导线连接
	}
	return markers[best], true, true
}

// tidyStubDirection 从 pin→标记锚的位移推 stub 方向(主轴)与长度(y-UP:
// dy>0 = up)。回滚重建原连接时用。
func tidyStubDirection(pinX, pinY, anchorX, anchorY float64) (string, float64) {
	dx, dy := anchorX-pinX, anchorY-pinY
	if math.Abs(dx) >= math.Abs(dy) {
		if dx >= 0 {
			return "right", math.Abs(dx)
		}
		return "left", math.Abs(dx)
	}
	if dy >= 0 {
		return "up", math.Abs(dy)
	}
	return "down", math.Abs(dy)
}

// ── 纯核:raw result 补齐 extractor ─────────────────────────────────────────

// tidyCompExtra 补齐共享 parseLayoutComps 丢掉的字段:器件 rotation(回滚要
// 恢复原姿态)与每 pin 的现有 net(分类要读现有连接)。
type tidyCompExtra struct {
	Rotation float64
	PinNets  map[string]string // pinNumber → net
}

// tidyExtractExtras 从 components.list 原始 result 提取 tidyCompExtra,按大写
// 位号索引;只看真器件(componentType 空或 "part")。
func tidyExtractExtras(result map[string]any) map[string]tidyCompExtra {
	out := map[string]tidyCompExtra{}
	raw, _ := result["components"].([]any)
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if ct := asString(m["componentType"]); ct != "" && ct != schLayoutPartType {
			continue
		}
		desig := strings.ToUpper(strings.TrimSpace(asString(m["designator"])))
		if desig == "" {
			continue
		}
		ex := tidyCompExtra{PinNets: map[string]string{}}
		if r, ok := finiteFloat(m["rotation"]); ok {
			ex.Rotation = r
		}
		if pins, ok := m["pins"].([]any); ok {
			for _, pr := range pins {
				pm, ok := pr.(map[string]any)
				if !ok {
					continue
				}
				num := asString(pm["pinNumber"])
				if num == "" {
					continue
				}
				if net := asString(pm["net"]); net != "" {
					ex.PinNets[num] = net
				}
			}
		}
		out[desig] = ex
	}
	return out
}

// tidyPinsAgree 判两次实测的 pin 集是否一致(double-read settle 判据):同数量、
// 同 pin 号、坐标差 ≤ schGroupEps。
func tidyPinsAgree(a, b []layoutPin) bool {
	if len(a) != len(b) {
		return false
	}
	byNum := map[string][2]float64{}
	for _, p := range a {
		byNum[p.Number] = [2]float64{p.X, p.Y}
	}
	for _, p := range b {
		prev, ok := byNum[p.Number]
		if !ok {
			return false
		}
		if math.Abs(prev[0]-p.X) > schGroupEps || math.Abs(prev[1]-p.Y) > schGroupEps {
			return false
		}
	}
	return true
}

// ── 纯核:live member 模型 + 计划编排 ───────────────────────────────────────

// tidyLivePin 是一个 pin 的画布事实:连接(分类输入)+ 实测坐标 + 挂着的标记
// (回滚重建原连接的几何来源)。
type tidyLivePin struct {
	Conn      tidyPinConn
	X, Y      float64
	Marker    layoutComp
	HasMarker bool
}

// tidyLiveMember 是组内一件的画布事实 + auto 判型结果。
type tidyLiveMember struct {
	Comp     layoutComp
	Rotation float64
	Pins     []tidyLivePin
	Role     tidyRole
}

func (m *tidyLiveMember) conns() []tidyPinConn {
	out := make([]tidyPinConn, len(m.Pins))
	for i, p := range m.Pins {
		out[i] = p.Conn
	}
	return out
}

func (m *tidyLiveMember) pin(number string) *tidyLivePin {
	for i := range m.Pins {
		if m.Pins[i].Conn.Pin == number {
			return &m.Pins[i]
		}
	}
	return nil
}

// tidyVerticalPortPins 找一件里 stub 竖着的 netport pin(铁则4 违例,signal-row
// 执行只修这些;已水平的 netport 不动 → 幂等)。
func tidyVerticalPortPins(m tidyLiveMember) []string {
	var out []string
	for _, p := range m.Pins {
		if !p.HasMarker || p.Marker.ComponentType != "netport" {
			continue
		}
		dir, _ := tidyStubDirection(p.X, p.Y, p.Marker.X, p.Marker.Y)
		if dir == "up" || dir == "down" {
			out = append(out, p.Conn.Pin)
		}
	}
	return out
}

// tidyResolveAnchor 定组内锚:优先含 IC(anchor-ic 角色)且 bbox 最大者
// (IC 不动,横排从其右侧起);无 IC 时取全体成员 bbox 并集的中心。第三返回值
// ok=false = 锚不可得(全员既无 bbox 又无锚坐标)—— 调用方必须报错而不是拿
// 零值锚继续,否则 power-updown 会把整排错排到 (0,0)(F4)。无几何的 IC 也
// 当不了锚(其 X/Y 是零值),跳过让位给有几何的成员。
func tidyResolveAnchor(members []tidyLiveMember) (tidyAnchor, string, bool) {
	bestIC, bestArea := -1, -1.0
	for i := range members {
		if members[i].Role != tidyRoleAnchorIC {
			continue
		}
		c := members[i].Comp
		if c.BBox == nil && !c.AnchorAvailable {
			continue // 无几何的 IC:X/Y 是零值,不能当锚
		}
		area := 0.0
		if c.BBox != nil {
			area = bboxArea(c.BBox)
		}
		if area > bestArea {
			bestIC, bestArea = i, area
		}
	}
	if bestIC >= 0 {
		c := members[bestIC].Comp
		a := tidyAnchor{X: c.X, Y: c.Y, IsIC: true}
		if c.BBox != nil {
			a.X = (c.BBox.MinX + c.BBox.MaxX) / 2
			a.Y = (c.BBox.MinY + c.BBox.MaxY) / 2
			a.HalfWidth = (c.BBox.MaxX - c.BBox.MinX) / 2
		}
		return a, c.Designator, true
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for i := range members {
		c := members[i].Comp
		if c.BBox != nil {
			minX, minY = math.Min(minX, c.BBox.MinX), math.Min(minY, c.BBox.MinY)
			maxX, maxY = math.Max(maxX, c.BBox.MaxX), math.Max(maxY, c.BBox.MaxY)
		} else if c.AnchorAvailable {
			minX, minY = math.Min(minX, c.X), math.Min(minY, c.Y)
			maxX, maxY = math.Max(maxX, c.X), math.Max(maxY, c.Y)
		}
	}
	if math.IsInf(minX, 1) {
		return tidyAnchor{}, "", false
	}
	return tidyAnchor{X: (minX + maxX) / 2, Y: (minY + maxY) / 2}, "", true
}

// tidyRoleEntry 是计划报告里的一行:位号 → 角色。
type tidyRoleEntry struct {
	Designator string
	Role       tidyRole
}

// tidyPlanned 是整组的 tidy 计划(dry-run 的报告体,--apply 的执行输入)。
type tidyPlanned struct {
	Pattern     string
	Spacing     float64
	Anchor      tidyAnchor
	AnchorDesig string
	Roles       []tidyRoleEntry
	Power       []tidyMemberPlan
	Signal      []tidySignalPlan // 只含需修复(stub 竖放)的 netport pin
	SignalNoop  []string         // signal-row 件但 netport 已全水平(幂等,no-op)
	Skipped     []string
}

// buildTidyPlan 把 live members 编排成计划(纯函数,I/O 已在调用方完成)。
// pattern 强制:power-updown 把非 IC 全按双旗排(不合规即错);signal-row 把
// 全员当 signal 候选(只修竖放 netport);auto 按 classify。
func buildTidyPlan(members map[string]tidyLiveMember, order []string, pattern string, spacing float64) (*tidyPlanned, error) {
	p := &tidyPlanned{Pattern: pattern, Spacing: spacing}
	lives := make([]tidyLiveMember, 0, len(order))
	for _, d := range order {
		lives = append(lives, members[d])
	}
	var anchorOK bool
	p.Anchor, p.AnchorDesig, anchorOK = tidyResolveAnchor(lives)

	var powerIns []tidyMemberIn
	var signalLives []tidyLiveMember
	for _, d := range order {
		m := members[d]
		role := m.Role
		switch pattern {
		case "power-updown":
			if role != tidyRoleAnchorIC {
				role = tidyRolePowerUpdown
			}
		case "signal-row":
			role = tidyRoleSignalRow
		}
		p.Roles = append(p.Roles, tidyRoleEntry{Designator: d, Role: role})
		switch role {
		case tidyRolePowerUpdown:
			powerIns = append(powerIns, tidyMemberIn{Designator: m.Comp.Designator, Pins: m.conns()})
		case tidyRoleSignalRow:
			signalLives = append(signalLives, m)
		case tidyRoleSkip:
			p.Skipped = append(p.Skipped, d)
		}
	}

	// F4 guard:锚不可得(全员无 bbox/锚坐标)时零值锚会把 power-updown 整排
	// 错排到 (0,0)—— 只要有件要按锚排,锚必须真实可得,否则报错拒绝出计划。
	if len(powerIns) > 0 && !anchorOK {
		return nil, fmt.Errorf("组锚不可得:全员既无 bbox 又无锚坐标 — power-updown 无法定横排位置(零值锚会错排到 (0,0)),拒绝出计划(确认目标页已渲染 bbox 后重跑)")
	}

	var err error
	p.Power, err = planPowerUpdown(powerIns, p.Anchor, spacing)
	if err != nil {
		return nil, err
	}

	// signal-row:只把 stub 竖着的 netport pin 列入执行计划(已水平 = no-op)。
	for _, m := range signalLives {
		vertical := map[string]bool{}
		for _, pin := range tidyVerticalPortPins(m) {
			vertical[pin] = true
		}
		if len(vertical) == 0 {
			p.SignalNoop = append(p.SignalNoop, strings.ToUpper(m.Comp.Designator))
			continue
		}
		center := m.Comp.X
		if m.Comp.BBox != nil {
			center = (m.Comp.BBox.MinX + m.Comp.BBox.MaxX) / 2
		}
		in := tidySignalMemberIn{Designator: m.Comp.Designator, CenterX: center}
		for _, lp := range m.Pins {
			if !vertical[lp.Conn.Pin] {
				continue
			}
			in.Pins = append(in.Pins, tidySignalPinIn{Pin: lp.Conn.Pin, X: lp.X, Net: lp.Conn.Net, IsPort: true})
		}
		plans, perr := planSignalRow([]tidySignalMemberIn{in})
		if perr != nil {
			return nil, perr
		}
		p.Signal = append(p.Signal, plans...)
	}
	return p, nil
}

// ── I/O 管线 ────────────────────────────────────────────────────────────────

// tidyReadScene 读一次画布事实:components(带 bbox+pins)+ 私有 extras +
// 稳定 wire 快照(fetchSchWirePolylinesStable,共享复用)。
func tidyReadScene(cfg *appConfig, win, docUUID string) ([]layoutComp, map[string]tidyCompExtra, []schGroupWire, error) {
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", win,
		map[string]any{"includePins": true, "includeBBox": true}, docUUID, "read tidy geometry")
	if err != nil {
		return nil, nil, nil, err
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return nil, nil, nil, err
	}
	extras := tidyExtractExtras(res.Result)
	wires, err := fetchSchWirePolylinesStable(cfg, win, docUUID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("读 wire 快照:%w", err)
	}
	return comps, extras, wires, nil
}

// buildTidyMembers 把组成员映射到画布事实。任一成员不在当前页即拒绝 —— 半个
// 组的 tidy 正是 group 要防的事故形态。
func buildTidyMembers(g *schGroup, comps []layoutComp, extras map[string]tidyCompExtra, wires []schGroupWire) (map[string]tidyLiveMember, []string, error) {
	var markers []layoutComp
	parts := map[string]layoutComp{}
	for _, c := range comps {
		switch {
		case isSchMarker(c.ComponentType):
			if c.AnchorAvailable && c.ID != "" {
				markers = append(markers, c)
			}
		case c.ComponentType == "" || c.ComponentType == schLayoutPartType:
			if d := strings.ToUpper(strings.TrimSpace(c.Designator)); d != "" {
				parts[d] = c
			}
		}
	}
	roots := tidyWireRoots(wires)
	out := map[string]tidyLiveMember{}
	var order, missing []string
	for _, d := range g.Members {
		c, ok := parts[d]
		if !ok {
			missing = append(missing, d)
			continue
		}
		ex := extras[d]
		live := tidyLiveMember{Comp: c, Rotation: ex.Rotation}
		for _, p := range c.Pins {
			marker, found, onWire := tidyPinAttachment(p.X, p.Y, wires, roots, markers)
			conn := tidyPinConn{Pin: p.Number, OnWire: onWire}
			if ex.PinNets != nil {
				conn.Net = ex.PinNets[p.Number]
			}
			if found {
				conn.Flag = marker.ComponentType
				if conn.Net == "" {
					conn.Net = marker.Net
				}
			}
			live.Pins = append(live.Pins, tidyLivePin{Conn: conn, X: p.X, Y: p.Y, Marker: marker, HasMarker: found})
		}
		live.Role = classifyTidyMember(c.Designator, live.conns())
		out[d] = live
		order = append(order, d)
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("group %s 有成员不在当前页:%s —— 成员齐才敢整组 tidy(`sch group list` 查看;`sch group remove` 清 stale)",
			describeSchGroup(g), strings.Join(missing, ","))
	}
	return out, order, nil
}

// tidySettledPins 读一件的 fresh pin 实位,带 settle(铁则2):每读间隔
// ≥350ms,连续两读一致才可信;预算内不稳定 = 平台快照未 settle,拒绝继续。
func tidySettledPins(cfg *appConfig, win, docUUID, desig string) ([]layoutPin, error) {
	var prev []layoutPin
	have := false
	for attempt := 0; attempt < tidySettleAttempts; attempt++ {
		time.Sleep(tidySettleDelay)
		res, err := requestAutolayoutAction(cfg, "schematic.components.list", win,
			map[string]any{"includePins": true}, docUUID, "measure tidy pins")
		if err != nil {
			return nil, err
		}
		comps, err := parseLayoutComps(res.Result)
		if err != nil {
			return nil, err
		}
		var cur []layoutPin
		found := false
		for _, c := range comps {
			if (c.ComponentType == "" || c.ComponentType == schLayoutPartType) &&
				strings.EqualFold(strings.TrimSpace(c.Designator), desig) {
				cur, found = c.Pins, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("实测找不到 %s(components.list 无此位号)", desig)
		}
		if have && tidyPinsAgree(prev, cur) {
			return cur, nil
		}
		prev, have = cur, true
	}
	return nil, fmt.Errorf("%s 的 pin 实测 %d 次仍未一致(平台快照未 settle)— 稍候重跑", desig, tidySettleAttempts)
}

func tidyPinCoord(pins []layoutPin, number string) (float64, float64, bool) {
	for _, p := range pins {
		if p.Number == number {
			return p.X, p.Y, true
		}
	}
	return 0, 0, false
}

// tidyModifyPose 改一件的位置+rotation(component modify,同 primitiveId 存活)。
func tidyModifyPose(cfg *appConfig, win, docUUID, primitiveID string, x, y, rot float64) error {
	_, err := requestAutolayoutAction(cfg, "schematic.component.modify", win,
		map[string]any{"primitiveId": primitiveID, "patch": map[string]any{"x": x, "y": y, "rotation": rot}},
		docUUID, "tidy pose")
	return err
}

// ── 回滚(铁则5) ───────────────────────────────────────────────────────────

// tidyPinRestore 记录一个被动过的 pin 的「前几何」连接,回滚重建用。
type tidyPinRestore struct {
	Pin       string
	Net       string
	Kind      string // connect_pin canonical kind;"" = netlabel 等无法重建(警告跳过)
	Direction string
	Offset    float64
	HasFlag   bool
}

// tidyStepRecord 是一步(一件)的前几何,失败时逐步回滚的输入。
type tidyStepRecord struct {
	Designator  string
	PrimitiveID string
	OrigX       float64
	OrigY       float64
	OrigRot     float64
	Restores    []tidyPinRestore
}

// tidyRestoreKind 从原标记推回 connect_pin 的 kind。netport 的 in/out/bi 子型
// components.list 不区分,回滚统一 net_port_bi(best-effort,net 名保真)。
func tidyRestoreKind(markerType, net string) string {
	switch markerType {
	case "netflag":
		if tidyNetClass(net) == "ground" {
			return "ground"
		}
		return "power"
	case "netport":
		return "net_port_bi"
	}
	return ""
}

// tidyBuildRecord 在动一件之前记录它的前几何(位置/rotation/每个将被动的 pin
// 的原连接)。touched = 本步将 disconnect+reconnect 的 pin 号。
func tidyBuildRecord(live tidyLiveMember, touched []string) tidyStepRecord {
	rec := tidyStepRecord{
		Designator:  live.Comp.Designator,
		PrimitiveID: live.Comp.ID,
		OrigX:       live.Comp.X,
		OrigY:       live.Comp.Y,
		OrigRot:     live.Rotation,
	}
	for _, num := range touched {
		lp := live.pin(num)
		if lp == nil || !lp.HasMarker {
			rec.Restores = append(rec.Restores, tidyPinRestore{Pin: num})
			continue
		}
		net := lp.Conn.Net
		if net == "" {
			net = lp.Marker.Net
		}
		dir, off := tidyStubDirection(lp.X, lp.Y, lp.Marker.X, lp.Marker.Y)
		rec.Restores = append(rec.Restores, tidyPinRestore{
			Pin: num, Net: net, Kind: tidyRestoreKind(lp.Marker.ComponentType, net),
			Direction: dir, Offset: off, HasFlag: true,
		})
	}
	return rec
}

// tidyRollback 按记录的每步前几何逆序回滚(铁则5)。best-effort:单步失败打
// 警告继续 —— 回滚里再抛错只会让现场更糟,残余交给收尾自检报告。
func tidyRollback(cfg *appConfig, win, docUUID string, steps []tidyStepRecord, stderr io.Writer) {
	for i := len(steps) - 1; i >= 0; i-- {
		st := steps[i]
		for _, r := range st.Restores {
			if _, err := requestAutolayoutAction(cfg, "schematic.pin.disconnect", win,
				map[string]any{"designator": st.Designator, "pin": r.Pin}, docUUID, "rollback disconnect"); err != nil {
				fmt.Fprintf(stderr, "  ⚠ 回滚 %s:%s disconnect:%v(可能本就无旗,继续)\n", st.Designator, r.Pin, err)
			}
		}
		if err := tidyModifyPose(cfg, win, docUUID, st.PrimitiveID, st.OrigX, st.OrigY, st.OrigRot); err != nil {
			fmt.Fprintf(stderr, "  ⚠ 回滚 %s 姿态失败:%v\n", st.Designator, err)
			continue
		}
		pins, err := tidySettledPins(cfg, win, docUUID, st.Designator)
		if err != nil {
			fmt.Fprintf(stderr, "  ⚠ 回滚 %s 实测 pin 失败:%v — 原连接未重建\n", st.Designator, err)
			continue
		}
		for _, r := range st.Restores {
			if !r.HasFlag {
				continue
			}
			if r.Kind == "" {
				fmt.Fprintf(stderr, "  ⚠ 回滚 %s:%s 原标记类型无法经 connect_pin 重建(netlabel 类)— 需手工补\n", st.Designator, r.Pin)
				continue
			}
			px, py, ok := tidyPinCoord(pins, r.Pin)
			if !ok {
				fmt.Fprintf(stderr, "  ⚠ 回滚 %s:%s 实测 pins 里找不到该 pin\n", st.Designator, r.Pin)
				continue
			}
			payload := map[string]any{"pinX": px, "pinY": py, "kind": r.Kind, "net": r.Net, "direction": r.Direction}
			if r.Offset > 0 {
				payload["offset"] = r.Offset
			}
			if rot, rerr := tidyLabelRotation(r.Kind, r.Direction); rerr == nil {
				payload["rotation"] = rot
			}
			if _, err := requestAutolayoutAction(cfg, "schematic.power.connect_pin", win, payload, docUUID, "rollback connect"); err != nil {
				fmt.Fprintf(stderr, "  ⚠ 回滚 %s:%s 重连失败:%v\n", st.Designator, r.Pin, err)
			}
		}
	}
}

// ── 执行 ────────────────────────────────────────────────────────────────────

// tidyDisconnectCollateral 从 disconnect result 提取 alsoDisconnectedPins ——
// 连接器对合并树整树删除时回报的「连带被断开的其它 pin」(desig:pin 列表)。
func tidyDisconnectCollateral(result map[string]any) []string {
	if result == nil {
		return nil
	}
	return asStringSlice(result["alsoDisconnectedPins"])
}

// tidyGuardDisconnect 判一次 disconnect 是否连带断开了共享导线上的邻件 pin
// (铁则5:两电容共享一根 GND rail 一面旗时,拆第一颗会整树删、第二颗被静默
// 断开)。非空立即按错误处理 —— 调用方触发既有逐步回滚;错误信息列出受影响
// pin,便于人工复核重连。纯函数,便于表驱动测试。
func tidyGuardDisconnect(designator, pin string, result map[string]any) error {
	collateral := tidyDisconnectCollateral(result)
	if len(collateral) == 0 {
		return nil
	}
	return fmt.Errorf("disconnect %s:%s 删的是合并导线树,连带断开了共享导线上的其它 pin:%s — 邻件被静默断开违反铁则5,按错误回滚(先分离共享 rail/旗再 tidy)",
		designator, pin, strings.Join(collateral, ","))
}

func tidyPinNumbers(targets []tidyPinTarget) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.Pin
	}
	return out
}

// tidyExecPowerMember 执行一件 power-updown:逐 pin disconnect → modify 到
// (x,y)+rot 候选1 → settle 实测 → 电源 pin 不在上则换候选2 再实测 → 显式坐标
// connect(铁则1/2/3 全程落实)。
func tidyExecPowerMember(cfg *appConfig, win, docUUID string, live tidyLiveMember, mp tidyMemberPlan, stdout io.Writer) error {
	// 旧桩/旗已由 tidyDeepSweep 整树删净(共享树在 sweep 期即拒),此处直接落位。
	usedRot := mp.RotationCandidates[0]
	if err := tidyModifyPose(cfg, win, docUUID, live.Comp.ID, mp.X, mp.Y, usedRot); err != nil {
		return fmt.Errorf("modify %s → (%g,%g) rot %g:%w", live.Comp.Designator, mp.X, mp.Y, usedRot, err)
	}
	pins, err := tidySettledPins(cfg, win, docUUID, live.Comp.Designator)
	if err != nil {
		return err
	}
	onTop, err := tidyPowerPinOnTop(pins, mp.PowerPin, mp.GndPin)
	if err != nil {
		return err
	}
	if !onTop {
		// rot 二义消解(铁则1):候选1 实测电源 pin 不在上 → 镜像符号,换候选2。
		usedRot = mp.RotationCandidates[1]
		if err := tidyModifyPose(cfg, win, docUUID, live.Comp.ID, mp.X, mp.Y, usedRot); err != nil {
			return fmt.Errorf("modify %s rot 候选2 %g:%w", live.Comp.Designator, usedRot, err)
		}
		if pins, err = tidySettledPins(cfg, win, docUUID, live.Comp.Designator); err != nil {
			return err
		}
		if onTop, err = tidyPowerPinOnTop(pins, mp.PowerPin, mp.GndPin); err != nil {
			return err
		}
		if !onTop {
			return fmt.Errorf("%s rot %g/%g 两候选实测电源 pin %s 都不在上 — 符号基向异常,拒绝半成品",
				live.Comp.Designator, mp.RotationCandidates[0], mp.RotationCandidates[1], mp.PowerPin)
		}
	}
	for _, t := range mp.Pins {
		px, py, ok := tidyPinCoord(pins, t.Pin)
		if !ok {
			return fmt.Errorf("%s 实测 pins 里没有 pin %s", live.Comp.Designator, t.Pin)
		}
		payload := map[string]any{
			"pinX": px, "pinY": py, "kind": t.Kind, "net": t.Net,
			"direction": t.Direction, "rotation": t.LabelRotation,
		}
		if _, err := requestAutolayoutAction(cfg, "schematic.power.connect_pin", win, payload, docUUID, "tidy connect"); err != nil {
			return fmt.Errorf("connect %s:%s → %s %s(%s):%w", live.Comp.Designator, t.Pin, t.Direction, t.Kind, t.Net, err)
		}
	}
	fmt.Fprintf(stdout, "  ✓ %s → (%g,%g) rot %g;pin%s→up(power) pin%s→down(gnd) 文字朝外\n",
		live.Comp.Designator, mp.X, mp.Y, usedRot, mp.PowerPin, mp.GndPin)
	return nil
}

// tidyExecSignalMember 执行一件 signal-row:只拆竖放的 netport(铁则4)→
// settle 实测 → 按左入右出水平重连(器件位置/rotation 不动)。
func tidyExecSignalMember(cfg *appConfig, win, docUUID string, live tidyLiveMember, sp tidySignalPlan, stdout io.Writer) error {
	// 旧竖桩已由 tidyDeepSweep 整树删净(共享树 sweep 期即拒)。
	pins, err := tidySettledPins(cfg, win, docUUID, live.Comp.Designator)
	if err != nil {
		return err
	}
	for _, t := range sp.Pins {
		px, py, ok := tidyPinCoord(pins, t.Pin)
		if !ok {
			return fmt.Errorf("%s 实测 pins 里没有 pin %s", live.Comp.Designator, t.Pin)
		}
		payload := map[string]any{
			"pinX": px, "pinY": py, "kind": t.Kind, "net": t.Net,
			"direction": t.Direction, "rotation": t.LabelRotation,
		}
		if _, err := requestAutolayoutAction(cfg, "schematic.power.connect_pin", win, payload, docUUID, "tidy connect"); err != nil {
			return fmt.Errorf("connect %s:%s → %s netport(%s):%w", live.Comp.Designator, t.Pin, t.Direction, t.Net, err)
		}
	}
	fmt.Fprintf(stdout, "  ✓ %s netport 水平化:%d 个 pin\n", live.Comp.Designator, len(sp.Pins))
	return nil
}

// tidySelfCheck 收尾自检(铁则5):layout-lint 0 overlap + bridge-check 0
// bridge。检查本身跑不了也算红(没有证明 = 不算过,fail-closed)。
func tidySelfCheck(cfg *appConfig, win, docUUID string) error {
	rep, err := collectLayoutLint(cfg, win, 2.54, 0, false, false, false)
	if err != nil {
		return fmt.Errorf("layout-lint 无法运行(没有证明不算过):%w", err)
	}
	if !rep.OK {
		return fmt.Errorf("layout-lint 红:%s", rep.Summary)
	}
	res, err := requestAutolayoutAction(cfg, "schematic.bridgeCheck", win, nil, docUUID, "tidy bridge-check")
	if err != nil {
		return fmt.Errorf("bridge-check 无法运行(没有证明不算过):%w", err)
	}
	brep, err := parseBridgeReport(res.Result)
	if err != nil {
		return fmt.Errorf("bridge-check 结果不可解析:%w", err)
	}
	if brep.Summary.Bridges > 0 {
		var nets []string
		for _, t := range brep.Trees {
			if strings.EqualFold(t.Kind, "BRIDGE") {
				nets = append(nets, "["+strings.Join(t.Nets, ",")+"]")
			}
		}
		return fmt.Errorf("bridge-check 红:%d 个 wire-bridge(真短路)%s", brep.Summary.Bridges, strings.Join(nets, " "))
	}
	return nil
}

// tidyApply 顺序执行计划,每步先记前几何;任一步失败或收尾自检红 → 逆序回滚。
// tidyDeepSweepPlan classifies, per tidy member, every wire TREE that touches the
// member's pins or grazes its bbox: a tree that also touches a NON-member part
// pin is a shared rail (same F2 semantics — refuse, the neighbour would be cut);
// everything else (old stubs, stacked repair flags, dangling remnants like the
// grey half-segment live-reported 2026-08-12) is debris to delete wholesale
// before rebuilding. Pure function — table-testable.
func tidyDeepSweepPlan(memberDesigs map[string]bool, comps []layoutComp, wires []schGroupWire) (deleteIDs []string, sharedErr error) {
	const eps = 0.5
	const graze = 2.0
	// Union-find over wires (same touch semantics as the group expansion family).
	parent := make([]int, len(wires))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i := 0; i < len(wires); i++ {
		for j := i + 1; j < len(wires); j++ {
			touched := false
			for k := 0; k+1 < len(wires[i].Points) && !touched; k += 2 {
				if pointOnPolyline(wires[i].Points[k], wires[i].Points[k+1], wires[j].Points, eps) {
					touched = true
				}
			}
			for k := 0; k+1 < len(wires[j].Points) && !touched; k += 2 {
				if pointOnPolyline(wires[j].Points[k], wires[j].Points[k+1], wires[i].Points, eps) {
					touched = true
				}
			}
			if touched {
				parent[find(i)] = find(j)
			}
		}
	}
	// Member pin set, member bboxes, and NON-member part pins.
	type pt struct{ x, y float64 }
	var memberPins []pt
	var memberBoxes []layoutBBox
	var otherPins []pt
	for _, c := range comps {
		isPart := c.ComponentType == "" || c.ComponentType == schLayoutPartType
		if !isPart {
			continue
		}
		if memberDesigs[strings.ToUpper(c.Designator)] {
			for _, p := range c.Pins {
				memberPins = append(memberPins, pt{p.X, p.Y})
			}
			if c.BBox != nil {
				memberBoxes = append(memberBoxes, *c.BBox)
			}
		} else {
			for _, p := range c.Pins {
				otherPins = append(otherPins, pt{p.X, p.Y})
			}
		}
	}
	inGrazeBox := func(x, y float64) bool {
		for _, b := range memberBoxes {
			if x >= b.MinX-graze && x <= b.MaxX+graze && y >= b.MinY-graze && y <= b.MaxY+graze {
				return true
			}
		}
		return false
	}
	// Tree membership: touches member pin OR grazes member bbox → candidate;
	// also touches a non-member pin → shared (refuse).
	treeTouchesMember := map[int]bool{}
	treeTouchesOther := map[int]bool{}
	for wi, w := range wires {
		root := find(wi)
		for _, p := range memberPins {
			if pointOnPolyline(p.x, p.y, w.Points, eps) {
				treeTouchesMember[root] = true
			}
		}
		for k := 0; k+1 < len(w.Points); k += 2 {
			if inGrazeBox(w.Points[k], w.Points[k+1]) {
				treeTouchesMember[root] = true
			}
		}
		for _, p := range otherPins {
			if pointOnPolyline(p.x, p.y, w.Points, eps) {
				treeTouchesOther[root] = true
			}
		}
	}
	for root := range treeTouchesMember {
		if treeTouchesOther[root] {
			return nil, fmt.Errorf("成员的连线树同时触及非成员器件 pin(共享导线)— 深度清扫会切断邻件,拒绝(先手工梳理该树或把邻件一并入组)")
		}
	}
	// Collect: all wires of member trees + all markers anchored on those trees.
	seen := map[string]bool{}
	for wi, w := range wires {
		if treeTouchesMember[find(wi)] && !seen[w.ID] {
			seen[w.ID] = true
			deleteIDs = append(deleteIDs, w.ID)
		}
	}
	for _, c := range comps {
		if !isSchMarker(c.ComponentType) || !c.AnchorAvailable {
			continue
		}
		for wi, w := range wires {
			if treeTouchesMember[find(wi)] && pointOnPolyline(c.X, c.Y, w.Points, eps) {
				if !seen[c.ID] {
					seen[c.ID] = true
					deleteIDs = append(deleteIDs, c.ID)
				}
				break
			}
		}
	}
	sort.Strings(deleteIDs)
	return deleteIDs, nil
}

// tidyDeepSweep executes the sweep: one prim-delete for the whole debris set.
// Replaces the old per-pin disconnect loop — that loop missed dangling remnants
// that touch no pin (the grey half-segment), and its "No stub wire found" error
// was a false failure on already-clean pins (live 2026-08-12).
func tidyDeepSweep(cfg *appConfig, win, docUUID string, plan *tidyPlanned, members map[string]tidyLiveMember, comps []layoutComp, stdout, stderr io.Writer) error {
	memberSet := map[string]bool{}
	for _, mp := range plan.Power {
		memberSet[strings.ToUpper(mp.Designator)] = true
	}
	for _, sp := range plan.Signal {
		memberSet[strings.ToUpper(sp.Designator)] = true
	}
	if len(memberSet) == 0 {
		return nil
	}
	wires, err := fetchSchWirePolylinesStable(cfg, win, docUUID)
	if err != nil {
		return fmt.Errorf("deep-sweep wire read:%w", err)
	}
	ids, err := tidyDeepSweepPlan(memberSet, comps, wires)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := requestAutolayoutAction(cfg, "schematic.primitives.delete", win,
		map[string]any{"primitiveIds": ids}, docUUID, "tidy deep-sweep"); err != nil {
		return fmt.Errorf("deep-sweep delete %d primitive(s):%w", len(ids), err)
	}
	fmt.Fprintf(stdout, "  深度清扫:删除 %d 个旧桩/旗/残段(整树)\n", len(ids))
	return nil
}

func tidyApply(cfg *appConfig, win, docUUID string, plan *tidyPlanned, members map[string]tidyLiveMember, comps []layoutComp, stdout, stderr io.Writer) error {
	// 深度清扫先行:成员的整树(旧桩+旧旗+不触 pin 的残段)一次删净,重建才
	// 从干净地基开始;共享树(触非成员 pin)拒绝 —— F2 同语义,零 mutation。
	if err := tidyDeepSweep(cfg, win, docUUID, plan, members, comps, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return err
	}
	var executed []tidyStepRecord
	fail := func(err error) error {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		if len(executed) > 0 {
			fmt.Fprintf(stderr, "按记录的每步前几何逐步回滚 %d 步(铁则5)…\n", len(executed))
			tidyRollback(cfg, win, docUUID, executed, stderr)
		}
		return err
	}
	for _, mp := range plan.Power {
		live, ok := members[strings.ToUpper(mp.Designator)]
		if !ok {
			return fail(fmt.Errorf("计划成员 %s 不在 live 集合(内部不一致)", mp.Designator))
		}
		executed = append(executed, tidyBuildRecord(live, tidyPinNumbers(mp.Pins)))
		if err := tidyExecPowerMember(cfg, win, docUUID, live, mp, stdout); err != nil {
			return fail(fmt.Errorf("tidy %s:%w", mp.Designator, err))
		}
	}
	for _, sp := range plan.Signal {
		live, ok := members[strings.ToUpper(sp.Designator)]
		if !ok {
			return fail(fmt.Errorf("计划成员 %s 不在 live 集合(内部不一致)", sp.Designator))
		}
		executed = append(executed, tidyBuildRecord(live, tidyPinNumbers(sp.Pins)))
		if err := tidyExecSignalMember(cfg, win, docUUID, live, sp, stdout); err != nil {
			return fail(fmt.Errorf("tidy %s:%w", sp.Designator, err))
		}
	}
	if len(executed) == 0 {
		fmt.Fprintln(stdout, "✓ 组内无需改动(已整理,幂等)")
		return nil
	}
	if err := tidySelfCheck(cfg, win, docUUID); err != nil {
		return fail(fmt.Errorf("收尾自检红,回滚:%w", err))
	}
	fmt.Fprintf(stdout, "✓ tidy 落地 %d 件;layout-lint + bridge-check 自检绿\n", len(executed))
	return nil
}

// ── 报告渲染 ────────────────────────────────────────────────────────────────

func renderTidyPlan(p *tidyPlanned, g *schGroup, w io.Writer) {
	fmt.Fprintf(w, "group %s tidy 计划(pattern %s,spacing %g):\n", describeSchGroup(g), p.Pattern, p.Spacing)
	if p.AnchorDesig != "" {
		fmt.Fprintf(w, "  锚:%s(anchor-ic,不动)@ (%.0f,%.0f) 半宽 %.0f\n", p.AnchorDesig, p.Anchor.X, p.Anchor.Y, p.Anchor.HalfWidth)
	} else {
		fmt.Fprintf(w, "  锚:组 bbox 中心 (%.0f,%.0f)(组内无 IC)\n", p.Anchor.X, p.Anchor.Y)
	}
	for _, r := range p.Roles {
		fmt.Fprintf(w, "  %-6s %s\n", r.Designator, r.Role)
	}
	for _, mp := range p.Power {
		fmt.Fprintf(w, "  → %s 竖放 @ (%g,%g) rot{%g|%g}(实测消解);pin%s up power %s(label %g)/ pin%s down gnd %s(label %g)\n",
			mp.Designator, mp.X, mp.Y, mp.RotationCandidates[0], mp.RotationCandidates[1],
			mp.PowerPin, mp.Pins[0].Net, mp.Pins[0].LabelRotation,
			mp.GndPin, mp.Pins[1].Net, mp.Pins[1].LabelRotation)
	}
	for _, sp := range p.Signal {
		for _, t := range sp.Pins {
			fmt.Fprintf(w, "  → %s pin%s netport 竖放 → %s 水平(net %s,label %g)\n",
				sp.Designator, t.Pin, t.Direction, t.Net, t.LabelRotation)
		}
	}
	for _, d := range p.SignalNoop {
		fmt.Fprintf(w, "  = %s netport 已全水平(no-op,幂等)\n", d)
	}
	if len(p.Skipped) > 0 {
		fmt.Fprintf(w, "  skip:%s(无 netport / 双{power,gnd}旗连接)\n", strings.Join(p.Skipped, ","))
	}
}

// ── cobra 构造函数(主会话统一注册到 `sch group` 下) ────────────────────────

// newSchGroupTidyCommand 返回 `tidy` 子命令(挂在 `sch group` 下 → `easyeda sch
// group tidy`)。签名与其它 sch 子命令一致:cfg + *window 闭包 + stdout/stderr。
func newSchGroupTidyCommand(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var groupRef, pattern string
	var spacing float64
	var dryRun, apply bool
	c := &cobra.Command{
		Use:   "tidy",
		Short: "组内布局计算:双旗无源件竖放上电下地文字朝外,netport 水平化(默认 dry-run)",
		Long: `组内布局计算(设计契约 docs/schematic-layout-hierarchy.md §1,Group 层 tidy)。

patterns(--pattern):
  auto(默认)  逐件判型:每 pin 的目标旗从现有连接读(net+旗类型)——
                IC → 锚不动;含 netport → signal-row;双{power,gnd}旗且无
                其它连接 → power-updown;其余 skip(信号 netflag/netlabel/
                普通导线连着的第三 pin 搬走会被扯断,一律不动)
  power-updown  双旗无源件竖放、上电源旗/下 GND 旗、文字朝外(校准表)、
                横排等距(--spacing,默认 50);IC 为锚,无 IC 用组 bbox 中心
  signal-row    保持横放,竖着的 netport 拆掉按左入右出水平重连(netport 永不竖放)

默认 dry-run 只打印计划;--apply 落地,执行管线全程落实契约铁则:逐件
disconnect → modify(rot 二义两候选)→ settle(double-read + ≥350ms)→ fresh
实测 pin 消解候选 → 显式坐标 connect(--rotation 走文字朝外校准表)→ 收尾
layout-lint + bridge-check 自检,红则按记录的每步前几何逐步回滚。`,
		Args: cobra.NoArgs,
		Example: `  easyeda sch group tidy --group g1                 # dry-run 看计划
  easyeda sch group tidy --group decaps --apply     # 落地
  easyeda sch group tidy --group g1 --pattern power-updown --spacing 60 --apply`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRun && apply {
				return fmt.Errorf("--dry-run 与 --apply 互斥(默认就是 dry-run)")
			}
			return runSchGroupTidy(cfg, *window, groupRef, pattern, spacing, apply, stdout, stderr)
		},
	}
	c.Flags().StringVar(&groupRef, "group", "", "组 id(g1)或名字(必填;`sch group list` 查看)")
	c.Flags().StringVar(&pattern, "pattern", "auto", "布局模式:auto | power-updown | signal-row")
	c.Flags().Float64Var(&spacing, "spacing", tidyDefaultSpacing, "power-updown 横排间距(原理图单位)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只打印计划不落地(默认行为,flag 仅作显式声明)")
	c.Flags().BoolVar(&apply, "apply", false, "落地执行(带 settle/实测/自检/回滚)")
	_ = c.MarkFlagRequired("group")
	return c
}

// runSchGroupTidy 是编排:读组 → 读画布 → 建 live members → 编计划 → 渲染;
// --apply 时执行。
func runSchGroupTidy(cfg *appConfig, window, groupRef, pattern string, spacing float64, apply bool, stdout, stderr io.Writer) error {
	switch pattern {
	case "auto", "power-updown", "signal-row":
	default:
		return fmt.Errorf("--pattern %q 无效,应为 auto | power-updown | signal-row", pattern)
	}
	if spacing <= 0 {
		return fmt.Errorf("--spacing 必须 > 0(单位 = 原理图 native 坐标,默认 %g)", tidyDefaultSpacing)
	}
	pinned, win, docUUID, _, _, groups, err := loadSchGroupsContext(cfg, window)
	if err != nil {
		return err
	}
	g, err := findSchGroup(groups, groupRef)
	if err != nil {
		return err
	}
	comps, extras, wires, err := tidyReadScene(pinned, win, docUUID)
	if err != nil {
		return err
	}
	members, order, err := buildTidyMembers(g, comps, extras, wires)
	if err != nil {
		return err
	}
	plan, err := buildTidyPlan(members, order, pattern, spacing)
	if err != nil {
		return err
	}
	renderTidyPlan(plan, g, stdout)
	if !apply {
		fmt.Fprintln(stdout, "dry-run(默认):未改画布 —— 加 --apply 落地")
		return nil
	}
	return tidyApply(pinned, win, docUUID, plan, members, comps, stdout, stderr)
}
