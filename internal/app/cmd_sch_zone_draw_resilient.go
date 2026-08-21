package app

// cmd_sch_zone_draw_resilient.go — `sch zone-draw` 的韧性创建路径(REPORT
// esp32mini-round2 新 3:连接器长负载下写操作高概率失败,旧路径「先清旧框、
// 再一次 exec_js 画全部区」在 P3 连试 6 次结构性全败,且清旧成功 + 画新失败
// 会让页面从「有框」变成「无框」)。
//
// 三条修法,全部 Go 侧(连接器零改动):
//
//  1. **单区单次 exec_js**:一个区的框线 + 区名标题合并进一段 JS(rect→text→
//     失败自清理,复用删除路径 buildZoneClearJS 早已验证的「逐个删+回读」内联
//     先例)——要么全成要么全败,消除「框成了名没成」的中间态。
//  2. **settle 重试 + 写前/写后轻读复核(假失败定律)**:报失败的写大概率已
//     落地。transport 层失败后先 survey(轻读页面 rect id + text 内容/锚点),
//     复核出「其实画上了」就直接收编 id、绝不重发;复核出「确实没画」才 settle
//     后重发一次;**复核不出来(读也失败/歧义)就不重发**,宁可保守不造重复。
//  3. **逐区推进 + partial 语义(#151)**:多个区之间单区失败不回滚已成功的
//     区;画布已变绝不抛错,报 notApplied 让下次重跑只补缺的区。幂等:重跑前
//     先 survey 画布,已有且与当前 plan 完全吻合的框(标题内容+锚点+配对 rect
//     存活)直接保留 —— 一页已画好的框重跑是零写操作,再也没有「清旧失败窗口」。
//
// runPartitionDraw(cmd_sch_zone_plan.go)保持原样,仍服务 autolayout /
// sheet-tidy / zone-move 的内部重画;`sch zone-draw` 命令入口走这里。

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// zoneAnchorEps 是「画布上已有的框是否就是当前 plan 要画的框」的坐标判定容差。
// 创建时传的是精确浮点,回读应当逐位一致;0.75 只是吸收平台可能的最小网格取整。
const zoneAnchorEps = 0.75

// zonePartitionTarget 是一个待画区的完整落地参数:标题内容、标题锚点(与
// buildPartitionDrawJS 同一几何:MinX+4 / MaxY-fontSize)、框 bbox。
// 判定(画布上有没有)与生成(画什么)共用这一份 —— 同一把尺。
type zonePartitionTarget struct {
	Title    string
	TX, TY   float64 // planned title anchor (top-left, y-UP)
	Rect     layoutBBox
	FontSize float64
}

// partitionTargets derives the per-zone draw targets from a validated plan.
// The anchor math MUST stay identical to buildPartitionDrawJS so the resilient
// path and the legacy batch path draw pixel-identical frames.
func partitionTargets(plan partitionPlan, fontSize float64) []zonePartitionTarget {
	out := make([]zonePartitionTarget, 0, len(plan.Partitions))
	for _, p := range plan.Partitions {
		out = append(out, zonePartitionTarget{
			Title:    strings.Join(p.Modules, " / "),
			TX:       p.TitleBBox.MinX + 4,
			TY:       p.TitleBBox.MaxY - fontSize,
			Rect:     p.BBox,
			FontSize: fontSize,
		})
	}
	return out
}

