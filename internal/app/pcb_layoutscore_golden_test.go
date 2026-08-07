package app

// pcb_layoutscore_golden_test.go —— 金标准好板回归（#167 第五层 LEARNING）。
//
// #167 原话：「拿一块人类公认的好板跑 layout-score，它就该得高分；**若某维在好板上
// 得了低分 → 是度量错了，回去校准**。把一批好板收进回归 fixtures：度量改了 / 规则
// 加了，好板一跑分数不该掉。」
//
// 没有这一层，九维的权重和阈值就只是拍脑袋的数字 —— 而它们现在**确实**大多在注释里
// 标着「待校准初值」。这个文件把那句话变成一条能失败的断言。
//
// ── 三条设计决定，每条都有代价，写下来免得后人凭直觉改回去 ──────────────────
//
// 1. **阈值断言，不是精确值 golden。** 本仓既有的 Go 测试全是「构造 N 个器件 → 断言
//    exact 数字」，那种写法在这里是毒药：度量是要被校准的，权重和拐点一动就得重新
//    冻结全部 golden，于是没人再敢动权重 —— 恰好把 #167 想要的校准闭环锁死。这里只
//    断言**下限/上限**：好板每维 ≥ 期望下限，坏板每维 ≤ 期望上限。分数在带宽内怎么
//    浮动都不算回归。
//
// 2. **好板 + 坏板成对。** 只有「好板得高分」时，一个退化成恒返 100 的度量照样全绿
//    —— 这正是本仓吃过的亏（audit 基线里那种「用得少所以坏了没人知道」的角落，以及
//    100% 失败率却没人发现的 titleblock.modify）。所以每块参考板配一块**注入缺陷的
//    负对照**，断言九维**全部**掉到 100 以下。少了负对照，这个回归只能证明度量不吵，
//    证明不了它还醒着。
//
// 3. **失败信息必须可归因。** 断言挂了要能一眼判「是度量退化了，还是期望该调整」，
//    所以失败输出带上：哪块板、哪一维、得几分、期望多少、谁拉低了它（contributors）、
//    以及两条处置分支。干巴巴的 got/want 会让人直接把期望值改小了事。
//
// ── 范围声明（别把它当成校准）────────────────────────────────────────────────
//
// 当前 fixture **全是合成板**，是照着九维判据手工摆出来的，所以它满分是**同义反复**。
// 它能证明的只有两件事：(a) 度量在一块摆对了的板上不产生误报；(b) 度量对缺陷仍有
// 反应。**真正的校准必须用真板**（`easyeda pcb dump` 抓下来放进 testdata/boards/），
// 判据见同目录 README.md。
//
// 另：#167 原文说把好板「收进 skills/easyeda-agent/scripts/tests/」是错的 —— 那个
// harness（run.py）是原理图 linter 专用，check_fixtures 会无条件把 fixtures/*.json
// 喂给 lint.py，塞一份 PCB dump 进去会直接崩。金标准板走 Go 侧 testdata。

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

// goldenBoardsDir 是金标准板 fixture 的家。本仓在此之前**零 testdata 目录**
// （`find -type d -name testdata` 为空），约定见同目录 README.md。
const goldenBoardsDir = "testdata/boards"

// ---------------------------------------------------------------------------
// 期望值格式
// ---------------------------------------------------------------------------

