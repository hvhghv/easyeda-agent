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
