package daemon

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

// Daemon-level stale-read GATE (SKILL iron rule 5 made mechanical).
//
// After a PCB mutation (rip-up / route / delete / via / track / pour edits) the
// per-document engine state serves STALE data to list/DRC reads until the
// document is reloaded (`easyeda doc reload`) — observed repeatedly on real
// boards. Until now this was enforced only by the agent remembering rule 5,
// helped along by a non-blocking `staleRisk` advisory this guard attached to the
// response.
//
// ── Why the advisory became a REFUSAL ─────────────────────────────────────
//
// A 49-day audit review (171554 action records under ~/.easyeda-agent/audit)
// measured how often each iron rule is actually obeyed, and rule 5 was the only
// one bleeding: **1780 stale PCB reads, an 18.1% violation rate**. Every one of
// those reads got the advisory on stderr and read on anyway. The rules that
// never leak (the 14-stage gate, the page/zone gate) are the ones a machine
// REFUSES, not the ones the prose asks nicely for — so this guard now refuses
// too. The advisory is still attached (see observe), but a bare stale read no
// longer returns data: it returns a `STALE_READ` error naming the mutation that
// dirtied the window and the exact command that fixes it.
//
// The refusal lives at the dispatch choke point (checkStaleRead, called from
// handleAction next to the stage gate), so a raw /action caller can't slip past
// the CLI. `forceReason` is the audited escape hatch.
//
// State machine (per windowId, in-memory):
//   SET    — a PCB-domain action with Mutates=true succeeds (catalog-driven,
//            same source of truth as autosave), except the exempt sets below.
//   CLEAR  — a `doc reload` completes. Reload is a CLI composite (save → close
//            via debug.exec_js closeDocument → reopen), so the daemon keys on
//            its unique discriminator: a successful debug.exec_js whose code
//            calls closeDocument (a real close resets the per-doc engine
//            state; a mere `doc switch`/document.open does NOT and must not
//            clear). pcb.pour.rebuild also clears — it recomputes the pour
//            connectivity that goes stale (pour-mediated Connection Errors).
//   BLOCK  — a PCB-domain read (Mutates=false) arrives while the flag is set:
//            refused with STALE_READ unless it carries a forceReason, or is in
//            the narrow staleBlockExemptReads set (still advisory-only).
//
// Exemptions (never SET the flag) — the first three are scar tissue, do not
// remove them:
//   - pcb.save: saving changes no copper; the daemon's debounced autosave also
//     bypasses /action entirely (dispatchSave), so neither path false-flags.
//   - pcb.pour.rebuild: it is the FIX for stale pour connectivity, not a new
//     hazard — it clears instead.
//   - any request carrying `dryRun:true`: the catalog's Mutates flag is
//     action-name granular and cannot see that a preview enumerates without
//     touching the board. `pcb clear --dry-run` used to arm the flag and make
//     every later read cry stale on an untouched board (issue #112).
//   - view-only editor state (staleViewOnlyActions): added when the advisory
//     became a refusal, because a false positive now costs a failed command
//     instead of a stderr line. See that var for the argument.
//
// windowIds churn on reconnect; a reconnected window starts clean (a window
// reload re-reads the saved document, which is exactly the stale-fix), so
// per-window in-memory state is the right lifetime.

// staleExemptActions never mark the window stale even though the catalog says
// Mutates=true (see package comment).
var staleExemptActions = map[string]bool{
	"pcb.save":         true,
	"pcb.pour.rebuild": true,
}

// staleViewOnlyActions are catalog-Mutates PCB actions that change only what the
// EDITOR SHOWS — the viewed side, the active layer, layer visibility — and never
// a primitive an enumeration reads back. They cannot make a later read stale, so
// they must not arm the flag.
//
// This was tolerable while the guard only annotated; as a refusal it would be a
// guaranteed false positive, because the action catalog itself prescribes the
// mutate→read sequence these would break: pcb.view.side declares
// VerifyWith=[pcb.layers.list, pcb.snapshot] and pcb.layers.set_current declares
// VerifyWith=[pcb.layers.list] (internal/protocol/actions.go). A guard that
// refuses the catalog's own documented verification step is wrong.
var staleViewOnlyActions = map[string]bool{
	"pcb.view.side":          true,
	"pcb.layers.set_current": true,
	"pcb.layers.visibility":  true,
}

