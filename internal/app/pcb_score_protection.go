package app

// pcb_score_protection.go — layout-score 的「保护件/去耦就近」维（dimProtection）。
//
// #167 审 box-v2 时发现人类板「好」在保护件各自贴着自己的端口：保险丝紧挨供电端子、
// TVS/ESD 紧挨 USB/天线入口。这条经验此前只有半条落进代码——`decap-too-far`
// （去耦贴 IC，pcb_check_dfm2.go §3.1）。本维把它补全成一对：
//
//	protection-too-far  保护件(F*/TVS/ESD/RV*) → 同网端子(J*/USB*/DC*…) 的最近中心距
//	decap-too-far       去耦电容 → 同轨 IC 引脚的最近中心距（**复用**既有实现）
//
// ── 分工：谁判、谁量 ────────────────────────────────────────────────────────
//
// 去耦这半边，`findDecapTooFar` 是**判据的唯一来源**（谁超标、finding 长什么样、
// 规范回指指向哪一节都归它）；本文件的 measureDecaps 只回答两个打分才需要、
// findDecapTooFar 不返回的量：**一共有几个候选**（分母）和**各自离多远**（扣分权重）。
// 这个项目吃过「计数与判定各算各的 → 0 个阻塞项却 FAIL」的亏，所以两条路径必须一致，
// 由 TestMeasureDecaps_AgreesWithFindDecapTooFar 机械钉住：measureDecaps 里超预算的
// 那批位号，必须与 findDecapTooFar 报出来的那批**完全相同**。
//
// ── 扣分模型（可加、可解释）────────────────────────────────────────────────
//
// 两族各自先算子分，再按「在场族数」等权合成：
//
//	子分_f      = 100 × (1 − Σ severity_i / 候选数_f)
//	族权重 w_f  = 1 / 在场族数        （只有去耦时 w=1，两族都在时各 0.5）
//	贡献者扣分_i = w_f × 100 × severity_i / 候选数_f
//	维度分       = 100 − Σ 贡献者扣分_i
//
// 为什么按族归一而不是「每件固定扣 N 分」：大板天然有几十个去耦电容，固定扣分会让
// 「50 个里坏 5 个」比「5 个里坏 1 个」惨得多，方向是反的。为什么两族**等权**而不是
// 按件数加权：去耦件数通常碾压保护件，按件数加权会把 #167 点名要看的保护件淹掉。
//
// 维度分**从贡献者扣分反推**（而不是先算分再单独凑归因），所以「Σ 归因 = 100 − 分数」
// 是恒等式而非巧合 —— 下游精修环按 Penalty 排序动器件时，动掉谁能涨多少分是可预测的。
//
// ── 这一维测不到什么 ────────────────────────────────────────────────────────
//
//  1. **方向性判不了**。「保护件应当夹在端子与被保护电路之间」才是完整判据，本维只判
//     距离。要判方向得先知道「被保护电路是谁」：要么 spec 显式声明端口拓扑，要么从
//     网表推上下游——两样当前都没有可靠输入，硬猜会把「共模电感后置」「π 型滤波第二级」
//     这类正确设计判成错的。所以只判距离，并且**一律 WARN/INFO 不 ERROR**。
//  2. **同网端子可能不是它保护的那个端子**。电源轨是全板性的：3V3 上的一颗 TVS 会被
//     配到最近的那个带 3V3 引脚的排针上，哪怕它其实在给 LDO 输出做钳位。取「最近的
//     同网端子」已经最大程度压低了这个误报（连最近的都远才报），但报文里必须写清
//     是对着哪个端子量的，让人能一眼驳回。
//  3. 只吃焊盘坐标 + 网名，**不吃 bbox** —— 所以快照里 bbox 缺失不影响本维（几何维会
//     降级，本维不会）。反过来，网名为空的板（还没做 import-changes）本维直接 skip。

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

