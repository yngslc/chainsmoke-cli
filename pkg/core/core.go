package core

import (
	"errors"
)

var ErrNotImplemented = errors.New("not implemented")

type Track struct {
	ID        string
	Title     string
	Uploader  string
	Duration  int // seconds
	Streams   int
	Likes     int
	Reposts   int
	Permalink string // soundcloud.com/... — kept internal, never printed into argv examples
}

type App struct {
	net   Net
	res   Resolver
	play  Player
	scrub Scrubber
	store Store
}

type Playlist struct {
	ID     string
	Title  string
	Tracks []Track
	Plays  int
}

type Net interface {
	Up() error
	Down() error
	Rotate() error
	SelfTest() error
	ProxyURL() string
}

type Resolver interface {
	Meta(pageURL string) ([]Track, error)
	Stream(permalink string) (streamURL string, err error)
	Search(term string, limit int) ([]Track, error)
}

type Player interface {
	Ensure() error
	Enqueue(streamURL string) error
	Command(args ...any) (any, error)
	NowPlaying() (string, error)
	Down() error
}

type Scrubber interface {
	Scrub(in, out string, nuclear bool) error
}

type Store interface {
	SaveResults([]Track) error
	Result(n int) (Track, error)
	Purge() error
}
