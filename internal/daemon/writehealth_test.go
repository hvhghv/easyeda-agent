package daemon

// Tests for the adaptive-backoff layer (writehealth.go, REPORT round2 新 3).
// All offline: dispatch is an injected fake, sleeps are captured.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

func okResp(id string) *protocol.Response {
	return &protocol.Response{Envelope: protocol.Envelope{ID: id}, OK: true}
}

func failResp(id, code string) *protocol.Response {
	return &protocol.Response{Envelope: protocol.Envelope{ID: id}, OK: false,
		Error: &protocol.ErrorInfo{Code: code, Message: code}}
}

// scriptedDispatch answers per-action queues so the probe and the original
// request can be scripted independently.
type scriptedDispatch struct {
	byAction map[string][]func() (*protocol.Response, error)
	calls    []string
}

func (d *scriptedDispatch) fn(_ context.Context, req protocol.Request) (*protocol.Response, error) {
	d.calls = append(d.calls, req.Action)
	q := d.byAction[req.Action]
	if len(q) == 0 {
		return nil, errors.New("unexpected dispatch: " + req.Action)
	}
	next := q[0]
	d.byAction[req.Action] = q[1:]
	return next()
}

func give(r *protocol.Response, err error) func() (*protocol.Response, error) {
	return func() (*protocol.Response, error) { return r, err }
}

func noSleepHooks(t *testing.T, tr *writeHealthTracker, windowID string) adaptiveHooks {
	t.Helper()
	return adaptiveHooks{
		observe: func(o outcome) { tr.observe(windowID, o) },
		sleep:   func(time.Duration) {},
	}
}

// ── 白名单幂等动作:失败 → 轻读探测 OK → 重发一次 ─────────────────────────

func TestAdaptiveRetryIdempotentActionRetriesAfterProbe(t *testing.T) {
	d := &scriptedDispatch{byAction: map[string][]func() (*protocol.Response, error){
		"document.open":    {give(failResp("r1", "EDA_ERROR"), nil), give(okResp("r1"), nil)},
		backoffProbeAction: {give(okResp("r1_probe"), nil)},
	}}
	tr := newWriteHealthTracker()
	slept := 0
	h := noSleepHooks(t, tr, "w1")
	h.sleep = func(time.Duration) { slept++ }
	audited := 0
	h.auditFirst = func(*protocol.Response, error) { audited++ }

	req := protocol.Request{Envelope: protocol.Envelope{ID: "r1", WindowID: "w1"}, Action: "document.open"}
	resp, err, retried := forwardWithAdaptiveRetry(context.Background(), req, d.fn, h)
	if err != nil || resp == nil || !resp.OK {
		t.Fatalf("resp=%+v err=%v, want retried success", resp, err)
	}
	if !retried || slept != 1 || audited != 1 {
		t.Fatalf("retried=%v slept=%d audited=%d, want true/1/1", retried, slept, audited)
	}
	// 调用序:原发 → 探测 → 重发。
	want := []string{"document.open", backoffProbeAction, "document.open"}
	if strings.Join(d.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", d.calls, want)
	}
	// 成功响应上要说明发生过自动重试。
	found := false
	for _, w := range resp.Warnings {
		if strings.Contains(w, "auto-retried") {
			found = true
		}
	}
	if !found {
		t.Fatalf("success after retry must carry a warning: %v", resp.Warnings)
	}
	// 两次尝试都进了滚动健康度(1 失败 1 成功)。
	if h := tr.snapshot("w1"); h.Samples != 2 || h.Failures != 1 {
		t.Fatalf("health = %+v, want samples=2 failures=1", h)
	}
}

// ── 探测也失败 = 连接器停摆 → 透传原失败,不加压 ──────────────────────────