// goldenExpect 是一块 fixture 的期望值，住在同名 `.expect.json` 里。
//
// 为什么是 sidecar 而不是塞进快照的 `_expect` 字段：fixture 必须与
// `easyeda pcb dump` 的输出**逐字节同形**，这样「抓一块真板 → 直接落进 testdata」
// 才是一条命令的事，重抓刷新几何时也不会把人工核定的期望值一起覆盖掉。测量数据与
// 人的判断分开放，也和本仓 snapshot(取数) / analyze(判定) 的既有分法一致。
//
// 数值边界一律用指针：`"maxBlocking": 0` 和「没写 maxBlocking」是两个意思，
// 零值语义会把前者悄悄变成后者。
type goldenExpect struct {
	// Kind 只影响失败文案里的处置建议（reference = 好板，degraded = 负对照）。
	Kind  string `json:"kind"`
	Title string `json:"title,omitempty"`
	// Note 是给人读的：这块板为什么长这样、维持分数的几何关系是什么。
	// 允许字符串或字符串数组（多行说明写成数组才不会挤成一坨）。
	Note goldenNote `json:"note,omitempty"`

	// Spec 覆盖「同名 .spec.json」的约定路径（相对 boards 目录）。留空走约定。
	Spec string `json:"spec,omitempty"`
	// NoSpec 显式声明这块板不配 spec（意图类维度理应 skipped）。
	NoSpec bool `json:"noSpec,omitempty"`

	MinOverall *float64 `json:"minOverall,omitempty"`
	MaxOverall *float64 `json:"maxOverall,omitempty"`

	MinDimension map[string]float64 `json:"minDimension,omitempty"`
	MaxDimension map[string]float64 `json:"maxDimension,omitempty"`

	MinBlocking *int `json:"minBlocking,omitempty"`
	MaxBlocking *int `json:"maxBlocking,omitempty"`

	// RequireScored 列出的维必须**参与加权**（status ∈ scored|degraded）。
	// 这条守的是硬约定①「没测 ≠ 测了满分」的反面：一次改动如果让某维悄悄变成
	// skipped，minDimension 是查不出来的（skipped 维压根不参与比较），报告却还是
	// 一片绿。要这条断言才拦得住。
	RequireScored []string `json:"requireScored,omitempty"`
	// ExpectSkipped 列出的维必须是 skipped（真板缺数据时的诚实声明）。
	ExpectSkipped []string `json:"expectSkipped,omitempty"`

	// GridMil / MinGapMil 覆盖打分参数（对齐 `--grid` / `--min-gap`）。0 = 用默认。
	GridMil   float64 `json:"gridMil,omitempty"`
	MinGapMil float64 `json:"minGapMil,omitempty"`
}

// goldenNote 收「一行字符串」和「多行字符串数组」两种写法。
//
// 为什么不直接用 json.RawMessage 放着不管：note 是这套 fixture 里唯一记录**为什么**
// 的地方（这块板凭什么算好板、注入了哪些缺陷），写坏了没人发现就等于没写。留一个
// 会失败的解析，比留一个永远能通过的 RawMessage 强。
type goldenNote []string

func (n *goldenNote) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*n = goldenNote{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("note 要么是字符串，要么是字符串数组：%w", err)
	}
	*n = many
	return nil
}

// ---------------------------------------------------------------------------
// 回归本体
// ---------------------------------------------------------------------------

// TestLayoutScore_GoldenBoards 是校准回归的入口。改了任何一维的判据/阈值/权重后
// 先跑它：`go test ./internal/app/ -run TestLayoutScore_GoldenBoards`
// （或 `make layout-calibrate`）。
func TestLayoutScore_GoldenBoards(t *testing.T) {
	boards := goldenBoardFiles(t)
	if len(boards) == 0 {
		// 空目录必须失败：fixture 被误删后测试静默变成 no-op，是这类回归最常见的
		// 死法（跑得飞快、永远绿、什么都没验）。
		t.Fatalf("%s 下一块 fixture 都没有 —— 金标准回归退化成了空跑。"+
			"补一块板（`easyeda pcb dump --out %s/<板名>.json`）或删掉这个测试，别留个永远绿的空壳。",
			goldenBoardsDir, goldenBoardsDir)
	}
	for _, board := range boards {
		t.Run(strings.TrimSuffix(filepath.Base(board), ".json"), func(t *testing.T) {
			runGoldenBoard(t, board)
		})
	}
}

