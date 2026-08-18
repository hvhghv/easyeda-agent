package app

// cmd_sch_zone_relayout.go — `sch zone relayout`:区级 placement-first 重排。
//
// 与 zone tidy(增量整理:挪带线的组)的根本差别是**顺序**(用户点名:「先确认
// 核心器件的方向和位置,自动计算出来;先对齐理顺电容电阻的方向和间隔,然后再连」):
//
//	1. 锚 IC 定位定向(V1:保持现位现向 —— 用户已确认的核心朝向);
//	2. 外围器件按角色纯计算终局:**全员竖放同一行平行对齐**(电上地下,
//	   netport 顺方向引出);pin 直连链(chain)共线横放串接、零折弯;
//	   间隔全部按实测几何计算(netport 网名实长、锚伸出),无拍脑袋常量;
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
)

// 其余间距不再有常量:首列起点按锚 netport 实际伸出、件间距按各件 netport 引出
// 实长(桩 + 网名占位宽)计算——「不要机械保证,严格算法实现」(用户拍板);
// 拍脑袋常量在 LED_CTRL(文字溢出长条 bbox)上实测穿帮过。

// relayoutPortWidth:netport 的占位实宽——长条符号 bbox 恒 31,但文字**不撑开
// 长条**(实测 LED_CTRL 8 字渲染溢出 bbox 右缘 ~25,压进邻件)。按网名实长算:
// ASCII ~6/字符 + 8 呼吸,短名保底 31。
func relayoutPortWidth(net string) float64 {
	w := 6*float64(len(net)) + 8
	if w < 31 {
		w = 31
	}
	return w
}

// relayoutRailItem 是竖放行的一个成员:PortReach 是该件 netport 朝左引出的实长
// (桩 30 + 占位实宽),决定与左邻的间距;0 = 纯双旗件(基础 60 紧凑)。
type relayoutRailItem struct {
	Desig     string
	HasPort   bool
	PortReach float64
}

// planZoneRelayoutPositions 是区级排位纯函数(用户拍板「全部外围平行对齐」):
// **所有**外围件竖放、同一行、同顶等距——电源端朝上、地端朝下、netport 端水平
// 引出;器件锚 y 全同(本体/顶旗/底旗三条线全对齐)。返回 designator → 器件锚
// 坐标;放不下(超出 band 右缘)返回错误,不硬塞。
// anchorPortReach:锚 IC 右侧水平 netport 的最大伸出(桩 30 + 占位实宽)——
// 首列起点必须让过它,否则锚的 netport 文字(如 LED_CTRL,溢出长条 bbox)压进
// 第一个外围件(实测相交)。
func planZoneRelayoutPositions(anchor tidyAnchor, anchorBBox *layoutBBox, anchorPortReach float64,
	rail []relayoutRailItem, band layoutBBox) (map[string][2]float64, error) {
	out := map[string][2]float64{}
	right := anchor.X + anchor.HalfWidth
	if anchorBBox != nil {
		right = anchorBBox.MaxX
	}
	// 首列 x = 锚右缘 + max(60, 锚 netport 实际伸出 + 40 呼吸)。
	gap0 := anchorPortReach + 40
	if gap0 < 60 {
		gap0 = 60
	}
	startX := right + gap0
	topY := anchor.Y // 无 bbox 时退锚 y
	if anchorBBox != nil {
		topY = anchorBBox.MaxY
	}
	// 器件锚 y = 行顶 - 50(统一总高 100 的中点,顶旗与锚顶平)。
	railY := snap5(topY - 50)
	x := startX
	for i, it := range rail {
		if i > 0 {
			// 与左邻间距:严格按本件 netport 引出实长 + 40 呼吸(朝左伸的文字
			// 决定所需间隙);纯双旗件基础 60。
			pitch := relayoutRailPitch
			if it.HasPort {
				if p := it.PortReach + 40; p > pitch {
					pitch = p
				}
			}
			x += pitch
		}
		if x > band.MaxX {
			return nil, fmt.Errorf("竖放行超出区带右缘(%s @ x=%.0f > %.0f)— 区带太窄", it.Desig, x, band.MaxX)
		}
		out[it.Desig] = [2]float64{snap5(x), railY}
	}
	return out, nil
}

