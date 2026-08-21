package app

// conn_seq.go — 连接器顺序证据的**观测侧**(判定侧在 sch_place_adopt.go)。
//
// 连接器(≥1.0.3)把每个窗口的动作串成一条 FIFO 链,并在**每一条**响应上带三个
// 顶层字段:`seq` / `seqAbandoned` / `unordered`(外加便利字段 `abandonedIds`)。
// 有了它们,「先发 W 再发 R」第一次变成可传输的先后关系:
//
//	R 的 handler 开跑时,W 的 handler 已经 settle —— **只要这中间没有动作被放弃**。
//
// 所以判定要的不是某一条响应上的绝对值,而是**两个时刻的差**:W 下发之前观测到的
// seqAbandoned,和 R 的响应上带回来的 seqAbandoned。本文件负责把「之前」那个值记
// 下来 —— 在 postAction 这个所有派发路径共享的咽喉上,免得每个命令自己穿线。
//
// ── 边界(写在这里,免得判定侧越界) ──────────────────────────────────
//
// seq 证明的是 **handler 边界**的先后,不是「文档已提交」。`eda.*` 完全可能在
// handler 返回之后才把改动写进文档模型;那一层我们没有任何观测点。

import (
	"encoding/json"
	"strings"
	"sync"
)

// schSeqCounters 是一次响应上的顺序计数器观测。
//
// Known=false 表示这条响应**没带**这些字段 —— 老连接器(市场装的那份会滞后
// CLI 若干 minor)。它绝不等于 0:把「没带」读成 0 就等于凭空造证据。
type schSeqCounters struct {
	Known        bool
	Seq          int
	SeqAbandoned int
	// Unordered = 这条响应走了连接器的旁路通道(纯诊断读,wedge 期仍可观测)。
	// 它的 Seq 是个快照,**不构成任何顺序证据**。
	Unordered    bool
	AbandonedIDs []string
}

// parseSeqCounters 从一份原始响应体里取出顺序计数器。指针语义在这里落地:
// 字段缺席 → Known=false;字段为 0 → Known=true, Seq=0(两者必须可分)。
func parseSeqCounters(body []byte) schSeqCounters {
	var parsed struct {
		Seq          *int     `json:"seq"`
		SeqAbandoned *int     `json:"seqAbandoned"`
		Unordered    bool     `json:"unordered"`
		AbandonedIDs []string `json:"abandonedIds"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return schSeqCounters{}
	}
	if parsed.Seq == nil || parsed.SeqAbandoned == nil {
		// 半套字段不算数:两个计数器缺一个,算术判定就不成立。
		return schSeqCounters{Unordered: parsed.Unordered}
	}
	return schSeqCounters{
		Known:        true,
		Seq:          *parsed.Seq,
		SeqAbandoned: *parsed.SeqAbandoned,
		Unordered:    parsed.Unordered,
		AbandonedIDs: parsed.AbandonedIDs,
	}
}

// ── 基线记录 ────────────────────────────────────────────────────────────
//
// 进程级、按目标窗口分桶。CLI 是「一次调用一条命令」,一条命令自始至终用同一个
// window 参数,所以桶键在一次判定内必然一致。跨窗口的混用只会让计数器看起来
// 倒退 → 判定退回弱证据档(安全方向),不会造出假证明。

var (
	connSeqMu   sync.Mutex
	connSeqLast = map[string]schSeqCounters{}
)

// connSeqKey 把一次派发归到一个桶。窗口 id 优先;没有就退到 project 提示;
// 都没有就落到默认桶(单窗口场景,绝大多数)。
func connSeqKey(window, project string) string {
	if w := strings.TrimSpace(window); w != "" {
		return "w:" + w
	}
	if p := strings.TrimSpace(project); p != "" {
		return "p:" + p
	}
	return "default"
}

// connSeqObserve 记录一次响应上的计数器,作为后续判定的基线。
//
// **只记 ordered 响应**:旁路响应的 seq 是个与 FIFO 无关的快照,拿它当基线会让
// 「基线时刻」本身变得说不清。少记一条的代价只是判定退回弱证据档(安全方向)。
func connSeqObserve(window, project string, body []byte) {
	c := parseSeqCounters(body)
	if !c.Known || c.Unordered {
		return
	}
	key := connSeqKey(window, project)
	connSeqMu.Lock()
	connSeqLast[key] = c
	connSeqMu.Unlock()
}

// connSeqSnapshot 取出该窗口最近一次观测到的计数器。Known=false = 本进程还没
// 见过任何带顺序证据的响应(老连接器,或这条命令还没发过动作)。
func connSeqSnapshot(window, project string) schSeqCounters {
	key := connSeqKey(window, project)
	connSeqMu.Lock()
	defer connSeqMu.Unlock()
	return connSeqLast[key]
}

// connSeqReset 清空基线 —— 仅供测试,让用例之间互不污染。
func connSeqReset() {
	connSeqMu.Lock()
	connSeqLast = map[string]schSeqCounters{}
	connSeqMu.Unlock()
}