func TestAdaptiveRetryProbeFailurePassesThroughWithoutRetry(t *testing.T) {
	d := &scriptedDispatch{byAction: map[string][]func() (*protocol.Response, error){
		"document.open":    {give(nil, errors.New("timeout"))},
		backoffProbeAction: {give(nil, errors.New("probe timeout"))},
	}}
	tr := newWriteHealthTracker()
	req := protocol.Request{Envelope: protocol.Envelope{ID: "r2", WindowID: "w1"}, Action: "document.open"}
	_, err, retried := forwardWithAdaptiveRetry(context.Background(), req, d.fn,
		noSleepHooks(t, tr, "w1"))
	if err == nil {
		t.Fatal("original failure must be passed through")
	}
	if retried {
		t.Fatal("must not report retried")
	}
	if len(d.calls) != 2 {
		t.Fatalf("calls = %v — the original action must NOT be resent when even a read fails", d.calls)
	}
}

// ── 非白名单动作(内容写 / exec_js)绝不 daemon 级重发 ─────────────────────

func TestAdaptiveRetryNeverRetriesNonWhitelistedWrites(t *testing.T) {
	for _, action := range []string{"schematic.wire.create", "schematic.power.connect_pin", "debug.exec_js"} {
		d := &scriptedDispatch{byAction: map[string][]func() (*protocol.Response, error){
			action: {give(failResp("r3", "EDA_ERROR"), nil)},
		}}
		tr := newWriteHealthTracker()
		req := protocol.Request{Envelope: protocol.Envelope{ID: "r3", WindowID: "w1"}, Action: action}
		resp, err, retried := forwardWithAdaptiveRetry(context.Background(), req, d.fn,
			noSleepHooks(t, tr, "w1"))
		if err != nil || resp.OK {
			t.Fatalf("%s: want the failed response passed through", action)
		}
		if retried || len(d.calls) != 1 {
			t.Fatalf("%s: calls=%v retried=%v — a possibly-landed write must never be blind-resent", action, d.calls, retried)
		}
	}
}

func TestAdaptiveRetrySuccessNeverProbes(t *testing.T) {
	d := &scriptedDispatch{byAction: map[string][]func() (*protocol.Response, error){
		"document.open": {give(okResp("r4"), nil)},
	}}
	tr := newWriteHealthTracker()
	req := protocol.Request{Envelope: protocol.Envelope{ID: "r4", WindowID: "w1"}, Action: "document.open"}
	resp, err, retried := forwardWithAdaptiveRetry(context.Background(), req, d.fn,
		noSleepHooks(t, tr, "w1"))
	if err != nil || !resp.OK || retried || len(d.calls) != 1 {
		t.Fatalf("success must be returned as-is (calls=%v)", d.calls)
	}
}

func TestRetryableSetIsIdempotentOnly(t *testing.T) {
	if !retryableOnFailure["document.open"] || !retryableOnFailure["schematic.page.open"] {
		t.Fatal("the two idempotent open actions must be retryable")
	}
	for _, banned := range []string{"debug.exec_js", "schematic.wire.create", "schematic.component.place",
		"schematic.power.connect_pin", "pcb.save", "schematic.save"} {
		if retryableOnFailure[banned] {
			t.Fatalf("%s must never be daemon-retried (duplicate risk / arbitrary code)", banned)
		}
	}
}

// ── 滚动健康度与退化阈值 ─────────────────────────────────────────────────