const (
	// pcbProtectMaxMil —— 保护件焊盘 ↔ 同网端子焊盘的**中心距**预算。
	//
	// 出处：pcb-design-rules.md §7.2「ESD 器件就近连接，走线尽量短（≤5mm）」。
	// 5mm ≈ 197mil 说的是**走线长度**；中心距是走线长度的下界（曼哈顿绕行只会更长），
	// 直接拿 197 当中心距门限会把「端子本体大、自身焊盘间距就吃掉预算」的正常摆法误判
	// ——一个 5.08mm 接线端子光相邻焊盘间距就 200mil。所以留一档余量取 250mil(≈6.35mm)，
	// 与 decap 那条「文档 2mm 是边距、门限取 100mil 中心距」的留余量口径一致。
	//
	// **待校准初值**：手册只对 ESD 给了数字，把它外推到保险丝/TVS/压敏是我们的推广，
	// 这也是本维只出 WARN 的原因。Metrics 暴露 worstProtectionDistMil 原始量，
	// 拿一批公认的好板跑分后回来定这个数。
	pcbProtectMaxMil = 250.0

	// pcbNearSevFloor —— 刚越线时的最低严重度（0-1 尺度），两族共用。
	//
	// 为什么不从 0 连续起步：那意味着「超标 1mil」几乎不扣分，一块所有保护件都刚好
	// 压线越界的板会拿到接近满分——越线就是越线，先扣一个固定底，再按超标倍数往上加，
	// 超到预算的 2 倍时封顶 1.0。**待校准初值**（0.4 = 压线越界约等于「坏了四成」）。
	pcbNearSevFloor = 0.4
)

// ---------------------------------------------------------------------------
// 器件识别
// ---------------------------------------------------------------------------

// protectStrongDesRe —— 位号强前缀：EDA 位号约定里这些前缀专指保护器件，单看位号即可
// 判定。F/FU 保险丝、RV/MOV 压敏、TVS 瞬态抑制、ESD 静电阵列、PTC/PPTC 自恢复保险丝。
//
// 刻意**不含** D（二极管）：整流/续流/LED 全是 D*，光看位号会把点灯的 LED 当保护件。
// D* 必须靠型号关键词才算数（见 protectDeviceRe）。也不含 FB（磁珠）——磁珠是 EMI
// 器件不是保护件，摆位判据不同。
var protectStrongDesRe = regexp.MustCompile(`(?i)^(?:PPTC|PTC|FU|F|RV|MOV|TVS|ESD)\d`)

// protectDeviceRe —— 型号关键词。来源有二：
//   - skills/easyeda-agent/references/standard-parts.json 里 class=esd/tvs/fuse 的实际
//     选型（USBLC6-2SC6 / ESD9B5.0ST5G / SMAJ5.0A / MF-MSMF050-2500MA / CFS12V6T2R0）；
//     那个文件在 skill 树里，go:embed 够不到，所以关键词表抄在这里。
//   - 常见系列前缀（SMAJ/SMBJ/SMCJ/P6SMB/1.5KE 的 TVS 家族，PESD/RCLAMP/SRV05/NUP/
//     TPD/SP05 的 ESD 阵列家族，MSMF 的自恢复保险丝家族）。
//
// 短词一律带 `\b` 前缀边界，避免在型号中段瞎命中；带数字的形式（smf\d / mov\d / tpd\d）
// 是为了把「SMF 封装代号」「MOVE」这类同形词挡掉。
var protectDeviceRe = regexp.MustCompile(
	`(?i)\b(?:tvs|esd|pesd|pptc|ptc|fuse|varistor|usblc|rclamp|srv05|nup21|psm712|cdsot|sp05|sp30|tpd\d|mov\d)` +
		`|\b(?:smaj|smbj|smcj|smf\d|p6smb|1\.?5ke|msmf)` +
		`|保险丝|自恢复|压敏|静电|瞬态`)

// protectPortDesRe —— 对外端子/入口的位号前缀。与 pcb_autoplace.go 的 connectorDesRe
// 同源（J/CN/CON/USB/SIM/BAT，isEdgeConnector 用的就是它），额外加 DC（DC 电源座）和
// ANT（天线口——天线也是一条对外通路，ESD 常挂在这里）。
//
// 与 isEdgeConnector 的差别：那边额外要求「封装够大」（≥200mil）以免把小件误判成板边
// 接口去拖动它；这里只是**测距的参照物**，一个 2P 小排针同样是电流/静电的入口，加尺寸
// 门槛只会把真端子漏掉。
var protectPortDesRe = regexp.MustCompile(`(?i)^(?:USB|CON|CN|SIM|BAT|ANT|DC|J)\d`)

// protectPortDeviceRe —— 端子的型号/名称关键词，兜住位号不守约定的板（有人把 Type-C
// 标成 U3）。
var protectPortDeviceRe = regexp.MustCompile(
	`(?i)type-?c|micro-?usb|micro-?b|\busb\b|\bheader\b|terminal|receptacle|\bjack\b|socket|` +
		`端子|插座|接线|排针|排母|连接器`)

