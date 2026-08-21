package app

// cmd_sch_zone_draw.go — `sch zone-draw`: make the claimed functional zones
// VISIBLE on the schematic sheet (行业规范的「先看区、再看线」分区框).
//
// `sch zones set` persists the S0 partitioning and layout-lint verifies it
// mechanically; this command draws it for humans: one dashed rectangle + a
// "module (zone)" label per claim, resolved from the LIVE sheet bbox with the
// same zoneRect() geometry the violation rule uses — what you see IS what the
// gate checks.
//
// Implementation note: the schematic graphics API (eda.sch_PrimitiveRectangle /
// sch_PrimitiveText — full CRUD, probed live 2026-07-19 on ceshi) has no typed
// action yet, so this goes through the debug.exec_js hatch, the documented path
// for scriptable behavior that doesn't warrant a connector re-import. Created
// primitive ids are recorded in the project workflow state so redraw/--clear
// removes exactly what this tool created and never touches user graphics.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// schZoneFrameInset shrinks each zone rectangle so adjacent frames don't sit
// on the exact same boundary line (schematic units).
const schZoneFrameInset = 4

// schZoneMinFrameSpan is the smallest frame worth drawing after the margin and
// title-block adjustments: below this a "frame" is a sliver that reads as a
// stray line, so the zone is skipped instead.
const schZoneMinFrameSpan = 40

// schZoneOpts are the fixed-grid frame tunables. The defaults mirror the
// partition mode's page margin so the two modes look like the same tool.
type schZoneOpts struct {
	Margin   float64 // inset from the sheet border, so frames never sit on it
	Inset    float64 // gap between adjacent frames
	LabelPad float64 // label inset from the frame's own lines
	Gap      float64 // clearance kept from the title-block keep-out
}

func defaultSchZoneOpts() schZoneOpts {
	return schZoneOpts{Margin: 20, Inset: schZoneFrameInset, LabelPad: 6, Gap: 8}
}

// schZoneFrameRect computes the rectangle actually drawn for one zone: the grid
// cell of the margin-inset sheet, shrunk by Inset, then lifted clear of the
// title-block keep-out. The bool is false when nothing usable survives.
//
// Issue #163: the frames used to be laid out on the RAW sheet bbox with only a
// 4-unit inset, so they sat on the sheet border and the bottom row ran straight
// through the title block — `sch check`'s own titleblock-overlap rule fired on
// frames zone-draw had just drawn, even though the keep-out geometry was
// already available from deriveSheetGeometry.
func schZoneFrameRect(zone string, sheet layoutBBox, tb *layoutBBox, o schZoneOpts) (layoutBBox, bool) {
	usable := layoutBBox{
		MinX: sheet.MinX + o.Margin, MaxX: sheet.MaxX - o.Margin,
		MinY: sheet.MinY + o.Margin, MaxY: sheet.MaxY - o.Margin,
	}
	if usable.MaxX-usable.MinX <= 0 || usable.MaxY-usable.MinY <= 0 {
		return layoutBBox{}, false
	}
	cell := zoneRect(zone, usable)
	frame := layoutBBox{
		MinX: cell.MinX + o.Inset, MaxX: cell.MaxX - o.Inset,
		MinY: cell.MinY + o.Inset, MaxY: cell.MaxY - o.Inset,
	}
	frame, ok := liftClearOfTitleBlock(frame, tb, o.Gap)
	if !ok {
		return layoutBBox{}, false
	}
	if frame.MaxX-frame.MinX < schZoneMinFrameSpan || frame.MaxY-frame.MinY < schZoneMinFrameSpan {
		return layoutBBox{}, false
	}
	return frame, true
}

// liftClearOfTitleBlock pulls a frame off the title-block keep-out. The title
// block sits at the sheet's bottom-right, so the natural move is to raise the
// frame's bottom edge above it; lowering the top edge is the fallback for the
// (unusual) keep-out that sits above the frame's middle.
func liftClearOfTitleBlock(frame layoutBBox, tb *layoutBBox, gap float64) (layoutBBox, bool) {
	if tb == nil || !boxesOverlap(frame, *tb) {
		return frame, true
	}
	// Keep-out covers the frame's lower part → raise the floor.
	if lifted := tb.MaxY + gap; lifted < frame.MaxY {
		frame.MinY = lifted
		return frame, true
	}
	// Keep-out covers the frame's upper part → drop the ceiling.
	if dropped := tb.MinY - gap; dropped > frame.MinY {
		frame.MaxY = dropped
		return frame, true
	}
	return layoutBBox{}, false // fully swallowed by the keep-out
}

