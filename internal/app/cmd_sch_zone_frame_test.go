package app

// cmd_sch_zone_frame_test.go — 外框单一函数 + 判据与生成解耦(2026-08-20 用户裁定)。
//
// 用户看图直接指出:「zone 虚拟框就是算法算出来的,应该依据虚拟组(内含器件元素 +
// 网络标签)+ title + notes 直接计算外框;做 title + note 的时候没有和其他虚拟组
// 一起进收紧布局,然后才画框。」
//
// 真机取证(工程 ceshi、POWER 页 ba37d25fc30e533e):`zone-plan` 报
// moduleOutsideZone=0、六项全 0,而把 `sch clusters` 的 L1 组体积与框逐个比,
// **8 个 L1 组里 5 个探出框外**。根因是判据复用了生成侧被削过的 m.BBox ——
// 生成漏掉的标签,判定也就看不见,判据结构上恒报 0。
//
// 本文件用真机数字把三件事钉死:
//   ① 旧尺(判据=生成侧 m.BBox)在这份 fixture 上恒报 0 —— 假绿的机械复现;
//   ② 新尺(判据=独立重算的 L1 组表)必须报出那 5 个探出;
//   ③ 框按新函数从 L1 组并集重算后,全部 L1 组被包含、判据归零(负对照)。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// ── 真机 fixture(工程 ceshi / POWER 页,2026-08-20)───────────────────────
//
// 两个框与 8 个 L1 组体积,数字逐个来自用户的取证,未做任何修饰。

var (
	pwrSheet = layoutBBox{0, 0, 1170, 825}

	// 画布上实际画出来的两个框。
	pwrFrameBuck  = layoutBBox{236, 472, 671, 754}
	pwrFramePower = layoutBBox{116, 206, 260, 370}

	// `sch clusters` 给出的 L1 组体积(器件本体 ∪ 它自己的 marker/桩线)。
	// 前 5 个是取证里逐条列出的**探出框外**的,后 3 个在框内(负对照的一半)。
	pwrClusters = map[string]layoutBBox{
		"R1": {152, 670, 330, 692}, // 左超 84
		"U1": {426, 660, 694, 702}, // 右超 23
		"L1": {604, 462, 686, 572}, // 右超 15、下超 10
		"C1": {220, 550, 330, 572}, // 左超 16
		"J2": {60, 284, 160, 317},  // 左超 56
		"C2": {300, 560, 340, 600}, // 框内
		"C3": {500, 500, 560, 560}, // 框内
		"J1": {140, 240, 200, 300}, // 框内
	}

	pwrParts = map[string][]string{
		"sy8089_buck_3v3(C1)": {"C1", "C2", "C3", "L1", "R1", "U1"},
		"POWER_IN":            {"J1", "J2"},
	}

	// 生成侧当时算出来的模块 bbox —— 只有**器件本体**的并集(marker/桩线全丢了,
	// 因为 computePartitionPlan 拉几何时没要 includePins,L1 归属整体失效)。
	// 它必然被框包住:框就是从它 + pad + 带推出来的。
	pwrBodyBuck  = layoutBBox{260, 538, 647, 700}
	pwrBodyPower = layoutBBox{140, 272, 236, 316}
)

func pwrLivePlan() partitionPlan {
	mk := func(name string, r layoutBBox) partitionRect {
		return partitionRect{
			Modules: []string{name}, BBox: r, BaseBBox: r,
			TitleBBox: layoutBBox{MinX: r.MinX, MinY: r.MaxY - 30, MaxX: r.MaxX, MaxY: r.MaxY},
			NoteBBox:  layoutBBox{MinX: r.MinX, MinY: r.MinY, MaxX: r.MaxX, MaxY: r.MinY + 42},
		}
	}
	return partitionPlan{Sheet: pwrSheet, Partitions: []partitionRect{
		mk("sy8089_buck_3v3(C1)", pwrFrameBuck), mk("POWER_IN", pwrFramePower)}}
}

