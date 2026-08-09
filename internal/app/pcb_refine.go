package app

// pcb_refine.go — 打分驱动的精修环（#167 ACHIEVE 层）+ PCB 侧第一个回滚器。
//
// #167 的收敛环长这样：
//
//	layout-score → 哪维低就对症下确定性变换 → 重新打分 → 循环到每维过阈值
//
// 关键在「对症」和「可回退」两件事，而这两件事此前在 PCB 侧都不存在：
//
//   - **对症**靠 layout-score 的逐维归因（Contributors）。没有归因的分数只能告诉你
//     "62 分"，不能告诉你该动谁——那样的环只能整盘重跑 auto-place，等于打地鼠。
//   - **可回退**是 #153 用实测换来的硬要求：`silk-align` 那轮 cleanup 把
//     `silk-over-pad` 从 0 条推到 3 条，而它自己对这几个都报 `clean: true`。
//     结论原文是「`tidy` 的『任一步新增 check finding 就回滚』这条护栏不是可选项，
//     是必需的」——没有旁边那个人跑 `pcb check` 对账，就会静默留下 3 条压焊盘的
//     丝印进 Gerber。
//
// PCB 侧此前**零回滚**：两个规划器的 apply 循环遇到失败只 append failures 继续跑，
// 结果是「一部分件按新方案、一部分留在旧位置」的混合态。精修环在这种状态上再打分，
// 梯度是假的。所以这个文件先把回滚做出来，再谈迭代。
//
// 回滚骨架照抄原理图侧 rollbackAutolayout（cmd_sch_autolayout_run.go:890）的三条
// 要领，它们都是踩出来的：
//  1. attempted 在 dispatch **之前** append —— 动作可能"失败"但已经改了画布，
//     只有把它算进待回滚集才安全；
//  2. 拒绝 unrollbackable move —— 拿不到原位就不动，而不是动完再说；
//  3. 只有**回读证实**的坐标才算 restored —— HTTP ok 不是保证，编辑器可能在模型
//     落定前就先应答了。

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// refineMove 是一次器件位移/旋转，自带回滚所需的原位。
type refineMove struct {
	ID         string  `json:"primitiveId"`
	Designator string  `json:"designator"`
	FromX      float64 `json:"fromX"`
	FromY      float64 `json:"fromY"`
	ToX        float64 `json:"toX"`
	ToY        float64 `json:"toY"`
	FromRot    float64 `json:"fromRotation,omitempty"`
	ToRot      float64 `json:"toRotation,omitempty"`
	SetRot     bool    `json:"setRotation,omitempty"`
	// HasOriginal 为 false 时**绝不能**下发：没有原位就回不去，而一个回不去的
	// 变换在"新增 finding 就回滚"的护栏下是不可接受的。
	HasOriginal bool   `json:"-"`
	Why         string `json:"why,omitempty"`
}

// shift 是这次移动的欧氏位移（mil）——位移预算按它裁。
func (m refineMove) shift() float64 { return math.Hypot(m.ToX-m.FromX, m.ToY-m.FromY) }

// refineStep 是精修环里的一步：一次针对某一维的确定性变换。
//
// 粒度刻意是「步」而不是「整条命令」：#153 那轮 cleanup 里 grid-snap 是纯收益、
// silk-align 制造了回归，如果按命令粒度回滚，会把好的那步也一起吐掉。
type refineStep struct {
	Name      string       `json:"name"`      // grid-snap / align-rows / nudge-facing …
	Dimension string       `json:"dimension"` // 针对哪一维（layout-score 的 dim id）
	Moves     []refineMove `json:"moves,omitempty"`
	Applied   int          `json:"applied"`
	Skipped   int          `json:"skipped"`

	ScoreBefore float64 `json:"scoreBefore"`
	ScoreAfter  float64 `json:"scoreAfter"`
	// FindingsBefore/After 是 pcb check 的可门控条数。任一步让它**上升**就回滚，
	// 哪怕分数涨了 —— 分数是启发式，check finding 是会进 Gerber 的真问题。
	FindingsBefore int      `json:"findingsBefore"`
	FindingsAfter  int      `json:"findingsAfter"`
	RolledBack     bool     `json:"rolledBack"`
	Reason         string   `json:"reason,omitempty"`
	Restored       int      `json:"restored,omitempty"` // 回读证实回到原位的件数
	Errors         []string `json:"errors,omitempty"`
}

