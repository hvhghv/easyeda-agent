package app

// pcb_layout_benchmark_test.go — 用 layout-score 量我们**自己**的布局工具。
//
// #167 的地基是打分，但打分真正的用处不是给别人的板挑刺，而是回答一个此前只能
// 靠感觉回答的问题：**place-constrained 摆出来的板，离人类工程师差多少？**
//
// docs/e2e-automation-acceptance.md §5 和 concepts.md「已消化 vs 待补」都记着
// 「偏散板」这个老毛病，但一直没有数 —— 只有「看起来比较散」。这个基准把它变成
// 逐维分差。
//
// 做法：拿一块**已有布局**的板当基线，把同一批器件的坐标全部交给规划器重摆，
// 两份布局喂同一个 analyzeLayoutScore。器件集、板框、网表完全相同，唯一变量就是
// 「谁摆的」。
//
// 默认跑仓库自带的参考 fixture（自包含、可回归）。要量一块真板：
//
//	easyeda pcb dump --project <名字> --out /tmp/board.json
//	EASYEDA_BENCH_BOARD=/tmp/board.json go test ./internal/app/ -run TestLayoutBenchmark -v
//
// 真板 dump **不入库**（商业设计），所以走环境变量而不是 testdata。

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

// benchSnapshotToCpComps 把板级快照投影成规划器的入参。
//
// 与 parseCpComps 的区别：那个吃连接器的原始 map，这个吃已经解析好的快照 ——
// 基准要的是「同一批器件」，从快照走能保证两侧看到的几何一模一样。
func benchSnapshotToCpComps(snap *boardSnapshot) []cpComp {
	out := make([]cpComp, 0, len(snap.Components))
	for _, c := range snap.Components {
		ap := apComp{
			id: c.ID, designator: c.Designator,
			x: c.X, y: c.Y, rotation: c.Rotation, locked: c.Locked,
		}
		if c.BBox != nil {
			ap.hasBBox = true
			ap.minX, ap.minY, ap.maxX, ap.maxY = c.BBox.MinX, c.BBox.MinY, c.BBox.MaxX, c.BBox.MaxY
		}
		for _, p := range c.Pads {
			ap.pads = append(ap.pads, apPad{
				num: p.Number, net: p.Net, x: p.X, y: p.Y,
				layer: p.Layer, w: p.W, h: p.H,
			})
		}
		layer := c.Layer
		if layer == 0 {
			layer = 1
		}
		out = append(out, cpComp{apComp: ap, footprint: c.Device, layer: layer})
	}
	return out
}

// benchApplyMoves 把规划器的 anchor 移动**离线**套回快照。几何投影走合法化器的
// applyMoveToVComp —— 同一套旋转感知近似（90° 整数倍转 bbox 四角取 AABB、焊盘
// 跟转、90/270 换 W/H），基准的判定和合法化的判定才不会各说各话。仍是近似：
// 渲染 bbox 里的丝印文字实际不随件转，旋转件的 overlap/间距差值要打个小折扣。
func benchApplyMoves(snap *boardSnapshot, moves []apMove) *boardSnapshot {
	byID := map[string]apMove{}
	for _, m := range moves {
		byID[m.ID] = m
	}
	out := &boardSnapshot{
		Outline: snap.Outline, Silk: snap.Silk,
		CopperLayers: snap.CopperLayers, Rules: snap.Rules,
	}
	for _, c := range snap.Components {
		m, moved := byID[c.ID]
		if !moved {
			out.Components = append(out.Components, c)
			continue
		}
		v := applyMoveToVComp(c, m)
		nc := c
		nc.X, nc.Y = m.NewX, m.NewY
		if m.SetRot {
			nc.Rotation = m.NewRot
		}
		if v.bbox != nil {
			nb := *v.bbox
			nc.BBox = &nb
		}
		nc.Pads = make([]boardPad, len(v.pads))
		for i, p := range v.pads {
			nc.Pads[i] = boardPad{Number: p.Number, Net: p.Net, Layer: p.Layer, X: p.X, Y: p.Y, W: p.W, H: p.H}
		}
		out.Components = append(out.Components, nc)
	}
	return out
}

// benchBoard 装一块基准板。
type benchBoard struct {
	name string
	snap *boardSnapshot
	s0   *spec.Spec
}

