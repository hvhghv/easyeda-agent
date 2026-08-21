package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// Tests for the rule-5 stale-read GATE: the daemon refuses a PCB read that
// arrives after a PCB mutation with no `doc reload` in between (previously a
// non-blocking advisory, ignored 1780 times / 18.1% over a 49-day audit window).
//
// Self-contained on purpose: local helpers only, no reuse of the advisory-era
// fixtures in stalereads_test.go.

// gateReq builds a request for the gate under test.
func gateReq(action, windowID, project string) *protocol.Request {
	r := &protocol.Request{
		Envelope: protocol.Envelope{ID: "req_gate", WindowID: windowID},
		Action:   action,
		Project:  project,
	}
	return r
}

// gateServer builds a Server whose audit trail lands in a temp dir, so a test
// can read back what the gate recorded. An explicit AuditDir is required: the
// in-test default writer is disabled (audit.go, issue #159).
func gateServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	return New(Options{AuditDir: dir}), dir
}

// gateMark drives the state machine the way a completed action would: a
// successful mutation arms the window.
func gateMark(s *Server, action, windowID string, payload map[string]any) {
	req := &protocol.Request{
		Envelope: protocol.Envelope{WindowID: windowID},
		Action:   action,
		Payload:  payload,
	}
	s.staleReads.observe(req, &protocol.Response{OK: true})
}

// gateAuditRows reads every audit row written to dir (all days).
func gateAuditRows(t *testing.T, dir string) []auditEntry {
	t.Helper()
	var out []auditEntry
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("open audit: %v", err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var row auditEntry
			if json.Unmarshal(sc.Bytes(), &row) == nil {
				out = append(out, row)
			}
		}
		f.Close()
	}
	return out
}

// TestStaleReadGate_RefusesAndNamesTheNextStep is the core contract: a PCB read
// on a mutated-but-unreloaded window is REFUSED, and the refusal carries a
// runnable command rather than a diagnosis.
func TestStaleReadGate_RefusesAndNamesTheNextStep(t *testing.T) {
	s, _ := gateServer(t)
	gateMark(s, "pcb.line.create", "w1", nil)

	resp := s.checkStaleRead(gateReq("pcb.components.list", "w1", ""))
	if resp == nil {
		t.Fatal("a PCB read after pcb.line.create with no reload must be REFUSED, not annotated")
	}
	if resp.OK {
		t.Fatal("refusal must be ok=false")
	}
	if resp.Error == nil || resp.Error.Code != "STALE_READ" {
		t.Fatalf("want error code STALE_READ, got %+v", resp.Error)
	}
	msg := resp.Error.Message
	for _, want := range []string{"pcb.components.list", "pcb.line.create", "easyeda doc reload", "--force-reason"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message must contain %q, got:\n%s", want, msg)
		}
	}
	// 判据必须给能执行的下一步 — the message, not just the detail, because the CLI
	// surfaces only error.message (internal/app/dispatch.go requestActionTimed).
	if !strings.Contains(msg, "下一步:") {
		t.Errorf("refusal message must spell out 下一步, got:\n%s", msg)
	}
	if resp.Error.Detail == "" || !strings.Contains(resp.Error.Detail, "easyeda doc reload") {
		t.Errorf("detail should repeat the command for raw/JSON callers, got %q", resp.Error.Detail)
	}
}

// TestStaleReadGate_NextStepIsCopyPasteable: when the daemon knows the project,
// the printed command carries it — a template placeholder is one more thing the
// caller has to resolve before it can act.
func TestStaleReadGate_NextStepIsCopyPasteable(t *testing.T) {
	s, _ := gateServer(t)
	gateMark(s, "pcb.route.rip_up", "w1", nil)

	resp := s.checkStaleRead(gateReq("pcb.drc.check", "w1", "ceshi"))
	if resp == nil {
		t.Fatal("want refusal")
	}
	if !strings.Contains(resp.Error.Message, "easyeda doc reload --project ceshi") {
		t.Errorf("want the resolved project in the command, got:\n%s", resp.Error.Message)
	}

	// With no identity at all the command degrades to a placeholder, never to a
	// command with a dangling flag.
	s2, _ := gateServer(t)
	gateMark(s2, "pcb.route.rip_up", "w9", nil)
	resp2 := s2.checkStaleRead(gateReq("pcb.drc.check", "w9", ""))
	if !strings.Contains(resp2.Error.Message, "easyeda doc reload --project <name>") {
		t.Errorf("want the <name> placeholder, got:\n%s", resp2.Error.Message)
	}
}

