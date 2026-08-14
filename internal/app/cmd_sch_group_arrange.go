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
	ID   string
	Name string
	// BBox 是**排布用的完整占地**:器件 + marker + 区框 + 说明带。第二层排的就是它,
	// 所以区框和说明的地方在求解时就被留出来了(ADR §2:注释与器件同级)。
	BBox layoutBBox
	// DeviceBox 是器件 + marker 的包络(不含区框/说明),画框时用。
	DeviceBox layoutBBox
	NoteLines []string
	Members   []string
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
func runGroupArrange(cfg *appConfig, window string, gap float64, dryRun, annotate bool, stdout, stderr io.Writer) error {
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
		// 先按**器件宽度**折行,再算占地:说明是功能区里的成员,该去适应电路的宽度,
		// 而不是反过来把框撑到一页装不下。
		notes := wrapNoteLines(groupNoteLinesFor(g.Name, annotate), box.MaxX-box.MinX)
		items = append(items, bslGroupItem{
			ID: g.ID, Name: describeSchGroup(g),
			BBox:      groupAnnotatedExtent(box, notes),
			DeviceBox: box, NoteLines: notes, Members: g.Members,
		})
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
	if annotate {
		if err := drawGroupAnnotations(cfg, win, docUUID, plan, byID, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "warn: 区框/说明未画上(%v)—— 器件位置已经落好,空间也留出来了,补画即可\n", err)
		}
	}
	return nil
}

// groupNoteLinesFor 取一个组的电路说明。组名形如 "ch340c_usb_serial(C7)",
// 前缀就是块 id —— block 是**虚拟组的配方**(ADR §1),说明自然从配方来。
func groupNoteLinesFor(name string, annotate bool) []string {
	if !annotate || name == "" {
		return nil
	}
	id := name
	if i := strings.IndexByte(id, '('); i > 0 {
		id = id[:i]
	}
	b, ok, err := blocks.Get(id)
	if err != nil || !ok {
		return nil
	}
	// schematic_notes 在 schema 里有,Go 结构没解析 —— 直接从 Raw 取,不为一个
	// 可选字段动块的公共类型。
	var raw struct {
		Notes []string `json:"schematic_notes"`
	}
	if err := json.Unmarshal(b.Raw, &raw); err != nil {
		return nil
	}
	var out []string
	for _, n := range raw.Notes {
		if s := strings.TrimSpace(n); s != "" {
			out = append(out, s)
		}
		if len(out) >= 2 { // 一个模块 1~2 行,再多就成了文档
			break
		}
	}
	return out
}

// wrapNoteLines 把说明折成不超过 maxWidth 的短行(与 noteSizeOf 同一把尺:
// CJK 全宽、ASCII 半宽)。maxWidth 太小时至少保证每行有内容,不做无限切碎。
func wrapNoteLines(notes []string, maxWidth float64) []string {
	if maxWidth < 120 {
		maxWidth = 120 // 器件很窄时也别切成一字一行
	}
	var out []string
	for _, n := range notes {
		runes := []rune(n)
		line, w := make([]rune, 0, len(runes)), 0.0
		for _, r := range runes {
			rw := groupNoteFontSize * 0.55
			if r > 0x2E80 { // CJK 全宽,与 noteSizeOf 同口径
				rw = groupNoteFontSize
			}
			if w+rw > maxWidth && len(line) > 0 {
				out = append(out, string(line))
				line, w = line[:0], 0
			}
			line = append(line, r)
			w += rw
		}
		if len(line) > 0 {
			out = append(out, string(line))
		}
	}
	return out
}