func benchLoadBoards(t *testing.T) []benchBoard {
	t.Helper()
	var out []benchBoard

	if p := os.Getenv("EASYEDA_BENCH_BOARD"); p != "" {
		f, err := os.Open(p)
		if err != nil {
			t.Fatalf("EASYEDA_BENCH_BOARD=%s: %v", p, err)
		}
		defer f.Close()
		snap, err := loadBoardSnapshotFile(f)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		b := benchBoard{name: p, snap: snap}
		if sp := os.Getenv("EASYEDA_BENCH_SPEC"); sp != "" {
			raw, rerr := os.ReadFile(sp)
			if rerr != nil {
				t.Fatalf("EASYEDA_BENCH_SPEC=%s: %v", sp, rerr)
			}
			if b.s0, err = spec.Parse(raw); err != nil {
				t.Fatalf("parse spec: %v", err)
			}
		}
		return append(out, b)
	}

	// 默认：仓库自带的参考板（合成，但自包含、可回归）。
	const dir = "testdata/boards"
	raw, err := os.ReadFile(dir + "/reference-4stage-compact.json")
	if err != nil {
		t.Skipf("no reference fixture (%v)", err)
	}
	var snap boardSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("parse reference fixture: %v", err)
	}
	b := benchBoard{name: "reference-4stage-compact", snap: &snap}
	if sraw, serr := os.ReadFile(dir + "/reference-4stage-compact.spec.json"); serr == nil {
		if b.s0, err = spec.Parse(sraw); err != nil {
			t.Fatalf("parse reference spec: %v", err)
		}
	}
	return append(out, b)
}

// TestLayoutBenchmark_PlannerVsExisting 量规划器相对既有布局的逐维差。
//
// 它**不是**一个会红的断言测试 —— 规划器暂时打不过人类是已知事实，把它做成硬门
// 只会让人跳过它。它是一份带数字的报告：`-v` 看逐维差，改了规划器就能看出动的是
// 哪一维、动了多少。唯一会失败的情况是规划器**崩了或什么都没摆**，那是真回归。
func TestLayoutBenchmark_PlannerVsExisting(t *testing.T) {
	for _, b := range benchLoadBoards(t) {
		t.Run(b.name, func(t *testing.T) {
			if len(b.snap.Components) < 4 {
				t.Skipf("%d components — too small to benchmark", len(b.snap.Components))
			}
			opts := layoutScoreOpts{}
			before := analyzeLayoutScore(b.snap, b.s0, opts)

			cps := benchSnapshotToCpComps(b.snap)
			cpOpt := defaultCpOptions()
			if b.snap.Outline != nil {
				bb := b.snap.Outline.BBox
				cpOpt.board = &cpRect{x0: bb.MinX, y0: bb.MinY, x1: bb.MaxX, y1: bb.MaxY}
			}
			moves, diags := planConstrainedPlace(cps, nil, cpOpt)
			if len(moves) == 0 {
				t.Fatalf("planner produced no moves for %d components — it is not exercising the board at all", len(cps))
			}
			// 生产路径同款合法化(#167 遗留②):新引入的重叠/短路/出板框
			// 就地重定位或弃子。基准量的是「跑完整条规划管线」的结果。
			var legal legalizeResult
			moves, lDiags, legal := legalizeConstrainedMoves(b.snap, moves)
			diags = append(diags, lDiags...)
			if legal.Adjusted+legal.Dropped > 0 {
				t.Logf("合法化: %d 件重定位, %d 件弃子", legal.Adjusted, legal.Dropped)
			}
			after := analyzeLayoutScore(benchApplyMoves(b.snap, moves), b.s0, opts)

			// 结论可信度自查。benchApplyMoves 现在走合法化器同款旋转感知投影
			// （90° 整数倍转 bbox 四角取 AABB、焊盘跟转）——比早期纯平移强得多，
			// 但仍是近似：渲染 bbox 里的丝印文字实际不随件转，平台的真实排版
			// 引擎离线拿不到。把比例报出来而不是藏着 —— 旋转件的 overlap/间距
			// 差值要打个小折扣。
			rotated := benchRotatedCount(b.snap, moves)
			if rotated > 0 {
				t.Logf("⚠️  规划器改了 %d/%d 件的朝向；虚拟套用按旋转 AABB 近似"+
					"（丝印不随转）—— 这些件的 overlap/间距差值有少量噪声",
					rotated, len(moves))
			}

			t.Logf("\n%s — %d 器件，规划器移动 %d 件（diag %d 条）",
				b.name, len(b.snap.Components), len(moves), len(diags))
			t.Logf("%-24s %8s %8s %8s", "维度", "既有", "规划器", "差")
			t.Logf("%-24s %8.1f %8.1f %+8.1f", "综合分", before.Overall, after.Overall, after.Overall-before.Overall)
			t.Logf("%-24s %8d %8d %+8d", "阻塞项", len(before.Blocking), len(after.Blocking), len(after.Blocking)-len(before.Blocking))

			// 阻塞项的**构成**比总数重要：综合分涨了但硬错多了 3 条，说明规划器
			// 是在拿电气正确性换布局分。旧的单标量分看不出这个 trade-off
			// （一处重叠直接归零），多维 + 硬错单列才让它显形。
			if d := benchBlockingDelta(before, after); len(d) > 0 {
				t.Logf("阻塞项构成变化：")
				for _, line := range d {
					t.Logf("   %s", line)
				}
			}

			ids := make([]string, 0, len(before.DimensionScores))
			for id := range before.DimensionScores {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				bs := before.DimensionScores[id]
				as, scored := after.DimensionScores[id]
				if !scored {
					t.Logf("%-24s %8.1f %8s   (规划后该维不可测)", dimLabelOf(id), bs, "—")
					continue
				}
				t.Logf("%-24s %8.1f %8.1f %+8.1f", dimLabelOf(id), bs, as, as-bs)
			}

			// 硬断言（合法化落地后从「不得新增 off-board」升级为「不得新增任何
			// blocking」）：规划管线不得制造新的硬错 —— 重叠/跨网短路/出板框
			// 任何一种冒出来都是回归，不是风格差异。「偏散板」可以慢慢改，
			// 制造硬错是坏掉。key 用合法化器同一套 blockingKeySet 与既有布局
			// 做差（板上本来就有的不算），判定不可能与生产路径各说各话。
			minGap := b.snap.Rules.toPcbRules().clearanceMil
			bc, bp := b.snap.toLayoutComps()
			baseL := analyzePcbLayout(bc, bp, outlineBBoxOf(b.snap), minGap)
			baseKeys := blockingKeySet(&baseL)
			afterSnap := benchApplyMoves(b.snap, moves)
			ac, ap := afterSnap.toLayoutComps()
			afterL := analyzePcbLayout(ac, ap, outlineBBoxOf(afterSnap), minGap)
			for key := range blockingKeySet(&afterL) {
				if !baseKeys[key] {
					t.Errorf("planner pipeline introduced a NEW blocking issue: %s — that is a regression, not a style difference", key)
				}
			}
		})
	}
}

