package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
)

func subgroupBlock(t *testing.T) (blocks.Block, bslRelations) {
	t.Helper()
	blk, ok, err := blocks.Get("block.ch340c_usb_serial")
	if err != nil || !ok {
		t.Fatalf("取不到块: %v", err)
	}
	layout, lerr := blk.SchematicLayout()
	if lerr != nil {
		t.Fatal(lerr)
	}
	rel, isRel := bslRelationsFrom(layout)
	if !isRel {
		t.Fatal("ch340c 应该是关系形态模板")
	}
	return blk, rel
}

// bslRealBlockSubgroups 走真块的完整链:取块 → 关系模板 → 选锚件 → 拆子群。
// 锚件也让 bslAnchorRole 自己选(不手喂),这样测的是 block-apply 真正走的那条路。
func bslRealBlockSubgroups(t *testing.T, id string) (blocks.Block, []bslSubgroup) {
	t.Helper()
	blk, ok, err := blocks.Get(id)
	if err != nil || !ok {
		t.Fatalf("取不到块 %s: %v", id, err)
	}
	layout, lerr := blk.SchematicLayout()
	if lerr != nil {
		t.Fatal(lerr)
	}
	rel, isRel := bslRelationsFrom(layout)
	if !isRel {
		t.Fatalf("%s 应该是关系形态模板", id)
	}
	anchor, aerr := bslAnchorRole(blk, rel, bslBlockNets(blk))
	if aerr != nil {
		t.Fatal(aerr)
	}
	return blk, bslFunctionalGroups(blk, rel, bslBlockNets(blk), anchor)
}

// bslAssertSubgroups 逐群精确断言(群名 + 成员,逐字),并证明「每个 role 恰好
// 属于一个子群」—— 不留孤儿、不重复分身。
func bslAssertSubgroups(t *testing.T, blk blocks.Block, got []bslSubgroup, want map[string][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("应拆成 %d 群,得到 %d: %+v", len(want), len(got), got)
	}
	for _, g := range got {
		w, ok := want[g.Name]
		if !ok {
			t.Fatalf("多出一个子群 %q: %+v", g.Name, got)
		}
		if strings.Join(g.Roles, ",") != strings.Join(w, ",") {
			t.Errorf("子群 %s 成员 = %v, want %v", g.Name, g.Roles, w)
		}
	}
	seen := map[string]int{}
	for _, g := range got {
		for _, r := range g.Roles {
			seen[r]++
		}
	}
	for r := range blk.Parts {
		if seen[r] != 1 {
			t.Errorf("role %s 被分到 %d 个子群(应恰好 1)", r, seen[r])
		}
	}
}

// 拆分必须**只来自块数据**:flow 每级一群、attach 跟宿主、pair 跟它连的那级。
// 真块验证 —— ch340c 应拆成「USB 口 / ESD / 桥芯片」三群,和人手认领的一致。
//
// **这条同时是 flow 路径的回归钉子**(2026-08-20 加引脚路径时):真值取自现网
// (ceshi/MCU_IO 登记出来的 `/D_ESD` `/J_USB` `/U`),群名与成员必须**逐字不变**。
// CH340C 的 attach 也指着两个不同的脚(U.VCC / U.V3),若把「按引脚细分」也套到
// flow 路径上,`/U` 会当场裂成三份 —— 那是回归,不是修复。
func TestBslFunctionalGroups_ChinaCH340C(t *testing.T) {
	blk, rel := subgroupBlock(t)
	got := bslFunctionalGroups(blk, rel, bslBlockNets(blk), "U")

	want := map[string][]string{
		"J_USB": {"J_USB", "R_CC1", "R_CC2"}, // CC 下拉挂在 USB 口上
		"D_ESD": {"D_ESD"},
		"U":     {"C_V3", "C_VCC", "U"}, // 去耦跟宿主
	}
	bslAssertSubgroups(t, blk, got, want)
}

