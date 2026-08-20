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
// 执行走 ADR-0004 单一安全 move 内核(schMoveKernel),**页级一次 sweep**
// (旧位置上各区标签互相穿插,分区 sweep 必然把邻区 pin 判成「共享树」而拒绝;
// 全员入集后这不再是共享):
//
//	快照 → 断言① → 内核[快照+bridge 基线 → 删证(回读)→ 逐件落位(转竖件
//	双候选实测消解)→ 重连(计划端子显式 connect_pin)→ 对账(网表逐 pin +
//	bridge 增量),失败自动恢复] → 断言②(区级)→ layout-lint → save。

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
	// Offset 是原桩长(pin → 标记锚的实测距离)。**曾经被丢掉**(`s.Dir, _ =
	// tidyStubDirection(...)`),于是「原形保留」区里由 zaaPadTermsToPins 合成出来的
	// 端子一律按默认短桩 zfStub 重建 —— 计划说「一个单位都不动」,落地却把桩换了长度。
	// 方向与长度是同一次观测的两半,不许只取一半。
	Offset float64
	Wired  bool // 树上无标记的普通导线直连 —— apply 盖不住,预检拒绝
	// PinX/PinY 是实测 pin 坐标:retain 刚体不变式(zaaGateRetainRigid)要按它
	// 重走一遍落地那条链,算出「不动的话该长什么样」。
	PinX, PinY float64
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
	Rotate             bool
	TargetCX, TargetCY float64
	Terms              []zaaTermExec
	Snaps              []zaaPinSnap // 本件的 pin 快照(回滚重建原料)
	// RetainBox 是「↩ 原形保留」区成员**落地后应有的** L1 包络(移动前实测几何
	// 刚体平移 DX/DY,按落地那条链 zfTermGeomCanon 复算)。非 retain 区为 nil。
	// 落地复判逐组拿它跟真机量出来的 cluster.Box 比 —— 这条判据不依赖
	// markerBBoxProfile 之外的任何规划假设,是「不动的东西真的没动」的机械形式。
	RetainBox *layoutBBox
}

