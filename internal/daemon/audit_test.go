package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

// TestAudit_EntryCarriesClientID pins the client-attribution field (issue
// #108): a request carrying a ClientID must land in the JSONL audit entry so
// multi-client incidents can be attributed from the log alone.
func TestAudit_EntryCarriesClientID(t *testing.T) {
	req := &protocol.Request{
		Envelope: protocol.Envelope{ID: "req_1", WindowID: "w1"},
		Action:   "pcb.via.create",
		ClientID: "mikas-mbp:12345:e2e-regression",
	}
	resp := &protocol.Response{OK: true}
	started := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	entry := fromResponse(started, req, resp)
	if entry.ClientID != "mikas-mbp:12345:e2e-regression" {
		t.Fatalf("fromResponse clientId = %q, want the request's", entry.ClientID)
	}

	// Round-trip through the writer: the JSONL row must carry clientId.
	w := newAuditWriter(t.TempDir())
	w.Append(entry)

	data, err := os.ReadFile(w.Path(started))
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("parse audit row: %v", err)
	}
	if got := row["clientId"]; got != "mikas-mbp:12345:e2e-regression" {
		t.Errorf("audit row clientId = %v, want mikas-mbp:12345:e2e-regression", got)
	}
	if got := row["action"]; got != "pcb.via.create" {
		t.Errorf("audit row action = %v, want pcb.via.create", got)
	}
}

// TestAudit_NoClientIDOmitted verifies anonymous callers stay unattributed
// rather than getting a fabricated identity.
func TestAudit_NoClientIDOmitted(t *testing.T) {
	req := &protocol.Request{
		Envelope: protocol.Envelope{ID: "req_2"},
		Action:   "pcb.line.list",
	}
	entry := fromResponse(time.Now().UTC(), req, &protocol.Response{OK: true})
	if entry.ClientID != "" {
		t.Fatalf("clientId should be empty for anonymous callers, got %q", entry.ClientID)
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(b, &row); err != nil {
		t.Fatalf("parse row: %v", err)
	}
	if _, present := row["clientId"]; present {
		t.Error("clientId must be omitted (omitempty) when the caller sent none")
	}
}

// TestAudit_TestRunNeverWritesRealLog pins issue #159: a Server built without
// an AuditDir must not append to the user's real ~/.easyeda-agent/audit while
// under `go test`. Fixture rows (fake windows "w1"/"w2", project "motobox")
// had been landing there and were later read back as genuine field failures.
func TestAudit_TestRunNeverWritesRealLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvAuditDir, "")

	w := newAuditWriter("")
	if !w.disabled {
		t.Fatalf("newAuditWriter(\"\") under test = enabled writer at %q, want disabled", w.Dir())
	}
	w.Append(auditEntry{Timestamp: time.Now().UTC(), Action: "schematic.components.list"})

	if _, err := os.Stat(filepath.Join(home, ".easyeda-agent")); !os.IsNotExist(err) {
		t.Fatalf("test run created %s/.easyeda-agent (err=%v), want untouched", home, err)
	}
}

// TestAudit_ServerWithoutAuditDirIsDisabled covers the actual regression path:
// every `New(Options{})` in the daemon tests reaches Append via handleAction.
func TestAudit_ServerWithoutAuditDirIsDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvAuditDir, "")

	s := New(Options{})
	s.audit.Append(auditEntry{Timestamp: time.Now().UTC(), Action: "pcb.via.create", WindowID: "w1"})

	if _, err := os.Stat(filepath.Join(home, ".easyeda-agent")); !os.IsNotExist(err) {
		t.Fatalf("New(Options{}) wrote an audit log under %s, want none", home)
	}
}

// TestAudit_EnvDirOverride pins EASYEDA_AUDIT_DIR (same convention as
// EASYEDA_WORKFLOW_DIR), and that an explicit dir still wins over it.
func TestAudit_EnvDirOverride(t *testing.T) {
	envDir := t.TempDir()
	t.Setenv(EnvAuditDir, envDir)

	w := newAuditWriter("")
	if w.disabled || w.Dir() != envDir {
		t.Fatalf("newAuditWriter(\"\") with %s set = %q (disabled=%v), want %q", EnvAuditDir, w.Dir(), w.disabled, envDir)
	}

	started := time.Date(2026, 8, 6, 4, 8, 49, 0, time.UTC)
	w.Append(auditEntry{Timestamp: started, Action: "schematic.components.list"})
	if _, err := os.ReadFile(w.Path(started)); err != nil {
		t.Fatalf("env-dir writer did not append: %v", err)
	}

	explicit := t.TempDir()
	if got := newAuditWriter(explicit).Dir(); got != explicit {
		t.Fatalf("explicit dir = %q, want %q (must win over %s)", got, explicit, EnvAuditDir)
	}
}
