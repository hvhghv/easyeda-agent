package app

// pcb_layoutscore_focus.go — `pcb layout-score --part` 的器件聚焦视角。
//
// 整体报告的归因是「维 → 器件」：每一维列出谁拉低了它。用户的真实工作流常常是
// 反过来的（用户原话：整体打分之后单独指出要优化的器件）——「J2 这个 Type-C
// 到底什么处境?」需要「器件 → 全维度」的汇总：它直接被哪些维扣了分、它作为
// 参照物被谁提及（TVS 离它太远,扣的是 TVS 的分但要动的可能是它）、它有没有
// 进 blocking、它的几何现状（位置/装配面/离板边多远）。
//
// 位号匹配用词边界：`C1` 绝不匹配 `C10`,但要匹配 `C1↔U2` 和焊盘引用 `C1.2`
// （`.` 与 `↔` 都是非词字符,天然成界）。

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// partFocusEntry 是一个器件在一维里的一条处境。
type partFocusEntry struct {
	Dimension string  `json:"dimension"`
	DimScore  float64 `json:"dimScore"`
	Penalty   float64 `json:"penalty,omitempty"` // 直接扣分才有;关联提及为 0
	Related   bool    `json:"related,omitempty"` // true = 出现在别人的归因详情里
	Detail    string  `json:"detail"`
}

// partFocus 是一个器件的完整聚焦卡。
type partFocus struct {
	Designator string           `json:"designator"`
	Found      bool             `json:"found"` // false = 板上没有这个位号
	X          float64          `json:"x,omitempty"`
	Y          float64          `json:"y,omitempty"`
	Side       string           `json:"side,omitempty"`
	Rotation   float64          `json:"rotation,omitempty"`
	EdgeMil    *float64         `json:"distToEdgeMil,omitempty"` // 有板框才有
	Blocking   []string         `json:"blocking,omitempty"`
	Entries    []partFocusEntry `json:"entries,omitempty"`
}

// designatorMentionRe 编一个「词边界包住位号」的匹配器。
func designatorMentionRe(des string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(des) + `($|[^A-Za-z0-9_])`)
}

// buildPartFocus 汇总一个位号在报告里的全部处境（纯函数,可离线单测）。
func buildPartFocus(rep *layoutScoreReport, snap *boardSnapshot, des string) partFocus {
	des = strings.ToUpper(strings.TrimSpace(des))
	pf := partFocus{Designator: des}
	mention := designatorMentionRe(des)

	if snap != nil {
		for _, c := range snap.Components {
			if strings.EqualFold(c.Designator, des) {
				pf.Found = true
				pf.X, pf.Y = round2(c.X), round2(c.Y)
				pf.Side = sideName(c.Layer)
				pf.Rotation = c.Rotation
				if snap.Outline != nil {
					cx, cy := c.center()
					d := round2(snap.Outline.distToEdge(cx, cy))
					pf.EdgeMil = &d
				}
				break
			}
		}
	}

	for _, b := range rep.Blocking {
		if strings.EqualFold(b.Designator, des) || mention.MatchString(b.Message) {
			pf.Blocking = append(pf.Blocking, b.Message)
		}
	}

	for _, d := range rep.Dimensions {
		if d.Status == dimSkipped {
			continue
		}
		for _, c := range d.Contributors {
			switch {
			case strings.EqualFold(c.Designator, des):
				pf.Entries = append(pf.Entries, partFocusEntry{
					Dimension: d.ID, DimScore: d.Score, Penalty: c.Penalty, Detail: c.Detail,
				})
			case mention.MatchString(c.Detail):
				// 关联提及:扣的是别人的分,但这个器件是那条判定的参照物/相对方
				//（TVS 离 J2 太远 —— 扣 TVS,提及 J2;要动哪个由人定）。
				pf.Entries = append(pf.Entries, partFocusEntry{
					Dimension: d.ID, DimScore: d.Score, Related: true,
					Detail: fmt.Sprintf("(%s 的归因提及本件) %s", c.Designator, c.Detail),
				})
			}
		}
	}
	// 直接扣分在前、扣得多的在前;关联提及殿后。
	sort.SliceStable(pf.Entries, func(i, j int) bool {
		if pf.Entries[i].Related != pf.Entries[j].Related {
			return !pf.Entries[i].Related
		}
		return pf.Entries[i].Penalty > pf.Entries[j].Penalty
	})
	return pf
}

// renderPartFocus 渲染聚焦卡片。
func renderPartFocus(pf partFocus, w io.Writer) {
	fmt.Fprintf(w, "\n■ %s", pf.Designator)
	if !pf.Found {
		fmt.Fprintf(w, " — 板上没有这个位号\n")
		return
	}
	fmt.Fprintf(w, "  @(%.1f, %.1f) %s面 rot=%g", pf.X, pf.Y, pf.Side, pf.Rotation)
	if pf.EdgeMil != nil {
		fmt.Fprintf(w, " · 离板边 %.1fmil", *pf.EdgeMil)
	}
	fmt.Fprintln(w)
	if len(pf.Blocking) > 0 {
		for _, b := range pf.Blocking {
			fmt.Fprintf(w, "  ⛔ %s\n", b)
		}
	}
	if len(pf.Entries) == 0 && len(pf.Blocking) == 0 {
		fmt.Fprintln(w, "  ✓ 没有任何维度扣它的分,也没有归因提及它")
		return
	}
	for _, e := range pf.Entries {
		if e.Related {
			fmt.Fprintf(w, "  · %s(%.1f) %s\n", dimensionTitles[e.Dimension], e.DimScore, e.Detail)
		} else {
			fmt.Fprintf(w, "  ↓ %s(%.1f) −%.1f — %s\n", dimensionTitles[e.Dimension], e.DimScore, e.Penalty, e.Detail)
		}
	}
}
