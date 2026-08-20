package app

// sch_place_adopt_seq_test.go — 两级新鲜度判定(算术档 / 探针档)的对照组。
//
// 这些用例守的是一句话:**「那里什么都没有」只有先被证明新鲜,才准被读成
// 「什么都没发生」**。算术档把这个证明从启发式升成计数;探针档是老连接器下的
// 降级路径,必须自曝弱证据、且绝不默认新鲜。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func seqCounters(seq, abandoned int, ids ...string) schSeqCounters {
	return schSeqCounters{Known: true, Seq: seq, SeqAbandoned: abandoned, AbandonedIDs: ids}
}

// ── 正对照:算术档证明「确实没落地」 ────────────────────────────────────
//
// seqAbandoned 没变 → 连接器 FIFO 保证这次回读的 handler 在那次 place 的 handler
// settle 之后才开跑 → 「那里没有新器件」是真的没有。
//
// 关键收益:**一个探针都没有也能出结论**。这正是 anchor-first(第一件就超时)
// 那条边界 —— 探针档在那里结构上无解,算术档能解。
func TestSchAdoptSeqProvenFreshConcludesWithoutAnyProbe(t *testing.T) {
	live := []layoutComp{adoptComp("old-1", "R9", "part", 100, 100)}
	ev := schSeqEvidence{Base: seqCounters(7, 0), Read: seqCounters(9, 0)}

	v := schAdoptOrphanPlacement(map[string]bool{"old-1": true}, nil, ev, live,
		schAdoptRequest{Designator: "U3", X: 690, Y: 460})

	if !v.Fresh || v.Uncertain {
		t.Fatalf("verdict=%+v, want a PROVEN-fresh negative even with zero probes", v)
	}
	if v.Tier != schFreshProven {
		t.Fatalf("tier=%q, want %q", v.Tier, schFreshProven)
	}
	if !strings.Contains(v.Reason, "确实没有落地") {
		t.Fatalf("reason must state the place did not land: %q", v.Reason)
	}
	if !strings.Contains(v.Reason, "强证据") {
		t.Fatalf("a proven verdict must say so, so a weak one can be told apart: %q", v.Reason)
	}
	// 诚实性:算术只证明到 handler 边界,报文必须自己划出这条线。
	if !strings.Contains(v.Reason, "尚未提交的写") {
		t.Fatalf("the proven wording must still disclaim the commit boundary: %q", v.Reason)
	}
	if !strings.Contains(v.Evidence, "seqAbandoned") {
		t.Fatalf("evidence must cite the counters: %q", v.Evidence)
	}
}

// ── 负对照 A:seqAbandoned 递增 → 必须 uncertain ─────────────────────────
//
// 连接器放弃过动作 = 有一个 handler 被丢下了但**还在跑**,它的效果可能稍后才落地。
// 此时「那里没有新器件」什么都不证明,哪怕探针全员到齐。
func TestSchAdoptSeqAbandonedForcesUncertainEvenWithProbesPresent(t *testing.T) {
	live := []layoutComp{
		adoptComp("old-1", "R9", "part", 100, 100),
		adoptComp("u3-id", "U3", "part", 690, 460), // 探针到齐 —— 探针档会说"新鲜"
	}
	ev := schSeqEvidence{Base: seqCounters(7, 0), Read: seqCounters(9, 1, "req-place-c8")}

	v := schAdoptOrphanPlacement(map[string]bool{"old-1": true, "u3-id": true},
		[]string{"u3-id"}, ev, live, schAdoptRequest{Designator: "C8", X: 440, Y: 535})

	if !v.Uncertain || v.Fresh {
		t.Fatalf("verdict=%+v, want uncertain — an abandoned handler voids every ordering claim", v)
	}
	for _, forbidden := range []string{"确实没有落地", "没有本次留下的残件"} {
		if strings.Contains(v.Reason, forbidden) {
			t.Fatalf("reason must never claim the place did not land: %q", v.Reason)
		}
	}
	if !strings.Contains(v.Reason, "无法判断") {
		t.Fatalf("reason must say it cannot judge: %q", v.Reason)
	}
	if !strings.Contains(v.Reason, "req-place-c8") {
		t.Fatalf("reason must NAME the abandoned request, not only count it: %q", v.Reason)
	}
	if !strings.Contains(v.Reason, "效果可能稍后才落地") {
		t.Fatalf("reason must explain WHY abandonment voids the claim: %q", v.Reason)
	}
}

