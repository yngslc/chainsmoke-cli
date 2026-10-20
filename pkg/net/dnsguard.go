package net

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

var probeName = "example.com"

const dnsProbeTimeout = 2 * time.Second

// CheckLocalDNSVisibility asks whether this host's resolver can see a name.
func CheckLocalDNSVisibility(ctx context.Context, host string) (visible bool, detail string) {
	ctx, cancel := context.WithTimeout(ctx, dnsProbeTimeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	switch {
	case err != nil:
		return false, fmt.Sprintf("%-16s not resolvable locally — nothing to observe", host)
	case len(addrs) == 0:
		return false, fmt.Sprintf("%-16s no local answer", host)
	default:
		shown := addrs
		if len(shown) > 2 {
			shown = shown[:2]
		}
		return true, fmt.Sprintf("%-16s resolves locally (%s) — a bypass here would be visible to your resolver",
			host, strings.Join(shown, ", "))
	}
}

// AuditDNS probes the local resolver with probeName.
func AuditDNS(ctx context.Context) (visible bool, lines []string) {
	v, detail := CheckLocalDNSVisibility(ctx, probeName)
	return v, []string{detail}
}
