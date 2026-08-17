package blocks

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var validCategories = map[string]bool{
	"audio": true, "button": true, "comms": true, "display": true,
	"indicator": true, "mcu": true, "mcu-support": true, "power": true,
	"protection": true, "rf": true, "sensing": true, "storage": true,
	"usb": true, "usb-serial": true,
}

var validPortDirections = map[string]bool{"in": true, "out": true, "bidir": true}
var validVerificationStatuses = map[string]bool{
	"passed": true, "failed": true, "pending": true, "not_tested": true,
}

// ValidationError identifies a bad field without hiding which block caused it.
type ValidationError struct {
	BlockID string
	Field   string
	Problem string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.BlockID, e.Field, e.Problem)
}

// Validate checks the executable core contract. Unknown top-level extension maps
// remain forward-compatible, but known fields and all topology references are strict.
func Validate(b Block) []error {
	var errs []error
	add := func(field, problem string) {
		errs = append(errs, ValidationError{BlockID: b.ID, Field: field, Problem: problem})
	}

	if strings.TrimSpace(b.ID) == "" {
		add("id", "required")
	} else if !strings.HasPrefix(b.ID, "block.") {
		add("id", "must start with block.")
	}
	if strings.TrimSpace(b.Desc) == "" {
		add("desc", "required")
	}
	if !validCategories[b.Category] {
		add("category", fmt.Sprintf("unknown value %q", b.Category))
	}
	if len(b.Parts) == 0 {
		add("parts", "must contain at least one role")
	}
	for role, p := range b.Parts {
		if strings.TrimSpace(role) == "" || strings.HasPrefix(role, "_") {
			add("parts."+role, "invalid role")
		}
		if strings.TrimSpace(p.Part) == "" {
			add("parts."+role+".part", "required")
		}
		if p.Qty < 1 {
			add("parts."+role+".qty", "must be >= 1")
		}
	}

	validateBomNoteCount(b, add)

	var topology struct {
		InternalNets [][]string `json:"internal_nets"`
	}
	if err := json.Unmarshal(b.Raw, &topology); err != nil {
		add("internal_nets", err.Error())
		return errs
	}
	pinNet := map[string]int{}
	usedPorts := map[string]bool{}
	for i, net := range topology.InternalNets {
		field := fmt.Sprintf("internal_nets[%d]", i)
		if len(net) < 2 {
			add(field, "must contain at least two members")
		}
		seen := map[string]bool{}
		for _, member := range net {
			if seen[member] {
				add(field, fmt.Sprintf("duplicate member %q", member))
				continue
			}
			seen[member] = true
			if strings.HasPrefix(member, "PORT:") {
				name := strings.TrimPrefix(member, "PORT:")
				if _, ok := b.Ports[name]; !ok {
					add(field, fmt.Sprintf("references unknown port %q", name))
				}
				usedPorts[name] = true
				continue
			}
			role, _, ok := splitPinRef(member)
			if !ok {
				add(field, fmt.Sprintf("invalid pin ref %q", member))
				continue
			}
			if _, ok := b.Parts[role]; !ok {
				add(field, fmt.Sprintf("references unknown role %q", role))
			}
			// Index by the bare pin ref: a trailing "*" (bond EVERY pin sharing this
			// function name — a connector's redundant VBUS/GND/shield) names the same
			// boundary as the plain ref, so "J.VBUS*" and "J.VBUS" must not read as two
			// different pins here.
			key := strings.TrimSuffix(member, pinFanoutSuffix)
			if previous, ok := pinNet[key]; ok && previous != i {
				add(field, fmt.Sprintf("pin %q already belongs to internal_nets[%d]", member, previous))
			} else {
				pinNet[key] = i
			}
		}
	}

	for name, p := range b.Ports {
		field := "ports." + name
		if strings.TrimSpace(name) == "" || strings.HasPrefix(name, "_") {
			add(field, "invalid port name")
		}
		if !validPortDirections[p.Dir] {
			add(field+".dir", fmt.Sprintf("unknown value %q", p.Dir))
		}
		role, _, ok := splitPinRef(p.At)
		if !ok {
			add(field+".at", fmt.Sprintf("invalid pin ref %q", p.At))
		} else if _, exists := b.Parts[role]; !exists {
			add(field+".at", fmt.Sprintf("references unknown role %q", role))
		}
		// A direct one-pin boundary need not appear in internal_nets. When a PORT:
		// marker is present, however, its declared anchor must be on that same net.
		if usedPorts[name] {
			if _, exists := pinNet[strings.TrimSuffix(p.At, pinFanoutSuffix)]; !exists {
				add(field+".at", "PORT marker exists but anchor is absent from internal_nets")
			}
		}
	}

	if b.Verification != nil {
		validateVerification(b, add)
	}
	validateSchematicLayout(b, add)
	return errs
}