func runGoldenBoard(t *testing.T, boardPath string) {
	t.Helper()
	name := strings.TrimSuffix(filepath.Base(boardPath), ".json")

	exp := loadGoldenExpect(t, boardPath)
	snap := loadGoldenSnapshot(t, boardPath)
	s0 := loadGoldenSpec(t, boardPath, exp)

	rep := analyzeLayoutScore(snap, s0, layoutScoreOpts{
		gridMil: exp.GridMil,
		minGap:  exp.MinGapMil,
	})

	ctx := goldenCtx{t: t, name: name, path: boardPath, exp: exp, rep: &rep}
	ctx.checkInvariants()
	ctx.checkBlocking()
	ctx.checkStatuses()
	ctx.checkOverall()
	ctx.checkDimensions()
}

// goldenCtx 把「板 + 期望 + 报告」捆一起，让每条断言的失败文案都能拿到全部上下文。
type goldenCtx struct {
	t    *testing.T
	name string
	path string
	exp  goldenExpect
	rep  *layoutScoreReport
}

// checkInvariants 断言报告本身自洽 —— 这是把 pcb_layoutscore.go 里那三条硬约定
// 放到**真板数据**上验一遍，而不是只在手写结构体上验。
func (c *goldenCtx) checkInvariants() {
	r := c.rep

	// 约定③：计数与判定同源。本仓踩过「0 个阻塞项却 FAIL」的判读陷阱，
	// verdict 必须只从 blocking 数和 overall 推得出来。
	if want := verdictFor(r); r.Verdict != want {
		c.t.Errorf("%s：verdict=%q 与报告里的数字对不上（按 blocking=%d / overall=%.1f 应为 %q）。"+
			"判定和计数分了家 —— 修 finalizeScoreReport，别改期望值。",
			c.name, r.Verdict, len(r.Blocking), r.Overall, want)
	}
	if hasBlocking := len(r.Blocking) > 0; hasBlocking != (r.Verdict == "blocked") {
		c.t.Errorf("%s：blocking=%d 但 verdict=%q —— 「有硬错」和「判定为 blocked」必须等价",
			c.name, len(r.Blocking), r.Verdict)
	}

	// 约定①：skipped 维不参与加权，且必须给得出原因。
	if got := r.ScoredDims + r.SkippedDims; got != len(r.Dimensions) {
		c.t.Errorf("%s：scoredDims(%d) + skippedDims(%d) = %d ≠ 维度总数 %d —— 有维既没算也没被记成跳过",
			c.name, r.ScoredDims, r.SkippedDims, got, len(r.Dimensions))
	}
	for _, d := range r.Dimensions {
		if d.Status == dimSkipped && strings.TrimSpace(d.Reason) == "" {
			c.t.Errorf("%s：维 %s 被标 skipped 却没写原因 —— 报告要能回答「这维为什么没分」，"+
				"skipDimension 的 format 参数不能是空串", c.name, d.ID)
		}
		if d.Status != dimSkipped && (d.Score < 0 || d.Score > 100) {
			c.t.Errorf("%s：维 %s 得分 %.1f 越界 [0,100]", c.name, d.ID, d.Score)
		}
	}

	// 归因必须按扣分降序 —— 精修环直接取前几个派活，顺序错了就先动了错的件。
	for _, d := range r.Dimensions {
		for i := 1; i < len(d.Contributors); i++ {
			if d.Contributors[i-1].Penalty < d.Contributors[i].Penalty {
				c.t.Errorf("%s：维 %s 的归因没按扣分降序（%s −%.2f 排在 %s −%.2f 前面）",
					c.name, d.ID,
					d.Contributors[i-1].Designator, d.Contributors[i-1].Penalty,
					d.Contributors[i].Designator, d.Contributors[i].Penalty)
				break
			}
		}
	}
}

