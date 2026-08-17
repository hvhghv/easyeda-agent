package app

// cmd_sch_zone_arrange_apply.go — `sch zone-arrange --apply`:两段规划的落地执行。
//
// J_USB 事故(2026-08-16,signal-column 接线把 R3/R4 搞断)留下的两条断言在
// 这里成为**执行前后的硬门**,缺一不放行:
//
//	断言① 删除集 = 重建集。
//	  - 名单一次构造:sweep 的 memberSet 与逐件执行名单来自同一份计划,
//	    执行前做**集合相等**校验(zaaGateSetEquality —— 事故的直接形式:
//	    sweep 按 3 件删、重建只轮到 1 件,就是这两个集合不等);
//	  - pin 级覆盖:每件「计划端子网名多重集 == 现存已连接 pin 网名多重集」
//	    (zaaGatePinCoverage)。普通导线直连/netlabel 连接盖不住 → 拒绝,
//	    画布零改动 —— fail-closed。
//	断言② sweep 前有连接的 pin,重建后必须仍有连接且网名一致。
//	  全部落位后重读场景,逐 pin 用导线归属实测(tidyPinAttachment)核对
//	  (zaaVerifyConnectivity)。上次事故里 layout-lint + bridge-check 双绿
//	  却断了两件 —— 孤立器件既不重叠也不短路,唯有这条判据看得见。
//
// 执行走 ADR-0003 舞步,**页级一次 sweep**(旧位置上各区标签互相穿插,分区
// sweep 必然把邻区 pin 判成「共享树」而拒绝;全员入集后这不再是共享):
//
//	快照(与规划同一份场景)→ 断言① → 全页深度清扫 → 逐件[落位(转竖件双候选
//	实测消解)→ settle → connect_pin 重连] → 断言② → layout-lint + bridge-check
//	→ save;任一步红 → 逐步回滚(位姿复原 + 按快照原 kind/net/方向重建)。

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// zaaPinSnap 是一只 sweep 前已连接 pin 的快照:断言②的基准 + 回滚的原料。
type zaaPinSnap struct {
	Desig, Pin string
	Net        string
	Kind       string // connect_pin 口径(ground/power/net_port_bi);"" = netlabel 等不可重建
	Dir        string // 原桩方向(回滚重建用)
	Wired      bool   // 树上无标记的普通导线直连 —— apply 盖不住,预检拒绝
}

// zaaTermExec 是一条已映射到具体 pin 的重连指令。
type zaaTermExec struct {
	Pin, Kind, Net, Dir string
	LabelRot            float64
	Offset              float64 // 计划桩长(多旗垂直梯次靠它错开;0 = connect_pin 默认)
	ExpectUpper         bool    // 转竖件消解用:该端子在计划里位于本体上方
}

// zaaMemberExec 是一件的执行指令。
type zaaMemberExec struct {
	Desig, PrimID         string
	OrigX, OrigY, OrigRot float64
	// 非转竖件:平移 Δ(snap5,件是格点公民)。
	DX, DY float64
	// 转竖件(R1):候选 OrigRot±90,实测 pin 上下序消解;落位按目标本体中心对
	// pin 中点(旋转后 bbox 未知,pin 驱动)。
	Rotate                 bool
	TargetCX, TargetCY     float64
	Terms                  []zaaTermExec
	Snaps                  []zaaPinSnap // 本件的 pin 快照(回滚重建原料)
}

// zaaConnectKind 把规划端子折成 connect_pin 口径。
func zaaConnectKind(t zfPlacedTerm) string {
	if t.Kind == "netport" {
		return "net_port_bi"
	}
	if tidyNetClass(t.Net) == "ground" {
		return "ground"
	}
	return "power"
}

