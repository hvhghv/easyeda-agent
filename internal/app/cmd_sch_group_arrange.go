package app

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// ── 第二层:组与组之间的排布(ADR-0003)──────────────────────────────────────
//
// 第一层(part→group)已经把每个块解成刚体。这一层把**组**当刚体排,输入输出与
// 第一层同构:一组刚体 + 它们之间的关系 + 一个边界 → 每个刚体的位置。
//
// **关系可以算,不需要块作者新声明**(ADR §4):两组之间的跨组信号网条数就是耦合
// 强度。电源/地不算 —— 它们连着所有人,没有任何区分度,拿它们算亲疏会把整页揉成
// 一团。

// bslGroupItem 是待排布的一个组:id、真实占地、当前锚点。
// **占地是实测的并集**(成员 + 桩线 + 旗),不是估算 —— 第一层封组时 marker 已经
// 挂上,所以组的 bbox 天然含 marker(ADR §5),这里直接量。
type bslGroupItem struct {
	ID      string
	Name    string
	BBox    layoutBBox
	Members []string
}

// bslGroupPlacement 是排布结果:该组应该整体平移多少。
type bslGroupPlacement struct {
	ID     string
	DX, DY float64
	Row    int
}

// groupOccupancy 量一个组的真实占地:成员本体 ∪ 它们的桩线 ∪ 桩线末端的 marker。
// 判定用的 marker 盒与 `sch check` 同尺(markerJudgeBBox = 本体 ∪ 文字带)——
// 「判定与生成同一把尺」这条定律在这一层同样成立。
func groupOccupancy(comps []layoutComp, wires []schGroupWire, members map[string]bool) (layoutBBox, bool) {
	var box layoutBBox
	got := false
	grow := func(b layoutBBox) {
		if !got {
			box, got = b, true
			return
		}
		box = layoutBBox{
			MinX: math.Min(box.MinX, b.MinX), MinY: math.Min(box.MinY, b.MinY),
			MaxX: math.Max(box.MaxX, b.MaxX), MaxY: math.Max(box.MaxY, b.MaxY),
		}
	}
	// 成员本体
	memberBoxes := make([]layoutBBox, 0, len(members))
	for _, c := range comps {
		if c.ComponentType != "part" || !members[strings.ToUpper(c.Designator)] || c.BBox == nil {
			continue
		}
		memberBoxes = append(memberBoxes, *c.BBox)
		grow(*c.BBox)
	}
	if !got {
		return box, false
	}
	// 桩线:任一端落在成员盒的引出范围内就算本组的
	const reach = 3 * schStubLen
	wireBox := func(w schGroupWire) (layoutBBox, bool) {
		if len(w.Points) < 4 {
			return layoutBBox{}, false
		}
		b := layoutBBox{MinX: w.Points[0], MinY: w.Points[1], MaxX: w.Points[0], MaxY: w.Points[1]}
		for i := 0; i+1 < len(w.Points); i += 2 {
			b.MinX = math.Min(b.MinX, w.Points[i])
			b.MaxX = math.Max(b.MaxX, w.Points[i])
			b.MinY = math.Min(b.MinY, w.Points[i+1])
			b.MaxY = math.Max(b.MaxY, w.Points[i+1])
		}
		return b, true
	}
	near := func(b layoutBBox) bool {
		for _, m := range memberBoxes {
			if b.MinX <= m.MaxX+reach && b.MaxX >= m.MinX-reach &&
				b.MinY <= m.MaxY+reach && b.MaxY >= m.MinY-reach {
				return true
			}
		}
		return false
	}
	for _, w := range wires {
		if b, ok := wireBox(w); ok && near(b) {
			grow(b)
		}
	}
	// marker:落在本组桩线可达范围内的
	for _, c := range comps {
		if !isSchMarker(c.ComponentType) || c.BBox == nil {
			continue
		}
		if jb := markerJudgeBBox(c); near(jb) {
			grow(jb)
		}
	}
	return box, true
}

