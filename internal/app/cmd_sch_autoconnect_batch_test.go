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
					// U1 的 GND 脚在 y=62、朝下引出。「U1 选 down」是本用例的前提,
					// 但它曾经只是评分巧合 —— predictedMarkerBBox 每次变精确
					// (up/down 订正、netport 宽度跟网名、并入文字带)U1 就改选一次,
					// fixture 跟着调了两轮。现在改用**几何强制**:左右两侧的异网线
					// 把 left/right 硬拒死,up 撞进 U1 自己的 bbox,down 成为唯一
					// 干净方向。这样评分再怎么精确,用例验的仍是批内互斥本身。
					"bbox": map[string]any{"minX": 0.0, "minY": 65.0, "maxX": 20.0, "maxY": 93.0},
					"pins": []any{
						map[string]any{"pinNumber": "1", "pinName": "GND", "x": 10.0, "y": 62.0, "net": ""},
					},
				},
				map[string]any{
					"componentType": "part", "designator": "U2",
					// maxY 12:给 U1 朝下的 ground **本体 + 文字带**(文字带落在
					// 12.5..24.5)一起让开 —— 判定侧 marker-overlap 算的就是
					// 「本体 ∪ 文字带」,预测侧现在同尺,所以这里必须按合并后的
					// 范围留白,否则 U1 会改选方向、冲突消失。
					"bbox": map[string]any{"minX": 0.0, "minY": 0.0, "maxX": 20.0, "maxY": 12.0},
					"pins": []any{
						map[string]any{"pinNumber": "1", "pinName": "VCC", "x": 10.0, "y": 30.0, "net": ""},
					},
				},
			},
				// 左右两道异网线:把两个器件的 left/right 通道**在任何 offset 上**
				// 都堵死(含扩展档位),于是方向选择由几何决定而非评分权重。
				"wires": []any{
					map[string]any{"x0": -2.0, "y0": -300.0, "x1": -2.0, "y1": 300.0, "net": "X"},
					map[string]any{"x0": 22.0, "y0": -300.0, "x1": 22.0, "y1": 300.0, "net": "X"},
				},
			}
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
	a := predictedMarkerBBox(first.EndPoint.X, first.EndPoint.Y, "net_port_bi", first.Direction, "N1")
	b := predictedMarkerBBox(second.EndPoint.X, second.EndPoint.Y, "net_port_bi", second.Direction, "N1")
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
						// maxY 12:给 U1 朝下的 ground **本体 + 文字带**一起让开
						// (文字带落在 12.5..24.5),U1 才会选 down 从而**占掉 up
						// 通道**——这条用例要验的正是「四个方向全堵死时大声失败」。
						"bbox": map[string]any{"minX": 0.0, "minY": 0.0, "maxX": 20.0, "maxY": 12.0},
						"pins": []any{
							map[string]any{"pinNumber": "1", "pinName": "VCC", "x": 10.0, "y": 30.0, "net": ""},
						},
					},
				},
				// Existing foreign-net wires box in U2:1's left, right and down
				// corridors; only "up" is geometrically clean — until U1:1's
				// batch stub claims it.
				"wires": []any{
					// 三面围栏开到 ±300:桩线在密集区可以拉长到 3×OffsetMax,短围栏
					// 会被长桩线绕过去,这条用例就从「无路可走必须失败」退化成
					// 「绕远了但成功」。围栏必须比最长的候选桩线还长,语义才成立。
					// 横线放在 y=20(pin 30 与最短端点 12 之间),于是**每一个** down
					// 候选都必须穿过它。
					map[string]any{"x0": -2.0, "y0": -300.0, "x1": -2.0, "y1": 300.0, "net": "X"},
					map[string]any{"x0": 22.0, "y0": -300.0, "x1": 22.0, "y1": 300.0, "net": "X"},
					map[string]any{"x0": -300.0, "y0": 20.0, "x1": 300.0, "y1": 20.0, "net": "X"},
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

// ── 密集区扩展 offset(marker 错开)──────────────────────────────────────────
//
// 常规档位(18..80)全被已有 marker 占满时,评分器过去只能「挑一个最不差的」——
// 挑出来的就是一处真实标签重叠。现在它会把桩线拉长继续找。

// 造一堵墙:把 pin 右侧常规 offset 能到的位置全用已有 flag 占住,
// 只在扩展区(>80)留一个空。左/上/下用 part 堵死,逼它必须向右找。
func TestPlanConnection_DenseArea_ExtendsBeyondOffsetMax(t *testing.T) {
	pin := acPin{X: 0, Y: 0, Designator: "U1", PinNumber: "1"}
	rules := defaultAutoconnectRules()
	// 围一圈已有 marker:上/下/左三面堵死,右面只堵到 x=100 —— 于是常规档位
	// (18..80)四个方向全撞,唯一干净的位置在 offset>90 的右侧扩展区。
	flags := []layoutBBox{
		{MinX: -300, MinY: -300, MaxX: 300, MaxY: -15}, // 下半平面
		{MinX: -300, MinY: 15, MaxX: 300, MaxY: 300},   // 上半平面
		{MinX: -300, MinY: -15, MaxX: -5, MaxY: 15},    // 左
		{MinX: 5, MinY: -15, MaxX: 100, MaxY: 15},      // 右(只堵到 100)
	}
	scene := acScene{Flags: flags}
	got := planConnection(pin, "gnd", "GND", scene, rules, 0)
	if len(got) == 0 {
		t.Fatal("没有候选")
	}
	best := got[0]
	if candidateCollidesWithMarker(best) {
		t.Fatalf("常规档位全被占时应拉长桩线找到干净位置,却仍选了重叠的: dir=%s off=%v",
			best.Direction, best.Offset)
	}
	if best.Offset <= rules.OffsetMax {
		t.Errorf("干净位置只存在于扩展区,选中的 offset=%v 应 > OffsetMax(%v)",
			best.Offset, rules.OffsetMax)
	}
}

// 只要常规档位里**还有一个**干净位置,就不许拉长 —— 扩展是兜底不是常态。
func TestPlanConnection_NotDense_StaysWithinOffsetMax(t *testing.T) {
	pin := acPin{X: 0, Y: 0, Designator: "U1", PinNumber: "1"}
	rules := defaultAutoconnectRules()
	got := planConnection(pin, "gnd", "GND", acScene{}, rules, 0)
	if len(got) == 0 {
		t.Fatal("没有候选")
	}
	if got[0].Offset > rules.OffsetMax {
		t.Errorf("空场景不该动用扩展档位: offset=%v", got[0].Offset)
	}
}

// 扩展档位照样过硬拒绝:拉长后的桩线碰到异网线,仍必须被 hard-reject
// (#64 的短路保护不能被「找干净标签位」绕过)。
func TestPlanConnection_ExtendedOffsetsStillHardRejected(t *testing.T) {
	pin := acPin{X: 0, Y: 0, Designator: "U1", PinNumber: "1"}
	rules := defaultAutoconnectRules()
	flags := []layoutBBox{ // 连扩展区也堵满 → 必然全撞,一定会动用扩展档位
		{MinX: -400, MinY: -400, MaxX: 400, MaxY: -15},
		{MinX: -400, MinY: 15, MaxX: 400, MaxY: 400},
		{MinX: -400, MinY: -15, MaxX: -5, MaxY: 15},
		{MinX: 5, MinY: -15, MaxX: 400, MaxY: 15},
	}
	// 一条横跨的异网线,任何向右的桩线都会碰到它
	wires := []wireSegment{{X0: 5, Y0: -50, X1: 5, Y1: 50, Net: "OTHER"}}
	got := planConnection(pin, "gnd", "GND", acScene{Flags: flags, Wires: wires}, rules, 0)
	for _, c := range got {
		if c.Direction == "right" && !candidateHardRejected(c) {
			t.Fatalf("向右的桩线穿过异网线却没被硬拒绝: off=%v", c.Offset)
		}
	}
}
