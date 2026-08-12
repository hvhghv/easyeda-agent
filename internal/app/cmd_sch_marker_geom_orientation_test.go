package app

import (
	"encoding/json"
	"os"
	"testing"
)

// TestFlagBodyRotationMatchesOrientationJSON 钉住 Go 侧 flagBodyRotation 与
// SSOT(skills/easyeda-agent/references/orientation.json frozenTable)的一致:
// 真值表分叉 = 生成侧与校验侧再度双盲(2026-08-12 修过一次,倒挂旗全绿两个月)。
func TestFlagBodyRotationMatchesOrientationJSON(t *testing.T) {
	raw, err := os.ReadFile("../../skills/easyeda-agent/references/orientation.json")
	if err != nil {
		t.Fatalf("read orientation.json: %v", err)
	}
	var spec struct {
		FrozenTable map[string]map[string]float64 `json:"frozenTable"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse orientation.json: %v", err)
	}
	for fam, table := range flagBodyRotation {
		want, ok := spec.FrozenTable[fam]
		if !ok {
			t.Fatalf("orientation.json frozenTable has no family %q", fam)
		}
		for dir, got := range table {
			if want[dir] != got {
				t.Errorf("flagBodyRotation[%s][%s]=%g but orientation.json says %g — the tables drifted", fam, dir, got, want[dir])
			}
		}
	}
}