// zaaGateSetEquality 是断言①的名单形式:sweep 集与重建集必须相等。
// J_USB 事故的直接守卫 —— 当时 sweep 按整组删(3 件),重建只轮到 1 件。
func zaaGateSetEquality(sweepSet map[string]bool, rebuild []string) error {
	seen := map[string]bool{}
	for _, d := range rebuild {
		seen[strings.ToUpper(d)] = true
	}
	var missing, extra []string
	for d := range sweepSet {
		if !seen[d] {
			missing = append(missing, d)
		}
	}
	for d := range seen {
		if !sweepSet[d] {
			extra = append(extra, d)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("断言①红:删除集 ≠ 重建集(sweep 有而重建没有:%v;重建有而 sweep 没有:%v)—— 拒绝执行,画布零改动",
			missing, extra)
	}
	return nil
}

// zaaGatePinCoverage 是断言①的 pin 级形式:计划端子网名多重集必须等于该件
// 现存已连接 pin 的网名多重集 —— 少一条就是「删了不重建」的静默断线。
func zaaGatePinCoverage(desig string, pre []zaaPinSnap, terms []zfPlacedTerm) error {
	var preNets, planNets []string
	for _, p := range pre {
		if p.Wired {
			return fmt.Errorf("断言①红:%s pin%s 经普通导线直连(树上无标记)—— apply 的重建指令盖不住它,拒绝(先手工梳理或 `sch group-move`)", desig, p.Pin)
		}
		if p.Kind == "" {
			return fmt.Errorf("断言①红:%s pin%s 的连接类型无法经 connect_pin 重建(netlabel 类)—— 拒绝", desig, p.Pin)
		}
		preNets = append(preNets, p.Net)
	}
	for _, t := range terms {
		planNets = append(planNets, t.Net)
	}
	sort.Strings(preNets)
	sort.Strings(planNets)
	if strings.Join(preNets, "\x00") != strings.Join(planNets, "\x00") {
		return fmt.Errorf("断言①红:%s 计划端子 %v ≠ 已连接 pin 网 %v —— 拒绝执行,画布零改动",
			desig, planNets, preNets)
	}
	return nil
}

// zaaPadTermsToPins 把计划端子按实际已连接 pin 的网名多重集「同网扩容」:
// 某网的实际 pin 数 > 计划端子数时,克隆该网第一个端子补齐(J2 真机:USB-C 的
// GND 焊盘组 6 只 pin 全部接地,块计划只有 5 只 —— 同网冗余接地是合法甚至更好
// 的画布状态,不该被断言①按「集合不等」拒掉;sweep 删几只就重建几只)。
// 只扩容不收缩:实际比计划**少**仍是「删了不重建」的红线,交给 gate 原样拒。
func zaaPadTermsToPins(terms []zfPlacedTerm, pre []zaaPinSnap, pageNets map[string]bool) []zfPlacedTerm {
	planCount := map[string]int{}
	firstOf := map[string]zfPlacedTerm{}
	for _, t := range terms {
		planCount[t.Net]++
		if _, ok := firstOf[t.Net]; !ok {
			firstOf[t.Net] = t
		}
	}
	preCount := map[string]int{}
	for _, p := range pre {
		preCount[p.Net]++
	}
	out := append([]zfPlacedTerm(nil), terms...)
	nets := make([]string, 0, len(preCount))
	for net := range preCount {
		nets = append(nets, net)
	}
	sort.Strings(nets) // 确定性:补齐顺序与 map 遍历序无关
	for _, net := range nets {
		tpl, hasTpl := firstOf[net]
		for i := planCount[net]; i < preCount[net]; i++ {
			if hasTpl {
				out = append(out, tpl) // 同网冗余(J2 六只 GND 焊盘):克隆计划端子
				continue
			}
			// 本组计划没有这个网,但它在**页内其他组**的计划里(pageNets)——
			// 共树 pin(Q1-E 与 R3 的 USB_DTR 合法共树,cluster 的「专属 marker」
			// 规则不把树算给 Q1,端子就缺了;而页级 sweep 会把整棵树删掉)。
			// 按实测侧合成端子一并重建,否则它是注定修不回来的静默断线。
			if !pageNets[net] {
				continue // 页内无人认领的网 = 真意外连接,留给 gate 拒
			}
			kind := "netport"
			if cls := tidyNetClass(net); cls == "ground" || cls == "power" {
				kind = "netflag"
			}
			dir := "right"
			for _, p := range pre {
				if p.Net == net && p.Dir != "" {
					dir = p.Dir
					break
				}
			}
			out = append(out, zfPlacedTerm{Kind: kind, Net: net, Dir: dir, Offset: zfStub})
		}
	}
	return out
}

// zaaMapTerms 把计划端子映射到具体 pin:优先 (net, 现侧) 匹配,退 net 匹配;
// 全确定性(pin 号自然序 × 计划序),J1 的双 U3_N4(左右各一)靠现侧区分。
func zaaMapTerms(pre []zaaPinSnap, terms []zfPlacedTerm, pinSide map[string]string,
	termUpper func(zfPlacedTerm) bool) ([]zaaTermExec, error) {
	used := map[int]bool{}
	sorted := append([]zaaPinSnap(nil), pre...)
	sort.SliceStable(sorted, func(i, j int) bool { return tidyDesignatorLess(sorted[i].Pin, sorted[j].Pin) })
	var out []zaaTermExec
	for _, t := range terms {
		pick := -1
		for pass := 0; pass < 2 && pick < 0; pass++ {
			for i, p := range sorted {
				if used[i] || p.Net != t.Net {
					continue
				}
				if pass == 0 && pinSide[p.Pin] != t.Dir {
					continue // 第一轮:net+现侧同时匹配
				}
				pick = i
				break
			}
		}
		if pick < 0 {
			return nil, fmt.Errorf("端子 %s(%s) 找不到对应 pin(断言①已过却映射失败 = 内部不一致)", t.Net, t.Dir)
		}
		used[pick] = true
		rot, err := tidyLabelRotation(zaaConnectKind(t), t.Dir)
		if err != nil {
			return nil, err
		}
		out = append(out, zaaTermExec{Pin: sorted[pick].Pin, Kind: zaaConnectKind(t), Net: t.Net,
			Dir: t.Dir, LabelRot: rot, Offset: t.Offset, ExpectUpper: termUpper(t)})
	}
	return out, nil
}

// zaaBuildExec 由计划 + 场景快照构造全部执行指令(纯函数,断言①在此落判)。
func zaaBuildExec(out *zoneArrangeOut, scene *zaScene, opts partitionOpts) ([]zaaMemberExec, map[string]bool, error) {
	partOf := map[string]layoutComp{}
	for _, c := range scene.comps {
		if c.ComponentType == "part" || c.ComponentType == "" || c.ComponentType == schLayoutPartType {
			partOf[strings.ToUpper(label(c))] = c
		}
	}
	roots := tidyWireRoots(scene.wires)
	var markers []layoutComp
	for _, c := range scene.comps {
		if isSchMarker(c.ComponentType) {
			markers = append(markers, c)
		}
	}
	rectOf := map[string]layoutBBox{}
	for _, p := range out.Arrange.Placed {
		rectOf[p.Name] = p.Rect
	}
	sweepSet := map[string]bool{}
	// 页级计划网集合:共树 pin 的端子合成要判「这个网页内有没有人认领」。
	pageNets := map[string]bool{}
	for _, z := range out.Zones {
		for _, g := range z.Groups {
			for _, t := range g.Terms {
				pageNets[t.Net] = true
			}
		}
	}
	var rebuild []string
	var execs []zaaMemberExec
	for _, z := range out.Zones {
		rect, ok := rectOf[z.Name]
		if !ok {
			return nil, nil, fmt.Errorf("区 %s 有收敛计划但无落位框(内部不一致)", z.Name)
		}
		offX := rect.MinX + partitionContentPad - z.Content.MinX
		offY := rect.MinY + opts.NoteBand + partitionContentPad - z.Content.MinY
		for _, g := range z.Groups {
			d := strings.ToUpper(g.Designator)
			live, ok := partOf[d]
			if !ok || live.BBox == nil {
				return nil, nil, fmt.Errorf("计划成员 %s 不在场景快照里(内部不一致)", g.Designator)
			}
			sweepSet[d] = true
			rebuild = append(rebuild, d)
			// pin 快照:现存连接(断言②基准 + 覆盖门 + 回滚原料)。
			bcx, bcy := bboxCenter(*live.BBox)
			var snaps []zaaPinSnap
			pinSide := map[string]string{}
			for _, p := range live.Pins {
				m, hasM, onWire := tidyPinAttachment(p.X, p.Y, scene.wires, roots, markers)
				if !onWire {
					continue // 本就悬空的 pin 不进快照(断言②只保「曾连接」的)
				}
				s := zaaPinSnap{Desig: g.Designator, Pin: p.Number}
				if !hasM {
					s.Wired = true
				} else {
					s.Net = m.Net
					s.Kind = tidyRestoreKind(m.ComponentType, m.Net)
					s.Dir, _ = tidyStubDirection(p.X, p.Y, m.X, m.Y)
				}
				snaps = append(snaps, s)
				// 现侧(左右口径,映射优先键):按 pin 相对本体中心的主轴。
				if math.Abs(p.X-bcx) >= math.Abs(p.Y-bcy) {
					if p.X < bcx {
						pinSide[p.Number] = "left"
					} else {
						pinSide[p.Number] = "right"
					}
				} else if p.Y < bcy {
					pinSide[p.Number] = "down"
				} else {
					pinSide[p.Number] = "up"
				}
			}
			gTerms := zaaPadTermsToPins(g.Terms, snaps, pageNets)
			if err := zaaGatePinCoverage(g.Designator, snaps, gTerms); err != nil {
				return nil, nil, err
			}
			gcy := (g.Body.MinY + g.Body.MaxY) / 2
			terms, err := zaaMapTerms(snaps, gTerms, pinSide, func(t zfPlacedTerm) bool {
				return (t.BBox.MinY+t.BBox.MaxY)/2 > gcy
			})
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", g.Designator, err)
			}
			rot := 0.0
			if live.Rotation != nil {
				rot = *live.Rotation
			}
			me := zaaMemberExec{
				Desig: g.Designator, PrimID: live.ID,
				OrigX: live.X, OrigY: live.Y, OrigRot: rot,
				Rotate: g.Rotated, Terms: terms, Snaps: snaps,
			}
			if g.Rotated {
				me.TargetCX = (g.Body.MinX+g.Body.MaxX)/2 + offX
				me.TargetCY = gcy + offY
			} else {
				me.DX = snap5(g.Body.MinX + offX - live.BBox.MinX)
				me.DY = snap5(g.Body.MinY + offY - live.BBox.MinY)
			}
			execs = append(execs, me)
		}
	}
	if err := zaaGateSetEquality(sweepSet, rebuild); err != nil {
		return nil, nil, err
	}
	return execs, sweepSet, nil
}

