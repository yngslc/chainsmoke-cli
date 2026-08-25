package core

import (
	"os"
	"syscall"
	"unsafe"
)

// The console API, reached through kernel32 rather than golang.org/x/sys, for
// the same reason as the unix side: one dependency is a property worth keeping.
var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

type coord struct{ X, Y int16 }

type smallRect struct{ Left, Top, Right, Bottom int16 }

// consoleScreenBufferInfo mirrors CONSOLE_SCREEN_BUFFER_INFO. Field order and
// widths are load-bearing: the kernel fills this by offset.
type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// terminalWidth returns the console's VISIBLE width, or 0 when stdout is not a
// console.
//
// Window, not Size: a Windows console's screen BUFFER is routinely wider than
// the window showing it (the classic 80x300 scrollback default is the other
// axis, but width can differ too). Formatting to the buffer width would produce
// rows the user has to scroll sideways to read.
func terminalWidth() int {
	var info consoleScreenBufferInfo
	r, _, _ := procGetConsoleScreenBufferInfo.Call(
		os.Stdout.Fd(),
		uintptr(unsafe.Pointer(&info)),
	)
	if r == 0 {
		return 0
	}
	return int(info.Window.Right-info.Window.Left) + 1
}
