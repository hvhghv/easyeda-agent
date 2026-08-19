package protocol

import "testing"

func TestPcbManufacturingExportActionsAreReadOnly(t *testing.T) {
	want := map[string]bool{
		"pcb.export.manufacture": false,
		"pcb.export.gerber":      false,
		"pcb.export.pick_place":  false,
		"pcb.export.ipc2581":     false,
	}
	for _, spec := range AllActions() {
		if _, ok := want[spec.Name]; !ok {
			continue
		}
		if spec.Domain != DomainPcb || spec.Mutates || !spec.NeedsWindow {
			t.Errorf("%s has unsafe catalog metadata: %+v", spec.Name, spec)
		}
		want[spec.Name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing action %s", name)
		}
	}
}
