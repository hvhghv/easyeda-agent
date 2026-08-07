package spec

import (
	"strings"
	"testing"
)

// realWorldSpec 是磁盘上那份真实存在的 S0 spec（.easyeda/s0-n8r8-ceshi.json）
// 的逐字副本。它同时展示了三处已发生的契约漂移：`board` 是字符串而非文档说的
// 对象、`stackup` 用 inner1/inner2 而非 groundStrategy/innerLayers、`assembly`
// 在文档里根本不存在。类型化之后这三处都必须继续能读，且不得报 ERROR ——
// 收紧契约不能把用户手上现存的 spec 一次性打死。
const realWorldSpec = `{
  "board": "compact",
  "sheet": "A4",
  "stackup": {"layers": 4, "inner1": "GND-PLANE", "inner2": "3V3"},
  "assembly": {"profile": "reflow", "side": "top"},
  "rf": {"parts": ["ANT1"], "keepoutLayers": "all", "feed": "50ohm"},
  "modules": [
    {"name": "MCU", "zone": "center", "block": "block.esp32s3r8_chip_minsys", "parts": ["U2", "X1", "C9"]},
    {"name": "FLASH", "zone": "center-bottom", "parts": ["U1", "C4", "R1"]},
    {"name": "RF", "zone": "top", "block": "block.ant_2g4_ceramic_pi", "parts": ["ANT1", "C11"]},
    {"name": "IO", "zone": "bottom", "parts": ["J1"]}
  ],
  "notes": "实际位号:U2=S3R8"
}`

func TestParse_RealWorldSpecStillLoads(t *testing.T) {
	s, err := Parse([]byte(realWorldSpec))
	if err != nil {
		t.Fatalf("the one real spec on disk must keep parsing: %v", err)
	}
	// board 写成字符串 —— 自定义 Unmarshal 必须抹平，而不是报错。
	if s.Board == nil || s.Board.Outline != "compact" {
		t.Errorf(`board:"compact" (string form) should map to Board.Outline, got %+v`, s.Board)
	}
	// stackup 的两种写法归一
	if got := s.Stackup.Inners(); len(got) != 2 || got[0] != "GND-PLANE" || got[1] != "3V3" {
		t.Errorf("Inners() = %v, want [GND-PLANE 3V3] from inner1/inner2", got)
	}
	if s.Assembly == nil || s.Assembly.Profile != "reflow" {
		t.Errorf("undocumented-but-real assembly field must be captured, got %+v", s.Assembly)
	}
	if len(s.Modules) != 4 {
		t.Fatalf("modules = %d, want 4", len(s.Modules))
	}

	issues := Validate(s)
	if HasErrors(issues) {
		for _, i := range issues {
			if i.Level == "ERROR" {
				t.Errorf("existing spec must not produce ERROR, got %s: %s", i.Field, i.Message)
			}
		}
	}
}