func pwrModules() []partitionModule {
	return []partitionModule{
		{Name: "sy8089_buck_3v3(C1)", BBox: pwrBodyBuck},
		{Name: "POWER_IN", BBox: pwrBodyPower},
	}
}

func pwrJudge(scope schZoneLabelScope) *partitionJudge {
	cl := map[string]layoutBBox{}
	for k, v := range pwrClusters {
		cl[k] = v
	}
	parts := map[string][]string{}
	for k, v := range pwrParts {
		parts[k] = append([]string(nil), v...)
	}
	return &partitionJudge{PartsOf: parts, ClusterOf: cl, Scope: scope}
}

// 验收①正向:同一份真机 fixture,旧尺恒报 0(假绿),新尺必须报出探出。
func TestPartitionJudge_RealPowerPageLabelsOutsideFrame(t *testing.T) {
	plan := pwrLivePlan()
	mods := pwrModules()

	// 旧尺 = 判据复用生成侧 m.BBox。它验的是一个已经被上游削过的集合,
	// 于是 5 个探出框外的 L1 组一个都看不见 —— 真机上的那句 moduleOutsideZone=0。
	old := validatePartitions(plan, mods, nil)
	if old.ModuleOutsideZone != 0 {
		t.Fatalf("fixture 失效:旧尺本该恒报 0(假绿),got %d", old.ModuleOutsideZone)
	}
	if !old.clean() {
		t.Fatalf("fixture 失效:旧尺本该六项全 0,got %+v", old)
	}

	// 新尺 = 独立重算的 L1 组表。两个模块各有成员探出,必须都报。
	got := validatePartitionsWithJudge(plan, mods, nil, pwrJudge(schZoneLabelScope{}))
	if got.ModuleOutsideZone == 0 {
		t.Fatal("判据必须报出探出的标签(取证:8 个 L1 组里 5 个在框外)")
	}
	if got.ModuleOutsideZone != 2 {
		t.Errorf("两个模块都有成员探出,want 2,got %d(%v)", got.ModuleOutsideZone, got.ModuleOutsideDetail)
	}
	joined := strings.Join(got.ModuleOutsideDetail, "\n")
	// 逐条超出量必须与取证对得上 —— 判据要给能执行的下一步,不是只给个数。
	for _, want := range []string{
		"R1", "左超 84",
		"U1", "右超 23",
		"L1", "右超 15", "下超 10",
		"C1", "左超 16",
		"J2", "左超 56",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("明细缺 %q:\n%s", want, joined)
		}
	}
	// 框内的三个组绝不许被误报。
	for _, inside := range []string{"C2", "C3", "J1"} {
		if strings.Contains(joined, "/"+inside+":") {
			t.Errorf("框内的 %s 被误报:\n%s", inside, joined)
		}
	}
}

// 验收①反向:框按**新函数**从 L1 组并集重算后,全部 L1 组被包含、判据归零。
func TestPartitionFrame_RecomputedFromL1GroupsContainsEveryLabel(t *testing.T) {
	opts := defaultPartitionOpts()
	union := func(names ...string) layoutBBox {
		b, has := layoutBBox{}, false
		for _, n := range names {
			zfGrow(&b, &has, pwrClusters[n])
		}
		return b
	}
	mods := []partitionModule{
		{Name: "sy8089_buck_3v3(C1)", BBox: union(pwrParts["sy8089_buck_3v3(C1)"]...), CoreBBox: pwrBodyBuck},
		{Name: "POWER_IN", BBox: union(pwrParts["POWER_IN"]...), CoreBBox: pwrBodyPower},
	}
	plan := planPartitions(pwrSheet, nil, mods, opts)
	if len(plan.Partitions) != 2 {
		t.Fatalf("want 2 partitions, got %+v", plan.Partitions)
	}
	judge := pwrJudge(schZoneLabelScope{})
	v := validatePartitionsWithJudge(plan, mods, nil, judge)
	if v.ModuleOutsideZone != 0 {
		t.Fatalf("按 L1 组并集重算的框必须罩住每一个标签:%v", v.ModuleOutsideDetail)
	}
	if !v.clean() {
		t.Fatalf("重算后的计划不干净:%+v", v)
	}
	// 逐个 L1 组做包含断言(不依赖判据自己的实现)。
	frameOf := map[string]layoutBBox{}
	for _, p := range plan.Partitions {
		for _, n := range p.Modules {
			frameOf[n] = p.BBox
		}
	}
	for zone, parts := range pwrParts {
		for _, d := range parts {
			if !bboxContains(frameOf[zone], pwrClusters[d]) {
				t.Errorf("%s 的 L1 组 %s %s 没被框 %s 罩住",
					zone, d, bboxText(pwrClusters[d]), bboxText(frameOf[zone]))
			}
		}
	}
	// 框 = 内容并集 + pad + 带,逐字段等于外框唯一函数的输出(没有裁剪、没有第二套算式)。
	for _, m := range mods {
		want := partitionFirstPassRect(m.BBox, opts, 0)
		if got := frameOf[m.Name]; got != want {
			t.Errorf("%s 的框不是外框唯一函数算的:got %s want %s", m.Name, bboxText(got), bboxText(want))
		}
	}
}

