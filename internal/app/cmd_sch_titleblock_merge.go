package app

// cmd_sch_titleblock_merge.go — 图签写入的**读改写**外壳。
//
// 平台的 `modifySchematicPageTitleBlock` 与它自己的类型定义不符,实测三条(2026-08-15,
// ceshi,逐条 debug.exec_js 探出来的):
//
//  1. **传子集就崩**:`{Name:{value:"X"}}` → `TypeError: Cannot set properties of
//     undefined (setting 'value')`。平台是拿**它认识的全部明细项**去遍历你传的对象,
//     缺哪一项就在哪一项上崩 —— 而官方 @remarks 写的是「未传入的项将保持默认状态」。
//     所以必须**先读回全量、改其中几项、再整体传回去**。
//  2. **`showTitle`/`showValue` 读回来是 `null`,原样带回去字段不生效**(返回 true,
//     值纹丝不动)。被改的那几项必须给**真布尔**。
//  3. **不传 `showTitleBlock` 也不生效**(同样静默返回 true)。
//
// 三条凑齐才写得进去。这层就干这件事,连接器一行不用改(它只是把 titleBlockData 透传)。

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// tbPreserve 原样回传一个**没被修改**的明细项,但把数字型字符串还原成数字。
//
// **这条是数据损坏修复**:`Title Block` / `Border` 这类结构开关读回来是字符串 `"1"`,
// 整体回传时平台把它解析成 0 —— 实测一次写图签就把**图框和明细表整个关掉**,
// sheet 图元的 bbox 塌成全零,`zone-plan`/`layout-lint` 的 sheet-check 当场不可用。
// 页面看上去还在,判据却瞎了,而且没有任何报错。
func tbPreserve(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for k, vv := range m {
		out[k] = vv
	}
	if sv, isStr := out["value"].(string); isStr {
		if n, err := strconv.ParseFloat(strings.TrimSpace(sv), 64); err == nil && sv != "" {
			out["value"] = n
		}
	}
	return out
}

// tbStructuralKeys 是**画布结构**开关 —— 它们不是文字栏位,而是「图框画不画」
// 「明细表画不画」。写坏了不是显示问题,是判据失明(sheet 图元没了,越界/分区
// 一概判不了),所以整包回传时单独按住。
var tbStructuralKeys = []string{"Title Block", "Border"}

// tbKeepStructural 把结构开关按住:值取原值(数字化),并显式给**真布尔**的
// showTitle/showValue —— 读回来是 null,原样带回去平台按默认处理,默认就是关。
func tbKeepStructural(full, out map[string]any) {
	for _, k := range tbStructuralKeys {
		v, ok := full[k].(map[string]any)
		if !ok {
			continue
		}
		kept, _ := tbPreserve(v).(map[string]any)
		if kept == nil {
			continue
		}
		if _, has := kept["value"]; !has {
			continue
		}
		kept["showTitle"] = tbBoolOr(v["showTitle"], false)
		kept["showValue"] = tbBoolOr(v["showValue"], true)
		out[k] = kept
	}
}

