package app

// pcb_via_bond.go — `pcb via-bond` (#118 follow-up, live-verified 2026-07-17).
//
// A footprint can EMBED vias — QFN EPAD thermal vias are the canonical case —
// and they land with net:"" no matter how the part was placed. Live findings on
// the ceshi board (ESP32-S3R8, QFN-56, 9 thermal vias):
//   - `pcb_PrimitiveVia.modify(id, {net})` DOES set the net in-session (DRC and
//     plane bonding then work), BUT
//   - the assignment does NOT survive a save + doc reload: the embedded vias are
//     re-materialized from the footprint definition with net:"" every time. The
//     platform has no instance-level storage for an embedded via's net.
//   - deleting them is equally impossible (#120): delete claims success, an
//     immediate getAll shows them gone, and the next reload resurrects them.
//
// So the honest shape of the fix is an IDEMPOTENT re-bond command, cheap enough
// to run whenever needed: scan netless vias sitting inside a net-carrying pad's
// copper rect and assign that pad's net via the raw-primitive escape hatch
// (debug.exec_js — works on every deployed connector, no re-import needed).
// `pcb check`'s netless-via-in-pad rule is the tripwire that says when to re-run
// (after any doc reload, before DRC / power-planes).

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// viaBondAssign is one planned net assignment.
type viaBondAssign struct {
	ViaID      string  `json:"viaId"`
	Net        string  `json:"net"`
	Designator string  `json:"designator"`
	PadNumber  string  `json:"padNumber"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
}

// planViaBond pairs every NETLESS via with the net-carrying pad whose copper
// rect contains its center. Pads without a real extent fall back to the nominal
// circle (old connector) — same stance as the router's obstacle model. only
// (a designator, optional) narrows the scan to one component's pads.
func planViaBond(pads []pcbPadP, vias []pcbViaP, only string) []viaBondAssign {
	only = strings.ToUpper(strings.TrimSpace(only))
	var out []viaBondAssign
	for _, v := range vias {
		if strings.TrimSpace(v.Net) != "" {
			continue
		}
		for _, p := range pads {
			if strings.TrimSpace(p.Net) == "" {
				continue
			}
			if only != "" && strings.ToUpper(strings.TrimSpace(p.Designator)) != only {
				continue
			}
			inside := false
			if p.W > 0 && p.H > 0 {
				inside = v.X >= p.X-p.W/2 && v.X <= p.X+p.W/2 && v.Y >= p.Y-p.H/2 && v.Y <= p.Y+p.H/2
			} else {
				dx, dy := v.X-p.X, v.Y-p.Y
				inside = dx*dx+dy*dy <= nominalPadHalf*nominalPadHalf
			}
			if inside {
				out = append(out, viaBondAssign{ViaID: v.ID, Net: p.Net, Designator: p.Designator, PadNumber: p.Number, X: v.X, Y: v.Y})
				break
			}
		}
	}
	return out
}

// runPcbViaBond executes the plan through debug.exec_js (raw
// eda.pcb_PrimitiveVia.modify + an in-session readback), so it works on every
// connector version already in the field.
func runPcbViaBond(cfg *appConfig, window, only string, dryRun bool, stdout, stderr io.Writer) error {
	// ADR-0004 Decision 4: dry-run 必须纯计算 —— 机械保证,Mutates 派发直接被拒。
	if dryRun {
		defer setDispatchDryRun(true)()
	}
	pads, err := fetchPcbPads(cfg, window)
	if err != nil {
		return fmt.Errorf("read pads: %w", err)
	}
	vias, err := fetchPcbVias(cfg, window)
	if err != nil {
		return fmt.Errorf("read vias: %w", err)
	}
	plan := planViaBond(pads, vias, only)
	out := map[string]any{
		"ok": true, "dryRun": dryRun, "planned": len(plan), "assignments": plan,
		"note": "embedded-via net assignments do NOT survive a doc reload (platform re-materializes them netless — live-verified #118); re-run via-bond after every reload, before DRC / power-planes. `pcb check` netless-via-in-pad is the tripwire.",
	}
	if dryRun || len(plan) == 0 {
		return writeJSON(stdout, out)
	}

	ids := make([]string, len(plan))
	nets := make([]string, len(plan))
	for i, a := range plan {
		ids[i] = a.ViaID
		nets[i] = a.Net
	}
	idsJSON, _ := json.Marshal(ids)
	netsJSON, _ := json.Marshal(nets)
	code := fmt.Sprintf(`
const ids = %s, nets = %s;
const failed = [];
for (let i = 0; i < ids.length; i++) {
  try { await eda.pcb_PrimitiveVia.modify(ids[i], { net: nets[i] }); }
  catch (e) { failed.push(ids[i]); }
}
let verified = 0;
const all = await eda.pcb_PrimitiveVia.getAll();
const byId = {};
for (const v of all ?? []) byId[v.getState_PrimitiveId()] = v.getState_Net();
for (let i = 0; i < ids.length; i++) if (byId[ids[i]] === nets[i]) verified++;
return { failed, verified };`, idsJSON, netsJSON)

	res, err := requestAction(cfg, "debug.exec_js", window, map[string]any{"code": code})
	if err != nil {
		return fmt.Errorf("via-bond exec: %w", err)
	}
	val := mnav(res.Result, "result", "value")
	if val == nil {
		val = mnav(res.Result, "value")
	}
	failedRaw, _ := mnav(val, "failed").([]any)
	verified, _ := asFloatOK(mnav(val, "verified"))
	out["assigned"] = len(plan) - len(failedRaw)
	out["verified"] = int(verified)
	if len(failedRaw) > 0 {
		out["ok"] = false
		out["failed"] = failedRaw
	}
	if int(verified) < len(plan) {
		fmt.Fprintf(stderr, "warning: %d/%d assignment(s) did not verify — pull `pcb via-list` and re-run\n", len(plan)-int(verified), len(plan))
	}
	return writeJSON(stdout, out)
}
