package app

// cmd_sch_zone_arrange.go — `sch zone-arrange`:功能区两段布局的 CLI 入口。
//
//	phase A 区内收敛(sch_zone_follow.go,跟随规则 R1–R5)
//	phase B 区间求解(sch_zone_arrange.go,边归属 + 回退链 + 货架扫描)
//	验证        同一把尺(validatePartitions 本体)
//	输出        三态:pass / blocked(报出是谁、回退链每条边距离)
//
// 设计对齐 2026-08-16 演示页 v3(用户逐条裁定):A4-only、标签入框、卫星跟随锚件。
// 稳定性(用户确认):确定的元器件集合 → 每次同一解;区内小幅挪件不改变质心平局
// 就不改变输出 —— 位置只参与边归属与排序平局,不参与落位坐标。
//
// **--plan(默认)是纯规划,零改动。**数据流:
//	zones claims(成员单一事实来源)+ components.list(bbox+pins)+ 导线
//	→ buildSchClusters(L1 归属)→ zfGroup(类型化端子)→ planZoneFollow(收敛)
//	→ zonesArrange(落位)→ zaValidate → verdict。
//
// 标签入框是硬约束:导线读不到**直接报错**(不像 zone-plan 降级可见)——收敛规划
// 依赖端子归属,距离启发式在这里必错,静默降级会规划出把标签甩在框外的收敛。
//
// --apply 走 ADR-0003 舞步,J_USB 事故的两条断言是执行前后的硬门 ——
// 见 cmd_sch_zone_arrange_apply.go(断言①删除集=重建集、断言②曾连接 pin 仍连接)。

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// zoneArrangeZoneOut 是一个区的规划输出。
type zoneArrangeZoneOut struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
	// RawW/H 是现状口径框(L1 全图元并集 + pad + 带),对照收敛效果。
	RawW   float64         `json:"rawW"`
	RawH   float64         `json:"rawH"`
	FrameW float64         `json:"frameW"`
	FrameH float64         `json:"frameH"`
	Home   [2]float64      `json:"home"`
	Groups []zfPlacedGroup `json:"groups"` // 区内局部坐标(说明带上沿为 y=0 基准之上)
	// Retained:本区没有采纳收敛(「不得变差」门拦下,原形保留)。**无 omitempty**
	// —— false 被抹掉的话读的人分不清「没回退」与「这版没这个字段」。
	Retained bool `json:"retained"`
	// RetainWhy 是回退理由(同一句话也在 Mode 尾巴上)。
	RetainWhy string `json:"retainWhy,omitempty"`
	// Content 是收敛后全图元并集(区内局部)—— 执行侧把局部坐标映射到落位框的
	// 绝对坐标要靠它:abs = rect.Min + (pad, noteBand+pad) + (local − Content.Min)。
	Content layoutBBox `json:"content"`
}

// zoneArrangeOut 是 --json 的完整输出。
type zoneArrangeOut struct {
	Sheet      layoutBBox           `json:"sheet"`
	Keepout    *layoutBBox          `json:"keepout,omitempty"`
	Zones      []zoneArrangeZoneOut `json:"zones"`
	Arrange    zaResult             `json:"arrange"`
	Validation *partitionValidation `json:"validation,omitempty"`
	Verdict    string               `json:"verdict"` // pass | blocked
	// SheetAssumed:页上没有图框图元(sheet),按 A4-only 域界假定 1170×825 +
	// 标准图签角在规划。真机 2026-08-16:P3 的图框在一次连接器停摆期的 save 中
	// 丢失,而平台没有任何重建图框的 API(sheet 组件 uuid 为空,titleblock 写
	// 通道对无框页拒写)—— 唯一修复是**人工**在 EasyEDA UI 给该页重放图框。
	// 假定必须在输出里可见,不许静默。
	SheetAssumed bool `json:"sheetAssumed,omitempty"`
}