// buildPartitionZoneDrawJS renders ONE zone's frame + title as a single
// exec_js: rect create → text create → ids returned, with the shared
// self-cleaning prelude/epilogue (a thrown SDK error deletes everything this
// call created and reports survivors). Returns "" for a degenerate bbox.
//
// 每区一段 JS 而不是整页一段:整页一段时第 N 区的 text 失败会回滚前 N-1 个区
// 的全部图元(实测 P3 连败 6 次的形态);拆开后失败被限制在单区内。
func buildPartitionZoneDrawJS(t zonePartitionTarget, colorJS []byte) string {
	var b strings.Builder
	writeZoneDrawPrelude(&b)
	if !writeZoneRectangleCreateJS(&b, t.Rect, colorJS) {
		return ""
	}
	title := jsonString(t.Title)
	fmt.Fprintf(&b, "  if (!rc) throw new Error(%s);\n", jsonString("rectangle create returned undefined for "+t.Title))
	fmt.Fprintf(&b, "  const rid = rc.getState_PrimitiveId(); if (!rid) { await eda.sch_PrimitiveRectangle.delete(rc); throw new Error(%s); } rects.push(rid);\n",
		jsonString("rectangle id missing for "+t.Title))
	fmt.Fprintf(&b, "  const tt = await eda.sch_PrimitiveText.create(%g, %g, %s, 0, %s, null, %g);\n",
		t.TX, t.TY, title, colorJS, t.FontSize)
	fmt.Fprintf(&b, "  if (!tt) throw new Error(%s);\n", jsonString("text create returned undefined for "+t.Title))
	fmt.Fprintf(&b, "  const tid = tt.getState_PrimitiveId(); if (!tid) { await eda.sch_PrimitiveText.delete(tt); throw new Error(%s); } texts.push(tid); }\n",
		jsonString("text id missing for "+t.Title))
	writeZoneDrawEpilogue(&b)
	return b.String()
}

// buildZoneSurveyJS is the LIGHT READ used everywhere in this path: all live
// rectangle ids + all texts (id/content/anchor). It doubles as (a) the
// idempotency probe before drawing, (b) the landed-or-not verification after a
// reported failure, and (c) the settle read that empirically raises the next
// write's success probability on a degraded connector.
func buildZoneSurveyJS() string {
	return `const rectIds = await eda.sch_PrimitiveRectangle.getAllPrimitiveId();
let allTexts = [];
try { allTexts = await eda.sch_PrimitiveText.getAll(); } catch (err) { allTexts = []; }
const textInfo = (Array.isArray(allTexts) ? allTexts : []).map(t => ({
  id: t.getState_PrimitiveId(),
  content: t.getState_Content(),
  x: t.getState_X(),
  y: t.getState_Y(),
}));
return { ok: true, rects: Array.isArray(rectIds) ? rectIds : [], texts: textInfo };`
}

// zoneFrameSurvey is the parsed light read of the page's graphics.
type zoneFrameSurvey struct {
	Rects map[string]bool
	Texts []zoneSurveyText
}

type zoneSurveyText struct {
	ID      string
	Content string
	X, Y    float64
}

func (s zoneFrameSurvey) hasText(id string) bool {
	for _, t := range s.Texts {
		if t.ID == id {
			return true
		}
	}
	return false
}

func parseZoneFrameSurvey(v map[string]any) zoneFrameSurvey {
	out := zoneFrameSurvey{Rects: map[string]bool{}}
	for _, id := range asStringSlice(v["rects"]) {
		out.Rects[id] = true
	}
	raw, _ := v["texts"].([]any)
	for _, it := range raw {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		out.Texts = append(out.Texts, zoneSurveyText{
			ID:      asString(m["id"]),
			Content: asString(m["content"]),
			X:       asFloat(m["x"]),
			Y:       asFloat(m["y"]),
		})
	}
	return out
}

// findSurveyTitleText returns the id of a text on the page whose content AND
// anchor exactly match one partition's planned title (within zoneAnchorEps).
func findSurveyTitleText(s zoneFrameSurvey, t zonePartitionTarget) (string, bool) {
	for _, tx := range s.Texts {
		if tx.Content == t.Title &&
			absF(tx.X-t.TX) <= zoneAnchorEps && absF(tx.Y-t.TY) <= zoneAnchorEps {
			return tx.ID, true
		}
	}
	return "", false
}

// matchExistingZoneFrame decides whether one partition is ALREADY correctly
// drawn on the page: its planned title text exists (content+anchor exact),
// that text id is in the prior per-page record, and the record's PAIRED rect
// (rects[i] ↔ texts[i], the push order of every draw path) is still alive.
// Anything weaker is treated as "not drawn" — the stale ids get cleared and
// the zone redrawn, which errs toward one extra write, never a duplicate.
func matchExistingZoneFrame(t zonePartitionTarget, prev *workflow.SchZoneFrames, s zoneFrameSurvey) (rectID, textID string, ok bool) {
	if prev == nil {
		return "", "", false
	}
	id, found := findSurveyTitleText(s, t)
	if !found {
		return "", "", false
	}
	for i, tid := range prev.Texts {
		if tid != id {
			continue
		}
		if i >= len(prev.Rects) {
			return "", "", false
		}
		rid := prev.Rects[i]
		if rid == "" || !s.Rects[rid] {
			return "", "", false
		}
		return rid, id, true
	}
	return "", "", false
}

