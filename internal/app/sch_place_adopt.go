package app

// sch_place_adopt.go — 「place 超时后的收编」:假失败定律在 place 上的缺口补丁。
//
// 真机复现(2026-08,工程 ceshi,block.esp32s3_wroom1_module):
//
//	failure: place U2 (mcu.esp32s3_wroom1): schematic.component.place failed:
//	         connector did not respond
//	rollback: complete=false verified=true attempted=1 survived=1 untracked=1
//	PARTIAL STATE: no reliable primitiveId for U2 (failed place may have created
//	               an untracked component)
//
// 三件事同时成立,缺一不可解释:
//
//  1. **超时 ≠ 没落地。** daemon 的 dispatch 超时只说明「回执没回来」,连接器侧
//     `eda.sch_PrimitiveComponent.create` 大概率**已经建好了件**(memory:
//     connector-wedge-fake-failure-under-load —— 「place 报超时但件已落」)。
//     Go 侧因此永远拿不到 primitiveId,回滚无从下手 → 残件。
//  2. **残件删不掉与「untracked」无关。** 同一份报文里 `attempted=1 survived=1`:
//     一个**正常拿到 id 的**器件也没删掉。所以那不是「未提交」态,是连接器
//     action 队列 wedge 期写操作(place/delete/document.open)整体被吞。
//  3. **于是残件会随重试繁殖**(一轮留下 U2/U2/U3),因为没有任何一条路径回头去
//     找「那次超时到底建没建件」。
//
// 本文件补的就是第 3 条:超时后做一次 settle 读,用**落地前快照的差集 + 下发坐标**
// 把刚出生的那个件认回来(收编),让它要么被继续使用、要么能被正常删掉。
//
// 判据只有三条硬门,顺序不可换:
//
//	① 新出现   —— primitiveId 不在「本命令开跑前的快照 ∪ 已成功放置的 id」里。
//	              这是**唯一**能保证不误收编页面上原有同型器件的门:一个早就在
//	              页上的同器件同坐标实例,天然在快照里,永远不会被认成新件。
//	② 是器件   —— componentType == "part";绝不把 flag/netport/图框认成放置件。
//	③ 坐标命中 —— 与本次下发的 (x,y) 相差 ≤ schAdoptTolerance。器件身份不能用
//	              uuid 认(memory: platform-delete-lies-and-pin-truth-table ——
//	              `sch list` 的 device.uuid 是 16 位符号 id,与 standard-parts 的
//	              32 位 deviceUuid 永远匹配不上,**用坐标匹配**)。
//
// 命中 1 个 = 收编。命中 ≥2 个 = 不收编,但**逐一点名**——它们同样是本命令
// 之后才出现的,交给调用方按名单清理,绝不再只打印一句 PARTIAL STATE 就放手。
//
// ── 命中 0 个:先证明回读新鲜,才准下「没落地」的结论(2026-08-20 修) ──
//
// 首版把「命中 0 个」直接判成「这次 place 确实没落地,页面上没有本次留下的残件」。
// 真机(2026-08-20,block.ch340c_usb_serial)证明这句话在**它唯一该起作用的场景里
// 系统性说反话**:
//
//	placed U3     ic.ch340c @ 690,460 [anchor]
//	adopt ✓ 落地前快照之后没有任何新器件出现在 (440,535) ±5 —— 这次 place 确实没有落地…
//	failure: place C8 (cap.100nf_0402): … connector did not respond
//	(清掉 U3 残件后重读页面)part C8 5e5803d829b1985d 440 535   ← C8 就在那儿
//
// 根因不是判据①②③错,而是**回读本身不成立**:让 place 超时的那个条件(连接器
// action 队列 wedge)同时让回读没有反映页面的当前状态 —— 要么 `components.list`
// 交回一份还没包含新件的旧快照,要么那次 `create` 此刻还堵在队列里、我们读完它才
// 落地。**两种机制在一次回读里长得一模一样**:都是「读得太早」。而
// 「什么都没读到」被当成了「什么都没发生」的证据。
//
// 于是本文件的不变式:
//
//	**一个「什么都没有」的回读,只有先证明它是新鲜的,才能当成证据。**
//
//	⓪ 新鲜度 —— 本命令此前**已经成功放置并拿到 primitiveId** 的那些器件(探针),
//	           必须一个不缺地出现在这次回读里。缺任何一个 = 这一读没反映当前页面
//	           = **禁止**得出「没落地」,降级为 uncertain(Uncertain=true)。
//
// 门⓪只管「命中 0 个」这一支:命中 ≥1 个时,回读里那个新器件本身就是它足够新鲜的
// 证明(它在快照时刻还不存在),不需要探针。
//
// 探针为什么只能是「本命令已落地的 id」:一次读得太早的回读 = 一份更早时刻的页面,
// 它照样包含**落地前快照里的全部 id**。所以拿快照 id 当探针**永远证明不了新鲜**
// (它只能识破「整页读空」这一种坏帧,而那种坏帧同时也会让探针全缺 —— 有探针时被
// 门⓪覆盖,没探针时本来就判 uncertain)。假装它能证明,等于把这次修复变成摆设。
//
// **anchor-first 失败的边界(第一件就超时,没有任何探针)**:探针档只能报
// uncertain。那一刻「place 没落地」与「回读读得太早」在观测上完全等价 —— 页面此时
// **本就应该**等于落地前快照,两种情形给出的字节完全相同,没有任何**探针**判据能分
// 开它们。这里不造一个看起来通用、实则证明不了的门:如实说不知道,并给出能执行的
// 下一步(schAdoptUncertainGuidance)。
//
// ── 两级判定:算术优先,探针兜底(2026-08-20,连接器 FIFO 上线后) ──────
//
// 上面整段之所以只能是启发式,根因是**写和随后的读之间没有 happens-before 关系**:
// 连接器把每条 WebSocket 消息交给各自的 onMessage 回调,`await` 不跨回调排队,于是
// 两条动作可以同时在飞(2026-08-20 用真 transport.ts 跑的探针实测:写还没 settle,
// 读的 handler 已经开跑,响应也先发出去了)。
//
// 连接器 1.0.3 把动作串成**一条 FIFO 链**,并在每条响应上带 `seq` /
// `seqAbandoned` / `unordered`。于是判定分两档:
//
//	算术档(强证据)—— 响应带序号:比较「W 下发之前观测到的 seqAbandoned」与
//	                  「回读响应上的 seqAbandoned」。没变 = 这中间没有动作被放弃
//	                  = FIFO 保证回读的 handler 在 W 的 handler settle 之后才开跑。
//	                  变了 = W 可能被放弃过(它还在跑,效果可能稍后才落地)→ 一律
//	                  uncertain。**anchor-first 无探针的场景在这一档能出结论**。
//
//	探针档(弱证据)—— 响应不带序号(连接器比 CLI 旧:侧载 .eext 与 CLI 严格同版,
//	                  但**市场装的那份会滞后若干 minor**),退回上面的门⓪启发式,
//	                  并在报文里明说这是弱证据 + 给出升级连接器的下一步。
//
// **绝不允许因为老连接器不带字段就默认「新鲜」** —— 那是把降级悄悄变成放行。
//
// ── 残余风险(诚实登记:seq **不能**证明什么) ────────────────────────
//
// 这一段是判据的边界,措辞不许越过它。本仓刚因为一句越界的自信结论
// (「页面上没有本次留下的残件」)吃过亏 —— 人照做就把残件永久留下了。
//
//  1. **seq 证明的是 handler 边界,不是文档已提交。** 它只说「W 的 handler 在 R 的
//     handler 开跑前就 settle 了」。`eda.*` 完全可能在 handler 返回之后才把改动写
//     进文档模型 —— 这一层(成因 3)我们**没有任何观测点**,只有官方能给保证。所以
//     「确实没有落地」的准确含义始终是:**在可证的最新一刻,那里没有新器件**。
//  2. **seq 不证明回读本身是对的。** 平台把一个已经存在的器件漏在
//     `components.list` 之外,序号照样是连续的。判定因此保留了一条交叉检查:
//     算术说新鲜、探针却缺席 = 自相矛盾 → 降级 uncertain,绝不放行。
//  3. **seq 不跨连接器重启。** 重连会新建队列、计数器归零。判定用「seq 必须严格
//     增长」识破这种重置(倒退即弃证);但如果重连之后又跑了足够多的动作把序号
//     追过基线,这条识破会失效。收编路径上重连后只会跑一两次回读,离基线很远,
//     所以实际风险极低 —— 但它是真实存在的洞,登记在此。
//  4. **旁路响应不是证据。** 走连接器旁路通道的纯诊断读带 `unordered: true`,
//     它的 seq 与 FIFO 无关,判定见到这个标记一律退回探针档。
//  5. **算术只覆盖「这条连接器 FIFO 上的先后」。** daemon 侧的重试、多客户端并发
//     写同一个窗口(concurrentWriter 咨询)都在它之外。

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// schAdoptTolerance 是收编的坐标容差(原理图单位 0.01inch)。放置坐标是我们自己
// 下发的,平台只做 5 单位栅格吸附,真件必落在 ±5 内。再放大就有把隔壁件认成
// 残件的风险 —— 块内相邻件的间距是数十单位量级,5 与它拉得开。
const schAdoptTolerance = 5.0