// zoneArrangeRawFrame 是 phase A 的**现状口径框**尺寸(L1 全图元并集 + pad +
// 区名带 + 说明带)。与 zone-plan 第一遍的 partitionFirstPassRect 共用同一个函数
// 本体(partitionFrame*),同一份带高口径 —— 逐字段配对由 ruler 一致性测试钉住。
func zoneArrangeRawFrame(raw layoutBBox, opts partitionOpts, noteH float64) (w, h float64) {
	return partitionFrameSize(raw, opts.TitleBand, schZoneNoteBandHeight(opts.NoteBand, noteH))
}

// schSheetOrA4 返回活页图框 bbox;图框图元缺失时按 A4-only 域界假定(带标志)。
func schSheetOrA4(comps []layoutComp) (*layoutBBox, bool) {
	if s := sheetBBoxOf(comps); s != nil {
		return s, false
	}
	return &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}, true
}

// zfSideOf 判一支端子挂在器件本体的**哪一条边** —— phase A 收敛的入口判定。
//
// ── 口径:边界语义,不是「相对中心的位移分量谁大」(2026-08-20 真机定案)────────
//
// 首版判据是 `|dx| ≥ |dy|` —— marker 中心相对**本体中心**的主轴。它隐含假设
// 「本体近似方形」:只有方形本体上,「离中心的哪个分量大」才等价于「挂在哪条边」。
// 高瘦符号上这个假设系统性失效:ESP32-S3-WROOM-1 本体 71×421(41 脚全在左右两侧),
// marker 横向触达只有百来个单位,于是**贴在上下两端行引脚旁的 marker,|dy| 反而
// 大于 |dx|**,被判成 up/down —— 它们物理上就在左右两侧。判成 up/down 就进
// zfGenMultiPin 的垂直梯次(桩长逐支递增),真机 MCU_IO 那一区的框因此
// 449×737 → 244×863:宽收了 205,高涨了 126,直接越过可用高 765,phase B 当场
// 四条边全报「纸面放不下」。
//
// 边界语义问的是另一个问题:**marker 中心从本体 bbox 的哪条边探出去最多**。
//
//	outLeft = body.MinX − mcx   outRight = mcx − body.MaxX
//	outDown = body.MinY − mcy   outUp    = mcy − body.MaxY
//
// 取四者的 argmax。这个式子天然把本体的长宽比算了进去(高瘦本体的上下两条边
// 「远」,横向探出一点点就赢),而中心口径把长宽比丢了。marker 中心落在本体
// bbox 之内时四个量全为负,argmax 退化成「离哪条边最近」—— 同一个式子,不需要
// 特例分支。平局序 left < right < down < up(与首版一致:首版 `≥` 偏横向、
// dx==0 取 left)。
//
// 判定与实测口径的配对:落地/回退侧的 zfMeasureCluster 用 tidyStubDirection
// (pin → 标记锚的实测位移)定方向。两处判的是同一件事,必须给同一个答案 ——
// TestZfSideOf_AgreesWithMeasuredStubDirection 把这条钉住。
func zfSideOf(body, marker layoutBBox) string {
	mcx, mcy := bboxCenter(marker)
	return zfPointSideOf(body, mcx, mcy)
}

// zfPointSideOf 是同一条边界语义的**点版本**:一只引脚(或一个标记锚)从本体
// bbox 的哪条边探出去最多。zfSideOf 就是它在 marker 中心上的特化 —— 两处判的是
// 同一件事,必须是同一个函数本体(各写一份就是又造一把尺)。
//
// 引脚版是 phase A 端子朝向的**唯一真值来源**:引脚坐标是符号锁死的事实,而
// marker 位置只是上一轮布线的结果 —— 拿 marker 定朝向会让收敛跟着上一轮的残局
// 走(真机三轮不收敛的一半原因)。
func zfPointSideOf(body layoutBBox, px, py float64) string {
	best, bestOut := "left", body.MinX-px
	for _, c := range []struct {
		side string
		out  float64
	}{
		{"right", px - body.MaxX},
		{"down", body.MinY - py},
		{"up", py - body.MaxY},
	} {
		if c.out > bestOut {
			best, bestOut = c.side, c.out
		}
	}
	return best
}

