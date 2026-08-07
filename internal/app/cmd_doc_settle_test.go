package app

import (
	"testing"
	"time"
)

// A non-empty count that repeats settles immediately; the whole point of the
// settle wait is to catch the 0 → N load, not to stall a page that already has
// its parts.
func TestSettleTracker_NonEmptyStable(t *testing.T) {
	s := &settleTracker{minEmptySamples: 3}
	if s.observe(93) {
		t.Fatal("first sample must not settle (no prior to compare)")
	}
	if !s.observe(93) {
		t.Fatal("two identical non-empty samples should settle")
	}
}

// A page mid-load (0 → 0 → 93 → 93) must NOT settle on the early zeros before
// the grace window, or `sch check` fired right after a switch gets empty
// findings.
func TestSettleTracker_LoadingNotMistakenEmpty(t *testing.T) {
	s := &settleTracker{minEmptySamples: 3}
	if s.observe(0) {
		t.Fatal("first zero must not settle")
	}
	if s.observe(0) {
		t.Fatal("second zero is within the grace window, must not settle")
	}
	if s.observe(93) {
		t.Fatal("count changed 0→93, must not settle yet")
	}
	if !s.observe(93) {
		t.Fatal("two identical non-empty samples after load should settle")
	}
}

// A genuinely empty page (stable 0 past the grace window) eventually settles so
// the wait doesn't burn the full deadline on every empty page.
func TestSettleTracker_GenuinelyEmptySettles(t *testing.T) {
	s := &settleTracker{minEmptySamples: 3}
	got := []bool{s.observe(0), s.observe(0), s.observe(0)}
	if got[0] || got[1] {
		t.Fatalf("zeros before grace window must not settle: %v", got)
	}
	if !got[2] {
		t.Fatal("stable zero past minEmptySamples should settle")
	}
}

// ─── #161: the settle probe must follow the document type ─────────────

func TestSettleProbeAction_PicksPerDocumentType(t *testing.T) {
	if got := settleProbeAction("pcb"); got != "pcb.components.list" {
		t.Fatalf("pcb probe = %q, want pcb.components.list", got)
	}
	for _, docType := range []string{"schematic", ""} {
		if got := settleProbeAction(docType); got != "schematic.components.list" {
			t.Fatalf("%q probe = %q, want schematic.components.list", docType, got)
		}
	}
}

// A PCB target must be polled with the PCB probe. Before #161 the schematic
// probe was hardcoded, so switching to a PCB produced 21 consecutive
// EDA_CALL_FAILED rows and never settled.
func TestWaitDocSettleFor_PcbUsesPcbProbe(t *testing.T) {
	cfg, state, closeFn := newAutolayoutTestDaemon(t, func(_ int, call autolayoutTestCall) string {
		if call.Action == "pcb.components.list" {
			return `{"ok":true,"result":{"count":69}}`
		}
		return `{"ok":false,"error":{"code":"EDA_CALL_FAILED","message":"Failed to list schematic components."}}`
	})
	defer closeFn()

	if !waitDocSettleFor(cfg, "w1", "pcb") {
		t.Fatal("PCB document did not settle with the PCB probe")
	}
	for _, call := range state.snapshot() {
		if call.Action == "schematic.components.list" {
			t.Fatalf("PCB settle used the schematic probe: %+v", state.snapshot())
		}
	}
}

// A probe that keeps failing is the wrong probe (or a dead window), not a page
// still loading: bail out after a few tries instead of burning the full 8s
// deadline, which is what filled the audit log with 21 identical failures.
func TestWaitDocSettleFor_BailsOutAfterConsecutiveProbeErrors(t *testing.T) {
	cfg, state, closeFn := newAutolayoutTestDaemon(t, func(_ int, _ autolayoutTestCall) string {
		return `{"ok":false,"error":{"code":"EDA_CALL_FAILED","message":"Failed to list schematic components."}}`
	})
	defer closeFn()

	started := time.Now()
	if waitDocSettleFor(cfg, "w1", "schematic") {
		t.Fatal("an always-failing probe must not report settled")
	}
	elapsed := time.Since(started)

	if n := len(state.snapshot()); n != docSettleMaxProbeErrors {
		t.Fatalf("probe calls = %d, want %d (no polling on to the deadline)", n, docSettleMaxProbeErrors)
	}
	if elapsed >= docSettleDeadline {
		t.Fatalf("gave up after %v, want well under the %v deadline", elapsed, docSettleDeadline)
	}
}