// zoneDrawDeps injects the side-effecting pieces so the retry logic is
// offline-testable: exec runs one JS, survey is the light read, sleep is the
// settle delay.
type zoneDrawDeps struct {
	exec   zoneJSExecutor
	survey func() (zoneFrameSurvey, error)
	sleep  func()
}

// zoneKnownIDs are the primitive ids already accounted for on the page
// (pre-existing graphics + frames drawn earlier in this run). The landed-check
// after a transport failure diffs the live page against this set.
type zoneKnownIDs struct {
	Rects map[string]bool
	Texts map[string]bool
}

func (k *zoneKnownIDs) add(rectID, textID string) {
	if rectID != "" {
		k.Rects[rectID] = true
	}
	if textID != "" {
		k.Texts[textID] = true
	}
}

func knownIDsFromSurvey(s zoneFrameSurvey) zoneKnownIDs {
	k := zoneKnownIDs{Rects: map[string]bool{}, Texts: map[string]bool{}}
	for id := range s.Rects {
		k.Rects[id] = true
	}
	for _, t := range s.Texts {
		k.Texts[t.ID] = true
	}
	return k
}

// zoneDrawOutcome is one zone's terminal state after at most two attempts.
type zoneDrawOutcome struct {
	RectID, TextID string
	// Adopted: the write REPORTED failure but the light read proved it landed
	// (假失败定律) — the ids were adopted from the canvas, nothing was resent.
	Adopted bool
	// Retried: success came from the second (settle-verified) attempt.
	Retried bool
	Err     error
	// Stranded ids are primitives proven (or created-then-unverifiable) on the
	// canvas when Err != nil. The caller records them in the per-page frame
	// record so a future --clear / redraw can remove them — losing a survivor
	// id would create an unrecoverable orphan.
	StrandedRects []string
	StrandedTexts []string
}

