package app

// cmd_sch_note.go — `sch note`: put a CIRCUIT-DESCRIPTION text note on the
// schematic sheet (电路说明).
//
// Why this exists: functional partitioning is only half of the "先看区、再看线"
// convention — a zone frame names a module, but a reviewer still needs the one
// or two lines that say what the block does and what its key parameters are
// (「LDO 5V→3V3 1A」「BOOT: GPIO0 拉低进烧录」). User feedback: agent-produced
// schematics shipped with zones but no descriptions, so the skill now treats a
// short note per module as part of the layout default — and this command is the
// typed path that makes that default executable.
//
// Implementation note: same situation as `sch zone-draw` — the schematic text
// API (eda.sch_PrimitiveText, full CRUD) has no typed action, so this goes
// through the CLI-internal exec_js hatch, with created-id readback verification
// and an explicit save. Notes are plain text primitives: enumerate them with
// `sch text-list`, remove with `sch prim-delete --ids`.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// schNoteDefaultFontSize keeps notes visually subordinate to zone labels
// (14) and partition titles (22): a note is an annotation, not a heading.
const schNoteDefaultFontSize = 10.0

// schNoteDefaultColor is a mid gray — readable, but clearly annotation-tier
// against the magenta zone frames and the black circuit.
const schNoteDefaultColor = "#5A5A5A"

// buildSchNoteJS renders the exec_js that creates one text primitive and
// returns its id. Pure (unit-testable). The create signature mirrors
// zone-draw's labels: (x, y, content, rotation, color, fontFamily, fontSize).
//
// 文本一律经 json.Marshal 进 JS 字符串字面量 —— `~`、`+/-`、引号、换行、`%`、
// 反引号全都安全(TestBuildSchNoteJSEscapesSpecialText 钉死)。2026-08-19 E2E
// 报的「含 ~ / +/- 的说明让 exec_js 挂掉」经审计日志定案为**误诊**:失败载荷
// 的 JS 完全合法且正常执行,是 eda.sch_PrimitiveText.create 偶发返回 undefined
// (同一段文本重试即成功;与 zone-draw 注释里「平台偶发吞创建请求」同一病),
// 修法是下面 RunE 里的 settle+重试,不是转义。
func buildSchNoteJS(x, y float64, text, color string, fontSize float64) string {
	content, _ := json.Marshal(text)
	colorJS, _ := json.Marshal(color)
	var b strings.Builder
	b.WriteString("const tx = await eda.sch_PrimitiveText.create(")
	fmt.Fprintf(&b, "%g, %g, %s, 0, %s, null, %g);\n", x, y, content, colorJS, fontSize)
	b.WriteString("if (!tx) throw new Error('text create returned undefined');\n")
	b.WriteString("const tid = tx.getState_PrimitiveId();\n")
	b.WriteString("if (!tid) { await eda.sch_PrimitiveObject.delete([tx]); throw new Error('text id missing'); }\n")
	b.WriteString("return { textId: tid };")
	return b.String()
}