// writeZoneRectangleCreateJS emits the one rectangle-create call shared by the
// fixed-grid and data-driven partition draw paths. EasyEDA anchors schematic
// rectangles at the visual TOP-LEFT corner: (MinX, MaxY) on the y-UP canvas,
// then extends toward -y by height. Keeping that conversion here prevents one
// mode from accidentally passing MinY and dropping the rendered frame one full
// height below its planned bbox.
func writeZoneRectangleCreateJS(b *strings.Builder, r layoutBBox, colorJS []byte) bool {
	w := r.MaxX - r.MinX
	h := r.MaxY - r.MinY
	if w <= 0 || h <= 0 {
		return false
	}
	fmt.Fprintf(b, "{ const rc = await eda.sch_PrimitiveRectangle.create(%g, %g, %g, %g, 0, 0, %s, null, 1, 1);\n",
		r.MinX, r.MaxY, w, h, colorJS)
	return true
}

// writeZoneDrawPrelude/Epilogue make a graphics draw self-cleaning. A thrown SDK
// error after rectangle N but before text N used to strand the already-created
// primitives because exec_js returned no ids. The catch path now deletes every id
// accumulated so far and reports any survivor.
func writeZoneDrawPrelude(b *strings.Builder) {
	b.WriteString(`const rects=[], texts=[];
const genericPrim = eda.sch_PrimitiveObject && typeof eda.sch_PrimitiveObject.delete === 'function'
  ? eda.sch_PrimitiveObject : null;
const cleanupCreated = async () => {
  const cleanupErrors = [];
  // Generic class first: the per-class text delete does not persist (issue #164),
  // so a rollback through it leaves the labels behind on the next reload.
  // 逐个删 + 回读 + 幸存者重试一次(缺陷 3:批量 delete 静默 no-op 仍返 true)。
  const delOne = async (id, cls) => {
    try { await (genericPrim ?? cls).delete([id]); }
    catch (err) { cleanupErrors.push(id + ': ' + String(err)); }
  };
  for (const id of rects) await delOne(id, eda.sch_PrimitiveRectangle);
  for (const id of texts) await delOne(id, eda.sch_PrimitiveText);
  try {
    let aliveRects = new Set(await eda.sch_PrimitiveRectangle.getAllPrimitiveId());
    let aliveTexts = new Set(await eda.sch_PrimitiveText.getAllPrimitiveId());
    const needRetry = rects.some(id => aliveRects.has(id)) || texts.some(id => aliveTexts.has(id));
    if (needRetry) {
      for (const id of rects) if (aliveRects.has(id)) await delOne(id, eda.sch_PrimitiveRectangle);
      for (const id of texts) if (aliveTexts.has(id)) await delOne(id, eda.sch_PrimitiveText);
      aliveRects = new Set(await eda.sch_PrimitiveRectangle.getAllPrimitiveId());
      aliveTexts = new Set(await eda.sch_PrimitiveText.getAllPrimitiveId());
    }
    return {cleanupSurvived:[...rects.filter(id => aliveRects.has(id)), ...texts.filter(id => aliveTexts.has(id))], cleanupErrors};
  } catch (err) {
    cleanupErrors.push(String(err));
    return {cleanupSurvived:[...rects, ...texts], cleanupErrors};
  }
};
try {
`)
}

func writeZoneDrawEpilogue(b *strings.Builder) {
	b.WriteString(`  return {ok:true, rects, texts};
} catch (err) {
  const cleanup = await cleanupCreated();
  return {ok:false, error: err instanceof Error ? err.message : String(err), rects, texts, ...cleanup};
}`)
}

