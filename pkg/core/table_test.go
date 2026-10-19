package core

import (
	"bytes"
	"strings"
	"testing"
)

func render(ts []Track, width int, tty bool) string {
	var b bytes.Buffer
	RenderResults(&b, ts, width, tty)
	return b.String()
}

// lines returns the rendered rows with the horizontal rule dropped, since the
// rule is decoration and every other assertion is about the columns.
func lines(out string) []string {
	var got []string
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(strings.ReplaceAll(ln, "─", "")) == "" && strings.Contains(ln, "─") {
			continue
		}
		got = append(got, ln)
	}
	return got
}

// TestTableColumnsAlign is the load-bearing one, and it is why the width
// arithmetic exists at all. TIME is the last column and right-aligned, so with
// correct display-width math EVERY row ends at exactly the same cell — a
// property that survives being read by a machine, unlike squinting at output.
//
// The CJK and emoji rows are the point: len() and RuneCountInString both get
// them wrong, in opposite directions.
func TestTableColumnsAlign(t *testing.T) {
	ts := []Track{
		{Title: "Lemon Black", Uploader: "Roshina", Duration: 68},
		{Title: "夜に駆ける / Yoru ni Kakeru", Uploader: "YOASOBI", Duration: 261},
		{Title: "🎧 lofi beats", Uploader: "chill", Duration: 3600},
		{Title: "Ünïcödé Ħeavy", Uploader: "Åmbient", Duration: 5},
		{Title: "A title long enough that it has to be cut somewhere", Uploader: "x", Duration: 4021},
	}
	for _, width := range []int{120, 80, 60, 44, 30} {
		got := lines(render(ts, width, true))
		if len(got) < 2 {
			t.Fatalf("width %d: got %d lines, want a header plus rows", width, len(got))
		}
		want := displayWidth(got[0])
		for i, ln := range got {
			w := displayWidth(ln)
			if w != want {
				t.Errorf("width %d: line %d ends at cell %d, want %d (columns are misaligned)\n%s",
					width, i, w, want, strings.Join(got, "\n"))
			}
			if w > width {
				t.Errorf("width %d: line %d is %d cells wide — it will wrap", width, i, w)
			}
		}
	}
}

// TestTableDropsColumnsTheDataDoesNotHave. Measured 2026-08-24 through Tor:
// scsearch entries carry uploader and duration, but user pages (/likes,
// /tracks) come back from --flat-playlist with only id, title and url. Two
// columns of blanks on every row of a `likes` listing would be noise paid for
// out of the title's width.
func TestTableDropsColumnsTheDataDoesNotHave(t *testing.T) {
	likes := []Track{
		{Title: "Copper Chantilly"},
		{Title: "Like A Star X Bicycle (Changing Currents Mashup)"},
	}
	out := render(likes, 100, true)
	if strings.Contains(out, "UPLOADER") {
		t.Errorf("UPLOADER column kept with no uploader in any row:\n%s", out)
	}
	if strings.Contains(out, "TIME") {
		t.Errorf("TIME column kept with no duration in any row:\n%s", out)
	}
	if !strings.Contains(out, "Copper Chantilly") {
		t.Errorf("titles missing:\n%s", out)
	}

	// One row with a duration is enough to earn the column back — and the rows
	// without one say "--" rather than "0:00".
	mixed := append([]Track{{Title: "Known", Duration: 90}}, likes...)
	out = render(mixed, 100, true)
	if !strings.Contains(out, "TIME") {
		t.Errorf("TIME column dropped even though a row has a duration:\n%s", out)
	}
	if !strings.Contains(out, "--") {
		t.Errorf("unknown duration not rendered as --:\n%s", out)
	}
}

// TestTableNeverPrintsThePermalink pins invariant #3 at the last place a URL
// could escape into a terminal, a scrollback buffer, or a screenshot. The same
// shape of test guards the player's signed URLs.
func TestTableNeverPrintsThePermalink(t *testing.T) {
	const secret = "https://soundcloud.com/ghostinnafurcoat/lemon-black"
	ts := []Track{{Title: "Lemon Black", Uploader: "Roshina", Duration: 68, Permalink: secret, ID: "2386417872"}}

	for _, tty := range []bool{true, false} {
		out := render(ts, 100, tty)
		if strings.Contains(out, secret) || strings.Contains(out, "soundcloud.com") {
			t.Errorf("tty=%v: permalink reached the terminal:\n%s", tty, out)
		}
	}
}

