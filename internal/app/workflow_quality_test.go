package app

// workflow_quality_test.go — 质量快照消费侧(#167)的离线单测。
//
// diff 是纯函数,三类关键场景必须锁死:
//   • 有快照+实时可得:掉分超阈值才提示,涨分/小抖动不打扰;
//   • 快照缺:明说"无历史快照可比",不假装比过;
//   • 维 skipped 变化:上次 scored 这次 skipped = 「失去可测性」,绝不能按掉到
//     0 分报退化(skipped=没测≠满分≠0 分,全仓硬约定)。
// 另有一条走假 daemon 的 status 集成测试,验证渲染与 --reconcile 的降级路径。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// qtestSummary 造一份最小可用的质量快照。
func qtestSummary(overall float64, dims map[string]float64) *workflow.QualitySummary {
	return &workflow.QualitySummary{
		Overall: overall, Verdict: "good", Dimensions: dims,
		ScoredDims: len(dims), SkippedDims: 9 - len(dims),
		At: "2026-08-09T00:00:00Z",
	}
}

func TestQualitySnapshotLines_NoSnapshot(t *testing.T) {
	lines := qualitySnapshotLines(nil)
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "未记录过") || !strings.Contains(lines[0], "confirm-layout") {
		t.Fatalf("missing-snapshot line must say 未记录过 + how to get one, got %q", lines[0])
	}
}

func TestQualitySnapshotLines_RendersOverallWeakestSkippedAt(t *testing.T) {
	q := qtestSummary(88.5, map[string]float64{
		"tidy": 61, "compact": 75, "routable": 92, "clearance": 97,
	})
	lines := qualitySnapshotLines(q)
	if len(lines) != 4 { // 首行 + 最弱三维(第四维 clearance 不该出现)
		t.Fatalf("want 1 summary + 3 weakest lines, got %d: %v", len(lines), lines)
	}
	head := lines[0]
	for _, want := range []string{"88.5/100", "[good]", "4 维参与加权", "5 维未测", "2026-08-09T00:00:00Z"} {
		if !strings.Contains(head, want) {
			t.Errorf("summary line missing %q: %q", want, head)
		}
	}
	// 最弱维升序:tidy(61) → compact(75) → routable(92)。
	for i, want := range []string{"齐整度(tidy) 61.0", "紧凑度(compact) 75.0", "可布性(routable) 92.0"} {
		if !strings.Contains(lines[i+1], want) {
			t.Errorf("weakest line %d: want %q in %q", i+1, want, lines[i+1])
		}
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "clearance") {
		t.Errorf("only the weakest 3 dims should be listed, but clearance leaked: %s", joined)
	}
}

func TestQualityDiffNotes_FlagsDropOverThresholdOnly(t *testing.T) {
	prev := qtestSummary(90, map[string]float64{"tidy": 90, "compact": 80, "rf": 70})
	curr := qtestSummary(75, map[string]float64{"tidy": 60, "compact": 78, "rf": 85})
	notes := qualityDiffNotes(prev, curr, 5)
	if len(notes) != 1 {
		t.Fatalf("want exactly 1 note (only tidy dropped ≥5), got %d: %v", len(notes), notes)
	}
	n := notes[0]
	for _, want := range []string{"⚠️", "齐整度(tidy)", "从 90.0 掉到 60.0", "confirm-layout"} {
		if !strings.Contains(n, want) {
			t.Errorf("drop note missing %q: %q", want, n)
		}
	}
	// compact 只掉 2 分(阈内)、rf 涨分 —— 都不该出现。
	if strings.Contains(n, "compact") || strings.Contains(n, "rf") {
		t.Errorf("in-threshold / improved dims must not be flagged: %q", n)
	}
}

func TestQualityDiffNotes_NoSnapshot(t *testing.T) {
	curr := qtestSummary(82, map[string]float64{"tidy": 82})
	notes := qualityDiffNotes(nil, curr, 5)
	if len(notes) != 1 || !strings.Contains(notes[0], "无历史快照可比") {
		t.Fatalf("want a single 无历史快照可比 note, got %v", notes)
	}
}

func TestQualityDiffNotes_NoLiveScore(t *testing.T) {
	prev := qtestSummary(90, map[string]float64{"tidy": 90})
	notes := qualityDiffNotes(prev, nil, 5)
	if len(notes) != 1 {
		t.Fatalf("want a single no-live note, got %v", notes)
	}
	if !strings.Contains(notes[0], "未做实时对比") || !strings.Contains(notes[0], "没测≠没变") {
		t.Fatalf("no-live note must say 未做实时对比 + 没测≠没变, got %q", notes[0])
	}
}

