package app

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ── sch destagger: 安全批量消 marker-overlap(issue #171)────────────────────
//
// `sch check` 早就能**检测** marker-overlap(#148),但一直没有**修**:实测一块
// 4 页板一次报 101 条纯视觉重叠,手工逐个挪不现实;而直接 `sch modify` 挪
// netflag/netport 坐标会**把标识从导线端点上挪脱 → 断网/悬空**(EasyEDA 铁律:
// 重叠坐标不算连)。所以这类 WARN 长期"修不动"。
//
// 本文件是修复侧的**规划器**(纯函数,离线可测)。安全性来自三条:
//
//  1. **只搬桩线**:marker 必须坐在一根**两点直线短桩**的端点上,另一端是宿主
//     (pin 侧)。挂在多段折线/网络主干上的 marker 一律跳过——那种线一动就要
//     重走,不是本命令该碰的(reason=not-a-stub)。
//  2. **带桩线一起挪**:落地走 `disconnect`(旗+桩线一起删)→ `connect_pin`
//     (按新 direction/offset 重新拉桩+落旗),而不是挪坐标。宿主端坐标一字不动,
//     所以电气拓扑天然不变。
//  3. **offset 是量出来的**:候选步长取该旗**文字带**(flagTextBand,#148 同一
//     套公式)的实际尺寸而非拍脑袋常量——2026-08-12 那次手动修复 AI 临场猜
//     30/40/50/70 改了三轮才收敛,正是本命令要消灭的路径。
//
// 落地侧(cmd_sch_destagger_run.go)还有第四条:每轮改完自动 `sch check` 复验,
// 电气项(floating pin / dangling wire / net-marker mismatch / multi-net-wire …)
// 任何一项变非 0 就回滚该次移动。

// destaggerStubMaxLen 是"短桩"的长度上限。连出来的桩线实测 30–120 单位
// (`sch connect --offset` 默认档),网络主干动辄几百。超过它的线视作主干,
// 挂在上面的 marker 不搬。
const destaggerStubMaxLen = 260

// destaggerGrid 必须与连接器 connect_pin 的 SCH_GRID(extension/src/actions.ts,
// 当前 5)一致:桩端坐标在那边被 nearestGrid 吸附,桩长不落在栅格上时**实际
// 落点与规划预测差半格**,预测 bbox 随之失真。圆整在规划侧做,让计划里的
// ToOffset 就是平台最终采用的值(计划 = 落地,不留解释差)。
const destaggerGrid = 5.0

// schMarkerOverlapEps 是 marker-overlap / titleblock-overlap 的重叠判定阈值
// (issue #148)。`sch check --overlap-eps` 与 `sch destagger --overlap-eps` 共用
// 同一个默认 —— 两边一旦漂移就会出现"check 报重叠、destagger 说不重叠"的
// 死循环。
const schMarkerOverlapEps = 0.5

// destaggerDirs 是四个正交方向的单位向量(y-UP:up = +y)。
var destaggerDirs = map[string][2]float64{
	"up":    {0, 1},
	"down":  {0, -1},
	"left":  {-1, 0},
	"right": {1, 0},
}

// destaggerDirPreference 给出每类 marker 的候选方向优先序。
//
// **power/gnd 只许竖直**(2026-08-13 真机验收抓到的缺陷:初版把 left/right 也列
// 进候选,规划器真的把一支 GND 旗扶去了 left)——横躺的 power/gnd 旗文字竖排
// 侧向渲染,是平台特性,极难看,skill conventions 早有铁律「信号链末端的电源/地
// 旗必须竖直」。地朝下、电朝上是正位(「电上地下」),另一竖直方向兜底。
// netport 顺导线方向摆布,四向都合法(2026-08-12 用户拍板)。
var destaggerDirPreference = map[string][]string{
	"ground": {"down", "up"},
	"power":  {"up", "down"},
	"port":   {"right", "left", "up", "down"},
}

// destaggerStub 是一个 marker 及其桩线的几何。
type destaggerStub struct {
	Flag   layoutComp
	WireID string
	// HostX/HostY 是桩线的**宿主端**(pin 侧),搬迁中一字不动 —— 电气拓扑的锚。
	HostX, HostY float64
	Dir          string  // 宿主 → 锚 的主轴向
	Offset       float64 // 桩长
}

// destaggerMove 是一次计划中的搬迁。落地时先 disconnect(FlagID),再
// connect_pin(HostX/HostY, Kind, Net, ToDir, ToOffset)。
type destaggerMove struct {
	FlagID        string  `json:"flagId"`
	Net           string  `json:"net"`
	ComponentType string  `json:"componentType"`
	Kind          string  `json:"kind"` // connector kind(ground/power/net_port_bi…)
	HostX         float64 `json:"hostX"`
	HostY         float64 `json:"hostY"`
	FromDir       string  `json:"fromDir"`
	FromOffset    float64 `json:"fromOffset"`
	ToDir         string  `json:"toDir"`
	ToOffset      float64 `json:"toOffset"`
	// ClearedWith 是这次搬迁解掉的重叠对手(便于人读报告与回归断言)。
	ClearedWith []string   `json:"clearedWith,omitempty"`
	NewBBox     layoutBBox `json:"newBBox"`
}

// destaggerSkip 是一个**参与重叠但没搬**的 marker + 原因。规划器绝不静默丢弃:
// 每个没解掉的重叠都必须在报告里留痕(否则"修完还剩 N 条"无法归因)。
type destaggerSkip struct {
	FlagID        string `json:"flagId"`
	Net           string `json:"net"`
	ComponentType string `json:"componentType"`
	Reason        string `json:"reason"`
}

// destaggerPlan 是一轮规划的全部产出。
type destaggerPlan struct {
	Moves          []destaggerMove `json:"moves"`
	Skips          []destaggerSkip `json:"skips"`
	OverlapsBefore int             `json:"overlapsBefore"`
}

// planDestagger 规划一轮 de-stagger:算出哪些 marker 该换到什么方向/桩长,才能
// 消掉 marker-overlap。纯函数 —— comps/wires 进,计划出,不碰网络。
//
// eps 与 `sch check` 的 overlap eps 同义(小于它的边缘擦碰不算重叠)。
func planDestagger(comps []layoutComp, wires []schGroupWire, eps float64) destaggerPlan {
	findings := markerOverlapFindings(comps, eps)
	plan := destaggerPlan{OverlapsBefore: len(findings)}
	if len(findings) == 0 {
		return plan
	}

	// 参与重叠的 marker → 它的对手集合。marker×part 的重叠里只有 marker 侧可搬
	// (器件是布局决定的,不归本命令管)。
	rivals := map[string][]string{}
	for _, f := range findings {
		a, b := f.PrimitiveId, f.Other.PrimitiveId
		if isSchMarker(f.ComponentType) {
			rivals[a] = append(rivals[a], b)
		}
		if isSchMarker(f.Other.ComponentType) {
			rivals[b] = append(rivals[b], a)
		}
	}

	byID := map[string]layoutComp{}
	for _, c := range comps {
		byID[c.ID] = c
	}
	// 障碍集 = 每个图元的**判定 bbox**(marker 含文字带),与 check 同一套几何。
	obstacles := map[string]layoutBBox{}
	for _, c := range comps {
		if c.BBox == nil || c.ComponentType == "sheet" {
			continue
		}
		obstacles[c.ID] = markerJudgeBBox(c)
	}

	// 稳定顺序:先地后电再端口(正位冲突时让优先级高的先占),同类按 id。
	ids := make([]string, 0, len(rivals))
	for id := range rivals {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		ci, cj := byID[ids[i]], byID[ids[j]]
		ri, rj := destaggerClassRank(ci), destaggerClassRank(cj)
		if ri != rj {
			return ri < rj
		}
		return ids[i] < ids[j]
	})

	for _, id := range ids {
		c, ok := byID[id]
		if !ok {
			continue
		}
		stub, why := stubOfMarker(c, wires)
		if why != "" {
			plan.Skips = append(plan.Skips, destaggerSkip{
				FlagID: id, Net: c.Net, ComponentType: c.ComponentType, Reason: why,
			})
			continue
		}
		mv, placed := planOneDestagger(*stub, obstacles, eps)
		if !placed {
			plan.Skips = append(plan.Skips, destaggerSkip{
				FlagID: id, Net: c.Net, ComponentType: c.ComponentType,
				Reason: "no-free-slot",
			})
			continue
		}
		mv.ClearedWith = append([]string(nil), rivals[id]...)
		sort.Strings(mv.ClearedWith)
		// 采纳:把自己的新 bbox 写回障碍集,后续 marker 不会再往这儿挤。
		obstacles[id] = mv.NewBBox
		plan.Moves = append(plan.Moves, mv)
	}
	sort.Slice(plan.Moves, func(i, j int) bool { return plan.Moves[i].FlagID < plan.Moves[j].FlagID })
	sort.Slice(plan.Skips, func(i, j int) bool { return plan.Skips[i].FlagID < plan.Skips[j].FlagID })
	return plan
}

