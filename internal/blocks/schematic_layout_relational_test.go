package blocks

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── 关系形态模板校验 V1–V7(issue #180 P1)──────────────────────────────────
//
// 用**内存构造**的 Block 测,不落新块文件:TestAllBlocksPassCoreValidation 扫的是
// 内嵌全库,加测试块会污染 `easyeda blocks ls`。

// relBlock 造一个最小可用的块:U(IC)+ C_VCC/C_V3(去耦)+ R_A/R_B(并列对),
// 内部网让 C_VCC 与 U.VCC 同网、C_V3 与 U.V3 同网(attach 的电气依据)。
func relBlock(t *testing.T, layout map[string]any) Block {
	t.Helper()
	raw := map[string]any{
		"id":   "block.test_rel",
		"desc": "test",
		"parts": map[string]any{
			"U":     map[string]any{"part": "ic.ch340c", "qty": 1},
			"C_VCC": map[string]any{"part": "cap.100nf_0402", "qty": 1},
			"C_V3":  map[string]any{"part": "cap.100nf_0402", "qty": 1},
			"R_A":   map[string]any{"part": "res.5k1_0402", "qty": 1},
			"R_B":   map[string]any{"part": "res.5k1_0402", "qty": 1},
			"J":     map[string]any{"part": "conn.usb_c_16p", "qty": 1},
		},
		"internal_nets": []any{
			[]any{"U.VCC", "C_VCC.1"},
			[]any{"U.V3", "C_V3.1"},
			[]any{"J.CC1", "R_A.1"},
			[]any{"J.CC2", "R_B.1"},
			[]any{"J.VBUS*", "U.GND"},
		},
	}
	if layout != nil {
		raw["schematic_layout"] = layout
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var blk Block
	if err := json.Unmarshal(b, &blk); err != nil {
		t.Fatal(err)
	}
	blk.Raw = b
	return blk
}

// collect 跑关系校验,返回 field→message 的扁平列表。
func collect(t *testing.T, layout map[string]any) []string {
	t.Helper()
	var out []string
	validateSchematicLayout(relBlock(t, layout), func(field, msg string) {
		out = append(out, field+": "+msg)
	})
	return out
}

func hasIssue(issues []string, substr string) bool {
	for _, i := range issues {
		if strings.Contains(i, substr) {
			return true
		}
	}
	return false
}

// 正例:合法的关系模板必须零告警。
func TestRelationalLayout_ValidPasses(t *testing.T) {
	got := collect(t, map[string]any{
		"anchor": "U",
		"flow":   []any{"J", "U"},
		"attach": map[string]any{"C_VCC": "U.VCC", "C_V3": "U.V3"},
		"pair":   []any{[]any{"R_A", "R_B"}},
		"orient": map[string]any{"C_VCC": "vertical"},
	})
	if len(got) != 0 {
		t.Errorf("合法关系模板不该报错: %v", got)
	}
}

// 两种形态互斥。
func TestRelationalLayout_LegacyAndRelationalAreExclusive(t *testing.T) {
	got := collect(t, map[string]any{
		"roles": map[string]any{"U": map[string]any{"dx": 0, "dy": 0}},
		"flow":  []any{"J", "U"},
	})
	if !hasIssue(got, "不可混用") {
		t.Errorf("混用两种形态必须报错: %v", got)
	}
}

// V1: 未知 role。
func TestRelationalLayout_V1UnknownRole(t *testing.T) {
	for _, layout := range []map[string]any{
		{"flow": []any{"NOPE", "U"}},
		{"attach": map[string]any{"NOPE": "U.VCC"}},
		{"pair": []any{[]any{"NOPE", "R_B"}}},
		// orient 是**修饰符**不是关系,单独出现时模板没有几何(另有用例);
		// 这里配一条 flow 让它进入关系分支。
		{"flow": []any{"J", "U"}, "orient": map[string]any{"NOPE": "vertical"}},
		{"anchor": "NOPE", "flow": []any{"J", "U"}},
	} {
		if got := collect(t, layout); !hasIssue(got, "unknown role") {
			t.Errorf("未知 role 必须报错 (layout=%v): %v", layout, got)
		}
	}
}

// V2: 一个 role 只能被一种关系定位。
func TestRelationalLayout_V2OneRelationPerRole(t *testing.T) {
	got := collect(t, map[string]any{
		"flow":   []any{"J", "C_VCC"},
		"attach": map[string]any{"C_VCC": "U.VCC"},
	})
	if !hasIssue(got, "只能由一种关系定位") {
		t.Errorf("同一 role 出现在两种关系里必须报错: %v", got)
	}
}

// V3: attach 值的形式 —— 不许 `*`、不许自贴、必须 ROLE.PIN。
func TestRelationalLayout_V3AttachRefShape(t *testing.T) {
	cases := []struct {
		target string
		want   string
	}{
		{"U.VBUS*", "attach 要的是**一个点**"},
		{"UVCC", "不是 ROLE.PIN 形式"},
	}
	for _, c := range cases {
		got := collect(t, map[string]any{"attach": map[string]any{"C_VCC": c.target}})
		if !hasIssue(got, c.want) {
			t.Errorf("attach=%q 应报 %q: %v", c.target, c.want, got)
		}
	}
	// 自贴
	if got := collect(t, map[string]any{"attach": map[string]any{"U": "U.VCC"}}); !hasIssue(got, "不能贴到自己") {
		t.Errorf("自贴必须报错: %v", got)
	}
}

// V4(本轮最有价值的一条):attach 必须有电气依据 —— 抓拼写错和「贴到无关脚上」。
func TestRelationalLayout_V4AttachNeedsElectricalBasis(t *testing.T) {
	// C_VCC 与 U.GND 之间没有共同的网 → 必须报。
	got := collect(t, map[string]any{"attach": map[string]any{"C_VCC": "U.GND"}})
	if !hasIssue(got, "没有电气依据") {
		t.Errorf("贴到无关脚必须报错: %v", got)
	}
	// 引脚名拼错(U.VCCC 根本不在任何网里)→ 同样被抓。
	got = collect(t, map[string]any{"attach": map[string]any{"C_VCC": "U.VCCC"}})
	if !hasIssue(got, "没有电气依据") {
		t.Errorf("拼错的引脚名必须被抓: %v", got)
	}
	// fanout 目标侧:J.VBUS* 与 U.GND 同网,贴 U 到 J.VBUS 应当成立(`*` 两侧同边界)。
	got = collect(t, map[string]any{"attach": map[string]any{"U": "J.VBUS"}})
	if hasIssue(got, "没有电气依据") {
		t.Errorf("fanout 边界应被视作同一个脚: %v", got)
	}
}

// V5: pair 组内必须同 part。
func TestRelationalLayout_V5PairSamePart(t *testing.T) {
	got := collect(t, map[string]any{"pair": []any{[]any{"R_A", "C_VCC"}}})
	if !hasIssue(got, "等距并列要求同型号") {
		t.Errorf("异型号并列必须报错: %v", got)
	}
	if got := collect(t, map[string]any{"pair": []any{[]any{"R_A"}}}); !hasIssue(got, "至少要两个成员") {
		t.Errorf("单成员并列组必须报错: %v", got)
	}
}

// V6: flow **不要求**覆盖全部 role —— 与 legacy 的全覆盖判据相反。
func TestRelationalLayout_V6FlowNeedNotCoverEveryRole(t *testing.T) {
	got := collect(t, map[string]any{"flow": []any{"J", "U"}})
	for _, g := range got {
		if strings.Contains(g, "not covered") {
			t.Errorf("关系形态不该要求全覆盖(那是 legacy 的判据): %v", got)
		}
	}
}

// legacy 判据必须**一字不改**地保留:全覆盖、落格、合法 rotation。
func TestLegacyLayoutRulesUnchanged(t *testing.T) {
	// 半张模板 → 全覆盖判据必须仍然响。
	got := collect(t, map[string]any{"roles": map[string]any{"U": map[string]any{"dx": 0, "dy": 0}}})
	if !hasIssue(got, "not covered") {
		t.Errorf("legacy 的全覆盖判据必须保留: %v", got)
	}
	// 落格判据。
	full := map[string]any{}
	for _, r := range []string{"U", "C_VCC", "C_V3", "R_A", "R_B", "J"} {
		full[r] = map[string]any{"dx": 0, "dy": 0}
	}
	full["U"] = map[string]any{"dx": 3, "dy": 0} // 3 不落 5 格
	if got := collect(t, map[string]any{"roles": full}); !hasIssue(got, "off the 5-unit placement grid") {
		t.Errorf("legacy 的落格判据必须保留: %v", got)
	}
}

// 空模板(只有 note)要明确报错,而不是静默当没有。
func TestRelationalLayout_EmptyTemplateIsAnError(t *testing.T) {
	got := collect(t, map[string]any{"note": "只有说明没有几何"})
	if !hasIssue(got, "must declare either roles") {
		t.Errorf("空模板必须报错: %v", got)
	}
}
