package app

// cmd_sch_zone_tidy.go — `sch zone-tidy`(设计契约 docs/schematic-layout-hierarchy.md §3)。
//
// 组间叠加布局:把一个功能区(`sch zones` claim)内的每个持久化组 —— 以及未入组的
// 认领散件(临时单件组,同 zone-move 语义)—— 当作刚体,在该区的区带(band)内重新
// 排布:
//
//   - 锚组 = 含最大 bbox 器件的组,不动(bbox 已落带内)或贴带内基准位(带左上);
//   - 其余组按 bbox 面积降序放置:优先与锚组垂直堆叠(上下布局,用户点名),行内
//     水平排(间距 ≥ hGap,默认 117 = 两个相向水平 netport 标签实测最小距),行满
//     换行(行距 ≥ vGap,默认 40);
//   - 所有组 bbox 必须互不重叠且完整落在 band 内;装不下时返回结构化诊断(需要的
//     最小 band 尺寸),绝不硬塞;
//   - 每组 {dx,dy} 吸附到 5 单位连接网格(schAnchorGrid):x 向 ceil、y 向 floor,
//     即吸附只会「拉大」间距、永不越界,组内成员的既有网格对齐整体保留;
//   - 确定性:同输入同输出(锚选择与放置排序都是全序,与输入顺序无关)。
//
// band 来源:优先 zone-plan 对应分区 rect(该分区必须只含本区一个模块,并扣掉顶部
// title band);取不到时降级为区内现有内容 bbox 外扩 zoneTidyBandPad(stderr 报告)。
//
// --apply 执行(契约铁则):对每个非零 {dx,dy} 的组走一次完整 group-move 集合派发
// (持久化组复用 expandSchGroupForMove;散件走同语义的单件展开),展开后对计划期
// 标记的双认领桩线/旗做差集过滤(zoneTidySubtractShared —— 执行与计划的「原地
// 不动」承诺一致),组间 settle 350ms(铁则 2:上一组 recreate 后立即展开下一组会
// churn);完成后 layout-lint + bridge-check 自检(铁则 5),红则按逆序把已移组移回
// (-dx,-dy)。回滚本身不重跑自检(固有语义限制,见 applyZoneTidy),完成后需人工
// `sch layout-lint` + `sch check` 复核。
//
// 共享依赖只读复用:expandSchGroupForMove / fetchSchWirePolylinesStable /
// expandGroupAttachments(cmd_sch_group.go)、loadSchZoneClaimsForPage
// (cmd_sch_zones.go)、computePartitionPlan / bboxContains / strInSlice
// (cmd_sch_zone_plan.go)、collectLayoutLint / parseBridgeReport(lint/bridge)。
// 本文件不修改任何共享文件;小 helper 一律私有实现。

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ── 契约常量 ────────────────────────────────────────────────────────────────

const (
	// zoneTidyHGapDefault:水平相邻组 bbox 最小间距(契约 §3:两个相向水平
	// netport 标签实测最小距,实测 2026-08-12)。
	zoneTidyHGapDefault = 117.0
	// zoneTidyVGapDefault:垂直相邻组 bbox 最小间距(契约 §3)。
	zoneTidyVGapDefault = 40.0
	// zoneTidyBandPad:band 降级(zone-plan 分区取不到 → 区内内容 bbox 外扩)的
	// pad,取 zone-plan 的 partitionContentPad 同值(24)保持视觉一致。
	zoneTidyBandPad = 24.0
	// zoneTidySettle:组间 settle(铁则 2)。fetchSchWirePolylinesStable 已经用
	// double-read 挡 churn,但 group-move recreate 桩线后立即展开下一组仍可能吃
	// 到 mid-churn 快照,所以组与组之间再加一拍。
	zoneTidySettle = 350 * time.Millisecond
)

// zonePackGridSnap:dx/dy 吸附网格 = 5 单位连接网格(schAnchorGrid,
// cmd_sch_autolayout.go)。整组平移量吸附到网格上,组内成员的既有网格对齐才不会
// 被 tidy 破坏(layout-lint off-grid WARN)。
const zonePackGridSnap = float64(schAnchorGrid)

// zonePackEps 吸收浮点尾巴(坐标都在 0.01inch 网格上,1e-6 远小于任何真实间隙)。
const zonePackEps = 1e-6

// ── planZonePack:核心纯函数(表驱动单测) ──────────────────────────────────

// zonePackGroup 是一个刚体输入:组(或散件的临时单件组)的全集 bbox
// (= expandSchGroupForMove 展开集的 bbox 并集)。
// Bucket 是形态桶(0=竖放,1=横放,按 bbox 纵横比推):行排时**同桶同行**——
// 竖放去耦(上电下地)与横放信号链(netport 只能水平)混排一行,顶对齐后高矮
// 参差,视觉上"横一个竖一个不规范"(用户点名);桶切换强制换行,竖的一排、
// 横的一排。全 0(如 sheet 层)时行为不变。
type zonePackGroup struct {
	ID       string
	BBox     layoutBBox
	IsAnchor bool
	Bucket   int
}

// zonePackMove 是一个组的刚移增量(已吸附 zonePackGridSnap 网格)。
type zonePackMove struct {
	ID     string  `json:"id"`
	DX     float64 `json:"dx"`
	DY     float64 `json:"dy"`
	Anchor bool    `json:"anchor,omitempty"`
}

// zonePackDiag 是「装不下」的结构化诊断:报告需要的最小 band 尺寸,不硬塞。
type zonePackDiag struct {
	Reason string  `json:"reason"`
	BandW  float64 `json:"bandW"`
	BandH  float64 `json:"bandH"`
	NeedW  float64 `json:"needW"`
	NeedH  float64 `json:"needH"`
}

// zonePackPlan 是 planZonePack 的输出:Fits 时 Moves 覆盖每个组(锚在首位),
// 否则 Diag 非空。
type zonePackPlan struct {
	Fits  bool           `json:"fits"`
	Moves []zonePackMove `json:"moves,omitempty"`
	Diag  *zonePackDiag  `json:"diag,omitempty"`
}

func zonePackArea(b layoutBBox) float64 { return (b.MaxX - b.MinX) * (b.MaxY - b.MinY) }

func zonePackOffset(b layoutBBox, dx, dy float64) layoutBBox {
	return layoutBBox{MinX: b.MinX + dx, MinY: b.MinY + dy, MaxX: b.MaxX + dx, MaxY: b.MaxY + dy}
}

// zonePackCeilGrid / zonePackFloorGrid:方向性网格吸附。x 用 ceil(目标 MinX 只
// 会更靠右 → 与左邻的间距只增不减),y 用 floor(目标 MaxY 只会更低 → 与上一行的
// 间距只增不减)。eps 防止恰在网格上的值被浮点尾巴推走一格。
func zonePackCeilGrid(v float64) float64 {
	return math.Ceil(v/zonePackGridSnap-zonePackEps) * zonePackGridSnap
}

func zonePackFloorGrid(v float64) float64 {
	return math.Floor(v/zonePackGridSnap+zonePackEps) * zonePackGridSnap
}

// zonePackBeats 报告 a 是否应取代 b 当锚:bbox 面积大者优先,ID 升序破平 ——
// 全序,与输入顺序无关(确定性)。
func zonePackBeats(a, b zonePackGroup) bool {
	aa, ab := zonePackArea(a.BBox), zonePackArea(b.BBox)
	if aa != ab {
		return aa > ab
	}
	return a.ID < b.ID
}

// pickZonePackAnchor 选锚:显式 IsAnchor 优先(多个时按 zonePackBeats),否则
// 全体按 zonePackBeats(即最大 bbox 组)。
func pickZonePackAnchor(groups []zonePackGroup) int {
	best := -1
	bestFlagged := false
	for i, g := range groups {
		switch {
		case best == -1:
			best, bestFlagged = i, g.IsAnchor
		case g.IsAnchor && !bestFlagged:
			best, bestFlagged = i, true
		case g.IsAnchor == bestFlagged:
			if zonePackBeats(g, groups[best]) {
				best = i
			}
		}
	}
	return best
}

