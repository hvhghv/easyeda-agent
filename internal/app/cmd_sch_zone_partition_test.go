package app

import (
	"sort"
	"strings"
	"testing"
)

// cmd_sch_zone_partition_test.go — 「哪几个组算一个分区」只有一个答案。
//
// ── 缺陷(2026-08-20 真机,ceshi / MCU_IO / A4 / 连接器 1.0.4)─────────────────
//
// `sch zone-arrange --apply` 三条断言全绿,断言③ 逐区量实测框、逐对判零重叠:
//
//	✓ 断言③绿(落地复判):实测框与规划框偏差 ≤ gutter 12,区框零重叠
//
// 紧接着 `sch zone-plan` 在同一张画布上报重叠、`zone-draw` 拒绝画框:
//
//	[led_indicator_gpio(LED1) / tactile_boot_reset(SW1)]  (36,168)..(274,790)   ← 两个区被并成一个分区
//	[wroom-passives]                                       (229,428)..(540,790)
//	[wroom-core]                                           (776,248)..(1136,790)
//	validation: … partitionOverlap=1 …    ✗ plan has violations
//
// 而「每页画分区框」是 SKILL 铁律 15 —— 交付被自己卡死。
//
// **根因是第二把尺**:zone-arrange 说「一个 zone 认领 = 一个分区」(每个 zaZone
// 一个落位框、断言③ 逐区量一个实测框);zone-plan 却先把整页切成列带/行带,
// 再把「同一格」的区**并成一个分区**。MCU_IO 的排布是「左列上下叠 + 邻列横跨」,
// 全局行分割切不开左列那两个区(wroom-passives 的 y 区间横跨它们),于是两区被并,
// 并集宽到 x=274,与 229 起的 wroom-passives 撞出 45×362。单看任一个区的框,
// 没有任何重叠。
//
// 网格带是 issue #149 首版的遗留:当时框会被 clamp 到格子里、格子决定几何;
// clamp 去掉之后(框 = 成员体积并集 + 带,构造保证「框住自己的内容」)格子只剩
// 归组这一个作用,而它给的答案与排布侧不一样。修法是删掉它,让归组回到唯一答案。
//
// ── 本文件的四组对照 ───────────────────────────────────────────────────────
//
//	正对照     MCU_IO 真机落地几何:不再合并、partitionOverlap=0、画得出来,
//	           且每个分区框与断言③ 的实测框逐字段相同
//	变异对照   同一份 fixture 用**首版网格带归组**(下面的复刻)必须仍然复现缺陷
//	           —— 修复被改回去时它当场转红,不必靠人手工做变异实验
//	负对照 A   两个区的体积**真的互相压**:partitionOverlap 照样非 0、照样拒绝画框
//	负对照 B   没跑过 zone-arrange 的页(手工搭的页 / 读不到导线的页)照样算得出
//	           分区、照样画得出来
//	配对测试   分区归属 ⟺ zone-arrange 的区归属(谁改一边这里就红)

// ── fixture:真机 MCU_IO 落地后的几何 ───────────────────────────────────────