// groupCouplings 数每一对组之间的**跨组信号网**条数。
//
// 为什么排除电源/地:它们连着页面上几乎每一个器件,如果计入耦合,任意两组都会
// 显得"强相关",排布退化成一团。真正决定谁挨着谁的是信号 —— USB 的 D+/D- 把
// 连接器和桥芯片绑在一起,DTR/RTS 把桥芯片和下载电路绑在一起。
func groupCouplings(live map[string]map[string]bool, groupOf map[string]string) map[string]int {
	out := map[string]int{}
	for net, pins := range live {
		if kind := bapFlagKind(net); kind == "gnd" || kind == "power" || kind == "agnd" || kind == "pgnd" {
			continue
		}
		touched := map[string]bool{}
		for ref := range pins {
			desig, _, ok := strings.Cut(ref, ".")
			if !ok {
				continue
			}
			if g := groupOf[strings.ToUpper(desig)]; g != "" {
				touched[g] = true
			}
		}
		if len(touched) < 2 {
			continue // 组内网,不产生耦合
		}
		gs := make([]string, 0, len(touched))
		for g := range touched {
			gs = append(gs, g)
		}
		sort.Strings(gs)
		for i := 0; i < len(gs); i++ {
			for j := i + 1; j < len(gs); j++ {
				out[gs[i]+"|"+gs[j]]++
			}
		}
	}
	return out
}

// couplingOf 读一对组的耦合强度(顺序无关)。
func couplingOf(coup map[string]int, a, b string) int {
	if a > b {
		a, b = b, a
	}
	return coup[a+"|"+b]
}