// 放弃过 + 坐标上确实认出了新器件 → 照常收编。命中本身就是它存在的证明,
// 与顺序无关;把这条也拖进 uncertain 等于扔掉唯一能拿到的残件句柄。
func TestSchAdoptSeqAbandonedStillAdoptsAVisibleOrphan(t *testing.T) {
	live := []layoutComp{adoptComp("ghost-1", "C8", "part", 440, 535)}
	ev := schSeqEvidence{Base: seqCounters(7, 0), Read: seqCounters(9, 2, "req-x")}
	v := schAdoptOrphanPlacement(map[string]bool{"old-1": true}, nil, ev, live,
		schAdoptRequest{Designator: "C8", X: 440, Y: 535})
	if v.Adopted == nil || v.Adopted.ID != "ghost-1" || v.Uncertain {
		t.Fatalf("verdict=%+v, want the visible orphan adopted regardless of abandonment", v)
	}
}

// ── 负对照 B:老连接器(响应不带 seq)→ 退回探针档,措辞降级,绝不默认新鲜 ──
func TestSchAdoptSeqAbsentFallsBackToProbeTierAndNeverDefaultsFresh(t *testing.T) {
	live := []layoutComp{adoptComp("old-1", "R9", "part", 100, 100)}

	// B-1:没有探针 → 什么都证明不了。**绝不许**因为"连接器没给字段"就放行。
	noProbe := schAdoptOrphanPlacement(map[string]bool{"old-1": true}, nil, schSeqEvidence{}, live,
		schAdoptRequest{Designator: "U3", X: 690, Y: 460})
	if noProbe.Fresh || !noProbe.Uncertain {
		t.Fatalf("verdict=%+v, want uncertain: absent seq fields are NOT evidence of freshness", noProbe)
	}
	if noProbe.Tier != schFreshProbe {
		t.Fatalf("tier=%q, want %q", noProbe.Tier, schFreshProbe)
	}
	if !strings.Contains(noProbe.Reason, "弱证据") {
		t.Fatalf("the downgraded tier must be stated in the report: %q", noProbe.Reason)
	}
	if !strings.Contains(noProbe.Reason, "不支持顺序证明") {
		t.Fatalf("the report must name the cause (old connector): %q", noProbe.Reason)
	}
	if !strings.Contains(noProbe.Reason, ".eext") || !strings.Contains(noProbe.Reason, "重启 EasyEDA") {
		t.Fatalf("the report must give a runnable upgrade step: %q", noProbe.Reason)
	}

	// B-2:探针到齐 → 仍可下结论,但措辞必须是**弱证据**,不得与算术档长得一样。
	withProbe := schAdoptOrphanPlacement(
		map[string]bool{"old-1": true, "u3-id": true}, []string{"u3-id"}, schSeqEvidence{},
		[]layoutComp{live[0], adoptComp("u3-id", "U3", "part", 690, 460)},
		schAdoptRequest{Designator: "C8", X: 440, Y: 535})
	if !withProbe.Fresh || withProbe.Uncertain {
		t.Fatalf("verdict=%+v, want the probe heuristic to still conclude", withProbe)
	}
	if withProbe.Tier != schFreshProbe {
		t.Fatalf("tier=%q, want %q", withProbe.Tier, schFreshProbe)
	}
	if !strings.Contains(withProbe.Reason, "弱证据") || strings.Contains(withProbe.Reason, "强证据") {
		t.Fatalf("a probe-tier conclusion must be labelled weak: %q", withProbe.Reason)
	}
}

