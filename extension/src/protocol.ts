/**
 * Wire protocol shapes shared between the easyeda-agent Go daemon and this
 * connector. Field names MUST match `docs/protocol.md`,
 * `docs/connector-contract.md` and `internal/protocol/envelope.go` exactly.
 */

// Replaced at build time by esbuild `define` with extension.json's version
// (see config/esbuild.common.ts). Falls back for non-esbuild contexts (tsc).
declare const __CONNECTOR_VERSION__: string;
export const CONNECTOR_VERSION =
	typeof __CONNECTOR_VERSION__ === 'undefined' ? '0.0.0-dev' : __CONNECTOR_VERSION__;
export const PROTOCOL_VERSION = 'v1';
export const SERVICE_ID = 'easyeda-agent';
export const CAPABILITIES = ['schematic.v1', 'pcb.v1'];

// ─── Daemon → connector frames ───────────────────────────────────────

export interface HandshakeFrame {
	type: 'handshake';
	service: string;
	version?: string;
}

export interface RequestFrame {
	type: 'request';
	id: string;
	version?: string;
	action: string;
	payload?: Record<string, unknown>;
	windowId?: string;
	/**
	 * 调用方的往返预算(毫秒),由 daemon 下发(protocol.Request.TimeoutMs)。
	 * 连接器用它当**放弃队首**的截止时间 —— 绝不写死常数:`sch check` / DRC
	 * 这类合法长操作能跑 60 秒以上。缺省 → ABANDON_FALLBACK_MS。
	 */
	timeoutMs?: number;
}

export interface PingFrame {
	type: 'ping';
	id?: string;
}

export interface PongFrame {
	type: 'pong';
	id?: string;
}

export type InboundFrame =
	| HandshakeFrame
	| RequestFrame
	| PingFrame
	| PongFrame
	| { type: string; [key: string]: unknown };

// ─── Connector → daemon frames ───────────────────────────────────────

export interface RegisterFrame {
	type: 'register';
	windowId: string;
	connectorVersion: string;
	easyedaVersion: string;
	capabilities: Array<string>;
}

export interface ContextFrame {
	type: 'context';
	windowId: string;
	projectUuid?: string;
	projectName?: string;
	documentUuid?: string;
	documentType?: string;
	tabId?: string;
	unit?: string;
}

export interface ResponseContext {
	projectUuid?: string;
	projectName?: string;
	documentUuid?: string;
	documentType?: string;
	tabId?: string;
	unit?: string;
}

export interface ResponseArtifact {
	id: string;
	kind: string;
	mimeType?: string;
	fileName?: string;
	inlineBase64?: string;
}

export interface ResponseError {
	code: string;
	message: string;
	detail?: string;
}

export interface ResponseFrame {
	type: 'response';
	id: string;
	version: string;
	ok: boolean;
	result?: Record<string, unknown>;
	context?: ResponseContext;
	artifacts?: Array<ResponseArtifact>;
	warnings?: Array<string>;
	error?: ResponseError;

	// ── 顺序证据(v1.0.3+,见 action-queue.ts) ───────────────────────────
	// 每一个 response frame 都带。Go 侧镜像在 internal/protocol/envelope.go,
	// **两侧形状必须一致**。老 daemon 收到多余字段会忽略;新 daemon 收不到这
	// 三个字段时退回弱证据档,绝不默认「新鲜」。

	/**
	 * 已完成的动作数(每个 FIFO handler settle 之后 +1,单调递增、永不回退)。
	 * 响应上带的是「本动作完成之后」的值。**它证明的是 handler 边界的先后,
	 * 不是「文档已提交」**。
	 */
	seq?: number;
	/** 累计被放弃的动作数。变化 = 那段时间的顺序证据作废。 */
	seqAbandoned?: number;
	/** true = 这条响应走了旁路通道,它的 seq 不构成任何顺序证据。 */
	unordered?: boolean;
	/** 最近被放弃的 request id(最多 32 条),供判定点名而不只是数数。 */
	abandonedIds?: Array<string>;
}

// ─── Stable error codes ──────────────────────────────────────────────

export const ErrorCodes = {
	UNKNOWN_ACTION: 'UNKNOWN_ACTION',
	MISSING_PAYLOAD_FIELD: 'MISSING_PAYLOAD_FIELD',
	EDA_API_UNAVAILABLE: 'EDA_API_UNAVAILABLE',
	EDA_CALL_FAILED: 'EDA_CALL_FAILED',
	UNSUPPORTED_CAPABILITY: 'UNSUPPORTED_CAPABILITY',
	UNSUPPORTED_RISKY_OPERATION: 'UNSUPPORTED_RISKY_OPERATION',
	INVALID_STATE: 'INVALID_STATE',
	INTERNAL_ERROR: 'INTERNAL_ERROR',
	/**
	 * 队首超过它自己的 timeoutMs 仍未 settle,被放弃以让队列继续流动。
	 * **它的效果可能稍后才落地** —— 收到这个码 = 关于这次写的任何结论都不成立。
	 */
	ACTION_ABANDONED: 'ACTION_ABANDONED',
	/** 队列积压到上限,这次提交被明确拒绝(而不是无限堆积)。它**没有执行**。 */
	QUEUE_OVERFLOW: 'QUEUE_OVERFLOW',
} as const;

/**
 * Result returned by an action handler. The dispatcher wraps this into a full
 * `ResponseFrame`.
 */
export interface ActionResult {
	result?: Record<string, unknown>;
	artifacts?: Array<ResponseArtifact>;
	warnings?: Array<string>;
	context?: ResponseContext;
}

/**
 * Thrown by handlers to produce a structured error response while preserving
 * the original eda error in `detail`.
 */
export class ActionError extends Error {
	public readonly code: string;
	public readonly detail?: string;

	constructor(code: string, message: string, detail?: string) {
		super(message);
		this.name = 'ActionError';
		this.code = code;
		this.detail = detail;
	}
}
