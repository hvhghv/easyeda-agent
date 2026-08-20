package app

// cmd_sch_block_apply_run.go — the I/O side of `sch block-apply` (the planner in
// cmd_sch_block_apply.go is pure).
//
// Pipeline, matching the vertical slice in chat/2026-07-16-blocks-data-model.md:
//   1. load the block (go:embed, offline)
//   2. resolve parts → standard-parts.json (the role-id → deviceUuid bridge)
//   3. read the page's existing designators so a second instance never collides
//   4. plan (pure)
//   5. place each role and retain the returned primitiveId
//   5b. re-read real bbox/pin geometry; fail closed and compensate by those IDs
//   6. only after 5b passes, wire internal_nets via the autoconnect planner, which
//      already
//      owns the geometry safety (pin → stub wire → flag, never a flag on a bare pin)
//   7. schematic.check
//   8. emit the instance manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
)

// bapManifest is the command's output: what was built, from which block
// revision, and — deliberately — what was NOT honoured.
type bapManifest struct {
	OK         string         `json:"ok"`
	BlockID    string         `json:"blockId"`
	Revision   int            `json:"revision,omitempty"`
	BlockState string         `json:"blockState"` // ready | verified | draft — never let a draft look production-ready
	Instance   string         `json:"instance"`
	Origin     *bapOrigin     `json:"origin,omitempty"`
	Placed     []bapPlacement `json:"placed"`
	Nets       []bapNet       `json:"nets"`
	Warnings   []string       `json:"warnings,omitempty"`
	Unconsumed []string       `json:"unconsumedConstraints,omitempty"`
	Note       string         `json:"note,omitempty"`
	// GroupID is the persistent virtual group this instance was registered as
	// (ADR-0003 第一步:归组即封刚体)。空 = 登记失败,已在 stderr 说明原因。
	GroupID   string `json:"groupId,omitempty"`
	GroupName string `json:"groupName,omitempty"`
	// Reconciled is true when the post-apply netlist read-back matched every
	// planned net (issue #135); Diffs carries the mismatches when it did not.
	Reconciled bool         `json:"reconciled,omitempty"`
	Diffs      []bapNetDiff `json:"diffs,omitempty"`
	// LayoutOverlaps is the post-apply real-bbox overlap read-back (the same
	// geometry `sch layout-lint` checks), restricted to pairs that involve this
	// instance's parts — the mechanical answer to "did the block land clean".
	// This manifest retains native schematic units; layout-lint schema v2 converts
	// its user-facing distance fields to mm.
	LayoutOverlaps        []layoutFinding `json:"layoutOverlaps,omitempty"`
	LayoutMeasurementUnit string          `json:"layoutMeasurementUnit,omitempty"`
	LayoutCoordinateUnit  string          `json:"layoutCoordinateUnit,omitempty"`
	// Renames records PLANNED → ACTUAL designators when EasyEDA re-numbered on
	// create (issue #144). Non-empty is normal, not a failure; it exists so the
	// manifest never claims a designator the board does not carry.
	Renames map[string]string `json:"designatorRenames,omitempty"`
	// A failed post-place safety gate is still observable state: Failure names
	// the cause, Rollback records exactly what cleanup was attempted and proven,
	// and PartialState is true whenever the canvas cannot be proven restored.
	// This is intentionally not presented as a transaction — EasyEDA mutations
	// autosave independently.
	Failure      string             `json:"failure,omitempty"`
	PartialState bool               `json:"partialState,omitempty"`
	Rollback     *bapRollbackReport `json:"rollback,omitempty"`
}

// bapRollbackReport is the audit trail for best-effort cleanup after placement
// but before wiring. Complete is true only after a fresh components.list proves
// every tracked primitive ID is absent and every successful placement returned
// an ID. MissingPrimitiveIDs contains designators whose successful place response
// could not identify the newly created primitive; those are never guessed at.
// AdoptedPrimitiveIDs 是**超时收编**认回来的 id:place 的回执丢了,但落地前
// 快照的差集 + 下发坐标证明这些器件是本命令建出来的(sch_place_adopt.go)。它们
// 与 AttemptedPrimitiveIDs 走同一条「逐个删 + 回读证实」的清扫,只是来源不同 ——
// 单独列出来,好让报文能说清「我没拿到回执,但我知道它是谁」。
type bapRollbackReport struct {
	AttemptedPrimitiveIDs []string `json:"attemptedPrimitiveIds,omitempty"`
	AdoptedPrimitiveIDs   []string `json:"adoptedPrimitiveIds,omitempty"`
	MissingPrimitiveIDs   []string `json:"missingPrimitiveIds,omitempty"`
	SurvivedPrimitiveIDs  []string `json:"survivedPrimitiveIds,omitempty"`
	Verified              bool     `json:"verified"`
	Complete              bool     `json:"complete"`
	DeleteError           string   `json:"deleteError,omitempty"`
	VerifyError           string   `json:"verifyError,omitempty"`
}

// fetchSchObstacles pulls the ACTIVE page's real part bboxes (best-effort) so
// the planner can dodge them when picking the block origin.
func fetchSchObstacles(cfg *appConfig, window string) []layoutBBox {
	parts, _, _, _ := fetchSchObstaclesAndKeepout(cfg, window)
	return parts
}

// fetchSchObstaclesAndKeepout is fetchSchObstacles plus the A4 title-block
// keep-out derived from the same components.list round-trip (issue #141). The
// "sheet" component spans the whole page and carries the frame bbox; feeding it
// through titleBlockKeepout (the single source of the keep-out geometry, shared
// with autoconnect/autolayout) yields the bottom-right 图签/明细表 rectangle so
// bapResolveOrigin never drops a block onto it. A missing/underivable sheet
// bbox degrades to nil (no keep-out enforced), matching the other callers.
//
// 第四个返回值是**落地前快照**:活动页每一个器件的 primitiveId(任意
// componentType)。它是超时收编唯一能保证不误认的门 —— 见 sch_place_adopt.go。
// 复用这一次 components.list,不额外增加往返;读失败时返回 nil,调用方据此关掉
// 收编(没有快照就只能猜,而猜等于允许误删)。
func fetchSchObstaclesAndKeepout(cfg *appConfig, window string) ([]layoutBBox, *layoutBBox, *layoutBBox, map[string]bool) {
	res, err := requestAction(cfg, "schematic.components.list", window, map[string]any{"includeBBox": true})
	if err != nil {
		return nil, nil, nil, nil
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return nil, nil, nil, nil
	}
	snapshot := schPageComponentSnapshot(comps)
	var sheet *layoutBBox
	for _, c := range comps {
		if c.ComponentType == "sheet" && c.BBox != nil {
			sheet = c.BBox
			break
		}
	}
	kept, _ := filterLayoutComps(comps, false)
	var out []layoutBBox
	for _, c := range kept {
		if c.BBox != nil {
			out = append(out, *c.BBox)
		}
	}
	tb, _ := titleBlockKeepout(sheet)
	return out, tb, sheet, snapshot
}