// buildZoneClearJS deletes only recorded ids and verifies them against the live
// page. The platform's delete boolean is not authoritative (large/no-op deletes
// may still return true), so a survivor makes the operation fail closed.
//
// Deletion goes through the GENERIC `sch_PrimitiveObject` class (issue #164):
// `sch_PrimitiveText.delete()` only drops the text from the in-memory index, so
// the labels came back on the next `doc reload` — and because the survivor check
// below re-reads IMMEDIATELY, it saw them gone and reported a clean sweep. Live
// run before the fix: "cleared 6 previous zone-frame primitive(s)" while all 3
// labels were still on the page. `sch_PrimitiveObject.delete()` persists.
//
// 缺陷 3(P1):**批量** delete 不可靠 —— 真机实锤本命令批量删旧框报 survived=4,
// 而逐个删成功率 100%(平台大批量静默 no-op 仍返 true 的已知病)。删除因此改成
// **逐个删 + 回读证实 + 幸存者重试一次**(与 Go 侧 deleteVerifiedOneByOne 同一套
// 语义,内联在单次 exec_js 里跑,免去逐 id 的连接器往返)。判定只信回读。
func buildZoneClearJS(f *workflow.SchZoneFrames) string {
	rects, _ := json.Marshal(f.Rects)
	texts, _ := json.Marshal(f.Texts)
	return fmt.Sprintf(`const rects=%s, texts=%s;
const generic = eda.sch_PrimitiveObject && typeof eda.sch_PrimitiveObject.delete === 'function'
  ? eda.sch_PrimitiveObject : null;
const aliveSets = async () => ({
  rect: new Set(await eda.sch_PrimitiveRectangle.getAllPrimitiveId()),
  text: new Set(await eda.sch_PrimitiveText.getAllPrimitiveId()),
});
let alive = await aliveSets();
const foundRects = rects.filter(id => alive.rect.has(id));
const foundTexts = texts.filter(id => alive.text.has(id));
const found = foundRects.length + foundTexts.length;
const deleteErrors = [];
const delOne = async (id, cls) => {
  try { await (generic ?? cls).delete([id]); }
  catch (err) { deleteErrors.push(id + ': ' + (err instanceof Error ? err.message : String(err))); }
};
for (const id of foundRects) await delOne(id, eda.sch_PrimitiveRectangle);
for (const id of foundTexts) await delOne(id, eda.sch_PrimitiveText);
alive = await aliveSets();
const retried = [];
for (const id of foundRects) if (alive.rect.has(id)) { retried.push(id); await delOne(id, eda.sch_PrimitiveRectangle); }
for (const id of foundTexts) if (alive.text.has(id)) { retried.push(id); await delOne(id, eda.sch_PrimitiveText); }
if (retried.length) alive = await aliveSets();
const survived = [...rects.filter(id => alive.rect.has(id)), ...texts.filter(id => alive.text.has(id))];
return {ok:survived.length===0, requested:rects.length+texts.length, found, deleted:found-survived.length, survived, retried, deleteErrors, viaGeneric: !!generic};`, rects, texts)
}

func asStringSlice(v any) []string {
	if raw, ok := v.([]string); ok {
		return append([]string(nil), raw...)
	}
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		if s := asString(it); s != "" {
			out = append(out, s)
		}
	}
	return out
}

const (
	defaultFixedZoneFontSize     = 14
	defaultPartitionZoneFontSize = 22
)

// pinZonePage resolves and pins one active schematic document before any zone
// read/write. It also honors the global --doc selector.
func pinZonePage(cfg *appConfig, window string) (*appConfig, string, string, error) {
	win, err := resolveTargetWindow(cfg, window)
	if err != nil {
		return nil, "", "", fmt.Errorf("schematic page: resolve target window: %w", err)
	}
	local := *cfg
	if err := ensureActiveDoc(&local, win); err != nil {
		return nil, "", "", fmt.Errorf("schematic page: activate target page: %w", err)
	}
	docUUID, err := currentSchematicDocumentUUID(&local, win)
	if err != nil {
		return nil, "", "", fmt.Errorf("schematic page: %w", err)
	}
	local.doc = docUUID
	return &local, win, docUUID, nil
}

func zoneFramesEmpty(f *workflow.SchZoneFrames) bool {
	return f == nil || (len(f.Rects) == 0 && len(f.Texts) == 0)
}

// recordedZoneFrames returns this page's record. A legacy unscoped record is a
// candidate only until its ids are inspected on the current page; if none are
// present we leave it untouched so visiting the original page can still recover
// and clear it.
func recordedZoneFrames(st *pcbStageState, docUUID string) (*workflow.SchZoneFrames, string) {
	if st == nil {
		return nil, ""
	}
	if f := st.SchZoneFrameIdsByPage[docUUID]; !zoneFramesEmpty(f) {
		return f, "page"
	}
	if f := st.SchZoneFrameIds; !zoneFramesEmpty(f) &&
		(f.DocumentUUID == "" || f.DocumentUUID == docUUID) {
		return f, "legacy"
	}
	return nil, ""
}

func removeRecordedZoneFrames(st *pcbStageState, docUUID, source string) {
	switch source {
	case "page":
		delete(st.SchZoneFrameIdsByPage, docUUID)
	case "legacy":
		st.SchZoneFrameIds = nil
	}
}

func setRecordedZoneFrames(st *pcbStageState, docUUID, mode string, f *workflow.SchZoneFrames) {
	if st.SchZoneFrameIdsByPage == nil {
		st.SchZoneFrameIdsByPage = map[string]*workflow.SchZoneFrames{}
	}
	f.DocumentUUID = docUUID
	f.Mode = mode
	st.SchZoneFrameIdsByPage[docUUID] = f
}

