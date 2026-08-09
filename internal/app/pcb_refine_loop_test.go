package app

// pcb_refine_loop_test.go — runRefineLoop 环本体的端到端测试（离线，有状态假 daemon）。
//
// 为什么必须有这层：pcb_refine_test.go 的 11 个用例全是纯函数（budgetMoves /
// planGridSnap / buildImmovableSet），而 #153 换来的三条护栏——「新增 finding 就
// 回滚」「按步回滚」「回读证实才算 restored」——全部活在 runRefineLoop 与 daemon
// 的交互序列里，纯函数测试一条都覆盖不到。这里照 pcb_sync_designators_test.go 的
// syncTestDaemon 范式造一个**有状态**假 daemon：器件表是活的，modify 真改它、
// 后续 components.list 读到改后的值 —— 断言因此能覆盖「写入 → 回读验证」全链路，
// 判定标准是回读一致，而不是 modify 返回 ok。
//
// 板子刻意设计成：C1..C4 全部带 0.5mil 的亚 mil 漂移（#153 实测的 635.0015 同款
// 脏值），tidy 维因 off-grid 子规则跌到 ~57 分 < 85 阈值 → 环必然规划一步
// grid-snap。其余维度分数如何无所谓——只有 tidy 有变换器，别的维只会产生
// 「没有对症手段」的警告，不影响这里的断言。

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// refineFakePad 是假器件的一个焊盘。dx/dy 是相对 anchor 的偏移 —— 器件被 modify
// 挪走时焊盘要跟着走，否则「回读」读到的是一具焊盘留在原地的尸体，快照会自相矛盾。
type refineFakePad struct {
	number, net string
	layer       int
	dx, dy      float64
	w, h        float64
}

// refineFakeComp 是活器件表里的一行。bbox 同样存相对 anchor 的偏移，序列化时叠加
// —— 这样 modify 只需要写 x/y，几何自动保持一致（真连接器也是整体平移）。
type refineFakeComp struct {
	id, designator string
	x, y           float64
	rotation       float64
	layer          int
	locked         bool
	bbox           [4]float64 // minX,minY,maxX,maxY 相对 anchor
	pads           []refineFakePad
}

func (c *refineFakeComp) toMap() map[string]any {
	pads := make([]any, 0, len(c.pads))
	for _, p := range c.pads {
		pads = append(pads, map[string]any{
			"padNumber": p.number, "net": p.net, "layer": p.layer,
			"x": c.x + p.dx, "y": c.y + p.dy, "width": p.w, "height": p.h,
		})
	}
	return map[string]any{
		"primitiveId": c.id, "designator": c.designator, "layer": c.layer,
		"x": c.x, "y": c.y, "rotation": c.rotation, "locked": c.locked,
		"bbox": map[string]any{
			"minX": c.x + c.bbox[0], "minY": c.y + c.bbox[1],
			"maxX": c.x + c.bbox[2], "maxY": c.y + c.bbox[3],
		},
		"pads": pads,
	}
}

// refineModifyCall 记录一次 pcb.component.modify 请求（含失败的 —— attempted 的
// 语义正是「下发过就算」）。
type refineModifyCall struct {
	id   string
	x, y float64
}

// refineLoopDaemon 是精修环的有状态假 daemon。
type refineLoopDaemon struct {
	srv      *httptest.Server
	comps    []*refineFakeComp
	modifies []refineModifyCall

	// modified 在第一次 modify 后置位，injectTracksAfterModify 用它决定何时开始
	// 往 pcb.line.list 里塞一对间距违规的异网轨 —— 模拟「变换制造了新 finding」
	// （#153 silk-align 把 silk-over-pad 从 0 推到 3 的同款事故）。
	modified                bool
	injectTracksAfterModify bool
	// driftMil 让每次 modify「返回 ok 但落点带偏差」—— 平台写 API 假成功的前科
	// （delete 静默 no-op / setState 不 done）在坐标上的翻版。回读验证必须抓住它。
	driftMil float64
	// failOnID 让该 primitiveId 的 modify 返回失败且不落。
	failOnID string
	// failLineListAfterModify 让 modify 之后的 pcb.line.list 读失败 ——
	// countGateableFindings 拿不到 check 报告返回 -1，护栏必须按「无法复核 =
	// 保守回滚」处理，而不是让 -1 > before 恒假地静默放行。
	failLineListAfterModify bool
}

