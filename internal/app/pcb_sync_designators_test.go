package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsPlaceholderDesignator(t *testing.T) {
	// 平台用 `<前缀>?` 表示未分配。前缀跟封装类别走，穷举不现实，所以只看 `?`。
	for _, d := range []string{"U?", "C?", "RF?", "BUZZER?", " C? "} {
		if !isPlaceholderDesignator(d) {
			t.Errorf("%q should read as a placeholder", d)
		}
	}
	for _, d := range []string{"U1", "C_SIMV", "J_VEH", "R_38B", "ANT1", ""} {
		if isPlaceholderDesignator(d) {
			t.Errorf("%q is a real designator, must not be treated as a placeholder", d)
		}
	}
}

// syncTestDaemon 造一个**有状态**的假 daemon：PCB 器件表是活的 —— modify 会真的
// 改它，后续 pcb.components.list 读到改后的状态。这样断言覆盖到「写入回读验证」：
// 修复的判定标准是读回一致，而不是 modify 返回 ok。
type syncTestDaemon struct {
	srv     *httptest.Server
	writes  []map[string]any // 每次 pcb.component.modify 的 patch
	saves   int              // pcb.save 次数
	pcb     []map[string]any // 活的 PCB 器件表
	schResp string
	failOn  string // 这个位号的写入返回失败（且不落）
	noopOn  string // 这个位号的写入返回 ok 但**不落**（平台静默 no-op 前科）
}

func newSyncTestDaemon(t *testing.T, pcbComponents []map[string]any, schResp string) (*appConfig, *syncTestDaemon, func()) {
	t.Helper()
	d := &syncTestDaemon{pcb: pcbComponents, schResp: schResp}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"service":"easyeda-agent","status":"ok","windows":[{"windowId":"w1","context":{"projectName":"t"}}]}`)
	})
	mux.HandleFunc("/action", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action  string         `json:"action"`
			Payload map[string]any `json:"payload"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		switch req.Action {
		case "pcb.components.list":
			out, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"components": d.pcb}})
			_, _ = w.Write(out)
		case "schematic.components.list":
			fmt.Fprint(w, d.schResp)
		case "pcb.component.modify":
			patch, _ := req.Payload["patch"].(map[string]any)
			d.writes = append(d.writes, patch)
			des, _ := patch["designator"].(string)
			if des == d.failOn {
				fmt.Fprint(w, `{"ok":false,"error":{"code":"EDA_CALL_FAILED","message":"boom"}}`)
				return
			}
			if des != d.noopOn { // noopOn：返回 ok 但状态不动
				pid, _ := req.Payload["primitiveId"].(string)
				for _, c := range d.pcb {
					if c["primitiveId"] == pid {
						c["designator"] = des
					}
				}
			}
			fmt.Fprint(w, `{"ok":true,"result":{}}`)
		case "pcb.save":
			d.saves++
			fmt.Fprint(w, `{"ok":true,"result":{}}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":{}}`)
		}
	})
	d.srv = httptest.NewServer(mux)
	host, port := splitHostPortForTest(t, d.srv.URL)
	cfg := &appConfig{host: host, ports: port + "-" + port}
	return cfg, d, d.srv.Close
}

func splitHostPortForTest(t *testing.T, url string) (string, string) {
	t.Helper()
	u := strings.TrimPrefix(url, "http://")
	i := strings.LastIndex(u, ":")
	if i < 0 {
		t.Fatalf("bad test server url %q", url)
	}
	return u[:i], u[i+1:]
}

func okResp(result string) string {
	return `{"ok":true,"result":` + result + `}`
}

func pcbComps(rows ...[3]string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"primitiveId": r[0], "uniqueId": r[1], "designator": r[2]})
	}
	return out
}

