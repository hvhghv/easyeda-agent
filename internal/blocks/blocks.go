// Package blocks embeds the standard circuit-block library (电路块库) into the
// binary so `easyeda blocks ls/show/search` works anywhere the CLI is installed —
// no skill files, no GitHub checkout. The data/ dir IS the single source-of-truth
// (community contributes one block per file here; go:embed compiles it in). The
// skill ships no block JSON — it queries via the `easyeda blocks` CLI instead.
package blocks

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// data holds every block JSON. A directory embed excludes `_`-prefixed files, so
// _schema.json (shared doc/schema, not a block) is left out automatically.
//
//go:embed data
var data embed.FS

// Block is the queryable projection of a circuit block. The full original JSON
// is kept in Raw for `show` so nothing is lost to the struct shape.
type Block struct {
	SchemaVersion int             `json:"schema_version,omitempty"`
	Revision      int             `json:"revision,omitempty"`
	ID            string          `json:"id"`
	Desc          string          `json:"desc"`
	Category      string          `json:"category"`
	Author        string          `json:"author"`
	Contributors  []string        `json:"contributors"`
	Added         string          `json:"added"`
	Updated       string          `json:"updated"`
	Source        string          `json:"source"`
	Validated     *string         `json:"validated"` // legacy evidence; verification supersedes it
	Verification  *Verification   `json:"verification,omitempty"`
	Parts         map[string]Part `json:"parts"`
	Ports         map[string]Port `json:"ports"`
	Raw           json.RawMessage `json:"-"`
}

// Part is one role in a block, backed by the standard-parts library.
type Part struct {
	Part          string   `json:"part"`
	Alt           []string `json:"alt,omitempty"`
	Qty           int      `json:"qty"`
	ValueOverride string   `json:"value_override,omitempty"`
	Note          string   `json:"note,omitempty"`
}

// Port is a block boundary connection. At is always a functional ROLE.pin ref.
type Port struct {
	Dir        string `json:"dir"`
	At         string `json:"at"`
	Desc       string `json:"desc"`
	DefaultNet string `json:"default_net,omitempty"`
}

// SchematicLayoutHint is one role's schematic placement relative to the block's
// apply origin (--at). Offsets are schematic units in the y-UP canvas
// space the autolayout planner uses (+dy places the part HIGHER on the sheet),
// and must land on the 5-unit placement grid — an off-grid anchor puts every
// symbol pin off-grid and connect_pin stubs then fail (see schAnchorGrid).
type SchematicLayoutHint struct {
	DX       float64 `json:"dx"`
	DY       float64 `json:"dy"`
	Rotation float64 `json:"rotation,omitempty"` // 0 | 90 | 180 | 270
}

// SchematicLayout is a block's schematic placement template. It has TWO forms:
//
//   - **relational**(推荐):只声明**意图** —— flow/attach/pair,坐标由布局求解器
//     算(吃引脚实测坐标、碰撞表、图纸边界、分区矩形)。
//   - **legacy**(roles: 绝对偏移,已废弃但仍受支持):块作者手算 dx/dy。
//
// 为什么关系形态取代绝对偏移(issue #180):块作者写模板时**根本不知道**实例最终
// 落在页面哪里、旁边有什么、图纸多大、分区标题带在哪 —— 那些信息只有落地时才有。
// 2026-08-13 同一轮就踩了三个坑:负偏移把件推出图纸左界、块太高顶出上界、去耦
// 电容顶到分区标题带。三个都是几何问题,本就该由求解器处理。
//
// 两种形态**互斥**:同时声明是数据错误(两条关系给同一个件两个种子点,求解器要么
// 随机选要么放两次)。
type SchematicLayout struct {
	Note string `json:"note,omitempty"`

	// Roles(legacy)是绝对偏移表。新块不要再写。
	Roles map[string]SchematicLayoutHint `json:"roles,omitempty"`

	// Anchor 是块的锚件 role(可选)。缺省由求解器推导:被 attach 指向次数最多的
	// role —— 那正是「谁是主芯片」的电路学定义(去耦挂在谁身上谁就是主角)。
	Anchor string `json:"anchor,omitempty"`
	// Flow 是信号流顺序(左→右),不要求覆盖全部 role:没进任何关系的件走 residual
	// 布局是合法的(bulk 电容不该被迫塞进 flow)。
	Flow []string `json:"flow,omitempty"`
	// Attach 是「角色 → 目标.引脚」:去耦贴电源脚、ESD 贴数据脚。值必须是
	// ROLE.PIN,且**不许带 `*`** —— attach 要的是一个点,同名多脚是歧义。
	Attach map[string]string `json:"attach,omitempty"`
	// Pair 是等距并列组(CC1/CC2 这种同型号同角色器件)。组内成员必须同 part。
	Pair [][]string `json:"pair,omitempty"`
	// Orient 只说**意图**(竖放/横放),不说角度 —— 具体 90 还是 270 由求解器按
	// 引脚朝向消解。
	Orient map[string]string `json:"orient,omitempty"`
}

