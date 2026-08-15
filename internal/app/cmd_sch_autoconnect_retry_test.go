package app

import (
	"errors"
	"testing"
)

// 重试判据的全部风险在**假阳性**:状态未知的失败被重试,会得到第二条桩线和第二面旗
// (画布上多出一份,而两次调用都"成功"过一次)。所以这里逐条钉住「什么不能重试」。

func TestACConnectPinRetryable_OnlyWhenConnectorProvesItRolledBack(t *testing.T) {
	retryable := []string{
		"schematic.power.connect_pin failed: Netflag create did not settle within 7000ms — " +
			"the platform dropped the request without rejecting (the stuck-at-99% hang). " +
			"Rolling back the stub wire and failing fast so the caller can retry this pin.",
		"Failed to create netflag/netport at wire end (stub wire rolled back).",
	}
	for _, msg := range retryable {
		if !acConnectPinRetryable(errors.New(msg)) {
			t.Errorf("该重试却判成不可重试:%s", msg[:60])
		}
	}

	// 状态未知 / 与画布状态无关的失败一律不重试。
	notRetryable := []string{
		"connector did not respond",                           // 我们没等到回应,对方可能已经建好了
		"Failed to create pin-stub wire (765,660)→(785,660)",  // 桩线本身没建成,但没有回滚声明
		"Pin (385, 809.5) sits OFF the 5-unit schematic grid", // 重试一万次也一样
		"offset must be non-zero (got 0)",                     // 参数错,重试无意义
		"",
	}
	for _, msg := range notRetryable {
		if acConnectPinRetryable(errors.New(msg)) {
			t.Errorf("不该重试却判成可重试:%q", msg)
		}
	}
	if acConnectPinRetryable(nil) {
		t.Error("nil 错误不该触发重试")
	}
}

func TestACConnectPinTimeout_ExceedsConnectorWorstCase(t *testing.T) {
	// 连接器内部最坏路径:wire 7s + 重试间隔 0.25s + wire 重试 7s + netflag 7s
	// = 21.25s。CLI 的预算必须**大于**它,否则 daemon 先放弃、连接器还在跑,
	// 报出来的是我们自己的不耐烦而不是对方的故障(实测 17 次 "connector did
	// not respond" 就是这么来的)。
	const connectorWorstCaseSeconds = 21.25
	if acConnectPinTimeout.Seconds() <= connectorWorstCaseSeconds {
		t.Fatalf("connect_pin 预算 %.0fs 没超过连接器最坏耗时 %.2fs —— 会重现 daemon 先放弃的假失败",
			acConnectPinTimeout.Seconds(), connectorWorstCaseSeconds)
	}
	// 也不该长到离谱:真挂住时要能失败,而不是让用户干等。
	if acConnectPinTimeout.Seconds() > 60 {
		t.Errorf("预算 %.0fs 过长 —— 真卡死时用户要等太久", acConnectPinTimeout.Seconds())
	}
}
