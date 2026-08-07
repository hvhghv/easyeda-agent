package app

// 射频维（dimRF）的离线单测。全部是纯结构体字面量喂纯函数，不连 daemon。
//
// 所有用例都把 scoreCtx.layout 留成 nil —— 这不是偷懒，而是「本维绝不依赖
// layout-lint 的 ratsnest」这条硬约定的机械证明：ratsnest 过滤掉了全局网，
// 一旦这里开始读它，馈线网名撞上 isGlobalNet 的正则就会让整维静默失明
// （见 pcb_score_rf.go 文件头的陷阱段）。

import (
	"math"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

// rfTestPad 造一个焊盘（坐标 mil）。
func rfTestPad(num, net string, x, y float64) boardPad {
	return boardPad{Number: num, Net: net, Layer: pcbSideTop, X: x, Y: y, W: 20, H: 20}
}

// rfScoreOf 跑一次射频维。layout 故意为 nil，见文件头。
func rfScoreOf(t *testing.T, comps []boardComp, s *spec.Spec) scoreDimension {
	t.Helper()
	ctx := &scoreCtx{snap: &boardSnapshot{Components: comps}, spec: s}
	return rfDimScorer{}.score(ctx)
}

// ── 阈值曲线 ────────────────────────────────────────────────────────────────

// 分段线性曲线必须单调不减，并在三个物理拐点上给出说得清的值。
func TestRFScoreFeedPenaltyCurve(t *testing.T) {
	cases := []struct{ len, want float64 }{
		{0, 0},
		{rfFeedIdealMil, 0},              // λ/10 之内不扣分
		{350, rfSoftPenalty / 2},         // ideal→budget 中点
		{rfFeedBudgetMil, rfSoftPenalty}, // λ/4 = 预算线
		{1250, rfSoftPenalty + (100-rfSoftPenalty)/2}, // budget→max 中点
		{rfFeedMaxMil, 100},                           // λ = 扣满
		{5000, 100},                                   // 再长也就是扣满
	}
	for _, c := range cases {
		if got := rfFeedPenalty(c.len); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("rfFeedPenalty(%.0f) = %.3f, want %.3f", c.len, got, c.want)
		}
	}
	prev := -1.0
	for l := 0.0; l <= 2500; l += 25 {
		got := rfFeedPenalty(l)
		if got < prev {
			t.Fatalf("penalty curve dips at %.0f mil: %.3f < %.3f", l, got, prev)
		}
		prev = got
	}
}

// ── 没有天线 = skipped，不是满分 ────────────────────────────────────────────

func TestRFScoreNoAntennaSkips(t *testing.T) {
	comps := []boardComp{
		{Designator: "R1", Device: "0402WGF1002TCE", Pads: []boardPad{rfTestPad("1", "NET1", 0, 0)}},
		{Designator: "C1", Device: "CL05A105KA5NNNC", Pads: []boardPad{rfTestPad("1", "3V3", 100, 0)}},
	}
	d := rfScoreOf(t, comps, nil)
	if d.Status != dimSkipped {
		t.Fatalf("status = %q, want %q", d.Status, dimSkipped)
	}
	if d.Score != 0 {
		t.Errorf("skipped dimension must not carry a score, got %.1f", d.Score)
	}
	if d.Reason == "" {
		t.Error("skipped dimension must explain itself")
	}
}

// ── 短馈线 = 满分，但状态恒为 degraded（keepout 那半边没测）────────────────