// 没有 flow 的小块(只有芯片 + 去耦)不该被硬拆 —— 那本来就是一个功能单元,
// 拆开只会把去耦和它的芯片画进两个框。
func TestBslFunctionalGroups_NoFlowStaysOneGroup(t *testing.T) {
	raw := map[string]any{
		"id": "block.tiny", "desc": "t",
		"parts": map[string]any{
			"U":  map[string]any{"part": "ic.ch340c", "qty": 1},
			"C1": map[string]any{"part": "cap.100nf_0402", "qty": 1},
		},
		"internal_nets": []any{[]any{"U.VCC", "C1.1"}},
	}
	j, _ := json.Marshal(raw)
	var blk blocks.Block
	_ = json.Unmarshal(j, &blk)
	blk.Raw = j
	rel := bslRelations{Attach: map[string]string{"C1": "U.VCC"}}
	got := bslFunctionalGroups(blk, rel, bslBlockNets(blk), "U")
	if len(got) != 1 || len(got[0].Roles) != 2 {
		t.Fatalf("无 flow 的块应保持一个子群(含全部件): %+v", got)
	}
	if got[0].Name != "tiny" {
		t.Errorf("单子群该用块短名: %q", got[0].Name)
	}
}

// ── 无 flow 的块:按 attach 目标引脚拆(2026-08-20 真机缺陷)───────────────

// 真机(ceshi / MCU_IO):`esp32s3_wroom1_module` 没有 flow,整块 6 件登记成一个组,
// phase A 收敛后仍是 507×712 —— 独占一整页也放不进 A4,`zone-arrange` 逐边报
// 「被图签挡」。功能划分的信息本来就在块里,只是 attach 归属把 `U.3V3`/`U.EN`/
// `U.IO0` 一律归约成了 `U`,引脚那一半被丢掉。
//
// 拆分真值(逐群成员精确断言):锚件一群 + 三个引脚簇。
func TestBslFunctionalGroups_WroomSplitsByAttachPin(t *testing.T) {
	blk, got := bslRealBlockSubgroups(t, "block.esp32s3_wroom1_module")
	want := map[string][]string{
		"esp32s3_wroom1_module": {"U"},               // 锚件(群名继承拆分前的块名)
		"U_3V3":                 {"C_BULK", "C_VDD"}, // 供电去耦
		"U_EN":                  {"C_EN", "R_EN"},    // EN 上电复位 RC
		"U_IO0":                 {"R_IO0"},           // IO0 boot strap
	}
	bslAssertSubgroups(t, blk, got, want)
}

// 负对照①:attach 全部指向**同一个引脚**的块 —— 按脚分没有区分度,不许拆。
// (这条挡的是「把判据改成见谁拆谁」:件数够多也不能拆,拆出来只是「锚件」+
// 「其余全部」两个框。)
func TestBslFunctionalGroups_SamePinStaysOneGroup(t *testing.T) {
	parts := map[string]any{"U": map[string]any{"part": "mcu.esp32s3_wroom1", "qty": 1}}
	attach := map[string]string{}
	var nets []any
	for _, r := range []string{"C1", "C2", "C3", "C4", "C5"} {
		parts[r] = map[string]any{"part": "cap.100nf_0402", "qty": 1}
		attach[r] = "U.3V3"
		nets = append(nets, []any{"U.3V3", r + ".1"})
	}
	raw := map[string]any{"id": "block.samepin", "desc": "t", "parts": parts, "internal_nets": nets}
	j, _ := json.Marshal(raw)
	var blk blocks.Block
	_ = json.Unmarshal(j, &blk)
	blk.Raw = j
	got := bslFunctionalGroups(blk, bslRelations{Anchor: "U", Attach: attach}, bslBlockNets(blk), "U")
	if len(got) != 1 {
		t.Fatalf("attach 全指同一个脚的块不该被拆: %+v", got)
	}
	if len(got[0].Roles) != 6 || got[0].Name != "samepin" {
		t.Errorf("单子群该含全部 6 件、用块短名: %+v", got[0])
	}
}

