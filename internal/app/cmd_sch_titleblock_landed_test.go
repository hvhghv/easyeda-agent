package app

import "testing"

// 复核的风险是**把真失败洗成成功**,所以判据必须只看**用户请求的项**:
// 整包回传里的全量明细项、以及我们主动按住的结构开关都不参与 —— 否则
// 「结构开关本来就对」会被当成写入成功的证据。
//
// 不再依赖连接器的错误文案(dispatchCapture 在 ok=false 时只回通用
// errActionFailed,消息早被丢在 stdout 里了)——任何失败都回读复验,
// 连不上时回读同样失败,判定自然保持失败。

func TestTBRequestedKeys_OnlyUserPatch(t *testing.T) {
	// 判成败只看用户请求的项 —— 整包回传里的全量明细项与我们按住的结构开关
	// (Title Block / Border)都不参与。
	got := tbRequestedKeys(map[string]any{"Name": "X", "Drawed": "Y"})
	if len(got) != 2 || got[0] != "Drawed" || got[1] != "Name" {
		t.Fatalf("got %v, want [Drawed Name](升序,且不含结构开关)", got)
	}
	if len(tbRequestedKeys(nil)) != 0 {
		t.Error("空 patch 该给空列表")
	}
}