func (c *goldenCtx) checkBlocking() {
	n := len(c.rep.Blocking)
	if c.exp.MaxBlocking != nil && n > *c.exp.MaxBlocking {
		c.t.Errorf("%s：阻塞项 %d 条，期望 ≤ %d。%s\n%s",
			c.name, n, *c.exp.MaxBlocking, c.blockingDigest(), c.fixHint("blocking", float64(n), float64(*c.exp.MaxBlocking), false))
	}
	if c.exp.MinBlocking != nil && n < *c.exp.MinBlocking {
		c.t.Errorf("%s：阻塞项 %d 条，期望 ≥ %d —— 这块板是负对照，注入的硬错**没被抓到**，"+
			"说明 overlap/short/off-board 那条路哑了（blockingFindings / analyzePcbLayout）。\n%s",
			c.name, n, *c.exp.MinBlocking, c.fixHint("blocking", float64(n), float64(*c.exp.MinBlocking), true))
	}
}

func (c *goldenCtx) checkStatuses() {
	for _, id := range c.exp.RequireScored {
		d := c.rep.dimension(id)
		switch {
		case d == nil:
			c.t.Errorf("%s：期望参与加权的维 %q 压根不在报告里 —— dimensionOrder 少了它，还是 --only/--skip 逻辑改坏了？", c.name, id)
		case d.Status == dimSkipped:
			c.t.Errorf("%s：维 %s(%s) 被标 skipped —— 期望它参与加权。\n"+
				"  跳过原因：%s\n"+
				"  这条断言专治「悄悄变 skipped」：skipped 维不参与 minDimension 比较，"+
				"报告会一片绿，实际上少测了一维。要么修数据/判据让它重新算得出来，"+
				"要么这块板确实缺这段数据 —— 那就把它从 requireScored 挪进 expectSkipped 并说明。",
				c.name, dimensionTitles[id], id, d.Reason)
		}
	}
	for _, id := range c.exp.ExpectSkipped {
		d := c.rep.dimension(id)
		if d == nil {
			c.t.Errorf("%s：期望被跳过的维 %q 不在报告里", c.name, id)
			continue
		}
		if d.Status != dimSkipped {
			c.t.Errorf("%s：维 %s(%s) 期望 skipped，实际 status=%s 得 %.1f 分 —— "+
				"这块板补上数据了？那就把它从 expectSkipped 挪回 requireScored 并定下限。",
				c.name, dimensionTitles[id], id, d.Status, d.Score)
		}
	}
}

func (c *goldenCtx) checkOverall() {
	if c.exp.MinOverall != nil && c.rep.Overall < *c.exp.MinOverall {
		c.t.Errorf("%s：综合分 %.1f，期望 ≥ %.1f（%d 维参与加权，%d 维跳过）。\n%s\n%s",
			c.name, c.rep.Overall, *c.exp.MinOverall, c.rep.ScoredDims, c.rep.SkippedDims,
			c.weakestDigest(), c.fixHint("overall", c.rep.Overall, *c.exp.MinOverall, false))
	}
	if c.exp.MaxOverall != nil && c.rep.Overall > *c.exp.MaxOverall {
		c.t.Errorf("%s：综合分 %.1f，期望 ≤ %.1f —— 负对照的分数**涨上去了**，"+
			"多半是某一维不再对注入的缺陷有反应。\n%s\n%s",
			c.name, c.rep.Overall, *c.exp.MaxOverall,
			c.weakestDigest(), c.fixHint("overall", c.rep.Overall, *c.exp.MaxOverall, true))
	}
}