// TestStaleReadGate_ForceReasonBypassesAndIsAudited: the escape hatch works and
// leaves a trace. A bypass with no trace is indistinguishable from a gate that
// never fired (same discipline as the stage gate's verdict.Audited).
func TestStaleReadGate_ForceReasonBypassesAndIsAudited(t *testing.T) {
	s, dir := gateServer(t)
	gateMark(s, "pcb.pour.create", "w1", nil)

	req := gateReq("pcb.pour.list", "w1", "ceshi")
	req.ForceReason = "对比 reload 前后的差异"
	req.ClientID = "host:123:test"
	if resp := s.checkStaleRead(req); resp != nil {
		t.Fatalf("forceReason must let the read through, got %+v", resp.Error)
	}

	rows := gateAuditRows(t, dir)
	var forced *auditEntry
	for i := range rows {
		if rows[i].Action == "daemon.stale_read.force" {
			forced = &rows[i]
		}
	}
	if forced == nil {
		t.Fatalf("the bypass must be audited, got rows %+v", rows)
	}
	if forced.Result["reason"] != "对比 reload 前后的差异" {
		t.Errorf("audit row must record the reason, got %+v", forced.Result)
	}
	if forced.Result["read"] != "pcb.pour.list" || forced.Result["mutation"] != "pcb.pour.create" {
		t.Errorf("audit row must name read + mutation, got %+v", forced.Result)
	}
	if forced.ClientID != "host:123:test" {
		t.Errorf("audit row must attribute the client, got %q", forced.ClientID)
	}
	// The pseudo-action name must never collide with a real catalog action, or
	// it would corrupt per-action call statistics.
	if knownActions["daemon.stale_read.force"] {
		t.Error("the audit pseudo-action must not be a real catalog action")
	}

	// Forcing authorizes THIS request only: the window is still dirty.
	if resp := s.checkStaleRead(gateReq("pcb.pour.list", "w1", "ceshi")); resp == nil {
		t.Error("force must not clear the stale mark for later un-forced reads")
	}
}

// TestStaleReadGate_ReloadAndPourRebuildOpenIt pins the two CLEAR transitions.
func TestStaleReadGate_ReloadAndPourRebuildOpenIt(t *testing.T) {
	s, _ := gateServer(t)
	gateMark(s, "pcb.via.create", "w1", nil)
	gateMark(s, "debug.exec_js", "w1", map[string]any{
		"code": `return await eda.dmt_EditorControl.closeDocument("tab-1")`,
	})
	if resp := s.checkStaleRead(gateReq("pcb.line.list", "w1", "")); resp != nil {
		t.Fatalf("a read after `doc reload` must be allowed, got %+v", resp.Error)
	}

	s2, _ := gateServer(t)
	gateMark(s2, "pcb.route.delete", "w1", nil)
	gateMark(s2, "pcb.pour.rebuild", "w1", nil)
	if resp := s2.checkStaleRead(gateReq("pcb.nets.list", "w1", "")); resp != nil {
		t.Fatalf("a read after pour-rebuild must be allowed, got %+v", resp.Error)
	}
}

// TestStaleReadGate_DocReloadItselfIsNeverBlocked is the deadlock check: the ONE
// remedy the refusal prescribes must be runnable on a refusing window. `doc
// reload` is a CLI composite of document.current → document.open → pcb.save →
// debug.exec_js(closeDocument) → document.open → document.current
// (internal/app/cmd_doc.go reloadDocumentByUUID); if any step were classified as
// a PCB read the gate would wedge the board permanently.
func TestStaleReadGate_DocReloadItselfIsNeverBlocked(t *testing.T) {
	s, _ := gateServer(t)
	gateMark(s, "pcb.clear_routing", "w1", nil)

	for _, step := range []string{"document.current", "document.open", "pcb.save", "debug.exec_js"} {
		if resp := s.checkStaleRead(gateReq(step, "w1", "ceshi")); resp != nil {
			t.Fatalf("`doc reload` step %s must never be gated (would deadlock the remedy), got %+v", step, resp.Error)
		}
	}
}

