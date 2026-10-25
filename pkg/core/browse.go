package core

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Collect resolves a page URL to its tracks, caches them so `play N` can index
// into them, and prints the table. limit <= 0 means the whole set.
func (a *App) Collect(pageURL string, limit int) error {
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
func LikesURL(handle string) (string, error) {
	h, err := cleanHandle(handle, "user handle")
	if err != nil {
		return "", err
	}
	return "https://soundcloud.com/" + h + "/likes", nil
}

// SetURL builds the page URL for one playlist ("set" in SoundCloud's
// vocabulary) from a "<user>/<set>" reference.
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

func cleanHandle(s, what string) (string, error) {
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
// the argument when there is one, otherwise read a single line.
func (a *App) HandleOrStdin(arg, prompt, usage string) (string, error) {
	if strings.TrimSpace(arg) != "" {
		return arg, nil
	}
	line, _ := a.readLine(prompt)
	if line == "" {
		return "", errors.New(usage)
	}
	return line, nil
}
