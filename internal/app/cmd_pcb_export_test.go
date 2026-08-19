package app

import (
	"io"
	"testing"
)

func TestPcbManufacturingExportCommandsAreRegistered(t *testing.T) {
	pcb := newPcbCmd(&appConfig{}, io.Discard, io.Discard)
	want := map[string][]string{
		"export-manufacture": {"name", "out"},
		"export-gerber":      {"name", "out", "color-silkscreen"},
		"export-pick-place":  {"name", "out", "type"},
		"export-ipc2581":     {"name", "out", "type", "oem-number"},
	}
	seen := map[string]bool{}
	for _, c := range pcb.Commands() {
		flags, ok := want[c.Name()]
		if !ok {
			continue
		}
		seen[c.Name()] = true
		for _, flag := range flags {
			if c.Flags().Lookup(flag) == nil {
				t.Errorf("pcb %s is missing --%s", c.Name(), flag)
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("pcb command %s is not registered", name)
		}
	}
}