// schAdoptRequest 是「我们请求平台放了什么」——收编判据的输入侧。
type schAdoptRequest struct {
	Designator string  // 计划位号,仅用于报文可读性(平台会重编号,不作判据)
	X, Y       float64 // 本次下发的放置坐标 —— 判据③
}

// schAdoptVerdict 是一次收编判定的结果。
//
// Adopted 非空 = 唯一命中,调用方可以拿它的 primitiveId 当作这次 place 的产物。
// Candidates 是全部疑似残件(含 Adopted);≥2 时 Adopted 为空但名单照给。
// Fresh = 新鲜度门通过,**只有它成立时才准说「确实没有落地」**。
// Uncertain = 命中 0 个但新鲜度门没通过:回读不可信,什么都没证明。四个结局互斥:
// 收编 / 点名疑似 / 证实没落地 / uncertain。
// Tier 记这次新鲜度用了哪一档证据(算术 / 探针 / 无),报文据此决定措辞强度。
// Evidence 是那一档证据的一句话说明,供报文引用。
// MissingProbes 是缺席的探针 id —— uncertain 的**可归因证据**,报文要点名。
// Reason 永远可读,并且必须给出**能执行的下一步**(判据不许只说"不确定")。
type schAdoptVerdict struct {
	Adopted       *layoutComp
	Candidates    []layoutComp
	Fresh         bool
	Uncertain     bool
	Tier          schFreshnessTier
	Evidence      string
	MissingProbes []string
	Reason        string
}

