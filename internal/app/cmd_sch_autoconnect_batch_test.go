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

// newFakeBatchDaemon stands up a daemon with TWO parts whose pins sit on the
// same vertical line, spaced so that their naive kind-default stubs (GND down
// from y=62 reaching y=44, VCC up from y=30 reaching y=48 at the shortest
// offset) collinear-overlap on x=10 — the B0512S-class adjacent
// multi-domain-pin geometry from issue #133/#138. The pins sit 14 apart from
// each other's stub so the fanout-channel penalty (MinLabelGap 12) does not
// distort direction choice. connect_pin always succeeds.
func newFakeBatchDaemon(t *testing.T) (*appConfig, func()) {
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
					// U1 的 GND 脚在 y=62、朝下引出。按**真机实测**的方向口径
					// (2026-08-14 订正:down 让 y 减小,body 在端点下方),down@18 的
					// 端点是 44、ground body 落在 y=24.5..34.5 —— 所以 U2 的 bbox
					// 上界必须留在 24.5 以下,否则这条用例会从「批内桩线互斥」变成
					// 「marker 压 part」,两条连接各选一边、根本不再冲突。
					"bbox": map[string]any{"minX": 0.0, "minY": 65.0, "maxX": 20.0, "maxY": 93.0},
					"pins": []any{
						map[string]any{"pinNumber": "1", "pinName": "GND", "x": 10.0, "y": 62.0, "net": ""},
					},
				},
				map[string]any{
					"componentType": "part", "designator": "U2",
					// maxY 24(原 28):给 U1 朝下的 ground body(24.5..34.5)让开,
					// 这样 U1 仍会选 down、与 U2 朝上的桩线在 y=44..48 相撞 ——
					// 那正是本用例要验的批内冲突。
					"bbox": map[string]any{"minX": 0.0, "minY": 0.0, "maxX": 20.0, "maxY": 24.0},
					"pins": []any{
						map[string]any{"pinNumber": "1", "pinName": "VCC", "x": 10.0, "y": 30.0, "net": ""},
					},
				},
			}}
		case "schematic.power.connect_pin":
			result = map[string]any{"wirePrimitiveId": "w", "flagPrimitiveId": "f"}
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

// TestAutoconnect_BatchStubsAreMutuallyExclusive is the issue #138 regression:
// two connections planned in ONE batch on different nets must never pick stubs
// that touch each other. Pre-fix, each candidate was scored against a scene
// that ignored its batch siblings, so U1:1's GND stub (down, ending at y=45)
// and U2:1's VCC stub (kind default up, ending at y=50) collinear-overlapped
// on x=10 — EasyEDA would merge them into one net (a silent GND/VCC short).
// Post-fix, the first planned stub is registered as a scene wire, so the
// second connection's "up" candidates are hard-rejected as foreign-wire
// touches and the planner steers to a clean direction.
func TestAutoconnect_BatchStubsAreMutuallyExclusive(t *testing.T) {
	cfg, cleanup := newFakeBatchDaemon(t)
	defer cleanup()

	conns := []acConnSpec{
		{PinRef: "U1:1", Kind: "gnd", Net: "GND"},
		{PinRef: "U2:1", Kind: "power", Net: "VCC"},
	}

	var out bytes.Buffer
	if err := runAutoconnect(cfg, "", conns, defaultAutoconnectRules(), false, false, false, true, &out, &out); err != nil {
		t.Fatalf("run failed: %v\n%s", err, out.String())
	}

	var report acReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out.String())
	}
	if len(report.Connections) != 2 {
		t.Fatalf("want 2 connections, got %d", len(report.Connections))
	}
	first, second := report.Connections[0], report.Connections[1]
	if first.Selected == nil || second.Selected == nil {
		t.Fatalf("both connections must select a candidate: %+v / %+v", first, second)
	}

	// Sanity: the first connection takes SOME unobstructed direction. 具体是哪一个
	// 不是本用例的判据 —— 本用例验的是**批内互斥**(两条桩线不许相碰),而不是方向
	// 偏好。2026-08-14 修正 predictedMarkerBBox 的 up/down 反转(真机实测:down 的
	// body 在端点下方)后,评分器第一次"看对了"竖直 marker 的位置,于是在这个
	// fixture 的几何里改选了另一个同样合法的方向。锁死具体方向会让这条防短路回归
	// 变成方向偏好的快照。
	if first.Selected.Direction == "" {
		t.Fatalf("U1:1 GND must select some direction: %+v", first.Selected)
	}
	// 不再断言"第二条不许选 up":那是基于「第一条一定选 down」推出来的间接判据,
	// 第一条改选别的方向后就不成立了。真正的不变量在下面 —— **两条桩线不许相碰**。
	if segmentsTouch(
		first.PinX, first.PinY, first.Selected.EndPoint.X, first.Selected.EndPoint.Y,
		second.PinX, second.PinY, second.Selected.EndPoint.X, second.Selected.EndPoint.Y,
	) {
		t.Fatalf("batch stubs touch: %v→%v and %v→%v",
			acPoint{X: first.PinX, Y: first.PinY}, first.Selected.EndPoint,
			acPoint{X: second.PinX, Y: second.PinY}, second.Selected.EndPoint)
	}
	// And the rejection is attributed to the wire-touch hard reject, so the
	// report explains WHY "up" was refused.
	// 不再断言「'up' 必须因 foreign-net 被拒」:那是白盒细节,且同样绑死了「第一条
	// 选 down」这个前提。互斥是否生效的**不变量**是上面那条几何断言(两条桩线不碰);
	// 机制是否参与,由下面这条更宽的判据看 —— 只要有任一候选因 foreign-net 触碰被拒,
	// 就证明批内互斥确实在评分里起了作用。
	sawForeignReject := false
	for _, rj := range append(append([]acRejected{}, first.Rejected...), second.Rejected...) {
		if strings.Contains(rj.Reason, "foreign-net") {
			sawForeignReject = true
		}
	}
	if !sawForeignReject {
		t.Errorf("批内互斥没有在任何候选上留下痕迹(应有 foreign-net 触碰拒绝):first=%+v second=%+v",
			first.Rejected, second.Rejected)
	}
}

