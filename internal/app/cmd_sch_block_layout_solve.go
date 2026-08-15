package app

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
)

// ── 关系约束布局求解器(issue #180 P2,纯函数层)────────────────────────────
//
// 用户拍板的方向:「只需要知道元器件和连接方式,就能用算法快速落地布局」。
// 关系模板(flow/attach/pair)只提供**语义意图**;所有几何保证来自
// findSlotNormalized 的四个谓词(collides / inBounds / hitsTitle / normalize)。
// 这个分工是本设计的全部:关系给**种子点 + 方向序**,谓词给**不出界、不压标题带、
// 不压别人、判定坐标 = 落地坐标**。
//
// 为什么不是绝对偏移(legacy):块作者写模板时根本不知道实例最终落在页面哪里、
// 旁边有什么、图纸多大、分区标题带在哪 —— 那些信息只有落地时才有。手算模板
// 同一轮踩了三个坑(负偏移出图纸左界、块太高出上界、去耦顶到标题带),三个都是
// 几何问题,本就该由求解器处理。
//
// **间距一律算出来,不许拍脑袋**(项目铁律):
//   - bslPartGap  = bapObstacleGap(复用 block-apply 既有契约,不新造第 7 套间距常量)
//   - bslLanePitch = 2 × schAnchorGrid(两条平行导线要分得开,最小就是两个连接网格)
//   - reach       = schStubLen + relayoutPortWidth(net)(桩长 + 标签实测宽度)
// 本文件新增的间距常量净增 0。

// schStubLen 是 connect_pin 落 netflag/netport 时的标准桩长。原本以裸 30 散落在
// cmd_sch_zone_relayout.go 的四处调用里;求解器要用它算「marker 会伸出多远」,
// 两处必须消费同一个数,否则间距算出来对不上实际渲染。
const schStubLen = 30.0

// bslPartGap 是件与件之间的最小视觉间隙。
const bslPartGap = bapObstacleGap

// bslLanePitch 是两条平行跨接导线之间的通道宽度,从连接网格推导(不是估的)。
const bslLanePitch = 2 * schAnchorGrid

// bslTightHalf 是一个件的半宽 —— **求解器全域唯一的一把尺**。
//
// bapRoleHalfExtent 刻意「只高不低」(fallback 网格的件间距靠它兜底),但贴脚要的是
// 紧密 —— 用 50 会把 0402 去耦推到离芯片 79 远,视觉上完全不像"贴"。分立件的 10 是
// ceshi 实测值(0402 符号 20×16),不是估的。
func bslTightHalf(partKey string) float64 {
	k := strings.ToLower(partKey)
	for _, pre := range []string{"cap.", "res.", "ind.", "diode.", "led.", "tvs.", "esd.", "bjt.", "mos."} {
		if strings.HasPrefix(k, pre) {
			return 10
		}
	}
	return bapRoleHalfExtent(partKey)
}

// bslPartBox 是件的判定 box(以件中心为原点)。放置前它只有下限,落地后由硬门
// (verifyBlockLayout / layout-lint 的真实 bbox)兜底。
//
// **求解、避让、推让必须共用它**:第一版 seed 用紧凑半宽(10)、box 用保守半宽(50),
// 于是贴脚的件被判成"撞上锚件"而降级走网格(实测 C8/R5 都中招);推让那一版更狠 ——
// `bapHalfExtentFn(0, nil)` 恒返 0,间隙里根本没算被推件自己的身宽,于是每次都少推
// 一个半宽。判定与生成同源是本项目反复吃亏的那条定律,这里把尺收成一把。
func bslPartBox(partKey string) layoutBBox {
	h := bslTightHalf(partKey)
	v := math.Min(h, bapPartMargin)
	return layoutBBox{MinX: -h, MinY: -v, MaxX: h, MaxY: v}
}

// bslRelations 是求解器的输入契约。
//
// **P5(从 internal_nets + ports.dir + 引脚坐标反推关系)的输出类型也是它** ——
// 那一步做完时求解器一行不用改,这正是把「关系」独立成类型的目的。
type bslRelations struct {
	Anchor string
	Flow   []string
	Attach map[string]string // 角色 → "目标ROLE.PIN"
	Pair   [][]string
	Orient map[string]string // 角色 → vertical|horizontal
}

// bslRelationsFrom 把块模板投影成求解器输入。legacy(roles)模板返回 ok=false ——
// 调用方走老的偏移路径。
func bslRelationsFrom(l *blocks.SchematicLayout) (bslRelations, bool) {
	if !l.IsRelational() {
		return bslRelations{}, false
	}
	return bslRelations{
		Anchor: l.Anchor,
		Flow:   append([]string(nil), l.Flow...),
		Attach: l.Attach,
		Pair:   l.Pair,
		Orient: l.Orient,
	}, true
}

// bslAnchorRole 选锚件 —— 块里那个「不动、其余件围着它排」的角色。
//
// 五级判据,每级都有确定性 tie-break(同输入同输出是回归测试的前提):
//  1. 显式 anchor;
//  2. **被 attach 指向次数最多的 role** —— 这正是「谁是主芯片」的电路学定义:
//     去耦挂在谁身上谁就是主角;
//  3. 半宽最大(bapRoleHalfExtent);
//  4. 在 internal_nets 里出现次数最多(引脚数的代理);
//  5. role 名字典序最小。
//
// fail-closed:显式 anchor 指向不存在的 role 时**报错拒绝**,绝不悄悄回退到推导 ——
// 否则作者以为自己指定生效了。
func bslAnchorRole(b blocks.Block, rel bslRelations, nets [][]string) (string, error) {
	if rel.Anchor != "" {
		if _, ok := b.Parts[rel.Anchor]; !ok {
			return "", fmt.Errorf("schematic_layout.anchor %q 不是本块的 role —— 拒绝出计划(不回退推导,否则你会以为指定生效了)", rel.Anchor)
		}
		return rel.Anchor, nil
	}
	roles := make([]string, 0, len(b.Parts))
	for r := range b.Parts {
		roles = append(roles, r)
	}
	if len(roles) == 0 {
		return "", fmt.Errorf("块没有任何 part,无法选锚件")
	}
	sort.Strings(roles)

	attachedTo := map[string]int{}
	for _, target := range rel.Attach {
		if tRole, _, ok := splitBlockPinRef(target); ok {
			attachedTo[tRole]++
		}
	}
	pinCount := map[string]int{}
	for _, net := range nets {
		for _, m := range net {
			if strings.HasPrefix(m, "PORT:") {
				continue
			}
			if r, _, ok := splitBlockPinRef(m); ok {
				pinCount[r]++
			}
		}
	}

	best := roles[0]
	for _, r := range roles[1:] {
		if bslAnchorBetter(b, r, best, attachedTo, pinCount) {
			best = r
		}
	}
	return best, nil
}

// bslAnchorBetter 报告 a 是否比 b 更适合当锚(五级判据的比较器)。
func bslAnchorBetter(blk blocks.Block, a, b string, attachedTo, pinCount map[string]int) bool {
	if attachedTo[a] != attachedTo[b] {
		return attachedTo[a] > attachedTo[b]
	}
	ha, hb := bapRoleHalfExtent(blk.Parts[a].Part), bapRoleHalfExtent(blk.Parts[b].Part)
	if ha != hb {
		return ha > hb
	}
	if pinCount[a] != pinCount[b] {
		return pinCount[a] > pinCount[b]
	}
	return a < b // 字典序兜底:同输入同输出
}

// bslReach 是一个引脚上挂了 marker 之后,从引脚往外伸出多远 —— 桩长 + 标签实测
// 宽度。件间距必须给它留够,否则两件靠得下、标签却糊在一起。
func bslReach(net string) float64 {
	return schStubLen + relayoutPortWidth(net)
}

// bslFlowGap 算信号流上相邻两件之间该留多宽。三项取最大:
//
//	① 视觉最小间隙;
//	② 跨接导线通道:两件之间每多一条网就多一条平行走线;
//	③ 两侧 marker 的伸出之和 —— 这一项才是把「标签互相糊住」挡在门外的东西。
//
// 三项都从数据/实测推导,没有一个是拍脑袋常量。
//
// 第三项**必须再加一个视觉间隙**:两件的 marker 是朝着对方伸的,只留
// `reachRight+reachLeft` 等于让两支标签首尾相接 —— 判据上不算重叠,看上去却是黏在
// 一起的一条。真机 `sch clusters --strict`:J1↔D1 只剩 14、D1↔U3 只剩 8,都低于
// 组间该有的 bslPartGap。
func bslFlowGap(crossNets int, reachRight, reachLeft float64) float64 {
	lanes := float64(crossNets)*bslLanePitch + 2*bslPartGap
	return math.Max(bslPartGap, math.Max(lanes, reachRight+reachLeft+bslPartGap))
}