// drawOneZoneResilient runs one zone's single-exec draw with the
// verify-before-retry protocol:
//
//	exec ok:true            → done.
//	exec ok:false, clean    → JS 自清理已证明画布干净:轻读(settle)后重发一次。
//	exec ok:false, 有幸存者  → 画布脏,绝不重发;幸存 id 交给调用方登记回收。
//	transport 失败          → 先轻读复核:已落地→收编 id 不重发;确实没落地→
//	                          settle 后重发一次;复核不出来(读失败/歧义)→不重发。
func drawOneZoneResilient(d zoneDrawDeps, t zonePartitionTarget, js string, known zoneKnownIDs) zoneDrawOutcome {
	if js == "" {
		return zoneDrawOutcome{Err: fmt.Errorf("degenerate frame bbox for %q — nothing to draw", t.Title)}
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		phase := fmt.Sprintf("draw partition frame %q", t.Title)
		if attempt > 0 {
			phase += " (retry)"
		}
		v, err := d.exec(phase, js)
		if err == nil {
			if asBool(v["ok"]) {
				rects, texts := asStringSlice(v["rects"]), asStringSlice(v["texts"])
				if len(rects) == 1 && len(texts) == 1 && rects[0] != "" && texts[0] != "" {
					return zoneDrawOutcome{RectID: rects[0], TextID: texts[0], Retried: attempt > 0}
				}
				return zoneDrawOutcome{
					Err:           fmt.Errorf("zone %q draw returned unexpected ids (rects=%v texts=%v)", t.Title, rects, texts),
					StrandedRects: rects,
					StrandedTexts: texts,
				}
			}
			// JS-level failure: the in-JS catch already deleted everything this
			// call created and re-read to prove it.
			survived := asStringSlice(v["cleanupSurvived"])
			cause := asString(v["error"])
			if len(survived) > 0 {
				return zoneDrawOutcome{
					Err: fmt.Errorf("zone %q draw failed (%s) and cleanup left survivors %v — canvas dirty, NOT retrying",
						t.Title, cause, survived),
					StrandedRects: intersectIDs(asStringSlice(v["rects"]), survived),
					StrandedTexts: intersectIDs(asStringSlice(v["texts"]), survived),
				}
			}
			lastErr = fmt.Errorf("zone %q draw failed: %s", t.Title, cause)
			if attempt == 0 {
				// Canvas proven clean by the JS itself → a resend cannot duplicate.
				// The settle read both confirms the channel is alive and (measured
				// on the degraded connector) raises the retry's success odds.
				if _, serr := d.survey(); serr != nil {
					return zoneDrawOutcome{Err: fmt.Errorf("%v; settle read also failed (%v) — connector wedged, NOT retrying", lastErr, serr)}
				}
				d.sleep()
				continue
			}
			return zoneDrawOutcome{Err: fmt.Errorf("%v (already retried once after a settle read)", lastErr)}
		}
		// Transport-level failure: the write may have LANDED even though the
		// call reported failure (假失败定律). Verify by light read before any
		// thought of resending.
		lastErr = err
		sv, serr := d.survey()
		if serr != nil {
			return zoneDrawOutcome{Err: fmt.Errorf(
				"zone %q draw transport failed (%v) and the landed-check read also failed (%v) — cannot prove the write did not land, NOT retrying (rerun zone-draw once the connector recovers; it will keep/skip anything that landed)",
				t.Title, err, serr)}
		}
		textID, textLanded := findSurveyTitleText(sv, t)
		if textLanded && known.Texts[textID] {
			textLanded = false // pre-existing text, not this write's product
		}
		var newRects []string
		for id := range sv.Rects {
			if !known.Rects[id] {
				newRects = append(newRects, id)
			}
		}
		switch {
		case textLanded && len(newRects) == 1:
			// The reported failure was fake: both primitives are on the canvas.
			return zoneDrawOutcome{RectID: newRects[0], TextID: textID, Adopted: true, Retried: attempt > 0}
		case !textLanded && len(newRects) == 0:
			// Proven not landed → a resend is exactly a first send.
			if attempt == 0 {
				d.sleep()
				continue
			}
			return zoneDrawOutcome{Err: fmt.Errorf("zone %q draw failed twice (%v); verified not landed both times", t.Title, err)}
		default:
			// Partial / ambiguous landing (e.g. rect on canvas, title missing, or
			// extra unexplained rects). Never resend into an unknown state; hand
			// every provable id back for recording so --clear can recover.
			out := zoneDrawOutcome{
				Err: fmt.Errorf(
					"zone %q draw transport failed (%v) and the landed-check is ambiguous (titleLanded=%v newRects=%v) — NOT retrying; stranded ids recorded for `sch zone-draw --clear`",
					t.Title, err, textLanded, newRects),
				StrandedRects: newRects,
			}
			if textLanded {
				out.StrandedTexts = []string{textID}
			}
			return out
		}
	}
	return zoneDrawOutcome{Err: lastErr}
}

func intersectIDs(ids, in []string) []string {
	set := map[string]bool{}
	for _, id := range in {
		set[id] = true
	}
	var out []string
	for _, id := range ids {
		if set[id] {
			out = append(out, id)
		}
	}
	return out
}

