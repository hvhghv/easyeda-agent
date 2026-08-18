package app

// cmd_sch_sheet_tidy.go — `sch sheet tidy`:三层布局体系的 Sheet 层(设计契约
// docs/schematic-layout-hierarchy.md;用户蓝图:「最终去适配纸张信息的,应该是
// 功能区布局调整」)。
//
// 把每个功能区(zones claim)当刚体——区 bbox 用与分区框同一口径(器件 ∪ 近旁
// 旗 ∪ 登记说明,即 computePartitionPlan 的 modules[].BBox)——在纸张可用区
// (sheet − margin,底部让出图签 keepout+safety 保守带,v1;L 形带留 v2)内
// 复用 planZonePack 排布:最大区为锚、行排、区间距 hGap/vGap。
//
// --apply:对每个非零 Δ 的区调 runSchZoneMove(不逐区重画框),全部完成后统一
// 重画一次分区框;层级联动:zone move → 组 → 器件+桩+旗+登记 note 全部随行。

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"
)

const (
	// hGap 必须 > 两框 pad(24+24=48):区内容间距决定框间距,60 留 12 缝。
	sheetTidyHGapDefault = 60.0
	// vGap 必须 > 两框 pad + 下区标题带(24+24+30=78):40 时相邻两行的框在
	// 垂直方向必然相叠(实测踩坑),90 留 12 缝。
	sheetTidyVGapDefault = 90.0
)

// planSheetTidy 是纯规划:zones(名字→draw bbox)在纸张带内 pack。
// sheetTidyDiag 填一份**能行动**的失败诊断。
//
// 三个失败分支原本只填 BandW/BandH,NeedW/NeedH 从没算过 —— 于是错误消息照着模板
// 打出「纸面装不下(needW=0 needH=0 bandW=1066 bandH=691)」:需要 0×0 却装不下,
// 读的人只能困惑。不是算错,是压根没算(2026-08-16 真机)。
//
// 现在分两种,因为**修法完全不同**:
//   - 单个区自己就超出可用带 → 换更大图纸/拆页,再怎么排都没用;
//   - 每个区都装得下、合起来排不进 → 拆页或收紧区内布局(`sch zone tidy`)。
func sheetTidyDiag(reason string, groups []zonePackGroup, band layoutBBox, obs []layoutBBox) *zonePackDiag {
	d := &zonePackDiag{Reason: reason, BandW: band.MaxX - band.MinX, BandH: band.MaxY - band.MinY}
	var corner *layoutBBox
	if len(obs) > 0 {
		corner = &obs[0] // sheet tidy 只有图签一个障碍
	}
	var area float64
	var worst string
	for _, g := range groups {
		w := g.BBox.MaxX - g.BBox.MinX
		h := g.BBox.MaxY - g.BBox.MinY
		area += w * h
		if w > d.NeedW {
			d.NeedW = w
		}
		if h > d.NeedH {
			d.NeedH = h
		}
		// **判「单区放不进」要算上图签这个角落障碍**,与 zone-plan 的容量诊断
		// 同一把尺(fitsAroundCorner)。只比区尺寸和整幅带尺寸会过松 —— 真机
		// P2_MCU 因此被说成「各区单独放得下,是排布问题」,而 zone-plan 同一页
		// 说「装不下」:同一页两个命令两个结论,读的人无所适从。
		if !fitsAroundCorner(w, h, band, corner) {
			worst = g.ID
		}
	}
	switch {
	case worst != "":
		d.Reason = fmt.Sprintf("%s —— 功能区 %q 自己就放不进可用带(它 %.0f×%.0f,带 %.0f×%.0f,"+
			"且避不开图签角落):换更大图纸或把它拆到下一页,重排没用",
			reason, worst, d.NeedW, d.NeedH, d.BandW, d.BandH)
	case area > d.BandW*d.BandH:
		d.Reason = fmt.Sprintf("%s —— 各区单独都放得下,但总面积 %.0f 超过可用带 %.0f:"+
			"拆页,或先 `sch zone tidy` 收紧各区内部", reason, area, d.BandW*d.BandH)
	default:
		d.Reason = fmt.Sprintf("%s —— 各区单独放得下、总面积也够(%.0f/%.0f),是**排布**放不下"+
			"(行排 + 图签避让):调 --h-gap/--v-gap,或把最大的区拆到下一页",
			reason, area, d.BandW*d.BandH)
	}
	return d
}