// drawGroupAnnotations 画每个组的区框 + 组名 + 说明。
// **幂等**:画之前先删掉这些组上一次画的(id 记在组的持久状态里)—— 平台不提供
// 矩形/文本的枚举接口,不自己记就没法清理,重排一次多一层框。
func drawGroupAnnotations(cfg *appConfig, win, docUUID string, plan []bslGroupPlacement,
	byID map[string]bslGroupItem, stdout, stderr io.Writer) error {

	st, err := loadPcbStageState(resolveStageProjectQuiet(cfg, win))
	if err != nil {
		return fmt.Errorf("取分组状态:%w", err)
	}
	groups := st.GroupsForPage(docUUID)
	var stale []string
	for _, g := range groups {
		stale = append(stale, g.Annotations...)
	}
	if len(stale) > 0 {
		if _, err := requestAutolayoutAction(cfg, "schematic.primitives.delete", win,
			map[string]any{"primitiveIds": stale}, docUUID, "清除上一次的区框/说明"); err != nil {
			fmt.Fprintf(stderr, "warn: 上一次的区框没删掉(%v)—— 可能出现重叠的框\n", err)
		}
	}

	var draws []groupAnnotationDraw
	for _, p := range plan {
		it := byID[p.ID]
		full := layoutBBox{
			MinX: it.BBox.MinX + p.DX, MinY: it.BBox.MinY + p.DY,
			MaxX: it.BBox.MaxX + p.DX, MaxY: it.BBox.MaxY + p.DY,
		}
		rect := groupFrameOf(full, len(it.NoteLines))
		draws = append(draws, groupAnnotationDraw{
			GroupID: p.ID, Rect: rect, Label: it.Name,
			LabelX: rect.MinX + 4, LabelY: rect.MaxY + 4,
			NoteLines: it.NoteLines,
		})
	}
	if len(draws) == 0 {
		return nil
	}
	res, err := requestActionTimed(cfg, "debug.exec_js", win,
		map[string]any{"code": buildGroupAnnotationJS(draws, "#7B7B7B", 12)}, 30*time.Second)
	if err != nil {
		return err
	}
	created, drawErr := parseAnnotationIDs(res.Result)
	if drawErr != nil {
		// 自清理已经把半成品删干净了 —— 这里必须**报错**而不是接着说"已画"。
		// 第一版只解析 rects/texts 不看 ok,于是脚本抛错、cleanup 删光之后,
		// 命令照样打印「✓ 2 个组已画」,而画布上一个框都没有。
		return drawErr
	}
	// 把新 id 记回组:下一次重排才删得掉。逐组记不了(exec_js 只回总表),
	// 全部挂到第一个组上 —— 清理是整批做的,归属不影响正确性。
	if len(groups) > 0 {
		for _, g := range groups {
			g.Annotations = nil
		}
		groups[0].Annotations = created
		if err := saveSchGroups(st, docUUID, groups); err != nil {
			fmt.Fprintf(stderr, "warn: 区框 id 没记下来(%v)—— 下次重排会叠一层框,手工删\n", err)
		}
	}
	fmt.Fprintf(stdout, "✓ 区框与说明:%d 个组已画(空间在排布时已计入,不是事后捡缝)\n", len(draws))
	return nil
}

// parseAnnotationIDs 从 exec_js 结果里取出创建的 primitiveId,**并判定这一次绘制
// 到底成没成**。epilogue 的 catch 路径会先把已创建的图元删干净再返回
// {ok:false, error, ...},所以只看 rects/texts 会把一次彻底失败读成成功。
func parseAnnotationIDs(result map[string]any) ([]string, error) {
	v, ok := result["value"]
	if !ok {
		return nil, fmt.Errorf("绘制脚本没有返回值")
	}
	var payload struct {
		OK              bool     `json:"ok"`
		Error           string   `json:"error"`
		Rects           []string `json:"rects"`
		Texts           []string `json:"texts"`
		CleanupSurvived []string `json:"cleanupSurvived"`
	}
	var raw []byte
	switch t := v.(type) {
	case string:
		raw = []byte(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("绘制结果无法解析:%w", err)
		}
		raw = b
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("绘制结果无法解析:%w", err)
	}
	if !payload.OK {
		msg := payload.Error
		if msg == "" {
			msg = "未知错误"
		}
		if len(payload.CleanupSurvived) > 0 {
			return nil, fmt.Errorf("绘制失败(%s);自清理没删干净,残留 %d 个图元需手工删除", msg, len(payload.CleanupSurvived))
		}
		return nil, fmt.Errorf("绘制失败(%s);已自动清理,画布未留残件", msg)
	}
	return append(payload.Rects, payload.Texts...), nil
}

