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
// 命中 0 个 = 这次 place **确实没落地**(负对照:绝不凭空造一个 id)。
// 命中 1 个 = 收编。命中 ≥2 个 = 不收编,但**逐一点名**——它们同样是本命令
// 之后才出现的,交给调用方按名单清理,绝不再只打印一句 PARTIAL STATE 就放手。

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
// Reason 永远可读,并且必须给出**能执行的下一步**(判据不许只说"不确定")。
type schAdoptVerdict struct {
	Adopted    *layoutComp
	Candidates []layoutComp
	Reason     string
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

// schAdoptOrphanPlacement 是收编的**纯判定**:给定落地前的已知 id 集合、一次活体
// 回读、以及本次放置请求,判断哪些器件是这次 place 生出来的孤儿。
//
// known 必须同时包含「命令开跑前页面上的全部器件」和「本命令此前已成功放置并拿到
// id 的器件」——少了后者,前面几件成功的放置会被当成孤儿。
func schAdoptOrphanPlacement(known map[string]bool, live []layoutComp, req schAdoptRequest) schAdoptVerdict {
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
		return schAdoptVerdict{Reason: fmt.Sprintf(
			"落地前快照之后没有任何新器件出现在 (%.0f,%.0f) ±%.0f —— 这次 place 确实没有落地,页面上没有本次留下的残件",
			req.X, req.Y, schAdoptTolerance)}
	case 1:
		c := fresh[0]
		desig := strings.TrimSpace(c.Designator)
		if desig == "" {
			desig = "(无位号)"
		}
		return schAdoptVerdict{
			Adopted:    &c,
			Candidates: fresh,
			Reason: fmt.Sprintf(
				"place 回执丢了但器件已落地:%s 是落地前快照之后唯一新出现在 (%.0f,%.0f) 的器件,按 id %s 收编",
				desig, req.X, req.Y, c.ID),
		}
	default:
		return schAdoptVerdict{
			Candidates: fresh,
			Reason: fmt.Sprintf(
				"(%.0f,%.0f) 附近同时出现 %d 个新器件(%s)—— 无法判定哪个是本次 place 的产物,不做收编;"+
					"它们都是本命令开跑后才出现的,已逐一点名交清理",
				req.X, req.Y, len(fresh), strings.Join(schAdoptIDs(fresh), ", ")),
		}
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
// 紧跟一次写的回读「读不到」不构成结论(settle_read.go 的教训)。满足条件是
// 「至少认出一个疑似残件」:认出来就立刻定案,认不出来才值得再读一拍。
//
// 回读范围与快照一致(活动页):create 落在活动页,两侧同尺才使差集有意义。
func schAdoptRead(cfg *appConfig, window string, known map[string]bool, req schAdoptRequest) (schAdoptVerdict, error) {
	type readOut struct {
		verdict schAdoptVerdict
		err     error
	}
	out, _ := settleRead(func() (readOut, bool) {
		res, err := requestAction(cfg, "schematic.components.list", window, map[string]any{})
		if err != nil {
			return readOut{err: fmt.Errorf("收编回读: %w", err)}, false
		}
		comps, perr := parseLayoutComps(res.Result)
		if perr != nil {
			return readOut{err: fmt.Errorf("收编回读: %w", perr)}, false
		}
		v := schAdoptOrphanPlacement(known, comps, req)
		return readOut{verdict: v}, len(v.Candidates) > 0
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
