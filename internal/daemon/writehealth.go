package daemon

// writehealth.go — 连接器负载退化的 daemon 侧对策(REPORT esp32mini-round2
// 新 3:长会话下 document.open 失败率 7%→41%、写操作系统性劣化,agent 收到
// 瞬时失败后盲重试造重复,是最贵的时间黑洞)。两件事:
//
//  1. **滚动健康度**:按窗口维护最近 N 次转发动作的成败环形窗 + 连续失败计数,
//     /health 暴露(writeHealth 字段),失败响应上附加结构化 degraded 提示。
//  2. **自适应退避重试(范围收窄,见下)**:失败后自动插一次轻读 + 短延迟再
//     重发一次 —— 但只对白名单里的动作。
//
// ## 重试范围的取舍(硬约束:绝不盲重发可能已落地的写)
//
// 通用层面无法可靠判定「上次写落没落地」:每种写的复核方式都不同(connect_pin
// 要查网表、place 要查器件存在、exec_js 是任意代码根本没有通用判据)。按任务
// 约定收窄为两档:
//
//   - **白名单重试**(retryableOnFailure):只收「重发与首发收敛到同一状态」的
//     幂等导航类动作(document.open / schematic.page.open —— 打开已打开的文档
//     是 no-op,不存在"造重复"),这类动作恰好也是实测退化最重的(41%)。重试
//     前先发一次轻读探测(project.current):轻读也失败 = 连接器停摆,加压只会
//     更糟 → 透传原失败,不重发。
//   - **其余写失败一律透传**,但当窗口处于退化态时在响应里附加结构化提示
//     (result.degraded + warning):告诉调用方先轻读复核上次写是否已落地
//     (假失败定律:报失败的写大概率已落地),复核到已落地就不要重发。逐命令
//     的复核留给命令层(zone-draw 的 survey、connect_pin 的网表回读)——
//     它们才知道各自的"落地"长什么样。

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

// retryableOnFailure lists the ONLY actions the daemon will auto-retry after a
// failure: idempotent navigation ops where a duplicate send converges to the
// same state whether or not the first send landed. Content-mutating actions
// and debug.exec_js (arbitrary code) must NEVER be here.
var retryableOnFailure = map[string]bool{
	"document.open":       true,
	"schematic.page.open": true,
}

// backoffProbeAction is the light read inserted before an auto-retry. It is
// cheap, domain-agnostic, and — measured on the degraded connector — inserting
// a read between writes markedly raises the next call's success odds.
const backoffProbeAction = "project.current"

// backoffProbeTimeout bounds the probe so a wedged connector fails the probe
// fast and the original failure is passed through instead of piling on load.
const backoffProbeTimeout = 5 * time.Second

// backoffSettleDelay is the pause between the probe and the retry (package
// variable so tests run without real sleeps).
var backoffSettleDelay = 500 * time.Millisecond

// writeHealthWindow is the ring size of per-window recent outcomes.
const writeHealthWindow = 20

// Degradation thresholds: enough samples to mean something, or an unbroken
// streak that speaks for itself.
const (
	writeHealthMinSamples   = 8
	writeHealthDegradedRate = 0.35
	writeHealthConsecFails  = 3
)

// WindowWriteHealth is the /health-exposed snapshot of one window's recent
// forwarded-action outcomes.
type WindowWriteHealth struct {
	Samples             int     `json:"samples"`
	Failures            int     `json:"failures"`
	FailureRate         float64 `json:"failureRate"`
	ConsecutiveFailures int     `json:"consecutiveFailures"`
	Degraded            bool    `json:"degraded"`
	LastFailureAction   string  `json:"lastFailureAction,omitempty"`
	LastFailureAt       string  `json:"lastFailureAt,omitempty"`
}

type windowHealthState struct {
	recent            []bool // ring of outcomes (true = ok), newest last
	consecFails       int
	lastFailureAction string
	lastFailureAt     time.Time
}

// writeHealthTracker keeps the rolling per-window outcome window. All methods
// are safe for concurrent use. Zero external cost: pure in-memory counters
// updated on the dispatch path the daemon already owns.
type writeHealthTracker struct {
	mu       sync.Mutex
	byWindow map[string]*windowHealthState
}

func newWriteHealthTracker() *writeHealthTracker {
	return &writeHealthTracker{byWindow: map[string]*windowHealthState{}}
}

// observe records one forwarded-action outcome for a window.
func (t *writeHealthTracker) observe(windowID, action string, ok bool) {
	if t == nil || windowID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.byWindow[windowID]
	if st == nil {
		st = &windowHealthState{}
		t.byWindow[windowID] = st
	}
	st.recent = append(st.recent, ok)
	if len(st.recent) > writeHealthWindow {
		st.recent = st.recent[len(st.recent)-writeHealthWindow:]
	}
	if ok {
		st.consecFails = 0
	} else {
		st.consecFails++
		st.lastFailureAction = action
		st.lastFailureAt = time.Now().UTC()
	}
}

// forget drops a window's state (call when its connector goes away — a
// reconnected window starts clean, matching the stale-read guard's lifetime).
func (t *writeHealthTracker) forget(windowID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byWindow, windowID)
}

