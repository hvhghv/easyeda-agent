const SOCKET_ID_PREFIX = 'easyeda-agent-';

function randomActivationToken(): string {
	if (typeof globalThis.crypto?.randomUUID === 'function') {
		return globalThis.crypto.randomUUID();
	}
	if (typeof globalThis.crypto?.getRandomValues === 'function') {
		const values = new Uint32Array(4);
		globalThis.crypto.getRandomValues(values);
		return Array.from(values, (value) => value.toString(16).padStart(8, '0')).join('');
	}
	return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

/** Create an activation-scoped id for EasyEDA's host-managed WebSocket table. */
export function createWebSocketId(randomToken = randomActivationToken): string {
	return `${SOCKET_ID_PREFIX}${randomToken()}`;
}
