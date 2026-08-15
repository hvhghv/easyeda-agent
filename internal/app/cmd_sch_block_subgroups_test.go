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

// 拆分必须**只来自块数据**:flow 每级一群、attach 跟宿主、pair 跟它连的那级。
// 真块验证 —— ch340c 应拆成「USB 口 / ESD / 桥芯片」三群,和人手认领的一致。
func TestBslFunctionalGroups_ChinaCH340C(t *testing.T) {
	blk, rel := subgroupBlock(t)
	got := bslFunctionalGroups(blk, rel, bslBlockNets(blk), "U")

	want := map[string][]string{
		"J_USB": {"J_USB", "R_CC1", "R_CC2"}, // CC 下拉挂在 USB 口上
		"D_ESD": {"D_ESD"},
		"U":     {"C_V3", "C_VCC", "U"}, // 去耦跟宿主
	}
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
	// 每个 role 恰好属于一个子群,不留孤儿、不重复。
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
