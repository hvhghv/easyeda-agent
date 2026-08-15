package app

import "testing"

// 写图签是「读全量→改几项→整包回传」。结构开关(图框/明细表)一旦在回传里被平台
// 按默认处理,图框会被整个关掉 —— 页面看着还在,sheet 图元却没了,越界判据集体
// 失明(2026-08-15 esp32Mini E2E:四页图纸静默丢失,直到 gate --strict 才暴露)。
func TestTbKeepStructural_PinsFrameSwitchesWithRealBooleans(t *testing.T) {
	full := map[string]any{
		"Title Block": map[string]any{"value": "1", "showTitle": nil, "showValue": nil},
		"Border":      map[string]any{"value": "1", "showTitle": nil, "showValue": nil},
		"Name":        map[string]any{"value": "old", "showTitle": nil, "showValue": nil},
	}
	out := map[string]any{}
	for k, v := range full {
		out[k] = tbPreserve(v)
	}
	tbKeepStructural(full, out)

	for _, k := range []string{"Title Block", "Border"} {
		m, ok := out[k].(map[string]any)
		if !ok {
			t.Fatalf("%s 应保留为对象: %#v", k, out[k])
		}
		if m["value"] != 1.0 {
			t.Errorf("%s 的值应还原成数字 1(字符串会被平台解析成 0): %#v", k, m["value"])
		}
		if b, isBool := m["showValue"].(bool); !isBool || !b {
			t.Errorf("%s 的 showValue 必须是真布尔 true(null 会让平台按默认关掉): %#v", k, m["showValue"])
		}
		if _, isBool := m["showTitle"].(bool); !isBool {
			t.Errorf("%s 的 showTitle 必须是真布尔: %#v", k, m["showTitle"])
		}
	}
}