// staleBlockExemptReads are PCB reads that keep the ADVISORY but are never
// REFUSED.
//
// pcb.snapshot is the only member, on two grounds. (1) It returns a rendered
// picture of the live canvas, not an enumeration off the stale per-document
// index — it is the cheapest way for an agent to SEE whether a write landed, and
// refusing it removes the eye rather than the hazard. (2) When a snapshot IS
// wrong it is wrong for a different reason with a different fix: a blank/stale
// FRAME is cured by bringing the window to the foreground, not by `doc reload`
// (memory: snapshot-blank-vs-stale-foreground). Refusing it would hand the
// caller a next-step command that does not fix its problem — and this gate's
// whole contract is that its message names a next step that works.
var staleBlockExemptReads = map[string]bool{
	"pcb.snapshot": true,
}

// pcbStaleMarks reports whether a successful request should mark the window's
// PCB engine state as possibly stale: any PCB-domain mutating request
// (requestMutates = catalog Mutates minus dry-run previews, the same predicate
// autosave uses) minus the exempt set.
func pcbStaleMarks(req *protocol.Request) bool {
	return docTypeForAction(req.Action) == "pcb" && requestMutates(req) &&
		!staleExemptActions[req.Action] && !staleViewOnlyActions[req.Action]
}

// pcbStaleRead reports whether a request is a PCB-domain read that can return
// stale data (any non-mutating pcb.* request: lists, DRC, report, snapshot …).
// A dry-run preview counts as a read on BOTH sides of the state machine: it
// changes nothing (so it never marks), and its enumeration is read back off the
// same engine state (so it earns the advisory) — `pcb clear --dry-run` on an
// un-reloaded board is exactly the miscount that opened issue #112.
func pcbStaleRead(req *protocol.Request) bool {
	return docTypeForAction(req.Action) == "pcb" && !requestMutates(req)
}

// pcbStaleClears reports whether a successful request resets the stale flag.
// `doc reload` has no single typed action — its unique step is the
// debug.exec_js closeDocument call (see package comment); pcb.pour.rebuild
// clears because rebuilding pours is the documented stale-connectivity fix.
func pcbStaleClears(req *protocol.Request) bool {
	switch req.Action {
	case "pcb.pour.rebuild":
		return true
	case "debug.exec_js":
		code, _ := req.Payload["code"].(string)
		return strings.Contains(code, "closeDocument")
	}
	return false
}

// staleGuard is the per-window stale-read state machine. Methods are safe for
// concurrent use.
type staleGuard struct {
	mu sync.Mutex
	// last maps windowId → the name of the last successful PCB mutation not yet
	// followed by a reload ("" / absent = no stale risk).
	last map[string]string
}

func newStaleGuard() *staleGuard {
	return &staleGuard{last: map[string]string{}}
}

// observe applies one completed action to the state machine: it may annotate
// resp with a staleRisk advisory (reads while stale) and updates the per-window
// flag (successful mutations set it, reload/pour-rebuild clear it). Call it
// with the connector's response before writing it to the caller.
func (g *staleGuard) observe(req *protocol.Request, resp *protocol.Response) {
	if g == nil || req == nil || resp == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	// Annotate reads first: the read itself never changes the state.
	if pcbStaleRead(req) {
		if mutation := g.last[req.WindowID]; mutation != "" {
			resp.StaleRisk = staleRiskMessage(mutation, req.Action)
		}
		return
	}

	// Only successful actions move the state machine.
	if !resp.OK {
		return
	}
	if pcbStaleClears(req) {
		delete(g.last, req.WindowID)
		return
	}
	if pcbStaleMarks(req) {
		g.last[req.WindowID] = req.Action
	}
}