// 半套字段也不算数:只有 seq 没有 seqAbandoned(或反过来)时算术不成立。
func TestSchAdoptSeqHalfSetIsNotEvidence(t *testing.T) {
	body := []byte(`{"ok":true,"seq":3}`)
	if c := parseSeqCounters(body); c.Known {
		t.Fatalf("counters=%+v, want Known=false when seqAbandoned is missing", c)
	}
	// 而字段存在且为 0 必须与"缺席"可分 —— 这是指针语义的全部意义。
	zero := parseSeqCounters([]byte(`{"ok":true,"seq":0,"seqAbandoned":0}`))
	if !zero.Known || zero.Seq != 0 || zero.SeqAbandoned != 0 {
		t.Fatalf("counters=%+v, want an explicit zero to be Known", zero)
	}
	absent := parseSeqCounters([]byte(`{"ok":true}`))
	if absent.Known {
		t.Fatalf("counters=%+v, want absent fields to stay unknown (never read as 0)", absent)
	}
}

// ── 负对照 C:unordered 响应不得被当成新鲜度证据 ─────────────────────────
//
// 旁路通道让 wedge 期仍可观测,代价是它的 seq 与 FIFO 无关。把它当证据 = 用一个
// 与顺序无关的数字去证明顺序。
func TestSchAdoptSeqUnorderedReadIsNeverFreshnessEvidence(t *testing.T) {
	live := []layoutComp{adoptComp("old-1", "R9", "part", 100, 100)}
	read := seqCounters(9, 0)
	read.Unordered = true
	ev := schSeqEvidence{Base: seqCounters(7, 0), Read: read}

	v := schAdoptOrphanPlacement(map[string]bool{"old-1": true}, nil, ev, live,
		schAdoptRequest{Designator: "U3", X: 690, Y: 460})

	if v.Fresh || !v.Uncertain {
		t.Fatalf("verdict=%+v, want uncertain — a bypass response proves no ordering", v)
	}
	if v.Tier != schFreshProbe {
		t.Fatalf("tier=%q, want the arithmetic tier refused and the probe tier used", v.Tier)
	}
	if !strings.Contains(v.Reason, "旁路") || !strings.Contains(v.Reason, "unordered") {
		t.Fatalf("the report must name the bypass as the reason for the downgrade: %q", v.Reason)
	}
}

// ── 计数器倒退 = 连接器中途重连过,队列与计数器一起重置 → 弃证 ───────────
func TestSchAdoptSeqResetIsNotProof(t *testing.T) {
	live := []layoutComp{
		adoptComp("old-1", "R9", "part", 100, 100),
		adoptComp("u3-id", "U3", "part", 690, 460), // 探针到齐,但序号说不通
	}
	// 重连后新队列从 0 起数:seq 没有前进,seqAbandoned 也回到 0。
	ev := schSeqEvidence{Base: seqCounters(12, 3), Read: seqCounters(1, 0)}
	v := schAdoptOrphanPlacement(map[string]bool{"old-1": true, "u3-id": true},
		[]string{"u3-id"}, ev, live, schAdoptRequest{Designator: "C8", X: 440, Y: 535})

	if v.Fresh || !v.Uncertain {
		t.Fatalf("verdict=%+v, want uncertain — a counter reset means there is no shared timeline", v)
	}
	if v.Tier != schFreshNone {
		t.Fatalf("tier=%q, want %q", v.Tier, schFreshNone)
	}
	if !strings.Contains(v.Reason, "重连") {
		t.Fatalf("reason must name the reset: %q", v.Reason)
	}
}

