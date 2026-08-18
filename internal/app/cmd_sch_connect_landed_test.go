package app

import "testing"

// readResult builds the subset of a schematic.read result the recheck consumes.
func readResult(nets map[string][]string) map[string]any {
	var list []any
	for name, members := range nets {
		var pins []any
		for _, m := range members {
			pins = append(pins, m)
		}
		list = append(list, map[string]any{"net": name, "pins": pins})
	}
	return map[string]any{"nets": list}
}

// TestConnectLanded: the slow-landed recheck after a connect_pin
// timeout/DISPATCH_FAILED — the pin already on the target net means the write
// applied and the failure was fake (retrying would mint a duplicate flag).
func TestConnectLanded(t *testing.T) {
	res := readResult(map[string][]string{
		"VCC":  {"U1.5", "C1.1"},
		"+3V3": {"U2.8"},
	})

	if !connectLanded(res, "U1", "5", "VCC") {
		t.Error("U1:5 on VCC must count as landed")
	}
	// Member match is case-insensitive (netlist normalizes designators).
	if !connectLanded(res, "u1", "5", "VCC") {
		t.Error("designator match must be case-insensitive")
	}
	// Wrong pin / wrong net → not landed.
	if connectLanded(res, "U1", "6", "VCC") {
		t.Error("U1:6 is not on VCC")
	}
	if connectLanded(res, "U1", "5", "GND") {
		t.Error("GND does not exist in the snapshot")
	}
	// NET names match exactly: +3V3 vs 3V3 are DIFFERENT nets (the cross-page
	// blind spot) — a near-miss name must NOT be forgiven as landed.
	if connectLanded(res, "U2", "8", "3V3") {
		t.Error("net name must match exactly: 3V3 must not match +3V3")
	}
	if !connectLanded(res, "U2", "8", "+3V3") {
		t.Error("U2:8 on +3V3 must count as landed")
	}
	// Degenerate inputs never panic and never claim success.
	if connectLanded(nil, "U1", "5", "VCC") {
		t.Error("nil result must not land")
	}
	if connectLanded(map[string]any{}, "U1", "5", "VCC") {
		t.Error("missing nets key must not land")
	}
	if connectLanded(map[string]any{"nets": "garbage"}, "U1", "5", "VCC") {
		t.Error("malformed nets must not land")
	}
}

func TestSplitPinRef(t *testing.T) {
	if d, p, ok := splitPinRef("U1:5"); !ok || d != "U1" || p != "5" {
		t.Errorf("U1:5 → (%q,%q,%v)", d, p, ok)
	}
	// Pin names containing ':' keep everything after the FIRST colon.
	if d, p, ok := splitPinRef("U1:A:1"); !ok || d != "U1" || p != "A:1" {
		t.Errorf("U1:A:1 → (%q,%q,%v)", d, p, ok)
	}
	for _, bad := range []string{"", "U1", ":5", "U1:"} {
		if _, _, ok := splitPinRef(bad); ok {
			t.Errorf("splitPinRef(%q) must fail", bad)
		}
	}
}
