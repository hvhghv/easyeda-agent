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
// 返回最后一次的结果与它是否满足条件 —— 失败时也把值给出来,让调用方能报出
// 「读到的是什么」而不只是「没读到」。
func settleRead[T any](read func() (T, bool)) (T, bool) {
	var last T
	for attempt := 0; attempt < settleAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(settleDelay)
		}
		v, ok := read()
		last = v
		if ok {
			return v, true
		}
	}
	return last, false
}
