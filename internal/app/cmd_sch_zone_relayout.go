package app

// cmd_sch_zone_relayout.go — `sch zone relayout`:区级 placement-first 重排。
//
// 与 zone tidy(增量整理:挪带线的组)的根本差别是**顺序**(用户点名:「先确认
// 核心器件的方向和位置,自动计算出来;先对齐理顺电容电阻的方向和间隔,然后再连」):
//
//	1. 锚 IC 定位定向(V1:保持现位现向 —— 用户已确认的核心朝向);
//	2. 外围器件按角色纯计算终局:去耦(双电源旗)竖放、锚右同顶行、等距;
//	   信号链(带 netport)横放、其下行排、行内基线共线;间隔/网格全部机械;
//	3. deep sweep 一次删净全部旧桩/旗(整树)→ modify 逐件落位 → connect_pin
//	   一遍性重连(方向/rotation 走真值表;链端电源旗竖直)。
//
// 全程**不搬带线的东西** —— 组带线刚移在暂态叠位时会被平台 merge 共点线,
// 再移就撕出跨区短路(zone tidy 实测两次)。placement-first 没有这一整类问题:
// 线不搬,只重生成。
//
// 执行复用 group tidy 的整条落地管线(tidyApply:sweep → power/signal exec →
// 自检 → 记录回滚),本文件只做「区级排位」这一层新计算。

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

const (
	// relayoutRailPitch:竖放去耦行的节距(件锚到件锚)。竖放件左右无文字,
	// 40 是符号+呼吸;60 留位号文字。
	relayoutRailPitch = 60.0
	// relayoutRowGap:横放信号链行的行距(基线到基线)。链的上/下竖旗伸出
	// ~63(桩 30 + 旗体 21 + 文字 12);两行相向旗(下行 GND 下伸 vs 上行 3V3 上伸)需 ≥126,130 留缝。
	relayoutRowGap = 130.0
	// relayoutChainGap:横放链行内间距——链左端 netport 文字自带视觉间隔,60 够;
	// 117(相向标签安全距)会把两条能同行的链挤成一列。
	relayoutChainGap = 60.0
	// relayoutColGap:锚 IC 右缘到第一列内容的间距。
	relayoutColGap = 100.0
)

// planZoneRelayoutPositions 是区级排位纯函数:锚不动,power 件(竖放)在锚右
// 同顶一行等距,signal 件(横放)在其下行排、行内基线共线(同 y)。返回
// designator → 器件锚坐标;放不下(超出 band 右缘/底缘)返回错误,不硬塞。
func planZoneRelayoutPositions(anchor tidyAnchor, anchorBBox *layoutBBox, powerDesigs, signalDesigs []string,
	signalWidth map[string]float64, band layoutBBox) (map[string][2]float64, error) {
	out := map[string][2]float64{}
	startX := anchor.X + anchor.HalfWidth + relayoutColGap
	topY := anchor.Y // 无 bbox 时退锚 y
	if anchorBBox != nil {
		topY = anchorBBox.MaxY
	}
	// 竖放行:器件锚 y = 行顶 - 50(统一总高 100 的中点,旗顶与锚顶平)。
	railY := snap5(topY - 50)
	x := startX
	for _, d := range powerDesigs {
		if x > band.MaxX {
			return nil, fmt.Errorf("竖放行超出区带右缘(%s @ x=%.0f > %.0f)— 区带太窄", d, x, band.MaxX)
		}
		out[d] = [2]float64{snap5(x), railY}
		x += relayoutRailPitch
	}
	// 横放行:竖放行底(topY-100)以下,行距 relayoutRowGap,行内基线共线。
	rowY := snap5(topY - 100 - relayoutRowGap/2)
	x = startX
	for _, d := range signalDesigs {
		w := signalWidth[d]
		if w <= 0 {
			w = 160
		}
		if x+w > band.MaxX { // 行满换行
			rowY = snap5(rowY - relayoutRowGap)
			x = startX
		}
		if rowY < band.MinY {
			return nil, fmt.Errorf("横放行超出区带底缘(%s @ y=%.0f < %.0f)— 区带太矮", d, rowY, band.MinY)
		}
		// 器件锚 x = 链左缘 + 左标签预留;链宽 w 已含标签,锚放中段。
		out[d] = [2]float64{snap5(x + w*0.6), rowY}
		x += w + relayoutChainGap
	}
	return out, nil
}

