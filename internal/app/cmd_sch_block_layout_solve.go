package app

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

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
func bslFlowGap(crossNets int, reachRight, reachLeft float64) float64 {
	lanes := float64(crossNets)*bslLanePitch + 2*bslPartGap
	return math.Max(bslPartGap, math.Max(lanes, reachRight+reachLeft))
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

// bslAttachSide 决定贴上去的件放在目标引脚的哪一侧,以及它自己该竖放还是横放。
//
// 侧向来自**实测引脚相对宿主 bbox 中心的位置**(outwardDirection),不猜:
//   - 引脚在左/右列 → 该件**竖放**,沿 x 推出去;竖放件自己的两个 marker 一上一下
//     (电上地下),而宿主引脚的 marker 朝左/右 —— **方向正交,天然不撞**。
//     这是「贴脚不撞标签」的机制性理由,不是运气。
//   - 引脚在上/下沿 → **横放**,沿 y 推出去。
//
// orient 显式声明时以它为准(作者的意图优先于推导)。
func bslAttachSide(pinSide string, orient string) (side string, vertical bool) {
	switch orient {
	case "vertical":
		return pinSide, true
	case "horizontal":
		return pinSide, false
	}
	switch pinSide {
	case "left", "right":
		return pinSide, true
	case "up", "down":
		return pinSide, false
	}
	return "right", true
}

// bslAttachSeed 算 attach 件的**语义理想中心**:从目标引脚沿 side 推出去
// 「marker 伸出 + 间隙 + 自身半宽」。这只是种子 —— 之后一律再过
// findSlotNormalized,所以「贴脚」是意图,「不出界/不压人」是保证。
func bslAttachSeed(pinX, pinY float64, side string, net string, ownHalf float64) (x, y float64) {
	// **不加 marker 伸出**:attach 表达的是「这两件同网直连」(V4 校验保证了这一点),
	// 它们之间画一根线,不各挂一个网络标签 —— 去耦电容就该紧贴电源脚,中间留出
	// marker 的空间反而把它推远(实测第一版隔了 159,视觉上完全不像"贴")。
	// 需要给 marker 让路的是**信号流上相邻的两件**(bslFlowGap 的第三项),不是这里。
	_ = net
	d := bslPartGap + ownHalf
	switch side {
	case "left":
		return pinX - d, pinY
	case "right":
		return pinX + d, pinY
	case "down":
		return pinX, pinY - d
	default: // up
		return pinX, pinY + d
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
	// 贴脚用**更紧的**半宽:bapRoleHalfExtent 刻意「只高不低」(fallback 网格的
	// 件间距靠它兜底),但贴脚要的是紧密 —— 用 50 会把 0402 去耦推到离芯片 79 远,
	// 视觉上完全不像"贴"。分立件的 10 是 ceshi 实测值(0402 符号 20×16),不是估的。
	tightHalf := func(role string) float64 {
		if p, ok := blk.Parts[role]; ok {
			k := strings.ToLower(p.Part)
			for _, pre := range []string{"cap.", "res.", "ind.", "diode.", "led.", "tvs.", "esd.", "bjt.", "mos."} {
				if strings.HasPrefix(k, pre) {
					return 10
				}
			}
			return bapRoleHalfExtent(p.Part)
		}
		return bapPartMargin
	}
	// 件的估算 box(以中心为原点);放置前只有下限,落地后由硬门兜底。
	//
	// **必须与种子距离用同一把尺**:第一版 seed 用紧凑半宽(10)、box 用保守半宽(50),
	// 于是贴脚的件被判成"撞上锚件"而降级走网格(实测 C8/R5 都中招)。判定与生成
	// 同源是本项目反复吃亏的那条定律。
	boxOf := func(role string) layoutBBox {
		h := tightHalf(role)
		v := math.Min(h, bapPartMargin)
		return layoutBBox{MinX: -h, MinY: -v, MaxX: h, MaxY: v}
	}
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
		net := bslNetOfPins(nets, target, role)
		sx, sy := bslAttachSeed(pin.X, pin.Y, side, net, tightHalf(role))
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
		for i := anchorIdx + 1; i >= 0 && i < len(rel.Flow); i++ {
			role := rel.Flow[i]
			gap := bslFlowGap(bslCrossNets(nets, anchorRole, role), bslReach(""), bslReach(""))
			seedX := prevRight + gap + tightHalf(role)
			if x, y, ok := fitAlong(role, seedX, cy, 1, 0, gap, 6); ok {
				out = append(out, bslSolved{Role: role, X: x, Y: y, Source: "flow"})
				prevRight = x + tightHalf(role)
			} else {
				notes = append(notes, fmt.Sprintf("%s: 信号流右侧放不下 —— 该件走网格", role))
			}
		}
		for i := anchorIdx - 1; i >= 0; i-- {
			role := rel.Flow[i]
			gap := bslFlowGap(bslCrossNets(nets, anchorRole, role), bslReach(""), bslReach(""))
			seedX := prevLeft - gap - tightHalf(role)
			if x, y, ok := fitAlong(role, seedX, cy, -1, 0, gap, 6); ok {
				out = append(out, bslSolved{Role: role, X: x, Y: y, Source: "flow"})
				prevLeft = x - tightHalf(role)
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
			baseX = anchorBBox.MinX - bslFlowGap(0, bslReach(""), bslReach("")) - tightHalf(first)
			baseY = anchorBBox.MinY - bslPartGap - bapPartMargin
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
func bslResolveLive(cfg *appConfig, window string, plan *bapPlan, sheet *layoutBBox, stderr io.Writer) []string {
	blk, ok, err := blocks.Get(plan.BlockID)
	if err != nil || !ok {
		return []string{fmt.Sprintf("关系求解跳过:取不到块 %s(%v)—— 其余件按网格坐标落地", plan.BlockID, err)}
	}
	layout, lerr := blk.SchematicLayout()
	if lerr != nil {
		return []string{"关系求解跳过:模板解析失败 —— 其余件按网格坐标落地"}
	}
	rel, isRel := bslRelationsFrom(layout)
	if !isRel {
		return nil
	}
	anchor := plan.Placements[0]
	if anchor.PrimitiveID == "" {
		return []string{"关系求解跳过:锚件没有 primitiveId —— 其余件按网格坐标落地"}
	}

	res, rerr := requestAction(cfg, "schematic.components.list", window,
		map[string]any{"includeBBox": true, "includePins": true})
	if rerr != nil {
		return []string{fmt.Sprintf("关系求解跳过:回读页面几何失败(%v)—— 其余件按网格坐标落地", rerr)}
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return []string{"关系求解跳过:几何解析失败 —— 其余件按网格坐标落地"}
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
		return []string{"关系求解跳过:回读里找不到锚件的实测 bbox —— 其余件按网格坐标落地"}
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
		return []string{fmt.Sprintf("关系求解跳过:锚件 %s 读不到引脚 —— 其余件按网格坐标落地", anchorDesig)}
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

	solved, notes := bslSolveAround(blk, rel, bslBlockNets(blk), plan.AnchorRole,
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
	return notes
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