func newRefineLoopDaemon(t *testing.T, comps []*refineFakeComp) (*appConfig, *refineLoopDaemon, func()) {
	t.Helper()
	// workflow 状态重定向到临时目录：resolveStageProject/loadPcbStageState 这条
	// 分支要真跑，但绝不能读写真用户的 ~/.easyeda-agent/workflow。
	t.Setenv(workflow.EnvDir, t.TempDir())

	d := &refineLoopDaemon{comps: comps}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"service":"easyeda-agent","status":"ok","windows":[{"windowId":"w1","context":{"projectName":"refinetest"}}]}`)
	})
	mux.HandleFunc("/action", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action  string         `json:"action"`
			Payload map[string]any `json:"payload"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		reply := func(result map[string]any) {
			out, _ := json.Marshal(map[string]any{"ok": true, "result": result})
			_, _ = w.Write(out)
		}
		switch req.Action {
		case "pcb.components.list":
			rows := make([]any, 0, len(d.comps))
			for _, c := range d.comps {
				rows = append(rows, c.toMap())
			}
			reply(map[string]any{"components": rows})
		case "pcb.outline.get":
			reply(map[string]any{"bbox": map[string]any{
				"minX": 0.0, "minY": 0.0, "maxX": 2000.0, "maxY": 2000.0,
			}})
		case "pcb.layers.list":
			reply(map[string]any{"copperLayerCount": 2, "layers": []any{}})
		case "pcb.silk.list":
			reply(map[string]any{"texts": []any{}})
		case "pcb.line.list":
			if d.failLineListAfterModify && d.modified {
				fmt.Fprint(w, `{"ok":false,"error":{"code":"EDA_CALL_FAILED","message":"check backend gone"}}`)
				return
			}
			lines := []any{}
			if d.injectTracksAfterModify && d.modified {
				// 两条 12mil 宽的异网轨中心距 4mil：边距 4−6−6 = −8 < 6mil 间距规则
				// → 恰好 1 条 clearance ERROR（可门控 finding 从 0 升到 ≥1）。
				lines = []any{
					map[string]any{"primitiveId": "t1", "net": "NETA", "layer": 1,
						"startX": 1500.0, "startY": 1500.0, "endX": 1600.0, "endY": 1500.0, "lineWidth": 12.0},
					map[string]any{"primitiveId": "t2", "net": "NETB", "layer": 1,
						"startX": 1500.0, "startY": 1504.0, "endX": 1600.0, "endY": 1504.0, "lineWidth": 12.0},
				}
			}
			reply(map[string]any{"lines": lines, "arcs": []any{}})
		case "pcb.component.modify":
			pid, _ := req.Payload["primitiveId"].(string)
			patch, _ := req.Payload["patch"].(map[string]any)
			px, _ := patch["x"].(float64)
			py, _ := patch["y"].(float64)
			d.modifies = append(d.modifies, refineModifyCall{id: pid, x: px, y: py})
			if pid == d.failOnID {
				fmt.Fprint(w, `{"ok":false,"error":{"code":"EDA_CALL_FAILED","message":"boom"}}`)
				return
			}
			for _, c := range d.comps {
				if c.id == pid {
					c.x = px + d.driftMil
					c.y = py + d.driftMil
				}
			}
			d.modified = true
			reply(map[string]any{})
		default:
			// via/fill/pour/region/drc.rules 等读一律返回空 —— 对应「板上没有这类
			// 图元」，pcb check 的全部 LIVE 规则据此得 0 finding。
			reply(map[string]any{})
		}
	})
	d.srv = httptest.NewServer(mux)
	host, port := splitHostPortForTest(t, d.srv.URL)
	// project 显式给定：resolveStageProject 直接用它当 workflow key，不再发
	// project.current（与 CLI 上 --project 的真实路径一致）。
	cfg := &appConfig{host: host, ports: port + "-" + port, project: "refinetest"}
	return cfg, d, d.srv.Close
}

func (d *refineLoopDaemon) comp(id string) *refineFakeComp {
	for _, c := range d.comps {
		if c.id == id {
			return c
		}
	}
	return nil
}

