package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const windowTransactionIdleTimeout = 30 * time.Second

type windowTransactionLease struct {
	owner        string
	inFlight     bool
	lastActivity time.Time
}

// windowTransactionGuard provides command-level serialization per EasyEDA
// window. A lease survives the gaps between actions in one CLI command, which
// is the part an action-level mutex cannot protect.
type windowTransactionGuard struct {
	mu     sync.Mutex
	leases map[string]windowTransactionLease
	now    func() time.Time
}

func newWindowTransactionGuard() *windowTransactionGuard {
	return &windowTransactionGuard{leases: map[string]windowTransactionLease{}, now: time.Now}
}

// acquire waits until owner may run one action on windowID. transactional=false
// creates an action-scoped lease for raw/legacy callers and removes it on return.
func (g *windowTransactionGuard) acquire(
	ctx context.Context,
	windowID, owner string,
	transactional bool,
) (func(), error) {
	if g == nil || windowID == "" || owner == "" {
		return func() {}, nil
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		now := g.now()
		g.mu.Lock()
		lease, exists := g.leases[windowID]
		expired := exists && !lease.inFlight && now.Sub(lease.lastActivity) >= windowTransactionIdleTimeout
		if !exists || expired {
			g.leases[windowID] = windowTransactionLease{owner: owner, inFlight: true, lastActivity: now}
			g.mu.Unlock()
			return g.releaseAction(windowID, owner, transactional), nil
		}
		if lease.owner == owner && !lease.inFlight {
			lease.inFlight = true
			lease.lastActivity = now
			g.leases[windowID] = lease
			g.mu.Unlock()
			return g.releaseAction(windowID, owner, transactional), nil
		}
		g.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("window %s is busy with transaction %s: %w", windowID, lease.owner, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (g *windowTransactionGuard) releaseAction(windowID, owner string, transactional bool) func() {
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		lease, ok := g.leases[windowID]
		if !ok || lease.owner != owner {
			return
		}
		if !transactional {
			delete(g.leases, windowID)
			return
		}
		lease.inFlight = false
		lease.lastActivity = g.now()
		g.leases[windowID] = lease
	}
}

func (g *windowTransactionGuard) end(windowID, owner string) bool {
	if g == nil || windowID == "" || owner == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	lease, ok := g.leases[windowID]
	if !ok || lease.owner != owner || lease.inFlight {
		return false
	}
	delete(g.leases, windowID)
	return true
}