// 验收②负对照:标签确实在框内时,判据必须为 0(防止把它改成恒报)。
func TestPartitionJudge_NegativeControlAllLabelsInside(t *testing.T) {
	frame := layoutBBox{100, 100, 700, 600}
	plan := partitionPlan{Sheet: pwrSheet, Partitions: []partitionRect{{
		Modules: []string{"Z"}, BBox: frame, BaseBBox: frame,
		TitleBBox: layoutBBox{100, 570, 700, 600},
		NoteBBox:  layoutBBox{100, 100, 700, 142},
	}}}
	mods := []partitionModule{{Name: "Z", BBox: layoutBBox{200, 200, 500, 450}}}
	judge := &partitionJudge{
		PartsOf: map[string][]string{"Z": {"U1", "C1", "R1"}},
		ClusterOf: map[string]layoutBBox{
			"U1": {150, 150, 400, 400},
			"C1": {420, 200, 480, 300},
			"R1": {200, 460, 300, 560}, // 顶到标题带下沿仍在框内
		},
	}
	got := validatePartitionsWithJudge(plan, mods, nil, judge)
	if got.ModuleOutsideZone != 0 {
		t.Fatalf("标签全在框内时判据必须为 0,got %d:%v", got.ModuleOutsideZone, got.ModuleOutsideDetail)
	}
	// 把其中一个挪出去一点点就必须响 —— 判据不是被关掉了。
	judge.ClusterOf["C1"] = layoutBBox{420, 200, 720, 300}
	if again := validatePartitionsWithJudge(plan, mods, nil, judge); again.ModuleOutsideZone != 1 {
		t.Fatalf("挪出框外必须报:got %d", again.ModuleOutsideZone)
	}
}

