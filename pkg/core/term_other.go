//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd

package core

// terminalWidth has no portable answer on the remaining targets (plan9, js,
// wasip1), so it says so and lets the caller fall back to $COLUMNS or the
// 80-column default. Returning a guess here would be worse than admitting
// ignorance: the table would silently truncate titles to fit a width nothing
// actually has.
func terminalWidth() int { return 0 }
