package app

// cmd_sch_zones.go — functional zones as a first-class SCHEMATIC object.
//
// The PCB side already made the S0 spec's modules[].zone executable (pcb_zones.go,
// issue #126); the schematic side had the same gap in a worse form: the S2/S3
// design-flow stages plan zones in prose, `sch autolayout --spec` consumes them
// once, and then nothing can ever say "U2 was supposed to live in the power zone
// but got parked over the MCU". This file closes that loop:
//
//   - `sch zones set --spec <s0-spec.json>` (or --module NAME=ZONE:D1,D2 …)
//     persists module → {page, grid zone, designators} into the same project
//     workflow state the PCB claims live in (State.SchZonesByPage — separate from
//     State.Zones because a module legitimately claims different zones on the
//     sheet vs the board);
//   - `sch autolayout --engine template` 用它决定模块该**落在纸面哪一格** —— 这是
//     认领现在唯一无可替代的职责:那一步在**布局之前**跑,件还没放下去,没有任何
//     活体几何可读,模块往哪去只能靠声明。
//
// **它不再负责「哪几件是一个模块」**(2026-08-15):`sch block-apply` 落块时已按功能
// 子群把件封成虚拟组,那是成员归属的单一事实来源,`sch zone-plan` / `zone-draw` 直接
// 读组。认领再抄一份成员列表就是第二处副本,而副本**不会跟着更新** —— 件被
// `group-move` 挪走或删掉,认领纹丝不动。没有虚拟组的页(手工搭的)才回落到认领。
//
// 同时废弃的还有 `sch layout-lint` 的 zone-violation 判据:分区框现在从活体模块 bbox
// 反推,「件在不在自己的框里」成了同义反复,永远不会失败的判据 = 没有判据。
//
// Zone names reuse the shared grid vocabulary (docs/concepts.md): columns
// left/center/right × rows top/bottom (sheet coords are y-UP, so "top" is the
// larger-y half — zoneRect in cmd_sch_autolayout.go is the single geometry
// source). The rectangle is resolved from the LIVE sheet bbox at placement time.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

type schZoneClaim = workflow.SchZoneClaim