// TestCleanRejectsHostileTitles: titles are third-party text. A newline would
// forge an extra numbered row (pointing `play N` at a track the user never
// saw), a tab would wreck the TSV form, and an ESC would let a track title
// repaint the terminal.
func TestCleanRejectsHostileTitles(t *testing.T) {
	ts := []Track{{
		Title:    "Innocent\n  9  Totally Legit Track",
		Uploader: "a\tb",
		Duration: 60,
	}}

	out := render(ts, 100, true)
	if n := len(lines(out)); n != 2 { // header + exactly one row
		t.Errorf("a newline in a title forged rows: got %d lines\n%s", n, out)
	}

	tsv := render(ts, 100, false)
	if got := strings.Count(strings.TrimRight(tsv, "\n"), "\t"); got != 3 {
		t.Errorf("TSV row has %d tabs, want exactly 3 — a tab in a field broke the format:\n%q", got, tsv)
	}

	esc := render([]Track{{Title: "red \x1b[31mALERT\x1b[0m", Duration: 1}}, 100, true)
	if strings.ContainsRune(esc, 0x1b) {
		t.Errorf("ESC survived into the output — a title can repaint the terminal:\n%q", esc)
	}

	// Past ASCII (2026-10-19 review, CQ-5): CSI's one-character form, a bidi
	// override that reorders the row, and Unicode's own line separator.
	for _, hostile := range []string{"a\u009b31mred", "song ‮kcart‬", "one two", "one\u0085two"} {
		got := Clean(hostile)
		if strings.ContainsAny(got, "\u009b‮‬ \u0085") {
			t.Errorf("Clean(%q) = %q, a control survived", hostile, got)
		}
	}
	if got := Clean("שלום‏ world"); !strings.ContainsRune(got, 0x200f) {
		t.Errorf("Clean dropped an RLM mark, which right-to-left titles need: %q", got)
	}
}

// TestRenderTSVIsStable: a pipe gets the same four fields every time, whatever
// the data looks like, because a script that has to detect the column count is
// a script that breaks.
func TestRenderTSVIsStable(t *testing.T) {
	ts := []Track{
		{Title: "Lemon Black", Uploader: "Roshina", Duration: 68},
		{Title: "No Metadata"},
	}
	out := strings.Split(strings.TrimRight(render(ts, 100, false), "\n"), "\n")
	if len(out) != 2 {
		t.Fatalf("got %d lines, want 2 (no header down a pipe)", len(out))
	}
	if out[0] != "1\tLemon Black\tRoshina\t68" {
		t.Errorf("row 1 = %q", out[0])
	}
	// Empty, not 0: "SoundCloud did not say" is not "zero seconds long".
	if out[1] != "2\tNo Metadata\t\t" {
		t.Errorf("row 2 = %q, want empty uploader and empty duration", out[1])
	}
	if strings.Contains(render(ts, 100, false), "─") {
		t.Error("the horizontal rule leaked into piped output")
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[int]string{
		-1: "--", 0: "--", // unknown, never "0:00"
		5: "0:05", 68: "1:08", 599: "9:59", 600: "10:00",
		3599: "59:59", 3600: "1:00:00", 4021: "1:07:01", 36000: "10:00:00",
	}
	for sec, want := range cases {
		if got := FormatDuration(sec); got != want {
			t.Errorf("FormatDuration(%d) = %q, want %q", sec, got, want)
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := map[string]int{
		"":        0,
		"abc":     3,
		"夜に駆ける":   10, // 5 wide runes
		"🎧":       2,
		"é":      1, // combining acute adds no cell
		"​hidden": 6, // zero-width space
		"Ünïcödé": 7,
		"ＡＢ":      4, // fullwidth latin
	}
	for s, want := range cases {
		if got := displayWidth(s); got != want {
			t.Errorf("displayWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

// TestTruncateNeverExceedsItsBudget: the cut has to land on a rune boundary AND
// respect cell width, or a CJK title clipped mid-character overflows the column
// and breaks every row below it.
func TestTruncateNeverExceedsItsBudget(t *testing.T) {
	inputs := []string{
		"short",
		"a much longer ascii string that needs cutting",
		"夜に駆ける / Yoru ni Kakeru",
		"🎧🎧🎧🎧🎧🎧🎧🎧",
		"ｆｕｌｌｗｉｄｔｈ",
	}
	for _, s := range inputs {
		for max := 0; max <= 20; max++ {
			got := truncate(s, max)
			if w := displayWidth(got); w > max {
				t.Errorf("truncate(%q, %d) = %q (%d cells) — over budget", s, max, got, w)
			}
			if max >= displayWidth(s) && got != s {
				t.Errorf("truncate(%q, %d) = %q — cut a string that already fit", s, max, got)
			}
		}
	}
	if got := truncate("abcdef", 3); got != "ab…" {
		t.Errorf("truncate = %q, want %q — the cut must be visible", got, "ab…")
	}
}

func TestOutputWidthPrefersColumns(t *testing.T) {
	t.Setenv("COLUMNS", "137")
	if got := OutputWidth(); got != 137 {
		t.Errorf("OutputWidth() = %d, want 137 — an exported COLUMNS is an explicit intent", got)
	}
	// Garbage must fall through to detection/default rather than to zero, which
	// would render a one-cell-wide table.
	t.Setenv("COLUMNS", "not-a-number")
	if got := OutputWidth(); got <= 0 {
		t.Errorf("OutputWidth() = %d with a malformed COLUMNS, want a usable width", got)
	}
}

// TestRenderEmptyIsSilent: `search` errors before it can hand us nothing, but a
// panic here would take out the whole verb for a case that costs one line.
func TestRenderEmptyIsSilent(t *testing.T) {
	if out := render(nil, 80, true); out != "" {
		t.Errorf("rendered %q for an empty set", out)
	}
	if out := render([]Track{}, 80, false); out != "" {
		t.Errorf("rendered %q for an empty set", out)
	}
}