// refineLoopComp 造一个 2 焊盘的 C 类件。两焊盘中心距 100mil（整 mil = 英制），
// 且脚数 < 4 —— 双保险避开公制间距件的吸栅豁免，保证它参与 grid-snap。
func refineLoopComp(id, des string, x, y float64, locked bool, netA, netB string) *refineFakeComp {
	return &refineFakeComp{
		id: id, designator: des, x: x, y: y, layer: 1, locked: locked,
		bbox: [4]float64{-60, -30, 60, 30},
		pads: []refineFakePad{
			{number: "1", net: netA, layer: 1, dx: -50, w: 20, h: 20},
			{number: "2", net: netB, layer: 1, dx: 50, w: 20, h: 20},
		},
	}
}

// refineLoopBoard 是基准板：C1..C4 全部带 (0.5, 0.5) mil 漂移（5mil 栅的典型脏值，
// 位移 ~0.71mil，> 0.01 的浮点噪声下限、< 5mil 预算上限）。四件同 Y 成一行且步距
// 恒 300mil、朝向全 0 —— tidy 的 rotation/array 子规则都干净，唯一的病灶是
// off-grid，环的「对症」诊断因此是确定的。
func refineLoopBoard() []*refineFakeComp {
	return []*refineFakeComp{
		refineLoopComp("p1", "C1", 300.5, 300.5, false, "N1", "N2"),
		refineLoopComp("p2", "C2", 600.5, 300.5, false, "N2", "N3"),
		refineLoopComp("p3", "C3", 900.5, 300.5, false, "N3", "N4"),
		refineLoopComp("p4", "C4", 1200.5, 300.5, false, "N4", "N5"),
	}
}

// runRefineForTest 统一跑环：默认护栏参数，只翻 DryRun。
func runRefineForTest(t *testing.T, cfg *appConfig, dryRun bool) refineReport {
	t.Helper()
	opts := defaultRefineOpts()
	opts.DryRun = dryRun
	rep, err := runRefineLoop(cfg, "", nil, opts, 0, io.Discard)
	if err != nil {
		t.Fatalf("runRefineLoop: %v", err)
	}
	return rep
}

func TestRefineLoop_DryRunSendsNoModify(t *testing.T) {
	// 默认约定是 dry-run（与本仓其它命令的 --dry-run 惯例相反且刻意）：
	// 计划要出全、一笔也不能落。
	cfg, d, done := newRefineLoopDaemon(t, refineLoopBoard())
	defer done()

	rep := runRefineForTest(t, cfg, true)
	if !rep.DryRun {
		t.Error("report must say it was a dry run")
	}
	if len(d.modifies) != 0 {
		t.Fatalf("dry run issued %d pcb.component.modify call(s) — it must issue none", len(d.modifies))
	}
	if len(rep.Steps) != 1 || rep.Steps[0].Name != "grid-snap" {
		t.Fatalf("want exactly one planned grid-snap step, got %+v", rep.Steps)
	}
	if got := len(rep.Steps[0].Moves); got != 4 {
		t.Errorf("planned moves = %d, want all 4 off-grid parts", got)
	}
	if rep.MovedParts != 4 {
		t.Errorf("MovedParts = %d, want the planned count 4 (dry run still reports scope)", rep.MovedParts)
	}
	if !strings.Contains(rep.Steps[0].Reason, "dry run") {
		t.Errorf("step reason must say nothing was moved, got %q", rep.Steps[0].Reason)
	}
	// 落地件必须原封不动。
	if c := d.comp("p1"); c.x != 300.5 || c.y != 300.5 {
		t.Errorf("dry run moved C1 to (%v,%v)", c.x, c.y)
	}
}