func TestSyncDesignators_RepairsPlaceholdersByUniqueId(t *testing.T) {
	// PCB：三件占位符 + 一件已有真实位号（不该被碰）。
	pcb := pcbComps(
		[3]string{"p1", "gge1", "U?"},
		[3]string{"p2", "gge2", "C?"},
		[3]string{"p3", "gge3", "RF?"},
		[3]string{"p4", "gge4", "MYNAME"},
	)
	// 原理图：同一套 gge* 命名空间。gge4 的位号与 PCB 上手改的不同 —— 不该覆盖。
	sch := okResp(`{"components":[
	  {"uniqueId":"gge1","designator":"U9"},
	  {"uniqueId":"gge2","designator":"C_SIMV"},
	  {"uniqueId":"gge3","designator":"ANT1"},
	  {"uniqueId":"gge4","designator":"SCHNAME"}
	]}`)
	cfg, d, done := newSyncTestDaemon(t, pcb, sch)
	defer done()

	rep, err := runSyncDesignators(cfg, "", false, io.Discard)
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if rep.PCBTotal != 4 || rep.Placeholder != 3 || rep.Repaired != 3 {
		t.Errorf("counts = total %d / placeholder %d / repaired %d, want 4/3/3", rep.PCBTotal, rep.Placeholder, rep.Repaired)
	}
	if len(d.writes) != 3 {
		t.Fatalf("wrote %d designator(s), want exactly the 3 placeholders", len(d.writes))
	}
	got := map[string]bool{}
	for _, w := range d.writes {
		got[w["designator"].(string)] = true
	}
	for _, want := range []string{"U9", "C_SIMV", "ANT1"} {
		if !got[want] {
			t.Errorf("missing write for %s (got %v)", want, got)
		}
	}
	// 最重要的一条：手工设过的真实位号绝不能被原理图静默覆盖。
	if got["SCHNAME"] {
		t.Error("a real PCB designator was overwritten from the schematic — a hand-set designator is a decision, not a placeholder")
	}
	// 修好之后必须立刻落检查点，不能只指望 autosave 的 debounce 窗口。
	if d.saves != 1 || !rep.Saved {
		t.Errorf("saves=%d saved=%v, want exactly one pcb.save checkpoint", d.saves, rep.Saved)
	}
}

func TestSyncDesignators_SkipsSchematicReadWhenNothingBroken(t *testing.T) {
	// 全是真实位号时不该去读原理图 —— 读全页会 cycle 文档，有实际代价。
	pcb := pcbComps([3]string{"p1", "gge1", "U9"})
	sch := `{"ok":false,"error":{"code":"SHOULD_NOT_BE_CALLED","message":"schematic must not be read"}}`
	cfg, d, done := newSyncTestDaemon(t, pcb, sch)
	defer done()

	rep, err := runSyncDesignators(cfg, "", false, io.Discard)
	if err != nil {
		t.Fatalf("should short-circuit without touching the schematic: %v", err)
	}
	if rep.Repaired != 0 || rep.Placeholder != 0 {
		t.Errorf("nothing to repair, got placeholder=%d repaired=%d", rep.Placeholder, rep.Repaired)
	}
	if !strings.Contains(rep.Summary, "already real") {
		t.Errorf("summary should say there was nothing to do, got %q", rep.Summary)
	}
	if d.saves != 0 {
		t.Errorf("no repair happened but pcb.save was called %d time(s)", d.saves)
	}
}

func TestSyncDesignators_OldConnectorWithoutUniqueId(t *testing.T) {
	// 旧连接器不返回 uniqueId —— 必须明确说是版本问题，不能让用户以为板子坏了，
	// 更不能静默"修了 0 个"就当成功。
	pcb := []map[string]any{{"primitiveId": "p1", "designator": "U?"}}
	cfg, _, done := newSyncTestDaemon(t, pcb, okResp(`{"components":[]}`))
	defer done()

	rep, err := runSyncDesignators(cfg, "", false, io.Discard)
	if err == nil {
		t.Fatal("a connector that reports no uniqueId must be an error, not a silent no-op")
	}
	if !strings.Contains(rep.Summary, ".eext") {
		t.Errorf("the message must point at re-importing the connector, got %q", rep.Summary)
	}
}