// planOneDestagger 给一个 marker 找不撞人的 (方向, 桩长)。候选 = 该类偏好方向
// × 递增桩长;步长取自文字带实测尺寸(不是常量)。返回 placed=false 表示所有候选
// 都被占——此时**宁可不动**也不硬塞(挪了还撞等于白改,还多一次网络手术风险)。
func planOneDestagger(stub destaggerStub, obstacles map[string]layoutBBox, eps float64) (destaggerMove, bool) {
	c := stub.Flag
	step := destaggerStep(c)
	dirs := destaggerDirPreference[destaggerClassOf(c)]
	if len(dirs) == 0 {
		dirs = []string{"down", "up", "left", "right"}
	}
	// 桩长候选:原长 + 逐级加一个文字带,全部圆整到连接器栅格 —— 计划里的桩长
	// 就是平台会采用的桩长。
	offsets := []float64{destaggerSnap(stub.Offset)}
	for k := 1; k <= 4; k++ {
		offsets = append(offsets, destaggerSnap(stub.Offset+float64(k)*step))
	}
	// 候选序:**先在原方向上拉开桩长**(de-stagger 的本义就是同向错开,视觉扰动
	// 最小、也不动旗的朝向),原方向排不下才换方向。初版是"方向外层、桩长内层",
	// 结果一支本可以拉长 45 就避开的 GND 旗被直接扶去了别的方向(2026-08-13 真机)。
	type destaggerCand struct {
		dir string
		off float64
	}
	var cands []destaggerCand
	for _, off := range offsets {
		if math.Abs(off-stub.Offset) < 0.5 {
			continue // 原地不动 = 没解决问题
		}
		cands = append(cands, destaggerCand{stub.Dir, off})
	}
	for _, dir := range dirs {
		if dir == stub.Dir {
			continue
		}
		for _, off := range offsets {
			cands = append(cands, destaggerCand{dir, off})
		}
	}
	for _, cd := range cands {
		box := predictMarkerBBox(stub, cd.dir, cd.off)
		if destaggerCollides(box, c.ID, obstacles, eps) {
			continue
		}
		return destaggerMove{
			FlagID:        c.ID,
			Net:           c.Net,
			ComponentType: c.ComponentType,
			Kind:          destaggerKindOf(c),
			HostX:         stub.HostX,
			HostY:         stub.HostY,
			FromDir:       stub.Dir,
			FromOffset:    round2(stub.Offset),
			ToDir:         cd.dir,
			ToOffset:      round2(cd.off),
			NewBBox:       box,
		}, true
	}
	return destaggerMove{}, false
}