// zpLandedMCUIO 把 fixture 场景过一遍 zone-arrange 的纯规划核心 + 执行折算,
// 得到**落地后**的每区几何:content(L1 体积并集)与 core(器件本体并集)。
//
// 这一份不是手抄的坐标,而是走生产链算出来的 —— 落地框逐区等于真机那三行:
// led (36,168)..(274,392) / tactile (36,418)..(204,790) /
// wroom-passives (229,428)..(540,790) / wroom-core (776,248)..(1136,790)。
func zpLandedMCUIO(t *testing.T, opts partitionOpts) (*zoneArrangeOut, []zaaLandedZone, []partitionModule) {
	t.Helper()
	comps, wires, _ := zfPinsScene(true)
	out, err := planZoneArrangeScene(zfMCUIOZoneClaims(), comps, wires, map[string]zoneNoteSize{}, opts)
	if err != nil {
		t.Fatalf("planZoneArrangeScene: %v", err)
	}
	if out.Verdict != "pass" {
		t.Fatalf("前提没成立:zone-arrange 规划该 pass,得到 %s(%s)", out.Verdict, out.Arrange.Tried)
	}
	execs, _, err := zaaBuildExec(out, &zaScene{comps: comps, wires: wires}, opts)
	if err != nil {
		t.Fatalf("zaaBuildExec: %v", err)
	}
	landed := zfPinsLandedZones(out, comps, execs, opts, nil)
	if f := zaaRecheckFindings(landed, opts.Gutter); len(f) > 0 {
		t.Fatalf("前提没成立:断言③ 该绿,得到 %s", strings.Join(f, ";"))
	}

	live := map[string]layoutComp{}
	for _, c := range comps {
		if c.ComponentType == "part" {
			live[strings.ToUpper(label(c))] = c
		}
	}
	execOf := map[string]zaaMemberExec{}
	for _, m := range execs {
		if m.Rotate {
			// 本 fixture 没有转竖件;真出现了,下面的刚体平移就算不出本体落点,
			// 与其静默算错,不如在这里说清楚。
			t.Fatalf("%s 被规划成转竖件 —— 本 helper 的 core 只会刚体平移,请先补旋转折算", m.Desig)
		}
		execOf[strings.ToUpper(m.Desig)] = m
	}
	coreOf := map[string]layoutBBox{}
	for _, z := range out.Zones {
		var core layoutBBox
		has := false
		for _, g := range z.Groups {
			d := strings.ToUpper(g.Designator)
			b := *live[d].BBox
			m := execOf[d]
			b.MinX, b.MaxX = b.MinX+m.DX, b.MaxX+m.DX
			b.MinY, b.MaxY = b.MinY+m.DY, b.MaxY+m.DY
			zfGrow(&core, &has, b)
		}
		coreOf[z.Name] = core
	}
	mods := make([]partitionModule, 0, len(landed))
	for _, z := range landed {
		mods = append(mods, partitionModule{Name: z.Name, BBox: z.Content, CoreBBox: coreOf[z.Name]})
	}
	return out, landed, mods
}

func zpSheet() layoutBBox { return layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825} }

// zpPartitionNameSets 折出「哪几个组算一个分区」的答案:每个分区的成员名集合,
// 整体按字典序 —— 两条路径的答案可以逐字比较。
func zpPartitionNameSets(plan partitionPlan) []string {
	out := make([]string, 0, len(plan.Partitions))
	for _, p := range plan.Partitions {
		names := append([]string(nil), p.Modules...)
		sort.Strings(names)
		out = append(out, strings.Join(names, "+"))
	}
	sort.Strings(out)
	return out
}

