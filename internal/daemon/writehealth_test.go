package daemon

// Tests for the adaptive-backoff layer (writehealth.go, REPORT round2 新 3).
// All offline: dispatch is an injected fake, sleeps are captured.

import (
	"context"
	"encoding/json"
	"errors"
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
		observe: func(action string, ok bool) { tr.observe(windowID, action, ok) },
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
	tr.observe("w1", "schematic.wire.create", false)
	tr.observe("w1", "schematic.wire.create", false)
	if tr.snapshot("w1").Degraded {
		t.Fatal("2 consecutive failures must not yet degrade")
	}
	tr.observe("w1", "schematic.wire.create", false)
	h := tr.snapshot("w1")
	if !h.Degraded || h.ConsecutiveFailures != 3 || h.LastFailureAction != "schematic.wire.create" {
		t.Fatalf("health = %+v, want degraded on 3 consecutive failures", h)
	}

	// 一次成功清零连败;比率不足阈值时恢复健康。
	for i := 0; i < 17; i++ {
		tr.observe("w1", "x", true)
	}
	h = tr.snapshot("w1")
	if h.Degraded || h.ConsecutiveFailures != 0 {
		t.Fatalf("health after recovery = %+v", h)
	}

	// 比率阈值:窗口 20,交替失败把比率顶过 0.35(不连败)。
	tr2 := newWriteHealthTracker()
	for i := 0; i < 10; i++ {
		tr2.observe("w2", "a", i%2 == 0) // 50% failure, never 3 consecutive
	}
	h = tr2.snapshot("w2")
	if !h.Degraded || h.FailureRate != 0.5 {
		t.Fatalf("health = %+v, want degraded at 50%% over %d samples", h, h.Samples)
	}

	// 环形窗:旧样本滚出。
	for i := 0; i < writeHealthWindow; i++ {
		tr2.observe("w2", "a", true)
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
		tr.observe("w1", "schematic.wire.create", false)
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
		s.writeHealth.observe("w9", "document.open", false)
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
