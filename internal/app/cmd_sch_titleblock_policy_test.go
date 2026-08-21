package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestSchTitleblockWritePolicyRefusesBeforeDaemonDispatch(t *testing.T) {
	cfg, captured, cleanup := newCapturingDaemon(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer
	cmd := newSchCmd(cfg, &stdout, &stderr)
	cmd.SetArgs([]string{"titleblock", "--data", "{not json"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("titleblock --data must be refused")
	}
	if !strings.Contains(err.Error(), "图签字段写入已禁用") || !strings.Contains(err.Error(), "sch note") {
		t.Fatalf("policy error must name the reason and safe path, got %q", err)
	}
	captured.mu.Lock()
	defer captured.mu.Unlock()
	if captured.action != "" {
		t.Fatalf("policy refusal must not dispatch any daemon action, got %q", captured.action)
	}
}

func TestSchTitleblockHealthDispatchesReadOnlyProbe(t *testing.T) {
	cfg, captured, cleanup := newCapturingDaemon(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer
	cmd := newSchCmd(cfg, &stdout, &stderr)
	cmd.SetArgs([]string{"titleblock-health"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("titleblock-health: %v", err)
	}
	captured.mu.Lock()
	defer captured.mu.Unlock()
	if captured.action != "schematic.titleblock.health" {
		t.Fatalf("health action = %q, want schematic.titleblock.health", captured.action)
	}
}