// bslCrossNets 数 a、b 两个角色之间有几条**不同的**内部网 —— 每条网将来都是一根
// 跨接导线,通道宽度按它算。
func bslCrossNets(nets [][]string, a, b string) int {
	n := 0
	for _, net := range nets {
		hasA, hasB := false, false
		for _, m := range net {
			if strings.HasPrefix(m, "PORT:") {
				continue
			}
			r, _, ok := splitBlockPinRef(m)
			if !ok {
				continue
			}
			if r == a {
				hasA = true
			}
			if r == b {
				hasB = true
			}
		}
		if hasA && hasB {
			n++
		}
	}
	return n
}

// bslAttachSide 决定贴上去的件放在宿主的哪一侧,以及它自己该竖放还是横放。
//
// **引脚在左/右列时,件放到宿主的上/下侧,不是同侧**(2026-08-15 用户拍板的 A 方案,
// ADR-0003 §2.5)。旧推导是「引脚在左列 → 该件沿 x 推出去,方向正交天然不撞」——
// 那只比了「该件自己的 marker」与「宿主该脚的 marker」两支,漏了真正的杀手:
// **同侧其它引脚的 marker 会横扫过去**。实测 U3 左侧 6 个 marker 要 276 深的通道,
// 而贴在 V3 脚上的 C7 就坐在通道里,按 L1 组口径是整块重叠(71×85)。
//
// 去耦贴上/下侧不损失电气质量(离脚一样近,PCB 侧照样紧贴),而另外两条路都更贵:
// 让 marker 换到背面撞「背面引出是红线」(fa8f969),让 marker 绕更深是拿版面赔。
// 竖放保持不变 —— 竖放件电上地下,与「power 上 / gnd 下」的画法一致。
//
// 引脚在上/下沿时**仍然同侧**:那一侧本来就是它的引出方向,没有横扫冲突。
// orient 显式声明只决定横竖,不再决定侧向(侧向是几何冲突问题,不是作者偏好)。
func bslAttachSide(pinSide string, orient string) (side string, vertical bool) {
	vertical = orient != "horizontal"
	switch pinSide {
	case "left", "right":
		// 上下由调用方按引脚在宿主里的高低选(bslAttachClearSide),这里给默认。
		return "up", vertical
	case "up", "down":
		return pinSide, orient == "vertical"
	}
	return "up", vertical
}

// bslAttachClearSide 在「上」和「下」之间挑一个:引脚在宿主上半就往上、下半就往下,
// 走最短的那条路。宿主 bbox 高度为 0(读不到)时退回 up。
func bslAttachClearSide(pinY float64, host layoutBBox) string {
	if host.MaxY <= host.MinY {
		return "up"
	}
	if pinY < (host.MinY+host.MaxY)/2 {
		return "down"
	}
	return "up"
}

// bslAttachSeed 算 attach 件的**语义理想中心**。
//
// 上/下侧要**让开宿主本体**(不是从引脚算):引脚在左右列时,它的 y 还在本体高度范围内,
// 从引脚算会把件按在芯片身上。以本体的上/下沿为基准往外推「间隙 + 自身半宽」,x 对齐目标
// 引脚那一列 —— 于是件落在本体正上/正下方、贴着目标脚那一头,读图的人一眼看得出它属于谁。
//
// 左/右侧(引脚本来就在上下沿的情形)仍从引脚算。
//
// **不加 marker 伸出**:attach 表达的是「这两件同网」,中间留出 marker 的空间反而把它推远
// (实测第一版隔了 159,视觉上完全不像"贴")。需要给 marker 让路的是信号流上相邻的两件
// (bslFlowGap 的第三项),不是这里。这只是种子 —— 之后一律再过 findSlotNormalized。
func bslAttachSeed(pinX, pinY float64, host layoutBBox, side string, ownHalf float64) (x, y float64) {
	d := bslPartGap + ownHalf
	switch side {
	case "left":
		return pinX - d, pinY
	case "right":
		return pinX + d, pinY
	case "down":
		return pinX, math.Min(pinY, host.MinY) - d
	default: // up
		return pinX, math.Max(pinY, host.MaxY) + d
	}
}

// bslPairPitch 是并列组的等距步长:成员实测宽度 + max(视觉间隙, 组内最大 marker
// 伸出)。V5(组内同 part)保证这个宽度对全组通用 —— 这正是那条校验是 error
// 而不是 warning 的原因。
func bslPairPitch(memberWidth float64, nets []string) float64 {
	maxReach := 0.0
	for _, n := range nets {
		if r := bslReach(n); r > maxReach {
			maxReach = r
		}
	}
	return memberWidth + math.Max(bslPartGap, maxReach)
}