// zfGroupFromCluster 把一个 L1 虚拟组折成 phase A 的类型化输入。
//
// ── 端子必须逐 **pin** 折,而且必须带上引脚坐标(2026-08-20 真机定案)────────────
//
// 首版逐 **marker** 折(c.Typed),只给类型/网名/挂侧,不给引脚位置。两处结构性
// 后果,真机 MCU_IO 连跑三轮都在同一处踩:
//
//	① **覆盖面**:cluster 的「专属 marker」规则不把共树 marker 算给本组
//	   (buildSchClusters ③:触到多个器件的导线连通块不属于任何一组),于是共树
//	   pin 在计划里根本没有端子 —— 而 --apply 是逐 pin 重建的(groupRebuildConnSpecs
//	   按活体网表出规格)。计划漏掉的那几只 pin 落地时走 autoconnect 自由评分,
//	   区框凭空胖一档,断言③ 只能如实报红。zfMeasureCluster 早就是逐 pin 折的
//	   (它的注释写着为什么),规划侧却没跟上。
//	② **朝向**:没有引脚坐标,zfGenPassive 只能**假定**两只脚在本体上下缘中线上,
//	   再按 R3 把 GND 派到下端。真机 C4/C6(rot 90,pin1=+3V3 在本体下方、
//	   pin2=GND 在本体上方)正好是反的 —— 计划把 GND 端子派给了物理上在**上面**
//	   的那只脚却给它 direction=down,把 +3V3 派给下面那只脚却给 direction=up,
//	   两根桩线双双钻进本体、当场合并 → **GND 整张网并进 +3V3**。页级网表一旦
//	   串,对账红 → 恢复段把全页 9 只地脚按自由评分重连 → 落地几何与计划无关,
//	   报文那句「计划未覆盖」其实是这条路径的产物。
//
// 所以:有实测几何(Measured,逐 pin 从活体折出)时一律按它折端子,引脚坐标折成
// **本体局部坐标**带进计划;挂侧改用引脚自己的边界语义(zfPointSideOf)。没有
// 实测(纯几何单测 / 老调用点)才退回逐 marker 的首版口径。
func zfGroupFromCluster(c schCluster, pinCount int, m *zfMeasured) zfGroup {
	g := zfGroup{
		Designator: c.Designator,
		BodyW:      c.Body.MaxX - c.Body.MinX,
		BodyH:      c.Body.MaxY - c.Body.MinY,
		MultiPin:   pinCount > 2,
	}
	if m != nil {
		// 逐 pin:落地会重建的端子集合 = 这一份(与 groupRebuildConnSpecs 同源)。
		for _, t := range m.Terms {
			g.Terms = append(g.Terms, zfTerm{Kind: t.Kind, Net: t.Net,
				Side:   zfPointSideOf(c.Body, t.PinX, t.PinY),
				PinX:   t.PinX - c.Body.MinX,
				PinY:   t.PinY - c.Body.MinY,
				HasPin: true})
		}
		return g
	}
	for _, tm := range c.Typed {
		var kind string
		switch tm.Kind {
		case "netport":
			kind = "netport"
		case "netflag", "netlabel":
			kind = "netflag"
		default:
			continue // part / wire 不是端子
		}
		g.Terms = append(g.Terms, zfTerm{Kind: kind, Net: tm.Net,
			W: tm.BBox.MaxX - tm.BBox.MinX, H: tm.BBox.MaxY - tm.BBox.MinY,
			Side: zfSideOf(c.Body, tm.BBox)})
	}
	return g
}

