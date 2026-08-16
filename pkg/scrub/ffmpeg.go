// Package scrub strips identifying metadata from downloaded audio:
// ffmpeg +bitexact -> selective retag -> xattr wipe -> optional mat2 pass.
// See script.sh scrub() for the reference pipeline and TASKS.md Phase 6.
package scrub

import (
	"chainsmoke/pkg/core"
)

// FFmpeg is the standard scrubber. Everything it does is a Phase-6 task; the
// signature is final so `get` can be wired against it now.
type FFmpeg struct{}

var _ core.Scrubber = (*FFmpeg)(nil)

func New() *FFmpeg { return &FFmpeg{} }

// Scrub is not implemented yet (TASKS.md T6.1-T6.4). The planned pipeline:
//   - ffmpeg -map_metadata -1 -map_metadata:s:a -1 -fflags +bitexact
//     -flags:a +bitexact -c copy   (never suppress ffmpeg's stderr — bash T1.10)
//   - selective retag: keep artist/title/album/art; drop comments, encoder,
//     source-URL frames, ISRC — via a Go tag library, no python dependency
//   - xattr cleanup (user.xdg.origin.url, user.xdg.referrer.url) + mtime normalize
//   - nuclear: mat2 --inplace when available, then a post-scrub audit print
func (s *FFmpeg) Scrub(in, out string, nuclear bool) error {
	return core.ErrNotImplemented
}