// relayoutVerticalizeSignal 把一件 signal 类(单电源/地旗 + netport)转成竖放
// 计划(与去耦件平行对齐,用户拍板):电源旗端朝上 / 地旗端朝下(定 Top/Bottom
// 消解判据),netport 从另一端水平朝左引出(netport 永不竖放)。两端都是
// netport(无电源轴)转不了,返回 false 留在横放路径。
func relayoutVerticalizeSignal(m tidyLiveMember) (tidyMemberPlan, bool) {
	var railPin, portPin *tidyLivePin
	railClass := ""
	for i := range m.Pins {
		p := &m.Pins[i]
		switch p.Conn.Flag {
		case "netflag":
			if c := tidyNetClass(p.Conn.Net); c == "power" || c == "ground" {
				if railPin != nil {
					return tidyMemberPlan{}, false // 双旗件不该在 signal 类
				}
				railPin, railClass = p, c
			}
		case "netport":
			if portPin != nil {
				return tidyMemberPlan{}, false // 双 netport:无电源轴,保持横放
			}
			portPin = p
		}
	}
	if railPin == nil || portPin == nil {
		return tidyMemberPlan{}, false
	}
	// 电源旗端朝上(top)/ 地旗端朝下(bottom);netport 占另一端,**顺方向**
	// 竖直引出(用户拍板「顺着方向摆布即可」,全员等距不受水平文字牵制)。
	topPin, bottomPin := railPin.Conn.Pin, portPin.Conn.Pin
	railDir, portDir := "up", "down"
	if railClass == "ground" {
		topPin, bottomPin = portPin.Conn.Pin, railPin.Conn.Pin
		railDir, portDir = "down", "up"
	}
	railRot, err := tidyLabelRotation(railClass, railDir)
	if err != nil {
		return tidyMemberPlan{}, false
	}
	portRot, err := tidyLabelRotation("netport", portDir)
	if err != nil {
		return tidyMemberPlan{}, false
	}
	railKind := "power"
	if railClass == "ground" {
		railKind = "ground"
	}
	return tidyMemberPlan{
		Designator:         m.Comp.Designator,
		RotationCandidates: [2]float64{90, 270},
		PowerPin:           topPin, // 消解判据语义:期望在上的 pin
		GndPin:             bottomPin,
		Pins: []tidyPinTarget{
			{Pin: railPin.Conn.Pin, Direction: railDir, Kind: railKind, Net: railPin.Conn.Net, LabelRotation: railRot},
			{Pin: portPin.Conn.Pin, Direction: portDir, Kind: "net_port_bi", Net: portPin.Conn.Net, LabelRotation: portRot},
		},
	}, true
}