// zfMeasureCluster 折出一个组的**现状实测几何**(「不得变差」门回退原形的原料)。
//
// 端子逐 **pin** 折(不是逐 marker):断言①比的是「计划端子网名多重集 == 已连接
// pin 网名多重集」,只有从 pin 出发才保证多重集一致 —— 共树 pin 的 marker 归不到
// 本组(cluster 的「专属 marker」规则),逐 marker 折就会少端子,原形计划反而过不了
// 自己的门。方向/桩长由 tidyStubDirection 从 pin→标记锚的实测位移量出来,
// 与 move 内核的 preserve 策略同一口径。
//
// 返回 nil:本体 bbox 读不到(没有实测就没有原形可保)。
func zfMeasureCluster(c schCluster, part layoutComp, wires []schGroupWire, roots []int,
	markers []layoutComp) *zfMeasured {
	if part.BBox == nil {
		return nil
	}
	m := &zfMeasured{Body: *part.BBox, Box: c.Box}
	for _, p := range part.Pins {
		mk, hasM, onWire := tidyPinAttachment(p.X, p.Y, wires, roots, markers)
		if !onWire || !hasM {
			continue // 悬空 / 普通导线直连:重建不了,交给 --apply 的断言①原样拒
		}
		kind := "netflag"
		if mk.ComponentType == "netport" {
			kind = "netport"
		}
		dir, off := tidyStubDirection(p.X, p.Y, mk.X, mk.Y)
		m.Terms = append(m.Terms, zfPlacedTerm{Kind: kind, Net: mk.Net, Dir: dir,
			PinX: p.X, PinY: p.Y, Offset: off})
	}
	// 确定性:平台给的 pin 顺序不进判据。自上而下、左右次之、同点按网名。
	sort.SliceStable(m.Terms, func(i, j int) bool {
		a, b := m.Terms[i], m.Terms[j]
		if a.PinY != b.PinY {
			return a.PinY > b.PinY
		}
		if a.PinX != b.PinX {
			return a.PinX < b.PinX
		}
		return a.Net < b.Net
	})
	return m
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// zaScene 是规划所用的那一份场景快照。--apply 必须与规划共用同一快照
// (判定坐标 = 落地坐标定律):重新取数得到的场景可能已变,拿它执行等于
// 按另一张图落位。
type zaScene struct {
	comps []layoutComp
	wires []schGroupWire
}

// computeZoneArrange 取真机数据 → 两段规划。纯读,零改动。
func computeZoneArrange(cfg *appConfig, window, docUUID string, opts partitionOpts) (*zoneArrangeOut, *zaScene, error) {
	zones, project, err := loadSchZoneModules(cfg, window, docUUID)
	if err != nil {
		return nil, nil, err
	}
	if len(zones) == 0 {
		return nil, nil, fmt.Errorf("%q 这一页既没有虚拟组也没有 zone 认领 —— 用 `sch block-apply` 落块,或手工 `sch group create` / `sch zones set`", project)
	}
	if err := ensureActiveDoc(cfg, window); err != nil {
		return nil, nil, fmt.Errorf("zone-arrange: restore pinned page %s: %w", docUUID, err)
	}
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", window,
		map[string]any{"includeBBox": true, "includePins": true}, docUUID, "read zone-arrange geometry")
	if err != nil {
		return nil, nil, err
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return nil, nil, perr
	}
	// 标签入框是硬约束:导线是端子归属的唯一可靠来源,读不到就不规划。
	wires, werr := fetchSchWirePolylines(cfg, window, docUUID)
	if werr != nil {
		return nil, nil, fmt.Errorf("zone-arrange 需要导线数据做端子归属(标签入框是硬约束,距离启发式必错):%w", werr)
	}
	// **收紧必须把区名带 + 说明带一起算进去,收紧完再画框**(用户裁定 2026-08-20)。
	// 此前 phase A 用的是常量 `opts.NoteBand`,而 zone-plan 侧的带高按**已登记说明
	// 的实际渲染高度**算 —— 两套带账。按常量带收紧出来的框,画完再放 note 就装不下,
	// 说明探出框外(9ee3e13 自己挂的那笔账)。尺寸只由内容+字号推导,与落点无关。
	noteSizes := zoneNoteSizes(zones, fetchZoneNoteTexts(cfg, window, docUUID, zones))
	out, perr := planZoneArrangeScene(zones, comps, wires, noteSizes, opts)
	if perr != nil {
		return nil, nil, perr
	}
	return out, &zaScene{comps: comps, wires: wires}, nil
}

