package app

// sch_prim_delete_settle.go — `sch prim-delete` 的 settle 复核。
//
// 连接器侧 `survivingSchPrimitives` 在 delete 之后**立刻** `getAll()` 判存活。
// 那一读可能采到还没落定的快照,于是把「已经删掉」报成 survived —— 上层
// (failOnSurvivingPrimitives)据此非零退出,人再删一遍,一轮轮空转。
//
// 这里不去改连接器(改 extension 意味着重打包 .eext,真机验证成本高,而这条能在
// Go 侧闭合):首轮报幸存时,等一拍 settle 再对**幸存 id**重发一次删除。第二次
// 回执就是定案:
//
//   - 那些 id 其实早删掉了 → 第二次它们进 notFound,不再 partial → 判定成功;
//   - 真没删掉(平台大批量静默 no-op / 刚建的图元短暂拒删)→ 第二次顺手补删并
//     再回读一次;
//   - 连接器队列 wedge(写整体被吞)→ 仍然 partial,如实失败,并给出重启处方。
//
// 与 deleteVerifiedOneByOne 是同一把尺:删一轮 → settle 回读 → 幸存者重删一次 →
// 再回读定案。重发 delete 是安全的:对已经不在页上的 id,连接器把它归 notFound
// 而不是再删一次别的东西。

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// primDeleteSettleRecheck 对首轮 delete 报出的幸存者做一次 settle 复核,返回
// **用于定案**的 result(复核成功时是第二轮的回执,否则退回首轮回执)。
//
// stdout 上留下的始终是首轮的原始回执;最终判定看 stderr 与退出码。
func primDeleteSettleRecheck(cfg *appConfig, window string, res *actionResult, stderr io.Writer) *actionResult {
	if res == nil || res.Result == nil {
		return res
	}
	if partial, _ := res.Result["partial"].(bool); !partial {
		return res
	}
	survivors := survivedIDSet(res.Result)
	if len(survivors) == 0 {
		return res
	}
	ids := make([]string, 0, len(survivors))
	for id := range survivors {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	fmt.Fprintf(stderr, "⚠ 首轮回读报 %d 个图元存活(%s)—— 等 %v settle 后复核一轮再定案\n",
		len(ids), strings.Join(ids, ", "), settleDelay)
	time.Sleep(settleDelay)

	second, err := requestAction(cfg, "schematic.primitives.delete", window,
		map[string]any{"primitiveIds": ids})
	if err != nil {
		fmt.Fprintf(stderr, "  复核失败(%v)—— 按首轮回执定案\n", err)
		return res
	}
	if partial, _ := second.Result["partial"].(bool); !partial {
		fmt.Fprintln(stderr, "✓ 复核:这些图元已不在页上(首轮回读是尚未落定的快照)")
		return second
	}
	return second
}

// primDeleteResidueGuidance 打印仍然删不掉时的处方。**必须能执行** —— 老文案
// 只说「在 EasyEDA UI 里删」,而真机上这类幸存几乎总是连接器 action 队列 wedge:
// 此期间 place/delete/document.open 会整体被吞,轻读照常,所以看起来像"删不掉"。
func primDeleteResidueGuidance(w io.Writer, res *actionResult) {
	ids := survivedIDSet(nil)
	if res != nil {
		ids = survivedIDSet(res.Result)
	}
	if len(ids) > 0 {
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		sort.Strings(list)
		fmt.Fprintf(w, "  still on the page: %s\n", strings.Join(list, ", "))
	}
	fmt.Fprintln(w, "  这几乎总是连接器 action 队列 wedge(某个重调用的 promise 永不 resolve,此后写操作")
	fmt.Fprintln(w, "  整体被吞而轻读照常):先 `easyeda sch save`,完全退出并重启 EasyEDA,再重跑本命令。")
	fmt.Fprintln(w, "  若重启后仍在,才是 issue #164 那类平台留件 —— 在 EasyEDA UI 里删。")
}