// tbBoolOr 把读回来的 null / 非布尔折成一个确定的布尔。
func tbBoolOr(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

// schTitleBlockMerge 读回当前页的全量明细项,把用户的 patch 合并进去。
//
// patch 接受两种写法:`{"Name":{"value":"X"}}`(与读回来的形状一致)与
// `{"Name":"X"}`(顺手写)。被改的项一律带上 showTitle/showValue=true。
func schTitleBlockMerge(cfg *appConfig, window string, patch map[string]any) (map[string]any, bool, error) {
	res, err := requestAction(cfg, "schematic.titleblock.get", window, map[string]any{})
	if err != nil {
		return nil, false, fmt.Errorf("图签写入前要先读回全量明细项(平台传子集会崩): %w", err)
	}
	shown, _ := res.Result["showTitleBlock"].(bool)
	full, _ := res.Result["titleBlockData"].(map[string]any)
	if full == nil {
		return nil, false, fmt.Errorf("读不到当前页的明细项 —— 无法安全写入(平台传子集会崩)")
	}
	out := make(map[string]any, len(full)+len(patch))
	for k, v := range full {
		out[k] = tbPreserve(v)
	}
	// 结构开关(图框 / 明细表本身)在整包回传里**必须原值原样活下来**。
	// 实测一次写图签把 `Title Block` 与 `Border` 双双变成 "0",图框和明细表被
	// 整个关掉:sheet 图元消失 → `sheet-geometry` 读回 bbox=null → `layout-lint`
	// 的 sheet-check 变 unavailable → `sch gate --strict` 四页全挂,而写图签的
	// 那条命令只报了一句无关的「nothing was applied」(2026-08-15 esp32Mini E2E)。
	// 页面看上去还在,判据却瞎了 —— 这是数据损坏,不是显示问题。
	tbKeepStructural(full, out)
	var unknown []string
	for k, v := range patch {
		if _, ok := full[k]; !ok {
			unknown = append(unknown, k)
		}
		value := v
		if m, isMap := v.(map[string]any); isMap {
			if inner, has := m["value"]; has {
				value = inner
			}
		}
		out[k] = map[string]any{"showTitle": true, "showValue": true, "value": value}
	}
	if len(unknown) > 0 {
		return nil, false, fmt.Errorf("这些明细项当前页没有:%s —— 先跑 `easyeda sch titleblock-get` 看可用 key(平台对不认识的项会崩或静默忽略)",
			strings.Join(unknown, ", "))
	}
	// 只在**当前没显示**时才带 showTitleBlock:图签已经显示还传一次,连接器的
	// 前后对比会把「本来就是 true」判成「没应用」,于是写成功了却报失败(实测)。
	return out, !shown, nil
}

// warnIfSheetLost 在写图签后回读一次图纸几何:sheet 图元没了就当场报错。
//
// 判据不是「命令返回了什么」而是「画布还剩什么」—— 平台会在返回 true 的同时
// 把图框关掉,只有回读能发现。返回非 nil 表示画布已损坏,调用方应当作失败。
func warnIfSheetLost(cfg *appConfig, window string, stderr io.Writer) error {
	// 走 settleRead:写图签会让平台重建图框,紧接着那一读常常拿不到 sheet 图元。
	// 这条误报的代价特别大 —— 它会让人照提示去执行「整包回传写回 Border/Title
	// Block」,而那才是唯一真能把图框写坏的操作:判据把好板子推向危险操作。
	_, ok := settleRead(func() (bool, bool) {
		res, err := requestAction(cfg, "schematic.components.list", window, map[string]any{"includeBBox": true})
		if err != nil {
			return false, false
		}
		comps, perr := parseLayoutComps(res.Result)
		if perr != nil {
			return false, false
		}
		for _, c := range comps {
			if c.ComponentType == "sheet" && c.BBox != nil {
				return true, true
			}
		}
		return false, false
	})
	if ok {
		return nil
	}
	return fmt.Errorf("写图签后本页找不到图纸边框(sheet 图元的 bbox)—— 图框/明细表很可能被整包回传关掉了。" +
		"修复:`easyeda sch titleblock --data '{\"Title Block\":{\"value\":1},\"Border\":{\"value\":1}}'`," +
		"再用 `easyeda sch sheet-geometry` 确认 bbox 回来了")
}

// tbRequestedKeys 从用户的 patch 里取出**他真正要写的**明细项名。
//
// 与整包回传里的 key 集合不是一回事:后者包含全量明细项(平台要求整体回传)以及
// 我们主动按住的结构开关(Title Block / Border)。判「这次写成功没有」只该看前者。
func tbRequestedKeys(patch map[string]any) []string {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// tbPatchLanded 回读图签,判**用户请求的每一项**是不是都已经是目标值。
//
// 为什么需要它:连接器的写后校验按「本次调用改变了什么」判定,于是
//  1. 我们主动按住的结构开关(值本来就对,写进去也不会变)永远落在 notApplied;
//  2. 重复写同一个值(幂等重跑、批量脚本重放)时,所有项都「没变化」,
//     校验判定 nothingProven 并抛错。
//
// 实测四页图签**内容全部写对了**(Name/Drawed/Description 都是目标值,
// Border/Title Block 都是 1),命令却报 ok:false —— 一次彻头彻尾的假失败。
// 而假失败比假成功更难缠:调用方会去重试、回滚,或者干脆认定这条路不通。
//
// 判据换成**画布的最终状态**:用户要的内容在不在图签上。这与「落地即判定」
// 是同一条原则 —— 平台的 applied 计数是过程量,画布才是结果。
func tbPatchLanded(cfg *appConfig, window string, patch map[string]any) (bool, []string) {
	// 走 settleRead:写图签会让平台重建图签对象,首读常常还是旧值 —— 真机实测,
	// 首次写入(值真的变了)复核不过,而幂等重写(平台不重建)一路通过,症状正好反着。
	missing, ok := settleRead(func() ([]string, bool) {
		res, err := requestAction(cfg, "schematic.titleblock.get", window, map[string]any{})
		if err != nil {
			return nil, false
		}
		full, _ := res.Result["titleBlockData"].(map[string]any)
		if full == nil {
			return nil, false
		}
		var miss []string
		for _, k := range tbRequestedKeys(patch) {
			want := patch[k]
			if m, ok := want.(map[string]any); ok {
				want = m["value"] // 接受 {"Name":{"value":"X"}} 与 {"Name":"X"} 两种写法
			}
			cur, _ := full[k].(map[string]any)
			if cur == nil || fmt.Sprint(cur["value"]) != fmt.Sprint(want) {
				miss = append(miss, k)
			}
		}
		return miss, len(miss) == 0
	})
	return ok, missing
}
