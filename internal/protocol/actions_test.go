package protocol

import (
	"strings"
	"testing"
)

func TestPhase1ActionsHaveStableNames(t *testing.T) {
	actions := AllActions()
	if len(actions) == 0 {
		t.Fatal("expected actions")
	}

	seen := map[string]bool{}
	for _, action := range actions {
		if action.Name == "" {
			t.Fatalf("action has empty name: %#v", action)
		}
		if seen[action.Name] {
			t.Fatalf("duplicate action name: %s", action.Name)
		}
		seen[action.Name] = true
		if action.Phase < 1 {
			t.Fatalf("action %s has invalid phase %d", action.Name, action.Phase)
		}
	}

	for _, required := range []string{
		"system.health",
		"schematic.components.list",
		"schematic.component.place",
		"schematic.wire.create",
		"schematic.drc.check",
		"schematic.export.bom",
	} {
		if !seen[required] {
			t.Fatalf("missing required action: %s", required)
		}
	}
}

func TestConnectPinActionDocumentsYUpContract(t *testing.T) {
	var description string
	for _, action := range AllActions() {
		if action.Name == "schematic.power.connect_pin" {
			description = action.Description
			break
		}
	}
	if description == "" {
		t.Fatal("schematic.power.connect_pin action missing")
	}
	for _, want := range []string{"y-UP", "up moves the endpoint to a larger y", "down to a smaller y"} {
		if !strings.Contains(description, want) {
			t.Errorf("connect_pin description missing %q: %s", want, description)
		}
	}
	if strings.Contains(description, "y-DOWN") {
		t.Errorf("connect_pin description still advertises y-DOWN: %s", description)
	}
}

func TestComponentsListDocumentsReadOnlyPreflightContract(t *testing.T) {
	var spec *ActionSpec
	for _, action := range AllActions() {
		if action.Name == "schematic.components.list" {
			copy := action
			spec = &copy
			break
		}
	}
	if spec == nil {
		t.Fatal("schematic.components.list action missing")
	}
	if spec.Mutates {
		t.Fatal("schematic.components.list must remain read-only (Mutates=false)")
	}
	text := strings.Join(append(append([]string{spec.Description}, spec.Inputs...), spec.Outputs...), " ")
	for _, want := range []string{
		"includeConnectivitySummary",
		"active page",
		"wires",
		"buses",
		"netflags",
		"netports",
		"netlabels",
		"pinsAvailable",
		"pinsError",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("components.list contract missing %q: %s", want, text)
		}
	}
}
