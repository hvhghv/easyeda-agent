package app

// workflow_quality.go — confirm-layout 落盘的多维质量快照的**消费侧**(#167 闭环审计)。
//
// 写入侧在 cmd_pcb_stage.go(captureLayoutQuality → GateSummary.Quality);此前
// 全仓没有任何读者 —— write-only 的快照等于没有。这里补上两个消费面:
//   • `workflow status` 渲染上次快照(综合分 + 最弱三维 + 未测维数 + 记录时间);
//   • `workflow status --reconcile` 若能实时打分,与快照做**逐维 diff**,掉分超
//     阈值的维提示出来("上次 confirm-layout 后 tidy 从 90 掉到 60 —— 布局被谁
//     动过?")。
//
// 三条硬约定(与全仓口径一致):
//   • skipped=没测≠满分,也≠0 分 —— 上次 scored 这次 skipped 是「该维失去可测性」,
//     绝不能当成掉到 0 分报退化;
//   • status 只展示+提示,不做新的门(质量退化永不导致非零退出——未校准的尺子
//     不担硬门,和写入侧"默认不拦"的理由同源);
//   • 实时打分不可得(离线/没窗口)时必须明说没做对比 —— 「没测≠没变」。
//
// 渲染与比对全是纯函数;取数(连 daemon)只在 reconcileQualityNotes 一处,所以
// diff 逻辑离线单测,不需要编辑器。

import (
	"fmt"
	"sort"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// qualityDropThreshold 是逐维 diff 的告警阈值(分)。5 分挡掉浮点噪声和轻微网格
// 漂移带来的抖动,又足以暴露真实退化;掉满阈值(≥)即提示。
const qualityDropThreshold = 5.0

// stateQuality 安全取出状态里的质量快照(state / Layout / Quality 任一为 nil 都
// 返回 nil)。单独成函数是因为 reconcile 的 InvalidateFrom 会把 st.Layout 连同
// 快照在内存里清掉 —— 消费方必须在 reconcile **前**取一次。
func stateQuality(st *pcbStageState) *workflow.QualitySummary {
	if st == nil || st.Layout == nil {
		return nil
	}
	return st.Layout.Quality
}

// qualitySnapshotLines 渲染上次快照(纯函数;不带缩进,调用方统一加)。
// 没有快照时给一行说明而不是沉默 —— 用户得知道这张表从哪来、怎么才会有。
func qualitySnapshotLines(q *workflow.QualitySummary) []string {
	if q == nil {
		return []string{"布局质量: 未记录过 —— `pcb stage confirm-layout` 时会自动记一张多维质量快照"}
	}
	lines := []string{fmt.Sprintf(
		"布局质量: %.1f/100 [%s] — %d 维参与加权,%d 维未测 (记录于 %s)",
		q.Overall, q.Verdict, q.ScoredDims, q.SkippedDims, q.At)}
	for _, l := range weakestQualityLines(q, 3) {
		lines = append(lines, "  "+l)
	}
	return lines
}

// qualityDimLabel 与 layout-score 报告同款的「中文标题(id)」展示名 —— id 必须
// 露出来,因为 `pcb layout-score --only/--skip` 认的是 id。
func qualityDimLabel(id string) string {
	if title := dimensionTitles[id]; title != "" {
		return title + "(" + id + ")"
	}
	return id
}

// qualityDiffNotes 逐维比对上次快照(prev)与实时打分(curr),返回给 status 的
// 提示行(已带 ⚠️/·/✓ 标记)。纯函数,永不产生错误/退出码 —— status 不是门。
//
// 快照的 Dimensions 只存 scored 的维(写入侧刻意排除 skipped,见
// captureLayoutQuality),所以「prev 有 curr 没有」唯一的含义就是该维从 scored
// 变成了 skipped —— 失去可测性,单独提示而不是按 0 分算退化。
func qualityDiffNotes(prev, curr *workflow.QualitySummary, dropThreshold float64) []string {
	switch {
	case prev == nil && curr == nil:
		// 快照行已说明"未记录过",两头都没有就没什么可比。
		return nil
	case prev == nil:
		return []string{fmt.Sprintf(
			"· 布局质量无历史快照可比(实时 %.1f/100) —— confirm-layout 落一张快照后 status 才能逐维对比",
			curr.Overall)}
	case curr == nil:
		return []string{"⚠️ 布局质量未做实时对比(实时打分不可得) —— 没测≠没变,上面显示的仍是上次快照"}
	}

	// 两次的维取并集、按 id 排序 —— map 遍历无序,输出必须确定。
	ids := make([]string, 0, len(prev.Dimensions)+len(curr.Dimensions))
	seen := map[string]bool{}
	for id := range prev.Dimensions {
		seen[id] = true
		ids = append(ids, id)
	}
	for id := range curr.Dimensions {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	var notes []string
	for _, id := range ids {
		pv, pok := prev.Dimensions[id]
		cv, cok := curr.Dimensions[id]
		switch {
		case pok && cok:
			// 只提示掉分;涨分不打扰 —— status 是提示面不是庆功面。
			if pv-cv >= dropThreshold {
				notes = append(notes, fmt.Sprintf(
					"⚠️ 上次 confirm-layout 后 %s 从 %.1f 掉到 %.1f(−%.1f) —— 布局被谁动过?",
					qualityDimLabel(id), pv, cv, pv-cv))
			}
		case pok && !cok:
			notes = append(notes, fmt.Sprintf(
				"⚠️ %s 上次测得 %.1f,这次 skipped —— 该维失去可测性(数据/意图输入缺了),不按 0 分算退化",
				qualityDimLabel(id), pv))
		case !pok && cok:
			notes = append(notes, fmt.Sprintf(
				"· %s 上次未测,这次测得 %.1f —— 新增可测维,暂无历史可比",
				qualityDimLabel(id), cv))
		}
	}
	if len(notes) == 0 {
		// 明说"比过了、没退化",与"根本没比"(上面 curr==nil 分支)可区分。
		notes = append(notes, fmt.Sprintf(
			"✓ 布局质量实时 %.1f/100(快照 %.1f) —— 逐维比对无 ≥%.0f 分退化",
			curr.Overall, prev.Overall, dropThreshold))
	}
	return notes
}

// reconcileQualityNotes 是 --reconcile 的取数入口:实时打一次分(连 daemon),再交
// 给纯函数 qualityDiffNotes 比对。任何取数失败都降级成一行提示,绝不让 status
// 因此失败 —— 质量表读不出来不该挡住状态查询,但「没测≠没变」必须说出口。
func reconcileQualityNotes(cfg *appConfig, window string, prev *workflow.QualitySummary) []string {
	live, err := captureLayoutQuality(cfg, window, "", 0)
	if err != nil {
		if prev == nil {
			return nil // 既无快照也打不了分:快照行已说明"未记录过"
		}
		return []string{fmt.Sprintf(
			"⚠️ 布局质量未做实时对比(实时打分不可得: %v) —— 没测≠没变,上面显示的仍是上次快照", err)}
	}
	return qualityDiffNotes(prev, live, qualityDropThreshold)
}
