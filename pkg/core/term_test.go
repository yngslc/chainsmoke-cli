package core

import (
	"os"
	"testing"
)

// TestNullDeviceIsNotATerminal: NUL and /dev/null are character devices, and
// the character-device bit was all the old check looked at. So `play 1 <
// /dev/null` (nohup, cron, a scheduled task) took the interactive path, read EOF
// at the prompt, and stopped playback at once. A pipe must not count either.
func TestNullDeviceIsNotATerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Errorf("%s reads as a terminal", os.DevNull)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) || isTerminal(w) {
		t.Error("a pipe reads as a terminal")
	}
}
