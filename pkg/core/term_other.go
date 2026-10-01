//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd

package core

import "os"

// terminalWidth has no portable answer on the remaining targets (plan9, js,
// wasip1), so it says so and lets the caller fall back to $COLUMNS or the
// 80-column default. Returning a guess here would be worse than admitting
// ignorance: the table would silently truncate titles to fit a width nothing
// actually has.
func terminalWidth() int { return 0 }

// isTerminal falls back to the character-device bit, the only answer the
// stdlib gives here. It over-counts, since a null device is one too.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
