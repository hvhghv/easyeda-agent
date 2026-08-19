package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── 缺陷 5 定案(2026-08-19 真机 E2E 报告 §六.5):`sch note` 文本"未转义"────
//
// 审计日志(~/.easyeda-agent/audit/2026-08-19.jsonl)取证结论:含 `~` / `+/-`
// 的说明失败**不是转义问题** —— 失败载荷的 JS 完全合法且已执行到
// eda.sch_PrimitiveText.create,errorDetail 是 "text create returned undefined",
// 且**同一段文本**在别的时刻创建成功(字符集差为空)。真因是平台偶发吞创建
// 请求(与 zone-draw 的 "平台偶发吞创建请求,重试通常就成" 同一病),修法是
// RunE 里的 settle+重试。本测试把「json.Marshal 进 JS 字面量对 ~ + - " % 换行
// 反引号全都安全」这一事实钉死,防止将来有人"修转义"时改坏。
func TestBuildSchNoteJSEscapesSpecialText(t *testing.T) {
	text := "限流(~1.7mA)\n丝印标 +/- 极性;效率>90%,\"引号\"与`反引号`都安全"
	js := buildSchNoteJS(850, 250, text, "#5A5A5A", 10)

	// 生成的代码里必须出现**恰好一个**合法 JSON/JS 字符串字面量承载全文 ——
	// 从 create( 的第三参位置截取并用 json.Unmarshal 反解,必须还原出原文。
	start := strings.Index(js, `.create(850, 250, `)
	if start < 0 {
		t.Fatalf("create 调用缺失或坐标未按 %%g 渲染:\n%s", js)
	}
	rest := js[start+len(`.create(850, 250, `):]
	end := strings.Index(rest, `, 0, "#5A5A5A"`)
	if end < 0 {
		t.Fatalf("找不到内容字面量的右边界:\n%s", js)
	}
	lit := rest[:end]
	var back string
	if err := json.Unmarshal([]byte(lit), &back); err != nil {
		t.Fatalf("内容字面量不是合法 JSON 字符串(JS 会语法错误): %v\nlit=%s", err, lit)
	}
	if back != text {
		t.Fatalf("往返丢真:\n want %q\n got  %q", text, back)
	}
	// 裸换行绝不许直接出现在字面量里(JS 字符串里裸换行 = 语法错误)。
	if strings.Contains(lit, "\n") {
		t.Fatalf("字面量含裸换行: %q", lit)
	}
	// 未转义的裸引号会截断字符串 —— 反解已证不了截断,这里再钉一条可读断言。
	if !strings.Contains(lit, `\"`) {
		t.Fatalf("引号未被转义: %q", lit)
	}
}