// ── 两个证据打架 → 降级,不放行 ─────────────────────────────────────────
//
// 序号连续只证明先后,不证明这一读没漏器件。算术说新鲜、探针缺席 = 平台把一个
// 已知存在的器件漏在 components.list 之外(另一类 bug)。诚实的答案是「不知道」。
func TestSchAdoptSeqProvenButProbeMissingIsDowngraded(t *testing.T) {
	stale := []layoutComp{adoptComp("old-1", "R9", "part", 100, 100)} // 丢了 u3-id
	ev := schSeqEvidence{Base: seqCounters(7, 0), Read: seqCounters(9, 0)}
	v := schAdoptOrphanPlacement(map[string]bool{"old-1": true, "u3-id": true},
		[]string{"u3-id"}, ev, stale, schAdoptRequest{Designator: "C8", X: 440, Y: 535})

	if v.Fresh || !v.Uncertain {
		t.Fatalf("verdict=%+v, want uncertain when the two evidences disagree", v)
	}
	if v.Tier != schFreshNone {
		t.Fatalf("tier=%q, want %q", v.Tier, schFreshNone)
	}
	if !strings.Contains(v.Reason, "打架") || !strings.Contains(v.Reason, "u3-id") {
		t.Fatalf("reason must state the contradiction and name the missing probe: %q", v.Reason)
	}
}

// 证据档必须出现在报文里,弱证据档必须带升级路径。
func TestSchAdoptTierNoticeStatesTheEvidenceTier(t *testing.T) {
	var proven bytes.Buffer
	schAdoptTierNotice(&proven, schAdoptVerdict{Tier: schFreshProven, Evidence: "顺序可证:… seqAbandoned 始终为 0"})
	if !strings.Contains(proven.String(), "可证") {
		t.Fatalf("proven tier must be stated: %q", proven.String())
	}
	var weak bytes.Buffer
	schAdoptTierNotice(&weak, schAdoptVerdict{Tier: schFreshProbe, Evidence: schAdoptProbeTierNote(schSeqEvidence{}, nil)})
	out := weak.String()
	if !strings.Contains(out, "弱") || !strings.Contains(out, ".eext") {
		t.Fatalf("weak tier must be stated WITH the upgrade step: %q", out)
	}
	var silent bytes.Buffer
	schAdoptTierNotice(&silent, schAdoptVerdict{})
	if silent.Len() != 0 {
		t.Fatalf("no evidence → no noise: %q", silent.String())
	}
}

// ── 接线级:基线从派发咽喉记下来,回读响应带回计数器 → 端到端出可证结论 ──
//
// 纯判定测不到这一段:基线取早了/取晚了都会让算术退化(取晚一步就是拿回读自己
// 当基线,恒等式必然成立 —— 一个永远说"新鲜"的假门)。