// 验收③两把尺配对:zone-plan 侧的框 与 zone-arrange phase A 的框,对同一输入
// 必须逐字段相同。修复前一个用「已登记说明的实际渲染高」、一个用常量 NoteBand。
func TestZoneFrameRuler_PlanAndArrangeAgreeFieldByField(t *testing.T) {
	opts := defaultPartitionOpts()
	content := layoutBBox{152, 462, 694, 702}
	noteH := threeLineNoteH // 39:三行默认字号说明,requiredNoteBand=55 ≠ 常量带 42

	// ① 纯函数层:同一个本体。
	r := partitionFirstPassRect(content, opts, noteH)
	w, h := zoneArrangeRawFrame(content, opts, noteH)
	if r.MaxX-r.MinX != w || r.MaxY-r.MinY != h {
		t.Fatalf("两把尺分家:zone-plan 框 %s(%.0f×%.0f) vs phase A %.0f×%.0f",
			bboxText(r), r.MaxX-r.MinX, r.MaxY-r.MinY, w, h)
	}

	// ② fixture 必须真的能分辨出这个缺陷:旧的常量带算式与新的不等。
	oldRawH := (content.MaxY - content.MinY) + 2*partitionContentPad + opts.TitleBand + opts.NoteBand
	if oldRawH == h {
		t.Fatalf("fixture 失效:常量带与登记带高在这份输入上恰好相等(%.0f),分辨不出缺陷", h)
	}

	// ③ 端到端:planPartitions 真跑出来的框,与两个纯函数逐字段相同。
	plan := planPartitions(pwrSheet, nil,
		[]partitionModule{{Name: "Z", BBox: content, NoteHeight: noteH}}, opts)
	if len(plan.Partitions) != 1 {
		t.Fatalf("want 1 partition, got %+v", plan.Partitions)
	}
	if got := plan.Partitions[0].BBox; got != r {
		t.Fatalf("planPartitions 的框与外框唯一函数分家:got %s want %s", bboxText(got), bboxText(r))
	}

	// ④ phase A 收敛后的框(planZoneFollow)走同一个本体、同一份带高。
	zopts := opts
	zopts.NoteBand = schZoneNoteBandHeight(opts.NoteBand, noteH)
	zp, err := planZoneFollow("Z", []zfGroup{{Designator: "R1", BodyW: 20, BodyH: 40,
		Terms: []zfTerm{{Kind: "netflag", Net: "GND", W: 30, H: 12, Side: "down"}}}}, zopts, zfDomain{})
	if err != nil {
		t.Fatal(err)
	}
	wantW, wantH := partitionFrameSize(zp.Content, opts.TitleBand, zopts.NoteBand)
	if zp.FrameW != wantW || zp.FrameH != wantH {
		t.Fatalf("phase A 收敛框没走外框唯一函数:got %.0f×%.0f want %.0f×%.0f",
			zp.FrameW, zp.FrameH, wantW, wantH)
	}
	// 收紧账里必须真的含区名带 + 说明带(不是只加了 pad)。
	if bare := (zp.Content.MaxY - zp.Content.MinY) + 2*partitionContentPad; zp.FrameH <= bare {
		t.Fatalf("收紧没把 title/note 带算进去:frameH %.0f ≤ 裸内容+pad %.0f", zp.FrameH, bare)
	}
}

// schZoneNoteBandHeight 是带高的唯一口径:没登记说明用默认带,登记了就按内容推。
func TestSchZoneNoteBandHeight(t *testing.T) {
	def := defaultPartitionOpts().NoteBand
	if got := schZoneNoteBandHeight(def, 0); got != def {
		t.Errorf("无登记说明应用默认带 %.0f,got %.0f", def, got)
	}
	if got, want := schZoneNoteBandHeight(def, threeLineNoteH), requiredNoteBand(threeLineNoteH); got != want {
		t.Errorf("三行说明带高 want %.0f got %.0f", want, got)
	}
	// 说明比默认带矮时不许缩带(默认带是下限)。
	if got := schZoneNoteBandHeight(def, 5); got != def {
		t.Errorf("矮说明不许把带缩掉:want %.0f got %.0f", def, got)
	}
}

// 验收④幂等:登记说明之后重跑 zone-plan,框一步不漂(防 9ee3e13 建立的反馈环回归)。
func TestPartitionFrame_IdempotentAfterNoteRegistration(t *testing.T) {
	opts := defaultPartitionOpts()
	mods := []partitionModule{
		{Name: "BUCK", BBox: layoutBBox{152, 462, 694, 702}, NoteWidth: 300, NoteHeight: threeLineNoteH},
		{Name: "IN", BBox: layoutBBox{60, 240, 200, 317}, NoteWidth: 120, NoteHeight: 13},
	}
	obstacles := []layoutBBox{mods[0].BBox, mods[1].BBox}
	first := planPartitionsWithNotes(pwrSheet, nil, mods, opts, obstacles)
	second := planPartitionsWithNotes(pwrSheet, nil, mods, opts, obstacles)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("zone-plan 必须幂等:\n1 %+v\n2 %+v", first, second)
	}
	// 带高只由内容+字号推导 —— 把模块整体平移不改变框的**尺寸**(不读落点)。
	shifted := make([]partitionModule, len(mods))
	for i, m := range mods {
		shifted[i] = m
		shifted[i].BBox = layoutBBox{m.BBox.MinX + 40, m.BBox.MinY, m.BBox.MaxX + 40, m.BBox.MaxY}
	}
	moved := planPartitionsWithNotes(pwrSheet, nil, shifted, opts,
		[]layoutBBox{shifted[0].BBox, shifted[1].BBox})
	for i := range first.Partitions {
		a, b := first.Partitions[i].BBox, moved.Partitions[i].BBox
		if (a.MaxX-a.MinX) != (b.MaxX-b.MinX) || (a.MaxY-a.MinY) != (b.MaxY-b.MinY) {
			t.Errorf("平移改变了框尺寸(带高读了落点?):%s vs %s", bboxText(a), bboxText(b))
		}
	}
}

