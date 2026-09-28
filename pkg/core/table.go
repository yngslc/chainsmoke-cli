package core

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// -----------------------------------------------------------------------------
// The results table (TASKS.md T8.1)
// -----------------------------------------------------------------------------
//
// Written against the standard library rather than tablewriter, which the task
// named. Three reasons, in order of weight:
//
//  1. Cost. `go get github.com/olekukonko/tablewriter@latest` pulls EIGHT
//     modules — tablewriter plus olekukonko/{cat,errors,ll}, mattn/{go-isatty,
//     go-runewidth}, olekukonko/ts and golang.org/x/sys. One of them is a
//     logging library. This project ships a single static binary to people who
//     chose it for its threat model, and its dependency set is one module.
//     Eight more for a results table is the wrong trade.
//  2. Lifespan. TASKS.md T10.12 already says the TUI gets its own table widget
//     and that the two must share DATA, not formatting. A dependency whose only
//     caller is superseded three phases later is a dependency taken on twice.
//  3. It does not solve the actual problem. The hard part here is not drawing
//     borders, it is deciding what to CUT when a title is longer than the
//     terminal — and that needs display-width arithmetic either way.
//
// What this gives up is exact East Asian width data (see runeWidth). The swap
// back is contained: RenderResults is the only function that would change.

// Layout constants. Everything else is derived from the data.
const (
	tableIndent   = 2  // left margin, so the table is not flush against the edge
	tableGap      = 2  // between columns
	minTitleWidth = 16 // below this a title is unreadable; drop a column instead
	maxUploader   = 22 // artist names are long; the title is the more useful column
	defaultWidth  = 80 // when nothing can tell us how wide the terminal is
)

// PrintResults writes the numbered result set.
//
// Index, title, uploader and duration only — the permalink stays in the store
// so it never reaches a terminal, a scrollback buffer, or a shell history
// (invariant #3). `play N` is what consumes the index, so truncating a title
// for display costs the user nothing functional.
func (a *App) PrintResults(ts []Track) {
	w := a.out()
	RenderResults(w, ts, OutputWidth(), w == os.Stdout && StdoutIsTerminal())
}

// RenderResults draws the result set at the given width.
//
// tty picks the format, and the split is deliberate: a terminal gets an aligned,
// truncated, header-topped table, while a pipe gets tab-separated fields with no
// header and no truncation. `chainsmoke search x | head -3` and
// `... | cut -f2` are real uses, and a box-drawn table serves neither. Width is
// meaningless down a pipe anyway.
func RenderResults(w io.Writer, ts []Track, width int, tty bool) {
	if len(ts) == 0 {
		return
	}
	if !tty {
		renderTSV(w, ts)
		return
	}
	renderTable(w, ts, width)
}

