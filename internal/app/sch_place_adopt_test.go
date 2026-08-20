package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func adoptComp(id, designator, typ string, x, y float64) layoutComp {
	return layoutComp{ID: id, Designator: designator, ComponentType: typ, X: x, Y: y, AnchorAvailable: true}
}

func TestSchAdoptOrphanPlacementAdoptsTheNewPartAtTheRequestedSpot(t *testing.T) {
	known := map[string]bool{"old-1": true}
	live := []layoutComp{
		adoptComp("old-1", "R9", "part", 400, 300),
		adoptComp("ghost-1", "U2", "part", 400, 300),
	}
	v := schAdoptOrphanPlacement(known, nil, live, schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted == nil || v.Adopted.ID != "ghost-1" {
		t.Fatalf("adopted=%+v, want ghost-1", v.Adopted)
	}
	if !reflect.DeepEqual(v.CandidateIDs(), []string{"ghost-1"}) {
		t.Fatalf("candidates=%v", v.CandidateIDs())
	}
	if !strings.Contains(v.Reason, "ghost-1") {
		t.Fatalf("reason must name the adopted id: %q", v.Reason)
	}
}

// 负对照 A(**门⓪不许退化成"永远保守"**):回读被证明新鲜(探针 placed-1 在读回
// 结果里),而下发坐标那里确实什么都没有 —— 必须照常给出「确实没有落地」的定论。
// 加了新鲜度门之后如果这条转红,说明修复退化成了「永远 uncertain」的假修复。
func TestSchAdoptOrphanPlacementNeverInventsAnIDWhenNothingLanded(t *testing.T) {
	known := map[string]bool{"old-1": true, "placed-1": true}
	live := []layoutComp{
		adoptComp("old-1", "R9", "part", 400, 300),
		adoptComp("placed-1", "U1", "part", 700, 460), // 本命令此前落地的件 = 探针
	}
	v := schAdoptOrphanPlacement(known, []string{"placed-1"}, live,
		schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted != nil || len(v.Candidates) != 0 {
		t.Fatalf("verdict=%+v, want nothing adopted and nothing named", v)
	}
	if !v.Fresh || v.Uncertain {
		t.Fatalf("verdict=%+v, want a PROVEN-fresh negative, not uncertain", v)
	}
	if !strings.Contains(v.Reason, "没有落地") {
		t.Fatalf("reason must state the place did not land: %q", v.Reason)
	}
}

// 负对照 B:同坐标、同类型、**页面上早就有**的实例 —— 快照里有它,所以它永远
// 不会被认成本次的产物。这是「不许误删已有同型器件」唯一的机械保证。
// 探针到齐(回读新鲜),所以这条验的是门①本身,而不是被 uncertain 顺带挡住。
func TestSchAdoptOrphanPlacementNeverAdoptsAPreexistingTwin(t *testing.T) {
	twin := adoptComp("preexisting", "U2", "part", 400, 300)
	probe := adoptComp("placed-1", "U1", "part", 700, 460)
	known := map[string]bool{"preexisting": true, "placed-1": true}
	v := schAdoptOrphanPlacement(known, []string{"placed-1"}, []layoutComp{twin, probe},
		schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted != nil || len(v.Candidates) != 0 {
		t.Fatalf("verdict=%+v, want the pre-existing twin left alone", v)
	}
	if !v.Fresh {
		t.Fatalf("verdict=%+v, want the read proven fresh so gate ① is what did the work", v)
	}
}

// 负对照 C:快照缺失时上层必须整个关掉收编。这里钉的是「known 为空集 ≠ 快照缺失」
// —— 空集是合法的空白页快照,缺失由调用方用 nil 表达(见 bapAdoptAfterPlaceFailure)。
func TestSchAdoptOrphanPlacementEmptySnapshotStillMeansEverythingIsNew(t *testing.T) {
	v := schAdoptOrphanPlacement(map[string]bool{}, nil,
		[]layoutComp{adoptComp("ghost-1", "U2", "part", 400, 300)},
		schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted == nil || v.Adopted.ID != "ghost-1" {
		t.Fatalf("verdict=%+v, want ghost-1 adopted on an empty page", v)
	}
}

func TestSchAdoptOrphanPlacementRejectsNonPartsAndDistantParts(t *testing.T) {
	live := []layoutComp{
		adoptComp("flag-1", "", "netflag", 400, 300),  // 门②:标志不是放置件
		adoptComp("sheet-1", "", "sheet", 400, 300),   // 门②:图框绝不能被删
		adoptComp("far-1", "R7", "part", 460, 300),    // 门③:不在下发坐标附近
		{ID: "noxy-1", ComponentType: "part"},         // 无可信坐标 → 不猜
		adoptComp("placed-1", "U1", "part", 700, 460), // 探针到齐 → 回读新鲜
	}
	v := schAdoptOrphanPlacement(map[string]bool{}, []string{"placed-1"}, live,
		schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted != nil || len(v.Candidates) != 0 {
		t.Fatalf("verdict=%+v, want no adoption", v)
	}
	if !v.Fresh || v.Uncertain {
		t.Fatalf("verdict=%+v, want gates ②③ judged on a proven-fresh read", v)
	}
}

func TestSchAdoptOrphanPlacementNamesEveryCandidateWhenAmbiguous(t *testing.T) {
	live := []layoutComp{
		adoptComp("ghost-b", "U2", "part", 402, 301),
		adoptComp("ghost-a", "U2", "part", 400, 300),
	}
	v := schAdoptOrphanPlacement(map[string]bool{}, nil, live, schAdoptRequest{Designator: "U2", X: 400, Y: 300})
	if v.Adopted != nil {
		t.Fatalf("ambiguous match must not be adopted: %+v", v.Adopted)
	}
	if got := v.CandidateIDs(); !reflect.DeepEqual(got, []string{"ghost-a", "ghost-b"}) {
		t.Fatalf("candidates=%v, want both ghosts named", got)
	}
}

func TestSchAdoptOrphanPlacementToleranceIsOneGridStep(t *testing.T) {
	inside := adoptComp("in", "U2", "part", 400+schAdoptTolerance, 300)
	outside := adoptComp("out", "U2", "part", 400+schAdoptTolerance+0.5, 300)
	if v := schAdoptOrphanPlacement(map[string]bool{}, nil, []layoutComp{inside},
		schAdoptRequest{X: 400, Y: 300}); v.Adopted == nil {
		t.Fatal("a part exactly at the tolerance edge must be adopted")
	}
	if v := schAdoptOrphanPlacement(map[string]bool{}, nil, []layoutComp{outside},
		schAdoptRequest{X: 400, Y: 300}); v.Adopted != nil {
		t.Fatal("a part past the tolerance must not be adopted")
	}
}

func TestSchPageComponentSnapshotKeepsEveryTypeAndSkipsBlankIDs(t *testing.T) {
	snap := schPageComponentSnapshot([]layoutComp{
		adoptComp("c1", "R1", "part", 0, 0),
		adoptComp("f1", "", "netflag", 0, 0),
		{ComponentType: "part"},
	})
	if len(snap) != 2 || !snap["c1"] || !snap["f1"] {
		t.Fatalf("snapshot=%v, want every id-bearing component of any type", snap)
	}
}

// ── 门⓪:新鲜度证明 ────────────────────────────────────────────────────────
//
// 正对照 = 真机 2026-08-20 那一幕的最小复现(block.ch340c_usb_serial):
// U3 先落地拿到 id;place C8 超时;收编回读交回一份**还没包含 C8、连 U3 也丢了**
// 的旧页面。地面真相是 C8 就在 (440,535)。首版据此报「确实没有落地,页面上没有
// 本次留下的残件」—— 在它唯一该起作用的场景里说反话,而且说得很自信。
func TestSchAdoptOrphanPlacementRefusesToConcludeFromAStaleRead(t *testing.T) {
	// 地面真相:C8 已经落在 (440,535)(测完由用户清残件时读到 5e5803d829b1985d)。
	// 但这次回读是 stale 的:既没有 C8,也丢了本命令此前落地的 U3。
	stale := []layoutComp{adoptComp("old-1", "R9", "part", 100, 100)}
	v := schAdoptOrphanPlacement(map[string]bool{"old-1": true}, []string{"u3-id"}, stale,
		schAdoptRequest{Designator: "C8", X: 440, Y: 535})

	if !v.Uncertain {
		t.Fatalf("verdict=%+v, want uncertain — a read that lost a known-present id proves nothing", v)
	}
	if v.Fresh {
		t.Fatalf("verdict=%+v, want Fresh=false", v)
	}
	if v.Adopted != nil || len(v.Candidates) != 0 {
		t.Fatalf("verdict=%+v, want nothing adopted/named from an untrustworthy read", v)
	}
	if !reflect.DeepEqual(v.MissingProbes, []string{"u3-id"}) {
		t.Fatalf("missingProbes=%v, want the absent probe named as the evidence", v.MissingProbes)
	}
	// 措辞是这条 bug 的本体:绝不许再出现「确实没有落地 / 没有残件」。
	for _, forbidden := range []string{"确实没有落地", "没有本次留下的残件", "确实没落地"} {
		if strings.Contains(v.Reason, forbidden) {
			t.Fatalf("reason must not claim the place did not land: %q", v.Reason)
		}
	}
	if !strings.Contains(v.Reason, "无法判断") {
		t.Fatalf("reason must say it cannot judge: %q", v.Reason)
	}
}

// 探针缺席但坐标上**确实**认出了新器件 → 照常收编。命中本身就是回读足够新鲜的
// 证明(那个件在快照时刻还不存在),门⓪不该把这条也拖进 uncertain —— 拖进去就
// 等于放弃唯一能拿到的残件句柄。
func TestSchAdoptOrphanPlacementStillAdoptsAHitEvenIfProbesAreMissing(t *testing.T) {
	live := []layoutComp{adoptComp("ghost-1", "C8", "part", 440, 535)}
	v := schAdoptOrphanPlacement(map[string]bool{"old-1": true}, []string{"u3-id"}, live,
		schAdoptRequest{Designator: "C8", X: 440, Y: 535})
	if v.Adopted == nil || v.Adopted.ID != "ghost-1" {
		t.Fatalf("verdict=%+v, want the visible orphan adopted regardless of probe bookkeeping", v)
	}
	if v.Uncertain {
		t.Fatalf("verdict=%+v, a positive hit is never uncertain", v)
	}
}

// anchor-first 边界:第一件就超时,没有任何探针。此刻「没落地」与「回读读得太早」
// 在观测上完全等价 —— 只能报 uncertain。这里同时钉住**不许**拿落地前快照里的 id
// 当探针来假装能证明:下面这次回读把快照件 old-1 完整带回来了,它依然不算新鲜。
func TestSchAdoptOrphanPlacementAnchorFirstFailureIsUncertainNotProven(t *testing.T) {
	live := []layoutComp{adoptComp("old-1", "R9", "part", 100, 100)}
	v := schAdoptOrphanPlacement(map[string]bool{"old-1": true}, nil, live,
		schAdoptRequest{Designator: "U3", X: 690, Y: 460})
	if !v.Uncertain || v.Fresh {
		t.Fatalf("verdict=%+v, want uncertain: a pre-command snapshot id can never prove freshness", v)
	}
	if len(v.MissingProbes) != 0 {
		t.Fatalf("missingProbes=%v, want empty — nothing was missing, there simply was no probe", v.MissingProbes)
	}
	if strings.Contains(v.Reason, "确实没有落地") {
		t.Fatalf("reason must not claim the place did not land: %q", v.Reason)
	}
	if !strings.Contains(v.Reason, "第一件") {
		t.Fatalf("reason must explain WHY it cannot prove freshness: %q", v.Reason)
	}
}

// 空白页 + 第一件超时也走同一条路:没有探针就是没有证据。
func TestSchAdoptOrphanPlacementEmptyPageFirstPlaceIsUncertain(t *testing.T) {
	v := schAdoptOrphanPlacement(map[string]bool{}, nil, nil,
		schAdoptRequest{Designator: "U1", X: 400, Y: 300})
	if !v.Uncertain || v.Fresh {
		t.Fatalf("verdict=%+v, want uncertain on an empty page with no probe", v)
	}
}

func TestSchAdoptFreshnessCountsOnlyRealProbes(t *testing.T) {
	live := []layoutComp{adoptComp("a", "U1", "part", 0, 0), {ID: "", ComponentType: "part"}}
	if fresh, missing := schAdoptFreshness([]string{"a", " a ", ""}, live); !fresh || len(missing) != 0 {
		t.Fatalf("fresh=%v missing=%v, want a deduped/trimmed probe set to pass", fresh, missing)
	}
	if fresh, missing := schAdoptFreshness([]string{"b", "a"}, live); fresh || !reflect.DeepEqual(missing, []string{"b"}) {
		t.Fatalf("fresh=%v missing=%v, want the absent probe reported", fresh, missing)
	}
	// 空探针集 ≠ 新鲜。这是门⓪的默认方向:没有证据就不许下结论。
	if fresh, _ := schAdoptFreshness(nil, live); fresh {
		t.Fatal("an empty probe set must never count as proof of freshness")
	}
}

// uncertain 的处方必须能直接跑(本仓铁律:判据要给能执行的下一步),
// 并且要点名「回读不可信」的证据。
func TestSchAdoptUncertainGuidanceIsRunnable(t *testing.T) {
	var b strings.Builder
	schAdoptUncertainGuidance(&b, schAdoptRequest{Designator: "C8", X: 440, Y: 535}, []string{"u3-id"})
	out := b.String()
	for _, want := range []string{"easyeda sch save", "easyeda sch list", "easyeda sch prim-delete --ids", "(440,535)", "u3-id"} {
		if !strings.Contains(out, want) {
			t.Fatalf("guidance must contain %q:\n%s", want, out)
		}
	}
	var noEvidence strings.Builder
	schAdoptUncertainGuidance(&noEvidence, schAdoptRequest{X: 1, Y: 2}, nil)
	if strings.Contains(noEvidence.String(), "证据") {
		t.Fatalf("no missing probe → no evidence line:\n%s", noEvidence.String())
	}
	if !strings.Contains(noEvidence.String(), "easyeda sch list") {
		t.Fatalf("the runnable steps must still be printed:\n%s", noEvidence.String())
	}
}

// ── 接线级:settleRead 的满足条件 = 「命中了 或 回读被证明新鲜」 ───────────────
//
// 纯判定的单测证明不了这一条:它管的是**读几次、拿哪一次定案**。首版的满足条件
// 只有「命中了」,于是一次 stale 回读会把 settle 预算耗光,然后交出**错误**的
// 「确实没有落地」——重试的代价照付,结论照样是反的。

// fakeAdoptFrames 按顺序交出 components.list 的回读帧,最后一帧重复供货,
// 并记录被调了几次(= settle 到底读了几拍)。
func newFakeAdoptDaemon(t *testing.T, frames [][]map[string]any) (*appConfig, func(), *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"service":"easyeda-agent","windows":[]}`))
			return
		}
		i := calls
		if i >= len(frames) {
			i = len(frames) - 1
		}
		calls++
		comps := make([]any, 0, len(frames[i]))
		for _, c := range frames[i] {
			comps = append(comps, c)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "result": map[string]any{"components": comps},
		})
	}))
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, _ := strings.Cut(hostPort, ":")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return &appConfig{host: host, ports: fmt.Sprintf("%d-%d", port, port)}, srv.Close, &calls
}

func adoptFrameComp(id, designator string, x, y float64) map[string]any {
	return map[string]any{"primitiveId": id, "designator": designator, "componentType": "part", "x": x, "y": y}
}

// 正对照(接线级):回读一直 stale → 读满 settle 预算 → 交出 uncertain + 可执行处方,
// **绝不**交出「确实没有落地」。
func TestBapAdoptAfterPlaceFailureStaysUncertainOnAPersistentlyStaleRead(t *testing.T) {
	stale := []map[string]any{adoptFrameComp("old-1", "R9", 100, 100)} // 丢了 u3-id,也没有 C8
	cfg, cleanup, calls := newFakeAdoptDaemon(t, [][]map[string]any{stale})
	defer cleanup()

	var errOut bytes.Buffer
	placement, uncertain, adopted := bapAdoptAfterPlaceFailure(cfg, "",
		map[string]bool{"old-1": true},
		[]bapPlacement{{Designator: "U3", PrimitiveID: "u3-id"}},
		bapPlacement{Designator: "C8", X: 440, Y: 535}, &errOut)

	if placement != nil || len(adopted) != 0 {
		t.Fatalf("placement=%+v adopted=%v, want nothing claimed from a stale read", placement, adopted)
	}
	if len(uncertain) != 1 {
		t.Fatalf("uncertain=%v, want exactly one uncertainty carried into the rollback report", uncertain)
	}
	if *calls != settleAttempts {
		t.Fatalf("components.list called %d time(s), want the full settle budget (%d)", *calls, settleAttempts)
	}
	out := errOut.String()
	if strings.Contains(out, "确实没有落地") {
		t.Fatalf("a stale read must never produce the confident wording:\n%s", out)
	}
	if !strings.Contains(out, "adopt ?") {
		t.Fatalf("uncertain must be marked as such on stderr:\n%s", out)
	}
	for _, want := range []string{"easyeda sch save", "easyeda sch list", "prim-delete", "u3-id"} {
		if !strings.Contains(out, want) {
			t.Fatalf("guidance must contain %q:\n%s", want, out)
		}
	}
}

// 负对照(接线级):第一拍 stale、第二拍新鲜且那里确实什么都没有 → 照常给出定论。
// 这条钉的是「stale 时重读一拍」真的换来了正确结论,而不是永远 uncertain。
func TestBapAdoptAfterPlaceFailureConcludesOnceTheReadSettlesFresh(t *testing.T) {
	stale := []map[string]any{adoptFrameComp("old-1", "R9", 100, 100)}
	fresh := []map[string]any{adoptFrameComp("old-1", "R9", 100, 100), adoptFrameComp("u3-id", "U3", 690, 460)}
	cfg, cleanup, calls := newFakeAdoptDaemon(t, [][]map[string]any{stale, fresh})
	defer cleanup()

	var errOut bytes.Buffer
	placement, uncertain, adopted := bapAdoptAfterPlaceFailure(cfg, "",
		map[string]bool{"old-1": true},
		[]bapPlacement{{Designator: "U3", PrimitiveID: "u3-id"}},
		bapPlacement{Designator: "C8", X: 440, Y: 535}, &errOut)

	if placement != nil || len(adopted) != 0 || len(uncertain) != 0 {
		t.Fatalf("placement=%+v uncertain=%v adopted=%v, want a clean proven-negative", placement, uncertain, adopted)
	}
	if *calls != 2 {
		t.Fatalf("components.list called %d time(s), want 2 (stale → re-read → fresh)", *calls)
	}
	if out := errOut.String(); !strings.Contains(out, "adopt ✓") || !strings.Contains(out, "确实没有落地") {
		t.Fatalf("a proven-fresh empty read must still conclude:\n%s", out)
	}
}

func TestSchAdoptResidueGuidanceIsRunnable(t *testing.T) {
	var b strings.Builder
	schAdoptResidueGuidance(&b, []string{"id-1", "id-2"})
	out := b.String()
	if !strings.Contains(out, "easyeda sch prim-delete --ids id-1,id-2") {
		t.Fatalf("guidance must carry a runnable command:\n%s", out)
	}
	if !strings.Contains(out, "wedge") {
		t.Fatalf("guidance must explain the wedge case:\n%s", out)
	}
	var empty strings.Builder
	schAdoptResidueGuidance(&empty, nil)
	if empty.Len() != 0 {
		t.Fatalf("no residue → no noise, got %q", empty.String())
	}
}