func TestRefineLoop_ApplyMovesSnapAndVerify(t *testing.T) {
	// 正常 --apply 路径：off-grid 件被吸到 5mil 栅上，modify 发出、回读一致、
	// 分数上升、无回滚。附带锁定件护栏：C5 同样漂移但 locked —— 一笔都不许动它
	// （锁是一个决定，不是布局缺陷）。
	board := append(refineLoopBoard(),
		refineLoopComp("p5", "C5", 1500.5, 300.5, true, "N5", "N6"))
	cfg, d, done := newRefineLoopDaemon(t, board)
	defer done()

	rep := runRefineForTest(t, cfg, false)
	if !rep.OK {
		t.Fatalf("clean apply must report OK, got %+v", rep)
	}
	if rep.Rounds != 1 || len(rep.Steps) != 1 {
		t.Fatalf("want one converged grid-snap round, got rounds=%d steps=%d", rep.Rounds, len(rep.Steps))
	}
	step := rep.Steps[0]
	if step.RolledBack {
		t.Fatalf("clean step must not roll back: %q", step.Reason)
	}
	if step.Applied != 4 || rep.MovedParts != 4 {
		t.Errorf("applied=%d moved=%d, want 4/4", step.Applied, rep.MovedParts)
	}
	if len(d.modifies) != 4 {
		t.Fatalf("issued %d modify call(s), want exactly 4", len(d.modifies))
	}
	for _, m := range d.modifies {
		if m.id == "p5" {
			t.Error("a locked part was moved — the immovable set failed")
		}
	}
	if rep.Immovable != 1 {
		t.Errorf("immovable count = %d, want the locked C5 alone", rep.Immovable)
	}
	// 判定标准是**落地状态**：活器件表必须停在格点上，不是 modify 返回 ok 就算。
	want := map[string]float64{"p1": 300, "p2": 600, "p3": 900, "p4": 1200}
	for id, wx := range want {
		c := d.comp(id)
		if c.x != wx || c.y != 300 {
			t.Errorf("%s landed at (%v,%v), want (%v,300)", c.designator, c.x, c.y, wx)
		}
	}
	if c := d.comp("p5"); c.x != 1500.5 {
		t.Errorf("locked C5 moved to x=%v", c.x)
	}
	if rep.ScoreAfter <= rep.ScoreBefore {
		t.Errorf("score must rise after snapping (tidy 病灶被清): %.1f → %.1f", rep.ScoreBefore, rep.ScoreAfter)
	}
}

func TestRefineLoop_NewFindingsRollBackTheStep(t *testing.T) {
	// #153 的硬护栏：变换后 pcb check 可门控 finding 上升 → 回滚该步，哪怕分数涨了。
	// 假 daemon 在第一次 modify 后往 pcb.line.list 注入一对间距违规的异网轨，
	// 模拟「动了件之后板上多了真问题」。
	cfg, d, done := newRefineLoopDaemon(t, refineLoopBoard())
	defer done()
	d.injectTracksAfterModify = true

	rep := runRefineForTest(t, cfg, false)
	if rep.OK {
		t.Fatal("a rolled-back step must flip the report to not-OK")
	}
	if len(rep.Steps) != 1 {
		t.Fatalf("want the single rolled-back step, got %d", len(rep.Steps))
	}
	step := rep.Steps[0]
	if !step.RolledBack || !strings.Contains(step.Reason, "findings rose") {
		t.Fatalf("step must roll back on the finding rise, got rolledBack=%v reason=%q", step.RolledBack, step.Reason)
	}
	if step.FindingsBefore != 0 || step.FindingsAfter < 1 {
		t.Errorf("findings %d → %d, want a rise from a clean 0", step.FindingsBefore, step.FindingsAfter)
	}
	if step.Restored != 4 || len(step.Errors) != 0 {
		t.Errorf("restored=%d errors=%v, want all 4 verified back with no errors", step.Restored, step.Errors)
	}
	if rep.MovedParts != 0 {
		t.Errorf("MovedParts = %d — a rolled-back step must not count as moved", rep.MovedParts)
	}
	// 回滚的判定标准同样是落地状态：件必须**真的**回到原位（含原来的脏漂移 ——
	// 回滚是还原，不是顺手美化）。
	for id, wx := range map[string]float64{"p1": 300.5, "p2": 600.5, "p3": 900.5, "p4": 1200.5} {
		c := d.comp(id)
		if c.x != wx || c.y != 300.5 {
			t.Errorf("%s not restored: at (%v,%v), want (%v,300.5)", c.designator, c.x, c.y, wx)
		}
	}
	// 4 笔 apply + 4 笔逆序回滚。
	if len(d.modifies) != 8 {
		t.Errorf("modify calls = %d, want 4 apply + 4 rollback", len(d.modifies))
	}
}