// splitBlockPinRef 把 "ROLE.PIN" 拆开(与 internal/blocks 的 splitPinRef 同义,
// 这边不引入跨包依赖)。尾部 fanout `*` 由调用方按需剥离。
func splitBlockPinRef(ref string) (role, pin string, ok bool) {
	i := strings.IndexByte(ref, '.')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// bslBlockNets 读块的 internal_nets(求解器只需要拓扑,不需要 ports)。
func bslBlockNets(b blocks.Block) [][]string {
	var doc struct {
		InternalNets [][]string `json:"internal_nets"`
	}
	if err := json.Unmarshal(b.Raw, &doc); err != nil {
		return nil
	}
	return doc.InternalNets
}

// ── 活求解器:放锚件 → 回读实测 → 求解其余件(issue #180 P2 第二步)──────────
//
// 两阶段的理由:attach(去耦贴电源脚)需要**目标引脚的真实坐标**,而引脚坐标只有
// 把宿主件放下去才知道 —— 平台的 place 响应不带几何。所以先落锚件、回读一次、
// 再算其余件。这与 `sch zone relayout` 的 placement-first 同一思路。
//
// 只回读**一次**(锚件之后),不是每件都回读:后续件的自身尺寸用 bapRoleHalfExtent
// 估算(只保证下限),最终由放置后的硬门 verifyBlockLayout 用真实 bbox 兜底。
// 每件多回读一次会让稠密页上的 SDK 往返变成 O(件数 × 页组件数)。

// bslSolved 是一个件求解出来的位姿。
type bslSolved struct {
	Role     string
	X, Y     float64
	Rotation float64
	Source   string // "anchor" | "attach" | "flow" | "pair" | "grid"
}

// bslSolveAround 用锚件的**实测几何**求解其余件的位姿。
//
// obstacles 是页面上已有图元的判定 bbox(含本块已放的锚件);usable 是图纸可用区;
// 每个候选位姿都过 findSlotNormalized,所以「贴脚/顺流/并列」是意图,
// 「不出界、不压别人、判定坐标=落地坐标」是保证。
//
// 求解顺序刻意固定:attach(贴脚,语义最强)→ pair(并列)→ flow(信号流)→
// 其余件保持规划器给的网格坐标。同输入同输出。
func bslSolveAround(
	blk blocks.Block,
	rel bslRelations,
	nets [][]string,
	roleReach map[string]float64, // 角色 → marker 伸出上限(用实例真网名算;nil 退回模板代理名)
	anchorRole string,
	anchorPins map[string]acPin, // "PIN名/编号" → 实测引脚(锚件的)
	anchorBBox layoutBBox,
	obstacles []layoutBBox,
	usable *layoutBBox,
) ([]bslSolved, []string) {

	var out []bslSolved
	var notes []string
	live := append([]layoutBBox(nil), obstacles...)

	half := func(role string) float64 {
		if p, ok := blk.Parts[role]; ok {
			return math.Max(bapRoleHalfExtent(p.Part), bapPartMargin)
		}
		return bapPartMargin
	}
	reachOf := func(role string) float64 {
		if r, ok := roleReach[role]; ok && r > 0 {
			return r
		}
		return bslRoleReach(nets, role) // 模板代理名兜底(纯函数测试走这条)
	}
	partKey := func(role string) string {
		if p, ok := blk.Parts[role]; ok {
			return p.Part
		}
		return ""
	}
	tightHalf := func(role string) float64 { return bslTightHalf(partKey(role)) }
	// 件的估算 box(以中心为原点);放置前只有下限,落地后由硬门兜底。
	boxOf := func(role string) layoutBBox { return bslPartBox(partKey(role)) }
	free := func(cx, cy float64, b layoutBBox) bool {
		cand := layoutBBox{MinX: cx + b.MinX, MinY: cy + b.MinY, MaxX: cx + b.MaxX, MaxY: cy + b.MaxY}
		if usable != nil && !boxInside(cand, *usable) {
			return false
		}
		for _, o := range live {
			if boxesGapOverlap(cand, o, bslPartGap) {
				return false
			}
		}
		return true
	}
	// fitAlong 是**受约束的**避让:只沿关系自己的轴推,另一轴钉死。
	//
	// 这是让布局「像人画的」的关键。第一版用 findSlotNormalized 的环形 8 方向推让,
	// 结果一躲障碍就把件甩到任意方向 —— 实测 flow 的两件 y 差了 220(本该共线)、
	// pair 的两件完全不成对。**关系语义当场被推让破坏**。
	// 受约束推让保证:flow 永远共线、pair 永远等距(躲让也是整数倍 pitch)、
	// attach 永远在目标引脚的那一侧。躲不开就返回 false 交给调用方降级,不硬塞。
	fitAlong := func(role string, seedX, seedY float64, dx, dy, step float64, tries int) (float64, float64, bool) {
		b := boxOf(role)
		for i := 0; i <= tries; i++ {
			cx := snapAnchor(seedX + float64(i)*dx*step)
			cy := snapAnchor(seedY + float64(i)*dy*step)
			if free(cx, cy, b) {
				live = append(live, layoutBBox{MinX: cx + b.MinX, MinY: cy + b.MinY, MaxX: cx + b.MaxX, MaxY: cy + b.MaxY})
				return cx, cy, true
			}
		}
		return 0, 0, false
	}

	// ① attach:贴到锚件的具体引脚上(只处理目标是锚件的;贴到非锚件的留给下一轮
	//    ——本版只回读锚件几何,非锚目标没有实测引脚可用)。
	attachRoles := make([]string, 0, len(rel.Attach))
	for r := range rel.Attach {
		attachRoles = append(attachRoles, r)
	}
	sort.Strings(attachRoles)
	for _, role := range attachRoles {
		target := rel.Attach[role]
		tRole, tPin, ok := splitBlockPinRef(target)
		if !ok || tRole != anchorRole {
			if ok && tRole != anchorRole {
				notes = append(notes, fmt.Sprintf("%s 贴的是非锚件 %s 的脚,本版只按锚件实测求解 —— 该件走网格", role, tRole))
			}
			continue
		}
		pin, found := anchorPins[tPin]
		if !found {
			notes = append(notes, fmt.Sprintf("%s: 锚件上找不到引脚 %q(实测引脚表里没有这个名字/编号)—— 该件走网格", role, tPin))
			continue
		}
		side := outwardDirection(pin)
		if side == "" {
			side = "right"
		}
		side, vertical := bslAttachSide(side, rel.Orient[role])
		if side == "up" || side == "down" { // 上下二选一:走离引脚最近的那一头
			side = bslAttachClearSide(pin.Y, anchorBBox)
		}
		sx, sy := bslAttachSeed(pin.X, pin.Y, anchorBBox, side, tightHalf(role))
		// 躲让方向 = 继续远离引脚,这样它始终待在该脚的那一侧(贴脚语义不破)。
		ax, ay := bslDirVec(side)
		x, y, ok := fitAlong(role, sx, sy, ax, ay, bslPartGap+2*half(role), 6)
		if !ok {
			notes = append(notes, fmt.Sprintf("%s: 贴 %s 的位置放不下(被占或出图纸)—— 该件走网格", role, target))
			continue
		}
		rot := 0.0
		if vertical {
			rot = 270 // 竖放:pin1 朝上(电)、pin2 朝下(地),与 orientation 真值表一致
		}
		out = append(out, bslSolved{Role: role, X: x, Y: y, Rotation: rot, Source: "attach"})
	}

	// ② flow:沿 +x 顺链排,以锚件为基准。锚件左边的逆序向 −x 长,右边的顺序向 +x 长。
	if len(rel.Flow) > 0 {
		anchorIdx := -1
		for i, r := range rel.Flow {
			if r == anchorRole {
				anchorIdx = i
				break
			}
		}
		prevRight := anchorBBox.MaxX
		prevLeft := anchorBBox.MinX
		cy := (anchorBBox.MinY + anchorBBox.MaxY) / 2
		// 间距按**链上相邻的那两件**算,不是一律对着锚件算:J1 挨着的是 D1,
		// 它们之间要留的是 J1 与 D1 两边标签的伸出,与锚件 U3 无关。
		rightPrev, leftPrev := anchorRole, anchorRole
		for i := anchorIdx + 1; i >= 0 && i < len(rel.Flow); i++ {
			role := rel.Flow[i]
			gap := bslFlowGap(bslCrossNets(nets, rightPrev, role), reachOf(rightPrev), reachOf(role))
			seedX := prevRight + gap + tightHalf(role)
			if x, y, ok := fitAlong(role, seedX, cy, 1, 0, gap, 6); ok {
				out = append(out, bslSolved{Role: role, X: x, Y: y, Source: "flow"})
				prevRight, rightPrev = x+tightHalf(role), role
			} else {
				notes = append(notes, fmt.Sprintf("%s: 信号流右侧放不下 —— 该件走网格", role))
			}
		}
		for i := anchorIdx - 1; i >= 0; i-- {
			role := rel.Flow[i]
			gap := bslFlowGap(bslCrossNets(nets, leftPrev, role), reachOf(leftPrev), reachOf(role))
			seedX := prevLeft - gap - tightHalf(role)
			if x, y, ok := fitAlong(role, seedX, cy, -1, 0, gap, 6); ok {
				out = append(out, bslSolved{Role: role, X: x, Y: y, Source: "flow"})
				prevLeft, leftPrev = x-tightHalf(role), role
			} else {
				notes = append(notes, fmt.Sprintf("%s: 信号流左侧放不下 —— 该件走网格", role))
			}
		}
	}

	// ③ pair:等距并列,挂在组内第一个成员下方(第一个成员若已由 attach 定位就贴它)。
	placed := map[string]bslSolved{}
	for _, s := range out {
		placed[s.Role] = s
	}
	for _, group := range rel.Pair {
		if len(group) < 2 {
			continue
		}
		var baseX, baseY float64
		first := group[0]
		if s, ok := placed[first]; ok {
			baseX, baseY = s.X, s.Y
		} else {
			baseX = anchorBBox.MinX - bslFlowGap(0, reachOf(anchorRole), reachOf(first)) - tightHalf(first)
			// 下方也有锚件自己的 marker 往下伸,别只留一个身位。
			baseY = anchorBBox.MinY - reachOf(anchorRole) - bslPartGap - bapPartMargin
			if x, y, ok := fitAlong(first, baseX, baseY, 0, -1, bslPartGap+2*bapPartMargin, 6); ok {
				out = append(out, bslSolved{Role: first, X: x, Y: y, Rotation: 270, Source: "pair"})
				baseX, baseY = x, y
			} else {
				notes = append(notes, fmt.Sprintf("%s: 并列组首件放不下 —— 该组走网格", first))
				continue
			}
		}
		pitch := bslPairPitch(2*tightHalf(first), bslGroupNets(nets, group))
		for i, role := range group[1:] {
			seedX := baseX + float64(i+1)*pitch
			if x, y, ok := fitAlong(role, seedX, baseY, 1, 0, pitch, 4); ok {
				out = append(out, bslSolved{Role: role, X: x, Y: y, Rotation: 270, Source: "pair"})
			} else {
				notes = append(notes, fmt.Sprintf("%s: 并列位放不下 —— 该件走网格", role))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Role < out[j].Role })
	return out, notes
}

// bslRoleReach 是某个角色的 marker 最远能伸出多少 —— 取它沾到的所有网里标签最宽的
// 那条。**flow 间距必须按两边各自的实测标签算**:此前两处调用都传 bslReach("")
// (空网名 = 最窄标签),于是 J1 与 D1 之间只留了最小间距,两边的标签在中间撞成一团
// (`sch clusters` 图元级判据抓到 J1 ↔ D1 重叠 24×11)。网名用与 bslNetOfPins 同一个
// 代理口径(PORT 名优先,否则引脚名),块模板里本来就没有实例化后的网名。
func bslRoleReach(nets [][]string, role string) float64 {
	max := bslReach("")
	for _, net := range nets {
		name, has := "", false
		for _, m := range net {
			if strings.HasPrefix(m, "PORT:") {
				if name == "" {
					name = strings.TrimPrefix(m, "PORT:")
				}
				continue
			}
			r, pin, ok := splitBlockPinRef(m)
			if !ok {
				continue
			}
			if r == role {
				has = true
				if name == "" {
					name = pin
				}
			}
		}
		if !has {
			continue
		}
		if r := bslReach(name); r > max {
			max = r
		}
	}
	return max
}

// bslRoleReachFrom 用**实例化后的真网名**算每个角色的 marker 伸出上限。
//
// 模板阶段只有 "DP1" 这样的引脚名当代理,而落地后的真名是 "C7_N4" —— 标签宽度差的
// 那几个单位,正好是 `sch clusters` 抓到的 J1↔D1 3×11。plan.Nets 里两样都有,
// 到了这一步就没有理由再用代理名。
func bslRoleReachFrom(plan *bapPlan) map[string]float64 {
	out := map[string]float64{}
	for _, n := range plan.Nets {
		r := bslReach(n.Net)
		for _, ref := range n.Roles {
			role, _, ok := splitBlockPinRef(ref)
			if !ok {
				continue
			}
			if r > out[role] {
				out[role] = r
			}
		}
	}
	return out
}

// bslNetOfPins 找同时含 target 引脚与 role 任一脚的那条网的名字(用第一个 PORT:
// 名,没有就用目标引脚名当代理)—— 只为算 marker 标签宽度,不参与电气。
func bslNetOfPins(nets [][]string, target, role string) string {
	tgt := strings.TrimSuffix(target, "*")
	for _, net := range nets {
		hasT, hasR := false, false
		portName := ""
		for _, m := range net {
			if strings.HasPrefix(m, "PORT:") {
				if portName == "" {
					portName = strings.TrimPrefix(m, "PORT:")
				}
				continue
			}
			if strings.TrimSuffix(m, "*") == tgt {
				hasT = true
			}
			if r, _, ok := splitBlockPinRef(m); ok && r == role {
				hasR = true
			}
		}
		if hasT && hasR {
			if portName != "" {
				return portName
			}
			return tgt
		}
	}
	return tgt
}

// bslGroupNets 收集并列组成员沾到的所有网名(算 pitch 时取最长标签)。
func bslGroupNets(nets [][]string, group []string) []string {
	member := map[string]bool{}
	for _, r := range group {
		member[r] = true
	}
	var out []string
	for _, net := range nets {
		name := ""
		hit := false
		for _, m := range net {
			if strings.HasPrefix(m, "PORT:") {
				if name == "" {
					name = strings.TrimPrefix(m, "PORT:")
				}
				continue
			}
			if r, _, ok := splitBlockPinRef(m); ok && member[r] {
				hit = true
				if name == "" {
					name = m
				}
			}
		}
		if hit && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// bslResolveLive 是活求解器的 I/O 外壳:锚件已落地,回读一次页面几何,用锚件的
// **实测引脚**求解其余件,把结果写回 plan.Placements(只改 X/Y/Rotation/Source,
// **绝不重建切片** —— 重建会丢掉 bapRemapDesignators 的结果,导致跨页误连 #144)。
//
// 任何一步失败都只降级为「其余件走网格坐标」+ 一条 warning:关系模板是布局优化,
// 不该因为读不到几何就让整个 apply 失败。
func bslResolveLive(cfg *appConfig, window string, plan *bapPlan, sheet *layoutBBox, stderr io.Writer) (*bslAnchorGeom, []string) {
	blk, ok, err := blocks.Get(plan.BlockID)
	if err != nil || !ok {
		return nil, []string{fmt.Sprintf("关系求解跳过:取不到块 %s(%v)—— 其余件按网格坐标落地", plan.BlockID, err)}
	}
	layout, lerr := blk.SchematicLayout()
	if lerr != nil {
		return nil, []string{"关系求解跳过:模板解析失败 —— 其余件按网格坐标落地"}
	}
	rel, isRel := bslRelationsFrom(layout)
	if !isRel {
		return nil, nil
	}
	anchor := plan.Placements[0]
	if anchor.PrimitiveID == "" {
		return nil, []string{"关系求解跳过:锚件没有 primitiveId —— 其余件按网格坐标落地"}
	}

	res, rerr := requestAction(cfg, "schematic.components.list", window,
		map[string]any{"includeBBox": true, "includePins": true})
	if rerr != nil {
		return nil, []string{fmt.Sprintf("关系求解跳过:回读页面几何失败(%v)—— 其余件按网格坐标落地", rerr)}
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return nil, []string{"关系求解跳过:几何解析失败 —— 其余件按网格坐标落地"}
	}
	scene := buildScene(res.Result)

	// 锚件的实测 bbox。
	var anchorBBox layoutBBox
	var anchorDesig string
	found := false
	for _, c := range comps {
		if c.ID == anchor.PrimitiveID && c.BBox != nil {
			anchorBBox, anchorDesig, found = *c.BBox, label(c), true
			break
		}
	}
	if !found {
		return nil, []string{"关系求解跳过:回读里找不到锚件的实测 bbox —— 其余件按网格坐标落地"}
	}
	// 锚件的实测引脚:名字与编号都建索引(attach 的目标两种写法都该认)。
	pins := map[string]acPin{}
	for _, p := range scene.Pins {
		if !strings.EqualFold(p.Designator, anchorDesig) {
			continue
		}
		if p.PinName != "" {
			pins[p.PinName] = p
		}
		if p.PinNumber != "" {
			pins[p.PinNumber] = p
		}
	}
	if len(pins) == 0 {
		return nil, []string{fmt.Sprintf("关系求解跳过:锚件 %s 读不到引脚 —— 其余件按网格坐标落地", anchorDesig)}
	}

	// 障碍表:页面上除本块未落地件之外的一切(含刚落地的锚件)。
	var obstacles []layoutBBox
	for _, c := range comps {
		if c.BBox == nil || c.ComponentType == "sheet" {
			continue
		}
		obstacles = append(obstacles, markerJudgeBBox(c))
	}
	var usable *layoutBBox
	if sheet != nil {
		usable = &layoutBBox{
			MinX: sheet.MinX + sheetEdgeMinGap, MinY: sheet.MinY + sheetEdgeMinGap,
			MaxX: sheet.MaxX - sheetEdgeMinGap, MaxY: sheet.MaxY - sheetEdgeMinGap,
		}
	}

	solved, notes := bslSolveAround(blk, rel, bslBlockNets(blk), bslRoleReachFrom(plan), plan.AnchorRole,
		pins, anchorBBox, obstacles, usable)

	byRole := map[string]bslSolved{}
	for _, s := range solved {
		byRole[s.Role] = s
	}
	n := 0
	for i := range plan.Placements {
		s, ok := byRole[plan.Placements[i].Role]
		if !ok || plan.Placements[i].PrimitiveID != "" { // 已落地的不动
			continue
		}
		plan.Placements[i].X, plan.Placements[i].Y = s.X, s.Y
		plan.Placements[i].Rotation = s.Rotation
		plan.Placements[i].Source = s.Source
		n++
	}
	fmt.Fprintf(stderr, "relational: 锚 %s 实测 bbox [%.0f,%.0f]-[%.0f,%.0f],%d 个引脚;求解 %d 件\n",
		anchorDesig, anchorBBox.MinX, anchorBBox.MinY, anchorBBox.MaxX, anchorBBox.MaxY, len(pins), n)
	notes = append(notes, bslExpandForMarkers(plan, rel, anchorBBox, pins, obstacles, usable, stderr)...)
	return &bslAnchorGeom{BBox: anchorBBox, Pins: pins}, notes
}

// bslDirVec 把方向名翻成单位向量(y-UP)。
func bslDirVec(side string) (dx, dy float64) {
	switch side {
	case "left":
		return -1, 0
	case "right":
		return 1, 0
	case "down":
		return 0, -1
	default:
		return 0, 1
	}
}

// bslDidSolve reports whether the solver actually consumed the relational
// template. bslResolveLive 的降级路径一律以「关系求解跳过」开头,那时件是按网格
// 坐标落的,关系确实没被执行。(名字不叫 bslSolved —— 那是求解出的位姿类型。)
func bslDidSolve(notes []string) bool {
	for _, n := range notes {
		if strings.HasPrefix(n, "关系求解跳过") {
			return false
		}
	}
	return true
}

// bapDropRelationalLayout removes the schematic_layout.* entries from a
// NOT-applied list — called only when the solver did consume them.
func bapDropRelationalLayout(in []string) []string {
	out := in[:0:0]
	for _, k := range in {
		if strings.HasPrefix(k, "schematic_layout.") {
			continue
		}
		out = append(out, k)
	}
	return out
}

// bslMarkerLanePitch 是同侧两条 marker lane 的间距 —— **直接问 autoconnect 要**,
// 别再自己写一个「同量级」的常量:落点那边的步长现在含网名(laneStepFor),布局这边
// 若还按 46 留,两条 lane 之间就差了一整个名字的宽度,marker 照样压在一起。
// 网名未知时用一个典型长度问价(布局阶段只需要量级,逐个网名的差异由 reach 那一项管)。
func bslMarkerLanePitchFor(net string) float64 { return laneStepFor("netport", net) }

// bslMarkerLanePitch 是没有具体网名时的默认 lane 间距。
var bslMarkerLanePitch = bslMarkerLanePitchFor("NET_1")

// ── marker 通道不够就把器件推开(ADR-0003 时间窗)──────────────────────────────
//
// **这一步发生在 place 之后、connect_pin 之前**:器件已落地(有实测引脚),marker 一根
// 都还没建 —— 挪一个件只是一次 component.modify,零风险。窗口之外挪件要面对
// 「删桩线→相邻共线导线合并→串网」。
//
// 为什么必须是**反馈**而不是前馈:上一版把所有 flow 间距按最坏情况预留(前馈),
// 块从 631 撑到 1000+,J1 直接放不下降级到网格,markerOverlaps 反而 5→10。
// 现在改成先落地、**实测哪一侧真的挤**、只推那一侧 —— 不会全局放大。
//
// 算术很简单,也正是重叠的全部来源:
//
//	该侧要挂 N 个 marker → 需要 N × bslMarkerLanePitch 的深度
//	通道带里的件与锚件的实际空隙 < 需求 → 差多少就让开多少
//	**让开之后与更外侧的件变挤 → 那一件跟着让**(连锁)
//
// 连锁是第一版缺的另一半:实测「把 D1 推开 146」正好把 D1 按在了 J1 身上 ——
// 通道是从另一处的重叠里抢来的,不算腾出来。推让必须整条链一起动,而且宁可
// **整条链一起截短**也不许推一半:半推等于把内侧件按在外侧件身上,制造
// part×part 重叠(layout-lint 的 ERROR),比 marker 挤在一起更严重。
func bslExpandForMarkers(plan *bapPlan, rel bslRelations, anchorBBox layoutBBox,
	pins map[string]acPin, walls []layoutBBox, usable *layoutBBox, stderr io.Writer) []string {

	lanes, count, reach := bslMarkerNeedPerSide(plan, anchorBBox, pins)

	var notes []string
	for _, side := range []string{"left", "right"} {
		cnt := lanes[side]
		if cnt == 0 {
			continue
		}
		want := bslSideDepth(reach[side], cnt)
		// 每一侧都用**当前**坐标重建 unit:左侧推完之后,右侧要看到新位置。
		units := bslPushUnitsOf(plan, rel, func(i int, p bapPlacement) (layoutBBox, bool) {
			b, ok := bslEstimatedBox(i, p)
			if !ok {
				return b, false
			}
			return bslClusterBoxOf(plan, []int{i}, b), true
		})
		allWalls := append(walls, bslAttachWalls(plan, rel)...)
		res := bslPushSolve(units, allWalls, usable, anchorBBox, side, want)
		if res.Head < 0 {
			continue // 这一侧没有别的件,marker 有整片空地
		}
		// 邻居自己的 marker 伸出已经算进它的簇包络里了,这里只补视觉间隙。
		want += bslPartGap
		res = bslPushSolve(units, allWalls, usable, anchorBBox, side, want)
		var detail []string
		for i, m := range res.Move {
			if m == 0 {
				continue
			}
			for _, idx := range units[i].Idx {
				plan.Placements[idx].X += m // 整个 unit 同一个位移:pair 的等距不动
			}
			detail = append(detail, fmt.Sprintf("%s 让 %.0f", units[i].Label, math.Abs(m)))
		}
		got := res.Gap + math.Abs(res.Move[res.Head])
		if len(detail) > 0 {
			fmt.Fprintf(stderr, "relational: %s 侧 %d 支 marker 排 %d 条 lane、需 %.0f,与 %s 只有 %.0f —— %s(通道 → %.0f)\n",
				side, count[side], cnt, want, units[res.Head].Label, res.Gap, strings.Join(detail, "、"), got)
		}
		// 推不满就如实说,并且说清是被谁顶住的 —— 人和 agent 照着这句就知道
		// 下一步该换更大图纸、拆页,还是先挪走那个外部图元。
		if res.Capped != "" {
			notes = append(notes, fmt.Sprintf(
				"%s 侧 %d 支 marker 要 %d 条 lane(需 %.0f),推让后通道只有 %.0f —— 被%s顶住;"+
					"这一块该换更大图纸或拆页", side, count[side], cnt, want, got, res.Capped))
		}
	}
	return notes
}

// bslLiveMove 是实测推让实际下发的一次平移(只动 x —— 推让永远只沿关系自己的轴)。
type bslLiveMove struct {
	Idx         int
	PrimitiveID string
	Designator  string
	FromX, ToX  float64
}

// bslAnchorGeom 是锚件的实测几何(bbox + 引脚),在「锚件落地后回读一次」那一步取得。
//
// **它是整条链上唯一需要引脚的地方**,而锚件此后再不移动(布局以它为基准),所以后面的
// 实测推让只需要 bbox —— 这不是省一次读,是**必须**:带引脚的回读会顺带跑一次 netlist
// 导出,导出之后的 component.modify 会被平台拒掉(见 bslMoveComponentX)。
type bslAnchorGeom struct {
	BBox layoutBBox
	Pins map[string]acPin
}

// bslExpandLive 用**实测 bbox** 把 marker 通道再解一次 —— ADR-0003 时间窗的正用法。
//
// 时机:place 全部件之后、布线前硬门之前。器件已经落地(真实 bbox),marker 一根都还
// 没建,所以挪一个件只是一次 component.modify(实测 10–23ms),没有桩线可被 EasyEDA
// 合并成串网。窗口之外挪件已经三次真机失败。
//
// 为什么落地前那一遍不够(它仍然有用 —— 它决定件**创建**在哪,少一堆 modify):
// 估算与真值差得离谱。`tvs.` 估半宽 10,而 D1 实测 bbox 是 [358,406] —— **锚点根本
// 不在 bbox 中心**,右侧伸出 36;`conn.` 估 90,而 J1 实测只有 35。于是估算版以为
// 通道 275、实际只有 259。这一遍算出来的通道就是渲染出来的通道。
//
// 求解器本体一行不改:bslPushSolve 是纯几何函数,换一批 box 进去就行 —— 这正是
// 当初把它写成纯函数的理由。
func bslExpandLive(cfg *appConfig, window string, plan *bapPlan, anchor *bslAnchorGeom,
	stderr io.Writer) ([]bslLiveMove, []string) {

	if anchor == nil || !plan.Relational || plan.AnchorRole == "" {
		return nil, nil
	}
	blk, ok, err := blocks.Get(plan.BlockID)
	if err != nil || !ok {
		return nil, nil
	}
	layout, lerr := blk.SchematicLayout()
	if lerr != nil {
		return nil, nil
	}
	rel, isRel := bslRelationsFrom(layout)
	if !isRel {
		return nil, nil
	}
	lanes, count, reach := bslMarkerNeedPerSide(plan, anchor.BBox, anchor.Pins)
	if lanes["left"] == 0 && lanes["right"] == 0 {
		return nil, nil
	}

	// **只读 bbox,绝不带 includePins**:带引脚的回读会跑 netlist 导出,而导出之后
	// 发出去的 modify 会被平台拒掉(bslMoveComponentX 记了实测)。引脚只有锚件需要,
	// 那份数据在 anchor 里已经有了。
	res, rerr := requestAction(cfg, "schematic.components.list", window,
		map[string]any{"includeBBox": true})
	if rerr != nil {
		return nil, []string{fmt.Sprintf("实测推让跳过:回读几何失败(%v)—— 布局保持落地时的样子", rerr)}
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return nil, []string{"实测推让跳过:几何解析失败 —— 布局保持落地时的样子"}
	}
	byID := map[string]layoutComp{}
	for _, c := range comps {
		if c.BBox != nil {
			byID[c.ID] = c
		}
	}

	// 墙:页面上一切**推不动**的实测图元 —— 别的块的件,以及本块的 attach 件。
	//
	// attach 件从前既不推**也不当墙**(理由是"它占自己那条脚的 lane")。那条理由错了,
	// 且已按 ADR-0003 §2.5 改掉:去耦现在贴在宿主上/下侧,不在左右通道带里,当墙零代价 ——
	// 万一哪个模板还是把它放进了带里,当墙才能挡住链把它压过去。
	notWall := map[string]bool{} // 锚件(它是基准)+ 会被推的件
	for _, p := range plan.Placements {
		if p.PrimitiveID == "" {
			continue
		}
		if _, isAttach := rel.Attach[p.Role]; !isAttach {
			notWall[p.PrimitiveID] = true
		}
	}
	var walls []layoutBBox
	for _, c := range comps {
		if c.BBox == nil || c.ComponentType == "sheet" || notWall[c.ID] {
			continue
		}
		walls = append(walls, markerJudgeBBox(c))
	}
	var usable *layoutBBox
	if sheet := sheetBBoxOf(comps); sheet != nil {
		usable = &layoutBBox{
			MinX: sheet.MinX + sheetEdgeMinGap, MinY: sheet.MinY + sheetEdgeMinGap,
			MaxX: sheet.MaxX - sheetEdgeMinGap, MaxY: sheet.MaxY - sheetEdgeMinGap,
		}
	}

	// 已下发的位移:实测 bbox 是读那一刻的,挪过之后要按位移平推(件是刚体,x 平移
	// 多少 bbox 就平移多少),否则右侧那一遍会拿着左侧挪之前的旧几何算。
	shift := map[int]float64{}
	liveBox := func(i int, p bapPlacement) (layoutBBox, bool) {
		c, ok := byID[p.PrimitiveID]
		if !ok {
			return layoutBBox{}, false // 没落地/读不回来的件不推 —— 量不到就不动它
		}
		b := bslClusterBoxOf(plan, []int{i}, markerJudgeBBox(c))
		d := shift[i]
		return layoutBBox{MinX: b.MinX + d, MinY: b.MinY, MaxX: b.MaxX + d, MaxY: b.MaxY}, true
	}

	var moves []bslLiveMove
	var notes []string
	for _, side := range []string{"left", "right"} {
		cnt := lanes[side]
		if cnt == 0 {
			continue
		}
		want := bslSideDepth(reach[side], cnt)
		units := bslPushUnitsOf(plan, rel, liveBox)
		res := bslPushSolve(units, walls, usable, anchor.BBox, side, want)
		if res.Head < 0 {
			continue
		}
		// 邻居自己的 marker 伸出已经算进它的簇包络里了,这里只补视觉间隙。
		want += bslPartGap
		res = bslPushSolve(units, walls, usable, anchor.BBox, side, want)
		// **从最外侧往里下发**:每一步之前外侧都已经让开了,于是任何一个中间状态
		// 都不重叠 —— 万一某次 modify 失败,画布停在一个仍然干净的状态上。
		order := make([]int, 0, len(units))
		for i, m := range res.Move {
			if m != 0 {
				order = append(order, i)
			}
		}
		sort.Slice(order, func(a, b int) bool {
			ca := (units[order[a]].Box.MinX + units[order[a]].Box.MaxX) / 2
			cb := (units[order[b]].Box.MinX + units[order[b]].Box.MaxX) / 2
			if side == "left" {
				return ca < cb
			}
			return ca > cb
		})
		var detail []string
		failed := ""
		for _, i := range order {
			m := res.Move[i]
			for _, idx := range units[i].Idx {
				p := &plan.Placements[idx]
				nx := p.X + m
				if err := bslMoveComponentX(cfg, window, p.PrimitiveID, nx, p.Y, stderr); err != nil {
					failed = fmt.Sprintf("%s 挪不动(%v)—— 这一侧到此为止", p.Designator, err)
					break
				}
				moves = append(moves, bslLiveMove{Idx: idx, PrimitiveID: p.PrimitiveID,
					Designator: p.Designator, FromX: p.X, ToX: nx})
				p.X = nx
				shift[idx] += m
			}
			if failed != "" {
				break
			}
			detail = append(detail, fmt.Sprintf("%s 让 %.0f", units[i].Label, math.Abs(m)))
		}
		if len(detail) > 0 {
			got := res.Gap + math.Abs(res.Move[res.Head])
			fmt.Fprintf(stderr, "relational(实测): %s 侧 %d 支 marker 排 %d 条 lane、需 %.0f,与 %s 实测只有 %.0f —— %s(通道 → %.0f)\n",
				side, count[side], cnt, want, units[res.Head].Label, res.Gap, strings.Join(detail, "、"), got)
		}
		if failed != "" {
			notes = append(notes, failed)
			break
		}
		if res.Capped != "" {
			notes = append(notes, fmt.Sprintf(
				"%s 侧 %d 支 marker 要 %d 条 lane(需 %.0f),实测推让后通道只有 %.0f —— 被%s顶住;这一块该换更大图纸或拆页",
				side, count[side], cnt, want, res.Gap+math.Abs(res.Move[res.Head]), res.Capped))
		}
	}
	return moves, notes
}

// bslMoveComponentX 平移一个已落地的件(只动 x/y,不碰属性)。
//
// **平台实测坑(2026-08-15,ceshi)**:紧跟在一次**带引脚的**回读之后发 modify,会在
// 1–7ms 内被平台内部拒掉,errorDetail 是
//
//	Cannot destructure property 'cmdKey' of 'i' as it is undefined.
//
// 复现是确定的:`components.list --include-bbox --include-pins` → modify,8 轮里 4 轮失败;
// 换成 `--include-bbox`(不带引脚)→ modify,10 轮全过;完全不读、连发 10 次 modify 也全过。
// 根因在连接器的引脚分支会调 `sch_ManufactureData.getNetlistFile()` —— **读引脚顺带跑了
// 一次 netlist 导出**,导出把编辑器的命令上下文搅了。所以正解是**排序**(实测推让排在
// 带引脚的硬门回读之前),不是重试;这里留一次短重试只是兜底,并且失败信息要把这条线索
// 带出去,免得下一个人再查一遍。
func bslMoveComponentX(cfg *appConfig, window, primitiveID string, x, y float64, stderr io.Writer) error {
	patch := map[string]any{"primitiveId": primitiveID, "patch": map[string]any{"x": x, "y": y}}
	_, err := requestAction(cfg, "schematic.component.modify", window, patch)
	if err == nil {
		return nil
	}
	time.Sleep(300 * time.Millisecond)
	if _, err2 := requestAction(cfg, "schematic.component.modify", window, patch); err2 != nil {
		return fmt.Errorf("%w(重试一次仍失败;若 errorDetail 是 cmdKey,说明这次 modify 前面跑过带引脚的回读/netlist 导出)", err2)
	}
	fmt.Fprintf(stderr, "note: modify 第一次被平台拒(%v),300ms 后重试成功\n", err)
	return nil
}

// bslUndoLiveMoves 把实测推让下发过的位移原样还原 —— 坐标是我们自己写进去的,
// 还原是精确的(不是"大概挪回去"),这也是这一步敢在硬门之后动画布的前提。
func bslUndoLiveMoves(cfg *appConfig, window string, plan *bapPlan, moves []bslLiveMove, stderr io.Writer) {
	for i := len(moves) - 1; i >= 0; i-- { // 逆序:先还原最后挪的,中间态照样不重叠
		m := moves[i]
		if _, err := requestAction(cfg, "schematic.component.modify", window, map[string]any{
			"primitiveId": m.PrimitiveID,
			"patch":       map[string]any{"x": m.FromX, "y": plan.Placements[m.Idx].Y},
		}); err != nil {
			fmt.Fprintf(stderr, "warn: %s 的位移还原失败(%v)—— 画布停在 %.0f,跑 `sch layout-lint` 确认\n",
				m.Designator, err, m.ToX)
			continue
		}
		plan.Placements[m.Idx].X = m.FromX
	}
}

// bslMarkerLanes 预测一侧会用掉几条 lane —— **不是「几支 marker 就几条」**。
//
// 旧口径是阶梯:同侧每多一支就再深一个 step,6 个脚 = 276 深,而器件本体才 71 宽,
// 簇被标签撑成本体的 6 倍(`sch clusters` 实测 J1 体积 486×292,D1 整个坐在里面)。
// autoconnect 现在只在**标签真的相撞**时才往深里挪(applyLaneStagger),所以 lane 数 =
// y 方向上「同时被覆盖最多的那一层」的重叠数:引脚隔 16、标签高 11 → 共用最浅那条,
// 1 条 lane 就够;而 GND 旗高 21 > 16,相邻两支就得分两条。
//
// 判定与生成必须同一把尺:这里的标签高度来自 predictedMarkerBBox —— 评分器判碰撞用的
// 就是它。
func bslMarkerLanes(spans [][2]float64) int {
	type ev struct {
		at   float64
		open bool
	}
	evs := make([]ev, 0, 2*len(spans))
	for _, s := range spans {
		evs = append(evs, ev{s[0], true}, ev{s[1], false})
	}
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].at != evs[j].at {
			return evs[i].at < evs[j].at
		}
		return !evs[i].open && evs[j].open // 先关后开:恰好相接不算重叠
	})
	cur, max := 0, 0
	for _, e := range evs {
		if e.open {
			cur++
			if cur > max {
				max = cur
			}
		} else {
			cur--
		}
	}
	return max
}

// bslMarkerNeedPerSide 算锚件每一侧需要多深的 marker 通道 —— 返回 lane 数与 marker 数。
// 引脚在锚件中心的哪边就算哪一侧(与 autoconnect 的按侧分配同口径)。
func bslMarkerNeedPerSide(plan *bapPlan, anchorBBox layoutBBox, pins map[string]acPin) (lanes, count map[string]int, reach map[string]float64) {
	cx := (anchorBBox.MinX + anchorBBox.MaxX) / 2
	lanes, count, reach = map[string]int{}, map[string]int{}, map[string]float64{}
	netOf := map[string]string{}
	for _, n := range plan.Nets {
		for _, m := range n.Members {
			netOf[strings.ToUpper(m)] = n.Net
		}
	}
	anchorDesig := ""
	for _, p := range plan.Placements {
		if p.Role == plan.AnchorRole {
			anchorDesig = strings.ToUpper(p.Designator)
		}
	}
	spans := map[string][][2]float64{}
	seen := map[string]bool{}
	for _, pin := range pins {
		if seen[pin.PinNumber] {
			continue // pins 同时按号和名索引,只数一次
		}
		seen[pin.PinNumber] = true
		net, ok := netOf[anchorDesig+":"+strings.ToUpper(pin.PinNumber)]
		if !ok {
			if net, ok = netOf[anchorDesig+":"+strings.ToUpper(pin.PinName)]; !ok {
				if anchorDesig != "" {
					continue // 这个引脚不挂 marker
				}
			}
		}
		side := "right"
		if pin.X < cx {
			side = "left"
		}
		count[side]++
		// 标签占的 y 区间 —— 与评分器判碰撞用同一把尺。
		b := predictedMarkerBBox(pin.X, pin.Y, bapFlagKind(net), side, net)
		spans[side] = append(spans[side], [2]float64{b.MinY, b.MaxY})
		if r := bslReach(net); r > reach[side] {
			reach[side] = r
		}
	}
	for side, s := range spans {
		lanes[side] = bslMarkerLanes(s)
	}
	return lanes, count, reach
}

// bslSideDepth 是一侧的 marker 一共要占多深。
//
// **第一条 lane 的深度 ≠ lane 间距**:一支 marker 从引脚伸出 = 桩长 + 标签实宽
// (bslReach,58–90),而 lane 之间只需要错开一个 body(bslMarkerLanePitch=46)。
// 旧口径拿 lane 数 × 46 当深度,连一支 marker 都装不下 —— 真机上 lane 收窄之后
// markerOverlaps 反而 2→7,就是这里少算了第一条 lane 的伸出。
func bslSideDepth(maxReach float64, lanes int) float64 {
	if lanes <= 0 {
		return 0
	}
	return maxReach + float64(lanes-1)*bslMarkerLanePitch
}

// bslUnitReach 是这个 unit 自己的 marker 会往外伸多远 —— 通道是**两边**的 marker
// 共用的,只留自己那一半必然撞上邻居的标签。网名从 plan.Nets 拿(标签越长伸得越远)。
func bslUnitReach(plan *bapPlan, idx []int) float64 {
	max := 0.0
	for _, i := range idx {
		desig := strings.ToUpper(plan.Placements[i].Designator) + ":"
		for _, n := range plan.Nets {
			for _, m := range n.Members {
				if !strings.HasPrefix(strings.ToUpper(m), desig) {
					continue
				}
				if r := bslReach(n.Net); r > max {
					max = r
				}
			}
		}
	}
	return max
}

// bslPushUnit 是推让链上的一个刚体。
//
// 通常一个 unit 就是一个件;**pair 组必须整体移动** —— 等距并列是电路语义
// (ADR-0003 §3:组内相对位置不是布局的自由度),推让只许平移整组,不许拆散。
type bslPushUnit struct {
	Idx   []int      // plan.Placements 下标(pair 组有多个)
	Box   layoutBBox // 判定 box:全组的并集,绝对坐标,与求解器同一把尺(bslPartBox)
	Label string     // 日志用位号
}

// bslPushResult 是一侧推让的求解结果(纯几何,不含 I/O)。
type bslPushResult struct {
	Move   []float64 // 与 units 下标对齐的位移(带方向号,0 = 不动)
	Head   int       // 通道带里离锚件最近的 unit;-1 = 这一侧空着
	Gap    float64   // Head 推让**前**与锚件的间隙
	Capped string    // 顶住推让的东西;"" = 需求已满足
}

// bslPushUnitsOf 把 plan 里**可以被推**的件拼成 unit 列表。
//
// 三类不进列表:
//   - 锚件 —— 整个布局以它为基准,它一动关系全废;
//   - attach 件 —— 贴脚是它的全部意义(去耦离芯片越近越好),为让 marker 把去耦
//     推走是拿电气质量换版面。它也**不当墙**:它占的是自己那条脚的 lane,不是整侧
//     通道的墙,当墙会让整条链当场失效(实测 C_VCC 就贴在 VCC 脚外 ~30 处);
//
// box 决定「这个件能不能推、判定 box 多大」——两条路径的差别全部收在这一个闭包里:
// 落地**前**用估算 box 且只认还没创建的件(改 plan 坐标即可);落地**后**用实测 bbox
// 且只认已创建的件(推它 = 一次 component.modify)。
func bslPushUnitsOf(plan *bapPlan, rel bslRelations,
	box func(i int, p bapPlacement) (layoutBBox, bool)) []bslPushUnit {
	skip := map[string]bool{}
	if plan.AnchorRole != "" {
		skip[plan.AnchorRole] = true
	}
	for r := range rel.Attach {
		skip[r] = true
	}
	boxes := map[int]layoutBBox{}
	movable := func(i int) bool {
		p := plan.Placements[i]
		if skip[p.Role] {
			return false
		}
		b, ok := box(i, p)
		if ok {
			boxes[i] = b
		}
		return ok
	}
	idxOf := map[string][]int{}
	for i := range plan.Placements {
		idxOf[plan.Placements[i].Role] = append(idxOf[plan.Placements[i].Role], i)
	}
	taken := map[int]bool{}
	var units []bslPushUnit
	for _, group := range rel.Pair { // pair 组先成 unit,整体移动
		var u bslPushUnit
		for _, role := range group {
			for _, i := range idxOf[role] {
				if taken[i] || !movable(i) {
					continue
				}
				u.Idx = append(u.Idx, i)
				taken[i] = true
			}
		}
		if len(u.Idx) > 0 {
			units = append(units, bslUnitGeom(plan, boxes, u))
		}
	}
	for i := range plan.Placements {
		if taken[i] || !movable(i) {
			continue
		}
		taken[i] = true
		units = append(units, bslUnitGeom(plan, boxes, bslPushUnit{Idx: []int{i}}))
	}
	return units
}

// bslAttachWalls 是本块 attach 件的估算 box —— 它们推不动(贴脚是全部意义),所以对推让
// 链来说是墙。落地前它们还没创建,不在页面障碍表里,只能从 plan 的求解结果拿。
func bslAttachWalls(plan *bapPlan, rel bslRelations) []layoutBBox {
	var out []layoutBBox
	for _, p := range plan.Placements {
		if _, isAttach := rel.Attach[p.Role]; !isAttach || p.PrimitiveID != "" {
			continue
		}
		b := bslPartBox(p.PartKey)
		out = append(out, layoutBBox{MinX: p.X + b.MinX, MinY: p.Y + b.MinY, MaxX: p.X + b.MaxX, MaxY: p.Y + b.MaxY})
	}
	return out
}

// bslEstimatedBox 是落地**前**的 box 提供者:估算 box + 只认还没创建的件。
func bslEstimatedBox(i int, p bapPlacement) (layoutBBox, bool) {
	if p.PrimitiveID != "" {
		return layoutBBox{}, false // 已在画布上,改 plan 坐标是静默空转
	}
	b := bslPartBox(p.PartKey)
	return layoutBBox{MinX: p.X + b.MinX, MinY: p.Y + b.MinY, MaxX: p.X + b.MaxX, MaxY: p.Y + b.MaxY}, true
}

// bslClusterBoxOf 把件的 box 撑成**簇包络**:本体 + 它自己的 marker 会往两边伸出多远。
//
// 推让此前作用在**本体**上,而缺陷在**簇**上:body 之间还很宽松,marker 早已顶在一起。
// 真机实证 —— 把 D1 往左推 40 去给 U3 腾通道,body 口径下 D1↔J1 还有很大富余、链不
// 传播,而簇口径下这一推把 J1↔D1 从 14 挤到了 9。判定与生成必须同一把尺:
// `sch clusters` 用簇判,推让就得用簇算。
func bslClusterBoxOf(plan *bapPlan, idx []int, body layoutBBox) layoutBBox {
	r := bslUnitReach(plan, idx)
	return layoutBBox{MinX: body.MinX - r, MinY: body.MinY, MaxX: body.MaxX + r, MaxY: body.MaxY}
}

// bslUnitGeom 补上 unit 的判定 box(成员并集)与日志标签。
func bslUnitGeom(plan *bapPlan, boxes map[int]layoutBBox, u bslPushUnit) bslPushUnit {
	labels := make([]string, 0, len(u.Idx))
	for k, i := range u.Idx {
		abs := boxes[i]
		if k == 0 {
			u.Box = abs
		} else {
			u.Box = layoutBBox{
				MinX: math.Min(u.Box.MinX, abs.MinX), MinY: math.Min(u.Box.MinY, abs.MinY),
				MaxX: math.Max(u.Box.MaxX, abs.MaxX), MaxY: math.Max(u.Box.MaxY, abs.MaxY),
			}
		}
		labels = append(labels, plan.Placements[i].Designator)
	}
	u.Label = strings.Join(labels, "+")
	return u
}

// bslPushSolve 求解一侧的连锁推让 —— 纯几何、无 I/O、同输入同输出。
//
// 这是一个一维约束问题(推让只沿 marker 引出的那根轴,另一轴钉死 —— 环形推让会
// 当场破坏 flow 共线 / pair 等距):
//
//	① 通道需求:通道带里的 unit,离锚件不足 want 的,差多少让多少;
//	② 连锁:内侧 unit 让开 d 之后,外侧 unit 跟着让 d − 自己那段富余
//	   (富余 = 两者现有间隙 − bslPartGap);
//	③ 上限:每个 unit 自己能动的极限 Lₖ(可用区边界 / 推不动的外部图元),
//	   **反推回内侧**(capₖ = min(Lₖ, cap外 + 富余)),于是链上任何一处顶住,
//	   整条链一起截短 —— 绝不出现「内侧推了、外侧没让」的半推重叠。
//
// 因为推让只沿 x、unit 按 x 严格排序,①②是一遍从内到外的松弛,③是一遍从外到内,
// 没有环、不需要迭代到收敛。
func bslPushSolve(units []bslPushUnit, walls []layoutBBox, usable *layoutBBox,
	from layoutBBox, side string, want float64) bslPushResult {

	res := bslPushResult{Move: make([]float64, len(units)), Head: -1, Gap: math.Inf(1)}
	if len(units) == 0 || want <= 0 {
		return res
	}
	// 需求先**向上**取到连接网格:位移落格时是向下取整(不许越过上限),两头都向下
	// 就会系统性地少让 —— 真机上通道停在 204 而需求是 208,那 4 个单位正是这里丢的。
	want = math.Ceil(want/schAnchorGrid) * schAnchorGrid
	dir := 1.0
	if side == "left" {
		dir = -1
	}
	cx := func(b layoutBBox) float64 { return (b.MinX + b.MaxX) / 2 }
	edgeIn := func(b layoutBBox) float64 { // 朝向锚件的那条边
		if dir > 0 {
			return b.MinX
		}
		return b.MaxX
	}
	edgeOut := func(b layoutBBox) float64 { // 背向锚件的那条边
		if dir > 0 {
			return b.MaxX
		}
		return b.MinX
	}
	// gapOf 是 b 位于 a 外侧时的边到边间隙(负数 = 已经压着)。
	gapOf := func(a, b layoutBBox) float64 { return dir * (edgeIn(b) - edgeOut(a)) }
	outward := func(a, b layoutBBox) bool { return dir*(cx(b)-cx(a)) > 0 }
	// 通道带 = 与锚件(或内侧件)同高的一条带。marker 从某侧引出,占的就是这条带;
	// 不在带上的件推了既不腾空间,又破坏关系语义(第一版要推下方的 pair 电阻)。
	bandHit := func(a, b layoutBBox) bool { return a.MinY <= b.MaxY && b.MinY <= a.MaxY }

	order := make([]int, len(units)) // 从内到外,确定性 tie-break
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		ca, cb := dir*cx(units[ia].Box), dir*cx(units[ib].Box)
		if ca != cb {
			return ca < cb
		}
		return ia < ib
	})

	// ① + ② 一遍从内到外:每个 unit 的最小让开量(先不看上限)。
	demand := make([]float64, len(units))
	for k, i := range order {
		u := units[i]
		if !outward(from, u.Box) {
			continue // 在锚件另一侧,与这条通道无关
		}
		m := 0.0
		if bandHit(from, u.Box) {
			if g := gapOf(from, u.Box); g < want {
				m = want - g
			}
		}
		for _, j := range order[:k] {
			v := units[j]
			if demand[j] <= 0 || !bandHit(v.Box, u.Box) || !outward(v.Box, u.Box) {
				continue
			}
			slack := math.Max(0, gapOf(v.Box, u.Box)-bslPartGap)
			if p := demand[j] - slack; p > m {
				m = p
			}
		}
		demand[i] = m
	}

	// Head:通道带里离锚件最近的那个件 —— 通道实际腾出多少按它算。
	for _, i := range order {
		u := units[i]
		if !outward(from, u.Box) || !bandHit(from, u.Box) {
			continue
		}
		if g := gapOf(from, u.Box); g < res.Gap {
			res.Head, res.Gap = i, g
		}
	}
	if res.Head < 0 {
		return res
	}

	// ③ 一遍从外到内:上限,并把顶住的原因往内传。
	limit := make([]float64, len(units))
	why := make([]string, len(units))
	for k := len(order) - 1; k >= 0; k-- {
		i := order[k]
		u := units[i]
		lim, who := math.Inf(1), ""
		if usable != nil {
			b := usable.MaxX - u.Box.MaxX
			if dir < 0 {
				b = u.Box.MinX - usable.MinX
			}
			lim, who = b, u.Label+" 到可用区边界"
		}
		for _, w := range walls {
			if !outward(u.Box, w) || !bandHit(u.Box, w) {
				continue
			}
			if g := gapOf(u.Box, w) - bslPartGap; g < lim {
				lim, who = g, u.Label+" 顶到页面上已有的图元"
			}
		}
		for _, j := range order[k+1:] { // 更外侧的 unit:它推不动,我也就到此为止
			v := units[j]
			if !bandHit(u.Box, v.Box) || !outward(u.Box, v.Box) {
				continue
			}
			slack := math.Max(0, gapOf(u.Box, v.Box)-bslPartGap)
			if l := limit[j] + slack; l < lim {
				lim, who = l, why[j]
			}
		}
		limit[i], why[i] = math.Max(0, lim), who
	}

	for i := range units {
		m := math.Min(demand[i], limit[i])
		// 落到连接网格,且**下取整** —— 宁可少让 4,也不许因为四舍五入越过刚算出的
		// 上限(判定坐标 = 落地坐标)。整格位移也让 pair 组成员平移后仍在格上。
		if m = math.Floor(m/schAnchorGrid) * schAnchorGrid; m <= 0 {
			continue
		}
		res.Move[i] = dir * m
	}
	if demand[res.Head] > 0 && limit[res.Head] < demand[res.Head] {
		res.Capped = why[res.Head]
	}
	return res
}