func planSheetTidy(modules []partitionModule, sheet layoutBBox, keepout *layoutBBox, opts partitionOpts, hGap, vGap float64) zonePackPlan {
	// band 是**内容**可占区域;分区框在内容外画 pad(四向)+ 顶部标题带,可用
	// 区按框的最终占位收缩(不收缩 = 内容顶到纸边 → 框压边/标题带压 IC 头顶)。
	band := layoutBBox{
		MinX: sheet.MinX + opts.Margin + partitionContentPad,
		MinY: sheet.MinY + opts.Margin + partitionContentPad,
		MaxX: sheet.MaxX - opts.Margin - partitionContentPad,
		MaxY: sheet.MaxY - opts.Margin - partitionContentPad - opts.TitleBand,
	}
	// 图签 keepout(+safety)作为**障碍物**参与行排(L 形可用区):只挡右下角,
	// 图签左侧的底部空间照常可用(整条底带让位曾差 20 单位把一块能装的板判死)。
	var obs []layoutBBox
	if safe := inflatedTitleKeepout(keepout); safe != nil {
		obs = append(obs, *safe)
	}
	groups := make([]zonePackGroup, 0, len(modules))
	for _, m := range modules {
		groups = append(groups, zonePackGroup{ID: m.Name, BBox: m.BBox})
	}
	// 幂等 no-op:现状各区已在带内、不压图签、两两留足框间距 → 不动(tidy 不是
	// 重排癖;zone tidy 收紧后区已各就各位时,shelf 从头重排只会白搬)。
	settled := true
	for i := range groups {
		if !bboxContains(band, groups[i].BBox) {
			settled = false
			break
		}
		for _, o := range obs {
			if boxesOverlap(groups[i].BBox, o) {
				settled = false
				break
			}
		}
		if !settled {
			break
		}
	}
	for i := 0; settled && i < len(groups); i++ {
		for j := i + 1; j < len(groups); j++ {
			a, b := groups[i].BBox, groups[j].BBox
			if b.MinX-a.MaxX >= hGap-zonePackEps || a.MinX-b.MaxX >= hGap-zonePackEps ||
				b.MinY-a.MaxY >= vGap-zonePackEps || a.MinY-b.MaxY >= vGap-zonePackEps {
				continue
			}
			settled = false
			break
		}
	}
	if settled {
		moves := make([]zonePackMove, 0, len(groups))
		for _, g := range groups {
			moves = append(moves, zonePackMove{ID: g.ID})
		}
		return zonePackPlan{Fits: true, Moves: moves}
	}
	// Sheet 层用通用 shelf(全部区从带顶第一行起横排,行满换行,图签作障碍右跳
	// 避让)——zonePack 的「锚+下方行排」是区内语义(组竖排偏好),对三个并列功
	// 能区会全部竖堆爆高。面积降序、ID 破平(确定性);行内左→右。
	sort.Slice(groups, func(i, j int) bool { return zonePackBeats(groups[i], groups[j]) })
	moves, ok := packRowsInto(groups, band, obs, hGap, vGap)
	if !ok {
		return zonePackPlan{Fits: false,
			Diag: sheetTidyDiag("zones do not fit the sheet band (title block avoided as an obstacle)", groups, band, obs)}
	}
	if err := zonePackValidate(groups, moves, band); err != nil {
		return zonePackPlan{Fits: false, Diag: sheetTidyDiag(err.Error(), groups, band, obs)}
	}
	// packRowsInto 只保证不叠区;图签避让终验(validate 不知道障碍)。
	for i, g := range groups {
		eff := zonePackOffset(g.BBox, moves[i].DX, moves[i].DY)
		for _, o := range obs {
			if boxesOverlap(eff, o) {
				return zonePackPlan{Fits: false,
					Diag: sheetTidyDiag(fmt.Sprintf("zone %s cannot avoid the title block", g.ID), groups, band, obs)}
			}
		}
	}
	return zonePackPlan{Fits: true, Moves: moves}
}

