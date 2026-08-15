package app

import (
	"testing"
	"time"
)

// ruler_consistency_test.go — **同一把尺**的守门测试。
//
// 这个仓库最常复发的一类 bug 不是算错,是**同一个概念被算了两遍**,两处慢慢漂开:
//
//	求解器按器件本体判间距,clusters 按含 marker 的组体积判  → 求解器认为合法的落点当场被门判死
//	连接器按「本次调用改变了什么」判成败,用户关心「内容在不在画布上」 → 假失败
//	status 读分区框的区名标签,check 读自由文本数              → 明明有四条说明却报 0
//
// 这三处的注释里都写了「必须与 X 同一把尺」,但注释拦不住下一次改动。所以把配对
// 关系钉成测试:**谁改了其中一边,这里就会红**。
//
// 加新判据时,如果它复用了别处的阈值或口径,就在这里补一条 —— 这比在两处注释里
// 互相叮嘱可靠得多。

func TestRuler_ClusterGapMatchesSolverGap(t *testing.T) {
	// 生成侧(块布局求解器件间最小间隙)与判定侧(clusters 组间 tight 阈值)
	// 必须是同一个数:求解器按 bslPartGap 摆开,门却按别的数判,布局就永远
	// "刚放好就被判不合格"。
	if bslPartGap != bapObstacleGap {
		t.Fatalf("bslPartGap=%v 与 bapObstacleGap=%v 漂开了 —— 求解器与落地前硬门用的不是同一把尺",
			bslPartGap, bapObstacleGap)
	}
	// gate 的 clusters 关也用它(见 gateClustersStage 的 minGap)。
	if got := bslPartGap; got != 20 {
		t.Errorf("间距基准变成了 %v —— 改它要同时确认 `sch clusters --min-gap` 的默认值与帮助文本", got)
	}
}

func TestRuler_GateDefaultsShared(t *testing.T) {
	// `sch status --gate` 复用 collectSchGate,必须传 gate 自己的默认阈值;
	// 各抄一份字面量的话,某次调参后同一张画布会给出两个判定。
	if gateDefaultMinGap != 2.54 || gateDefaultPinEps != 0 || gateDefaultOverlapEps != 0.5 {
		t.Fatalf("gate 默认阈值被改了(%v/%v/%v)—— 确认 `sch gate` 的 flag 默认值与 `sch status --gate` 的传参都跟着改",
			gateDefaultMinGap, gateDefaultPinEps, gateDefaultOverlapEps)
	}
}

func TestRuler_CircuitNoteCountSingleSource(t *testing.T) {
	// 「电路说明有几条」只有 schCircuitNoteCount 一个口径,check 与 status 都调它。
	// 这几条断言本身平淡,它们的价值在于:任何人想再写一遍这个减法时,会先看到这里。
	if got := schCircuitNoteCount(5, 2); got != 3 {
		t.Errorf("自由文本 5 − 区名标签 2 = %d, want 3", got)
	}
	if got := schCircuitNoteCount(2, 5); got != 0 {
		t.Errorf("标签比文本多时该给 0(不是负数), got %d", got)
	}
	if got := schCircuitNoteCount(0, 0); got != 0 {
		t.Errorf("空页该给 0, got %d", got)
	}
}

func TestRuler_SettleReadBudgetSane(t *testing.T) {
	// 回读稳定窗口:太短退化成"读两次同一个旧值",太长让每条写命令都变慢。
	if settleDelay < 200*time.Millisecond || settleDelay > time.Second {
		t.Errorf("settleDelay=%v 超出合理区间 [200ms,1s]", settleDelay)
	}
	if settleAttempts < 2 {
		t.Errorf("settleAttempts=%d < 2 —— 那就等于没有重试", settleAttempts)
	}
	if settleAttempts > 4 {
		t.Errorf("settleAttempts=%d 过多 —— 确定的失败会被拖成漫长等待", settleAttempts)
	}
}

func TestRuler_ConnectPinBudgetExceedsConnectorWorstCase(t *testing.T) {
	// CLI 的预算必须大于连接器内部最坏路径(wire 7s + 0.25s + wire 重试 7s +
	// netflag 7s = 21.25s),否则 daemon 先放弃、对方还在跑 —— 报出来的是我们
	// 自己的不耐烦。与 cmd_sch_autoconnect_retry_test.go 重复是有意的:
	// 那边测行为,这里登记"尺子"。
	const connectorWorstCase = 21250 * time.Millisecond
	if acConnectPinTimeout <= connectorWorstCase {
		t.Fatalf("connect_pin 预算 %v 没超过连接器最坏耗时 %v", acConnectPinTimeout, connectorWorstCase)
	}
}
