package app

// sch_delete_verified.go — 逐个删 + 回读证实 + 失败重试一次(缺陷 3,P1)。
//
// 平台已知病:delete 大批量静默 no-op 仍返 true(memory 定案:分批+回读)。真机
// 实锤(2026-08 esp32Mini E2E):zone-draw 批量删旧框报 survived=4、block-apply
// 回滚的 component.delete 报 deleted=false —— 而 `prim-delete` **逐个**删成功率
// 100%。所以删除的可信姿势只有一种:一次删一个,删完整批后回读活体集证实,
// 幸存者再补删一轮,仍活着才算真幸存。
//
// 本文件是那个姿势的唯一 Go 侧实现;调用方自带「怎么删一个」和「怎么读活体集」
// (component.delete / primitives.delete / exec_js 各不相同),判定逻辑共享。
// zone-draw 的删除发生在 exec_js 的 JS 里,同一套「逐个删+回读+重试」以 JS 形式
// 内联在 buildZoneClearJS(单次连接器往返,不走本 helper,语义一致)。

import (
	"fmt"
	"strings"
)

// verifiedDeleteResult 是一轮「逐个删+回读证实」的结果。判定只信回读:
// Deleted 是回读证实**已消失**的 id;Survived 是重试后**仍活着**的 id。
// Errors 只作归因参考(平台的 delete 返回值与错误都不可信,不参与判定)。
type verifiedDeleteResult struct {
	Deleted  []string // 回读证实已消失
	Survived []string // 重试一次后仍存活
	Retried  []string // 首轮删后仍活、进入重试的 id
	Errors   []string // 逐个删除时的错误(informational)
}

// settleAliveSet 是删除后的**证实回读**,走 settleRead(settle_read.go)。
//
// 为什么删完不能立刻读:平台的写入不是同步生效的,紧跟一次删的回读会读到还没
// 落定的快照,于是把「已经删掉」渲染成「survived」。这个误报有实际代价 ——
// 上层照着它重删、重试,而重试期间的每一次写又都是新的鬼影来源。判据自己的
// 满足条件写在这里:**这批 id 一个都不在活体集里**。满足即定案,不满足才值得
// 再读一拍(2×400ms,与仓里其它回读同一把尺,不另造一把)。
//
// 注意这只重复**读**,不重发写 —— 写的重试是本函数外面那一轮显式重删。
//
// ── 为什么这里**不**加 sch_place_adopt.go 那道新鲜度门(2026-08-20 复核) ──
//
// 收编那边的教训是「一个『什么都没有』的回读,先证明新鲜才算证据」。这条判据看着
// 对称,方向却是反的,别顺手照抄:
//
//	读得太早 = 拿到一份**更早时刻**的页面 = 该消失的还在、该出现的还没出现。
//	  收编问的是「我建的件出现了吗」→ 早读答「没有」→ **假安全**(报『确实没落地』,
//	                                  人不去清,残件永久留下)。必须加门。
//	  本函数问的是「我删的件消失了吗」→ 早读答「还在」→ ok=false → 重读 → 仍在就
//	                                  报 survived → **偏保守**:上层再删一轮、
//	                                  报 PARTIAL STATE、打印可执行的清理处方。
//	                                  代价是一次假警报,不会造成假安全。
//
// 唯一能在这边造出假安全的坏帧,是回读**倒退到本命令自己的 create 之前**
// (block-apply 回滚删的正是本命令几秒前建的件,那时它们本来就不在页上)。这一档
// 无法用探针补,因为**没有可用探针**:回滚把本命令建的件全删了,而「回滚真的删干净」
// 与「回读倒退到开跑前」在观测上是同一张页面(都等于落地前快照),拿快照 id 当探针
// 只会全数命中 —— 那正是收编那边明令拒绝的、看起来通用实则证明不了的假门。
//
// 不加门还有两条实证依据:(1) 两边需要的倒退量差着数量级 —— 收编只需回读早于
// 「刚刚那次 create」(毫秒级),本函数要求回读早于本命令**全部** create(跨越一次
// place 超时,秒~分钟级);(2) 真机 2026-08-20 那一幕里,收编回读已经说错话的同一
// 分钟内,回滚回读**正确**地把刚建的 U3 报成 survived。两者不是同一个风险面。
func settleAliveSet(ids []string, aliveSet func() (map[string]bool, error)) (map[string]bool, error) {
	type readOut struct {
		alive map[string]bool
		err   error
	}
	out, _, _ := settleRead(func() (readOut, bool, error) {
		alive, err := aliveSet()
		if err != nil {
			return readOut{err: err}, false, err
		}
		for _, id := range ids {
			if alive[id] {
				return readOut{alive: alive}, false, nil // 还有活着的 → 可能是没落定的快照
			}
		}
		return readOut{alive: alive}, true, nil
	})
	if out.err != nil {
		return nil, out.err
	}
	return out.alive, nil
}

// deleteVerifiedOneByOne 逐个调 deleteOne,整批删完后用 aliveSet 回读证实;
// 幸存者逐个重试一次,再回读一次定案。两次证实回读都经过 settleAliveSet,
// 所以「survived」只在**settle 之后仍然活着**时才成立。返回 error 仅当回读在
// settle 预算内始终失败(删了但无法证实 —— 调用方应按「未验证」处理,绝不能
// 当成删干净)。
func deleteVerifiedOneByOne(
	ids []string,
	deleteOne func(id string) error,
	aliveSet func() (map[string]bool, error),
) (verifiedDeleteResult, error) {
	var out verifiedDeleteResult
	seen := map[string]bool{}
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return out, nil
	}
	for _, id := range uniq {
		if err := deleteOne(id); err != nil {
			out.Errors = append(out.Errors, id+": "+err.Error())
		}
	}
	alive, err := settleAliveSet(uniq, aliveSet)
	if err != nil {
		return out, fmt.Errorf("delete verification read failed: %w", err)
	}
	for _, id := range uniq {
		if alive[id] {
			out.Retried = append(out.Retried, id)
		}
	}
	if len(out.Retried) > 0 {
		for _, id := range out.Retried {
			if err := deleteOne(id); err != nil {
				out.Errors = append(out.Errors, id+" (retry): "+err.Error())
			}
		}
		alive, err = settleAliveSet(out.Retried, aliveSet)
		if err != nil {
			return out, fmt.Errorf("delete verification read failed after retry: %w", err)
		}
	}
	for _, id := range uniq {
		if alive[id] {
			out.Survived = append(out.Survived, id)
		} else {
			out.Deleted = append(out.Deleted, id)
		}
	}
	return out, nil
}

// survivedIDSet 从 delete 类 action 的 result 里抽出幸存 id 集。兼容两种形状:
//   - schematic.primitives.delete:result.survived = {类目: [id,…]}(map)
//   - schematic.component.delete: result.survived = [id,…](array)
//
// 读不出就返回空集 —— 调用方只用它做「哪些 id 证实删掉了」的差集,空集意味着
// 全部按已删处理,与 failOnSurvivingPrimitives 的 fail-closed 判定互不越权。
func survivedIDSet(result map[string]any) map[string]bool {
	out := map[string]bool{}
	if result == nil {
		return out
	}
	add := func(v any) {
		if s := asString(v); s != "" {
			out[s] = true
		}
	}
	switch sv := result["survived"].(type) {
	case []any:
		for _, v := range sv {
			add(v)
		}
	case []string:
		for _, v := range sv {
			out[v] = true
		}
	case map[string]any:
		for _, group := range sv {
			if list, ok := group.([]any); ok {
				for _, v := range list {
					add(v)
				}
			}
		}
	}
	return out
}
