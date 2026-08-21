package protocol

import "time"

type Envelope struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Version   string    `json:"version"`
	WindowID  string    `json:"windowId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type Request struct {
	Envelope
	Action string `json:"action"`
	// Project is an optional stable routing hint: a project name or uuid the
	// daemon resolves to the current windowId. Use instead of WindowID when the
	// ephemeral windowId churns (reconnects) — multi-window/multi-agent routing.
	Project string `json:"project,omitempty"`
	// OutputDir is the CLI's working directory. The daemon (which has its own,
	// different cwd) writes artifacts under <OutputDir>/.easyeda/artifacts so
	// screenshots/exports land in the user's project, not the daemon's. Empty for
	// callers that don't set it (the daemon then falls back to its ArtifactDir).
	OutputDir string `json:"outputDir,omitempty"`
	// ForceReason explicitly overrides a workflow stage gate for THIS request
	// only (e.g. routing actions before outline_confirmed + pre_route_passed).
	// The daemon records it in the project's stage history so the bypass is
	// auditable, never silent. Empty = no override. The override is TIERED
	// (issue #132): plain force only bypasses soft gaps — when the mechanical
	// skeleton is entirely unconfirmed (neither placement_confirmed nor
	// outline_confirmed) it is refused unless ForceUnsafe is also set.
	ForceReason string `json:"forceReason,omitempty"`
	// ForceUnsafe escalates ForceReason to bypass EVERYTHING, including a
	// zero-confirmation board — the deliberate, higher-friction escape hatch
	// (`--force-unsafe <reason>`). Meaningless without ForceReason.
	ForceUnsafe bool `json:"forceUnsafe,omitempty"`
	// TimeoutMs is the caller's round-trip budget. The daemon shortens its own
	// connector wait to (TimeoutMs - grace) so the caller receives a structured
	// DISPATCH_FAILED instead of a raw HTTP timeout when the connector hangs
	// (e.g. DRC recompute on a background window never finishes). 0 = daemon
	// default.
	TimeoutMs int `json:"timeoutMs,omitempty"`
	// ClientID identifies the calling client process for audit attribution and
	// the concurrent-writer advisory (issue #108): multiple CLIs/agents can
	// drive the same board through one daemon, and without an identity field
	// the audit log cannot say WHO replayed a stale plan. The CLI fills it once
	// per process as "<hostname>:<pid>[:<EASYEDA_CLIENT_LABEL>]". Optional —
	// raw HTTP callers that omit it simply stay unattributed.
	ClientID string `json:"clientId,omitempty"`
	// TransactionID groups every connector action issued by one CLI command.
	// The daemon holds a per-window lease for this id until the command sends
	// system.transaction.release, preventing another command from switching the
	// foreground document between a --doc guard and its protected read/write.
	TransactionID string         `json:"transactionId,omitempty"`
	Payload       map[string]any `json:"payload,omitempty"`
}

type Response struct {
	Envelope
	OK        bool           `json:"ok"`
	Result    map[string]any `json:"result,omitempty"`
	Context   *Context       `json:"context,omitempty"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	Warnings  []string       `json:"warnings,omitempty"`
	Error     *ErrorInfo     `json:"error,omitempty"`
	// StaleRisk is a daemon-attached advisory set on PCB read actions
	// (list/DRC/report …) that arrive after a PCB mutation but before a `doc
	// reload`: the per-document engine state may serve stale data (SKILL iron
	// rule 5). Purely additive — absent when there is no risk.
	//
	// Since the rule-5 gate landed, such a read is normally REFUSED outright with
	// error code STALE_READ (internal/daemon/stalereads.go) rather than answered
	// with this advisory. What still carries it is the residue the gate lets
	// through: the block-exempt reads (pcb.snapshot) and any read that bought its
	// way past with an audited `forceReason`.
	StaleRisk string `json:"staleRisk,omitempty"`
	// ConcurrentWriter is a daemon-attached, non-blocking advisory set on a
	// mutating action when a DIFFERENT client mutated the same window recently
	// (issue #108): two clients driving one board with no mutex silently fight
	// each other. Purely additive — absent when the last writer is the same
	// client, the request is a read, or the last write is old enough to no
	// longer conflict.
	ConcurrentWriter string `json:"concurrentWriter,omitempty"`

	// ── Connector ordering evidence (connector ≥ 1.0.3) ──────────────────
	//
	// The connector runs every action through ONE FIFO queue per window
	// (extension/src/action-queue.ts), so "dispatch W, then dispatch R" now
	// implies "R's handler started after W's handler settled". These three
	// fields are how that becomes ARITHMETIC on this side instead of a guess.
	//
	// They are POINTERS on purpose: a connector older than the FIFO change
	// sends no such fields, and "absent" must never be read as 0 (see
	// internal/app/sch_place_adopt.go — an absent counter falls back to the
	// weak probe heuristic, never to "fresh").
	//
	// WHAT THEY DO NOT PROVE: that the document was committed. `eda.*` may
	// finish its internal write after the handler returns; we have no
	// observation point for that layer. Every conclusion drawn from these
	// fields must stop at the handler boundary.

	// Seq is the number of actions that have settled on this connector's FIFO
	// queue, counted AFTER this action completed ("this action was the Seq-th
	// to complete"). Monotone, never reused, never rolled back — except across
	// a connector reload, which restarts it at 0 (detectable as a decrease).
	Seq *int `json:"seq,omitempty"`
	// SeqAbandoned is the cumulative count of actions the connector gave up
	// waiting for (queue head past its own timeoutMs). Monotone. A change
	// across a window of interest voids every ordering conclusion about that
	// window: an abandoned handler is still running and its effect may land
	// later.
	SeqAbandoned *int `json:"seqAbandoned,omitempty"`
	// Unordered marks a response that took the connector's bypass channel
	// (a short whitelist of pure diagnostic reads, so the editor stays
	// observable while the queue head is wedged). Its Seq is a snapshot, NOT
	// ordering evidence, and must never be used to prove freshness.
	Unordered bool `json:"unordered,omitempty"`
	// AbandonedIDs are the most recent abandoned request ids (bounded ring,
	// ≤32) so a verdict can name names instead of only counting.
	AbandonedIDs []string `json:"abandonedIds,omitempty"`
}

type Context struct {
	ProjectUUID  string `json:"projectUuid,omitempty"`
	ProjectName  string `json:"projectName,omitempty"`
	DocumentUUID string `json:"documentUuid,omitempty"`
	DocumentType string `json:"documentType,omitempty"`
	TabID        string `json:"tabId,omitempty"`
	Unit         string `json:"unit,omitempty"`
}

type Artifact struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	FileName string `json:"fileName,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Size     int64  `json:"size,omitempty"`
	SHA256   string `json:"sha256,omitempty"`

	// InlineBase64 carries the artifact bytes from the connector, which cannot
	// write to the daemon's disk. The daemon decodes it, persists the file, fills
	// Path/Size/SHA256, and clears this field before returning to the caller.
	InlineBase64 string `json:"inlineBase64,omitempty"`
}

type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}