// CandidateIDs 是 Candidates 的 primitiveId 列表(稳定排序)。
func (v schAdoptVerdict) CandidateIDs() []string {
	out := make([]string, 0, len(v.Candidates))
	for _, c := range v.Candidates {
		if c.ID != "" {
			out = append(out, c.ID)
		}
	}
	sort.Strings(out)
	return out
}

// schAdoptFreshness 是门⓪的**纯判定**:一次回读是否被证明反映了页面的当前状态。
//
// probes = 本命令此前已经成功放置并拿到 primitiveId 的器件。它们必然在页面上
// (平台回了 id = 建好了;放置阶段本命令不删任何东西),所以一次新鲜的回读必须把
// 它们一个不缺地带回来。
//
// 返回 (fresh, missing):fresh 只在**有探针且全员到齐**时为 true —— 没有探针
// 就是没有证据,绝不默认新鲜(那正是首版的错:把「读不到」当成「不存在」)。
// missing 去重 + 稳定排序,供报文点名。
func schAdoptFreshness(probes []string, live []layoutComp) (bool, []string) {
	present := make(map[string]bool, len(live))
	for _, c := range live {
		if c.ID != "" {
			present[c.ID] = true
		}
	}
	seen := map[string]bool{}
	var missing []string
	total := 0
	for _, id := range probes {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		total++
		if !present[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return total > 0 && len(missing) == 0, missing
}

// ── 两级新鲜度判定 ──────────────────────────────────────────────────────

// schFreshnessTier 记一次新鲜度判定用了哪一档证据。
type schFreshnessTier string

const (
	// schFreshProven —— 算术档:连接器的 FIFO 序号**证明**了先后关系。强证据。
	schFreshProven schFreshnessTier = "proven"
	// schFreshProbe —— 探针档:连接器不带序号(比 CLI 旧),只能用「本命令此前
	// 已落地的器件是否全在这一读里」推断。弱证据。
	schFreshProbe schFreshnessTier = "probe"
	// schFreshNone —— 什么都没证明:序号倒退(连接器重连过)、或算术与回读自相
	// 矛盾。这一档永远 Fresh=false。
	schFreshNone schFreshnessTier = "none"
)

// schSeqEvidence 是算术档的输入:W 下发之前观测到的计数器(Base),和这次回读
// 响应上带回来的计数器(Read)。两边都必须带字段、且都不是旁路响应,算术才成立。
type schSeqEvidence struct {
	Base schSeqCounters
	Read schSeqCounters
}

// usable 报告这对观测能不能支撑算术判定。
func (e schSeqEvidence) usable() bool {
	return e.Base.Known && e.Read.Known && !e.Base.Unordered && !e.Read.Unordered
}

// schFreshnessVerdict 是新鲜度门的结果。Fresh 只在**被证明**时为 true —— 没有
// 证据就是没有证据,绝不默认新鲜(那正是首版的错:把「读不到」当成「不存在」)。
type schFreshnessVerdict struct {
	Fresh   bool
	Tier    schFreshnessTier
	Missing []string
	Note    string
}

// schAdoptFreshnessTiered 是新鲜度门的**纯判定**,两档:
//
//	算术档 —— ev.usable():比较 seqAbandoned。没变 = FIFO 保证这次回读的 handler
//	          在那次 place 的 handler settle 之后才开跑(**强证据**,连探针都不需要
//	          —— 这就是 anchor-first 无探针场景第一次能出结论的原因)。
//	探针档 —— 连接器不带序号 / 回读走了旁路:退回 schAdoptFreshness 的启发式,
//	          **并把结论标成弱证据**。
//
// 算术说新鲜但探针缺席时**降级为 none**:序号连续只证明先后,不证明这一读没漏
// 器件;两个独立证据打架时,唯一诚实的答案是「不知道」。
func schAdoptFreshnessTiered(ev schSeqEvidence, probes []string, live []layoutComp) schFreshnessVerdict {
	probeFresh, missing := schAdoptFreshness(probes, live)

	if ev.usable() {
		switch {
		case ev.Read.SeqAbandoned < ev.Base.SeqAbandoned || ev.Read.Seq <= ev.Base.Seq:
			return schFreshnessVerdict{Tier: schFreshNone, Missing: missing, Note: fmt.Sprintf(
				"连接器顺序序号没有前进(基线 seq=%d/abandoned=%d → 回读 seq=%d/abandoned=%d)——"+
					"本命令中途连接器重连过、队列与计数器一起重置了,先后关系无从谈起",
				ev.Base.Seq, ev.Base.SeqAbandoned, ev.Read.Seq, ev.Read.SeqAbandoned)}
		case ev.Read.SeqAbandoned > ev.Base.SeqAbandoned:
			return schFreshnessVerdict{Tier: schFreshProven, Missing: missing, Note: fmt.Sprintf(
				"连接器在这中间放弃了 %d 个动作(%s)——被放弃的 handler 仍在跑,**它的效果可能稍后才落地**,"+
					"所以这段时间里关于那次 place 的任何结论都不成立",
				ev.Read.SeqAbandoned-ev.Base.SeqAbandoned, schAdoptAbandonedNames(ev.Read.AbandonedIDs))}
		case len(missing) > 0:
			return schFreshnessVerdict{Tier: schFreshNone, Missing: missing, Note: fmt.Sprintf(
				"两个证据打架:连接器序号说这一读在那次 place 之后(seq %d→%d,无动作被放弃),"+
					"但本命令此前已落地的 %d 个器件(%s)没出现在这一读里 —— 顺序可证不代表这一读没漏器件,"+
					"不放行",
				ev.Base.Seq, ev.Read.Seq, len(missing), strings.Join(missing, ", "))}
		default:
			return schFreshnessVerdict{Fresh: true, Tier: schFreshProven, Note: fmt.Sprintf(
				"顺序可证:连接器 FIFO 序号 %d→%d 且 seqAbandoned 始终为 %d(无动作被放弃)——"+
					"这次回读的 handler 在那次 place 的 handler settle 之后才开跑",
				ev.Base.Seq, ev.Read.Seq, ev.Read.SeqAbandoned)}
		}
	}

	return schFreshnessVerdict{
		Fresh:   probeFresh,
		Tier:    schFreshProbe,
		Missing: missing,
		Note:    schAdoptProbeTierNote(ev, probes),
	}
}

// schAdoptProbeTierNote 说明**为什么**这次只能用探针档 —— 三种成因指向的下一步
// 不同,不许糊成一句「不支持」。
func schAdoptProbeTierNote(ev schSeqEvidence, probes []string) string {
	const upgrade = "升级连接器后这条判定可升为可证:侧载装的重导与 CLI 同版的 .eext" +
		"(卸载旧的 → 导入新的 → **完全退出并重启 EasyEDA**);市场装的会原地自动更新,但可能滞后 CLI"
	switch {
	case ev.Read.Known && ev.Read.Unordered:
		return "这次回读的响应走了连接器旁路通道(unordered),它的 seq 与 FIFO 无关、不构成顺序证据 ——" +
			"退回探针启发式,判定为**弱证据**"
	case !ev.Read.Known:
		return "当前连接器不支持顺序证明(响应不带 seq/seqAbandoned)——退回探针启发式,判定为**弱证据**。" + upgrade
	case !ev.Base.Known:
		return fmt.Sprintf("本命令此前没有任何带顺序证据的响应可作基线(已落地 %d 件)——退回探针启发式,"+
			"判定为**弱证据**", schAdoptProbeCount(probes))
	default:
		return "顺序证据不完整 —— 退回探针启发式,判定为**弱证据**。" + upgrade
	}
}

// schAdoptAbandonedNames 把被放弃的 request id 列表变成可读的一句;为空时明说
// 「连接器没给名单」,不留一个空括号让人以为是 bug。
func schAdoptAbandonedNames(ids []string) string {
	if len(ids) == 0 {
		return "连接器未给出 id 名单"
	}
	return "最近被放弃的: " + strings.Join(ids, ", ")
}

// schAdoptOrphanPlacement 是收编的**纯判定**:给定落地前的已知 id 集合、新鲜度
// 探针与顺序证据、一次活体回读、以及本次放置请求,判断哪些器件是这次 place 生出
// 来的孤儿。
//
// known 必须同时包含「命令开跑前页面上的全部器件」和「本命令此前已成功放置并拿到
// id 的器件」——少了后者,前面几件成功的放置会被当成孤儿。
// probes + ev 一起决定新鲜度门(见 schAdoptFreshnessTiered):**只有它证明回读
// 新鲜,「命中 0 个」才准被读作「没落地」**;否则一律 uncertain。
// ev 全零值 = 老连接器,自动退回探针档 —— 老调用方不改代码也不会误升级证据。
func schAdoptOrphanPlacement(known map[string]bool, probes []string, ev schSeqEvidence, live []layoutComp, req schAdoptRequest) schAdoptVerdict {
	freshness := schAdoptFreshnessTiered(ev, probes, live)
	freshRead, missingProbes := freshness.Fresh, freshness.Missing
	var fresh []layoutComp
	for _, c := range live {
		if c.ID == "" || known[c.ID] {
			continue // 门①:不是本次新出现的
		}
		if c.ComponentType != "" && c.ComponentType != schLayoutPartType {
			continue // 门②:flag / netport / 图框都不是放置件
		}
		if !c.AnchorAvailable {
			continue // 没有可信坐标 → 判不了门③,不猜
		}
		if math.Abs(c.X-req.X) > schAdoptTolerance || math.Abs(c.Y-req.Y) > schAdoptTolerance {
			continue // 门③:不在下发坐标附近
		}
		fresh = append(fresh, c)
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].ID < fresh[j].ID })

	switch len(fresh) {
	case 0:
		// 新鲜度门:回读没被证明新鲜 → 这一读什么也没证明。**不许**说「没落地」。
		if !freshRead {
			return schAdoptVerdict{
				Uncertain:     true,
				Tier:          freshness.Tier,
				Evidence:      freshness.Note,
				MissingProbes: missingProbes,
				Reason:        schAdoptUncertainReason(req, freshness),
			}
		}
		return schAdoptVerdict{
			Fresh:    true,
			Tier:     freshness.Tier,
			Evidence: freshness.Note,
			Reason:   schAdoptNoResidueReason(req, freshness, probes),
		}
	case 1:
		// 命中本身就是回读足够新鲜的证明:这个件在落地前快照时刻还不存在。
		c := fresh[0]
		desig := strings.TrimSpace(c.Designator)
		if desig == "" {
			desig = "(无位号)"
		}
		return schAdoptVerdict{
			Adopted:    &c,
			Candidates: fresh,
			Fresh:      true,
			Tier:       freshness.Tier,
			Evidence:   freshness.Note,
			Reason: fmt.Sprintf(
				"place 回执丢了但器件已落地:%s 是落地前快照之后唯一新出现在 (%.0f,%.0f) 的器件,按 id %s 收编",
				desig, req.X, req.Y, c.ID),
		}
	default:
		return schAdoptVerdict{
			Candidates: fresh,
			Fresh:      true,
			Tier:       freshness.Tier,
			Evidence:   freshness.Note,
			Reason: fmt.Sprintf(
				"(%.0f,%.0f) 附近同时出现 %d 个新器件(%s)—— 无法判定哪个是本次 place 的产物,不做收编;"+
					"它们都是本命令开跑后才出现的,已逐一点名交清理",
				req.X, req.Y, len(fresh), strings.Join(schAdoptIDs(fresh), ", ")),
		}
	}
}

