package app

// stale_read_optin_gate_test.go — 写后回读放行位的离线回归(自带假 daemon)。
//
// 这里的假 daemon **真的实现那道门**(照 internal/daemon/stalereads.go 的状态机:
// pcb 域的写置位、doc reload 的 closeDocument / pour.rebuild 清位、置位期间的 pcb
// 域读一律拒 STALE_READ,除非请求带 forceReason)。所以断言的不是「代码里写了
// staleReadOptIn」,而是「这条命令在门下还能不能得出正确结论」—— 判据与落地同一把尺。
//
// 覆盖的是**前一轮漏掉的三处**加放行位本身的收窄规则:
//   - pcb refine 的回滚回读证实(rollbackRefineMoves)
//   - pcb clear 的 #121 收尾计数(runPcbClearVerified,dryRun 预览按读判)
//   - apply playbook 的 verify 块(跨进程内 CLI 再入的作用域放行位)

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ── 假 daemon:带 STALE_READ 门 ────────────────────────────────────────────

// gateFakeCall 记录一次到达 /action 的请求 —— forceReason 是本文件全部断言的核心
// 证据(放行位有没有真的落到线上)。
type gateFakeCall struct {
	Action      string
	ForceReason string
	Payload     map[string]any
}

type gateFakeDaemon struct {
	srv *httptest.Server

	mu     sync.Mutex
	calls  []gateFakeCall
	stale  string            // 非空 = 门关着,值是弄脏它的那个动作名
	refuse int               // 被门拒掉的次数
	result map[string]string // action → 原样返回的 result JSON

	// comps 是活器件表(id → x,y),让 modify → components.list 这条回读链真的自洽。
	comps map[string][2]float64
}

// handle 复刻 daemon 的三段式:先过门,再执行,最后更新门的状态。
func (d *gateFakeDaemon) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action      string         `json:"action"`
		Payload     map[string]any `json:"payload"`
		ForceReason string         `json:"forceReason"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)

	d.mu.Lock()
	d.calls = append(d.calls, gateFakeCall{Action: req.Action, ForceReason: req.ForceReason, Payload: req.Payload})

	dry, _ := req.Payload["dryRun"].(bool)
	isPcb := strings.HasPrefix(req.Action, "pcb.")
	mutates := isPcb && gateFakeMutates[req.Action] && !dry
	isRead := isPcb && !mutates

	// BLOCK:门关着 + 这是一次 pcb 读 + 没带 forceReason → 拒。
	if isRead && d.stale != "" && strings.TrimSpace(req.ForceReason) == "" {
		d.refuse++
		mutation := d.stale
		d.mu.Unlock()
		fmt.Fprintf(w, `{"ok":false,"error":{"code":"STALE_READ","message":%q}}`,
			req.Action+" —— PCB 自 "+mutation+" 后未 reload")
		return
	}

	// CLEAR / SET。
	switch {
	case req.Action == "pcb.pour.rebuild":
		d.stale = ""
	case req.Action == "debug.exec_js" && strings.Contains(fmt.Sprint(req.Payload["code"]), "closeDocument"):
		d.stale = ""
	case mutates && req.Action != "pcb.save":
		d.stale = req.Action
	}

	// 让 modify 真改器件表 —— 回读证实必须读到写下去的值,否则测的是空气。
	if req.Action == "pcb.component.modify" {
		pid, _ := req.Payload["primitiveId"].(string)
		patch, _ := req.Payload["patch"].(map[string]any)
		px, _ := patch["x"].(float64)
		py, _ := patch["y"].(float64)
		if _, ok := d.comps[pid]; ok {
			d.comps[pid] = [2]float64{px, py}
		}
	}

	res := d.result[req.Action]
	if req.Action == "pcb.components.list" {
		rows := make([]any, 0, len(d.comps))
		for id, xy := range d.comps {
			rows = append(rows, map[string]any{
				"primitiveId": id, "designator": strings.ToUpper(id), "layer": 1,
				"x": xy[0], "y": xy[1], "rotation": 0.0,
			})
		}
		b, _ := json.Marshal(map[string]any{"components": rows})
		res = string(b)
	}
	d.mu.Unlock()

	if res == "" {
		res = "{}"
	}
	fmt.Fprintf(w, `{"ok":true,"result":%s}`, res)
}

// gateFakeMutates 是本测试用的「哪些 pcb 动作写画布」表。刻意手写而不是从目录里
// 生成:它模拟的是 daemon 的判断,若两边都从同一份目录派生,表错了也测不出来。
var gateFakeMutates = map[string]bool{
	"pcb.component.modify": true,
	"pcb.page.clear":       true,
	"pcb.line.create":      true,
	"pcb.pour.create":      true,
	"pcb.pour.delete":      true,
	"pcb.pour.rebuild":     true,
	"pcb.save":             true,
	"pcb.stackup.set":      true,
	"pcb.import_changes":   true,
}

func newGateFakeDaemon(t *testing.T) (*appConfig, *gateFakeDaemon) {
	t.Helper()
	d := &gateFakeDaemon{
		result: map[string]string{},
		comps:  map[string][2]float64{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"service":"easyeda-agent","status":"ok","windows":[{"windowId":"w1","context":{"projectName":"gatetest"}}]}`)
	})
	mux.HandleFunc("/action", d.handle)
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)

	u := strings.TrimPrefix(d.srv.URL, "http://")
	i := strings.LastIndex(u, ":")
	if i < 0 {
		t.Fatalf("bad test server url %q", d.srv.URL)
	}
	host, port := u[:i], u[i+1:]
	return &appConfig{host: host, ports: port + "-" + port, project: "gatetest"}, d
}

