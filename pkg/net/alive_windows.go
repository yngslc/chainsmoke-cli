package net

import (
	"errors"
	"syscall"
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259 // STILL_ACTIVE: GetExitCodeProcess of a running process

	// errorInvalidParameter is what OpenProcess returns for a pid nobody holds.
	errorInvalidParameter syscall.Errno = 87
)

// processAlive reports whether pid is a running process. Only "no such
// process" counts as dead: one we may not open (another user's) is alive as
// far as the sweep is concerned, and so is anything this cannot read.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, errorInvalidParameter)
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive // an exited process lingers while someone holds a handle
}
