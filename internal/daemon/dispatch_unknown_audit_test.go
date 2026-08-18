package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestUnknownActionIsAudited: an UNKNOWN_ACTION rejection must land in the
// audit log. It used to be the ONE dispatch outcome with no trace at all, so a
// CLI↔daemon catalog drift (`board rebind` shipped in the CLI while the daemon
// never registered board.rebind) was invisible to audit-baseline forensics.
func TestUnknownActionIsAudited(t *testing.T) {
	dir := t.TempDir()
	s := &Server{audit: &auditWriter{dir: dir}}

	body := strings.NewReader(`{"action":"board.not_a_real_action","clientId":"test"}`)
	r := httptest.NewRequest(http.MethodPost, "/action", body)
	w := httptest.NewRecorder()
	s.handleAction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	raw, err := os.ReadFile(s.audit.Path(time.Now()))
	if err != nil {
		t.Fatalf("UNKNOWN_ACTION left no audit entry (the pre-fix blind spot): %v", err)
	}
	var entry struct {
		Action    string `json:"action"`
		OK        bool   `json:"ok"`
		ErrorCode string `json:"errorCode"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)[0]), &entry); err != nil {
		t.Fatalf("decode audit entry: %v", err)
	}
	if entry.Action != "board.not_a_real_action" || entry.OK || entry.ErrorCode != "UNKNOWN_ACTION" {
		t.Errorf("audit entry = %+v, want action=board.not_a_real_action ok=false errorCode=UNKNOWN_ACTION", entry)
	}
}
