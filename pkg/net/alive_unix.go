//go:build linux || darwin || freebsd || netbsd || openbsd

package net

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid is a running process: signal 0 checks
// without sending anything. Only ESRCH counts as dead — EPERM is another
// user's live process.
func processAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