// relayoutSignalWidth 估一条信号链的占位宽:左 netport(文字 6/字符 + 长条 31)
// + 桩 18 + pin 距 + 尾旗(竖直,占宽 ~20)。
func relayoutSignalWidth(m tidyLiveMember) float64 {
	maxNet := 3
	for _, p := range m.Pins {
		if p.Conn.Flag == "netport" && len(p.Conn.Net) > maxNet {
			maxNet = len(p.Conn.Net)
		}
	}
	// pinSpan 用 pin 实测跨度(bbox 含旧标签会高估 60-80,链宽虚胖 → 明明能
	// 同行的两条链被挤成一列,实测踩坑)。
	pinSpan := 40.0
	if len(m.Pins) >= 2 {
		minX, maxX := m.Pins[0].X, m.Pins[0].X
		for _, p := range m.Pins[1:] {
			if p.X < minX {
				minX = p.X
			}
			if p.X > maxX {
				maxX = p.X
			}
		}
		if s := maxX - minX; s > 0 {
			pinSpan = s
		}
	}
	return float64(6*maxNet) + 31 + 18 + pinSpan + 20
}

func runSchZoneRelayout(cfg *appConfig, window, zoneName string, apply bool, stdout, stderr io.Writer) error {
	pinned, win, docUUID, err := pinZonePage(cfg, window)
	if err != nil {
		return err
	}
	zones, _, err := loadSchZoneClaimsForPage(pinned, win, docUUID)
	if err != nil {
		return err
	}
	claim := zones[zoneName]
	if claim == nil || len(claim.Parts) == 0 {
		return fmt.Errorf("zone %q 无认领件(先 `sch zones set`)", zoneName)
	}

	comps, extras, wires, err := tidyReadScene(pinned, win, docUUID)
	if err != nil {
		return err
	}
	// 区认领件当一个临时大组走 group tidy 的件语义管线。
	fake := &schGroup{ID: "zone:" + zoneName, Name: zoneName, Members: append([]string(nil), claim.Parts...)}
	members, order, err := buildTidyMembers(fake, comps, extras, wires)
	if err != nil {
		return err
	}
	plan, err := buildTidyPlan(members, order, "auto", tidyDefaultSpacing, true)
	if err != nil {
		return err
	}

	// 区带(与 zone tidy 同源)+ **无条件生长**:分区 rect 是「当前内容」的
	// 函数(内容窄 → band 窄),而 relayout 要的是「可用空间」——不生长就是
	// 鸡生蛋(上一轮排成一列 → band 缩到 430 宽 → 这一轮还是只能排一列)。
	// 生长避开其他分区/图签/纸边(zoneTidyGrowBand 同一套夹逼)。
	band := layoutBBox{}
	var partPlan *partitionPlan
	if pplan, _, perr := computePartitionPlan(pinned, win, docUUID, defaultPartitionOpts()); perr == nil {
		partPlan = &pplan
		if b, ok := zoneTidyBandFromPlan(pplan, zoneName); ok {
			band = b
		}
	}
	if band.MaxX <= band.MinX {
		var boxes []layoutBBox
		for _, d := range claim.Parts {
			if m, ok := members[strings.ToUpper(d)]; ok && m.Comp.BBox != nil {
				boxes = append(boxes, *m.Comp.BBox)
			}
		}
		b, ok := zoneTidyContentBand(boxes, 120)
		if !ok {
			return fmt.Errorf("cannot derive a band for zone %q", zoneName)
		}
		band = b
	}
	if partPlan != nil {
		band = zoneTidyGrowBand(band, *partPlan, zoneName, defaultPartitionOpts())
	}

	// 区级排位:锚不动,power 竖放行 + signal 横放行。
	var anchorBBox *layoutBBox
	if plan.AnchorDesig != "" {
		if m, ok := members[strings.ToUpper(plan.AnchorDesig)]; ok {
			anchorBBox = m.Comp.BBox
		}
	}
	var powerDesigs, signalDesigs []string
	for _, mp := range plan.Power {
		powerDesigs = append(powerDesigs, strings.ToUpper(mp.Designator))
	}
	sort.SliceStable(powerDesigs, func(i, j int) bool { return tidyDesignatorLess(powerDesigs[i], powerDesigs[j]) })
	signalWidth := map[string]float64{}
	for _, sp := range plan.Signal {
		d := strings.ToUpper(sp.Designator)
		signalDesigs = append(signalDesigs, d)
		if m, ok := members[d]; ok {
			signalWidth[d] = relayoutSignalWidth(m)
		}
	}
	sort.SliceStable(signalDesigs, func(i, j int) bool { return tidyDesignatorLess(signalDesigs[i], signalDesigs[j]) })

	fmt.Fprintf(stderr, "band=(%.0f,%.0f)..(%.0f,%.0f)\n", band.MinX, band.MinY, band.MaxX, band.MaxY)
	pos, err := planZoneRelayoutPositions(plan.Anchor, anchorBBox, powerDesigs, signalDesigs, signalWidth, band)
	if err != nil {
		return err
	}
	// 注入区级坐标:power 件改 X/Y;signal 件加 pose。
	for i := range plan.Power {
		if p, ok := pos[strings.ToUpper(plan.Power[i].Designator)]; ok {
			plan.Power[i].X, plan.Power[i].Y = p[0], p[1]
		}
	}
	for i := range plan.Signal {
		if p, ok := pos[strings.ToUpper(plan.Signal[i].Designator)]; ok {
			plan.Signal[i].HasPose, plan.Signal[i].X, plan.Signal[i].Y = true, p[0], p[1]
		}
	}

	fmt.Fprintf(stdout, "zone relayout [%s] placement-first — 锚 %s 不动,%d 竖放 + %d 横放:\n",
		zoneName, plan.AnchorDesig, len(powerDesigs), len(signalDesigs))
	for _, d := range powerDesigs {
		fmt.Fprintf(stdout, "  %-6s 竖放 → (%g,%g) 上电下地,总高 100\n", d, pos[d][0], pos[d][1])
	}
	for _, d := range signalDesigs {
		fmt.Fprintf(stdout, "  %-6s 横放 → (%g,%g) 左入右出,链端电源旗竖直\n", d, pos[d][0], pos[d][1])
	}
	for _, d := range plan.Skipped {
		fmt.Fprintf(stdout, "  %-6s skip(未建模连接,原位不动)\n", d)
	}
	if !apply {
		fmt.Fprintln(stdout, "dry-run(默认):未改画布 —— 加 --apply 落地(sweep → 落位 → 一遍性重连)")
		return nil
	}
	if err := tidyApply(pinned, win, docUUID, plan, members, comps, stdout, stderr); err != nil {
		return err
	}
	// 锚 IC 的横躺电源/地旗也竖直化(与外围统一「电上地下」;用户点名:外围都
	// 遵守约定,中心器件不能例外)。仅当竖直桩不穿本件其他 pin(列顶的电源脚 /
	// 列底的地脚才安全);不安全的保持横躺并报告。
	if plan.AnchorDesig != "" {
		if am, ok := members[strings.ToUpper(plan.AnchorDesig)]; ok {
			if err := relayoutAnchorRails(pinned, win, docUUID, am, stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "⚠ 锚电源旗竖直化未完成:%v(横躺旗保留,手动 `sch disconnect`+`sch connect --direction up|down`)\n", err)
			}
		}
	}
	fmt.Fprintf(stdout, "✓ zone relayout applied:%d 件 placement-first 重排,自检绿\n", len(powerDesigs)+len(signalDesigs))
	return nil
}

