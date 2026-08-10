package app

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
)

// pcb_place_constrained.go — constraint-driven TIERED placement (daemon-side).
//
// The fix for whack-a-mole layout: place POSITION-CONSTRAINED parts first and
// LOCK them, then legalize the rest around the locked set — so a satellite pass
// can never push an edge connector off its edge. Tiers (highest priority first):
//
//	Tier 1  mounting holes            — passed in as obstacles (placed via `pcb slot`), never moved.
//	Tier 2  edge-constrained parts    — connectors (USB / terminal / card socket / IPEX) + RF
//	                                     modules → snapped flush to their NEAREST board edge, fixed.
//	Tier 3  main chips + crystals     — anchors, kept where they are (fixed).
//	        + block-anchored parts    — a part the block pinned to a deliberate
//	                                     non-edge spot (e.g. a bus-terminal jumper).
//	Tier 4  satellites + user-facing  — legalized (spiral) around the fixed set, avoiding holes.
//
// Classification CONSUMES the circuit-block library's declarative placement hints
// (internal/blocks/data/*.json → placement.<REF>.{board_edge,edge,...}): a placed
// part is reverse-mapped to its block role by its distinctive designator prefix,
// and only falls back to the hardcoded footprint/designator regex when no block
// role matches. The block data is the single source-of-truth (see the
// improvements-sink-to-blocks rule) — the regex merely mirrors it as a safety net.
// A board that was block-assembled or built from the schematic both work: we read
// what's placed, not how it got there (placed parts carry no block link, hence the
// device-name / prefix reverse lookup).

type cpClass int

const (
	cpSatellite  cpClass = iota // tier 4 (default)
	cpUserFacing                // tier 4, but wants to stay near an edge / visible (LED, button)
	cpMainChip                  // tier 3
	cpEdgeMust                  // tier 2 — MUST sit at a board edge (connector / module / IPEX)
	cpAnchored                  // tier 3 — block declares a DELIBERATE non-edge position
	// (e.g. a bus-terminal jumper next to its resistor); keep it put, don't spiral.
)

func (k cpClass) String() string {
	switch k {
	case cpEdgeMust:
		return "edge"
	case cpMainChip:
		return "main"
	case cpAnchored:
		return "anchored"
	case cpUserFacing:
		return "user-facing"
	default:
		return "satellite"
	}
}

// cpPlacementIndex lazily loads (and caches) the block library's placement hints
// so classifyCP can consult the declarative source-of-truth before falling back
// to the hardcoded regex below. The block data is go:embed'd, so this never
// touches disk. Built once; the index is read-only after.
var (
	cpIdxOnce sync.Once
	cpIdx     blocks.PlacementIndex
)

func placementIndex() blocks.PlacementIndex {
	cpIdxOnce.Do(func() {
		// Errors leave cpIdx zero-valued (empty maps → classifyCP just uses the
		// regex fallback), so a broken block library degrades, never panics.
		if idx, err := blocks.LoadPlacementIndex(); err == nil {
			cpIdx = idx
		}
	})
	return cpIdx
}

// cpOpenOnce lazily loads the block library's connector-opening declarations
// (which local side a connector's opening faces), so Tier-2 can orient a symmetric
// terminal whose opening isn't in the pad geometry. Built once; read-only after.
var (
	cpOpenOnce sync.Once
	cpOpen     []blocks.ConnectorOpening
)

func connectorOpenings() []blocks.ConnectorOpening {
	cpOpenOnce.Do(func() {
		if o, err := blocks.LoadConnectorOpenings(); err == nil {
			cpOpen = o
		}
	})
	return cpOpen
}

// openingVec parses a local opening string ("+x"/"-x"/"+y"/"-y") into a unit vector.
func openingVec(local string) (float64, float64, bool) {
	switch strings.ToLower(strings.TrimSpace(local)) {
	case "+x", "x":
		return 1, 0, true
	case "-x":
		return -1, 0, true
	case "+y", "y":
		return 0, 1, true
	case "-y":
		return 0, -1, true
	}
	return 0, 0, false
}

// connOpeningFor returns the block-declared LOCAL opening vector for a device (by
// manufacturerId substring), if any block declares one.
func connOpeningFor(device string) (float64, float64, bool) {
	d := strings.ToLower(strings.TrimSpace(device))
	if d == "" {
		return 0, 0, false
	}
	for _, o := range connectorOpenings() {
		if strings.Contains(d, strings.ToLower(o.Match)) {
			if vx, vy, ok := openingVec(o.Local); ok {
				return vx, vy, true
			}
		}
	}
	return 0, 0, false
}

// rotate2d rotates (x,y) by deg CCW — the EasyEDA component-rotation convention
// (calibrated on a real KF301: its local -y opening points +x at rotation 90).
func rotate2d(x, y, deg float64) (float64, float64) {
	r := deg * math.Pi / 180
	c, s := math.Cos(r), math.Sin(r)
	return x*c - y*s, x*s + y*c
}

// openingTargetDelta returns the rotation delta ∈ {0,90,180,270} to ADD to curRot
// so the connector's local opening ends up facing OFF the assigned board edge.
func openingTargetDelta(curRot, lox, loy float64, edge apEdge) float64 {
	ix, iy := edgeInteriorDir(edge)
	obx, oby := -ix, -iy // off-board = away from the interior
	bestDelta, bestDot := 0.0, math.Inf(-1)
	for _, d := range []float64{0, 90, 180, 270} {
		ax, ay := rotate2d(lox, loy, curRot+d)
		if dot := ax*obx + ay*oby; dot > bestDot {
			bestDot, bestDelta = dot, d
		}
	}
	return bestDelta
}

// classFromHint maps a block placement hint to a placement tier. It decides ONLY
// on an EXPLICIT signal; an advisory hint (a side/orientation note with no
// board_edge / user-facing / anchor) returns ok=false so classifyCP falls through
// to the regex/pin-count heuristic — an ordinary decoupling cap that merely
// carries a "side: top" note must NOT be frozen in place.
//
//   - board_edge=true                  → edge-must (tier 2), snapped to an edge.
//   - edge="user-facing" (not edge)    → tier 4 user-facing (LED / button).
//   - anchor=true (deliberate non-edge)→ anchored (tier 3): the block pinned it to
//     a specific spot beside another part (e.g. JP701 by its 120R terminator), so
//     keep it put rather than spiral it to a board corner.
//   - otherwise                        → (_, false): advisory only, no tier forced.
//
// The hint only ever promotes a part to a stronger placement role; it never
// demotes a main chip (which the pin-count fallback still catches).
func classFromHint(h blocks.PlacementHint) (cpClass, bool) {
	switch {
	case h.BoardEdge:
		return cpEdgeMust, true
	case strings.EqualFold(strings.TrimSpace(h.Edge), "user-facing"):
		return cpUserFacing, true
	case h.Anchor:
		return cpAnchored, true
	default:
		return 0, false
	}
}

// Footprint / designator patterns for the position-constrained categories. These
// only MIRROR the block library's placement hints — they are the FALLBACK, used
// when classifyCP can't reverse-map a placed part to a block role by its
// designator prefix (see placementIndex). The block data is the source-of-truth.
var (
	cpReEdgeConn = regexp.MustCompile(`(?i)usb|type-?c|micro-?sd|tf[-_ ]?card|sd[-_]?card|push-?push|ipex|u\.?fl|sma|ufl|kf301|kf128|kf2edg|terminal|screw|hdr|header|pin-?header|conn`)
	cpReModule   = regexp.MustCompile(`(?i)wroom|wrover|esp32.*(module|wifi|smd)`)
	cpReSwitch   = regexp.MustCompile(`(?i)tact|switch|\bkey\b|button|sw-?smd`)
	cpReLED      = regexp.MustCompile(`(?i)\bled\b`)
	cpReCrystal  = regexp.MustCompile(`(?i)xtal|crystal|osc|3225|3215|2016|2520|smd-?4p`)
	// cpReAnyEdge mirrors the block edge="any" role (RF antenna / radio module —
	// must sit at *an* edge but any works, so it is NOT grouped with user-facing
	// I/O). Checked before cpReEdgeConn so an IPEX/U.FL antenna reads as "any", not
	// a user-facing connector.
	cpReAnyEdge = regexp.MustCompile(`(?i)wroom|wrover|esp32.*(module|wifi|smd)|ipex|u\.?fl|\bsma\b|\bufl\b|antenna|\bant\b`)
)

