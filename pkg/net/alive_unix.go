//go:build linux || darwin || freebsd || netbsd || openbsd

package net

import (
	"errors"
	"syscall"
)

func processAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
