package daemon

import (
	"context"
	"testing"
	"time"
)

func TestWindowTransactionSerializesWholeCommand(t *testing.T) {
	g := newWindowTransactionGuard()
	ctx := context.Background()
	releaseA, err := g.acquire(ctx, "w1", "command-a", true)
	if err != nil {
		t.Fatal(err)
	}
	releaseA() // action ended, command lease deliberately remains

	done := make(chan error, 1)
	go func() {
		ctxB, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		releaseB, err := g.acquire(ctxB, "w1", "command-b", true)
		if err == nil {
			releaseB()
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("command-b entered before command-a ended: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	if !g.end("w1", "command-a") {
		t.Fatal("command-a lease did not release")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("command-b did not acquire after release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("command-b stayed blocked after command-a released")
	}
}

func TestWindowTransactionIsolationAndCrashExpiry(t *testing.T) {
	g := newWindowTransactionGuard()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	releaseA, err := g.acquire(context.Background(), "w1", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	releaseA()

	// A different EasyEDA window is independent.
	releaseOther, err := g.acquire(context.Background(), "w2", "b", true)
	if err != nil {
		t.Fatalf("other window was blocked: %v", err)
	}
	releaseOther()

	// Simulate a crashed command that never sent transaction.release.
	now = now.Add(windowTransactionIdleTimeout + time.Second)
	releaseB, err := g.acquire(context.Background(), "w1", "b", true)
	if err != nil {
		t.Fatalf("expired crashed lease was not reclaimed: %v", err)
	}
	releaseB()
}

func TestLegacyActionLeaseDoesNotPersist(t *testing.T) {
	g := newWindowTransactionGuard()
	release, err := g.acquire(context.Background(), "w1", "request-1", false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	release2, err := g.acquire(ctx, "w1", "request-2", false)
	if err != nil {
		t.Fatalf("legacy action-scoped lease persisted: %v", err)
	}
	release2()
}
