package app

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

// ── 选中候选的带痕告警 + --strict 拒绝 ──────────────────────────────────────
//
// 真机事故:唯一的门是 score≥1e9 硬拒,软惩罚累加成千分照样静默入选 ——
// score=1737 的 down/78 长桩扎进邻组标签区,而 summarizeRejected 只报落选项。
// 修复后的契约:选中候选 score 超软阈值(100,依据:干净落点 -30~90,软惩罚起步
// 100+)或 reasons 含碰撞类时,结果行打 WARN;--strict 时直接失败不落地。

// 干净落点(bonus + 长度成本,score 落在 -30~90 区间)必须保持沉默。
func TestSelectedCandidateWarning_CleanIsSilent(t *testing.T) {
	c := acCandidate{Direction: "down", Offset: 24, Score: -27.6, Reasons: []acReason{
		{Cost: bonusOutwardSide, Desc: "direction matches pin outward side"},
		{Cost: bonusKindDefault, Desc: "kind default direction"},
		{Cost: 2.4, Desc: "offset cost"},
	}}
	if w := selectedCandidateWarning(c); w != "" {
		t.Errorf("干净候选不该告警: %q", w)
	}
	// 阈值边界:恰好 100 不告警(> 才算超)。
	if w := selectedCandidateWarning(acCandidate{Score: acScoreWarnThreshold}); w != "" {
		t.Errorf("恰好在阈值上不该告警: %q", w)
	}
}

// 真机案例的形状:score=1737(flag 碰撞 1000 + 长 offset + 折叠),必须点名 reasons。
func TestSelectedCandidateWarning_HighScoreWarnsWithReasons(t *testing.T) {
	c := acCandidate{Direction: "down", Offset: 78, Score: 1737, Reasons: []acReason{
		{Cost: costFlagCollision, Desc: "label collides with existing flag GND"},
		{Cost: costFoldedPort, Desc: "vertical netport folds its label"},
	}}
	w := selectedCandidateWarning(c)
	if w == "" {
		t.Fatal("score=1737 的选中候选必须告警")
	}
	if !strings.Contains(w, "落点带痕") || !strings.Contains(w, "建议挪件") {
		t.Errorf("告警措辞必须给出可执行建议: %q", w)
	}
	if !strings.Contains(w, "label collides with existing flag GND") {
		t.Errorf("告警必须点名碰撞类 reason: %q", w)
	}
}

// reasons 含碰撞类时,即使总分被 bonus 拉低到阈值之下也要告警 —— 碰撞是画布上
// 肉眼可见的破坏,不因总账好看而豁免。
func TestSelectedCandidateWarning_CollisionClassAloneWarns(t *testing.T) {
	c := acCandidate{Direction: "up", Offset: 18, Score: 60, Reasons: []acReason{
		{Cost: costOppositeSide, Desc: "exits the pin's back side"},
	}}
	if w := selectedCandidateWarning(c); w == "" || !strings.Contains(w, "exits the pin's back side") {
		t.Errorf("碰撞类 reason 必须触发告警并点名: %q", w)
	}
}

// applySelectionGate:默认档记 Warning 照连;--strict 档写 Error 拒绝落地。
func TestApplySelectionGate_WarnVsStrict(t *testing.T) {
	tainted := acCandidate{Direction: "down", Offset: 78, Score: 1737, Reasons: []acReason{
		{Cost: costFlagCollision, Desc: "label collides with existing flag GND"},
	}}
	t.Run("默认档打 WARN 照连", func(t *testing.T) {
		var cr acConnResult
		if !applySelectionGate(&cr, tainted, false) {
			t.Fatal("非 strict 档带痕候选仍应落地")
		}
		if cr.Warning == "" || cr.Error != "" {
			t.Errorf("应记 Warning 不记 Error: warning=%q error=%q", cr.Warning, cr.Error)
		}
	})
	t.Run("strict 档直接失败不落地", func(t *testing.T) {
		var cr acConnResult
		if applySelectionGate(&cr, tainted, true) {
			t.Fatal("--strict 下带痕候选必须拒绝落地")
		}
		if !strings.Contains(cr.Error, "--strict") || !strings.Contains(cr.Error, "落点带痕") {
			t.Errorf("strict 拒绝必须写明原因: %q", cr.Error)
		}
	})
	t.Run("干净候选两档都放行", func(t *testing.T) {
		var cr acConnResult
		if !applySelectionGate(&cr, acCandidate{Score: -20}, true) || cr.Error != "" || cr.Warning != "" {
			t.Errorf("干净候选在 strict 档也应无痕通过: %+v", cr)
		}
	})
}