func verifyZoneClearResult(v map[string]any) (int, error) {
	survived := asStringSlice(v["survived"])
	if !asBool(v["ok"]) || len(survived) > 0 {
		return int(asFloat(v["found"])), fmt.Errorf("zone frame delete was not verified; survived=%v", survived)
	}
	return int(asFloat(v["found"])), nil
}

func validateZoneDrawResult(v map[string]any, expected int) (*workflow.SchZoneFrames, error) {
	f := &workflow.SchZoneFrames{
		Rects: asStringSlice(v["rects"]),
		Texts: asStringSlice(v["texts"]),
		At:    nowRFC3339(),
	}
	if !asBool(v["ok"]) {
		return f, fmt.Errorf("zone draw failed: %s (cleanup survivors: %v, cleanup errors: %v)",
			asString(v["error"]), asStringSlice(v["cleanupSurvived"]), v["cleanupErrors"])
	}
	seen := map[string]bool{}
	for _, id := range append(append([]string(nil), f.Rects...), f.Texts...) {
		if id == "" || seen[id] {
			return f, fmt.Errorf("zone draw returned an empty or duplicate primitive id %q", id)
		}
		seen[id] = true
	}
	if len(f.Rects) != expected || len(f.Texts) != expected {
		return f, fmt.Errorf("zone draw returned %d rectangle id(s) and %d text id(s), want %d each",
			len(f.Rects), len(f.Texts), expected)
	}
	return f, nil
}

type zoneJSExecutor func(phase, code string) (map[string]any, error)

func clearZoneFrames(exec zoneJSExecutor, f *workflow.SchZoneFrames, phase string) (int, error) {
	v, err := exec(phase, buildZoneClearJS(f))
	if err != nil {
		return 0, err
	}
	return verifyZoneClearResult(v)
}

// cleanupNewZoneFrames is the compensation path for count/state failures after a
// draw. It never changes workflow state; callers preserve the old record so a
// retry remains recoverable.
func cleanupNewZoneFrames(exec zoneJSExecutor, f *workflow.SchZoneFrames) error {
	if zoneFramesEmpty(f) {
		return nil
	}
	_, err := clearZoneFrames(exec, f, "clean up incomplete zone draw")
	return err
}

// clearPriorZoneFrames removes this page's prior record in memory only after a
// verified delete. Callers persist state at the same checkpoint as the new draw
// (or after schematic.save for --clear).
func clearPriorZoneFrames(st *pcbStageState, docUUID string, exec zoneJSExecutor, stderr io.Writer) (bool, error) {
	prev, source := recordedZoneFrames(st, docUUID)
	if prev == nil {
		return false, nil
	}
	found, err := clearZoneFrames(exec, prev, "clear previous zone frames")
	if err != nil {
		return false, fmt.Errorf("clear previous zone frames: %w", err)
	}
	// An old unscoped record may belong to another page. No matching live id means
	// this page must not consume it; retain it for recovery on its actual page.
	if source == "legacy" && prev.DocumentUUID == "" && found == 0 {
		fmt.Fprintln(stderr, "note: legacy unscoped zone-frame ids are not present on this page; record retained for its original page")
		return false, nil
	}
	removeRecordedZoneFrames(st, docUUID, source)
	fmt.Fprintf(stderr, "cleared %d previous zone-frame primitive(s) on page %s\n", found, docUUID)
	return true, nil
}

func saveZoneDocument(cfg *appConfig, window, docUUID, phase string) error {
	return saveAutolayoutDocument(cfg, window, docUUID, phase)
}