func TestRFScoreShortFeedIsFullMarksButDegraded(t *testing.T) {
	comps := []boardComp{
		{Designator: "U1", Device: "ESP32-WROOM-32E", Pads: []boardPad{
			rfTestPad("1", "GND", 0, 100),
			rfTestPad("2", "ANT_FEED", 0, 0),
		}},
		{Designator: "L1", Device: "0402CS-2N2XJLW", Pads: []boardPad{
			rfTestPad("1", "ANT_FEED", 120, 0), // 120mil ≈ 3mm，λ/10 之内
		}},
	}
	d := rfScoreOf(t, comps, nil)
	if d.Score != 100 {
		t.Errorf("score = %.1f, want 100 (120mil feed is inside the λ/10 budget)", d.Score)
	}
	// 恒 degraded：keepout 覆盖需要 region 数据，快照里没有。一块天线下铺满铜的
	// 板在这一维照样可能满分，报告必须自己把这句话说出来。
	if d.Status != dimDegraded {
		t.Errorf("status = %q, want %q (keepout half is unmeasured)", d.Status, dimDegraded)
	}
	if !strings.Contains(d.Reason, "keepout") {
		t.Errorf("reason must name the unmeasured half, got %q", d.Reason)
	}
	if len(d.Contributors) != 0 {
		t.Errorf("nothing pulled this dimension down, got contributors %+v", d.Contributors)
	}
	if d.Metrics["keepoutChecked"] != 0 {
		t.Errorf("keepoutChecked = %v, want 0", d.Metrics["keepoutChecked"])
	}
	if d.Metrics["worstFeedLenMil"] != 120 {
		t.Errorf("worstFeedLenMil = %v, want 120", d.Metrics["worstFeedLenMil"])
	}
	if d.Metrics["rfParts"] != 1 || d.Metrics["feedsResolved"] != 1 {
		t.Errorf("metrics = %+v", d.Metrics)
	}
}

// ── 长馈线 = 重扣 + 归因到具体器件 ─────────────────────────────────────────

func TestRFScoreLongFeedPenalized(t *testing.T) {
	comps := []boardComp{
		{Designator: "ANT1", Device: "ANT-SMD-2450AT", Pads: []boardPad{
			rfTestPad("1", "RF_OUT", 0, 0),
			rfTestPad("2", "GND", 0, 50),
		}},
		{Designator: "U1", Device: "ESP32-C3FH4", Pads: []boardPad{
			rfTestPad("7", "RF_OUT", 2000, 0), // λ 那么远 → 扣满
		}},
	}
	d := rfScoreOf(t, comps, nil)
	if d.Score != 0 {
		t.Errorf("score = %.1f, want 0 (a full-wavelength feed is a wrecked RF path)", d.Score)
	}
	if len(d.Contributors) != 1 || d.Contributors[0].Designator != "ANT1" {
		t.Fatalf("contributors = %+v, want ANT1 alone", d.Contributors)
	}
	if d.Contributors[0].Penalty != 100 {
		t.Errorf("penalty = %.1f, want 100", d.Contributors[0].Penalty)
	}
	if d.Contributors[0].Detail == "" {
		t.Error("contributor must carry a human-readable detail")
	}
	var warn int
	for _, f := range d.Findings {
		if f.Type == "rf-feed-length" && f.Level == "WARN" {
			warn++
			if f.At == nil {
				t.Error("rf-feed-length must point at the feed pad")
			}
			if !strings.Contains(f.Message, "规范 §") {
				t.Errorf("finding lost its manual reference: %q", f.Message)
			}
		}
	}
	if warn != 1 {
		t.Errorf("rf-feed-length WARN count = %d, want 1 (%+v)", warn, d.Findings)
	}
}

// ── 陷阱回归：馈线网名撞上 isGlobalNet 也必须量得到 ─────────────────────────
//
// 这是本维最容易静默失效的一条路。layout-lint 的 ratsnest 用 isGlobalNet 滤网，
// 而那个正则宽到 `^[+-]` 开头就算全局网 —— 一个从 Altium/OrCAD 习惯导进来的
// `+RF` 馈线网会被整条吞掉。本维必须自己从 netPads 取网，不经过任何全局过滤。
func TestRFScoreGlobalNetNamedFeedStillMeasured(t *testing.T) {
	const feed = "+RF"
	if !isGlobalNet(feed) {
		t.Fatalf("测试前提失效：isGlobalNet(%q) 已经不再为 true，这条回归就失去意义了，"+
			"请换一个会被 isGlobalNet 吞掉的网名", feed)
	}
	comps := []boardComp{
		{Designator: "ANT1", Device: "2450AT18A100", Pads: []boardPad{rfTestPad("1", feed, 0, 0)}},
		// RF 源刻意不用 "ESP8266EX"：isAntennaDevice 的关键词表含 "ESP8266"，
		// 裸芯片会被一起判成天线，于是网上只剩天线、量不出馈线（见
		// TestRFScoreAllAntennaNetIsUnresolvable 把这条行为钉死）。
		{Designator: "U1", Device: "ESP32-C3FH4", Pads: []boardPad{rfTestPad("9", feed, 900, 0)}},
	}
	d := rfScoreOf(t, comps, nil)
	if d.Status == dimSkipped {
		t.Fatalf("馈线被全局网过滤吞掉了 —— 这一维瞎了：%q", d.Reason)
	}
	if d.Metrics["worstFeedLenMil"] != 900 {
		t.Errorf("worstFeedLenMil = %v, want 900", d.Metrics["worstFeedLenMil"])
	}
	if d.Score >= 100 {
		t.Errorf("score = %.1f, want a penalty for a 900mil feed", d.Score)
	}
}