// refineOpts 是环的护栏参数。默认值全部偏保守：精修的价值在于"稳赚不赔"，
// 一个会偶尔搞坏板子的自动美化工具没人敢开。
type refineOpts struct {
	// MaxShiftMil 是单件位移上限。#153 的原话是「保证只做『吸附』不做『重排』」，
	// 默认取网格步长。超限的移动被跳过而不是截断——截断会把件放到一个既不是原位
	// 也不是目标的地方，那是最坏的结果。
	MaxShiftMil float64
	// MaxRounds 是收敛轮数上限。
	MaxRounds int
	// TargetScore 是每维的过关线；所有参与加权的维都达标就提前收敛。
	TargetScore float64
	// DryRun 只出计划不落笔（#153 要求 --dry-run 必须能先跑）。
	DryRun bool
	// IncludeLocked 打开后才碰锁定件（默认不碰）。
	IncludeLocked bool
}

func defaultRefineOpts() refineOpts {
	return refineOpts{MaxShiftMil: 5, MaxRounds: 4, TargetScore: 85}
}

// refineReport 是整个环的结果。
type refineReport struct {
	OK          bool         `json:"ok"`
	Rounds      int          `json:"rounds"`
	Steps       []refineStep `json:"steps"`
	ScoreBefore float64      `json:"scoreBefore"`
	ScoreAfter  float64      `json:"scoreAfter"`
	MovedParts  int          `json:"movedParts"`
	Immovable   int          `json:"immovableParts"`
	DryRun      bool         `json:"dryRun"`
	Converged   bool         `json:"converged"`
	// Blocking = layout-score 的一票否决项(短路/重叠/出板框)在**精修开始时**的数量。
	// refine 的变换器都修不了它们(它做的是维度微调,不是布局合法化)——非零时报告
	// 必须显眼地说出来,否则「refine OK」会被误读成「板子没问题」。
	Blocking int      `json:"blockingIssues,omitempty"`
	Summary  string   `json:"summary"`
	Warnings []string `json:"warnings,omitempty"`
}

// ---------------------------------------------------------------------------
// 不可动集合
// ---------------------------------------------------------------------------

// immovableSet 是精修**绝不能碰**的器件位号集合。
//
// #153 原文：「锁定件、edge-bound 件、`stage confirm-tier` 已确认的功能位一律不动」。
// 这三类的共同点是：它们的位置是**人或工艺决定的**，不是几何优化的产物 ——
// 挪动它们等于用一个启发式覆盖掉一个决定。
//
// 数据其实一直是齐的（workflow.State.PlacementTiers 里有逐档确认的位号 + 指纹），
// 但**没有任何执行器读过它**：place-constrained 完全不看 locked，
// align/distribute/grid-snap/move 里也只有 arrange 过滤了锁定件。这个函数是第一个。
type immovableReason struct {
	Designator string
	Reason     string
}

func buildImmovableSet(snap *boardSnapshot, tiers map[int][]string, includeLocked bool) (map[string]string, []immovableReason) {
	out := map[string]string{}
	var list []immovableReason
	add := func(des, why string) {
		key := strings.ToUpper(strings.TrimSpace(des))
		if key == "" {
			return
		}
		if _, dup := out[key]; dup {
			return
		}
		out[key] = why
		list = append(list, immovableReason{Designator: des, Reason: why})
	}

	if !includeLocked {
		for _, c := range snap.Components {
			if c.Locked {
				add(c.Designator, "locked in the editor")
			}
		}
	}
	// 已签字的分档：档 1(孔/结构件) 与档 2(边缘接口件，朝向经用户确认) 是硬的；
	// 档 3/4 也签过字，但它们是几何摆放的结果，精修动它们是本分。
	// 这里的取舍：**只保护 1/2 档**，让 3/4 档可被吸附微调（位移仍受 MaxShift 限制）。
	for tier, parts := range tiers {
		if tier > 2 {
			continue
		}
		for _, p := range parts {
			add(p, fmt.Sprintf("tier-%d confirmed (%s)", tier, tierPurpose(tier)))
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Designator < list[j].Designator })
	return out, list
}

func tierPurpose(tier int) string {
	switch tier {
	case 1:
		return "mounting holes / mechanical"
	case 2:
		return "edge connectors — orientation was user-confirmed"
	default:
		return "confirmed"
	}
}

// ---------------------------------------------------------------------------
// 位移预算
// ---------------------------------------------------------------------------