// ── 验收⑤:降级可见 + fail-closed ──────────────────────────────────────────

// 真机根因的机械复现:回读没带 includePins → 每个 L1 组的体积退化成器件本体,
// 而 clusterOf 非空、每个键都在 —— 旧代码于是 labelScopeDegraded=false,判据全 0。
func TestModulesFromClaimsScoped_NoPinGeometryIsDegraded(t *testing.T) {
	body := func(d string, b layoutBBox, pins bool) layoutComp {
		c := layoutComp{ID: "id-" + d, Designator: d, ComponentType: "part", BBox: &b}
		c.PinsAvailable = pins
		c.PinsProofKnown = pins
		return c
	}
	comps := []layoutComp{
		body("U1", layoutBBox{300, 300, 400, 400}, false),
		body("R1", layoutBBox{420, 300, 440, 340}, false),
	}
	// 引脚缺失时 buildSchClusters 归不了任何 marker/桩线 → 体积 = 本体。
	clusterOf := map[string]layoutBBox{"U1": {300, 300, 400, 400}, "R1": {420, 300, 440, 340}}
	zones := map[string]*schZoneClaim{"Z": {Parts: []string{"U1", "R1"}}}

	mods, scope := modulesFromClaimsScoped(zones, comps, clusterOf)
	if len(mods) != 1 {
		t.Fatalf("want 1 module, got %+v", mods)
	}
	if !scope.Degraded {
		t.Fatal("引脚几何缺失 = L1 归属结构上做不成,必须报降级(真机那次假绿的根因)")
	}
	if !reflect.DeepEqual(scope.NoPinGeometry, []string{"R1", "U1"}) {
		t.Errorf("降级必须点名是谁:%+v", scope.NoPinGeometry)
	}

	// fail-closed:框明明罩住了(退化的)组体积,判据仍不许放行 —— 验不了就不报绿。
	frame := layoutBBox{200, 200, 600, 600}
	plan := partitionPlan{Sheet: pwrSheet, Partitions: []partitionRect{{
		Modules: []string{"Z"}, BBox: frame, BaseBBox: frame,
		TitleBBox: layoutBBox{200, 570, 600, 600}, NoteBBox: layoutBBox{200, 200, 600, 242}}}}
	judge := &partitionJudge{PartsOf: map[string][]string{"Z": {"R1", "U1"}}, ClusterOf: clusterOf, Scope: scope}
	v := validatePartitionsWithJudge(plan, mods, nil, judge)
	if v.ModuleOutsideZone != 1 {
		t.Fatalf("口径不可信时必须 fail-closed,got %d:%v", v.ModuleOutsideZone, v.ModuleOutsideDetail)
	}
	if !strings.Contains(strings.Join(v.ModuleOutsideDetail, "\n"), "降级") {
		t.Errorf("明细必须说清是降级而不是几何超出:%v", v.ModuleOutsideDetail)
	}

	// 有引脚几何时同一份输入必须干净(负对照:降级判据不是恒报)。
	comps[0].PinsAvailable, comps[0].PinsProofKnown = true, true
	comps[1].PinsAvailable, comps[1].PinsProofKnown = true, true
	mods2, scope2 := modulesFromClaimsScoped(zones, comps, clusterOf)
	if scope2.Degraded {
		t.Fatalf("引脚齐全时不该降级:%+v", scope2)
	}
	judge2 := &partitionJudge{PartsOf: map[string][]string{"Z": {"R1", "U1"}}, ClusterOf: clusterOf, Scope: scope2}
	if v2 := validatePartitionsWithJudge(plan, mods2, nil, judge2); v2.ModuleOutsideZone != 0 {
		t.Fatalf("引脚齐全 + 框罩得住 → 必须干净,got %d:%v", v2.ModuleOutsideZone, v2.ModuleOutsideDetail)
	}
}

