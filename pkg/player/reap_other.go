//go:build !windows && !linux

package player

// daemons finds nothing on the remaining targets (darwin, the BSDs): there is
// no /proc to read and no stdlib call that lists another process's argv. Down
// still quits the daemon that owns the endpoint; only strays go unreaped here.
func daemons(string) []int { return nil }