// schAdoptNoResidueReason 是「命中 0 个 + 新鲜度门通过」的说法。措辞强度**必须**
// 跟着证据档走:算术档说「可证」,探针档必须自曝是弱证据并给出升级路径。
//
// 两档共有的边界(不许省):这句话的准确含义是**在可证的最新一刻,那里没有新器件**
// —— 它证明不了平台内部有没有一次尚未提交的写(成因 3,我们没有观测点)。
func schAdoptNoResidueReason(req schAdoptRequest, f schFreshnessVerdict, probes []string) string {
	head := fmt.Sprintf(
		"落地前快照之后没有任何新器件出现在 (%.0f,%.0f) ±%.0f —— 这次 place 确实没有落地,"+
			"页面上没有本次留下的残件", req.X, req.Y, schAdoptTolerance)
	if f.Tier == schFreshProven {
		return head + fmt.Sprintf("(强证据:%s;准确含义是「在可证的最新一刻那里没有新器件」——"+
			"它不证明平台内部没有一次尚未提交的写)", f.Note)
	}
	return head + fmt.Sprintf("(**弱证据**:%s;判据只是本命令此前落地的 %d 件全在这一读里 ——"+
		"这只说明这一读不早于最后一次成功 place,证明不了它不早于**失败那次 place 的效果**)",
		f.Note, schAdoptProbeCount(probes))
}