// clusterOf 缺键(某个位号根本没有 L1 组记录)时也必须显式降级,绝不静默退回本体。
func TestModulesFromClaimsScoped_MissingClusterIsReported(t *testing.T) {
	b1, b2 := layoutBBox{300, 300, 400, 400}, layoutBBox{420, 300, 440, 340}
	comps := []layoutComp{
		{ID: "a", Designator: "U1", ComponentType: "part", BBox: &b1, PinsAvailable: true, PinsProofKnown: true},
		{ID: "b", Designator: "R9", ComponentType: "part", BBox: &b2, PinsAvailable: true, PinsProofKnown: true},
	}
	zones := map[string]*schZoneClaim{"Z": {Parts: []string{"U1", "R9", "J7"}}}
	clusterOf := map[string]layoutBBox{"U1": b1} // R9 没有组记录;J7 不在本页

	mods, scope := modulesFromClaimsScoped(zones, comps, clusterOf)
	if !scope.Degraded || !reflect.DeepEqual(scope.NoCluster, []string{"R9"}) {
		t.Fatalf("缺 L1 组必须显式降级并点名:%+v", scope)
	}
	if !reflect.DeepEqual(scope.OffPage, []string{"J7"}) {
		t.Errorf("跨页认领应记为 OffPage(信息项):%+v", scope.OffPage)
	}
	// 退回本体的那一件仍进 bbox(否则整页画不出框),但判据把它按不可信处理。
	if !bboxContains(mods[0].BBox, b2) {
		t.Errorf("退回本体的件仍须进模块 bbox:%s", bboxText(mods[0].BBox))
	}
	if !scope.untrusted("r9") || scope.untrusted("U1") {
		t.Errorf("untrusted 判定不对:%+v", scope)
	}
}

// labelScopeDegraded 必须**恒定出现**在 JSON 里 —— 三份真机快照里查不到这个字段
// (omitempty 把 false 抹掉了),等于这个降级信号是哑的。
func TestPartitionPlan_LabelScopeAlwaysSerialized(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan partitionPlan
	}{
		{"未降级", partitionPlan{Sheet: pwrSheet}},
		{"已降级", partitionPlan{Sheet: pwrSheet, LabelScopeDegraded: true,
			LabelScope: schZoneLabelScope{Degraded: true, NoPinGeometry: []string{"U1"}}}},
	} {
		raw, err := json.Marshal(tc.plan)
		if err != nil {
			t.Fatal(err)
		}
		var back map[string]any
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if _, ok := back["labelScopeDegraded"]; !ok {
			t.Errorf("[%s] JSON 里必须恒有 labelScopeDegraded:%s", tc.name, raw)
		}
		if _, ok := back["labelScope"]; !ok {
			t.Errorf("[%s] JSON 里必须恒有 labelScope:%s", tc.name, raw)
		}
	}
	// 降级原因必须可读、且给出核对命令。
	msg := labelScopeReason(schZoneLabelScope{Degraded: true, NoPinGeometry: []string{"U1", "R1"}, UnownedMarkers: 3})
	for _, want := range []string{"U1", "R1", "引脚几何", "sch clusters"} {
		if !strings.Contains(msg, want) {
			t.Errorf("降级说明缺 %q:%s", want, msg)
		}
	}
}

