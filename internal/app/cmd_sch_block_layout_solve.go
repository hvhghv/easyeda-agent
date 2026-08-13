package app

import (
	"encoding/json"
	"fmt"
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
	d := bslReach(net) + bslPartGap + ownHalf
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