// compensateZoneDraw removes a just-created, not-yet-committed frame set and
// explicitly saves the cleanup. If deletion or save cannot be proven, the full
// candidate id set is persisted as this page's recovery record: future --clear
// re-reads live ids, so retaining already-deleted ids is safe while losing a
// survivor id would create an unrecoverable orphan.
func compensateZoneDraw(
	cfg *appConfig,
	window, docUUID string,
	st *pcbStageState,
	mode string,
	exec zoneJSExecutor,
	f *workflow.SchZoneFrames,
	cause error,
) error {
	if cerr := cleanupNewZoneFrames(exec, f); cerr != nil {
		setRecordedZoneFrames(st, docUUID, mode, f)
		if perr := savePcbStageState(st); perr != nil {
			return fmt.Errorf("%v; cleanup also failed: %v; recovery ids=%v/%v; persist recovery record failed: %w",
				cause, cerr, f.Rects, f.Texts, perr)
		}
		return fmt.Errorf("%v; cleanup also failed: %w; recovery ids retained for page %s",
			cause, cerr, docUUID)
	}
	if serr := saveZoneDocument(cfg, window, docUUID, "save zone-draw cleanup"); serr != nil {
		setRecordedZoneFrames(st, docUUID, mode, f)
		if perr := savePcbStageState(st); perr != nil {
			return fmt.Errorf("%v; cleanup succeeded in memory but save failed: %v; recovery ids=%v/%v; persist recovery record failed: %w",
				cause, serr, f.Rects, f.Texts, perr)
		}
		return fmt.Errorf("%v; cleanup succeeded in memory but save failed: %w; recovery ids retained for page %s",
			cause, serr, docUUID)
	}
	// The old frame record was already removed in memory before drawing. Persist
	// that checkpoint as well, otherwise a validation failure leaves stale ids on
	// disk even though both old and new graphics were verified absent.
	removeRecordedZoneFrames(st, docUUID, "page")
	if perr := savePcbStageState(st); perr != nil {
		return fmt.Errorf("%v; cleanup was verified and saved, but clearing stale recovery state failed: %w",
			cause, perr)
	}
	return cause
}

// newSchZoneDrawCmd builds `sch zone-draw`.
func newSchZoneDrawCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var color string
	var clear bool
	var mode string
	var fontSize, margin, gutter, titleBand float64
	c := &cobra.Command{
		Use:   "zone-draw",
		Short: "Draw the claimed functional zones as dashed frames + labels on the sheet (--clear removes them)",
		Long: `Visualize the ` + "`sch zones set`" + ` claims: one dashed rectangle + "module (zone)"
label per claim, resolved from the LIVE sheet bbox with the same geometry the
layout-lint zone-violation rule uses — what you see is exactly what the gate
checks (行业规范「先看区、再看线」的分区框标注).

Frames are annotation graphics, not electrical objects. Their primitive ids are
recorded by document UUID in the project workflow state; re-running redraws
(old frames verified removed first) and --clear deletes them without touching
another page or user graphics. Draw/clear explicitly saves the schematic.
Use the global --doc <page> selector for multi-page projects.`,
		Example: `  easyeda sch zones set --spec s0.json --project ceshi
  easyeda sch zone-draw --doc P1_MCU --font-size 14 --project ceshi
  easyeda sch zone-draw --doc P1_MCU --clear --project ceshi`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Partition mode (issue #149): whole-sheet data-driven functional partitions
			// via the planner + per-page frame persistence, instead of the fixed
			// zones-grid rectangles below.
			// 固定九宫格模式已废弃(用户拍板 2026-08-15):框的几何一律由**活体模块
			// bbox** 反推。九宫格的框与电路的实际位置无关 —— 单模块页铺满整纸时,
			// 框套不住电路、框里大半是空的,而且它还让 zone-violation 对着一张画得好
			// 好的图报违规。分区是**版面**,版面就该跟着电路走。
			if mode != "" && mode != "partition" {
				return fmt.Errorf("--mode %q 已废弃:分区框一律数据驱动(从活体模块 bbox 反推);去掉 --mode 即可", mode)
			}
			if fontSize <= 0 {
				fontSize = defaultPartitionZoneFontSize
			}
			// 韧性路径(REPORT esp32mini-round2 新 3):逐区单次 exec_js +
			// 复核后重试 + partial 语义。见 cmd_sch_zone_draw_resilient.go。
			return runPartitionDrawResilient(cfg, *window,
				partitionOptsFrom(margin, gutter, titleBand), fontSize, color, clear, stdout, stderr)
		},
	}
	c.Flags().StringVar(&color, "color", "#AA00AA", "frame + label color")
	c.Flags().BoolVar(&clear, "clear", false, "remove the frames drawn by the last zone-draw on this page")
	c.Flags().StringVar(&mode, "mode", "", "已废弃:分区框一律数据驱动;传 zones 会报错")
	c.Flags().Float64Var(&fontSize, "font-size", 0, "分区标题字号(默认 22)")
	// Defaults come from defaultPartitionOpts so the flag layer can never drift
	// from the planner's own defaults (margin 20 lingered here after the planner
	// moved to 28 — live 2026-08-11).
	def := defaultPartitionOpts()
	c.Flags().Float64Var(&margin, "margin", def.Margin, "--mode partition: page margin inset from the sheet edge")
	c.Flags().Float64Var(&gutter, "gutter", def.Gutter, "为说明扩边时与邻框/障碍保持的间距(分区怎么分与它无关——一区一框)")
	c.Flags().Float64Var(&titleBand, "title-band", def.TitleBand, "--mode partition: height of each partition's title band")
	return c
}
