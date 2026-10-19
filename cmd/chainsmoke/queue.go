package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"

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
// each resolve is a round-trip over Tor.

// listQueue maps mpv's playlist back to result numbers. It is touched only from
// the session's own goroutine, so it needs no lock: a background preload
// touches nothing here and hands its result back over its own channel.
type listQueue struct {
	app     *core.App
	list    []core.Track // the snapshot being walked
	entries []listEntry  // what is in mpv's playlist, in order
	shown   int          // the entry last announced
	shuffle bool
	order   []int  // with shuffle: the result numbers still to come
	repeat  string // "", "off", "one" or "all"

	cursor  int            // the result preloading continues after: last queued or skipped
	loading chan preloaded // a background resolve in flight; nil when none
	pending listEntry      // what loading is resolving, for the "next:" line
	skips   int            // results skipped in a row; preloading stops at 3
}

type listEntry struct {
	n     int // result number
	title string
}

// preloaded is a background resolve's result.
type preloaded struct {
	entry listEntry
	url   string
	err   error
}

// start plays result n now, replacing whatever was queued, and preloads what
// follows it. It takes a fresh snapshot of the result cache; shuffle and repeat
// carry over.
func (q *listQueue) start(n int) error {
	list, err := q.snapshotFrom(q.app.Store, n)
	if err != nil {
		return err
	}
	return q.playAt(list, n)
}

// startList plays a set given whole — a set or track URL from stdin, which
// Meta expanded — as a list of its own: 1 now, next/prev along the rest. It
// used to queue two tracks and suggest a `next` that then failed (2026-10-05
// review, R2). Skip-not-abort (T1.14): a first track that will not play costs
// only itself; after 3 in a row the set is reported as failed.
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

// playAt plays entry n of list, which becomes the list being walked. Nothing
// changes — not mpv, not this queue — until n has resolved and passed mpv's
// preflight, so a track that will not play leaves the old one playing. It used
// to stop first and leave silence (2026-10-05 review, R5).
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
	q.cursor, q.skips, q.loading = n, 0, nil // a preload in flight was for the old list
	q.order = nil
	if q.shuffle {
		q.reshuffle(false)
	}
	q.preload()
	q.announce(0)
	return nil
}

// switchToLast makes the entry just appended the one playing and drops the
// rest of mpv's playlist. Measured on mpv 0.41: after playlist-play-index,
// playlist-clear keeps exactly the new entry, and it plays.
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
	_, err = q.app.Play.Command("playlist-clear")
	return err
}

// snapshotFrom takes this list's own copy of the result cache, so a search,
// likes or playlist that lands mid-play cannot change what is being walked
// (2026-10-05 review, R1). One Results() read: a Result(n) loop re-parses the
// cache file every call, 5.7s for 1,700 likes. n must be in the set.
func (q *listQueue) snapshotFrom(s core.Store, n int) ([]core.Track, error) {
	list, err := s.Results()
	if err != nil {
		return nil, err
	}
	if n < 1 || n > len(list) {
		if _, err := s.Result(n); err != nil {
			return nil, err // the store's own "no cached result #n (have 1-m)"
		}
		return nil, fmt.Errorf("no cached result #%d", n)
	}
	return slices.Clone(list), nil
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
	if n < len(q.list) {
		return n + 1, true
	}
	if q.repeat == "all" && len(q.list) > 0 {
		return 1, true
	}
	return 0, false
}

// reshuffle draws a new random order of the list. fresh starts over with every
// track; otherwise the ones already played this session are left out, so
// turning shuffle on does not replay what was just heard.
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

// preload starts resolving, in the background, the track that follows the
// last one queued — unless one is already queued behind the one playing, or
// already resolving. A resolve is a yt-dlp run over Tor, seconds to minutes;
// run on the prompt's goroutine it froze the prompt and held off Ctrl+C until
// it finished (2026-10-05 review, R6). landed() queues the result.
func (q *listQueue) preload() {
	if q.loading != nil || q.skips >= 3 || len(q.entries) == 0 {
		return
	}
	if i, ok := q.pos(); ok && i < len(q.entries)-1 {
		return // the next one is already queued
	}
	// Not ok is mpv idle at the end of the list: a song ended before the next
	// one resolved, or that one was skipped. landed() checks the playlist is
	// still ours before queuing anything.
	n, ok := q.following(q.cursor)
	if !ok {
		return // the end of the list
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

// landed queues a finished background resolve in mpv. A track that will not
// resolve or play is skipped — the error says which — and the one after it
// starts resolving. If mpv's playlist changed size while it resolved (a `stop`
// clears it), the result is dropped: queuing it would start playback nobody
// asked for. A song that simply ended leaves the playlist as it was, so the
// list carries on.
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
		q.preload() // the one after it
		return fmt.Errorf("skipping %d. %s: %w", p.entry.n, core.Clean(p.entry.title), err)
	}
	q.skips = 0
	q.entries = append(q.entries, p.entry)
	return nil
}

// settle waits for a background resolve to land, and for any it chains into
// after a skip.
func (q *listQueue) settle() {
	for q.loading != nil {
		if err := q.landed(<-q.loading); err != nil {
			fmt.Fprintln(os.Stderr, "chainsmoke:", err)
		}
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

// next moves to the next result. The preloaded one plays at once; if it has
// not landed yet, next waits for it — it is the very track asked for.
func (q *listQueue) next() error {
	i, ok := q.pos()
	if !ok { // not playing from the list: plain mpv
		_, err := q.app.Play.Command("playlist-next")
		return err
	}
	if i == len(q.entries)-1 {
		q.skips = 0 // an explicit next gets fresh tries
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
	return q.playAt(q.list, q.entries[0].n-1) // same snapshot: not a new `play N`
}

// setShuffle turns shuffle on or off from the very next track: what was
// preloaded in the old order, or is still resolving, is dropped, and the next
// one is preloaded in the new order. Outside list playback it is mpv's own
// playlist that gets shuffled.
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
	if mode == "all" {
		q.preload() // at the end of the list, repeat all has something to queue now
	}
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
// unannounced, and a `next` meant to start playback skipped it). It does not
// wait for the next track to resolve: a background preload already knows
// which one it is.
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