// newSeqAdoptDaemon 是带顺序计数器的假 daemon:每次 components.list 都回一份
// 指定的器件表 + 指定的 seq/seqAbandoned。
func newSeqAdoptDaemon(t *testing.T, comps []map[string]any, seq, abandoned int) (*appConfig, func()) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"service":"easyeda-agent","windows":[]}`))
			return
		}
		calls++
		list := make([]any, 0, len(comps))
		for _, c := range comps {
			list = append(list, c)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":           true,
			"result":       map[string]any{"components": list},
			"seq":          seq + calls,
			"seqAbandoned": abandoned,
		})
	}))
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, _ := strings.Cut(hostPort, ":")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return &appConfig{host: host, ports: fmt.Sprintf("%d-%d", port, port)}, srv.Close
}

func TestBapAdoptAfterPlaceFailureUsesTheArithmeticTierEndToEnd(t *testing.T) {
	connSeqReset()
	// 基线:失败那次 place **之前**最后一条带顺序证据的响应。
	connSeqObserve("", "", []byte(`{"ok":true,"seq":7,"seqAbandoned":0}`))

	// 回读:那里确实什么都没有,而且一个探针都没有(anchor-first 那条边界)。
	cfg, cleanup := newSeqAdoptDaemon(t, []map[string]any{adoptFrameComp("old-1", "R9", 100, 100)}, 7, 0)
	defer cleanup()

	var errOut bytes.Buffer
	placement, uncertain, adopted := bapAdoptAfterPlaceFailure(cfg, "",
		map[string]bool{"old-1": true}, nil,
		bapPlacement{Designator: "U3", X: 690, Y: 460}, &errOut)

	if placement != nil || len(adopted) != 0 || len(uncertain) != 0 {
		t.Fatalf("placement=%+v uncertain=%v adopted=%v, want a clean PROVEN negative", placement, uncertain, adopted)
	}
	out := errOut.String()
	if !strings.Contains(out, "adopt ✓") || !strings.Contains(out, "确实没有落地") {
		t.Fatalf("the arithmetic tier must conclude with no probes at all:\n%s", out)
	}
	if !strings.Contains(out, "证据档:可证") {
		t.Fatalf("the report must state which evidence tier was used:\n%s", out)
	}
}

func TestBapAdoptAfterPlaceFailureUncertainWhenTheConnectorAbandonedSomething(t *testing.T) {
	connSeqReset()
	connSeqObserve("", "", []byte(`{"ok":true,"seq":7,"seqAbandoned":0}`))

	// 回读带 seqAbandoned=1:这中间连接器丢下过一个还在跑的 handler。
	cfg, cleanup := newSeqAdoptDaemon(t, []map[string]any{adoptFrameComp("old-1", "R9", 100, 100)}, 7, 1)
	defer cleanup()

	var errOut bytes.Buffer
	placement, uncertain, adopted := bapAdoptAfterPlaceFailure(cfg, "",
		map[string]bool{"old-1": true}, nil,
		bapPlacement{Designator: "U3", X: 690, Y: 460}, &errOut)

	if placement != nil || len(adopted) != 0 {
		t.Fatalf("placement=%+v adopted=%v, want nothing claimed", placement, adopted)
	}
	if len(uncertain) != 1 {
		t.Fatalf("uncertain=%v, want the uncertainty carried into the rollback report", uncertain)
	}
	out := errOut.String()
	if strings.Contains(out, "确实没有落地") {
		t.Fatalf("an abandoned handler must never yield the confident wording:\n%s", out)
	}
	if !strings.Contains(out, "adopt ?") {
		t.Fatalf("uncertain must be marked as such on stderr:\n%s", out)
	}
	for _, want := range []string{"easyeda sch save", "easyeda sch list", "prim-delete"} {
		if !strings.Contains(out, want) {
			t.Fatalf("guidance must stay runnable, got:\n%s", out)
		}
	}
	connSeqReset()
}

// 基线桶按窗口分,且只记 ordered 响应 —— 旁路响应的 seq 不该变成任何判定的基线。
func TestConnSeqObserveIgnoresUnorderedAndBucketsByWindow(t *testing.T) {
	connSeqReset()
	defer connSeqReset()

	connSeqObserve("win-a", "", []byte(`{"ok":true,"seq":4,"seqAbandoned":1}`))
	connSeqObserve("win-a", "", []byte(`{"ok":true,"seq":99,"seqAbandoned":9,"unordered":true}`))
	if got := connSeqSnapshot("win-a", ""); !got.Known || got.Seq != 4 || got.SeqAbandoned != 1 {
		t.Fatalf("snapshot=%+v, want the bypass response ignored as a baseline", got)
	}
	if got := connSeqSnapshot("win-b", ""); got.Known {
		t.Fatalf("snapshot=%+v, want per-window buckets", got)
	}
	connSeqObserve("", "proj-x", []byte(`{"ok":true,"seq":2,"seqAbandoned":0}`))
	if got := connSeqSnapshot("", "proj-x"); !got.Known || got.Seq != 2 {
		t.Fatalf("snapshot=%+v, want the project hint to key its own bucket", got)
	}
}
