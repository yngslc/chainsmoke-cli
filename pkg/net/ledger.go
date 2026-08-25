package net

import (
	"sync"

	"chainsmoke/pkg/core"
)

// ledger accounts for what crossed this bridge (TASKS.md T7.2).
//
// It lives on the bridge rather than beside it because the bridge is the one
// place that sees every tunnel by construction: a CONNECT that does not pass
// through here is not proxied at all, which is the property the accounting is
// supposed to demonstrate.
//
// Deliberately record-only. Refusing an unexpected host would be a stronger
// control and a worse default: a CDN redirect to a domain we have not seen
// before would become silence instead of music, and the tool's first duty is to
// work. Classification lives in core.ExpectedHost, where audit can report drift
// without the bridge enforcing it.
type ledger struct {
	mu      sync.Mutex
	targets map[string]int
	refused int
	failed  int
}

func newLedger() *ledger { return &ledger{targets: map[string]int{}} }

// connect records an attempted tunnel to "host:port".
func (l *ledger) connect(target string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.targets[target]++
}

// refuse records a non-CONNECT request — a child that tried to push plaintext
// through the bridge and was rejected.
func (l *ledger) refuse() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refused++
}

// fail records a CONNECT that never became a tunnel.
func (l *ledger) fail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed++
}

// snapshot copies the ledger for reporting. The map is copied rather than
// shared: audit formats it while the bridge may still be serving.
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
// is no bridge to account for — a user-supplied --proxy, or DIRECT mode — so
// that "we could not look" never renders as "nothing left the machine".
func (b *Bridge) EgressLedger() core.EgressLedger {
	if b.cb == nil {
		return core.EgressLedger{Observed: false}
	}
	return b.cb.ledger.snapshot()
}