// zaaRetry:平台会「随机吃掉一个连接/短暂不响应」(block-apply 真机备忘),
// 单次失败先歇口气重试一次,再失败才算数。
func zaaRetry(op func() error) error {
	if err := op(); err == nil {
		return nil
	}
	time.Sleep(2 * time.Second)
	return op()
}

// zaaExecMember 落位 + 重连一件(舞步与 tidyExec* 同源:modify → settle 实测 →
// connect_pin;转竖件双候选实测消解,与 tidyExecPowerMember 的 rot 二义同法)。
//
// 返回 (落位成败, 未接上的端子清单):**端子失败不打断本件也不打断整页** ——
// 首跑实录(2026-08-16):连接器中途卡死一次,第 10 件的 connect 失败触发整页
// 回滚,而回滚也是写操作,对着卡死的连接器全数无效 —— 10 件好的没保住,坏的
// 也没修好;断言②的清单 + 逐脚补连两分钟就修完了。缺连接走对账修复,不回滚。
func zaaExecMember(cfg *appConfig, win, docUUID string, m zaaMemberExec, stdout, stderr io.Writer) (bool, []zaaTermExec) {
	var pins []layoutPin
	var err error
	if !m.Rotate {
		if err = zaaRetry(func() error {
			return tidyModifyPose(cfg, win, docUUID, m.PrimID, m.OrigX+m.DX, m.OrigY+m.DY, m.OrigRot)
		}); err != nil {
			fmt.Fprintf(stderr, "  ✗ %s 落位失败(重试后):%v —— 本件跳过,连接留给对账修复\n", m.Desig, err)
			return false, m.Terms
		}
		if pins, err = tidySettledPins(cfg, win, docUUID, m.Desig); err != nil {
			fmt.Fprintf(stderr, "  ✗ %s settle 失败:%v\n", m.Desig, err)
			return false, m.Terms
		}
	} else {
		cands := []float64{math.Mod(m.OrigRot+90, 360), math.Mod(m.OrigRot+270, 360)}
		ok := false
		for _, cand := range cands {
			// 先原地转(旋转后 bbox 未知,pin 中点驱动落位),再平移对中。
			if err = zaaRetry(func() error {
				return tidyModifyPose(cfg, win, docUUID, m.PrimID, m.OrigX, m.OrigY, cand)
			}); err != nil {
				fmt.Fprintf(stderr, "  ✗ %s rot %g 失败:%v\n", m.Desig, cand, err)
				return false, m.Terms
			}
			if pins, err = tidySettledPins(cfg, win, docUUID, m.Desig); err != nil {
				fmt.Fprintf(stderr, "  ✗ %s settle 失败:%v\n", m.Desig, err)
				return false, m.Terms
			}
			mx, my := zaaPinMidpoint(pins)
			dx, dy := snap5(m.TargetCX-mx), snap5(m.TargetCY-my)
			if err = zaaRetry(func() error {
				return tidyModifyPose(cfg, win, docUUID, m.PrimID, m.OrigX+dx, m.OrigY+dy, cand)
			}); err != nil {
				fmt.Fprintf(stderr, "  ✗ %s 平移失败:%v\n", m.Desig, err)
				return false, m.Terms
			}
			if pins, err = tidySettledPins(cfg, win, docUUID, m.Desig); err != nil {
				fmt.Fprintf(stderr, "  ✗ %s settle 失败:%v\n", m.Desig, err)
				return false, m.Terms
			}
			if zaaVerticalOrderOK(pins, m.Terms) {
				ok = true
				break
			}
		}
		if !ok {
			fmt.Fprintf(stderr, "  ✗ %s 转竖两候选实测上下序都不符 —— 符号基向异常,本件跳过\n", m.Desig)
			return false, m.Terms
		}
	}
	var missed []zaaTermExec
	for _, t := range m.Terms {
		px, py, found := tidyPinCoord(pins, t.Pin)
		if !found {
			fmt.Fprintf(stderr, "  ⚠ %s 实测 pins 里没有 pin %s —— 留给对账修复\n", m.Desig, t.Pin)
			missed = append(missed, t)
			continue
		}
		payload := map[string]any{"pinX": px, "pinY": py, "kind": t.Kind, "net": t.Net,
			"direction": t.Dir, "rotation": t.LabelRot}
		if t.Offset > 0 {
			payload["offset"] = t.Offset // 计划梯次桩长 —— 不带它,同向多旗全落默认桩长必竖叠
		}
		if err := zaaRetry(func() error {
			_, e := requestAutolayoutAction(cfg, "schematic.power.connect_pin", win, payload, docUUID, "zone-arrange connect")
			return e
		}); err != nil {
			fmt.Fprintf(stderr, "  ⚠ connect %s:%s → %s %s(%s) 失败(重试后):%v —— 留给对账修复\n",
				m.Desig, t.Pin, t.Dir, t.Kind, t.Net, err)
			missed = append(missed, t)
		}
	}
	fmt.Fprintf(stdout, "  ✓ %s 落位%s + 重连 %d/%d 端子\n", m.Desig,
		map[bool]string{true: "(转竖)", false: ""}[m.Rotate], len(m.Terms)-len(missed), len(m.Terms))
	return true, missed
}