// budgetMoves 按护栏裁一批候选移动：剔除不可动件、超位移件、以及位移小到没意义的。
//
// 返回 (放行, 拒绝原因)。**超限的移动被剔除而不是截断**：截断会把件放到既不是
// 原位也不是目标的第三个位置，那比不动更糟。
func budgetMoves(moves []refineMove, immovable map[string]string, maxShift float64) ([]refineMove, []string) {
	var out []refineMove
	var rejects []string
	for _, m := range moves {
		key := strings.ToUpper(m.Designator)
		if why, blocked := immovable[key]; blocked {
			rejects = append(rejects, fmt.Sprintf("%s: not moved — %s", m.Designator, why))
			continue
		}
		if !m.HasOriginal || m.ID == "" {
			// 没有原位 = 回不去。宁可不做（对齐 applyAutolayout 的
			// "refusing an unrollbackable move" 守卫）。
			rejects = append(rejects, fmt.Sprintf("%s: refusing an unrollbackable move (no original anchor / primitive id)", m.Designator))
			continue
		}
		if s := m.shift(); maxShift > 0 && s > maxShift {
			rejects = append(rejects, fmt.Sprintf("%s: %.2f mil exceeds the %.1f mil shift budget — snapping only, not rearranging", m.Designator, s, maxShift))
			continue
		}
		// 亚 0.01 mil 的移动是浮点噪声，发出去只会白白触发 InvalidatesStage
		// 和 autosave（auto-place 不幂等的老毛病就是这么来的）。
		if m.shift() < 0.01 && !m.SetRot {
			continue
		}
		out = append(out, m)
	}
	return out, rejects
}

// ---------------------------------------------------------------------------
// 应用与回滚
// ---------------------------------------------------------------------------

// applyRefineMoves 下发一批移动，全程记录已尝试项以备回滚。
//
// 与既有 auto-place / place-constrained 的 apply 循环的**关键区别**：那两个遇到
// 单件失败只 append failures 继续跑，留下混合态；这里任一件失败就立刻停并把
// 已下发的全部回滚 —— 精修环要在结果上再打分，混合态会让梯度是假的。
func applyRefineMoves(cfg *appConfig, window string, moves []refineMove, stderr io.Writer) (attempted []refineMove, applied int, err error) {
	for _, m := range moves {
		patch := map[string]any{"x": m.ToX, "y": m.ToY}
		if m.SetRot {
			patch["rotation"] = m.ToRot
		}
		// 先入待回滚集再下发：动作可能"失败"却已经改了画布。
		attempted = append(attempted, m)
		if _, aerr := requestAction(cfg, "pcb.component.modify", window, map[string]any{
			"primitiveId": m.ID,
			"patch":       patch,
		}); aerr != nil {
			return attempted, applied, fmt.Errorf("move %s: %w", m.Designator, aerr)
		}
		applied++
	}
	return attempted, applied, nil
}

// rollbackRefineMoves 逆序还原并**回读证实**。
//
// 返回证实回到原位的件数。没被证实的逐条进 errors —— 静默的"回滚成功"是这个项目
// 反复吃过亏的东西（平台的 delete 会假成功、setState 不 done() 也返回真），
// 所以判据一律是回读，不是应答。
func rollbackRefineMoves(cfg *appConfig, window string, attempted []refineMove, stderr io.Writer) (restored int, errs []string) {
	if len(attempted) == 0 {
		return 0, nil
	}
	for i := len(attempted) - 1; i >= 0; i-- {
		m := attempted[i]
		if !m.HasOriginal || m.ID == "" {
			msg := fmt.Sprintf("rollback %s: no original anchor — cannot restore", m.Designator)
			errs = append(errs, msg)
			fmt.Fprintf(stderr, "refine: %s\n", msg)
			continue
		}
		patch := map[string]any{"x": m.FromX, "y": m.FromY}
		if m.SetRot {
			patch["rotation"] = m.FromRot
		}
		if _, err := requestAction(cfg, "pcb.component.modify", window, map[string]any{
			"primitiveId": m.ID, "patch": patch,
		}); err != nil {
			msg := fmt.Sprintf("rollback %s to (%.2f, %.2f): %v", m.Designator, m.FromX, m.FromY, err)
			errs = append(errs, msg)
			fmt.Fprintf(stderr, "refine: %s\n", msg)
		}
	}

	// 回读证实。PCB 侧读回前要注意 stale：mutation 后第一次读可能是旧值
	// （铁律 5）。这里读的是刚写过的同一批 primitive，用 components.list 直读；
	// 若出现大面积"未证实"，调用方应 doc reload 后重查而不是直接相信。
	res, err := requestAction(cfg, "pcb.components.list", window, map[string]any{})
	if err != nil {
		msg := fmt.Sprintf("rollback verification read failed: %v", err)
		return 0, append(errs, msg)
	}
	byID := map[string]boardComp{}
	for _, c := range parseBoardComponents(res.Result) {
		byID[c.ID] = c
	}
	for _, m := range attempted {
		c, ok := byID[m.ID]
		if ok && math.Abs(c.X-m.FromX) <= refineCoordEps && math.Abs(c.Y-m.FromY) <= refineCoordEps {
			restored++
			continue
		}
		msg := fmt.Sprintf("rollback %s not confirmed at (%.2f, %.2f)", m.Designator, m.FromX, m.FromY)
		errs = append(errs, msg)
		fmt.Fprintf(stderr, "refine: %s\n", msg)
	}
	return restored, errs
}