// forcedFor 返回该动作最后一次调用带的 forceReason。
func (d *gateFakeDaemon) forcedFor(action string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.calls) - 1; i >= 0; i-- {
		if d.calls[i].Action == action {
			return d.calls[i].ForceReason, true
		}
	}
	return "", false
}

func (d *gateFakeDaemon) refusals() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refuse
}

// ── 1. 收窄规则(纯函数)────────────────────────────────────────────────

func TestStaleReadEligibleRequestNarrowing(t *testing.T) {
	cases := []struct {
		name    string
		action  string
		payload any
		want    bool
	}{
		{"pcb 读", "pcb.components.list", nil, true},
		{"pcb DRC 读", "pcb.drc.check", nil, true},
		{"pcb 写", "pcb.component.modify", map[string]any{"primitiveId": "p1"}, false},
		{"布线门下的写", "pcb.line.create", map[string]any{"net": "GND"}, false},
		{"布线门下的写 + 谎称 dryRun", "pcb.line.create", map[string]any{"dryRun": true}, false},
		{"原理图读(门管不着)", "schematic.components.list", nil, false},
		{"page.clear 实删", "pcb.page.clear", map[string]any{"dryRun": false}, false},
		{"page.clear 预览", "pcb.page.clear", map[string]any{"dryRun": true}, true},
		{"page.clear 无载荷", "pcb.page.clear", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := staleReadEligibleRequest(c.action, c.payload); got != c.want {
				t.Fatalf("staleReadEligibleRequest(%q) = %v, want %v", c.action, got, c.want)
			}
		})
	}
}

// staleReadOptIn 必须返回副本 —— 放行位外溢到共享 cfg 就等于「整条命令放行」,
// 那正是 stale_read_optin.go 开头论证要避免的东西。
func TestStaleReadOptInIsScopedCopy(t *testing.T) {
	cfg := &appConfig{host: "127.0.0.1"}
	scoped := staleReadOptIn(cfg, "只此一次")
	if scoped == cfg {
		t.Fatal("staleReadOptIn 返回了同一个指针 —— 放行位会外溢到本命令其余的读")
	}
	if cfg.staleReadReason != "" {
		t.Fatalf("共享 cfg 被改写了: %q", cfg.staleReadReason)
	}
	if scoped.staleReadReason != "只此一次" {
		t.Fatalf("副本没带上理由: %q", scoped.staleReadReason)
	}
	if same := staleReadOptIn(cfg, "   "); same != cfg {
		t.Fatal("空理由必须原样返回 cfg(不放行),不能造出一个「开了但没理由」的半吊子副本")
	}
}

