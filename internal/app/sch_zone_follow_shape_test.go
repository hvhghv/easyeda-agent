package app

import (
	"strings"
	"testing"
)

// sch_zone_follow_shape_test.go — phase A **域感知选形**的四组对照。
//
// ── 缺陷(2026-08-20 用户真机取证:ceshi / 页 MCU_IO / A4)──────────────────
//
// phase A 给每个区选形状时**完全不看空地长什么样**:
//
//	① 「无主导锚件」那条支路**根本没有候选** —— 「全员单列」是硬编码的。5 个
//	   0402/0805 小无源件(C 去耦 ×3 + R 上拉 ×2)于是被排成一根又细又高的柱子;
//	② 锚件支路虽有三个候选,目标函数却是 argmin max(w,h)(求方),同样域盲。
//
// 这一页的可用域 1110×765 被图签切成两条通道:**左通道 396 宽 × 765 高**、
// **上通道 1110 宽 × 555 高**。四个区收敛后:
//
//	wroom-core      325×556.5   高 556.5 > 555 → 只进得了左通道
//	wroom-passives  152×696     高 696   > 555 → **也**只进得了左通道
//	led_indicator   172×314     两条通道都进得去
//	tactile_reset   207×328     两条通道都进得去
//
// 于是 core 与 passives 抢同一条 396 宽的通道:并排 325+152+12 > 396、上下叠
// 556.5+696+12 > 765 → phase B blocked(报「wroom-core 被 wroom-passives 挡」)。
// 那 5 个小件排成 3+2 的货架只有 261×352,**上通道轻轻松松就吃下了**。
//
// 关键在于:两个区**各自**的 fitRank 都是 2(都有落点)—— 「不得变差」门的掉档
// 判据结构上看不见这种病。病在**落点自由度**,不在「有没有落点」。所以选形用
// 两把钥匙:fitRank(三档)+ stripFits(**几条通道装得下**),两者都是 zfDomain
// 同一份 strips() 的投影(fits 已重写成 stripFits>0),与 phase B 同源。
//
// ── 四组对照(全部机械可验)────────────────────────────────────────────────
//
//	正对照      四个区 → phase A → phase B 必须落位 + validation 四项 0
//	针对性      5 个小件那一区,收敛结果必须装得进通道(不再是只能进左通道的柱子)
//	负对照 A    已经很好、两条通道都装得下的区,**不许**被改成一根横条
//	负对照 B    所有候选都装不进任何通道的区,照常 blocked 且理由可读
//
// **变异验证不需要手工改代码**:`zfDomain{}`(域未知)按设计就退回首版那套
// 域盲偏好(硬编码单列 / argmin max(w,h)),所以正对照与针对性 fixture 都把
// 「域盲跑一遍必须复现缺陷」写成常驻断言 —— 修复被改回去,这两条当场转红。

// ── fixture:四个区 ─────────────────────────────────────────────────────────
//
// 真机只留下了四个区的**框尺寸**(179×265 / 179×346 / 349×609 / 181×652),没有
// 逐件 bbox,所以本 fixture 是重建的,并尽量复用仓库里已经反标定过的两份:
//   - wroom-core     = zfFixtureWroom6(第二轮真机那一区的重建,gate 测试在用);
//   - wroom-passives = wroomZfGroups() 的五个小件(块 fixture,整组框已按真机
//     507×712 反标定 ±15%)。
// led / tactile 两区按「LED+限流电阻」「两颗轻触开关」的实际拓扑重建。
//
// **fixture 的判别力不靠数值像不像,靠通道归属**:core 与 passives(域盲形态)
// 必须双双只进得了同一条通道,led / tactile 必须两条都进得去 —— 下面每个用例
// 都先把这个前提断言一遍,fixture 一旦失效会自己喊出来,不会静默变成自证。

func zfShapePassiveGroups() []zfGroup {
	fx := wroomZfGroups()
	out := make([]zfGroup, 0, 5)
	for _, role := range []string{"C_BULK", "C_VDD", "C_EN", "R_EN", "R_IO0"} {
		g, ok := fx[role]
		if !ok {
			panic("wroomZfGroups 缺 role " + role)
		}
		out = append(out, g)
	}
	return out
}