// TestStaleReadGate_ExemptMutationsNeverArmIt covers the whole no-mark set: the
// three scarred exemptions (#112) plus the view-only actions added with the
// refusal.
func TestStaleReadGate_ExemptMutationsNeverArmIt(t *testing.T) {
	cases := []struct {
		action  string
		payload map[string]any
	}{
		{"pcb.save", nil},
		{"pcb.pour.rebuild", nil},
		{"pcb.page.clear", map[string]any{"dryRun": true}},
		{"pcb.view.side", map[string]any{"side": "bottom"}},
		{"pcb.layers.set_current", map[string]any{"layer": 2}},
		{"pcb.layers.visibility", map[string]any{"layer": 2, "visible": false}},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			s, _ := gateServer(t)
			gateMark(s, tc.action, "w1", tc.payload)
			if resp := s.checkStaleRead(gateReq("pcb.components.list", "w1", "ceshi")); resp != nil {
				t.Fatalf("%s changes no primitive — a later read must NOT be refused, got:\n%s",
					tc.action, resp.Error.Message)
			}
		})
	}
}

// TestStaleReadGate_ViewOnlyCatalogVerifyStepStaysRunnable: the action catalog
// itself prescribes reading right after a view-only change (VerifyWith). A gate
// that refuses the catalog's own verification step is wrong by construction.
func TestStaleReadGate_ViewOnlyCatalogVerifyStepStaysRunnable(t *testing.T) {
	verify := map[string][]string{}
	for _, a := range protocol.AllActions() {
		verify[a.Name] = a.VerifyWith
	}
	for action := range staleViewOnlyActions {
		s, _ := gateServer(t)
		gateMark(s, action, "w1", nil)
		for _, v := range verify[action] {
			if resp := s.checkStaleRead(gateReq(v, "w1", "ceshi")); resp != nil {
				t.Errorf("%s declares VerifyWith=%s; the gate must not refuse it, got:\n%s",
					action, v, resp.Error.Message)
			}
		}
	}
}

// TestStaleReadGate_SnapshotStaysAdvisory: a snapshot is a picture of the live
// canvas, and when a snapshot IS wrong the fix is foregrounding the window, not
// `doc reload` — so refusing it would prescribe a next step that does not work.
// It keeps the advisory instead.
func TestStaleReadGate_SnapshotStaysAdvisory(t *testing.T) {
	s, _ := gateServer(t)
	gateMark(s, "pcb.line.create", "w1", nil)

	if resp := s.checkStaleRead(gateReq("pcb.snapshot", "w1", "ceshi")); resp != nil {
		t.Fatalf("pcb.snapshot must stay readable on a dirty window, got %+v", resp.Error)
	}
	// …but it must still be told.
	out := &protocol.Response{OK: true}
	s.staleReads.observe(gateReq("pcb.snapshot", "w1", "ceshi"), out)
	if out.StaleRisk == "" {
		t.Error("pcb.snapshot must still carry the staleRisk advisory")
	}
}

// TestStaleReadGate_ScopeIsPcbReadsOnly: mutations, other domains and other
// windows are untouched.
func TestStaleReadGate_ScopeIsPcbReadsOnly(t *testing.T) {
	s, _ := gateServer(t)
	gateMark(s, "pcb.line.create", "w1", nil)

	// A follow-up MUTATION is about to change the board anyway.
	if resp := s.checkStaleRead(gateReq("pcb.route.rip_up", "w1", "")); resp != nil {
		t.Error("mutating actions must not be gated as stale reads")
	}
	// Schematic reads have their own engine.
	if resp := s.checkStaleRead(gateReq("schematic.components.list", "w1", "")); resp != nil {
		t.Error("schematic reads must not be gated by a PCB mutation")
	}
	// Another window is clean.
	if resp := s.checkStaleRead(gateReq("pcb.components.list", "w2", "")); resp != nil {
		t.Error("a mutation on w1 must not gate reads on w2")
	}
	// A clean window is clean.
	s2, _ := gateServer(t)
	if resp := s2.checkStaleRead(gateReq("pcb.components.list", "w1", "")); resp != nil {
		t.Error("an untouched window must never refuse a read")
	}
}

// TestStaleReadGate_FailedMutationDoesNotArmIt: only a successful mutation can
// dirty the engine state.
func TestStaleReadGate_FailedMutationDoesNotArmIt(t *testing.T) {
	s, _ := gateServer(t)
	req := &protocol.Request{Envelope: protocol.Envelope{WindowID: "w1"}, Action: "pcb.line.create"}
	s.staleReads.observe(req, &protocol.Response{OK: false})

	if resp := s.checkStaleRead(gateReq("pcb.line.list", "w1", "")); resp != nil {
		t.Fatalf("a failed mutation must not refuse later reads, got %+v", resp.Error)
	}
}

