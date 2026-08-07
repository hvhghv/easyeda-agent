package app

// pcb_board_snapshot.go — 板级快照：多维布局打分(#167)的统一数据层。
//
// 为什么需要它。在此之前每个消费者都自己发 `pcb.components.list` 再手工弱类型
// 解包：layout-lint 把解析内联在 runner 里(pcb_layoutlint.go)、place-constrained
// 有 parseCpComps、auto-place 有 parseApComps、check 的 antenna/zone 规则各拉一次
// ——全仓 21 处调用点，参数组合还各不相同。多维打分要同时看器件姿态 / 焊盘网表 /
// 丝印 / 板框 / 规则，逐维各拉一次会把一次打分放大成十几个往返，而精修环要反复
// 打分，这条会直接成为迭代瓶颈。
//
// 第二个理由更硬：**金标准好板回归**(#167 第五层)需要把真板落成离线 fixture。
// 现状 analyzePcbLayout 的输入解析在 runner 里，没有 `map[string]any → 结构体`
// 的纯函数缝，testdata 喂不进去。这里把「取数」和「解析」彻底分开：
//
//	fetchBoardSnapshot  = I/O（发 action）
//	parseBoard*         = 纯函数（可单测、可吃 testdata JSON）
//
// 快照本身带 json tag，`pcb snapshot --out board.json` 落盘的就是它，Go 单测
// os.ReadFile + json.Unmarshal 回来即可重放同一块板。
//
// 单位：全部 mil，y-UP（PCB 图元原生 mil；只有 DRC leaf 的 pos 是 mil/10，那条
// 在 pcb_drc_flat.go 里已归一，不流到这里）。

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 数据模型
// ---------------------------------------------------------------------------

