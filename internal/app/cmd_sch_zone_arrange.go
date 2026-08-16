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

// schSheetOrA4 返回活页图框 bbox;图框图元缺失时按 A4-only 域界假定(带标志)。
func schSheetOrA4(comps []layoutComp) (*layoutBBox, bool) {
	if s := sheetBBoxOf(comps); s != nil {
		return s, false
	}
	return &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}, true
}

// zfGroupFromCluster 把一个 L1 虚拟组折成 phase A 的类型化输入。
// 端子挂侧由 marker 中心相对器件本体中心的主轴判定(确定性,无打分)。
func zfGroupFromCluster(c schCluster, pinCount int) zfGroup {
	bcx, bcy := bboxCenter(c.Body)
	g := zfGroup{
		Designator: c.Designator,
		BodyW:      c.Body.MaxX - c.Body.MinX,
		BodyH:      c.Body.MaxY - c.Body.MinY,
		MultiPin:   pinCount > 2,
	}
	for _, m := range c.Typed {
		var kind string
		switch m.Kind {
		case "netport":
			kind = "netport"
		case "netflag", "netlabel":
			kind = "netflag"
		default:
			continue // part / wire 不是端子
		}
		mcx, mcy := bboxCenter(m.BBox)
		dx, dy := mcx-bcx, mcy-bcy
		side := "left"
		if absF(dx) >= absF(dy) {
			if dx > 0 {
				side = "right"
			}
		} else {
			side = "down"
			if dy > 0 {
				side = "up"
			}
		}
		g.Terms = append(g.Terms, zfTerm{Kind: kind, Net: m.Net,
			W: m.BBox.MaxX - m.BBox.MinX, H: m.BBox.MaxY - m.BBox.MinY, Side: side})
	}
	return g
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
	sheet, sheetAssumed := schSheetOrA4(comps)
	keepout, _ := titleBlockKeepout(sheet)
	// 标签入框是硬约束:导线是端子归属的唯一可靠来源,读不到就不规划。
	wires, werr := fetchSchWirePolylines(cfg, window, docUUID)
	if werr != nil {
		return nil, nil, fmt.Errorf("zone-arrange 需要导线数据做端子归属(标签入框是硬约束,距离启发式必错):%w", werr)
	}
	clusters, _ := buildSchClusters(comps, wires)
	byDesig := map[string]schCluster{}
	for _, c := range clusters {
		byDesig[strings.ToUpper(c.Designator)] = c
	}
	pinCount := map[string]int{}
	for _, c := range comps {
		if c.ComponentType == "part" {
			pinCount[strings.ToUpper(label(c))] = len(c.Pins)
		}
	}

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
			groups = append(groups, zfGroupFromCluster(c, pinCount[strings.ToUpper(d)]))
			zfGrow(&raw, &hasRaw, c.Box)
		}
		if len(groups) == 0 {
			continue
		}
		plan, ferr := planZoneFollow(name, groups, opts)
		if ferr != nil {
			return nil, nil, fmt.Errorf("phase A(%s): %w", name, ferr)
		}
		rawW := (raw.MaxX - raw.MinX) + 2*partitionContentPad
		rawH := (raw.MaxY - raw.MinY) + 2*partitionContentPad + opts.TitleBand + opts.NoteBand
		home := [2]float64{(raw.MinX + raw.MaxX) / 2, (raw.MinY + raw.MaxY) / 2}
		out.Zones = append(out.Zones, zoneArrangeZoneOut{
			Name: name, Mode: plan.Mode, RawW: rawW, RawH: rawH,
			FrameW: plan.FrameW, FrameH: plan.FrameH, Home: home, Groups: plan.Groups,
			Content: plan.Content,
		})
		zaZones = append(zaZones, zaZone{Name: name, W: plan.FrameW, H: plan.FrameH, Home: home})
	}
	if len(zaZones) == 0 {
		return nil, nil, fmt.Errorf("no zone resolved any parts on this page — 认领的件不在本页(place / `doc switch`)")
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
	return out, &zaScene{comps: comps, wires: wires}, nil
}

func renderZoneArrange(out *zoneArrangeOut, w io.Writer) {
	if out.SheetAssumed {
		fmt.Fprintf(w, "⚠ 本页没有图框图元 —— 按 A4-only 域界假定 1170×825 + 标准图签角规划;图框需人工在 EasyEDA UI 重放(平台无重建 API)\n")
	}
	fmt.Fprintf(w, "phase A 区内收敛(跟随规则 R1-R5)\n")
	for _, z := range out.Zones {
		fmt.Fprintf(w, "  %-8s %-42s 框 %.0f×%.0f → %.0f×%.0f\n",
			z.Name, z.Mode, z.RawW, z.RawH, z.FrameW, z.FrameH)
	}
	if !out.Arrange.OK {
		fmt.Fprintf(w, "phase B 落位:blocked —— %s 无处可放,回退链已试尽:%s\n",
			out.Arrange.Blocked, out.Arrange.Tried)
		fmt.Fprintf(w, "verdict: blocked(出路:进一步收敛该区,或 `sch page-new` 拆页 —— A4-only,不建议换纸)\n")
		return
	}
	fmt.Fprintf(w, "phase B 落位(边归属 → 回退链 → 货架扫描)\n")
	for _, p := range out.Arrange.Placed {
		fb := ""
		if p.Edge != p.Chain[0] {
			fb = fmt.Sprintf("(回退,首选 %s)", p.Chain[0])
		}
		fmt.Fprintf(w, "  %-8s %s%-14s steps %-4d 框 [%.0f,%.0f → %.0f,%.0f]\n",
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
		Short: "Two-phase deterministic zone layout plan: intra-zone compaction (R1-R5) + edge-affinity shelf placement (A4-only, no mutation)",
		Long: `Plan the whole-page functional-zone layout deterministically — same input, ONE output:

  phase A  区内收敛:卫星无源件竖放平行跟随锚件(R1-R5;GND 下/电源上是推论不是查表)
  phase B  区间求解:边归属(质心回退+回退链)→ 货架扫描(只沿边轴,5 格律)
  验证     复用 zone-plan 的 validatePartitions(同一把尺)
  输出     三态:pass / blocked(报出是谁、回退链每条边距离)—— 永不「大概摆一下」

A4-only:装不下的出路是收敛或 ` + "`sch page-new`" + ` 拆页,不建议换纸。
纯规划零改动;落地执行(--apply)未接入 —— 要先补齐 ADR-0003 执行断言。`,
		Example: `  easyeda sch zone-arrange --project ceshi --doc P3_USB_DEBUG --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			pinnedCfg, win, docUUID, err := pinZonePage(cfg, *window)
			if err != nil {
				return err
			}
			opts := partitionOptsFrom(margin, gutter, titleBand, 0, 0)
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