// newSchNoteCmd builds `sch note`.
func newSchNoteCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var text, color, zoneRef string
	var x, y, fontSize float64
	var asJSON bool
	c := &cobra.Command{
		Use:   "note",
		Short: "Place a circuit-description text note (电路说明) on the schematic sheet",
		Long: `Place a circuit-description text note (电路说明) on the schematic sheet.

Functional partitioning is only half of the layout convention — a zone frame
names a module, but a reviewer still needs the one or two lines that say what
the block does and what its key parameters are. The skill's schematic layout
default is: one short note per module, parked just below/beside its zone frame.

  - **省略 --x/--y = 自动落点(推荐)**:说明文字和器件、marker、已有文字、图签
    keep-out 是**同级的布局对象**,一起进同一张碰撞表求解。给了 --zone 时,说明
    的家是该区分区框底部的**说明带**:先按框宽折行,框装不下就把框**横向扩边**
    (窄框扩到最小可读宽度),带内被邻区桩线占住就把框底**下探**到占用之下 ——
    **框为说明扩边,而不是把说明踢出框**。扩过边要重跑
    sch zone-draw --mode partition 让画布上的框跟上(命令会在 stderr 提示)。
  - 区里实在装不下(可扩边界被纸边/图签/邻框顶死)才退到区外走廊/整页扫描,
    并明确警告;整页都放不下就报错**拒绝画**,不把说明糊在电路上。
  - 显式给 --x/--y 时坐标一字不改,但仍会回读碰撞;压到东西会明确警告(不静默)。
  - Multi-line: a literal \n in --text becomes a real line break.
  - Coordinates are schematic units, y-UP (larger y = higher on the sheet).
  - Notes are plain text primitives: enumerate with sch text-list, remove with
    sch prim-delete --ids. The schematic is saved after a successful create.`,
		Example: `  easyeda sch note --zone POWER --text "LDO: 5V→3V3 1A\n输入 22µF / 输出 22µF"   # 自动落点
  easyeda sch note --text "BOOT: GPIO0 拉低进烧录" --x 640 --y 210 --font-size 9        # 显式坐标`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("--text is required (the note content)")
			}
			if fontSize <= 0 {
				fontSize = schNoteDefaultFontSize
			}
			// A literal backslash-n typed in a shell argument means "line break".
			content := strings.ReplaceAll(text, `\n`, "\n")

			pinnedCfg, win, docUUID, err := pinZonePage(cfg, *window)
			if err != nil {
				return err
			}
			// 不给坐标 = 让代码算:说明与器件/marker/已有文字/图签 keep-out 同级
			// 参与碰撞求解(用户纠偏 2026-08-13)。给了坐标就一字不改地照放,
			// 但仍然回读一次碰撞并在压到东西时明确警告 —— 不静默画上去。
			auto := !cmd.Flags().Changed("x") && !cmd.Flags().Changed("y")
			warns, zoneMatched, aerr := placeSchNote(pinnedCfg, win, docUUID, zoneRef, &content, fontSize, auto, &x, &y)
			if aerr != nil {
				return aerr
			}
			for _, wmsg := range warns {
				fmt.Fprintf(stderr, "warning: %s\n", wmsg)
			}
			// 平台偶发吞创建请求:eda.sch_PrimitiveText.create 偶发返回 undefined
			// (2026-08-19 审计日志定案:同一段文本、连接器活跃、1~5ms 即败,重试
			// 即成 —— 与 zone-draw 画框同一病同一修法)。失败时半成品不会留在画布
			// 上(tx undefined = 没建出来;tid 缺失时 JS 已自删),重发等价于第一次。
			js := buildSchNoteJS(x, y, content, color, fontSize)
			v, err := execAutolayoutZoneJS(pinnedCfg, win, docUUID, "create schematic note", js)
			if err != nil {
				time.Sleep(settleDelay)
				v, err = execAutolayoutZoneJS(pinnedCfg, win, docUUID, "create schematic note (retry)", js)
			}
			if err != nil {
				return err
			}
			tid := asString(mnav(v, "textId"))
			if tid == "" {
				return fmt.Errorf("note create returned no primitive id — nothing was written (raw: %v)", v)
			}
			if err := saveZoneDocument(pinnedCfg, win, docUUID, "save schematic note"); err != nil {
				return err
			}
			// --zone:把说明登记为功能区的内置对象(Zone = 外框+标题+说明+组+散件,
			// 用户定义的对象模型)。登记后 zone move 无条件带走(不再依赖"锚点恰好
			// 在框内"的几何猜)。注意:登记的说明**不再**反哺分区框的内容 bbox
			// (根因 C 的自增长反馈环已断)——它的家是分区框内的说明带。
			emit := func(registered bool) error {
				if asJSON {
					out := map[string]any{"textId": tid, "x": x, "y": y, "page": docUUID, "saved": true}
					if zoneRef != "" {
						out["zone"] = zoneRef
						out["registered"] = registered
						// zoneMatched = 该区是否在本页分区计划里命中(命中才有
						// 说明带可落;false = 已按整页避让兜底,见 stderr warning)。
						out["zoneMatched"] = zoneMatched
					}
					enc := json.NewEncoder(stdout)
					enc.SetIndent("", "  ")
					return enc.Encode(out)
				}
				if zoneRef != "" {
					fmt.Fprintf(stdout, "note created (primitiveId %s) at (%g, %g), registered to zone %q (zoneMatched=%v); schematic saved\n", tid, x, y, zoneRef, zoneMatched)
					return nil
				}
				fmt.Fprintf(stdout, "note created (primitiveId %s) at (%g, %g) on page %s; schematic saved\n", tid, x, y, docUUID)
				return nil
			}
			if zoneRef != "" {
				if rerr := registerSchZoneNote(pinnedCfg, win, docUUID, zoneRef, tid); rerr != nil {
					return fmt.Errorf("note %s 已创建并保存,但登记到区 %q 失败:%w(可重跑 `sch note` 前先 prim-delete,或忽略登记)", tid, zoneRef, rerr)
				}
				return emit(true)
			}
			return emit(false)
		},
	}
	c.Flags().StringVar(&text, "text", "", "note content; a literal \\n becomes a line break (required)")
	c.Flags().Float64Var(&x, "x", 0, "text anchor x (schematic units) — 省略 --x/--y 即自动落点(推荐):说明与器件/marker/已有文字/图签 keep-out 同级参与碰撞求解,自己找不压任何东西的位置")
	c.Flags().Float64Var(&y, "y", 0, "text anchor y (schematic units, y-UP) — 省略即自动落点")
	c.Flags().Float64Var(&fontSize, "font-size", schNoteDefaultFontSize, "font size")
	c.Flags().StringVar(&color, "color", schNoteDefaultColor, "text color")
	c.Flags().StringVar(&zoneRef, "zone", "", "把说明登记到一个布局对象(模块认领/块组/子组统一命名空间,全名/末段短名/组 id/唯一前缀均可,`sch zones status` 看全表)—— 自动落点优先落进该区分区框内的说明带,`sch zone move` 无条件带走它")
	c.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出结果(textId/x/y/zoneMatched 等)")
	_ = c.MarkFlagRequired("text")
	return c
}