// destaggerCollides 判断候选 bbox 是否与除自己外的任何障碍正面积重叠。
func destaggerCollides(box layoutBBox, selfID string, obstacles map[string]layoutBBox, eps float64) bool {
	for id, ob := range obstacles {
		if id == selfID {
			continue
		}
		ox, oy, hit := overlapExtent(box, ob)
		if hit && math.Min(ox, oy) > eps {
			return true
		}
	}
	return false
}

// stubOfMarker 找 marker 坐落的**短桩线**并解出宿主端/方向/桩长。返回非空
// reason 表示不可搬(调用方记 skip)。判据严格是故意的——issue #171 的安全原语
// 就是"只动能安全重连的那种线"。
func stubOfMarker(c layoutComp, wires []schGroupWire) (*destaggerStub, string) {
	if !isSchMarker(c.ComponentType) {
		return nil, "not-a-marker"
	}
	if !c.AnchorAvailable || c.BBox == nil {
		return nil, "no-anchor-or-bbox"
	}
	if c.Net == "" {
		return nil, "no-net"
	}
	const eps = 0.5
	for _, w := range wires {
		pts := w.Points
		if len(pts) != 4 {
			continue // 只认两点直线段:折线一动就要重走
		}
		ax, ay := pts[0], pts[1]
		bx, by := pts[2], pts[3]
		var hx, hy float64
		switch {
		case pointsClose(c.X, c.Y, ax, ay, eps):
			hx, hy = bx, by
		case pointsClose(c.X, c.Y, bx, by, eps):
			hx, hy = ax, ay
		default:
			continue
		}
		dx, dy := c.X-hx, c.Y-hy
		length := math.Hypot(dx, dy)
		if length > destaggerStubMaxLen {
			return nil, "stub-too-long"
		}
		if length < 1 {
			return nil, "zero-length-stub"
		}
		// 只认正交桩:斜桩重连语义不明(connect_pin 的 direction 是四正交)。
		if math.Abs(dx) > eps && math.Abs(dy) > eps {
			return nil, "diagonal-stub"
		}
		dir := ""
		switch {
		case math.Abs(dx) <= eps && dy > 0:
			dir = "up"
		case math.Abs(dx) <= eps && dy < 0:
			dir = "down"
		case math.Abs(dy) <= eps && dx > 0:
			dir = "right"
		default:
			dir = "left"
		}
		return &destaggerStub{Flag: c, WireID: w.ID, HostX: hx, HostY: hy, Dir: dir, Offset: length}, ""
	}
	return nil, "not-a-stub" // 锚不在任何两点桩线端点上(主干线/孤儿旗)
}