func TestWriteHealthTrackerRatesAndDegradation(t *testing.T) {
	tr := newWriteHealthTracker()
	if h := tr.snapshot("w1"); h.Degraded || h.Samples != 0 {
		t.Fatalf("empty window = %+v", h)
	}

	// 连续失败阈值:3 连败即退化,不需要样本量。
	tr.observe("w1", outcome{Action: "schematic.wire.create", OK: false})
	tr.observe("w1", outcome{Action: "schematic.wire.create", OK: false})
	if tr.snapshot("w1").Degraded {
		t.Fatal("2 consecutive failures must not yet degrade")
	}
	tr.observe("w1", outcome{Action: "schematic.wire.create", OK: false})
	h := tr.snapshot("w1")
	if !h.Degraded || h.ConsecutiveFailures != 3 || h.LastFailureAction != "schematic.wire.create" {
		t.Fatalf("health = %+v, want degraded on 3 consecutive failures", h)
	}

	// 一次成功清零连败;比率不足阈值时恢复健康。
	for i := 0; i < 17; i++ {
		tr.observe("w1", outcome{Action: "x", OK: true})
	}
	h = tr.snapshot("w1")
	if h.Degraded || h.ConsecutiveFailures != 0 {
		t.Fatalf("health after recovery = %+v", h)
	}

	// 比率阈值:窗口 20,交替失败把比率顶过 0.35(不连败)。
	tr2 := newWriteHealthTracker()
	for i := 0; i < 10; i++ {
		tr2.observe("w2", outcome{Action: "a", OK: i%2 == 0}) // 50% failure, never 3 consecutive
	}
	h = tr2.snapshot("w2")
	if !h.Degraded || h.FailureRate != 0.5 {
		t.Fatalf("health = %+v, want degraded at 50%% over %d samples", h, h.Samples)
	}

	// 环形窗:旧样本滚出。
	for i := 0; i < writeHealthWindow; i++ {
		tr2.observe("w2", outcome{Action: "a", OK: true})
	}
	if h := tr2.snapshot("w2"); h.Failures != 0 || h.Samples != writeHealthWindow {
		t.Fatalf("ring did not roll: %+v", h)
	}

	// forget 清空(重连窗口从零开始)。
	tr2.forget("w2")
	if h := tr2.snapshot("w2"); h.Samples != 0 {
		t.Fatalf("forget did not clear: %+v", h)
	}
}

// ── 失败响应上的结构化 degraded 提示 ─────────────────────────────────────

func TestAnnotateDegradedOnFailedWrite(t *testing.T) {
	tr := newWriteHealthTracker()
	for i := 0; i < 3; i++ {
		tr.observe("w1", outcome{Action: "schematic.wire.create", OK: false})
	}
	req := &protocol.Request{Envelope: protocol.Envelope{ID: "r", WindowID: "w1"}, Action: "schematic.wire.create"}
	resp := failResp("r", "EDA_ERROR")
	tr.annotateDegraded(req, resp)
	deg, _ := resp.Result["degraded"].(map[string]any)
	if deg == nil || deg["degraded"] != true {
		t.Fatalf("result.degraded missing: %+v", resp.Result)
	}
	if deg["consecutiveFailures"] != 3 {
		t.Fatalf("degraded detail = %+v", deg)
	}
	// 写失败必须带假失败定律的建议:先轻读复核,勿盲重发。
	joined := strings.Join(resp.Warnings, "\n")
	if !strings.Contains(joined, "verify with a light read") || !strings.Contains(joined, "假失败") {
		t.Fatalf("warnings must carry the fake-failure-law advice: %v", resp.Warnings)
	}

	// 健康窗口的失败不加噪音。
	resp2 := failResp("r", "EDA_ERROR")
	tr.annotateDegraded(&protocol.Request{Envelope: protocol.Envelope{ID: "r", WindowID: "w_healthy"},
		Action: "schematic.wire.create"}, resp2)
	if resp2.Result != nil || len(resp2.Warnings) != 0 {
		t.Fatalf("healthy window must stay unannotated: %+v", resp2)
	}

	// 成功响应绝不标注。
	resp3 := okResp("r")
	tr.annotateDegraded(req, resp3)
	if resp3.Result != nil || len(resp3.Warnings) != 0 {
		t.Fatalf("success must stay unannotated: %+v", resp3)
	}

	// 读失败给通用建议(不提"已落地",那是写的语义)。
	resp4 := failResp("r", "EDA_ERROR")
	tr.annotateDegraded(&protocol.Request{Envelope: protocol.Envelope{ID: "r", WindowID: "w1"},
		Action: "schematic.components.list"}, resp4)
	joined = strings.Join(resp4.Warnings, "\n")
	if !strings.Contains(joined, "light read") || strings.Contains(joined, "假失败") {
		t.Fatalf("read advice wrong: %v", resp4.Warnings)
	}
}

// ── /health 暴露 writeHealth ─────────────────────────────────────────────