// schAdoptProbeCount 数去重后的非空探针。
func schAdoptProbeCount(probes []string) int {
	seen := map[string]bool{}
	for _, id := range probes {
		if id = strings.TrimSpace(id); id != "" {
			seen[id] = true
		}
	}
	return len(seen)
}

// schAdoptUncertainReason 是新鲜度门没通过时的说法。**绝不能出现「确实没有落地」
// 「没有残件」这类字样** —— 那正是首版说反话的那句。四种成因分开写,因为它们指向
// 的下一步不同:被放弃过 = 那次写可能稍后才落地;序号重置 = 连接器重连过;
// 缺探针 = 这次读到的页面是旧的;没探针 = 无从证明。
func schAdoptUncertainReason(req schAdoptRequest, f schFreshnessVerdict) string {
	tail := fmt.Sprintf("所以「(%.0f,%.0f) ±%.0f 附近没有新器件」不构成任何证据,"+
		"这次 place 到底落没落地**无法判断**", req.X, req.Y, schAdoptTolerance)

	// 算术档判出「被放弃过」——这是最强的一种「判不了」:我们**知道**连接器丢下过
	// 一个还在跑的 handler,它的效果可能稍后才落地。
	if f.Tier == schFreshProven {
		return "顺序证据作废:" + f.Note + " —— " + tail
	}
	// 序号倒退 / 算术与探针自相矛盾。
	if f.Tier == schFreshNone {
		return "回读不可信:" + f.Note + " —— " + tail
	}
	if len(f.Missing) > 0 {
		return fmt.Sprintf(
			"回读不可信:本命令此前已落地的器件有 %d 个没出现在这次 components.list 里(%s)——"+
				"这一读没有反映页面的当前状态,%s(典型成因:连接器 action 队列 wedge)。%s",
			len(f.Missing), strings.Join(f.Missing, ", "), tail, f.Note)
	}
	return fmt.Sprintf(
		"回读无法证明新鲜:本命令此前没有任何已落地的器件可以当探针(失败的就是第一件),"+
			"而此刻「页面等于落地前快照」既可能是真没落地,也可能是回读读得太早 —— 两者在观测上完全等价,"+
			"%s;(%.0f,%.0f) ±%.0f 按可能有残件处理。%s",
		tail, req.X, req.Y, schAdoptTolerance, f.Note)
}