// 负对照②:真块 `ams1117_ldo_3v3`(锚 + 3 件贴 2 个不同的脚)—— 引脚是分开的,
// 但件太少,拆开只是多两个带标题的框(锚件旁一列本来就排得下)。件数阈值
// (bslPinSplitMinAttach)就是被这块和 WROOM 两端钉住的。
func TestBslFunctionalGroups_SmallRealBlockStaysWhole(t *testing.T) {
	blk, got := bslRealBlockSubgroups(t, "block.ams1117_ldo_3v3")
	if len(got) != 1 {
		t.Fatalf("ams1117(4 件)不该被拆: %+v", got)
	}
	bslAssertSubgroups(t, blk, got, map[string][]string{
		"ams1117_ldo_3v3": {"C_BYP", "C_IN", "C_OUT", "U"},
	})
}

// attach 链(件贴在件上)必须解析到**根引脚**:C 贴 R.2、R 贴 U.EN 时两件同群,
// 否则 R 与吊在它身上的 C 会被画进两个框。同时验「不留孤儿」:没有任何 attach
// 声明的件(D1)按跨接网归到与它连得最紧的那群。
func TestBslPinSubgroups_AttachChainAndOrphans(t *testing.T) {
	parts := map[string]any{
		"U":  map[string]any{"part": "mcu.esp32s3_wroom1", "qty": 1},
		"R1": map[string]any{"part": "res.10k_0402", "qty": 1},
		"C1": map[string]any{"part": "cap.1uf_0402", "qty": 1},
		"C2": map[string]any{"part": "cap.100nf_0402", "qty": 1},
		"C3": map[string]any{"part": "cap.10uf_0805", "qty": 1},
		"D1": map[string]any{"part": "led.red_0603", "qty": 1},
	}
	raw := map[string]any{"id": "block.chain", "desc": "t", "parts": parts, "internal_nets": []any{
		[]any{"U.EN", "R1.1"},
		[]any{"R1.2", "C1.1"},
		[]any{"U.3V3", "C2.1", "C3.1"},
		[]any{"R1.2", "D1.1"}, // D1 没有 attach 声明 —— 只能靠跨接网归属
	}}
	j, _ := json.Marshal(raw)
	var blk blocks.Block
	_ = json.Unmarshal(j, &blk)
	blk.Raw = j
	rel := bslRelations{Anchor: "U", Attach: map[string]string{
		"R1": "U.EN", "C1": "R1.2", "C2": "U.3V3", "C3": "U.3V3",
	}}
	got := bslFunctionalGroups(blk, rel, bslBlockNets(blk), "U")
	bslAssertSubgroups(t, blk, got, map[string][]string{
		"chain": {"U"},
		"U_3V3": {"C2", "C3"},
		"U_EN":  {"C1", "D1", "R1"}, // C1 顺链归根;D1 跨接网最多的也是这一群
	})
}

// 确定性:同一个块反复求解,子群集合与顺序**完全一致**(map 遍历不驱动输出)。
// Go 每次 range map 的序都不同,重复求解就是对「顺序依赖」的穷举。
func TestBslFunctionalGroups_Deterministic(t *testing.T) {
	for _, id := range []string{"block.esp32s3_wroom1_module", "block.ch340c_usb_serial", "block.ams1117_ldo_3v3"} {
		_, base := bslRealBlockSubgroups(t, id)
		want, _ := json.Marshal(base)
		for i := 0; i < 200; i++ {
			_, got := bslRealBlockSubgroups(t, id)
			if g, _ := json.Marshal(got); string(g) != string(want) {
				t.Fatalf("%s 第 %d 次求解漂移:\n base=%s\n got =%s", id, i, want, g)
			}
		}
	}
}

