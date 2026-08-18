package app

// cmd_sch_zone_move.go — `sch zone move`(设计契约 docs/schematic-layout-hierarchy.md §2):
// 功能区整体刚移。移动链的中间层:zone move → 带动区内全部 group → 带动器件+导线+标志。
//
// 展开集(契约 §2,全区一份展开):
//   - 移动集 = 区内每个组的全部成员 + 散件(被区认领但未入任何组)。组的刚体
//     校验照旧:跨区组=配置错误直接报错;成员不在页=半移预防硬拒。
//   - wire/flag 采集**不逐单元做**:构造一份全区 groupExpandInput(MemberPins=
//     全部移动件的 pin,OtherPins=页上不在移动集的件的 pin,Wires/Flags 全页),
//     只调一次 expandGroupAttachments。区内跨单元直连线(组↔散件、组↔组)两端
//     都是 member → 随区刚移;SharedTrees 因此真正 = 终止于区外件 pin 的跨区
//     布线,留在原地由旗对接。(教训:早期逐单元展开时,同区另一单元的 pin 也
//     算 foreign,区内直连线两边都判 shared 留在原地 —— 刚体被撕开、两端悬空
//     断网,且同一棵树被两单元各计一次,计数虚高。回归测试
//     TestZoneMoveExpandIntraZoneDirectWire 钉死此语义。)
//   - 区内 note 文本:schematic.text.list 里锚点落在区内容 bbox(外扩 --text-pad)内、
//     且 id 不在 zone-draw 的框记录(SchZoneFrameIdsByPage / 遗留 SchZoneFrameIds)里
//     ——框图元不搬,move 后默认重画。
//
// 文本移动方式:protocol/actions.go 已查证 schematic.text 只有 read-only 的
// schematic.text.list,没有 modify 类 action,所以走契约的 delete+recreate:
// 单次 exec_js 先 create 新文本(content/rotation/color/fontSize 保留、坐标平移)、
// 验证新 id 后再删旧 id 并复核删除(sch_PrimitiveObject.delete 才落盘,#164 教训)。
// 注意 recreate 只保留 content/rotation/color/fontSize/fontName —— **bold/italic
// 等富文本样式不保留**(create 接口不收这些参数)。
//
// 目的地预检(move 前,纯几何):
//   - 出 sheet / 压图签 keepout(titleBlockKeepout)→ 硬拒,--force 也不放行;
//   - 与其它区认领件的 union bbox 相交 → 警告,--force 放行(压他区 layout-lint 会红)。
//
// 移动执行(ADR-0004):走单一安全 move 内核 schMoveKernel(快照→删证→移动→
// 重连→对账,失败自动恢复)—— 不再 schematic.group.move 带线刚移(平台 merge
// 共点线的暂态短路一整类问题);区内直连线删后按同网 marker 对接。
// 文本逐条 delete+recreate;成功后显式 schematic.save。
// --redraw-frame(默认 true):move 后按本页框记录的 mode 重画 —— partition 模式
// 直接复用 runPartitionDraw(内部先 computePartitionPlan 校验六项 0,不清洁拒绝重画),
// zones 模式复用 runFixedZoneDraw。**重画前必须 settle**(铁则2:重 mutation 后
// 读几何要 double-read 一致):components.list 的 (id+bbox) 指纹连续两次一致才
// 允许重画(350ms 间隔、4 次预算,照 fetchSchWirePolylinesStable 骨架的本文件
// 私有实现 waitZoneMoveGeometrySettled);超预算降级 —— 框不重画并提示手动
// `sch zone-draw`,绝不按未稳定的几何画错框。
//
// 注册方式:主会话统一挂载 —— 本命令 Use 为 "move",预期挂在 `sch zone` 父命令下
// (`zone := &cobra.Command{Use: "zone"}; zone.AddCommand(newSchZoneMoveCommand(...))`)。

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// schZoneMoveTextPad 是判定「区内 note 文本」时,区内容 bbox 向四周外扩的距离
// (schematic 单位)。note 的约定位置是「框下/框旁空白处」(cmd_sch_note.go),
// 即在器件 union bbox 之外、框(内容 pad 24 + 标题带 30,cmd_sch_zone_plan.go)
// 附近 —— 60 覆盖「框边再往外一小段」的常见停放位,又不至于把邻区的说明吸进来。
const schZoneMoveTextPad = 60.0

// schZoneMoveFrameColor 与 zone-draw 的默认框色一致(--color 默认值),重画时沿用。
const schZoneMoveFrameColor = "#AA00AA"

// ── 纯函数:展开集组装 ──────────────────────────────────────────────────────

// zoneMoveUnits 是功能区刚移的两类移动单元:整组(全体成员都被本区认领)与散件
// (被本区认领但未入任何组)。
type zoneMoveUnits struct {
	Groups []*schGroup // 全员∈区的组,按 ID 排序、去重
	Loose  []string    // 认领但未入组的散件位号,upper-case、排序、去重
}