// planZoneArrangeScene 是 zone-arrange 规划的**纯函数核心**:一份场景快照
// (器件 + 导线 + 认领表 + 说明带高)进,两段规划出。I/O 全部留在 computeZoneArrange
// 里 —— 这样「计划覆盖了哪些 pin」这类判据可以在离线 fixture 上机械验收,
// 不必开着 EasyEDA 才敢改规划器。
func planZoneArrangeScene(zones map[string]*schZoneClaim, comps []layoutComp, wires []schGroupWire,
	noteSizes map[string]zoneNoteSize, opts partitionOpts) (*zoneArrangeOut, error) {

	sheet, sheetAssumed := schSheetOrA4(comps)
	keepout, _ := titleBlockKeepout(sheet)
	clusters, _ := buildSchClusters(comps, wires)
	byDesig := map[string]schCluster{}
	for _, c := range clusters {
		byDesig[strings.ToUpper(c.Designator)] = c
	}
	pinCount := map[string]int{}
	// partOf / roots / markers 是「不得变差」门回退原形时的实测原料
	// (zfMeasureCluster):逐 pin 折现状端子要它们。
	partOf := map[string]layoutComp{}
	var markers []layoutComp
	for _, c := range comps {
		if c.ComponentType == "part" {
			pinCount[strings.ToUpper(label(c))] = len(c.Pins)
			partOf[strings.ToUpper(label(c))] = c
		}
		if isSchMarker(c.ComponentType) {
			markers = append(markers, c)
		}
	}
	roots := tidyWireRoots(wires)

	names := make([]string, 0, len(zones))
	for n := range zones {
		names = append(names, n)
	}
	sort.Strings(names)

	out := &zoneArrangeOut{Sheet: *sheet, Keepout: keepout, SheetAssumed: sheetAssumed}
	var zaZones []zaZone
	for _, name := range names {
		zc := zones[name]
		if zc == nil {
			continue
		}
		var groups []zfGroup
		var raw layoutBBox
		hasRaw := false
		for _, d := range zc.Parts {
			c, ok := byDesig[strings.ToUpper(d)]
			if !ok {
				continue
			}
			// 实测先算:它既是「不得变差」门的回退原料,**也是计划端子的来源**
			// (逐 pin + 引脚坐标)—— 顺序反了就等于又回到逐 marker 的首版口径。
			measured := zfMeasureCluster(c, partOf[strings.ToUpper(d)], wires, roots, markers)
			g := zfGroupFromCluster(c, pinCount[strings.ToUpper(d)], measured)
			g.Measured = measured
			groups = append(groups, g)
			zfGrow(&raw, &hasRaw, c.Box)
		}
		if len(groups) == 0 {
			continue
		}
		// 本区的**有效带高**:有登记说明就按它的渲染高度,没有就用默认带。
		// planZoneFollow 收敛后的框(FrameW/FrameH)与这里的现状框(rawW/rawH)
		// 都走同一个 partitionFrame* 本体,zone-plan 的第一遍框也是它 —— 一把尺。
		zopts := opts
		zopts.NoteBand = schZoneNoteBandHeight(opts.NoteBand, noteSizes[name].H)
		// **「不得变差」门**(2026-08-20 真机):收敛在大符号单件组上是负优化 ——
		// esp32s3_wroom1_module 433×541 → 244×767,宽收了 189 而高涨了 226,
		// 越过可用高 765 → phase B blocked,而不收敛本来排得下。门按**可排布性**
		// (不是面积)逐区判:原形可排而收敛不可排就保留原形,理由挂进 Mode。
		//
		// **域是 phase A 的输入,不只是门的输入**(2026-08-20 第二笔真机取证):
		// 收敛给一个区选形状时也要看空地长什么样 —— 域盲的 argmin max(w,h) 会把
		// 5 个小无源件排成只进得了左通道的柱子,跟主控区抢同一条道。zfDomainFor
		// 是与 phase B(newZaSearch)同源的那一份域,选形与门共用它。
		plan, ferr := planZoneFollowGated(name, groups, zopts, zfDomainFor(*sheet, keepout, opts))
		if ferr != nil {
			return nil, fmt.Errorf("phase A(%s): %w", name, ferr)
		}
		rawW, rawH := zoneArrangeRawFrame(raw, opts, noteSizes[name].H)
		home := [2]float64{(raw.MinX + raw.MaxX) / 2, (raw.MinY + raw.MaxY) / 2}
		out.Zones = append(out.Zones, zoneArrangeZoneOut{
			Name: name, Mode: plan.Mode, RawW: rawW, RawH: rawH,
			FrameW: plan.FrameW, FrameH: plan.FrameH, Home: home, Groups: plan.Groups,
			Content: plan.Content, Retained: plan.Retained, RetainWhy: plan.RetainWhy,
		})
		zaZones = append(zaZones, zaZone{Name: name, W: plan.FrameW, H: plan.FrameH, Home: home})
	}
	if len(zaZones) == 0 {
		return nil, fmt.Errorf("no zone resolved any parts on this page — 认领的件不在本页(place / `doc switch`)")
	}
	out.Arrange = zonesArrange(zaZones, *sheet, keepout, opts)
	if out.Arrange.OK {
		v := zaValidate(out.Arrange, *sheet, keepout, opts)
		out.Validation = &v
		out.Verdict = "pass"
		if v.SheetOverflow != 0 || v.PartitionOverlap != 0 || v.TitleBlockHits != 0 || v.SheetMarginHits != 0 {
			// 结构上不该发生(求解器与验证器同口径);真发生 = 求解器缺陷,如实报。
			out.Verdict = "blocked"
		}
	} else {
		out.Verdict = "blocked"
	}
	return out, nil
}

