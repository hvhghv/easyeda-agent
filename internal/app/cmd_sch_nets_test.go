package app

import "testing"

// 这条规则的风险全在**假警报**上:一条假警报就足以让人开始忽略整条规则,
// 而它要抓的缺陷(主控和稳压器没连上)恰恰是最贵的那种。所以测试的重心是
// 「哪些**不该**被合并」。

func TestNormalizeSchNetName_ConventionEquivalentsOnly(t *testing.T) {
	same := [][2]string{
		{"+3V3", "3V3"},       // 极性号
		{"+5V", "5V"},         //
		{"3V3", "3.3V"},       // V 代小数点是行业标准写法
		{"1V8", "1.8V"},       //
		{"vbus_5v", "VBUS5V"}, // 分隔符 + 大小写
		{"GND", "gnd"},
	}
	for _, p := range same {
		if normalizeSchNetName(p[0]) != normalizeSchNetName(p[1]) {
			t.Errorf("%q 与 %q 该归一到同一条轨(归一化得到 %q / %q)",
				p[0], p[1], normalizeSchNetName(p[0]), normalizeSchNetName(p[1]))
		}
	}

	// **绝不能合并**的:字母前缀带着电源域语义。
	diff := [][2]string{
		{"GND", "AGND"},   // 模拟地 ≠ 数字地(单点桥接是设计决定,不是笔误)
		{"GND", "PGND"},   // 功率地
		{"VCC", "VDD"},    // 不同电源域惯例
		{"VCC", "VCC_IO"}, // 子域
		{"3V3", "5V"},     // 不同电压
		{"3V3", "3V3_AU"}, // 音频专供轨
	}
	for _, p := range diff {
		if normalizeSchNetName(p[0]) == normalizeSchNetName(p[1]) {
			t.Errorf("%q 与 %q 被错误合并成 %q —— 会产生假警报",
				p[0], p[1], normalizeSchNetName(p[0]))
		}
	}
}

func TestAuditSchNets_CatchesTheRealDefect(t *testing.T) {
	// E2E #2 的真实现场:电源块出 +3V3,MCU/CH340 块要 3V3。
	nets := []schNetInfo{
		{Name: "+3V3", Pins: 3, Parts: []string{"C1", "C3", "U1"}},
		{Name: "3V3", Pins: 9, Parts: []string{"C4", "C6", "R1", "R2", "U2"}},
		{Name: "GND", Pins: 26, Parts: []string{"U1", "U2"}},
	}
	rep := auditSchNets(nets)
	if rep.OK {
		t.Fatal("没抓到 +3V3 / 3V3 —— 这正是主控与稳压器没连上的那个缺陷")
	}
	if len(rep.Variants) != 1 || len(rep.Variants[0].Nets) != 2 {
		t.Fatalf("变体分组不对:%+v", rep.Variants)
	}
	// 报告必须点名两个原名,否则不知道改哪个。
	got := rep.Variants[0].Nets
	if got[0].Name != "+3V3" || got[1].Name != "3V3" {
		t.Errorf("变体应按原名列出,得到 %q / %q", got[0].Name, got[1].Name)
	}
}

func TestAuditSchNets_AutoAndBlockInternalNetsExempt(t *testing.T) {
	// 块实例内部网与平台自动名本来就该各不相同,让它们参与判定会刷屏。
	nets := []schNetInfo{
		{Name: "Q_N3", Pins: 2}, {Name: "Q_N4", Pins: 2},
		{Name: "U3_N3", Pins: 2}, {Name: "U3_N4", Pins: 4},
		{Name: "$1N2", Pins: 2},
		{Name: "GND", Pins: 26},
	}
	if rep := auditSchNets(nets); !rep.OK {
		t.Errorf("块内网/自动名不该报变体:%+v", rep.Variants)
	}
}

func TestAuditSchNets_SinglePinNets(t *testing.T) {
	nets := []schNetInfo{
		{Name: "LED_CTRL", Pins: 1, Parts: []string{"R7"}},
		{Name: "GND", Pins: 26, Parts: []string{"U1"}},
	}
	rep := auditSchNets(nets)
	if len(rep.Lonely) != 1 || rep.Lonely[0].Name != "LED_CTRL" {
		t.Fatalf("该报单引脚网 LED_CTRL,得到 %+v", rep.Lonely)
	}
	// 单引脚网本身不翻 OK —— 它是 WARN 级(--strict 才阻塞),因为跨页
	// net_port 接续在**导出时机**上可能只看到一头。
	if !rep.OK {
		t.Error("单引脚网不该让 ok=false(它是 --strict 档的判据)")
	}
}

func TestParseSchNetlistNets_ExtractsPinsAndParts(t *testing.T) {
	raw := []byte(`{"components":{
	  "gge1":{"props":{"Designator":"U1"},"pinInfoMap":{"1":{"net":"3V3"},"2":{"net":"GND"}}},
	  "gge2":{"props":{"Designator":"C1"},"pinInfoMap":{"1":{"net":"3V3"},"2":{"net":"GND"}}},
	  "gge3":{"props":{"Designator":"R1"},"pinInfoMap":{"1":{"net":""},"2":{"net":"3V3"}}}
	}}`)
	nets, err := parseSchNetlistNets(raw)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]schNetInfo{}
	for _, n := range nets {
		byName[n.Name] = n
	}
	if got := byName["3V3"]; got.Pins != 3 || len(got.Parts) != 3 {
		t.Errorf("3V3 应为 3 脚 / 3 件,得到 %d 脚 %v", got.Pins, got.Parts)
	}
	if _, has := byName[""]; has {
		t.Error("空网名不该成为一张网")
	}
}