func zaaPinMidpoint(pins []layoutPin) (float64, float64) {
	if len(pins) == 0 {
		return 0, 0
	}
	var sx, sy float64
	for _, p := range pins {
		sx += p.X
		sy += p.Y
	}
	return sx / float64(len(pins)), sy / float64(len(pins))
}

// zaaVerticalOrderOK:计划里在上方的端子,其映射 pin 实测必须在更高处。
func zaaVerticalOrderOK(pins []layoutPin, terms []zaaTermExec) bool {
	var upY, downY []float64
	for _, t := range terms {
		y := math.Inf(1)
		if py, ok := func() (float64, bool) { _, v, ok := tidyPinCoord(pins, t.Pin); return v, ok }(); ok {
			y = py
		}
		if t.ExpectUpper {
			upY = append(upY, y)
		} else {
			downY = append(downY, y)
		}
	}
	for _, u := range upY {
		for _, d := range downY {
			if u <= d {
				return false
			}
		}
	}
	return true
}

// zaaVerifyConnectivity 是断言②:重读场景,sweep 前有连接的 pin 现在必须仍
// 有连接且网名一致。layout-lint/bridge-check 对孤立断线结构性失明,这条才是
// J_USB 事故真正缺的判据。
func zaaVerifyConnectivity(cfg *appConfig, win, docUUID string, snaps []zaaPinSnap) error {
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", win,
		map[string]any{"includeBBox": true, "includePins": true}, docUUID, "verify connectivity")
	if err != nil {
		return fmt.Errorf("断言②无法运行(没有证明不算过):%w", err)
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return fmt.Errorf("断言②解析场景失败:%w", perr)
	}
	wires, werr := fetchSchWirePolylinesStable(cfg, win, docUUID)
	if werr != nil {
		return fmt.Errorf("断言②读导线失败(没有证明不算过):%w", werr)
	}
	roots := tidyWireRoots(wires)
	var markers []layoutComp
	pinsOf := map[string][]layoutPin{}
	for _, c := range comps {
		if isSchMarker(c.ComponentType) {
			markers = append(markers, c)
		}
		if c.ComponentType == "part" || c.ComponentType == schLayoutPartType {
			pinsOf[strings.ToUpper(label(c))] = c.Pins
		}
	}
	var bad []string
	for _, s := range snaps {
		px, py, ok := tidyPinCoord(pinsOf[strings.ToUpper(s.Desig)], s.Pin)
		if !ok {
			bad = append(bad, fmt.Sprintf("%s:%s(重读丢 pin)", s.Desig, s.Pin))
			continue
		}
		m, hasM, onWire := tidyPinAttachment(px, py, wires, roots, markers)
		switch {
		case !onWire:
			bad = append(bad, fmt.Sprintf("%s:%s 断连(原 %s)", s.Desig, s.Pin, s.Net))
		case !hasM:
			bad = append(bad, fmt.Sprintf("%s:%s 在线上但无标记(原 %s)", s.Desig, s.Pin, s.Net))
		case m.Net != s.Net:
			bad = append(bad, fmt.Sprintf("%s:%s 网名漂移 %s→%s", s.Desig, s.Pin, s.Net, m.Net))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("断言②红:%d 处连接性回退 —— %s", len(bad), strings.Join(bad, ";"))
	}
	return nil
}

