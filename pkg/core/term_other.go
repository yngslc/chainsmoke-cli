//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd

package core

import "os"

func terminalWidth() int { return 0 }

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