// schAdoptTierNotice 把「这次判定用了哪一档证据」写进报文。弱证据档**必须**给出
// 升级连接器的下一步 —— 否则用户永远不知道自己在用一个可证判据的降级版,而降级
// 版恰恰在最需要它的场景(连接器 wedge)里最不可靠。
func schAdoptTierNotice(w io.Writer, v schAdoptVerdict) {
	if w == nil || v.Evidence == "" {
		return
	}
	switch v.Tier {
	case schFreshProven:
		fmt.Fprintf(w, "  证据档:可证(连接器 FIFO 顺序序号)—— %s\n", v.Evidence)
	case schFreshProbe:
		fmt.Fprintf(w, "  证据档:弱(探针启发式)—— %s\n", v.Evidence)
	default:
		fmt.Fprintf(w, "  证据档:无 —— %s\n", v.Evidence)
	}
}

func schAdoptIDs(comps []layoutComp) []string {
	out := make([]string, 0, len(comps))
	for _, c := range comps {
		out = append(out, c.ID)
	}
	return out
}

// schPageComponentSnapshot 读活动页的全部器件 id(任意 componentType),作为收编的
// 「落地前快照」。返回 nil 表示**读失败**——调用方必须据此关掉收编:没有快照就
// 分不清「新出现的件」和「本来就在的件」,那时按坐标猜等于允许误删。
func schPageComponentSnapshot(comps []layoutComp) map[string]bool {
	out := make(map[string]bool, len(comps))
	for _, c := range comps {
		if c.ID != "" {
			out[c.ID] = true
		}
	}
	return out
}