// zfShapeLEDGroups:LED + 限流电阻(真机 led_indicator_gpio 179×265)。
func zfShapeLEDGroups() []zfGroup {
	return []zfGroup{
		{Designator: "LED1", BodyW: 24, BodyH: 34, Terms: []zfTerm{
			{Kind: "netport", Net: "LED_IO", W: 74, Side: "left"},
			{Kind: "netflag", Net: "GND", W: 37, H: 23, Side: "down"},
		}},
		{Designator: "R5", BodyW: 24, BodyH: 12, Terms: []zfTerm{
			{Kind: "netport", Net: "LED_IO", W: 74, Side: "right"},
			{Kind: "netflag", Net: "3V3", W: 30, H: 20, Side: "up"},
		}},
	}
}

// zfShapeSwitchGroups:BOOT / RESET 两颗轻触开关(真机 tactile_boot_reset 179×346)。
func zfShapeSwitchGroups() []zfGroup {
	return []zfGroup{
		{Designator: "SW1", BodyW: 40, BodyH: 30, MultiPin: true, Terms: []zfTerm{
			{Kind: "netport", Net: "BOOT", W: 60, Side: "left"},
			{Kind: "netflag", Net: "GND", W: 37, H: 23, Side: "down"},
		}},
		{Designator: "SW2", BodyW: 40, BodyH: 30, MultiPin: true, Terms: []zfTerm{
			{Kind: "netport", Net: "RESET", W: 70, Side: "left"},
			{Kind: "netflag", Net: "GND", W: 37, H: 23, Side: "down"},
		}},
	}
}

// zfShapeZones 是那一页的四个区(名字 + 成员 + 现状质心,顺序固定)。
func zfShapeZones() []struct {
	name   string
	groups []zfGroup
	home   [2]float64
} {
	core, _ := zfFixtureWroom6()
	return []struct {
		name   string
		groups []zfGroup
		home   [2]float64
	}{
		{"wroom-core", []zfGroup{core}, [2]float64{532, 410}},
		{"wroom-passives", zfShapePassiveGroups(), [2]float64{200, 600}},
		{"led_indicator_gpio", zfShapeLEDGroups(), [2]float64{900, 400}},
		{"tactile_boot_reset", zfShapeSwitchGroups(), [2]float64{850, 250}},
	}
}

// zfShapePlan 走**生产入口**(planZoneFollowGated:收敛 + 「不得变差」门)。
func zfShapePlan(t *testing.T, zone string, groups []zfGroup, dom zfDomain) zfZonePlan {
	t.Helper()
	p, err := planZoneFollowGated(zone, groups, defaultPartitionOpts(), dom)
	if err != nil {
		t.Fatalf("phase A(%s): %v", zone, err)
	}
	return p
}

// zfShapeChannels 报「这个框进得了哪几条通道」(归因用;算术复用 dom.strips())。
func zfShapeChannels(d zfDomain, w, h float64) []string {
	if !d.fitsBare(w, h) {
		return nil
	}
	if d.Keep == nil {
		return []string{"整页"}
	}
	const eps = 1e-9
	left, right, below, above := d.strips()
	var out []string
	for _, s := range []struct {
		name        string
		avail, need float64
	}{{"左", left, w}, {"右", right, w}, {"下", below, h}, {"上", above, h}} {
		if s.avail > 0 && s.need <= s.avail+eps {
			out = append(out, s.name)
		}
	}
	return out
}

// zfShapeArrange 把四个区过一遍 phase A(给定域)再喂 phase B。
func zfShapeArrange(t *testing.T, dom zfDomain) (zaResult, map[string]zfZonePlan) {
	t.Helper()
	opts := defaultPartitionOpts()
	sheet, keepout, _ := zfGateA4Domain(opts)
	plans := map[string]zfZonePlan{}
	var zones []zaZone
	for _, z := range zfShapeZones() {
		p := zfShapePlan(t, z.name, z.groups, dom)
		plans[z.name] = p
		zones = append(zones, zaZone{Name: z.name, W: p.FrameW, H: p.FrameH, Home: z.home})
	}
	return zonesArrange(zones, sheet, keepout, opts), plans
}

