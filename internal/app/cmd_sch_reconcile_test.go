package app

import "testing"

// 对账判的是**连通性**不是网名:块内网的实例名、被 --bind 重绑的边界网名都会变,
// 而「这几个引脚必须在同一张网上」不会变。
func TestReconcileGroupNets(t *testing.T) {
	nets := [][]string{
		{"U.VCC", "C1.1", "PORT:VBUS_5V"}, // PORT 前缀不参与连通性对账
		{"U.GND", "C1.2"},
		{"U.TXD", "R1.1"},
	}
	roles := map[string]string{"U": "U3", "C1": "C8", "R1": "R4"}
	pins := map[string]map[string][]string{
		"U3": {"VCC": {"16"}, "GND": {"1"}, "TXD": {"2"}},
		"C8": {"1": {"1"}, "2": {"2"}},
		"R4": {"1": {"1"}},
	}

	// ① 全对:三条网各自成团。
	live := map[string]map[string]bool{
		"5V":     {"U3.16": true, "C8.1": true},
		"GND":    {"U3.1": true, "C8.2": true},
		"MCU_RX": {"U3.2": true, "R4.1": true},
	}
	if got := reconcileGroupNets(nets, roles, live, pins); len(got) != 0 {
		t.Fatalf("网名与配方不同不该报差异(判的是连通性): %+v", got)
	}

	// ② split:本该同网的两个引脚落在两张网上 —— 真正的电气缺陷。
	split := map[string]map[string]bool{
		"5V":     {"U3.16": true},
		"5V_ALT": {"C8.1": true},
		"GND":    {"U3.1": true, "C8.2": true},
		"MCU_RX": {"U3.2": true, "R4.1": true},
	}
	got := reconcileGroupNets(nets, roles, split, pins)
	if len(got) != 1 || got[0].Kind != "split" || len(got[0].LiveIn) != 2 {
		t.Fatalf("该报一处 split 且列出两张活体网: %+v", got)
	}

	// ③ missing:成员根本没连上。
	missing := map[string]map[string]bool{
		"5V":     {"U3.16": true, "C8.1": true},
		"GND":    {"U3.1": true},
		"MCU_RX": {"U3.2": true, "R4.1": true},
	}
	got = reconcileGroupNets(nets, roles, missing, pins)
	if len(got) != 1 || got[0].Kind != "missing" || len(got[0].Missing) != 1 {
		t.Fatalf("该报一处 missing: %+v", got)
	}

	// ④ role 不在映射里 → unresolved,不许静默跳过(块数据与实际器件对不上)。
	got = reconcileGroupNets(nets, map[string]string{"U": "U3", "C1": "C8"}, live, pins)
	hasUnresolved := false
	for _, d := range got {
		if d.Kind == "unresolved" {
			hasUnresolved = true
		}
	}
	if !hasUnresolved {
		t.Fatalf("解析不出的引脚必须报 unresolved,不能当成对上了: %+v", got)
	}
}
