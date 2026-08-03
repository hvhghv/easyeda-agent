package app

import (
	"bytes"
	"strings"
	"testing"
)

// The gate's whole point is that "which checkers, in what order, whose exit code
// counts" stops being re-decided per run. These tests pin that contract.

func TestResolveGateStagesDefaultsToTheFullFixedPipeline(t *testing.T) {
	run, skipped, err := resolveGateStages("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"layout-lint", "check", "bridge-check", "drc"}
	if strings.Join(run, ",") != strings.Join(want, ",") {
		t.Fatalf("pipeline order changed: got %v want %v", run, want)
	}
	if len(skipped) != 0 {
		t.Fatalf("nothing should be skipped by default, got %v", skipped)
	}
}

func TestResolveGateStagesOnlyKeepsPipelineOrderNotArgumentOrder(t *testing.T) {
	// Arguments deliberately reversed: the report must always read as the same
	// fixed pipeline, otherwise two runs of the same board look different.
	run, skipped, err := resolveGateStages("drc,layout-lint", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(run, ",") != "layout-lint,drc" {
		t.Fatalf("got %v, want pipeline order [layout-lint drc]", run)
	}
	if strings.Join(skipped, ",") != "check,bridge-check" {
		t.Fatalf("excluded stages wrong: %v", skipped)
	}
}

func TestResolveGateStagesSkip(t *testing.T) {
	run, skipped, err := resolveGateStages("", "drc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(run, ",") != "layout-lint,check,bridge-check" {
		t.Fatalf("got %v", run)
	}
	if strings.Join(skipped, ",") != "drc" {
		t.Fatalf("got %v", skipped)
	}
}

func TestResolveGateStagesRejectsUnknownName(t *testing.T) {
	// A typo'd --only must never silently gate on fewer checks than asked.
	if _, _, err := resolveGateStages("layout-lnt", ""); err == nil {
		t.Fatal("a misspelled stage must be an error, not a silent no-op")
	}
	if _, _, err := resolveGateStages("", "drcc"); err == nil {
		t.Fatal("a misspelled --skip stage must be an error")
	}
}

func TestResolveGateStagesRejectsOnlyPlusSkipAndEmptySelection(t *testing.T) {
	if _, _, err := resolveGateStages("check", "drc"); err == nil {
		t.Fatal("--only with --skip must be rejected")
	}
	if _, _, err := resolveGateStages("", "layout-lint,check,bridge-check,drc"); err == nil {
		t.Fatal("skipping every stage must be an error, not an empty pass")
	}
}

func TestGradeGateReportPassWhenEveryStagePasses(t *testing.T) {
	rep := gateReport{Stages: []gateStage{
		{Name: "layout-lint", Status: gateStatusPass},
		{Name: "check", Status: gateStatusPass},
	}}
	gradeGateReport(&rep)
	if !rep.OK || rep.Verdict != "pass" {
		t.Fatalf("got ok=%v verdict=%q", rep.OK, rep.Verdict)
	}
	if len(rep.Blockers) != 0 {
		t.Fatalf("unexpected blockers: %v", rep.Blockers)
	}
}

func TestGradeGateReportWarningsDoNotBlock(t *testing.T) {
	rep := gateReport{Stages: []gateStage{
		{Name: "layout-lint", Status: gateStatusPass, Warnings: 3, Summary: "3 tight"},
	}}
	gradeGateReport(&rep)
	if !rep.OK || rep.Verdict != "pass" {
		t.Fatalf("advisory findings must not gate: ok=%v verdict=%q", rep.OK, rep.Verdict)
	}
	if len(rep.Warnings) != 1 {
		t.Fatalf("warnings should still be surfaced: %v", rep.Warnings)
	}
}

func TestGradeGateReportFailWhenAStageHasBlockingFindings(t *testing.T) {
	rep := gateReport{Stages: []gateStage{
		{Name: "layout-lint", Status: gateStatusPass},
		{Name: "bridge-check", Status: gateStatusFail, Errors: 2, Summary: "2 bridge(short)"},
	}}
	gradeGateReport(&rep)
	if rep.OK || rep.Verdict != "fail" {
		t.Fatalf("got ok=%v verdict=%q", rep.OK, rep.Verdict)
	}
	if len(rep.Blockers) != 1 || !strings.Contains(rep.Blockers[0], "bridge-check") {
		t.Fatalf("blockers wrong: %v", rep.Blockers)
	}
}

func TestGradeGateReportBlockedOutranksFail(t *testing.T) {
	// This is the distinction the audit log said agents were missing: a checker
	// that could not RUN must not be reported as a broken board — otherwise the
	// agent "fixes" a schematic that was never judged.
	rep := gateReport{Stages: []gateStage{
		{Name: "layout-lint", Status: gateStatusFail, Errors: 1, Summary: "1 overlap"},
		{Name: "check", Status: gateStatusError, Error: "no EasyEDA connector is available"},
		{Name: "drc", Status: gateStatusSkipped},
	}}
	gradeGateReport(&rep)
	if rep.Verdict != "blocked" {
		t.Fatalf("a stage that could not run must win the verdict, got %q", rep.Verdict)
	}
	if rep.OK {
		t.Fatal("blocked must not be ok")
	}
	joined := strings.Join(rep.Blockers, " | ")
	if !strings.Contains(joined, "没能跑起来") || !strings.Contains(joined, "connector") {
		t.Fatalf("the infra cause must reach the blockers list: %v", rep.Blockers)
	}
}

func TestGradeGateReportErrorStageContributesNoWarnings(t *testing.T) {
	// A stage that never ran has no findings to report — counting its zeroed
	// fields as "clean" would understate what is unknown.
	rep := gateReport{Stages: []gateStage{
		{Name: "check", Status: gateStatusError, Error: "boom", Warnings: 7},
	}}
	gradeGateReport(&rep)
	if len(rep.Warnings) != 0 {
		t.Fatalf("a stage that could not run must not emit warnings: %v", rep.Warnings)
	}
}

func TestRenderGateReportShowsVerdictStagesAndPrescribedNextStep(t *testing.T) {
	rep := gateReport{Stages: []gateStage{
		{Name: "layout-lint", Status: gateStatusFail, Errors: 2, Summary: "2 overlap, 0 pin-coincidence"},
		{Name: "check", Status: gateStatusPass, Summary: "0 finding(s)"},
		{Name: "drc", Status: gateStatusSkipped, Summary: "被 --only/--skip 排除"},
	}}
	gradeGateReport(&rep)
	var buf bytes.Buffer
	renderGateReport(rep, &buf)
	out := buf.String()
	for _, want := range []string{"FAIL", "layout-lint", "SKIP", "阻塞项", "下一步"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
	// The prescribed next action must travel with the failure — the whole reason
	// agents invented four different next steps was that nothing prescribed one.
	if !strings.Contains(out, "autolayout") {
		t.Fatalf("layout-lint failure must carry its fix path:\n%s", out)
	}
}

func TestRenderGateReportBlockedExplainsItIsNotTheBoard(t *testing.T) {
	rep := gateReport{Stages: []gateStage{
		{Name: "layout-lint", Status: gateStatusError, Error: "no connector"},
	}}
	gradeGateReport(&rep)
	var buf bytes.Buffer
	renderGateReport(rep, &buf)
	out := buf.String()
	if !strings.Contains(out, "BLOCKED") {
		t.Fatalf("verdict missing:\n%s", out)
	}
	// The 146 blind NO_CONNECTOR retries in the audit log are exactly what this
	// guidance is for: check health/doc first, do not retry other checkers.
	for _, want := range []string{"不是「板子有问题」", "health", "doc switch"} {
		if !strings.Contains(out, want) {
			t.Fatalf("blocked guidance missing %q:\n%s", want, out)
		}
	}
}

func TestGateAdviceCoversEveryStage(t *testing.T) {
	// Every stage must have a prescribed next action, or the gate reintroduces
	// the "agent invents its own next step" problem for that stage.
	for _, spec := range gateStages {
		if gateAdviceFor(spec.Name) == "" {
			t.Fatalf("stage %q has no prescribed next step", spec.Name)
		}
	}
}