// edgeRoleOf returns the block `edge` semantic for an edge-must part —
// "user-facing" (USB / SD / screw-terminal / header the user plugs into → group
// on one accessible edge), "any" (RF antenna / module — any edge is fine → keep
// nearest), or "" (unknown → treated as any). It consumes the block placement
// hint (data/*.json placement.<ref>.edge, the source-of-truth) via the
// DISTINCTIVE designator prefix, and falls back to the device-name regex — the
// documented mirror of that data — for the generic-prefix connectors (J*, U*)
// the prefix index deliberately can't reach (see blocks.PlacementIndex).
func edgeRoleOf(c cpComp) string {
	des := strings.ToUpper(strings.TrimSpace(c.designator))
	if h, ok := placementIndex().ByRefPrefix[refPrefixCP(des)]; ok {
		switch strings.ToLower(strings.TrimSpace(h.Edge)) {
		case "user-facing":
			return "user-facing"
		case "any":
			return "any"
		}
	}
	fp := c.footprint
	if cpReAnyEdge.MatchString(fp) {
		return "any"
	}
	if cpReEdgeConn.MatchString(fp) || (strings.HasPrefix(des, "J") && !strings.HasPrefix(des, "JP")) {
		return "user-facing"
	}
	return ""
}

// cpComp carries a placed component's device NAME (PCB components expose no
// footprint-name string, so we pattern-match the device name — e.g.
// "esp32-s3-wroom-1u-n8" — for module detection; connectors are caught by the
// J* designator prefix anyway) + copper layer (TOP=1 / BOTTOM=2).
type cpComp struct {
	apComp
	footprint string // device name, used for module/connector pattern matching
	layer     int
}

// classify decides the placement tier from footprint + designator + pin count.
//
// Block-data FIRST: the declarative placement hints in internal/blocks/data/*.json
// are the source-of-truth (see improvements-sink-to-blocks). We reverse-map the
// placed part to a block role by its DISTINCTIVE designator prefix (a placed part
// carries no block link, so we can only match on what it exposes; device-level
// precision is a future layer — see blocks.PlacementIndex). Only when the prefix
// yields no explicit hint do we fall through to the hardcoded regex heuristic
// below — that regex is the fallback, not the primary path.
func classifyCP(c cpComp, mainPins int) cpClass {
	fp := c.footprint
	des := strings.ToUpper(c.designator)

	// Block data FIRST: reverse-map the placed part to a block role by DISTINCTIVE
	// designator prefix (JP/SW/LED/ANT…). The prefix is itself block-declared, so
	// this consumes the source-of-truth (per improvements-sink-to-blocks). Only an
	// EXPLICIT hint (board_edge/user-facing/anchor) decides a tier; an advisory
	// hint falls through to the regex heuristic below.
	idx := placementIndex()
	if h, ok := idx.ByRefPrefix[refPrefixCP(des)]; ok {
		if cls, decided := classFromHint(h); decided {
			return cls
		}
	}
	// 位号优先于器件名（与 pcb check 的连接器判定同口径，2f45269 的学费在这边
	// 没同步过来）：USBLC6("usb")/SMAJ("sma") 这类保护件的**器件名**撞连接器
	// 关键词，曾被下面的正则钉到板边 —— 离被保护端口 1000+mil，真板 protection
	// 40→7 的直接根因。位号已表明不是连接器的件（C/R/L/D/Q/F/TVS/ESD…前缀），
	// 绝不走 edge/user-facing 的器件名正则；保护件落卫星档后由 cpNeedsHugging
	// 贴到它保护的端口旁。显式块 hint（上面）不受此限 —— 块数据是真源。
	vetoedByDesignator := nonConnectorDesRe.MatchString(des)

	// A connector/module footprint, OR a Jxx designator that isn't a plain header
	// resistor — treat as edge-must.
	if !vetoedByDesignator && cpReModule.MatchString(fp) {
		return cpEdgeMust
	}
	// Jxx connectors are edge-must, but NOT JPxx (a jumper/link — belongs by its net,
	// not at a board edge). A J-prefix with a connector-ish footprint also qualifies.
	if (!vetoedByDesignator && cpReEdgeConn.MatchString(fp)) || (strings.HasPrefix(des, "J") && !strings.HasPrefix(des, "JP")) {
		return cpEdgeMust
	}
	if (!vetoedByDesignator && cpReSwitch.MatchString(fp)) || strings.HasPrefix(des, "SW") {
		return cpUserFacing
	}
	if (!vetoedByDesignator && cpReLED.MatchString(fp)) || strings.HasPrefix(des, "LED") {
		return cpUserFacing
	}
	// Main chip by distinct-pin count (crystals are few-pin but anchor near their IC
	// — fold them into main so they stay put in tier 3).
	if c.distinctPins() >= mainPins || cpReCrystal.MatchString(fp) {
		return cpMainChip
	}
	return cpSatellite
}

// refPrefixCP returns the leading alphabetic run of a designator, upper-cased
// ("JP701" → "JP", "J4" → "J"). Mirrors blocks.refPrefix so the app-side lookup
// key matches the index key.
func refPrefixCP(des string) string {
	des = strings.ToUpper(strings.TrimSpace(des))
	i := 0
	for i < len(des) && des[i] >= 'A' && des[i] <= 'Z' {
		i++
	}
	return des[:i]
}

// edgeInteriorDir is the unit vector pointing from a board edge toward the board
// interior. An edge connector should sit with its PADS on the interior side (traces
// route inward) and its OPENING facing out — so we orient it to maximize the
// pad-centroid projection along this direction.
func edgeInteriorDir(e apEdge) (float64, float64) {
	switch e {
	case edgeLeft:
		return 1, 0
	case edgeRight:
		return -1, 0
	case edgeBottom:
		return 0, 1
	default: // edgeTop
		return 0, -1
	}
}

// connGeom returns, for component c rotated by deltaDeg about its anchor, the pad
// centroid and rotated bbox (all absolute). Used to pick the orientation whose
// opening faces off-board.
func connGeom(c cpComp, deltaDeg float64) (pcx, pcy, bx0, by0, bx1, by1 float64) {
	var sx, sy float64
	n := 0
	for _, p := range c.pads {
		rx, ry := rotateVec(p.x-c.x, p.y-c.y, deltaDeg)
		sx, sy = sx+c.x+rx, sy+c.y+ry
		n++
	}
	if n > 0 {
		pcx, pcy = sx/float64(n), sy/float64(n)
	} else {
		pcx, pcy = c.x, c.y
	}
	bx0, by0 = math.Inf(1), math.Inf(1)
	bx1, by1 = math.Inf(-1), math.Inf(-1)
	for _, cn := range [4][2]float64{{c.minX, c.minY}, {c.maxX, c.minY}, {c.maxX, c.maxY}, {c.minX, c.maxY}} {
		rx, ry := rotateVec(cn[0]-c.x, cn[1]-c.y, deltaDeg)
		ax, ay := c.x+rx, c.y+ry
		bx0, by0 = math.Min(bx0, ax), math.Min(by0, ay)
		bx1, by1 = math.Max(bx1, ax), math.Max(by1, ay)
	}
	return
}