func (c *goldenCtx) checkDimensions() {
	for _, id := range sortedDimKeys(c.exp.MinDimension) {
		want := c.exp.MinDimension[id]
		d := c.rep.dimension(id)
		if d == nil {
			c.t.Errorf("%s：minDimension 提到的维 %q 不在报告里", c.name, id)
			continue
		}
		if d.Status == dimSkipped {
			continue // 由 requireScored / expectSkipped 管，这里不重复报
		}
		if d.Score+goldenScoreEps < want {
			c.t.Errorf("%s：维 %s(%s) 得 %.1f 分，期望 ≥ %.1f。\n%s\n%s",
				c.name, dimensionTitles[id], id, d.Score, want,
				dimDigest(d), c.fixHint("minDimension."+id, d.Score, want, false))
		}
	}
	for _, id := range sortedDimKeys(c.exp.MaxDimension) {
		want := c.exp.MaxDimension[id]
		d := c.rep.dimension(id)
		if d == nil {
			c.t.Errorf("%s：maxDimension 提到的维 %q 不在报告里", c.name, id)
			continue
		}
		if d.Status == dimSkipped {
			c.t.Errorf("%s：维 %s(%s) 被跳过，负对照要求它**必须响**（≤ %.1f）。跳过原因：%s",
				c.name, dimensionTitles[id], id, want, d.Reason)
			continue
		}
		if d.Score > want+goldenScoreEps {
			c.t.Errorf("%s：维 %s(%s) 得 %.1f 分，期望 ≤ %.1f —— 这块板在这一维**注入了缺陷**，"+
				"分数没掉下来说明这一维对它不再有反应。\n%s\n%s",
				c.name, dimensionTitles[id], id, d.Score, want,
				dimDigest(d), c.fixHint("maxDimension."+id, d.Score, want, true))
		}
	}
}

// goldenScoreEps 吸收浮点噪声。clampScore 已经保留一位小数，0.05 只是防止
// 「刚好等于阈值」被舍入判成越界。
const goldenScoreEps = 0.05

// fixHint 是失败信息里最重要的一段：把「要么改度量、要么改期望」这个岔路口摆出来，
// 并给出改哪个文件。缺了它，读到失败的人默认就会把期望值改松 —— 那正是这套回归
// 存在的意义被抵消的方式。
func (c *goldenCtx) fixHint(field string, got, want float64, wantCeiling bool) string {
	dir := "掉了"
	if wantCeiling {
		dir = "涨了"
	}
	kind := "参考板（人工核定的好布局）"
	consequence := "它掉分 = 度量在一块摆对了的板上开始误报"
	if c.exp.Kind == "degraded" {
		kind = "负对照板（逐维注入了缺陷）"
		consequence = "它涨分 = 度量对注入的缺陷不再有反应（哑了）"
	}
	if t := strings.TrimSpace(c.exp.Title); t != "" {
		kind = t + " —— " + kind
	}
	return fmt.Sprintf(
		"  这块板是 %s，%s。分数%s（%.1f vs 期望 %.1f）意味着二者之一：\n"+
			"    (a) 本次改动让这一维退化了 —— 修度量（internal/app/pcb_score_*.go）；\n"+
			"    (b) 判据变了且是有意的 —— 更新 %s.expect.json 的 %s，并在 note 里写清为什么变。\n"+
			"  别默认选 (b)。人工核定的期望值一旦被随手改松，这条回归就只剩装饰作用。\n"+
			"  复现：easyeda pcb layout-score --from %s%s --all",
		kind, consequence, dir, got, want,
		c.name, field,
		repoPath(c.path), c.specHintFor(),
	)
}

func (c *goldenCtx) specHintFor() string {
	p := goldenSpecPath(c.path, c.exp)
	if p == "" {
		return ""
	}
	return " --spec " + repoPath(p)
}

// repoPath 把测试内的相对路径（go test 的 cwd 是包目录）改写成**仓库根相对**路径，
// 这样失败信息里那条复现命令可以直接从仓库根粘贴执行 —— 差这一步的话，读到失败的人
// 会照抄一条在自己终端里 file-not-found 的命令。
func repoPath(p string) string { return filepath.ToSlash(filepath.Join("internal/app", p)) }

// blockingDigest 列出硬错，让「阻塞项超了」能直接看到是哪几条。
func (c *goldenCtx) blockingDigest() string {
	if len(c.rep.Blocking) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n  阻塞项：")
	for i, f := range c.rep.Blocking {
		if i >= 5 {
			fmt.Fprintf(&b, "\n    … 另有 %d 条", len(c.rep.Blocking)-i)
			break
		}
		fmt.Fprintf(&b, "\n    %-18s %s", f.Type, f.Message)
	}
	return b.String()
}