// ── 地脚不是馈点 ────────────────────────────────────────────────────────────

func TestRFScoreGroundPadIsNotAFeed(t *testing.T) {
	// 天线的地脚离主 GND 铜（这里用 U1 的 GND 脚代表）很近，信号脚离 RF 源很远。
	// 若误把地脚当馈点，量出来会是那条很短的距离 → 假满分。
	comps := []boardComp{
		{Designator: "ANT1", Device: "ANT-SMD-2450", Pads: []boardPad{
			rfTestPad("1", "GND", 0, 0),
			rfTestPad("2", "RF_IN", 0, 40),
		}},
		{Designator: "U1", Device: "ESP32-C3FH4", Pads: []boardPad{
			rfTestPad("1", "GND", 10, 0),      // 10mil —— 误判成馈线就是这个数
			rfTestPad("9", "RF_IN", 1600, 40), // 真馈线
		}},
	}
	d := rfScoreOf(t, comps, nil)
	if d.Metrics["worstFeedLenMil"] != 1600 {
		t.Fatalf("worstFeedLenMil = %v, want 1600 (地脚被当成馈点了)", d.Metrics["worstFeedLenMil"])
	}
}

// ── 馈线没画 / 网上只有天线自己 → skipped，绝不是满分 ──────────────────────

func TestRFScoreUnresolvedFeedSkips(t *testing.T) {
	comps := []boardComp{
		// 信号脚有网，但全板同网只有它自己（原理图还没接）。
		{Designator: "ANT1", Device: "ANT-SMD-2450", Pads: []boardPad{
			rfTestPad("1", "RF_IN", 0, 0),
			rfTestPad("2", "GND", 0, 50),
		}},
		{Designator: "U1", Device: "ESP32-C3FH4", Pads: []boardPad{rfTestPad("1", "GND", 500, 0)}},
	}
	d := rfScoreOf(t, comps, nil)
	if d.Status != dimSkipped {
		t.Fatalf("status = %q, want %q — 量不到就不能给分", d.Status, dimSkipped)
	}
	if d.Score != 0 {
		t.Errorf("skipped dimension carries score %.1f", d.Score)
	}
	// 「板上有天线但它没接」这条信息必须保留下来，否则 skip 之后再没人看得见。
	if d.Metrics["rfParts"] != 1 || d.Metrics["feedsUnresolved"] != 1 {
		t.Errorf("metrics = %+v", d.Metrics)
	}
	found := false
	for _, f := range d.Findings {
		if f.Type == "rf-feed-unresolved" && f.Designator == "ANT1" {
			found = true
		}
	}
	if !found {
		t.Errorf("missing rf-feed-unresolved finding: %+v", d.Findings)
	}
}

// ── worst-case：一根坏馈线就能把整维打下来 ─────────────────────────────────

func TestRFScoreWorstAntennaDominates(t *testing.T) {
	comps := []boardComp{
		{Designator: "ANT1", Device: "ANT-SMD-A", Pads: []boardPad{rfTestPad("1", "RF_A", 0, 0)}},
		{Designator: "ANT2", Device: "ANT-SMD-B", Pads: []boardPad{rfTestPad("1", "RF_B", 0, 500)}},
		{Designator: "U1", Device: "ESP32-C3FH4", Pads: []boardPad{
			rfTestPad("1", "RF_A", 100, 0),    // 好：λ/10 内
			rfTestPad("2", "RF_B", 2000, 500), // 坏：λ
		}},
	}
	d := rfScoreOf(t, comps, nil)
	if d.Score != 0 {
		t.Errorf("score = %.1f, want 0 —— 平均会把坏馈线稀释掉", d.Score)
	}
	if len(d.Contributors) != 1 || d.Contributors[0].Designator != "ANT2" {
		t.Fatalf("contributors = %+v, want ANT2 alone (ANT1 是干净的)", d.Contributors)
	}
	if d.Metrics["feedLenMil"] != 2100 { // 100 + 2000，两根之和
		t.Errorf("feedLenMil = %v, want 2100", d.Metrics["feedLenMil"])
	}
}