// schAdoptRead 做收编所需的活体回读,并走 settleRead —— 平台的写入不是同步生效的,
// 紧跟一次写的回读「读不到」不构成结论(settle_read.go 的教训)。
//
// 满足条件是「**命中了疑似残件** 或 **回读被证明新鲜**」。两半都是必需的:
//   - 只有前半(首版)时,一次 stale 回读会把 settle 预算耗光,然后交出**错误**的
//     「确实没落地」——重试的代价照付,结论照样是反的。
//   - 加上后半,stale 回读会被重读一拍(给平台落定的机会),仍然 stale 就诚实地
//     交出 uncertain。
//
// 回读范围与快照一致(活动页):create 落在活动页,两侧同尺才使差集有意义。
//
// base 是**失败那次 place 下发之前**观测到的连接器计数器(conn_seq.go 在派发咽喉
// 上记的)。它必须在这里之前就取好:回读自己的响应会覆盖那个记录,取晚了基线就
// 变成回读自己,算术当场退化成恒等式。
func schAdoptRead(cfg *appConfig, window string, known map[string]bool, probes []string,
	base schSeqCounters, req schAdoptRequest) (schAdoptVerdict, error) {
	type readOut struct {
		verdict schAdoptVerdict
		err     error
	}
	out, _, _ := settleRead(func() (readOut, bool, error) {
		res, err := requestAction(cfg, "schematic.components.list", window, map[string]any{})
		if err != nil {
			return readOut{err: fmt.Errorf("收编回读: %w", err)}, false, err
		}
		comps, perr := parseLayoutComps(res.Result)
		if perr != nil {
			return readOut{err: fmt.Errorf("收编回读: %w", perr)}, false, perr
		}
		v := schAdoptOrphanPlacement(known, probes, schSeqEvidence{Base: base, Read: res.Seq}, comps, req)
		return readOut{verdict: v}, len(v.Candidates) > 0 || v.Fresh, nil
	})
	if out.err != nil {
		return schAdoptVerdict{}, out.err
	}
	return out.verdict, nil
}

