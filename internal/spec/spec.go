// Package spec 是 S0 设计方案书（S0 spec）的**唯一类型定义**。
//
// 为什么要建这个包。S0 spec 是 design-flow.md 里说的「人类意图唯一入口」，但在
// 此之前它只是一段散文 + 示例 JSON：没有 Go 结构体、没有 schema、没有校验命令。
// 真正读它的只有两个函数，各自写一份匿名 struct 只取 modules[] 的一小撮键
// （pcb_zones.go 的 parseZoneSpec 不读 page，cmd_sch_zones.go 的 parseSchZoneSpec
// 读），字段集已经不一致；stackup/rf/board/interfaces/pages/costTier 这些文档里
// 写着的字段**零代码消费**，全靠 agent 读文本自己执行。
//
// 后果是契约松到已经漂移：磁盘上唯一那份真实 spec（.easyeda/s0-n8r8-ceshi.json）
// 把 `board` 写成字符串而不是文档说的对象、`stackup` 用 inner1/inner2 而不是
// groundStrategy/innerLayers、还多出一个文档里根本没有的 `assembly` —— 而这些
// 全部静默通过，因为没有任何东西在看。
//
// #167 要往 spec 加 `flow`（信号流向意图）和连接器的 `edge/facing/internal`，
// 再按老办法「每个消费者自己再解析一遍」就是把这个坑挖深。所以先立类型 + 校验，
// 再谈新字段。
//
// 兼容性原则：**只加不改**。所有既有写法（含上面那三处漂移）必须继续能读，
// 校验器把它们报成 warning 而不是 error —— 硬失败会让用户手上的 spec 一夜之间
// 全废，那不是契约收紧，那是破坏。
package spec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 词汇表
// ---------------------------------------------------------------------------

// ZoneNames 是分区格的合法取值（3 列 × 2 行的九宫格变体，共 11 个）。
//
// 必须与 internal/app/pcb_zones.go 的 pcbZoneNames 保持一致；那边是消费侧，
// 这边是契约侧。internal/app 有一条单测断言两者相同（app 可以 import spec，
// 反向不行，所以词汇表在这里定义、那边校验）。
var ZoneNames = map[string]bool{
	"left": true, "center": true, "right": true,
	"top": true, "bottom": true,
	"left-top": true, "left-bottom": true,
	"center-top": true, "center-bottom": true,
	"right-top": true, "right-bottom": true,
}

// ModuleKinds 是功能域词汇 —— `flow` 里出现的就是这些值，modules[].kind 也用它。
//
// 为什么需要一个受控词汇：flow-order 维要算「模块质心沿流向轴的排序 vs spec 声明
// 顺序」的一致度，两边必须能对上。现有 spec 的 modules[].name 是自由文本
// （实例里是 MCU/FLASH/RF/IO），靠 name 匹配会在大小写和同义词上碎掉。
var ModuleKinds = map[string]bool{
	"POWER":      true, // 输入保护 / 稳压 / 电源树
	"MCU":        true, // 主控及其最小系统
	"RF":         true, // 射频前端 / 无线模组
	"ANT":        true, // 天线及其净空区
	"IO":         true, // 对外接口 / 连接器
	"ANALOG":     true, // 模拟前端 / 基准
	"SENSOR":     true,
	"STORAGE":    true, // Flash / SD / eMMC
	"USB":        true,
	"DEBUG":      true, // 调试口 / 下载电路
	"PROTECTION": true, // 保险丝 / TVS / ESD
	"POWER_MON":  true, // 采样 / 计量
	"OTHER":      true,
}

// ConnectorFacings 是连接器的「朝向谁」语义 —— #168 internal-on-edge 的判据源。
//
//	user-facing：用户插拔的对外口（USB / 电源端子 / 调试口），外壳要开孔，
//	             理应占用板外沿。
//	internal   ：只在箱内连线的连接器（备份电池座 / 板间排针）。它占了外沿就是
//	             浪费稀缺资源，而且电芯辫线还得从边上绕回箱内 —— 这正是 box-v2
//	             那块外包板上 J1(PH2.0-3P 电池座) 的问题。
//	any        ：必须在某条边但哪条都行（RF 天线 / 无线模组）。
var ConnectorFacings = map[string]bool{
	"user-facing": true, "internal": true, "any": true,
}

