package app

// pcb_score_rf.go — 射频维（dimRF）：天线馈线长度。
//
// #167 给这一维的定义是「RF 路径短 + keepout 全层」。keepout 那半边**这里测不了**，
// 原因和处理见文件末尾的「keepout 为什么不在这里算」一节 —— 结论是本维恒为
// degraded，Reason 里明说漏了哪半边，绝不假装体检完整。
//
// 能测的那半边是馈线长度：天线馈点到 RF 源（主芯片 RF 脚 / 匹配网络第一个元件）的
// 距离。2.4GHz 在 FR-4 微带上波长 ≈50mm，馈线一旦长到波长的量级，它本身就是一根
// 失配的辐射体 —— 损耗、驻波、EMI 全从这里来。这个量纯几何、只需要焊盘坐标，是
// 快照里现成就有的。
//
// ── 一个必须写死在注释里的陷阱 ─────────────────────────────────────────────
//
// **不要用 ctx.layout 的 ratsnest 来找馈线。** analyzePcbLayout 的 ratsnest 只跑
// 信号网，它在建网桶时用 isGlobalNet 过滤掉了 GND/电源。而 isGlobalNet 的正则相当
// 宽（reGlobalNet1 里有 `^[+-]`、`^v(cc|dd|ss|in|out|bus|bat|sys|ref)\b`，
// reGlobalNet2 更是「网名里出现 gnd/vcc/vdd/vss 就算」），一个叫 `+RF` 或
// `RF_VDD` 的馈线网会被整条吞掉 —— 这一维会**静默地什么都测不到**，然后因为
// 「没有可测的天线」被标成 skipped，看起来像板子没有 RF，实际上是我们瞎了。
//
// 所以馈线一律自己从 ctx.snap.netPads() 取天线焊盘所在的网，不经过任何全局网过滤。
// 本维压根不读 ctx.layout（单测里 layout 传 nil 也必须能跑，那就是这条约定的机械
// 证明）。判「哪个脚是馈点」时也不用 isGlobalNet，只排掉**地**（rfIsGroundNet），
// 因为馈点是信号脚，而地脚才是要排除的那些。

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

// ---------------------------------------------------------------------------
// 阈值
// ---------------------------------------------------------------------------

// 馈线长度分档（mil）。**三个都是待校准初值**，出处如下：
//
//	2.4GHz 自由空间波长 125mm；FR-4 微带 εeff≈2.4 → 板上波长 λ≈51mm ≈ 2000mil。
//	射频工程里「电气短」的经验门槛是 λ/10 —— 短于它的一段线可以当集总元件看，
//	不会显著失配。λ/10 ≈ 200mil ≈ 5mm，与实测的嘉立创 ESP32 模组板（馈线 2–8mm）
//	吻合，所以 200mil 以内不扣分。
//	λ/4 ≈ 500mil 是另一个物理拐点：到这个长度线段本身开始像个辐射体/阻抗变换器，
//	必须明确扣分。
//	λ ≈ 2000mil 时天线基本被扔到了板子另一头，射频指标已经没有讨论价值 → 扣满。
//
// Metrics 里同时暴露 feedLenMil / worstFeedLenMil 原始量，就是为了后续拿真板回来
// 校准这三个数（#167 第五层）：好板的实测馈线长度分布应当整体落在 ideal 以内。
const (
	rfFeedIdealMil  = 200.0  // λ/10：不扣分区
	rfFeedBudgetMil = 500.0  // λ/4：预算线，越过开始重扣
	rfFeedMaxMil    = 2000.0 // λ：扣满
)

// rfSoftPenalty 是 ideal→budget 这段「可以更短但还没坏」的最大扣分。分两段是因为
// 3mm 和 12mm 的性质完全不同：前者只是不够漂亮，后者已经影响指标，用一条直线拉过去
// 会让前者扣得过重、后者扣得过轻。
const rfSoftPenalty = 30.0