// bestConnDelta picks the rotation (0/90/180/270 relative to current) whose opening
// faces off the assigned edge — i.e. maximizes the pad-centroid projection toward
// the board interior — returning the delta and that projection score.
func bestConnDelta(c cpComp, edge apEdge) (delta, score float64) {
	ix, iy := edgeInteriorDir(edge)
	score = math.Inf(-1)
	for _, dd := range []float64{0, 90, 180, 270} {
		pcx, pcy, gx0, gy0, gx1, gy1 := connGeom(c, dd)
		s := (pcx-(gx0+gx1)/2)*ix + (pcy-(gy0+gy1)/2)*iy
		if s > score {
			score, delta = s, dd
		}
	}
	return
}

type cpHole struct{ x, y, r float64 }

// cpZoneClaim binds a designator to its functional zone's board sub-rect
// (issue #126: S0 modules[].zone made executable at P2).
type cpZoneClaim struct {
	rect   cpRect
	module string
	zone   string
}

type cpOptions struct {
	// zones maps UPPER-case designator → zone claim. Mains/satellites with a
	// claim are placed INTO the rect; edge-must parts are exempt (the board
	// edge is a harder constraint than the zone — an interface connector's
	// zone is advisory only).
	zones      map[string]cpZoneClaim
	mainPins   int
	edgeMargin float64 // gap between an edge part's bbox and the board edge
	partGap    float64 // clearance between any two parts / part-to-hole
	board      *cpRect // REAL board-outline bbox; nil → fall back to the part-cloud union extent
}

// cpFollowRadius 是「原本贴着这个端子」的判据半径（mil）。
//
// 取 400mil ≈ 10mm：protection 维给保护件贴端子的预算是 250mil、去耦贴 IC 是
// 100mil，400 比两者都宽 —— 宁可多跟一件（相对关系本来就该保持），也不要漏掉一个
// 真正贴着端子的保护件。**待校准初值**：真板上若出现「跟着跑了但其实不相干」的件，
// 收窄它；判读看 diag 里的 satellite:follows-edge-part 条目。
const cpFollowRadius = 400.0

// cpFollowRadiusGlobal 是「只能靠全局网(VCC/GND)判亲缘」时的跟随半径。
//
// 收到 150mil ≈ 3.8mm：纯去耦电容与它服务的 IC 引脚本来就该在 100mil 内
// （protection 维的去耦预算），150 留一点余量。放宽到全局网是因为纯去耦没有任何
// 本地网可认，但 GND 连着板上每一个件 —— 半径不收紧就会变成「谁动了都跟」。
const cpFollowRadiusGlobal = 150.0

// cpSpiralMaxRad 是全板合法化的螺旋搜索上限（mil）—— 原先写死在 spiralIn 里。
const cpSpiralMaxRad = 2200.0

// cpNeedsHugging 判断一个卫星是否**真有贴脚约束** —— 只有这类件值得「跟着端子走」。
//
// 判据刻意与 protection 维同口径（那一维只对这两类扣分）：
//   - 保护件：保险丝 / TVS / ESD / 压敏，它们必须守在入口处；
//   - 去耦电容：恰好 2 脚、一脚地一脚非地电源网 —— 与 findDecapTooFar 的筛选一致。
//
// 为什么要收窄：跟随一开始对**所有**卫星生效，真板实测把移动件数从 19 推到 46，
// 而 protection 一分没涨 —— 被拖着跑的大多是没有贴脚约束的普通电阻电容，
// 挪它们既无收益又平白扰动一块人工调过的板。稀疏板上看不出来（件少、都跟得上），
// 密板上就是纯粹的噪声。
func cpNeedsHugging(c cpComp) bool {
	des := strings.ToUpper(strings.TrimSpace(c.designator))
	dev := strings.ToLower(c.footprint)
	if cpReProtection.MatchString(des) || cpReProtectionDev.MatchString(dev) {
		return true
	}
	// 去耦：2 脚电容，一脚 GND、另一脚是非 GND 的全局(电源)网。
	if !strings.HasPrefix(des, "C") {
		return false
	}
	nets := map[string]bool{}
	for _, p := range c.pads {
		if n := strings.TrimSpace(p.net); n != "" {
			nets[n] = true
		}
	}
	if len(nets) != 2 {
		return false
	}
	var hasGnd, hasPwr bool
	for n := range nets {
		switch {
		case isGndNetName(n):
			hasGnd = true
		case isGlobalNet(n):
			hasPwr = true
		}
	}
	return hasGnd && hasPwr
}

var (
	// 位号前缀：F=保险丝 D=二极管(含 TVS/ESD) RV/VR=压敏 TVS/ESD=显式命名
	cpReProtection    = regexp.MustCompile(`(?i)^(?:F|D|RV|VR|TVS|ESD)[\d_]`)
	cpReProtectionDev = regexp.MustCompile(`(?i)tvs|esd|smbj|smaj|pesd|usblc|fuse|pptc|\bptc\b|保险丝|压敏`)
)

func defaultCpOptions() cpOptions {
	return cpOptions{mainPins: 8, edgeMargin: 45, partGap: 14}
}

type cpRect struct{ x0, y0, x1, y1 float64 }

func (r cpRect) overlaps(o cpRect) bool {
	return !(r.x1 <= o.x0 || o.x1 <= r.x0 || r.y1 <= o.y0 || o.y1 <= r.y0)
}

