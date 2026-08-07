package app

// cmd_spec.go — `easyeda spec`：S0 设计方案书的校验与查看。
//
// 在此之前 S0 spec 写错是**完全静默**的：只要 modules[].zone/parts 对，
// `pcb zones set` 就成功，其余字段无人看 —— 磁盘上那份真实 spec 已经把 board 写成
// 字符串、stackup 用了未文档化的键，一直没人发现。#167 又要往里加 flow 和连接器
// facing/internal，再没有校验，写错一个枚举值就会让整维打分静默失灵（flow 里拼错
// 一个 "MUC"，flow-order 维直接跳过，报告上只是少了一行）。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

func newSpecCmd(stdout, stderr io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:   "spec",
		Short: "Validate and inspect the S0 design proposal (S0 spec)",
		Long: "S0 spec 是设计意图的唯一入口：功能模块分区、信号流向(flow)、对外接口的\n" +
			"边与朝向(edge/facing/internal)、叠层、射频禁布。`pcb zones set --spec` 与\n" +
			"`pcb layout-score --spec` 都读它。",
	}
	c.AddCommand(newSpecValidateCmd(stdout, stderr))
	c.AddCommand(newSpecShowCmd(stdout))
	return c
}

func newSpecValidateCmd(stdout, stderr io.Writer) *cobra.Command {
	var asJSON, strict bool
	c := &cobra.Command{
		Use:   "validate <s0-spec.json>",
		Short: "Check an S0 spec for unknown enum values, contradictions and gaps",
		Long: "判定口径刻意宽松，理由是兼容既有 spec：\n\n" +
			"  ERROR  写了但写错 —— 枚举外的 zone/kind/facing、flow 里重复或不存在的\n" +
			"         阶段、internal:true 与 facing:\"user-facing\" 自相矛盾。\n" +
			"  WARN   缺了会让某维测不了 —— 没有 flow、模块没归功能域、flow 阶段在板上\n" +
			"         没有对应模块。\n" +
			"  INFO   能力降级 —— 接口没写 ref 就钉不到具体器件，连接器规则只能退回\n" +
			"         启发式（报 INFO 而非 WARN）。\n\n" +
			"默认只有 ERROR 才非零退出；--strict 让 WARN 也失败。",
		Example: "  easyeda spec validate .easyeda/s0-ceshi.json\n" +
			"  easyeda spec validate s0.json --strict   # 交付前用\n" +
			"  easyeda spec validate s0.json --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("read spec: %w", err)
			}
			s, err := spec.Parse(raw)
			if err != nil {
				return err
			}
			issues := spec.Validate(s)

			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(map[string]any{
					"ok":     !spec.HasErrors(issues),
					"issues": issues,
					"counts": issueCounts(issues),
				}); err != nil {
					return err
				}
			} else {
				renderSpecIssues(args[0], issues, stdout)
			}

			counts := issueCounts(issues)
			if counts["ERROR"] > 0 {
				return fmt.Errorf("S0 spec has %d error(s)", counts["ERROR"])
			}
			if strict && counts["WARN"] > 0 {
				return fmt.Errorf("S0 spec has %d warning(s) (--strict)", counts["WARN"])
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit the issue list as JSON")
	c.Flags().BoolVar(&strict, "strict", false, "fail on warnings too (use before handoff)")
	return c
}

func newSpecShowCmd(stdout io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:   "show <s0-spec.json>",
		Short: "Print the normalized S0 spec (both legacy spellings folded into one shape)",
		Long: "把 spec 归一化后打印：board 的字符串写法折进 outline、stackup 的 inner1/\n" +
			"inner2 折进 innerLayers、模块的功能域(kind)按 name 补全。用来确认「工具\n" +
			"实际读到的意图」与你写的是不是一回事。",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("read spec: %w", err)
			}
			s, err := spec.Parse(raw)
			if err != nil {
				return err
			}
			view := map[string]any{
				"board":       s.Board,
				"flow":        s.Flow,
				"flowAxis":    s.Axis(),
				"stackup":     s.Stackup,
				"innerLayers": s.Stackup.Inners(),
				"assembly":    s.Assembly,
				"rf":          s.RF,
				"modules":     normalizedModules(s),
				"interfaces":  normalizedInterfaces(s),
			}
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(view)
		},
	}
	return c
}

// normalizedModules 展示每个模块**生效的**功能域（含从 name 反查补全的），
// 而不是原样回显 —— 「工具读到了什么」才是要确认的东西。
func normalizedModules(s *spec.Spec) []map[string]any {
	out := make([]map[string]any, 0, len(s.Modules))
	for _, m := range s.Modules {
		kind := m.KindOf()
		entry := map[string]any{
			"name":  m.Name,
			"kind":  kind,
			"zone":  m.Zone,
			"parts": m.PartsOf(),
		}
		if kind == "" {
			entry["kindNote"] = "未归功能域 —— 不参与 flow-order 维"
		} else if m.Kind == "" {
			entry["kindNote"] = "由 name 推断（建议显式写 kind）"
		}
		if m.Block != "" {
			entry["block"] = m.Block
		}
		out = append(out, entry)
	}
	return out
}

// normalizedInterfaces 展示每个接口生效的 facing（internal 简写已折算）。
func normalizedInterfaces(s *spec.Spec) []map[string]any {
	out := make([]map[string]any, 0, len(s.Interfaces))
	for _, i := range s.Interfaces {
		entry := map[string]any{
			"name":   i.Name,
			"ref":    i.Ref,
			"edge":   i.Edge,
			"facing": i.FacingOf(),
		}
		if i.Ref == "" {
			entry["refNote"] = "无 ref —— 连接器规则对它只能走启发式(INFO 档)"
		}
		if i.PlugWidthMM > 0 {
			entry["plugWidthMm"] = i.PlugWidthMM
		}
		out = append(out, entry)
	}
	return out
}

func issueCounts(issues []spec.Issue) map[string]int {
	c := map[string]int{"ERROR": 0, "WARN": 0, "INFO": 0}
	for _, i := range issues {
		c[i.Level]++
	}
	return c
}

func renderSpecIssues(path string, issues []spec.Issue, w io.Writer) {
	counts := issueCounts(issues)
	if len(issues) == 0 {
		fmt.Fprintf(w, "✅ %s — 无问题\n", path)
		return
	}
	fmt.Fprintf(w, "\n%s\n%s\n", path, strings.Repeat("─", 72))
	for _, i := range issues {
		icon := "ℹ️ "
		switch i.Level {
		case "ERROR":
			icon = "❌"
		case "WARN":
			icon = "⚠️ "
		}
		field := i.Field
		if field == "" {
			field = "(root)"
		}
		fmt.Fprintf(w, "%s %-28s %s\n", icon, field, i.Message)
	}
	fmt.Fprintf(w, "\n%d error / %d warn / %d info\n\n", counts["ERROR"], counts["WARN"], counts["INFO"])
}