// rfFeedPenalty 把一条馈线的长度映射成 0-100 的扣分（分段线性、单调不减）。
func rfFeedPenalty(lenMil float64) float64 {
	switch {
	case lenMil <= rfFeedIdealMil:
		return 0
	case lenMil <= rfFeedBudgetMil:
		return rfSoftPenalty * (lenMil - rfFeedIdealMil) / (rfFeedBudgetMil - rfFeedIdealMil)
	case lenMil >= rfFeedMaxMil:
		return 100
	default:
		return rfSoftPenalty + (100-rfSoftPenalty)*(lenMil-rfFeedBudgetMil)/(rfFeedMaxMil-rfFeedBudgetMil)
	}
}

// ---------------------------------------------------------------------------
// 器件识别
// ---------------------------------------------------------------------------

// rfIsGroundNet 判「这是地网吗」。
//
// 刻意**不复用 isGlobalNet**：那个函数的职责是「这个网连着全板、不该拿来聚类」，
// 口径宽到会把馈线网一起吞掉（见文件头的陷阱）。这里只要排掉地脚，多排一个就等于
// 把馈点判丢，宁可窄。
func rfIsGroundNet(net string) bool {
	n := strings.ToUpper(strings.TrimSpace(net))
	if n == "" {
		return false
	}
	return strings.Contains(n, "GND") || strings.Contains(n, "GROUND") ||
		n == "EARTH" || n == "VSS" || strings.HasPrefix(n, "VSS_")
}

// rfDeviceOf 取用于天线识别的器件名：Device（manufacturerId 优先）为空时退回 Name。
// placed 件的 name 常是 "={Manufacturer Part}" 模板串，所以顺序不能反。
func rfDeviceOf(c boardComp) string {
	if d := strings.TrimSpace(c.Device); d != "" {
		return d
	}
	return strings.TrimSpace(c.Name)
}

// rfPartMatches 判一条 spec.rf.parts 声明是否指向这个器件：位号全等，或器件名包含
// 该串。长度 <3 的声明只允许位号全等 —— 否则 "U" 这种写法会把满板器件全圈进来。
func rfPartMatches(c boardComp, want string) bool {
	w := strings.ToUpper(strings.TrimSpace(want))
	if w == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(c.Designator), w) {
		return true
	}
	if len(w) < 3 {
		return false
	}
	return strings.Contains(strings.ToUpper(rfDeviceOf(c)), w)
}

