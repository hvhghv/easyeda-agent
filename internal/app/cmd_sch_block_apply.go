package app

// cmd_sch_block_apply.go — the PURE planner for `sch block-apply`.
//
// This is the vertical-slice experiment from chat/2026-07-16-blocks-data-model.md:
// blocks have proven useful as a structured KNOWLEDGE base, but nothing yet
// proves the JSON is worth maintaining as an executable IR. The only way to find
// out is to actually execute one: load a block → place its parts → wire its
// internal_nets → bind its ports → check → emit a traceable instance manifest.
//
// Scope is deliberately minimal (per that doc): parts / internal_nets / ports
// only. pcb_layout, placement, signals and silk are NOT consumed here — and the
// manifest says so explicitly rather than implying the whole block was honoured.
//
// The planner is PURE and deterministic: same block + parts library + scene
// always yields the same designators, coordinates and nets, so it is unit
// testable with no connector. The I/O side lives in cmd_sch_block_apply_run.go.

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
)

// ── the role-id → device bridge ─────────────────────────────────────────────

// bapDevice is the standard-parts.json entry a block's parts[role].part points
// at. This is the bridge internal/blocks/placement.go documents as missing: a
// block declares an internal role-id ("res.1k_0402"), and only this table turns
// it into something placeable ({libraryUuid, deviceUuid}).
type bapDevice struct {
	LibraryUUID string
	DeviceUUID  string
	MPN         string
	LCSC        string
	Value       string
	Basic       bool
}

// ── designator prefixes ─────────────────────────────────────────────────────

// bapPrefixes maps a part-key NAMESPACE (the segment before the dot in
// "res.1k_0402") to a reference-designator prefix.
//
// Provenance matters here, so it is recorded rather than asserted. Six prefixes
// are EVIDENCED — they are what this project's own validated boards actually
// used (examples/esp32-mini playbooks): C, R, J, U, SW, LED. The rest follow the
// standard IEEE-315 / IPC reference-designator classes. They are marked below so
// a future reader knows which ones are backed by a real board and which are
// convention awaiting first use.
//
// An UNKNOWN namespace is a hard error, never a guessed prefix: silently filing
// a new part class under the wrong designator would corrupt a board in a way
// that is tedious to unpick.
var bapPrefixes = map[string]string{
	// evidenced on real validated boards (examples/esp32-mini)
	"cap":  "C",
	"res":  "R",
	"conn": "J",
	"ic":   "U",
	"mcu":  "U",
	"sw":   "SW",
	"led":  "LED",
	// conventional (IEEE-315 classes) — not yet exercised by a placed board
	"ind":       "L",
	"fb":        "FB",
	"diode":     "D",
	"zener":     "D",
	"tvs":       "D",
	"esd":       "D",
	"bjt":       "Q",
	"pmos":      "Q",
	"mosfet":    "Q",  // StickS3 吸收引入(2N7002/2N7002DW/CJ3439KDW)
	"sensor":    "U",  // StickS3 吸收引入(BMI270 IMU)
	"mic":       "MK", // StickS3 吸收引入(MSM381 MEMS mic;IEEE-315 麦克风=MK)
	"xtal":      "X",
	"fuse":      "F",
	"ant":       "ANT",
	"opto":      "U",
	"ldo":       "U",
	"buck":      "U",
	"buckboost": "U",
	"charger":   "U",
	"pmu":       "U",
	"gnss":      "U",
	"storage":   "U",
}

// bapPrefixFor resolves a part key to its designator prefix.
func bapPrefixFor(partKey string) (string, error) {
	ns, _, ok := strings.Cut(partKey, ".")
	if !ok || ns == "" {
		return "", fmt.Errorf("part key %q has no namespace (expected e.g. res.1k_0402)", partKey)
	}
	p, ok := bapPrefixes[ns]
	if !ok {
		return "", fmt.Errorf("part namespace %q has no designator prefix mapping — add it to bapPrefixes "+
			"(guessing a prefix would misfile the part)", ns)
	}
	return p, nil
}

// ── flag kinds ──────────────────────────────────────────────────────────────

// bapFlagKind picks the netflag kind for a net name. Blocks do not declare flag
// kinds, so this is a small deterministic rule rather than a judgment call:
// ground names get a ground flag, supply-looking names get a power flag, and
// everything else gets a bidirectional net port (which merges purely by name and
// is therefore always structurally safe).
//
// It is a NAME heuristic and can be wrong for an unconventionally named rail —
// `--kind NET=kind` overrides it per net.
func bapFlagKind(net string) string {
	u := strings.ToUpper(strings.TrimSpace(net))
	switch u {
	case "GND", "VSS", "0V":
		return "gnd"
	case "AGND":
		return "agnd"
	case "PGND", "EARTH":
		return "pgnd"
	}
	if strings.HasSuffix(u, "_GND") || strings.HasSuffix(u, "GND") {
		return "gnd"
	}
	// Supply rails: a leading + (+3V3), a V-prefixed rail (VCC/VBAT/VBUS/VSYS/VIN),
	// or an nVn / nV form (3V3, 5V).
	if strings.HasPrefix(u, "+") ||
		strings.HasPrefix(u, "VCC") || strings.HasPrefix(u, "VDD") || strings.HasPrefix(u, "VBAT") ||
		strings.HasPrefix(u, "VBUS") || strings.HasPrefix(u, "VSYS") || strings.HasPrefix(u, "VIN") {
		return "power"
	}
	if bapLooksLikeRail(u) {
		return "power"
	}
	return "netport"
}

