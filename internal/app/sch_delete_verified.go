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

// deleteVerifiedOneByOne 逐个调 deleteOne,整批删完后用 aliveSet 回读证实;
// 幸存者逐个重试一次,再回读一次定案。返回 error 仅当回读本身失败
// (删了但无法证实 —— 调用方应按「未验证」处理,绝不能当成删干净)。
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
	alive, err := aliveSet()
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
		alive, err = aliveSet()
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