// ── 变异对照:首版「网格带归组」的复刻 ──────────────────────────────────────
//
// 生产代码里已经删掉(clusterSplits / boundsFrom / bandIndex),这份复刻只活在
// 测试里,存在的唯一理由是给根因修复做**常驻**变异对照:同一份真机 fixture 在
// 首版归组下必须仍然把两个区并成一个分区并报重叠 —— 归组一旦被改回按几何合并,
// 正对照当场转红,而这一条负责证明「fixture 还在复现那个缺陷」。
// 参数与首版默认一致(gutter 12、最多 3 列 2 行)。
func zpLegacyGridGroups(modules []partitionModule, usable layoutBBox, gutter float64, maxCols, maxRows int) [][]int {
	type iv struct{ lo, hi, center float64 }
	splits := func(ivs []iv, maxK int) []float64 {
		if len(ivs) <= 1 || maxK <= 1 {
			return nil
		}
		s := append([]iv(nil), ivs...)
		sort.Slice(s, func(i, j int) bool { return s[i].center < s[j].center })
		type gap struct{ size, mid float64 }
		var gaps []gap
		for i := 1; i < len(s); i++ {
			if band := s[i].lo - s[i-1].hi; band >= gutter {
				gaps = append(gaps, gap{band, (s[i].lo + s[i-1].hi) / 2})
			}
		}
		sort.Slice(gaps, func(i, j int) bool {
			if gaps[i].size != gaps[j].size {
				return gaps[i].size > gaps[j].size
			}
			return gaps[i].mid < gaps[j].mid
		})
		var out []float64
		for _, g := range gaps {
			if len(out) >= maxK-1 {
				break
			}
			out = append(out, g.mid)
		}
		sort.Float64s(out)
		return out
	}
	band := func(v float64, lo, hi float64, cuts []float64) int {
		bounds := append([]float64{lo}, append(append([]float64(nil), cuts...), hi)...)
		for i := 0; i+1 < len(bounds); i++ {
			if v < bounds[i+1] {
				return i
			}
		}
		return len(bounds) - 2
	}
	colIvs := make([]iv, len(modules))
	rowIvs := make([]iv, len(modules))
	cx := make([]float64, len(modules))
	cy := make([]float64, len(modules))
	for i, m := range modules {
		core := moduleCoreBBox(m)
		cx[i], cy[i] = bboxCenter(core)
		colIvs[i] = iv{core.MinX, core.MaxX, cx[i]}
		rowIvs[i] = iv{core.MinY, core.MaxY, cy[i]}
	}
	colCuts := splits(colIvs, maxCols)
	rowCuts := splits(rowIvs, maxRows)
	type key struct{ c, r int }
	cells := map[key][]int{}
	var order []key
	for i := range modules {
		k := key{band(cx[i], usable.MinX, usable.MaxX, colCuts), band(cy[i], usable.MinY, usable.MaxY, rowCuts)}
		if _, ok := cells[k]; !ok {
			order = append(order, k)
		}
		cells[k] = append(cells[k], i)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].r != order[j].r {
			return order[i].r > order[j].r
		}
		return order[i].c < order[j].c
	})
	out := make([][]int, 0, len(order))
	for _, k := range order {
		out = append(out, cells[k])
	}
	return out
}

// zpPlanWithGroups 用给定的归组算一份计划(其余算术与生产完全一致:框 = 成员
// 体积并集 + 带,再让开图签/纸边)—— 变异对照据此在同一算术下只换归组。
func zpPlanWithGroups(sheet layoutBBox, keepout *layoutBBox, modules []partitionModule,
	opts partitionOpts, groups [][]int) partitionPlan {

	plan := partitionPlan{Sheet: sheet, Keepout: keepout}
	for _, grp := range groups {
		merged := modules[grp[0]]
		names := []string{merged.Name}
		for _, i := range grp[1:] {
			b := modules[i].BBox
			merged.BBox.MinX = minF(merged.BBox.MinX, b.MinX)
			merged.BBox.MinY = minF(merged.BBox.MinY, b.MinY)
			merged.BBox.MaxX = maxF(merged.BBox.MaxX, b.MaxX)
			merged.BBox.MaxY = maxF(merged.BBox.MaxY, b.MaxY)
			c := moduleCoreBBox(modules[i])
			merged.CoreBBox.MinX = minF(merged.CoreBBox.MinX, c.MinX)
			merged.CoreBBox.MinY = minF(merged.CoreBBox.MinY, c.MinY)
			merged.CoreBBox.MaxX = maxF(merged.CoreBBox.MaxX, c.MaxX)
			merged.CoreBBox.MaxY = maxF(merged.CoreBBox.MaxY, c.MaxY)
			names = append(names, modules[i].Name)
		}
		sort.Strings(names)
		one := planPartitions(sheet, keepout, []partitionModule{merged}, opts)
		if len(one.Partitions) != 1 {
			panic("zpPlanWithGroups: 单模块该出一个分区")
		}
		p := one.Partitions[0]
		p.Modules = names
		plan.Partitions = append(plan.Partitions, p)
	}
	plan.Validation = validatePartitions(plan, nil, keepout)
	return plan
}

// ── 正对照 + 变异对照 ──────────────────────────────────────────────────────