// weakestDigest 给出综合分失衡时的第一现场：最弱的三维。
func (c *goldenCtx) weakestDigest() string {
	var b strings.Builder
	b.WriteString("  最弱的三维：")
	for _, d := range c.rep.weakest(3) {
		fmt.Fprintf(&b, "\n    %-22s %5.1f  (权重 %.1f, %s)", dimLabel(d), d.Score, d.Weight, d.Status)
	}
	return b.String()
}

// dimDigest 是单维的归因摘要 —— 「谁拉低了它」才是判断度量对不对的依据，
// 只给一个分数没法判。
func dimDigest(d *scoreDimension) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  status=%s weight=%.1f", d.Status, d.Weight)
	if d.Reason != "" {
		fmt.Fprintf(&b, "\n  说明：%s", d.Reason)
	}
	if len(d.Contributors) == 0 {
		b.WriteString("\n  归因：（无）—— 分数不是 100 却没有归因，本身就是个 bug：精修环无从下手")
		return b.String()
	}
	b.WriteString("\n  归因（谁拉低了它）：")
	for i, ct := range d.Contributors {
		if i >= 5 {
			fmt.Fprintf(&b, "\n    … 另有 %d 个", len(d.Contributors)-i)
			break
		}
		fmt.Fprintf(&b, "\n    %-10s −%.2f  %s", ct.Designator, ct.Penalty, ct.Detail)
	}
	if len(d.Metrics) > 0 {
		fmt.Fprintf(&b, "\n  原始量：%s", formatMetrics(d.Metrics))
	}
	return b.String()
}

// formatMetrics 把原始量排序输出 —— 「这维为什么是 62 分」得靠这些数回答，
// map 的随机序会让两次失败输出对不上。
func formatMetrics(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		if v == math.Trunc(v) {
			parts = append(parts, fmt.Sprintf("%s=%.0f", k, v))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%.3g", k, v))
		}
	}
	return strings.Join(parts, " ")
}

func sortedDimKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// 加载
// ---------------------------------------------------------------------------