// packRowsInto:纯 shelf 行排(无锚语义):others 依序放入 region,从 region 顶
// 部第一行起左→右,行满换行向下;任何一组放不下即 ok=false(不硬塞)。
// obs 是区域内的禁放障碍(如图签):落位与障碍相交时 cursor 右跳到障碍右缘重试,
// 右跳出界则换行——障碍只挡它真实覆盖的角落,不再整条带让位(L 形可用区)。
// packZoneRows(锚下)/ packZoneRowsBeside(锚侧)/ planSheetTidy(纸面)共用。
func packRowsInto(others []zonePackGroup, region layoutBBox, obs []layoutBBox, hGap, vGap float64) ([]zonePackMove, bool) {
	moves := make([]zonePackMove, 0, len(others))
	rowTop := region.MaxY
	rowLow := rowTop
	cursor := region.MinX
	firstInRow := true
	lastBucket := -1
	for _, g := range others {
		// 形态桶切换强制换行(竖放组一排、横放组一排;全同桶时无感)。
		if !firstInRow && lastBucket >= 0 && g.Bucket != lastBucket {
			rowTop = rowLow - vGap
			rowLow = rowTop
			cursor = region.MinX
			firstInRow = true
		}
		lastBucket = g.Bucket
		placed := false
		for !placed {
			dx := zonePackCeilGrid(cursor - g.BBox.MinX)
			eff := zonePackOffset(g.BBox, dx, 0)
			if eff.MaxX > region.MaxX+zonePackEps {
				if firstInRow && cursor <= region.MinX+zonePackEps {
					return nil, false // 行首无障碍跳位仍放不下 = 单组比区域还宽,无解
				}
				// 行满(或行首被障碍右跳挤爆)换行;空行连换由 MinY 触底终止
				rowTop = rowLow - vGap
				rowLow = rowTop
				cursor = region.MinX
				firstInRow = true
				continue
			}
			dy := zonePackFloorGrid(rowTop - g.BBox.MaxY)
			eff = zonePackOffset(g.BBox, dx, dy)
			if eff.MinY < region.MinY-zonePackEps {
				return nil, false // 纵向溢出区域底
			}
			hit := false
			for _, o := range obs {
				if boxesOverlap(eff, o) {
					cursor = o.MaxX + hGap // 右跳避障;出界由上方 MaxX 检查转换行
					hit = true
					break
				}
			}
			if hit {
				continue
			}
			moves = append(moves, zonePackMove{ID: g.ID, DX: dx, DY: dy})
			cursor = eff.MaxX + hGap
			if eff.MinY < rowLow {
				rowLow = eff.MinY
			}
			firstInRow = false
			placed = true
		}
	}
	return moves, true
}

// packZoneRows 在 band 内做一次锚定 shelf 打包:锚固定在 (anchorDX,anchorDY)
// 偏移处(须完整落带),其余组从锚正下方一行开始左→右放置,行满换行继续向下。
// 返回 (moves, ok);任何一组放不下即 ok=false(不硬塞)。
func packZoneRows(anchor zonePackGroup, anchorDX, anchorDY float64, others []zonePackGroup, band layoutBBox, hGap, vGap float64) ([]zonePackMove, bool) {
	effA := zonePackOffset(anchor.BBox, anchorDX, anchorDY)
	if !bboxContains(band, effA) {
		return nil, false
	}
	// 行区域 = 锚正下方(优先与锚组垂直堆叠——上下布局,用户点名)。
	region := layoutBBox{MinX: band.MinX, MinY: band.MinY, MaxX: band.MaxX, MaxY: effA.MinY - vGap}
	rows, ok := packRowsInto(others, region, nil, hGap, vGap)
	if !ok {
		return nil, false
	}
	moves := make([]zonePackMove, 0, len(others)+1)
	moves = append(moves, zonePackMove{ID: anchor.ID, DX: anchorDX, DY: anchorDY, Anchor: true})
	return append(moves, rows...), true
}

// packZoneRowsBeside(策略 B):锚固定在 (anchorDX,anchorDY),其余组行排到锚
// **右侧**子带(x: 锚右缘+hGap → band 右缘,y: band 全高)。锚下行排对「超高锚」
// (IC 高度 ≈ 带高,如 40 脚模组)结构性无解——去耦/外围本就该贴芯片侧,这是
// 电气与几何双正确的第二形状。planZonePack 先试锚下(A)再试锚侧(B)。
func packZoneRowsBeside(anchor zonePackGroup, anchorDX, anchorDY float64, others []zonePackGroup, band layoutBBox, hGap, vGap float64) ([]zonePackMove, bool) {
	effA := zonePackOffset(anchor.BBox, anchorDX, anchorDY)
	if !bboxContains(band, effA) {
		return nil, false
	}
	region := layoutBBox{MinX: effA.MaxX + hGap, MinY: band.MinY, MaxX: band.MaxX, MaxY: band.MaxY}
	if region.MaxX <= region.MinX {
		return nil, false
	}
	rows, ok := packRowsInto(others, region, nil, hGap, vGap)
	if !ok {
		return nil, false
	}
	moves := make([]zonePackMove, 0, len(others)+1)
	moves = append(moves, zonePackMove{ID: anchor.ID, DX: anchorDX, DY: anchorDY, Anchor: true})
	return append(moves, rows...), true
}

// zonePackValidate 是契约不变量的机械终验:每组恰有一个 move、落位后 bbox 完整
// 在 band 内、且两两不重叠。planZonePack 在返回前自验;测试直接调用。
func zonePackValidate(groups []zonePackGroup, moves []zonePackMove, band layoutBBox) error {
	byID := map[string]zonePackMove{}
	for _, m := range moves {
		byID[m.ID] = m
	}
	eff := make([]layoutBBox, len(groups))
	for i, g := range groups {
		m, ok := byID[g.ID]
		if !ok {
			return fmt.Errorf("group %s has no move", g.ID)
		}
		eff[i] = zonePackOffset(g.BBox, m.DX, m.DY)
		if !bboxContains(band, eff[i]) {
			return fmt.Errorf("group %s lands outside the band", g.ID)
		}
	}
	for i := range eff {
		for j := i + 1; j < len(eff); j++ {
			if boxesOverlap(eff[i], eff[j]) {
				return fmt.Errorf("groups %s and %s overlap after the pack", groups[i].ID, groups[j].ID)
			}
		}
	}
	return nil
}

// zonePackNeeded 计算装得下所需的最小 band 尺寸(诊断用):宽 = max(带宽, 最宽组
// + 一格吸附余量),高 = 同宽度、锚贴左上、带底无限深时打包实际用掉的高度。
func zonePackNeeded(anchor zonePackGroup, others []zonePackGroup, band layoutBBox, hGap, vGap float64) (needW, needH float64) {
	maxW := anchor.BBox.MaxX - anchor.BBox.MinX
	for _, g := range others {
		if w := g.BBox.MaxX - g.BBox.MinX; w > maxW {
			maxW = w
		}
	}
	needW = maxW + zonePackGridSnap // 行首 ceil 吸附最多右漂一格内
	if bw := band.MaxX - band.MinX; bw > needW {
		needW = bw
	}
	virtual := layoutBBox{MinX: band.MinX, MinY: band.MinY - 1e15, MaxX: band.MinX + needW, MaxY: band.MaxY}
	sdx := zonePackCeilGrid(virtual.MinX - anchor.BBox.MinX)
	sdy := zonePackFloorGrid(virtual.MaxY - anchor.BBox.MaxY)
	moves, ok := packZoneRows(anchor, sdx, sdy, others, virtual, hGap, vGap)
	if !ok {
		// 不应发生(宽度已保证);兜底给一个纯堆叠上界。
		total := anchor.BBox.MaxY - anchor.BBox.MinY
		for _, g := range others {
			total += vGap + (g.BBox.MaxY - g.BBox.MinY)
		}
		return needW, total + zonePackGridSnap
	}
	bboxByID := map[string]layoutBBox{anchor.ID: anchor.BBox}
	for _, g := range others {
		bboxByID[g.ID] = g.BBox
	}
	lowest := virtual.MaxY
	for _, m := range moves {
		if eff := zonePackOffset(bboxByID[m.ID], m.DX, m.DY); eff.MinY < lowest {
			lowest = eff.MinY
		}
	}
	return needW, band.MaxY - lowest
}