// ── 验收⑤:拆了到底放不放得下(离线机械判据)────────────────────────────
//
// 口头推断不算数 —— 把「拆完能排下」变成机械判定:块的每个功能子群各过一遍
// **phase A**(planZoneFollow,收敛出区框),再把这些框喂给 **phase B**
// (zonesArrange,A4 + 图签 keep-out 的排布器)。判定用的是真机报 blocked 的
// 同一个函数,不是测试里另写的一把尺。
//
// **fixture 是重建的,不是逐图元实测**:真机只留下了「整组框 507×712」这一个
// 数,没有留每件的 bbox/marker。所以本 fixture 按 MCU_IO 那一页的形态重建
// (WROOM-1 41 脚模组 + 页面上的 netport/netflag,尺寸取 P3 真机 fixture
// zfFixtureU/J 的同量级),并**用整组框反标定**:重建出来的整组框必须落在真机
// 数值的 ±15% 内(见 TestBslSubgroups_WroomOneGroupBlocked),否则这条验收就是
// 在自说自话。
func wroomZfGroups() map[string]zfGroup {
	return map[string]zfGroup{
		// 锚件:WROOM-1 模组(41 脚),页面上引出 EN/IO0/串口/若干 IO 的 netport,
		// 3V3 与 GND 走 netflag。MultiPin —— 符号朝向锁死,端子保持实测侧。
		"U": {Designator: "U1", BodyW: 120, BodyH: 300, MultiPin: true, Terms: []zfTerm{
			{Kind: "netport", Net: "EN", W: 52, Side: "left"},
			{Kind: "netport", Net: "IO0", W: 58, Side: "left"},
			{Kind: "netport", Net: "TXD0", W: 66, Side: "left"},
			{Kind: "netport", Net: "RXD0", W: 66, Side: "left"},
			{Kind: "netport", Net: "RESET", W: 70, Side: "left"},
			{Kind: "netport", Net: "SPI_MISO", W: 84, Side: "left"},
			{Kind: "netport", Net: "LED_IO", W: 74, Side: "right"},
			{Kind: "netport", Net: "SDA", W: 56, Side: "right"},
			{Kind: "netport", Net: "SCL", W: 56, Side: "right"},
			{Kind: "netport", Net: "BOOT", W: 60, Side: "right"},
			{Kind: "netport", Net: "SPI_CLK", W: 78, Side: "right"},
			{Kind: "netport", Net: "SPI_MOSI", W: 84, Side: "right"},
			{Kind: "netflag", Net: "3V3", W: 30, H: 20, Side: "up"},
			{Kind: "netflag", Net: "GND", W: 37, H: 23, Side: "down"},
		}},
		"C_BULK": {Designator: "C1", BodyW: 24, BodyH: 34, Terms: []zfTerm{
			{Kind: "netflag", Net: "3V3", W: 30, H: 20, Side: "up"},
			{Kind: "netflag", Net: "GND", W: 37, H: 23, Side: "down"},
		}},
		"C_VDD": {Designator: "C2", BodyW: 20, BodyH: 30, Terms: []zfTerm{
			{Kind: "netflag", Net: "3V3", W: 30, H: 20, Side: "up"},
			{Kind: "netflag", Net: "GND", W: 37, H: 23, Side: "down"},
		}},
		"C_EN": {Designator: "C3", BodyW: 20, BodyH: 30, Terms: []zfTerm{
			{Kind: "netport", Net: "EN", W: 52, Side: "left"},
			{Kind: "netflag", Net: "GND", W: 37, H: 23, Side: "down"},
		}},
		"R_EN": {Designator: "R1", BodyW: 24, BodyH: 12, Terms: []zfTerm{
			{Kind: "netflag", Net: "3V3", W: 30, H: 20, Side: "up"},
			{Kind: "netport", Net: "EN", W: 52, Side: "right"},
		}},
		"R_IO0": {Designator: "R2", BodyW: 24, BodyH: 12, Terms: []zfTerm{
			{Kind: "netflag", Net: "3V3", W: 30, H: 20, Side: "up"},
			{Kind: "netport", Net: "IO0", W: 58, Side: "right"},
		}},
	}
}