// 归因必须按扣分降序（精修环靠这个梯度决定先动谁）。
func TestRFScoreContributorsSortedByPenalty(t *testing.T) {
	comps := []boardComp{
		{Designator: "ANT1", Device: "ANT-SMD-A", Pads: []boardPad{rfTestPad("1", "RF_A", 0, 0)}},
		{Designator: "ANT2", Device: "ANT-SMD-B", Pads: []boardPad{rfTestPad("1", "RF_B", 0, 500)}},
		{Designator: "U1", Device: "ESP32-C3FH4", Pads: []boardPad{
			rfTestPad("1", "RF_A", 400, 0),    // 轻扣
			rfTestPad("2", "RF_B", 1500, 500), // 重扣
		}},
	}
	d := rfScoreOf(t, comps, nil)
	if len(d.Contributors) != 2 {
		t.Fatalf("contributors = %+v, want 2", d.Contributors)
	}
	if d.Contributors[0].Designator != "ANT2" || d.Contributors[0].Penalty < d.Contributors[1].Penalty {
		t.Errorf("contributors not sorted by penalty desc: %+v", d.Contributors)
	}
}

// ── spec 声明优先 ───────────────────────────────────────────────────────────

// spec.rf.parts 是权威名单：它能圈中关键词表认不出的自研/冷门模组。
func TestRFScoreSpecPartsWinOverKeywords(t *testing.T) {
	comps := []boardComp{
		// 关键词表认不出这个器件名，只有 spec 知道它是 RF 模组。
		{Designator: "M1", Device: "RAK3172-SIP", Pads: []boardPad{rfTestPad("1", "LORA_RF", 0, 0)}},
		{Designator: "U1", Device: "STM32WLE5", Pads: []boardPad{rfTestPad("20", "LORA_RF", 1500, 0)}},
		// 名单外但长得像天线 → 出 INFO 提示 spec 可能漏写，但不参与打分。
		{Designator: "ANT9", Device: "ANT-SMD-2450", Pads: []boardPad{rfTestPad("1", "SPARE_RF", 0, 900)}},
	}
	s := &spec.Spec{RF: &spec.RF{Parts: []string{"M1"}}}
	d := rfScoreOf(t, comps, s)
	if d.Metrics["rfParts"] != 1 {
		t.Fatalf("rfParts = %v, want 1 (spec 名单只有 M1)", d.Metrics["rfParts"])
	}
	if d.Metrics["worstFeedLenMil"] != 1500 {
		t.Errorf("worstFeedLenMil = %v, want 1500", d.Metrics["worstFeedLenMil"])
	}
	if !strings.Contains(d.Reason, "spec") {
		t.Errorf("reason should record the detection source, got %q", d.Reason)
	}
	undeclared := false
	for _, f := range d.Findings {
		if f.Type == "rf-part-undeclared" && f.Designator == "ANT9" {
			undeclared = true
		}
	}
	if !undeclared {
		t.Errorf("名单外的天线件必须出 INFO 提示: %+v", d.Findings)
	}
}

// spec 声明的位号在板上一个都找不到（位号漂移 / 还没放件）：退回启发式，但 Reason
// 必须把这个不一致说出来 —— 静默退回会让人以为 spec 生效了。
func TestRFScoreSpecPartsUnmatchedFallsBack(t *testing.T) {
	comps := []boardComp{
		{Designator: "ANT1", Device: "ANT-SMD-2450", Pads: []boardPad{rfTestPad("1", "RF_IN", 0, 0)}},
		{Designator: "U1", Device: "ESP32-C3FH4", Pads: []boardPad{rfTestPad("9", "RF_IN", 300, 0)}},
	}
	s := &spec.Spec{RF: &spec.RF{Parts: []string{"M77"}}}
	d := rfScoreOf(t, comps, s)
	if d.Status == dimSkipped {
		t.Fatalf("spec 名单对不上时应退回启发式，而不是整维跳过：%q", d.Reason)
	}
	if !strings.Contains(d.Reason, "M77") {
		t.Errorf("reason must surface the unmatched spec entry, got %q", d.Reason)
	}
}

