//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd

package net

// processAlive cannot tell on the remaining targets (plan9, js, wasip1), so it
// says alive: the sweep then leaves every dir alone rather than guess.
func processAlive(int) bool { return true }