// planConstrainedPlace runs the tiered placement over a snapshot of components +
// mounting holes, returning the anchor moves. Pure: no I/O, so it unit-tests.
func planConstrainedPlace(comps []cpComp, holes []cpHole, opt cpOptions) ([]apMove, []apDiag) {
	var moves []apMove
	var diags []apDiag
	if len(comps) == 0 {
		return moves, diags
	}
	// Board rect. Prefer the REAL board outline when the caller supplies it; the
	// part-cloud union below is only the fallback. Using the part cloud as "the
	// board" is wrong whenever the outline differs from the placed extent — the
	// topmost part would otherwise define its own "top edge", so an edge connector
	// snaps to the part pile instead of the actual board edge (the flaky U1 WROOM
	// edge:top on ceshi was exactly this).
	bx0, by0 := math.Inf(1), math.Inf(1)
	bx1, by1 := math.Inf(-1), math.Inf(-1)
	for _, c := range comps {
		if !c.hasBBox {
			continue
		}
		bx0, by0 = math.Min(bx0, c.minX), math.Min(by0, c.minY)
		bx1, by1 = math.Max(bx1, c.maxX), math.Max(by1, c.maxY)
	}
	if opt.board != nil {
		bx0, by0, bx1, by1 = opt.board.x0, opt.board.y0, opt.board.x1, opt.board.y1
	}
	m := opt.partGap
	// placed holds the FIXED rects (edge parts + mains + holes) satellites avoid.
	var placed []cpRect
	for _, h := range holes {
		placed = append(placed, cpRect{h.x - h.r, h.y - h.r, h.x + h.r, h.y + h.r})
	}
	// Layer-aware: a satellite only clashes with same-layer fixed parts. Track layer per rect.
	type lrect struct {
		cpRect
		layer int
	}
	var lplaced []lrect
	addFixed := func(r cpRect, layer int) {
		placed = append(placed, r)
		lplaced = append(lplaced, lrect{r, layer})
	}
	clashFixed := func(r cpRect, layer int) bool {
		for _, h := range holes { // holes cut every layer
			if (cpRect{h.x - h.r, h.y - h.r, h.x + h.r, h.y + h.r}).overlaps(r) {
				return true
			}
		}
		for _, lr := range lplaced {
			if lr.layer == layer && lr.cpRect.overlaps(r) {
				return true
			}
		}
		return false
	}
	inside := func(r cpRect) bool {
		return !(r.x0 < bx0-20 || r.y0 < by0-20 || r.x1 > bx1+20 || r.y1 > by1+20)
	}
	// Zone claims (issue #126): resolve a component's functional zone, if any.
	zoneFor := func(c cpComp) (cpZoneClaim, bool) {
		if opt.zones == nil {
			return cpZoneClaim{}, false
		}
		z, ok := opt.zones[strings.ToUpper(c.designator)]
		return z, ok
	}
	rectInsideZone := func(r cpRect, z cpRect) bool {
		return !(r.x0 < z.x0-20 || r.y0 < z.y0-20 || r.x1 > z.x1+20 || r.y1 > z.y1+20)
	}
	// spiralIn finds the nearest non-clashing on-board spot for a hw×hh part,
	// starting at (sx,sy), optionally constrained to zone rect z.
	// spiralRadius 是从 seed 向外螺旋找第一个合法位的通用搜索。maxRad 决定它肯走
	// 多远：全板合法化用 cpSpiralMaxRad（把件放下才是第一要务），而「跟着端子走」
	// 只肯在端子附近找（见 cpFollowRadius）——跟随的价值是保住相对位置，跟到几百
	// mil 外就失去意义了，还不如不跟。
	spiralRadius := func(sx, sy, hw, hh float64, layer int, z *cpRect, maxRad float64) (float64, float64, bool) {
		for rad := 0.0; rad <= maxRad; rad += 25 {
			steps := 1
			if rad > 0 {
				steps = 24
			}
			for s := 0; s < steps; s++ {
				ang := float64(s) * math.Pi / 12
				px, py := sx+rad*math.Cos(ang), sy+rad*math.Sin(ang)
				r := cpRect{px - hw - m, py - hh - m, px + hw + m, py + hh + m}
				if !inside(r) {
					continue
				}
				if z != nil && !rectInsideZone(r, *z) {
					continue
				}
				if !clashFixed(r, layer) {
					return px, py, true
				}
			}
		}
		return 0, 0, false
	}
	// spiralIn 是全板合法化用的那档：肯一路找到 cpSpiralMaxRad，因为「把件放下」
	// 比「放得近」更要紧 —— 放不下就是 off-board。
	spiralIn := func(sx, sy, hw, hh float64, layer int, z *cpRect) (float64, float64, bool) {
		return spiralRadius(sx, sy, hw, hh, layer, z, cpSpiralMaxRad)
	}

	// Classify.
	kinds := make([]cpClass, len(comps))
	for i, c := range comps {
		kinds[i] = classifyCP(c, opt.mainPins)
	}

	// ── Tier 2: edge-must → snap to an edge, fix. ─────────────────────────────
	// Consume the block `edge` role (data/*.json placement.<ref>.edge — the
	// source-of-truth): connectors a block marks edge="user-facing" (USB / SD /
	// screw-terminals / headers the user plugs into) are GROUPED onto ONE shared
	// board edge and packed along it, so external I/O sits together on one
	// accessible side instead of each snapping to whichever edge it was nearest
	// (the scatter the per-part rule caused on a spread seed → oversized board).
	// edge="any" parts (RF antenna / radio module — must be at *an* edge, any
	// works) keep the per-part nearest-edge rule.
	edgeIdx := []int{}
	for i := range comps {
		if kinds[i] == cpEdgeMust {
			edgeIdx = append(edgeIdx, i)
		}
	}
	// Biggest first (big connectors claim edge space before small ones).
	sort.Slice(edgeIdx, func(a, b int) bool {
		ca, cb := comps[edgeIdx[a]], comps[edgeIdx[b]]
		return ca.width()*ca.height() > cb.width()*cb.height()
	})

	nearestEdge := func(px, py float64) apEdge {
		dL, dR, dB, dT := px-bx0, bx1-px, py-by0, by1-py
		best, edge := dL, edgeLeft
		if dR < best {
			best, edge = dR, edgeRight
		}
		if dB < best {
			best, edge = dB, edgeBottom
		}
		if dT < best {
			edge = edgeTop
		}
		return edge
	}
	// edgeConnDelta is the rotation (relative to current) that turns c's opening
	// off the assigned edge — block-declared opening first (deterministic), else
	// the asymmetric pad-geometry guess ONLY when the current opening clearly
	// faces the interior, else 0 (symmetric / already-correct → preserve).
	edgeConnDelta := func(c cpComp, edge apEdge) float64 {
		if lox, loy, ok := connOpeningFor(c.footprint); ok {
			return openingTargetDelta(c.rotation, lox, loy, edge)
		}
		delta, score := bestConnDelta(c, edge)
		pcx, pcy, gx0, gy0, gx1, gy1 := connGeom(c, 0)
		ix, iy := edgeInteriorDir(edge)
		curScore := (pcx-(gx0+gx1)/2)*ix + (pcy-(gy0+gy1)/2)*iy
		if curScore < -30 && score > 30 {
			return delta
		}
		return 0
	}
	// placeEdgePart orients connector i for `edge`, snaps it flush (opening
	// off-board), and — when alongCenter != nil (the grouped user-facing path) —
	// packs its along-edge center to alongCenter. Records the move + diag and adds
	// it to the fixed set.
	//
	// edgeShift 记录每个 T2 件被挪动的量（primitiveId → dx,dy），T4 用它把
	// 「原本贴着这个端子」的卫星一起带走。
	edgeShift := map[string][2]float64{}
	placeEdgePart := func(i int, edge apEdge, alongCenter *float64) {
		c := comps[i]
		cx, cy := c.bboxCenter()
		var best float64
		switch edge {
		case edgeLeft:
			best = cx - bx0
		case edgeRight:
			best = bx1 - cx
		case edgeBottom:
			best = cy - by0
		default: // edgeTop
			best = by1 - cy
		}
		delta := edgeConnDelta(c, edge)
		_, score := bestConnDelta(c, edge)
		pcx, pcy, ux0, uy0, ux1, uy1 := connGeom(c, 0)
		ix, iy := edgeInteriorDir(edge)
		curScore := (pcx-(ux0+ux1)/2)*ix + (pcy-(uy0+uy1)/2)*iy
		// 插接面器件特性(Type-C/USB/SD/耳机口,与 edge-io 的 plug-face 规则同一
		// 正则):插头从板外水平进入,插接面必须与板边**齐平**(0 边距;外突量待
		// 块库声明后可为负)。通用 45mil 边距对它们 = 永远插不进去 —— 车机 J2
		// (Type-C)缩板内 129mil 正是这么来的:在旧「edgeMargin+30 内算就位」的
		// 判据下既不算错也不被挪。
		margin := opt.edgeMargin
		if edgeIOPlugFaceRe.MatchString(c.footprint) {
			margin = 0
		}
		alreadyGood := best <= margin+30 && curScore > 15
		clearlyWrong := curScore < -30 && score > 30
		_, _, blockOriented := connOpeningFor(c.footprint)
		// Geometry after the chosen rotation (about the anchor).
		_, _, gx0, gy0, gx1, gy1 := connGeom(c, delta)
		var shiftX, shiftY float64
		switch edge {
		case edgeLeft:
			shiftX = (bx0 + margin) - gx0
		case edgeRight:
			shiftX = (bx1 - margin) - gx1
		case edgeBottom:
			shiftY = (by0 + margin) - gy0
		case edgeTop:
			shiftY = (by1 - margin) - gy1
		}
		if alongCenter != nil { // grouped: pack the along-edge center
			if edge.vertical() {
				shiftY += *alongCenter - (gy0+gy1)/2
			} else {
				shiftX += *alongCenter - (gx0+gx1)/2
			}
		}
		nx, ny := c.x+shiftX, c.y+shiftY
		nr := cpRect{gx0 + shiftX - m, gy0 + shiftY - m, gx1 + shiftX + m, gy1 + shiftY + m}
		addFixed(nr, c.layer)
		// 记下这一档件被挪了多远，供 T4 的「跟随」用。
		//
		// 为什么需要：保护件(保险丝/TVS/ESD)的正确位置是**贴着它保护的那个端子**，
		// 不是贴着主芯片。而 T2 把端子沿板边重新分组打包时，位移可能上百 mil ——
		// 贴着它的保护件如果原地不动，就被甩下了。参考板实测：J1 被挪 609mil，
		// F1/D1 原地不动，于是 protection 维从 100 掉到 33.8，两件各扣 25 分
		// （F1 离 J1.1 614mil，预算 250mil）。
		if math.Abs(shiftX) > 0.5 || math.Abs(shiftY) > 0.5 {
			edgeShift[c.id] = [2]float64{shiftX, shiftY}
		}
		if math.Abs(shiftX) > 1 || math.Abs(shiftY) > 1 || delta != 0 {
			moves = append(moves, apMove{ID: c.id, Designator: c.designator,
				NewX: round1(nx), NewY: round1(ny), NewRot: c.rotation + delta, SetRot: delta != 0, Edge: edge.String()})
		}
		// Symmetric connector whose opening isn't in the pads (couldn't confirm or
		// rotate) → flag for user confirmation (per "对称件保留用户手调").
		needsConfirm := !blockOriented && !alreadyGood && !clearlyWrong && curScore <= 15
		reason := "edge:" + edge.String()
		switch {
		case blockOriented && delta != 0:
			reason += ":oriented-by-block"
		case blockOriented:
			reason += ":block-ok"
		case alreadyGood:
			reason += ":recognized"
		case delta != 0:
			reason += ":oriented"
		case needsConfirm:
			reason += ":confirm-orientation"
		}
		if alongCenter != nil {
			reason += ":grouped"
		}
		if z, hasZone := zoneFor(c); hasZone {
			// The board edge is a harder constraint than the functional zone —
			// an interface connector's zone claim is advisory (issue #126).
			reason += ":zone-exempt(" + z.module + ")"
		}
		diags = append(diags, apDiag{Designator: c.designator, Reason: reason})
	}

	// Group block-declared user-facing connectors onto one shared edge (packed
	// and centered along it). Only engages with >=2 of them — a lone connector
	// just takes its nearest edge like before.
	grouped := map[int]bool{}
	var uf []int
	for _, i := range edgeIdx {
		if comps[i].hasBBox && edgeRoleOf(comps[i]) == "user-facing" {
			uf = append(uf, i)
		}
	}
	if len(uf) >= 2 {
		var scx, scy float64
		for _, i := range uf {
			x, y := comps[i].bboxCenter()
			scx, scy = scx+x, scy+y
		}
		scx, scy = scx/float64(len(uf)), scy/float64(len(uf))
		// Net-aware shared edge: put the group on the edge nearest the FIXED chips
		// these connectors electrically drive (their partner mains) — so USB lands
		// beside its CH340 instead of on whichever edge the connectors happened to
		// cluster, i.e. group WITHOUT lengthening the nets / adding crossings. Local
		// (signal) nets decide the partner (global GND/VCC are shared by every chip,
		// so they'd pick a geometrically-near but electrically-unrelated one); fall
		// back to the geometric group centroid when no partner main is found. Reuses
		// the Tier-4 net-aware seeding pattern (mainNetPads / nearestPad).
		mainNetPads := map[string][]apPad{}
		for j, mc := range comps {
			if kinds[j] != cpMainChip && kinds[j] != cpAnchored {
				continue
			}
			for _, p := range mc.pads {
				if n := strings.TrimSpace(p.net); n != "" {
					mainNetPads[n] = append(mainNetPads[n], p)
				}
			}
		}
		partnerOf := func(c cpComp) (float64, float64, bool) {
			collect := func(localOnly bool) []apPad {
				var cand []apPad
				seen := map[string]bool{}
				for _, p := range c.pads {
					n := strings.TrimSpace(p.net)
					if n == "" || seen[n] || (localOnly && isGlobalNet(n)) {
						continue
					}
					seen[n] = true
					cand = append(cand, mainNetPads[n]...)
				}
				return cand
			}
			cx, cy := c.bboxCenter()
			if pad, ok := nearestPad(collect(true), cx, cy); ok {
				return pad.x, pad.y, true
			}
			if pad, ok := nearestPad(collect(false), cx, cy); ok {
				return pad.x, pad.y, true
			}
			return 0, 0, false
		}
		var tx, ty float64
		partners := 0
		for _, i := range uf {
			if px, py, ok := partnerOf(comps[i]); ok {
				tx, ty, partners = tx+px, ty+py, partners+1
			}
		}
		shared := nearestEdge(scx, scy)
		if partners > 0 {
			shared = nearestEdge(tx/float64(partners), ty/float64(partners))
		}
		alongExtent := func(i int) float64 {
			c := comps[i]
			_, _, gx0, gy0, gx1, gy1 := connGeom(c, edgeConnDelta(c, shared))
			if shared.vertical() {
				return gy1 - gy0
			}
			return gx1 - gx0
		}
		alongOf := func(i int) float64 {
			x, y := comps[i].bboxCenter()
			if shared.vertical() {
				return y
			}
			return x
		}
		sort.SliceStable(uf, func(a, b int) bool { return alongOf(uf[a]) < alongOf(uf[b]) })
		total := 0.0
		for k, i := range uf {
			if k > 0 {
				total += opt.partGap
			}
			total += alongExtent(i)
		}
		var amin, amax, ctr float64
		if shared.vertical() {
			amin, amax, ctr = by0, by1, scy
		} else {
			amin, amax, ctr = bx0, bx1, scx
		}
		lo, hi := amin+opt.edgeMargin, amax-opt.edgeMargin-total
		if hi < lo {
			hi = lo
		}
		start := ctr - total/2
		if start < lo {
			start = lo
		}
		if start > hi {
			start = hi
		}
		cursor := start
		for _, i := range uf {
			ext := alongExtent(i)
			center := cursor + ext/2
			placeEdgePart(i, shared, &center)
			cursor += ext + opt.partGap
			grouped[i] = true
		}
	}

	// Remaining edge parts (RF/module/any + a lone user-facing connector): nearest edge.
	for _, i := range edgeIdx {
		if grouped[i] || !comps[i].hasBBox {
			continue
		}
		cx, cy := comps[i].bboxCenter()
		placeEdgePart(i, nearestEdge(cx, cy), nil)
	}

	// ── Tier 3: main chips + crystals + block-anchored parts → keep, fix. ─────
	// cpAnchored is a part the block deliberately pinned to a specific non-edge
	// spot (e.g. a bus-terminal jumper beside its resistor). Like a main chip we
	// leave it where it is and add it to the fixed set, so the Tier-4 spiral can
	// never fling it to a corner.
	//
	// Zone-claimed mains (issue #126) that sit OUTSIDE their functional zone are
	// relocated into it first (spiral from the zone center) — S0's partitioning
	// beats "keep where imported". A main already inside its zone is untouched.
	for i, c := range comps {
		if (kinds[i] != cpMainChip && kinds[i] != cpAnchored) || !c.hasBBox {
			continue
		}
		cx, cy := c.bboxCenter()
		hw, hh := c.width()/2, c.height()/2
		if z, hasZone := zoneFor(c); hasZone {
			if !(cx >= z.rect.x0 && cx <= z.rect.x1 && cy >= z.rect.y0 && cy <= z.rect.y1) {
				zcx, zcy := (z.rect.x0+z.rect.x1)/2, (z.rect.y0+z.rect.y1)/2
				if px, py, ok := spiralIn(zcx, zcy, hw, hh, c.layer, &z.rect); ok {
					addFixed(cpRect{px - hw - m, py - hh - m, px + hw + m, py + hh + m}, c.layer)
					moves = append(moves, apMove{ID: c.id, Designator: c.designator,
						NewX: round1(c.x + (px - cx)), NewY: round1(c.y + (py - cy)), Edge: "zone:" + z.zone})
					diags = append(diags, apDiag{Designator: c.designator, Reason: "main:zoned:" + z.module})
					continue
				}
				diags = append(diags, apDiag{Designator: c.designator, Reason: "main:zone-no-fit:" + z.module})
			}
		}
		addFixed(cpRect{c.minX - m, c.minY - m, c.maxX + m, c.maxY + m}, c.layer)
		reason := "main:fixed"
		if kinds[i] == cpAnchored {
			reason = "anchored:fixed"
		}
		diags = append(diags, apDiag{Designator: c.designator, Reason: reason})
	}

	// ── Tier 4: satellites + user-facing → legalize (spiral) around fixed. ────
	satIdx := []int{}
	for i := range comps {
		if kinds[i] == cpSatellite || kinds[i] == cpUserFacing {
			satIdx = append(satIdx, i)
		}
	}
	// 贴脚约束件（保护件/去耦，cpNeedsHugging）优先落子，其余按大件先行。
	// 为什么：跟随的落点合法位是**先到先得**的 —— 端子搬家后它旁边的空间有限，
	// 普通卫星先占掉就轮不到真正必须贴身的保护件（真板实锤：TVS/ESD 的跟随
	// 目标 400mil 内找不到合法位，被螺旋推到 678mil 外，protection 直接崩）。
	sort.Slice(satIdx, func(a, b int) bool {
		ca, cb := comps[satIdx[a]], comps[satIdx[b]]
		ha, hb := cpNeedsHugging(ca), cpNeedsHugging(cb)
		if ha != hb {
			return ha
		}
		return ca.width()*ca.height() > cb.width()*cb.height()
	})
	// Net-aware seed source: pads of the FIXED, NON-MOVED parts (mains + anchored).
	// Tier-2 edge parts DID move, so their pad coords are stale — exclude them.
	// A satellite that must be relocated is seeded near its nearest electrically-
	// related fixed pad, so a decoupling cap clusters onto its chip instead of
	// landing at the first free slot near a bad import position.
	fixedNetPads := map[string][]apPad{}
	for i, c := range comps {
		if kinds[i] != cpMainChip && kinds[i] != cpAnchored {
			continue
		}
		for _, p := range c.pads {
			if n := strings.TrimSpace(p.net); n != "" {
				fixedNetPads[n] = append(fixedNetPads[n], p)
			}
		}
	}
	// Prefer LOCAL (signal) nets so a satellite clusters onto the chip it truly
	// belongs to; global power/ground nets (GND/VCC/3V3) are shared by EVERY chip,
	// so seeding on them would pick a geometrically-near but electrically-unrelated
	// chip. Fall back to all nets only when the part has no local net (a pure
	// decoupling cap on VCC+GND) — matching the auto-place convention (localNets).
	netSeed := func(c cpComp, fromX, fromY float64) (float64, float64, bool) {
		collect := func(localOnly bool) []apPad {
			var cand []apPad
			seen := map[string]bool{}
			for _, p := range c.pads {
				n := strings.TrimSpace(p.net)
				if n == "" || seen[n] || (localOnly && isGlobalNet(n)) {
					continue
				}
				seen[n] = true
				cand = append(cand, fixedNetPads[n]...)
			}
			return cand
		}
		if pad, ok := nearestPad(collect(true), fromX, fromY); ok { // local nets first
			return pad.x, pad.y, true
		}
		if pad, ok := nearestPad(collect(false), fromX, fromY); ok { // fall back to all
			return pad.x, pad.y, true
		}
		return 0, 0, false
	}

	// followEdgeShift 是「跟着自己的端子走」的位移。
	//
	// 保护件（保险丝 / TVS / ESD）与端子去耦的正确位置是**贴着那个端子**，不是贴着
	// 主芯片 —— 而 netSeed 只认 main/anchored 的 pad（T2 件动过、坐标 stale，被显式
	// 排除），所以它永远把这类件往主芯片吸。参考板实测：J1 被 T2 挪了 609mil，
	// 贴着它的 F1/D1 因为**当前位置仍合法**连重定位分支都进不去，就地被甩下，
	// protection 维 100 → 33.8。
	//
	// 修法不是重新 seed（那会把件扔到芯片边上，丢掉人工摆好的相对关系），而是**跟随**：
	// 端子平移多少，贴着它的件平移多少，相对位置原样保留，然后照常走合法化。
	//
	// 两条收敛判据，避免把整板拖着跑：
	//   - 只认**非全局网**的伙伴。GND/VCC 连着所有东西，跟着它们走等于随机跟。
	//   - 只认**原本就贴着**的（cpFollowRadius 内）。远处的同网件不是它的保护对象。
	//
	// 多个伙伴都动了时跟最近的那个 —— 最近的才是它真正服务的端口。
	// nearestPortOwnerIdx 按**打分器同规则**选保护件的伙伴：measureProtection 对
	// 每个保护件取「非地共享网上最近的端子焊盘」，伙伴就是那个焊盘的拥有者。
	// 端子判定同样复用打分器的 isPortIdent。无半径——打分器量距离也没有半径，
	// 伙伴远近改变的是扣多少分，不改变「它是参照物」这件事。
	nearestPortOwnerIdx := func(c cpComp) (int, [2]float64, bool) {
		bestD := math.Inf(1)
		best := -1
		var bestPad [2]float64
		for j, other := range comps {
			if other.id == c.id || !isPortIdent(other.designator, other.footprint, "") {
				continue
			}
			for _, op := range other.pads {
				onet := strings.TrimSpace(op.net)
				if onet == "" || isGndNetName(onet) {
					continue
				}
				for _, mp := range c.pads {
					if strings.TrimSpace(mp.net) != onet {
						continue
					}
					if d := math.Hypot(op.x-mp.x, op.y-mp.y); d < bestD {
						bestD, best = d, j
						bestPad = [2]float64{op.x, op.y}
					}
				}
			}
		}
		return best, bestPad, best >= 0
	}

	// followEdgeShift 返回 (刚性位移, 伙伴焊盘终位, 有终位, 要跟)。伙伴焊盘终位
	// 只在保护件的归因对齐路径给出 —— 刚性跟随落不下时，落点搜索退回以它为
	// 圆心（protection 维量的就是到这个 pad 的距离，贴不回原相对位置时贴 pad
	// 本身是次优但同口径的目标）。
	followEdgeShift := func(c cpComp) ([2]float64, [2]float64, bool, string, bool) {
		none := [2]float64{}
		if len(edgeShift) == 0 {
			return none, none, false, "", false
		}
		// 保护件走**归因对齐**路径（#21）：伙伴 = 打分器会归因的那个端口件，
		// 不是「最近的动过的边缘件」。真板实锤过错位：TVS_VBUS/ESD1 按原始最近
		// pad 对跟了 J_VEH 的位移，而 protection 维按 J2.A4B9 归因 —— 连接器组
		// 换边重排后两者分道扬镳，保住的距离不是被打分的那段距离。三种结局：
		//   伙伴动了 → 跟它的位移（pad 刚性随件动，打分距离原样保留）；
		//   伙伴没动 → 不跟（被保护的端口就在原地，跟别人只会拉远）；
		//   没有端口伙伴（如纯地 TVS）→ 落到下面的泛化路径。
		if isProtectionIdent(c.designator, c.footprint, "") {
			if j, pad, ok := nearestPortOwnerIdx(c); ok {
				if sh, moved := edgeShift[comps[j].id]; moved {
					return sh, [2]float64{pad[0] + sh[0], pad[1] + sh[1]}, true, comps[j].id, true
				}
				return none, none, false, "", false
			}
		}
		nets := func(localOnly bool) map[string]bool {
			out := map[string]bool{}
			for _, p := range c.pads {
				n := strings.TrimSpace(p.net)
				if n == "" || (localOnly && isGlobalNet(n)) {
					continue
				}
				out[n] = true
			}
			return out
		}
		// 两轮：本地网宽半径，纯去耦（只有 VCC+GND，没有任何本地网）退回全局网但
		// 收紧半径。**纯去耦恰恰是最需要跟着走的那一类**，一刀切「没有本地网就不跟」
		// 会把它们全漏掉（参考板实测：C9/C2 就是这样被落下的）；但 GND 连着板上
		// 所有东西，放宽到全局网后只有「就在旁边」才构成跟随关系，所以半径必须收窄。
		try := func(cand map[string]bool, radius float64) ([2]float64, string, bool) {
			if len(cand) == 0 {
				return [2]float64{}, "", false
			}
			bestD := math.Inf(1)
			var bestShift [2]float64
			var bestID string
			var found bool
			for j, other := range comps {
				if kinds[j] != cpEdgeMust && kinds[j] != cpUserFacing && kinds[j] != cpMainChip {
					continue
				}
				sh, moved := edgeShift[other.id]
				if !moved {
					continue
				}
				// 距离用**共享网的最近焊盘对**，不是 bbox 中心距 —— 必须与
				// protection 维同口径（它判的是 pad 中心到 pad 中心）。
				// 大芯片上两者差很多：参考板实测 C9 到 U2 的中心距 366mil，
				// 但 U2 的 3V3 焊盘就在 C9 旁边、在去耦预算内。用中心距判
				// 「原本贴着」会把这类摆对了的去耦判成不相干，跟随就此漏掉。
				d := math.Inf(1)
				for _, op := range other.pads {
					if !cand[strings.TrimSpace(op.net)] {
						continue
					}
					for _, mp := range c.pads {
						if strings.TrimSpace(mp.net) != strings.TrimSpace(op.net) {
							continue
						}
						d = math.Min(d, math.Hypot(op.x-mp.x, op.y-mp.y))
					}
				}
				if math.IsInf(d, 1) {
					continue // 没有共享网的焊盘对
				}
				if d < bestD && d <= radius {
					bestD, bestShift, bestID, found = d, sh, other.id, true
				}
			}
			return bestShift, bestID, found
		}
		if sh, id, ok := try(nets(true), cpFollowRadius); ok {
			return sh, none, false, id, true
		}
		sh, id, ok := try(nets(false), cpFollowRadiusGlobal)
		return sh, none, false, id, ok
	}

	for _, i := range satIdx {
		c := comps[i]
		if !c.hasBBox {
			continue
		}
		// ocx/ocy 是**原始** bbox 中心 —— 所有 move 的换算基准（anchor 与 bbox 中心
		// 的偏移是固定的，所以 newAnchor = c.x + (目标中心 − 原始中心)）。
		// cx0/cy0 是**工作**中心，跟随平移会改它，绝不能拿它当基准，否则跟随的那段
		// 位移会在最后的 move 里凭空消失。
		ocx, ocy := c.bboxCenter()
		cx0, cy0 := ocx, ocy
		hw, hh := c.width()/2, c.height()/2
		z, hasZone := zoneFor(c)
		inZone := func(r cpRect) bool { return !hasZone || rectInsideZone(r, z.rect) }
		// 端子搬家 → 贴着它的保护件/去耦跟着搬，相对关系原样保留。
		//
		// **只在跟得上时才跟**：跟过去的位置必须仍然合法。跟过去撞了就退回原位，
		// 因为撞了之后 spiralIn 会把这件螺旋到更远处 —— 那比原地不动还糟。
		//
		// 这条守卫是真板逼出来的。稀疏板上无条件跟随是纯收益（参考板 protection
		// 33.8 → 75.7）；但 166 器件的密板上跟过去几乎必然碰撞，无条件跟随让移动
		// 件数从 19 涨到 50、protection 反而从 10.2 掉到 8.7。密板没有空位可跟，
		// 硬跟只是把件甩得更散。
		followed := false
		followsID := ""
		if cpNeedsHugging(c) {
			if sh, pad, hasPad, partnerID, ok := followEdgeShift(c); ok {
				fx, fy := cx0+sh[0], cy0+sh[1]
				var zr *cpRect
				if hasZone {
					zr = &z.rect
				}
				// 只在端子附近找落点。找不到就放弃跟随，让它走原有逻辑 ——
				// 硬跟会让 spiralIn 从跟随点一路螺旋到几百 mil 外，比原地不动更散
				// （真板实测：无界跟随把移动件数从 19 推到 50，protection 反而更差）。
				//
				// 有伙伴 pad（保护件的归因对齐路径）时取**双候选之近者**：刚性
				// 跟随点周边 vs 伙伴焊盘终位周边，各 spiral 一次，选离 pad 更近
				// 的落点。真板实锤过单候选的陷阱：刚性跟随的 spiral 在盘缘
				//（400mil）"成功"落位，看似跟上了，量到 pad 却是 678mil ——
				// protection 维量的就是到这个 pad 的距离，落点必须按它择优。
				px, py, okNear := spiralRadius(fx, fy, hw, hh, c.layer, zr, cpFollowRadius)
				if hasPad {
					if p2x, p2y, ok2 := spiralRadius(pad[0], pad[1], hw, hh, c.layer, zr, cpFollowRadius); ok2 {
						if !okNear || math.Hypot(p2x-pad[0], p2y-pad[1]) < math.Hypot(px-pad[0], py-pad[1]) {
							px, py, okNear = p2x, p2y, true
						}
					}
				}
				if okNear {
					cx0, cy0 = px, py
					followed = true
					followsID = partnerID
				}
			}
		}
		// Keep a well-placed satellite EXACTLY where it is (no gratuitous moves —
		// don't disturb a hand-placed layout). Only relocate one that clashes —
		// or one sitting outside its claimed functional zone (issue #126).
		cur := cpRect{cx0 - hw - m, cy0 - hh - m, cx0 + hw + m, cy0 + hh + m}
		if inside(cur) && !clashFixed(cur, c.layer) && inZone(cur) {
			addFixed(cur, c.layer)
			// 跟随位移落笔。没有这一步，跟随就只改了个内部变量而画布上什么都没变
			// —— 而「当前位置仍合法」恰恰是 F1/D1 那类件走的分支。
			if followed {
				if dx, dy := cx0-ocx, cy0-ocy; math.Abs(dx) > 1 || math.Abs(dy) > 1 {
					moves = append(moves, apMove{ID: c.id, Designator: c.designator,
						NewX: round1(c.x + dx), NewY: round1(c.y + dy), Edge: kinds[i].String(),
						FollowsID: followsID})
					diags = append(diags, apDiag{Designator: c.designator, Reason: "satellite:follows-edge-part"})
				}
			}
			continue
		}
		// Must relocate. A pure satellite (decoupling cap / resistor) is seeded near
		// its chip (nearest shared-net fixed pad) so it clusters there; a USER-FACING
		// part (LED / button) is NOT net-hugged — it should stay where it is visible
		// / accessible, so it just spirals out from its current position. A zone
		// claim clamps the seed into the zone so the spiral starts inside it.
		seedX, seedY := cx0, cy0
		if kinds[i] == cpSatellite {
			if sx, sy, ok := netSeed(c, cx0, cy0); ok {
				seedX, seedY = sx, sy
			}
		}
		if hasZone {
			seedX = math.Max(z.rect.x0+hw+m, math.Min(z.rect.x1-hw-m, seedX))
			seedY = math.Max(z.rect.y0+hh+m, math.Min(z.rect.y1-hh-m, seedY))
		}
		var zrect *cpRect
		if hasZone {
			zrect = &z.rect
		}
		px, py, ok := spiralIn(seedX, seedY, hw, hh, c.layer, zrect)
		if !ok && hasZone {
			// Zone full — better placed outside the zone than stranded off-board.
			// Loud diag: pcb check's zone-violation will keep flagging it.
			px, py, ok = spiralIn(seedX, seedY, hw, hh, c.layer, nil)
			if ok {
				diags = append(diags, apDiag{Designator: c.designator, Reason: "satellite:zone-overflow:" + z.module})
			}
		}
		if !ok {
			diags = append(diags, apDiag{Designator: c.designator, Reason: "satellite:no-fit"})
			continue
		}
		addFixed(cpRect{px - hw - m, py - hh - m, px + hw + m, py + hh + m}, c.layer)
		if hasZone {
			diags = append(diags, apDiag{Designator: c.designator, Reason: "satellite:zoned:" + z.module})
		}
		dx, dy := px-ocx, py-ocy
		if math.Abs(dx) > 1 || math.Abs(dy) > 1 {
			moves = append(moves, apMove{ID: c.id, Designator: c.designator, NewX: round1(c.x + dx), NewY: round1(c.y + dy), Edge: kinds[i].String(), FollowsID: followsID})
			if followed {
				diags = append(diags, apDiag{Designator: c.designator, Reason: "satellite:follows-edge-part:relocated"})
			}
		}
	}
	return snapMovesToAnchorGrid(moves), diags
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// cpAnchorGrid is the anchor grid every planned target lands on, in mil — the
// same 5-mil grid `pcb refine`'s grid-snap transform enforces and `pcb
// layout-score`'s tidy dimension measures against.
const cpAnchorGrid = 5.0

// snapMovesToAnchorGrid rounds every planned target anchor to the 5-mil grid.
//
// Without this the planner UNDOES the refine loop: targets are derived from
// live (possibly off-grid) source anchors plus real-valued pad geometry, so
// they land off-grid (live: 854.1 on ceshi), tidy drops back to 25 right after
// a refine pass had just fixed it, and every place-constrained run costs an
// extra refine round. Snapping at plan time shifts a target by ≤2.5 mil —
// #153 measured grid-snap at this magnitude as zero-side-effect (check
// findings unchanged) — and keeps "planned position" == "final position",
// which the plan/apply parity contract depends on.
func snapMovesToAnchorGrid(moves []apMove) []apMove {
	for i := range moves {
		moves[i].NewX = math.Round(moves[i].NewX/cpAnchorGrid) * cpAnchorGrid
		moves[i].NewY = math.Round(moves[i].NewY/cpAnchorGrid) * cpAnchorGrid
	}
	return moves
}

// cpDeviceName returns the string classifyCP pattern-matches on. A PLACED part's
// `name` is frequently the UNRESOLVED EasyEDA template "={Manufacturer Part}"
// (confirmed on the ceshi board — every part reported it), which makes the
// module/connector NAME regexes blind (e.g. an ESP32-S3-WROOM-1 module fails the
// `wroom` test and drops to the pin-count fallback → misclassified as a main chip
// instead of an edge part). So prefer the real manufacturerId; fall back to `name`
// only when manufacturerId is absent AND name isn't itself a "={…}" template.
func cpDeviceName(cm map[string]any) string {
	if mpn := strings.TrimSpace(asString(cm["manufacturerId"])); mpn != "" {
		return mpn
	}
	if n := strings.TrimSpace(asString(cm["name"])); !strings.HasPrefix(n, "={") {
		return n
	}
	return ""
}

// parseCpComps parses pcb.components.list into cpComps (apComp + device name +
// layer). A PCB component's identifying string is its device `name` (no footprint
// name is exposed); the layer is TOP=1 / BOTTOM=2.
func parseCpComps(result map[string]any) []cpComp {
	base := parseApComps(result)
	byID := map[string]cpComp{}
	raw, _ := result["components"].([]any)
	for _, ri := range raw {
		cm, ok := ri.(map[string]any)
		if !ok {
			continue
		}
		id := asString(cm["primitiveId"])
		layer := int(asFloat(cm["layer"]))
		if layer == 0 {
			layer = 1
		}
		byID[id] = cpComp{footprint: cpDeviceName(cm), layer: layer}
	}
	out := make([]cpComp, 0, len(base))
	for _, b := range base {
		extra := byID[b.id]
		out = append(out, cpComp{apComp: b, footprint: extra.footprint, layer: extra.layer})
	}
	return out
}

// readCpHoles reads mounting-hole cutouts (fills on the MULTI layer, id 12 — where
// `pcb slot` / `pcb mount-holes` put board cutouts) and reduces each to a center +
// radius obstacle. Geometry MUST be requested as the rendered bbox (includeBBox,
// the fetchPcbSlots precedent): pcb.fill.list never returns a polygon "points"
// field, which is why the old points-parsing version always saw 0 holes (issue
// #104 — `pcb slot`-milled M3 holes were invisible to Tier-1 avoidance).
func readCpHoles(cfg *appConfig, window string) []cpHole {
	res, err := requestAction(cfg, "pcb.fill.list", window,
		map[string]any{"layer": 12, "includeBBox": true})
	if err != nil || res == nil {
		return nil
	}
	fills, _ := res.Result["fills"].([]any)
	return cpHolesFromFills(fills)
}

// cpHolesFromFills is the pure parse half of readCpHoles (unit-testable):
// layer-12 fill entries → center+radius obstacles. Prefers the bbox field
// (what pcb.fill.list --includeBBox returns); falls back to a raw points array
// when a caller feeds primitive data directly. Unreadable fills are skipped.
func cpHolesFromFills(fills []any) []cpHole {
	var out []cpHole
	for _, fi := range fills {
		fm, ok := fi.(map[string]any)
		if !ok || int(asFloat(fm["layer"])) != 12 {
			continue
		}
		minX, minY := math.Inf(1), math.Inf(1)
		maxX, maxY := math.Inf(-1), math.Inf(-1)
		if bb, ok := fm["bbox"].(map[string]any); ok {
			x0, ok1 := asFloatOK(bb["minX"])
			y0, ok2 := asFloatOK(bb["minY"])
			x1, ok3 := asFloatOK(bb["maxX"])
			y1, ok4 := asFloatOK(bb["maxY"])
			if ok1 && ok2 && ok3 && ok4 {
				minX, minY, maxX, maxY = x0, y0, x1, y1
			}
		}
		if math.IsInf(minX, 1) { // no bbox → raw points fallback (debug/exec paths)
			pts, _ := fm["points"].([]any)
			for _, pi := range pts {
				p, ok := pi.([]any)
				if !ok || len(p) < 2 {
					continue
				}
				x, y := asFloat(p[0]), asFloat(p[1])
				minX, minY = math.Min(minX, x), math.Min(minY, y)
				maxX, maxY = math.Max(maxX, x), math.Max(maxY, y)
			}
		}
		if math.IsInf(minX, 1) {
			continue
		}
		cx, cy := (minX+maxX)/2, (minY+maxY)/2
		// clearance radius = hole radius + washer margin (M3 head ≈ R118 mil)
		r := math.Max((maxX-minX)/2, (maxY-minY)/2) + 60
		out = append(out, cpHole{x: cx, y: cy, r: r})
	}
	return out
}
