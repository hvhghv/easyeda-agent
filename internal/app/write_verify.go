package app

// write_verify.go — 把命令侧的**回读结论**回流给 daemon 的写健康度(通道 B)。
//
// 背景:daemon 的 writeHealth 一开始只统计「调用返回码」,而 2026-08 那场端到端
// 的主要故障形态是**返回成功但画布没变**(6 件 place 只落地 1 件、prim-delete
// 报 ok 却有幸存者)和**返回失败但其实已落地**(connect_pin 报 dispatch failed,
// 回读发现连接已建好)。两类都让 failureRate 停在 0.05、degraded 一路绿。
//
// 工具侧其实早就在回读了 —— block-apply 的落地回读、connect 的 slow-landed 复核、
// zone-draw 的 landed-check —— 只是结论打印在 CLI 层就没了。这里把它们回传:
//
//	POST /writeverify {windowId|project, action, landed, notLanded, returnedOK?, requestId?}
//
// **只回传响应里挖不出来的结论**。连接器自己在 result 里报的东西(#151 的
// partial / notApplied、issue #164 的 survivedTotal)由 daemon 在转发路径上直接
// 内省(通道 A),这里再报一次只会重复计数。
//
// 纪律:纯遥测,best-effort —— 找不到 daemon、超时、HTTP 报错一律静默返回,
// 绝不让一次健康度上报把用户的命令搞失败。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// writeVerifyTimeout bounds the whole report (port scan + POST). Short on
// purpose: this runs at the tail of a command, and telemetry must never be the
// reason a command feels slow.
const writeVerifyTimeout = 2 * time.Second

// writeVerdict is one command's read-back conclusion about writes it issued.
// landed/notLanded are COUNTS: one verdict routinely covers a whole batch
// (block-apply 发 6 次 place,回读只找到 1 件 → landed=1 notLanded=5).
type writeVerdict struct {
	action    string
	requestID string // daemon-assigned request id, when the caller kept it
	source    string // the verifying command, for diagnostics ("sch block-apply")
	// returnedOK is what the verified call(s) RETURNED. false marks the
	// fake-failure shape (报失败但回读证明已落地) — the daemon counts those
	// separately instead of as failures.
	returnedOK bool
	landed     int
	notLanded  int
}

// writeVerifyBody builds the JSON payload. Pure, so the contract with the
// daemon's WriteVerification struct is unit-testable without a daemon.
func writeVerifyBody(cfg *appConfig, window string, v writeVerdict) map[string]any {
	body := map[string]any{
		"action":     v.action,
		"landed":     v.landed,
		"notLanded":  v.notLanded,
		"returnedOK": v.returnedOK,
	}
	if window != "" {
		body["windowId"] = window
	} else if cfg != nil && cfg.project != "" {
		// No windowId in hand: let the daemon resolve the project the same way
		// /action does (the ephemeral windowId churns on reconnect).
		body["project"] = cfg.project
	}
	if v.requestID != "" {
		body["requestId"] = v.requestID
	}
	if v.source != "" {
		body["source"] = v.source
	}
	return body
}

// reportWriteVerified posts a read-back verdict to the daemon. Best-effort:
// every failure path returns silently.
func reportWriteVerified(cfg *appConfig, window string, v writeVerdict) {
	if cfg == nil || v.action == "" || (v.landed <= 0 && v.notLanded <= 0) {
		return
	}
	if window == "" && cfg.project == "" {
		return // nothing to attribute the verdict to
	}
	portStart, portEnd, err := cfg.portRange()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeVerifyTimeout)
	defer cancel()
	scan := scanHealth(ctx, hostPortOptions{host: cfg.host, portStart: portStart, portEnd: portEnd})
	if scan.Found == nil {
		return
	}
	buf, err := json.Marshal(writeVerifyBody(cfg, window, v))
	if err != nil {
		return
	}
	url := fmt.Sprintf("http://%s:%d/writeverify", cfg.host, scan.Found.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}