func TestQualityDiffNotes_BothNil(t *testing.T) {
	if notes := qualityDiffNotes(nil, nil, 5); notes != nil {
		t.Fatalf("nothing to compare should yield no notes, got %v", notes)
	}
}

// 上次 scored 这次 skipped:提示「失去可测性」,绝不能当成掉到 0 分。
func TestQualityDiffNotes_ScoredToSkippedIsLostMeasurability(t *testing.T) {
	prev := qtestSummary(90, map[string]float64{"tidy": 90, "compact": 80})
	curr := qtestSummary(80, map[string]float64{"compact": 80}) // tidy 这次 skipped(快照只存 scored 维)
	notes := qualityDiffNotes(prev, curr, 5)
	if len(notes) != 1 {
		t.Fatalf("want exactly 1 note, got %d: %v", len(notes), notes)
	}
	n := notes[0]
	for _, want := range []string{"齐整度(tidy)", "失去可测性", "skipped"} {
		if !strings.Contains(n, want) {
			t.Errorf("lost-measurability note missing %q: %q", want, n)
		}
	}
	if strings.Contains(n, "掉到") {
		t.Errorf("scored→skipped must NOT read as a score drop (to 0): %q", n)
	}
}

// 上次 skipped 这次 scored:新增可测维,信息性提示、无历史可比。
func TestQualityDiffNotes_SkippedToScoredIsGained(t *testing.T) {
	prev := qtestSummary(80, map[string]float64{"compact": 80})
	curr := qtestSummary(85, map[string]float64{"compact": 80, "rf": 95})
	notes := qualityDiffNotes(prev, curr, 5)
	if len(notes) != 1 || !strings.Contains(notes[0], "射频(rf)") || !strings.Contains(notes[0], "暂无历史可比") {
		t.Fatalf("want a single gained-dim note about rf, got %v", notes)
	}
}

// 无退化时给一行 ✓ —— 「比过了没退化」与「根本没比」必须可区分。
func TestQualityDiffNotes_NoRegressionSaysSo(t *testing.T) {
	dims := map[string]float64{"tidy": 90, "compact": 80}
	notes := qualityDiffNotes(qtestSummary(88, dims), qtestSummary(89, map[string]float64{"tidy": 91, "compact": 79}), 5)
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "✓") {
		t.Fatalf("want a single ✓ no-regression note, got %v", notes)
	}
}

// status 集成测试(假 daemon):普通 status 渲染快照;--reconcile 对一块空板实时
// 打分(全维 skipped)后,上次 scored 的维报「失去可测性」而不是掉到 0 分。
func TestWorkflowStatus_QualitySnapshotAndReconcileDiff(t *testing.T) {
	t.Setenv(workflow.EnvDir, t.TempDir())
	cfg, _, cleanup := newCapturingDaemon(t)
	defer cleanup()
	cfg.project = "ceshi-quality"

	// 模拟 confirm-layout 的写入侧:落一份带质量快照的状态。
	st := &pcbStageState{Project: "ceshi-quality", Confirmed: map[pcbStage]bool{}}
	st.Layout = &pcbLayoutGateSummary{
		Score: 80, At: "2026-08-09T00:00:00Z",
		Quality: qtestSummary(88.5, map[string]float64{"tidy": 90, "compact": 75}),
	}
	if err := savePcbStageState(st); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	run := func(args ...string) string {
		var stdout, stderr bytes.Buffer
		cmd := newWorkflowCmd(cfg, &stdout, &stderr)
		cmd.SetArgs(args)
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("workflow %v: %v (stderr: %s)", args, err, stderr.String())
		}
		return stdout.String()
	}

	// 1) 普通 status:快照被渲染(综合分 + 最弱维 + 未测数 + 时间)。
	out := run("status")
	for _, want := range []string{"布局质量: 88.5/100", "最弱维 紧凑度(compact) 75.0", "7 维未测", "2026-08-09T00:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain status missing %q:\n%s", want, out)
		}
	}

	// 2) --reconcile:假 daemon 返回空板 → 实时打分全维 skipped → 上次 scored 的
	//    tidy/compact 报「失去可测性」;绝不能出现"掉到"(那是把 skipped 当 0 分)。
	out = run("status", "--reconcile")
	if !strings.Contains(out, "失去可测性") {
		t.Errorf("reconcile on an empty board must report lost measurability:\n%s", out)
	}
	if strings.Contains(out, "掉到") {
		t.Errorf("scored→skipped must not be rendered as a score drop:\n%s", out)
	}
	// 快照本身仍在渲染(diff 用的是 reconcile 前的快照)。
	if !strings.Contains(out, "布局质量: 88.5/100") {
		t.Errorf("reconcile status must still render the stored snapshot:\n%s", out)
	}
}