// ── STAGE_BLOCKED: the refusal names the repair command ────────────────────

func TestStageBlockedMessageCarriesTheRepairCommand(t *testing.T) {
	t.Setenv(workflow.EnvDir, t.TempDir())
	s, _ := gateServer(t)

	req := &protocol.Request{Action: "pcb.line.create", Project: "gate-proj"}
	req.ID = "req_stage"

	resp := s.checkStageGate(req)
	if resp == nil || resp.Error == nil || resp.Error.Code != "STAGE_BLOCKED" {
		t.Fatalf("want STAGE_BLOCKED on a fresh project, got %+v", resp)
	}
	msg := resp.Error.Message
	if !strings.Contains(msg, "下一步:") {
		t.Errorf("STAGE_BLOCKED must spell out 下一步, got:\n%s", msg)
	}
	// Zero confirmations = the #132 hard tier: the first real step is the
	// placement tier ladder, which is NOT in verdict.Missing.
	if !strings.Contains(msg, "confirm-tier") || !strings.Contains(msg, "confirm-layout") {
		t.Errorf("an unconfirmed skeleton must point at the tier ladder, got:\n%s", msg)
	}
	if !strings.Contains(msg, "--project gate-proj") {
		t.Errorf("the command must carry the resolved project, got:\n%s", msg)
	}
	// The old hint only pointed at a status command; it must no longer be the
	// only thing the caller gets.
	if strings.TrimSpace(resp.Error.Detail) == "see `easyeda workflow status` for the project's stage state" {
		t.Error("detail must be the repair command, not just a pointer to status")
	}
}

func TestStageNextStepPicksTheEarliestUnmetGate(t *testing.T) {
	t.Setenv(workflow.EnvDir, t.TempDir())

	// Skeleton partly confirmed (placement signed off) → the earliest entry of
	// Missing wins: outline before pre-route.
	st, _ := workflow.Load("next-proj")
	st.Confirm(workflow.StagePlacementConfirmed, "confirm", "")
	next := stageNextStep(st, []string{"pre_route_passed", "outline_confirmed"}, "ceshi")
	if !strings.Contains(next, "confirm-outline") {
		t.Errorf("outline_confirmed outranks pre_route_passed, got %q", next)
	}
	if strings.Contains(next, "layout-lint") {
		t.Errorf("must name ONE next step, got %q", next)
	}

	// Outline done too → the lint gate is what is left.
	st.Confirm(workflow.StageOutlineConfirmed, "confirm", "")
	next = stageNextStep(st, []string{"pre_route_passed"}, "ceshi")
	if !strings.Contains(next, "easyeda pcb layout-lint --gate --project ceshi") {
		t.Errorf("want the lint gate command, got %q", next)
	}

	// No project known → no dangling --project flag.
	next = stageNextStep(st, []string{"pre_route_passed"}, "")
	if strings.Contains(next, "--project") {
		t.Errorf("an unknown project must not emit a bare --project, got %q", next)
	}
}

func TestStageCommandForCoversEveryGateStage(t *testing.T) {
	// Every stage a gate can report missing must map to a real command, never to
	// the generic status fallback.
	for _, stg := range []workflow.Stage{
		workflow.StagePlacementConfirmed,
		workflow.StageOutlineConfirmed,
		workflow.StagePreRoutePassed,
		workflow.StagePostRouteChecked,
	} {
		cmd := stageCommandFor(stg, " --project ceshi")
		if !strings.HasPrefix(cmd, "easyeda ") {
			t.Errorf("%s: command must start with `easyeda`, got %q", stg, cmd)
		}
		if strings.Contains(cmd, "workflow status") {
			t.Errorf("%s fell through to the status fallback: %q", stg, cmd)
		}
		if !strings.Contains(cmd, "--project ceshi") {
			t.Errorf("%s: command must carry the project, got %q", stg, cmd)
		}
	}
}

// TestStaleReadRefusalIsTimestampFree keeps the message stable so a CLI can
// deduplicate it within one composite command (same rationale as the advisory).
func TestStaleReadRefusalIsTimestampFree(t *testing.T) {
	a, _ := staleReadRefusal("pcb.line.list", "pcb.via.create", "ceshi")
	time.Sleep(2 * time.Millisecond)
	b, _ := staleReadRefusal("pcb.line.list", "pcb.via.create", "ceshi")
	if a != b {
		t.Errorf("refusal text must be deterministic:\n%s\nvs\n%s", a, b)
	}
}