func snapshotLocked(st *windowHealthState) WindowWriteHealth {
	s := WindowWriteHealth{
		Samples:             len(st.recent),
		ConsecutiveFailures: st.consecFails,
		LastFailureAction:   st.lastFailureAction,
	}
	for _, ok := range st.recent {
		if !ok {
			s.Failures++
		}
	}
	if s.Samples > 0 {
		s.FailureRate = float64(s.Failures) / float64(s.Samples)
	}
	if !st.lastFailureAt.IsZero() {
		s.LastFailureAt = st.lastFailureAt.Format(time.RFC3339)
	}
	s.Degraded = (s.Samples >= writeHealthMinSamples && s.FailureRate >= writeHealthDegradedRate) ||
		s.ConsecutiveFailures >= writeHealthConsecFails
	return s
}

// snapshot returns one window's health (zero-value when never observed).
func (t *writeHealthTracker) snapshot(windowID string) WindowWriteHealth {
	if t == nil {
		return WindowWriteHealth{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.byWindow[windowID]
	if st == nil {
		return WindowWriteHealth{}
	}
	return snapshotLocked(st)
}

// all returns every observed window's health, for /health. nil when empty so
// the JSON field is omitted on a quiet daemon.
func (t *writeHealthTracker) all() map[string]WindowWriteHealth {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.byWindow) == 0 {
		return nil
	}
	out := make(map[string]WindowWriteHealth, len(t.byWindow))
	for id, st := range t.byWindow {
		out[id] = snapshotLocked(st)
	}
	return out
}

// annotateDegraded attaches the structured degraded advisory to a FAILED
// response when the window's rolling health says the connector is degraded.
// Purely additive — success responses and healthy windows are untouched.
// The advice encodes the fake-failure law: verify by light read before ANY
// client-side resend of a mutating action.
func (t *writeHealthTracker) annotateDegraded(req *protocol.Request, resp *protocol.Response) {
	if t == nil || req == nil || resp == nil || resp.OK {
		return
	}
	h := t.snapshot(req.WindowID)
	if !h.Degraded {
		return
	}
	if resp.Result == nil {
		resp.Result = map[string]any{}
	}
	resp.Result["degraded"] = map[string]any{
		"degraded":            true,
		"recentFailureRate":   h.FailureRate,
		"recentSamples":       h.Samples,
		"consecutiveFailures": h.ConsecutiveFailures,
	}
	advice := "connector looks DEGRADED under load (recent failure rate %.0f%%, %d consecutive failures)"
	if requestMutates(req) {
		advice += " — this write may have LANDED despite the failure (假失败定律): verify with a light read before resending, and do NOT blind-retry"
	} else {
		advice += " — insert a light read + short pause before retrying; consider `easyeda doc reload` if this persists"
	}
	resp.Warnings = append(resp.Warnings, fmt.Sprintf(advice, h.FailureRate*100, h.ConsecutiveFailures))
}

// dispatchFn matches (*conn).dispatch — injected so the retry protocol is
// unit-testable without a live WebSocket.
type dispatchFn func(ctx context.Context, req protocol.Request) (*protocol.Response, error)

// adaptiveHooks are the side effects of the retry protocol, injected for tests.
type adaptiveHooks struct {
	// observe records one attempt's outcome in the rolling health window.
	observe func(action string, ok bool)
	// auditFirst logs the superseded first attempt so audit-derived failure
	// rates keep seeing the degradation the retry papers over.
	auditFirst func(resp *protocol.Response, err error)
	sleep      func(d time.Duration)
}

// forwardWithAdaptiveRetry forwards one request; on failure of a whitelisted
// idempotent action it inserts a light read + settle delay and retries ONCE.
//
//	原失败 ──不在白名单──────────────────────────▶ 透传(上层按 degraded 提示自理)
//	原失败 ──白名单──▶ 轻读探测 ──探测也失败──▶ 透传(连接器停摆,不加压)
//	                        └──探测 OK──▶ settle ──▶ 重发一次(幂等,重发无害)
func forwardWithAdaptiveRetry(ctx context.Context, req protocol.Request, dispatch dispatchFn, h adaptiveHooks) (*protocol.Response, error, bool) {
	resp, err := dispatch(ctx, req)
	ok := err == nil && resp != nil && resp.OK
	if h.observe != nil {
		h.observe(req.Action, ok)
	}
	if ok || !retryableOnFailure[req.Action] {
		return resp, err, false
	}

	// Light-read probe: proves the connector is answering at all, and measurably
	// improves the retry's odds on a load-degraded connector.
	probe := protocol.Request{
		Envelope: protocol.Envelope{
			ID:        req.ID + "_probe",
			Type:      protocol.TypeRequest,
			Version:   req.Version,
			WindowID:  req.WindowID,
			CreatedAt: time.Now().UTC(),
		},
		Action: backoffProbeAction,
	}
	pctx, cancel := context.WithTimeout(ctx, backoffProbeTimeout)
	presp, perr := dispatch(pctx, probe)
	cancel()
	if perr != nil || presp == nil || !presp.OK {
		// Cannot even read → the connector is wedged; pass the original failure
		// through rather than piling more load on (恢复探测用轻读非重发).
		return resp, err, false
	}

	if h.auditFirst != nil {
		h.auditFirst(resp, err)
	}
	if h.sleep != nil {
		h.sleep(backoffSettleDelay)
	}
	resp2, err2 := dispatch(ctx, req)
	ok2 := err2 == nil && resp2 != nil && resp2.OK
	if h.observe != nil {
		h.observe(req.Action, ok2)
	}
	if ok2 {
		resp2.Warnings = append(resp2.Warnings, fmt.Sprintf(
			"%s failed once and was auto-retried by the daemon after a light read + %s settle (connector load degradation, idempotent action)",
			req.Action, backoffSettleDelay))
	}
	return resp2, err2, true
}