// EdgeNames 是板边词汇（与 internal/app 的 apEdge.String() 对齐）+ "any"。
var EdgeNames = map[string]bool{
	"left": true, "right": true, "top": true, "bottom": true, "any": true,
}

// FlowAxes 是信号流向轴。auto = 用板框长边（长边通常就是信号流的方向）。
var FlowAxes = map[string]bool{"x": true, "y": true, "auto": true}

// ---------------------------------------------------------------------------
// 类型
// ---------------------------------------------------------------------------

// Module 是一个功能模块。Name 是自由文本标识（沿用既有 spec 的写法），Kind 是
// 新增的受控功能域 —— flow-order 维靠 Kind 对齐意图，Name 只用于人读和报错。
type Module struct {
	Name  string   `json:"name"`
	Kind  string   `json:"kind,omitempty"` // #167 新增：ModuleKinds 之一
	Page  string   `json:"page,omitempty"` // 原理图分页（PCB 侧忽略）
	Zone  string   `json:"zone,omitempty"`
	Block string   `json:"block,omitempty"` // 电路块 id（internal/blocks）
	Parts []string `json:"parts,omitempty"`
}

// Interface 是一个对外接口 / 连接器的**板级意图**。
//
// 与块库 placement.<ROLE> 的分工：块声明的是**器件类别的通用知识**（Type-C 天生
// 是对外口、开口朝哪），这里声明的是**这块板的具体决定**（J1 这个 PH2.0 座在本
// 设计里接箱内电芯，所以是 internal）。两者冲突时 spec 赢 —— 板级决定比类别经验
// 更具体。消费侧必须把来源标出来（spec / block / heuristic），因为置信度不同：
// spec 显式标注报 WARN，启发式推定只报 INFO。
type Interface struct {
	Name        string  `json:"name"`
	Ref         string  `json:"ref,omitempty"`         // #167 新增：位号（J1/USB1…），把意图钉到具体器件
	Edge        string  `json:"edge,omitempty"`        // #167 新增：EdgeNames 之一
	Facing      string  `json:"facing,omitempty"`      // #167 新增：ConnectorFacings 之一
	Internal    bool    `json:"internal,omitempty"`    // #168：facing=="internal" 的简写
	Orientation string  `json:"orientation,omitempty"` // 既有：单/双取向等自由文本
	PlugWidthMM float64 `json:"plugWidthMm,omitempty"` // #168②：插头护套宽（查不到表时的人工覆盖）
}

// FacingOf 归一化朝向：显式 Facing 优先，其次 Internal 简写，最后留空由消费侧
// 走启发式。
func (i Interface) FacingOf() string {
	if f := strings.TrimSpace(i.Facing); f != "" {
		return f
	}
	if i.Internal {
		return "internal"
	}
	return ""
}

// Stackup 是叠层意图。兼容两种写法：文档的 groundStrategy/innerLayers 与磁盘
// 实例的 inner1/inner2。
type Stackup struct {
	Layers         int      `json:"layers,omitempty"`
	GroundStrategy string   `json:"groundStrategy,omitempty"`
	InnerLayers    []string `json:"innerLayers,omitempty"`
	Inner1         string   `json:"inner1,omitempty"`
	Inner2         string   `json:"inner2,omitempty"`
}

// Inners 把两种写法归一成有序内层列表。
func (s *Stackup) Inners() []string {
	if s == nil {
		return nil
	}
	if len(s.InnerLayers) > 0 {
		return s.InnerLayers
	}
	var out []string
	if s.Inner1 != "" {
		out = append(out, s.Inner1)
	}
	if s.Inner2 != "" {
		out = append(out, s.Inner2)
	}
	return out
}