// verifyBlockLayout re-reads the page's real bboxes and pins after placement and
// returns the hard findings that involve the freshly placed primitive IDs.
//
// This is a wiring gate, not telemetry: every read/parse/geometry-completeness
// failure is returned as an error. A missing bbox or pins array means the page
// was not actually checked, so it must never collapse into "no findings".
// 返回值分两组:blocking(重叠/引脚重合 —— wiring 前硬门,触发整单回滚)与
// advisory(出图纸 —— 与 `sch layout-lint` 的分档一致,只警告不回滚:块比图纸大
// 是版面决策,不该让 apply 变成不可能)。
//
// **这一步带 includePins,而带引脚的回读会顺带跑一次 netlist 导出** —— 导出之后紧接着
// 发 component.modify 会被平台拒掉(见 bslMoveComponentX)。所以实测推让排在它**前面**,
// 而它验的是推让之后的最终几何。
func verifyBlockLayout(cfg *appConfig, window string, placed []bapPlacement) ([]layoutFinding, []layoutFinding, error) {
	// includePins is load-bearing: PIN COINCIDENCE is the failure this check exists
	// for. Two parts can sit at a clean bbox distance and still land a pin of one
	// exactly on a pin of the other — an implicit short with no wire to show for it,
	// invisible to an overlap-only scan. Real case: the grid fallback put CH334F's
	// U3:20 (VDD33) and the crystal's X2:4 (GND) both at (470,510), so the GND stub
	// bonded straight onto VDD33 while this check happily printed "✓ no overlap".
	// 预算要显式给足:includePins 的代价随页面引脚数涨(一颗 81 脚模组实测 18s),
	// 默认 20s 会随机超时 —— 而这一步超时等于**硬门没跑成**,比慢更糟。
	res, err := requestActionTimed(cfg, "schematic.components.list", window,
		map[string]any{"includeBBox": true, "includePins": true}, 90*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("read components with real bbox/pin geometry: %w", err)
	}
	if err := validateBlockLayoutResult(res.Result); err != nil {
		return nil, nil, err
	}
	comps, err := parseLayoutComps(res.Result)
	if err != nil {
		return nil, nil, fmt.Errorf("parse components with real bbox/pin geometry: %w", err)
	}
	kept, _ := filterLayoutComps(comps, false)
	// minGap 0 → true overlaps only (tight spacing is not this gate's business);
	// pinEps 0 → strict pin equality, the same default `sch layout-lint` uses.
	rep := analyzeLayout(kept, 0, 0)
	// 出图纸判据也在这里跑,用的是**实测 bbox**。此前 block-apply 自己这条链上
	// 只有事后拿**锚点**比 sheet 的一条 warning —— 锚点在框内而 body 探出框外
	// 就漏报,而且要等用户另跑一次 layout-lint 才看得见(issue #180)。
	// sheet 必须从**未过滤**的 comps 取:filterLayoutComps 把 sheet 本身滤掉了。
	if sheet := sheetBBoxOf(comps); sheet != nil {
		rep.OutOfSheet = detectOutOfSheet(kept, *sheet, sheetEdgeMinGap)
	}
	mineIDs := map[string]bool{}
	mineLabels := map[string]bool{}
	for _, p := range placed {
		if strings.TrimSpace(p.PrimitiveID) == "" {
			return nil, nil, fmt.Errorf("placed component %s has no primitiveId; layout ownership and rollback cannot be proven", p.Designator)
		}
		mineIDs[p.PrimitiveID] = true
		mineLabels[strings.ToUpper(p.Designator)] = true
	}
	foundIDs := map[string]bool{}
	for _, c := range kept {
		if mineIDs[c.ID] {
			foundIDs[c.ID] = true
			// Use the live label as well as the post-place designator. This keeps
			// filtering correct if the platform normalized case in its read-back.
			mineLabels[strings.ToUpper(label(c))] = true
		}
	}
	var missing []string
	for id := range mineIDs {
		if !foundIDs[id] {
			missing = append(missing, id)
		}
	}
	// 落地回读的结论回传给 daemon 写健康度(通道 B):每次 place 大多返回成功,
	// 只有这次回读知道有几件真在页面上。真机跑出过「6 件只落地 1 件」而
	// writeHealth 全程 0.05/绿灯 —— 那是因为这个结论以前只打印在 CLI 层。
	// 响应里挖不出这个判决,所以必须走上报通道而不是 daemon 内省。
	reportWriteVerified(cfg, window, writeVerdict{
		action: "schematic.component.place", source: "sch block-apply",
		returnedOK: true, landed: len(foundIDs), notLanded: len(missing),
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, nil, fmt.Errorf("layout read-back did not contain placed primitiveId(s): %s", strings.Join(missing, ", "))
	}
	mine := func(f layoutFinding) bool {
		return mineLabels[strings.ToUpper(f.A)] || mineLabels[strings.ToUpper(f.B)]
	}
	var blocking, advisory []layoutFinding
	for _, f := range append(append([]layoutFinding{}, rep.Overlaps...), rep.PinCoincidences...) {
		if mine(f) {
			blocking = append(blocking, f)
		}
	}
	for _, f := range rep.OutOfSheet {
		if mine(f) {
			advisory = append(advisory, f)
		}
	}
	return blocking, advisory, nil
}

// validateBlockLayoutResult enforces the data contract needed to prove a clean
// landing. parseLayoutComps is intentionally permissive for diagnostic callers;
// block-apply cannot be permissive because it is about to create electrical
// connections based on this result.
func validateBlockLayoutResult(result map[string]any) error {
	raw, ok := result["components"].([]any)
	if !ok {
		return fmt.Errorf("parse components with real bbox/pin geometry: missing components array")
	}
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("parse components with real bbox/pin geometry: component[%d] is not an object", i)
		}
		componentType := asString(m["componentType"])
		if componentType != "" && componentType != schLayoutPartType {
			continue
		}
		if strings.TrimSpace(asString(m["primitiveId"])) == "" {
			return fmt.Errorf("parse components with real bbox/pin geometry: component[%d] has no primitiveId", i)
		}
		bbox, ok := m["bbox"].(map[string]any)
		if !ok {
			return fmt.Errorf("layout geometry incomplete: component %s has no bbox", blockLayoutComponentLabel(m, i))
		}
		minX, okMinX := finiteFloat(bbox["minX"])
		minY, okMinY := finiteFloat(bbox["minY"])
		maxX, okMaxX := finiteFloat(bbox["maxX"])
		maxY, okMaxY := finiteFloat(bbox["maxY"])
		if !okMinX || !okMinY || !okMaxX || !okMaxY || maxX <= minX || maxY <= minY {
			return fmt.Errorf("layout geometry malformed: component %s has invalid bbox", blockLayoutComponentLabel(m, i))
		}
		pinsAvailable, proofKnown := m["pinsAvailable"].(bool)
		if !proofKnown || !pinsAvailable {
			detail := strings.TrimSpace(asString(m["pinsError"]))
			if detail != "" {
				detail = ": " + detail
			}
			return fmt.Errorf("layout geometry incomplete: component %s pin read was not explicitly proven%s",
				blockLayoutComponentLabel(m, i), detail)
		}
		pins, ok := m["pins"].([]any)
		if !ok {
			return fmt.Errorf("layout geometry incomplete: component %s has no pins array", blockLayoutComponentLabel(m, i))
		}
		for pinIndex, item := range pins {
			pin, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("layout geometry malformed: component %s pin[%d] is not an object",
					blockLayoutComponentLabel(m, i), pinIndex)
			}
			if _, ok := finiteFloat(pin["x"]); !ok {
				return fmt.Errorf("layout geometry malformed: component %s pin[%d] has no numeric x",
					blockLayoutComponentLabel(m, i), pinIndex)
			}
			if _, ok := finiteFloat(pin["y"]); !ok {
				return fmt.Errorf("layout geometry malformed: component %s pin[%d] has no numeric y",
					blockLayoutComponentLabel(m, i), pinIndex)
			}
		}
	}
	return nil
}

func blockLayoutComponentLabel(component map[string]any, index int) string {
	if designator := strings.TrimSpace(asString(component["designator"])); designator != "" {
		return designator
	}
	if id := strings.TrimSpace(asString(component["primitiveId"])); id != "" {
		return id
	}
	return fmt.Sprintf("[%d]", index)
}