// isProtectionPart 判定一个封装是不是保护器件。
func isProtectionPart(c boardComp) bool {
	if protectStrongDesRe.MatchString(strings.TrimSpace(c.Designator)) {
		return true
	}
	// Device 优先（placed 件的 Name 常是 "={Manufacturer Part}" 模板，见 boardComp），
	// 但两个都查——模板串本来也命中不了关键词，多查一次没有代价。
	return protectDeviceRe.MatchString(c.Device) || protectDeviceRe.MatchString(c.Name)
}

// isPortPart 判定一个封装是不是对外端子/入口。
//
// **先排保护件**：USBLC6 的型号里带 "USB"，不先排就会被 protectPortDeviceRe 认成端子，
// 于是这颗 ESD 阵列拿自己当参照物、距离恒为 0、永远满分——正是硬约定 1（「没测」不能
// 伪装成「测了满分」）要堵的那种洞。
func isPortPart(c boardComp) bool {
	if isProtectionPart(c) {
		return false
	}
	if protectPortDesRe.MatchString(strings.TrimSpace(c.Designator)) {
		return true
	}
	return protectPortDeviceRe.MatchString(c.Device) || protectPortDeviceRe.MatchString(c.Name)
}

// ---------------------------------------------------------------------------
// 测量
// ---------------------------------------------------------------------------

// protectHit 是一个保护件的测量结果。Matched=false 表示「判不了」——它跟「测了、很近」
// 必须区分开，前者不进分母也不扣分，只进 Reason 和一条 INFO。
type protectHit struct {
	Designator string
	Net        string  // 与端子共享的那条网（不含 GND）
	PortRef    string  // 参照端子焊盘，形如 "J1.1"
	Dist       float64 // 中心距，mil
	At         pcbXY   // 保护件那一侧的焊盘坐标，给 finding 定位
	Matched    bool
}

// measureProtection 对每个保护件求「到同网端子焊盘的最近中心距」。
//
// 排除 GND：地把全板连成一片，拿它配对等于随便挑一个端子当参照物，量出来的距离没有
// 物理含义（一颗内部 TVS 会因为共 GND 被配到板另一头的排针上）。所以只用非地网配对，
// 一个保护件若只有地网可用，就判为 Matched=false 而不是硬凑一个数。
//
// 同网端子有多个时取最近的——最近的那个就是它实际保护的对象（#167 的判据原文）。
// 距离是纯 XY 中心距，不看层：TVS 摆在端子正下方的底层同样算贴身。
func measureProtection(snap *boardSnapshot) []protectHit {
	type refPad struct {
		ref  string
		x, y float64
	}
	ports := map[string][]refPad{}
	for _, c := range snap.Components {
		if !isPortPart(c) {
			continue
		}
		for _, p := range c.Pads {
			net := strings.TrimSpace(p.Net)
			if net == "" || isGndNetName(net) {
				continue
			}
			ports[net] = append(ports[net], refPad{ref: protectPadRef(c.Designator, p.Number), x: p.X, y: p.Y})
		}
	}

	var out []protectHit
	for _, c := range snap.Components {
		if !isProtectionPart(c) {
			continue
		}
		h := protectHit{Designator: c.Designator, Dist: math.Inf(1)}
		for _, p := range c.Pads {
			net := strings.TrimSpace(p.Net)
			if net == "" || isGndNetName(net) {
				continue
			}
			for _, q := range ports[net] {
				d := math.Hypot(q.x-p.X, q.y-p.Y)
				if d >= h.Dist {
					continue
				}
				h.Dist, h.Net, h.PortRef, h.Matched = d, net, q.ref, true
				h.At = pcbXY{round2(p.X), round2(p.Y)}
			}
		}
		if !h.Matched {
			h.Dist = 0 // +Inf 会毒死 metrics 的 max，判不了就归零并靠 Matched 区分
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Designator < out[j].Designator })
	return out
}

// protectPadRef 拼「位号.脚号」，与 pcb check 各规则报文里的引用格式一致。
func protectPadRef(des, num string) string {
	if strings.TrimSpace(num) == "" {
		return des
	}
	return des + "." + num
}

// decapHit 是一个去耦电容候选的测量结果（到同轨最近 IC 引脚的中心距）。
type decapHit struct {
	Designator string
	Net        string  // 电源轨
	Dist       float64 // 中心距，mil
}