// predictMarkerBBox 预测 marker 换到 (dir, offset) 后的判定 bbox。
//
// 平台不给"某方向下的 bbox",所以从**当前**几何反解符号尺寸:沿当前桩向的
// 前伸 along、后伸 back、横向半宽 cross —— 换方向即把这三个量绕锚点转过去
// (旗/端口符号本就是绕锚旋转的)。文字带按新方向用 flagTextBand 同一套公式重算。
//
// 这是估算不是实测:平台真实 bbox 只有落地后才知道。所以落地侧按轮迭代 +
// 每轮用真实 check 复验 —— 估偏最多是这轮没消掉(下轮再来),不会造成危害。
func predictMarkerBBox(stub destaggerStub, dir string, offset float64) layoutBBox {
	c := stub.Flag
	along, back, cross := markerExtents(c, stub.Dir)
	u := destaggerDirs[dir]
	ax := stub.HostX + u[0]*offset
	ay := stub.HostY + u[1]*offset
	var box layoutBBox
	if u[0] != 0 { // 水平
		x0, x1 := ax-back, ax+along
		if u[0] < 0 {
			x0, x1 = ax-along, ax+back
		}
		box = layoutBBox{MinX: x0, MaxX: x1, MinY: ay - cross, MaxY: ay + cross}
	} else { // 竖直
		y0, y1 := ay-back, ay+along
		if u[1] < 0 {
			y0, y1 = ay-along, ay+back
		}
		box = layoutBBox{MinX: ax - cross, MaxX: ax + cross, MinY: y0, MaxY: y1}
	}
	// 文字带:构造搬迁后的等价 comp 走 flagTextBand(netport 的文字已在平台 bbox
	// 内,该函数只对 netflag 返回非 nil,正合适)。
	moved := c
	moved.X, moved.Y = ax, ay
	moved.BBox = &box
	if rot, ok := destaggerRotationFor(c, dir); ok {
		moved.Rotation = &rot
	}
	if band := flagTextBand(moved); band != nil {
		box = layoutBBox{
			MinX: math.Min(box.MinX, band.MinX), MinY: math.Min(box.MinY, band.MinY),
			MaxX: math.Max(box.MaxX, band.MaxX), MaxY: math.Max(box.MaxY, band.MaxY),
		}
	}
	return box
}

// markerExtents 解出符号相对锚点的三个尺寸:沿桩向前伸/后伸/横向半宽。
func markerExtents(c layoutComp, dir string) (along, back, cross float64) {
	b := *c.BBox
	switch dir {
	case "up":
		return math.Max(0, b.MaxY-c.Y), math.Max(0, c.Y-b.MinY), math.Max(b.MaxX-c.X, c.X-b.MinX)
	case "down":
		return math.Max(0, c.Y-b.MinY), math.Max(0, b.MaxY-c.Y), math.Max(b.MaxX-c.X, c.X-b.MinX)
	case "right":
		return math.Max(0, b.MaxX-c.X), math.Max(0, c.X-b.MinX), math.Max(b.MaxY-c.Y, c.Y-b.MinY)
	default: // left
		return math.Max(0, c.X-b.MinX), math.Max(0, b.MaxX-c.X), math.Max(b.MaxY-c.Y, c.Y-b.MinY)
	}
}

