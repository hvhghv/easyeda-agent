package app

// dry-run 纯计算铁律 (ADR-0004 Decision 4) 的机械保证测试:
//   1. 标志开启时 mutating action 在派发层被拒(daemon 一个字节都收不到),
//      读 action 照常放行;
//   2. restore 语义正确(嵌套/恢复);
//   3. 一条真实命令(sch autolayout 模板引擎 dry-run)全程跑完,零 mutating 派发。

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestDryRunGuardRejectsMutatingAllowsReads(t *testing.T) {
	restore := setDispatchDryRun(true)
	defer restore()

	if err := dryRunGuard("schematic.component.place"); err == nil {
		t.Fatalf("mutating action must be rejected while the dry-run flag is set")
	}
	delErr := dryRunGuard("schematic.component.delete")
	if delErr == nil {
		t.Fatalf("schematic.component.delete must be rejected in dry-run mode")
	}
	if !strings.Contains(delErr.Error(), "dry-run") {
		t.Fatalf("rejection must name dry-run mode, got: %v", delErr)
	}
	// exec_js is catalogued Mutates=true — the escape hatch must not bypass the guard.
	if err := dryRunGuard("debug.exec_js"); err == nil {
		t.Fatalf("debug.exec_js must be rejected in dry-run mode (it can mutate)")
	}
	// Reads pass.
	for _, read := range []string{"schematic.components.list", "document.current", "pcb.nets.list"} {
		if err := dryRunGuard(read); err != nil {
			t.Fatalf("read action %s must pass the dry-run guard, got: %v", read, err)
		}
	}
}

func TestSetDispatchDryRunRestoreSemantics(t *testing.T) {
	if dispatchDryRunActive() {
		t.Fatalf("flag must start unset")
	}
	restore := setDispatchDryRun(true)
	if !dispatchDryRunActive() {
		t.Fatalf("flag must be set after setDispatchDryRun(true)")
	}
	// Nested set (route-critical dry-run → runPowerPlanes dry-run) keeps the flag
	// after the inner restore.
	inner := setDispatchDryRun(true)
	inner()
	if !dispatchDryRunActive() {
		t.Fatalf("inner restore must restore the OUTER value (true), not clear it")
	}
	restore()
	if dispatchDryRunActive() {
		t.Fatalf("outer restore must clear the flag")
	}
}

// TestPostActionDryRunRejectsBeforeAnyDispatch: with the flag set, a mutating
// action must be refused at the dispatch layer — the daemon never sees the
// request — while reads still go through.
func TestPostActionDryRunRejectsBeforeAnyDispatch(t *testing.T) {
	cfg, daemon, closeFn := newAutolayoutTestDaemon(t, func(int, autolayoutTestCall) string {
		return `{"ok":true,"result":{}}`
	})
	defer closeFn()

	restore := setDispatchDryRun(true)
	defer restore()

	_, err := postAction(cfg, "schematic.component.modify", "w1", map[string]any{"primitiveId": "x"}, defaultActionTimeout)
	if err == nil || !strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("mutating postAction must fail with the dry-run error, got: %v", err)
	}
	if calls := daemon.snapshot(); len(calls) != 0 {
		t.Fatalf("the daemon must never see a mutating action in dry-run mode; saw %d call(s): %+v", len(calls), calls)
	}

	// A read passes the guard and reaches the daemon.
	if _, err := postAction(cfg, "schematic.components.list", "w1", nil, defaultActionTimeout); err != nil {
		t.Fatalf("read action must pass in dry-run mode: %v", err)
	}
	calls := daemon.snapshot()
	if len(calls) != 1 || calls[0].Action != "schematic.components.list" {
		t.Fatalf("expected exactly the read action to reach the daemon, got %+v", calls)
	}
}

// TestRunAutolayoutDryRunIsPureComputation: the template-engine dry-run path
// (the ADR-0004 exemplar) runs END TO END with the purity guard armed — it
// succeeds, and every action that reached the daemon is a read.
func TestRunAutolayoutDryRunIsPureComputation(t *testing.T) {
	cfg, daemon, closeFn := newAutolayoutTestDaemon(t, func(_ int, call autolayoutTestCall) string {
		if resp, ok := autolayoutTargetMetadata(call, "doc-1", "P1"); ok {
			return resp
		}
		if call.Action == "schematic.components.list" {
			return autolayoutScene("doc-1", 400, 300, 0)
		}
		return `{"ok":true,"result":{}}`
	})
	defer closeFn()

	spec := alSpec{
		Page:    "P1",
		Modules: []alSpecModule{{Name: "MCU", Zone: "center", Core: "U1", Parts: []string{"U1"}}},
	}
	var out, errW bytes.Buffer
	err := runAutolayout(cfg, "w1", spec, defaultAutolayoutRules(), false, false, false, false, &out, &errW)
	if err != nil {
		t.Fatalf("template dry-run must succeed: %v\nstdout: %s\nstderr: %s", err, out.String(), errW.String())
	}
	if dispatchDryRunActive() {
		t.Fatalf("the dry-run flag must be restored after the command finishes")
	}
	for _, call := range daemon.snapshot() {
		if actionMutates(call.Action) {
			t.Fatalf("dry-run dispatched mutating action %q — the purity guard is not wired", call.Action)
		}
	}
	if len(daemon.snapshot()) == 0 {
		t.Fatalf("expected the dry-run to have performed read dispatches")
	}
}

// TestRunAutolayoutDryRunGuardTrips: negative control — if a future edit makes
// the dry-run path emit a mutating action, the guard must turn that into a
// loud error instead of a canvas write. Simulated by dispatching a mutation
// inside the guarded window.
func TestRunAutolayoutDryRunGuardTrips(t *testing.T) {
	cfg, daemon, closeFn := newAutolayoutTestDaemon(t, func(int, autolayoutTestCall) string {
		return `{"ok":true,"result":{}}`
	})
	defer closeFn()

	restore := setDispatchDryRun(true)
	defer restore()
	err := dispatch(cfg, "schematic.wire.create", "w1", map[string]any{"points": []float64{0, 0, 10, 0}}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("dispatch of a mutating action inside dry-run must trip the guard, got: %v", err)
	}
	if len(daemon.snapshot()) != 0 {
		t.Fatalf("the guarded mutation must never reach the daemon")
	}
}