// refineCoordEps 是回读比对的坐标容差（mil）。平台会把坐标量化，严格相等比不出来。
const refineCoordEps = 0.05

// ---------------------------------------------------------------------------
// 变换：grid-snap
// ---------------------------------------------------------------------------

// planGridSnap 生成落格移动 —— 精修环的第一个、也是最安全的变换。
//
// #153 在 BBClaw（69 器件）上的实测：只对 37 个未锁定卫星件跑 grid-snap，
// C/R/D 落 5mil 网格从 16/39 变成 37/39，最大位移 **1.998 mil**，
// pcb check findings 25→25 不变，layout-lint 分数不动。原话是「零副作用，纯收益」。
//
// 坐标长这样：`C2(635.0015, 1109.998)`、`C6(455.0015, 839.998)` —— 典型的
// auto-place / GUI 拖动留下的亚 mil 漂移，目视看不出来，但让行列对齐永远差一点。
//
// 网格默认 5mil 而不是 conventions §9.1 写的 25mil：两者目的不同。25mil 是"该吸到
// 哪"的目标网格，5mil 是"有没有亚 mil 脏值"的判据。精修环要做的是后者——
// 把件从 635.0015 拉回 635，而不是把它搬到 625。
//
// 公制间距件与锁定件排除（conventions §9.1）：吸栅会把它们的 pad 推离原生子栅。
func planGridSnap(snap *boardSnapshot, gridMil float64, immovable map[string]string) []refineMove {
	if gridMil <= 0 {
		gridMil = 5
	}
	var out []refineMove
	for _, c := range snap.Components {
		if _, blocked := immovable[strings.ToUpper(c.Designator)]; blocked {
			continue
		}
		if isMetricPitchPart(c) {
			continue
		}
		nx := math.Round(c.X/gridMil) * gridMil
		ny := math.Round(c.Y/gridMil) * gridMil
		if math.Abs(nx-c.X) < 1e-9 && math.Abs(ny-c.Y) < 1e-9 {
			continue
		}
		out = append(out, refineMove{
			ID: c.ID, Designator: c.Designator,
			FromX: c.X, FromY: c.Y, ToX: nx, ToY: ny,
			HasOriginal: c.ID != "",
			Why:         fmt.Sprintf("off-grid by (%.4f, %.4f) mil", nx-c.X, ny-c.Y),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Designator < out[j].Designator })
	return out
}

// metricPitchHints 是公制间距器件的设备名特征。吸英制栅会把它们的焊盘推离自己的
// 原生子栅（conventions §9.1 明确要求排除）。表按设备名小写子串匹配，
// 与块库 openings/keepout 的匹配范式一致。
var metricPitchHints = []string{
	"jst", "ph2.0", "ph-2.0", "xh2.54", "zh1.5", "sh1.0", "molex",
	"type-c", "typec", "usb-c", "micro-usb", "microusb",
	"qfn", "bga", "csp", "lga", // 0.4/0.5/0.65/0.8mm pitch
	"0.4mm", "0.5mm", "0.65mm", "0.8mm", "1.0mm", "1.25mm", "1.5mm", "2.0mm",
}

// isMetricPitchPart 判断器件是否是公制间距件（不该被吸到英制栅上）。
//
// 判据是设备名关键词 —— 这是保守的：漏判一个（把公制件吸了栅）的代价是它的焊盘
// 偏离原生子栅，比误判一个（放过一个本可以吸的件）严重得多。
func isMetricPitchPart(c boardComp) bool {
	dev := strings.ToLower(c.Device + " " + c.Name)
	for _, h := range metricPitchHints {
		if strings.Contains(dev, h) {
			return true
		}
	}
	return false
}
