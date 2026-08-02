import assert from 'node:assert/strict';
import test from 'node:test';

import { createWebSocketId } from './transport-identity';

test('WebSocket ids are activation-scoped and keep the diagnostic prefix', () => {
	const ids = ['activation-a', 'activation-b'].map((id) => createWebSocketId(() => id));
	assert.deepEqual(ids, ['easyeda-agent-activation-a', 'easyeda-agent-activation-b']);
	assert.notEqual(ids[0], ids[1]);
});