func TestHealthEndpointExposesWriteHealth(t *testing.T) {
	s := New(Options{})
	for i := 0; i < 3; i++ {
		s.writeHealth.observe("w9", outcome{Action: "document.open", OK: false})
	}
	rec := httptest.NewRecorder()
	s.routes(60832).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var h health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("bad health json: %v (%s)", err, rec.Body.String())
	}
	wh, ok := h.WriteHealth["w9"]
	if !ok {
		t.Fatalf("writeHealth missing window w9: %s", rec.Body.String())
	}
	if !wh.Degraded || wh.ConsecutiveFailures != 3 || wh.LastFailureAction != "document.open" {
		t.Fatalf("writeHealth = %+v", wh)
	}

	// 静默 daemon 不带该字段(omitempty)。
	s2 := New(Options{})
	rec2 := httptest.NewRecorder()
	s2.routes(60832).ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/health", nil))
	if strings.Contains(rec2.Body.String(), "writeHealth") {
		t.Fatalf("quiet daemon must omit writeHealth: %s", rec2.Body.String())
	}
}

// ══════════════════════════════════════════════════════════════════════════
// 口径修订(2026-08-19 真机复验):健康度测「写的效果」而不是「调用的返回码」。
// 下面每条用例的输入都是那场端到端跑出来的真实数字。
// ══════════════════════════════════════════════════════════════════════════

// mutResp 是一次「返回成功」的写响应,result 由调用方给(用来喂通道 A 的证据)。
func mutResp(id string, result map[string]any) *protocol.Response {
	return &protocol.Response{Envelope: protocol.Envelope{ID: id}, OK: true, Result: result}
}

// ── 通道 A:连接器自带的回读结论(响应里就能挖出来) ───────────────────────

func TestEffectFromResponseMinesConnectorReadback(t *testing.T) {
	del := &protocol.Request{Envelope: protocol.Envelope{ID: "r"}, Action: "schematic.primitives.delete"}
	mod := &protocol.Request{Envelope: protocol.Envelope{ID: "r"}, Action: "schematic.component.modify"}
	read := &protocol.Request{Envelope: protocol.Envelope{ID: "r"}, Action: "schematic.components.list"}

	cases := []struct {
		name string
		req  *protocol.Request
		resp *protocol.Response
		want effectVerdict
	}{
		{
			// issue #164 真机形状:ok:true,连接器回读后报 1 个图元还活着。
			// 这就是首版完全看不见、却让 degraded 一路绿的那一类。
			"prim-delete 幸存者", del,
			mutResp("r", map[string]any{
				"deleted": map[string]any{"text": float64(3)}, "total": float64(3), "requested": float64(4),
				"partial": true, "survivedTotal": float64(1),
				"survived": map[string]any{"text": []any{"gge123"}},
			}),
			effectNotLanded,
		},
		{"survivedTotal 单独出现", del,
			mutResp("r", map[string]any{"survivedTotal": float64(2)}), effectNotLanded},
		{"#151 notApplied", mod,
			mutResp("r", map[string]any{"applied": []any{"Value"}, "notApplied": []any{"Designator"}}), effectNotLanded},
		{"deleted:false", del, mutResp("r", map[string]any{"deleted": false}), effectNotLanded},
		{"干净成功没有证据 → unknown(ok 不是落地的证明)", del,
			mutResp("r", map[string]any{"deleted": map[string]any{"text": float64(4)}, "total": float64(4)}), effectUnknown},
		{"读动作没有「效果」可验", read,
			mutResp("r", map[string]any{"partial": true}), effectUnknown},
		{"失败响应交给返回码口径", del, failResp("r", "EDA_ERROR"), effectUnknown},
		{"nil 响应", del, nil, effectUnknown},
	}
	for _, c := range cases {
		if got := effectFromResponse(c.req, c.resp); got != c.want {
			t.Errorf("%s: effectFromResponse = %v, want %v", c.name, got, c.want)
		}
	}
}