func renderZoneArrange(out *zoneArrangeOut, w io.Writer) {
	if out.SheetAssumed {
		fmt.Fprintf(w, "⚠ 本页没有图框图元 —— 按 A4-only 域界假定 1170×825 + 标准图签角规划;图框需人工在 EasyEDA UI 重放(平台无重建 API)\n")
	}
	fmt.Fprintf(w, "phase A 区内收敛(跟随规则 R1-R5;**域感知选形**:候选先比可排布档位、再比「几条通道装得下」,平局才回到原有紧凑序;「不得变差」门:收敛使本区排不下就保留原形)\n")
	for _, z := range out.Zones {
		mark := " " // 回退必须一眼可见,不许只藏在 Mode 的长句里
		if z.Retained {
			mark = "↩"
		}
		fmt.Fprintf(w, " %s%-8s %-42s 框 %.0f×%.0f → %.0f×%.0f\n",
			mark, z.Name, z.Mode, z.RawW, z.RawH, z.FrameW, z.FrameH)
	}
	if !out.Arrange.OK {
		how := "回退链 + 多层货架已试尽"
		if out.Arrange.Exhausted {
			how = "搜索预算耗尽(未证明无解,但本域界内没搜到)"
		}
		fmt.Fprintf(w, "phase B 落位:blocked —— %s 无处可放,%s:%s\n",
			out.Arrange.Blocked, how, out.Arrange.Tried)
		fmt.Fprintf(w, "verdict: blocked(出路:进一步收敛该区,或 `sch page-new` 拆页 —— A4-only,不建议换纸)\n")
		return
	}
	fmt.Fprintf(w, "phase B 落位(边归属 → 回退链 → 多层货架装箱 + 回溯)\n")
	for _, p := range out.Arrange.Placed {
		fb := ""
		if p.Edge != p.Chain[0] {
			fb = fmt.Sprintf("(回退,首选 %s)", p.Chain[0])
		}
		if p.Shelf > 0 {
			fb += fmt.Sprintf("(第%d层货架)", p.Shelf+1)
		}
		fmt.Fprintf(w, "  %-8s %s%-18s steps %-4d 框 [%.0f,%.0f → %.0f,%.0f]\n",
			p.Name, p.Edge, fb, p.Steps, p.Rect.MinX, p.Rect.MinY, p.Rect.MaxX, p.Rect.MaxY)
	}
	if out.Validation != nil {
		fmt.Fprintf(w, "validation: sheetOverflow=%d partitionOverlap=%d titleBlockHits=%d sheetMarginHits=%d\n",
			out.Validation.SheetOverflow, out.Validation.PartitionOverlap,
			out.Validation.TitleBlockHits, out.Validation.SheetMarginHits)
	}
	fmt.Fprintf(w, "verdict: %s\n", out.Verdict)
}