// ── 正对照:域感知之后 phase B 必须落位 ─────────────────────────────────────
//
// 同一份四区几何跑两遍,唯一的差别是 phase A 知不知道空地长什么样:
//
//	域盲(zfDomain{})   → 缺陷复现:blocked
//	域感知(真域)        → 全部落位 + validation 四项 0
func TestZfShape_MCUIOFourZones_DomainAwareUnblocksPhaseB(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet, keepout, dom := zfGateA4Domain(opts)

	// ① 缺陷复现:域盲选形(= 首版的硬编码单列 / argmin max(w,h))排不下。
	blind, blindPlans := zfShapeArrange(t, zfDomain{})
	core, pass := blindPlans["wroom-core"], blindPlans["wroom-passives"]
	coreCh := zfShapeChannels(dom, core.FrameW, core.FrameH)
	passCh := zfShapeChannels(dom, pass.FrameW, pass.FrameH)
	if len(coreCh) != 1 || len(passCh) != 1 || coreCh[0] != passCh[0] {
		t.Fatalf("fixture 失效:缺陷的前提是两个区抢同一条通道,实际 core %.0f×%.0f→%v、passives %.0f×%.0f→%v",
			core.FrameW, core.FrameH, coreCh, pass.FrameW, pass.FrameH, passCh)
	}
	if blind.OK {
		t.Fatalf("fixture 失效:域盲选形本该 blocked(两个区抢 %q 通道),却排下了", coreCh[0])
	}
	t.Logf("域盲复现:blocked=%s tried=%s(core %.0f×%.0f、passives %.0f×%.0f 都只进得了「%s」通道)",
		blind.Blocked, blind.Tried, core.FrameW, core.FrameH, pass.FrameW, pass.FrameH, coreCh[0])

	// ② 域感知:同一批区必须全部落位。
	aware, awarePlans := zfShapeArrange(t, dom)
	if !aware.OK {
		t.Fatalf("域感知选形后仍排不下:blocked=%s tried=%s", aware.Blocked, aware.Tried)
	}
	if len(aware.Placed) != 4 {
		t.Fatalf("落位数 %d ≠ 区数 4", len(aware.Placed))
	}
	v := zaValidate(aware, sheet, keepout, opts)
	if v.SheetOverflow != 0 || v.PartitionOverlap != 0 || v.TitleBlockHits != 0 || v.SheetMarginHits != 0 {
		t.Fatalf("validation 四项该全 0,得到 %+v", v)
	}
	for _, z := range zfShapeZones() {
		p := awarePlans[z.name]
		t.Logf("  %-20s 框 %.0f×%.0f 通道 %v | %s", z.name, p.FrameW, p.FrameH,
			zfShapeChannels(dom, p.FrameW, p.FrameH), p.Mode)
	}
}

// ── 针对性:5 个小无源件那一区 ──────────────────────────────────────────────
//
// 域盲时它是一根 152×696 的柱子 —— **只**进得了左通道(696 > 上通道的 555),
// 于是跟主控区抢同一条道。域感知后必须装得进通道,而且不再是「只进一条」。
func TestZfShape_SmallPassivesGetAFlatShape(t *testing.T) {
	_, _, dom := zfGateA4Domain(defaultPartitionOpts())
	groups := zfShapePassiveGroups()

	blind := zfShapePlan(t, "wroom-passives", groups, zfDomain{})
	blindCh := zfShapeChannels(dom, blind.FrameW, blind.FrameH)
	if len(blindCh) != 1 {
		t.Fatalf("fixture 失效:域盲的单列本该只进得了一条通道,得到 %v(%.0f×%.0f)",
			blindCh, blind.FrameW, blind.FrameH)
	}
	if !strings.Contains(blind.Mode, "单列") {
		t.Fatalf("域盲路径该是首版的「全员单列」:%s", blind.Mode)
	}

	got := zfShapePlan(t, "wroom-passives", groups, dom)
	ch := zfShapeChannels(dom, got.FrameW, got.FrameH)
	if len(ch) == 0 {
		t.Fatalf("收敛结果 %.0f×%.0f 一条通道都装不进 —— 域感知没起作用:%s",
			got.FrameW, got.FrameH, got.Mode)
	}
	if n := dom.stripFits(got.FrameW, got.FrameH); n <= len(blindCh) {
		t.Fatalf("落点自由度没变好:域盲 %d 条通道 → 域感知 %d 条(%.0f×%.0f,%s)",
			len(blindCh), n, got.FrameW, got.FrameH, got.Mode)
	}
	// 五个 0402/0805 小件排出来的框不该再比主控区还高。
	if got.FrameH >= blind.FrameH {
		t.Errorf("高度没收下来:%.0f → %.0f", blind.FrameH, got.FrameH)
	}
	// 决策必须可见:换了形态就要在 Mode 里说清换成什么、为什么。
	if !strings.Contains(got.Mode, "域感知选形") || !strings.Contains(got.Mode, "改选") {
		t.Errorf("改了形态却没在 Mode 里交代:%s", got.Mode)
	}
	t.Logf("passives:域盲 %.0f×%.0f 通道%v → 域感知 %.0f×%.0f 通道%v | %s",
		blind.FrameW, blind.FrameH, blindCh, got.FrameW, got.FrameH, ch, got.Mode)
}