// 去耦候选的位号约定，必须与 findDecapTooFar 内部那两条保持一致（C* 电容 / U* 芯片）。
// 一致性由 TestMeasureDecaps_AgreesWithFindDecapTooFar 钉住。
var (
	scoreCapRe = regexp.MustCompile(`(?i)^C\d`)
	scoreICRe  = regexp.MustCompile(`(?i)^U\d`)
)

// measureDecaps 给出去耦族的**量表**：谁是候选、各自离最近的同轨 IC 引脚多远。
//
// 它**不产出 finding**，也不做「超没超标」的判定——那归 findDecapTooFar（判据唯一
// 来源）。这里只提供打分才需要的两个量：分母（候选数）和权重（距离）。分类口径逐条
// 对齐 findDecapTooFar：恰好 2 个焊盘的 C*、一脚地一脚非地全局网、且该轨上确实有 IC
// 引脚（没有 IC 的轨是 bulk/输入电容，不是去耦，两边都得豁免，否则分母被灌水、每个
// 真正的越界者被稀释）。
func measureDecaps(pads []pcbPadP) []decapHit {
	icPadsByNet := map[string][]pcbPadP{}
	byDesig := map[string][]pcbPadP{}
	var order []string
	for _, p := range pads {
		net := strings.TrimSpace(p.Net)
		if scoreICRe.MatchString(p.Designator) && net != "" && isGlobalNet(net) && !isGndNetName(net) {
			icPadsByNet[net] = append(icPadsByNet[net], p)
		}
		if scoreCapRe.MatchString(p.Designator) {
			if _, ok := byDesig[p.Designator]; !ok {
				order = append(order, p.Designator)
			}
			byDesig[p.Designator] = append(byDesig[p.Designator], p)
		}
	}
	sort.Strings(order)

	var out []decapHit
	for _, d := range order {
		ps := byDesig[d]
		if len(ps) != 2 {
			continue
		}
		var pwr *pcbPadP
		gnd := false
		for i := range ps {
			net := strings.TrimSpace(ps[i].Net)
			switch {
			case isGndNetName(net):
				gnd = true
			case net != "" && isGlobalNet(net):
				pwr = &ps[i]
			}
		}
		if !gnd || pwr == nil {
			continue
		}
		ics := icPadsByNet[strings.TrimSpace(pwr.Net)]
		if len(ics) == 0 {
			continue
		}
		best := math.Inf(1)
		for _, ip := range ics {
			if dd := math.Hypot(ip.X-pwr.X, ip.Y-pwr.Y); dd < best {
				best = dd
			}
		}
		out = append(out, decapHit{Designator: d, Net: strings.TrimSpace(pwr.Net), Dist: best})
	}
	return out
}

// nearnessSeverity 把「超预算多少」映射成 0-1 的严重度：没超 = 0，刚超 = 底值，
// 超到预算 2 倍及以上 = 1（封顶，免得一颗放在板角的电容把整维打成 0 而掩盖其它问题）。
func nearnessSeverity(dist, budget float64) float64 {
	if budget <= 0 || dist <= budget {
		return 0
	}
	over := (dist - budget) / budget
	return math.Min(1, pcbNearSevFloor+(1-pcbNearSevFloor)*over)
}

// ---------------------------------------------------------------------------
// 维度实现
// ---------------------------------------------------------------------------

type protectionScorer struct{}

func init() { registerDimScorer(protectionScorer{}) }

func (protectionScorer) id() string { return dimProtection }