// 降级兜底路径(读不到导线)的 marker 收编:按**最近的认领件**归属,不再用
// 拍出来的 60 硬截断;而且这条路径恒被标记降级。
func TestModulesFromClaimsScoped_DegradedFoldUsesNearestPartReach(t *testing.T) {
	pb := layoutBBox{300, 300, 340, 340}
	mb := layoutBBox{300, 200, 340, 220} // 锚点离本体 80 > 旧常数 60
	far := layoutBBox{300, 20, 340, 40}  // 离本体 260 > schMarkerFoldReach(180)
	comps := []layoutComp{
		{ID: "a", Designator: "C1", ComponentType: "part", BBox: &pb, PinsAvailable: true, PinsProofKnown: true},
		{ID: "m", ComponentType: "netflag", Net: "GND", BBox: &mb, X: 320, Y: 220, AnchorAvailable: true},
		{ID: "f", ComponentType: "netflag", Net: "VCC", BBox: &far, X: 320, Y: 40, AnchorAvailable: true},
	}
	zones := map[string]*schZoneClaim{"Z": {Parts: []string{"C1"}}}
	mods, scope := modulesFromClaimsScoped(zones, comps, nil)
	if !scope.Degraded || !scope.WiresUnavailable {
		t.Fatalf("无导线路径必须恒报降级:%+v", scope)
	}
	if len(mods) != 1 {
		t.Fatalf("want 1 module, got %+v", mods)
	}
	if !bboxContains(mods[0].BBox, mb) {
		t.Errorf("80 远的旗必须被收编(旧的 60 硬截断会漏掉它):%s", bboxText(mods[0].BBox))
	}
	if bboxContains(mods[0].BBox, far) {
		t.Errorf("超出 schMarkerFoldReach(%.0f)的旗不该硬塞进来:%s", float64(schMarkerFoldReach), bboxText(mods[0].BBox))
	}
}

// ── 端到端(假连接器):真 bug 在管线上,不在纯函数里 ────────────────────────
//
// `computePartitionPlan` 拉几何时只要了 includeBBox、没要 includePins ——
// buildSchClusters 靠引脚坐标把桩线连通块认到器件头上,没有引脚就一根线也归不了属,
// 于是每个 L1 组的体积原样退化成器件本体,而 clusterOf 非空、每个键都在,
// labelScopeDegraded 恒 false。整条链路上没有一个函数是"错"的,错的是输入。

type zoneFrameE2EOpts struct {
	pinsAvailable bool
}

func zoneFramePlanE2E(t *testing.T, o zoneFrameE2EOpts) (partitionPlan, []autolayoutTestCall) {
	t.Helper()
	t.Setenv(workflow.EnvDir, t.TempDir())
	st := &workflow.State{Project: "zone-project"}
	st.SetSchZonesForPage("page-a", map[string]*workflow.SchZoneClaim{
		"BUCK": {Zone: "center", Parts: []string{"U1"}},
	})
	if err := workflow.Save(st); err != nil {
		t.Fatal(err)
	}
	// U1 本体 (380,600)..(440,660);GND 旗挂在 pin(410,600) 下方 (390,500)..(430,530),
	// 由一根桩线 (410,600)→(410,530) 相连。旗离本体 70 —— 只有走导线归属才收得进来。
	pinsField := `,"pinsAvailable":false`
	if o.pinsAvailable {
		pinsField = `,"pinsAvailable":true,"pins":[{"pinNumber":"1","x":410,"y":600}]`
	}
	comps := `{"components":[` +
		`{"primitiveId":"sheet1","componentType":"sheet","x":585,"y":412,"bbox":{"minX":0,"minY":0,"maxX":1170,"maxY":825}},` +
		`{"primitiveId":"u1","designator":"U1","componentType":"part","x":410,"y":630,` +
		`"bbox":{"minX":380,"minY":600,"maxX":440,"maxY":660}` + pinsField + `},` +
		`{"primitiveId":"f1","componentType":"netflag","net":"GND","x":410,"y":530,` +
		`"bbox":{"minX":390,"minY":500,"maxX":430,"maxY":530}}` +
		`],"count":3}`

	cfg, daemon, cleanup := newAutolayoutTestDaemon(t, func(_ int, call autolayoutTestCall) string {
		switch call.Action {
		case "document.current":
			return autolayoutOK("page-a", `{"uuid":"page-a"}`)
		case "schematic.pages.list":
			return autolayoutOK("page-a", `{"pages":[{"uuid":"page-a","name":"Page A"}]}`)
		case "pcb.documents.list":
			return autolayoutOK("page-a", `{"pcbs":[]}`)
		case "project.current":
			return autolayoutOK("page-a", `{"friendlyName":"zone-project"}`)
		case "schematic.components.list":
			return autolayoutOK("page-a", comps)
		case "debug.exec_js":
			return autolayoutOK("page-a", `{"value":{"wires":[{"id":"w1","line":[410,600,410,530]}]}}`)
		case "schematic.text.list":
			return autolayoutOK("page-a", `{"texts":[]}`)
		default:
			return autolayoutOK("page-a", `{}`)
		}
	})
	defer cleanup()
	cfg.doc = "page-a"

	plan, _, err := computePartitionPlan(cfg, "", "page-a", defaultPartitionOpts())
	if err != nil {
		t.Fatalf("computePartitionPlan: %v", err)
	}
	return plan, daemon.snapshot()
}