// partitionZoneMoveUnits 把区的认领位号切分成移动单元(纯函数)。
// 铁则:组是刚体 —— 一个组要么整组属于本区(全部成员都被本区认领),要么与本区
// 无关(零成员被认领);「部分成员在区内」= 跨区组 = 配置错误,拒绝移动并指名
// 区外成员归属(他区认领 or 未认领),让修复动作可执行。
func partitionZoneMoveUnits(zoneName string, claimed []string, groups []*schGroup, claimOf map[string]string) (zoneMoveUnits, error) {
	inZone := map[string]bool{}
	for _, d := range claimed {
		if u := strings.ToUpper(strings.TrimSpace(d)); u != "" {
			inZone[u] = true
		}
	}
	var units zoneMoveUnits
	seen := map[string]bool{}
	grouped := map[string]bool{}
	for _, g := range groups {
		if g == nil || seen[g.ID] {
			continue
		}
		seen[g.ID] = true
		var inside, outside []string
		for _, m := range g.Members {
			if inZone[m] {
				inside = append(inside, m)
			} else {
				outside = append(outside, m)
			}
		}
		if len(inside) == 0 {
			continue // 与本区无关的组
		}
		if len(outside) > 0 {
			det := make([]string, 0, len(outside))
			for _, m := range outside {
				owner := claimOf[m]
				if owner == "" {
					det = append(det, m+"(未被任何区认领)")
				} else {
					det = append(det, fmt.Sprintf("%s(属于区 %q)", m, owner))
				}
			}
			return zoneMoveUnits{}, fmt.Errorf(
				"组 %s 跨区:成员 %s 被区 %q 认领,但 %s — 跨区组是配置错误,拒绝移区;先修正分组(`sch group remove/add`)或 zone 认领(`sch zones set`)再试",
				describeSchGroup(g), strings.Join(inside, ","), zoneName, strings.Join(det, "、"))
		}
		units.Groups = append(units.Groups, g)
		for _, m := range g.Members {
			grouped[m] = true
		}
	}
	sort.Slice(units.Groups, func(i, j int) bool { return units.Groups[i].ID < units.Groups[j].ID })
	looseSeen := map[string]bool{}
	for u := range inZone {
		if !grouped[u] && !looseSeen[u] {
			looseSeen[u] = true
			units.Loose = append(units.Loose, u)
		}
	}
	sort.Strings(units.Loose)
	return units, nil
}

// buildZoneMoveExpandInput 构造「全区一份」的展开输入(纯函数):
// MemberPins = 全部移动件(入选组的所有成员 + 散件)的 pin,OtherPins = 页上
// **不在移动集**的实体件的 pin,Wires/Flags 全页。区内跨单元直连线两端都是
// MemberPins → 随区刚移;只有终止于区外件 pin 的树才是 SharedTrees(真实跨区
// 布线,留在原地由旗对接)。逐单元展开是错的:同区另一单元的 pin 会被算成
// foreign,把区内直连线判 shared 留在原地,刚体被撕开(评审 F1)。
// 返回值第二项是页上找不到的移动位号(半移预防:缺件即硬拒)。
func buildZoneMoveExpandInput(moving []string, comps []layoutComp, wires []schGroupWire) (groupExpandInput, []string) {
	movingSet := map[string]bool{}
	for _, d := range moving {
		if u := strings.ToUpper(strings.TrimSpace(d)); u != "" {
			movingSet[u] = true
		}
	}
	in := groupExpandInput{Wires: wires}
	present := map[string]bool{}
	for _, c := range comps {
		switch {
		case schGroupFlagTypes[c.ComponentType]:
			if c.AnchorAvailable && c.ID != "" {
				in.Flags = append(in.Flags, schGroupFlag{ID: c.ID, X: c.X, Y: c.Y})
			}
		case c.ComponentType == "" || c.ComponentType == schLayoutPartType:
			d := strings.ToUpper(c.Designator)
			if d != "" && movingSet[d] {
				present[d] = true
				for _, p := range c.Pins {
					in.MemberPins = append(in.MemberPins, [2]float64{p.X, p.Y})
				}
			} else {
				for _, p := range c.Pins {
					in.OtherPins = append(in.OtherPins, [2]float64{p.X, p.Y})
				}
			}
		}
	}
	var missing []string
	for d := range movingSet {
		if !present[d] {
			missing = append(missing, d)
		}
	}
	sort.Strings(missing)
	return in, missing
}

// ── 纯函数:区内文本判定 ────────────────────────────────────────────────────

// zoneMoveText 是 schematic.text.list 一条文本的搬移所需字段(delete+recreate
// 时全部保留:内容/旋转/字号/颜色/字体,坐标平移)。
type zoneMoveText struct {
	ID       string
	X, Y     float64
	Content  string
	Rotation float64
	FontSize float64
	Color    string
	FontName string
}

// parseZoneMoveTexts 解析 schematic.text.list 结果(容忍缺字段;无 id 或坐标非
// 有限数的条目跳过 —— 搬不动也删不准的文本宁可不动)。
func parseZoneMoveTexts(result map[string]any) []zoneMoveText {
	if result == nil {
		return nil
	}
	raw, _ := result["texts"].([]any)
	out := make([]zoneMoveText, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := asString(m["primitiveId"])
		x, okX := finiteFloat(m["x"])
		y, okY := finiteFloat(m["y"])
		if id == "" || !okX || !okY {
			continue
		}
		out = append(out, zoneMoveText{
			ID: id, X: x, Y: y,
			Content:  asString(m["content"]),
			Rotation: asFloat(m["rotation"]),
			FontSize: asFloat(m["fontSize"]),
			Color:    asString(m["color"]),
			FontName: asString(m["fontName"]),
		})
	}
	return out
}