// 转发路径必须把通道 A 的判决记进健康度:prim-delete 返回 ok:true 却带幸存者,
// 在 daemon 眼里就是一次失败的写。
func TestForwardRecordsFakeSuccessAsFailure(t *testing.T) {
	d := &scriptedDispatch{byAction: map[string][]func() (*protocol.Response, error){
		"schematic.primitives.delete": {give(mutResp("r1", map[string]any{
			"partial": true, "survivedTotal": float64(1)}), nil)},
	}}
	tr := newWriteHealthTracker()
	req := protocol.Request{Envelope: protocol.Envelope{ID: "r1", WindowID: "w1"}, Action: "schematic.primitives.delete"}
	resp, err, _ := forwardWithAdaptiveRetry(context.Background(), req, d.fn, noSleepHooks(t, tr, "w1"))
	if err != nil || resp == nil || !resp.OK {
		t.Fatalf("the ok:true response must still be passed through untouched: %+v %v", resp, err)
	}
	h := tr.snapshot("w1")
	if h.Samples != 1 || h.Failures != 1 || h.FakeSuccesses != 1 || h.Verified != 1 {
		t.Fatalf("health = %+v, want 1 sample / 1 failure / 1 fakeSuccess (返回成功但回读证明没生效)", h)
	}
}

// ── 真机复现:0.05 的绿灯是假的 ────────────────────────────────────────────
//
// 那一整场端到端里 writeHealth 全程 failureRate 0.05 / degraded:false,而画布上
// 大面积的写没生效。这条用例先复现那个读数,再把回读结论灌进去,证明同一批样本
// 在新口径下会亮红。
func TestVerifiedNotLandedFlipsTheGreenReading(t *testing.T) {
	tr := newWriteHealthTracker()
	// 20 次转发,只有 1 次返回失败 —— 首版口径:5%,不退化。
	for i := 0; i < 20; i++ {
		tr.observe("w1", outcome{Action: "schematic.component.place", RequestID: fmt.Sprintf("req_%d", i), OK: i != 7})
	}
	h := tr.snapshot("w1")
	if h.FailureRate != 0.05 || h.Degraded {
		t.Fatalf("baseline = %+v, want the misleading 5%%/healthy reading", h)
	}
	if h.Verified != 0 {
		t.Fatalf("nothing has been verified yet: %+v", h)
	}
	// 回读:8 件其实没落地(逐条按 requestId 定位,不新增样本)。
	for _, i := range []int{0, 1, 2, 3, 4, 5, 6, 8} {
		tr.verify("w1", WriteVerification{Action: "schematic.component.place",
			RequestID: fmt.Sprintf("req_%d", i), NotLanded: 1, Source: "sch block-apply"})
	}
	h = tr.snapshot("w1")
	if h.Samples != 20 {
		t.Fatalf("keyed verification must AMEND samples, not add them: %+v", h)
	}
	if h.Failures != 9 || h.FailureRate != 0.45 || h.FakeSuccesses != 8 {
		t.Fatalf("health = %+v, want 9 failures / 45%% / 8 fakeSuccesses", h)
	}
	if !h.Degraded {
		t.Fatal("45% effect-failure rate must degrade the window (the whole point of the rewrite)")
	}
	if h.Verified != 8 {
		t.Fatalf("verified = %d, want 8 (口径:多少样本真有回读证据)", h.Verified)
	}
}

// ── 假失败:报失败但回读证明已落地 → 不计 failure,单独归一类 ───────────────
//
// 单独归类而不是直接丢掉:它同样是连接器不健康的信号,但**处置动作相反** ——
// 假成功要补写,假失败绝不能重发(会造重复旗)。混进一个数字就没法指导决策。
func TestFakeFailureIsNotAFailureButStaysVisible(t *testing.T) {
	tr := newWriteHealthTracker()
	for i := 0; i < 3; i++ {
		tr.observe("w1", outcome{Action: "schematic.power.connect_pin", OK: false})
	}
	if h := tr.snapshot("w1"); !h.Degraded || h.Failures != 3 {
		t.Fatalf("before verification the streak must look degraded: %+v", h)
	}
	// `sch connect` 的 slow-landed 复核:三次都已经连上了(通道 B,无 requestId)。
	returnedFail := false
	for i := 0; i < 3; i++ {
		tr.verify("w1", WriteVerification{Action: "schematic.power.connect_pin",
			ReturnedOK: &returnedFail, Landed: 1, Source: "sch connect"})
	}
	h := tr.snapshot("w1")
	if h.Failures != 0 || h.FailureRate != 0 {
		t.Fatalf("health = %+v, want 0 failures — 报失败但已落地不算失败", h)
	}
	if h.FakeFailures != 3 {
		t.Fatalf("health = %+v, want fakeFailures=3 (单独一类,重发会造重复)", h)
	}
	if h.ConsecutiveFailures != 0 {
		t.Fatalf("late verdicts must break the streak, got %+v", h)
	}
	if h.Degraded {
		t.Fatalf("three proven-landed writes are not a degraded window: %+v", h)
	}
	if h.Samples != 3 {
		t.Fatalf("verification must amend the existing samples, not append: %+v", h)
	}
}

