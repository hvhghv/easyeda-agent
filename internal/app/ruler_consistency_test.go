package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ruler_consistency_test.go — **同一把尺**的守门测试。
//
// 这个仓库最常复发的一类 bug 不是算错,是**同一个概念被算了两遍**,两处慢慢漂开:
//
//	求解器按器件本体判间距,clusters 按含 marker 的组体积判  → 求解器认为合法的落点当场被门判死
//	连接器按「本次调用改变了什么」判成败,用户关心「内容在不在画布上」 → 假失败
//	status 读分区框的区名标签,check 读自由文本数              → 明明有四条说明却报 0
//
// 这三处的注释里都写了「必须与 X 同一把尺」,但注释拦不住下一次改动。所以把配对
// 关系钉成测试:**谁改了其中一边,这里就会红**。
//
// 加新判据时,如果它复用了别处的阈值或口径,就在这里补一条 —— 这比在两处注释里
// 互相叮嘱可靠得多。

func TestRuler_ClusterGapMatchesSolverGap(t *testing.T) {
	// 生成侧(块布局求解器件间最小间隙)与判定侧(clusters 组间 tight 阈值)
	// 必须是同一个数:求解器按 bslPartGap 摆开,门却按别的数判,布局就永远
	// "刚放好就被判不合格"。
	if bslPartGap != bapObstacleGap {
		t.Fatalf("bslPartGap=%v 与 bapObstacleGap=%v 漂开了 —— 求解器与落地前硬门用的不是同一把尺",
			bslPartGap, bapObstacleGap)
	}
	// gate 的 clusters 关也用它(见 gateClustersStage 的 minGap)。
	if got := bslPartGap; got != 20 {
		t.Errorf("间距基准变成了 %v —— 改它要同时确认 `sch clusters --min-gap` 的默认值与帮助文本", got)
	}
	// zone-arrange Phase A 的组间/锚卫间距也归这把尺(2026-08-17:各立 10/12 时
	// P3 真机三处浅擦 —— 规划裸 bbox vs check 文字渲染宽度,gap 必须吃下外延)。
	if zfGroupGap != bslPartGap || zfAnchorGap != bslPartGap {
		t.Fatalf("zfGroupGap=%v / zfAnchorGap=%v ≠ bslPartGap=%v(两把尺!)", zfGroupGap, zfAnchorGap, bslPartGap)
	}
}

func TestRuler_GateDefaultsShared(t *testing.T) {
	// `sch status --gate` 复用 collectSchGate,必须传 gate 自己的默认阈值;
	// 各抄一份字面量的话,某次调参后同一张画布会给出两个判定。
	// overlapEps 不再钉字面量:它的真值源是 schMarkerOverlapEps(2026-08-17 归一,
	// 见 TestRuler_GateOverlapEpsMatchesCheck)—— 这里只钉「gate 与 check 同源」。
	if gateDefaultMinGap != 2.54 || gateDefaultPinEps != 0 {
		t.Fatalf("gate 默认阈值被改了(%v/%v)—— 确认 `sch gate` 的 flag 默认值与 `sch status --gate` 的传参都跟着改",
			gateDefaultMinGap, gateDefaultPinEps)
	}
}

func TestRuler_CircuitNoteCountSingleSource(t *testing.T) {
	// 「电路说明有几条」只有 schCircuitNoteCount 一个口径,check 与 status 都调它。
	// 这几条断言本身平淡,它们的价值在于:任何人想再写一遍这个减法时,会先看到这里。
	if got := schCircuitNoteCount(5, 2); got != 3 {
		t.Errorf("自由文本 5 − 区名标签 2 = %d, want 3", got)
	}
	if got := schCircuitNoteCount(2, 5); got != 0 {
		t.Errorf("标签比文本多时该给 0(不是负数), got %d", got)
	}
	if got := schCircuitNoteCount(0, 0); got != 0 {
		t.Errorf("空页该给 0, got %d", got)
	}
}

func TestRuler_SettleReadBudgetSane(t *testing.T) {
	// 回读稳定窗口:太短退化成"读两次同一个旧值",太长让每条写命令都变慢。
	if settleDelay < 200*time.Millisecond || settleDelay > time.Second {
		t.Errorf("settleDelay=%v 超出合理区间 [200ms,1s]", settleDelay)
	}
	if settleAttempts < 2 {
		t.Errorf("settleAttempts=%d < 2 —— 那就等于没有重试", settleAttempts)
	}
	if settleAttempts > 4 {
		t.Errorf("settleAttempts=%d 过多 —— 确定的失败会被拖成漫长等待", settleAttempts)
	}
}