// planZonePack 是组间叠加布局的核心纯函数(契约 §3):锚组不动或贴带内基准位,
// 其余组面积降序、锚下行排、行满换行;全部组 bbox 互不重叠且落在 band 内;无解
// 返回结构化诊断(需要的最小 band 尺寸);同输入同输出。
func planZonePack(groups []zonePackGroup, band layoutBBox, hGap, vGap float64) zonePackPlan {
	if len(groups) == 0 {
		return zonePackPlan{Fits: true}
	}
	anchorIdx := pickZonePackAnchor(groups)
	anchor := groups[anchorIdx]
	others := make([]zonePackGroup, 0, len(groups)-1)
	for i, g := range groups {
		if i != anchorIdx {
			others = append(others, g)
		}
	}
	// 形态桶升序(竖放去耦排前,横放信号链其后)→ 面积降序 → 宽降序 → ID 升
	// 序:全序,重排输入不改输出(确定性)。桶结合 packRowsInto 的桶切换换行,
	// 竖的一排横的一排,不再混排参差。
	sort.Slice(others, func(i, j int) bool {
		if others[i].Bucket != others[j].Bucket {
			return others[i].Bucket < others[j].Bucket
		}
		ai, aj := zonePackArea(others[i].BBox), zonePackArea(others[j].BBox)
		if ai != aj {
			return ai > aj
		}
		wi, wj := others[i].BBox.MaxX-others[i].BBox.MinX, others[j].BBox.MaxX-others[j].BBox.MinX
		if wi != wj {
			return wi > wj
		}
		return others[i].ID < others[j].ID
	})

	// 候选 1:锚不动(bbox 已完整落带内,dx=dy=0 无吸附漂移);候选 2:锚贴带内
	// 基准位(带左上,吸附网格)。先 1 后 2,失败才降级 —— 确定性。
	type anchorCand struct{ dx, dy float64 }
	var cands []anchorCand
	if bboxContains(band, anchor.BBox) {
		cands = append(cands, anchorCand{0, 0})
	}
	sdx := zonePackCeilGrid(band.MinX - anchor.BBox.MinX)
	sdy := zonePackFloorGrid(band.MaxY - anchor.BBox.MaxY)
	if len(cands) == 0 || sdx != 0 || sdy != 0 {
		cands = append(cands, anchorCand{sdx, sdy})
	}
	for _, c := range cands {
		if moves, ok := packZoneRows(anchor, c.dx, c.dy, others, band, hGap, vGap); ok {
			if err := zonePackValidate(groups, moves, band); err == nil {
				return zonePackPlan{Fits: true, Moves: moves}
			}
		}
		// 策略 B:锚下装不下(典型:超高模组锚吃满带高)→ 其余组行排到锚右侧。
		if moves, ok := packZoneRowsBeside(anchor, c.dx, c.dy, others, band, hGap, vGap); ok {
			if err := zonePackValidate(groups, moves, band); err == nil {
				return zonePackPlan{Fits: true, Moves: moves}
			}
		}
	}
	needW, needH := zonePackNeeded(anchor, others, band, hGap, vGap)
	bandW, bandH := band.MaxX-band.MinX, band.MaxY-band.MinY
	return zonePackPlan{Diag: &zonePackDiag{
		Reason: fmt.Sprintf("band %.0f×%.0f cannot hold %d group(s) at hGap=%g/vGap=%g — need at least %.0f×%.0f",
			bandW, bandH, len(groups), hGap, vGap, needW, needH),
		BandW: round2(bandW), BandH: round2(bandH),
		NeedW: round2(needW), NeedH: round2(needH),
	}}
}

// ── 区内成员 → 刚体单元(纯函数) ────────────────────────────────────────────

// zoneTidyUnit 是区内的一个刚体单元:持久化组,或散件的临时单件组。
type zoneTidyUnit struct {
	Ref     string   `json:"ref"`            // 组 id(g1)或散件位号
	Name    string   `json:"name,omitempty"` // 组名(散件为空)
	Members []string `json:"members"`        // 位号(大写)
	IsGroup bool     `json:"isGroup"`
}

// zoneTidyUnits 把区认领的位号切成刚体单元:全员被认领的组整组进入;部分成员在
// 区内、部分在区外的组 = 跨区组 = 配置错误,报错拒绝(契约数据模型);成员全不在
// 区内的组忽略;未入组的认领件按临时单件组进入(排序稳定:组按传入顺序,散件按
// 位号升序)。
func zoneTidyUnits(claimed []string, groups []*schGroup) ([]zoneTidyUnit, error) {
	claimedNorm := normalizeDesignators(claimed)
	claimSet := map[string]bool{}
	for _, d := range claimedNorm {
		claimSet[d] = true
	}
	grouped := map[string]bool{}
	var units []zoneTidyUnit
	for _, g := range groups {
		if g == nil || len(g.Members) == 0 {
			continue
		}
		var in, out []string
		for _, m := range g.Members {
			m = strings.ToUpper(strings.TrimSpace(m))
			if claimSet[m] {
				in = append(in, m)
			} else {
				out = append(out, m)
			}
		}
		if len(in) == 0 {
			continue
		}
		if len(out) > 0 {
			return nil, fmt.Errorf("group %s straddles the zone boundary — member(s) %s are claimed by this zone but %s are not; a cross-zone group is a configuration error (fix the claims with `sch zones set`, or split the group with `sch group remove`)",
				describeSchGroup(g), strings.Join(in, ","), strings.Join(out, ","))
		}
		for _, m := range in {
			grouped[m] = true
		}
		units = append(units, zoneTidyUnit{Ref: g.ID, Name: g.Name, Members: append([]string(nil), in...), IsGroup: true})
	}
	for _, d := range claimedNorm { // normalizeDesignators 已排序 → 散件顺序稳定
		if !grouped[d] {
			units = append(units, zoneTidyUnit{Ref: d, Members: []string{d}})
		}
	}
	return units, nil
}

// zoneTidyAnchorRef 返回「含最大 bbox 器件」的单元 Ref(契约:锚组);面积平手按
// 位号升序破平;区内无可测面积时返回 ""(planZonePack 退化为最大组 bbox 当锚)。
func zoneTidyAnchorRef(units []zoneTidyUnit, areaByDesig map[string]float64) string {
	bestRef, bestDesig, bestArea := "", "", -1.0
	for _, u := range units {
		for _, m := range u.Members {
			a, ok := areaByDesig[m]
			if !ok {
				continue
			}
			if a > bestArea || (a == bestArea && m < bestDesig) {
				bestArea, bestDesig, bestRef = a, m, u.Ref
			}
		}
	}
	return bestRef
}

// zoneTidyUnionBBox 求刚体单元全集 bbox:成员 bbox ∪ 附着桩线折线点 ∪ 附着旗锚点。
func zoneTidyUnionBBox(boxes []layoutBBox, wirePolys [][]float64, points [][2]float64) (layoutBBox, bool) {
	var u layoutBBox
	has := false
	add := func(minX, minY, maxX, maxY float64) {
		if !has {
			u = layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
			has = true
			return
		}
		u.MinX = math.Min(u.MinX, minX)
		u.MinY = math.Min(u.MinY, minY)
		u.MaxX = math.Max(u.MaxX, maxX)
		u.MaxY = math.Max(u.MaxY, maxY)
	}
	for _, b := range boxes {
		add(b.MinX, b.MinY, b.MaxX, b.MaxY)
	}
	for _, p := range wirePolys {
		for i := 0; i+1 < len(p); i += 2 {
			add(p[i], p[i+1], p[i], p[i+1])
		}
	}
	for _, p := range points {
		add(p[0], p[1], p[0], p[1])
	}
	return u, has
}

// ── 双认领图元(计划/执行共用纯函数) ───────────────────────────────────────

// zoneTidyUnitExp 是一个刚体单元计划期的附着展开(桩线/旗 primitiveId 集)。
type zoneTidyUnitExp struct {
	wireIDs []string
	flagIDs []string
}

