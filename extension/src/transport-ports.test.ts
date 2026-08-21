/// <reference types="@jlceda/pro-api-types" />
// 导入 ./transport 会连带编译整个 transport.ts,它引用全局 `eda`;
// 与 actions.test.ts 同理,靠上面这行加载 ambient 声明。
//
// 钉死端口 + 退避 —— 这两条是「daemon 重启后秒级重连」的全部机制:
// daemon 只绑 60832 且从不外溢(internal/app/cmd_daemon.go),所以扫 60833-60841
// 是纯空转:register() 从不报「连接被拒」,每个死端口都要烧满 CONNECTION_TIMEOUT_MS。
// 真实日志里那 5 个死端口花了 7 秒,而 `make dev` 每改一次 .go 就要重连一次。
import assert from 'node:assert/strict';
import test from 'node:test';

import { DAEMON_PORT, RESERVED_PORT_END, backoffDelayMs, parsePorts, resolvePorts } from './transport';

// ── 默认:只试一个端口 ──────────────────────────────────────────────────

test('默认就是钉死的 60832,一个端口,绝不横扫', () => {
	assert.equal(DAEMON_PORT, 60832);
	assert.deepEqual(resolvePorts(undefined, null), [60832]);
	// 这条是本次改动的核心:长度必须是 1。长度一旦 >1,每次重连都要为不可能有
	// daemon 的端口烧掉 CONNECTION_TIMEOUT_MS。
	assert.equal(resolvePorts(undefined, null).length, 1, '默认端口表长度必须是 1');
});

test('lastGood 不会把默认表变长,也不会引入别的端口', () => {
	assert.deepEqual(resolvePorts(undefined, 60840), [60832], '历史值不得让我们回去试别的端口');
	assert.deepEqual(resolvePorts(null, 60832), [60832]);
});

test('坏配置一律退回钉死的默认值(重连路径上绝不抛错)', () => {
	for (const bad of [undefined, null, true, false, '', '   ', 'abc', {}, [], 0, -1, 70000, '0', '65536']) {
		assert.deepEqual(resolvePorts(bad, null), [60832], `坏配置 ${JSON.stringify(bad)} 应退回默认`);
	}
});

// ── 逃生口:daemonPorts 覆盖 ────────────────────────────────────────────

test('逃生口:单值 / 列表 / 区间 / 数组都能写', () => {
	assert.deepEqual(parsePorts(60900), [60900]);
	assert.deepEqual(parsePorts('60900'), [60900]);
	assert.deepEqual(parsePorts('60840,60832'), [60840, 60832], '顺序按用户写的来');
	assert.deepEqual(parsePorts(' 60840 , 60832 '), [60840, 60832], '空白要容忍');
	assert.deepEqual(parsePorts('60832-60836'), [60832, 60833, 60834, 60835, 60836]);
	assert.deepEqual(parsePorts([60832, '60900']), [60832, 60900]);
});

test('逃生口能覆盖到我们保留段的全部 10 个口', () => {
	const ports = parsePorts(`${DAEMON_PORT}-${RESERVED_PORT_END}`);
	assert.equal(ports.length, 10);
	assert.equal(ports[0], 60832);
	assert.equal(ports[9], 60841);
});

test('覆盖列表去重、且有硬上限(手滑写个 1-65535 不能把重连变成无尽扫描)', () => {
	assert.deepEqual(parsePorts('60832,60832,60833'), [60832, 60833]);
	const huge = parsePorts('1-65535');
	assert.ok(huge.length <= 12, `覆盖列表必须封顶,实际 ${huge.length}`);
	assert.equal(huge[0], 1);
});

test('多端口覆盖时,上次握手成功的端口排最前且不重复', () => {
	const order = resolvePorts('60832-60836', 60835);
	assert.equal(order[0], 60835, 'lastGood 优先 —— 多端口下这是省掉整轮超时的唯一手段');
	assert.deepEqual(order, [60835, 60832, 60833, 60834, 60836]);
	assert.equal(new Set(order).size, order.length, '端口不得重复(重复=白烧一个超时)');
});

test('覆盖列表里没有 lastGood 时,顺序原样不动', () => {
	assert.deepEqual(resolvePorts('60832-60834', 60900), [60832, 60833, 60834]);
});

// ── 退避 ────────────────────────────────────────────────────────────────

const noJitter = (): number => 0.5; // rand=0.5 → 抖动因子恰好 1

test('退避是指数的:0.5s → 1s → 2s → 4s → 8s', () => {
	const seq = [1, 2, 3, 4, 5].map((n) => backoffDelayMs(n, noJitter));
	assert.deepEqual(seq, [500, 1000, 2000, 4000, 8000]);
});

test('退避封顶在 8s,永不继续翻倍(也永不溢出成 Infinity)', () => {
	for (const n of [6, 10, 40, 1000, 1e9]) {
		assert.equal(backoffDelayMs(n, noJitter), 8000, `第 ${n} 次失败的退避应封顶`);
	}
});

test('首次重试是亚秒级 —— daemon 重启后必须秒级重连', () => {
	// 一次尝试的成本 ≈ REGISTER_DELAY_MS(200ms) + CONNECTION_TIMEOUT_MS(1500ms),
	// 加上第一步退避,重连墙钟应当稳稳在 3s 以内。
	const firstRetry = backoffDelayMs(1, () => 1); // 抖动取最大
	assert.ok(firstRetry <= 1000, `首次重试 ${firstRetry}ms 太慢`);
	assert.ok(200 + 1500 + firstRetry < 3000, '「秒级重连」的验收目标不成立了');
});

test('抖动有界(±25%),且不会退化成 0 或负数', () => {
	for (const r of [0, 0.25, 0.5, 0.75, 0.999]) {
		const d = backoffDelayMs(1, () => r);
		assert.ok(d >= 375 && d <= 625, `抖动越界: ${d}`);
	}
	assert.ok(backoffDelayMs(0, noJitter) > 0, '失败次数为 0/负数时不得算出 0 延迟');
	assert.equal(backoffDelayMs(-5, noJitter), 500);
});