// ── 分桶:单条路的高失败率不能被混合流量稀释 ───────────────────────────────

func TestSingleActionFailureRateSurvivesMixedTraffic(t *testing.T) {
	cases := []struct {
		name        string
		action      string
		calls       int
		failAt      []int // 失败落在第几次调用(均匀铺开,绝不连着 3 次)
		wantRate    float64
		otherAction string
	}{
		// 真机:connect_pin 一批里约 40% 失败,混合窗口读数仍是 5%。
		{"connect_pin 40%", "schematic.power.connect_pin", 10, []int{0, 3, 6, 9}, 0.4, "schematic.components.list"},
		// 真机:长会话下 document.open 失败率 7% → 41%。
		{"document.open 41%", "document.open", 12, []int{0, 3, 5, 8, 10}, 0.4167, "schematic.components.list"},
	}
	for _, c := range cases {
		tr := newWriteHealthTracker()
		fail := map[int]bool{}
		for _, i := range c.failAt {
			fail[i] = true
		}
		for i := 0; i < c.calls; i++ {
			// 失败之间夹着别的动作 —— 否则窗口级连败规则就把它抓了,
			// 这条用例要验的恰恰是**只有分桶才看得见**的那种退化。
			tr.observe("w1", outcome{Action: c.action, OK: !fail[i]})
			tr.observe("w1", outcome{Action: c.otherAction, OK: true})
		}
		h := tr.snapshot("w1")
		if h.ConsecutiveFailures >= writeHealthConsecFails {
			t.Fatalf("%s: fixture accidentally built a failure streak: %+v", c.name, h)
		}
		if h.FailureRate >= writeHealthDegradedRate {
			t.Fatalf("%s: window rate %.2f already trips the mixed rule — the test would prove nothing",
				c.name, h.FailureRate)
		}
		if !h.Degraded {
			t.Fatalf("%s: window rate %.2f hides a %.0f%% action; degraded must still fire: %+v",
				c.name, h.FailureRate, c.wantRate*100, h)
		}
		if len(h.DegradedActions) != 1 || h.DegradedActions[0] != c.action {
			t.Fatalf("%s: degradedActions = %v, want exactly [%s]", c.name, h.DegradedActions, c.action)
		}
		a, ok := h.Actions[c.action]
		if !ok {
			t.Fatalf("%s: the broken road must be reported in actions: %+v", c.name, h.Actions)
		}
		if !a.Degraded || math.Abs(a.FailureRate-c.wantRate) > 0.005 {
			t.Fatalf("%s: bucket = %+v, want ~%.2f and degraded", c.name, a, c.wantRate)
		}
		if _, listed := h.Actions[c.otherAction]; listed {
			t.Fatalf("%s: a clean action must not clutter /health: %+v", c.name, h.Actions)
		}
	}
}