// 放行位只对够格的请求落到线上:即便有人把副本误传进一个会写画布的调用,
// forceReason 也不许出现在线上(否则顺手解锁了布线阶段门)。
func TestStaleReadForceReasonRefusesToArmWrites(t *testing.T) {
	cfg := staleReadOptIn(&appConfig{}, "写后回读理由")
	if got := staleReadForceReason(cfg, "pcb.components.list", nil); got == "" {
		t.Fatal("pcb 读应当带上放行位")
	} else if !strings.HasPrefix(got, "写后回读: ") {
		t.Fatalf("放行理由缺少可 grep 的前缀: %q", got)
	}
	for _, a := range []string{"pcb.component.modify", "pcb.line.create", "pcb.via.create", "pcb.import_autoroute"} {
		if got := staleReadForceReason(cfg, a, map[string]any{"dryRun": true}); got != "" {
			t.Fatalf("%s 不该带 forceReason(会解锁布线门/是写动作),得到 %q", a, got)
		}
	}
}

// ── 2. pcb refine:回滚后的回读证实 ──────────────────────────────────────

// 前一轮停在 refine 之前,回滚路径(pcb_refine.go rollbackRefineMoves)的回读证实
// 被漏掉了。它是本函数**最坏**的失败形态:回滚其实做完了,回读被门拒 →
// restored=0 + "verification read failed" → 报出来的是「回滚没成功」。
func TestRollbackRefineMovesReadsBackThroughTheGate(t *testing.T) {
	cfg, d := newGateFakeDaemon(t)
	d.comps["p1"] = [2]float64{610, 310} // 已被挪走,等待回滚
	d.comps["p2"] = [2]float64{910, 310}
	// 先弄脏窗口:模拟 applyRefineMoves 刚发过一批 modify。
	d.mu.Lock()
	d.stale = "pcb.component.modify"
	d.mu.Unlock()

	attempted := []refineMove{
		{ID: "p1", Designator: "C1", FromX: 600, FromY: 300, ToX: 610, ToY: 310, HasOriginal: true},
		{ID: "p2", Designator: "C2", FromX: 900, FromY: 300, ToX: 910, ToY: 310, HasOriginal: true},
	}
	restored, errs := rollbackRefineMoves(cfg, "w1", attempted, io.Discard)
	if restored != 2 {
		t.Fatalf("restored = %d, want 2 —— 回滚证实读被 STALE_READ 门吞了,好状态被报成坏状态 (errs=%v)", restored, errs)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected rollback errors: %v", errs)
	}
	reason, ok := d.forcedFor("pcb.components.list")
	if !ok {
		t.Fatal("回滚没有发出回读 —— 「回读证实」的契约没兑现")
	}
	if reason == "" {
		t.Fatal("回滚的回读证实没带写后回读放行位")
	}
	if d.refusals() != 0 {
		t.Fatalf("有 %d 次读被门拒 —— 放行位没生效", d.refusals())
	}
}

// 负对照:去掉放行位(直接 requestAction)就一定被拒 —— 证明这道门在假 daemon 里
// 是真的会响,上面那条绿灯不是因为门根本没装。
func TestGateActuallyRefusesUnoptedRead(t *testing.T) {
	cfg, d := newGateFakeDaemon(t)
	d.mu.Lock()
	d.stale = "pcb.component.modify"
	d.mu.Unlock()

	_, err := requestAction(cfg, "pcb.components.list", "w1", nil)
	if err == nil {
		t.Fatal("门没有拦下未放行的 PCB 读 —— 这个测试装置本身是坏的")
	}
	if !isStaleRead(err) {
		t.Fatalf("错误没有被识别成 STALE_READ: %v", err)
	}
}

// ── 3. pcb clear:#121 的收尾计数(dryRun 预览按读判)────────────────────