// goldenBoardFiles 列出 fixture 板（排除 .spec.json / .expect.json 两类 sidecar）。
func goldenBoardFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob(filepath.Join(goldenBoardsDir, "*.json"))
	if err != nil {
		t.Fatalf("扫描 %s 失败：%v", goldenBoardsDir, err)
	}
	var out []string
	for _, p := range all {
		base := filepath.Base(p)
		if strings.HasSuffix(base, ".spec.json") || strings.HasSuffix(base, ".expect.json") {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// loadGoldenExpect 读期望值。**缺 .expect.json 是硬失败**：往目录里丢一块板却忘了
// 定期望，测试会照跑不误、一条断言都不做，看起来还多了个绿勾 —— 那比没有 fixture
// 更危险。
func loadGoldenExpect(t *testing.T, boardPath string) goldenExpect {
	t.Helper()
	p := strings.TrimSuffix(boardPath, ".json") + ".expect.json"
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("fixture %s 没有配套的期望值文件 %s：%v\n"+
			"每块 fixture 都必须有 .expect.json —— 否则它只是躺在目录里，一条断言都不参与。"+
			"格式见 %s/README.md。", filepath.Base(boardPath), p, err, goldenBoardsDir)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields() // 拼错的键（minDimensions / maxBlockings…）必须报错而不是静默失效
	var exp goldenExpect
	if err := dec.Decode(&exp); err != nil {
		t.Fatalf("解析 %s 失败：%v\n（未知字段会被拒绝：拼错的键静默失效等于这条期望从未生效）", p, err)
	}
	if exp.Kind != "reference" && exp.Kind != "degraded" {
		t.Fatalf("%s：kind=%q 未知，只认 reference（人工核定的好板）| degraded（注入缺陷的负对照）", p, exp.Kind)
	}
	if exp.MinOverall == nil && exp.MaxOverall == nil &&
		len(exp.MinDimension) == 0 && len(exp.MaxDimension) == 0 {
		t.Fatalf("%s 一条分数期望都没写 —— 这块 fixture 不会拦下任何回归", p)
	}
	if len(exp.Note) == 0 {
		// 强制留档：期望值是**人核定**的，凭据只存在于写它的人脑子里。半年后有人
		// 看到某维掉分，唯一能判断「该修度量还是该改期望」的依据就是这段 note。
		t.Fatalf("%s 没写 note —— 期望值必须附上凭据：这块板凭什么算参考板/注入了哪些缺陷、"+
			"维持分数的几何关系是什么。格式见 %s/README.md", p, goldenBoardsDir)
	}
	for _, id := range append(append([]string{}, sortedDimKeys(exp.MinDimension)...), sortedDimKeys(exp.MaxDimension)...) {
		if scorerFor(id) == nil && !knownDimension(id) {
			t.Fatalf("%s：期望里出现未知维度 %q（合法值：%s）", p, id, strings.Join(dimensionOrder, ", "))
		}
	}
	for _, id := range append(append([]string{}, exp.RequireScored...), exp.ExpectSkipped...) {
		if !knownDimension(id) {
			t.Fatalf("%s：requireScored/expectSkipped 里出现未知维度 %q（合法值：%s）", p, id, strings.Join(dimensionOrder, ", "))
		}
	}
	return exp
}

func knownDimension(id string) bool {
	for _, d := range dimensionOrder {
		if d == id {
			return true
		}
	}
	return false
}

// loadGoldenSnapshot 从磁盘读回一块板 —— 走的正是 `layout-score --from` 的同一条
// 解析路径（loadBoardSnapshotFile），所以 fixture 格式漂移会在这里立刻现形。
func loadGoldenSnapshot(t *testing.T, path string) *boardSnapshot {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开 fixture %s 失败：%v", path, err)
	}
	defer f.Close()
	snap, err := loadBoardSnapshotFile(f)
	if err != nil {
		t.Fatalf("解析 fixture %s 失败：%v\n"+
			"fixture 就是 `easyeda pcb dump` 的原样输出；这个错误通常意味着 boardSnapshot "+
			"的字段变了而 fixture 没跟着重抓。", path, err)
	}
	if len(snap.Components) == 0 {
		t.Fatalf("fixture %s 一个器件都没有 —— 空板打分没有意义", path)
	}
	return snap
}

// goldenSpecPath 解析 S0 spec 的位置：expect.spec 显式指定 > 同名 .spec.json 约定。
func goldenSpecPath(boardPath string, exp goldenExpect) string {
	if exp.NoSpec {
		return ""
	}
	if exp.Spec != "" {
		return filepath.Join(filepath.Dir(boardPath), exp.Spec)
	}
	p := strings.TrimSuffix(boardPath, ".json") + ".spec.json"
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// loadGoldenSpec 读 S0 意图。spec 有 ERROR 直接 fail —— fixture 自带一份写错的
// spec，会让 flow-order / edge-io 的期望值全部建立在一个没人看的意图上。
func loadGoldenSpec(t *testing.T, boardPath string, exp goldenExpect) *spec.Spec {
	t.Helper()
	p := goldenSpecPath(boardPath, exp)
	if p == "" {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("expect 指定了 spec %s 却读不到：%v", p, err)
	}
	s, err := spec.Parse(raw)
	if err != nil {
		t.Fatalf("解析 spec %s 失败：%v", p, err)
	}
	if issues := spec.Validate(s); spec.HasErrors(issues) {
		var msgs []string
		for _, i := range issues {
			if i.Level == "ERROR" {
				msgs = append(msgs, i.Field+": "+i.Message)
			}
		}
		t.Fatalf("fixture 的 spec %s 有 ERROR：\n  %s", p, strings.Join(msgs, "\n  "))
	}
	return s
}