func TestZonePartition_MCUIOLandedDrawsAfterArrangeGreen(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	keepout, _ := titleBlockKeepout(&sheet)
	_, landed, mods := zpLandedMCUIO(t, opts)

	// 变异对照:首版网格带归组必须仍然复现缺陷(合并 + 重叠)。
	usable := layoutBBox{MinX: sheet.MinX + opts.Margin, MinY: sheet.MinY + opts.Margin,
		MaxX: sheet.MaxX - opts.Margin, MaxY: sheet.MaxY - opts.Margin}
	legacy := zpPlanWithGroups(sheet, keepout, mods, opts,
		zpLegacyGridGroups(mods, usable, opts.Gutter, 3, 2))
	merged := 0
	for _, p := range legacy.Partitions {
		if len(p.Modules) > 1 {
			merged++
		}
	}
	if merged == 0 || legacy.Validation.PartitionOverlap == 0 {
		t.Fatalf("变异对照失效:首版网格带归组居然没合并(%d 个多区分区)、没重叠(%d)—— 这份 fixture 不再复现缺陷:%v",
			merged, legacy.Validation.PartitionOverlap, zpPartitionNameSets(legacy))
	}
	t.Logf("变异对照:首版归组 %v,partitionOverlap=%d", zpPartitionNameSets(legacy), legacy.Validation.PartitionOverlap)

	// 修复后:一个区一个分区,零重叠,画得出来。
	plan := planPartitions(sheet, keepout, mods, opts)
	if len(plan.Partitions) != len(mods) {
		t.Fatalf("%d 个区该出 %d 个分区,得到 %d:%v", len(mods), len(mods), len(plan.Partitions), zpPartitionNameSets(plan))
	}
	for _, p := range plan.Partitions {
		if len(p.Modules) != 1 {
			t.Errorf("分区 %v 合并了 %d 个区 —— 归组又多出一把尺", p.Modules, len(p.Modules))
		}
	}
	if plan.Validation.PartitionOverlap != 0 {
		t.Errorf("partitionOverlap=%d,want 0:%v", plan.Validation.PartitionOverlap, zpPartitionNameSets(plan))
	}
	if !plan.Validation.clean() {
		t.Fatalf("validation 不干净:%+v", plan.Validation)
	}
	if err := partitionDrawGate(plan); err != nil {
		t.Fatalf("断言③ 绿的页面必须画得出来,却被拒:%v", err)
	}

	// 画框侧与判定侧同源:每个分区框逐字段等于断言③ 量出来的实测框。
	rectOf := map[string]layoutBBox{}
	for _, z := range landed {
		rectOf[z.Name] = z.Rect
	}
	for _, p := range plan.Partitions {
		want, ok := rectOf[p.Modules[0]]
		if !ok {
			t.Fatalf("分区 %q 在断言③ 的实测表里没有对应项", p.Modules[0])
		}
		if p.BBox != want {
			t.Errorf("%s:zone-plan 框 %s ≠ 断言③ 实测框 %s(两把尺!)",
				p.Modules[0], bboxText(p.BBox), bboxText(want))
		}
	}

	// 真的能画:JS 里每个分区一个矩形,渲染 bbox 与计划逐字段相同。
	js := buildPartitionDrawJS(plan, defaultPartitionZoneFontSize, "#AA00AA")
	rendered := renderedZoneRectangleBBoxes(t, js)
	if len(rendered) != len(plan.Partitions) {
		t.Fatalf("draw 发出 %d 个矩形,want %d\n%s", len(rendered), len(plan.Partitions), js)
	}
	for i, r := range rendered {
		requireZoneBBoxEqual(t, r, plan.Partitions[i].BBox)
	}
	for _, p := range plan.Partitions {
		t.Logf("  [%s] %s", strings.Join(p.Modules, " / "), bboxText(p.BBox))
	}
}

// ── 配对测试:分区归属 ⟺ zone-arrange 的区归属 ───────────────────────────────

func TestZonePartition_GroupingMatchesArrangeOnRealFixture(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	keepout, _ := titleBlockKeepout(&sheet)
	out, _, mods := zpLandedMCUIO(t, opts)

	arrange := zpPartitionNameSets(zaPartitionPlan(out.Arrange, sheet, keepout, opts))
	draw := zpPartitionNameSets(planPartitions(sheet, keepout, mods, opts))
	if strings.Join(arrange, " | ") != strings.Join(draw, " | ") {
		t.Fatalf("两把尺:zone-arrange 的区归属 %v ≠ zone-draw 的分区归属 %v", arrange, draw)
	}
	t.Logf("同一把尺:%v", draw)
}