// spec 名单对不上、启发式也找不到天线 → skipped，理由要点名是 spec 对不上，
// 而不是笼统的「板上没有 RF」（后者会误导人以为 spec 写对了）。
func TestRFScoreSpecPartsUnmatchedAndNoAntenna(t *testing.T) {
	comps := []boardComp{
		{Designator: "R1", Device: "0402WGF", Pads: []boardPad{rfTestPad("1", "N1", 0, 0)}},
	}
	s := &spec.Spec{RF: &spec.RF{Parts: []string{"M77"}}}
	d := rfScoreOf(t, comps, s)
	if d.Status != dimSkipped {
		t.Fatalf("status = %q, want %q", d.Status, dimSkipped)
	}
	if !strings.Contains(d.Reason, "M77") {
		t.Errorf("reason = %q, want it to name the unmatched spec entry", d.Reason)
	}
}

// spec.rf.feed 直接指定馈线网：绕开一切启发式，哪怕它不是天线上最短的那条网。
func TestRFScoreSpecFeedNetWins(t *testing.T) {
	comps := []boardComp{
		{Designator: "ANT1", Device: "ANT-SMD-2450", Pads: []boardPad{
			rfTestPad("1", "AUX_SENSE", 0, 0), // 更近，但不是馈线
			rfTestPad("2", "RF_MAIN", 0, 100),
		}},
		{Designator: "U1", Device: "ESP32-C3FH4", Pads: []boardPad{
			rfTestPad("1", "AUX_SENSE", 50, 0),
			rfTestPad("9", "RF_MAIN", 1000, 100),
		}},
	}
	if got := rfScoreOf(t, comps, nil).Metrics["worstFeedLenMil"]; got != 50 {
		t.Fatalf("测试前提失效：不给 spec 时应挑到最短的 50mil，实得 %v", got)
	}
	s := &spec.Spec{RF: &spec.RF{Parts: []string{"ANT1"}, Feed: "RF_MAIN", KeepoutLayers: "all"}}
	d := rfScoreOf(t, comps, s)
	if d.Metrics["worstFeedLenMil"] != 1000 {
		t.Errorf("worstFeedLenMil = %v, want 1000 (spec.rf.feed 指定的网)", d.Metrics["worstFeedLenMil"])
	}
	// 用户声明了 keepout 层意图而我们没验，必须如实转述而不是假装验过。
	if !strings.Contains(d.Reason, "keepoutLayers") {
		t.Errorf("reason should relay the unverified keepout declaration, got %q", d.Reason)
	}
}

// ── 另一根天线不能当 RF 源 ──────────────────────────────────────────────────

func TestRFScoreOtherAntennaIsNotAnRFSource(t *testing.T) {
	// 两根天线共网（分集天线常见接法）：若把对方当 RF 源，量出来是它们之间的
	// 距离，跟"到芯片有多远"完全无关。
	comps := []boardComp{
		{Designator: "ANT1", Device: "ANT-SMD-2450", Pads: []boardPad{rfTestPad("1", "RF_IN", 0, 0)}},
		{Designator: "ANT2", Device: "ANT-SMD-2450", Pads: []boardPad{rfTestPad("1", "RF_IN", 60, 0)}},
		{Designator: "U1", Device: "ESP32-C3FH4", Pads: []boardPad{rfTestPad("9", "RF_IN", 800, 0)}},
	}
	d := rfScoreOf(t, comps, nil)
	if d.Metrics["worstFeedLenMil"] != 800 {
		t.Errorf("worstFeedLenMil = %v, want 800 (另一根天线被当成 RF 源了)", d.Metrics["worstFeedLenMil"])
	}
}