// runZoneArrangeApply 是 --apply 主编排。
func runZoneArrangeApply(cfg *appConfig, win, docUUID string, out *zoneArrangeOut, scene *zaScene,
	opts partitionOpts, stdout, stderr io.Writer) error {
	if out.Verdict != "pass" {
		return fmt.Errorf("规划 verdict=%s,拒绝落地(先解决 blocked)", out.Verdict)
	}
	execs, sweepSet, err := zaaBuildExec(out, scene, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "断言①绿:删除集 = 重建集(%d 件),pin 级覆盖逐件相等\n", len(execs))

	// 页级一次深度清扫(见文件头:分区 sweep 会把邻区 pin 判成共享树而拒绝)。
	ids, err := tidyDeepSweepPlan(sweepSet, scene.comps, scene.wires)
	if err != nil {
		return fmt.Errorf("deep-sweep 规划:%w", err)
	}
	ids = uniqueIDs(ids) // 平台对含重复 id 的删除批次整批静默拒(P2 实锤)
	ids = dropSheetIDs(ids, scene.comps) // 图框守卫:CLI prim-delete 有守卫,内部删单也不能裸奔
	if len(ids) > 0 {
		if _, err := requestAutolayoutAction(cfg, "schematic.primitives.delete", win,
			map[string]any{"primitiveIds": ids}, docUUID, "zone-arrange deep-sweep"); err != nil {
			return fmt.Errorf("deep-sweep delete %d primitive(s):%w", len(ids), err)
		}
		fmt.Fprintf(stdout, "深度清扫:删除 %d 个旧桩/旗/残段(整树,页级一次)\n", len(ids))
	}
	// 执行:端子失败不打断(见 zaaExecMember 头注);落位失败跳过本件,坐等对账。
	executed := 0
	termByPin := map[string]zaaTermExec{}
	for _, m := range execs {
		for _, t := range m.Terms {
			termByPin[strings.ToUpper(m.Desig)+":"+t.Pin] = t
		}
		if ok, _ := zaaExecMember(cfg, win, docUUID, m, stdout, stderr); ok {
			executed++
		}
	}
	fmt.Fprintf(stdout, "落位 %d/%d 件;进入断言②对账…\n", executed, len(execs))

	// 断言② + 对账修复:缺哪只 pin 就按计划端子补哪只(最多两轮),修不动才报。
	// 首跑实录:平台随机吃掉 2/4 条补连,对账循环正是治它的(block-apply 同款)。
	var verr error
	for round := 0; round < 3; round++ {
		verr = zaaVerifyConnectivity(cfg, win, docUUID, allSnaps(execs))
		if verr == nil {
			break
		}
		broken := zaaBrokenPins(verr)
		if round == 2 || len(broken) == 0 {
			break
		}
		fmt.Fprintf(stdout, "对账修复第 %d 轮:%d 处缺连接,按计划端子补连…\n", round+1, len(broken))
		for _, key := range broken {
			t, ok := termByPin[key]
			if !ok {
				continue
			}
			desig := strings.SplitN(key, ":", 2)[0]
			pins, perr := tidySettledPins(cfg, win, docUUID, desig)
			if perr != nil {
				fmt.Fprintf(stderr, "  ⚠ 修复 %s settle 失败:%v\n", key, perr)
				continue
			}
			px, py, found := tidyPinCoord(pins, t.Pin)
			if !found {
				continue
			}
			payload := map[string]any{"pinX": px, "pinY": py, "kind": t.Kind, "net": t.Net,
				"direction": t.Dir, "rotation": t.LabelRot}
			if t.Offset > 0 {
				payload["offset"] = t.Offset
			}
			if err := zaaRetry(func() error {
				_, e := requestAutolayoutAction(cfg, "schematic.power.connect_pin", win, payload, docUUID, "zone-arrange repair")
				return e
			}); err != nil {
				fmt.Fprintf(stderr, "  ⚠ 修复 %s 失败:%v\n", key, err)
			}
		}
	}
	if verr != nil {
		// 缺连接修不动:如实报清单(结构化下一步),**不回滚** —— 位姿是好的,
		// 回滚只会把 10 件好的也拆掉;剩余缺口一条 `sch connect` 一条命令可补。
		return fmt.Errorf("对账修复后仍红:%w —— 按清单逐脚 `sch connect --pin 位号:脚 --kind … --net … --direction …` 补齐", verr)
	}
	fmt.Fprintf(stdout, "断言②绿:%d 只曾连接 pin 全部仍连接且网名一致\n", len(allSnaps(execs)))

	// 假失败清创(自动化的例行步,此前每页人肉扫多轮):停摆期「报失败的写」
	// 大概率已落地,重试即同位重复/同树冗余标记 —— 判据现成(check 的
	// duplicate/redundant-net-marker 带 suggestDeleteIds),这里直接执行处方。
	// best-effort:清不掉只 warn,电气正确性由 bridge-check 把关。
	if n, derr := zaaSweepGhostMarkers(cfg, win, docUUID); derr != nil {
		fmt.Fprintf(stderr, "⚠ 假失败清创未完成(%v)—— 手补:`sch check` 按 suggestDeleteIds `sch prim-delete`\n", derr)
	} else if n > 0 {
		fmt.Fprintf(stdout, "假失败清创:清除 %d 个重复/冗余标记(停摆期已落地的\"失败\"写)\n", n)
	}

	// 分级收尾:bridge-check 红 = 真短路,主动有害 → 整体回滚;
	// layout-lint 红 = 标签实测伸展超出规划估算 → 如实报,重跑一轮收敛即修
	// (两遍法:落地实测反哺下一轮规划),不为几个单位的标签擦碰拆掉整页落位。
	if berr := zaaBridgeCheck(cfg, win, docUUID); berr != nil {
		fmt.Fprintf(stderr, "✗ %v\n", berr)
		fmt.Fprintf(stderr, "真短路 → 整体回滚 %d 件…\n", len(execs))
		var records []tidyStepRecord
		for _, m := range execs {
			records = append(records, zaaStepRecord(m))
		}
		tidyRollback(cfg, win, docUUID, records, stderr)
		return berr
	}
	lintWarn := ""
	if rep, lerr := collectLayoutLint(cfg, win, 2.54, 0, false, false, false); lerr != nil {
		lintWarn = fmt.Sprintf("layout-lint 无法运行:%v", lerr)
	} else if !rep.OK {
		lintWarn = fmt.Sprintf("layout-lint:%s(标签实测伸展 > 规划估算 —— 重跑一轮 `zone-arrange --apply` 用实测反哺收敛)", rep.Summary)
	}
	if _, err := requestAutolayoutAction(cfg, "schematic.save", win, nil, docUUID, "zone-arrange save"); err != nil {
		fmt.Fprintf(stderr, "⚠ 显式保存失败(%v)—— daemon 防抖自动保存仍会兜底\n", err)
	}
	if lintWarn != "" {
		fmt.Fprintf(stdout, "△ zone-arrange 落地 %d/%d 件;断言①② + bridge-check 绿,已保存;%s\n", executed, len(execs), lintWarn)
	} else {
		fmt.Fprintf(stdout, "✓ zone-arrange 落地 %d/%d 件;断言①② + layout-lint + bridge-check 全绿,已保存\n", executed, len(execs))
	}
	fmt.Fprintln(stdout, "note: 分区框未重画 —— `sch zone-draw --mode partition` 更新;区名/说明带随框走")
	return nil
}

