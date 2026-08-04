/// <reference types="@jlceda/pro-api-types" />
// 导入 ./transport(为 scanOrder)会连带编译整个 transport.ts,它引用全局 `eda`;
// 与 actions.test.ts 同理,靠上面这行加载 ambient 声明。
import assert from 'node:assert/strict';
import test from 'node:test';

import { createWebSocketId } from './transport-identity';

test('WebSocket ids are activation-scoped and keep the diagnostic prefix', () => {
	const ids = ['activation-a', 'activation-b'].map((id) => createWebSocketId(() => id));
	assert.deepEqual(ids, ['easyeda-agent-activation-a', 'easyeda-agent-activation-b']);
	assert.notEqual(ids[0], ids[1]);
});

// ── scanOrder: 上次成功的端口优先 ────────────────────────────────────────
//
// 首次拿到断线期日志(2026-08-04)后才看清:每个死端口都要烧满
// CONNECTION_TIMEOUT_MS,因为 eda.sys_WebSocket.register() 从不报告"连接被拒"。
// 于是整轮扫描是纯粹的固定开销,而重启后的 daemon 几乎总在同一个端口上。
import { scanOrder } from './transport';

test('scanOrder: 无历史时就是原顺序', () => {
	assert.deepEqual(scanOrder(null, 10, 14), [10, 11, 12, 13, 14]);
});

test('scanOrder: 上次成功的端口排到最前,且不重复出现', () => {
	const order = scanOrder(13, 10, 14);
	assert.equal(order[0], 13, '上次成功的端口必须先试 —— 这是把重连从整轮扫描变成一次尝试的关键');
	assert.deepEqual(order, [13, 10, 11, 12, 14]);
	assert.equal(new Set(order).size, order.length, '端口不得重复(重复=白烧一个超时)');
});

test('scanOrder: 范围外的历史值被忽略,不会漏扫或越界', () => {
	assert.deepEqual(scanOrder(99, 10, 12), [10, 11, 12]);
	assert.deepEqual(scanOrder(9, 10, 12), [10, 11, 12]);
});

test('scanOrder: 任何情况下都覆盖整个范围', () => {
	for (const hint of [null, 10, 11, 12, 99]) {
		const order = scanOrder(hint, 10, 12);
		assert.deepEqual([...order].sort((a, b) => a - b), [10, 11, 12], `hint=${hint} 漏扫了端口`);
	}
});