// block-apply 真机形态:6 件器件每次 place 都返回成功,回读只找到 1 件。
// 一条聚合判决(landed=1 notLanded=5)必须把那条路顶成 degraded。
func TestBatchVerificationFlipsTheActionBucket(t *testing.T) {
	tr := newWriteHealthTracker()
	for i := 0; i < 6; i++ {
		tr.observe("w1", outcome{Action: "schematic.component.place", OK: true})
	}
	if h := tr.snapshot("w1"); h.Degraded || h.Failures != 0 {
		t.Fatalf("six ok places look perfect until someone reads the page back: %+v", h)
	}
	tr.verify("w1", WriteVerification{Action: "schematic.component.place",
		Landed: 1, NotLanded: 5, Source: "sch block-apply"})
	h := tr.snapshot("w1")
	if h.Samples != 6 {
		t.Fatalf("an aggregate verdict must reuse the existing samples: %+v", h)
	}
	if h.Failures != 5 || h.FakeSuccesses != 5 || h.Verified != 6 {
		t.Fatalf("health = %+v, want 5 failures / 5 fakeSuccesses / 6 verified", h)
	}
	if !h.Degraded || len(h.DegradedActions) != 1 {
		t.Fatalf("6 件只落地 1 件必须退化: %+v", h)
	}
}

// 迟到的判决:被判的样本早已滚出环形窗(或压根来自另一次会话)——补记新样本,
// 绝不静默丢掉,否则批量假成功又变回不可见。
func TestLateVerdictWithNoMatchingSampleIsRecorded(t *testing.T) {
	tr := newWriteHealthTracker()
	tr.verify("w1", WriteVerification{Action: "schematic.component.place", NotLanded: 2})
	h := tr.snapshot("w1")
	if h.Samples != 2 || h.Failures != 2 || h.FakeSuccesses != 2 {
		t.Fatalf("health = %+v, want the verdict recorded as 2 failed samples", h)
	}
}

// 成败面必须对得上:一条「返回失败但已落地」的判决不能去改写一条成功样本。
func TestVerificationMatchesTheReportedReturnCode(t *testing.T) {
	tr := newWriteHealthTracker()
	tr.observe("w1", outcome{Action: "schematic.power.connect_pin", OK: true})
	returnedFail := false
	tr.verify("w1", WriteVerification{Action: "schematic.power.connect_pin",
		ReturnedOK: &returnedFail, Landed: 1})
	h := tr.snapshot("w1")
	if h.Samples != 2 {
		t.Fatalf("the ok sample must be left alone and a new one recorded: %+v", h)
	}
	if h.FakeFailures != 1 || h.Failures != 0 {
		t.Fatalf("health = %+v, want 1 fakeFailure and 0 failures", h)
	}
}

// 分桶不能永久闩死:一条早就没人调的路,不该让窗口一直红着。
func TestActionBucketAgesOut(t *testing.T) {
	tr := newWriteHealthTracker()
	for i := 0; i < 5; i++ {
		tr.observe("w1", outcome{Action: "schematic.page.rename", OK: false})
	}
	if h := tr.snapshot("w1"); !h.Degraded {
		t.Fatal("5/5 failures on one action must degrade")
	}
	for i := 0; i < writeHealthActionHorizon; i++ {
		tr.observe("w1", outcome{Action: "schematic.components.list", OK: true})
	}
	h := tr.snapshot("w1")
	if h.Degraded || len(h.DegradedActions) != 0 {
		t.Fatalf("an aged-out bucket must stop shouting: %+v", h)
	}
	if _, listed := h.Actions["schematic.page.rename"]; listed {
		t.Fatalf("aged-out bucket must not be reported: %+v", h.Actions)
	}
}

// ── 假成功响应也要带 degraded 提示(它以前是「干净成功」) ─────────────────