// zaaSweepGhostMarkers 清掉停摆期假失败留下的鬼影标记:同位重复
// (duplicate-net-marker)与同树冗余(redundant-net-marker)。判据复用 `sch check`
// 的函数本体(同一把尺),删除清单 = 两条规则的 suggestDeleteIds 并集(去重 ——
// 平台对含重复 id 的批次整批静默拒)。返回实际提交删除的 id 数。
func zaaSweepGhostMarkers(cfg *appConfig, win, docUUID string) (int, error) {
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", win,
		map[string]any{"includeBBox": true}, docUUID, "ghost-marker sweep read")
	if err != nil {
		return 0, err
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return 0, err
	}
	wires, err := fetchSchWirePolylinesStable(cfg, win, docUUID)
	if err != nil {
		return 0, err
	}
	var ids []string
	for _, f := range duplicateNetMarkerFindings(comps) {
		ids = append(ids, f.SuggestDeleteIds...)
	}
	for _, f := range redundantNetMarkerFindings(comps, wires) {
		ids = append(ids, f.SuggestDeleteIds...)
	}
	ids = dropSheetIDs(uniqueIDs(ids), comps)
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := requestAutolayoutAction(cfg, "schematic.primitives.delete", win,
		map[string]any{"primitiveIds": ids}, docUUID, "ghost-marker sweep delete"); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// zaaBrokenPins 从断言②的报文提取「位号:脚」键(与 zaaVerifyConnectivity 的
