package app

// settle_read.go — 「写完立刻读」的统一处理。
//
// 这是一类**反复出现**的 bug,不是某一处的疏忽(2026-08-16 一轮里踩了三次):
//
//	图签回读     写图签会让平台重建图签对象 → 首读拿到旧值 → 判「没写进去」
//	图纸自检     同一次写入后 → 首读拿不到 sheet 图元 → 判「图框被写没了」
//	导线快照     刚落完块 → 首读拿到半截列表 → 判「组还在 churning」
//
// 三次的症状各不相同,根因是同一个:**平台的写入不是同步生效的**,而我们的判据
// 默认「读到什么就是什么」。更糟的是失败方向 —— 每次都把「还没稳定」判成
// 「东西不在/没写上」,也就是**把时序问题渲染成设计缺陷**,把人引向危险的修复
// (图纸自检那次的提示是「重新整包回传写回 Border/Title Block」,而那正是唯一
// 真能把图框写坏的操作)。
//
// 所以收成一个入口,并把这条经验写在这里而不是散在三处注释里:
// **任何紧跟写操作的回读,都要经过 settleRead**;一次读不到不构成结论。
//
// 与重试的区别:settleRead 只重复**读**,绝不重发写。写的重试是另一回事
// (幂等性、状态未知的分支,见 acConnectPinWithRetry),两者不能混。

import "time"

// settleSeconds 是回读之间的等待。400ms 是实测值:图签重建与 sheet 图元重建都在
// 这个量级内完成;取更短会退化成"读两次同一个旧值",更长则让每条写命令都变慢。
const settleDelay = 400 * time.Millisecond

// settleAttempts 是默认读几次。**2 就够**:平台要么这一拍好了,要么是真出事了 ——
// 无限重试只会把一个确定的失败拖成一次漫长的等待。
const settleAttempts = 2

// settleRead 反复调用 read 直到它说「这次的结果作数」(ok=true),或者用尽次数。
//
// read 的第二个返回值是**判据自己的满足条件**,不是「调用成功」:
// 例如图签回读里,调用成功但值还是旧的 → ok=false → 值得再读一次。
// 这样调用方不必各自写重试循环,也不会再有人忘记写。
//
// read 的第三个返回值是**这次调用的原始错误**,settleRead 靠它区分「还没稳定」
// 和「重试也没用」。签名里必须有它,而不是另开一个「安全版」入口:两个入口意味着
// 有人会挑错的那个,而挑错的代价正是下面这条 ——
//
// ── 为什么非加不可:STALE_READ ─────────────────────────────────────────
//
// daemon 现在会**拒绝** PCB mutation 之后、doc reload 之前的 PCB 读
// (internal/daemon/stalereads.go,错误码 STALE_READ)。这种拒绝是**确定性**的:
// 门的状态不会因为等 400ms 就变。旧签名看不见错误,只能把它当成「没落地」,重试
// 两次再报「写没落地」—— 正好是本文件开头警告的那种误诊,而且更毒:真因是
// 「你该先 reload」,报出来的却是「你的写入丢了」,把人推向重写/回滚。
//
// 所以 settleRead 认得这个码,并**当场收手**:不睡、不重试,原样把 STALE_READ
// 交回去,让调用方报真因。判据只认 error.code,不做文本匹配。
//
// 目前 PCB 侧的写后回读走的是 requestReadAfterWrite 放行位(stale_read_optin.go),
// 本不该撞上这道门;这里的处理是给「有人漏了放行位」留的诚实失败路径 ——
// 定时炸弹的正确拆法是让它爆得清楚,不是假装它不存在。
//
// 返回最后一次的结果、它是否满足条件、以及最后一次的错误 —— 失败时也把值给出来,
// 让调用方能报出「读到的是什么」而不只是「没读到」。
func settleRead[T any](read func() (T, bool, error)) (T, bool, error) {
	var (
		last    T
		lastErr error
	)
	for attempt := 0; attempt < settleAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(settleDelay)
		}
		v, ok, err := read()
		last, lastErr = v, err
		if ok {
			return v, true, nil
		}
		if isStaleRead(err) {
			// 确定性拒绝:再读一次只会再被拒一次,而每多睡一拍就多一分把真因
			// 埋进"超时/没落地"的机会。
			return v, false, err
		}
	}
	return last, false, lastErr
}
