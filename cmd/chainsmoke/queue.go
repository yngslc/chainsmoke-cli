package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"

	"chainsmoke/pkg/core"
)

type listQueue struct {
	app     *core.App
	list    []core.Track
	entries []listEntry
	shown   int
	shuffle bool
	order   []int
	repeat  string

	cursor  int
	loading chan preloaded
	pending listEntry
	skips   int
}

type listEntry struct {
	n     int
	title string
}

type preloaded struct {
	entry listEntry
	url   string
	err   error
}

func (q *listQueue) start(n int) error {
	list, err := q.snapshotFrom(q.app.Store, n)
	if err != nil {
		return err
	}
	return q.playAt(list, n)
}

func (q *listQueue) startList(list []core.Track) error {
	if len(list) == 0 {
		return errors.New("nothing resolved")
	}
	list = slices.Clone(list)
	for n := 1; ; n++ {
		err := q.playAt(list, n)
		if err == nil || n == len(list) || n == 3 {
			return err
		}
		fmt.Fprintf(os.Stderr, "chainsmoke: skipping %d. %s: %v\n", n, core.Clean(list[n-1].Title), err)
	}
}

func (q *listQueue) playAt(list []core.Track, n int) error {
	t := list[n-1]
	url, err := q.app.Res.Stream(t.Permalink)
	if err != nil {
		return core.YtdlpHint(err)
	}
	if err := q.app.Play.Enqueue(url, t.Title); err != nil {
		return err
	}
	if err := q.switchToLast(); err != nil {
		return err
	}
	q.list = list
	q.entries = []listEntry{{n, t.Title}}
	q.cursor, q.skips, q.loading = n, 0, nil
	q.order = nil
	if q.shuffle {
		q.reshuffle(false)
	}
	q.preload()
	q.announce(0)
	return nil
}

func (q *listQueue) switchToLast() error {
	v, err := q.app.Play.Command("get_property", "playlist-count")
	count, ok := v.(float64)
	if err != nil || !ok || count < 1 {
		return fmt.Errorf("mpv: playlist-count unreadable (%v)", err)
	}
	last := int(count) - 1
	if pos, _ := q.app.Play.Command("get_property", "playlist-pos"); pos != float64(last) {
		if _, err := q.app.Play.Command("playlist-play-index", last); err != nil {
			return err
		}
	}
	if _, err := q.app.Play.Command("playlist-clear"); err != nil {
		return err
	}
	_, err = q.app.Play.Command("set_property", "pause", false)
	return err
}

func (q *listQueue) snapshotFrom(s core.Store, n int) ([]core.Track, error) {
	list, err := s.Results()
	if err != nil {
		return nil, err
	}
	if n < 1 || n > len(list) {
		if _, err := s.Result(n); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("no cached result #%d", n)
	}
	return slices.Clone(list), nil
}

func (q *listQueue) pos() (int, bool) {
	if len(q.entries) == 0 {
		return 0, false
	}
	v, err := q.app.Play.Command("get_property", "playlist-pos")
	f, ok := v.(float64)
	i := int(f)
	return i, err == nil && ok && i >= 0 && i < len(q.entries)
}

func (q *listQueue) following(n int) (int, bool) {
	if q.shuffle {
		if len(q.order) == 0 && q.repeat == "all" {
			q.reshuffle(true)
		}
		if len(q.order) == 0 {
			return 0, false
		}
		next := q.order[0]
		q.order = q.order[1:]
		return next, true
	}
	if n < len(q.list) {
		return n + 1, true
	}
	if q.repeat == "all" && len(q.list) > 0 {
		return 1, true
	}
	return 0, false
}

func (q *listQueue) reshuffle(fresh bool) {
	played := map[int]bool{}
	if !fresh {
		for _, e := range q.entries {
			played[e.n] = true
		}
	}
	q.order = nil
	for n := 1; n <= len(q.list); n++ {
		if !played[n] {
			q.order = append(q.order, n)
		}
	}
	rand.Shuffle(len(q.order), func(i, j int) { q.order[i], q.order[j] = q.order[j], q.order[i] })
}

func (q *listQueue) preload() {
	if q.loading != nil || q.skips >= 3 || len(q.entries) == 0 {
		return
	}
	if i, ok := q.pos(); ok && i < len(q.entries)-1 {
		return
	}
	n, ok := q.following(q.cursor)
	if !ok {
		return
	}
	q.cursor = n
	t := q.list[n-1]
	q.pending = listEntry{n, t.Title}
	ch, res := make(chan preloaded, 1), q.app.Res
	q.loading = ch
	go func(e listEntry, permalink string) {
		url, err := res.Stream(permalink)
		ch <- preloaded{e, url, err}
	}(q.pending, t.Permalink)
}