func TestRuler_ConnectPinBudgetExceedsConnectorWorstCase(t *testing.T) {
	// CLI 的预算必须大于连接器内部最坏路径(wire 7s + 0.25s + wire 重试 7s +
	// netflag 7s = 21.25s),否则 daemon 先放弃、对方还在跑 —— 报出来的是我们
	// 自己的不耐烦。与 cmd_sch_autoconnect_retry_test.go 重复是有意的:
	// 那边测行为,这里登记"尺子"。
	const connectorWorstCase = 21250 * time.Millisecond
	if acConnectPinTimeout <= connectorWorstCase {
		t.Fatalf("connect_pin 预算 %v 没超过连接器最坏耗时 %v", acConnectPinTimeout, connectorWorstCase)
	}
}

// tidyLabelRotation 是 orientation.json frozenTable 的 Go 手抄本 —— 手抄必漂:
// 2026-08-16 zone-arrange --apply 首跑被 D1 的右向 GND 旗拦下,根因是手抄本缺了
// 横向四值却把缺失说成「契约未校准」。钉死:12 个值逐项等于 frozenTable。
func TestRuler_TidyLabelRotationMatchesFrozenTable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "skills", "easyeda-agent", "references", "orientation.json"))
	if err != nil {
		t.Fatalf("读 orientation.json:%v", err)
	}
	var doc struct {
		FrozenTable map[string]map[string]float64 `json:"frozenTable"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	kindOf := map[string]string{"power": "power", "ground": "ground", "port": "net_port_bi"}
	for row, kind := range kindOf {
		for dir, want := range doc.FrozenTable[row] {
			got, gerr := tidyLabelRotation(kind, dir)
			if gerr != nil {
				t.Errorf("%s %s:frozenTable 有值 %g,Go 表却拒绝:%v", row, dir, want, gerr)
				continue
			}
			if got != want {
				t.Errorf("%s %s:Go 表 %g ≠ frozenTable %g(两把尺!)", row, dir, got, want)
			}
		}
	}
}

// 外框只有**一个**函数:zone-plan 第一遍的框、zone-arrange phase A 的现状框、
// phase A 收敛后的框,三者必须逐字段同源(2026-08-20 用户裁定)。此前 phase A 自己
// 拿常量 NoteBand 拼了一份,而 zone-plan 按已登记说明的实际渲染高算 —— 两套带账,
// 收紧出来的框装不下后放的 note。详细正负对照见 cmd_sch_zone_frame_test.go。
func TestRuler_ZoneFrameSingleFunction(t *testing.T) {
	opts := defaultPartitionOpts()
	content := layoutBBox{100, 200, 500, 640}
	for _, noteH := range []float64{0, 13, 39, 91} {
		r := partitionFirstPassRect(content, opts, noteH)
		w, h := zoneArrangeRawFrame(content, opts, noteH)
		if r.MaxX-r.MinX != w || r.MaxY-r.MinY != h {
			t.Fatalf("noteH=%.0f:zone-plan 框 %.0f×%.0f ≠ phase A 框 %.0f×%.0f(两把尺!)",
				noteH, r.MaxX-r.MinX, r.MaxY-r.MinY, w, h)
		}
		// 带高只由「已登记说明的内容+字号」推导 —— 读落点就会造出自增长反馈环。
		band := schZoneNoteBandHeight(opts.NoteBand, noteH)
		want := layoutBBox{
			MinX: content.MinX - partitionContentPad, MinY: content.MinY - partitionContentPad - band,
			MaxX: content.MaxX + partitionContentPad, MaxY: content.MaxY + partitionContentPad + opts.TitleBand,
		}
		if r != want {
			t.Fatalf("noteH=%.0f:外框算式漂了 %+v want %+v", noteH, r, want)
		}
	}
}

// 「哪几个组算一个分区」也只有一个答案:**一个虚拟组 / zone 认领 = 一个分区**。
// 画框侧(partitionGrouping → planPartitions)与排布侧(zaPartitionPlan,每个
// 落位区一个框)必须逐字给出同一套分组 —— 2026-08-20 真机:画框侧按网格带把
// 「同一格」的两个区并成一个分区,于是 zone-arrange 断言③ 全绿(逐区框零重叠)
// 的 MCU_IO 页,zone-plan 报 partitionOverlap=1、zone-draw 拒绝画框,而画分区框
// 是 SKILL 铁律 15。这里钉结构(同一批区名 → 同一套分组),真机几何的行为级
// 正负对照见 cmd_sch_zone_partition_test.go。
func TestRuler_ZonePartitionGroupingMatchesArrange(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	boxes := map[string]layoutBBox{
		"pwr": {MinX: 100, MinY: 500, MaxX: 300, MaxY: 700},
		"mcu": {MinX: 500, MinY: 200, MaxX: 800, MaxY: 700},
		"usb": {MinX: 100, MinY: 150, MaxX: 300, MaxY: 300},
	}
	var mods []partitionModule
	res := zaResult{OK: true}
	for _, n := range []string{"mcu", "pwr", "usb"} {
		mods = append(mods, partitionModule{Name: n, BBox: boxes[n], CoreBBox: boxes[n]})
		res.Placed = append(res.Placed, zaPlaced{Name: n, Rect: boxes[n]})
	}
	arrange := zpPartitionNameSets(zaPartitionPlan(res, sheet, nil, opts))
	draw := zpPartitionNameSets(planPartitions(sheet, nil, mods, opts))
	if strings.Join(arrange, " | ") != strings.Join(draw, " | ") {
		t.Fatalf("分区归属两把尺:排布侧 %v ≠ 画框侧 %v", arrange, draw)
	}
}

// gate 的 check 段与 `sch check` 必须用同一个 marker-overlap 阈值:2026-08-17
// 真机上 check 报 0、gate --strict 报 9,根因是 gateDefaultOverlapEps 手抄了 0.5。
func TestRuler_GateOverlapEpsMatchesCheck(t *testing.T) {
	if gateDefaultOverlapEps != schMarkerOverlapEps {
		t.Fatalf("gate overlap eps %g ≠ check 的 schMarkerOverlapEps %g(两把尺!)",
			gateDefaultOverlapEps, schMarkerOverlapEps)
	}
}

// 「桩线伸展」也只有一把尺。2026-08-20 真机 4 轮取证:同一件事有三套算法
// (phase A 自拼端子盒 / --apply 未覆盖 pin 走自由 autoconnect / group-move 刚体
// 平移也走自由 autoconnect),于是 dry-run 每轮 pass、落地每轮重叠,而且不收敛。
// 钉住三条配对:
//
//	① kind 映射:规划侧 zfCanonKind == 落地侧 zaaConnectKind;
//	② 端子几何:zfTermGeom 的 marker 盒 == 落点评分器/`sch check` 的
//	   predictedMarkerBBox(**同一个函数**,不是"近似相等");
//	③ 桩端点:走 endpointFor(连接器 connect_pin 的 5 网格吸附),不许另算。
func TestRuler_StubGeometrySingleFunction(t *testing.T) {
	cases := []struct{ kind, net, dir string }{
		{"netflag", "GND", "down"}, {"netflag", "GND", "up"},
		{"netflag", "+3V3", "up"}, {"netflag", "5V", "left"},
		{"netport", "USB_DTR", "right"}, {"netport", "MCU_TX", "left"},
	}
	for _, c := range cases {
		// ① 两侧的 canonical kind 必须逐字相同。
		if got, want := zfCanonKind(c.kind, c.net), zaaConnectKind(zfPlacedTerm{Kind: c.kind, Net: c.net}); got != want {
			t.Fatalf("%s/%s:规划侧 kind %q ≠ 落地侧 kind %q(两把尺!)", c.kind, c.net, got, want)
		}
		// ②③ 几何必须是落地那条链本身。
		const px, py, off = 100.0, 200.0, 30.0
		ex, ey := endpointFor(px, py, off, c.dir)
		wire, marker := zfTermGeom(px, py, off, c.dir, c.kind, c.net, 0)
		if want := predictedMarkerBBox(ex, ey, zfCanonKind(c.kind, c.net), c.dir, c.net); marker != want {
			t.Fatalf("%s/%s/%s:规划的 marker 盒 %+v ≠ predictedMarkerBBox %+v(两把尺!)",
				c.kind, c.net, c.dir, marker, want)
		}
		if wire.MinX > ex || wire.MaxX < ex || wire.MinY > ey || wire.MaxY < ey {
			t.Fatalf("%s/%s/%s:桩线段没盖住 endpointFor 的端点(%g,%g):%+v", c.kind, c.net, c.dir, ex, ey, wire)
		}
	}
	// 落地余量必须就是桩端点吸附的那一格 —— 它是规划框成为**上界**的依据。
	if zfLandSlack != acSchGrid {
		t.Fatalf("zfLandSlack=%v ≠ acSchGrid=%v —— 余量与吸附网格分家,规划框不再是落地框的上界",
			float64(zfLandSlack), float64(acSchGrid))
	}
}
