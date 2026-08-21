package app

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

const titleBlockWriteDisabledMessage = "图签字段写入已禁用：EasyEDA Pro 的 modifySchematicPageTitleBlock 会损坏图签内部模型。请使用 sch note 放置图纸说明；标题栏字段保持平台默认值。"

func titleBlockWriteDisabledError() error {
	return fmt.Errorf("%s", titleBlockWriteDisabledMessage)
}

// newSchTitleBlockHealthCmd exposes the connector's read-only model probe. An
// explicit --reload performs the documented save-close-reopen stability check;
// it is intentionally opt-in because reload saves the active document.
func newSchTitleBlockHealthCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var reload bool
	c := &cobra.Command{
		Use:   "titleblock-health",
		Short: "Check title-block model health (official data, sheet geometry, and DRC)",
		Long: `Check the official title-block model without writing it.

Healthy requires non-empty official titleBlockData with core keys, a valid sheet
primitive/bounding box, and no fatal/error official DRC findings. --reload adds
a save-close-reopen pass and compares the same model after the reload. Do not
attempt setDocumentSource repair when this reports unhealthy; restore the
standard Drawing-Symbol_A4 in the EasyEDA UI after preserving a backup.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !reload {
				return dispatch(cfg, "schematic.titleblock.health", *window, nil, stdout, stderr)
			}

			win, err := resolveTargetWindow(cfg, *window)
			if err != nil {
				return err
			}
			before, err := requestAction(cfg, "schematic.titleblock.health", win, nil)
			if err != nil {
				return err
			}
			cur, err := requestAction(cfg, "document.current", win, nil)
			if err != nil {
				return fmt.Errorf("titleblock-health: resolve active document for reload: %w", err)
			}
			if cur.Context == nil || cur.Context.DocumentUUID == "" {
				return fmt.Errorf("titleblock-health: no active document available for reload")
			}
			if cur.Context.DocumentType != "schematic" {
				return fmt.Errorf("titleblock-health: active document %s is %s, not a schematic", cur.Context.DocumentUUID, cur.Context.DocumentType)
			}
			if _, err := reloadDocumentByUUID(cfg, win, cur.Context.DocumentUUID); err != nil {
				return fmt.Errorf("titleblock-health: reload verification failed: %w", err)
			}
			after, err := requestAction(cfg, "schematic.titleblock.health", win, nil)
			if err != nil {
				return err
			}
			return writeJSON(stdout, map[string]any{
				"before":   before.Result,
				"after":    after.Result,
				"reloaded": cur.Context.DocumentUUID,
			})
		},
	}
	c.Flags().BoolVar(&reload, "reload", false, "save, close, reopen, then re-check title-block health")
	return c
}
