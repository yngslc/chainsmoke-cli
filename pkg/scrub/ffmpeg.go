package scrub

import (
	"chainsmoke/pkg/core"
)

type ffmpegScrubber struct{}

func (s *ffmpegScrubber) Scrub(in, out string, nuclear bool) error { return core.ErrNotImplemented }