func TestAnnotateDegradedOnFakeSuccessResponse(t *testing.T) {
	tr := newWriteHealthTracker()
	for i := 0; i < 6; i++ {
		tr.observe("w1", outcome{Action: "schematic.primitives.delete", OK: false})
	}
	req := &protocol.Request{Envelope: protocol.Envelope{ID: "r", WindowID: "w1"}, Action: "schematic.primitives.delete"}
	resp := mutResp("r", map[string]any{"partial": true, "survivedTotal": float64(1)})
	tr.annotateDegraded(req, resp)
	deg, _ := resp.Result["degraded"].(map[string]any)
	if deg == nil || deg["degraded"] != true {
		t.Fatalf("an ok-but-not-landed response must carry the advisory too: %+v", resp.Result)
	}
	joined := strings.Join(resp.Warnings, "\n")
	if !strings.Contains(joined, "假成功") {
		t.Fatalf("warnings must name the fake-success shape: %v", resp.Warnings)
	}
	if !strings.Contains(joined, "schematic.primitives.delete") {
		t.Fatalf("warnings must name the degraded road: %v", resp.Warnings)
	}
	// 干净的成功响应仍然一个字都不加。
	clean := mutResp("r", map[string]any{"total": float64(3)})
	tr.annotateDegraded(req, clean)
	if _, has := clean.Result["degraded"]; has || len(clean.Warnings) != 0 {
		t.Fatalf("a clean success must stay unannotated: %+v", clean)
	}
}

// ── 通道 B 的上报端点 ─────────────────────────────────────────────────────

func postVerify(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/writeverify", strings.NewReader(body))
	s.routes(60832).ServeHTTP(rec, req)
	return rec
}

func TestWriteVerifyEndpoint(t *testing.T) {
	s := New(Options{})
	for i := 0; i < 6; i++ {
		s.writeHealth.observe("w1", outcome{Action: "schematic.component.place", OK: true})
	}
	rec := postVerify(t, s, `{"windowId":"w1","action":"schematic.component.place","landed":1,"notLanded":5,"source":"sch block-apply"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		OK          bool              `json:"ok"`
		WindowID    string            `json:"windowId"`
		WriteHealth WindowWriteHealth `json:"writeHealth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body.String())
	}
	if !out.OK || out.WindowID != "w1" {
		t.Fatalf("response = %+v", out)
	}
	if out.WriteHealth.Failures != 5 || !out.WriteHealth.Degraded {
		t.Fatalf("the endpoint must echo the amended health: %+v", out.WriteHealth)
	}
	if h := s.writeHealth.snapshot("w1"); h.FakeSuccesses != 5 {
		t.Fatalf("tracker not updated: %+v", h)
	}

	// 校验:动作必填、计数必须非零、无法归账的判决不能静默吞掉。
	for _, bad := range []struct {
		body string
		want int
	}{
		{`{"windowId":"w1","landed":1}`, http.StatusBadRequest},
		{`{"windowId":"w1","action":"schematic.component.place"}`, http.StatusBadRequest},
		{`{"action":"schematic.component.place","landed":1}`, http.StatusServiceUnavailable},
		{`not json`, http.StatusBadRequest},
	} {
		if rec := postVerify(t, s, bad.body); rec.Code != bad.want {
			t.Fatalf("%s → %d, want %d (%s)", bad.body, rec.Code, bad.want, rec.Body.String())
		}
	}
	rec = httptest.NewRecorder()
	s.routes(60832).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/writeverify", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /writeverify = %d, want 405", rec.Code)
	}
}

// /health 的新字段必须真的出得来(报告读起来对不对,是这层唯一的验收面)。
func TestHealthEndpointExposesEffectDimensions(t *testing.T) {
	s := New(Options{})
	for i := 0; i < 6; i++ {
		s.writeHealth.observe("w9", outcome{Action: "schematic.component.place", OK: true})
	}
	s.writeHealth.verify("w9", WriteVerification{Action: "schematic.component.place", Landed: 1, NotLanded: 5})
	rec := httptest.NewRecorder()
	s.routes(60832).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var h health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("bad health json: %v (%s)", err, rec.Body.String())
	}
	wh := h.WriteHealth["w9"]
	if !wh.Degraded || wh.FakeSuccesses != 5 || wh.Verified != 6 {
		t.Fatalf("writeHealth = %+v", wh)
	}
	if len(wh.DegradedActions) != 1 || wh.DegradedActions[0] != "schematic.component.place" {
		t.Fatalf("degradedActions = %v", wh.DegradedActions)
	}
	if a := wh.Actions["schematic.component.place"]; a.Samples != 6 || a.Failures != 5 {
		t.Fatalf("actions bucket = %+v", a)
	}
}