// relayoutAnchorRails 把锚 IC 的横躺 power/gnd 旗重连为竖直(power up / gnd
// down)。安全判:竖直桩路径(pin ±3 x 带、伸出 50)上不得有本件其他 pin。
func relayoutAnchorRails(cfg *appConfig, win, docUUID string, anchor tidyLiveMember, stdout, stderr io.Writer) error {
	rails := tidyHorizontalRailPins(anchor)
	if len(rails) == 0 {
		return nil
	}
	for _, r := range rails {
		var px, py float64
		found := false
		for _, p := range anchor.Pins {
			if p.Conn.Pin == r.Pin {
				px, py, found = p.X, p.Y, true
				break
			}
		}
		if !found {
			continue
		}
		dir := "up"
		if r.Class == "ground" {
			dir = "down"
		}
		safe := true
		for _, p := range anchor.Pins {
			if p.Conn.Pin == r.Pin || mathAbs(p.X-px) > 3 {
				continue
			}
			if (dir == "up" && p.Y > py && p.Y <= py+50) || (dir == "down" && p.Y < py && p.Y >= py-50) {
				safe = false
				break
			}
		}
		if !safe {
			fmt.Fprintf(stderr, "  ⚠ %s:%s(%s)竖直桩会穿本件相邻 pin — 保持横躺\n", anchor.Comp.Designator, r.Pin, r.Net)
			continue
		}
		rot, err := tidyLabelRotation(r.Class, dir)
		if err != nil {
			return err
		}
		if _, err := requestAutolayoutAction(cfg, "schematic.pin.disconnect", win,
			map[string]any{"pinX": px, "pinY": py}, docUUID, "relayout anchor rail disconnect"); err != nil {
			return fmt.Errorf("disconnect %s:%s:%w", anchor.Comp.Designator, r.Pin, err)
		}
		kind := "power"
		if r.Class == "ground" {
			kind = "ground"
		}
		if _, err := requestAutolayoutAction(cfg, "schematic.power.connect_pin", win,
			map[string]any{"pinX": px, "pinY": py, "kind": kind, "net": r.Net, "direction": dir, "rotation": rot, "offset": 30.0},
			docUUID, "relayout anchor rail connect"); err != nil {
			return fmt.Errorf("connect %s:%s → %s %s:%w", anchor.Comp.Designator, r.Pin, dir, r.Net, err)
		}
		fmt.Fprintf(stdout, "  ✓ 锚 %s:%s %s 旗竖直化(%s)\n", anchor.Comp.Designator, r.Pin, r.Net, dir)
	}
	return nil
}

func mathAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func newSchZoneRelayoutCommand(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var zone string
	var apply bool
	c := &cobra.Command{
		Use:   "relayout",
		Short: "区级 placement-first 重排:锚定核心器件 → 外围纯计算落位 → 一遍性重连(不搬带线的东西)",
		Long: `与 zone tidy(挪带线的组)的根本差别是顺序:先确认核心器件的方向和位置
(V1 锚 IC 不动),再纯计算外围电容电阻的方向/位置/间隔(去耦竖放同顶等距、
信号链横放基线共线),最后 deep sweep 删净旧桩旗、逐件落位、一遍性重连
(方向/rotation 走 orientation 真值表,链端电源旗竖直)。

全程不搬带线的图元——组刚移在暂态叠位时会被平台 merge 共点线再撕出短路
(实测),placement-first 没有这一类问题。默认 dry-run。`,
		Args: cobra.NoArgs,
		Example: `  easyeda sch zone relayout --zone MCU           # dry-run 看每件目标位
  easyeda sch zone relayout --zone MCU --apply   # sweep → 落位 → 重连 → 自检`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(zone) == "" {
				return fmt.Errorf("--zone 必填(sch zones status 看认领)")
			}
			return runSchZoneRelayout(cfg, *window, zone, apply, stdout, stderr)
		},
	}
	c.Flags().StringVar(&zone, "zone", "", "功能区名(zones claim)")
	c.Flags().BoolVar(&apply, "apply", false, "执行(默认 dry-run)")
	return c
}