// RF 是射频/天线意图。
type RF struct {
	Parts         []string `json:"parts,omitempty"`
	KeepoutLayers string   `json:"keepoutLayers,omitempty"`
	Feed          string   `json:"feed,omitempty"`
}

// Assembly 是装配意图（文档里没有，但磁盘实例已经在用 —— 收编而不是拒绝）。
type Assembly struct {
	Profile string `json:"profile,omitempty"` // hand-solder | reflow
	Side    string `json:"side,omitempty"`    // top | both
}

// Board 是板形意图。文档写成对象 {"outline":"compact"}，磁盘实例写成字符串
// "compact" —— 两种都收，用自定义 Unmarshal 抹平。
type Board struct {
	Outline  string  `json:"outline,omitempty"`
	WidthMM  float64 `json:"widthMm,omitempty"`
	HeightMM float64 `json:"heightMm,omitempty"`
}

// UnmarshalJSON 兼容 `"board": "compact"` 与 `"board": {"outline": "compact"}`。
func (b *Board) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		b.Outline = s
		return nil
	}
	type alias Board // 防递归
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*b = Board(a)
	return nil
}

// Page 是原理图分页意图。
type Page struct {
	Name    string   `json:"name"`
	Sheet   string   `json:"sheet,omitempty"`
	Modules []string `json:"modules,omitempty"`
}

// Spec 是 S0 设计方案书的完整形状。
//
// Flow 是 #167 的核心新增：一个有序的功能域列表，表达「电源 → 数字 → RF → 天线」
// 这种信号流向意图。flow-order 维把各模块质心沿 FlowAxis 投影排序，与这个声明
// 顺序算 Kendall-tau 一致度。没写 flow 就没有目标序列，那一维会被标成 skipped
// 而不是给满分 —— 「没测」和「测了满分」必须可区分。
type Spec struct {
	Board      *Board      `json:"board,omitempty"`
	Sheet      string      `json:"sheet,omitempty"`
	Stackup    *Stackup    `json:"stackup,omitempty"`
	Assembly   *Assembly   `json:"assembly,omitempty"`
	RF         *RF         `json:"rf,omitempty"`
	Modules    []Module    `json:"modules,omitempty"`
	Pages      []Page      `json:"pages,omitempty"`
	Interfaces []Interface `json:"interfaces,omitempty"`
	Flow       []string    `json:"flow,omitempty"`     // #167 新增
	FlowAxis   string      `json:"flowAxis,omitempty"` // #167 新增：x | y | auto（默认 auto）
	CostTier   string      `json:"costTier,omitempty"`
	Notes      any         `json:"notes,omitempty"`
}

// Axis 返回生效的流向轴（默认 auto）。
func (s *Spec) Axis() string {
	if s == nil {
		return "auto"
	}
	if a := strings.ToLower(strings.TrimSpace(s.FlowAxis)); FlowAxes[a] {
		return a
	}
	return "auto"
}

// ModuleByKind 按功能域分组模块（Kind 归一为大写）。没写 kind 的模块会尝试用
// Name 反查一次 —— 老 spec 的 name 恰好常是 MCU/RF/IO 这类词，能救回一部分。
func (s *Spec) ModuleByKind() map[string][]Module {
	out := map[string][]Module{}
	if s == nil {
		return out
	}
	for _, m := range s.Modules {
		k := m.KindOf()
		if k == "" {
			continue
		}
		out[k] = append(out[k], m)
	}
	return out
}

// KindOf 是模块的生效功能域：显式 Kind 优先，否则拿 Name 去词汇表碰一次运气。
func (m Module) KindOf() string {
	if k := strings.ToUpper(strings.TrimSpace(m.Kind)); ModuleKinds[k] {
		return k
	}
	if n := strings.ToUpper(strings.TrimSpace(m.Name)); ModuleKinds[n] {
		return n
	}
	return ""
}

