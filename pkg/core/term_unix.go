//go:build linux || darwin || freebsd || netbsd || openbsd

package core

import (
	"os"
	"syscall"
	"unsafe"
)

// winsize mirrors struct winsize from <sys/ioctl.h>. Only Col is read, but the
// whole struct has to be passed or the kernel writes past our buffer.
type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

// terminalWidth asks the tty how wide it is, returning 0 when stdout is not one.
//
// Done with a raw ioctl rather than golang.org/x/term to keep the dependency
// set at one module (see table.go for why that matters here). The
// uintptr(unsafe.Pointer(&ws)) form is the sanctioned syscall idiom — vet's
// unsafeptr check permits it precisely in a syscall argument, because the
// pointer cannot be moved across the call.
func terminalWidth() int {
	var ws winsize
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return 0
	}
	return int(ws.Col)
}