// validSchLayoutRotations mirrors what schematic.component.place accepts.
var validSchLayoutRotations = map[float64]bool{0: true, 90: true, 180: true, 270: true}

// schLayoutGrid is the schematic placement grid (see app.schAnchorGrid): an
// off-grid template offset would put every pin of that role off-grid and break
// connect_pin stubs, so it is a data error, not a runtime surprise.
const schLayoutGrid = 5

// validateSchematicLayout dispatches by TEMPLATE FORM. The two forms' rules are
// **mutually exclusive semantics**, so they must not share one loop:
//
//   - legacy(roles): 必须覆盖**每一个** part —— 半张模板会让模板几何与 fallback
//     网格几何静默混用,那是作者失误;
//   - relational(flow/attach/pair): flow **不要求**覆盖全部 role —— 没进任何关系
//     的件走 residual 布局是合法的。
//
// 同时声明两种形态是数据错误(见 SchematicLayout 的注释)。
func validateSchematicLayout(b Block, add func(string, string)) {
	layout, err := b.SchematicLayout()
	if err != nil {
		add("schematic_layout", err.Error())
		return
	}
	if layout == nil {
		return
	}
	switch {
	case layout.IsLegacy() && layout.IsRelational():
		add("schematic_layout", "roles(绝对偏移)与 flow/attach/pair(关系)不可混用 —— "+
			"两种形态会给同一个件两个种子点;legacy 已废弃,新块只写关系")
	case layout.IsRelational():
		validateSchLayoutRelational(b, layout, add)
	case layout.IsLegacy():
		validateSchLayoutLegacy(b, layout, add)
	default:
		add("schematic_layout", "must declare either roles(legacy 绝对偏移)or flow/attach/pair(关系)")
	}
}

// validateSchLayoutLegacy 是原样搬过来的 legacy 判据 —— 一字不改,现有 4 个带
// 模板的块的行为必须逐字节不变。
func validateSchLayoutLegacy(b Block, layout *SchematicLayout, add func(string, string)) {
	onGrid := func(v float64) bool {
		m := v / schLayoutGrid
		return m == float64(int64(m))
	}
	for role, h := range layout.Roles {
		field := "schematic_layout.roles." + role
		if _, ok := b.Parts[role]; !ok {
			add(field, "references unknown role")
		}
		if !onGrid(h.DX) || !onGrid(h.DY) {
			add(field, fmt.Sprintf("offset (%g,%g) is off the %d-unit placement grid", h.DX, h.DY, schLayoutGrid))
		}
		if !validSchLayoutRotations[h.Rotation] {
			add(field+".rotation", fmt.Sprintf("must be 0/90/180/270, got %g", h.Rotation))
		}
	}
	for role := range b.Parts {
		if _, ok := layout.Roles[role]; !ok {
			add("schematic_layout.roles", fmt.Sprintf("role %q not covered — the template must place every part or be omitted", role))
		}
	}
}

// bom_note is prose, but a "共 N 件" claim inside it is a checkable fact: agents
// and humans use it to audit BOM completeness before ordering, so a stale count
// causes real mis-orders (#128). N must equal the sum of parts[].qty.
var reBomNoteCount = regexp.MustCompile(`共\s*(\d+)\s*件`)

func validateBomNoteCount(b Block, add func(string, string)) {
	var extra struct {
		BomNote string `json:"bom_note"`
	}
	if err := json.Unmarshal(b.Raw, &extra); err != nil || extra.BomNote == "" {
		return
	}
	m := reBomNoteCount.FindStringSubmatch(extra.BomNote)
	if m == nil {
		return
	}
	claimed, _ := strconv.Atoi(m[1])
	total := 0
	for _, p := range b.Parts {
		total += p.Qty
	}
	if claimed != total {
		add("bom_note", fmt.Sprintf("claims 共 %d 件 but parts qty sums to %d", claimed, total))
	}
}

// pinFanoutSuffix marks a pin ref that bonds EVERY pin sharing that function name
// on the part — "J.VBUS*" for a USB-C's two VBUS pins, its two GNDs, its four EP
// tabs. It exists because referring to such a pin by name alone is genuinely
// ambiguous (and `sch autoconnect` rightly refuses to pick one), while the intent
// for power/ground/shield is invariably "all of them"; USB-C's dual orientation in
// fact REQUIRES both the A- and B-side pins be connected. Blocks declare it
// explicitly rather than letting the planner infer it from the net's kind.
const pinFanoutSuffix = "*"