// renderTSV is the machine-readable form: index, title, uploader, duration in
// SECONDS. Seconds rather than m:ss because a script should not have to parse
// our formatting, and an EMPTY field rather than 0 because "SoundCloud did not
// tell us" and "zero seconds long" are different facts.
func renderTSV(w io.Writer, ts []Track) {
	for i, t := range ts {
		dur := ""
		if t.Duration > 0 {
			dur = strconv.Itoa(t.Duration)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", i+1, clean(t.Title), clean(t.Uploader), dur)
	}
}

func renderTable(w io.Writer, ts []Track, width int) {
	if width <= 0 {
		width = defaultWidth
	}

	// Column presence is decided by the DATA, not by a flag. Measured
	// 2026-08-24 through Tor: `scsearch` entries carry uploader and duration,
	// but user pages (/likes, /tracks) come back from --flat-playlist with only
	// id, title and url. Printing two columns of blanks for every row of a
	// `likes` listing would be noise that costs the title its space.
	var hasUploader, hasDuration bool
	for _, t := range ts {
		if strings.TrimSpace(t.Uploader) != "" {
			hasUploader = true
		}
		if t.Duration > 0 {
			hasDuration = true
		}
	}

	idxW := len(strconv.Itoa(len(ts)))
	if idxW < 1 {
		idxW = 1
	}

	titleW, uploaderW, timeW := 0, 0, 0
	for _, t := range ts {
		titleW = max(titleW, displayWidth(clean(t.Title)))
		if hasUploader {
			uploaderW = max(uploaderW, displayWidth(clean(t.Uploader)))
		}
		if hasDuration {
			timeW = max(timeW, displayWidth(FormatDuration(t.Duration)))
		}
	}
	uploaderW = min(uploaderW, maxUploader)
	if hasUploader {
		uploaderW = max(uploaderW, len("UPLOADER"))
	}
	if hasDuration {
		timeW = max(timeW, len("TIME"))
	}

	// Fit, then degrade. The order is a claim about what a user needs: the
	// index and the title are the whole point (one identifies the track, the
	// other is what `play N` acts on), the duration is cheap at five cells, and
	// the uploader is the first thing worth losing.
	fixed := func() int {
		n := tableIndent + idxW + tableGap
		if hasUploader {
			n += uploaderW + tableGap
		}
		if hasDuration {
			n += timeW + tableGap
		}
		return n
	}
	for titleW+fixed() > width {
		switch {
		case titleW > minTitleWidth:
			titleW = max(minTitleWidth, width-fixed())
		case hasUploader:
			hasUploader = false
		case hasDuration:
			hasDuration = false
		default:
			titleW = max(1, width-fixed())
		}
		if titleW+fixed() <= width {
			break
		}
		if !hasUploader && !hasDuration && titleW <= minTitleWidth {
			titleW = max(1, width-fixed())
			break
		}
	}

	row := func(idx, title, uploader, dur string) string {
		var b strings.Builder
		b.WriteString(strings.Repeat(" ", tableIndent))
		b.WriteString(padLeft(idx, idxW))
		b.WriteString(strings.Repeat(" ", tableGap))
		b.WriteString(padRight(truncate(title, titleW), titleW))
		if hasUploader {
			b.WriteString(strings.Repeat(" ", tableGap))
			b.WriteString(padRight(truncate(uploader, uploaderW), uploaderW))
		}
		if hasDuration {
			b.WriteString(strings.Repeat(" ", tableGap))
			b.WriteString(padLeft(dur, timeW))
		}
		return b.String()
	}

	// Build first, measure, then print. The rule has to span the widest row that
	// actually exists, not the width the layout was allowed — with a short
	// result set those differ, and a rule running past the last column looks
	// like a rendering bug.
	rows := make([]string, 0, len(ts)+1)
	rows = append(rows, row("#", "TITLE", "UPLOADER", "TIME"))
	for i, t := range ts {
		rows = append(rows, row(strconv.Itoa(i+1), clean(t.Title), clean(t.Uploader), FormatDuration(t.Duration)))
	}

	rule := 0
	for _, r := range rows {
		rule = max(rule, displayWidth(strings.TrimRight(r, " ")))
	}

	// Trailing padding is trimmed on the way out so copy-pasting a row does not
	// pick up a tail of spaces. It has to survive until here, though: the
	// columns are aligned by it.
	fmt.Fprintln(w, strings.TrimRight(rows[0], " "))
	fmt.Fprintf(w, "%s%s\n", strings.Repeat(" ", tableIndent),
		strings.Repeat("─", max(1, rule-tableIndent)))
	for _, r := range rows[1:] {
		fmt.Fprintln(w, strings.TrimRight(r, " "))
	}
}

// FormatDuration renders seconds the way a music player does. An unknown
// duration is "--", never "0:00": SoundCloud not telling us how long a track is
// must not read as a zero-length track.
func FormatDuration(sec int) string {
	if sec <= 0 {
		return "--"
	}
	h, m, s := sec/3600, (sec%3600)/60, sec%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// OutputWidth reports how many columns are available for output.
//
// $COLUMNS first, because a user who exported it is stating an intent that
// beats anything we can measure — and because it is the only knob a test or a
// script has. Then the real terminal. Then 80, the width every terminal is at
// least as wide as.
func OutputWidth() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if n := terminalWidth(); n > 0 {
		return n
	}
	return defaultWidth
}

// StdoutIsTerminal mirrors StdinIsTerminal: the character-device bit, so this
// needs no dependency and answers the same on Windows and POSIX.
func StdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// -----------------------------------------------------------------------------
// Width arithmetic
// -----------------------------------------------------------------------------

// clean strips the characters that would break a row rather than appear in it.
// Track titles are attacker-influenced text from a third party: a tab would
// wreck the TSV form, a newline would forge an extra row, and an ANSI escape
// would let a title repaint the terminal. None of them belong in a cell.
func clean(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t', r == '\n', r == '\r':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f: // control chars, ESC included
			continue
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// displayWidth is how many terminal cells s occupies — not len(), which counts
// bytes, and not utf8.RuneCountInString, which counts a CJK ideograph as one
// cell when it draws as two.
func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// runeWidth approximates East Asian Width.
//
// APPROXIMATES is the honest word: the exact answer is a Unicode table that
// mattn/go-runewidth maintains and that changes every Unicode release. This
// covers the ranges that actually appear in SoundCloud titles — CJK, kana,
// Hangul, fullwidth forms and the common emoji blocks — plus the combining
// marks that occupy no cell of their own. Latin, Cyrillic and Greek are exact.
//
// The failure mode is bounded and cosmetic: an unlisted wide rune makes one row
// two cells short, which shows up as a ragged column, never as a wrong result
// or a leaked URL. If exactness ever matters more than the dependency count,
// this function is the seam — replace its body with runewidth.RuneWidth.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0): // C0/C1 control
		return 0
	case isCombining(r):
		return 0
	case isWide(r):
		return 2
	}
	return 1
}

