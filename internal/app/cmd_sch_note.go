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
	c := &cobra.Command{
		Use:   "note",
		Short: "Place a circuit-description text note (电路说明) on the schematic sheet",
		Long: `Place a circuit-description text note (电路说明) on the schematic sheet.

Functional partitioning is only half of the layout convention — a zone frame
names a module, but a reviewer still needs the one or two lines that say what
the block does and what its key parameters are. The skill's schematic layout
default is: one short note per module, parked just below/beside its zone frame.

  - **省略 --x/--y = 自动落点(推荐)**:说明文字和器件、marker、已有文字、图签
    keep-out 是**同级的布局对象**,一起进同一张碰撞表求解 —— 优先贴该区内容
    下沿(读图习惯:先看电路再看下面那行说明),排不下依次试区内上沿/区外侧/
    区正下方,最后整页从左下往上扫。放不下就报错**拒绝画**,不把说明糊在电路上。
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
			if hit, aerr := placeSchNote(pinnedCfg, win, docUUID, zoneRef, &content, fontSize, auto, &x, &y); aerr != nil {
				return aerr
			} else if hit != "" {
				fmt.Fprintf(stderr, "warning: %s\n", hit)
			}
			v, err := execAutolayoutZoneJS(pinnedCfg, win, docUUID, "create schematic note", buildSchNoteJS(x, y, content, color, fontSize))
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
			// 用户定义的对象模型)。登记后:分区框把它 fold 进画框口径、zone move
			// 无条件带走(不再依赖"锚点恰好在框内"的几何猜)。
			if zoneRef != "" {
				if rerr := registerSchZoneNote(pinnedCfg, win, docUUID, zoneRef, tid); rerr != nil {
					return fmt.Errorf("note %s 已创建并保存,但登记到区 %q 失败:%w(可重跑 `sch note` 前先 prim-delete,或忽略登记)", tid, zoneRef, rerr)
				}
				fmt.Fprintf(stdout, "note created (primitiveId %s) at (%g, %g), registered to zone %q; schematic saved\n", tid, x, y, zoneRef)
				return nil
			}
			fmt.Fprintf(stdout, "note created (primitiveId %s) at (%g, %g) on page %s; schematic saved\n", tid, x, y, docUUID)
			return nil
		},
	}
	c.Flags().StringVar(&text, "text", "", "note content; a literal \\n becomes a line break (required)")
	c.Flags().Float64Var(&x, "x", 0, "text anchor x (schematic units) — 省略 --x/--y 即自动落点(推荐):说明与器件/marker/已有文字/图签 keep-out 同级参与碰撞求解,自己找不压任何东西的位置")
	c.Flags().Float64Var(&y, "y", 0, "text anchor y (schematic units, y-UP) — 省略即自动落点")
	c.Flags().Float64Var(&fontSize, "font-size", schNoteDefaultFontSize, "font size")
	c.Flags().StringVar(&color, "color", schNoteDefaultColor, "text color")
	c.Flags().StringVar(&zoneRef, "zone", "", "把说明登记到一个布局对象(模块认领/块组/子组统一命名空间,`sch zones status` 看全表)—— 分区框会围住它,`sch zone move` 无条件带走它")
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