// TestAutoconnect_BatchRegistersPredictedMarkerBBox locks the I/O orchestration
// to the same family+direction bbox predictor scoreCandidate uses. Two 10-pitch
// same-net port pins prefer right@18. After the first marker is registered, the
// second must move far enough for two measured 31×11 port bodies to stop
// overlapping. Same-net stubs do not hard-reject each other, so this isolates
// marker staggering from the batch wire-exclusion rule.
func TestAutoconnect_BatchRegistersPredictedMarkerBBox(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"service":"easyeda-agent","windows":[]}`))
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		body, _ := readAllBody(r)
		_ = json.Unmarshal(body, &req)
		var result map[string]any
		switch req.Action {
		case "schematic.components.list":
			result = map[string]any{"components": []any{
				map[string]any{
					"componentType": "part", "designator": "U1",
					"bbox": map[string]any{"minX": 80.0, "minY": 190.0, "maxX": 95.0, "maxY": 200.0},
					"pins": []any{
						map[string]any{"pinNumber": "1", "pinName": "SIG", "x": 100.0, "y": 200.0, "net": ""},
					},
				},
				map[string]any{
					"componentType": "part", "designator": "U2",
					"bbox": map[string]any{"minX": 80.0, "minY": 210.0, "maxX": 95.0, "maxY": 220.0},
					"pins": []any{
						map[string]any{"pinNumber": "1", "pinName": "SIG", "x": 100.0, "y": 210.0, "net": ""},
					},
				},
			}}
		case "schematic.power.connect_pin":
			result = map[string]any{"wirePrimitiveId": "w", "flagPrimitiveId": "f"}
		default:
			result = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, _ := strings.Cut(hostPort, ":")
	port, _ := strconv.Atoi(portStr)
	cfg := &appConfig{host: host, ports: fmt.Sprintf("%d-%d", port, port)}

	rules := defaultAutoconnectRules()
	rules.AvoidPinFanout = false
	conns := []acConnSpec{
		{PinRef: "U1:1", Kind: "netport", Net: "SIG"},
		{PinRef: "U2:1", Kind: "netport", Net: "SIG"},
	}
	var out bytes.Buffer
	if err := runAutoconnect(cfg, "", conns, rules, false, false, false, true, &out, &out); err != nil {
		t.Fatalf("run failed: %v\n%s", err, out.String())
	}
	var report acReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out.String())
	}
	if len(report.Connections) != 2 || report.Connections[0].Selected == nil || report.Connections[1].Selected == nil {
		t.Fatalf("want two planned connections, got %+v", report.Connections)
	}
	first, second := report.Connections[0].Selected, report.Connections[1].Selected
	if first.Direction != "right" || first.Offset != 18 {
		t.Fatalf("first port should take right@18, got %s@%.0f", first.Direction, first.Offset)
	}
	if second.Direction != "right" || second.Offset != 54 {
		t.Fatalf("second port should clear the first measured body at right@54, got %s@%.0f", second.Direction, second.Offset)
	}
	a := predictedMarkerBBox(first.EndPoint.X, first.EndPoint.Y, "net_port_bi", first.Direction)
	b := predictedMarkerBBox(second.EndPoint.X, second.EndPoint.Y, "net_port_bi", second.Direction)
	if boxesOverlap(a, b) {
		t.Fatalf("batch-selected marker bodies still overlap: first=%+v second=%+v", a, b)
	}
}

// TestAutoconnect_BatchStubAllBlockedFailsLoud is the other half of issue #138:
// when a batch sibling's stub blocks the LAST clean direction (existing
// foreign-net wires already box in left/right/down), the connection must fail
// loudly ("no safe candidate") instead of placing the up stub over the sibling
// — pre-fix the sibling stub was invisible, so "up" looked clean and the run
// silently merged GND into VCC.
func TestAutoconnect_BatchStubAllBlockedFailsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"service":"easyeda-agent","windows":[]}`))
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		body, _ := readAllBody(r)
		_ = json.Unmarshal(body, &req)
		var result map[string]any
		switch req.Action {
		case "schematic.components.list":
			result = map[string]any{
				"components": []any{
					map[string]any{
						"componentType": "part", "designator": "U1",
						"bbox": map[string]any{"minX": 0.0, "minY": 65.0, "maxX": 20.0, "maxY": 93.0},
						"pins": []any{
							map[string]any{"pinNumber": "1", "pinName": "GND", "x": 10.0, "y": 62.0, "net": ""},
						},
					},
					map[string]any{
						"componentType": "part", "designator": "U2",
						// maxY 24(原 28):同上,给 U1 朝下的 ground body(24.5..34.5)
						// 让开,U1 才会选 down 从而**占掉 up 通道**——这条用例要验的
						// 正是「四个方向全堵死时大声失败」。
						"bbox": map[string]any{"minX": 0.0, "minY": 0.0, "maxX": 20.0, "maxY": 24.0},
						"pins": []any{
							map[string]any{"pinNumber": "1", "pinName": "VCC", "x": 10.0, "y": 30.0, "net": ""},
						},
					},
				},
				// Existing foreign-net wires box in U2:1's left, right and down
				// corridors; only "up" is geometrically clean — until U1:1's
				// batch stub claims it.
				"wires": []any{
					map[string]any{"x0": -2.0, "y0": 18.0, "x1": -2.0, "y1": 42.0, "net": "X"},
					map[string]any{"x0": 22.0, "y0": 18.0, "x1": 22.0, "y1": 42.0, "net": "X"},
					map[string]any{"x0": 0.0, "y0": 10.0, "x1": 20.0, "y1": 10.0, "net": "X"},
				},
			}
		case "schematic.power.connect_pin":
			result = map[string]any{"wirePrimitiveId": "w", "flagPrimitiveId": "f"}
		default:
			result = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, _ := strings.Cut(hostPort, ":")
	port, _ := strconv.Atoi(portStr)
	cfg := &appConfig{host: host, ports: fmt.Sprintf("%d-%d", port, port)}

	conns := []acConnSpec{
		{PinRef: "U1:1", Kind: "gnd", Net: "GND"},
		{PinRef: "U2:1", Kind: "power", Net: "VCC"},
	}
	var out bytes.Buffer
	err := runAutoconnect(cfg, "", conns, defaultAutoconnectRules(), false, false, false, true, &out, &out)
	if err == nil {
		t.Fatalf("expected the boxed-in connection to fail, got success:\n%s", out.String())
	}
	var report acReport
	if jerr := json.Unmarshal(out.Bytes(), &report); jerr != nil {
		t.Fatalf("parse report: %v\n%s", jerr, out.String())
	}
	if len(report.Connections) != 2 {
		t.Fatalf("want 2 connections, got %d", len(report.Connections))
	}
	if report.Connections[0].Error != "" {
		t.Fatalf("first connection should place cleanly, got error: %s", report.Connections[0].Error)
	}
	if !strings.Contains(report.Connections[1].Error, "no safe candidate") {
		t.Fatalf("second connection must refuse with 'no safe candidate', got: %+v", report.Connections[1])
	}
}