func isCombining(r rune) bool {
	switch {
	case r >= 0x0300 && r <= 0x036f, // combining diacriticals
		r >= 0x0483 && r <= 0x0489, // Cyrillic combining
		r >= 0x0591 && r <= 0x05bd, // Hebrew points
		r >= 0x0610 && r <= 0x061a, // Arabic marks
		r >= 0x064b && r <= 0x065f,
		r >= 0x0e31 && r <= 0x0e3a, // Thai vowels above/below
		r >= 0x1ab0 && r <= 0x1aff,
		r >= 0x1dc0 && r <= 0x1dff,
		r >= 0x20d0 && r <= 0x20f0, // combining symbols
		r >= 0xfe00 && r <= 0xfe0f, // variation selectors
		r >= 0xfe20 && r <= 0xfe2f,
		r == 0x200b, r == 0x200c, r == 0x200d: // zero-width space/NJ/joiner
		return true
	}
	return false
}

func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo
		r >= 0x2e80 && r <= 0x303e, // CJK radicals, Kangxi, CJK symbols
		r >= 0x3041 && r <= 0x33ff, // kana, Bopomofo, Hangul compat, CJK compat
		r >= 0x3400 && r <= 0x4dbf, // CJK ext A
		r >= 0x4e00 && r <= 0x9fff, // CJK unified
		r >= 0xa000 && r <= 0xa4cf, // Yi
		r >= 0xac00 && r <= 0xd7a3, // Hangul syllables
		r >= 0xf900 && r <= 0xfaff, // CJK compat ideographs
		r >= 0xfe10 && r <= 0xfe19,
		r >= 0xfe30 && r <= 0xfe6f, // CJK compat forms
		r >= 0xff00 && r <= 0xff60, // fullwidth forms
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f, // emoji: symbols/pictographs, emoticons
		r >= 0x1f680 && r <= 0x1f6ff, // transport & map
		r >= 0x1f900 && r <= 0x1f9ff, // supplemental symbols
		r >= 0x20000 && r <= 0x3fffd: // CJK ext B and beyond
		return true
	}
	return false
}

// truncate cuts s to at most max CELLS, marking the cut with a single-cell "…"
// so a clipped title is visibly clipped rather than quietly wrong.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if displayWidth(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	n := 0
	var b strings.Builder
	for _, r := range s {
		rw := runeWidth(r)
		if n+rw > max-1 { // leave a cell for the ellipsis
			break
		}
		b.WriteRune(r)
		n += rw
	}
	return b.String() + "…"
}

func padRight(s string, w int) string {
	if pad := w - displayWidth(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

func padLeft(s string, w int) string {
	if pad := w - displayWidth(s); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}
