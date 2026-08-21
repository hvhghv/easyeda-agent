package daemon

import (
	"fmt"
	"strings"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// Daemon-level workflow stage gate (issue #97 follow-up).
//
// The CLI gates its composite route commands, but the daemon is the choke point
// every typed action must pass — so the gate lives HERE too, driven by the
// action catalog (RequiresGate / InvalidatesStage) the same way autosave is
// driven by Mutates. A raw /action caller therefore cannot draw copper tracks
// past an unconfirmed layout, and any placement/outline mutation clears the
// stale downstream confirmations no matter which client performed it.
//
// debug.exec_js remains an un-gateable escape hatch by design; the document
// fingerprints stored at confirm time (verified by the CLI gates) catch what
// it changes after the fact.

// gateForAction maps action name → required workflow gate ("" = ungated).
var gateForAction = func() map[string]string {
	m := map[string]string{}
	for _, a := range protocol.AllActions() {
		if a.RequiresGate != "" {
			m[a.Name] = a.RequiresGate
		}
	}
	return m
}()

// invalidatesForAction maps action name → earliest workflow stage the action
// invalidates on success ("" = none).
var invalidatesForAction = func() map[string]workflow.Stage {
	m := map[string]workflow.Stage{}
	for _, a := range protocol.AllActions() {
		if a.InvalidatesStage != "" {
			m[a.Name] = workflow.Stage(a.InvalidatesStage)
		}
	}
	return m
}()

// stageKeyCandidates lists every identity the project's workflow state may be
// filed under: the caller's routing hint plus the target window's project name
// and uuid. The CLI usually writes by --project (a name); a raw caller may only
// know the windowId — the candidates make both find the same record.
func (s *Server) stageKeyCandidates(req *protocol.Request) []string {
	out := []string{}
	if strings.TrimSpace(req.Project) != "" {
		out = append(out, req.Project)
	}
	if c, ok := s.hub.get(req.WindowID); ok {
		snap := c.snapshot()
		if snap.Context.ProjectName != "" {
			out = append(out, snap.Context.ProjectName)
		}
		if snap.Context.ProjectUUID != "" {
			out = append(out, snap.Context.ProjectUUID)
		}
	}
	return out
}

// checkStageGate enforces a gated action's workflow preconditions. Returns a
// ready-to-send error response when the action must be refused, nil when it may
// proceed. Fail-closed: an unreadable state or an unresolvable project identity
// blocks the action (with an explicit forceReason as the audited override).
func (s *Server) checkStageGate(req *protocol.Request) *protocol.Response {
	gate := gateForAction[req.Action]
	if gate == "" {
		return nil
	}
	force := strings.TrimSpace(req.ForceReason) != ""
	candidates := s.stageKeyCandidates(req)
	if len(candidates) == 0 && !force {
		resp := errorResponse(req.ID, "STAGE_BLOCKED",
			fmt.Sprintf("%s requires the %q gate but the target window has no project identity\n下一步: 重跑本命令并带上 --project <name>(见 `easyeda health`);确需放行用 --force-reason \"<理由>\"(入审计)", req.Action, gate),
			"pass --project (or a forceReason for an audited override)")
		return &resp
	}
	st, err := workflow.LoadAny(candidates...)
	if err != nil {
		// Fail-closed: a corrupt/unreadable state file must not read as "un-gated".
		// LoadAny only errors while READING an existing file, so a key is known —
		// name the file, because "unreadable" without a path is not a next step.
		next := "easyeda workflow status"
		if key := s.projectHint(req); key != "" {
			next = fmt.Sprintf("修好或删掉 %s,再 easyeda workflow init --project %s", workflow.Path(key), key)
		}
		resp := errorResponse(req.ID, "STAGE_BLOCKED",
			fmt.Sprintf("%s: workflow stage state unreadable — refusing gated action\n下一步: %s", req.Action, next),
			err.Error())
		return &resp
	}
	verdict := workflow.CheckRouteGate(st, force, req.ForceUnsafe, strings.TrimSpace(req.ForceReason))
	if verdict.Audited {
		// Persist every audit event — a granted bypass AND a refused --force
		// attempt (#132) both belong in the trail. Authorization stays
		// per-request (nothing is confirmed), so the next un-forced call is
		// gated again.
		if serr := workflow.Save(st); serr != nil {
			s.logf("stage gate: could not persist force audit for %s: %v", req.Action, serr)
		}
	}
	if !verdict.Allowed {
		next := stageNextStep(st, verdict.Missing, s.projectHint(req))
		resp := errorResponse(req.ID, "STAGE_BLOCKED",
			fmt.Sprintf("%s: %s\n下一步: %s", req.Action, verdict.Message, next),
			next+"  (全局状态: `easyeda workflow status`)")
		return &resp
	}
	if verdict.Forced {
		s.logf("stage gate: %s FORCED past %s (unsafe=%v, reason: %s)", req.Action, strings.Join(verdict.Missing, ", "), req.ForceUnsafe, req.ForceReason)
	}
	return nil
}

// ── refusal messages that name a runnable next step ────────────────────────
//
// A refusal that only points at a status command ("see `easyeda workflow
// status`") makes the caller run one more read to learn what it must do; the
// 49-day audit review found that the rules agents actually obey are the ones
// whose refusal hands back the repair command itself. So every STAGE_BLOCKED
// carries the concrete command for the EARLIEST unmet gate — the one that has
// to run first — in the message (the CLI surfaces `error.message`; `detail`
// only reaches raw/JSON callers).

// stageCommandFor maps one unmet workflow stage to the command that satisfies
// it. Kept in step with the CLI's own ladder (internal/app/cmd_workflow.go
// workflowNext) and the subcommands in internal/app/cmd_pcb_stage.go.
func stageCommandFor(stage workflow.Stage, projectFlag string) string {
	switch stage {
	case workflow.StagePlacementConfirmed:
		// confirm-layout refuses until all four placement tiers are signed off,
		// so the tier ladder is the real first step.
		return "easyeda pcb stage confirm-tier <1|2|3|4> --parts …" + projectFlag +
			" (四档签完) → easyeda pcb stage confirm-layout" + projectFlag + " --note \"...\""
	case workflow.StageOutlineConfirmed:
		return "easyeda pcb stage confirm-outline" + projectFlag + " --note \"...\""
	case workflow.StagePreRoutePassed:
		return "easyeda pcb layout-lint --gate" + projectFlag
	case workflow.StagePostRouteChecked:
		return "easyeda workflow advance" + projectFlag + " (跑 pcb-check 门)"
	default:
		return "easyeda workflow status" + projectFlag
	}
}

// stageNextStep picks WHICH unmet gate to name.
//
// Normally that is the lowest-ranked entry of verdict.Missing — the ladder is
// ordered, so the earliest gap must close first. The exception is the #132 hard
// tier: when NEITHER placement_confirmed nor outline_confirmed is live, the
// mechanical skeleton is entirely unconfirmed and a plain --force is refused;
// placement_confirmed is not in Missing (CheckRouteGate does not require it) yet
// it is what the caller has to do first, so name it explicitly.
func stageNextStep(st *workflow.State, missing []string, project string) string {
	projectFlag := ""
	if p := strings.TrimSpace(project); p != "" {
		projectFlag = " --project " + p
	}
	if st != nil && !st.Has(workflow.StagePlacementConfirmed) && !st.Has(workflow.StageOutlineConfirmed) {
		return stageCommandFor(workflow.StagePlacementConfirmed, projectFlag)
	}
	earliest := workflow.Stage("")
	rank := -1
	for _, m := range missing {
		stg := workflow.Stage(m)
		r := workflow.Rank(stg)
		if r < 0 {
			continue
		}
		if rank < 0 || r < rank {
			earliest, rank = stg, r
		}
	}
	return stageCommandFor(earliest, projectFlag)
}

// maybeInvalidateStage clears downstream workflow confirmations after a
// successful placement/outline mutation, catalog-driven. The cleared stages are
// surfaced as a response warning so every client sees what its edit invalidated.
func (s *Server) maybeInvalidateStage(req *protocol.Request, resp *protocol.Response) {
	stg, ok := invalidatesForAction[req.Action]
	if !ok || resp == nil || !resp.OK {
		return
	}
	cleared := workflow.InvalidateAll(s.stageKeyCandidates(req), stg, "action "+req.Action)
	if len(cleared) > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf(
			"workflow stage invalidated: %s (cause: %s) — re-confirm layout/outline and re-run `pcb layout-lint --gate` before routing",
			strings.Join(cleared, ", "), req.Action))
	}
}