// orderGroupsByFlow 把组排成一条链:**耦合最强的相邻**。
//
// 贪心链式构造:从耦合总度数最小的组起头(它多半是链的一端 —— 接口/末梢),
// 每次接上与当前链尾耦合最强的那个。这不是最优哈密顿路径,但原理图上一页的组
// 数是个位数,贪心的结果与人工画法一致(USB→桥→下载→MCU),而且**确定性**。
func orderGroupsByFlow(items []bslGroupItem, coup map[string]int) []bslGroupItem {
	if len(items) <= 1 {
		return items
	}
	degree := map[string]int{}
	for _, it := range items {
		for _, other := range items {
			if it.ID != other.ID {
				degree[it.ID] += couplingOf(coup, it.ID, other.ID)
			}
		}
	}
	remaining := append([]bslGroupItem(nil), items...)
	sort.Slice(remaining, func(i, j int) bool {
		if degree[remaining[i].ID] != degree[remaining[j].ID] {
			return degree[remaining[i].ID] < degree[remaining[j].ID]
		}
		return remaining[i].ID < remaining[j].ID // 平手按 id,保证可复现
	})
	chain := []bslGroupItem{remaining[0]}
	remaining = remaining[1:]
	for len(remaining) > 0 {
		tail := chain[len(chain)-1].ID
		best, bestScore := 0, -1
		for i, cand := range remaining {
			s := couplingOf(coup, tail, cand.ID)
			if s > bestScore || (s == bestScore && remaining[i].ID < remaining[best].ID) {
				best, bestScore = i, s
			}
		}
		chain = append(chain, remaining[best])
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	return chain
}

// arrangeGroups 把排好序的组铺进 bounds:一行行从左到右、从上往下。
//
// **「填满纸张」不是目标**(ADR §7):区内紧凑、区间有隔。所以行内间距和行距都用
// 同一个可见间隙,放不下就换行,换行放不下就**明确返回放不下**,而不是硬塞或
// 溢出图纸 —— 每一层都要自己保证不出界(ADR §6)。
func arrangeGroups(ordered []bslGroupItem, bounds layoutBBox, gap float64) ([]bslGroupPlacement, error) {
	if len(ordered) == 0 {
		return nil, nil
	}
	usableW := bounds.MaxX - bounds.MinX
	usableH := bounds.MaxY - bounds.MinY
	var out []bslGroupPlacement
	// y 从上往下(y-UP:从 MaxY 开始往下走)
	cursorX, rowTop, rowH, row := bounds.MinX, bounds.MaxY, 0.0, 0
	for _, it := range ordered {
		w := it.BBox.MaxX - it.BBox.MinX
		h := it.BBox.MaxY - it.BBox.MinY
		if w > usableW || h > usableH {
			return nil, fmt.Errorf("组 %s 占地 %.0f×%.0f 比可用区 %.0f×%.0f 还大 —— 这一页放不下它,该拆到别的页",
				it.Name, w, h, usableW, usableH)
		}
		if cursorX+w > bounds.MaxX && cursorX > bounds.MinX {
			// 换行
			rowTop -= rowH + gap
			cursorX, rowH, row = bounds.MinX, 0, row+1
		}
		if rowTop-h < bounds.MinY {
			return nil, fmt.Errorf("排到第 %d 行时下边界不够了(还剩 %.0f,需要 %.0f)—— 这一页装不下这 %d 个组,拆页或缩减",
				row+1, rowTop-bounds.MinY, h, len(ordered))
		}
		// 目标位置:本组左上角对齐到 (cursorX, rowTop)。
		//
		// **位移必须吸附到 5 格**(schAnchorGrid):器件原本落在网格上,平移量若带小数,
		// 引脚就会落到格外,connect_pin 直接拒绝「Pin (612.5, 706.5) sits OFF the
		// 5-unit schematic grid」—— 重连全线失败。判定坐标必须等于落地坐标,这一层
		// 也不例外。吸附朝**向内**取(往可用区里挪),避免吸附本身把组推出边界。
		grid := float64(schAnchorGrid)
		dx := math.Floor((cursorX-it.BBox.MinX)/grid) * grid
		dy := math.Floor((rowTop-it.BBox.MaxY)/grid) * grid
		if it.BBox.MinX+dx < bounds.MinX {
			dx += grid
		}
		if it.BBox.MaxY+dy > bounds.MaxY {
			dy -= grid
		}
		out = append(out, bslGroupPlacement{ID: it.ID, DX: dx, DY: dy, Row: row})
		cursorX += w + gap
		if h > rowH {
			rowH = h
		}
	}
	return out, nil
}

// ── I/O 编排 ────────────────────────────────────────────────────────────────

// runGroupArrange 是第二层的落地入口:读全部组 → 量占地 → 算耦合 → 链式排序 →
// 铺进图纸可用区 → 用**已验证的刚体平移**逐个落位。
//
// 落地刻意复用 groupMoveRebuild(删净→modify→重连→电气自检)而不是另造:那条路径
// 的两条刚体判据(位移逐件一致、网表逐引脚不变)是真机验过的,自造一条等于把同一个
// 「带线搬必断线」的坑再踩一次。
func runGroupArrange(cfg *appConfig, window string, gap float64, dryRun bool, stdout, stderr io.Writer) error {
	pinned, win, docUUID, _, _, groups, err := loadSchGroupsContext(cfg, window)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return fmt.Errorf("本页没有虚拟组 —— 第二层排的是**组**;先用 `sch block-apply` 落块(会自动归组)或 `sch group create` 手工建组")
	}
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", win,
		map[string]any{"includeBBox": true, "includePins": true}, docUUID, "group-arrange 读场景")
	if err != nil {
		return fmt.Errorf("读场景:%w", err)
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return fmt.Errorf("解析场景:%w", err)
	}
	wires, werr := fetchSchWirePolylinesStable(cfg, win, docUUID)
	if werr != nil {
		fmt.Fprintf(stderr, "warn: 读不到导线(%v)—— 组的占地将不含桩线,可能算小\n", werr)
	}
	live, _, lerr := readLiveNets(pinned, win)
	if lerr != nil {
		return fmt.Errorf("读网表(组间关系的唯一依据):%w", lerr)
	}

	// 组 → 占地;位号 → 组
	groupOf := map[string]string{}
	var items []bslGroupItem
	for _, g := range groups {
		members := map[string]bool{}
		for _, m := range g.Members {
			up := strings.ToUpper(m)
			members[up] = true
			groupOf[up] = g.ID
		}
		box, ok := groupOccupancy(comps, wires, members)
		if !ok {
			fmt.Fprintf(stderr, "warn: 组 %s 在本页找不到任何成员器件,跳过(位号可能已过时)\n", describeSchGroup(g))
			continue
		}
		items = append(items, bslGroupItem{ID: g.ID, Name: describeSchGroup(g), BBox: box, Members: g.Members})
	}
	if len(items) == 0 {
		return fmt.Errorf("没有一个组能在本页找到成员 —— 用 `sch group list` 核对位号")
	}

	// 边界:图纸可用区减图签(每一层自己保证不出界,ADR §6)
	bounds, ok := arrangeBoundsOf(sheetBBoxOf(comps))
	if !ok {
		return fmt.Errorf("取不到图纸几何 —— 无法保证排布不越界,拒绝动手(先确认页面有图框)")
	}
	coup := groupCouplings(live, groupOf)
	ordered := orderGroupsByFlow(items, coup)
	plan, err := arrangeGroups(ordered, bounds, gap)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "第二层排布:%d 个组,可用区 [%.0f,%.0f]-[%.0f,%.0f],间隙 %.0f\n",
		len(ordered), bounds.MinX, bounds.MinY, bounds.MaxX, bounds.MaxY, gap)
	byID := map[string]bslGroupItem{}
	for _, it := range ordered {
		byID[it.ID] = it
	}
	for i, p := range plan {
		it := byID[p.ID]
		link := ""
		if i > 0 {
			link = fmt.Sprintf("  ← 与上一个共 %d 条跨组信号", couplingOf(coup, plan[i-1].ID, p.ID))
		}
		fmt.Fprintf(stdout, "  行%d  %-28s 占地 %.0f×%.0f  Δ=(%.0f,%.0f)%s\n",
			p.Row, it.Name, it.BBox.MaxX-it.BBox.MinX, it.BBox.MaxY-it.BBox.MinY, p.DX, p.DY, link)
	}
	if dryRun {
		fmt.Fprintln(stdout, "(dry-run:未改动画布;去掉 --dry-run 执行)")
		return nil
	}
	moved := 0
	for _, p := range plan {
		if p.DX == 0 && p.DY == 0 {
			continue // 已经在位
		}
		if err := groupMoveRebuild(cfg, window, p.ID, p.DX, p.DY, stdout, stderr); err != nil {
			return fmt.Errorf("落位组 %s:%w(已落位 %d 个,重跑本命令可继续)", p.ID, err, moved)
		}
		moved++
	}
	fmt.Fprintf(stdout, "✓ 第二层落地:%d 个组已按跨组信号关系排布\n", moved)
	return nil
}

// arrangeBoundsOf 把图纸几何换算成可用区:减去边距,再减去图签 keep-out 的那一条。
func arrangeBoundsOf(sheet *layoutBBox) (layoutBBox, bool) {
	if sheet == nil {
		return layoutBBox{}, false
	}
	b := layoutBBox{
		MinX: sheet.MinX + sheetEdgeMinGap, MinY: sheet.MinY + sheetEdgeMinGap,
		MaxX: sheet.MaxX - sheetEdgeMinGap, MaxY: sheet.MaxY - sheetEdgeMinGap,
	}
	// 图签在右下角:把可用区的下界抬到它上沿之上 —— 排布不与图签抢地方。
	// provisional(平台没暴露图签几何)时不强加一个猜出来的框,宁可不收窄。
	if ko, provisional := titleBlockKeepout(sheet); !provisional && ko != nil && ko.MaxY > b.MinY {
		b.MinY = ko.MaxY
	}
	return b, b.MaxX > b.MinX && b.MaxY > b.MinY
}