// bad 条目格式配对 —— 同文件同口径,格式变更两处一起改)。
func zaaBrokenPins(verr error) []string {
	var out []string
	for _, part := range strings.Split(verr.Error(), ";") {
		f := strings.Fields(strings.TrimSpace(part))
		for _, w := range f {
			if i := strings.Index(w, ":"); i > 0 && !strings.Contains(w, "断言") {
				key := strings.ToUpper(strings.SplitN(w, "(", 2)[0])
				if strings.Count(key, ":") == 1 {
					out = append(out, key)
				}
				break
			}
		}
	}
	return out
}

// zaaBridgeCheck 只跑真短路判据(与 tidySelfCheck 的 bridge 段同源)。
func zaaBridgeCheck(cfg *appConfig, win, docUUID string) error {
	res, err := requestAutolayoutAction(cfg, "schematic.bridgeCheck", win, nil, docUUID, "zone-arrange bridge-check")
	if err != nil {
		return fmt.Errorf("bridge-check 无法运行(没有证明不算过):%w", err)
	}
	brep, err := parseBridgeReport(res.Result)
	if err != nil {
		return fmt.Errorf("bridge-check 结果不可解析:%w", err)
	}
	if brep.Summary.Bridges > 0 {
		var nets []string
		for _, t := range brep.Trees {
			if strings.EqualFold(t.Kind, "BRIDGE") {
				nets = append(nets, "["+strings.Join(t.Nets, ",")+"]")
			}
		}
		return fmt.Errorf("bridge-check 红:%d 个 wire-bridge(真短路)%s", brep.Summary.Bridges, strings.Join(nets, " "))
	}
	return nil
}

func allSnaps(execs []zaaMemberExec) []zaaPinSnap {
	var out []zaaPinSnap
	for _, m := range execs {
		out = append(out, m.Snaps...)
	}
	return out
}

// zaaStepRecord 把执行指令折成 tidy 的回滚记录(位姿 + 原连接)。
func zaaStepRecord(m zaaMemberExec) tidyStepRecord {
	rec := tidyStepRecord{Designator: m.Desig, PrimitiveID: m.PrimID,
		OrigX: m.OrigX, OrigY: m.OrigY, OrigRot: m.OrigRot}
	for _, s := range m.Snaps {
		rec.Restores = append(rec.Restores, tidyPinRestore{
			Pin: s.Pin, Net: s.Net, Kind: s.Kind, Direction: s.Dir, HasFlag: !s.Wired && s.Kind != "",
		})
	}
	return rec
}