// ── 负对照 A:真重叠仍要拦 ──────────────────────────────────────────────────
//
// 两个区的 L1 体积**真的互相压**(相邻区的标签/桩线穿插进对方,典型的没排过的页)。
// 这时不存在「既包住自己内容又互不重叠」的一组矩形 —— 判据必须如实报,画框必须拒。
// 这一条防的是「把容差调大 / 把并集排除出校验」那种假修复。
func TestZonePartition_RealOverlapStillRefused(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	mods := []partitionModule{
		{Name: "A", BBox: layoutBBox{300, 300, 620, 560}, CoreBBox: layoutBBox{320, 320, 600, 540}},
		// B 的体积横插进 A 的右半边(x 500..760 与 A 的 300..620 相交,y 也相交)。
		{Name: "B", BBox: layoutBBox{500, 380, 760, 620}, CoreBBox: layoutBBox{520, 400, 740, 600}},
	}
	plan := planPartitions(sheet, nil, mods, opts)
	if len(plan.Partitions) != 2 {
		t.Fatalf("两个区该出两个分区(不许靠合并把重叠藏起来),得到 %v", zpPartitionNameSets(plan))
	}
	if plan.Validation.PartitionOverlap == 0 {
		t.Fatalf("负对照失效:体积真的互相压,partitionOverlap 却是 0 —— 判据被绕过去了:%v",
			zpPartitionNameSets(plan))
	}
	err := partitionDrawGate(plan)
	if err == nil {
		t.Fatal("负对照失效:重叠的计划居然放行画框")
	}
	if !strings.Contains(err.Error(), "PartitionOverlap") || !strings.Contains(err.Error(), "分区框重叠") {
		t.Errorf("拒绝理由没点名重叠(判据要给能执行的下一步):%v", err)
	}
	t.Logf("负对照 A 如期拒绝:%v", err)
}

// ── 负对照 B:没跑过 zone-arrange 的页仍要能画框 ─────────────────────────────

// zpHandBuiltScene 造一页**手工搭的图**:三组件各带一支 GND 旗,彼此摆开,
// 从没跑过 zone-arrange(件在哪就是哪)。wired=false 时连导线都读不到 ——
// 归属退回距离启发式,那是「没有虚拟组的页」最差的一档。
func zpHandBuiltScene(wired bool) ([]layoutComp, []schGroupWire, map[string]*schZoneClaim) {
	sheet := zpSheet()
	comps := []layoutComp{{ID: "sheet", ComponentType: "sheet", BBox: &sheet}}
	var wires []schGroupWire
	type spec struct {
		desig string
		x, y  float64
	}
	specs := []spec{{"R1", 150, 600}, {"C1", 560, 600}, {"U1", 950, 300}}
	for _, s := range specs {
		body := layoutBBox{MinX: s.x - 10, MinY: s.y - 20, MaxX: s.x + 10, MaxY: s.y + 20}
		bb := body
		rot := 0.0
		part := layoutComp{ID: "pid-" + s.desig, Designator: s.desig, ComponentType: "part",
			X: s.x, Y: s.y, AnchorAvailable: true, Rotation: &rot, BBox: &bb, PinsAvailable: true,
			Pins: []layoutPin{{Number: "1", X: s.x, Y: s.y - 20}}}
		comps = append(comps, part)
		ex, ey := endpointFor(s.x, s.y-20, zfStub, "down")
		mb := predictedMarkerBody(ex, ey, "ground", "down", "GND")
		mrot, err := tidyLabelRotation("ground", "down")
		if err != nil {
			panic(err)
		}
		r := mrot
		comps = append(comps, layoutComp{ID: "m-" + s.desig, ComponentType: "netflag", Net: "GND",
			X: ex, Y: ey, AnchorAvailable: true, BBox: &mb, Rotation: &r})
		if wired {
			wires = append(wires, schGroupWire{ID: "w-" + s.desig, Points: []float64{s.x, s.y - 20, ex, ey}})
		}
	}
	zones := map[string]*schZoneClaim{
		"bias":  {Parts: []string{"R1"}},
		"decap": {Parts: []string{"C1"}},
		"mcu":   {Parts: []string{"U1"}},
	}
	return comps, wires, zones
}