// PartsOf 返回模块声明的位号（去空白）。
func (m Module) PartsOf() []string {
	out := make([]string, 0, len(m.Parts))
	for _, p := range m.Parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// InterfaceByRef 建位号 → 接口意图的索引（Ref 为空的条目退而用 Name 当 key，
// 因为老 spec 的 interfaces[] 只有 name）。
func (s *Spec) InterfaceByRef() map[string]Interface {
	out := map[string]Interface{}
	if s == nil {
		return out
	}
	for _, i := range s.Interfaces {
		key := strings.TrimSpace(i.Ref)
		if key == "" {
			key = strings.TrimSpace(i.Name)
		}
		if key != "" {
			out[strings.ToUpper(key)] = i
		}
	}
	return out
}

// PartModule 建位号 → 模块的反查索引。
func (s *Spec) PartModule() map[string]Module {
	out := map[string]Module{}
	if s == nil {
		return out
	}
	for _, m := range s.Modules {
		for _, p := range m.PartsOf() {
			out[strings.ToUpper(p)] = m
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 加载 + 校验
// ---------------------------------------------------------------------------

// Issue 是一条校验结果。Level: ERROR 阻塞、WARN 可继续、INFO 提示。
type Issue struct {
	Level   string `json:"level"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Parse 解析一份 S0 spec。只有 JSON 本身坏了才报错；字段层面的问题一律走
// Validate 的 Issue 列表，因为「spec 写得不完整」不该让命令跑不起来。
func Parse(raw []byte) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse S0 spec: %w", err)
	}
	return &s, nil
}

// Validate 检查一份 spec 并返回全部问题（按 Level 再按 Field 排序，输出确定）。
//
// 判定口径刻意宽松：ERROR 只留给「写了但写错」（枚举外的取值、flow 引用了不存在
// 的功能域），缺字段一律 WARN/INFO。理由见包注释 —— 现存 spec 全都缺新字段，
// 把缺失判成 ERROR 等于把所有人的 spec 一次性打死。
func Validate(s *Spec) []Issue {
	var out []Issue
	add := func(level, field, format string, args ...any) {
		out = append(out, Issue{Level: level, Field: field, Message: fmt.Sprintf(format, args...)})
	}
	if s == nil {
		add("ERROR", "", "spec is empty")
		return out
	}

	// --- modules ---
	if len(s.Modules) == 0 {
		add("WARN", "modules", "no modules declared — zone/flow dimensions have no intent to score against")
	}
	seenModule := map[string]bool{}
	kindSeen := map[string]bool{}
	for i, m := range s.Modules {
		field := fmt.Sprintf("modules[%d]", i)
		name := strings.TrimSpace(m.Name)
		if name == "" {
			add("ERROR", field+".name", "module name is required")
		} else if seenModule[strings.ToUpper(name)] {
			add("ERROR", field+".name", "duplicate module name %q", name)
		} else {
			seenModule[strings.ToUpper(name)] = true
		}
		if m.Zone != "" && !ZoneNames[m.Zone] {
			add("ERROR", field+".zone", "unknown zone %q (want one of %s)", m.Zone, sortedKeys(ZoneNames))
		}
		if k := strings.TrimSpace(m.Kind); k != "" && !ModuleKinds[strings.ToUpper(k)] {
			add("ERROR", field+".kind", "unknown module kind %q (want one of %s)", k, sortedKeys(ModuleKinds))
		}
		if got := m.KindOf(); got != "" {
			kindSeen[got] = true
		} else if len(s.Flow) > 0 {
			// 声明了 flow 却有模块没归域 → 这些模块在 flow-order 维里是隐形的。
			add("WARN", field+".kind", "module %q has no kind — it cannot participate in the flow-order dimension", name)
		}
		if len(m.PartsOf()) == 0 && m.Block == "" {
			add("INFO", field+".parts", "module %q declares neither parts nor a block", name)
		}
	}

	// --- flow ---
	if len(s.Flow) == 0 {
		// 提前告知而不是让用户在打分报告里发现少了一维：没有 flow 就没有目标序列，
		// flow-order 维会被标 skipped。这是能力降级不是错误，所以只报 INFO。
		add("INFO", "flow", "no signal flow declared — the flow-order dimension of `pcb layout-score` will be skipped (e.g. \"flow\": [\"POWER\",\"MCU\",\"RF\",\"ANT\"])")
	} else {
		seenFlow := map[string]bool{}
		for i, f := range s.Flow {
			field := fmt.Sprintf("flow[%d]", i)
			k := strings.ToUpper(strings.TrimSpace(f))
			switch {
			case k == "":
				add("ERROR", field, "empty flow entry")
			case !ModuleKinds[k]:
				add("ERROR", field, "unknown flow stage %q (want one of %s)", f, sortedKeys(ModuleKinds))
			case seenFlow[k]:
				add("ERROR", field, "duplicate flow stage %q — the flow must be a strict order", f)
			default:
				seenFlow[k] = true
				if len(s.Modules) > 0 && !kindSeen[k] {
					add("WARN", field, "flow stage %q has no module of that kind on the board", f)
				}
			}
		}
		if len(seenFlow) < 2 {
			add("WARN", "flow", "a flow with fewer than 2 stages cannot express an order")
		}
	}
	if a := strings.TrimSpace(s.FlowAxis); a != "" && !FlowAxes[strings.ToLower(a)] {
		add("ERROR", "flowAxis", "unknown flow axis %q (want x | y | auto)", a)
	}

	// --- interfaces ---
	for i, in := range s.Interfaces {
		field := fmt.Sprintf("interfaces[%d]", i)
		if strings.TrimSpace(in.Name) == "" && strings.TrimSpace(in.Ref) == "" {
			add("ERROR", field, "interface needs a name or a ref")
		}
		if e := strings.TrimSpace(in.Edge); e != "" && !EdgeNames[strings.ToLower(e)] {
			add("ERROR", field+".edge", "unknown edge %q (want left|right|top|bottom|any)", e)
		}
		if f := strings.TrimSpace(in.Facing); f != "" && !ConnectorFacings[strings.ToLower(f)] {
			add("ERROR", field+".facing", "unknown facing %q (want user-facing|internal|any)", f)
		}
		if in.Internal && strings.EqualFold(in.Facing, "user-facing") {
			add("ERROR", field, "internal:true contradicts facing:\"user-facing\"")
		}
		if strings.TrimSpace(in.Ref) == "" {
			// 没有 ref 就钉不到具体器件，internal-on-edge 只能退回启发式(INFO 档)。
			add("INFO", field+".ref", "interface %q has no ref — connector rules fall back to heuristics for it", in.Name)
		}
		if in.PlugWidthMM < 0 {
			add("ERROR", field+".plugWidthMm", "plug width must be positive")
		}
	}

	// --- stackup ---
	if s.Stackup != nil {
		switch s.Stackup.Layers {
		case 0:
			add("INFO", "stackup.layers", "layer count not declared")
		case 1, 2, 4, 6, 8, 10:
		default:
			add("WARN", "stackup.layers", "unusual layer count %d", s.Stackup.Layers)
		}
		if n := len(s.Stackup.Inners()); s.Stackup.Layers > 2 && n == 0 {
			add("WARN", "stackup", "%d-layer board declares no inner layer roles", s.Stackup.Layers)
		}
	}

	// --- assembly ---
	if s.Assembly != nil && s.Assembly.Profile != "" {
		switch strings.ToLower(s.Assembly.Profile) {
		case "hand-solder", "reflow":
		default:
			add("ERROR", "assembly.profile", "unknown profile %q (want hand-solder|reflow)", s.Assembly.Profile)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if r := levelRank(out[i].Level) - levelRank(out[j].Level); r != 0 {
			return r < 0
		}
		return out[i].Field < out[j].Field
	})
	return out
}

// HasErrors 报告校验结果里是否有 ERROR。
func HasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Level == "ERROR" {
			return true
		}
	}
	return false
}

func levelRank(l string) int {
	switch l {
	case "ERROR":
		return 0
	case "WARN":
		return 1
	default:
		return 2
	}
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "|")
}