// staleRiskMessage builds the advisory. Deliberately free of timestamps so the
// CLI can deduplicate identical warnings within one composite command.
//
// Reads reaching this point are the ones the gate did NOT refuse: a
// staleBlockExemptReads member (pcb.snapshot) or a forceReason bypass. The
// advisory is what remains of the old behaviour for exactly those.
func staleRiskMessage(mutation, read string) string {
	return fmt.Sprintf(
		"PCB was mutated by %s since the last reload — %s (and DRC) may read stale engine state; run `easyeda doc reload` first (SKILL 铁律 5)",
		mutation, read)
}

// ── the gate ───────────────────────────────────────────────────────────────

// blockedBy reports the un-reloaded mutation that makes req a stale read, or ""
// when the read may proceed (not a PCB read, window clean, or block-exempt).
// It does NOT consider forceReason — that is the caller's policy decision, so
// the bypass can be audited in one place.
func (g *staleGuard) blockedBy(req *protocol.Request) string {
	if g == nil || req == nil {
		return ""
	}
	if !pcbStaleRead(req) || staleBlockExemptReads[req.Action] {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last[req.WindowID]
}

// staleReadRefusal renders the STALE_READ error text.
//
// The message MUST carry a runnable next step, not a diagnosis: a verdict that
// cannot be acted on is what produced 1780 ignored advisories. project is the
// resolved project name when the daemon knows it, so the printed command is
// copy-pasteable rather than a template.
func staleReadRefusal(read, mutation, project string) (message, detail string) {
	target := project
	if target == "" {
		target = "<name>"
	}
	next := "easyeda doc reload --project " + target
	message = fmt.Sprintf(
		"%s —— PCB 自 %s 后未 reload,读到的是旧引擎状态。\n下一步: %s\n(绕过: --force-reason \"<理由>\",入审计)",
		read, mutation, next)
	detail = fmt.Sprintf("SKILL 铁律 5 (机械门): 下一步 `%s`;确需读旧状态时用 --force-reason \"<理由>\"(入审计)", next)
	return message, detail
}

// projectHint resolves the best project identity for the refusal message: the
// caller's --project hint, else the target window's project name/uuid.
func (s *Server) projectHint(req *protocol.Request) string {
	for _, c := range s.stageKeyCandidates(req) {
		if strings.TrimSpace(c) != "" {
			return c
		}
	}
	return ""
}

// checkStaleRead enforces iron rule 5 at the dispatch choke point: a PCB read on
// a window mutated since its last reload is REFUSED with STALE_READ. Returns a
// ready-to-send error response when the read must be refused, nil when it may
// proceed.
//
// forceReason is the escape hatch, and every use of it is written to the audit
// trail as its own `daemon.stale_read.force` row — the same discipline the stage
// gate applies to workflow-state history (stagegate.go, verdict.Audited): a
// bypass that leaves no trace is indistinguishable from a guard that never fired.
// A distinct pseudo-action name (never a real catalog action) keeps the real
// action's call/failure statistics intact while staying greppable.
//
// Unlike the stage gate there is no refused-force tier here: forcing a READ
// cannot damage the board, only mislead the reader, so it is always granted —
// and the response still carries the staleRisk advisory (observe), which the CLI
// prints on stderr.
func (s *Server) checkStaleRead(req *protocol.Request) *protocol.Response {
	mutation := s.staleReads.blockedBy(req)
	if mutation == "" {
		return nil
	}
	if reason := strings.TrimSpace(req.ForceReason); reason != "" {
		s.logf("stale-read gate: %s FORCED past %s (reason: %s)", req.Action, mutation, reason)
		s.audit.Append(auditEntry{
			Timestamp: time.Now().UTC(),
			RequestID: req.ID,
			WindowID:  req.WindowID,
			ClientID:  req.ClientID,
			Action:    "daemon.stale_read.force",
			OK:        true,
			Result: map[string]any{
				"read":     req.Action,
				"mutation": mutation,
				"reason":   reason,
				"project":  s.projectHint(req),
			},
		})
		return nil
	}
	message, detail := staleReadRefusal(req.Action, mutation, s.projectHint(req))
	resp := errorResponse(req.ID, "STALE_READ", message, detail)
	return &resp
}
