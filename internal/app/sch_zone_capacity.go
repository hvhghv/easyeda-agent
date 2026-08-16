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

// fitsAroundCorner 判一个 w×h 的矩形能不能放进「可用区 usable 减去角落障碍
// obstacle」的 L 形区域。
//
// **两个命令必须用同一把尺**:zone-plan 的容量诊断与 sheet tidy 的排布诊断此前
// 各判各的 —— 同一页 P2_MCU,前者说「装不下,换 A3」、后者说「各区单独放得下,
// 是排布问题」。矛盾的根源是双方都只做了一半:前者无脑扣掉整条图签高度(过严),
// 后者只比区尺寸和整幅带尺寸(过松,完全没算图签)。
//
// 正确判据是 L 形:矩形要么塞进障碍**左侧**的窄长条(宽受限、高不受限),要么落在
// 障碍**上方**的整幅(宽不受限、高要让开)。两条都不成立才是真装不下。
func fitsAroundCorner(w, h float64, usable layoutBBox, obstacle *layoutBBox) bool {
	uw, uh := usable.MaxX-usable.MinX, usable.MaxY-usable.MinY
	if obstacle == nil {
		return w <= uw && h <= uh
	}
	leftW := obstacle.MinX - usable.MinX // 障碍左侧的净宽
	aboveH := uh - (obstacle.MaxY - usable.MinY)
	if leftW < 0 {
		leftW = 0
	}
	if aboveH < 0 {
		aboveH = 0
	}
	return (w <= leftW && h <= uh) || (w <= uw && h <= aboveH)
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
	// **图签是角落障碍,不是整条底带**。首版直接把 keepout 高度从可用高里减掉,
	// 于是 P2_MCU 被判「装不下、换 A3」——而那颗模组放在图签左边(x < keepout.MinX)
	// 根本不碰它,`sheet tidy` 同一页算出的可用带就有 691 高。**把「摆得不好」
	// 误判成「装不下」会让人白换一张大纸,而真正的毛病原封不动** —— 这正是本文件
	// 开头声明要防的那件事,首版自己犯了。
	//
	// 现在按两段算,取宽松者:
	//   ① 放在图签左侧的窄长条 —— 宽度受限、**高度不受限**;
	//   ② 跨过图签横向的整幅 —— 宽度不受限、高度要让开图签。
	// 与 sheet tidy 的「keepout 作障碍物参与行排」同一口径。
	usable := layoutBBox{
		MinX: sheet.MinX + opts.Margin, MinY: sheet.MinY + opts.Margin,
		MaxX: sheet.MaxX - opts.Margin, MaxY: sheet.MaxY - opts.Margin,
	}
	cap.HaveW, cap.HaveH = usableW, usableH

	for _, m := range modules {
		b := m.BBox // draw 口径:器件 ∪ 它自己的 marker —— 框要框住的正是这些
		w := (b.MaxX - b.MinX) + 2*partitionContentPad
		h := (b.MaxY - b.MinY) + 2*partitionContentPad + opts.TitleBand + opts.NoteBand
		if w > cap.NeedW {
			cap.NeedW = w
		}
		if h > cap.NeedH {
			cap.NeedH = h
			cap.Blocking = m.Name // 先记最大的那个;装不装得下由下面两段判据定
		}
	}
	cap.Fits = fitsAroundCorner(cap.NeedW, cap.NeedH, usable, inflatedTitleKeepout(keepout))
	if cap.Fits {
		cap.Blocking = "" // 上面那轮循环记的是"超出整幅"的粗判,这里推翻它
	} else {
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
	// 措辞必须与判据一致:判的是 L 形(图签左侧的长条 ∪ 图签上方的整幅),
	// 说成「可用区只有 W×H」会让人拿框去比那个矩形,越比越糊涂 —— 框明明比它小,
	// 凭什么说装不下?
	// **说清是哪个方向、差多少**,并且**必须写明这是「当前摆法」而不是「这一页」**。
	//
	// 用户实测质疑(2026-08-16):P2_MCU 页面上肉眼看空间很大 —— 因为水平方向确实
	// 空着一大片,紧张的只有垂直。而模块 bbox 是**器件 ∪ 它自己的 marker**,那一页
	// 576 的内容高里有一半是标签撑出来的(C4 本体 21 高、组 134 高),而 marker 的
	// 方向是可调的。把「当前摆法装不下」说成「这一页装不下」,还建议换 A3,
	// 是把一个能重排解决的问题推给了买纸。
	short := "垂直"
	gap := cap.NeedH - cap.HaveH
	if cap.NeedW > cap.HaveW && cap.NeedW-cap.HaveW > gap {
		short, gap = "水平", cap.NeedW-cap.HaveW
	}
	base := fmt.Sprintf("%s 的框在**当前摆法**下放不进:框 %.0f×%.0f,纸面去掉页边距 %.0f×%.0f,"+
		"图签占右下角(可用区是 L 形:要么窄到塞进图签左侧,要么矮到落在图签上方)—— **%s方向**不够",
		who, cap.NeedW, cap.NeedH, cap.HaveW, cap.HaveH, short)
	if gap > 0 {
		base += fmt.Sprintf("(差约 %.0f)", gap)
	}
	// 先给能自己动手的那条路:bbox 含 marker,而 marker 方向可调。
	base += "。**先试重排**:模块 bbox = 器件 ∪ 它自己的 marker,竖排的标签能把组撑高好几倍" +
		"(实测本体 21 高的电容,组高 134;改成横向后 58)。步骤:①`sch clusters` 看「组高 vs 本体高」" +
		"找出被自己 marker 撑大的组;②对它的脚 `sch disconnect --pin X:n` + " +
		"`sch connect --pin X:n --kind … --net … --direction left|right`(**方向要用 `sch connect`;" +
		"`autoconnect` 自己打分选方向,不接受 --direction**);③件本身超出主芯片 y 跨度时还要挪件" +
		"(`sch group-move`)。真机实测:改 4 个标签方向,一页总高 576→537"
	// **必须标明换纸是人工动作**:2026-08-16 实测,平台没有任何改图纸尺寸的 API ——
	// dmt_Schematic 的 17 个方法里没有,运行时扫全部 eda.* 命名空间对
	// sheet|paper|size|format 零命中,getSchematicPageInfo 不返回尺寸字段,
	// sheet 图元也只有通用的 setState_X/Y/Rotation(bbox 是渲染结果不是可写属性)。
	// 不写清楚的话,这条建议看起来像 CLI 能做的事,而 agent 会去找那条不存在的命令。
	if cap.Suggest != "" {
		return base + fmt.Sprintf(";重排后仍放不下再考虑 ①手工把图纸改成 %s"+
			"(**平台无 API,只能人工**)或 ②拆到单独一页(`sch page-new`)。调 margin/gutter 无解。", cap.Suggest)
	}
	return base + ";重排后仍放不下就必须拆到多页(`sch page-new`)——标准图纸里没有装得下的。调 margin/gutter 无解。"
}