// zoneMoveExcludedTextIDs 汇总 zone-draw 框记录里的全部图元 id(rect + 框标题
// 文本,页记录 + 遗留未分页记录)——这些是「框图元」,契约明确不搬(move 后重画)。
func zoneMoveExcludedTextIDs(records ...*workflow.SchZoneFrames) map[string]bool {
	out := map[string]bool{}
	for _, f := range records {
		if f == nil {
			continue
		}
		for _, id := range f.Rects {
			out[id] = true
		}
		for _, id := range f.Texts {
			out[id] = true
		}
	}
	return out
}

// zoneMoveInflate 四向外扩 bbox(纯函数)。
func zoneMoveInflate(b layoutBBox, pad float64) layoutBBox {
	return layoutBBox{MinX: b.MinX - pad, MinY: b.MinY - pad, MaxX: b.MaxX + pad, MaxY: b.MaxY + pad}
}

// selectZoneMoveTexts 选出属于本区的 note 文本(纯函数):锚点落在(已外扩的)
// 区 bbox 内、且 id 不在框记录里。text.list 不带渲染 bbox,以锚点代中心判定。
// 结果按 id 排序,搬移顺序确定。
func selectZoneMoveTexts(texts []zoneMoveText, zone layoutBBox, excluded map[string]bool) []zoneMoveText {
	var out []zoneMoveText
	for _, t := range texts {
		if t.ID == "" || excluded[t.ID] {
			continue
		}
		if t.X >= zone.MinX && t.X <= zone.MaxX && t.Y >= zone.MinY && t.Y <= zone.MaxY {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ── 纯函数:几何与目的地预检 ────────────────────────────────────────────────

// zoneMoveUnionBBox 求点集+盒集的 union bbox;空输入返回 ok=false。
func zoneMoveUnionBBox(points [][2]float64, boxes []layoutBBox) (layoutBBox, bool) {
	var u layoutBBox
	first := true
	grow := func(minX, minY, maxX, maxY float64) {
		if first {
			u = layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
			first = false
			return
		}
		u.MinX = minF(u.MinX, minX)
		u.MinY = minF(u.MinY, minY)
		u.MaxX = maxF(u.MaxX, maxX)
		u.MaxY = maxF(u.MaxY, maxY)
	}
	for _, b := range boxes {
		grow(b.MinX, b.MinY, b.MaxX, b.MaxY)
	}
	for _, p := range points {
		grow(p[0], p[1], p[0], p[1])
	}
	return u, !first
}

// zoneNamedBBox 是「他区认领件 union bbox」——目的地预检的相交对象。
type zoneNamedBBox struct {
	Name string
	BBox layoutBBox
}

// zoneMoveOtherZoneBBoxes 计算目标区之外每个区的认领件 union bbox(纯函数)。
// 不在页上的认领件跳过;一个 bbox 都凑不出的区不参与预检。
func zoneMoveOtherZoneBBoxes(zones map[string]*schZoneClaim, target string, byDesig map[string]layoutComp) []zoneNamedBBox {
	var names []string
	for n := range zones {
		if n != target && zones[n] != nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []zoneNamedBBox
	for _, n := range names {
		var boxes []layoutBBox
		for _, d := range zones[n].Parts {
			if c, ok := byDesig[strings.ToUpper(d)]; ok && c.BBox != nil {
				boxes = append(boxes, *c.BBox)
			}
		}
		if u, ok := zoneMoveUnionBBox(nil, boxes); ok {
			out = append(out, zoneNamedBBox{Name: n, BBox: u})
		}
	}
	return out
}

// zoneMoveDestReport 是目的地预检结论:硬拒项(出 sheet / 压图签)与警告项
// (压他区,--force 放行)。
type zoneMoveDestReport struct {
	Moved        layoutBBox // 全展开 bbox 平移后的落点
	OffSheet     bool       // 超出 sheet → 硬拒
	TitleBlock   bool       // 压图签 keepout → 硬拒
	ZoneOverlaps []string   // 与这些区的认领件 bbox 相交 → 警告(--force 放行)
}

// checkZoneMoveDestination 目的地预检(纯函数):把当前全展开 bbox 平移 (dx,dy),
// 与 sheet(必须完全落内)、图签 keepout(不得相交)、他区 bbox(相交仅警告)逐项比对。
// sheet/keepout 为 nil 时对应检查跳过(读不到 sheet 时不瞎猜硬拒)。
func checkZoneMoveDestination(current layoutBBox, dx, dy float64, sheet, keepout *layoutBBox, others []zoneNamedBBox) zoneMoveDestReport {
	moved := layoutBBox{
		MinX: current.MinX + dx, MinY: current.MinY + dy,
		MaxX: current.MaxX + dx, MaxY: current.MaxY + dy,
	}
	rep := zoneMoveDestReport{Moved: moved}
	if sheet != nil && !bboxContains(*sheet, moved) {
		rep.OffSheet = true
	}
	if keepout != nil && boxesOverlap(moved, *keepout) {
		rep.TitleBlock = true
	}
	for _, o := range others {
		if boxesOverlap(moved, o.BBox) {
			rep.ZoneOverlaps = append(rep.ZoneOverlaps, o.Name)
		}
	}
	sort.Strings(rep.ZoneOverlaps)
	return rep
}

// ── 纯函数:文本搬移 JS(delete+recreate)─────────────────────────────────────

// buildZoneMoveTextJS 生成单条文本搬移的 exec_js:先 create 新文本(内容/旋转/
// 颜色/字体/字号保留,坐标 +dx,+dy)并验证 id,再删旧 id —— 顺序保证 create 失败
// 时旧文本原样保留(不会文本丢失)。删除走 sch_PrimitiveObject(#164:per-class
// text delete 不落盘,reload 后复活),并用 getAllPrimitiveId 复核 oldDeleted。
func buildZoneMoveTextJS(t zoneMoveText, dx, dy float64) string {
	content, _ := json.Marshal(t.Content)
	oldID, _ := json.Marshal(t.ID)
	color := "null"
	if strings.TrimSpace(t.Color) != "" {
		cb, _ := json.Marshal(t.Color)
		color = string(cb)
	}
	font := "null"
	if strings.TrimSpace(t.FontName) != "" {
		fb, _ := json.Marshal(t.FontName)
		font = string(fb)
	}
	size := t.FontSize
	if size <= 0 {
		size = schNoteDefaultFontSize
	}
	var b strings.Builder
	fmt.Fprintf(&b, "const oldId = %s;\n", oldID)
	fmt.Fprintf(&b, "const tx = await eda.sch_PrimitiveText.create(%g, %g, %s, %g, %s, %s, %g);\n",
		t.X+dx, t.Y+dy, content, t.Rotation, color, font, size)
	b.WriteString("if (!tx) throw new Error('text create returned undefined');\n")
	b.WriteString("const tid = tx.getState_PrimitiveId();\n")
	b.WriteString("if (!tid) { await eda.sch_PrimitiveObject.delete([tx]); throw new Error('text id missing'); }\n")
	b.WriteString("const generic = eda.sch_PrimitiveObject && typeof eda.sch_PrimitiveObject.delete === 'function' ? eda.sch_PrimitiveObject : null;\n")
	b.WriteString("if (generic) { await generic.delete([oldId]); } else { await eda.sch_PrimitiveText.delete([oldId]); }\n")
	b.WriteString("const alive = new Set(await eda.sch_PrimitiveText.getAllPrimitiveId());\n")
	b.WriteString("return { textId: tid, oldDeleted: !alive.has(oldId) };")
	return b.String()
}

// findZoneMoveClaim 按名字解析 --zone(纯函数):先精确匹配,再唯一的大小写不敏感
// 匹配;找不到时列出本页全部区名。
func findZoneMoveClaim(zones map[string]*schZoneClaim, ref string) (string, *schZoneClaim, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil, fmt.Errorf("--zone is required(功能区名 = `sch zones` 的模块名,`sch zones status` 查看)")
	}
	var names []string
	for n := range zones {
		if zones[n] != nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if n == ref {
			return n, zones[n], nil
		}
	}
	var folded []string
	for _, n := range names {
		if strings.EqualFold(n, ref) {
			folded = append(folded, n)
		}
	}
	switch len(folded) {
	case 1:
		return folded[0], zones[folded[0]], nil
	case 0:
		return "", nil, fmt.Errorf("功能区 %q 不存在 — 本页已认领的区:%s(`sch zones status` 查看,`sch zones set` 认领)",
			ref, strings.Join(names, ", "))
	default:
		return "", nil, fmt.Errorf("功能区名 %q 大小写歧义(%s)— 用精确名字", ref, strings.Join(folded, ", "))
	}
}

// ── 纯函数 + settle:move 后重画前的几何稳定判定(评审 F2 / 铁则2)────────────

// zoneMoveGeomFingerprint 把 components.list 快照压成 (id + bbox) 指纹(纯函数):
// 每件一条 "id@minX,minY,maxX,maxY"(无 bbox 者退化为 "id"),排序后拼接 ——
// 与读取顺序无关;任何件的增删或 bbox 变化都改变指纹。两次读取指纹一致 =
// 平台快照已 settle,重画才许读几何。
func zoneMoveGeomFingerprint(comps []layoutComp) string {
	keys := make([]string, 0, len(comps))
	for _, c := range comps {
		key := c.ID
		if c.BBox != nil {
			key = fmt.Sprintf("%s@%g,%g,%g,%g", c.ID, c.BBox.MinX, c.BBox.MinY, c.BBox.MaxX, c.BBox.MaxY)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ";")
}

// waitZoneMoveGeometrySettled 在框重画前等 components.list 的 (id+bbox) 指纹
// 连续两次一致(350ms 间隔、4 次预算 —— 照抄 fetchSchWirePolylinesStable 的
// double-read 骨架,私有实现,不动共享文件)。schematic.group.move + 文本
// delete+recreate 都是重 mutation,紧跟着的单次 components.list 可能读到平台
// 半更新的快照,拿它算分区框就是画错框;超预算返回错误,调用方降级不画。
func waitZoneMoveGeometrySettled(cfg *appConfig, window, docUUID string) error {
	const attempts = 4
	var prevKey string
	havePrev := false
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(350 * time.Millisecond)
		}
		res, err := requestAutolayoutAction(cfg, "schematic.components.list", window,
			map[string]any{"includeBBox": true}, docUUID, "settle zone-move geometry")
		if err != nil {
			return err
		}
		comps, err := parseLayoutComps(res.Result)
		if err != nil {
			return err
		}
		key := zoneMoveGeomFingerprint(comps)
		if havePrev && key == prevKey {
			return nil
		}
		prevKey, havePrev = key, true
	}
	return fmt.Errorf("components.list 指纹 %d 次读取仍未稳定(move 后平台快照还在收敛)", attempts)
}

// ── I/O 执行 ────────────────────────────────────────────────────────────────

// zoneMoveIDSet 聚合去重后的刚移 id 集。
type zoneMoveIDSet struct {
	comp, wire, flag map[string]bool
	shared           int
}

func newZoneMoveIDSet() *zoneMoveIDSet {
	return &zoneMoveIDSet{comp: map[string]bool{}, wire: map[string]bool{}, flag: map[string]bool{}}
}

func (s *zoneMoveIDSet) sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		if id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (s *zoneMoveIDSet) all() []string {
	out := s.sorted(s.comp)
	out = append(out, s.sorted(s.wire)...)
	return append(out, s.sorted(s.flag)...)
}

func runSchZoneMove(cfg *appConfig, window, zoneRef string, dx, dy, textPad float64, force, dryRun, redraw bool, stdout, stderr io.Writer) error {
	pinned, win, docUUID, err := pinZonePage(cfg, window)
	if err != nil {
		return err
	}
	zones, project, err := loadSchZoneModules(pinned, win, docUUID)
	if err != nil {
		return err
	}
	if len(zones) == 0 {
		return fmt.Errorf("工程 %q 本页(%s)没有功能模块 — 块驱动的页用 `sch block-apply` 自动归组;手工页 `sch group create` 或 `sch zones set`", project, docUUID)
	}
	zoneName, claim, err := findZoneMoveClaim(zones, zoneRef)
	if err != nil {
		return err
	}
	st, err := loadPcbStageState(project)
	if err != nil {
		return err
	}
	groups := st.GroupsForPage(docUUID)
	claimOf := map[string]string{}
	for name, zc := range zones {
		if zc == nil {
			continue
		}
		for _, d := range zc.Parts {
			claimOf[strings.ToUpper(d)] = name
		}
	}
	units, err := partitionZoneMoveUnits(zoneName, claim.Parts, groups, claimOf)
	if err != nil {
		return err
	}

	// 1) 全页几何一次拉齐(pin + bbox)+ stable 导线快照 —— 组与散件共用同一份
	//    快照,展开必须「全区一份」(逐单元展开会把区内跨单元直连线误判成
	//    SharedTrees 留在原地,刚体被撕开;评审 F1)。
	res, err := requestAutolayoutAction(pinned, "schematic.components.list", win,
		map[string]any{"includePins": true, "includeBBox": true}, docUUID, "read zone-move geometry")
	if err != nil {
		return err
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return err
	}
	wires, err := fetchSchWirePolylinesStable(pinned, win, docUUID)
	if err != nil {
		return fmt.Errorf("读取导线几何:%w", err)
	}
	partByDesig := map[string]layoutComp{}
	flagPosByID := map[string][2]float64{}
	for _, c := range comps {
		switch {
		case schGroupFlagTypes[c.ComponentType]:
			if c.AnchorAvailable && c.ID != "" {
				flagPosByID[c.ID] = [2]float64{c.X, c.Y}
			}
		case c.ComponentType == "" || c.ComponentType == schLayoutPartType:
			if d := strings.ToUpper(c.Designator); d != "" {
				partByDesig[d] = c
			}
		}
	}

	// 2) 移动集 = 入选组的全部成员 + 散件;成员在页校验(半移预防:缺件即硬拒,
	//    组按组点名、散件汇总点名,修复动作可执行)。
	movingSeen := map[string]bool{}
	var movingList []string
	addMoving := func(d string) {
		if u := strings.ToUpper(strings.TrimSpace(d)); u != "" && !movingSeen[u] {
			movingSeen[u] = true
			movingList = append(movingList, u)
		}
	}
	for _, g := range units.Groups {
		var miss []string
		for _, m := range g.Members {
			if _, ok := partByDesig[strings.ToUpper(m)]; !ok {
				miss = append(miss, m)
			}
			addMoving(m)
		}
		if len(miss) > 0 {
			return fmt.Errorf("区 %q 的组 %s 有成员不在当前页:%s — 半移预防拒绝移区;`sch group remove --group %s --members %s` 修组,或 `easyeda doc switch` 切到正确页",
				zoneName, describeSchGroup(g), strings.Join(miss, ","), g.ID, strings.Join(miss, ","))
		}
	}
	for _, d := range units.Loose {
		addMoving(d)
	}
	sort.Strings(movingList)

	// 3) 全区一份展开输入 → expandGroupAttachments 只调一次:MemberPins=全部
	//    移动件 pin,OtherPins=页上移动集之外的件 pin。区内跨单元直连线两端都是
	//    member → 随区刚移;SharedTrees 此后真正 = 跨区布线。suspects 残骸预检
	//    与组展开同一判据,零容忍硬拒。
	in, missing := buildZoneMoveExpandInput(movingList, comps, wires)
	if len(missing) > 0 {
		return fmt.Errorf("区 %q 认领的散件不在当前页:%s — 半移预防,先补齐/修正认领(`sch zones set`)或切到正确页(`easyeda doc switch`)",
			zoneName, strings.Join(missing, ","))
	}
	exp := expandGroupAttachments(in)
	if len(exp.Suspects) > 0 {
		ownerOf := func(x, y float64) string {
			for _, d := range movingList {
				for _, p := range partByDesig[d].Pins {
					if p.X == x && p.Y == y {
						return d
					}
				}
			}
			return "?"
		}
		var lines []string
		for _, sp := range exp.Suspects {
			lines = append(lines, fmt.Sprintf("  wire %s [%g,%g → %g,%g] 共线擦过 %s 的 pin (%g,%g) 却未连接",
				sp.WireID, sp.X0, sp.Y0, sp.X1, sp.Y1, ownerOf(sp.PinX, sp.PinY), sp.PinX, sp.PinY))
		}
		return fmt.Errorf("区 %q 的展开不完整 — 检出半移残骸(同线断触):\n%s\n拒绝移区;先清理(`sch prim-delete --ids <wireId>` + `sch check`)再重连重试",
			zoneName, strings.Join(lines, "\n"))
	}
	ids := newZoneMoveIDSet()
	for _, d := range movingList {
		if c := partByDesig[d]; c.ID != "" {
			ids.comp[c.ID] = true
		}
	}
	for _, id := range exp.WireIDs {
		ids.wire[id] = true
	}
	for _, id := range exp.FlagIDs {
		ids.flag[id] = true
	}
	ids.shared = exp.SharedTrees
	if len(ids.comp) == 0 {
		return fmt.Errorf("区 %q 在本页展开不出任何器件 — 检查认领(`sch zones status`)", zoneName)
	}

	// 4) 区内容 bbox:认领件 bbox ∪ 被搬导线端点 ∪ 被搬旗锚点。
	var pts [][2]float64
	var boxes []layoutBBox
	for _, d := range claim.Parts {
		c, ok := partByDesig[strings.ToUpper(d)]
		if !ok {
			continue // 移动集已在步骤 2/3 做过在页校验;此处宽松
		}
		if c.BBox != nil {
			boxes = append(boxes, *c.BBox)
		} else if c.AnchorAvailable {
			pts = append(pts, [2]float64{c.X, c.Y})
		}
	}
	wireByID := map[string]schGroupWire{}
	for _, w := range wires {
		wireByID[w.ID] = w
	}
	for _, id := range ids.sorted(ids.wire) {
		if w, ok := wireByID[id]; ok {
			for i := 0; i+1 < len(w.Points); i += 2 {
				pts = append(pts, [2]float64{w.Points[i], w.Points[i+1]})
			}
		}
	}
	for _, id := range ids.sorted(ids.flag) {
		if p, ok := flagPosByID[id]; ok {
			pts = append(pts, p)
		}
	}
	content, ok := zoneMoveUnionBBox(pts, boxes)
	if !ok {
		return fmt.Errorf("区 %q 凑不出内容 bbox(认领件无 bbox/锚点)— 无法做目的地预检,拒绝盲移", zoneName)
	}

	// 5) 区内 note 文本(框图元排除)。
	tres, err := requestAutolayoutAction(pinned, "schematic.text.list", win, map[string]any{}, docUUID, "list zone texts")
	if err != nil {
		return fmt.Errorf("读取文本列表:%w", err)
	}
	excluded := zoneMoveExcludedTextIDs(st.SchZoneFrameIdsByPage[docUUID], st.SchZoneFrameIds)
	if textPad < 0 {
		textPad = schZoneMoveTextPad
	}
	notes := selectZoneMoveTexts(parseZoneMoveTexts(tres.Result), zoneMoveInflate(content, textPad), excluded)
	// 功能区对象模型:登记的说明(claim.NoteIDs)无条件随区走——锚点在不在几何
	// 框内都算区的内置对象(几何选择只是未登记文本的兜底)。
	if len(claim.NoteIDs) > 0 {
		have := map[string]bool{}
		for _, t := range notes {
			have[t.ID] = true
		}
		reg := map[string]bool{}
		for _, id := range claim.NoteIDs {
			reg[id] = true
		}
		for _, t := range parseZoneMoveTexts(tres.Result) {
			if reg[t.ID] && !have[t.ID] && !excluded[t.ID] {
				notes = append(notes, t)
			}
		}
	}

	// 6) 目的地预检:全展开 bbox(含文本锚点)平移后逐项比对。
	var notePts [][2]float64
	for _, t := range notes {
		notePts = append(notePts, [2]float64{t.X, t.Y})
	}
	full, _ := zoneMoveUnionBBox(notePts, []layoutBBox{content})
	sheet := sheetBBoxOf(comps)
	var keepout *layoutBBox
	if sheet != nil {
		keepout, _ = titleBlockKeepout(sheet)
	}
	dest := checkZoneMoveDestination(full, dx, dy, sheet, keepout,
		zoneMoveOtherZoneBBoxes(zones, zoneName, partByDesig))
	if dest.OffSheet {
		return fmt.Errorf("目的地出 sheet:平移后 bbox (%.0f,%.0f)..(%.0f,%.0f) 超出图纸 (%.0f,%.0f)..(%.0f,%.0f) — 硬拒(--force 也不放行),改小 dx/dy",
			dest.Moved.MinX, dest.Moved.MinY, dest.Moved.MaxX, dest.Moved.MaxY,
			sheet.MinX, sheet.MinY, sheet.MaxX, sheet.MaxY)
	}
	if dest.TitleBlock {
		return fmt.Errorf("目的地压图签 keepout:平移后 bbox (%.0f,%.0f)..(%.0f,%.0f) 与图签区 (%.0f,%.0f)..(%.0f,%.0f) 相交 — 硬拒(--force 也不放行)",
			dest.Moved.MinX, dest.Moved.MinY, dest.Moved.MaxX, dest.Moved.MaxY,
			keepout.MinX, keepout.MinY, keepout.MaxX, keepout.MaxY)
	}
	if len(dest.ZoneOverlaps) > 0 {
		if !force {
			return fmt.Errorf("目的地与其它区认领件 bbox 相交:%s — layout-lint 会红;确认要压过去用 --force 放行",
				strings.Join(dest.ZoneOverlaps, ", "))
		}
		fmt.Fprintf(stderr, "⚠ --force:目的地与区 %s 的认领件 bbox 相交 — move 后跑 `sch layout-lint` 收拾重叠\n",
			strings.Join(dest.ZoneOverlaps, ", "))
	}

	summary := fmt.Sprintf("区 %q:%d 组 + %d 散件 → %d 器件 + %d 导线 + %d 旗 + %d 文本;bbox (%.0f,%.0f)..(%.0f,%.0f) → (%.0f,%.0f)..(%.0f,%.0f)",
		zoneName, len(units.Groups), len(units.Loose),
		len(ids.comp), len(ids.wire), len(ids.flag), len(notes),
		full.MinX, full.MinY, full.MaxX, full.MaxY,
		dest.Moved.MinX, dest.Moved.MinY, dest.Moved.MaxX, dest.Moved.MaxY)
	if ids.shared > 0 {
		fmt.Fprintf(stderr, "note: %d 条 wire tree 终止在区外件的 pin(跨区直连线)— 安全内核会整树拒绝(删掉它会切断区外电路);先把跨区直连改为 netport 对接(`sch disconnect` + `sch connect`)再移区\n", ids.shared)
	}
	if dryRun {
		fmt.Fprintf(stdout, "[dry-run] %s\n", summary)
		return nil
	}

	// 7) 执行只准调内核(ADR-0004):不再 schematic.group.move 带线刚移(平台
	//    merge 共点线的暂态短路一整类问题),改为内核的删净→平移→重连→对账;
	//    区内跨单元直连线删后按同网 marker 对接,电气逐引脚一致由对账保证。
	items := make([]moveItem, 0, len(movingList))
	for _, d := range movingList {
		c := partByDesig[d]
		items = append(items, moveItem{Designator: d, HasTarget: true, X: c.X + dx, Y: c.Y + dy})
	}
	if _, kerr := schMoveKernel(pinned, win, docUUID, items,
		moveKernelOpts{Label: "zone-move", Stdout: stdout, Stderr: stderr}); kerr != nil {
		return fmt.Errorf("zone move 刚移失败:%w", kerr)
	}

	// 8) 文本 delete+recreate(create 先行,失败不丢原文;旧 id 删除必须复核)。
	for _, t := range notes {
		v, terr := execAutolayoutZoneJS(pinned, win, docUUID, "move zone note", buildZoneMoveTextJS(t, dx, dy))
		if terr != nil {
			return fmt.Errorf("搬移文本 %s 失败(器件/导线已移动,文本半移 — 处理后可单独 `sch note` 补):%w", t.ID, terr)
		}
		newID := asString(mnav(v, "textId"))
		if newID == "" {
			return fmt.Errorf("文本 %s 重建未返回新 id(raw: %v)— 旧文本保留在原位,人工确认后重试", t.ID, v)
		}
		if !asBool(mnav(v, "oldDeleted")) {
			return fmt.Errorf("旧文本 %s 删除未验证(新文本 %s 已建,页面出现重复)— `sch prim-delete --ids %s` 清理", t.ID, newID, t.ID)
		}
	}

	// 9) 显式落盘(autosave 只是兜底)。
	if err := saveZoneDocument(pinned, win, docUUID, "save zone move"); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✓ 刚移完成 (%+g,%+g) — %s;已保存\n", dx, dy, summary)

	// 10) 框重画(默认):框图元没搬,按本页记录的 mode 重画。partition 模式内部先
	//    zone-plan 校验(六项 0,不清洁拒绝重画),满足契约的「校验 → 重画」链。
	if !redraw {
		fmt.Fprintln(stdout, "⚠ --redraw-frame=false:分区框未重画,仍停在旧位置 — 记得 `sch zone-draw`(--mode 按原样)手动重画")
		return nil
	}
	prev, _ := recordedZoneFrames(st, docUUID)
	if prev == nil {
		fmt.Fprintln(stdout, "本页无 zone-draw 框记录 — 跳过重画(要框:`sch zone-draw --mode partition`)")
		return nil
	}
	// 铁则2 settle:group.move + 文本重建是重 mutation,重画内部要重新读几何;
	// (id+bbox) 指纹连续两次一致才放行。超预算降级不画 —— 宁可没框,不画错框。
	if serr := waitZoneMoveGeometrySettled(pinned, win, docUUID); serr != nil {
		fmt.Fprintf(stdout, "⚠ move 后几何未稳定(%v)— 框未重画,稍后手动 `sch zone-draw`重画\n", serr)
		return nil
	}
	// 旧账里可能还记着 mode="zones"(固定九宫格已废弃)——一律按数据驱动重画。
	if err := runPartitionDraw(cfg, window, defaultPartitionOpts(), defaultPartitionZoneFontSize, schZoneMoveFrameColor, false, stdout, stderr); err != nil {
		return fmt.Errorf("移动已完成并保存,但框重画失败:%w — 先 `sch zone-plan` 看六项哪项不为 0,再 `sch zone-draw --mode partition`", err)
	}
	return nil
}

// ── cobra ───────────────────────────────────────────────────────────────────

// newSchZoneMoveCommand 构造 `sch zone move` 子命令(Use "move";由主会话挂载到
// `sch zone` 父命令下)。签名与其它 sch 子命令一致:cfg + window 指针 + stdout/stderr。
func newSchZoneMoveCommand(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var zoneRef string
	var dx, dy, textPad float64
	var redraw, force, dryRun bool
	c := &cobra.Command{
		Use:   "move",
		Short: "功能区整体刚移:区内组+散件+桩线+旗+note 一起平移 (dx,dy),分区框自动重画",
		Long: `功能区(zone)整体刚性平移 — 三层布局体系(docs/schematic-layout-hierarchy.md)
的中间层移动:zone move → 带动区内全部 group → 带动器件+导线+标志。

展开集(全区一份展开):
  - 移动集 = 区内每个组的全部成员 + 散件(被区认领但未入组);桩线 + 远端旗
    按全区一次展开自动纳入 — 区内跨单元的直连线随区刚移,只有终止于区外件
    pin 的布线(SharedTrees)留在原地由旗对接;完整性预检照常生效 — 半移残骸
    suspects 直接拒绝;
  - 区内 note 文本(sch note 放的说明):锚点落在区内容 bbox(外扩 --text-pad)内、
    且不属于 zone-draw 框图元的文本,随区平移(平台无 text modify,delete+recreate,
    内容/字号/颜色/旋转保留;bold/italic 等富文本样式不保留);
  - zone-draw 的分区框不搬:move 后默认自动重画(--redraw-frame=false 跳过;
    重画前等几何 settle,超预算则不画并提示手动 sch zone-draw)。

跨区组 = 配置错误(组是刚体,不能被区撕开),直接报错。

目的地预检(move 前):出 sheet / 压图签 keepout → 硬拒;与其它区认领件 bbox
相交 → 报错提示,--force 放行(压区重叠交给 layout-lint 收口)。

坐标为 schematic 单位(0.01 inch),y-UP:+dy 向上。收尾自检建议:
sch layout-lint + sch bridge-check。`,
		Args: cobra.NoArgs,
		Example: `  easyeda sch zone move --zone POWER --dx 0 --dy -200
  easyeda sch zone move --zone POWER --dx 300 --dy 0 --dry-run
  easyeda sch zone move --zone USB --dx -150 --dy 0 --redraw-frame=false`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("dx") && !cmd.Flags().Changed("dy") {
				return fmt.Errorf("至少给 --dx / --dy 之一(零位移是 no-op)")
			}
			if dx == 0 && dy == 0 {
				return fmt.Errorf("dx 与 dy 均为 0 — 零位移无意义")
			}
			return runSchZoneMove(cfg, *window, zoneRef, dx, dy, textPad, force, dryRun, redraw, stdout, stderr)
		},
	}
	c.Flags().StringVar(&zoneRef, "zone", "", "功能区名(= `sch zones` 的模块名,`sch zones status` 查看;必填)")
	c.Flags().Float64Var(&dx, "dx", 0, "X 平移(schematic 单位)")
	c.Flags().Float64Var(&dy, "dy", 0, "Y 平移(schematic 单位,y-UP:正值向上)")
	c.Flags().BoolVar(&redraw, "redraw-frame", true, "move 后自动重画分区框(zone-plan 校验通过才画;false 跳过并提示)")
	c.Flags().BoolVar(&force, "force", false, "目的地与其它区认领件 bbox 相交时仍然移动(出 sheet/压图签仍硬拒)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只展开+预检并打印计划,不移动")
	c.Flags().Float64Var(&textPad, "text-pad", schZoneMoveTextPad, "note 文本归区判定:区内容 bbox 的外扩距离")
	_ = c.MarkFlagRequired("zone")
	return c
}