func TestSyncDesignators_UnmatchedAreReportedNotSilent(t *testing.T) {
	// PCB 上有件在原理图里找不到（uniqueId 对不上）：必须报出来。静默跳过会让
	// 用户以为全修好了，而那几个件的下游规则仍然是瞎的。
	pcb := pcbComps(
		[3]string{"p1", "gge1", "U?"},
		[3]string{"p9", "ggeORPHAN", "C?"},
	)
	sch := okResp(`{"components":[{"uniqueId":"gge1","designator":"U9"}]}`)
	cfg, _, done := newSyncTestDaemon(t, pcb, sch)
	defer done()

	rep, err := runSyncDesignators(cfg, "", false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Repaired != 1 || len(rep.Unmatched) != 1 || rep.Unmatched[0] != "ggeORPHAN" {
		t.Errorf("repaired=%d unmatched=%v, want 1 repaired and ggeORPHAN reported", rep.Repaired, rep.Unmatched)
	}
	if !strings.Contains(rep.Summary, "unmatched") {
		t.Errorf("summary must surface the unmatched count, got %q", rep.Summary)
	}
}

func TestSyncDesignators_SchematicPlaceholderIsClassifiedNotUnmatched(t *testing.T) {
	// 原理图侧本身没标注（位号还是 U?）：这不是「找不到对应件」——把它报成
	// unmatched 会把用户引向查文档同源性的错误方向；正确指向是先标注原理图。
	pcb := pcbComps([3]string{"p1", "gge1", "U?"})
	sch := okResp(`{"components":[{"uniqueId":"gge1","designator":"U?"}]}`)
	cfg, d, done := newSyncTestDaemon(t, pcb, sch)
	defer done()

	rep, err := runSyncDesignators(cfg, "", false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unmatched) != 0 {
		t.Errorf("schematic placeholder must NOT be counted unmatched, got %v", rep.Unmatched)
	}
	if len(rep.SchUnannotated) != 1 || !strings.Contains(rep.SchUnannotated[0], "gge1") {
		t.Errorf("want gge1 classified as schematic-unannotated, got %v", rep.SchUnannotated)
	}
	if len(d.writes) != 0 {
		t.Errorf("nothing repairable, but %d write(s) were issued", len(d.writes))
	}
	if !strings.Contains(rep.Summary, "unannotated") {
		t.Errorf("summary must mention the unannotated count, got %q", rep.Summary)
	}
}

func TestSyncDesignators_DryRunWritesNothing(t *testing.T) {
	pcb := pcbComps([3]string{"p1", "gge1", "U?"})
	sch := okResp(`{"components":[{"uniqueId":"gge1","designator":"U9"}]}`)
	cfg, d, done := newSyncTestDaemon(t, pcb, sch)
	defer done()

	rep, err := runSyncDesignators(cfg, "", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.writes) != 0 || d.saves != 0 {
		t.Errorf("dry run wrote %d designator(s), saved %d time(s)", len(d.writes), d.saves)
	}
	if rep.Matched != 1 || rep.Repaired != 0 {
		t.Errorf("dry run should match 1 and repair 0, got matched=%d repaired=%d", rep.Matched, rep.Repaired)
	}
}

func TestSyncDesignators_WriteFailureIsReported(t *testing.T) {
	pcb := pcbComps(
		[3]string{"p1", "gge1", "U?"},
		[3]string{"p2", "gge2", "C?"},
	)
	sch := okResp(`{"components":[
	  {"uniqueId":"gge1","designator":"U9"},
	  {"uniqueId":"gge2","designator":"C1"}
	]}`)
	cfg, d, done := newSyncTestDaemon(t, pcb, sch)
	defer done()
	d.failOn = "C1"

	rep, err := runSyncDesignators(cfg, "", false, io.Discard)
	if err != nil {
		t.Fatalf("a single write failure must not abort the whole repair: %v", err)
	}
	// 一件失败不该拖累其余：其它件仍要修好，失败的要点名。
	if rep.Repaired != 1 {
		t.Errorf("repaired=%d, want the other one still repaired", rep.Repaired)
	}
	if len(rep.Failed) != 1 || !strings.Contains(rep.Failed[0], "C1") {
		t.Errorf("failed list must name the part, got %v", rep.Failed)
	}
	if !strings.Contains(rep.Summary, "failed") {
		t.Errorf("summary must surface the failed count, got %q", rep.Summary)
	}
}

func TestSyncDesignators_SilentNoopWriteIsCaughtByReadback(t *testing.T) {
	// 平台的写 API 有「返回 ok 但没落」的前科（delete 批量静默 no-op）。修复的
	// 判定标准必须是回读一致 —— modify 的 ok 不算数。
	sch := okResp(`{"components":[
	  {"uniqueId":"gge1","designator":"U9"},
	  {"uniqueId":"gge2","designator":"C1"}
	]}`)
	cfg, d, done := newSyncTestDaemon(t, pcbComps(
		[3]string{"p1", "gge1", "U?"},
		[3]string{"p2", "gge2", "C?"},
	), sch)
	defer done()
	d.noopOn = "U9" // U9 的写返回 ok 但状态不动;C1 正常落

	rep, err := runSyncDesignators(cfg, "", false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Repaired != 1 {
		t.Errorf("repaired=%d, want only the verified write counted", rep.Repaired)
	}
	if len(rep.Failed) != 1 || !strings.Contains(rep.Failed[0], "read back") {
		t.Errorf("the silent no-op must be surfaced as a read-back failure, got %v", rep.Failed)
	}
}

func TestSyncDesignators_OldConnectorButNothingBrokenIsFine(t *testing.T) {
	// 判定顺序的回归：先问「有没有活要干」再问「工具够不够」。一块位号本来就正常
	// 的板，在不返回 uniqueId 的旧连接器上必须干净通过，而不是被误报成错误。
	pcb := []map[string]any{{"primitiveId": "p1", "designator": "U9"}}
	cfg, _, done := newSyncTestDaemon(t, pcb, okResp(`{"components":[]}`))
	defer done()

	rep, err := runSyncDesignators(cfg, "", false, io.Discard)
	if err != nil {
		t.Fatalf("nothing to repair must not require a new connector: %v", err)
	}
	if !strings.Contains(rep.Summary, "already real") {
		t.Errorf("summary = %q", rep.Summary)
	}
}
