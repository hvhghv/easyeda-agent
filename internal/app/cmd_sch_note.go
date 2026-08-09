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
	var text, color string
	var x, y, fontSize float64
	c := &cobra.Command{
		Use:   "note",
		Short: "Place a circuit-description text note (电路说明) on the schematic sheet",
		Long: `Place a circuit-description text note (电路说明) on the schematic sheet.

Functional partitioning is only half of the layout convention — a zone frame
names a module, but a reviewer still needs the one or two lines that say what
the block does and what its key parameters are. The skill's schematic layout
default is: one short note per module, parked just below/beside its zone frame.

  - Multi-line: a literal \n in --text becomes a real line break.
  - Coordinates are schematic units, y-UP (larger y = higher on the sheet).
    Read the target zone frame / part bbox first (sch list --include-bbox,
    sch zones status) and park the note where it does not overlap the circuit;
    verify with sch layout-lint afterwards.
  - Notes are plain text primitives: enumerate with sch text-list, remove with
    sch prim-delete --ids. The schematic is saved after a successful create.`,
		Example: `  easyeda sch note --text "LDO: 5V→3V3 1A\n输入 22µF / 输出 22µF" --x 120 --y 300
  easyeda sch note --doc P2 --text "BOOT: GPIO0 拉低进烧录" --x 640 --y 210 --font-size 9`,
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
			fmt.Fprintf(stdout, "note created (primitiveId %s) at (%g, %g) on page %s; schematic saved\n", tid, x, y, docUUID)
			return nil
		},
	}
	c.Flags().StringVar(&text, "text", "", "note content; a literal \\n becomes a line break (required)")
	c.Flags().Float64Var(&x, "x", 0, "text anchor x (schematic units)")
	c.Flags().Float64Var(&y, "y", 0, "text anchor y (schematic units, y-UP)")
	c.Flags().Float64Var(&fontSize, "font-size", schNoteDefaultFontSize, "font size")
	c.Flags().StringVar(&color, "color", schNoteDefaultColor, "text color")
	_ = c.MarkFlagRequired("text")
	_ = c.MarkFlagRequired("x")
	_ = c.MarkFlagRequired("y")
	return c
}