func dimLabelOf(id string) string {
	if t := dimensionTitles[id]; t != "" {
		return fmt.Sprintf("%s(%s)", t, id)
	}
	return id
}

// benchRotatedCount 数规划器真正改了朝向的件数（SetRot 但角度没变的不算）。
func benchRotatedCount(snap *boardSnapshot, moves []apMove) int {
	byID := map[string]float64{}
	for _, c := range snap.Components {
		byID[c.ID] = c.Rotation
	}
	n := 0
	for _, m := range moves {
		if m.SetRot && m.NewRot != byID[m.ID] {
			n++
		}
	}
	return n
}

// benchBlockingDelta 按类型对比两侧的阻塞项，只列有变化的类型。
func benchBlockingDelta(before, after layoutScoreReport) []string {
	count := func(r layoutScoreReport) map[string]int {
		m := map[string]int{}
		for _, f := range r.Blocking {
			m[f.Type]++
		}
		return m
	}
	b, a := count(before), count(after)
	types := map[string]bool{}
	for t := range b {
		types[t] = true
	}
	for t := range a {
		types[t] = true
	}
	keys := make([]string, 0, len(types))
	for t := range types {
		keys = append(keys, t)
	}
	sort.Strings(keys)
	var out []string
	for _, t := range keys {
		if b[t] == a[t] {
			continue
		}
		out = append(out, fmt.Sprintf("%-20s %d → %d (%+d)", t, b[t], a[t], a[t]-b[t]))
	}
	return out
}

func countBlockingType(rep layoutScoreReport, typ string) int {
	n := 0
	for _, f := range rep.Blocking {
		if f.Type == typ {
			n++
		}
	}
	return n
}