func (protectionScorer) score(ctx *scoreCtx) scoreDimension {
	if ctx == nil {
		return skipDimension(dimProtection, layoutScoreOpts{}, "no scoring context")
	}
	if ctx.snap == nil || len(ctx.snap.Components) == 0 {
		return skipDimension(dimProtection, ctx.opts, "no components on the board")
	}
	snap := ctx.snap
	pads := snap.toCheckPads()

	prot := measureProtection(snap)
	decaps := measureDecaps(pads)
	decapFindings := findDecapTooFar(pads) // 判据唯一来源

	// ── 保护件族 ──
	type protOffender struct {
		hit protectHit
		sev float64
	}
	var (
		protOff        []protOffender
		unmatchedNames []string
		protJudged     int
		worstProt      float64
		sumProtSev     float64
	)
	for _, h := range prot {
		if !h.Matched {
			unmatchedNames = append(unmatchedNames, h.Designator)
			continue
		}
		protJudged++
		worstProt = math.Max(worstProt, h.Dist)
		if sev := nearnessSeverity(h.Dist, pcbProtectMaxMil); sev > 0 {
			protOff = append(protOff, protOffender{hit: h, sev: sev})
			sumProtSev += sev
		}
	}

	// ── 去耦族 ──
	// 分母取「量表候选 ∪ findDecapTooFar 报出来的位号」：即便哪天两边分叉，被判超标的
	// 也一定在分母里，不会出现「扣分比例 >100%」这种不自洽的报告。
	decapDist := make(map[string]decapHit, len(decaps))
	names := map[string]bool{}
	for _, h := range decaps {
		decapDist[h.Designator] = h
		names[h.Designator] = true
	}
	var offNames []string
	for _, f := range decapFindings {
		if f.Designator == "" {
			continue
		}
		names[f.Designator] = true
		offNames = append(offNames, f.Designator)
	}
	sort.Strings(offNames)
	decapTotal := len(names)
	var worstDecap float64
	for _, h := range decaps {
		worstDecap = math.Max(worstDecap, h.Dist)
	}

	// ── 在场族数 ──
	families := 0
	if protJudged > 0 {
		families++
	}
	if decapTotal > 0 {
		families++
	}
	if families == 0 {
		// 两种「没测」必须说清楚是哪一种，否则读者无法判断是板子干净还是度量瞎了。
		if len(prot) > 0 {
			return skipDimension(dimProtection, ctx.opts,
				"%d protection part(s) present (%s) but none shares a non-GND net with a recognized connector/terminal — proximity cannot be measured (nets not imported yet, or the port uses an unrecognized designator)",
				len(prot), strings.Join(unmatchedNames, ", "))
		}
		return skipDimension(dimProtection, ctx.opts,
			"board has no protection part (F*/TVS/ESD/RV*) and no rail-to-GND decoupling cap sitting on an IC rail — nothing to measure")
	}
	fw := 1.0 / float64(families)

	// ── 归因 + finding ──
	var (
		contributors []scoreContributor
		findings     []pcbCheckFinding
	)
	for _, o := range protOff {
		pen := round1(fw * 100 * o.sev / float64(protJudged))
		contributors = append(contributors, scoreContributor{
			Designator: o.hit.Designator,
			Penalty:    pen,
			Detail: fmt.Sprintf("protection: %.0fmil (%.1fmm) from port pad %s on net %s, budget %.0fmil",
				o.hit.Dist, o.hit.Dist*0.0254, o.hit.PortRef, o.hit.Net, pcbProtectMaxMil),
		})
		at := o.hit.At
		findings = append(findings, pcbCheckFinding{
			Type: "protection-too-far", Level: "WARN", Net: o.hit.Net,
			Designator: o.hit.Designator, At: &at,
			Message: fmt.Sprintf("protection part %s sits %.0fmil (%.1fmm) from %s — a fuse/TVS/ESD belongs AT the entry point it guards (budget %.0fmil); if it is deliberately placed downstream (post common-mode choke, second π stage) this WARN is expected%s",
				o.hit.Designator, o.hit.Dist, o.hit.Dist*0.0254, o.hit.PortRef, pcbProtectMaxMil,
				docRule("7.2", "保护器件就近端子(ESD 走线≤5mm)")),
		})
	}
	var sumDecapSev float64
	for _, name := range offNames {
		// findDecapTooFar 已经判定它超标，所以严重度至少是底值；量表只决定往上加多少。
		sev := 1.0
		detail := fmt.Sprintf("decoupling: over the %.0fmil budget (distance unavailable)", pcbDecapMaxMil)
		if h, ok := decapDist[name]; ok {
			sev = math.Max(pcbNearSevFloor, nearnessSeverity(h.Dist, pcbDecapMaxMil))
			detail = fmt.Sprintf("decoupling: %.0fmil (%.1fmm) from the nearest %s IC pin, budget %.0fmil",
				h.Dist, h.Dist*0.0254, h.Net, pcbDecapMaxMil)
		}
		sumDecapSev += sev
		contributors = append(contributors, scoreContributor{
			Designator: name,
			Penalty:    round1(fw * 100 * sev / float64(decapTotal)),
			Detail:     detail,
		})
	}
	findings = append(findings, decapFindings...)

	d := newDimension(dimProtection, ctx.opts)

	// ── 降级说明 ──
	// 本维名下有两族，只要有一族（或一族里的某几件）没被覆盖，分数就只反映了它的一部分
	// ——那属于「输入不全」，必须标 degraded 并说清楚，绝不能让 100 分被读成「两族都完美」。
	var degrade []string

	// 判不了的保护件：没进分母也没扣分，不说一声就等于悄悄放过。
	if len(unmatchedNames) > 0 {
		degrade = append(degrade, fmt.Sprintf("%d protection part(s) (%s) share no non-GND net with any recognized connector/terminal and are NOT covered by this score — an internal-node clamp, or a port designator this heuristic missed",
			len(unmatchedNames), strings.Join(unmatchedNames, ", ")))
		findings = append(findings, pcbCheckFinding{
			Type: "protection-unmatched", Level: "INFO",
			Message: fmt.Sprintf("protection part(s) %s could not be paired with a connector/terminal on a shared non-GND net — proximity not scored for them",
				strings.Join(unmatchedNames, ", ")),
		})
	}
	// 「有对外端子却一颗保护件都没有」：保护那半边整体没测。仍出一条 INFO（缺保护件本身
	// 是值得提的电气决策），但**不扣分**——加不加保护件不是布局问题。没有端子的板不提，
	// 免得每块纯数字小板都挂一条噪声。
	if protJudged == 0 && hasPortPart(snap) {
		degrade = append(degrade, "no protection part (fuse/TVS/ESD/varistor) recognized on a board that has external connectors — the protection half of this dimension is unmeasured")
		findings = append(findings, pcbCheckFinding{
			Type: "protection-absent", Level: "INFO",
			Message: fmt.Sprintf("board has external connector(s) but no protection part (fuse/TVS/ESD) was recognized — this dimension scored on decoupling only%s",
				docRule("7.2", "保护器件就近端子(ESD 走线≤5mm)")),
		})
	}
	// 对称的另一半：有 IC 却一个去耦候选都没有，去耦那半边同样没测。
	if decapTotal == 0 && hasICPowerPad(pads) {
		degrade = append(degrade, "no rail-to-GND decoupling cap sits on any IC rail — the decoupling half of this dimension is unmeasured")
	}
	if len(degrade) > 0 {
		d.Status = dimDegraded
		d.Reason = strings.Join(degrade, "; ")
	}

	// 分数从归因反推，保证「Σ 归因 = 100 − 分数」是恒等式（见文件头扣分模型）。
	score := 100.0
	for _, c := range contributors {
		score -= c.Penalty
	}
	d.Score = clampScore(score)
	d.Contributors = sortContributors(contributors)
	d.Findings = findings
	d.Metrics = map[string]float64{
		"protectionBudgetMil":    pcbProtectMaxMil,
		"decapBudgetMil":         pcbDecapMaxMil,
		"protectionParts":        float64(len(prot)),
		"protectionJudged":       float64(protJudged),
		"protectionUnmatched":    float64(len(unmatchedNames)),
		"protectionTooFar":       float64(len(protOff)),
		"worstProtectionDistMil": round1(worstProt),
		"decapParts":             float64(decapTotal),
		"decapTooFar":            float64(len(offNames)),
		"worstDecapDistMil":      round1(worstDecap),
	}
	// 子分只在该族真的在场时才给——缺席的族给 0 会被读成「这族烂透了」，给 100 会被
	// 读成「这族完美」，两个都是谎。
	if protJudged > 0 {
		d.Metrics["protectionSubscore"] = clampScore(100 - 100*sumProtSev/float64(protJudged))
	}
	if decapTotal > 0 {
		d.Metrics["decapSubscore"] = clampScore(100 - 100*sumDecapSev/float64(decapTotal))
	}
	return d
}

// hasPortPart 报告板上是否存在对外端子（protection-absent 提示的前置条件）。
func hasPortPart(snap *boardSnapshot) bool {
	for _, c := range snap.Components {
		if isPortPart(c) {
			return true
		}
	}
	return false
}

// hasICPowerPad 报告板上是否有 IC 的电源引脚——「有 IC 却没有去耦候选」才值得降级，
// 一块只有连接器和电阻的板本来就无耦可去。
func hasICPowerPad(pads []pcbPadP) bool {
	for _, p := range pads {
		net := strings.TrimSpace(p.Net)
		if scoreICRe.MatchString(p.Designator) && net != "" && isGlobalNet(net) && !isGndNetName(net) {
			return true
		}
	}
	return false
}
