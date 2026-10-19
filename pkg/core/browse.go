package core

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// -----------------------------------------------------------------------------
// Browsing a SoundCloud page (TASKS.md T8.2)
// -----------------------------------------------------------------------------
//
// `likes` and `playlist` are thin over Meta, and thin is the point: both are the
// same three steps `search` already runs — resolve, cache, print — differing
// only in how the page URL is built. Collect is that shared middle, so a fourth
// listing verb costs a URL builder rather than a copy of the flow.

// Collect resolves a page URL to its tracks, caches them so `play N` can index
// into them, and prints the table. limit <= 0 means the whole set.
//
// The cache write and the print see the SAME slice, in the same order: if they
// could diverge, `play 3` would start a track the user never saw on screen.
func (a *App) Collect(pageURL string, limit int) error {
	// A whole likes list comes a page at a time over Tor: well over a minute
	// for a long one, which reads as a hang unless something says otherwise.
	fmt.Fprintln(os.Stderr, "chainsmoke: fetching the list through Tor — a long one can take a minute or two")
	tracks, err := a.Res.Meta(pageURL)
	if err != nil {
		return YtdlpHint(err)
	}
	if limit > 0 && len(tracks) > limit {
		tracks = tracks[:limit]
	}
	if err := a.Store.SaveResults(tracks); err != nil {
		return err
	}
	a.PrintResults(tracks)
	return nil
}

// LikesURL builds the page URL for a user's public likes.
//
// The handle rides in argv, and that is a deliberate, bounded exception rather
// than an oversight: `search <term>` already does the same, and a handle is the
// same class of disclosure as a search term. Track URLs stay out of argv
// (invariant #3) because they identify one exact thing you are about to play;
// this identifies a page you are about to list. Anyone who wants neither in
// argv can pipe the handle on stdin, as with `play`.
func LikesURL(handle string) (string, error) {
	h, err := cleanHandle(handle, "user handle")
	if err != nil {
		return "", err
	}
	return "https://soundcloud.com/" + h + "/likes", nil
}

// SetURL builds the page URL for one playlist ("set" in SoundCloud's vocabulary)
// from a "<user>/<set>" reference.
//
// Two segments, not one, because a set slug is only unique within a user —
// there is no global playlist namespace to look one up in.
func SetURL(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	ref = strings.Trim(ref, "/")

	user, set, ok := strings.Cut(ref, "/")
	if !ok {
		if err := rejectIfURL(ref, "playlist"); err != nil {
			return "", err
		}
		return "", fmt.Errorf("playlist takes <user>/<set>, not %q\n"+
			"  chainsmoke playlist ghostinnafurcoat/late-night   # the tail of the /sets/ URL", ref)
	}
	// A set reference is sometimes copied as ".../sets/<slug>"; accept that
	// spelling rather than making the user edit it out, since it names exactly
	// the same thing.
	set = strings.TrimPrefix(set, "sets/")

	u, err := cleanHandle(user, "user handle")
	if err != nil {
		return "", err
	}
	s, err := cleanHandle(set, "set name")
	if err != nil {
		return "", err
	}
	return "https://soundcloud.com/" + u + "/sets/" + s, nil
}

// cleanHandle validates one SoundCloud permalink segment.
//
// The allowlist is what makes these verbs safe to build a URL from: with only
// [A-Za-z0-9_-] permitted, no input can introduce a second path component, a
// query string, a scheme, or a host. "evil.com" fails on the dot; "a/b" fails
// on the slash; "?x=1" fails on both. So the host of the URL we hand the
// resolver is always literally soundcloud.com — the one place worth being
// certain about, since the resolver is the single egress chokepoint.
func cleanHandle(s, what string) (string, error) {
	// TrimSpace first: "  /handle/ " starts with a space, so trimming slashes
	// before whitespace would leave both in place.
	s = strings.Trim(strings.TrimSpace(s), "/")
	if s == "" {
		return "", fmt.Errorf("empty %s", what)
	}
	if err := rejectIfURL(s, what); err != nil {
		return "", err
	}
	for _, r := range s {
		ok := r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return "", fmt.Errorf("%s %q contains %q — SoundCloud handles are letters, digits, - and _ only",
				what, s, string(r))
		}
	}
	return s, nil
}

// rejectIfURL turns "I pasted the whole URL" into the invariant-#3 message
// rather than a puzzling character complaint. The same check runs on an
// argument, a piped line and a line typed at the session prompt, so the
// message says which of those exposed it: it used to tell all three that the
// URL "already reached" the process list (2026-10-19 review, Fix 6). It does
// not echo the URL back.
func rejectIfURL(s, what string) error {
	low := strings.ToLower(s)
	if !strings.Contains(low, "://") && !strings.HasPrefix(low, "soundcloud.com") && !strings.HasPrefix(low, "www.") {
		return nil
	}
	return fmt.Errorf(
		"likes and playlist take a handle, not a URL (URLs are not accepted in argv).\n" +
			"Passed as an argument, a URL is in the OS process list, which is world-readable:\n" +
			"treat that one as exposed. Typed at the prompt or piped in, it is not.\n\n" +
			"  chainsmoke likes ghostinnafurcoat            # the handle alone is enough\n" +
			"  chainsmoke playlist ghostinnafurcoat/chill   # <user>/<set>\n" +
			"  chainsmoke likes < handle.txt                # for scripts")
}

// HandleOrStdin is the likes/playlist counterpart to ResolveIndexOrStdin: take
// the argument when there is one, otherwise read a single line. Kept separate
// because what comes back is a page reference, not a cached Track.
func (a *App) HandleOrStdin(arg, prompt, usage string) (string, error) {
	if strings.TrimSpace(arg) != "" {
		return arg, nil
	}
	// The read error is deliberately not returned: on a closed stdin it is
	// io.EOF, and "chainsmoke: EOF" tells a user nothing about what to type.
	// An empty line is the same situation however it arose.
	line, _ := a.readLine(prompt)
	if line == "" {
		return "", errors.New(usage)
	}
	return line, nil
}