func TestComputePartitionPlan_FrameCoversTheWholeL1Group(t *testing.T) {
	plan, calls := zoneFramePlanE2E(t, zoneFrameE2EOpts{pinsAvailable: true})

	// ① 回归守卫:几何回读必须带 includePins,否则 L1 归属结构上做不成。
	found := false
	for _, c := range calls {
		if c.Action == "schematic.components.list" {
			found = true
			if c.Payload["includePins"] != true {
				t.Fatalf("几何回读必须带 includePins(否则 marker/桩线归不了属):%v", c.Payload)
			}
		}
	}
	if !found {
		t.Fatal("没发出 components.list?")
	}

	// ② 框必须罩住整个 L1 虚拟组 —— 包括那支离本体 70 远的 GND 旗和它的桩线。
	if len(plan.Partitions) != 1 {
		t.Fatalf("want 1 partition, got %+v", plan.Partitions)
	}
	flag := layoutBBox{390, 500, 430, 530}
	if !bboxContains(plan.Partitions[0].BBox, flag) {
		t.Errorf("GND 旗 %s 垂在框 %s 外面(用户截图一眼看出的那个症状)",
			bboxText(flag), bboxText(plan.Partitions[0].BBox))
	}
	if plan.LabelScopeDegraded {
		t.Errorf("引脚 + 导线都读到了,不该报降级:%+v", plan.LabelScope)
	}
	if !plan.Validation.clean() {
		t.Errorf("计划应干净:%+v", plan.Validation)
	}
}

// 负对照:连接器报 pinsAvailable=false(引脚读失败)时,归属结构上做不成 ——
// 必须显式降级 + fail-closed,而不是像修复前那样静默报六项全 0。
func TestComputePartitionPlan_NoPinGeometryFailsClosed(t *testing.T) {
	plan, _ := zoneFramePlanE2E(t, zoneFrameE2EOpts{pinsAvailable: false})
	if !plan.LabelScopeDegraded || !plan.LabelScope.Degraded {
		t.Fatalf("引脚读失败必须报降级:%+v", plan.LabelScope)
	}
	if !strInSlice(plan.LabelScope.NoPinGeometry, "U1") {
		t.Errorf("降级必须点名 U1:%+v", plan.LabelScope)
	}
	if plan.Validation.ModuleOutsideZone == 0 {
		t.Fatal("口径不可信时判据不许报 0(那正是真机上的假绿)")
	}
	// 框此时只罩得住器件本体 —— 判据说不可信,说的是实话。
	if bboxContains(plan.Partitions[0].BBox, layoutBBox{390, 500, 430, 530}) {
		t.Errorf("fixture 失效:没有引脚就不该收得进那支旗,got %s", bboxText(plan.Partitions[0].BBox))
	}
}