// ── 负对照 A:不许退化成「永远选最扁」───────────────────────────────────────
//
// 两个已经收敛得很好、两条通道都装得下的区(真机 led_indicator_gpio 179×265、
// tactile_boot_reset 179×346):域感知的两把钥匙在这里全平局 → 必须**逐字**保持
// 原有紧凑性偏好选出来的那个形态(单列),一个单位都不许动。
func TestZfShape_AlreadyGoodZonesKeepCompactShape(t *testing.T) {
	_, _, dom := zfGateA4Domain(defaultPartitionOpts())
	for _, tc := range []struct {
		zone   string
		groups []zfGroup
	}{
		{"led_indicator_gpio", zfShapeLEDGroups()},
		{"tactile_boot_reset", zfShapeSwitchGroups()},
	} {
		blind := zfShapePlan(t, tc.zone, tc.groups, zfDomain{})
		if n := len(zfShapeChannels(dom, blind.FrameW, blind.FrameH)); n < 2 {
			t.Fatalf("%s fixture 失效:负对照的前提是「两条通道都装得下」,实际只有 %d 条(%.0f×%.0f)",
				tc.zone, n, blind.FrameW, blind.FrameH)
		}
		// 前提②:候选里**确实存在**更扁的形状(否则这条负对照钉不住任何东西)。
		flat := false
		for _, c := range zfShapeCandFrames(tc.groups) {
			if c.w > blind.FrameW && c.h < blind.FrameH {
				flat = true
			}
		}
		if !flat {
			t.Fatalf("%s fixture 失效:候选里没有更扁的形状,负对照是自证", tc.zone)
		}

		got := zfShapePlan(t, tc.zone, tc.groups, dom)
		if got.FrameW != blind.FrameW || got.FrameH != blind.FrameH {
			t.Errorf("%s 已经很好的区被域感知改形了:%.0f×%.0f → %.0f×%.0f(%s)",
				tc.zone, blind.FrameW, blind.FrameH, got.FrameW, got.FrameH, got.Mode)
		}
		if got.FrameW > got.FrameH {
			t.Errorf("%s 被摊成了横条(%.0f×%.0f)—— 紧凑性偏好失效", tc.zone, got.FrameW, got.FrameH)
		}
		if !strings.Contains(got.Mode, "原有偏好即最优") {
			t.Errorf("%s 该走「原有偏好即最优」分支:%s", tc.zone, got.Mode)
		}
		// 落位也必须逐字相同(形状没变,groups 的局部坐标就不该变)。
		if len(got.Groups) != len(blind.Groups) {
			t.Fatalf("%s 组数变了", tc.zone)
		}
		for i := range got.Groups {
			if zfGroupBBox(got.Groups[i]) != zfGroupBBox(blind.Groups[i]) {
				t.Errorf("%s 组 %s 的落位被动过了", tc.zone, got.Groups[i].Designator)
			}
		}
	}
}

// zfShapeCandFrames 是候选族的框尺寸(测试侧只读,走的是生产的候选生成 +
// 同一个外框函数)。
func zfShapeCandFrames(groups []zfGroup) []struct{ w, h float64 } {
	opts := defaultPartitionOpts()
	var gen []zfGenned
	for _, g := range groups {
		pg, err := zfGenGroup(g)
		if err != nil {
			return nil
		}
		gen = append(gen, zfGenned{pg, zfGroupBBox(pg), g.MultiPin})
	}
	cands := zfShapeCands(gen)
	out := make([]struct{ w, h float64 }, 0, len(cands))
	for i := range cands {
		zfFinishCand(&cands[i], opts)
		out = append(out, struct{ w, h float64 }{cands[i].w, cands[i].h})
	}
	return out
}