func TestZonePartition_HandBuiltPageWithoutArrangeStillDraws(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	keepout, _ := titleBlockKeepout(&sheet)
	for _, wired := range []bool{true, false} {
		name := "有导线(能折 L1 组)"
		if !wired {
			name = "读不到导线(距离启发式兜底)"
		}
		comps, wires, zones := zpHandBuiltScene(wired)
		var clusterOf map[string]layoutBBox
		if wired {
			cs, _ := buildSchClusters(comps, wires)
			clusterOf = map[string]layoutBBox{}
			for _, c := range cs {
				clusterOf[strings.ToUpper(c.Designator)] = c.Box
			}
		}
		mods, scope := modulesFromClaimsScoped(zones, comps, clusterOf)
		if len(mods) != len(zones) {
			t.Fatalf("%s:%d 个认领只折出 %d 个模块", name, len(zones), len(mods))
		}
		if wired == scope.WiresUnavailable {
			t.Fatalf("%s:走错了分支(WiresUnavailable=%v)", name, scope.WiresUnavailable)
		}
		plan := planPartitions(sheet, keepout, mods, opts)
		if len(plan.Partitions) != len(zones) {
			t.Fatalf("%s:%d 个认领该出 %d 个分区,得到 %v", name, len(zones), len(zones), zpPartitionNameSets(plan))
		}
		if !plan.Validation.clean() {
			t.Fatalf("%s:手工搭的干净页面居然不合格:%+v", name, plan.Validation)
		}
		if err := partitionDrawGate(plan); err != nil {
			t.Fatalf("%s:兜底路径画不出框:%v", name, err)
		}
		js := buildPartitionDrawJS(plan, defaultPartitionZoneFontSize, "#AA00AA")
		if got := len(renderedZoneRectangleBBoxes(t, js)); got != len(zones) {
			t.Fatalf("%s:draw 发出 %d 个矩形,want %d", name, got, len(zones))
		}
		// 每个框必须罩住自己那个区的体积(兜底路径也不许把标签甩在框外)。
		for _, p := range plan.Partitions {
			for _, m := range mods {
				if m.Name == p.Modules[0] && !bboxContains(p.BBox, m.BBox) {
					t.Errorf("%s:%s 的框 %s 没罩住体积 %s", name, m.Name, bboxText(p.BBox), bboxText(m.BBox))
				}
			}
		}
		t.Logf("%s:%v", name, zpPartitionNameSets(plan))
	}
}

// 归组不看几何,所以「页面跑没跑过 zone-arrange」不影响答案:同一批区名,
// 件挪到哪儿都还是一区一框。这条是性质 1(一个页面只有一套答案)的直接形式。
func TestZonePartition_GroupingIsPositionIndependent(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	mods := []partitionModule{
		{Name: "A", BBox: layoutBBox{100, 500, 260, 700}, CoreBBox: layoutBBox{110, 510, 250, 690}},
		{Name: "B", BBox: layoutBBox{100, 200, 260, 400}, CoreBBox: layoutBBox{110, 210, 250, 390}},
		{Name: "C", BBox: layoutBBox{600, 200, 900, 700}, CoreBBox: layoutBBox{610, 210, 890, 690}},
	}
	base := zpPartitionNameSets(planPartitions(sheet, nil, mods, opts))
	if len(base) != 3 {
		t.Fatalf("三个区该出三个分区,得到 %v", base)
	}
	// 把 B 挪到 A 正下方紧贴处(首版归组下 A/B/C 的格子归属会变),答案不许变。
	moved := append([]partitionModule(nil), mods...)
	moved[1].BBox = layoutBBox{100, 420, 260, 490}
	moved[1].CoreBBox = layoutBBox{110, 430, 250, 480}
	got := zpPartitionNameSets(planPartitions(sheet, nil, moved, opts))
	if strings.Join(got, " | ") != strings.Join(base, " | ") {
		t.Fatalf("挪了件就换了一套分区答案:%v → %v", base, got)
	}
}