// jsonString marshals a Go string into a JS string literal (never fails for
// strings; centralizing keeps the `_ = err` noise out of the JS builders).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// runPartitionDrawResilient is the `sch zone-draw` command path. --clear keeps
// the proven legacy flow (verified per-id delete). The draw flow is the
// per-zone resilient protocol documented at the top of this file.
func runPartitionDrawResilient(cfg *appConfig, window string, opts partitionOpts, fontSize float64, color string, clear bool, stdout, stderr io.Writer) error {
	if clear {
		return runPartitionDraw(cfg, window, opts, fontSize, color, true, stdout, stderr)
	}
	pinnedCfg, win, docUUID, err := pinZonePage(cfg, window)
	if err != nil {
		return err
	}
	project, err := resolveStageProject(pinnedCfg, win)
	if err != nil {
		return err
	}
	st, err := loadPcbStageState(project)
	if err != nil {
		return err
	}
	exec := func(phase, code string) (map[string]any, error) {
		return execAutolayoutZoneJS(pinnedCfg, win, docUUID, phase, code)
	}
	surveyFn := func() (zoneFrameSurvey, error) {
		v, serr := exec("survey zone frames (light read)", buildZoneSurveyJS())
		if serr != nil {
			return zoneFrameSurvey{}, serr
		}
		return parseZoneFrameSurvey(v), nil
	}

	// Read-only planning/validation happens before ANY write, same as before.
	plan, _, err := computePartitionPlan(pinnedCfg, win, docUUID, opts)
	if err != nil {
		return err
	}
	// 判据与 runPartitionDraw 共用同一个函数本体(partitionDrawGate)——
	// 此前两处各写一遍,韧性路径的报文丢了 detail 与降级说明。
	if err := partitionDrawGate(plan); err != nil {
		return err
	}
	if fontSize <= 0 {
		fontSize = defaultPartitionZoneFontSize
	}
	targets := partitionTargets(plan, fontSize)
	if len(targets) == 0 {
		return fmt.Errorf("the partition plan has no partitions — nothing to draw")
	}
	colorJS, _ := json.Marshal(color)

	// Idempotency probe (light read, still zero writes): which planned zones are
	// ALREADY correctly on the page from a prior (possibly partial) run?
	sv, err := surveyFn()
	if err != nil {
		return fmt.Errorf("survey the page before drawing: %w (no write was attempted)", err)
	}
	prev, source := recordedZoneFrames(st, docUUID)
	// A legacy unscoped record whose ids are all absent here belongs to another
	// page: keep it for recovery on its actual page (same rule as the old path).
	if prev != nil && source == "legacy" && prev.DocumentUUID == "" && !anyZoneFrameIDLive(prev, sv) {
		fmt.Fprintln(stderr, "note: legacy unscoped zone-frame ids are not present on this page; record retained for its original page")
		prev, source = nil, ""
	}
	type matchedFrame struct{ rectID, textID string }
	matched := make([]*matchedFrame, len(targets))
	usedPrev := map[string]bool{}
	for i, t := range targets {
		if rid, tid, ok := matchExistingZoneFrame(t, prev, sv); ok {
			matched[i] = &matchedFrame{rectID: rid, textID: tid}
			usedPrev[rid], usedPrev[tid] = true, true
		}
	}

	// Clear ONLY the stale prior ids (recorded but not matching the current
	// plan). Matching frames are kept untouched — a page whose frames are
	// already correct does zero writes, so the old "cleared the good frames,
	// then failed to draw new ones" net-loss window no longer exists.
	cleared := 0
	if prev != nil {
		staleFrames := &workflow.SchZoneFrames{}
		for _, id := range prev.Rects {
			if !usedPrev[id] {
				staleFrames.Rects = append(staleFrames.Rects, id)
			}
		}
		for _, id := range prev.Texts {
			if !usedPrev[id] {
				staleFrames.Texts = append(staleFrames.Texts, id)
			}
		}
		if len(staleFrames.Rects)+len(staleFrames.Texts) > 0 {
			found, cerr := clearZoneFrames(exec, staleFrames, "clear stale zone frames")
			if cerr != nil {
				return fmt.Errorf("clear stale zone frames: %w (matching frames were kept; nothing new was drawn)", cerr)
			}
			cleared = found
			for _, id := range staleFrames.Rects {
				delete(sv.Rects, id)
			}
		}
		removeRecordedZoneFrames(st, docUUID, source)
	}

	known := knownIDsFromSurvey(sv)
	deps := zoneDrawDeps{
		exec:   exec,
		survey: surveyFn,
		sleep:  func() { time.Sleep(settleDelay) },
	}
	newFrames := &workflow.SchZoneFrames{At: nowRFC3339()}
	persist := func() error {
		setRecordedZoneFrames(st, docUUID, "partition", newFrames)
		return savePcbStageState(st)
	}
	kept, drawn, adopted := 0, 0, 0
	var notApplied []string
	for i, t := range targets {
		if m := matched[i]; m != nil {
			newFrames.Rects = append(newFrames.Rects, m.rectID)
			newFrames.Texts = append(newFrames.Texts, m.textID)
			kept++
			continue
		}
		out := drawOneZoneResilient(deps, t, buildPartitionZoneDrawJS(t, colorJS), known)
		if out.Err != nil {
			// Record every provable stranded id so --clear / the next redraw can
			// recover it, then move on — one zone's failure must not roll back
			// the zones already on the canvas (#151 partial semantics).
			newFrames.Rects = append(newFrames.Rects, out.StrandedRects...)
			newFrames.Texts = append(newFrames.Texts, out.StrandedTexts...)
			for _, id := range out.StrandedRects {
				known.Rects[id] = true
			}
			for _, id := range out.StrandedTexts {
				known.Texts[id] = true
			}
			if perr := persist(); perr != nil {
				return fmt.Errorf("zone %q failed (%v) AND persisting the recovery record failed: %w — ids on canvas: rects=%v texts=%v",
					t.Title, out.Err, perr, newFrames.Rects, newFrames.Texts)
			}
			notApplied = append(notApplied, fmt.Sprintf("%s: %v", t.Title, out.Err))
			fmt.Fprintf(stderr, "zone %q not applied: %v\n", t.Title, out.Err)
			continue
		}
		newFrames.Rects = append(newFrames.Rects, out.RectID)
		newFrames.Texts = append(newFrames.Texts, out.TextID)
		known.add(out.RectID, out.TextID)
		drawn++
		if out.Adopted {
			adopted++
			// 假失败回传(通道 B):daemon 把那次 exec_js 记成失败了,landed-check
			// 证明它其实落地了。不回传 = 健康度把「连接器慢」当成「连接器坏」。
			reportWriteVerified(pinnedCfg, win, writeVerdict{
				action: "debug.exec_js", source: "sch zone-draw",
				returnedOK: false, landed: 1,
			})
			fmt.Fprintf(stderr, "zone %q: write reported failure but the light read proved it landed — ids adopted, nothing was resent (假失败定律)\n", t.Title)
		} else if out.Retried {
			fmt.Fprintf(stderr, "zone %q: first attempt failed, succeeded on the settle-verified retry\n", t.Title)
		}
		// Persist after EVERY zone: a crash mid-run must leave a recoverable
		// record of everything already on the canvas.
		if perr := persist(); perr != nil {
			return fmt.Errorf("zone %q drawn (rect %s, text %s) but persisting the frame record failed: %w — rerun zone-draw after fixing the state dir",
				t.Title, out.RectID, out.TextID, perr)
		}
	}
	if perr := persist(); perr != nil {
		return fmt.Errorf("persist partition-frame ids: %w", perr)
	}
	if drawn > 0 || cleared > 0 {
		if err := saveZoneDocument(pinnedCfg, win, docUUID, "save partition zone frames"); err != nil {
			return fmt.Errorf("frames are on the canvas (drawn %d, kept %d) but the save failed: %w — save the schematic before closing the window", drawn, kept, err)
		}
	}

	fmt.Fprintf(stdout, "partition frames on page %s: %d drawn, %d kept (already correct), %d stale primitive(s) cleared", docUUID, drawn, kept, cleared)
	if adopted > 0 {
		fmt.Fprintf(stdout, ", %d adopted from a fake-failed write", adopted)
	}
	fmt.Fprintln(stdout, "; schematic saved")
	if len(notApplied) > 0 {
		if drawn == 0 && kept == 0 && cleared == 0 {
			// Nothing on this page changed → a plain failure is honest and safe.
			return fmt.Errorf("zone-draw: no partition frame could be drawn (%d/%d failed):\n  %s",
				len(notApplied), len(targets), strings.Join(notApplied, "\n  "))
		}
		// Canvas HAS changed → structured partial success (#151): report what is
		// missing and exit 0; a rerun keeps the good frames and only fills gaps.
		fmt.Fprintf(stdout, "partial: %d/%d zone(s) not applied — rerun `sch zone-draw` to fill only the missing ones:\n", len(notApplied), len(targets))
		for _, n := range notApplied {
			fmt.Fprintf(stdout, "  - %s\n", n)
		}
	}
	return nil
}

// anyZoneFrameIDLive reports whether any of a record's ids is present on the
// surveyed page (rect id alive or text id alive).
func anyZoneFrameIDLive(f *workflow.SchZoneFrames, s zoneFrameSurvey) bool {
	for _, id := range f.Rects {
		if s.Rects[id] {
			return true
		}
	}
	for _, id := range f.Texts {
		if s.hasText(id) {
			return true
		}
	}
	return false
}