func runSchZoneRelayout(cfg *appConfig, window, zoneName string, apply bool, stdout, stderr io.Writer) error {
	// ADR-0004 Decision 4(dry-run 纯计算铁律)在此**有意不接** setDispatchDryRun:
	// 规划段 tidyReadScene 硬依赖 fetchSchWirePolylinesStable(debug.exec_js 读通道,
	// catalog 上 Mutates=true),接了会把 dry-run 规划直接打死。等导线读升格为
	// typed action 后再接。
	pinned, win, docUUID, err := pinZonePage(cfg, window)
	if err != nil {
		return err
	}
	// 统一注册表解析(ADR-0004 Decision 3):模块认领 / 块组 / 子组同一张表,
	// 解析失败的报错自带本页全量可用名 + 来源。
	zoneObj, _, _, err := resolveLayoutZone(pinned, win, docUUID, zoneName)
	if err != nil {
		return err
	}
	claim := zoneObj.zoneClaim()
	zoneName = zoneObj.zoneName() // 分区计划/band 的取名口径(子组=末段)
	if len(claim.Parts) == 0 {
		return fmt.Errorf("布局对象 %s 无成员件(块页看虚拟组 `sch group list`,手工页 `sch zones set`)", zoneObj.describe())
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

	// 区级排位:锚不动,**全部外围竖放一行平行对齐**(用户拍板)。signal 件转
	// 竖放(电源/地端竖直、netport 水平朝左);转不了的(双 netport)留横放。
	var anchorBBox *layoutBBox
	if plan.AnchorDesig != "" {
		if m, ok := members[strings.ToUpper(plan.AnchorDesig)]; ok {
			anchorBBox = m.Comp.BBox
		}
	}
	// 串联链识别(pin-to-pin 直连拓扑):链成员从 signal/skip 中拿走,共线串接。
	chains, chained := relayoutDetectChains(members, wires)
	if len(chained) > 0 {
		var ks []string
		for _, d := range plan.Skipped {
			if !chained[strings.ToUpper(d)] {
				ks = append(ks, d)
			}
		}
		plan.Skipped = ks
	}
	hasPort := map[string]bool{}
	var keptSignal []tidySignalPlan
	for _, sp := range plan.Signal {
		if chained[strings.ToUpper(sp.Designator)] {
			continue
		}
		d := strings.ToUpper(sp.Designator)
		m, ok := members[d]
		if !ok {
			keptSignal = append(keptSignal, sp)
			continue
		}
		if vp, ok := relayoutVerticalizeSignal(m); ok {
			plan.Power = append(plan.Power, vp)
			hasPort[d] = true
		} else {
			keptSignal = append(keptSignal, sp)
		}
	}
	plan.Signal = keptSignal
	var rail []relayoutRailItem
	for _, mp := range plan.Power {
		d := strings.ToUpper(mp.Designator)
		// netport 顺方向竖直引出后不再占水平空间(竖排文字带宽 ~12)——全员
		// 回归 60 等距(水平引出时代的 per-件 reach 计算已不需要)。
		rail = append(rail, relayoutRailItem{Desig: d, HasPort: hasPort[d]})
	}
	sort.SliceStable(rail, func(i, j int) bool { return tidyDesignatorLess(rail[i].Desig, rail[j].Desig) })

	// 锚右侧水平 netport 的最大伸出(首列必须让过它的文字)。
	anchorPortReach := 0.0
	if plan.AnchorDesig != "" {
		if am, ok := members[strings.ToUpper(plan.AnchorDesig)]; ok {
			ax := am.Comp.X
			for _, p := range am.Pins {
				if p.HasMarker && p.Marker.ComponentType == "netport" && p.Marker.X > ax {
					if r := schStubLen + relayoutPortWidth(p.Conn.Net); r > anchorPortReach {
						anchorPortReach = r
					}
				}
			}
		}
	}

	fmt.Fprintf(stderr, "band=(%.0f,%.0f)..(%.0f,%.0f)\n", band.MinX, band.MinY, band.MaxX, band.MaxY)
	pos, err := planZoneRelayoutPositions(plan.Anchor, anchorBBox, anchorPortReach, rail, band)
	if err != nil {
		return err
	}
	for i := range plan.Power {
		if p, ok := pos[strings.ToUpper(plan.Power[i].Designator)]; ok {
			plan.Power[i].X, plan.Power[i].Y = p[0], p[1]
		}
	}

	fmt.Fprintf(stdout, "zone relayout [%s] placement-first — 锚 %s 不动,%d 件竖放平行对齐(%d 带水平 netport):\n",
		zoneName, plan.AnchorDesig, len(rail), len(hasPort))
	for _, it := range rail {
		tag := "上电下地"
		if it.HasPort {
			tag = "电源轴竖直 + netport 顺方向竖直"
		}
		fmt.Fprintf(stdout, "  %-6s 竖放 → (%g,%g) %s\n", it.Desig, pos[it.Desig][0], pos[it.Desig][1], tag)
	}
	for _, sp := range plan.Signal {
		fmt.Fprintf(stdout, "  %-6s 横放保留(双 netport 无电源轴)\n", sp.Designator)
	}
	for _, ch := range chains {
		var names []string
		for _, cm := range ch.Members {
			names = append(names, cm.Desig)
		}
		fmt.Fprintf(stdout, "  串联链:%s ─ %s ─ %s(共线串接,零折弯)\n", ch.LeftNet, strings.Join(names, " ─ "), ch.RightNet)
	}
	for _, d := range plan.Skipped {
		fmt.Fprintf(stdout, "  %-6s skip(未建模连接,原位不动)\n", d)
	}
	if !apply {
		fmt.Fprintln(stdout, "dry-run(默认):未改画布 —— 加 --apply 落地(sweep → 落位 → 一遍性重连)")
		return nil
	}
	if err := tidyApply(pinned, win, docUUID, plan, members, stdout, stderr); err != nil {
		return err
	}
	// 串联链落地:sweep 链树(成员=链件)→ 共线横放 → 端标 + 直线。
	if len(chains) > 0 {
		chainSet := map[string]bool{}
		for d := range chained {
			chainSet[d] = true
		}
		freshWires, werr := fetchSchWirePolylinesStable(pinned, win, docUUID)
		if werr != nil {
			return fmt.Errorf("chain sweep wire read:%w", werr)
		}
		ids, serr := tidyDeepSweepPlan(chainSet, comps, freshWires)
		if serr != nil {
			return serr
		}
		if len(ids) > 0 {
			if _, err := requestAutolayoutAction(pinned, "schematic.primitives.delete", win,
				map[string]any{"primitiveIds": ids}, docUUID, "chain sweep"); err != nil {
				return fmt.Errorf("chain sweep delete:%w", err)
			}
			fmt.Fprintf(stdout, "  链深度清扫:%d 个旧桩/旗/线\n", len(ids))
		}
		// 链行放竖放行下一行(rail 空则顶行)。
		right := plan.Anchor.X + plan.Anchor.HalfWidth
		topY := plan.Anchor.Y
		if anchorBBox != nil {
			right, topY = anchorBBox.MaxX, anchorBBox.MaxY
		}
		if plan.AnchorDesig == "" { // 无锚区(如 LED):从区带左上排
			right, topY = band.MinX, band.MaxY
		}
		chainY := snap5(topY - 50)
		if len(rail) > 0 {
			chainY = snap5(topY - 100 - 130)
		}
		startX := right + 80
		for _, ch := range chains {
			// 链总宽预检:超区带即报错(v1 不自动分段;分段=断点两侧同名 netport 对)。
			w := 0.0
			for _, cm := range ch.Members {
				w += cm.PinSpan
			}
			w += 20*float64(len(ch.Members)-1) + 80
			if startX+w > band.MaxX {
				return fmt.Errorf("串联链宽 %.0f 超区带(%.0f)— 请手动分段:断点两侧同名 netport 对(2+2),再各自成链", w, band.MaxX-startX)
			}
			if err := relayoutApplyChain(pinned, win, docUUID, ch, members, startX, chainY, stdout); err != nil {
				return err
			}
			chainY = snap5(chainY - 130)
		}
		if err := tidySelfCheck(pinned, win, docUUID); err != nil {
			return fmt.Errorf("链落地后自检红:%w(按 sch check findings 修复)", err)
		}
		if err := saveAutolayoutDocument(pinned, win, docUUID, "save chain relayout"); err != nil {
			fmt.Fprintf(stderr, "⚠ save: %v\n", err)
		}
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
	fmt.Fprintf(stdout, "✓ zone relayout applied:%d 件 placement-first 重排,自检绿\n", len(rail))
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
			map[string]any{"pinX": px, "pinY": py, "kind": kind, "net": r.Net, "direction": dir, "rotation": rot, "offset": schStubLen},
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
	c.Flags().StringVar(&zone, "zone", "", "布局对象名(模块认领/块组/子组统一命名空间,`sch zones status` 看全表)")
	c.Flags().BoolVar(&apply, "apply", false, "执行(默认 dry-run)")
	return c
}

// ── 串联链(chain)──────────────────────────────────────────────────────────
//
// 用户拍板(2026-08-12):「可以做成一串完成而不用折弯」——器件 pin 经普通导线
// pin-to-pin 直连成链(如 GND—LED—R—netport)时,布局为**全链共线横放串接**:
// 相邻 pin 20 短直线、零折弯;地端在左(旗竖直向下)、netport 端在右(水平顺
// 链向)。链宽超区带才断链(两侧同名 netport 对,2+2 分行)——能一串就一串,
// 不主动加标签;v1 超宽先报错提示手动分段。

type relayoutChainMember struct {
	Desig    string
	LeftPin  string // 链序左端 pin(rot 消解判据:实测必须在左)
	RightPin string
	PinSpan  float64
}

type relayoutChain struct {
	Members   []relayoutChainMember // 左→右
	LeftKind  string                // connect_pin kind(ground/power/net_port_bi)
	LeftNet   string
	LeftDir   string // down(gnd)/up(power)/left(netport)
	RightKind string
	RightNet  string
	RightDir  string
}

// relayoutDetectChains 从「有 OnWire 无标 pin」的区成员里重建串联链:两件的
// OnWire pin 共 wire 树 = 链边;分量线性(每件 ≤2 边、无分叉)才成链;链两端
// pin 的旗/netport 定端标。返回链与消耗的位号集合。
func relayoutDetectChains(members map[string]tidyLiveMember, wires []schGroupWire) ([]relayoutChain, map[string]bool) {
	roots := tidyWireRoots(wires)
	pinTree := func(x, y float64) int {
		for i, w := range wires {
			if pointOnPolyline(x, y, w.Points, schGroupEps) {
				return roots[i]
			}
		}
		return -1
	}
	// 每件的 OnWire pin → 树根;树根 → 挂着的件列表。
	type chainEnd struct {
		desig string
		pin   string
	}
	treeEnds := map[int][]chainEnd{}
	for d, m := range members {
		for _, p := range m.Pins {
			if p.Conn.Flag == "" && p.Conn.OnWire {
				if r := pinTree(p.X, p.Y); r >= 0 {
					treeEnds[r] = append(treeEnds[r], chainEnd{d, p.Conn.Pin})
				}
			}
		}
	}
	// 邻接:每树恰 2 端 = 一条链边(>2 = 分叉树,不支持)。
	adj := map[string][]chainEnd{}
	for _, ends := range treeEnds {
		if len(ends) != 2 {
			continue
		}
		a, b := ends[0], ends[1]
		if a.desig == b.desig {
			continue
		}
		adj[a.desig] = append(adj[a.desig], b)
		adj[b.desig] = append(adj[b.desig], a)
	}
	consumed := map[string]bool{}
	var chains []relayoutChain
	for d := range adj {
		if consumed[d] || len(adj[d]) != 1 {
			continue // 链从度 1 的端件开始走
		}
		// 走链。
		var order []string
		cur, prev := d, ""
		for cur != "" {
			order = append(order, cur)
			next := ""
			for _, e := range adj[cur] {
				if e.desig != prev {
					next = e.desig
					break
				}
			}
			prev, cur = cur, next
			if len(order) > 32 {
				break // 环兜底
			}
		}
		if len(order) < 2 {
			continue
		}
		// 端标:链首件的非链 pin 上的旗;链尾件同理。链内 pin 配对从 adj 边读。
		ch := relayoutChain{}
		ok := true
		for i, desig := range order {
			m := members[desig]
			// 本件的链邻 pin(与前/后件共树的 pin)。
			linkPins := map[string]bool{}
			for _, p := range m.Pins {
				if p.Conn.Flag == "" && p.Conn.OnWire {
					linkPins[p.Conn.Pin] = true
				}
			}
			var endPin string // 非链 pin(带标)
			var endConn tidyPinConn
			for _, p := range m.Pins {
				if !linkPins[p.Conn.Pin] {
					endPin, endConn = p.Conn.Pin, p.Conn
				}
			}
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
			cm := relayoutChainMember{Desig: desig, PinSpan: pinSpan}
			switch i {
			case 0: // 链首:端 pin 在左
				cm.LeftPin = endPin
				for p := range linkPins {
					cm.RightPin = p
				}
				switch tidyNetClass(endConn.Net) {
				case "ground":
					ch.LeftKind, ch.LeftDir = "ground", "down"
				case "power":
					ch.LeftKind, ch.LeftDir = "power", "up"
				default:
					if endConn.Flag == "netport" {
						ch.LeftKind, ch.LeftDir = "net_port_bi", "left"
					} else {
						ok = false
					}
				}
				ch.LeftNet = endConn.Net
			case len(order) - 1: // 链尾:端 pin 在右
				cm.RightPin = endPin
				for p := range linkPins {
					cm.LeftPin = p
				}
				switch tidyNetClass(endConn.Net) {
				case "ground":
					ch.RightKind, ch.RightDir = "ground", "down"
				case "power":
					ch.RightKind, ch.RightDir = "power", "up"
				default:
					if endConn.Flag == "netport" {
						ch.RightKind, ch.RightDir = "net_port_bi", "right"
					} else {
						ok = false
					}
				}
				ch.RightNet = endConn.Net
			default: // 链中件:两 pin 都是链 pin,左右按走向由 exec 实测消解
				var ps []string
				for p := range linkPins {
					ps = append(ps, p)
				}
				sort.Strings(ps)
				if len(ps) >= 2 {
					cm.LeftPin, cm.RightPin = ps[0], ps[1]
				} else {
					ok = false
				}
			}
			ch.Members = append(ch.Members, cm)
		}
		if !ok || ch.LeftKind == "" || ch.RightKind == "" {
			continue
		}
		// 惯例:地端在左;若右端是地而左端不是 → 反转链。
		if ch.RightKind == "ground" && ch.LeftKind != "ground" {
			for i, j := 0, len(ch.Members)-1; i < j; i, j = i+1, j-1 {
				ch.Members[i], ch.Members[j] = ch.Members[j], ch.Members[i]
			}
			for i := range ch.Members {
				ch.Members[i].LeftPin, ch.Members[i].RightPin = ch.Members[i].RightPin, ch.Members[i].LeftPin
			}
			ch.LeftKind, ch.RightKind = ch.RightKind, ch.LeftKind
			ch.LeftNet, ch.RightNet = ch.RightNet, ch.LeftNet
			ch.LeftDir, ch.RightDir = ch.RightDir, ch.LeftDir
			if ch.LeftDir == "right" {
				ch.LeftDir = "left"
			}
			if ch.RightDir == "left" {
				ch.RightDir = "right"
			}
		}
		for _, cm := range ch.Members {
			consumed[cm.Desig] = true
		}
		chains = append(chains, ch)
	}
	return chains, consumed
}

// relayoutApplyChain 执行一条链:sweep(树整删)已由调用方完成;逐件横放落位
// (rot 消解:LeftPin 实测在左)→ 端标 connect_pin → 相邻 pin 直线 wire。
func relayoutApplyChain(cfg *appConfig, win, docUUID string, ch relayoutChain, members map[string]tidyLiveMember,
	startX, rowY float64, stdout io.Writer) error {
	const pinGap = 20.0
	cursor := startX
	type placed struct{ leftX, rightX, y float64 }
	pos := make([]placed, len(ch.Members))
	for i, cm := range ch.Members {
		m, okm := members[strings.ToUpper(cm.Desig)]
		if !okm {
			return fmt.Errorf("chain member %s 不在 live 集合", cm.Desig)
		}
		anchorX := snap5(cursor + cm.PinSpan/2)
		// 横放候选 rot 0/180;实测 LeftPin 在左消解(极性件如 LED 靠这一步保向)。
		usedRot := 0.0
		if err := tidyModifyPose(cfg, win, docUUID, m.Comp.ID, anchorX, rowY, usedRot); err != nil {
			return fmt.Errorf("modify %s:%w", cm.Desig, err)
		}
		pins, err := tidySettledPins(cfg, win, docUUID, cm.Desig)
		if err != nil {
			return err
		}
		lx, ly, ok1 := tidyPinCoord(pins, cm.LeftPin)
		rx, _, ok2 := tidyPinCoord(pins, cm.RightPin)
		if !ok1 || !ok2 {
			return fmt.Errorf("%s 实测 pins 缺 %s/%s", cm.Desig, cm.LeftPin, cm.RightPin)
		}
		if lx > rx { // 左 pin 不在左 → 翻 180
			usedRot = 180
			if err := tidyModifyPose(cfg, win, docUUID, m.Comp.ID, anchorX, rowY, usedRot); err != nil {
				return fmt.Errorf("modify %s rot180:%w", cm.Desig, err)
			}
			if pins, err = tidySettledPins(cfg, win, docUUID, cm.Desig); err != nil {
				return err
			}
			lx, ly, _ = tidyPinCoord(pins, cm.LeftPin)
			rx, _, _ = tidyPinCoord(pins, cm.RightPin)
			if lx > rx {
				return fmt.Errorf("%s rot 0/180 两候选 %s 都不在左 — 符号基向异常", cm.Desig, cm.LeftPin)
			}
		}
		pos[i] = placed{leftX: lx, rightX: rx, y: ly}
		cursor = rx + pinGap
		fmt.Fprintf(stdout, "  ✓ %s 链位 (%g,%g) rot %g\n", cm.Desig, anchorX, rowY, usedRot)
	}
	// 端标。
	first, last := pos[0], pos[len(pos)-1]
	lRot, err := tidyLabelRotation(ch.LeftKind, ch.LeftDir)
	if err != nil {
		return err
	}
	if _, err := requestAutolayoutAction(cfg, "schematic.power.connect_pin", win,
		map[string]any{"pinX": first.leftX, "pinY": first.y, "kind": ch.LeftKind, "net": ch.LeftNet,
			"direction": ch.LeftDir, "rotation": lRot, "offset": schStubLen}, docUUID, "chain left end"); err != nil {
		return fmt.Errorf("chain 左端 %s:%w", ch.LeftNet, err)
	}
	rRot, err := tidyLabelRotation(ch.RightKind, ch.RightDir)
	if err != nil {
		return err
	}
	if _, err := requestAutolayoutAction(cfg, "schematic.power.connect_pin", win,
		map[string]any{"pinX": last.rightX, "pinY": last.y, "kind": ch.RightKind, "net": ch.RightNet,
			"direction": ch.RightDir, "rotation": rRot, "offset": schStubLen}, docUUID, "chain right end"); err != nil {
		return fmt.Errorf("chain 右端 %s:%w", ch.RightNet, err)
	}
	// 链内 pin-to-pin 直线(共线零折弯)。
	for i := 0; i+1 < len(pos); i++ {
		pts := []any{[]any{pos[i].rightX, pos[i].y}, []any{pos[i+1].leftX, pos[i+1].y}}
		if _, err := requestAutolayoutAction(cfg, "schematic.wire.create", win,
			map[string]any{"points": pts}, docUUID, "chain link wire"); err != nil {
			return fmt.Errorf("chain 直线 %s→%s:%w", ch.Members[i].Desig, ch.Members[i+1].Desig, err)
		}
	}
	fmt.Fprintf(stdout, "  ✓ 串联链 %d 件共线串接(%s ← 链 → %s)\n", len(ch.Members), ch.LeftNet, ch.RightNet)
	return nil
}
