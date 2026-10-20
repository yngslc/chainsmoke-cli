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

	mixed := append([]Track{{Title: "Known", Duration: 90}}, likes...)
	out = render(mixed, 100, true)
	if !strings.Contains(out, "TIME") {
		t.Errorf("TIME column dropped even though a row has a duration:\n%s", out)
	}
	if !strings.Contains(out, "--") {
		t.Errorf("unknown duration not rendered as --:\n%s", out)
	}
}

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

func TestCleanRejectsHostileTitles(t *testing.T) {
	ts := []Track{{
		Title:    "Innocent\n  9  Totally Legit Track",
		Uploader: "a\tb",
		Duration: 60,
	}}

	out := render(ts, 100, true)
	if n := len(lines(out)); n != 2 {
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
	if out[1] != "2\tNo Metadata\t\t" {
		t.Errorf("row 2 = %q, want empty uploader and empty duration", out[1])
	}
	if strings.Contains(render(ts, 100, false), "─") {
		t.Error("the horizontal rule leaked into piped output")
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[int]string{
		-1: "--", 0: "--",
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
		"夜に駆ける":   10,
		"🎧":       2,
		"é":      1,
		"​hidden": 6,
		"Ünïcödé": 7,
		"ＡＢ":      4,
	}
	for s, want := range cases {
		if got := displayWidth(s); got != want {
			t.Errorf("displayWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

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
	t.Setenv("COLUMNS", "not-a-number")
	if got := OutputWidth(); got <= 0 {
		t.Errorf("OutputWidth() = %d with a malformed COLUMNS, want a usable width", got)
	}
}

func TestRenderEmptyIsSilent(t *testing.T) {
	if out := render(nil, 80, true); out != "" {
		t.Errorf("rendered %q for an empty set", out)
	}
	if out := render([]Track{}, 80, false); out != "" {
		t.Errorf("rendered %q for an empty set", out)
	}
}