// bapLooksLikeRail reports whether a name reads as a voltage rail: 5V, 3V3,
// 12V, 1V8 — a leading digit, then V, then optional digits, and nothing else.
func bapLooksLikeRail(u string) bool {
	i := 0
	for i < len(u) && u[i] >= '0' && u[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(u) || u[i] != 'V' {
		return false
	}
	for j := i + 1; j < len(u); j++ {
		if u[j] < '0' || u[j] > '9' {
			return false
		}
	}
	return true
}

// ── plan shapes ─────────────────────────────────────────────────────────────

// bapPlacement is one part the plan will create.
type bapPlacement struct {
	Role        string  `json:"role"`
	PartKey     string  `json:"part"`
	Designator  string  `json:"designator"`
	PrimitiveID string  `json:"primitiveId,omitempty"`
	LibraryUUID string  `json:"libraryUuid"`
	DeviceUUID  string  `json:"deviceUuid"`
	LCSC        string  `json:"lcsc,omitempty"`
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	Rotation    float64 `json:"rotation,omitempty"`
	Source      string  `json:"layout,omitempty"` // "template" | "grid"
}

// bapOrigin records where the block actually landed vs where the caller asked,
// so a silent auto-relocation never reads as "placed at --at".
type bapOrigin struct {
	RequestedX float64 `json:"requestedX"`
	RequestedY float64 `json:"requestedY"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Relocated  bool    `json:"relocated"`
	Reason     string  `json:"reason,omitempty"`
}

// bapNet is one internal net, resolved to placed-pin references.
type bapNet struct {
	Net     string   `json:"net"`
	Kind    string   `json:"kind"`
	Port    string   `json:"port,omitempty"`  // set when this net is a block boundary
	Bound   bool     `json:"bound,omitempty"` // true when the host supplied --bind
	Members []string `json:"members"`         // DESIGNATOR:PIN, ready for autoconnect
	Roles   []string `json:"roles"`           // ROLE.pin, for traceability back to the block
}

// bapPlan is the full deterministic plan + the instance manifest skeleton.
type bapPlan struct {
	BlockID    string         `json:"blockId"`
	Revision   int            `json:"revision,omitempty"`
	Instance   string         `json:"instance"`
	Origin     *bapOrigin     `json:"origin,omitempty"`
	Placements []bapPlacement `json:"placements"`
	Nets       []bapNet       `json:"nets"`
	Warnings   []string       `json:"warnings,omitempty"`
	// Unconsumed names the block's constraint maps that this command does NOT
	// execute, so a caller never reads a successful apply as "block fully honoured".
	Unconsumed []string `json:"unconsumedConstraints,omitempty"`
	// Relational/AnchorRole 标记这是关系形态模板:落地时先放锚件、回读它的实测
	// 引脚,再用求解器算其余件的位姿(issue #180 P2)。
	Relational bool   `json:"relationalLayout,omitempty"`
	AnchorRole string `json:"anchorRole,omitempty"`
}

// ── the planner ─────────────────────────────────────────────────────────────

// bapInput is everything the pure planner needs.
type bapInput struct {
	Block    blocks.Block
	Topology [][]string           // internal_nets, already parsed out of Raw
	Devices  map[string]bapDevice // part key → device (from standard-parts.json)
	Existing map[string]bool      // designators already on the page (upper-cased)
	Instance string               // instance id, e.g. "led1"
	OriginX  float64
	OriginY  float64
	Spacing  float64
	PerRow   int
	Bind     map[string]string // PORT → host net
	KindOver map[string]string // NET → flag kind override
	// Layout is the block's schematic_layout template (nil → fallback grid).
	Layout *blocks.SchematicLayout
	// Obstacles are the existing parts' rendered bboxes on the active page; when
	// present, the planner auto-relocates a colliding origin (unless AtExplicit).
	Obstacles  []layoutBBox
	AtExplicit bool // --at was passed explicitly: the origin is the caller's decision
	// TitleBlock is the A4 图签/明细表 keep-out rectangle (bottom-right), when the
	// sheet bbox is known; nil → not enforced. Treated as an extra obstacle so the
	// block origin never lands on the title block (issue #141).
	TitleBlock *layoutBBox
	// Sheet 是图纸边框 bbox。**它必须参与螺旋搜索**,不能只做事后 warning:
	// origin 搜索从前给 findSlot 的 inBounds 传 nil,于是"最近的空位"可以在图纸
	// 外面 —— 实测把 J_USB 放到 x=-20、把 R6 放到 y=880(sheet 上界 825)。
	// nil → 不约束(并在 manifest 里如实说明未强制)。见 issue #180 Fix B。
	Sheet *layoutBBox
}

// bapRoleOffset is one role's resolved offset from the block origin.
type bapRoleOffset struct {
	dx, dy, rot float64
	source      string // "template" | "grid"
}

// bapPartMargin approximates half a typical small symbol's rendered extent
// (schematic units) — used only to estimate the block's footprint rectangle for
// origin collision avoidance, never for exact geometry (real bboxes exist only
// after placement; `sch layout-lint` remains the post-placement ground truth).
const bapPartMargin = 50

// bapObstacleGap is the min edge-to-edge clearance the block's estimated
// footprint keeps from existing parts (mirrors autolayout's PartGap).
const bapObstacleGap = 20

// bapRoleHalfExtent estimates half a symbol's rendered width by part class. Crude
// on purpose — real bboxes exist only after placement — but it must not UNDER-shoot,
// because the grid uses it to decide how far apart to put parts.
func bapRoleHalfExtent(partKey string) float64 {
	k := strings.ToLower(partKey)
	switch {
	// High-pin-count silicon: the symbol is a tall box with two pin columns, and
	// its half width grows with the pin count. ESP32-S3 (QFN56, 57 pins) overlapped
	// its neighbour by 126 at the 100 estimate below, so these get their own tier.
	case strings.HasPrefix(k, "mcu."),
		strings.Contains(k, "qfn56"), strings.Contains(k, "qfn48"),
		strings.Contains(k, "lqfp"), strings.Contains(k, "bga"),
		strings.Contains(k, "wroom"), strings.Contains(k, "wrover"):
		// 250, not 200: at 200 the ESP32-S3 chip symbol still overlapped its
		// neighbouring SOP-8 by 26. These numbers are MEASURED corrections, not
		// theory — the estimate can only ever be a floor, which is why blocks that
		// care about geometry should ship a schematic_layout template instead.
		return 250
	// Ordinary ICs: CH334F (QFN24) measured ~70 anchor-to-pin-column.
	case strings.HasPrefix(k, "ic."),
		strings.HasPrefix(k, "buck."), strings.HasPrefix(k, "buckboost."),
		strings.HasPrefix(k, "ldo."), strings.HasPrefix(k, "charger."),
		strings.HasPrefix(k, "pmu."), strings.HasPrefix(k, "esd."),
		strings.HasPrefix(k, "opto."), strings.HasPrefix(k, "gnss."),
		strings.HasPrefix(k, "storage."):
		return 100
	// Connectors and sockets are wide too (USB-C 16P, microSD, SIM).
	case strings.HasPrefix(k, "conn."):
		return 90
	}
	return float64(bapPartMargin) // discretes: R/C/L/D/crystal
}

// bapGridSpacing sizes the fallback grid from the BIGGEST part in the block. The
// old fixed 100 equalled one small symbol's full width (2×bapPartMargin), leaving
// literally zero gap — fine for discretes, but an IC's pins then reach into the
// neighbouring cell. That is not a cosmetic overlap: CH334F's U3:20 (VDD33) landed
// on exactly the same point as the crystal's X2:4 (GND), an implicit short with no
// wire to show for it. Blocks that ship a schematic_layout template never come
// here; this is the floor for the 24 blocks that still lack one.
func bapGridSpacing(roles []string, b blocks.Block) float64 {
	maxHalf := float64(bapPartMargin)
	for _, role := range roles {
		if p, ok := b.Parts[role]; ok {
			if h := bapRoleHalfExtent(p.Part); h > maxHalf {
				maxHalf = h
			}
		}
	}
	return 2*maxHalf + float64(bapObstacleGap)
}

// bapRoleOffsets resolves every role to an offset: template roles use their
// authored geometry; roles the template misses (or all roles, when there is no
// template) fall back to the legacy grid — laid out BELOW the template extent so
// the two never interleave.
// halfOf gives each role's estimated half extent; nil means "uniform spacing"
// (an explicit --spacing). Per-part sizing matters: one IC in the block used to
// widen EVERY cell, so a 21-part block at the IC's 220 pitch ran to y=1400 on an
// 825-tall A4 sheet. Sizing each cell to its own part keeps the decoupling caps
// tight and only pays the wide pitch where a wide part actually sits.
// bapHalfExtentFn 是块内某个 role 的**半宽估算的唯一来源**:fallback 网格排布
// (bapRoleOffsets)与块 footprint 估算(bapBlockRect)必须共用它。
//
// 两套估算一旦分家就会「螺旋搜索说没撞、放完实测撞」——那正是 bapBlockRect 此前
// 的病:它统一按 bapPartMargin(50)外扩,不看 bapRoleHalfExtent(模组/MCU 半宽
// 250、IC 100、连接器 90),含 WROOM 的块每边低估 200,螺旋判定"空位"的地方放完
// 必然 overlap → 撞硬门 → 整单回滚(issue #180)。
//
// halfOf 为 nil(调用方显式给了 --spacing)时,所有件按 spacing/2 一刀切,这是
// 调用方的显式决定;有 halfOf 时逐件用自己的实测/查表半宽。
func bapHalfExtentFn(spacing float64, halfOf map[string]float64) func(role string) float64 {
	return func(role string) float64 {
		if halfOf == nil {
			return spacing / 2
		}
		if h, ok := halfOf[role]; ok {
			return h
		}
		return float64(bapPartMargin)
	}
}

func bapRoleOffsets(roles []string, layout *blocks.SchematicLayout, spacing float64, perRow int,
	halfOf map[string]float64) map[string]bapRoleOffset {

	out := make(map[string]bapRoleOffset, len(roles))
	tmplMaxDY := 0.0
	hasTmpl := false
	if layout != nil {
		for _, role := range roles {
			if h, ok := layout.Roles[role]; ok {
				out[role] = bapRoleOffset{dx: h.DX, dy: h.DY, rot: h.Rotation, source: "template"}
				hasTmpl = true
				if h.DY > tmplMaxDY {
					tmplMaxDY = h.DY
				}
			}
		}
	}
	gridBaseY := 0.0
	if hasTmpl {
		gridBaseY = tmplMaxDY + spacing
	}

	grid := make([]string, 0, len(roles))
	for _, role := range roles {
		if _, ok := out[role]; !ok {
			grid = append(grid, role)
		}
	}

	half := bapHalfExtentFn(spacing, halfOf)

	// Walk row by row, advancing x by the two neighbours' half extents plus a gap,
	// and dropping y by the tallest part in the row just finished.
	x, y := 0.0, gridBaseY
	rowMaxHalf, prevHalf := 0.0, 0.0
	for i, role := range grid {
		h := half(role)
		if i%perRow == 0 {
			if i > 0 {
				y += rowMaxHalf + h + float64(bapObstacleGap)
			}
			x, rowMaxHalf, prevHalf = 0, h, h
		} else {
			x += prevHalf + h + float64(bapObstacleGap)
			prevHalf = h
			if h > rowMaxHalf {
				rowMaxHalf = h
			}
		}
		out[role] = bapRoleOffset{dx: x, dy: y, source: "grid"}
	}
	return out
}

// bapBlockRect is the block's estimated footprint rectangle at a given origin.
// bapBlockRect 估算整块落地后占的矩形(螺旋搜索用它当刚体)。
//
// **负 dx/dy 一直是被正确计入的**(math.Min/Max 覆盖负值)——不要照 issue #180
// 正文最初那句「螺旋搜索不把负偏移计进 footprint」去"修"这里,那条归因是错的,
// 已在 issue 评论里订正:件出图纸的真机制是 bapResolveOrigin 从前给 findSlot 的
// inBounds/hitsTitle 传了 nil(边界谓词根本没接上)。
//
// half 是每个 role 的半宽(bapHalfExtentFn,与网格排布同源);传 nil 退回旧的
// 统一 bapPartMargin 外扩,只为老调用点兼容——新代码一律传 half。
func bapBlockRect(originX, originY float64, offsets map[string]bapRoleOffset, half func(string) float64) layoutBBox {
	first := true
	var minX, maxX, minY, maxY float64
	for role, o := range offsets {
		// **半宽与半高必须分开**:bapRoleHalfExtent 的文档口径是「half a symbol's
		// rendered WIDTH」——高引脚数芯片的 250 说的是两列引脚拉开的横向尺寸。
		// 拿它同时外扩纵向会让一个 MCU 顶出 500 高,实测使 4 个块在 A4 上
		// 「怎么摆都不可能 inBounds」(可用高只有 801),把边界约束变成死锁。
		// 半高没有实测表,沿用 bapPartMargin 这个通用下限;估算本来就只能给下限,
		// 真判据是放置后的实测 bbox(verifyBlockLayout)。
		hw := float64(bapPartMargin)
		if half != nil {
			hw = math.Max(half(role), bapPartMargin)
		}
		hh := float64(bapPartMargin)
		lo, hi := o.dx-hw, o.dx+hw
		bo, to := o.dy-hh, o.dy+hh
		if first {
			minX, maxX, minY, maxY = lo, hi, bo, to
			first = false
			continue
		}
		minX = math.Min(minX, lo)
		maxX = math.Max(maxX, hi)
		minY = math.Min(minY, bo)
		maxY = math.Max(maxY, to)
	}
	return layoutBBox{
		MinX: originX + minX, MinY: originY + minY,
		MaxX: originX + maxX, MaxY: originY + maxY,
	}
}

// bapResolveOrigin collision-checks the requested origin against the existing
// parts and, when the caller did not pin --at explicitly, spirals the block's
// footprint rectangle to the nearest free region (reusing autolayout's findSlot).
// It always returns a usable origin — on failure it keeps the request and says
// so in the warnings, because a placed-but-overlapping block is diagnosable by
// layout-lint while a refused apply loses all the work.
func bapResolveOrigin(in bapInput, offsets map[string]bapRoleOffset, half func(string) float64) (float64, float64, *bapOrigin, []string) {
	origin := &bapOrigin{
		RequestedX: in.OriginX, RequestedY: in.OriginY,
		X: in.OriginX, Y: in.OriginY,
	}
	// 间隙判定一律走 boxesGapOverlap:旧代码用 rectGap(欧氏角距,math.Hypot),
	// 对角方向名义留 20 实际只保证 ~14 单轴 —— 判据必须逐轴。
	collides := func(b layoutBBox) bool {
		if in.TitleBlock != nil && boxesGapOverlap(b, *in.TitleBlock, bapObstacleGap) {
			return true
		}
		for _, o := range in.Obstacles {
			if boxesGapOverlap(b, o, bapObstacleGap) {
				return true
			}
		}
		return false
	}
	// inBounds/hitsTitle 是 issue #180 三个坑的正解:此前它们给 findSlot 传的是
	// nil,图纸边界与分区标题带**从未参与搜索**,出界只在事后打 warning(而且比
	// 的是锚点不是 bbox)。接上之后,"搜出来的空位在图纸外"从构造上不可能。
	rect := bapBlockRect(in.OriginX, in.OriginY, offsets, half)
	var inBounds func(layoutBBox) bool
	var usable layoutBBox
	var oversize bool
	if in.Sheet != nil {
		usable = layoutBBox{
			MinX: in.Sheet.MinX + sheetEdgeMinGap, MinY: in.Sheet.MinY + sheetEdgeMinGap,
			MaxX: in.Sheet.MaxX - sheetEdgeMinGap, MaxY: in.Sheet.MaxY - sheetEdgeMinGap,
		}
		// **块比图纸还大时不许让 inBounds 连坐掉避让**:此时它恒 false,96 个候选
		// 全否决 → 回落原坐标 → 压在已有器件上 → wiring 前硬门整单回滚。也就是
		// 说「放不下」会被升级成「连躲都不躲」,比不加边界约束更差。
		// 正确行为:降级为只避器件,并单独说清楚为什么。
		bw, bh := bboxSize(rect)
		uw, uh := bboxSize(usable)
		if bw > uw || bh > uh {
			oversize = true
		} else {
			inBounds = func(b layoutBBox) bool { return boxInside(b, usable) }
		}
	}
	fits := !collides(rect) && (inBounds == nil || inBounds(rect))
	if fits {
		return in.OriginX, in.OriginY, origin, nil
	}
	if in.AtExplicit {
		msg := "--at 指定的原点与已有器件或标题栏图签重叠 — 按你的坐标照常放置(显式 --at 优先);放完请跑 `sch layout-lint` 确认"
		if inBounds != nil && !inBounds(rect) {
			msg = fmt.Sprintf("--at 指定的原点会让这一块超出图纸可用区(块估算范围 [%.0f,%.0f]-[%.0f,%.0f],图纸 [%.0f,%.0f]-[%.0f,%.0f] 内缩 %.0f)— 按你的坐标照常放置(显式 --at 优先);放完必须跑 `sch layout-lint --strict` 看 out-of-sheet",
				rect.MinX, rect.MinY, rect.MaxX, rect.MaxY,
				in.Sheet.MinX, in.Sheet.MinY, in.Sheet.MaxX, in.Sheet.MaxY, sheetEdgeMinGap)
		}
		return in.OriginX, in.OriginY, origin, []string{msg}
	}
	w, h := bboxSize(rect)
	step := math.Max(w, h)/2 + 2*bapObstacleGap
	cx, cy := bboxCenter(rect)
	// normalize:候选中心换算回 origin 后吸附 5 格再还原成 box —— 判定坐标必须
	// 等于落地坐标,否则吸附引入的 ±2.5 会让"判定说不撞、放下去撞"。
	normalize := func(cand layoutBBox) layoutBBox {
		ccx, ccy := bboxCenter(cand)
		nx := snapAnchor(in.OriginX + (ccx - cx))
		ny := snapAnchor(in.OriginY + (ccy - cy))
		return bapBlockRect(nx, ny, offsets, half)
	}
	slot, _, ok := findSlotNormalized(rect, cx, cy, step, true, normalize, collides, inBounds, nil, nil)
	if !ok && inBounds != nil {
		// 螺旋的步长 = max(w,h)/2 + 2*gap,**随块尺寸放大**;而合法原点窗口
		// = 可用区跨度 − 块跨度,**随块尺寸缩小**。两者反向,中等块就交叉:
		// 实测 ch340c(step 380)在 A4 上 96 个候选一个都落不进可用区,而 5 格
		// 暴力扫描能找到距请求原点仅 170 的合法位置。所以螺旋落空**不等于放不下**,
		// 补一次有界扫描把假阴性救回来 —— 否则回落原坐标压在别人身上,撞硬门整单回滚。
		if nx, ny, found := bapScanOrigin(in.OriginX, in.OriginY, offsets, half, usable, collides); found {
			origin.X, origin.Y = nx, ny
			origin.Relocated = true
			origin.Reason = "默认原点与已有器件重叠,螺旋步长对本块过粗,已按网格扫描移到最近的合法空位(显式传 --at 可固定原点)"
			return nx, ny, origin, nil
		}
	}
	if !ok {
		why := "与已有器件重叠"
		switch {
		case oversize:
			bw, bh := bboxSize(rect)
			uw, uh := bboxSize(usable)
			why = fmt.Sprintf("与已有器件重叠;另注:本块估算 %.0f×%.0f 大于图纸可用区 %.0f×%.0f,已只按器件避让、不判图纸边界(换大图纸/拆页/给块加 schematic_layout 模板)", bw, bh, uw, uh)
		case inBounds != nil:
			why = "与已有器件重叠或超出图纸可用区"
		}
		return in.OriginX, in.OriginY, origin, []string{
			fmt.Sprintf("默认原点%s,且螺旋搜索 %d 个候选 + 网格扫描后仍无空位 — 按原坐标放置,预期有 overlap/出图,放完必须跑 `sch layout-lint --strict`", why, alMaxRing*len(alDirsVertical)),
		}
	}
	ncx, ncy := bboxCenter(slot)
	nx := snapAnchor(in.OriginX + (ncx - cx))
	ny := snapAnchor(in.OriginY + (ncy - cy))
	origin.X, origin.Y = nx, ny
	origin.Relocated = true
	origin.Reason = "默认原点与已有器件重叠,已自动移到最近空位(显式传 --at 可固定原点)"
	return nx, ny, origin, nil
}

// planBlockApply turns a block + a parts library + the current page into a
// deterministic placement/wiring plan. It never touches the connector.
func planBlockApply(in bapInput) (bapPlan, error) {
	plan := bapPlan{
		BlockID:  in.Block.ID,
		Revision: in.Block.Revision,
		Instance: in.Instance,
	}
	// NOTE — the instance id is what keeps two instances of the same block apart,
	// because it names every PORT-less internal net. Deriving it from the BLOCK id
	// would hand both instances the same synthetic net names, and same-name nets
	// MERGE: instance 2's internal node would silently short to instance 1's. So an
	// unset instance is resolved AFTER designator allocation, from the first
	// allocated designator (LED1 → LED1_N2, LED2 → LED2_N2) — designators are
	// already collision-free against the page, so the net names inherit that.

	// Roles in a stable order so designators and coordinates are reproducible.
	roles := make([]string, 0, len(in.Block.Parts))
	for r := range in.Block.Parts {
		roles = append(roles, r)
	}
	sort.Strings(roles)

	// A block may bind a port the host never mentioned; validate --bind early so a
	// typo'd port name fails before anything is placed.
	for port := range in.Bind {
		if _, ok := in.Block.Ports[port]; !ok {
			return plan, fmt.Errorf("--bind %s=…: block %s has no port %q", port, in.Block.ID, port)
		}
	}

	// 1. Allocate designators + coordinates.
	used := map[string]bool{}
	for d := range in.Existing {
		used[strings.ToUpper(d)] = true
	}
	next := map[string]int{}
	roleDesig := map[string]string{}
	perRow := in.PerRow
	if perRow < 1 {
		perRow = 4
	}
	// An explicit --spacing means "uniform cells, caller's choice"; otherwise each
	// cell is sized to its own part (halfOf), so one wide IC no longer inflates the
	// whole grid off the sheet.
	spacing := in.Spacing
	var halfOf map[string]float64
	if spacing <= 0 {
		spacing = bapGridSpacing(roles, in.Block)
		halfOf = make(map[string]float64, len(roles))
		for _, role := range roles {
			if p, ok := in.Block.Parts[role]; ok {
				halfOf[role] = bapRoleHalfExtent(p.Part)
			}
		}
	}
	// Geometry: the block's schematic_layout template wins over the fallback
	// grid, and the whole block dodges existing parts when the caller left the
	// origin to us (the old blind 4-column grid at 400,300 was a top overlap
	// source — every second apply landed on the first).
	offsets := bapRoleOffsets(roles, in.Layout, spacing, perRow, halfOf)
	// **footprint 估算与网格间距是两件事**,不能共用一把尺:
	//   - 网格间距回答「调用方想要多宽」——显式 `--spacing` 时就该听调用方的;
	//   - footprint 回答「这块实际占多大」——物理事实,与 --spacing 无关。
	// 此前两者共用 bapHalfExtentFn:显式 `--spacing 220` 让 half=110,把模组
	// (真实半宽 250)的 footprint 每边低估 140,螺旋判"没撞"、放完实测撞、
	// 撞硬门整单回滚 —— 正是 Fix A 要根治的病在 --spacing 路径上原样复发。
	half := func(role string) float64 {
		if p, ok := in.Block.Parts[role]; ok {
			return math.Max(bapRoleHalfExtent(p.Part), bapPartMargin)
		}
		return float64(bapPartMargin)
	}
	originX, originY, origin, warns := bapResolveOrigin(in, offsets, half)
	plan.Origin = origin
	plan.Warnings = append(plan.Warnings, warns...)
	// 关系形态:标记出来并选定锚件。规划阶段所有件仍拿网格坐标(它们是**兜底**),
	// 真正的位姿在 runBlockApply 里放完锚件、回读实测引脚后由求解器算 —— attach
	// 需要目标引脚的真实坐标,而那只有把宿主放下去才知道。
	if rel, ok := bslRelationsFrom(in.Layout); ok {
		nets := bslBlockNets(in.Block)
		anchor, aerr := bslAnchorRole(in.Block, rel, nets)
		if aerr != nil {
			return plan, aerr
		}
		plan.Relational = true
		plan.AnchorRole = anchor
	}
	for _, role := range roles {
		p := in.Block.Parts[role]
		if p.Qty != 1 {
			// qty>1 would need one designator per instance; the minimal slice does
			// not do it, and silently placing one part would under-build the block.
			return plan, fmt.Errorf("role %s: qty=%d not supported yet (block-apply v1 places qty=1 roles only)", role, p.Qty)
		}
		dev, ok := in.Devices[p.Part]
		if !ok {
			return plan, fmt.Errorf("role %s: part %q not found in standard-parts.json", role, p.Part)
		}
		if dev.DeviceUUID == "" {
			return plan, fmt.Errorf("role %s: part %q has no deviceUuid in standard-parts.json", role, p.Part)
		}
		prefix, err := bapPrefixFor(p.Part)
		if err != nil {
			return plan, fmt.Errorf("role %s: %w", role, err)
		}
		d := bapNextDesignator(prefix, used, next)
		roleDesig[role] = d
		off := offsets[role]
		plan.Placements = append(plan.Placements, bapPlacement{
			Role:        role,
			PartKey:     p.Part,
			Designator:  d,
			LibraryUUID: dev.LibraryUUID,
			DeviceUUID:  dev.DeviceUUID,
			LCSC:        dev.LCSC,
			X:           snapAnchor(originX + off.dx),
			Y:           snapAnchor(originY + off.dy),
			Rotation:    off.rot,
			Source:      off.source,
		})
	}

	if strings.TrimSpace(in.Instance) == "" {
		if len(plan.Placements) == 0 {
			return plan, fmt.Errorf("block %s has no parts", in.Block.ID)
		}
		in.Instance = plan.Placements[0].Designator
	}
	plan.Instance = in.Instance

	// 2. Resolve internal_nets → named nets over placed pins.
	for i, net := range in.Topology {
		var (
			members []string
			roleRef []string
			port    string
		)
		for _, m := range net {
			if strings.HasPrefix(m, "PORT:") {
				name := strings.TrimPrefix(m, "PORT:")
				if port == "" {
					port = name
				}
				continue
			}
			role, pin, ok := strings.Cut(m, ".")
			if !ok {
				return plan, fmt.Errorf("internal_nets[%d]: bad pin ref %q", i, m)
			}
			d, ok := roleDesig[role]
			if !ok {
				return plan, fmt.Errorf("internal_nets[%d]: unknown role %q", i, role)
			}
			members = append(members, d+":"+pin)
			roleRef = append(roleRef, m)
		}
		if len(members) == 0 {
			// A net of nothing but PORT markers has no pin to attach a flag to.
			return plan, fmt.Errorf("internal_nets[%d]: no pin members", i)
		}

		name, bound := bapNetName(in, port, i)
		kind := bapFlagKind(name)
		if k, ok := in.KindOver[strings.ToUpper(name)]; ok {
			kind = k
		}
		plan.Nets = append(plan.Nets, bapNet{
			Net: name, Kind: kind, Port: port, Bound: bound,
			Members: members, Roles: roleRef,
		})
	}

	plan.Unconsumed = bapUnconsumed(in.Block)
	return plan, nil
}

// bapNetName picks a net's name: an explicit --bind wins, then the port's
// default_net, then the port name; a purely internal net gets an
// instance-scoped synthetic name that cannot collide with a host net.
func bapNetName(in bapInput, port string, idx int) (string, bool) {
	if port != "" {
		if host, ok := in.Bind[port]; ok && strings.TrimSpace(host) != "" {
			return host, true
		}
		p := in.Block.Ports[port]
		if p.DefaultNet != "" {
			return p.DefaultNet, false
		}
		return port, false
	}
	return fmt.Sprintf("%s_N%d", strings.ToUpper(in.Instance), idx+1), false
}

// bapNextDesignator returns the next free designator for a prefix, skipping any
// already on the page so a second instance never collides with the first.
func bapNextDesignator(prefix string, used map[string]bool, next map[string]int) string {
	for {
		next[prefix]++
		d := prefix + strconv.Itoa(next[prefix])
		if !used[strings.ToUpper(d)] {
			used[strings.ToUpper(d)] = true
			return d
		}
	}
}

// bapRemapDesignators rewrites a plan in place after EasyEDA assigned designators
// different from the planned ones (issue #144).
//
// The platform re-numbers on create to dodge designators it already knows about —
// INCLUDING ones on schematic pages we cannot see. `sch_PrimitiveComponent.getAll(_,
// allPages)` only returns pages that are LOADED, so a never-visited page is invisible
// to the pre-flight scan yet still steers the platform's numbering: planning C1 and
// landing C11 is NORMAL, not an error. What is fatal is leaving the plan's downstream
// references on the planned name — wiring then resolves "C1:VCC" against whatever C1
// exists elsewhere in the document (the netlist is keyed by designator.pin
// document-wide), silently connecting another page's part.
//
// renames maps PLANNED (upper-cased) → ACTUAL. It rewrites:
//   - each placement's designator
//   - every net member's "DESIGNATOR:PIN" reference
//   - the instance id and the "<INSTANCE>_N<i>" internal net names derived from it,
//     so two instances never share an internal net name (a same-named internal net
//     on another page would MERGE with this one)
//
// Port-bound nets carry HOST net names and are never rewritten.
func bapRemapDesignators(plan *bapPlan, renames map[string]string) {
	if plan == nil || len(renames) == 0 {
		return
	}
	lookup := func(d string) (string, bool) {
		n, ok := renames[strings.ToUpper(strings.TrimSpace(d))]
		return n, ok
	}

	for i := range plan.Placements {
		if n, ok := lookup(plan.Placements[i].Designator); ok {
			plan.Placements[i].Designator = n
		}
	}

	// The instance id defaults to the first placement's designator, so it follows
	// that rename; an explicit --instance matches no designator and is left alone.
	oldPrefix := strings.ToUpper(plan.Instance) + "_N"
	if n, ok := lookup(plan.Instance); ok {
		plan.Instance = n
	}
	newPrefix := strings.ToUpper(plan.Instance) + "_N"

	for i := range plan.Nets {
		for j, m := range plan.Nets[i].Members {
			desig, pin, ok := strings.Cut(m, ":")
			if !ok {
				continue
			}
			if n, ok := lookup(desig); ok {
				plan.Nets[i].Members[j] = n + ":" + pin
			}
		}
		if plan.Nets[i].Port == "" && oldPrefix != newPrefix &&
			strings.HasPrefix(plan.Nets[i].Net, oldPrefix) {
			plan.Nets[i].Net = newPrefix + strings.TrimPrefix(plan.Nets[i].Net, oldPrefix)
		}
	}
}

// ── post-apply reconciliation (issue #135) ──────────────────────────────────
//
// Per-stub autoconnect success is NOT proof the block's topology landed:
// EasyEDA auto-merges touching wires, so a stub can silently fuse into a
// foreign net AFTER it was "successfully" created — and both check and
// bridge-check have historically missed the swallowed-flag shape. The netlist
// is the only authority on what actually connected, so block-apply now closes
// the loop: read it back and diff every planned net against reality.

// bapNetDiff is one planned net whose live membership does not match the plan.
type bapNetDiff struct {
	Net     string            `json:"net"`
	Missing []string          `json:"missing"`           // planned members absent from the live net (DESIGNATOR.PIN)
	FoundIn map[string]string `json:"foundIn,omitempty"` // missing member → the net it actually landed in
}

// bapPinKey resolves a planned member "DESIGNATOR:PINREF" (name or number) to
// the netlist's "DESIGNATOR.NUMBER" key via the designator→(name|number)→number
// map from the live component read.
// A "NAME*" member fans out to EVERY pin carrying that name, so it resolves to
// SEVERAL netlist keys and reconciliation must find all of them — a `GND*` that
// landed on only half the ground pins is exactly the silent half-connection this
// whole mechanism exists to catch.
func bapPinKeys(member string, pinNumbers map[string]map[string][]string) ([]string, bool) {
	desig, ref, ok := strings.Cut(member, ":")
	if !ok {
		return nil, false
	}
	byRef := pinNumbers[strings.ToUpper(desig)]
	if byRef == nil {
		return nil, false
	}
	nums, ok := byRef[strings.ToUpper(strings.TrimSuffix(ref, acPinFanoutSuffix))]
	if !ok || len(nums) == 0 {
		return nil, false
	}
	if !strings.HasSuffix(ref, acPinFanoutSuffix) {
		// A plain ref names one pin; if the symbol has several with that name the
		// planner never resolved it either, so take the first deterministically.
		return []string{desig + "." + nums[0]}, true
	}
	keys := make([]string, 0, len(nums))
	for _, n := range nums {
		keys = append(keys, desig+"."+n)
	}
	return keys, true
}

// reconcileBlockNets diffs the plan against the live netlist. liveNets maps net
// name → set of "DESIGNATOR.NUMBER"; pinNumbers maps DESIGNATOR → pin name/number
// → number. A planned net passes when every planned member is present in that
// live net — the live net MAY hold extra pins (bound host nets legitimately
// aggregate the rest of the board).
func reconcileBlockNets(plan bapPlan, liveNets map[string]map[string]bool, pinNumbers map[string]map[string][]string) []bapNetDiff {
	var diffs []bapNetDiff
	for _, n := range plan.Nets {
		live := liveNets[n.Net]
		var missing []string
		foundIn := map[string]string{}
		for _, m := range n.Members {
			keys, ok := bapPinKeys(m, pinNumbers)
			if !ok {
				// Pin ref did not resolve — count as missing so it can't hide.
				missing = append(missing, m)
				continue
			}
			for _, key := range keys {
				if live != nil && live[key] {
					continue
				}
				missing = append(missing, key)
				for otherNet, pins := range liveNets {
					if otherNet != n.Net && pins[key] {
						foundIn[key] = otherNet
						break
					}
				}
			}
		}
		if len(missing) > 0 {
			d := bapNetDiff{Net: n.Net, Missing: missing}
			if len(foundIn) > 0 {
				d.FoundIn = foundIn
			}
			diffs = append(diffs, d)
		}
	}
	return diffs
}

// bapUnconsumed lists the constraint maps present in the block that this command
// does not execute. Honesty surface: `must` constraints that no consumer honours
// must not hide behind a green exit code.
func bapUnconsumed(b blocks.Block) []string {
	var raw map[string]any
	if err := json.Unmarshal(b.Raw, &raw); err != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"pcb_layout", "placement", "signals", "silk", "keepout", "openings"} {
		if v, ok := raw[k]; ok && v != nil {
			out = append(out, k)
		}
	}
	// 关系形态的 schematic_layout(flow/attach/pair)**本版尚未执行** —— 数据模型
	// 与校验先落地(issue #180 P1),求解器是 P2。块库因此可以先收关系数据而不必
	// 等求解器,但一落库 agent 就会当真,所以必须在 manifest 里明说"声明了没执行"
	// (本项目的诚实性铁律:命令绝不能让人以为做到了没做到的事)。
	if layout, err := b.SchematicLayout(); err == nil && layout.IsRelational() {
		// 单返回值类型断言在失败时会 panic —— 用双返回值形式,块数据是外部输入。
		if sl, ok := raw["schematic_layout"].(map[string]any); ok {
			for _, k := range []string{"flow", "attach", "pair"} {
				if v, ok := sl[k]; ok && v != nil {
					out = append(out, "schematic_layout."+k)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// bapScanOrigin 是螺旋搜索的**兜底**:在图纸可用区内按连接网格枚举原点,取第一个
// 「块完全在可用区内 且 与任何已有器件保持 bapObstacleGap」的位置,按到请求原点的
// 距离取最近。
//
// 为什么需要它:findSlot 的候选是 8 条射线 × 12 环 = 96 个点,步长
// max(w,h)/2 + 2*gap **随块尺寸放大**,而合法原点窗口 = 可用区跨度 − 块跨度
// **随块尺寸缩小**——两者反向,中等块(ch340c 680×310,step 380)在 A4 上就已经
// 一个候选都落不进可用区,而合法位置其实距请求原点只有 170。没有这层兜底,
// 「螺旋落空」会被当成「放不下」回落原坐标,压在已有器件上撞 wiring 前硬门,
// 把 apply 从「搬个位置」变成「整单回滚」。
//
// 判定坐标 = 落地坐标:枚举的就是吸附后的 origin,rect 由同一个 bapBlockRect 算,
// 不存在「判完再吸附」的半格漂移。纯函数。
func bapScanOrigin(reqX, reqY float64, offsets map[string]bapRoleOffset, half func(string) float64,
	usable layoutBBox, collides func(layoutBBox) bool) (float64, float64, bool) {

	// rect 相对 origin 的偏移(与 origin 无关,算一次即可)。
	rel := bapBlockRect(0, 0, offsets, half)
	// origin 的合法区间:让 rect 完全落进 usable。
	loX, hiX := usable.MinX-rel.MinX, usable.MaxX-rel.MaxX
	loY, hiY := usable.MinY-rel.MinY, usable.MaxY-rel.MaxY
	if loX > hiX || loY > hiY {
		return 0, 0, false // 块比可用区还大,调用方已按 oversize 分支处理
	}
	best, bestX, bestY := math.Inf(1), 0.0, 0.0
	for y := snapAnchor(loY); y <= hiY; y += bapScanStep {
		for x := snapAnchor(loX); x <= hiX; x += bapScanStep {
			r := bapBlockRect(x, y, offsets, half)
			if !boxInside(r, usable) || collides(r) {
				continue
			}
			if d := math.Hypot(x-reqX, y-reqY); d < best {
				best, bestX, bestY = d, x, y
			}
		}
	}
	if math.IsInf(best, 1) {
		return 0, 0, false
	}
	return bestX, bestY, true
}

// bapScanStep 是兜底扫描的步长:4 个连接网格。取 20 而不是 5 是纯算力权衡
// (A4 可用区 1146×801 → 约 2300 个候选,离线毫秒级);落点仍在 5 格网格上,
// 因为 20 是 schAnchorGrid 的整数倍。
const bapScanStep = 4 * schAnchorGrid