// schAdoptResidueGuidance 打印残件清理处方。**必须给能执行的下一步** —— 真机上
// 这些残件之所以能攒到三个,就是因为报文只说了「PARTIAL STATE」而没人知道删什么。
func schAdoptResidueGuidance(w io.Writer, ids []string) {
	if len(ids) == 0 {
		return
	}
	fmt.Fprintf(w, "  残件清理(这些 id 是本命令开跑后才出现的,删它们不会碰到页面原有器件):\n")
	fmt.Fprintf(w, "    easyeda sch prim-delete --ids %s\n", strings.Join(ids, ","))
	fmt.Fprintln(w, "  删不动时是连接器 action 队列 wedge(此期间 place/delete/document.open 会整体被吞,")
	fmt.Fprintln(w, "  而轻读照常):先 `easyeda sch save`,完全退出并重启 EasyEDA,再重跑上面的 prim-delete。")
}

// schAdoptUncertainGuidance 打印「回读不可信 → 判不了」时的处方。
//
// 本仓铁律:判据必须给**能执行的下一步**,不许只说「不确定」。uncertain 比
// 「证实没落地」更需要这条 —— 它把「去看一眼」的责任交回给人,那就必须告诉人
// 看哪里、怎么看、看到了怎么办。步骤顺序有意义:先 save 钉住已落地的东西,再
// 重启清掉 wedge(否则后面的读还是不可信),然后才查坐标、按结果二选一。
func schAdoptUncertainGuidance(w io.Writer, req schAdoptRequest, missingProbes []string) {
	fmt.Fprintln(w, "  判不了时的下一步(按顺序,每条都能直接跑):")
	fmt.Fprintln(w, "    1) easyeda sch save                 # 先把已经落地的东西钉住")
	fmt.Fprintln(w, "    2) 完全退出并重启 EasyEDA            # 清掉连接器 action 队列的 wedge,此后回读才可信")
	fmt.Fprintf(w, "    3) easyeda sch list                 # 看 (%.0f,%.0f) ±%.0f 有没有多出来的器件\n",
		req.X, req.Y, schAdoptTolerance)
	fmt.Fprintln(w, "    4) 有 → easyeda sch prim-delete --ids <那个 id> 清掉再重跑;没有 → 这次 place 确实没落地,直接重跑")
	if len(missingProbes) > 0 {
		fmt.Fprintf(w, "  (回读不可信的证据:本命令此前已落地的 %s 没出现在这次回读里)\n",
			strings.Join(missingProbes, ", "))
	}
}
