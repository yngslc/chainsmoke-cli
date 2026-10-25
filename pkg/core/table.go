package core

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	tableIndent   = 2
	tableGap      = 2
	minTitleWidth = 16
	maxUploader   = 22
	defaultWidth  = 80
)

// PrintResults writes the numbered result set.
func (a *App) PrintResults(ts []Track) {
	w := a.out()
	RenderResults(w, ts, OutputWidth(), w == os.Stdout && StdoutIsTerminal())
}

// RenderResults draws the result set at the given width.
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

func renderTSV(w io.Writer, ts []Track) {
	for i, t := range ts {
		dur := ""
		if t.Duration > 0 {
			dur = strconv.Itoa(t.Duration)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", i+1, Clean(t.Title), Clean(t.Uploader), dur)
	}
}

func renderTable(w io.Writer, ts []Track, width int) {
	if width <= 0 {
		width = defaultWidth
	}

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
		titleW = max(titleW, displayWidth(Clean(t.Title)))
		if hasUploader {
			uploaderW = max(uploaderW, displayWidth(Clean(t.Uploader)))
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

	rows := make([]string, 0, len(ts)+1)
	rows = append(rows, row("#", "TITLE", "UPLOADER", "TIME"))
	for i, t := range ts {
		rows = append(rows, row(strconv.Itoa(i+1), Clean(t.Title), Clean(t.Uploader), FormatDuration(t.Duration)))
	}

	rule := 0
	for _, r := range rows {
		rule = max(rule, displayWidth(strings.TrimRight(r, " ")))
	}

	fmt.Fprintln(w, strings.TrimRight(rows[0], " "))
	fmt.Fprintf(w, "%s%s\n", strings.Repeat(" ", tableIndent),
		strings.Repeat("─", max(1, rule-tableIndent)))
	for _, r := range rows[1:] {
		fmt.Fprintln(w, strings.TrimRight(r, " "))
	}
}

// FormatDuration renders seconds the way a music player does.
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

// StdoutIsTerminal mirrors StdinIsTerminal for stdout.
func StdoutIsTerminal() bool { return isTerminal(os.Stdout) }

// Clean strips the characters that would break a row rather than appear in it.
func Clean(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t', r == '\n', r == '\r', r == 0x85, r == 0x2028, r == 0x2029:
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			continue
		case r >= 0x80 && r <= 0x9f:
			continue
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
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
	case r >= 0x0300 && r <= 0x036f,
		r >= 0x0483 && r <= 0x0489,
		r >= 0x0591 && r <= 0x05bd,
		r >= 0x0610 && r <= 0x061a,
		r >= 0x064b && r <= 0x065f,
		r >= 0x0e31 && r <= 0x0e3a,
		r >= 0x1ab0 && r <= 0x1aff,
		r >= 0x1dc0 && r <= 0x1dff,
		r >= 0x20d0 && r <= 0x20f0,
		r >= 0xfe00 && r <= 0xfe0f,
		r >= 0xfe20 && r <= 0xfe2f,
		r == 0x200b, r == 0x200c, r == 0x200d:
		return true
	}
	return false
}

func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f,
		r >= 0x2e80 && r <= 0x303e,
		r >= 0x3041 && r <= 0x33ff,
		r >= 0x3400 && r <= 0x4dbf,
		r >= 0x4e00 && r <= 0x9fff,
		r >= 0xa000 && r <= 0xa4cf,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe10 && r <= 0xfe19,
		r >= 0xfe30 && r <= 0xfe6f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f,
		r >= 0x1f680 && r <= 0x1f6ff,
		r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd:
		return true
	}
	return false
}

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
		if n+rw > max-1 {
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