// bslZoneFrameOf 把一组 role 过 phase A,返回收敛后的区框尺寸。
func bslZoneFrameOf(t *testing.T, name string, roles []string) (w, h float64) {
	t.Helper()
	fx := wroomZfGroups()
	groups := make([]zfGroup, 0, len(roles))
	for _, r := range roles {
		g, ok := fx[r]
		if !ok {
			t.Fatalf("fixture 缺 role %s —— 块改了就要同步 fixture", r)
		}
		groups = append(groups, g)
	}
	p, err := planZoneFollow(name, groups, defaultPartitionOpts())
	if err != nil {
		t.Fatalf("phase A(%s): %v", name, err)
	}
	return p.FrameW, p.FrameH
}

// 整块一个组 = 独占一整页也放不下(真机 blocked 的复现)。同时校准 fixture:
// 重建出来的整组框必须与真机 507×712 同量级(±15%)。
func TestBslSubgroups_WroomOneGroupBlocked(t *testing.T) {
	const realW, realH = 507.0, 712.0 // 真机取证:ceshi / MCU_IO,phase A 收敛后
	roles := []string{"U", "C_BULK", "C_VDD", "C_EN", "R_EN", "R_IO0"}
	w, h := bslZoneFrameOf(t, "esp32s3_wroom1_module", roles)
	if w < realW*0.85 || w > realW*1.15 || h < realH*0.85 || h > realH*1.15 {
		t.Fatalf("fixture 与真机数量级对不上:整组框 %.0f×%.0f,真机 %.0f×%.0f(±15%%)", w, h, realW, realH)
	}
	sheet, keepout := zaSheetA4()
	res := zonesArrange([]zaZone{{Name: "MCU_IO", W: w, H: h, Home: [2]float64{500, 500}}},
		sheet, keepout, defaultPartitionOpts())
	if res.OK {
		t.Fatalf("整块一个组(%.0f×%.0f)本该排不下 —— 缺陷复现失败,fixture 该重标定", w, h)
	}
	if !strings.Contains(res.Tried, "图签") {
		t.Errorf("blocked 该逐边归因到图签,得到 %q", res.Tried)
	}
}

// 拆完放得下:四个子群各自的区框,先逐个满足「避得开图签」的单区条件,
// 再作为一整套喂给排布器 —— 必须在同一页 A4 上全部落位。
func TestBslSubgroups_WroomFitsAfterSplit(t *testing.T) {
	_, subs := bslRealBlockSubgroups(t, "block.esp32s3_wroom1_module")
	if len(subs) != 4 {
		t.Fatalf("WROOM 应拆成 4 群,得到 %d: %+v", len(subs), subs)
	}
	sheet, keepout := zaSheetA4()
	opts := defaultPartitionOpts()
	// 单区可行域:图签 keep-out 按 titleBlockSafety 膨胀后,一个区要么整块待在
	// 图签左侧(宽 ≤ leftBand),要么绕到图签上方(高 ≤ aboveBand)。两条都不满足
	// 的区,独占一整页也放不下 —— 那正是整块一个组的下场。
	leftBand := (keepout.MinX - titleBlockSafety) - (sheet.MinX + opts.Margin)
	aboveBand := (sheet.MaxY - opts.Margin) - (keepout.MaxY + titleBlockSafety)
	zones := make([]zaZone, 0, len(subs))
	for _, sg := range subs {
		w, h := bslZoneFrameOf(t, sg.Name, sg.Roles)
		if w > leftBand && h > aboveBand {
			t.Errorf("子群 %s 的区框 %.0f×%.0f 仍绕不开图签(宽 ≤ %.0f 或 高 ≤ %.0f 才行)",
				sg.Name, w, h, leftBand, aboveBand)
		}
		zones = append(zones, zaZone{Name: sg.Name, W: w, H: h, Home: [2]float64{500, 500}})
	}
	res := zonesArrange(zones, sheet, keepout, opts)
	if !res.OK {
		t.Fatalf("拆完的 %d 个区仍排不下:blocked=%s tried=%s", len(zones), res.Blocked, res.Tried)
	}
	if len(res.Placed) != len(zones) {
		t.Fatalf("落位数 %d ≠ 区数 %d", len(res.Placed), len(zones))
	}
}