func TestRefineLoop_UnreadableCheckRollsBackConservatively(t *testing.T) {
	// countGateableFindings 的契约:读失败返回 -1 = 「无法复核」= 保守回滚。
	// 旧实现漏了这条 —— FindingsAfter=-1 > FindingsBefore 恒为假,check 读挂的
	// 那一步会静默通过护栏(审计④抓到的文档-实现不符,已修)。
	cfg, d, done := newRefineLoopDaemon(t, refineLoopBoard())
	defer done()
	d.failLineListAfterModify = true // modify 之后 check 读挂

	rep := runRefineForTest(t, cfg, false)
	if rep.OK {
		t.Fatal("an unverifiable step must flip the report to not-OK")
	}
	if len(rep.Steps) != 1 {
		t.Fatalf("want the single rolled-back step, got %d", len(rep.Steps))
	}
	step := rep.Steps[0]
	if !step.RolledBack || !strings.Contains(step.Reason, "unreadable") {
		t.Fatalf("step must roll back conservatively when check is unreadable, got rolledBack=%v reason=%q", step.RolledBack, step.Reason)
	}
	if step.FindingsAfter != -1 {
		t.Errorf("FindingsAfter = %d, want the honest -1", step.FindingsAfter)
	}
	if rep.MovedParts != 0 {
		t.Errorf("MovedParts = %d — an unverified step must not count as moved", rep.MovedParts)
	}
	// 件必须真的回到原位。
	for id, wx := range map[string]float64{"p1": 300.5, "p2": 600.5, "p3": 900.5, "p4": 1200.5} {
		c := d.comp(id)
		if c.x != wx || c.y != 300.5 {
			t.Errorf("%s not restored: at (%v,%v), want (%v,300.5)", c.designator, c.x, c.y, wx)
		}
	}
}

func TestRefineLoop_DriftedRollbackIsNotReportedRestored(t *testing.T) {
	// 回读漂移的诚实性：modify 返回 ok 但落点带 0.2mil 偏差（> refineCoordEps
	// 0.05）。回滚的「还原」必须以回读为准 —— 偏着落的件一个都不能算 restored，
	// 且逐件报错。平台写 API「假成功」的前科（delete 静默 no-op）就是这条的出处。
	cfg, d, done := newRefineLoopDaemon(t, refineLoopBoard())
	defer done()
	d.injectTracksAfterModify = true // 触发回滚路径
	d.driftMil = 0.2

	rep := runRefineForTest(t, cfg, false)
	if rep.OK {
		t.Fatal("report must not be OK")
	}
	step := rep.Steps[0]
	if !step.RolledBack {
		t.Fatalf("want the rollback path, got %+v", step)
	}
	if step.Restored != 0 {
		t.Errorf("restored = %d — a drifted landing must NOT be counted restored", step.Restored)
	}
	if len(step.Errors) != 4 {
		t.Fatalf("want one honesty error per unconfirmed part, got %v", step.Errors)
	}
	for _, e := range step.Errors {
		if !strings.Contains(e, "not confirmed") {
			t.Errorf("error must say the restore was not confirmed by read-back, got %q", e)
		}
	}
	// 落地状态与 report 一致：件确实偏在原位 0.2mil 之外。
	if c := d.comp("p1"); math.Abs(c.x-300.5) <= refineCoordEps {
		t.Errorf("test setup: drift did not land, p1 at %v", c.x)
	}
}

func TestRefineLoop_BlockingIssuesSurfaceInWarnings(t *testing.T) {
	// blocking(短路/重叠/出板框)不归精修管，但报告必须显眼说出来 —— 否则
	// 「refine OK」会被读成「板子没问题」，而它带着出板框的件。J9 的焊盘落在
	// 板框外（x=-100 < 0）→ layout-lint 判 off-board → 1 条 blocking。
	board := append(refineLoopBoard(), &refineFakeComp{
		id: "p9", designator: "J9", x: -50, y: 1000, layer: 1,
		bbox: [4]float64{-60, -30, 60, 30},
		pads: []refineFakePad{{number: "1", net: "N9", layer: 1, dx: -50, w: 20, h: 20}},
	})
	cfg, _, done := newRefineLoopDaemon(t, board)
	defer done()

	rep := runRefineForTest(t, cfg, true)
	if rep.Blocking != 1 {
		t.Fatalf("blocking = %d, want the single off-board finding", rep.Blocking)
	}
	joined := strings.Join(rep.Warnings, "\n")
	if !strings.Contains(joined, "blocking issue") || !strings.Contains(joined, "place-constrained") {
		t.Errorf("warnings must call out the blocking issues AND point at the fix path, got %q", joined)
	}
	if !strings.Contains(rep.Summary, "UNTOUCHED") {
		t.Errorf("summary must say blocking issues remain untouched, got %q", rep.Summary)
	}
}