func newSchSheetTidyCommand(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var apply, dryRun bool
	var hGap, vGap float64
	c := &cobra.Command{
		Use:   "tidy",
		Short: "Sheet 层布局:全部功能区当刚体依据纸张排布(锚区+行排;默认 dry-run,--apply 逐区 zone move)",
		Long: `三层布局体系的最外层(docs/schematic-layout-hierarchy.md):把每个功能区
(zones claim)——区 bbox 含器件、旗、登记说明(与分区框同口径)——当刚体,
在纸张可用区内排布(最大区为锚,行排,区间距 hGap/vGap;底部让出图签带)。

--apply 时对每个非零位移的区执行 zone move(组/器件/桩/旗/登记 note 全部随行,
区间 settle),全部完成后统一重画分区框。装不下时给最小纸面诊断,不硬塞。`,
		Args: cobra.NoArgs,
		Example: `  easyeda sch sheet tidy                 # dry-run 看各区位移
  easyeda sch sheet tidy --apply         # 执行 + 统一重画框`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apply && dryRun {
				return fmt.Errorf("--dry-run and --apply are mutually exclusive")
			}
			// ADR-0004 Decision 4(dry-run 纯计算铁律)在此**有意不接**
			// setDispatchDryRun:computePartitionPlan 的标签入框归属走
			// fetchSchWirePolylines(debug.exec_js 读通道,catalog 上 Mutates=true),
			// 接了会让 dry-run 恒降级(labelScopeDegraded)而 --apply 不降 ——
			// 规划分叉比不接更糟。等导线读升格为 typed action 后再接。
			pinned, win, docUUID, err := pinZonePage(cfg, *window)
			if err != nil {
				return err
			}
			opts := defaultPartitionOpts()
			plan, zones, err := computePartitionPlan(pinned, win, docUUID, opts)
			if err != nil {
				return err
			}
			_ = zones
			// modules 的 draw bbox 已在 computePartitionPlan 内部生成——但它不外露
			// modules;为避免改共享签名,这里按分区反拼:每个分区 rect 就是该区框,
			// 但 sheet-pack 需要「区」而非「合并分区」。直接重取 modules 口径:
			modsRes, err := requestAutolayoutAction(pinned, "schematic.components.list", win,
				map[string]any{"includeBBox": true}, docUUID, "sheet-tidy geometry")
			if err != nil {
				return err
			}
			comps, perr := parseLayoutComps(modsRes.Result)
			if perr != nil {
				return perr
			}
			sheetBB := sheetBBoxOf(comps)
			if sheetBB == nil {
				return fmt.Errorf("no sheet bbox on the active page")
			}
			keepout, _ := titleBlockKeepout(sheetBB)
			zonesMap, _, err := loadSchZoneModules(pinned, win, docUUID)
			if err != nil {
				return err
			}
			modules := modulesFromClaims(zonesMap, comps, nil)
			foldZoneNotesIntoModules(pinned, win, docUUID, zonesMap, modules)
			if len(modules) == 0 {
				return fmt.Errorf("no zone modules resolved(块驱动的页用 `sch block-apply` 自动归组;手工页 `sch group create` 或 `sch zones set` 认领)")
			}
			pk := planSheetTidy(modules, *sheetBB, keepout, opts, hGap, vGap)
			for _, mv := range pk.Moves {
				tag := ""
				if mv.Anchor {
					tag = "  [anchor]"
				}
				fmt.Fprintf(stdout, "  %-8s Δ(%g,%g)%s\n", mv.ID, mv.DX, mv.DY, tag)
			}
			if !pk.Fits {
				// Diag.Reason 已经带上了「哪种装不下 + 该怎么办」(sheetTidyDiag),
				// 这里不再拼一句放之四海的「缩紧各区或换更大图纸」——两种失败的修法
				// 相反,给一句通用建议等于让人两条都试一遍。
				return fmt.Errorf("sheet tidy: %s(最大区 %.0f×%.0f,可用带 %.0f×%.0f)",
					pk.Diag.Reason, pk.Diag.NeedW, pk.Diag.NeedH, pk.Diag.BandW, pk.Diag.BandH)
			}
			fmt.Fprintln(stdout, "✓ sheet plan fits — 区两两不叠且落纸面带内")
			if !apply {
				fmt.Fprintln(stdout, "dry-run — pass --apply to execute")
				return nil
			}
			moved := 0
			for _, mv := range pk.Moves {
				if mv.DX == 0 && mv.DY == 0 {
					continue
				}
				// 逐区刚移(note/组/桩全随行);框不逐区重画,最后统一画。
				// force:计划已验证终态两两不叠,移动次序造成的暂态压区无害放行。
				if err := runSchZoneMove(pinned, win, mv.ID, mv.DX, mv.DY, -1, true, false, false, stdout, stderr); err != nil {
					return fmt.Errorf("sheet tidy 在区 %s 处停止:%w(已移 %d 区;逆向 zone move 可回退)", mv.ID, err, moved)
				}
				moved++
				time.Sleep(350 * time.Millisecond)
			}
			// 统一重画分区框(六项 validation 把关)。
			plan2, _, err := computePartitionPlan(pinned, win, docUUID, opts)
			if err != nil {
				return fmt.Errorf("重画前取几何:%w", err)
			}
			if !plan2.Validation.clean() {
				fmt.Fprintf(stderr, "⚠ 框未重画:validation %+v — 手动 `sch zone-plan` 排查后 `sch zone-draw`\n", plan2.Validation)
			} else if err := runPartitionDraw(pinned, win, opts, 22, "#AA00AA", false, stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "⚠ 框重画失败:%v — 手动 `sch zone-draw --mode partition`\n", err)
			}
			fmt.Fprintf(stdout, "✓ sheet tidy applied:%d 区移动\n", moved)
			_ = plan
			return nil
		},
	}
	c.Flags().Float64Var(&hGap, "h-gap", sheetTidyHGapDefault, "区间最小水平间距")
	c.Flags().Float64Var(&vGap, "v-gap", sheetTidyVGapDefault, "区间最小垂直间距")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只规划不执行(默认)")
	c.Flags().BoolVar(&apply, "apply", false, "执行:逐区 zone move + 统一重画框")
	return c
}