// specRFParts 取 spec 声明的 RF 器件列表（去空白、去空项）。
func specRFParts(s *spec.Spec) []string {
	if s == nil || s.RF == nil {
		return nil
	}
	out := make([]string, 0, len(s.RF.Parts))
	for _, p := range s.RF.Parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// specFeedNet 取 spec 声明的馈线网名（rf.feed）。它是绕开一切启发式的**直接指定**：
// 用户说了哪个网是馈线，就按那个网找馈点，连 rfIsGroundNet 都不再插手。
func specFeedNet(s *spec.Spec) string {
	if s == nil || s.RF == nil {
		return ""
	}
	return strings.TrimSpace(s.RF.Feed)
}

// rfAntennas 挑出板上的天线/RF 器件。
//
// 优先级：spec 显式声明 > 关键词启发式。理由同 spec.Interface 的注释 —— 板级决定
// 比类别经验更具体。spec 一旦声明，它就是**权威名单**（不再叠加启发式结果），但
// 启发式看到名单外的天线件会单独出一条 INFO，因为那大概率是 spec 漏写而不是误判。
//
// 关键词表（isAntennaDevice）是按**模组**名写的，裸芯片名撞上同串时会被一起判成
// 天线（"ESP8266EX" 撞 "ESP8266"），于是馈线网上全是天线、找不到 RF 源 → 本维
// skipped。这正是 spec.rf.parts 存在的价值：一句显式声明就能把 RF 源还回来。
// 不在本维私自放宽判据 —— 那会让 pcb check 的 antenna-keepout 和这里对"谁是天线"
// 给出两种答案，这个项目吃过两套引擎长期矛盾的亏。
func rfAntennas(comps []boardComp, s *spec.Spec) (ants []boardComp, source string, unmatched []string, extras []string) {
	declared := specRFParts(s)
	if len(declared) > 0 {
		hit := map[string]bool{}
		for _, c := range comps {
			for _, w := range declared {
				if rfPartMatches(c, w) {
					ants = append(ants, c)
					hit[w] = true
					break
				}
			}
		}
		for _, w := range declared {
			if !hit[w] {
				unmatched = append(unmatched, w)
			}
		}
		if len(ants) > 0 {
			inList := map[string]bool{}
			for _, a := range ants {
				inList[a.Designator] = true
			}
			for _, c := range comps {
				if !inList[c.Designator] && isAntennaDevice(rfDeviceOf(c), c.Designator) {
					extras = append(extras, c.Designator)
				}
			}
			sort.Strings(extras)
			return ants, "spec", unmatched, extras
		}
		// 声明了却一个都没匹配上（位号漂移 / 器件还没放）：退回启发式，但把
		// unmatched 带出去，报告里要说清楚 spec 和板子对不上。
	}
	for _, c := range comps {
		if isAntennaDevice(rfDeviceOf(c), c.Designator) {
			ants = append(ants, c)
		}
	}
	return ants, "heuristic", unmatched, nil
}

// ---------------------------------------------------------------------------
// 馈线
// ---------------------------------------------------------------------------

// rfFeed 是一根解析出来的馈线。
type rfFeed struct {
	ant     string  // 天线位号
	net     string  // 馈线网名
	padNum  string  // 天线侧焊盘号（不含位号前缀）
	src     string  // RF 源侧「位号.脚号」
	lenMil  float64 // 馈点到 RF 源的直线距离
	x, y    float64 // 馈点坐标（finding 的 At）
	penalty float64
}

// padOwner 从 netPads() 桶里的焊盘号还原所属位号。netPads 把 Number 改写成
// "U1.3" 形式好让焊盘可回指器件（pcb_board_snapshot.go），这里做反向还原。
// 位号本身不含 "."，所以按第一个 "." 切一刀是安全的。
func padOwner(number string) (des, pin string) {
	if i := strings.Index(number, "."); i >= 0 {
		return number[:i], number[i+1:]
	}
	return number, ""
}

// resolveRFFeed 给一个天线器件算出它的馈线。
//
// 馈点 = 天线的信号脚（非地脚；spec.rf.feed 指定了网名时以它为准）。RF 源 = 与该脚
// **同网**的、属于**别的非天线器件**的最近焊盘 —— 中间隔着匹配网络（L/C）时量到的
// 就是「天线 → 匹配网络第一个元件」这一跳，那恰恰是物理上最该短的一段。
//
// 多个信号脚时取「离各自 RF 源最近」的那条：一根 IPEX 座有信号脚 + 若干壳地脚，
// 真正的馈点只有一个，最短的那条就是它。
//
// ok=false 表示这根天线量不出馈线（脚上没网 = 原理图还没接；同网找不到对端 = 网上
// 只有天线自己）。这种天线**不扣分**——那是网表问题不是布局问题，pcb check 的网表
// 规则管它——但会被计进 feedsUnresolved 并出一条 INFO，绝不静默吞掉。
func resolveRFFeed(ant boardComp, netPads map[string][]boardPad, antSet map[string]bool, feedNet string) (rfFeed, bool) {
	best := rfFeed{ant: ant.Designator}
	found := false
	for _, p := range ant.Pads {
		if strings.TrimSpace(p.Net) == "" {
			continue
		}
		if feedNet != "" {
			// 显式指定：只认这个网，其余脚一概不看。
			if !strings.EqualFold(p.Net, feedNet) {
				continue
			}
		} else if rfIsGroundNet(p.Net) {
			continue // 地脚不是馈点
		}
		for _, q := range netPads[p.Net] {
			owner, pin := padOwner(q.Number)
			if owner == ant.Designator || antSet[owner] {
				continue // 自己的脚、以及另一根天线，都不是 RF 源
			}
			d := math.Hypot(q.X-p.X, q.Y-p.Y)
			if !found || d < best.lenMil {
				best = rfFeed{
					ant: ant.Designator, net: p.Net, padNum: p.Number,
					src: owner + "." + pin, lenMil: d, x: p.X, y: p.Y,
				}
				found = true
			}
		}
	}
	if !found {
		return rfFeed{ant: ant.Designator}, false
	}
	best.lenMil = round2(best.lenMil)
	best.penalty = rfFeedPenalty(best.lenMil)
	return best, true
}

// ---------------------------------------------------------------------------
// 维度
// ---------------------------------------------------------------------------

type rfDimScorer struct{}

func (rfDimScorer) id() string { return dimRF }

func init() { registerDimScorer(rfDimScorer{}) }

func (rfDimScorer) score(ctx *scoreCtx) scoreDimension {
	opts := layoutScoreOpts{}
	if ctx != nil {
		opts = ctx.opts
	}
	if ctx == nil || ctx.snap == nil {
		return skipDimension(dimRF, opts, "no board snapshot")
	}
	snap := ctx.snap

	ants, source, unmatched, extras := rfAntennas(snap.Components, ctx.spec)
	if len(ants) == 0 {
		// 绝大多数板没有 RF —— 这不是缺陷，但也绝不能给满分（"没测" ≠ "测了满分"）。
		reason := "no antenna/RF part on the board (device keywords WROOM/WROVER/ANTENNA/2450AT/ANT-SMD or an ANT* designator)"
		if len(unmatched) > 0 {
			reason = fmt.Sprintf("spec rf.parts declares %s but no placed component matches, and no antenna was detected heuristically",
				strings.Join(unmatched, ", "))
		}
		return skipDimension(dimRF, opts, "%s", reason)
	}

	antSet := map[string]bool{}
	for _, a := range ants {
		antSet[a.Designator] = true
	}
	netPads := snap.netPads()
	feedNet := specFeedNet(ctx.spec)

	var findings []pcbCheckFinding
	var feeds []rfFeed
	var unresolved []string
	for _, a := range ants {
		f, ok := resolveRFFeed(a, netPads, antSet, feedNet)
		if !ok {
			unresolved = append(unresolved, a.Designator)
			// 归因要说清是「哪一步断的」：spec 指定了馈线网时，最常见的失败是那个
			// 网名和板上实际网名对不上，笼统一句"没接"会把人引到错的地方去查。
			why := "no signal pad carries a net shared with a non-antenna part (馈线还没画,或天线网是孤立的)"
			if feedNet != "" {
				why = fmt.Sprintf("no pad on the spec-declared feed net %q reaches a non-antenna part (网名对不上,或馈线还没画)", feedNet)
			}
			findings = append(findings, pcbCheckFinding{
				Type: "rf-feed-unresolved", Level: "INFO", Designator: a.Designator,
				Message: fmt.Sprintf("antenna %s has no measurable feed: %s — feed length not scored for it", a.Designator, why),
			})
			continue
		}
		feeds = append(feeds, f)
	}

	if len(feeds) == 0 {
		// 有天线但一根馈线都量不出来：这一维**没有测到任何东西**，必须 skipped
		// （不是 0 分也不是 100 分）。findings/metrics 仍然挂上去 —— 「板上有天线
		// 但它没接」这条信息比分数本身还重要，丢了就再没人看得见。
		sk := skipDimension(dimRF, opts,
			"found %d antenna/RF part(s) (%s) but none has a measurable feed net — nothing to score",
			len(ants), strings.Join(unresolved, ", "))
		sk.Findings = findings
		sk.Metrics = map[string]float64{
			"rfParts":         float64(len(ants)),
			"feedsResolved":   0,
			"feedsUnresolved": float64(len(unresolved)),
			"keepoutChecked":  0,
		}
		return sk
	}

	d := newDimension(dimRF, opts)

	// worst-case 而非平均：一根过长的馈线就足以毁掉整块板的射频指标，取平均会被
	// 其它几根短馈线稀释成"还行"。精修环的梯度不因此丢失 —— 修掉最差的那根之后
	// 次差的自动变成 worst，是一条正常收敛的下降链。
	var worst, total float64
	for _, f := range feeds {
		total += f.lenMil
		if f.penalty > worst {
			worst = f.penalty
		}
		if f.penalty > 0 {
			d.Contributors = append(d.Contributors, scoreContributor{
				Designator: f.ant,
				Penalty:    round2(f.penalty),
				Detail: fmt.Sprintf("feed %s(%s) → %s: %.0f mil (%.1f mm), budget %.0f mil",
					f.padNum, f.net, f.src, f.lenMil, f.lenMil/mmToMil, rfFeedBudgetMil),
			})
			lvl := "INFO"
			if f.lenMil > rfFeedBudgetMil {
				lvl = "WARN"
			}
			findings = append(findings, pcbCheckFinding{
				Type: "rf-feed-length", Level: lvl, Designator: f.ant, Net: f.net,
				At: &pcbXY{X: f.x, Y: f.y},
				Message: fmt.Sprintf("RF feed %s.%s(%s) → %s runs %.0f mil (%.1f mm) — 射频馈线越长损耗和辐射越大,目标 ≤%.0f mil(λ/10),预算 %.0f mil(λ/4)%s",
					f.ant, f.padNum, f.net, f.src, f.lenMil, f.lenMil/mmToMil,
					rfFeedIdealMil, rfFeedBudgetMil, docRule("3.1", "布局优先级 — 板边接口(含天线)先定位")),
			})
		}
	}
	d.Contributors = sortContributors(d.Contributors)
	d.Score = clampScore(100 - worst)

	var worstLen float64
	for _, f := range feeds {
		if f.lenMil > worstLen {
			worstLen = f.lenMil
		}
	}
	d.Metrics = map[string]float64{
		"rfParts":         float64(len(ants)),
		"feedLenMil":      round2(total), // 已解析馈线长度之和（单天线时即那一根）
		"worstFeedLenMil": round2(worstLen),
		"feedsResolved":   float64(len(feeds)),
		"feedsUnresolved": float64(len(unresolved)),
		"keepoutChecked":  0, // 见下方「keepout 为什么不在这里算」
	}

	// ── keepout 为什么不在这里算 ──────────────────────────────────────────
	// 判天线 keepout 覆盖需要 no-copper region 列表（pcb.region.list），而
	// BoardSnapshot 没有拉 region。维度实现**必须是纯函数**：一旦在这里偷偷发一次
	// action，金标准 fixture（离线快照重放）就再也复现不出同一个分数，#167 第五层
	// 的校准回归当场作废。所以本维只算馈线，keepout 由 pcb check 的 antenna-keepout
	// 规则（findAntennaKeepout，要求 top+bottom+inner 全覆盖）负责。
	//
	// 但「这半边没测」必须让人看见：本维因此**恒为 degraded**，keepoutChecked 恒
	// 为 0。这不是噪声而是持续提醒 —— 一块天线底下铺满铜的板，在这一维照样可能拿
	// 高分，报告必须自己把这句话说出来。
	d.Status = dimDegraded
	d.Reason = fmt.Sprintf("only feed length is scored: keepout coverage needs region data (pcb.region.list) that the board snapshot does not carry — run `pcb check` for the antenna-keepout rule. Feed length is the straight-line pad-to-pad distance (a lower bound; real routing is longer). Antennas detected via %s.", source)
	if kl := rfKeepoutLayers(ctx.spec); kl != "" {
		d.Reason += fmt.Sprintf(" spec declares rf.keepoutLayers=%q — unverified here.", kl)
	}
	if len(unmatched) > 0 {
		d.Reason += fmt.Sprintf(" spec rf.parts entries with no placed match: %s.", strings.Join(unmatched, ", "))
	}
	for _, e := range extras {
		findings = append(findings, pcbCheckFinding{
			Type: "rf-part-undeclared", Level: "INFO", Designator: e,
			Message: fmt.Sprintf("%s looks like an antenna part but is not listed in spec rf.parts — 该维只按 spec 名单打分,漏写的天线不会被计入", e),
		})
	}
	d.Findings = findings
	return d
}

// rfKeepoutLayers 取 spec 声明的 keepout 层意图（仅用于在 Reason 里如实转述「用户
// 声明了但我们没验」，本维不消费它做判定）。
func rfKeepoutLayers(s *spec.Spec) string {
	if s == nil || s.RF == nil {
		return ""
	}
	return strings.TrimSpace(s.RF.KeepoutLayers)
}