// zoneTidySharedIDs 收集被 >1 个单元认领的桩线/旗 primitiveId(区内组间互连:
// 一棵树同时压在两个单元的成员脚上、又不 terminate 在任何一方 —— 不属于任何单个
// 刚体)。primitiveId 全文档唯一(group.move 收的就是平铺混合列表),wire/flag
// 合并进一个集合即可。
func zoneTidySharedIDs(exps []zoneTidyUnitExp) map[string]bool {
	wireClaims, flagClaims := map[string]int{}, map[string]int{}
	for _, e := range exps {
		for _, id := range e.wireIDs {
			wireClaims[id]++
		}
		for _, id := range e.flagIDs {
			flagClaims[id]++
		}
	}
	shared := map[string]bool{}
	for id, n := range wireClaims {
		if n > 1 {
			shared[id] = true
		}
	}
	for id, n := range flagClaims {
		if n > 1 {
			shared[id] = true
		}
	}
	return shared
}

// zoneTidySubtractShared 从一个 move 集里剔除计划期标记的双认领图元(差集过滤,
// 保序,不改输入)。计划侧与执行侧必须用同一把刀:computeZoneTidy 把双认领桩线/
// 旗从所有单元剔除并承诺「原地不动」,但 --apply 的 expandSchGroupForMove /散件
// 展开是独立重展开、不知道这次 dedup —— 不过滤的话,一棵 member-touch 单元 A 且
// 掠过单元 B 成员脚的树会随 A 被拖走,与计划几何承诺相悖(且收尾自检可能不红,
// 静默偏差绿灯通过)。按计划期 id 过滤是可靠的:双认领图元恰恰全程不被移动,
// group.move 的 recreate 只发生在被移集合上,它们的 id 在整个 apply/回滚循环中
// 保持稳定。
func zoneTidySubtractShared(ids []string, shared map[string]bool) []string {
	if len(shared) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !shared[id] {
			out = append(out, id)
		}
	}
	return out
}

// ── band 来源(纯函数) ─────────────────────────────────────────────────────

// zoneTidyBandFromPlan 从 zone-plan 里取本区的分区 rect 当区带:该分区必须独占
// (只含本区一个模块 —— 共享 cell 内还有别的模块内容,整格打包会压别人),并扣掉
// 顶部 title band(大字标题预留,内容不得侵入)。
func zoneTidyBandFromPlan(plan partitionPlan, zone string) (layoutBBox, bool) {
	for _, p := range plan.Partitions {
		if !strInSlice(p.Modules, zone) {
			continue
		}
		if len(p.Modules) != 1 {
			return layoutBBox{}, false
		}
		band := layoutBBox{MinX: p.BBox.MinX, MinY: p.BBox.MinY, MaxX: p.BBox.MaxX, MaxY: p.TitleBBox.MinY}
		if band.MaxX <= band.MinX || band.MaxY <= band.MinY {
			return layoutBBox{}, false
		}
		return band, true
	}
	return layoutBBox{}, false
}

// zoneTidyGrowBand:区带装不下时向纸面空地生长——sheet−margin 可用区内、避开
// 其他分区 rect(inflate gutter)与图签 safe 带,四向独立夹逼、两轮定点、只长
// 不缩。区带锁死在旧分区 rect 会把「区内容本来就要长大」判成无解(超高模组锚
// + 去耦列在旧带里永远装不下);邻居约束由夹逼保证,纸面适配交给 sheet tidy。
func zoneTidyGrowBand(band layoutBBox, pplan partitionPlan, zone string, opts partitionOpts) layoutBBox {
	// band 是**内容**可占区域;分区框还要在内容外画 pad(四向)+ 顶部标题带,
	// 所以可用区按框的最终占位收缩——不收缩就是「内容顶到纸边 → 框压图签/标题
	// 带压 IC 头顶」(validation: LabelCollisions / ModuleOutsideZone,实测踩坑)。
	avail := layoutBBox{
		MinX: pplan.Sheet.MinX + opts.Margin + partitionContentPad,
		MinY: pplan.Sheet.MinY + opts.Margin + partitionContentPad,
		MaxX: pplan.Sheet.MaxX - opts.Margin - partitionContentPad,
		MaxY: pplan.Sheet.MaxY - opts.Margin - partitionContentPad - opts.TitleBand,
	}
	var obs []layoutBBox
	pad := opts.Gutter + partitionContentPad + opts.TitleBand // 本区框 pad+标题带也要挤进缝里(全向保守)
	for _, p := range pplan.Partitions {
		if strInSlice(p.Modules, zone) {
			continue
		}
		obs = append(obs, layoutBBox{
			MinX: p.BBox.MinX - pad, MinY: p.BBox.MinY - pad,
			MaxX: p.BBox.MaxX + pad, MaxY: p.BBox.MaxY + pad,
		})
	}
	if safe := inflatedTitleKeepout(pplan.Keepout); safe != nil {
		obs = append(obs, *safe)
	}
	g := band
	for round := 0; round < 2; round++ { // 两轮定点:先长的方向让后判的方向看到新范围
		top := avail.MaxY // 上(+y):夹到最近上方障碍底或纸带顶
		for _, o := range obs {
			if o.MinX < g.MaxX && o.MaxX > g.MinX && o.MinY >= g.MaxY-zonePackEps && o.MinY < top {
				top = o.MinY
			}
		}
		if top > g.MaxY {
			g.MaxY = top
		}
		bot := avail.MinY // 下(−y)
		for _, o := range obs {
			if o.MinX < g.MaxX && o.MaxX > g.MinX && o.MaxY <= g.MinY+zonePackEps && o.MaxY > bot {
				bot = o.MaxY
			}
		}
		if bot < g.MinY {
			g.MinY = bot
		}
		left := avail.MinX // 左(−x)
		for _, o := range obs {
			if o.MinY < g.MaxY && o.MaxY > g.MinY && o.MaxX <= g.MinX+zonePackEps && o.MaxX > left {
				left = o.MaxX
			}
		}
		if left < g.MinX {
			g.MinX = left
		}
		right := avail.MaxX // 右(+x)
		for _, o := range obs {
			if o.MinY < g.MaxY && o.MaxY > g.MinY && o.MinX >= g.MaxX-zonePackEps && o.MinX < right {
				right = o.MinX
			}
		}
		if right > g.MaxX {
			g.MaxX = right
		}
	}
	return g
}

// zoneTidyContentBand 是降级 band:区内现有内容 bbox 并集外扩 pad。
func zoneTidyContentBand(boxes []layoutBBox, pad float64) (layoutBBox, bool) {
	if len(boxes) == 0 {
		return layoutBBox{}, false
	}
	u := boxes[0]
	for _, b := range boxes[1:] {
		u.MinX = math.Min(u.MinX, b.MinX)
		u.MinY = math.Min(u.MinY, b.MinY)
		u.MaxX = math.Max(u.MaxX, b.MaxX)
		u.MaxY = math.Max(u.MaxY, b.MaxY)
	}
	return layoutBBox{MinX: u.MinX - pad, MinY: u.MinY - pad, MaxX: u.MaxX + pad, MaxY: u.MaxY + pad}, true
}

// findZoneTidyClaim 解析 --zone:先精确名,再大小写不敏感唯一名;找不到时列出
// 可用区名。
func findZoneTidyClaim(zones map[string]*schZoneClaim, ref string) (string, *schZoneClaim, error) {
	if len(zones) == 0 {
		return "", nil, fmt.Errorf("no schematic zone claims on this page — run `sch zones set --spec <s0-spec.json>` first")
	}
	if zc := zones[ref]; zc != nil {
		return ref, zc, nil
	}
	var hits []string
	for n := range zones {
		if strings.EqualFold(n, ref) {
			hits = append(hits, n)
		}
	}
	sort.Strings(hits)
	switch len(hits) {
	case 1:
		return hits[0], zones[hits[0]], nil
	case 0:
		var names []string
		for n := range zones {
			names = append(names, n)
		}
		sort.Strings(names)
		return "", nil, fmt.Errorf("zone %q not found on this page — available: %s", ref, strings.Join(names, ", "))
	default:
		return "", nil, fmt.Errorf("zone %q is ambiguous (%s) — use the exact name", ref, strings.Join(hits, ", "))
	}
}

// ── live 计算(读几何 → 出 plan;不 mutate) ────────────────────────────────