// zaaConnectKind 把规划端子折成 connect_pin 口径。**映射本体在 zfCanonKind** ——
// 规划侧(端子几何预测)与落地侧(connect_pin 的 kind)必须是同一个函数,各自
// switch 会出现「预测的是 power 盒、落地的是 ground 旗」这种看不见的分家。
func zaaConnectKind(t zfPlacedTerm) string {
	return zfCanonKind(t.Kind, t.Net)
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

// ── retain 刚体不变式:「↩ 原形保留」必须兑现 ────────────────────────────────
//
// 真机 ceshi / 页 MCU_IO(2026-08-20):区 esp32s3_wroom1_module 走了 retain 路径,
// 输出白纸黑字写着「↩ 原形保留(不重排、不重生桩)」,落地后 L1 组却从 391×421
// 变成 391×562 —— 宽度分毫不差(横向复现是准的),高度凭空多了 141。
//
// 「不动的东西真的没动」是本命令**最强的可验证不变式**:它不依赖 markerBBoxProfile
// 这类标定,也不依赖规划器的任何假设,只要求 (dir, offset, kind, net) 逐 pin 与
// 移动前逐字相同 —— 落地就必然是刚体平移。所以它该是一条**执行前**的算术门
// (断言①的几何形式),而不是事后复判:计划与实测一旦对不上,那是我们自己的
// 计划/映射缺陷,拿画布去试错没有意义。
//
// 门只对 `retained` 的区开(收敛区本来就要改几何)。

// zaaRetainDeviation 是一条 retain 违例的可读描述。
type zaaRetainDeviation struct {
	Pin               string
	WantDir, GotDir   string
	WantOff, GotOff   float64
	WantKind, GotKind string
	Note              string
}

func (d zaaRetainDeviation) String() string {
	if d.Note != "" {
		return fmt.Sprintf("pin%s %s", d.Pin, d.Note)
	}
	var parts []string
	if d.WantDir != d.GotDir {
		parts = append(parts, fmt.Sprintf("方向 %s→%s", d.WantDir, d.GotDir))
	}
	if math.Abs(d.WantOff-d.GotOff) > zaaRetainEps {
		parts = append(parts, fmt.Sprintf("桩长 %.0f→%.0f", d.WantOff, d.GotOff))
	}
	if d.WantKind != d.GotKind {
		parts = append(parts, fmt.Sprintf("类型 %s→%s", d.WantKind, d.GotKind))
	}
	return fmt.Sprintf("pin%s %s", d.Pin, strings.Join(parts, "、"))
}

// zaaRetainEps 是刚体判定的容差:计划端子与快照来自**同一份场景快照的同一次
// 观测**,理论上逐字相等,半格容差只是挡浮点噪声 —— 真正的缺陷(默认短桩 20
// 顶掉实测 40、端子被换到另一只同网 pin)都在一格以上。
const zaaRetainEps = 0.5

// zaaGateRetainRigid 逐 pin 比对「原形保留」区的执行指令与移动前快照。
// 纯函数;返回非 nil = 计划没兑现原形,fail-closed(画布零改动)。
func zaaGateRetainRigid(desig string, snaps []zaaPinSnap, terms []zaaTermExec) error {
	byPin := map[string]zaaPinSnap{}
	for _, s := range snaps {
		byPin[s.Pin] = s
	}
	var bad []zaaRetainDeviation
	seen := map[string]bool{}
	for _, t := range terms {
		s, ok := byPin[t.Pin]
		if !ok {
			bad = append(bad, zaaRetainDeviation{Pin: t.Pin, Note: "计划端子落到了移动前没有连接的 pin 上"})
			continue
		}
		if seen[t.Pin] {
			bad = append(bad, zaaRetainDeviation{Pin: t.Pin, Note: "同一只 pin 被两条计划端子占用(同网扩容把原形改胖了)"})
			continue
		}
		seen[t.Pin] = true
		if s.Dir != t.Dir || math.Abs(s.Offset-t.Offset) > zaaRetainEps || s.Kind != t.Kind {
			bad = append(bad, zaaRetainDeviation{Pin: t.Pin,
				WantDir: s.Dir, GotDir: t.Dir, WantOff: s.Offset, GotOff: t.Offset,
				WantKind: s.Kind, GotKind: t.Kind})
		}
	}
	for _, s := range snaps {
		if !seen[s.Pin] {
			bad = append(bad, zaaRetainDeviation{Pin: s.Pin, Note: "移动前有连接,计划却没有对应端子(原形会缺一支)"})
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Slice(bad, func(i, j int) bool { return tidyDesignatorLess(bad[i].Pin, bad[j].Pin) })
	msgs := make([]string, 0, len(bad))
	for _, d := range bad {
		msgs = append(msgs, d.String())
	}
	return fmt.Errorf("断言①红(retain 刚体不变式):%s 标着「原形保留」,但 %d 处执行指令与移动前几何不符 —— %s;拒绝执行,画布零改动。这是计划/映射缺陷不是画布问题:`sch zone-arrange --json` 看该区 retainWhy,单区搬运可先用 `sch group-move`(它走同一个内核的 preserve 策略)",
		desig, len(bad), strings.Join(msgs, ";"))
}

// zaaRetainEnvelope 按落地那条链(zfTermGeomCanon)算一件在给定平移下的落地包络:
// 本体 ∪ 每支桩线 ∪ 每支 marker。**判定与落地同一把尺** —— 复判要拿它跟真机量出来
// 的 L1 体积比,自造第二把尺就什么也证明不了。
func zaaRetainEnvelope(body layoutBBox, snaps []zaaPinSnap, dx, dy float64) layoutBBox {
	out := layoutBBox{MinX: body.MinX + dx, MinY: body.MinY + dy, MaxX: body.MaxX + dx, MaxY: body.MaxY + dy}
	has := true
	for _, s := range snaps {
		if s.Kind == "" || s.Dir == "" {
			continue
		}
		wire, marker := zfTermGeomCanon(s.PinX+dx, s.PinY+dy, s.Offset, s.Dir, s.Kind, s.Net, 0)
		zfGrow(&out, &has, wire)
		zfGrow(&out, &has, marker)
	}
	return out
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
			// 方向**与桩长**都取实测(同一次观测的两半):只取方向、桩长退回 zfStub,
			// 就是「原形保留」区里最后那处偷偷改几何的地方 —— 真机 U2 高度 +141 的
			// 一类成因。没有实测(pre 里这个网没有可复现的桩)才退默认短桩。
			dir, off := "right", zfStub
			for _, p := range pre {
				if p.Net == net && p.Dir != "" {
					dir = p.Dir
					if p.Offset > 0 {
						off = p.Offset
					}
					break
				}
			}
			out = append(out, zfPlacedTerm{Kind: kind, Net: net, Dir: dir, Offset: off})
		}
	}
	return out
}

// zaaMapTerms 把计划端子映射到具体 pin。四轮由紧到松,全确定性(pin 号自然序 ×
// 计划序):
//
//	① net + 实测桩方向 + 实测桩长(几何全等)—— **原形保留区靠这一轮**:
//	   retain 计划的每个端子就是从某只 pin 量出来的,(dir,offset) 是它的指纹;
//	② net + 实测桩方向 —— 收敛计划里方向已被规划改写,但同网多脚仍能靠方向区分;
//	③ net + 现侧(pin 相对本体中心的主轴)—— J1 的双 U3_N4(左右各一)靠它区分;
//	④ net。
//
// 为什么 ①② 必须排在「现侧」前面:**现侧 ≠ 桩方向**。本体越高瘦,两者分歧越
// 系统性 —— 真机 U2(71×421)上下两端行的 pin,|dy| 反而大于 |dx|,现侧被判成
// up/down,而它们的桩实际是 left/right。只有 ③ 时这些端子第一轮全落空,退到 ④
// 按 pin 号顺序乱配,同网多脚的桩几何当场互换 → 原形保留的区照样变形。
func zaaMapTerms(pre []zaaPinSnap, terms []zfPlacedTerm, pinSide map[string]string,
	termUpper func(zfPlacedTerm) bool) ([]zaaTermExec, error) {
	used := map[int]bool{}
	sorted := append([]zaaPinSnap(nil), pre...)
	sort.SliceStable(sorted, func(i, j int) bool { return tidyDesignatorLess(sorted[i].Pin, sorted[j].Pin) })
	var out []zaaTermExec
	for _, t := range terms {
		pick := -1
		for pass := 0; pass < 4 && pick < 0; pass++ {
			for i, p := range sorted {
				if used[i] || p.Net != t.Net {
					continue
				}
				switch pass {
				case 0:
					if p.Dir == "" || p.Dir != t.Dir || math.Abs(p.Offset-t.Offset) > 1e-6 {
						continue
					}
				case 1:
					if p.Dir == "" || p.Dir != t.Dir {
						continue
					}
				case 2:
					if pinSide[p.Pin] != t.Dir {
						continue
					}
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
		// 说明带高必须取**本区**的(zaaZoneNoteBand 从规划框反推,框是唯一函数),
		// 不是全局默认 opts.NoteBand:规划给登记过说明的区留的是 42 以上的带,
		// 执行却按 42 落位 —— 内容整体下沉进说明带,框跟着往下长,严重时直接探出
		// 图纸下沿(真机 `C5 左沿 -34`、`U2 上沿 840` 那一类越界的帮凶)。
		// 复判侧早就用的是反推带高,这里再用一次全局常量就是两把尺。
		offY := rect.MinY + zaaZoneNoteBand(z, opts.TitleBand) + partitionContentPad - z.Content.MinY
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
				s := zaaPinSnap{Desig: g.Designator, Pin: p.Number, PinX: p.X, PinY: p.Y}
				if !hasM {
					s.Wired = true
				} else {
					s.Net = m.Net
					s.Kind = tidyRestoreKind(m.ComponentType, m.Net)
					s.Dir, s.Offset = tidyStubDirection(p.X, p.Y, m.X, m.Y)
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
			// 「↩ 原形保留」区:执行指令必须逐 pin 与移动前几何相同,否则这份计划
			// 根本没在保留原形 —— 执行前就拒(算术判定,画布零改动)。
			if z.Retained {
				if g.Rotated {
					return nil, nil, fmt.Errorf("断言①红(retain 刚体不变式):%s 标着「原形保留」却带转竖标记 —— 内部不一致,拒绝执行", g.Designator)
				}
				if err := zaaGateRetainRigid(g.Designator, snaps, terms); err != nil {
					return nil, nil, err
				}
				box := zaaRetainEnvelope(*live.BBox, snaps, me.DX, me.DY)
				me.RetainBox = &box
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
	_ = sweepSet // 名单守卫已在 zaaBuildExec 落判;清扫本体归内核(同一份成员集)

	// 执行只准调内核(ADR-0004):快照 → 页级一次深度清扫(删证回读)→ 逐件
	// 落位(snap5;转竖件双候选实测消解 + pin 中点对中)→ 合并早检(删桩线触发
	// 的共线合并吞第三方 pin,在新桩线落地前修回)→ 重连(计划端子显式
	// connect_pin,梯次桩长原样执行)→ 对账(网表逐 pin + bridge 增量),任一步
	// 失败自动进入**全页**恢复段(esp32Mini P2 实锤:GND 树上 9 个第三方地脚被
	// 灌进 +3V3,只救移动集合=抓到了但救不回;修不动时 kerr 自带结构化清单
	// REF→期望网,可直接喂 `sch connect`,报告从「页面已毁」降级为「N 个 pin
	// 待手工恢复」)。此前散在这里的 sweep/exec/回滚逻辑全部由内核承接。
	termByPin := map[string]zaaTermExec{}
	items := make([]moveItem, 0, len(execs))
	for _, m := range execs {
		m := m
		for _, t := range m.Terms {
			termByPin[strings.ToUpper(m.Desig)+":"+t.Pin] = t
		}
		it := moveItem{Designator: m.Desig, HasTarget: true}
		if m.Rotate {
			// 转竖件(R1):候选 OrigRot±90,pin 中点驱动落位,上下序实测消解。
			it.X, it.Y = m.TargetCX, m.TargetCY
			it.CenterOnPins = true
			it.RotCandidates = []float64{math.Mod(m.OrigRot+90, 360), math.Mod(m.OrigRot+270, 360)}
			it.VerifyPins = func(pins []layoutPin) (bool, error) {
				return zaaVerticalOrderOK(pins, m.Terms), nil
			}
		} else {
			it.X, it.Y = m.OrigX+m.DX, m.OrigY+m.DY
		}
		it.Terms = func(pins []layoutPin) ([]moveConnTerm, error) {
			out := make([]moveConnTerm, 0, len(m.Terms))
			for _, t := range m.Terms {
				out = append(out, moveConnTerm{Pin: t.Pin, Kind: t.Kind, Net: t.Net,
					Direction: t.Dir, Rotation: t.LabelRot, Offset: t.Offset})
			}
			return out, nil
		}
		items = append(items, it)
	}
	// 桩长硬上限 = 计划里最长的桩:计划端子本来就原样执行(Offset 显式喂
	// connect_pin),没被计划覆盖的 pin 走内核 preserve/autoconnect 兜底时也不许
	// 比规划走得更深 —— 否则区框当场比规划胖一档,而 dry-run 还在打 pass。
	krep, kerr := schMoveKernel(cfg, win, docUUID, items,
		moveKernelOpts{Label: "zone-arrange", Stdout: stdout, Stderr: stderr,
			StubPolicy: moveStubPreserve, MaxStub: zaaMaxPlannedStub(execs)})
	if kerr != nil {
		return kerr
	}
	executed := len(krep.Moved) + len(krep.Skipped)
	fmt.Fprintf(stdout, "内核落位 %d/%d 件(对账绿);进入断言②区级对账…\n", executed, len(execs))

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

	// 真短路(wire-bridge)判据已内置在内核对账(bridge 增量检查,红即失败 +
	// 恢复段),这里不再重复跑;layout-lint 红 = 标签实测伸展超出规划估算 →
	// 如实报,重跑一轮收敛即修(两遍法),不为几个单位的标签擦碰拆掉整页落位。
	lintWarn := ""
	if rep, lerr := collectLayoutLint(cfg, win, 2.54, 0, false, false, false); lerr != nil {
		lintWarn = fmt.Sprintf("layout-lint 无法运行:%v", lerr)
	} else if !rep.OK {
		lintWarn = fmt.Sprintf("layout-lint:%s(标签实测伸展 > 规划估算 —— 重跑一轮 `zone-arrange --apply` 用实测反哺收敛)", rep.Summary)
	}
	if _, err := requestAutolayoutAction(cfg, "schematic.save", win, nil, docUUID, "zone-arrange save"); err != nil {
		fmt.Fprintf(stderr, "⚠ 显式保存失败(%v)—— daemon 防抖自动保存仍会兜底\n", err)
	}

	// ── 落地复判(断言③)────────────────────────────────────────────────────
	// 「规划 pass → 落地 overlap」是本命令最贵的一类假绿:断言①②看电气、内核对账
	// 看网表、layout-lint 看器件两两重叠,没有一条看得见「区框胖了撞邻区」。
	// 这里重读一次真几何,按**同一个外框函数**算实测框,与规划框逐区比 ——
	// 偏差 > gutter 或区框重叠 → 如实报,并以非零退出让它可 gate。
	var recheck []string
	if landed, rerr := zaaLandedRecheck(cfg, win, docUUID, out, execs, opts); rerr != nil {
		recheck = []string{fmt.Sprintf("落地复判无法运行(%v)—— 没有证明不算过", rerr)}
	} else {
		recheck = zaaRecheckFindings(landed, opts.Gutter)
		for _, z := range landed {
			mark := ""
			if len(z.Rigid) > 0 {
				mark = "  ↩✗ 原形被改动"
			}
			fmt.Fprintf(stdout, "  复判 %s:实测框 %.0f×%.0f / 规划框 %.0f×%.0f%s\n",
				z.Name, z.FrameW, z.FrameH, z.PlanW, z.PlanH, mark)
		}
	}
	// 自由落点是「规划没覆盖到的 pin」——它的几何不在任何计划里,于是区框可以
	// 凭空胖一档而 dry-run 毫不知情。内核把它们点名带回来(FreeConnected),这里
	// 并进断言③:**偏差可以有,但必须可见**。
	if len(krep.FreeConnected) > 0 {
		recheck = append(recheck, fmt.Sprintf("%d 只 pin 走了 autoconnect 自由落点(计划未覆盖:%s)—— 它们的方向/桩长不在规划里,落地框可能凭空胖一档",
			len(krep.FreeConnected), strings.Join(krep.FreeConnected, " ")))
	}

	if lintWarn != "" {
		fmt.Fprintf(stdout, "△ zone-arrange 落地 %d/%d 件;断言①② + 内核对账(网表+bridge)绿,已保存;%s\n", executed, len(execs), lintWarn)
	} else {
		fmt.Fprintf(stdout, "✓ zone-arrange 落地 %d/%d 件;断言①② + 内核对账(网表+bridge)+ layout-lint 绿,已保存\n", executed, len(execs))
	}
	fmt.Fprintln(stdout, "note: 分区框未重画 —— `sch zone-draw --mode partition` 更新;区名/说明带随框走")
	if len(recheck) > 0 {
		// **不回滚**:位姿与电气都是好的,回滚只会把好的也拆掉。如实报 + 非零退出。
		return fmt.Errorf("断言③红(落地复判):%s —— 电气与位姿已落地并保存,但分区几何与规划不符;`sch zone-plan` 复核后按上表调整(收敛不了就拆页/改 --gutter)",
			strings.Join(recheck, ";"))
	}
	fmt.Fprintf(stdout, "✓ 断言③绿(落地复判):实测框与规划框偏差 ≤ gutter %.0f,区框零重叠\n", opts.Gutter)
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

// ── 落地复判:绿勾必须与事实相符 ─────────────────────────────────────────────
//
// 真机 4 轮取证:`--apply` 每轮都打「断言①② + 内核对账 + layout-lint 全绿,已保存」,
// 而落地后 `zone-plan` 实测分区框重叠 2 / 1 / 2 处。断言①②管的是**电气**(删除集
// = 重建集、曾连接 pin 仍连接),内核对账管的是**网表**,layout-lint 管的是**器件
// 两两重叠** —— 三条判据没有一条看得见「区框比规划胖了、于是撞上邻区」。
// 缺的那条判据在这里补上:落地后重读一次,按同一个外框函数算实测框,与规划框比。

// zaaLandedZone 是一个区的落地实测(与规划的对照项)。
type zaaLandedZone struct {
	Name           string
	Content        layoutBBox // 实测内容并集(成员 L1 簇体积)
	Rect           layoutBBox // 实测框 = partitionFrameRect(内容, 区名带, 说明带)
	FrameW, FrameH float64
	PlanW, PlanH   float64
	// Missing 是这一区**读不到**的成员。unknown 是一等公民:读不到就绝不排除出
	// 分母、也绝不合成 0,否则一次读故障会伪装成「完美收敛」。
	Missing []string
	// Outside 是探出图纸可用区(图框内缩 sheetEdgeMinGap)的成员 —— 与
	// `sch clusters` 的 out-of-sheet 同一把尺、同一个常量。真机 MCU_IO:--apply
	// 打完绿勾,事后 `sch clusters --strict` 才报 `C5 左沿 -34 < 12`、`U2 上沿
	// 840 > 813`。落地路径自己看得见的事,不该等下一条命令来发现。
	Outside []string
	// Rigid 是「↩ 原形保留」区里**落地几何与原形平移不符**的成员(逐组比,
	// 不依赖规划模型)。空 = 这一区真的一个单位都没动。
	Rigid []string
}

// zaaRecheckFindings 是复判的纯判据:实测框与规划框的偏差 > gutter,或实测区框
// 两两重叠,或有成员读不到 —— 任一条成立就出条目(空 = 复判绿)。
//
// 为什么阈值是 gutter:排布器就是靠 gutter 把相邻区隔开的,偏差一旦大于它,
// 「规划无重叠」就不再蕴含「落地无重叠」。这正是本次缺陷的形式化。
func zaaRecheckFindings(zones []zaaLandedZone, gutter float64) []string {
	var out []string
	for _, z := range zones {
		if len(z.Missing) > 0 {
			out = append(out, fmt.Sprintf("区 %s 有 %d 个成员读不到(%s)—— 实测框不可信,不算过",
				z.Name, len(z.Missing), strings.Join(z.Missing, ",")))
			continue
		}
		// retain 区先判**刚体不变式**:它比框尺寸更强也更可信(不依赖任何预测
		// 模型),而且尺寸偏差往往只是它的后果 —— 先报因,再报果。
		if len(z.Rigid) > 0 {
			out = append(out, fmt.Sprintf("区 %s 标着「↩ 原形保留」却动了几何:%s —— 「不重排、不重生桩」没有兑现",
				z.Name, strings.Join(z.Rigid, ";")))
		}
		// **单边判据**:只有「落地比规划**胖**」才是缺陷 —— 那正是「规划无重叠却
		// 落地重叠」的成因。落地更瘦只说明落地余量(zfLandSlack)没用满,不是问题,
		// 更不该占掉 gutter 预算(否则结构性余量会把判据的容忍度先吃掉一半)。
		//
		// **注意这不是「上界成立」的断言**:规划框只在「同一份 pin 坐标 + 同一份
		// 桩长」的模型内是上界(见 zfLandSlack 的适用范围),真机上并不成立 ——
		// 这条判据存在的全部理由,就是把不成立的那几次如实报出来。
		dw, dh := z.FrameW-z.PlanW, z.FrameH-z.PlanH
		if dw > gutter || dh > gutter {
			out = append(out, fmt.Sprintf("区 %s 落地框 %.0f×%.0f 比规划框 %.0f×%.0f 胖(超出 %+.0f/%+.0f,gutter %.0f)——「规划无重叠」不再蕴含「落地无重叠」",
				z.Name, z.FrameW, z.FrameH, z.PlanW, z.PlanH, dw, dh, gutter))
		}
		if len(z.Outside) > 0 {
			out = append(out, fmt.Sprintf("区 %s 有 %d 个成员探出图纸可用区:%s —— 落地即出图纸(`sch clusters --strict` 同一把尺)",
				z.Name, len(z.Outside), strings.Join(z.Outside, ";")))
		}
	}
	for i := 0; i < len(zones); i++ {
		for j := i + 1; j < len(zones); j++ {
			a, b := zones[i], zones[j]
			if len(a.Missing) > 0 || len(b.Missing) > 0 {
				continue
			}
			ox := minF(a.Rect.MaxX, b.Rect.MaxX) - maxF(a.Rect.MinX, b.Rect.MinX)
			oy := minF(a.Rect.MaxY, b.Rect.MaxY) - maxF(a.Rect.MinY, b.Rect.MinY)
			if ox > 0 && oy > 0 {
				out = append(out, fmt.Sprintf("区框实测重叠 %s ↔ %s:%.0f×%.0f", a.Name, b.Name, ox, oy))
			}
		}
	}
	return out
}

// zaaZoneNoteBand 从规划输出反推这一区的说明带高 —— 框是唯一函数
// (partitionFrameRect),带高就是它的可逆量:frameH − 内容高 − 2·pad − 区名带。
// 这样复判用的带高与规划**逐区一致**,不必再读一遍 note(读第二遍就是第二把尺)。
func zaaZoneNoteBand(z zoneArrangeZoneOut, titleBand float64) float64 {
	band := z.FrameH - (z.Content.MaxY - z.Content.MinY) - 2*partitionContentPad - titleBand
	if band < 0 {
		return 0
	}
	return band
}

// zaaOutOfSheetWhy 判一个盒子探出图纸可用区多少(可用区 = 图框内缩
// sheetEdgeMinGap,与 `sch clusters` 的 usable 逐字段同源 —— 同一个常量、同一条
// 不等式,别在这里另立一套边距)。返回 "" = 没探出。
func zaaOutOfSheetWhy(b, sheet layoutBBox) string {
	if sheet.MaxX-sheet.MinX <= 0 || sheet.MaxY-sheet.MinY <= 0 {
		return "" // 没有图框就没有可用区可判 —— 不许拿零尺寸的"图纸"把整页判成越界
	}
	usable := layoutBBox{
		MinX: sheet.MinX + sheetEdgeMinGap, MinY: sheet.MinY + sheetEdgeMinGap,
		MaxX: sheet.MaxX - sheetEdgeMinGap, MaxY: sheet.MaxY - sheetEdgeMinGap,
	}
	var why []string
	if b.MinX < usable.MinX {
		why = append(why, fmt.Sprintf("左沿 %.0f < %.0f", b.MinX, usable.MinX))
	}
	if b.MaxX > usable.MaxX {
		why = append(why, fmt.Sprintf("右沿 %.0f > %.0f", b.MaxX, usable.MaxX))
	}
	if b.MinY < usable.MinY {
		why = append(why, fmt.Sprintf("下沿 %.0f < %.0f", b.MinY, usable.MinY))
	}
	if b.MaxY > usable.MaxY {
		why = append(why, fmt.Sprintf("上沿 %.0f > %.0f", b.MaxY, usable.MaxY))
	}
	return strings.Join(why, "、")
}

// zaaRigidWhy 判一个 retain 成员的落地实测 box 与「原形刚体平移后应有的包络」
// 差多少。容差 acSchGrid(桩端点 5 网格吸附的一格);返回 "" = 真的没动。
func zaaRigidWhy(desig string, want, got layoutBBox) string {
	d := [4]float64{got.MinX - want.MinX, got.MinY - want.MinY, got.MaxX - want.MaxX, got.MaxY - want.MaxY}
	worst := 0.0
	for _, v := range d {
		if math.Abs(v) > math.Abs(worst) {
			worst = v
		}
	}
	if math.Abs(worst) <= acSchGrid {
		return ""
	}
	return fmt.Sprintf("%s 实测 %.0f×%.0f 与原形平移后应有的 %.0f×%.0f 差 %.0f(四边偏差 %.0f/%.0f/%.0f/%.0f)",
		desig, got.MaxX-got.MinX, got.MaxY-got.MinY, want.MaxX-want.MinX, want.MaxY-want.MinY,
		worst, d[0], d[1], d[2], d[3])
}

// zaaLandedRecheck 重读落地后的页面,按 L1 簇算每个区的实测框。纯读。
// execs 提供 retain 区的「原形平移后应有的包络」(RetainBox);为空时只做尺寸/
// 重叠/出图纸三条。
func zaaLandedRecheck(cfg *appConfig, win, docUUID string, out *zoneArrangeOut,
	execs []zaaMemberExec, opts partitionOpts) ([]zaaLandedZone, error) {
	res, err := requestAutolayoutAction(cfg, "schematic.components.list", win,
		map[string]any{"includeBBox": true, "includePins": true}, docUUID, "zone-arrange 落地复判")
	if err != nil {
		return nil, err
	}
	comps, perr := parseLayoutComps(res.Result)
	if perr != nil {
		return nil, perr
	}
	wires, werr := fetchSchWirePolylinesStable(cfg, win, docUUID)
	if werr != nil {
		return nil, fmt.Errorf("读导线:%w", werr)
	}
	clusters, _ := buildSchClusters(comps, wires)
	byDesig := map[string]schCluster{}
	for _, c := range clusters {
		byDesig[strings.ToUpper(c.Designator)] = c
	}
	retainBox := map[string]layoutBBox{}
	for _, e := range execs {
		if e.RetainBox != nil {
			retainBox[strings.ToUpper(e.Desig)] = *e.RetainBox
		}
	}
	var zones []zaaLandedZone
	for _, z := range out.Zones {
		lz := zaaLandedZone{Name: z.Name, PlanW: z.FrameW, PlanH: z.FrameH}
		has := false
		for _, g := range z.Groups {
			d := strings.ToUpper(g.Designator)
			c, ok := byDesig[d]
			if !ok {
				lz.Missing = append(lz.Missing, g.Designator)
				continue
			}
			zfGrow(&lz.Content, &has, c.Box)
			if why := zaaOutOfSheetWhy(c.Box, out.Sheet); why != "" {
				lz.Outside = append(lz.Outside, fmt.Sprintf("%s %s", g.Designator, why))
			}
			if want, isRetain := retainBox[d]; isRetain {
				if why := zaaRigidWhy(g.Designator, want, c.Box); why != "" {
					lz.Rigid = append(lz.Rigid, why)
				}
			}
		}
		if !has {
			lz.Missing = append(lz.Missing, "(全区无实测几何)")
			zones = append(zones, lz)
			continue
		}
		lz.Rect = partitionFrameRect(lz.Content, opts.TitleBand, zaaZoneNoteBand(z, opts.TitleBand))
		lz.FrameW, lz.FrameH = lz.Rect.MaxX-lz.Rect.MinX, lz.Rect.MaxY-lz.Rect.MinY
		zones = append(zones, lz)
	}
	return zones, nil
}

// zaaMaxPlannedStub 是本次计划里最长的桩 —— 内核常规重连步的桩长硬上限。
// 落地桩不越过规划桩,落地框就不越过规划框(恢复段有意不夹,见 moveKernelOpts）。
func zaaMaxPlannedStub(execs []zaaMemberExec) float64 {
	m := zfStub
	for _, e := range execs {
		for _, t := range e.Terms {
			m = maxF(m, t.Offset)
		}
	}
	return m
}

func allSnaps(execs []zaaMemberExec) []zaaPinSnap {
	var out []zaaPinSnap
	for _, m := range execs {
		out = append(out, m.Snaps...)
	}
	return out
}
