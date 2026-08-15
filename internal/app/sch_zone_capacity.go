package app

// sch_zone_capacity.go — 「装不下」与「摆得不好」的区分。
//
// 立项现场(2026-08-16 esp32Mini E2E #2):P2_MCU 页 zone-plan 报 titleBlockHits=1,
// 提示是「adjust margins/gutter or the zone claims」。可那一页的内容是一颗
// ESP32-S3-WROOM-1:符号本体就 421 高,算上自己的 marker 后虚拟组高 462;而 A4 横放
// 可用区高 825,图签 keepout 占掉底部 198,再留 30 的安全带 —— 竖直方向只剩 597,
// 框还要 ±24 的 pad 和顶部 30 的区名带。**怎么调 margin 都装不下。**
//
// 于是判据给出的是一条**做不到的建议**,而人(或 agent)会照着试、试不动、然后
// 把这条判据当噪音跳过。判据的价值不在于报错,在于报出**能执行的下一步**。
//
// 所以这里把两件事分开:
//   - 摆得不好 —— 挪一挪/收紧间距就能解决,原提示有效;
//   - **装不下** —— 换更大的图纸(或把模块拆到下一页)才有解,并且要**算给你看**
//     差多少、该换到多大。
//
// 判据是保守的:只有当「最紧凑的可行摆法」都放不下时才报装不下 —— 即不考虑
// 模块之间怎么排,只问单个模块自己的框能不能塞进可用区。这样绝不会把「摆得不好」
// 误判成「装不下」(那会让人白换一张大纸)。

import "fmt"

// schSheetTemplate 是一档标准图纸的可用尺寸(schematic units,横放)。
// 数值取自平台 a-series-landscape 模板族:A4 实测 1170×825,其余按 √2 递推
// 并圆整到 5 的格点 —— 建议换纸时只需要量级正确,不需要毫米级精确。
type schSheetTemplate struct {
	Name string
	W, H float64
}

var schSheetLadder = []schSheetTemplate{
	{"A4", 1170, 825},
	{"A3", 1655, 1170},
	{"A2", 2340, 1655},
	{"A1", 3310, 2340},
	{"A0", 4680, 3310},
}

// schZoneCapacity 是一页的容量诊断。
type schZoneCapacity struct {
	Fits bool `json:"fits"`
	// NeedW/NeedH 是**最大那个模块**的框所需的净尺寸(含 pad、区名带、说明带)。
	NeedW float64 `json:"needW"`
	NeedH float64 `json:"needH"`
	// HaveW/HaveH 是扣掉页边距与图签安全带之后真正可用的尺寸。
	HaveW    float64 `json:"haveW"`
	HaveH    float64 `json:"haveH"`
	Blocking string  `json:"blockingModule,omitempty"`
	Suggest  string  `json:"suggestedSheet,omitempty"`
}

// diagnoseZoneCapacity 判「这一页是不是根本装不下」。纯函数,可单测。
//
// 只问一个问题:**最大的那个模块**,它自己的框(内容 + pad + 区名带 + 说明带)
// 能不能塞进「可用区减去图签安全带」。答案是否,再怎么排布都无解。
func diagnoseZoneCapacity(sheet layoutBBox, keepout *layoutBBox, modules []partitionModule, opts partitionOpts) schZoneCapacity {
	cap := schZoneCapacity{Fits: true}
	if len(modules) == 0 {
		return cap
	}
	usableW := (sheet.MaxX - sheet.MinX) - 2*opts.Margin
	usableH := (sheet.MaxY - sheet.MinY) - 2*opts.Margin
	// 图签把可用区的一角吃掉。竖直方向按最坏情况扣:框要整体抬到 keepout 上沿
	// 之上(inflatedTitleKeepout 的口径),所以可用高度直接减去它的高度 + 安全带。
	if keepout != nil {
		usableH -= (keepout.MaxY - keepout.MinY) + titleBlockSafety
	}
	cap.HaveW, cap.HaveH = usableW, usableH

	for _, m := range modules {
		b := m.BBox // draw 口径:器件 ∪ 它自己的 marker —— 框要框住的正是这些
		w := (b.MaxX - b.MinX) + 2*partitionContentPad
		h := (b.MaxY-b.MinY) + 2*partitionContentPad + opts.TitleBand + opts.NoteBand
		if w > cap.NeedW {
			cap.NeedW = w
		}
		if h > cap.NeedH {
			cap.NeedH = h
			if h > usableH {
				cap.Blocking = m.Name
			}
		}
		if w > usableW && cap.Blocking == "" {
			cap.Blocking = m.Name
		}
	}
	cap.Fits = cap.NeedW <= usableW && cap.NeedH <= usableH
	if !cap.Fits {
		cap.Suggest = suggestSheetFor(cap.NeedW, cap.NeedH, keepout, opts)
	}
	return cap
}

// suggestSheetFor 在标准纸阶梯上找第一张装得下的。找不到就说实话 —— 建议拆页,
// 而不是推荐一张连它自己都装不下的纸。
func suggestSheetFor(needW, needH float64, keepout *layoutBBox, opts partitionOpts) string {
	koH := 0.0
	if keepout != nil {
		koH = (keepout.MaxY - keepout.MinY) + titleBlockSafety
	}
	for _, t := range schSheetLadder {
		if needW <= t.W-2*opts.Margin && needH <= t.H-2*opts.Margin-koH {
			return t.Name
		}
	}
	return ""
}

// capacityAdvice 把诊断折成一句**可执行**的话。
func capacityAdvice(cap schZoneCapacity) string {
	if cap.Fits {
		return ""
	}
	who := cap.Blocking
	if who == "" {
		who = "最大的模块"
	}
	base := fmt.Sprintf("这一页**装不下**:%s 的框要 %.0f×%.0f,而可用区(扣掉页边距与图签安全带)只有 %.0f×%.0f",
		who, cap.NeedW, cap.NeedH, cap.HaveW, cap.HaveH)
	if cap.Suggest != "" {
		return base + fmt.Sprintf(" —— 换 %s 图纸,或把这个模块单独拆一页;调 margin/gutter 无解。", cap.Suggest)
	}
	return base + " —— 标准图纸里没有装得下的,必须把这个模块拆开到多页;调 margin/gutter 无解。"
}