// parseSchZoneSpec reads an S0 spec file's modules[] into schematic zone claims.
// Modules without a zone or without parts are skipped (not every module is zoned).
func parseSchZoneSpec(raw []byte) (map[string]*schZoneClaim, error) {
	var doc struct {
		Modules []struct {
			Name  string   `json:"name"`
			Page  string   `json:"page"`
			Zone  string   `json:"zone"`
			Parts []string `json:"parts"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	out := map[string]*schZoneClaim{}
	for i, m := range doc.Modules {
		zone := strings.ToLower(strings.TrimSpace(m.Zone))
		if zone == "" || len(m.Parts) == 0 {
			continue
		}
		if !pcbZoneNames[zone] {
			return nil, fmt.Errorf("modules[%d] %q: unknown zone %q (grid vocabulary: %s)",
				i, m.Name, m.Zone, strings.Join(sortedZoneNames(), ", "))
		}
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = fmt.Sprintf("module%d", i+1)
		}
		if prior := out[name]; prior != nil {
			return nil, fmt.Errorf("modules[%d] %q duplicates an earlier module name (module names must be unique across pages)", i, name)
		}
		out[name] = &schZoneClaim{
			Zone: zone, Page: strings.TrimSpace(m.Page),
			Parts: normalizeDesignators(m.Parts), Note: "spec",
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("spec has no zoned modules (modules[].zone + modules[].parts)")
	}
	return out, nil
}

// parseSchZoneModuleFlags parses repeatable --module NAME=ZONE:D1,D2 flags.
func parseSchZoneModuleFlags(items []string) (map[string]*schZoneClaim, error) {
	out := map[string]*schZoneClaim{}
	for _, it := range items {
		name, rest, ok := strings.Cut(it, "=")
		if !ok {
			return nil, fmt.Errorf("--module %q: expected NAME=ZONE:D1,D2", it)
		}
		zone, parts, ok := strings.Cut(rest, ":")
		zone = strings.ToLower(strings.TrimSpace(zone))
		if !ok || strings.TrimSpace(parts) == "" {
			return nil, fmt.Errorf("--module %q: expected NAME=ZONE:D1,D2", it)
		}
		if !pcbZoneNames[zone] {
			return nil, fmt.Errorf("--module %q: unknown zone %q (grid vocabulary: %s)", it, zone, strings.Join(sortedZoneNames(), ", "))
		}
		out[strings.TrimSpace(name)] = &schZoneClaim{
			Zone: zone, Parts: normalizeDesignators(strings.Split(parts, ",")), Note: "manual",
		}
	}
	return out, nil
}

// currentSchematicDocumentUUID returns the active schematic page identity. Zone
// claims and visual annotations are page-scoped; accepting an empty UUID would
// collapse unrelated pages into the same workflow-state key.
func currentSchematicDocumentUUID(cfg *appConfig, window string) (string, error) {
	cur, err := requestAction(cfg, "document.current", window, nil)
	if err != nil {
		return "", err
	}
	if cur.Context == nil || cur.Context.DocumentType != "schematic" {
		dt := "unknown"
		if cur.Context != nil && cur.Context.DocumentType != "" {
			dt = cur.Context.DocumentType
		}
		return "", fmt.Errorf("active document is %q, not a schematic", dt)
	}
	if cur.Context.DocumentUUID == "" {
		return "", fmt.Errorf("active schematic response omitted documentUuid")
	}
	return cur.Context.DocumentUUID, nil
}

// groupSchZoneClaimsByPage resolves each claim's optional Page selector into a
// document UUID. Claims without Page belong to the active page. The resolver is
// injected to keep the grouping/multi-page overwrite rules unit-testable.
func groupSchZoneClaimsByPage(
	claims map[string]*schZoneClaim,
	activeUUID string,
	resolve func(string) (string, error),
) (map[string]map[string]*schZoneClaim, error) {
	out := map[string]map[string]*schZoneClaim{}
	for name, claim := range claims {
		if claim == nil {
			continue
		}
		docUUID := activeUUID
		if selector := strings.TrimSpace(claim.Page); selector != "" {
			var err error
			docUUID, err = resolve(selector)
			if err != nil {
				return nil, fmt.Errorf("module %q page %q: %w", name, selector, err)
			}
		}
		if strings.TrimSpace(docUUID) == "" {
			return nil, fmt.Errorf("module %q has no page and the active schematic document UUID is unavailable", name)
		}
		if out[docUUID] == nil {
			out[docUUID] = map[string]*schZoneClaim{}
		}
		if prior := out[docUUID][name]; prior != nil {
			return nil, fmt.Errorf("page %s has duplicate module name %q", docUUID, name)
		}
		out[docUUID][name] = claim
	}
	return out, nil
}

// loadSchZoneClaimsForPage loads one pinned page's schematic zone table without
// consulting the mutable foreground document. Legacy project-wide SchZones
// remains readable until the first page-scoped write.
func loadSchZoneClaimsForPage(cfg *appConfig, window, docUUID string) (map[string]*schZoneClaim, string, error) {
	project, err := resolveStageProject(cfg, window)
	if err != nil {
		return nil, "", err
	}
	st, err := loadPcbStageState(project)
	if err != nil {
		return nil, project, err
	}
	if len(st.SchZonesByPage) == 0 {
		return st.SchZones, project, nil
	}
	if strings.TrimSpace(docUUID) == "" {
		return nil, project, fmt.Errorf("select page-scoped schematic zones: pinned document UUID is empty")
	}
	return st.SchZonesForPage(docUUID), project, nil
}

// sheetBBoxOf pulls the sheet primitive's bbox out of a components.list parse
// (componentType "sheet"). Nil when the page exposes none.
func sheetBBoxOf(comps []layoutComp) *layoutBBox {
	for _, c := range comps {
		if c.ComponentType == "sheet" && c.BBox != nil {
			return c.BBox
		}
	}
	return nil
}

// newSchZonesCmd builds the `sch zones` command group.
func newSchZonesCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	zones := &cobra.Command{
		Use:   "zones",
		Short: "原理图分区认领(S0 modules[].zone → 图纸格位):set / status / clear",
		Long: `把 S0 方案书的功能分区(modules[].zone)落到原理图这一侧。

**它只剩一个不可替代的用途**:在**布局之前**告诉 ` + "`sch autolayout --engine template`" + `
每个模块该落在纸面的哪一格 —— 那一步跑的时候件还没放下去,没有任何活体几何可读,
模块往哪去只能靠声明。

**它不再负责「哪几件是一个模块」**:` + "`sch block-apply`" + ` 落块时已按**功能子群**
把件封成虚拟组(flow 的每一级 + 跟着它的去耦/并列组),` + "`sch zone-plan`" + ` /
` + "`zone-draw`" + ` 直接读组 —— 那是成员归属的**单一事实来源**。认领再抄一份成员列表
就是第二处副本,而副本不会跟着更新(件被 ` + "`group-move`" + ` 挪走或删掉,认领纹丝不动)。
只有**手工搭的页**(没有块、没有虚拟组)才需要用它来告诉工具谁跟谁一组。

` + "`sch layout-lint`" + ` 的 zone-violation 判据**已废弃**:分区框现在从活体模块 bbox
反推,「件在不在自己的框里」是同义反复 —— 永远不会失败的判据等于没有判据。
分区画得对不对由 ` + "`sch zone-plan`" + ` 的六项落笔前 validation 判。

Zone names are the shared grid vocabulary (same as autolayout + pcb zones):
left / center / right × top / bottom (e.g. right-top), or full-height/width
left / right / top / bottom / center. The canvas is y-UP, and "top" means the
VISUALLY upper half (larger y — zoneRect owns the mapping). The rectangle is
resolved from the LIVE sheet bbox at placement time. Claims live by schematic
document UUID in the project workflow state
(~/.easyeda-agent/workflow/<project>.json); modules[].page in a spec is resolved
to its page UUID, while manual --module claims apply to the active/--doc page.
This is separate from PCB zone claims — the same module may claim different
zones on sheet vs board.`,
	}
	zones.AddCommand(newSchZonesSetCmd(cfg, window, stdout, stderr))
	zones.AddCommand(newSchZonesStatusCmd(cfg, window, stdout))
	zones.AddCommand(newSchZonesClearCmd(cfg, window, stdout))
	return zones
}

func newSchZonesSetCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var specPath string
	var modules []string
	c := &cobra.Command{
		Use:   "set",
		Short: "认领模块的落位格位(给布局前的 autolayout 用;成员归属请用虚拟组)",
		Example: `  easyeda sch zones set --spec s0-esp32mini.json --project ceshi
  easyeda sch zones set --module "POWER=left-top:U3,C5,C6" --module "MCU=center:U1" --project ceshi`,
		RunE: func(cmd *cobra.Command, args []string) error {
			pinnedCfg, win, activeUUID, err := pinZonePage(cfg, *window)
			if err != nil {
				return err
			}
			var claims map[string]*schZoneClaim
			switch {
			case specPath != "" && len(modules) > 0:
				return fmt.Errorf("--spec and --module are mutually exclusive")
			case specPath != "":
				raw, rerr := os.ReadFile(specPath)
				if rerr != nil {
					return rerr
				}
				claims, err = parseSchZoneSpec(raw)
			case len(modules) > 0:
				claims, err = parseSchZoneModuleFlags(modules)
			default:
				return fmt.Errorf("pass --spec <s0-spec.json> or --module NAME=ZONE:D1,D2")
			}
			if err != nil {
				return err
			}
			project, perr := resolveStageProject(pinnedCfg, win)
			if perr != nil {
				return perr
			}
			st, serr := loadPcbStageState(project)
			if serr != nil {
				return serr
			}
			for _, zc := range claims {
				zc.At = nowRFC3339()
			}

			if specPath != "" {
				docs, _, _, derr := discoverDocs(pinnedCfg, win)
				if derr != nil {
					return fmt.Errorf("resolve modules[].page: %w", derr)
				}
				byPage, gerr := groupSchZoneClaimsByPage(claims, activeUUID, func(selector string) (string, error) {
					doc, rerr := resolveDoc(docs, selector)
					if rerr != nil {
						return "", rerr
					}
					if doc.Type != "schematic" {
						return "", fmt.Errorf("%q resolves to a %s document, want schematic", selector, doc.Type)
					}
					return doc.UUID, nil
				})
				if gerr != nil {
					return gerr
				}
				st.ReplaceSchZonesByPage(byPage)
			} else {
				st.SetSchZonesForPage(activeUUID, claims)
			}
			// A page-scoped write is authoritative. Keeping the legacy table in the
			// JSON would be ambiguous to older tools and confusing during audits.
			st.SchZones = nil
			if err := savePcbStageState(st); err != nil {
				return err
			}
			total := 0
			for name, zc := range claims {
				total += len(zc.Parts)
				page := ""
				if zc.Page != "" {
					page = " page=" + zc.Page
				}
				fmt.Fprintf(stderr, "✓ %s → %s%s (%d part(s): %s)\n", name, zc.Zone, page, len(zc.Parts), strings.Join(zc.Parts, ","))
			}
			fmt.Fprintf(stderr, "已记下 %q 的分区认领 —— %d 个模块 / %d 件,按页(documentUuid)持久化;"+
				"消费者只有 `sch autolayout --engine template`(布局前的落位目标格)。"+
				"分区框与说明读的是**虚拟组**,不读这里;手工搭的页(没有虚拟组)才回落到它\n",
				project, len(claims), total)
			return nil
		},
	}
	c.Flags().StringVar(&specPath, "spec", "", "S0 spec JSON with modules[].zone + modules[].parts (+ optional modules[].page)")
	c.Flags().StringArrayVar(&modules, "module", nil, `manual claim: NAME=ZONE:D1,D2 (repeatable)`)
	return c
}

func newSchZonesStatusCmd(cfg *appConfig, window *string, stdout io.Writer) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "列出已记下的分区认领(活体违规判据已随 zone-violation 一并废弃)",
		RunE: func(cmd *cobra.Command, args []string) error {
			pinnedCfg, win, docUUID, err := pinZonePage(cfg, *window)
			if err != nil {
				return err
			}
			zones, project, err := loadSchZoneClaimsForPage(pinnedCfg, win, docUUID)
			if err != nil {
				return err
			}
			if asJSON {
				st, serr := loadPcbStageState(project)
				if serr != nil {
					return serr
				}
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"project": project, "documentUuid": docUUID, "schZones": zones,
					"schZonesByPage": st.SchZonesByPage, "legacySchZones": st.SchZones,
				})
			}
			if len(zones) == 0 {
				fmt.Fprintf(stdout, "no schematic zone claims for %q on page %s — `sch zones set --spec <s0-spec.json>`\n", project, docUUID)
				return nil
			}
			fmt.Fprintf(stdout, "schematic zone claims — project %q, page %s\n", project, docUUID)
			var names []string
			for n := range zones {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				zc := zones[n]
				page := "-"
				if zc.Page != "" {
					page = zc.Page
				}
				fmt.Fprintf(stdout, "  %-12s %-13s page=%-16s %d part(s): %s\n", n, zc.Zone, page, len(zc.Parts), strings.Join(zc.Parts, ","))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit claims as JSON")
	return c
}

func newSchZonesClearCmd(cfg *appConfig, window *string, stdout io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:   "clear",
		Short: "Remove all schematic zone claims",
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := resolveStageProject(cfg, *window)
			if err != nil {
				return err
			}
			st, err := loadPcbStageState(project)
			if err != nil {
				return err
			}
			n := len(st.SchZones)
			for _, pageClaims := range st.SchZonesByPage {
				n += len(pageClaims)
			}
			st.SchZones = nil
			st.SchZonesByPage = nil
			st.History = append(st.History, workflow.Event{
				Stage: "sch-zones", At: nowRFC3339(), Action: "confirm",
				Note: "cleared all schematic page zone claims",
			})
			if err := savePcbStageState(st); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "cleared %d schematic zone claim(s) for %q\n", n, project)
			return nil
		},
	}
	return c
}