// 端到端(fake daemon):整页被一面巨旗盖住 → 每个方向的候选都吃 costFlagCollision
// 软惩罚(无硬拒)。默认档:连上 + 结果带 Warning;--strict 档:拒绝且 connect_pin
// 一次都不发。
func TestAutoconnect_TaintedSelection_WarnAndStrict(t *testing.T) {
	newCfg := func(st *fakeSchState) (*appConfig, func()) {
		cfg, cleanup := newFakeSchDaemonWithScene(t, st)
		return cfg, cleanup
	}
	conns := []acConnSpec{{PinRef: "U1:1", Kind: "gnd", Net: "GND"}}

	t.Run("默认档 WARN 照连", func(t *testing.T) {
		st := &fakeSchState{}
		cfg, cleanup := newCfg(st)
		defer cleanup()
		var out bytes.Buffer
		rep, err := runAutoconnectOpts(cfg, "", conns, defaultAutoconnectRules(), acRunOpts{}, &out, &out)
		if err != nil {
			t.Fatalf("默认档应照连: %v\n%s", err, out.String())
		}
		if st.connectHit != 1 {
			t.Fatalf("默认档应真的落地, connect_pin hits=%d", st.connectHit)
		}
		if len(rep.Connections) != 1 || rep.Connections[0].Warning == "" {
			t.Errorf("结果必须带 Warning: %+v", rep.Connections)
		}
		if !strings.Contains(out.String(), "WARN") {
			t.Errorf("文本报告必须打出 WARN 行:\n%s", out.String())
		}
	})
	t.Run("strict 档拒绝不落地", func(t *testing.T) {
		st := &fakeSchState{}
		cfg, cleanup := newCfg(st)
		defer cleanup()
		var out bytes.Buffer
		rep, err := runAutoconnectOpts(cfg, "", conns, defaultAutoconnectRules(), acRunOpts{Strict: true}, &out, &out)
		if err == nil {
			t.Fatalf("--strict 下带痕候选必须失败\n%s", out.String())
		}
		if st.connectHit != 0 {
			t.Fatalf("--strict 拒绝时绝不能落地, connect_pin hits=%d", st.connectHit)
		}
		if len(rep.Connections) != 1 || !strings.Contains(rep.Connections[0].Error, "--strict") {
			t.Errorf("结果必须写明 strict 拒绝: %+v", rep.Connections)
		}
	})
}

// newFakeSchDaemonWithScene 在 newFakeSchDaemon 的单引脚场景上加一面盖住整页的
// 巨型 netflag,让每个方向/offset 的候选 label 都与它相撞(软惩罚,非硬拒)。
// 与 newFakeSchDaemon 同构的极简 fake daemon,只是 components.list 的场景可定制。
func newFakeSchDaemonWithScene(t *testing.T, st *fakeSchState) (*appConfig, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"service":"easyeda-agent","windows":[]}`))
			return
		}
		if r.URL.Path != "/action" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Action  string         `json:"action"`
			Payload map[string]any `json:"payload"`
		}
		body, _ := readAllBody(r)
		_ = json.Unmarshal(body, &req)

		var result map[string]any
		switch req.Action {
		case "schematic.components.list":
			result = map[string]any{"components": []any{
				map[string]any{
					"componentType": "part", "designator": "U1",
					"bbox": map[string]any{"minX": 0.0, "minY": 0.0, "maxX": 20.0, "maxY": 20.0},
					"pins": []any{
						map[string]any{"pinNumber": "1", "pinName": "GND", "x": 10.0, "y": 25.0, "net": st.pinNet},
					},
				},
				// 盖住整页的巨旗:所有候选 label 都吃 costFlagCollision(软),
				// 但没有异网导线/非目标引脚 → 不触发 1e9 硬拒。
				map[string]any{
					"componentType": "netflag",
					"bbox":          map[string]any{"minX": -500.0, "minY": -500.0, "maxX": 500.0, "maxY": 500.0},
				},
			}}
		case "schematic.power.connect_pin":
			st.connectHit++
			st.flagCount++
			st.wireCount++
			st.pinNet = asString(req.Payload["net"])
			result = map[string]any{"wirePrimitiveId": "w1", "flagPrimitiveId": "f1"}
		default:
			result = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, _ := strings.Cut(hostPort, ":")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := &appConfig{host: host, ports: fmt.Sprintf("%d-%d", port, port)}
	return cfg, srv.Close
}