func TestParse_BoardObjectForm(t *testing.T) {
	s, err := Parse([]byte(`{"board": {"outline": "compact", "widthMm": 40, "heightMm": 25}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Board.Outline != "compact" || s.Board.WidthMM != 40 || s.Board.HeightMM != 25 {
		t.Errorf("object form lost data: %+v", s.Board)
	}
}

func TestModule_KindFallsBackToName(t *testing.T) {
	// 老 spec 没有 kind，但 name 常常恰好就是功能域词（MCU/RF/IO）—— 能救回来的
	// 就救，救不回来的（FLASH）留空并在校验里提示，而不是硬塞一个 OTHER 装作有值。
	cases := map[string]string{"MCU": "MCU", "rf": "RF", "IO": "IO", "FLASH": "", "电源": ""}
	for name, want := range cases {
		if got := (Module{Name: name}).KindOf(); got != want {
			t.Errorf("Module{Name:%q}.KindOf() = %q, want %q", name, got, want)
		}
	}
	// 显式 kind 永远优先于 name 猜测
	if got := (Module{Name: "MCU", Kind: "storage"}).KindOf(); got != "STORAGE" {
		t.Errorf("explicit kind must win, got %q", got)
	}
}

func TestValidate_FlowOrder(t *testing.T) {
	s, _ := Parse([]byte(`{
	  "flow": ["POWER", "MCU", "RF", "ANT"],
	  "flowAxis": "x",
	  "modules": [
	    {"name": "PWR", "kind": "POWER", "parts": ["U1"]},
	    {"name": "BRAIN", "kind": "MCU", "parts": ["U2"]},
	    {"name": "RADIO", "kind": "RF", "parts": ["U3"]},
	    {"name": "AERIAL", "kind": "ANT", "parts": ["ANT1"]}
	  ]
	}`))
	if issues := Validate(s); HasErrors(issues) {
		t.Errorf("a well-formed flow must validate clean: %+v", issues)
	}
	if s.Axis() != "x" {
		t.Errorf("Axis() = %q, want x", s.Axis())
	}
	byKind := s.ModuleByKind()
	if len(byKind) != 4 {
		t.Errorf("ModuleByKind = %v, want 4 kinds", byKind)
	}
}

func TestValidate_FlowRejectsBadStages(t *testing.T) {
	s, _ := Parse([]byte(`{"flow": ["POWER", "WIDGET", "POWER"], "modules": [{"name":"P","kind":"POWER","parts":["U1"]}]}`))
	issues := Validate(s)
	if !HasErrors(issues) {
		t.Fatal("unknown + duplicate flow stages must be ERRORs")
	}
	var unknown, dup bool
	for _, i := range issues {
		if strings.Contains(i.Message, "unknown flow stage") {
			unknown = true
		}
		if strings.Contains(i.Message, "duplicate flow stage") {
			dup = true
		}
	}
	if !unknown || !dup {
		t.Errorf("want both unknown and duplicate reported, got %+v", issues)
	}
}

func TestValidate_FlowStageWithoutModuleIsWarnNotError(t *testing.T) {
	// 声明了 RF 阶段但板上没有 RF 模块：这是设计上的遗漏，值得提醒，但不该
	// 阻塞命令 —— spec 常常先于布局写好。
	s, _ := Parse([]byte(`{"flow": ["POWER","RF"], "modules": [{"name":"P","kind":"POWER","parts":["U1"]}]}`))
	issues := Validate(s)
	if HasErrors(issues) {
		t.Errorf("a flow stage with no module should be WARN, not ERROR: %+v", issues)
	}
	found := false
	for _, i := range issues {
		if i.Level == "WARN" && strings.Contains(i.Message, "no module of that kind") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a WARN about the empty flow stage, got %+v", issues)
	}
}

func TestValidate_InterfaceFacing(t *testing.T) {
	s, _ := Parse([]byte(`{
	  "interfaces": [
	    {"name": "USB", "ref": "J2", "edge": "bottom", "facing": "user-facing"},
	    {"name": "BATT", "ref": "J1", "facing": "internal", "plugWidthMm": 8.5},
	    {"name": "ANT", "ref": "ANT1", "edge": "right", "facing": "any"}
	  ]
	}`))
	if issues := Validate(s); HasErrors(issues) {
		t.Errorf("valid interfaces must pass: %+v", issues)
	}
	idx := s.InterfaceByRef()
	if idx["J1"].FacingOf() != "internal" {
		t.Errorf("J1 facing = %q, want internal", idx["J1"].FacingOf())
	}
	if idx["J2"].Edge != "bottom" {
		t.Errorf("J2 edge lost: %+v", idx["J2"])
	}
}

func TestValidate_InterfaceContradictionIsError(t *testing.T) {
	// internal:true 与 facing:"user-facing" 直接矛盾 —— 这不是缺字段，是写错了，
	// 必须 ERROR（否则 internal-on-edge 规则会按哪个执行全看代码顺序）。
	s, _ := Parse([]byte(`{"interfaces":[{"name":"X","ref":"J9","internal":true,"facing":"user-facing"}]}`))
	if !HasErrors(Validate(s)) {
		t.Error("internal:true + facing:user-facing must be an ERROR")
	}
}

func TestValidate_InternalShorthandImpliesFacing(t *testing.T) {
	s, _ := Parse([]byte(`{"interfaces":[{"name":"BATT","ref":"J1","internal":true}]}`))
	if issues := Validate(s); HasErrors(issues) {
		t.Errorf("internal shorthand alone is valid: %+v", issues)
	}
	if got := s.InterfaceByRef()["J1"].FacingOf(); got != "internal" {
		t.Errorf("internal:true should imply facing=internal, got %q", got)
	}
}

func TestValidate_RefLessInterfaceOnlyInfo(t *testing.T) {
	// 老 spec 的 interfaces[] 只有 name，钉不到具体器件 → 规则退回启发式。
	// 这是能力降级，不是用户的错，只报 INFO。
	s, _ := Parse([]byte(`{"interfaces":[{"name":"USB-C","orientation":"dual"}]}`))
	issues := Validate(s)
	if HasErrors(issues) {
		t.Errorf("a ref-less interface must not be an ERROR: %+v", issues)
	}
	found := false
	for _, i := range issues {
		if i.Level == "INFO" && strings.Contains(i.Message, "fall back to heuristics") {
			found = true
		}
	}
	if !found {
		t.Errorf("want an INFO explaining the heuristic fallback, got %+v", issues)
	}
}

func TestValidate_BadZoneAndKindAreErrors(t *testing.T) {
	s, _ := Parse([]byte(`{"modules":[{"name":"M","zone":"middle","kind":"BRAIN","parts":["U1"]}]}`))
	issues := Validate(s)
	if !HasErrors(issues) {
		t.Fatalf("bad zone + bad kind must be ERRORs, got %+v", issues)
	}
	var zoneErr, kindErr bool
	for _, i := range issues {
		if strings.HasSuffix(i.Field, ".zone") && i.Level == "ERROR" {
			zoneErr = true
		}
		if strings.HasSuffix(i.Field, ".kind") && i.Level == "ERROR" {
			kindErr = true
		}
	}
	if !zoneErr || !kindErr {
		t.Errorf("want both zone and kind errors, got %+v", issues)
	}
}

func TestValidate_DuplicateModuleName(t *testing.T) {
	s, _ := Parse([]byte(`{"modules":[{"name":"IO","parts":["J1"]},{"name":"io","parts":["J2"]}]}`))
	if !HasErrors(Validate(s)) {
		t.Error("duplicate module names (case-insensitive) must be an ERROR")
	}
}

func TestValidate_PartModuleIndex(t *testing.T) {
	s, _ := Parse([]byte(`{"modules":[{"name":"MCU","kind":"MCU","parts":["U2","c9"]}]}`))
	idx := s.PartModule()
	if idx["U2"].Name != "MCU" || idx["C9"].Name != "MCU" {
		t.Errorf("PartModule index should be case-insensitive: %+v", idx)
	}
}

func TestValidate_EmptySpec(t *testing.T) {
	if !HasErrors(Validate(nil)) {
		t.Error("a nil spec must be an ERROR")
	}
	// 空对象是合法 JSON 但没有意图 —— 只警告，不阻塞。
	s, _ := Parse([]byte(`{}`))
	if HasErrors(Validate(s)) {
		t.Error("an empty-but-valid spec should warn, not error")
	}
}

func TestValidate_IssuesAreDeterministicallyOrdered(t *testing.T) {
	s, _ := Parse([]byte(`{
	  "flowAxis": "diagonal",
	  "modules": [{"name":"A","zone":"nope","parts":["U1"]}],
	  "interfaces": [{"name":"I","edge":"sideways"}]
	}`))
	first := Validate(s)
	for range 5 {
		if got := Validate(s); len(got) != len(first) {
			t.Fatalf("issue count unstable: %d vs %d", len(got), len(first))
		} else {
			for j := range got {
				if got[j] != first[j] {
					t.Fatalf("issue order unstable at %d: %+v vs %+v", j, got[j], first[j])
				}
			}
		}
	}
	// ERROR 必须排在 WARN/INFO 前面
	if first[0].Level != "ERROR" {
		t.Errorf("errors must sort first, got %+v", first[0])
	}
}