// IsRelational reports whether the template declares relationships (the form the
// layout solver consumes).
func (l *SchematicLayout) IsRelational() bool {
	if l == nil {
		return false
	}
	return len(l.Flow) > 0 || len(l.Attach) > 0 || len(l.Pair) > 0
}

// IsLegacy reports whether the template declares absolute offsets.
func (l *SchematicLayout) IsLegacy() bool {
	return l != nil && len(l.Roles) > 0
}

// SchematicLayout parses the block's optional schematic_layout template out of
// Raw (nil when the block does not declare one). The typed projection keeps
// extension maps in Raw, same as internal_nets.
func (b Block) SchematicLayout() (*SchematicLayout, error) {
	var doc struct {
		SchematicLayout *SchematicLayout `json:"schematic_layout"`
	}
	if err := json.Unmarshal(b.Raw, &doc); err != nil {
		return nil, fmt.Errorf("parse schematic_layout: %w", err)
	}
	return doc.SchematicLayout, nil
}

// VerificationStage records one independently reviewable readiness dimension.
// Status is passed, failed, pending, or not_tested; evidence remains human-readable.
type VerificationStage struct {
	Status   string   `json:"status"`
	Evidence string   `json:"evidence,omitempty"`
	Issues   []string `json:"issues,omitempty"`
}

// Verification separates topology evidence from selection, PCB, and bring-up.
// ProductionReady is deliberately explicit and is validated against all four gates.
type Verification struct {
	Schematic          VerificationStage `json:"schematic"`
	ComponentSelection VerificationStage `json:"component_selection"`
	PCBDRC             VerificationStage `json:"pcb_drc"`
	Bringup            VerificationStage `json:"bringup"`
	ProductionReady    bool              `json:"production_ready"`
}

// Ready reports production readiness for v2 data. Legacy blocks retain the old
// non-empty validated behavior until they are migrated to structured verification.
func (b Block) Ready() bool {
	if b.Verification != nil {
		return b.Verification.ProductionReady
	}
	return b.Validated != nil && strings.TrimSpace(*b.Validated) != ""
}

// Status is the user-facing maturity label. Structured verification avoids
// collapsing a schematic-verified block back into the ambiguous legacy "draft".
func (b Block) Status() string {
	if b.Ready() {
		return "ready"
	}
	if b.Verification != nil && b.Verification.Schematic.Status == "passed" {
		return "verified"
	}
	return "draft"
}

// Load parses every embedded block, sorted by id. It never touches the disk, so
// it works from a bare binary.
func Load() ([]Block, error) {
	entries, err := fs.ReadDir(data, "data")
	if err != nil {
		return nil, err
	}
	var out []Block
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "_") {
			continue
		}
		raw, err := data.ReadFile("data/" + name)
		if err != nil {
			return nil, err
		}
		var b Block
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("block %s: %w", name, err)
		}
		b.Raw = raw
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns the block whose id matches exactly, with or without the `block.`
// prefix (both `block.xl1509_buck_12v_5v` and `xl1509_buck_12v_5v` resolve).
func Get(id string) (Block, bool, error) {
	all, err := Load()
	if err != nil {
		return Block{}, false, err
	}
	want := strings.TrimPrefix(id, "block.")
	for _, b := range all {
		if b.ID == id || strings.TrimPrefix(b.ID, "block.") == want {
			return b, true, nil
		}
	}
	return Block{}, false, nil
}

// Search returns blocks whose id/desc/category/ports/parts contain the query
// (case-insensitive), so an agent can find "the buck block" without knowing its id.
func Search(query string) ([]Block, error) {
	all, err := Load()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return all, nil
	}
	var out []Block
	for _, b := range all {
		var sb strings.Builder
		sb.WriteString(b.ID)
		sb.WriteByte(' ')
		sb.WriteString(b.Desc)
		sb.WriteByte(' ')
		sb.WriteString(b.Category)
		for k := range b.Ports {
			sb.WriteByte(' ')
			sb.WriteString(k)
		}
		for k := range b.Parts {
			sb.WriteByte(' ')
			sb.WriteString(k)
		}
		if strings.Contains(strings.ToLower(sb.String()), q) {
			out = append(out, b)
		}
	}
	return out, nil
}
