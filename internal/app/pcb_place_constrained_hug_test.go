package app

import "testing"

func hugComp(des, dev string, nets ...string) cpComp {
	c := cpComp{footprint: dev, layer: 1}
	c.designator = des
	for i, n := range nets {
		c.pads = append(c.pads, apPad{num: string(rune('1' + i)), net: n})
	}
	return c
}

// TestCpNeedsHugging 钉住「谁值得跟着端子走」。
//
// 这条判据是真板逼出来的：跟随一开始对**所有**卫星生效，166 器件的板上把移动件数
// 从 19 推到 46，而 protection 一分没涨 —— 被拖着跑的大多是没有贴脚约束的普通
// 电阻电容。收窄到保护件 + 去耦后扰动降回 24，稀疏板上的收益（+41.9）一分未丢。
func TestCpNeedsHugging(t *testing.T) {
	cases := []struct {
		name string
		c    cpComp
		want bool
	}{
		// —— 保护件：必须守在入口处 ——
		{"保险丝", hugComp("F1", "PPTC FUSE", "VBATT", "VIN"), true},
		{"TVS(位号)", hugComp("TVS_VBUS", "SMAJ5.0A", "VIN", "GND"), true},
		{"ESD(位号)", hugComp("ESD1", "USBLC6-2SC6", "USB_DP", "USB_DM"), true},
		{"二极管带下划线", hugComp("D_ESD_ANT", "ESD9B5.0ST5G", "ANT_FEED", "GND"), true},
		{"器件名认出 TVS", hugComp("Z3", "SMBJ5.0A TVS", "VIN", "GND"), true},

		// —— 去耦：恰好 2 脚、一脚地一脚非地电源网（与 findDecapTooFar 同口径）——
		{"去耦 3V3/GND", hugComp("C1", "CAP0402", "3V3", "GND"), true},
		{"去耦 VCC/GND", hugComp("C9", "CAP0402", "GND", "VCC"), true},

		// —— 不该跟的 ——
		{"信号电容(无地)", hugComp("C5", "CAP0402", "RF_OUT", "RF_ANT"), false},
		{"耦合电容(双电源)", hugComp("C6", "CAP0402", "3V3", "5V"), false},
		{"三脚电容", hugComp("C7", "CAP0402", "3V3", "GND", "EN"), false},
		{"普通电阻", hugComp("R1", "RES0402", "3V3", "IO0"), false},
		{"上拉电阻到电源地", hugComp("R2", "RES0402", "3V3", "GND"), false},
		{"主控", hugComp("U2", "ESP32-S3", "3V3", "GND"), false},
		{"晶振", hugComp("X1", "40MHZ XTAL", "XTAL1", "XTAL2"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cpNeedsHugging(tc.c); got != tc.want {
				t.Errorf("cpNeedsHugging(%s/%s nets=%d) = %v, want %v",
					tc.c.designator, tc.c.footprint, len(tc.c.pads), got, tc.want)
			}
		})
	}
}

// TestCpNeedsHugging_ResistorNotDecap 单独钉住一条最容易写错的边界：
// 位号必须是 C 开头才算去耦。一个跨接在 3V3/GND 之间的电阻（分压、假负载）
// 网表形态与去耦电容一模一样，但它没有贴脚约束 —— 拿网表形态一刀切会把它也拖着跑。
func TestCpNeedsHugging_ResistorNotDecap(t *testing.T) {
	r := hugComp("R9", "RES0402", "3V3", "GND")
	c := hugComp("C9", "CAP0402", "3V3", "GND")
	if cpNeedsHugging(r) {
		t.Error("a resistor across 3V3/GND has the same net shape as a decap but no hugging constraint")
	}
	if !cpNeedsHugging(c) {
		t.Error("the cap with the identical net shape SHOULD hug")
	}
}