// markerJudgeBBox 是 marker 参与碰撞判定的 bbox:符号本体 ∪ 文字带,与
// markerOverlapFindings 用的是同一套(否则"规划说不撞、check 说撞")。
func markerJudgeBBox(c layoutComp) layoutBBox {
	box := *c.BBox
	if band := flagTextBand(c); band != nil {
		box = layoutBBox{
			MinX: math.Min(box.MinX, band.MinX), MinY: math.Min(box.MinY, band.MinY),
			MaxX: math.Max(box.MaxX, band.MaxX), MaxY: math.Max(box.MaxY, band.MaxY),
		}
	}
	return box
}

// destaggerStep 是桩长递增的步长 —— **量出来的**:优先取该旗文字带的沿轴尺寸,
// 拿不到(netport/无 net class)时退到符号本体尺寸。加 5 余量避免新位置贴脸。
func destaggerStep(c layoutComp) float64 {
	if band := flagTextBand(c); band != nil {
		s := math.Max(band.MaxX-band.MinX, band.MaxY-band.MinY)
		if s > 0 {
			return s + 5
		}
	}
	if c.BBox != nil {
		s := math.Max(c.BBox.MaxX-c.BBox.MinX, c.BBox.MaxY-c.BBox.MinY)
		if s > 0 {
			return s + 5
		}
	}
	return 20
}

// destaggerSnap 把桩长圆整到连接器栅格,且至少留一格(0 长桩会让旗压在宿主上)。
func destaggerSnap(v float64) float64 {
	s := math.Round(v/destaggerGrid) * destaggerGrid
	if s < destaggerGrid {
		s = destaggerGrid
	}
	return s
}

// destaggerClassOf 把 marker 归到方向偏好类。
func destaggerClassOf(c layoutComp) string {
	if c.ComponentType == "netport" {
		return "port"
	}
	switch tidyNetClass(c.Net) {
	case "ground":
		return "ground"
	case "power":
		return "power"
	}
	return "port"
}

// destaggerClassRank 决定谁先挑方向:地 > 电 > 端口(与「电上地下」约定同源,
// 让最该守正位的先占)。
func destaggerClassRank(c layoutComp) int {
	switch destaggerClassOf(c) {
	case "ground":
		return 0
	case "power":
		return 1
	}
	return 2
}

// destaggerKindOf 反推 connect_pin 需要的 kind。netport 用最通用的双向口;
// 旗按网名分类走 ground/power(与 `sch connect --kind` 的取值同一张表)。
func destaggerKindOf(c layoutComp) string {
	if c.ComponentType == "netport" {
		return "net_port_bi"
	}
	switch tidyNetClass(c.Net) {
	case "ground":
		n := strings.ToUpper(strings.TrimSpace(c.Net))
		switch {
		case strings.HasPrefix(n, "AGND"):
			return "analog_ground"
		case strings.HasPrefix(n, "PGND"):
			return "protective_ground"
		}
		return "ground"
	case "power":
		return "power"
	}
	return "net_port_bi"
}

// destaggerRotationFor 给出该 marker 在某方向下的 stored rotation(真值表反查,
// 与 reversed-net-flag 判据共用 flagBodyRotation —— 判据和生成永远同一张表)。
func destaggerRotationFor(c layoutComp, dir string) (float64, bool) {
	family := ""
	switch tidyNetClass(c.Net) {
	case "power":
		family = "power"
	case "ground":
		family = "ground"
	default:
		return 0, false
	}
	table, ok := flagBodyRotation[family]
	if !ok {
		return 0, false
	}
	v, ok := table[dir]
	return v, ok
}

// destaggerPlanSummary 是给人读的一行摘要。
func destaggerPlanSummary(p destaggerPlan) string {
	return fmt.Sprintf("marker-overlap %d 条 → 计划搬迁 %d 个 marker,跳过 %d 个",
		p.OverlapsBefore, len(p.Moves), len(p.Skips))
}