func TestRefineLoop_ConfirmedTierPartsAreImmovable(t *testing.T) {
	// 已签字的 tier-1/2 是不可动集合（#153：「stage confirm-tier 已确认的功能位
	// 一律不动」）。写一份带 tier-2 签字（C2）的 workflow 状态到重定向后的目录，
	// 验证环真的读它并把 C2 从计划里剔除。
	cfg, d, done := newRefineLoopDaemon(t, refineLoopBoard())
	defer done()
	st := &pcbStageState{Project: "refinetest", Confirmed: map[pcbStage]bool{}}
	st.ConfirmTier(2, &stageTierConfirm{At: nowRFC3339(), Designators: []string{"C2"}})
	if err := savePcbStageState(st); err != nil {
		t.Fatalf("seed workflow state: %v", err)
	}

	rep := runRefineForTest(t, cfg, true)
	if rep.Immovable != 1 {
		t.Fatalf("immovable = %d, want the tier-2 C2 alone", rep.Immovable)
	}
	if len(rep.Steps) != 1 || len(rep.Steps[0].Moves) != 3 {
		t.Fatalf("want a 3-move plan (C2 excluded), got %+v", rep.Steps)
	}
	for _, m := range rep.Steps[0].Moves {
		if m.Designator == "C2" {
			t.Error("tier-2 confirmed C2 must not be planned for a move")
		}
	}
	if strings.Contains(strings.Join(rep.Warnings, "\n"), "no confirmed placement tiers") {
		t.Error("tier state was found — the no-tiers degradation warning must not fire")
	}
	_ = d
}

func TestRefineLoop_ApplyFailureRollsBackAttempted(t *testing.T) {
	// 中途失败：C3 的 modify 返回失败（且不落）。与既有 auto-place「append
	// failures 继续跑」的混合态不同，环必须立刻停下并回滚**已下发**的全部 ——
	// C1/C2 回原位、C4 根本不该被下发。
	cfg, d, done := newRefineLoopDaemon(t, refineLoopBoard())
	defer done()
	d.failOnID = "p3"

	rep := runRefineForTest(t, cfg, false)
	if rep.OK {
		t.Fatal("an aborted apply must not report OK")
	}
	step := rep.Steps[0]
	if !step.RolledBack || !strings.Contains(step.Reason, "apply failed") {
		t.Fatalf("want an apply-failed rollback, got rolledBack=%v reason=%q", step.RolledBack, step.Reason)
	}
	if step.Applied != 2 {
		t.Errorf("applied = %d, want the 2 that landed before the failure", step.Applied)
	}
	// attempted 含失败的 C3（下发前先入待回滚集）：它没动过，回读证实在原位也算
	// restored —— 回滚的口径是「回读 == 原位」，不是「回滚写成功」。C3 的回滚写
	// 同样失败，进 errors 点名。
	if step.Restored != 3 {
		t.Errorf("restored = %d, want C1+C2+C3 all read back at their originals", step.Restored)
	}
	if len(step.Errors) != 1 || !strings.Contains(step.Errors[0], "C3") {
		t.Errorf("the failed rollback write must be named, got %v", step.Errors)
	}
	// C4 排在失败件之后：绝不能有它的 modify（既没 apply 也不需要回滚）。
	for _, m := range d.modifies {
		if m.id == "p4" {
			t.Error("C4 was touched — apply must stop at the first failure")
		}
	}
	for id, wx := range map[string]float64{"p1": 300.5, "p2": 600.5, "p3": 900.5, "p4": 1200.5} {
		if c := d.comp(id); c.x != wx {
			t.Errorf("%s at x=%v, want restored/untouched %v", c.designator, c.x, wx)
		}
	}
}
