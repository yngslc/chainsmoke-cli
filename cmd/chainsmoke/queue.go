package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"

	"chainsmoke/pkg/core"
)

// -----------------------------------------------------------------------------
// List playback
// -----------------------------------------------------------------------------
//
// search, likes and playlist all fill the same numbered cache, and `play N`
// plays from it like a playlist: N now, the next one preloaded behind it. In a
// play session next/prev walk the list and keep one track preloaded, so a skip
// starts at once instead of waiting on yt-dlp over Tor. Before 2026-10-03
// `play N` queued N alone, and `next` failed with "playlist-next: error running
// command" (dev's report). shuffle and repeat (one / all) change what "next"
// means; every track change, including one mpv makes by itself when a song
// ends, is announced with what comes after it.
//
// One track ahead, never the whole list: SoundCloud's signed URLs expire, and
// each resolve is a round-trip over Tor (the same reason as preloadAhead).

// listQueue maps mpv's playlist back to result numbers. It is touched only from
// the session's own goroutine, so it needs no lock.
type listQueue struct {
	app     *core.App
	entries []listEntry // what is in mpv's playlist, in order
	shown   int         // the entry last announced
	shuffle bool
	order   []int  // with shuffle: the result numbers still to come
	repeat  string // "", "off", "one" or "all"
}

type listEntry struct {
	n     int // result number
	title string
}

// start plays result n now, replacing whatever was queued, and preloads what
// follows it. shuffle and repeat carry over.
func (q *listQueue) start(n int) error {
	t, err := q.app.Store.Result(n)
	if err != nil {
		return err
	}
	if _, err := q.app.Play.Command("stop"); err != nil {
		return err
	}
	q.entries = nil
	if q.shuffle {
		q.reshuffle(false, n)
	}
	if err := q.enqueue(n, t); err != nil {
		return err
	}
	q.preload()
	q.announce(0)
	return nil
}

// enqueue resolves result n and appends it to mpv's playlist, which starts it
// if mpv is idle. The resolve is the slow part: a yt-dlp run over Tor.
func (q *listQueue) enqueue(n int, t core.Track) error {
	url, err := q.app.Res.Stream(t.Permalink)
	if err != nil {
		return core.YtdlpHint(err)
	}
	if err := q.app.Play.Enqueue(url, t.Title); err != nil {
		return err
	}
	q.entries = append(q.entries, listEntry{n, t.Title})
	return nil
}

// pos is mpv's place in its playlist, when that is one of our entries.
func (q *listQueue) pos() (int, bool) {
	if len(q.entries) == 0 {
		return 0, false
	}
	v, err := q.app.Play.Command("get_property", "playlist-pos")
	f, ok := v.(float64) // JSON numbers decode as float64
	i := int(f)
	return i, err == nil && ok && i >= 0 && i < len(q.entries)
}

// following is the result to play after result n: the next in the list, or in
// shuffle the next of the shuffled order. With repeat all the list starts
// over (reshuffled, in shuffle) instead of ending.
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
	if _, err := q.app.Store.Result(n + 1); err == nil {
		return n + 1, true
	}
	if q.repeat == "all" {
		if _, err := q.app.Store.Result(1); err == nil {
			return 1, true
		}
	}
	return 0, false
}

// reshuffle draws a new random order of the list. fresh starts over with every
// track; otherwise the ones already played this session (and skip) are left
// out, so turning shuffle on does not replay what was just heard.
func (q *listQueue) reshuffle(fresh bool, skip ...int) {
	played := map[int]bool{}
	for _, s := range skip {
		played[s] = true
	}
	if !fresh {
		for _, e := range q.entries {
			played[e.n] = true
		}
	}
	q.order = nil
	for n := 1; n <= resultCount(q.app.Store); n++ {
		if !played[n] {
			q.order = append(q.order, n)
		}
	}
	rand.Shuffle(len(q.order), func(i, j int) { q.order[i], q.order[j] = q.order[j], q.order[i] })
}

// resultCount is how many results the cache holds. The Store answers only
// "result n", so this searches: about 2·log2(n) lookups, 22 for 1,700 likes.
func resultCount(s core.Store) int {
	if _, err := s.Result(1); err != nil {
		return 0
	}
	lo, hi := 1, 2
	for {
		if _, err := s.Result(hi); err != nil {
			break
		}
		lo, hi = hi, hi*2
	}
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if _, err := s.Result(mid); err == nil {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo
}

// preload keeps one track queued behind the one playing. A result that will
// not resolve is skipped, up to three in a row.
func (q *listQueue) preload() {
	i, ok := q.pos()
	if !ok || i < len(q.entries)-1 {
		return // not playing from the list, or the next one is already queued
	}
	last := q.entries[len(q.entries)-1].n
	for tries := 0; tries < 3; tries++ {
		n, ok := q.following(last)
		if !ok {
			return // the end of the list
		}
		last = n
		t, err := q.app.Store.Result(n)
		if err != nil {
			return
		}
		if err := q.enqueue(n, t); err != nil {
			fmt.Fprintf(os.Stderr, "chainsmoke: skipping %d. %s: %v\n", n, core.Clean(t.Title), err)
			continue
		}
		return
	}
}

// follow tracks mpv when it moves on by itself (a song ended): it tops up the
// preload and reports which entry is playing, and whether that is a new one
// the user has not been told about.
func (q *listQueue) follow() (int, bool) {
	i, ok := q.pos()
	if !ok {
		return 0, false
	}
	q.preload()
	return i, i != q.shown
}

// next moves to the next result. The preloaded one plays at once; if the
// preload has not landed (slow or failed), it is resolved now.
func (q *listQueue) next() error {
	i, ok := q.pos()
	if !ok { // not playing from the list: plain mpv
		_, err := q.app.Play.Command("playlist-next")
		return err
	}
	if i == len(q.entries)-1 {
		q.preload()
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

// prev moves to the track played before this one. Ones already played are
// still in mpv's playlist; in list order, one from before where `play N`
// started has to be resolved first. Shuffle has no "before" past its start.
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
	return q.start(q.entries[0].n - 1)
}

// setShuffle turns shuffle on or off from the very next track: what was
// preloaded in the old order is dropped and preloaded again in the new one.
// Outside list playback it is mpv's own playlist that gets shuffled.
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
	q.order = nil
	if on {
		q.reshuffle(false)
	}
	q.preload()
	return nil
}

// setRepeat sets repeat to off, one or all. One is mpv's loop-file. All is
// the list wrapping around (following), or mpv's loop-playlist outside list
// playback, where mpv holds the whole queue itself.
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
	q.preload() // at the end of the list, repeat all has something to queue now
	return nil
}

// modes describes shuffle and repeat for the prompt.
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

// announce says what is playing now and what comes after it, so a track that
// starts by itself is never a surprise (dev's report, 2026-10-03: a song began
// unannounced, and a `next` meant to start playback skipped it).
func (q *listQueue) announce(i int) {
	w := q.app.Out
	if w == nil {
		w = os.Stdout
	}
	q.shown = i
	e := q.entries[i]
	line := fmt.Sprintf("▶ %d. %s", e.n, core.Clean(e.title))
	switch {
	case q.repeat == "one":
		line += "  ·  repeating"
	case i+1 < len(q.entries):
		after := q.entries[i+1]
		line += fmt.Sprintf("  ·  next: %d. %s", after.n, core.Clean(after.title))
	}
	fmt.Fprintln(w, line)
}