func splitPinRef(ref string) (role, pin string, ok bool) {
	i := strings.IndexByte(ref, '.')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

func validateVerification(b Block, add func(string, string)) {
	v := b.Verification
	stages := []struct {
		name  string
		stage VerificationStage
	}{
		{"schematic", v.Schematic},
		{"component_selection", v.ComponentSelection},
		{"pcb_drc", v.PCBDRC},
		{"bringup", v.Bringup},
	}
	allPassed := true
	for _, item := range stages {
		field := "verification." + item.name
		if !validVerificationStatuses[item.stage.Status] {
			add(field+".status", fmt.Sprintf("unknown value %q", item.stage.Status))
			allPassed = false
			continue
		}
		if item.stage.Status != "passed" {
			allPassed = false
		}
		if item.stage.Status == "passed" && strings.TrimSpace(item.stage.Evidence) == "" {
			add(field+".evidence", "required when status is passed")
		}
		if item.stage.Status == "failed" && len(item.stage.Issues) == 0 {
			add(field+".issues", "required when status is failed")
		}
	}
	if v.ProductionReady && !allPassed {
		add("verification.production_ready", "cannot be true unless every verification stage passed")
	}
}

// ValidateAll returns every data error instead of failing at the first block.
func ValidateAll(blocks []Block) []error {
	var errs []error
	for _, b := range blocks {
		errs = append(errs, Validate(b)...)
	}
	return errs
}

// ── 关系形态模板的校验(issue #180 P1)──────────────────────────────────────
//
// 判据 V1–V7 全部**离线可判**,不需要连接器、不需要放置。这层是"数据一落库
// agent 就当真"的唯一防线:块库先收关系数据、求解器慢一步落地,期间
// bapUnconsumed 会诚实披露"已声明但本版未执行"。
func validateSchLayoutRelational(b Block, layout *SchematicLayout, add func(string, string)) {
	nets := blockInternalNets(b)

	// role 引用必须存在(V1)——与 legacy 的 role 校验同口径。
	checkRole := func(field, role string) bool {
		if _, ok := b.Parts[role]; !ok {
			add(field, fmt.Sprintf("references unknown role %q", role))
			return false
		}
		return true
	}

	// V7: anchor 若显式给出必须是真 role。fail-closed —— 给了个不存在的锚
	// 却悄悄回退到推导,等于让作者以为自己指定生效了。
	if layout.Anchor != "" {
		checkRole("schematic_layout.anchor", layout.Anchor)
	}

	// V2: 一个 role 最多出现在**一种**关系里。两条关系给同一个件两个种子点,
	// 求解器要么随机选要么放两次 —— 数据层禁掉比运行期消解便宜。
	seenIn := map[string]string{}
	claim := func(field, role, kind string) {
		if prev, ok := seenIn[role]; ok && prev != kind {
			add(field, fmt.Sprintf("role %q 同时出现在 %s 与 %s 里 —— 一个件只能由一种关系定位", role, prev, kind))
			return
		}
		seenIn[role] = kind
	}

	for i, role := range layout.Flow {
		field := fmt.Sprintf("schematic_layout.flow[%d]", i)
		if checkRole(field, role) {
			claim(field, role, "flow")
		}
	}

	for key, target := range layout.Attach {
		field := "schematic_layout.attach." + key
		if !checkRole(field, key) {
			continue
		}
		claim(field, key, "attach")

		// V3: 值必须是 ROLE.PIN;不许自贴;不许带 `*`。
		if strings.Contains(target, pinFanoutSuffix) {
			add(field, fmt.Sprintf("目标 %q 带 %q —— attach 要的是**一个点**,同名多脚是歧义;"+
				"USB-C 双 VBUS 这类请写引脚编号", target, pinFanoutSuffix))
			continue
		}
		tRole, tPin, ok := splitPinRef(target)
		if !ok {
			add(field, fmt.Sprintf("目标 %q 不是 ROLE.PIN 形式", target))
			continue
		}
		if !checkRole(field, tRole) {
			continue
		}
		if tRole == key {
			add(field, "不能贴到自己身上")
			continue
		}
		if strings.TrimSpace(tPin) == "" {
			add(field, fmt.Sprintf("目标 %q 缺引脚名", target))
			continue
		}
		// V4: attach 必须有**电气依据** —— internal_nets 里存在一条网,同时含
		// 目标引脚和 key 的任一脚。这是「去耦贴电源脚」的可机械判定形式,抓的是
		// 拼写错和「贴到一个跟自己没关系的脚上」(#145 的教训:块标着 verified
		// 却静默错接了十几天)。
		if !attachHasElectricalBasis(nets, key, target) {
			add(field, fmt.Sprintf("没有电气依据:internal_nets 里没有任何一条网同时连着 %s 和 %s 的引脚 —— "+
				"attach 表达的是「贴着它连的那个脚」,不是随便挨着放", target, key))
		}
	}

	for i, group := range layout.Pair {
		field := fmt.Sprintf("schematic_layout.pair[%d]", i)
		if len(group) < 2 {
			add(field, "并列组至少要两个成员")
			continue
		}
		var part string
		for _, role := range group {
			if !checkRole(field, role) {
				continue
			}
			claim(field, role, "pair")
			// V5: 组内必须同 part —— 「等距并列」的等距 pitch 合法性来自同型号
			// 同尺寸(求解器用第一个成员的实测宽度当全组 pitch)。
			p := b.Parts[role].Part
			if part == "" {
				part = p
			} else if p != part {
				add(field, fmt.Sprintf("成员 %q 的 part 是 %q,与组内其他成员的 %q 不同 —— "+
					"等距并列要求同型号(pitch 取自实测宽度,对全组通用)", role, p, part))
			}
		}
	}

	for role, o := range layout.Orient {
		field := "schematic_layout.orient." + role
		checkRole(field, role)
		if o != "vertical" && o != "horizontal" {
			add(field, fmt.Sprintf("must be vertical|horizontal, got %q", o))
		}
	}
	// V6 是**没有**全覆盖判据 —— 与 legacy 相反,这里刻意不检查"每个 part 都被
	// 关系覆盖":没进关系的件走 residual 布局是合法的。
}

// blockInternalNets 解析块的 internal_nets(校验用;解析失败交给 validateTopology
// 报,这里静默返回空 —— 同一个错误不重复报两遍)。
func blockInternalNets(b Block) [][]string {
	var doc struct {
		InternalNets [][]string `json:"internal_nets"`
	}
	if err := json.Unmarshal(b.Raw, &doc); err != nil {
		return nil
	}
	return doc.InternalNets
}

// attachHasElectricalBasis 报告是否存在一条内部网,同时连着 target 引脚与
// keyRole 的任一引脚。`*`(fanout)在两侧都按同一个边界处理。
//
// 编号宽恕(2026-08-17,三方互斥实锤):V3 对同名多脚(如 AMS1117 的 VOUT
// pin2+pin4)禁 `*` 并建议写引脚编号,但 internal_nets 用的是功能名 fanout
// (`U.VOUT*`)—— 字符串级匹配对不上,`U.VOUT`/`U.VOUT*`/`U.2` 三种写法被
// V3/V4/audit 三个校验器各拒一种,数据无解。离线校验器没有符号引脚表,判不了
// 「编号 2 是否属于 VOUT」;宽恕条件收敛为:target 是**纯数字编号** 且存在一条
// 网同时含「同 role 的 fanout 成员」与 key 的脚 —— 编号的真伪由带引脚表快照的
// blocks-audit(`make blocks-audit`)把关,两器互补不重叠。
func attachHasElectricalBasis(nets [][]string, keyRole, target string) bool {
	tgt := strings.TrimSuffix(target, pinFanoutSuffix)
	tRole, tPin, tOK := splitPinRef(tgt)
	numericPin := tOK && strings.IndexFunc(tPin, func(r rune) bool { return r < '0' || r > '9' }) < 0
	for _, net := range nets {
		hasTarget, hasKey := false, false
		for _, m := range net {
			if strings.HasPrefix(m, "PORT:") {
				continue
			}
			if strings.TrimSuffix(m, pinFanoutSuffix) == tgt {
				hasTarget = true
			}
			if numericPin && !hasTarget && strings.HasSuffix(m, pinFanoutSuffix) {
				if r, _, ok := splitPinRef(strings.TrimSuffix(m, pinFanoutSuffix)); ok && r == tRole {
					hasTarget = true // 同 role 的 fanout 网:编号真伪交给 blocks-audit
				}
			}
			if r, _, ok := splitPinRef(m); ok && r == keyRole {
				hasKey = true
			}
		}
		if hasTarget && hasKey {
			return true
		}
	}
	return false
}