// registerSchZoneNote 把新建说明的 primitiveId 记到它所属的布局对象名下(幂等)。
//
// **解析统一,写回分叉**:--zone 走统一注册表解析(resolveLayoutObject,模块认领 /
// 块组 / 子组同一张表,子组末段与组 id 都是别名),但写必须落到数据真正的家 ——
// 命中虚拟组就写 Group.NoteIDs,命中认领就写 claim.NoteIDs(zoneClaim 对认领
// 直通存储指针,对组是投影 —— 往投影上写会随返回值一起蒸发,所以组走 o.Group)。
func registerSchZoneNote(cfg *appConfig, window, docUUID, zoneRef, textID string) error {
	project, err := resolveStageProject(cfg, window)
	if err != nil {
		return err
	}
	st, err := loadPcbStageState(project)
	if err != nil {
		return err
	}
	claims := st.SchZonesForPage(docUUID)
	groups := st.GroupsForPage(docUUID)
	obj, err := resolveLayoutObject(buildLayoutObjectTable(claims, groups), zoneRef)
	if err != nil {
		return err
	}
	if obj.Group != nil {
		for _, id := range obj.Group.NoteIDs {
			if id == textID {
				return nil // 幂等
			}
		}
		obj.Group.NoteIDs = append(obj.Group.NoteIDs, textID)
		return saveSchGroups(st, docUUID, groups)
	}
	for _, id := range obj.Claim.NoteIDs {
		if id == textID {
			return nil
		}
	}
	obj.Claim.NoteIDs = append(obj.Claim.NoteIDs, textID)
	st.SetSchZonesForPage(docUUID, claims)
	return savePcbStageState(st)
}