// loadStandardParts reads the parts library into the role-id → device bridge.
func loadStandardParts(path string) (map[string]bapDevice, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		LibraryUUID string `json:"libraryUuid"`
		Parts       map[string]struct {
			MPN        string `json:"mpn"`
			LCSC       string `json:"lcsc"`
			Value      any    `json:"value"`
			DeviceUUID string `json:"deviceUuid"`
			Basic      bool   `json:"basic"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make(map[string]bapDevice, len(doc.Parts))
	for k, p := range doc.Parts {
		out[k] = bapDevice{
			LibraryUUID: doc.LibraryUUID,
			DeviceUUID:  p.DeviceUUID,
			MPN:         p.MPN,
			LCSC:        p.LCSC,
			Value:       asString(p.Value),
			Basic:       p.Basic,
		}
	}
	return out, nil
}

// blockTopology pulls internal_nets out of the block's raw JSON. The typed
// projection deliberately keeps it in Raw (unknown maps stay forward-compatible),
// so the executable core parses it here.
func blockTopology(b blocks.Block) ([][]string, error) {
	var doc struct {
		InternalNets [][]string `json:"internal_nets"`
	}
	if err := json.Unmarshal(b.Raw, &doc); err != nil {
		return nil, fmt.Errorf("parse internal_nets: %w", err)
	}
	return doc.InternalNets, nil
}

// parseKV splits repeatable KEY=VALUE flags.
func parseKV(items []string, flag string) (map[string]string, error) {
	out := map[string]string{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("%s %q: expected KEY=VALUE", flag, it)
		}
		out[k] = v
	}
	return out, nil
}

// existingDesignators reads the WHOLE document (all schematic pages) so
// allocation can skip taken designators. Active-page-only scanning caused issue
// #136: an instance on a fresh page allocated C1/R1/U1 colliding with another
// page's parts, and the document-wide netlist (keyed by designator.pin) then
// mis-attributed every collided pin's net. Non-active pages return shallow data
// but the designator field is always present, which is all this needs.
//
// allPages alone is NOT enough: EasyEDA loads page data lazily, so
// getAll(_, allPages) returns only pages that have been OPENED this session — a
// never-visited page stays invisible here while still steering the platform's own
// numbering, which is how issue #144 planned C1 against a page already holding
// C1-C10. tagPages makes the connector visit every page (and restore the original)
// before the scan, which loads them; the remaining drift is caught by the
// post-place designator read-back in runBlockApply.
func existingDesignators(cfg *appConfig, window string) (map[string]bool, error) {
	res, err := requestAction(cfg, "schematic.components.list", window,
		map[string]any{"allPages": true, "tagPages": true})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	raw, _ := res.Result["components"].([]any)
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if d := asString(m["designator"]); d != "" {
			out[strings.ToUpper(d)] = true
		}
	}
	return out, nil
}

// bapPlacedDesignator digs the authoritative designator out of a
// schematic.component.place response ({component:{designator}}). Empty means the
// response did not carry one — the caller then keeps the planned name rather than
// guessing, so an older connector degrades to the previous behaviour instead of
// silently clearing designators.
func bapPlacedDesignator(res *actionResult) string {
	if res == nil {
		return ""
	}
	comp, _ := res.Result["component"].(map[string]any)
	if comp == nil {
		return ""
	}
	return strings.TrimSpace(asString(comp["designator"]))
}

// bapPlacedPrimitiveID returns the only safe rollback handle for a newly placed
// component. Current connectors expose it at result.primitiveId and also inside
// the serialized component; accepting both keeps the reader tolerant without
// ever guessing an ID from a designator.
func bapPlacedPrimitiveID(res *actionResult) string {
	if res == nil {
		return ""
	}
	if id := strings.TrimSpace(asString(res.Result["primitiveId"])); id != "" {
		return id
	}
	comp, _ := res.Result["component"].(map[string]any)
	return strings.TrimSpace(asString(comp["primitiveId"]))
}

// rollbackBlockPlacements removes only primitives whose IDs came directly from
// successful place responses, then independently reads the page back. It never
// falls back to deleting by designator: a stale/duplicate label is not a safe
// mutation target.
//
// 缺陷 3(P1):批量 component.delete 不可靠(平台大批量静默 no-op 仍返 true;
// 真机实锤 deleted=false / survived=4,而逐个删成功率 100%)。删除因此走
// deleteVerifiedOneByOne:逐个删 + 回读证实 + 幸存者重试一次。判定只信回读。
func rollbackBlockPlacements(cfg *appConfig, window string, placed []bapPlacement,
	uncertain, adopted []string) bapRollbackReport {

	rep := bapRollbackReport{}
	for _, p := range placed {
		if id := strings.TrimSpace(p.PrimitiveID); id != "" {
			rep.AttemptedPrimitiveIDs = append(rep.AttemptedPrimitiveIDs, id)
		} else {
			rep.MissingPrimitiveIDs = append(rep.MissingPrimitiveIDs, p.Designator)
		}
	}
	rep.MissingPrimitiveIDs = append(rep.MissingPrimitiveIDs, uncertain...)
	sweepIDs := append([]string(nil), rep.AttemptedPrimitiveIDs...)
	inSweep := map[string]bool{}
	for _, id := range sweepIDs {
		inSweep[id] = true
	}
	// 收编认回的 id 进同一张清扫单。它们不是猜出来的:每一个都通过了「不在落地前
	// 快照里」这道门,所以删它们不可能碰到页面上原有的器件。已经有回执的那个
	// (单一命中时同时进了 placed)只登记不重复入单。
	for _, id := range adopted {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		rep.AdoptedPrimitiveIDs = append(rep.AdoptedPrimitiveIDs, id)
		if !inSweep[id] {
			inSweep[id] = true
			sweepIDs = append(sweepIDs, id)
		}
	}
	sort.Strings(rep.AttemptedPrimitiveIDs)
	sort.Strings(rep.AdoptedPrimitiveIDs)
	sort.Strings(rep.MissingPrimitiveIDs)
	sort.Strings(sweepIDs)

	// Nothing to delete: still do the independent read-back so "Verified" keeps
	// meaning "the page was re-read", same as before the one-by-one rewrite.
	if len(sweepIDs) == 0 {
		if _, verr := readBlockComponentIDs(cfg, window); verr != nil {
			rep.VerifyError = verr.Error()
			return rep
		}
		rep.Verified = true
		rep.Complete = len(rep.MissingPrimitiveIDs) == 0
		return rep
	}

	sweep, err := deleteVerifiedOneByOne(sweepIDs,
		func(id string) error {
			res, derr := requestAction(cfg, "schematic.component.delete", window,
				map[string]any{"primitiveIds": []string{id}})
			if derr != nil {
				return derr
			}
			// ADR-0004 Decision 5: the connector cascades exclusive stub trees +
			// riding flags — report the cleanup so it never looks like data loss.
			printCascadeCleanup(res, os.Stderr)
			if deleted, ok := res.Result["deleted"].(bool); ok && !deleted {
				return fmt.Errorf("schematic.component.delete reported deleted=false")
			}
			return nil
		},
		func() (map[string]bool, error) { return readBlockComponentIDs(cfg, window) },
	)
	if len(sweep.Errors) > 0 {
		rep.DeleteError = strings.Join(sweep.Errors, "; ")
	}
	if err != nil {
		rep.VerifyError = err.Error()
		return rep
	}
	rep.Verified = true
	rep.SurvivedPrimitiveIDs = append([]string(nil), sweep.Survived...)
	sort.Strings(rep.SurvivedPrimitiveIDs)

	// 缺陷 2(P1)的回滚支路:证实已删的器件同步摘除组注册(位号可能与某个
	// 陈旧组撞名 —— 那条注册无论如何都已失效)。Fail-soft,绝不影响回滚判定。
	if len(sweep.Deleted) > 0 {
		deleted := map[string]bool{}
		for _, id := range sweep.Deleted {
			deleted[id] = true
		}
		var gone []string
		for _, p := range placed {
			if deleted[strings.TrimSpace(p.PrimitiveID)] && strings.TrimSpace(p.Designator) != "" {
				gone = append(gone, p.Designator)
			}
		}
		cascadeSchGroupMembership(cfg, window, gone, os.Stderr)
	}

	rep.Complete = len(rep.MissingPrimitiveIDs) == 0 && len(rep.SurvivedPrimitiveIDs) == 0
	return rep
}

// bapAdoptAfterPlaceFailure 是 place 失败(超时 / 成功但没回 id)后的收编入口。
//
// 返回三样东西,对应三种可证明的结局:
//
//	placement —— 唯一命中:这次 place 的产物被认回来了,调用方把它当已放置件处理。
//	uncertain —— 什么也证明不了(没有落地前快照 / 回读失败):如实说不知道。
//	adopted   —— 收编认出的**全部** id(唯一命中时就是那一个;多个疑似时全给)。
//	             它们进回滚清扫单,并单独在报文里点名。
//
// 命中 0 个疑似且快照可用 = **证实这次 place 没落地**,三者全空 —— 那正是负对照:
// 绝不凭空造一个 id,也绝不再吓唬调用方说"可能建了个残件"。
func bapAdoptAfterPlaceFailure(cfg *appConfig, window string, known map[string]bool,
	created []bapPlacement, p bapPlacement, stderr io.Writer) (*bapPlacement, []string, []string) {

	if known == nil {
		msg := p.Designator + ": 没有落地前的器件快照,无法证明画布上多出来的是不是本次 place 的产物 —— " +
			"拒绝按坐标猜测(那会误删页面上原有的同型器件)"
		fmt.Fprintf(stderr, "adopt ✗ %s\n", msg)
		return nil, []string{msg}, nil
	}
	// known = 快照 ∪ 本命令已成功放置的 id。少了后者,先落地的件会被当成孤儿。
	scope := make(map[string]bool, len(known)+len(created))
	for id := range known {
		scope[id] = true
	}
	for _, c := range created {
		if id := strings.TrimSpace(c.PrimitiveID); id != "" {
			scope[id] = true
		}
	}

	verdict, err := schAdoptRead(cfg, window, scope, schAdoptRequest{Designator: p.Designator, X: p.X, Y: p.Y})
	if err != nil {
		msg := fmt.Sprintf("%s: 收编回读失败(%v)—— 无法判断这次 place 是否已经落地", p.Designator, err)
		fmt.Fprintf(stderr, "adopt ✗ %s\n", msg)
		return nil, []string{msg}, nil
	}
	if verdict.Adopted != nil {
		placement := p
		placement.PrimitiveID = verdict.Adopted.ID
		if d := strings.TrimSpace(verdict.Adopted.Designator); d != "" {
			placement.Designator = d
		}
		fmt.Fprintf(stderr, "adopt ✓ %s\n", verdict.Reason)
		return &placement, nil, []string{verdict.Adopted.ID}
	}
	if ids := verdict.CandidateIDs(); len(ids) > 0 {
		fmt.Fprintf(stderr, "adopt ~ %s\n", verdict.Reason)
		schAdoptResidueGuidance(stderr, ids)
		return nil, nil, ids
	}
	// 证实没落地 —— 这是好消息,说出来。旧文案无条件吓唬"可能建了个 untracked
	// 器件",于是每次超时都要人肉去页面上找,找不到也不敢确定。
	fmt.Fprintf(stderr, "adopt ✓ %s\n", verdict.Reason)
	return nil, nil, nil
}

// readBlockComponentIDs is deliberately stricter than the normal diagnostic
// parser: malformed entries make rollback unverifiable instead of disappearing
// from the live-ID set and producing a false "restored" claim.
func readBlockComponentIDs(cfg *appConfig, window string) (map[string]bool, error) {
	// Verify document-wide. An active-page-only empty result could be a false
	// success if the foreground page drifted between delete and read-back.
	// tagPages forces EasyEDA's lazily loaded pages into the allPages scan.
	res, err := requestAction(cfg, "schematic.components.list", window,
		map[string]any{"allPages": true, "tagPages": true})
	if err != nil {
		return nil, fmt.Errorf("rollback read-back: %w", err)
	}
	raw, ok := res.Result["components"].([]any)
	if !ok {
		return nil, fmt.Errorf("rollback read-back: missing components array")
	}
	out := make(map[string]bool, len(raw))
	for i, item := range raw {
		component, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("rollback read-back: component[%d] is not an object", i)
		}
		id := strings.TrimSpace(asString(component["primitiveId"]))
		if id == "" {
			return nil, fmt.Errorf("rollback read-back: component[%d] has no primitiveId", i)
		}
		out[id] = true
	}
	return out, nil
}

// failBlockApplyAfterPlacement is the single pre-wiring abort path. It emits a
// failure manifest and returns a non-nil error regardless of rollback outcome.
// A proven cleanup is "failed-rolled-back"; every uncertain/surviving component
// is "failed-partial" and called out as PARTIAL STATE.
//
// adopted 是超时收编认回的 id(见 bapAdoptAfterPlaceFailure):没有回执,但有
// 「落地前快照差集 + 下发坐标」的证明,所以照样进清扫单。
func failBlockApplyAfterPlacement(cfg *appConfig, window string, man *bapManifest,
	placed []bapPlacement, uncertain, adopted []string, cause error, asJSON bool,
	stdout, stderr io.Writer) error {

	man.Failure = cause.Error()
	man.Placed = append([]bapPlacement(nil), placed...)
	rollback := rollbackBlockPlacements(cfg, window, placed, uncertain, adopted)
	man.Rollback = &rollback
	man.PartialState = !rollback.Complete

	state := "rollback verified: all newly placed primitive IDs are absent; wiring was not started"
	swept := len(rollback.AttemptedPrimitiveIDs)
	for _, id := range rollback.AdoptedPrimitiveIDs {
		if !slices.Contains(rollback.AttemptedPrimitiveIDs, id) {
			swept++
		}
	}
	if rollback.Complete {
		man.OK = "failed-rolled-back"
		adopted := ""
		if len(rollback.AdoptedPrimitiveIDs) > 0 {
			adopted = fmt.Sprintf(" (%d of them adopted after a lost place response)", len(rollback.AdoptedPrimitiveIDs))
		}
		fmt.Fprintf(stderr, "rollback ✓ %d/%d newly placed component(s) removed and verified%s; wiring was not started\n",
			swept, swept, adopted)
	} else {
		man.OK = "failed-partial"
		var details []string
		if len(rollback.MissingPrimitiveIDs) > 0 {
			details = append(details, "no reliable primitiveId for "+strings.Join(rollback.MissingPrimitiveIDs, ", "))
		}
		if len(rollback.SurvivedPrimitiveIDs) > 0 {
			details = append(details, "survived delete: "+strings.Join(rollback.SurvivedPrimitiveIDs, ", "))
		}
		if rollback.DeleteError != "" {
			details = append(details, "delete error: "+rollback.DeleteError)
		}
		if rollback.VerifyError != "" {
			details = append(details, "verification error: "+rollback.VerifyError)
		}
		if len(details) == 0 {
			details = append(details, "cleanup could not be proven complete")
		}
		state = "PARTIAL STATE: " + strings.Join(details, "; ") + "; wiring was not started"
		fmt.Fprintf(stderr, "rollback ✗ %s\n", state)
		// 只报 PARTIAL STATE 而不说删什么,正是残件能攒到三个的原因:每一个还在
		// 页上的 id 都必须被点名,并配一条能直接跑的清理命令(判据要给下一步)。
		schAdoptResidueGuidance(stderr, rollback.SurvivedPrimitiveIDs)
	}
	if err := emitBapManifest(*man, asJSON, stdout); err != nil {
		return fmt.Errorf("block-apply stopped before wiring: %w; %s; emit failure manifest: %v", cause, state, err)
	}
	return fmt.Errorf("block-apply stopped before wiring: %w; %s", cause, state)
}

// runBlockApply is the command core.
func runBlockApply(cfg *appConfig, window, blockID string, in bapInput, partsPath string,
	dryRun, asJSON bool, stdout, stderr io.Writer) error {

	// ADR-0004 Decision 4: dry-run 必须纯计算 —— 机械保证,Mutates 派发直接被拒。
	if dryRun {
		defer setDispatchDryRun(true)()
	}
	b, ok, err := blocks.Get(blockID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such block %q — `easyeda blocks ls` to list", blockID)
	}
	in.Block = b

	if in.Topology, err = blockTopology(b); err != nil {
		return err
	}
	if len(in.Topology) == 0 {
		return fmt.Errorf("block %s has no internal_nets — nothing to wire", b.ID)
	}

	path, err := resolveStandardParts(partsPath)
	if err != nil {
		return err
	}
	if in.Devices, err = loadStandardParts(path); err != nil {
		return err
	}

	// The block's schematic placement template (nil → fallback grid).
	if in.Layout, err = b.SchematicLayout(); err != nil {
		return err
	}

	// A dry run must not need a window: the point is to inspect the plan. Only
	// the designator scan needs the page, so fall back to an empty page.
	var sheetBBox *layoutBBox
	// preplaceIDs 是「本命令开跑前页面上有哪些器件」。nil = 没读到 → 收编停用
	// (没有它就分不清「新出现」和「本来就在」,按坐标猜等于允许误删)。
	var preplaceIDs map[string]bool
	if !dryRun || window != "" || cfg.project != "" {
		if in.Existing, err = existingDesignators(cfg, window); err != nil {
			if !dryRun {
				return err
			}
			fmt.Fprintf(stderr, "warn: could not read the page (%v) — planning against an empty page\n", err)
			in.Existing = map[string]bool{}
		}
		// Existing part bboxes (active page) so the block origin dodges them,
		// plus the A4 title-block keep-out so a right/bottom origin never lands
		// on the 图签 (issue #141). The fourth value is the pre-placement id
		// snapshot the lost-response adoption diffs against (sch_place_adopt.go).
		in.Obstacles, in.TitleBlock, sheetBBox, preplaceIDs = fetchSchObstaclesAndKeepout(cfg, window)
		// 图纸边框必须**进搜索**,不能只留给事后 warning:origin 螺旋从前把
		// findSlot 的 inBounds 传成 nil,于是"最近的空位"可以落在图纸外
		// (实测 J_USB→x=-20、R6→y=880 而图纸上界 825)。issue #180 Fix B。
		in.Sheet = sheetBBox
	}

	plan, err := planBlockApply(in)
	if err != nil {
		return err
	}
	// 出图纸的判定统一走**放置后的实测 bbox**(verifyBlockLayout → detectOutOfSheet),
	// 不再在这里拿规划坐标的**锚点**比 sheet:锚点在框内而 body 探出框外就漏报 ——
	// 2026-08-13 实测,同一次 apply 锚点判据只报 LED2,bbox 判据报 LED2+R8。
	// dry-run 没有实测几何,那条路径靠 bapResolveOrigin 的 --at 越界警告兜底。
	if plan.Origin != nil && plan.Origin.Relocated {
		fmt.Fprintf(stderr, "origin: %.0f,%.0f → %.0f,%.0f (%s)\n",
			plan.Origin.RequestedX, plan.Origin.RequestedY, plan.Origin.X, plan.Origin.Y, plan.Origin.Reason)
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(stderr, "warn: %s\n", w)
	}

	man := bapManifest{
		OK: "planned", BlockID: plan.BlockID, Revision: plan.Revision,
		BlockState: b.Status(), Instance: plan.Instance, Origin: plan.Origin,
		Placed: plan.Placements, Nets: plan.Nets, Warnings: plan.Warnings,
		Unconsumed: plan.Unconsumed,
	}
	if len(plan.Unconsumed) > 0 {
		man.Note = "block-apply v1 executes parts/internal_nets/ports only; the listed constraint maps were NOT applied"
	}

	if dryRun {
		man.OK = "dry-run"
		return emitBapManifest(man, asJSON, stdout)
	}

	// A draft block's pin names are, by definition, not verified against the real
	// symbols. Placing it is legitimate (that is how a draft gets validated) but it
	// must be said out loud, not discovered when the wiring silently misses.
	if b.Status() == "draft" {
		fmt.Fprintf(stderr, "warn: block %s is a DRAFT — its pin names are unverified; expect autoconnect to fail on any wrong name\n", b.ID)
	}

	// 5. place
	//
	// The placement response carries the AUTHORITATIVE post-assignment component
	// (the connector assigns the designator via modify and returns that state), so
	// every planned designator is verified against what the board actually took.
	// EasyEDA re-numbers on create to dodge designators it knows about — including
	// ones on pages our pre-flight scan cannot see, because getAll(_, allPages)
	// only returns LOADED pages (issue #144). Planning C1 and landing C11 is normal;
	// carrying "C1" into the wiring stage is what silently connects another page's
	// part, so the plan is remapped onto reality before anything downstream runs.
	renames := map[string]string{}
	var created []bapPlacement
	// 关系形态:把锚件排到最前面。它先落地、被回读出实测引脚,其余件的位姿才算得出来
	// (attach 贴的是**具体引脚**,而引脚坐标只有放下去才知道)。issue #180 P2。
	if plan.Relational && plan.AnchorRole != "" {
		for i := range plan.Placements {
			if plan.Placements[i].Role == plan.AnchorRole {
				plan.Placements[0], plan.Placements[i] = plan.Placements[i], plan.Placements[0]
				plan.Placements[0].Source = "anchor"
				break
			}
		}
	}
	// anchorGeom 是锚件的实测 bbox + 引脚,落地后回读那一次取得;锚件此后不动,
	// 所以放置全部完成后的实测推让直接复用它,不必再要一次带引脚的回读。
	var anchorGeom *bslAnchorGeom
	for i := range plan.Placements {
		// 锚件落地后回读一次,用实测引脚求解其余件 —— 只回读这一次:后续件的尺寸
		// 用估算(只保证下限),最终由放置后的实测推让 + 硬门用真实 bbox 兜底。
		// 每件都回读会让稠密页上的 SDK 往返变成 O(件数 × 页组件数)。
		if plan.Relational && i == 1 {
			geom, notes := bslResolveLive(cfg, window, &plan, sheetBBox, stderr)
			anchorGeom = geom
			if len(notes) > 0 {
				plan.Warnings = append(plan.Warnings, notes...)
				man.Warnings = plan.Warnings
			}
			// 「NOT applied」这行是诚实性出口,它自己必须诚实:求解器**真的**消费了
			// flow/attach/pair 时(LAYOUT 列写着 anchor/flow/attach/pair),就不能再
			// 说没执行 —— 那段文案是 P1 只落数据模型时写的,P2 求解器上线后它反过来
			// 在撒谎。只有降级回网格坐标时,关系才确实没被执行。
			if bslDidSolve(notes) {
				man.Unconsumed = bapDropRelationalLayout(man.Unconsumed)
				if len(man.Unconsumed) == 0 {
					man.Note = ""
				}
			}
		}
		p := plan.Placements[i]
		payload := map[string]any{
			"libraryUuid": p.LibraryUUID,
			"uuid":        p.DeviceUUID,
			"x":           p.X,
			"y":           p.Y,
			"designator":  p.Designator,
		}
		if p.Rotation != 0 {
			payload["rotation"] = p.Rotation
		}
		res, err := requestActionTimed(cfg, "schematic.component.place", window, payload, placeTimeout)
		if err != nil {
			// 假失败定律:一次失败的 place(尤其是 "connector did not respond")
			// 大概率已经在画布上建好了件,只是回执丢了。以前这里直接放手,那个件
			// 就成了谁也删不掉的 untracked 残件,并随重试繁殖。现在先做一次收编。
			placement, uncertain, adopted := bapAdoptAfterPlaceFailure(
				cfg, window, preplaceIDs, created, p, stderr)
			if placement != nil {
				created = append(created, *placement)
			}
			return failBlockApplyAfterPlacement(cfg, window, &man, created, uncertain, adopted,
				fmt.Errorf("place %s (%s): %w", p.Designator, p.PartKey, err),
				asJSON, stdout, stderr)
		}
		p.PrimitiveID = bapPlacedPrimitiveID(res)
		plan.Placements[i].PrimitiveID = p.PrimitiveID
		actual := bapPlacedDesignator(res)
		createdPlacement := p
		if actual != "" && !strings.EqualFold(actual, p.Designator) {
			renames[strings.ToUpper(p.Designator)] = actual
			createdPlacement.Designator = actual
			fmt.Fprintf(stderr, "placed %-6s %-18s @ %.0f,%.0f [%s] → platform renumbered to %s\n",
				p.Designator, p.PartKey, p.X, p.Y, p.Source, actual)
		} else {
			fmt.Fprintf(stderr, "placed %-6s %-18s @ %.0f,%.0f [%s]\n", p.Designator, p.PartKey, p.X, p.Y, p.Source)
		}
		if p.PrimitiveID == "" {
			// 「成功但没回 id」与超时同族:件在画布上,句柄丢了。同一套收编 ——
			// 认回来就当作已放置件(回滚照常删),认不回来才按无句柄报 PARTIAL。
			placement, _, adopted := bapAdoptAfterPlaceFailure(cfg, window, preplaceIDs, created, p, stderr)
			if placement != nil {
				createdPlacement.PrimitiveID = placement.PrimitiveID
				plan.Placements[i].PrimitiveID = placement.PrimitiveID
			}
			created = append(created, createdPlacement)
			return failBlockApplyAfterPlacement(cfg, window, &man, created, nil, adopted,
				fmt.Errorf("place %s (%s) succeeded but returned no primitiveId; refusing to wire an untracked component",
					createdPlacement.Designator, p.PartKey),
				asJSON, stdout, stderr)
		}
		created = append(created, createdPlacement)
		if preplaceIDs != nil {
			// 收编的门①要求 known = 快照 ∪ 已成功放置;逐件补进去,否则前面几件
			// 会被后面的收编当成"新出现的孤儿"。
			preplaceIDs[p.PrimitiveID] = true
		}
	}
	if len(renames) > 0 {
		bapRemapDesignators(&plan, renames)
		man.Instance, man.Placed, man.Nets, man.Renames = plan.Instance, plan.Placements, plan.Nets, renames
		pairs := make([]string, 0, len(renames))
		for planned, actual := range renames {
			pairs = append(pairs, planned+"→"+actual)
		}
		sort.Strings(pairs)
		w := fmt.Sprintf("EasyEDA renumbered %d designator(s) on create (%s) — it dodges designators "+
			"on pages our pre-flight scan cannot see (getAll only returns LOADED pages). The plan, its "+
			"net members and instance-scoped net names were remapped onto the real designators.",
			len(renames), strings.Join(pairs, ", "))
		man.Warnings = append(man.Warnings, w)
		fmt.Fprintf(stderr, "warn: %s\n", w)
	}
	// All creates are now represented by their authoritative IDs/designators.
	// Use this remapped slice for any subsequent cleanup or failure manifest.
	created = append(created[:0], plan.Placements...)
	man.Placed = append([]bapPlacement(nil), plan.Placements...)

	// 5b-0. 实测推让(ADR-0003 时间窗)排在硬门**之前**:器件全部落地、marker 一根都
	// 还没建,此刻挪件只是一次 component.modify;硬门随后验的就是推让之后的最终几何,
	// 不必再验第二遍。顺序还有一个硬理由:硬门那次回读带 includePins,而带引脚的回读
	// 会顺带跑 netlist 导出,导出之后的 modify 会被平台拒掉(bslMoveComponentX 有实测)。
	moves, pushNotes := bslExpandLive(cfg, window, &plan, anchorGeom, stderr)
	man.Warnings = append(man.Warnings, pushNotes...)
	if len(moves) > 0 {
		man.Placed = append([]bapPlacement(nil), plan.Placements...)
		created = append(created[:0], plan.Placements...)
	}

	// 5b. real-geometry read-back: the estimated-footprint dodge above is a
	// heuristic; the rendered bboxes and pin coordinates are the truth. Findings
	// involving this instance are a hard pre-wiring gate, not a warning.
	// A pin coincidence reports OvX/OvY 0 (it is a point, not an area) — call it out
	// by name, because it shorts two nets with no wire to show for it and is far more
	// dangerous than the bbox overlap it hides behind.
	findings, offSheet, verifyErr := verifyBlockLayout(cfg, window, plan.Placements)
	if verifyErr != nil {
		fmt.Fprintf(stderr, "layout ✗ verification unavailable: %v\n", verifyErr)
		return failBlockApplyAfterPlacement(cfg, window, &man, created, nil, nil,
			fmt.Errorf("layout verification failed: %w", verifyErr), asJSON, stdout, stderr)
	}
	// 硬门在推让之后报重叠时,先把位移原样还原再验一次:布局本来是干净的,不该因为
	// 一次版面优化把整单回滚掉(位移是我们自己写进去的,还原是精确的)。
	if len(findings) > 0 && len(moves) > 0 {
		fmt.Fprintf(stderr, "layout ✗ 推让后出现 %d 处重叠/引脚重合 —— 还原位移再验一次\n", len(findings))
		bslUndoLiveMoves(cfg, window, &plan, moves, stderr)
		man.Placed = append([]bapPlacement(nil), plan.Placements...)
		created = append(created[:0], plan.Placements...)
		if f2, off2, err := verifyBlockLayout(cfg, window, plan.Placements); err == nil && len(f2) == 0 {
			w := "实测推让会造成重叠,已整体还原到落地时的位置(布局照旧,只是没优化)"
			man.Warnings = append(man.Warnings, w)
			fmt.Fprintf(stderr, "warn: %s\n", w)
			findings, offSheet = f2, off2
		}
	}
	if len(findings) > 0 {
		man.LayoutOverlaps = findings
		man.LayoutMeasurementUnit = "0.01inch"
		man.LayoutCoordinateUnit = "0.01inch"
		overlaps, coincidences := 0, 0
		for _, f := range findings {
			if f.Type == "pin-coincidence" {
				coincidences++
				fmt.Fprintf(stderr, "layout ✗ PIN COINCIDENCE %s ↔ %s — two pins share one point = implicit short; "+
					"adjust the block origin/template before re-running\n", f.A, f.B)
				continue
			}
			overlaps++
			fmt.Fprintf(stderr, "layout ✗ overlap %s ↔ %s (%.0f×%.0f) — fix with `sch modify`/`sch autoplace-free`, then `sch layout-lint`\n",
				f.A, f.B, f.OvX, f.OvY)
		}
		return failBlockApplyAfterPlacement(cfg, window, &man, created, nil, nil,
			fmt.Errorf("layout verification found %d overlap(s) and %d pin coincidence(s)",
				overlaps, coincidences),
			asJSON, stdout, stderr)
	}
	fmt.Fprintf(stderr, "layout ✓ no overlap or pin coincidence involving this instance\n")

	// 出图纸:按**实测 bbox** 判(取代事后拿锚点比 sheet 的老 warning —— 锚点在框内
	// 而 body 探出框外就漏报)。只警告不回滚,与 layout-lint 的分档一致。
	for _, f := range offSheet {
		w := fmt.Sprintf("%s 越出图纸可用区(图框内缩 %.0f 单位),该件照样连线、netlist 也对得上,但印不出来 — 换大图纸/拆页/给块加 schematic_layout 模板;`sch layout-lint --strict` 会把它判为阻塞",
			f.A, sheetEdgeMinGap)
		man.Warnings = append(man.Warnings, w)
		fmt.Fprintf(stderr, "warn: %s\n", w)
	}

	// 6. wire — delegate to autoconnect, which owns the stub geometry + idempotency.
	var conns []acConnSpec
	for _, n := range plan.Nets {
		for _, m := range n.Members {
			conns = append(conns, acConnSpec{PinRef: m, Kind: n.Kind, Net: n.Net})
		}
	}
	// autoconnect's per-connection report goes to STDERR, not io.Discard: when a
	// connection fails, that report IS the diagnosis (which pin, which candidates
	// were rejected and why). Discarding it leaves a bare "1 connection(s) failed"
	// and forces the caller to re-run each pin by hand to find out anything.
	// stdout stays clean for the manifest.
	// **不因一条连接失败就丢掉后续状态固化**(部分应用约定 #151)。平台有一个已知
	// 的「卡死在 99%」故障:每轮 apply 会随机吃掉一条 connect_pin(7s 超时),而器件
	// 此刻已全部落地。过去这里直接 return,于是 check / reconcile / **归组** 全部
	// 跳过 —— 画布上留下 29/30 连好的电路,却没有虚拟组,上层拿不到刚体,而用户看到
	// 的只是一句 "wire: 1 connection(s) failed"。现在照常走完,最后再统一决定退出码。
	var wireErr error
	if err := runAutoconnect(cfg, window, conns, defaultAutoconnectRules(), false, false, false, false, stderr, stderr); err != nil {
		wireErr = err
	}

	// 7. check
	man.OK = "applied"
	if wireErr != nil {
		man.OK = "applied-partial"
	}
	if _, err := requestAction(cfg, "schematic.check", window, map[string]any{}); err != nil {
		fmt.Fprintf(stderr, "warn: schematic.check failed to run: %v\n", err)
	}

	// 8. reconcile the live netlist against the plan (issue #135). Per-stub wiring
	// success is not topology success: EasyEDA merges touching wires, and a merged
	// short has slipped past BOTH check and bridge-check before. The netlist is
	// the authority; a mismatch fails the command instead of hiding behind the
	// green per-stub report.
	liveNets, pinNumbers, rerr := readLiveNets(cfg, window)
	if rerr != nil {
		fmt.Fprintf(stderr, "warn: could not read back the netlist to reconcile (%v) — verify with `easyeda sch read` manually\n", rerr)
	} else {
		diffs := reconcileBlockNets(plan, liveNets, pinNumbers)
		man.Diffs = diffs
		man.Reconciled = len(diffs) == 0
		if len(diffs) > 0 {
			man.OK = "applied-mismatch"
			for _, d := range diffs {
				fmt.Fprintf(stderr, "reconcile ✗ net %s: missing %s", d.Net, strings.Join(d.Missing, ", "))
				for pin, other := range d.FoundIn {
					fmt.Fprintf(stderr, " (%s landed in %q — likely a merged-wire short)", pin, other)
				}
				fmt.Fprintln(stderr)
			}
		} else {
			// 只有真的一条 diff 都没有才报 ✓ —— 这行原本靠上面那个 return 守卫,
			// 改成「部分失败也走完流程」后守卫没了,它一度和 ✗ 同时打印,同一次运行
			// 给出两个相反结论。报告自相矛盾比报错更伤:人会信后打印的那条。
			fmt.Fprintf(stderr, "reconcile ✓ %d net(s) match the live netlist\n", len(plan.Nets))
		}
	}

	// 8b. 虚拟组体检 —— 连完线才有 marker 和桩线,这一刻才量得出「组的体积」。
	//
	// **必须在这里报,不能只靠用户另跑一次。** `layout-lint` 默认排除全部非 part 图元,
	// 于是 marker 互相压、去耦被标签罩住、簇探出图纸的页,它照样报 0 overlap —— 一路
	// 假绿到交付。判定不失败整单(器件与连线都已落地,版面问题是可后修的),但必须
	// 出现在 stderr 和 manifest 里,并指出用哪条命令 gate。
	bapReportClusters(cfg, window, &man, stderr)

	// 9. 归组 —— ADR-0003 的第一步产物必须是**一个刚体**,不是一堆散件。
	// 次序在这里而不是更早:组的 bbox 必须已经包含连线和 marker(放件→连线→
	// 挂 marker→封组),上层(zone tidy / zone-plan)拿到的刚体尺寸才是真实占地,
	// 也才不需要另算「四侧引出通道」。
	bapRegisterGroup(cfg, window, plan, &man, stderr)

	// 10. 统一收尾:状态已经全部固化(连线尽力、对账已做、组已封),**先出 manifest**,
	// 再按严重程度决定退出码。顺序很重要 —— 调用方即使拿到非零退出码,也必须能从
	// manifest 读到画布的真实状态,否则「部分应用」就退化成了「不知道发生了什么」。
	if err := emitBapManifest(man, asJSON, stdout); err != nil {
		return err
	}
	if len(man.Diffs) > 0 {
		return fmt.Errorf("block-apply: %d net(s) do not match the plan — run `easyeda sch bridge-check` and fix before trusting this instance", len(man.Diffs))
	}
	if wireErr != nil {
		return fmt.Errorf("wire: %w (器件与其余连线均已落地,虚拟组已登记 —— 只需重试上面列出的那几个引脚)", wireErr)
	}
	return nil
}

// readLiveNets pulls the post-wiring truth via schematic.read: live net → set of
// "DESIGNATOR.NUMBER" members, plus each component's pin name/number → number
// map (plan members reference pins by NAME; the netlist speaks numbers).
func readLiveNets(cfg *appConfig, window string) (map[string]map[string]bool, map[string]map[string][]string, error) {
	res, err := requestAction(cfg, "schematic.read", window, map[string]any{"includeCheck": false})
	if err != nil {
		return nil, nil, err
	}
	liveNets := map[string]map[string]bool{}
	if nets, ok := res.Result["nets"].([]any); ok {
		for _, n := range nets {
			m, ok := n.(map[string]any)
			if !ok {
				continue
			}
			name := asString(m["net"])
			if name == "" {
				continue
			}
			set := map[string]bool{}
			if pins, ok := m["pins"].([]any); ok {
				for _, p := range pins {
					if s := asString(p); s != "" {
						set[s] = true
					}
				}
			}
			liveNets[name] = set
		}
	}
	pinNumbers := map[string]map[string][]string{}
	if comps, ok := res.Result["components"].([]any); ok {
		for _, c := range comps {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			desig := strings.ToUpper(asString(m["designator"]))
			if desig == "" {
				continue
			}
			byRef := pinNumbers[desig]
			if byRef == nil {
				byRef = map[string][]string{}
				pinNumbers[desig] = byRef
			}
			if pins, ok := m["pins"].([]any); ok {
				for _, p := range pins {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					num := asString(pm["number"])
					if num == "" {
						num = asString(pm["pinNumber"])
					}
					if num == "" {
						continue
					}
					add := func(k string) {
						k = strings.ToUpper(k)
						for _, existing := range byRef[k] {
							if existing == num {
								return
							}
						}
						byRef[k] = append(byRef[k], num)
					}
					add(num)
					if name := asString(pm["name"]); name != "" {
						add(name)
					} else if name := asString(pm["pinName"]); name != "" {
						add(name)
					}
				}
			}
		}
	}
	return liveNets, pinNumbers, nil
}

func emitBapManifest(m bapManifest, asJSON bool, stdout io.Writer) error {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(m)
	}
	fmt.Fprintf(stdout, "%s  %s", m.OK, m.BlockID)
	if m.Revision > 0 {
		fmt.Fprintf(stdout, " rev%d", m.Revision)
	}
	fmt.Fprintf(stdout, "  [%s]  instance=%s\n", m.BlockState, m.Instance)
	if m.Failure != "" {
		fmt.Fprintf(stdout, "failure: %s\n", m.Failure)
	}
	if m.Rollback != nil {
		// adopted = 回执丢了但被收编认回、并进了清扫单的 id;untracked 现在只剩
		// 「连名字都点不出来」的那一类,数字变小才说明真的收敛了。
		fmt.Fprintf(stdout, "rollback: complete=%t verified=%t attempted=%d adopted=%d survived=%d untracked=%d\n",
			m.Rollback.Complete, m.Rollback.Verified, len(m.Rollback.AttemptedPrimitiveIDs),
			len(m.Rollback.AdoptedPrimitiveIDs),
			len(m.Rollback.SurvivedPrimitiveIDs), len(m.Rollback.MissingPrimitiveIDs))
		if len(m.Rollback.AdoptedPrimitiveIDs) > 0 {
			fmt.Fprintf(stdout, "adopted (place response lost, identified by a pre-placement snapshot diff): %s\n",
				strings.Join(m.Rollback.AdoptedPrimitiveIDs, ", "))
		}
		if len(m.Rollback.SurvivedPrimitiveIDs) > 0 {
			fmt.Fprintf(stdout, "still on the page: %s\n", strings.Join(m.Rollback.SurvivedPrimitiveIDs, ", "))
			fmt.Fprintf(stdout, "  easyeda sch prim-delete --ids %s\n",
				strings.Join(m.Rollback.SurvivedPrimitiveIDs, ","))
		}
	}
	if m.PartialState {
		fmt.Fprintln(stdout, "PARTIAL STATE: canvas restoration was not proven; inspect rollback details before retrying")
	}
	if m.Origin != nil && m.Origin.Relocated {
		fmt.Fprintf(stdout, "origin relocated: %.0f,%.0f → %.0f,%.0f\n",
			m.Origin.RequestedX, m.Origin.RequestedY, m.Origin.X, m.Origin.Y)
	}
	fmt.Fprintf(stdout, "\n%-6s %-8s %-20s %-12s %s\n", "REF", "ROLE", "PART", "AT", "LAYOUT")
	for _, p := range m.Placed {
		rot := ""
		if p.Rotation != 0 {
			rot = fmt.Sprintf(" r%g", p.Rotation)
		}
		fmt.Fprintf(stdout, "%-6s %-8s %-20s %-12s %s%s\n", p.Designator, p.Role, p.PartKey,
			fmt.Sprintf("%.0f,%.0f", p.X, p.Y), p.Source, rot)
	}
	for _, w := range m.Warnings {
		fmt.Fprintf(stdout, "warn: %s\n", w)
	}
	for _, f := range m.LayoutOverlaps {
		if f.Type == "pin-coincidence" {
			fmt.Fprintf(stdout, "pin-coincidence: %s:%s ↔ %s:%s at %.0f,%.0f\n",
				f.A, f.APin, f.B, f.BPin, f.X, f.Y)
			continue
		}
		fmt.Fprintf(stdout, "overlap: %s ↔ %s (%.0f×%.0f)\n", f.A, f.B, f.OvX, f.OvY)
	}
	fmt.Fprintf(stdout, "\n%-14s %-9s %s\n", "NET", "KIND", "MEMBERS")
	for _, n := range m.Nets {
		tag := ""
		if n.Port != "" {
			tag = " (port " + n.Port + ")"
			if n.Bound {
				tag = " (port " + n.Port + ", bound)"
			}
		}
		fmt.Fprintf(stdout, "%-14s %-9s %s%s\n", n.Net, n.Kind, strings.Join(n.Members, " "), tag)
	}
	if len(m.Unconsumed) > 0 {
		fmt.Fprintf(stdout, "\nNOT applied (v1 scope): %s\n", strings.Join(m.Unconsumed, ", "))
	}
	return nil
}

// newSchBlockApplyCmd builds the cobra command.
func newSchBlockApplyCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var (
		at, instance, partsPath string
		binds, kinds            []string
		spacing                 float64
		perRow                  int
		dryRun, asJSON          bool
	)
	c := &cobra.Command{
		Use:   "block-apply <block-id>",
		Short: "Instantiate a circuit block: place its parts, wire internal_nets, bind ports",
		Long: `Instantiate a standard circuit block onto the active schematic page.

Loads the block from the embedded library, resolves each role to a real device via
standard-parts.json, places the parts with allocated designators, wires the block's
internal_nets, binds its boundary ports to host nets, and prints a traceable
instance manifest.

PLACEMENT GEOMETRY: a block that declares a schematic_layout template places each
role at its authored offset+rotation from the origin (信号流左入右出、去耦贴芯片
one-time-reviewed geometry); blocks without one fall back to the legacy
--per-row/--spacing grid. Either way the ORIGIN dodges existing parts: when --at
is NOT passed explicitly, the block's estimated footprint spiral-searches the
nearest free region (existing real bboxes as obstacles); an explicit --at is
honoured verbatim (with a warning if it collides). After placing, the real
rendered bboxes AND pin coordinates are re-read. Read/parse/incomplete-geometry
failures, bbox overlaps, and pins from different parts sharing one point are hard
errors BEFORE wiring; a dirty or unverified landing never reaches autoconnect.

FAILURE / CLEANUP: every successful place must return the new primitiveId. When
the pre-wiring layout gate fails, block-apply deletes exactly those newly placed
IDs and re-reads the page to prove they are gone. A proven cleanup is reported as
failed-rolled-back. A missing ID, surviving delete, or unavailable cleanup
read-back is reported as failed-partial + PARTIAL STATE. EasyEDA mutations
autosave independently, so this is explicit compensation, never a fake
transaction claim.

LOST PLACE RESPONSE (ADOPTION): a place that fails with "connector did not
respond" has usually ALREADY created the component — only the response was lost.
Those orphans used to stay on the page forever (nothing knew their ID) and
multiplied on every retry. block-apply now snapshots the page's component IDs
before placing; on a lost/ID-less place response it re-reads (settled) and adopts
the component that is BOTH absent from that snapshot AND at the requested x/y
(±5). Adopted IDs appear as rollback.adoptedPrimitiveIds and are deleted like any
tracked ID. Nothing new at that spot is reported as "the place did not land" —
no ID is ever invented, and a pre-existing part at the same coordinates is in the
snapshot, so it can never be adopted or deleted. Without a usable snapshot
adoption is disabled outright and the run reports an honest PARTIAL STATE.

SCOPE (v1): parts / internal_nets / ports only. A block's pcb_layout, placement,
signals and silk maps are NOT applied — the manifest lists them under
"NOT applied" so a green exit never reads as "the whole block was honoured".

EACH RUN CREATES A NEW INSTANCE — this command is NOT idempotent, by design: two
LEDs means running it twice. Designators are allocated around whatever is already
in the DOCUMENT — all schematic pages, not just the active one, because the
netlist is keyed by designator.pin document-wide and a cross-page collision
poisons every net attribution (issue #136). Each instance's PORT-less internal nets are
named after its own first designator (LED1_N2 vs LED2_N2) so instances never
merge. Re-running after a partial failure therefore does NOT repair that instance,
it builds another one. If status is failed-rolled-back, fix the origin/template
and retry. If status is failed-partial, inspect rollback.survivedPrimitiveIds /
adoptedPrimitiveIds / missingPrimitiveIds, delete or repair the residue explicitly
(the manifest prints a ready-to-run ` + "`sch prim-delete --ids …`" + `), then retry — never
assume the failed run left a clean page. Residue that will not delete is almost
always a wedged connector action queue (writes are swallowed while light reads
still work): ` + "`sch save`" + `, fully restart EasyEDA, then delete.

Wiring itself is delegated to the ` + "`sch autoconnect`" + ` planner, which IS
idempotent per pin — an already-connected pin is skipped rather than re-flagged.`,
		Args: cobra.ExactArgs(1),
		Example: `  easyeda sch block-apply led_indicator_gpio --dry-run
  easyeda sch block-apply led_indicator_gpio --at 400,300 --bind CTRL=IO2 --bind GND=GND
  easyeda sch block-apply block.led_indicator_gpio --instance led2 --at 400,500 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			bind, err := parseKV(binds, "--bind")
			if err != nil {
				return err
			}
			kindRaw, err := parseKV(kinds, "--kind")
			if err != nil {
				return err
			}
			kindOver := map[string]string{}
			for net, k := range kindRaw {
				if _, err := resolveNetflagKind(k); err != nil {
					return err
				}
				kindOver[strings.ToUpper(net)] = k
			}
			x, y, err := parseXY(at)
			if err != nil {
				return err
			}
			in := bapInput{
				Instance: instance, OriginX: x, OriginY: y,
				Spacing: spacing, PerRow: perRow, Bind: bind, KindOver: kindOver,
				AtExplicit: cmd.Flags().Changed("at"),
			}
			return runBlockApply(cfg, *window, args[0], in, partsPath, dryRun, asJSON, stdout, stderr)
		},
	}
	c.Flags().StringVar(&at, "at", "400,300", "origin coordinate x,y for the first part")
	c.Flags().Float64Var(&spacing, "spacing", 0, "fallback-grid spacing between placed parts; "+
		"0 = auto-size from the block's biggest part (an IC needs ~220, a discrete ~120 — a fixed 100 "+
		"put a QFN's power pin exactly on a neighbouring crystal's ground pin)")
	c.Flags().IntVar(&perRow, "per-row", 4, "parts per row before wrapping")
	c.Flags().StringArrayVar(&binds, "bind", nil, "bind a block PORT to a host net: --bind CTRL=IO2 (repeatable)")
	c.Flags().StringArrayVar(&kinds, "kind", nil, "override a net's flag kind: --kind LED_CTRL=netport (repeatable)")
	c.Flags().StringVar(&instance, "instance", "", "instance id used to name internal nets (default: the first allocated designator, e.g. LED1 → LED1_N2)")
	c.Flags().StringVar(&partsPath, "parts", "", "path to standard-parts.json (auto-detected if omitted)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "plan and print without placing or wiring")
	c.Flags().BoolVar(&asJSON, "json", false, "emit the instance manifest as JSON")
	return c
}

// parseXY parses an "x,y" flag.
func parseXY(s string) (float64, float64, error) {
	xs, ys, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, fmt.Errorf("--at %q: expected x,y", s)
	}
	var x, y float64
	if _, err := fmt.Sscanf(strings.TrimSpace(xs), "%g", &x); err != nil {
		return 0, 0, fmt.Errorf("--at %q: bad x", s)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(ys), "%g", &y); err != nil {
		return 0, 0, fmt.Errorf("--at %q: bad y", s)
	}
	return x, y, nil
}