// 已知边界：isAntennaDevice 的关键词表按**模组**名写的（WROOM/ESP8266/…），裸芯片
// 名撞上同样的串时会被一起判成天线。此时馈线网上全是"天线"，没有可当 RF 源的对端 →
// 本维 skipped 而不是瞎给分。这条不是缺陷掩盖，是把已知盲区钉在测试里：真要修得去
// 改 isAntennaDevice（pcb check 与本维共用的判据），别在本维偷偷放宽。
func TestRFScoreAllAntennaNetIsUnresolvable(t *testing.T) {
	comps := []boardComp{
		{Designator: "ANT1", Device: "2450AT18A100", Pads: []boardPad{rfTestPad("1", "RF_IN", 0, 0)}},
		{Designator: "U1", Device: "ESP8266EX", Pads: []boardPad{rfTestPad("9", "RF_IN", 900, 0)}},
	}
	d := rfScoreOf(t, comps, nil)
	if d.Status != dimSkipped {
		t.Fatalf("status = %q, want %q", d.Status, dimSkipped)
	}
	// 显式声明可以救回来：spec 说了只有 ANT1 是天线，U1 就重新成为合法 RF 源。
	s := &spec.Spec{RF: &spec.RF{Parts: []string{"ANT1"}}}
	if got := rfScoreOf(t, comps, s).Metrics["worstFeedLenMil"]; got != 900 {
		t.Errorf("worstFeedLenMil = %v, want 900 (spec 名单应能纠正关键词过宽)", got)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

func TestRFPadOwner(t *testing.T) {
	// netPads() 把 owner 编码进 Number（"U1.3"），这里做反向还原。
	if d, p := padOwner("U1.3"); d != "U1" || p != "3" {
		t.Errorf("padOwner(\"U1.3\") = %q,%q", d, p)
	}
	// 引脚号本身带点（少见但不是不可能）时，只切第一刀。
	if d, p := padOwner("J1.A.1"); d != "J1" || p != "A.1" {
		t.Errorf("padOwner(\"J1.A.1\") = %q,%q", d, p)
	}
	if d, p := padOwner("U1"); d != "U1" || p != "" {
		t.Errorf("padOwner(\"U1\") = %q,%q", d, p)
	}
}

func TestRFIsGroundNet(t *testing.T) {
	for _, n := range []string{"GND", "AGND", "gnd_1", "DGND", "EARTH", "VSS", "VSS_A", "PGND"} {
		if !rfIsGroundNet(n) {
			t.Errorf("rfIsGroundNet(%q) = false", n)
		}
	}
	// 关键的负向断言：馈线常用名一个都不能被当成地。尤其 `+RF` / `VBAT_RF`
	// 这类会被 isGlobalNet 吞掉的网名，在这里必须活下来。
	for _, n := range []string{"", "RF_IN", "ANT", "ANT_FEED", "+RF", "VBAT_RF", "LORA_RF"} {
		if rfIsGroundNet(n) {
			t.Errorf("rfIsGroundNet(%q) = true —— 馈点会被判丢", n)
		}
	}
}

func TestRFPartMatches(t *testing.T) {
	c := boardComp{Designator: "M1", Device: "RAK3172-SIP"}
	for _, w := range []string{"M1", "m1", "RAK3172", "rak3172-sip"} {
		if !rfPartMatches(c, w) {
			t.Errorf("rfPartMatches(%q) = false", w)
		}
	}
	// 短串只允许位号全等 —— 否则 "U" 之类的写法会把满板器件圈进来。
	for _, w := range []string{"", "M", "R", "U1", "ESP32"} {
		if rfPartMatches(c, w) {
			t.Errorf("rfPartMatches(%q) = true", w)
		}
	}
	// Device 为空时退回 Name（placed 件的 name 常是模板串，所以顺序不能反）。
	if !rfPartMatches(boardComp{Designator: "ANT1", Name: "ANT-SMD-2450AT"}, "ANT-SMD") {
		t.Error("device 为空时应退回 name 匹配")
	}
}

// 注册表里必须有这一维，否则 analyzeLayoutScore 会把它报成 "not implemented yet"。
func TestRFScorerRegistered(t *testing.T) {
	if s := scorerFor(dimRF); s == nil {
		t.Fatal("rf dimension not registered")
	}
}
