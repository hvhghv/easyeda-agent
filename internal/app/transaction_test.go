package app

import "testing"

func TestTrackCLITransactionTargetUsesResolvedWindow(t *testing.T) {
	finish := beginCLITransaction()
	trackCLITransactionTarget("127.0.0.1", 60832, []byte(`{"ok":true,"windowId":"live-window"}`))
	cliTransactionState.Lock()
	if len(cliTransactionState.targets) != 1 {
		cliTransactionState.Unlock()
		t.Fatalf("tracked targets=%d, want 1", len(cliTransactionState.targets))
	}
	cliTransactionState.Unlock()
	// Avoid making a real release HTTP call in this pure state test.
	cliTransactionState.Lock()
	cliTransactionState.targets = nil
	cliTransactionState.Unlock()
	finish()
}

func TestNestedCLITransactionKeepsOuterLease(t *testing.T) {
	finishOuter := beginCLITransaction()
	outerID := currentCLITransactionID()
	finishInner := beginCLITransaction()
	if innerID := currentCLITransactionID(); innerID != outerID {
		t.Fatalf("nested CLI changed transaction id: outer=%q inner=%q", outerID, innerID)
	}
	finishInner()
	if got := currentCLITransactionID(); got != outerID {
		t.Fatalf("nested CLI release ended outer transaction: got %q want %q", got, outerID)
	}
	finishOuter()
	if got := currentCLITransactionID(); got != "" {
		t.Fatalf("outer CLI release left transaction active: %q", got)
	}
}
