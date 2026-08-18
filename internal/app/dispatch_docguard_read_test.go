package app

import "testing"

// TestDocGuardAppliesToReads: --doc must pin READ actions too. Reads used to
// skip the guard entirely (actionMutates gate), so `--doc <page>` on
// schematic.components.list was silently ignored and returned the FOREGROUND
// page's data — page drift in read form.
func TestDocGuardAppliesToReads(t *testing.T) {
	// A read action with --doc set triggers the switch/confirm path.
	if !docGuardApplies("Page2", "schematic.components.list") {
		t.Error("read action + --doc must trigger the doc guard (silently ignoring --doc is page drift)")
	}
	// Mutations keep their existing behavior.
	if !docGuardApplies("Page2", "schematic.component.place") {
		t.Error("mutating action + --doc must trigger the doc guard")
	}
	// The guard's own navigation/read tools stay exempt (recursion guard):
	// ensureActiveDoc itself calls these with cfg.doc still set.
	for _, a := range []string{
		"document.current", "document.open", "schematic.page.open",
		"schematic.pages.list", "pcb.documents.list",
	} {
		if docGuardApplies("Page2", a) {
			t.Errorf("%s must stay exempt or ensureActiveDoc recurses through itself", a)
		}
	}
	// Daemon-local health touches no document.
	if docGuardApplies("Page2", "system.health") {
		t.Error("system.health must not be gated (daemon-local, no document)")
	}
	// No --doc → no guard, ever.
	if docGuardApplies("", "schematic.components.list") || docGuardApplies("", "schematic.component.place") {
		t.Error("guard must be a no-op when --doc is unset")
	}
}