func (q *listQueue) landed(p preloaded) error {
	q.loading = nil
	if v, _ := q.app.Play.Command("get_property", "playlist-count"); v != float64(len(q.entries)) {
		return nil
	}
	err := p.err
	if err != nil {
		err = core.YtdlpHint(err)
	} else {
		err = q.app.Play.Enqueue(p.url, p.entry.title)
	}
	if err != nil {
		q.skips++
		q.preload()
		return fmt.Errorf("skipping %d. %s: %w", p.entry.n, core.Clean(p.entry.title), err)
	}
	q.skips = 0
	q.entries = append(q.entries, p.entry)
	return nil
}

func (q *listQueue) settle() {
	for q.loading != nil {
		if err := q.landed(<-q.loading); err != nil {
			fmt.Fprintln(os.Stderr, "chainsmoke:", err)
		}
	}
}

func (q *listQueue) follow() (int, bool) {
	i, ok := q.pos()
	if !ok {
		return 0, false
	}
	q.preload()
	return i, i != q.shown
}

func (q *listQueue) next() error {
	i, ok := q.pos()
	if !ok {
		_, err := q.app.Play.Command("playlist-next")
		return err
	}
	if i == len(q.entries)-1 {
		q.skips = 0
		q.preload()
		q.settle()
		if i == len(q.entries)-1 {
			return errors.New("end of the list")
		}
	}
	if _, err := q.app.Play.Command("playlist-next"); err != nil {
		return err
	}
	q.preload()
	q.announce(i + 1)
	return nil
}

func (q *listQueue) prev() error {
	i, ok := q.pos()
	if !ok {
		_, err := q.app.Play.Command("playlist-prev")
		return err
	}
	if i > 0 {
		if _, err := q.app.Play.Command("playlist-prev"); err != nil {
			return err
		}
		q.announce(i - 1)
		return nil
	}
	if q.shuffle {
		return errors.New("nothing before this one in this shuffle")
	}
	if q.entries[0].n == 1 {
		return errors.New("start of the list")
	}
	return q.playAt(q.list, q.entries[0].n-1)
}

func (q *listQueue) setShuffle(on bool) error {
	q.shuffle = on
	i, ok := q.pos()
	if !ok {
		cmd := "playlist-unshuffle"
		if on {
			cmd = "playlist-shuffle"
		}
		_, err := q.app.Play.Command(cmd)
		return err
	}
	for j := len(q.entries) - 1; j > i; j-- {
		if _, err := q.app.Play.Command("playlist-remove", j); err != nil {
			return err
		}
	}
	q.entries = q.entries[:i+1]
	q.cursor, q.skips, q.loading = q.entries[i].n, 0, nil
	q.order = nil
	if on {
		q.reshuffle(false)
	}
	q.preload()
	return nil
}

func (q *listQueue) setRepeat(mode string) error {
	switch mode {
	case "off", "one", "all":
	default:
		return fmt.Errorf("usage: repeat one | all | off (got %q)", mode)
	}
	loop := map[bool]string{true: "inf", false: "no"}
	if _, err := q.app.Play.Command("set_property", "loop-file", loop[mode == "one"]); err != nil {
		return err
	}
	q.repeat = mode
	if _, ok := q.pos(); !ok {
		_, err := q.app.Play.Command("set_property", "loop-playlist", loop[mode == "all"])
		return err
	}
	if mode == "all" {
		q.preload()
	}
	return nil
}

func (q *listQueue) modes() string {
	shuffle, repeat := "off", q.repeat
	if q.shuffle {
		shuffle = "on"
	}
	if repeat == "" {
		repeat = "off"
	}
	return fmt.Sprintf("shuffle %s · repeat %s", shuffle, repeat)
}

func (q *listQueue) announce(i int) {
	w := q.app.Out
	if w == nil {
		w = os.Stdout
	}
	q.shown = i
	e := q.entries[i]
	line := fmt.Sprintf("▶ %d. %s", e.n, core.Clean(e.title))
	after, known := listEntry{}, false
	switch {
	case i+1 < len(q.entries):
		after, known = q.entries[i+1], true
	case q.loading != nil:
		after, known = q.pending, true
	}
	switch {
	case q.repeat == "one":
		line += "  ·  repeating"
	case known:
		line += fmt.Sprintf("  ·  next: %d. %s", after.n, core.Clean(after.title))
	}
	fmt.Fprintln(w, line)
}