// resolveStageProjectQuiet 取项目名,失败返回空(调用方已有降级路径)。
func resolveStageProjectQuiet(cfg *appConfig, win string) string {
	p, err := resolveStageProject(cfg, win)
	if err != nil {
		return ""
	}
	return p
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

// ── P2:区框与说明是**同级占位对象**(ADR-0003 §2)────────────────────────────
//
// 用户的原话:「每个编组对象还有 title 注释 属于同级别的,他们在计算摆放位置的
// 时候可以计算现有的虚拟组的 xy 位置和他的长宽碰撞,计算出来对齐和层叠方式」。
//
// 关键不是「画得好看」,而是**排布时就把它们的地方留出来**。事后画的框只能捡缝,
// 缝不够就压电路 —— 那正是 `sch note` / `sch zone-draw` 今天的处境。
//
// 硬约束:平台**不给文本 bbox**(只能按字数估)、**矩形连枚举接口都没有**,所以这些
// 几何只能由求解器自己持有(画完把 primitiveId 记进组),不能靠读回。

const (
	// groupFramePad 是区框离器件包络的距离:框贴着器件会像是器件的一部分。
	groupFramePad = 20.0
	// groupLabelBand 是框顶标签带的高度(组名)。
	groupLabelBand = 22.0
	// groupNoteLine 是说明每行的高度。
	groupNoteLine = 16.0
	// groupNoteFontSize 是说明字号 —— 宽度估算与绘制必须用同一个值,
	// 否则算出来的框装不下画出来的字(判定与生成同一把尺)。
	groupNoteFontSize = 10.2
)

// groupAnnotatedExtent 把器件包络扩成**连同区框和说明一起**的完整占地。
// 排布用的是这个 —— 于是框和说明的空间在第二层求解时就被计入,而不是画的时候
// 才发现没地方。
func groupAnnotatedExtent(deviceBox layoutBBox, notes []string) layoutBBox {
	b := layoutBBox{
		MinX: deviceBox.MinX - groupFramePad,
		MinY: deviceBox.MinY - groupFramePad,
		MaxX: deviceBox.MaxX + groupFramePad,
		MaxY: deviceBox.MaxY + groupFramePad + groupLabelBand, // 顶上给组名留一条
	}
	if len(notes) > 0 {
		b.MinY -= float64(len(notes)) * groupNoteLine // 框内下部给说明留一条带
	}
	// **说明不许把框撑宽**。一行「交叉耦合真值表…」比四个三极管加起来还长,让框
	// 跟着它变宽,两个组就一页装不下了(真机:换行后第二行差 6 个单位报「装不下」)。
	// 人工画法里说明本来就是几行短句 —— 所以调用方先按器件宽度折行(wrapNoteLines),
	// 这里只做兜底:万一还是更宽,才让框跟上,总比文字溢到框外强。
	widest := 0.0
	for _, n := range notes {
		if w, _ := noteSizeOf(n, groupNoteFontSize); w > widest {
			widest = w
		}
	}
	if need := b.MinX + widest + 2*groupFramePad; need > b.MaxX {
		b.MaxX = need
	}
	return b
}

// groupFrameOf 就是**完整占地本身** —— 框把标题、器件、说明**全包进去**。
//
// 曾经把标签带和说明带从框里减掉,于是说明落在框外面,读图的人看到的是「一个框
// 旁边飘着两行字」,而不是「这个功能区在说什么」。用户的判词:标题和 note
// **身份等同于虚拟组**,是功能区之下的成员,得跟器件一起待在框里。
func groupFrameOf(full layoutBBox, noteLines int) layoutBBox {
	return full
}

// groupNoteYFor 是框内第 i 行说明的基线:贴着框的下沿往上排,顺序与阅读一致
// (第 0 行在最上)。说明带的高度在 groupAnnotatedExtent 里已经让出来了,
// 所以这里落下去不会压到器件。
func groupNoteYFor(frame layoutBBox, i, total int) float64 {
	return frame.MinY + float64(total-i)*groupNoteLine - groupNoteLine*0.75
}

// buildGroupAnnotationJS 画一组的区框 + 组名 + 说明,并返回创建出来的 primitiveId。
// 复用 zone-draw 的自清理前后缀:中途抛错会把已创建的图元删干净,不留半张框。
func buildGroupAnnotationJS(frames []groupAnnotationDraw, color string, fontSize float64) string {
	var b strings.Builder
	writeZoneDrawPrelude(&b)
	colorJS := fmt.Sprintf("%q", color)
	for _, f := range frames {
		w, h := f.Rect.MaxX-f.Rect.MinX, f.Rect.MaxY-f.Rect.MinY
		if w <= 0 || h <= 0 {
			continue
		}
		// 参数与 zone-draw 同源(sch_PrimitiveRectangle.create 的 y 取**上沿**)。
		fmt.Fprintf(&b, "{ const rc = await eda.sch_PrimitiveRectangle.create(%g, %g, %g, %g, 0, 0, %s, null, 1, 1);\n",
			f.Rect.MinX, f.Rect.MaxY, w, h, colorJS)
		fmt.Fprintf(&b, "  if (rc) { const rid = rc.getState_PrimitiveId(); if (rid) rects.push(rid); } }\n")
		// 组名:贴在框内左上角(文本锚点在左下、向上生长)
		fmt.Fprintf(&b, "{ const tx = await eda.sch_PrimitiveText.create(%g, %g, %q, 0, %s, null, %g);\n",
			f.Rect.MinX+4, f.Rect.MaxY-fontSize-4, f.Label, colorJS, fontSize)
		fmt.Fprintf(&b, "  if (tx) { const tid = tx.getState_PrimitiveId(); if (tid) texts.push(tid); } }\n")
		// 说明:在**框内**贴下沿排列(与标题一样是功能区的成员)
		for i, line := range f.NoteLines {
			fmt.Fprintf(&b, "{ const tx = await eda.sch_PrimitiveText.create(%g, %g, %q, 0, %s, null, %g);\n",
				f.Rect.MinX+8, groupNoteYFor(f.Rect, i, len(f.NoteLines)), line, colorJS, groupNoteFontSize)
			fmt.Fprintf(&b, "  if (tx) { const tid = tx.getState_PrimitiveId(); if (tid) texts.push(tid); } }\n")
		}
	}
	// prelude 开了 `try {` —— 必须用配套的 epilogue 闭合,否则整段是语法错误
	// (症状就是 exec_js failed 而没有任何 detail)。
	writeZoneDrawEpilogue(&b)
	return b.String()
}

// groupAnnotationDraw 是一个组要画的东西。
type groupAnnotationDraw struct {
	GroupID        string
	Rect           layoutBBox
	Label          string
	LabelX, LabelY float64
	NoteLines      []string
}
