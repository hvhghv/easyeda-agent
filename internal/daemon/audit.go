package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

// auditEntry is one JSONL row appended per dispatched action. The shape is
// intentionally flat so future `easyeda audit` tooling can pipe it through
// jq/grep without parsing nested envelopes.
type auditEntry struct {
	Timestamp time.Time `json:"ts"`
	RequestID string    `json:"requestId"`
	WindowID  string    `json:"windowId,omitempty"`
	// ClientID attributes the entry to the calling client process
	// ("<hostname>:<pid>[:<label>]", see protocol.Request.ClientID) so
	// multi-client incidents are attributable from the audit log alone
	// (issue #108). Empty for callers that sent no identity.
	ClientID   string         `json:"clientId,omitempty"`
	Action     string         `json:"action"`
	Payload    map[string]any `json:"payload,omitempty"`
	OK         bool           `json:"ok"`
	DurationMs int64          `json:"durationMs"`
	Result     map[string]any `json:"result,omitempty"`
	ErrorCode  string         `json:"errorCode,omitempty"`
	ErrorMsg   string         `json:"errorMsg,omitempty"`
	// ErrorDetail carries the connector's raw platform error (protocol
	// Error.Detail — e.g. what eda.* actually threw), which our handlers wrap
	// behind a generic message. Without it the log only ever shows our own
	// wrapper ("Failed to modify schematic page title block.") and post-hoc
	// root-causing is impossible: the titleblock.modify 0/32 investigation had
	// to reconstruct the cause from the payload alone.
	ErrorDetail string `json:"errorDetail,omitempty"`
}

// EnvAuditDir overrides the audit log root, mirroring EASYEDA_WORKFLOW_DIR
// (internal/workflow). Set it to keep a run's audit trail out of the user's
// real log — the daemon's own tests rely on this.
const EnvAuditDir = "EASYEDA_AUDIT_DIR"

// auditWriter serializes appends to one JSONL file per UTC day so log files
// stay rotatable by date without a separate rotation process.
type auditWriter struct {
	mu  sync.Mutex
	dir string
	// disabled writers drop every entry. Only the in-test default fallback
	// sets this — see newAuditWriter.
	disabled bool
}

// newAuditWriter resolves the audit root: the explicit dir, else
// EASYEDA_AUDIT_DIR, else ~/.easyeda-agent/audit.
//
// The last fallback is DISABLED under `go test` (issue #159). The audit log is
// a first-class output that `easyeda audit` / audit-baseline.py read to judge
// which actions are failing in the field; a test that constructs a Server
// without an AuditDir used to append its fixtures — fake windows "w1"/"w2",
// project "motobox" — straight into the user's real log, manufacturing failure
// signals that a later investigation then chases (33 rows had accumulated).
// Tests that actually exercise the writer pass an explicit t.TempDir().
func newAuditWriter(dir string) *auditWriter {
	if dir == "" {
		dir = os.Getenv(EnvAuditDir)
	}
	if dir == "" {
		if testing.Testing() {
			return &auditWriter{disabled: true}
		}
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = os.Getenv("HOME")
		}
		dir = filepath.Join(home, ".easyeda-agent", "audit")
	}
	return &auditWriter{dir: dir}
}

// Append writes one entry. Failures are best-effort: audit-log errors must
// never break the dispatch path.
func (a *auditWriter) Append(entry auditEntry) {
	if a == nil || a.disabled {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(a.dir, 0o755); err != nil {
		return
	}
	day := entry.Timestamp.UTC().Format("2006-01-02")
	path := filepath.Join(a.dir, day+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(entry)
}

// Path returns the audit log file path for a given day.
func (a *auditWriter) Path(day time.Time) string {
	return filepath.Join(a.dir, day.UTC().Format("2006-01-02")+".jsonl")
}

// Dir returns the audit directory the writer is using.
func (a *auditWriter) Dir() string { return a.dir }

// fromResponse builds an auditEntry from the dispatched request and the
// connector's response (or daemon-local error). started is the wall-clock
// at which the daemon accepted the action.
func fromResponse(started time.Time, req *protocol.Request, resp *protocol.Response) auditEntry {
	e := auditEntry{
		Timestamp:  started,
		RequestID:  req.ID,
		WindowID:   req.WindowID,
		ClientID:   req.ClientID,
		Action:     req.Action,
		Payload:    req.Payload,
		DurationMs: time.Since(started).Milliseconds(),
	}
	if resp != nil {
		e.OK = resp.OK
		e.Result = resp.Result
		if resp.Error != nil {
			e.ErrorCode = resp.Error.Code
			e.ErrorMsg = resp.Error.Message
			e.ErrorDetail = resp.Error.Detail
		}
	}
	return e
}