// bapRegisterGroup 把刚落地的这批器件登记成一个持久虚拟组(ADR-0003 第一步)。
//
// 为什么必须有这一步:在此之前 block-apply 的产物是**散件** —— 第二层
// (`sch zone tidy`)接手时手里没有刚体可排,只能把它们当独立零件重新摊开,
// 组内相对位置(flow 共线 / attach 贴脚 / pair 等距)当场作废。链子断在这里。
//
// **fail-soft**:走到这一步器件已经放好、线已经连上、netlist 已经对账通过。
// 登记失败只是「上层少了一个刚体」,绝不能把一次成功的 apply 变成报错回滚
// (部分应用约定 #151:画布已变后绝不抛错)。失败原因写 stderr,并给出手工补登
// 的命令。
func bapRegisterGroup(cfg *appConfig, window string, plan bapPlan, man *bapManifest, stderr io.Writer) {
	members := make([]string, 0, len(plan.Placements))
	for _, p := range plan.Placements {
		// 位号取 plan.Placements 而不是块配方里的 role —— place 会回读并 remap
		// 真实位号(#144),用配方名会登记出一组页面上不存在的位号。
		if d := strings.TrimSpace(p.Designator); d != "" {
			members = append(members, d)
		}
	}
	if len(members) == 0 {
		return
	}
	pinned, win, docUUID, _, st, groups, err := loadSchGroupsContext(cfg, window)
	if err != nil {
		fmt.Fprintf(stderr, "warn: 归组跳过(取不到页面分组表:%v)—— 器件与连线均已落地,上层布局会把它们当散件;可手工补登:easyeda sch group create --name %q --members %s\n",
			err, bapGroupName(plan), strings.Join(members, ","))
		return
	}
	_ = pinned
	_ = win
	roles := map[string]string{}
	for _, p := range plan.Placements {
		if r, d := strings.TrimSpace(p.Role), strings.TrimSpace(p.Designator); r != "" && d != "" {
			roles[r] = strings.ToUpper(d)
		}
	}
	// **按功能子群登记**,不是整块一个组(2026-08-15):块自己就知道哪几件构成一个
	// 功能单元(flow 的每一级 + 跟着它的去耦/并列组),拆出来之后「把 USB 口那一簇
	// 整体挪开」才有抓手 —— 整块一个组时 group-move 只能 7 件一起动,组间根本留不出
	// 通道。拆分规则见 bslFunctionalGroups(全部来自块数据,不靠人手认领)。
	subs := bapSubgroupsOf(plan)
	next := groups
	var created []*schGroup
	for _, sg := range subs {
		var mem []string
		sub := map[string]string{}
		for _, r := range sg.Roles {
			if d, ok := roles[r]; ok {
				mem = append(mem, d)
				sub[r] = d
			}
		}
		if len(mem) == 0 {
			continue
		}
		gname := bapGroupName(plan)
		if len(subs) > 1 {
			gname += "/" + sg.Name
		}
		nx, g, cerr := groupsCreate(next, gname, mem)
		if cerr != nil {
			fmt.Fprintf(stderr, "warn: 子群 %s 归组跳过(%v)\n", sg.Name, cerr)
			continue
		}
		g.BlockID, g.Instance, g.Roles = plan.BlockID, plan.Instance, sub
		next, created = nx, append(created, g)
	}
	if len(created) == 0 {
		fmt.Fprintf(stderr, "warn: 归组跳过(没有可登记的子群)—— 器件与连线均已落地\n")
		return
	}
	if serr := saveSchGroups(st, docUUID, next); serr != nil {
		fmt.Fprintf(stderr, "warn: 归组未能落盘(%v)—— 器件与连线均已落地;可手工补登:easyeda sch group create --name %q --members %s\n",
			serr, bapGroupName(plan), strings.Join(members, ","))
		return
	}
	man.GroupID, man.GroupName = created[0].ID, created[0].Name
	for _, g := range created {
		fmt.Fprintf(stderr, "grouped ✓ %s (%s) — %d 件\n", g.ID, g.Name, len(g.Members))
	}
	if len(created) > 1 {
		fmt.Fprintf(stderr, "grouped: 按功能子群拆成 %d 组 —— 组间留通道用 `sch group-move --group <id>`\n", len(created))
	}
}

// bapGroupName 是这个实例的人类可读组名:块名(去 block. 前缀)+ 实例号,
// 例如 ch340c_usb_serial(C7)。
func bapGroupName(plan bapPlan) string {
	id := strings.TrimPrefix(plan.BlockID, "block.")
	if plan.Instance == "" {
		return id
	}
	return fmt.Sprintf("%s(%s)", id, plan.Instance)
}
