package app

// sch_sheet_guard.go — 图框(sheet)误删守卫。
//
// 2026-08-17 真机事故(audit 铁证):用户人工重放的 P3 图框在 `sch list` 里长着
// 一副「PARTIAL 残件」的脸 —— designator 空、锚点 (0,0)、无名 —— 于是被一次
// 残件清理 `prim-delete --ids dd27ee95d8144295` 误删。图框丢失后 titleblock
// 写通道拒写、sheet-check 永远 unavailable,而平台**没有重建图框的 API**,
// 唯一恢复路径是人工在 UI 重放 —— 一次误删的代价是把整页打回人工步。
//
// 守卫口径:prim-delete 发送前读一次活画布,--ids 命中 componentType=sheet 的
// id 即拒绝(fail-closed;--allow-sheet 显式放行)。读失败时**放行并警告**而不是
// 拒绝 —— 守卫是为了拦「明知是图框还删」,不是给停摆通道再加一个卡点:通道坏时
// 读不到类型,拦下所有删除只会把修复工作也堵死。

import (
	"fmt"
	"io"
	"strings"
)

// schSheetGuard returns an error when any of ids is a sheet primitive on the
// live page. Read failures degrade to a warning (nil error) — see file header.
func schSheetGuard(cfg *appConfig, window string, ids []string, allowSheet bool, stderr io.Writer) error {
	if allowSheet || len(ids) == 0 {
		return nil
	}
	res, err := dispatchCapture(cfg, "schematic.components.list", window, nil, io.Discard)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ 图框守卫读不到画布(%v)—— 本次删除未经 sheet 校验\n", err)
		return nil
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		fmt.Fprintf(stderr, "⚠ 图框守卫解析失败(%v)—— 本次删除未经 sheet 校验\n", perr)
		return nil
	}
	sheetIDs := map[string]bool{}
	for _, c := range comps {
		if c.ComponentType == "sheet" && c.ID != "" {
			sheetIDs[c.ID] = true
		}
	}
	var hit []string
	for _, id := range ids {
		if sheetIDs[id] {
			hit = append(hit, id)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return fmt.Errorf("拒绝删除:%s 是图框(sheet)图元 —— 图框在 list 里就是「无位号 @(0,0)」的样子,极易被当残件误删,而平台没有重建图框的 API(丢了只能人工在 UI 重放)。确认要删就加 --allow-sheet",
		strings.Join(hit, ","))
}

// dropSheetIDs filters sheet primitives out of a programmatic delete batch
// (deep-sweep / ghost-marker prescriptions) against an already-loaded scene —
// the CLI guard protects `prim-delete`, but internal deleters must not run bare.
func dropSheetIDs(ids []string, comps []layoutComp) []string {
	sheet := map[string]bool{}
	for _, c := range comps {
		if c.ComponentType == "sheet" && c.ID != "" {
			sheet[c.ID] = true
		}
	}
	if len(sheet) == 0 {
		return ids
	}
	out := ids[:0:0]
	for _, id := range ids {
		if !sheet[id] {
			out = append(out, id)
		}
	}
	return out
}
