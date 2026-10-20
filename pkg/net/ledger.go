package net

import (
	"sync"

	"chainsmoke/pkg/core"
)

type ledger struct {
	mu      sync.Mutex
	targets map[string]int
	refused int
	failed  int
}

func newLedger() *ledger { return &ledger{targets: map[string]int{}} }

func (l *ledger) connect(target string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.targets[target]++
}

func (l *ledger) refuse() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refused++
}

func (l *ledger) fail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed++
}

func (l *ledger) snapshot() core.EgressLedger {
	l.mu.Lock()
	defer l.mu.Unlock()

	targets := make(map[string]int, len(l.targets))
	for k, v := range l.targets {
		targets[k] = v
	}
	return core.EgressLedger{
		Observed: true,
		Targets:  targets,
		Refused:  l.refused,
		Failed:   l.failed,
	}
}

// EgressLedger reports what crossed this bridge. Observed is false when there
// is no bridge to account for — a user-supplied --proxy, or DIRECT mode —
// so that "we could not look" never renders as "nothing left the machine".
func (b *Bridge) EgressLedger() core.EgressLedger {
	if b.cb == nil {
		return core.EgressLedger{Observed: false}
	}
	return b.cb.ledger.snapshot()
}