// ── 负对照 B:真无解要照样报,而且理由可读 ─────────────────────────────────
//
// 六个大件:竖着摆太高(244×1994)、横着摆太宽(1274 > 可用 1110)、网格摆
// 两头不靠(656×738:比左通道宽、比上通道高)—— **没有一个候选装得进任何一条
// 通道**。这一组的判别力在于候选**不是一样烂**:三个网格候选是 1 档(装得进
// 可用域,只是被图签挡),其余是 0 档。选形必须爬到能爬的最高档,然后**如实
// 报告一条通道都进不去**,而不是挑一个「看起来最方」的假装没事;phase B 照常
// blocked,逐边归因读得出来。
func TestZfShape_NoCandidateFits_StaysBlockedWithReadableWhy(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet, keepout, dom := zfGateA4Domain(opts)
	var groups []zfGroup
	for _, d := range []string{"U10", "U11", "U12", "U13", "U14", "U15"} {
		groups = append(groups, zfGroup{Designator: d, BodyW: 200, BodyH: 186, Terms: []zfTerm{
			{Kind: "netflag", Net: "3V3", W: 30, H: 20, Side: "up"},
			{Kind: "netflag", Net: "GND", W: 37, H: 23, Side: "down"},
		}})
	}
	// 前提:候选族里一个都装不进通道,但**存在**比首版形态更高档的候选
	// (逐候选机械判定,不是口头推断)。
	bestRank := 0
	for i, c := range zfShapeCandFrames(groups) {
		if n := dom.stripFits(c.w, c.h); n != 0 {
			t.Fatalf("fixture 失效:候选 #%d(%.0f×%.0f)装得进 %d 条通道", i, c.w, c.h, n)
		}
		if r := dom.fitRank(c.w, c.h); r > bestRank {
			bestRank = r
		}
	}
	if bestRank != 1 {
		t.Fatalf("fixture 失效:候选族最高档该是 1(装得进可用域但被图签挡),得到 %d", bestRank)
	}
	got := zfShapePlan(t, "fat_zone", groups, dom)
	if r := dom.fitRank(got.FrameW, got.FrameH); r != bestRank {
		t.Errorf("没爬到能爬的最高档:选中 %.0f×%.0f(%d 档),族里有 %d 档",
			got.FrameW, got.FrameH, r, bestRank)
	}
	if !strings.Contains(got.Mode, "没有一个装得进任何通道") {
		t.Errorf("无解必须说人话(说清没救 + 下一步),得到:%s", got.Mode)
	}
	for _, want := range []string{"拆区", "page-new"} {
		if !strings.Contains(got.Mode, want) {
			t.Errorf("归因里读不到出路 %q:%s", want, got.Mode)
		}
	}
	res := zonesArrange([]zaZone{{Name: "fat_zone", W: got.FrameW, H: got.FrameH,
		Home: [2]float64{500, 500}}}, sheet, keepout, opts)
	if res.OK {
		t.Fatalf("一条通道都装不进的区居然落位了 —— 选形与求解器分家了(%.0f×%.0f)",
			got.FrameW, got.FrameH)
	}
	t.Logf("真无解:框 %.0f×%.0f | %s | phase B %s", got.FrameW, got.FrameH, got.Mode, res.Tried)
}

// ── 一把尺:fits 与 stripFits 是同一份通道算术的两个投影 ────────────────────
//
// 「通道装得下几条」和「还有没有落点」如果各算各的,就是又造了一把尺 ——
// 这个仓库刚为同一件事付过两次学费(判定与生成两把尺、fits 单布尔分不清
// 「被图签挡」和「纸面放不下」)。
func TestZfStripFits_SameRulerAsFits(t *testing.T) {
	opts := defaultPartitionOpts()
	_, _, dom := zfGateA4Domain(opts)
	empty := zfDomain{L: 30, R: 1140, B: 30, T: 795} // 无图签的同尺寸页
	for _, d := range []zfDomain{dom, empty} {
		for w := 40.0; w <= 1250; w += 37 {
			for h := 40.0; h <= 900; h += 41 {
				if got, want := d.stripFits(w, h) > 0, d.fits(w, h); got != want {
					t.Fatalf("%.0f×%.0f:stripFits>0=%v 而 fits=%v —— 两把尺", w, h, got, want)
				}
				// 单调:两维都不变大,通道数不许变少(选形与门都靠这条性质)。
				if a, b := d.stripFits(w, h), d.stripFits(w-10, h-10); b < a {
					t.Fatalf("%.0f×%.0f 通道数 %d,收小到 %.0f×%.0f 反而 %d —— 不单调",
						w, h, a, w-10, h-10, b)
				}
			}
		}
	}
	// 域未知必须自己认得出来(否则零值域会把所有候选判成 0 档,静默改选形)。
	if (zfDomain{}).known() {
		t.Fatal("零值域不该自称已知")
	}
	if !dom.known() {
		t.Fatal("A4 域该是已知的")
	}
}