// newSchZoneArrangeCmd 注册 `sch zone-arrange`。
func newSchZoneArrangeCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var asJSON, apply bool
	var margin, gutter, titleBand float64
	c := &cobra.Command{
		Use:   "zone-arrange",
		Short: "Two-phase deterministic zone layout plan: intra-zone compaction (R1-R5) + edge-affinity multi-shelf packing (A4-only, no mutation)",
		Long: `Plan the whole-page functional-zone layout deterministically — same input, ONE output:

  phase A  区内收敛:卫星无源件竖放平行跟随锚件(R1-R5;GND 下/电源上是推论不是查表)
           带**「不得变差」门**:收敛后的框若在本页(A4 域界 + 图签 keep-out)
           不再有落点,就保留原形(行首 ↩,理由挂在 mode / JSON retainWhy)
  phase B  区间求解:边归属(质心回退+回退链)→ 每条边可开多层货架(第二列/第二行)
           → 放不下就回溯换上一个区的候选(确定性 DFS,5 格律,无随机)
  验证     复用 zone-plan 的 validatePartitions(同一把尺)
  输出     三态:pass / blocked(报出是谁、每条边卡在谁身上)—— 永不「大概摆一下」

A4-only:装不下的出路是收敛或 ` + "`sch page-new`" + ` 拆页,不建议换纸。
默认 dry-run 纯规划零改动;--apply 走 ADR-0004 move 内核落地(页级 sweep →
落位重连 → 电气对账,失败自动恢复,救不回的 pin 以 REF→期望网清单退出)。`,
		Example: `  easyeda sch zone-arrange --project ceshi --doc P3_USB_DEBUG --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// ADR-0004 Decision 4(dry-run 纯计算铁律)在此**有意不接**
			// setDispatchDryRun:computeZoneArrange 硬依赖 fetchSchWirePolylines
			// (debug.exec_js 读通道,catalog 上 Mutates=true),接了会把 dry-run
			// 规划直接打死。等导线读升格为 typed action 后再接。
			pinnedCfg, win, docUUID, err := pinZonePage(cfg, *window)
			if err != nil {
				return err
			}
			opts := partitionOptsFrom(margin, gutter, titleBand)
			out, scene, err := computeZoneArrange(pinnedCfg, win, docUUID, opts)
			if err != nil {
				return err
			}
			if asJSON && !apply {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			renderZoneArrange(out, stdout)
			if !apply {
				if out.Verdict != "pass" {
					return fmt.Errorf("zone-arrange: %s", out.Verdict)
				}
				fmt.Fprintln(stdout, "dry-run(默认):未改画布 —— 加 --apply 落地(断言① → 页级 sweep → 落位重连 → 断言② → 自检)")
				return nil
			}
			return runZoneArrangeApply(pinnedCfg, win, docUUID, out, scene, opts, stdout, stderr)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit the full two-phase plan + validation as JSON")
	c.Flags().BoolVar(&apply, "apply", false, "落地执行:断言①(删除集=重建集) → 页级深度清扫 → 逐件落位重连 → 断言②(曾连接 pin 仍连接) → layout-lint+bridge-check → save;任一红逐步回滚")
	def := defaultPartitionOpts()
	c.Flags().Float64Var(&margin, "margin", def.Margin, "page margin inset from the sheet edge")
	c.Flags().Float64Var(&gutter, "gutter", def.Gutter, "gutter between zone frames (and keep-out inflation)")
	c.Flags().Float64Var(&titleBand, "title-band", def.TitleBand, "height of each zone's title band")
	return c
}