// zoneTidyGroupReport 是报告里的一个刚体单元 + 它的移动增量。
type zoneTidyGroupReport struct {
	Ref     string     `json:"ref"`
	Name    string     `json:"name,omitempty"`
	IsGroup bool       `json:"isGroup"`
	Members []string   `json:"members"`
	BBox    layoutBBox `json:"bbox"`
	DX      float64    `json:"dx"`
	DY      float64    `json:"dy"`
	Anchor  bool       `json:"anchor,omitempty"`
}

type zoneTidyReport struct {
	Zone       string                `json:"zone"`
	Band       layoutBBox            `json:"band"`
	BandSource string                `json:"bandSource"` // "zone-plan" | "content-bbox"
	HGap       float64               `json:"hGap"`
	VGap       float64               `json:"vGap"`
	Fits       bool                  `json:"fits"`
	Groups     []zoneTidyGroupReport `json:"groups"`
	Diag       *zonePackDiag         `json:"diag,omitempty"`
}

// zoneTidyGroupRefs(--deep 用):区内持久化组的 id 列表(确定性升序)。散件没有
// 组内布局可做,跳过;跨区组由 zoneTidyUnits 的既有校验拒绝。
func zoneTidyGroupRefs(pinned *appConfig, win, docUUID, zoneRef string) ([]string, error) {
	zones, project, err := loadSchZoneClaimsForPage(pinned, win, docUUID)
	if err != nil {
		return nil, err
	}
	_, claim, err := findZoneTidyClaim(zones, zoneRef)
	if err != nil {
		return nil, err
	}
	st, err := loadPcbStageState(project)
	if err != nil {
		return nil, err
	}
	units, err := zoneTidyUnits(claim.Parts, st.GroupsForPage(docUUID))
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, u := range units {
		if u.IsGroup {
			refs = append(refs, u.Ref)
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// computeZoneTidy 读一次几何快照(components.list + 稳定桩线读),把区成员切成
// 刚体单元、求每单元全集 bbox、定 band、跑 planZonePack。只读不 mutate。
// 第三个返回值 = 计划期标记的双认领桩线/旗 id 集,--apply 侧用它做差集过滤,
// 保证执行与计划的「原地不动」承诺一致。
func computeZoneTidy(pinned *appConfig, win, docUUID, zoneRef string, hGap, vGap float64, stderr io.Writer) (*zoneTidyReport, map[string]zoneTidyUnit, map[string]bool, error) {
	zones, project, err := loadSchZoneClaimsForPage(pinned, win, docUUID)
	if err != nil {
		return nil, nil, nil, err
	}
	zoneName, claim, err := findZoneTidyClaim(zones, zoneRef)
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := loadPcbStageState(project)
	if err != nil {
		return nil, nil, nil, err
	}
	units, err := zoneTidyUnits(claim.Parts, st.GroupsForPage(docUUID))
	if err != nil {
		return nil, nil, nil, err
	}
	if len(units) == 0 {
		return nil, nil, nil, fmt.Errorf("zone %q claims no placeable parts", zoneName)
	}

	res, err := requestAutolayoutAction(pinned, "schematic.components.list", win,
		map[string]any{"includeBBox": true, "includePins": true}, docUUID, "read zone-tidy geometry")
	if err != nil {
		return nil, nil, nil, err
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return nil, nil, nil, err
	}
	wires, err := fetchSchWirePolylinesStable(pinned, win, docUUID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read zone-tidy wire geometry: %w", err)
	}

	// 器件/旗几何索引(pin 实测,禁假设 —— 铁则 1)。
	partPins := map[string][][2]float64{}
	partBBox := map[string]*layoutBBox{}
	partSeen := map[string]bool{}
	var flags []schGroupFlag
	for _, c := range comps {
		switch {
		case schGroupFlagTypes[c.ComponentType]:
			if c.AnchorAvailable && c.ID != "" {
				flags = append(flags, schGroupFlag{ID: c.ID, X: c.X, Y: c.Y})
			}
		case c.ComponentType == "" || c.ComponentType == schLayoutPartType:
			d := strings.ToUpper(c.Designator)
			if d == "" {
				continue
			}
			partSeen[d] = true
			for _, p := range c.Pins {
				partPins[d] = append(partPins[d], [2]float64{p.X, p.Y})
			}
			if c.BBox != nil {
				if prior := partBBox[d]; prior == nil {
					b := *c.BBox
					partBBox[d] = &b
				} else { // 多子符号同位号:并集
					prior.MinX = math.Min(prior.MinX, c.BBox.MinX)
					prior.MinY = math.Min(prior.MinY, c.BBox.MinY)
					prior.MaxX = math.Max(prior.MaxX, c.BBox.MaxX)
					prior.MaxY = math.Max(prior.MaxY, c.BBox.MaxY)
				}
			}
		}
	}

	// 每单元附着展开(同 group-move 的树语义;共享纯函数 expandGroupAttachments)。
	wiresByID := map[string]schGroupWire{}
	for _, w := range wires {
		wiresByID[w.ID] = w
	}
	flagByID := map[string][2]float64{}
	for _, f := range flags {
		flagByID[f.ID] = [2]float64{f.X, f.Y}
	}
	exps := make([]zoneTidyUnitExp, len(units))
	sharedTrees := 0
	for i, u := range units {
		memberSet := map[string]bool{}
		for _, m := range u.Members {
			if !partSeen[m] {
				return nil, nil, nil, fmt.Errorf("claimed part %s (unit %s) is not on the active page — stale claim/group, or wrong page (`doc switch`)", m, u.Ref)
			}
			memberSet[m] = true
		}
		in := groupExpandInput{Wires: wires, Flags: flags}
		for d, pins := range partPins {
			if memberSet[d] {
				in.MemberPins = append(in.MemberPins, pins...)
			} else {
				in.OtherPins = append(in.OtherPins, pins...)
			}
		}
		exp := expandGroupAttachments(in)
		if len(exp.Suspects) > 0 {
			var ids []string
			for _, s := range exp.Suspects {
				ids = append(ids, s.WireID)
			}
			return nil, nil, nil, fmt.Errorf("unit %s expansion is INCOMPLETE — %d residue wire(s) collinearly graze a member pin without attaching (half-move residue): %s; clean up first (`sch prim-delete --ids %s`, then `sch check`) and retry",
				u.Ref, len(exp.Suspects), strings.Join(ids, ","), strings.Join(ids, ","))
		}
		sharedTrees += exp.SharedTrees
		exps[i] = zoneTidyUnitExp{wireIDs: exp.WireIDs, flagIDs: exp.FlagIDs}
	}
	// 跨单元共用的桩线/旗(区内组间互连)不属于任何单个刚体 —— 从所有单元剔除,
	// 原地不动(移动会撕另一端),报告提示。sharedIDs 同时返回给 --apply 做同一把
	// 差集过滤(执行侧独立重展开不知道这次 dedup;见 zoneTidySubtractShared)。
	sharedIDs := zoneTidySharedIDs(exps)
	dupWires, dupFlags := 0, 0
	for i := range exps {
		kw := zoneTidySubtractShared(exps[i].wireIDs, sharedIDs)
		kf := zoneTidySubtractShared(exps[i].flagIDs, sharedIDs)
		dupWires += len(exps[i].wireIDs) - len(kw)
		dupFlags += len(exps[i].flagIDs) - len(kf)
		exps[i].wireIDs, exps[i].flagIDs = kw, kf
	}
	if sharedTrees > 0 || dupWires > 0 {
		fmt.Fprintf(stderr, "note: %d wire tree(s) reach outside their unit and %d wire/flag reference(s) tie multiple units — left in place (real inter-part wiring); re-check with `sch check` after apply\n",
			sharedTrees, dupWires+dupFlags)
	}

	// 每单元全集 bbox。
	unitBoxes := make([]layoutBBox, len(units))
	for i, u := range units {
		var boxes []layoutBBox
		for _, m := range u.Members {
			b := partBBox[m]
			if b == nil {
				return nil, nil, nil, fmt.Errorf("part %s (unit %s) has no rendered bbox — shallow page data; `doc switch` to the page and retry", m, u.Ref)
			}
			boxes = append(boxes, *b)
		}
		var polys [][]float64
		for _, id := range exps[i].wireIDs {
			polys = append(polys, wiresByID[id].Points)
		}
		var pts [][2]float64
		for _, id := range exps[i].flagIDs {
			pts = append(pts, flagByID[id])
		}
		bb, ok := zoneTidyUnionBBox(boxes, polys, pts)
		if !ok {
			return nil, nil, nil, fmt.Errorf("unit %s produced no geometry", u.Ref)
		}
		unitBoxes[i] = bb
	}

	// band:优先 zone-plan 对应分区 rect;取不到时降级为区内内容 bbox 外扩 pad。
	var band layoutBBox
	bandSource := ""
	var pplan *partitionPlan
	if plan, _, perr := computePartitionPlan(pinned, win, docUUID, defaultPartitionOpts()); perr == nil {
		pplan = &plan
		if b, ok := zoneTidyBandFromPlan(plan, zoneName); ok {
			band, bandSource = b, "zone-plan"
		} else {
			fmt.Fprintf(stderr, "note: zone-plan has no exclusive partition for %q — falling back to the zone's content bbox + %g pad\n", zoneName, zoneTidyBandPad)
		}
	} else {
		fmt.Fprintf(stderr, "note: zone-plan unavailable (%v) — falling back to the zone's content bbox + %g pad\n", perr, zoneTidyBandPad)
	}
	if bandSource == "" {
		b, ok := zoneTidyContentBand(unitBoxes, zoneTidyBandPad)
		if !ok {
			return nil, nil, nil, fmt.Errorf("cannot derive a band for zone %q — no content bbox", zoneName)
		}
		band, bandSource = b, "content-bbox"
	}

	// 锚 = 含最大 bbox 器件的单元。
	areaByDesig := map[string]float64{}
	for _, d := range claim.Parts {
		if b := partBBox[strings.ToUpper(d)]; b != nil {
			areaByDesig[strings.ToUpper(d)] = zonePackArea(*b)
		}
	}
	anchorRef := zoneTidyAnchorRef(units, areaByDesig)

	packGroups := make([]zonePackGroup, len(units))
	unitByRef := map[string]zoneTidyUnit{}
	bboxByRef := map[string]layoutBBox{}
	for i, u := range units {
		// 形态桶按 bbox 纵横比:高>宽 = 竖放(双电源旗去耦,桶 0 排前),
		// 否则横放(带 netport 的信号链,桶 1)——同桶同行,横竖不混排。
		bucket := 1
		if unitBoxes[i].MaxY-unitBoxes[i].MinY > unitBoxes[i].MaxX-unitBoxes[i].MinX {
			bucket = 0
		}
		packGroups[i] = zonePackGroup{ID: u.Ref, BBox: unitBoxes[i], IsAnchor: u.Ref == anchorRef, Bucket: bucket}
		unitByRef[u.Ref] = u
		bboxByRef[u.Ref] = unitBoxes[i]
	}
	plan := planZonePack(packGroups, band, hGap, vGap)
	if !plan.Fits && pplan != nil {
		// 装不下 → band 向纸面空地生长(避开其他分区/图签/纸边)后重试。
		if grown := zoneTidyGrowBand(band, *pplan, zoneName, defaultPartitionOpts()); grown != band {
			fmt.Fprintf(stderr, "note: band %.0f×%.0f too small — grown to %.0f×%.0f within free sheet space\n",
				band.MaxX-band.MinX, band.MaxY-band.MinY, grown.MaxX-grown.MinX, grown.MaxY-grown.MinY)
			band, bandSource = grown, bandSource+"+grown"
			plan = planZonePack(packGroups, band, hGap, vGap)
		}
	}

	rep := &zoneTidyReport{
		Zone: zoneName, Band: band, BandSource: bandSource,
		HGap: hGap, VGap: vGap, Fits: plan.Fits, Diag: plan.Diag,
	}
	if plan.Fits {
		for _, m := range plan.Moves {
			u := unitByRef[m.ID]
			rep.Groups = append(rep.Groups, zoneTidyGroupReport{
				Ref: u.Ref, Name: u.Name, IsGroup: u.IsGroup, Members: u.Members,
				BBox: bboxByRef[m.ID], DX: m.DX, DY: m.DY, Anchor: m.Anchor,
			})
		}
	} else {
		for i, u := range units { // 装不下也把单元 bbox 列全,让诊断可动手
			rep.Groups = append(rep.Groups, zoneTidyGroupReport{
				Ref: u.Ref, Name: u.Name, IsGroup: u.IsGroup, Members: u.Members,
				BBox: unitBoxes[i], Anchor: u.Ref == anchorRef,
			})
		}
	}
	return rep, unitByRef, sharedIDs, nil
}

func renderZoneTidyReport(rep *zoneTidyReport, w io.Writer) {
	fmt.Fprintf(w, "zone-tidy [%s] band=(%.0f,%.0f)..(%.0f,%.0f) source=%s hGap=%g vGap=%g\n",
		rep.Zone, rep.Band.MinX, rep.Band.MinY, rep.Band.MaxX, rep.Band.MaxY, rep.BandSource, rep.HGap, rep.VGap)
	for _, g := range rep.Groups {
		kind := "group"
		if !g.IsGroup {
			kind = "loose"
		}
		tag := ""
		if g.Anchor {
			tag = "  [anchor]"
		}
		fmt.Fprintf(w, "  %-6s %-5s members=%s bbox=(%.0f,%.0f)..(%.0f,%.0f) Δ(%g,%g)%s\n",
			g.Ref, kind, strings.Join(g.Members, ","), g.BBox.MinX, g.BBox.MinY, g.BBox.MaxX, g.BBox.MaxY, g.DX, g.DY, tag)
	}
	if rep.Fits {
		fmt.Fprintln(w, "✓ plan fits — groups pairwise disjoint and inside the band")
	} else if rep.Diag != nil {
		fmt.Fprintf(w, "✗ does not fit: %s\n", rep.Diag.Reason)
	}
}

// ── --apply 执行(逐组 group-move + settle + 自检 + 红则逆序回滚) ───────────

// zoneTidyExpandUnitIDs 把一个刚体单元展开成 group-move 的完整 primitiveId 集:
// 持久化组复用 expandSchGroupForMove(自带完整性预检 + 稳定读);散件走同语义的
// 临时单件组展开(残骸 graze 同样硬拒)。
func zoneTidyExpandUnitIDs(cfg *appConfig, win, docUUID string, u zoneTidyUnit) ([]string, error) {
	if u.IsGroup {
		set, err := expandSchGroupForMove(cfg, win, u.Ref)
		if err != nil {
			return nil, err
		}
		return set.AllIDs(), nil
	}
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", win,
		map[string]any{"includePins": true}, docUUID, "resolve loose part "+u.Ref)
	if err != nil {
		return nil, err
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return nil, err
	}
	target := strings.ToUpper(u.Ref)
	in := groupExpandInput{}
	var compIDs []string
	for _, c := range comps {
		desig := strings.ToUpper(c.Designator)
		switch {
		case desig == target && (c.ComponentType == "" || c.ComponentType == schLayoutPartType):
			compIDs = append(compIDs, c.ID)
			for _, p := range c.Pins {
				in.MemberPins = append(in.MemberPins, [2]float64{p.X, p.Y})
			}
		case schGroupFlagTypes[c.ComponentType]:
			if c.AnchorAvailable && c.ID != "" {
				in.Flags = append(in.Flags, schGroupFlag{ID: c.ID, X: c.X, Y: c.Y})
			}
		case c.ComponentType == "" || c.ComponentType == schLayoutPartType:
			for _, p := range c.Pins {
				in.OtherPins = append(in.OtherPins, [2]float64{p.X, p.Y})
			}
		}
	}
	if len(compIDs) == 0 {
		return nil, fmt.Errorf("loose part %s is not on the active page", u.Ref)
	}
	in.Wires, err = fetchSchWirePolylinesStable(cfg, win, docUUID)
	if err != nil {
		return nil, err
	}
	exp := expandGroupAttachments(in)
	if len(exp.Suspects) > 0 {
		var ids []string
		for _, s := range exp.Suspects {
			ids = append(ids, s.WireID)
		}
		return nil, fmt.Errorf("loose part %s expansion is INCOMPLETE — %d residue wire(s) graze its pin without attaching; clean up first (`sch prim-delete --ids %s`, then `sch check`) and retry",
			u.Ref, len(exp.Suspects), strings.Join(ids, ","))
	}
	ids := append(compIDs, exp.WireIDs...)
	return append(ids, exp.FlagIDs...), nil
}

// zoneTidyBridgeRed 跑一次 bridge-check 数据面:red = 存在 BRIDGE(真短路)。
// transport/解析失败作为「自检不可用」返回 err,与「红」区分 —— 不可用不触发回滚。
func zoneTidyBridgeRed(cfg *appConfig, win string) (red bool, detail string, err error) {
	res, err := requestAction(cfg, "schematic.bridgeCheck", win, nil)
	if err != nil {
		return false, "", err
	}
	rep, perr := parseBridgeReport(res.Result)
	if perr != nil {
		return false, "", perr
	}
	if rep.Summary.Bridges > 0 {
		return true, fmt.Sprintf("%d bridge(s) — real short(s)", rep.Summary.Bridges), nil
	}
	return false, "", nil
}

// applyZoneTidy 执行 plan:逐组完整集合刚移(组间 settle;每次展开后按 sharedIDs
// 差集过滤,双认领桩线/旗按计划承诺原地不动),收尾 layout-lint + bridge-check
// 自检,红则按逆序回滚已移组。
// zoneTidyMoveStep 是一次刚移步骤;staging 的组出现两步(先停带外 parking 再进终位)。
type zoneTidyMoveStep struct {
	ref     string
	dx, dy  float64
	staging bool
}

// zoneTidyOrderMoves 按**暂态依赖**排移动次序:组 i 的目标 bbox 压组 j 的当前
// bbox(j 未移)→ j 必须先走。平台会把暂态叠位期间共点的线 MERGE 成一根,之后
// 再移就撕出短路(实测 2026-08-12:C5 落到还没搬走的 R2 原位,EN/IO0 两树被
// merge 成 multi-net,回滚更撕成同点双 netport + 悬空)。计划终态两两不叠是
// plan validate 的事;这里管的是**次序过程**。有环时选组两跳走位(先停到带右
// 外 parking 打破环,他组清场后再进终位)。
func zoneTidyOrderMoves(groups []zoneTidyGroupReport, band layoutBBox) []zoneTidyMoveStep {
	const pad = 8.0 // 线头 merge 判定容差:bbox 相切时线端点也可能共点
	inflate := func(b layoutBBox) layoutBBox {
		return layoutBBox{MinX: b.MinX - pad, MinY: b.MinY - pad, MaxX: b.MaxX + pad, MaxY: b.MaxY + pad}
	}
	var movers []zoneTidyGroupReport
	for _, g := range groups {
		if g.DX != 0 || g.DY != 0 {
			movers = append(movers, g)
		}
	}
	steps := make([]zoneTidyMoveStep, 0, len(movers)+1)
	done := make([]bool, len(movers))
	cur := make([]layoutBBox, len(movers)) // 当前占位(未移=原位;parked 组已离场)
	for i, g := range movers {
		cur[i] = g.BBox
	}
	parkX := band.MaxX + 200 // parking 起点:带右外,不与任何在场内容相交
	var parkedFinal []zoneTidyMoveStep
	for remaining := len(movers); remaining > 0; {
		progress := false
		for i, g := range movers {
			if done[i] {
				continue
			}
			blocked := false
			for j := range movers {
				if j == i || done[j] {
					continue
				}
				if boxesOverlap(inflate(zonePackOffset(g.BBox, g.DX, g.DY)), inflate(cur[j])) {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			steps = append(steps, zoneTidyMoveStep{ref: g.Ref, dx: g.DX, dy: g.DY})
			done[i], progress = true, true
			remaining--
		}
		if progress {
			continue
		}
		// 环:取第一个未完组停到 parking(两跳),他组清场后从 parking 进终位。
		for i, g := range movers {
			if done[i] {
				continue
			}
			pdx := zonePackCeilGrid(parkX - g.BBox.MinX)
			steps = append(steps, zoneTidyMoveStep{ref: g.Ref, dx: pdx, dy: 0, staging: true})
			parkedFinal = append(parkedFinal, zoneTidyMoveStep{ref: g.Ref, dx: g.DX - pdx, dy: g.DY})
			parkX += (g.BBox.MaxX - g.BBox.MinX) + 100
			cur[i] = layoutBBox{MinX: 1e18, MinY: 1e18, MaxX: 1e18, MaxY: 1e18} // 已离场,不再挡人
			done[i] = true
			remaining--
			break
		}
	}
	return append(steps, parkedFinal...)
}

func applyZoneTidy(pinned *appConfig, win, docUUID string, rep *zoneTidyReport, unitByRef map[string]zoneTidyUnit, sharedIDs map[string]bool, stdout, stderr io.Writer) error {
	type appliedMove struct {
		unit   zoneTidyUnit
		dx, dy float64
	}
	var applied []appliedMove
	// rollback 逆序重放 (-dx,-dy)(eda.* 无程序化 undo,回滚 = 再做一次刚移)。
	// 回滚完成后**必须复检** bridge-check:回滚本身也是刚移,若 apply 期间平台
	// 已把叠位共点线 merge,逆移会撕出短路/同点双标——静默留坏板比报错更糟
	// (实测 2026-08-12:回滚后 EN/IO0 同点 netport + multi-net 线,直到下一轮
	// gate 才被发现)。红则大字报受损,别让调用方以为板子完好。
	rollback := func(cause error) error {
		if len(applied) == 0 {
			return cause
		}
		fmt.Fprintf(stderr, "rolling back %d applied move(s) in reverse order…\n", len(applied))
		var failed []string
		for i := len(applied) - 1; i >= 0; i-- {
			time.Sleep(zoneTidySettle) // 铁则 2:回滚同样要等上一次 recreate 落定
			mv := applied[i]
			ids, err := zoneTidyExpandUnitIDs(pinned, win, docUUID, mv.unit)
			if err == nil {
				ids = zoneTidySubtractShared(ids, sharedIDs) // 双认领图元正着没动,倒着也不能拖
				_, err = requestAutolayoutAction(pinned, "schematic.group.move", win,
					map[string]any{"primitiveIds": ids, "dx": -mv.dx, "dy": -mv.dy},
					docUUID, "zone-tidy rollback "+mv.unit.Ref)
			}
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s (applied Δ%g,%g): %v", mv.unit.Ref, mv.dx, mv.dy, err))
				continue
			}
			fmt.Fprintf(stderr, "  ↩ %s moved back Δ(%g,%g)\n", mv.unit.Ref, -mv.dx, -mv.dy)
		}
		if len(failed) > 0 {
			return fmt.Errorf("%w; ROLLBACK INCOMPLETE — still displaced: %s (undo manually with `sch group-move` using the inverse deltas, then re-verify with `sch layout-lint` + `sch check`)",
				cause, strings.Join(failed, "; "))
		}
		time.Sleep(zoneTidySettle)
		if red, detail, err := zoneTidyBridgeRed(pinned, win); err != nil {
			return fmt.Errorf("%w; all applied moves rolled back but the post-rollback re-check was UNAVAILABLE (%v) — verify manually with `sch check` + `sch bridge-check`", cause, err)
		} else if red {
			return fmt.Errorf("%w; all applied moves rolled back but the board is DAMAGED (%s) — the platform merged wires during the transient overlap and the rollback tore them; repair per `sch check` findings (multi-net wire → delete + reconnect both pins)", cause, detail)
		}
		return fmt.Errorf("%w; all applied moves rolled back, post-rollback bridge-check green", cause)
	}

	// 暂态依赖排序:目标位压未移组原位的后走(平台 merge 共点线 → 撕裂短路)。
	for _, st := range zoneTidyOrderMoves(rep.Groups, rep.Band) {
		u, ok := unitByRef[st.ref]
		if !ok {
			return rollback(fmt.Errorf("internal: no unit for move %s", st.ref))
		}
		if len(applied) > 0 {
			time.Sleep(zoneTidySettle) // 铁则 2:组间 settle
		}
		ids, err := zoneTidyExpandUnitIDs(pinned, win, docUUID, u)
		if err != nil {
			return rollback(fmt.Errorf("expand %s: %w", st.ref, err))
		}
		// 差集过滤:计划期标记的双认领桩线/旗不随本单元移动(执行 = 计划承诺)。
		ids = zoneTidySubtractShared(ids, sharedIDs)
		if _, err := requestAutolayoutAction(pinned, "schematic.group.move", win,
			map[string]any{"primitiveIds": ids, "dx": st.dx, "dy": st.dy},
			docUUID, "zone-tidy move "+st.ref); err != nil {
			return rollback(fmt.Errorf("move %s: %w", st.ref, err))
		}
		applied = append(applied, appliedMove{unit: u, dx: st.dx, dy: st.dy})
		tag := ""
		if st.staging {
			tag = "  [staging:带外暂存打破次序环]"
		}
		fmt.Fprintf(stderr, "✓ %s moved Δ(%g,%g) — %d primitive(s)%s\n", st.ref, st.dx, st.dy, len(ids), tag)
	}
	if len(applied) == 0 {
		fmt.Fprintln(stdout, "zone already tidy — no group needed to move")
		return nil
	}

	time.Sleep(zoneTidySettle)
	// 收尾自检(铁则 5):layout-lint(0 overlap)+ bridge-check(0 bridge)。
	lintRep, lintErr := collectLayoutLint(pinned, win, 2.54, 0, false, false, false)
	bridgeRed, bridgeDetail, bridgeErr := zoneTidyBridgeRed(pinned, win)
	if lintErr != nil || bridgeErr != nil {
		// 自检不可用 ≠ 红:回滚同样要动图,盲滚只会更糟 —— 保留结果并要求人工复核。
		return fmt.Errorf("zone-tidy applied %d move(s) but the self-check was UNAVAILABLE (layout-lint: %v; bridge-check: %v) — verify manually with `sch layout-lint` + `sch bridge-check`",
			len(applied), lintErr, bridgeErr)
	}
	var red []string
	if !lintRep.OK {
		red = append(red, fmt.Sprintf("layout-lint: %d overlap(s), %d pin-coincidence(s)", len(lintRep.Overlaps), len(lintRep.PinCoincidences)))
	}
	if bridgeRed {
		red = append(red, "bridge-check: "+bridgeDetail)
	}
	if len(red) > 0 {
		return rollback(fmt.Errorf("post-apply self-check RED (%s)", strings.Join(red, "; ")))
	}
	if err := saveAutolayoutDocument(pinned, win, docUUID, "save zone-tidy result"); err != nil {
		return fmt.Errorf("zone-tidy applied and the self-check is green, but the explicit save failed: %w (the daemon autosave net still applies; run `easyeda sch save` to be sure)", err)
	}
	fmt.Fprintf(stdout, "✓ zone-tidy applied: %d group(s) moved, layout-lint + bridge-check green, schematic saved\n", len(applied))
	return nil
}

// ── cobra surface(主会话统一注册进 cmd_sch.go) ────────────────────────────

// newSchZoneTidyCommand builds `sch zone-tidy` — 组间叠加布局(契约 §3)。
func newSchZoneTidyCommand(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var zone string
	var hGap, vGap float64
	var apply, dryRun, asJSON, deep bool
	c := &cobra.Command{
		Use:   "tidy",
		Short: "组间叠加布局:把功能区内的组与散件当刚体在区带内重排(默认 dry-run;--apply 执行)",
		Long: `组间叠加布局(设计契约 §3):把一个功能区(` + "`sch zones`" + ` claim)内的每个
持久化组 —— 以及未入组的认领散件(临时单件组)—— 当作刚体,在该区的区带内重排:

  - 锚组 = 含最大 bbox 器件的组,不动(已落带内)或贴带内基准位(带左上);
  - 其余组按面积降序,优先与锚组垂直堆叠(上下布局),行内水平排(间距 ≥ --h-gap,
    默认 117 = 两个相向水平 netport 标签实测最小距),行满换行(行距 ≥ --v-gap);
  - 全部组 bbox 互不重叠且落在带内;装不下时报告需要的最小带尺寸,不硬塞;
  - {dx,dy} 吸附 5 单位连接网格,组内既有对齐整体保留;同输入同输出。

band 优先取 zone-plan 对应分区 rect(独占分区,扣掉顶部 title band);取不到时降级
为区内内容 bbox 外扩(stderr 报告)。跨区组(成员部分在区内、部分在区外)是配置
错误,直接拒绝。

--apply:逐组走完整 group-move 集合派发(成员 + 桩线 + 远端旗,同
` + "`sch group-move --group`" + ` 语义),组间 settle;完成后 layout-lint + bridge-check
自检,红则按逆序把已移组移回。默认(不带 --apply)只算不动。`,
		Args: cobra.NoArgs,
		Example: `  easyeda sch zone-tidy --zone POWER              # 只算不动(dry-run)
  easyeda sch zone-tidy --zone POWER --json
  easyeda sch zone-tidy --zone POWER --apply      # 逐组刚移 + 自检 + 红则回滚`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if apply && dryRun {
				return fmt.Errorf("--dry-run and --apply are mutually exclusive")
			}
			if hGap < 0 || vGap < 0 {
				return fmt.Errorf("--h-gap / --v-gap must be ≥ 0")
			}
			pinned, win, docUUID, err := pinZonePage(cfg, *window)
			if err != nil {
				return err
			}
			// --deep(一键):组间 pack 之前先逐组跑 group tidy(组内布局计算),
			// 把每个虚拟组内部整理成规范形态(竖放/上电下地/文字朝外/深度清扫
			// 残段),然后再排组。深度失败即停(组内没整好,组间排了也是白排)。
			if deep {
				if !apply {
					return fmt.Errorf("--deep 需要 --apply(逐组 tidy 是 mutation;先单独 dry-run 看各组计划:`sch group tidy --group <id>`)")
				}
				groupRefs, derr := zoneTidyGroupRefs(pinned, win, docUUID, zone)
				if derr != nil {
					return derr
				}
				for _, ref := range groupRefs {
					fmt.Fprintf(stdout, "── deep:group tidy %s ──\n", ref)
					if terr := runSchGroupTidy(pinned, win, ref, "auto", 50, true, stdout, stderr); terr != nil {
						return fmt.Errorf("--deep 在组 %s 处停止:%w", ref, terr)
					}
					time.Sleep(350 * time.Millisecond) // 组间 settle(铁则2)
				}
			}
			rep, unitByRef, sharedIDs, err := computeZoneTidy(pinned, win, docUUID, zone, hGap, vGap, stderr)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else {
				renderZoneTidyReport(rep, stdout)
			}
			if !rep.Fits {
				return fmt.Errorf("zone-tidy: %s — enlarge the zone's band (re-run `sch zone-plan` / adjust claims / `sch zone move`) instead of force-packing", rep.Diag.Reason)
			}
			if !apply {
				if !asJSON {
					fmt.Fprintln(stdout, "dry-run — pass --apply to execute the moves")
				}
				return nil
			}
			return applyZoneTidy(pinned, win, docUUID, rep, unitByRef, sharedIDs, stdout, stderr)
		},
	}
	c.Flags().StringVar(&zone, "zone", "", "functional zone name (a `sch zones` claim / module name; required)")
	c.Flags().Float64Var(&hGap, "h-gap", zoneTidyHGapDefault, "min horizontal gap between group bboxes, native units (117 = 两个相向水平 netport 标签实测最小距)")
	c.Flags().Float64Var(&vGap, "v-gap", zoneTidyVGapDefault, "min vertical gap between group bboxes, native units")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "compute and report only (this is already the default; explicit flag for scripts)")
	c.Flags().BoolVar(&apply, "apply", false, "execute the plan: per-group rigid moves with settle, then layout-lint + bridge-check; rollback in reverse order on red")
	c.Flags().BoolVar(&asJSON, "json", false, "emit the plan as JSON")
	c.Flags().BoolVar(&deep, "deep", false, "一键:组间 pack 前先对区内每个组跑 `group tidy --apply`(组内竖放/上电下地/文字朝外/深度清扫)——需与 --apply 同用")
	_ = c.MarkFlagRequired("zone")
	return c
}
