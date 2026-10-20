// Package scrub strips identifying metadata from downloaded audio: ffmpeg
// +bitexact -> selective retag -> xattr wipe -> optional mat2 pass.
package scrub

import (
	"chainsmoke/pkg/core"
)

// FFmpeg is the standard scrubber.
type FFmpeg struct{}

var _ core.Scrubber = (*FFmpeg)(nil)

func New() *FFmpeg { return &FFmpeg{} }

// Scrub is not implemented yet.
func (s *FFmpeg) Scrub(in, out string, nuclear bool) error {
	return core.ErrNotImplemented
}