// boardPad 是一个已放置焊盘。W/H 是连接器从焊盘形状元组抠出的真实铜箔尺寸，
// 0 = 未知（POLYGON 焊盘 padExtent 返回 null）——下游必须把 0 当"测不到"而不是
// "尺寸为零"。Layer 是焊盘的铜层：1=top / 2=bottom / 12=multi(通孔桶，每层导通)。
type boardPad struct {
	ID       string  `json:"primitiveId,omitempty"`
	Number   string  `json:"padNumber"`
	Net      string  `json:"net"`
	Layer    int     `json:"layer"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	W        float64 `json:"width,omitempty"`
	H        float64 `json:"height,omitempty"`
	Rotation float64 `json:"rotation,omitempty"`
	PadType  int     `json:"padType,omitempty"` // 0=NORMAL 1=TEST 2=MARK_POINT
}

// isThroughHole 报告焊盘是否为通孔（连接器不给 hole 字段，只能用 MULTI 层反推）。
func (p boardPad) isThroughHole() bool { return p.Layer == pcbLayerMulti }

// boardComp 是一个已放置封装的完整姿态。相对既有的 pcbLComp（只有
// designator/layer/bbox）多出 #153 整齐度维度必需的 X/Y/Rotation/Locked ——
// 连接器 serializePcbComponent 一直在返回这些字段，只是过去被 Go 侧丢弃了。
//
// X/Y 是封装 anchor（原点），**不是 bbox 中心**：两者的偏移随旋转变，所以判位置
// 一律用 center()，写坐标才用 anchor（对齐 `pcb modify --center` 的既有约定）。
type boardComp struct {
	ID         string      `json:"primitiveId"`
	Designator string      `json:"designator"`
	Name       string      `json:"name,omitempty"`
	Device     string      `json:"device,omitempty"` // manufacturerId 优先，见 cpDeviceName
	Layer      int         `json:"layer"`            // 装配面 1=top 2=bottom 0=unknown
	X          float64     `json:"x"`                // anchor，非中心
	Y          float64     `json:"y"`
	Rotation   float64     `json:"rotation"`
	Locked     bool        `json:"locked,omitempty"`
	BBox       *layoutBBox `json:"bbox,omitempty"` // 渲染包围盒（含丝印，非 IPC courtyard）
	Pads       []boardPad  `json:"pads,omitempty"`
}

// center 返回判位置该用的坐标：有 bbox 用 bbox 几何中心，否则退回 anchor。
func (c boardComp) center() (float64, float64) {
	if c.BBox != nil {
		return (c.BBox.MinX + c.BBox.MaxX) / 2, (c.BBox.MinY + c.BBox.MaxY) / 2
	}
	return c.X, c.Y
}

func (c boardComp) width() float64 {
	if c.BBox == nil {
		return 0
	}
	return c.BBox.MaxX - c.BBox.MinX
}

func (c boardComp) height() float64 {
	if c.BBox == nil {
		return 0
	}
	return c.BBox.MaxY - c.BBox.MinY
}

// area 是渲染包围盒面积（mil²）。注意这是**渲染** bbox 不是 IPC-7351 courtyard：
// 它把丝印外框算进去，实测常比封装本体大 40%+。任何用它做分母的密度/紧凑度指标
// 都会系统性偏大，阈值必须用真板校准（#167 第五层），不能照搬 KiCad 的经验值。
func (c boardComp) area() float64 { return c.width() * c.height() }

// nets 返回该器件涉及的全部非空网名（去重、排序，保证确定性）。
func (c boardComp) nets() []string {
	seen := map[string]bool{}
	for _, p := range c.Pads {
		if p.Net != "" {
			seen[p.Net] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// signalNets 是 nets() 去掉电源/地（全局网）后的剩余——功能分区聚类只该看信号网，
// GND 连着所有东西，拿它聚类会把整块板并成一坨。
func (c boardComp) signalNets() []string {
	out := make([]string, 0, len(c.Pads))
	for _, n := range c.nets() {
		if !isGlobalNet(n) {
			out = append(out, n)
		}
	}
	return out
}

// 板框的边沿用 apEdge（pcb_autoplace.go:171）——auto-place / place-constrained
// 已经在用这套词汇选边，打分侧再造一套 left/right/top/bottom 只会让两边长期分叉。
// y-UP：edgeTop = 高 Y。

// boardOutline 是板框。Points 是真实闭合多边形（顺序点列，mil）；连接器旧版本
// 或读不到时为空，此时 Source=="bbox" 且所有几何退化成 AABB。
//
// 为什么要区分：#168 的 internal-on-edge 判据是「到板框外沿的距离」，异形板
// （Type-C 突出 / 切角 / 挖槽）用 AABB 算会系统性算错——突出部位的件明明贴边，
// AABB 看却离边很远。降级时下游必须把这件事标出来（outlineSource 进报告），
// 而不是假装算准了。
type boardOutline struct {
	BBox   layoutBBox   `json:"bbox"`
	Points [][2]float64 `json:"points,omitempty"`
	Source string       `json:"source"` // "polygon" | "bbox"
}

func (o *boardOutline) width() float64  { return o.BBox.MaxX - o.BBox.MinX }
func (o *boardOutline) height() float64 { return o.BBox.MaxY - o.BBox.MinY }

// area 是板面积：有多边形用鞋带公式，否则 AABB 面积。
func (o *boardOutline) area() float64 {
	if len(o.Points) >= 3 {
		var s float64
		for i := range o.Points {
			j := (i + 1) % len(o.Points)
			s += o.Points[i][0]*o.Points[j][1] - o.Points[j][0]*o.Points[i][1]
		}
		if a := math.Abs(s) / 2; a > 0 {
			return a
		}
	}
	return o.width() * o.height()
}

// longAxis 返回板框较长的轴（"x" 或 "y"）——flow-order 维在 spec 没显式声明
// flowAxis 时的默认信号流向轴。
func (o *boardOutline) longAxis() string {
	if o.width() >= o.height() {
		return "x"
	}
	return "y"
}

// distToEdge 是点到板框边界的最短距离（板内为正，板外为负）。多边形可用时走真实
// 边界，否则退回 AABB 的四向轴距。
func (o *boardOutline) distToEdge(x, y float64) float64 {
	if len(o.Points) >= 3 {
		d := math.Inf(1)
		for i := range o.Points {
			j := (i + 1) % len(o.Points)
			d = math.Min(d, segPtDist(x, y, o.Points[i][0], o.Points[i][1], o.Points[j][0], o.Points[j][1]))
		}
		if !o.containsPoint(x, y) {
			return -d
		}
		return d
	}
	inside := x >= o.BBox.MinX && x <= o.BBox.MaxX && y >= o.BBox.MinY && y <= o.BBox.MaxY
	d := math.Min(
		math.Min(math.Abs(x-o.BBox.MinX), math.Abs(o.BBox.MaxX-x)),
		math.Min(math.Abs(y-o.BBox.MinY), math.Abs(o.BBox.MaxY-y)),
	)
	if !inside {
		return -rectPtDist(o.BBox.MinX, o.BBox.MinY, o.BBox.MaxX, o.BBox.MaxY, x, y)
	}
	return d
}

// nearestEdge 返回离点最近的那条 AABB 边及其轴向距离。即使有多边形也按 AABB 判
// 「哪条边」——边的**名字**（left/right/top/bottom）本来就是相对整板朝向说的，
// 用局部多边形边算出来的法向反而会把突出部位的件判到奇怪的边上。
func (o *boardOutline) nearestEdge(x, y float64) (apEdge, float64) {
	cands := []struct {
		e apEdge
		d float64
	}{
		{edgeLeft, x - o.BBox.MinX},
		{edgeRight, o.BBox.MaxX - x},
		{edgeBottom, y - o.BBox.MinY},
		{edgeTop, o.BBox.MaxY - y},
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if c.d < best.d {
			best = c
		}
	}
	return best.e, best.d
}

// containsPoint 是多边形内外判定（射线法）。仅在 Points 可用时有意义。
func (o *boardOutline) containsPoint(x, y float64) bool {
	if len(o.Points) < 3 {
		return x >= o.BBox.MinX && x <= o.BBox.MaxX && y >= o.BBox.MinY && y <= o.BBox.MaxY
	}
	in := false
	for i, j := 0, len(o.Points)-1; i < len(o.Points); j, i = i, i+1 {
		xi, yi := o.Points[i][0], o.Points[i][1]
		xj, yj := o.Points[j][0], o.Points[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

// boardSnapshot 是一次拉齐的板级只读视图。任何多维打分/规则都该消费它，而不是
// 各自再发一遍 action。Partial 记录哪些部分没取到（后台文档 / 旧连接器 / 平台
// 报错），下游据此降级并在报告里说明——**绝不能因为某段数据缺失就静默跳过一整维
// 却仍然给满分**，那会让"好板得高分"的校准判据失去意义。
type boardSnapshot struct {
	Components   []boardComp   `json:"components"`
	Outline      *boardOutline `json:"outline,omitempty"`
	Silk         []pcbSilkText `json:"silk,omitempty"`
	CopperLayers int           `json:"copperLayers,omitempty"`
	Rules        *boardRules   `json:"rules,omitempty"`
	Partial      []string      `json:"partial,omitempty"`
	CapturedAt   string        `json:"capturedAt,omitempty"`
	Project      string        `json:"project,omitempty"`
}

// boardRules 是 pcbRules 的可序列化投影（pcbRules 字段不导出，进不了 fixture）。
type boardRules struct {
	ClearanceMil     float64 `json:"clearanceMil"`
	TrackWidthMil    float64 `json:"trackWidthMil"`
	PowerWidthMil    float64 `json:"powerWidthMil"`
	TrackWidthMinMil float64 `json:"trackWidthMinMil"`
	ViaDrillMil      float64 `json:"viaDrillMil"`
	ViaDiameterMil   float64 `json:"viaDiameterMil"`
	CopperToEdgeMil  float64 `json:"copperToEdgeMil"`
	Source           string  `json:"source"` // live | fallback
}

func rulesToBoard(r pcbRules) *boardRules {
	return &boardRules{
		ClearanceMil: r.clearanceMil, TrackWidthMil: r.trackWidthMil,
		PowerWidthMil: r.powerWidthMil, TrackWidthMinMil: r.trackWidthMinMil,
		ViaDrillMil: r.viaDrillMil, ViaDiameterMil: r.viaDiameterMil,
		CopperToEdgeMil: r.copperToEdgeMil, Source: r.source,
	}
}

func (b *boardRules) toPcbRules() pcbRules {
	if b == nil {
		return defaultPcbRules()
	}
	return pcbRules{
		clearanceMil: b.ClearanceMil, trackWidthMil: b.TrackWidthMil,
		powerWidthMil: b.PowerWidthMil, trackWidthMinMil: b.TrackWidthMinMil,
		viaDrillMil: b.ViaDrillMil, viaDiameterMil: b.ViaDiameterMil,
		copperToEdgeMil: b.CopperToEdgeMil, source: b.Source,
	}
}

// ---------------------------------------------------------------------------
// 派生视图 —— 投影给既有纯核，避免把 overlap/short/ratsnest 再实现一遍
// ---------------------------------------------------------------------------

// toLayoutComps 把快照投影成 analyzePcbLayout 的入参。layout-score 的
// overlap / tight / off-board / short / ratsnest 交叉几维直接复用 layout-lint
// 的纯核，不重复实现（也就不会出现两套引擎长期给矛盾答案的老问题）。
func (s *boardSnapshot) toLayoutComps() ([]pcbLComp, []pcbLPad) {
	comps := make([]pcbLComp, 0, len(s.Components))
	var pads []pcbLPad
	for _, c := range s.Components {
		comps = append(comps, pcbLComp{Designator: c.Designator, Layer: c.Layer, BBox: c.BBox})
		for _, p := range c.Pads {
			pads = append(pads, pcbLPad{
				Designator: c.Designator, Number: p.Number, Net: p.Net,
				Layer: p.Layer, X: p.X, Y: p.Y, W: p.W, H: p.H,
			})
		}
	}
	return comps, pads
}

// toCheckPads 投影成 pcb check 规则族的焊盘入参（decap-too-far / via-in-pad …）。
func (s *boardSnapshot) toCheckPads() []pcbPadP {
	var out []pcbPadP
	for _, c := range s.Components {
		for _, p := range c.Pads {
			out = append(out, pcbPadP{
				Designator: c.Designator, Number: p.Number, Net: p.Net,
				Layer: p.Layer, X: p.X, Y: p.Y, W: p.W, H: p.H,
			})
		}
	}
	return out
}

// byDesignator 建位号索引（同名取第一个；平台不该产生重名，但真板上出现过）。
func (s *boardSnapshot) byDesignator() map[string]boardComp {
	m := make(map[string]boardComp, len(s.Components))
	for _, c := range s.Components {
		if _, dup := m[c.Designator]; !dup {
			m[c.Designator] = c
		}
	}
	return m
}

// netPads 按网名分桶所有焊盘（含全局网——调用方自己决定要不要滤）。
func (s *boardSnapshot) netPads() map[string][]boardPad {
	m := map[string][]boardPad{}
	for _, c := range s.Components {
		for _, p := range c.Pads {
			if p.Net == "" {
				continue
			}
			p.Number = c.Designator + "." + p.Number // 让桶里的焊盘可回指器件
			m[p.Net] = append(m[p.Net], p)
		}
	}
	return m
}

// netMembers 按网名给出参与该网的器件位号集合（去重排序）。功能分区聚类的原料。
func (s *boardSnapshot) netMembers() map[string][]string {
	m := map[string]map[string]bool{}
	for _, c := range s.Components {
		for _, n := range c.nets() {
			if m[n] == nil {
				m[n] = map[string]bool{}
			}
			m[n][c.Designator] = true
		}
	}
	out := make(map[string][]string, len(m))
	for n, set := range m {
		lst := make([]string, 0, len(set))
		for d := range set {
			lst = append(lst, d)
		}
		sort.Strings(lst)
		out[n] = lst
	}
	return out
}

// silkByComponent 按父器件 primitiveId 分组丝印文本（位号方位/字号一致性用）。
func (s *boardSnapshot) silkByComponent() map[string][]pcbSilkText {
	m := map[string][]pcbSilkText{}
	for _, t := range s.Silk {
		if t.CompID == "" {
			continue
		}
		m[t.CompID] = append(m[t.CompID], t)
	}
	return m
}

// hasOutlinePolygon 报告板框是否为真实多边形（而非退化 AABB）。异形板上任何
// 「到板边」类判据都必须先问这一句，答案是 false 时要在 finding 里降级说明。
func (s *boardSnapshot) hasOutlinePolygon() bool {
	return s.Outline != nil && len(s.Outline.Points) >= 3
}

// note 追加一条降级说明（去重）。
func (s *boardSnapshot) note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, e := range s.Partial {
		if e == msg {
			return
		}
	}
	s.Partial = append(s.Partial, msg)
}

// ---------------------------------------------------------------------------
// 纯解析器 —— 可离线单测，可吃 testdata JSON
// ---------------------------------------------------------------------------

// parseBoardComponents 把 `pcb.components.list {includeBBox,includePads}` 的
// result 解成 []boardComp。纯函数：喂 testdata 里的真板 dump 即可离线重放。
//
// 与既有 parseApComps/parseCpComps 的关系：那两个各自只取自己要的子集且丢弃
// rotation/locked（apComp 有 locked 但无 device，cpComp 有 device 但走两趟合并）。
// 这里一次取全，是它们的超集。
func parseBoardComponents(result map[string]any) []boardComp {
	raw, _ := result["components"].([]any)
	out := make([]boardComp, 0, len(raw))
	for _, ri := range raw {
		cm, ok := ri.(map[string]any)
		if !ok {
			continue
		}
		c := boardComp{
			ID:         asString(cm["primitiveId"]),
			Designator: asString(cm["designator"]),
			Name:       asString(cm["name"]),
			Device:     cpDeviceName(cm),
		}
		if v, ok := asFloatOK(cm["layer"]); ok {
			c.Layer = int(v)
		}
		c.X, _ = asFloatOK(cm["x"])
		c.Y, _ = asFloatOK(cm["y"])
		c.Rotation, _ = asFloatOK(cm["rotation"])
		c.Locked, _ = cm["locked"].(bool)
		if bb, ok := cm["bbox"].(map[string]any); ok {
			minX, ok1 := asFloatOK(bb["minX"])
			minY, ok2 := asFloatOK(bb["minY"])
			maxX, ok3 := asFloatOK(bb["maxX"])
			maxY, ok4 := asFloatOK(bb["maxY"])
			if ok1 && ok2 && ok3 && ok4 {
				c.BBox = &layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
			}
		}
		if rawPads, ok := cm["pads"].([]any); ok {
			for _, rp := range rawPads {
				pm, ok := rp.(map[string]any)
				if !ok {
					continue
				}
				p := boardPad{
					ID:     asString(pm["primitiveId"]),
					Number: asString(pm["padNumber"]),
					Net:    asString(pm["net"]),
				}
				if v, ok := asFloatOK(pm["layer"]); ok {
					p.Layer = int(v)
				}
				p.X, _ = asFloatOK(pm["x"])
				p.Y, _ = asFloatOK(pm["y"])
				// 0 = 连接器测不到（POLYGON 焊盘）。保持 0，下游用 halfExt 兜底，
				// 不在这里瞎猜一个尺寸——猜出来的短路判定比没有更危险。
				p.W, _ = asFloatOK(pm["width"])
				p.H, _ = asFloatOK(pm["height"])
				p.Rotation, _ = asFloatOK(pm["rotation"])
				if v, ok := asFloatOK(pm["padType"]); ok {
					p.PadType = int(v)
				}
				c.Pads = append(c.Pads, p)
			}
		}
		out = append(out, c)
	}
	return out
}

// parseBoardOutline 解析 `pcb.outline.get` 的 result。优先吃 points（连接器新增
// 的真实多边形，#167）；没有就退回 bbox 并把 Source 标成 "bbox"，让下游知道
// 自己算的是 AABB 近似而不是真板形状。
func parseBoardOutline(result map[string]any) *boardOutline {
	if result == nil {
		return nil
	}
	o := &boardOutline{Source: "bbox"}
	bbOK := false
	if bb, ok := mnav(result, "bbox").(map[string]any); ok {
		minX, ok1 := asFloatOK(bb["minX"])
		minY, ok2 := asFloatOK(bb["minY"])
		maxX, ok3 := asFloatOK(bb["maxX"])
		maxY, ok4 := asFloatOK(bb["maxY"])
		if ok1 && ok2 && ok3 && ok4 && maxX > minX && maxY > minY {
			o.BBox = layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
			bbOK = true
		}
	}
	if pts, ok := result["points"].([]any); ok {
		for _, pi := range pts {
			pair, ok := pi.([]any)
			if !ok || len(pair) < 2 {
				continue
			}
			x, xok := asFloatOK(pair[0])
			y, yok := asFloatOK(pair[1])
			if xok && yok {
				o.Points = append(o.Points, [2]float64{x, y})
			}
		}
	}
	if len(o.Points) >= 3 {
		o.Source = "polygon"
		if !bbOK { // 连接器只给了点列时自己算 AABB
			minX, minY := math.Inf(1), math.Inf(1)
			maxX, maxY := math.Inf(-1), math.Inf(-1)
			for _, p := range o.Points {
				minX, minY = math.Min(minX, p[0]), math.Min(minY, p[1])
				maxX, maxY = math.Max(maxX, p[0]), math.Max(maxY, p[1])
			}
			o.BBox = layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
			bbOK = true
		}
	} else {
		o.Points = nil // <3 点的退化点列没有几何意义，丢掉别让下游误用
	}
	if !bbOK {
		return nil
	}
	return o
}

// ---------------------------------------------------------------------------
// 取数
// ---------------------------------------------------------------------------

// boardSnapshotOpts 选择要拉哪些部分。默认（零值）只拉器件+板框，这是所有
// 布局维度的最小集；丝印/规则/层数按需开，省往返。
type boardSnapshotOpts struct {
	withSilk   bool
	withRules  bool
	withLayers bool
}

// fetchBoardSnapshot 一次拉齐板级只读视图。任何一段失败都只记进 Partial 并继续
// ——照 gatherPcbCheckReport 的既有容错口径（一段读不到不该让整份审计失败），
// 但与之不同的是这里把降级**显式记录**下来而不是只往 stderr 打一行 warning，
// 因为打分报告必须能说明"这一维为什么没算"。
func fetchBoardSnapshot(cfg *appConfig, window string, opts boardSnapshotOpts) (*boardSnapshot, error) {
	snap := &boardSnapshot{CapturedAt: time.Now().UTC().Format(time.RFC3339)}

	res, err := requestAction(cfg, "pcb.components.list", window,
		map[string]any{"includeBBox": true, "includePads": true})
	if err != nil {
		return nil, fmt.Errorf("fetch PCB components: %w", err)
	}
	snap.Components = parseBoardComponents(res.Result)
	if len(snap.Components) == 0 {
		snap.note("no components on the board")
	}
	missingBBox := 0
	for _, c := range snap.Components {
		if c.BBox == nil {
			missingBBox++
		}
	}
	if missingBBox > 0 {
		snap.note("%d/%d component(s) have no rendered bbox — geometric dimensions degrade to anchor points", missingBBox, len(snap.Components))
	}

	// 板框：pcb.outline.get 在 PCB 非前台时返 null（既有坑），此时 outline 为 nil，
	// 所有「到板边」维度必须降级而不是当成 0 距离。
	if ores, oerr := requestAction(cfg, "pcb.outline.get", window, nil); oerr == nil && ores != nil {
		snap.Outline = parseBoardOutline(ores.Result)
	}
	if snap.Outline == nil {
		snap.note("board outline unavailable (is the PCB the foreground document?) — edge/off-board dimensions skipped")
	} else if snap.Outline.Source != "polygon" {
		snap.note("board outline is an AABB approximation (connector returned no polygon) — edge distances on a non-rectangular board are approximate")
	}

	if opts.withSilk {
		silk, serr := fetchPcbSilk(cfg, window)
		if serr != nil {
			snap.note("silkscreen unreadable (%v) — silk consistency dimensions skipped", serr)
		} else {
			snap.Silk = silk
		}
	}
	if opts.withRules {
		snap.Rules = rulesToBoard(fetchPcbRules(cfg, window))
	}
	if opts.withLayers {
		if n, lerr := fetchCopperLayerCount(cfg, window); lerr == nil {
			snap.CopperLayers = n
		} else {
			snap.note("copper layer count unreadable (%v)", lerr)
		}
	}
	return snap, nil
}

// fetchCopperLayerCount 读铜层数（叠层相关维度用）。pcb.layers.list 返回全部层，
// 铜层是 type=="SIGNAL" 或 "PLANE" 的那些。
func fetchCopperLayerCount(cfg *appConfig, window string) (int, error) {
	res, err := requestAction(cfg, "pcb.layers.list", window, nil)
	if err != nil {
		return 0, err
	}
	raw, _ := mnav(res.Result, "layers").([]any)
	n := 0
	for _, li := range raw {
		lm, ok := li.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToUpper(asString(lm["type"])) {
		case "SIGNAL", "PLANE":
			n++
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("no copper layers reported")
	}
	return n, nil
}

// loadBoardSnapshotFile 从磁盘读回一份快照（金标准回归 / --from 离线打分）。
func loadBoardSnapshotFile(r io.Reader) (*boardSnapshot, error) {
	var snap boardSnapshot
	dec := json.NewDecoder(r)
	if err := dec.Decode(&snap); err != nil {
		return nil, fmt.Errorf("parse board snapshot: %w", err)
	}
	return &snap, nil
}