func TestPcbClearVerifiedKeepsRemainingCountThroughTheGate(t *testing.T) {
	cfg, d := newGateFakeDaemon(t)
	d.result["pcb.page.clear"] = `{"total":0,"deleted":{}}`
	d.result["document.current"] = `{}`
	d.result["document.open"] = `{}`
	d.result["pcb.save"] = `{}`
	d.result["debug.exec_js"] = `{}`

	// document.current 要带 context(reloadDocumentByUUID 靠它拿 tabId/uuid)。
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"service":"easyeda-agent","status":"ok","windows":[{"windowId":"w1","context":{"projectName":"gatetest"}}]}`)
	})
	mux.HandleFunc("/action", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Action == "document.current" {
			d.mu.Lock()
			d.calls = append(d.calls, gateFakeCall{Action: probe.Action})
			d.mu.Unlock()
			fmt.Fprint(w, `{"ok":true,"result":{},"context":{"documentUuid":"doc1","documentType":"pcb","tabId":"tab1"}}`)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		d.handle(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u := strings.TrimPrefix(srv.URL, "http://")
	i := strings.LastIndex(u, ":")
	cfg.host, cfg.ports = u[:i], u[i+1:]+"-"+u[i+1:]

	payload, err := buildPcbClearPayload("", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runPcbClearVerified(cfg, "w1", payload, "", false, false, &out, io.Discard); err != nil {
		t.Fatalf("runPcbClearVerified: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("decode output: %v (%s)", err, out.String())
	}
	if _, ok := got["remainingAfterVerify"]; !ok {
		t.Fatal("remainingAfterVerify 不见了 —— #121 唯一的证实/证伪判据被 STALE_READ 门静默吃掉")
	}
	// 最后一次 page.clear 就是那次 dry-run 计数,它必须带着放行位。
	reason, _ := d.forcedFor("pcb.page.clear")
	if reason == "" {
		t.Fatal("收尾 dry-run 计数没带写后回读放行位")
	}
	if d.refusals() != 0 {
		t.Fatalf("有 %d 次读被门拒", d.refusals())
	}
}

// ── 4. playbook verify:跨进程内 CLI 再入的作用域放行位 ────────────────────

func TestDispatchStaleReadReasonScopeAndReach(t *testing.T) {
	cfg, d := newGateFakeDaemon(t)
	d.mu.Lock()
	d.stale = "pcb.import_changes"
	d.mu.Unlock()

	// 作用域内:读放得过去,且线上带着理由。
	func() {
		defer setDispatchStaleReadReason("verify 块回读")()
		if _, err := requestAction(cfg, "pcb.components.list", "w1", nil); err != nil {
			t.Fatalf("作用域内的读被拒了: %v", err)
		}
	}()
	if reason, _ := d.forcedFor("pcb.components.list"); reason == "" {
		t.Fatal("作用域全局没有落到线上")
	}

	// 作用域外:必须恢复成「拦」。放行位若泄漏到 verify 块之外,后面每一条命令的
	// PCB 读都在无声地读旧状态 —— 比一开始就没有放行位更糟。
	if _, err := requestAction(cfg, "pcb.nets.list", "w1", nil); !isStaleRead(err) {
		t.Fatalf("放行位泄漏出了作用域(pcb.nets.list err=%v)", err)
	}

	// 写动作即便在作用域内也不许被武装。
	func() {
		defer setDispatchStaleReadReason("verify 块回读")()
		_, _ = requestAction(cfg, "pcb.component.modify", "w1", map[string]any{"primitiveId": "p1", "patch": map[string]any{}})
	}()
	if reason, ok := d.forcedFor("pcb.component.modify"); ok && reason != "" {
		t.Fatalf("写动作被作用域全局武装了 forceReason=%q —— 会顺手解锁阶段门", reason)
	}
}

// ── 5. settleRead 对 STALE_READ 当场收手 ────────────────────────────────

func TestSettleReadStopsOnStaleRead(t *testing.T) {
	attempts := 0
	stale := &actionError{Action: "pcb.components.list", Code: staleReadCode, Message: "gate"}
	_, ok, err := settleRead(func() (int, bool, error) {
		attempts++
		return 0, false, stale
	})
	if ok {
		t.Fatal("STALE_READ 不该被判成读成功")
	}
	if attempts != 1 {
		t.Fatalf("重试了 %d 次 —— 门的拒绝是确定性的,重试只会把真因埋进「写没落地」", attempts)
	}
	if !isStaleRead(err) {
		t.Fatalf("真因没有原样交回调用方: %v", err)
	}

	// 对照:普通失败仍然重试满 settleAttempts 次(退回原语义,别把两类混成一类)。
	attempts = 0
	if _, _, err := settleRead(func() (int, bool, error) {
		attempts++
		return 0, false, fmt.Errorf("connector busy")
	}); err == nil {
		t.Fatal("普通失败应当把最后一次的错误带出来")
	}
	if attempts != settleAttempts {
		t.Fatalf("普通失败重试了 %d 次,want %d", attempts, settleAttempts)
	}
}
